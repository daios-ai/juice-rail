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

const (
	// anvil's first two accounts, which arrive with ether: one deploys, one
	// relays. Everyone else is a fresh key and holds no ether at all.
	deployerKeyHex = "ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"
	relayerKeyHex  = "59c6995e998f97a5a0044966f0945389dc9e86dae88c7a8412f4603b6b78690d"
	relayer2KeyHex = "5de4111afa1a4b94908f83103eb1f1706367c2e68ca870fc3fb9a804cdab365a"

	aliceKeyHex = "1111111111111111111111111111111111111111111111111111111111111111"
	bobKeyHex   = "2222222222222222222222222222222222222222222222222222222222222222"

	defaultChainID     = 31337
	slotsInAnEpoch     = 1
	blocksToFinalise   = 3 // finalized lags the head by two blocks
	tokenDecimalsScale = 1_000_000
)

// harness is one isolated fixture: its own chain, its own deployment, its own
// stores. Nothing is shared between stories.
type harness struct {
	t          *testing.T
	dir        string
	rpcURL     string
	client     *ethclient.Client
	chainID    *big.Int
	addr       map[string]common.Address
	configPath string
}

func newHarness(t *testing.T) *harness {
	return newHarnessOn(t, defaultChainID)
}

// newHarnessOn builds a fixture on a named chain. A second chain is a second
// domain: separate balances, separate identifiers, no bridging.
func newHarnessOn(t *testing.T, chain int64) *harness {
	t.Helper()
	dir := t.TempDir()
	h := &harness{t: t, dir: dir, chainID: big.NewInt(chain), addr: map[string]common.Address{}}

	h.startAnvil()
	h.deployAll()
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

// settleAndFinalise mines the pending work and pushes it past finality.
func (h *harness) settleAndFinalise() {
	h.t.Helper()
	h.mine(1)
	h.finalise()
}

// advanceTime moves chain time forward, which is what expires a signed
// operation.
func (h *harness) advanceTime(d time.Duration) {
	h.t.Helper()
	h.rpcCall("evm_increaseTime", int64(d.Seconds()))
	h.mine(1)
}

// --- deployment ---

func creationCode(t *testing.T, name string) []byte {
	t.Helper()
	artifact := filepath.Join("..", "contracts", "out", name+".sol", name+".json")
	raw, err := os.ReadFile(artifact)
	if err != nil {
		t.Fatalf("no creation code for %s: %v (run `forge build` in contracts/)", name, err)
	}
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

// deployAll stands up one domain. There is nothing else to deploy and nobody
// to fund: the rail is adminless and needs no operator infrastructure.
func (h *harness) deployAll() {
	h.t.Helper()
	token := h.deploy("MockUSDT0")
	h.deploy("JuiceRail", wordOf(token))
}

// mintTo funds an address with tokens, standing in for money arriving from
// outside.
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

func (h *harness) etherBalance(account common.Address) *big.Int {
	h.t.Helper()
	balance, err := h.client.BalanceAt(context.Background(), account, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	return balance
}

// assertNoEther is the persona requirement, checked rather than assumed:
// ordinary participants never hold the chain's native currency.
func (h *harness) assertNoEther(who string, account common.Address) {
	h.t.Helper()
	if balance := h.etherBalance(account); balance.Sign() != 0 {
		h.t.Fatalf("%s holds %s wei: participants must never need gas", who, balance)
	}
}

// railEvents counts the rail events an account emitted under an identifier, so
// a story can assert that money moved exactly once.
func (h *harness) railEvents(account common.Address, id string) int {
	h.t.Helper()
	logs, err := h.client.FilterLogs(context.Background(), ethereum.FilterQuery{
		FromBlock: big.NewInt(0),
		Addresses: []common.Address{h.addr["JuiceRail"]},
		Topics: [][]common.Hash{
			{
				crypto.Keccak256Hash([]byte("Deposited(address,bytes32,address,uint256,uint256,address,uint256)")),
				crypto.Keccak256Hash([]byte("Transferred(address,bytes32,address,uint256,uint256,address,uint256)")),
				crypto.Keccak256Hash([]byte("Withdrawn(address,bytes32,address,uint256,uint256,address,uint256)")),
			},
			{common.BytesToHash(account.Bytes())},
			{common.HexToHash(id)},
		},
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return len(logs)
}

// assertSolvent is the invariant every story ends on: every balance is backed.
func (h *harness) assertSolvent(accounts ...common.Address) {
	h.t.Helper()
	sum := new(big.Int)
	for _, a := range accounts {
		sum.Add(sum, h.railBalance(a))
	}
	if held := h.heldByRail(); held.Cmp(sum) < 0 {
		h.t.Fatalf("held %s < sum of balances %s", held, sum)
	}
}

// --- the railctl binary ---

func (h *harness) writeConfig() {
	h.t.Helper()
	cfg := map[string]any{
		"name":     "local",
		"chainId":  h.chainID.Uint64(),
		"rpc":      h.rpcURL,
		"rail":     h.addr["JuiceRail"].Hex(),
		"token":    h.addr["MockUSDT0"].Hex(),
		"finality": "finalized",
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
		os.RemoveAll(dir)
		os.Exit(1)
	}

	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// participant is one railctl user: its own key and its own store.
type participant struct {
	h     *harness
	name  string
	key   string
	store string
}

func (h *harness) participant(name, key string) *participant {
	return &participant{h: h, name: name, key: key, store: filepath.Join(h.dir, name+".db")}
}

// run drives the binary and returns its stdout. Diagnostics stay on stderr.
func (p *participant) run(args ...string) (string, error) {
	p.h.t.Helper()
	cmd := exec.Command(railctlBinary,
		append([]string{"-config", p.h.configPath, "-store", p.store}, args...)...)
	cmd.Env = append(os.Environ(), "RAILCTL_KEY="+p.key)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return string(out), fmt.Errorf("railctl %s: %w: %s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(string(out)), nil
}

func (p *participant) mustRun(args ...string) string {
	p.h.t.Helper()
	out, err := p.run(args...)
	if err != nil {
		p.h.t.Fatalf("%v", err)
	}
	return out
}

// mustFail runs a command that must be refused, and returns the refusal.
func (p *participant) mustFail(args ...string) string {
	p.h.t.Helper()
	out, err := p.run(args...)
	if err == nil {
		p.h.t.Fatalf("railctl %s was expected to fail, got %q", strings.Join(args, " "), out)
	}
	return err.Error()
}

// account is the participant's address: an ordinary Ethereum address.
func (p *participant) account() common.Address {
	p.h.t.Helper()
	return common.HexToAddress(p.mustRun("account"))
}

// status reads the intent's status through the binary.
func (p *participant) status(id string) string {
	p.h.t.Helper()
	var reply struct {
		Status string `json:"status"`
	}
	out := p.mustRun("-json", "status", id)
	if err := json.Unmarshal([]byte(out), &reply); err != nil {
		p.h.t.Fatalf("status output %q: %v", out, err)
	}
	return reply.Status
}

// sign writes a signed operation for a relayer to carry, and returns its path.
func (p *participant) sign(kind, id string, party common.Address, amount, fee int64, relayer common.Address, extra ...string) string {
	p.h.t.Helper()
	// One file per (signer, kind, intent, relayer): re-signing for the same
	// relayer must land on the same file, so a story can see that the variant
	// was reused rather than replaced.
	path := filepath.Join(p.h.dir, fmt.Sprintf("%s-%s-%s-%s.json", p.name, kind, id[:10], relayer.Hex()[:10]))
	args := append([]string{
		"-fee", fmt.Sprint(fee),
		"-relayer", relayer.Hex(),
		"-out", path,
	}, extra...)
	args = append(args, kind, id, party.Hex(), fmt.Sprint(amount))
	p.mustRun(args...)
	return path
}

// --- shared ABIs ---

var (
	railABI = mustABI(`[
      {"type":"function","name":"balanceOf","inputs":[{"name":"account","type":"address"}],"outputs":[{"type":"uint256"}],"stateMutability":"view"}]`)

	tokenABI = mustABI(`[
      {"type":"function","name":"mint","inputs":[{"name":"to","type":"address"},{"name":"amount","type":"uint256"}]},
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

func addressOf(t *testing.T, hexKey string) common.Address {
	t.Helper()
	return crypto.PubkeyToAddress(mustKey(t, hexKey).PublicKey)
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

// freshID is an unguessable identifier, which is what a host would issue.
func freshID(t *testing.T) string {
	t.Helper()
	var b [32]byte
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	copy(b[:], crypto.Keccak256(crypto.FromECDSA(key)))
	return "0x" + common.Bytes2Hex(b[:])
}
