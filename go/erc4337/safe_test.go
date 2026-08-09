package erc4337

import (
	"context"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// The digest must be the one Safe4337Module v0.3.0 itself computes; the value
// below came from the deployed module via getOperationHash.
func TestSafeOpDigestMatchesTheModule(t *testing.T) {
	want := common.HexToHash("0xec75c4da19f717ba689103a1a5a1723934ba0fce71c5e86cd84ad801499417b5")
	contracts := Contracts{EntryPoint: goldenEntryPoint, SafeModule: goldenModule}

	got := SafeOpDigest(goldenOp(), contracts, goldenChainID, 0, 1_893_456_000)
	if got != want {
		t.Fatalf("safe operation digest = %s, want %s", got, want)
	}
}

// The digest binds the module and the chain, so an operation signed for one
// deployment cannot be replayed against another.
func TestSafeOpDigestBindsTheDomain(t *testing.T) {
	contracts := Contracts{EntryPoint: goldenEntryPoint, SafeModule: goldenModule}
	base := SafeOpDigest(goldenOp(), contracts, goldenChainID, 0, 100)

	other := contracts
	other.SafeModule = common.BigToAddress(big.NewInt(0x99))
	if SafeOpDigest(goldenOp(), other, goldenChainID, 0, 100) == base {
		t.Error("the module must be bound")
	}

	other = contracts
	other.EntryPoint = common.BigToAddress(big.NewInt(0x98))
	if SafeOpDigest(goldenOp(), other, goldenChainID, 0, 100) == base {
		t.Error("the entry point must be bound")
	}

	if SafeOpDigest(goldenOp(), contracts, big.NewInt(42161), 0, 100) == base {
		t.Error("the chain must be bound")
	}
	if SafeOpDigest(goldenOp(), contracts, goldenChainID, 0, 101) == base {
		t.Error("the validity window must be bound")
	}
}

func TestSignSafeOpProducesTheModuleEncoding(t *testing.T) {
	key, err := crypto.ToECDSA(common.BigToHash(big.NewInt(1)).Bytes())
	if err != nil {
		t.Fatal(err)
	}
	contracts := Contracts{EntryPoint: goldenEntryPoint, SafeModule: goldenModule}
	op := goldenOp()

	const validUntil = 1_893_456_000
	if err := SignSafeOp(op, contracts, goldenChainID, 0, validUntil, key); err != nil {
		t.Fatal(err)
	}
	if len(op.Signature) != 12+65 {
		t.Fatalf("signature = %d bytes, want 77", len(op.Signature))
	}
	if got := new(big.Int).SetBytes(op.Signature[0:6]).Uint64(); got != 0 {
		t.Errorf("validAfter = %d, want 0", got)
	}
	if got := new(big.Int).SetBytes(op.Signature[6:12]).Uint64(); got != validUntil {
		t.Errorf("validUntil = %d, want %d", got, validUntil)
	}

	// The owner signature must recover to the owner under the module's digest.
	sig := append([]byte(nil), op.Signature[12:]...)
	if v := sig[64]; v != 27 && v != 28 {
		t.Fatalf("recovery id = %d, want 27 or 28", v)
	}
	sig[64] -= 27
	digest := SafeOpDigest(op, contracts, goldenChainID, 0, validUntil)
	pub, err := crypto.SigToPub(digest.Bytes(), sig)
	if err != nil {
		t.Fatal(err)
	}
	if crypto.PubkeyToAddress(*pub) != crypto.PubkeyToAddress(key.PublicKey) {
		t.Fatal("the signature must recover to the account owner")
	}
}

// fakeCaller answers the reads account derivation needs.
type fakeCaller struct {
	creationCode []byte
	code         map[common.Address][]byte
	nonce        *big.Int
}

func (f *fakeCaller) CallContract(_ context.Context, call ethereum.CallMsg, _ *big.Int) ([]byte, error) {
	switch string(call.Data[:4]) {
	case string(proxyFactoryABI.Methods["proxyCreationCode"].ID):
		out := append(word(big.NewInt(32)), word(big.NewInt(int64(len(f.creationCode))))...)
		return append(out, common.RightPadBytes(f.creationCode, 32*((len(f.creationCode)+31)/32))...), nil
	case string(entryPointABI.Methods["getNonce"].ID):
		return word(f.nonce), nil
	}
	return word(new(big.Int)), nil
}

func (f *fakeCaller) CodeAt(_ context.Context, addr common.Address, _ *big.Int) ([]byte, error) {
	return f.code[addr], nil
}

func testContracts() Contracts {
	return Contracts{
		EntryPoint:        common.BigToAddress(big.NewInt(0x2001)),
		SafeSingleton:     common.BigToAddress(big.NewInt(0x2002)),
		SafeProxyFactory:  common.BigToAddress(big.NewInt(0x2003)),
		SafeModule:        common.BigToAddress(big.NewInt(0x2004)),
		SafeModuleSetup:   common.BigToAddress(big.NewInt(0x2005)),
		MultiSendCallOnly: common.BigToAddress(big.NewInt(0x2006)),
	}
}

// The address must be derivable before the account exists, and must follow the
// factory's own CREATE2 rule.
func TestAccountAddressFollowsTheFactoryRule(t *testing.T) {
	caller := &fakeCaller{creationCode: []byte("proxy-creation-code"), code: map[common.Address][]byte{}}
	account := Account{Owner: common.BigToAddress(big.NewInt(1)), Contracts: testContracts()}
	ctx := context.Background()

	got, err := account.Address(ctx, caller)
	if err != nil {
		t.Fatal(err)
	}

	init, err := account.Initializer()
	if err != nil {
		t.Fatal(err)
	}
	var saltNonce, singleton [32]byte
	copy(singleton[12:], account.Contracts.SafeSingleton.Bytes())
	salt := crypto.Keccak256Hash(concat(crypto.Keccak256(init), saltNonce[:]))
	want := crypto.CreateAddress2(
		account.Contracts.SafeProxyFactory, salt,
		crypto.Keccak256(concat(caller.creationCode, singleton[:])),
	)
	if got != want {
		t.Fatalf("address = %s, want %s", got, want)
	}

	deployed, err := account.Deployed(ctx, caller, got)
	if err != nil || deployed {
		t.Fatalf("a counterfactual account must not report as deployed: %v %v", deployed, err)
	}
	caller.code[got] = []byte{0x60}
	if deployed, _ := account.Deployed(ctx, caller, got); !deployed {
		t.Fatal("a deployed account must report as deployed")
	}
}

// A different owner, salt or deployment is a different account.
func TestAccountAddressSeparatesOwnersAndSalts(t *testing.T) {
	caller := &fakeCaller{creationCode: []byte("code"), code: map[common.Address][]byte{}}
	ctx := context.Background()
	base := Account{Owner: common.BigToAddress(big.NewInt(1)), Contracts: testContracts()}

	first, err := base.Address(ctx, caller)
	if err != nil {
		t.Fatal(err)
	}
	again, err := base.Address(ctx, caller)
	if err != nil || again != first {
		t.Fatalf("derivation must be deterministic: %s %s", first, again)
	}

	other := base
	other.Owner = common.BigToAddress(big.NewInt(2))
	if addr, _ := other.Address(ctx, caller); addr == first {
		t.Error("a different owner must give a different account")
	}

	salted := base
	salted.SaltNonce = big.NewInt(1)
	if addr, _ := salted.Address(ctx, caller); addr == first {
		t.Error("a different salt must give a different account")
	}

	moved := base
	moved.Contracts.SafeModule = common.BigToAddress(big.NewInt(0x9999))
	if addr, _ := moved.Address(ctx, caller); addr == first {
		t.Error("a different module must give a different account")
	}
}

func TestFactoryDataDeploysTheInitializedAccount(t *testing.T) {
	account := Account{Owner: common.BigToAddress(big.NewInt(1)), Contracts: testContracts()}

	data, err := account.FactoryData()
	if err != nil {
		t.Fatal(err)
	}
	if string(data[:4]) != string(proxyFactoryABI.Methods["createProxyWithNonce"].ID) {
		t.Fatal("factory data must call createProxyWithNonce")
	}

	init, err := account.Initializer()
	if err != nil {
		t.Fatal(err)
	}
	if string(init[:4]) != string(safeABI.Methods["setup"].ID) {
		t.Fatal("the initializer must be a Safe setup call")
	}
	// The module is installed both as a module and as the fallback handler,
	// which is what makes the account answer the EntryPoint.
	if !contains(init, account.Contracts.SafeModule.Bytes()) {
		t.Fatal("the initializer must reference the module")
	}
	if !contains(init, account.Contracts.SafeModuleSetup.Bytes()) {
		t.Fatal("the initializer must reference the module setup")
	}
}

func TestNonceReadsTheEntryPoint(t *testing.T) {
	caller := &fakeCaller{nonce: big.NewInt(42), code: map[common.Address][]byte{}}
	account := Account{Owner: common.BigToAddress(big.NewInt(1)), Contracts: testContracts()}

	got, err := account.Nonce(context.Background(), caller, account.Owner, big.NewInt(7))
	if err != nil {
		t.Fatal(err)
	}
	if got.Cmp(big.NewInt(42)) != 0 {
		t.Fatalf("nonce = %s, want 42", got)
	}
}

func contains(haystack, needle []byte) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if string(haystack[i:i+len(needle)]) == string(needle) {
			return true
		}
	}
	return false
}
