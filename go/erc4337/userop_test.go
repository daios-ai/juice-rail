package erc4337

import (
	"encoding/json"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// goldenOp is the operation the on-chain vectors were taken from.
func goldenOp() *UserOperation {
	paymaster := common.BigToAddress(big.NewInt(0xBEEF))
	return &UserOperation{
		Sender:                        common.BigToAddress(big.NewInt(0xA11CE)),
		Nonce:                         big.NewInt(7),
		CallData:                      []byte{0x12, 0x34},
		VerificationGasLimit:          big.NewInt(200_000),
		CallGasLimit:                  big.NewInt(400_000),
		PreVerificationGas:            big.NewInt(100_000),
		MaxPriorityFeePerGas:          big.NewInt(1_000_000_000),
		MaxFeePerGas:                  big.NewInt(2_000_000_000),
		Paymaster:                     &paymaster,
		PaymasterVerificationGasLimit: big.NewInt(100_000),
		PaymasterPostOpGasLimit:       big.NewInt(1),
		PaymasterData:                 []byte{0xaa, 0xbb},
	}
}

var (
	goldenEntryPoint = common.HexToAddress("0x5615dEB798BB3E4dFa0139dFa1b3D433Cc23b72f")
	goldenModule     = common.HexToAddress("0x2e234DAe75C793f67A35089C9d99245E1C58470b")
	goldenChainID    = big.NewInt(31337)
)

// The hash must be the one the EntryPoint itself computes; the value below
// came from EntryPoint v0.7.0 via getUserOpHash.
func TestHashMatchesTheEntryPoint(t *testing.T) {
	want := common.HexToHash("0xe773b6bd049e9319d210899fb2ea170cac85be8005f9b9b66708f04020ab674f")
	if got := goldenOp().Hash(goldenEntryPoint, goldenChainID); got != want {
		t.Fatalf("user operation hash = %s, want %s", got, want)
	}
}

// The hash covers every field but the signature, so a change anywhere else
// invalidates it.
func TestHashCoversEveryFieldButTheSignature(t *testing.T) {
	base := goldenOp().Hash(goldenEntryPoint, goldenChainID)

	withSignature := goldenOp()
	withSignature.Signature = []byte{1, 2, 3}
	if withSignature.Hash(goldenEntryPoint, goldenChainID) != base {
		t.Error("the signature must not be covered")
	}

	mutate := map[string]func(*UserOperation){
		"sender":        func(o *UserOperation) { o.Sender = common.BigToAddress(big.NewInt(1)) },
		"nonce":         func(o *UserOperation) { o.Nonce = big.NewInt(8) },
		"callData":      func(o *UserOperation) { o.CallData = []byte{0x12, 0x35} },
		"callGas":       func(o *UserOperation) { o.CallGasLimit = big.NewInt(1) },
		"verifyGas":     func(o *UserOperation) { o.VerificationGasLimit = big.NewInt(1) },
		"preVerifyGas":  func(o *UserOperation) { o.PreVerificationGas = big.NewInt(1) },
		"maxFee":        func(o *UserOperation) { o.MaxFeePerGas = big.NewInt(1) },
		"priorityFee":   func(o *UserOperation) { o.MaxPriorityFeePerGas = big.NewInt(1) },
		"paymasterData": func(o *UserOperation) { o.PaymasterData = []byte{0xaa} },
		"factory": func(o *UserOperation) {
			f := common.BigToAddress(big.NewInt(2))
			o.Factory, o.FactoryData = &f, []byte{9}
		},
	}
	for name, apply := range mutate {
		op := goldenOp()
		apply(op)
		if op.Hash(goldenEntryPoint, goldenChainID) == base {
			t.Errorf("%s must change the hash", name)
		}
	}

	// The domain is bound too: the same operation elsewhere is a different one.
	if goldenOp().Hash(common.BigToAddress(big.NewInt(3)), goldenChainID) == base {
		t.Error("entry point must be bound")
	}
	if goldenOp().Hash(goldenEntryPoint, big.NewInt(42161)) == base {
		t.Error("chain id must be bound")
	}
}

func TestPackedFieldLayout(t *testing.T) {
	op := goldenOp()

	gasLimits := op.AccountGasLimits()
	if got := new(big.Int).SetBytes(gasLimits[0:16]); got.Cmp(op.VerificationGasLimit) != 0 {
		t.Errorf("verification gas must occupy the high half, got %s", got)
	}
	if got := new(big.Int).SetBytes(gasLimits[16:32]); got.Cmp(op.CallGasLimit) != 0 {
		t.Errorf("call gas must occupy the low half, got %s", got)
	}

	fees := op.GasFees()
	if got := new(big.Int).SetBytes(fees[0:16]); got.Cmp(op.MaxPriorityFeePerGas) != 0 {
		t.Errorf("priority fee must occupy the high half, got %s", got)
	}
	if got := new(big.Int).SetBytes(fees[16:32]); got.Cmp(op.MaxFeePerGas) != 0 {
		t.Errorf("fee cap must occupy the low half, got %s", got)
	}

	pmd := op.PaymasterAndData()
	if len(pmd) != 52+len(op.PaymasterData) {
		t.Fatalf("paymasterAndData = %d bytes", len(pmd))
	}
	if common.BytesToAddress(pmd[:20]) != *op.Paymaster {
		t.Error("paymaster must lead paymasterAndData")
	}

	empty := &UserOperation{}
	if empty.PaymasterAndData() != nil || empty.InitCode() != nil {
		t.Error("an unsponsored, deployed account carries neither field")
	}
}

func TestInitCodePacksFactoryAndData(t *testing.T) {
	factory := common.BigToAddress(big.NewInt(0xF0))
	op := goldenOp()
	op.Factory, op.FactoryData = &factory, []byte{1, 2, 3}

	code := op.InitCode()
	if len(code) != 23 || common.BytesToAddress(code[:20]) != factory {
		t.Fatalf("init code = %x", code)
	}
}

// The stored attempt is re-presented after a restart, so the round trip must
// preserve the operation exactly.
func TestJSONRoundTripPreservesTheOperation(t *testing.T) {
	op := goldenOp()
	op.Signature = []byte{0xde, 0xad}
	factory := common.BigToAddress(big.NewInt(0xF0))
	op.Factory, op.FactoryData = &factory, []byte{7, 7}

	blob, err := json.Marshal(op)
	if err != nil {
		t.Fatal(err)
	}
	var back UserOperation
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatal(err)
	}
	if back.Hash(goldenEntryPoint, goldenChainID) != op.Hash(goldenEntryPoint, goldenChainID) {
		t.Fatal("round trip changed the operation")
	}
	if string(back.Signature) != string(op.Signature) {
		t.Fatal("round trip lost the signature")
	}
}

// The wire shape is the standard v0.7 one, which is what a bundler expects.
func TestJSONUsesTheStandardWireShape(t *testing.T) {
	blob, err := json.Marshal(goldenOp())
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(blob, &fields); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"sender", "nonce", "callData", "callGasLimit", "verificationGasLimit",
		"preVerificationGas", "maxFeePerGas", "maxPriorityFeePerGas", "signature",
		"paymaster", "paymasterVerificationGasLimit", "paymasterPostOpGasLimit", "paymasterData",
	} {
		if _, ok := fields[name]; !ok {
			t.Errorf("missing field %q", name)
		}
	}
	// An operation for an existing account carries no factory fields at all.
	if _, ok := fields["factory"]; ok {
		t.Error("factory must be omitted when the account exists")
	}
}
