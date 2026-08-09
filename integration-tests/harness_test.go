//go:build integration

// Package integration runs the six user stories against the compiled railctl
// binary on a live chain. Only the compiled surface is driven; chain reads are
// used as an independent oracle, never as a way into the rail's internals.
package integration

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

// anvil's first account, which funds every deployment and the shim.
const (
	deployerKeyHex     = "ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"
	railKeyHex         = "59c6995e998f97a5a0044966f0945389dc9e86dae88c7a8412f4603b6b78690d"
	paymasterKeyHex    = "5de4111afa1a4b94908f83103eb1f1706367c2e68ca870fc3fb9a804cdab365a"
	otherRailKeyHex    = "7c852118294e51e653712a81e05800f419141751be58f605c371e15141b007a6"
	defaultChainID     = 31337
	slotsInAnEpoch     = 1
	blocksToFinalise   = 3 // finalized lags the head by two blocks
	tokenDecimalsScale = 1_000_000
)

// harness is one isolated fixture: its own chain, its own deployment, its own
// store. Nothing is shared between stories.
type harness struct {
	t          *testing.T
	dir        string
	rpcURL     string
	client     *ethclient.Client
	chainID    *big.Int
	addr       map[string]common.Address
	shim       *shim
	configPath string
	txMu       sync.Mutex
}

func newHarness(t *testing.T) *harness {
	return newHarnessOn(t, defaultChainID)
}

// newHarnessOn builds a fixture on a named chain. A second chain is a second
// settlement domain: separate balances, separate identifiers, no bridging.
func newHarnessOn(t *testing.T, chain int64) *harness {
	t.Helper()
	dir := t.TempDir()
	h := &harness{t: t, dir: dir, chainID: big.NewInt(chain), addr: map[string]common.Address{}}

	h.startAnvil()
	h.deployAll()
	h.shim = newShim(t, h)
	h.writeConfig()
	// Setup is done: from here the test decides when blocks appear.
	h.rpcCall("evm_setAutomine", false)
	return h
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func (h *harness) startAnvil() {
	port := freePort(h.t)
	h.rpcURL = fmt.Sprintf("http://127.0.0.1:%d", port)

	cmd := exec.Command("anvil",
		"--port", fmt.Sprint(port),
		"--chain-id", h.chainID.String(),
		"--slots-in-an-epoch", fmt.Sprint(slotsInAnEpoch),
		"--silent",
	)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		// The story suite is opt-in, so a missing chain is a failure to run
		// it, never a quiet pass.
		h.t.Fatalf("anvil is required for the story tests: %v", err)
	}
	h.t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	deadline := time.Now().Add(30 * time.Second)
	for {
		client, err := ethclient.Dial(h.rpcURL)
		if err == nil {
			if _, err := client.BlockNumber(context.Background()); err == nil {
				h.client = client
				h.t.Cleanup(client.Close)
				return
			}
			client.Close()
		}
		if time.Now().After(deadline) {
			h.t.Fatal("anvil did not become ready")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// --- chain control ---

func (h *harness) rpcCall(method string, args ...any) {
	h.t.Helper()
	c, err := rpc.Dial(h.rpcURL)
	if err != nil {
		h.t.Fatal(err)
	}
	defer c.Close()
	if err := c.Call(nil, method, args...); err != nil {
		h.t.Fatalf("%s: %v", method, err)
	}
}

// mine produces n blocks. Nothing advances unless a story asks it to.
func (h *harness) mine(n int) {
	h.t.Helper()
	for i := 0; i < n; i++ {
		h.rpcCall("evm_mine")
	}
}

// finalise mines until the head at call time is finalized, which is the only
// point at which the rail may report a confirmed fact.
func (h *harness) finalise() {
	h.t.Helper()
	h.mine(blocksToFinalise)
}

// advanceTime moves chain time forward, which is what expires a sponsorship.
func (h *harness) advanceTime(d time.Duration) {
	h.t.Helper()
	h.rpcCall("evm_increaseTime", int64(d.Seconds()))
	h.mine(1)
}

// --- deployment ---

func creationCode(t *testing.T, name string) []byte {
	t.Helper()
	// Our own contracts and the EntryPoint are compiled from source.
	forgeArtifact := filepath.Join("..", "contracts", "out", name+".sol", name+".json")
	if raw, err := os.ReadFile(forgeArtifact); err == nil {
		var a struct {
			Bytecode struct {
				Object string `json:"object"`
			} `json:"bytecode"`
		}
		if err := json.Unmarshal(raw, &a); err != nil {
			t.Fatal(err)
		}
		return common.FromHex(a.Bytecode.Object)
	}
	// The Safe suite is used as published, never recompiled.
	raw, err := os.ReadFile(filepath.Join("..", "deployments", "bytecode", name+".json"))
	if err != nil {
		t.Fatalf("no creation code for %s: %v (run `forge build` in contracts/)", name, err)
	}
	var vendored struct {
		Bytecode string `json:"bytecode"`
	}
	if err := json.Unmarshal(raw, &vendored); err != nil {
		t.Fatal(err)
	}
	return common.FromHex(vendored.Bytecode)
}

func (h *harness) deploy(name string, args ...[]byte) common.Address {
	h.t.Helper()
	code := creationCode(h.t, name)
	for _, a := range args {
		code = append(code, a...)
	}
	receipt := h.sendTx(nil, code, nil)
	if receipt.ContractAddress == (common.Address{}) {
		h.t.Fatalf("%s did not deploy", name)
	}
	h.addr[name] = receipt.ContractAddress
	return receipt.ContractAddress
}

func (h *harness) deployAll() {
	h.t.Helper()
	entryPoint := h.deploy("EntryPoint")
	token := h.deploy("MockUSDT0")
	railAddr := h.deploy("JuiceRail", wordOf(token))

	h.deploy("Safe")
	h.deploy("SafeProxyFactory")
	h.deploy("MultiSendCallOnly")
	h.deploy("SafeModuleSetup")
	h.deploy("Safe4337Module", wordOf(entryPoint))

	paymasterSigner := crypto.PubkeyToAddress(mustKey(h.t, paymasterKeyHex).PublicKey)
	h.deploy("JuiceRailPaymaster",
		wordOf(entryPoint), wordOf(railAddr), wordOf(token),
		wordOf(h.addr["MultiSendCallOnly"]), wordOf(paymasterSigner))

	// Story 1: the operator funds sponsorship. Account holders never hold ETH.
	h.fundPaymaster(big.NewInt(5e18))
}

func (h *harness) fundPaymaster(amount *big.Int) {
	h.t.Helper()
	data := mustPack(h.t, entryPointABI, "depositTo", h.addr["JuiceRailPaymaster"])
	to := h.addr["EntryPoint"]
	h.sendTx(&to, data, amount)
}

// paymasterDeposit is the operator's remaining sponsorship budget.
func (h *harness) paymasterDeposit() *big.Int {
	h.t.Helper()
	return h.callUint(h.addr["EntryPoint"], entryPointABI, "balanceOf", h.addr["JuiceRailPaymaster"])
}

// mintTo funds an address with tokens, standing in for money arriving from
// outside. It works on an account that does not exist on chain yet.
func (h *harness) mintTo(account common.Address, amount *big.Int) {
	h.t.Helper()
	to := h.addr["MockUSDT0"]
	h.sendTx(&to, mustPack(h.t, tokenABI, "mint", account, amount), nil)
}

// --- transactions ---

func (h *harness) sendTx(to *common.Address, data []byte, value *big.Int) *types.Receipt {
	h.t.Helper()
	ctx := context.Background()
	key := mustKey(h.t, deployerKeyHex)
	from := crypto.PubkeyToAddress(key.PublicKey)

	nonce, err := h.client.PendingNonceAt(ctx, from)
	if err != nil {
		h.t.Fatal(err)
	}
	gasPrice, err := h.client.SuggestGasPrice(ctx)
	if err != nil {
		h.t.Fatal(err)
	}
	if value == nil {
		value = new(big.Int)
	}
	tx := types.NewTx(&types.LegacyTx{
		Nonce: nonce, To: to, Value: value, Gas: 15_000_000,
		GasPrice: new(big.Int).Mul(gasPrice, big.NewInt(2)), Data: data,
	})
	signed, err := types.SignTx(tx, types.NewEIP155Signer(h.chainID), key)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := h.client.SendTransaction(ctx, signed); err != nil {
		h.t.Fatalf("send transaction: %v", err)
	}
	// Setup transactions land immediately; the stories control everything
	// after that with mine and finalise.
	h.rpcCall("evm_mine")

	deadline := time.Now().Add(20 * time.Second)
	for {
		receipt, err := h.client.TransactionReceipt(ctx, signed.Hash())
		if err == nil {
			return receipt
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("transaction %s was never mined", signed.Hash())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// sendTxAsync submits without waiting: with automatic mining off, the story
// decides when the transaction lands.
func (h *harness) sendTxAsync(to *common.Address, data []byte) {
	h.t.Helper()
	ctx := context.Background()
	key := mustKey(h.t, deployerKeyHex)
	from := crypto.PubkeyToAddress(key.PublicKey)

	h.txMu.Lock()
	defer h.txMu.Unlock()

	nonce, err := h.client.PendingNonceAt(ctx, from)
	if err != nil {
		h.t.Fatal(err)
	}
	gasPrice, err := h.client.SuggestGasPrice(ctx)
	if err != nil {
		h.t.Fatal(err)
	}
	tx := types.NewTx(&types.LegacyTx{
		Nonce: nonce, To: to, Value: new(big.Int), Gas: 15_000_000,
		GasPrice: new(big.Int).Mul(gasPrice, big.NewInt(2)), Data: data,
	})
	signed, err := types.SignTx(tx, types.NewEIP155Signer(h.chainID), key)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := h.client.SendTransaction(ctx, signed); err != nil {
		h.t.Fatalf("submit bundle: %v", err)
	}
}

// beneficiary receives the bundler's gas refund.
func (h *harness) beneficiary() common.Address {
	return crypto.PubkeyToAddress(mustKey(h.t, deployerKeyHex).PublicKey)
}

// --- reads used as an independent oracle ---

func (h *harness) callUint(to common.Address, parsed abi.ABI, method string, args ...any) *big.Int {
	h.t.Helper()
	out, err := h.client.CallContract(context.Background(),
		ethereum.CallMsg{To: &to, Data: mustPack(h.t, parsed, method, args...)}, nil)
	if err != nil {
		h.t.Fatalf("%s: %v", method, err)
	}
	return new(big.Int).SetBytes(out)
}

func (h *harness) railBalance(account common.Address) *big.Int {
	return h.callUint(h.addr["JuiceRail"], railABI, "balanceOf", account)
}

func (h *harness) tokenBalance(account common.Address) *big.Int {
	return h.callUint(h.addr["MockUSDT0"], tokenABI, "balanceOf", account)
}

// heldByRail is the contract's own token holding, the left side of the
// solvency invariant.
func (h *harness) heldByRail() *big.Int {
	return h.tokenBalance(h.addr["JuiceRail"])
}

// railEvents counts the rail events carrying an identifier, so a story can
// assert that money moved exactly once.
func (h *harness) railEvents(id string) int {
	h.t.Helper()
	logs, err := h.client.FilterLogs(context.Background(), ethereum.FilterQuery{
		FromBlock: big.NewInt(0),
		Addresses: []common.Address{h.addr["JuiceRail"]},
		Topics: [][]common.Hash{
			{
				crypto.Keccak256Hash([]byte("Deposited(bytes32,address,uint256)")),
				crypto.Keccak256Hash([]byte("Settled(bytes32,address,address,uint256)")),
				crypto.Keccak256Hash([]byte("Withdrawn(bytes32,address,address,uint256)")),
			},
			{common.HexToHash(id)},
		},
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return len(logs)
}

// --- the railctl binary ---

func (h *harness) writeConfig() {
	h.t.Helper()
	cfg := map[string]any{
		"name":              "local",
		"chainId":           h.chainID.Uint64(),
		"rpc":               h.rpcURL,
		"bundler":           h.shim.url,
		"rail":              h.addr["JuiceRail"].Hex(),
		"token":             h.addr["MockUSDT0"].Hex(),
		"paymaster":         h.addr["JuiceRailPaymaster"].Hex(),
		"entryPoint":        h.addr["EntryPoint"].Hex(),
		"safeSingleton":     h.addr["Safe"].Hex(),
		"safeProxyFactory":  h.addr["SafeProxyFactory"].Hex(),
		"safeModule":        h.addr["Safe4337Module"].Hex(),
		"safeModuleSetup":   h.addr["SafeModuleSetup"].Hex(),
		"multiSendCallOnly": h.addr["MultiSendCallOnly"].Hex(),
		"finality":          "finalized",
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		h.t.Fatal(err)
	}
	h.configPath = filepath.Join(h.dir, "domain.json")
	if err := os.WriteFile(h.configPath, raw, 0o600); err != nil {
		h.t.Fatal(err)
	}
}

var railctlBinary string

// TestMain compiles the app once. Every story drives that one binary, so the
// tests exercise the compiled surface and nothing else.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "railctl-binary")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	railctlBinary = filepath.Join(dir, "railctl")

	build := exec.Command("go", "build", "-o", railctlBinary, "github.com/daios-ai/juice-rail/cmd/railctl")
	build.Dir = ".."
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build railctl: %v\n%s", err, out)
		os.Remove(dir)
		os.Exit(1)
	}

	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func buildRailctl(t *testing.T) string {
	t.Helper()
	return railctlBinary
}

// operator is one railctl user: its own key and its own store.
type operator struct {
	h     *harness
	name  string
	key   string
	store string
}

func (h *harness) operator(name, key string) *operator {
	return &operator{h: h, name: name, key: key, store: filepath.Join(h.dir, name+".db")}
}

// run drives the binary and returns its stdout. Diagnostics stay on stderr.
func (o *operator) run(args ...string) (string, error) {
	o.h.t.Helper()
	cmd := exec.Command(buildRailctl(o.h.t),
		append([]string{"-config", o.h.configPath, "-store", o.store}, args...)...)
	cmd.Env = append(os.Environ(),
		"RAILCTL_KEY="+o.key,
		"RAILCTL_PAYMASTER_KEY="+paymasterKeyHex,
	)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return string(out), fmt.Errorf("railctl %s: %w: %s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(string(out)), nil
}

func (o *operator) mustRun(args ...string) string {
	o.h.t.Helper()
	out, err := o.run(args...)
	if err != nil {
		o.h.t.Fatalf("%v", err)
	}
	return out
}

// account is the operator's counterfactual address on this domain.
func (o *operator) account() common.Address {
	o.h.t.Helper()
	return common.HexToAddress(o.mustRun("account"))
}

// status reads the identifier's status through the binary.
func (o *operator) status(id string) string {
	o.h.t.Helper()
	var reply struct {
		Status string `json:"status"`
	}
	out := o.mustRun("-json", "status", id)
	if err := json.Unmarshal([]byte(out), &reply); err != nil {
		o.h.t.Fatalf("status output %q: %v", out, err)
	}
	return reply.Status
}

// settleAndFinalise mines the pending work and pushes it past finality.
func (h *harness) settleAndFinalise() {
	h.t.Helper()
	h.mine(1)
	h.finalise()
}

// --- shared ABIs ---

var (
	entryPointABI = mustABI(`[
      {"type":"function","name":"depositTo","inputs":[{"name":"account","type":"address"}],"stateMutability":"payable"},
      {"type":"function","name":"balanceOf","inputs":[{"name":"account","type":"address"}],"outputs":[{"type":"uint256"}],"stateMutability":"view"},
      {"type":"function","name":"handleOps","inputs":[
        {"name":"ops","type":"tuple[]","components":[
          {"name":"sender","type":"address"},{"name":"nonce","type":"uint256"},
          {"name":"initCode","type":"bytes"},{"name":"callData","type":"bytes"},
          {"name":"accountGasLimits","type":"bytes32"},{"name":"preVerificationGas","type":"uint256"},
          {"name":"gasFees","type":"bytes32"},{"name":"paymasterAndData","type":"bytes"},
          {"name":"signature","type":"bytes"}]},
        {"name":"beneficiary","type":"address"}]}]`)

	railABI = mustABI(`[
      {"type":"function","name":"balanceOf","inputs":[{"name":"account","type":"address"}],"outputs":[{"type":"uint256"}],"stateMutability":"view"},
      {"type":"function","name":"deposit","inputs":[
        {"name":"id","type":"bytes32"},{"name":"account","type":"address"},{"name":"amount","type":"uint256"}]}]`)

	tokenABI = mustABI(`[
      {"type":"function","name":"mint","inputs":[{"name":"to","type":"address"},{"name":"amount","type":"uint256"}]},
      {"type":"function","name":"approve","inputs":[{"name":"spender","type":"address"},{"name":"amount","type":"uint256"}],"outputs":[{"type":"bool"}]},
      {"type":"function","name":"balanceOf","inputs":[{"name":"account","type":"address"}],"outputs":[{"type":"uint256"}],"stateMutability":"view"}]`)
)

func mustABI(s string) abi.ABI {
	parsed, err := abi.JSON(strings.NewReader(s))
	if err != nil {
		panic(err)
	}
	return parsed
}

func mustPack(t *testing.T, parsed abi.ABI, method string, args ...any) []byte {
	t.Helper()
	out, err := parsed.Pack(method, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func mustKey(t *testing.T, hex string) *ecdsa.PrivateKey {
	t.Helper()
	key, err := crypto.HexToECDSA(hex)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// wordOf encodes an address as one ABI word, for constructor arguments.
func wordOf(a common.Address) []byte {
	var w [32]byte
	copy(w[12:], a.Bytes())
	return w[:]
}

func tokens(n int64) *big.Int {
	return big.NewInt(n * tokenDecimalsScale)
}
