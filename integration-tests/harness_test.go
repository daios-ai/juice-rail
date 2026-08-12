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
	// anvil's first account arrives with ether and does the setup. Everyone
	// else is a fresh key that starts with nothing at all.
	deployerKeyHex = "ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"
	aliceKeyHex    = "1111111111111111111111111111111111111111111111111111111111111111"
	bobKeyHex      = "2222222222222222222222222222222222222222222222222222222222222222"
	payerKeyHex    = "3333333333333333333333333333333333333333333333333333333333333333"

	defaultChainID   = 31337
	slotsInAnEpoch   = 1
	blocksToFinalise = 3 // finalized lags the head by two blocks

	tokenScale = 1_000_000 // the token has six decimals

	// The reserve policy the stories run under, in wei.
	reserveMin      = "20000000000000000" // 0.02
	reserveMax      = "50000000000000000" // 0.05
	reserveFeeBound = "10000000000000000" // 0.01
	slippageBps     = 50

	// The venue prices one unit of native currency at 3000 token units.
	venuePrice = 3000 * tokenScale
	feeTier    = 500
)

// wethPlaceholder stands for the wrapped native token. The mock venue holds
// native currency directly, so this is only ever a label in its calldata.
var wethPlaceholder = common.HexToAddress("0x000000000000000000000000000000000000eeee")

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
// domain: separate money, separate records, no bridging.
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
		// The story suite is opt-in, so a missing chain is a failure to run it,
		// never a quiet pass.
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

// setReserve puts an account's native balance at an exact figure, which is how
// a story arranges a reserve too low to pay from.
func (h *harness) setReserve(account common.Address, wei *big.Int) {
	h.t.Helper()
	h.rpcCall("anvil_setBalance", account, "0x"+wei.Text(16))
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

func (h *harness) deploy(name string, value *big.Int, args ...[]byte) common.Address {
	h.t.Helper()
	code := creationCode(h.t, name)
	for _, a := range args {
		code = append(code, a...)
	}
	receipt := h.sendTx(nil, code, value)
	if receipt.ContractAddress == (common.Address{}) {
		h.t.Fatalf("%s did not deploy", name)
	}
	h.addr[name] = receipt.ContractAddress
	return receipt.ContractAddress
}

// deployAll stands up one domain: the token accounted on it and the venue
// where an account buys its own gas. There is no rail contract and nobody in
// charge of one.
func (h *harness) deployAll() {
	h.t.Helper()
	token := h.deploy("MockUSDT0", nil)
	h.deploy("MockRouter", ether(10),
		wordOf(token), wordOf(wethPlaceholder),
		wordOfInt(big.NewInt(feeTier)), wordOfInt(big.NewInt(venuePrice)))
}

// mintTo funds an address with tokens, standing in for money arriving from
// outside.
func (h *harness) mintTo(account common.Address, amount *big.Int) {
	h.t.Helper()
	to := h.addr["MockUSDT0"]
	h.sendTx(&to, mustPack(h.t, tokenABI, "mint", account, amount), nil)
}

// fund gives an address native currency, which is what onboarding asks a new
// participant to do once.
func (h *harness) fund(account common.Address, wei *big.Int) {
	h.t.Helper()
	h.sendTx(&account, nil, wei)
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
	// Setup transactions land immediately; the stories control everything after
	// that with mine and finalise.
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

// transferFrom sends tokens from an arbitrary key, standing in for an exchange
// or a wallet paying money in.
func (h *harness) transferFrom(keyHex string, to common.Address, amount *big.Int) {
	h.t.Helper()
	ctx := context.Background()
	key := mustKey(h.t, keyHex)
	from := crypto.PubkeyToAddress(key.PublicKey)

	nonce, err := h.client.PendingNonceAt(ctx, from)
	if err != nil {
		h.t.Fatal(err)
	}
	gasPrice, err := h.client.SuggestGasPrice(ctx)
	if err != nil {
		h.t.Fatal(err)
	}
	token := h.addr["MockUSDT0"]
	tx := types.NewTx(&types.LegacyTx{
		Nonce: nonce, To: &token, Gas: 200_000,
		GasPrice: new(big.Int).Mul(gasPrice, big.NewInt(2)),
		Data:     mustPack(h.t, tokenABI, "transfer", to, amount),
	})
	signed, err := types.SignTx(tx, types.NewEIP155Signer(h.chainID), key)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := h.client.SendTransaction(ctx, signed); err != nil {
		h.t.Fatalf("send transfer: %v", err)
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

func (h *harness) tokenBalance(account common.Address) *big.Int {
	return h.callUint(h.addr["MockUSDT0"], tokenABI, "balanceOf", account)
}

// allowance is what the venue may still take. A refill authorises at most its
// own input bound, and the next refill replaces whatever is left.
func (h *harness) allowance(owner common.Address) *big.Int {
	return h.callUint(h.addr["MockUSDT0"], tokenABI, "allowance", owner, h.addr["MockRouter"])
}

func (h *harness) reserve(account common.Address) *big.Int {
	h.t.Helper()
	balance, err := h.client.BalanceAt(context.Background(), account, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	return balance
}

func (h *harness) nonce(account common.Address) uint64 {
	h.t.Helper()
	n, err := h.client.NonceAt(context.Background(), account, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	return n
}

// transfers counts the token transfers between two addresses, so a story can
// assert that money moved exactly once.
func (h *harness) transfers(from, to common.Address) int {
	h.t.Helper()
	logs, err := h.client.FilterLogs(context.Background(), ethereum.FilterQuery{
		FromBlock: big.NewInt(0),
		Addresses: []common.Address{h.addr["MockUSDT0"]},
		Topics: [][]common.Hash{
			{crypto.Keccak256Hash([]byte("Transfer(address,address,uint256)"))},
			{common.BytesToHash(from.Bytes())},
			{common.BytesToHash(to.Bytes())},
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
		"name":      "local",
		"chainId":   h.chainID.Uint64(),
		"rpc":       h.rpcURL,
		"token":     h.addr["MockUSDT0"].Hex(),
		"decimals":  6,
		"finality":  "finalized",
		"fromBlock": 0,
		"venue": map[string]any{
			"router":  h.addr["MockRouter"].Hex(),
			"quoter":  h.addr["MockRouter"].Hex(),
			"weth":    wethPlaceholder.Hex(),
			"feeTier": feeTier,
		},
		"gas": map[string]any{
			"min":         reserveMin,
			"max":         reserveMax,
			"slippageBps": slippageBps,
			"feeBound":    reserveFeeBound,
		},
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

// pay runs a payment and reports what happened: whether it was submitted, and
// whether the account had to buy gas first.
type outcome struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	Note      string `json:"note"`
	Refill    string `json:"refill"`
	Tx        string `json:"tx"`
	Submitted bool   `json:"submitted"`
}

func (p *participant) pay(kind, id string, to common.Address, amount string, extra ...string) outcome {
	p.h.t.Helper()
	args := append([]string{"-json"}, extra...)
	args = append(args, kind, id, to.Hex(), amount)
	out := p.mustRun(args...)
	var got outcome
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		p.h.t.Fatalf("output %q: %v", out, err)
	}
	return got
}

// deposits lists the finalized incoming transfers the account has recorded.
func (p *participant) deposits() []struct {
	Tx       string `json:"tx"`
	LogIndex uint   `json:"logIndex"`
	From     string `json:"from"`
	Amount   string `json:"amount"`
} {
	p.h.t.Helper()
	var reply struct {
		Deposits []struct {
			Tx       string `json:"tx"`
			LogIndex uint   `json:"logIndex"`
			From     string `json:"from"`
			Amount   string `json:"amount"`
		} `json:"deposits"`
	}
	out := p.mustRun("-json", "deposits")
	if err := json.Unmarshal([]byte(out), &reply); err != nil {
		p.h.t.Fatalf("deposits output %q: %v", out, err)
	}
	return reply.Deposits
}

// --- shared ABIs ---

var tokenABI = mustABI(`[
      {"type":"function","name":"mint","inputs":[{"name":"to","type":"address"},{"name":"amount","type":"uint256"}]},
      {"type":"function","name":"transfer","inputs":[{"name":"to","type":"address"},{"name":"amount","type":"uint256"}],"outputs":[{"type":"bool"}]},
      {"type":"function","name":"allowance","inputs":[{"name":"owner","type":"address"},{"name":"spender","type":"address"}],"outputs":[{"type":"uint256"}],"stateMutability":"view"},
      {"type":"function","name":"balanceOf","inputs":[{"name":"account","type":"address"}],"outputs":[{"type":"uint256"}],"stateMutability":"view"}]`)

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

func wordOfInt(v *big.Int) []byte {
	return common.LeftPadBytes(v.Bytes(), 32)
}

func tokens(n int64) *big.Int { return big.NewInt(n * tokenScale) }

func ether(n int64) *big.Int {
	return new(big.Int).Mul(big.NewInt(n), big.NewInt(1_000_000_000_000_000_000))
}

func wei(text string) *big.Int {
	v, ok := new(big.Int).SetString(text, 10)
	if !ok {
		panic("bad wei " + text)
	}
	return v
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
