package erc4337

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

const (
	goldenValidUntil = 1_893_456_000
	goldenMaxGasCost = 1_234_567_890
)

// goldenPaymaster is the address the on-chain vector was taken from.
var goldenPaymaster = common.HexToAddress("0x2e234DAe75C793f67A35089C9d99245E1C58470b")

// The digest must be the one JuiceRailPaymaster.getHash computes; if the two
// ever drift, every sponsored operation would be rejected on chain.
func TestPaymasterDigestMatchesTheContract(t *testing.T) {
	want := common.HexToHash("0xea50e5cdc8f8cf0ce8a0a8d31058a37011f823937c5d53a0de1219bd2582efa8")

	got := PaymasterDigest(goldenOp(), goldenPaymaster, goldenEntryPoint, goldenChainID,
		goldenValidUntil, big.NewInt(goldenMaxGasCost))
	if got != want {
		t.Fatalf("paymaster digest = %s, want %s", got, want)
	}
}

// The authorisation is bound to the operation, the domain and its own limits,
// so it cannot be lifted onto anything else.
func TestPaymasterDigestBindsAuthorisationAndDomain(t *testing.T) {
	base := PaymasterDigest(goldenOp(), goldenPaymaster, goldenEntryPoint, goldenChainID,
		goldenValidUntil, big.NewInt(goldenMaxGasCost))

	changed := map[string]common.Hash{
		"paymaster": PaymasterDigest(goldenOp(), common.BigToAddress(big.NewInt(9)), goldenEntryPoint,
			goldenChainID, goldenValidUntil, big.NewInt(goldenMaxGasCost)),
		"entryPoint": PaymasterDigest(goldenOp(), goldenPaymaster, common.BigToAddress(big.NewInt(9)),
			goldenChainID, goldenValidUntil, big.NewInt(goldenMaxGasCost)),
		"chain": PaymasterDigest(goldenOp(), goldenPaymaster, goldenEntryPoint,
			big.NewInt(42161), goldenValidUntil, big.NewInt(goldenMaxGasCost)),
		"expiry": PaymasterDigest(goldenOp(), goldenPaymaster, goldenEntryPoint,
			goldenChainID, goldenValidUntil+1, big.NewInt(goldenMaxGasCost)),
		"ceiling": PaymasterDigest(goldenOp(), goldenPaymaster, goldenEntryPoint,
			goldenChainID, goldenValidUntil, big.NewInt(goldenMaxGasCost+1)),
	}
	for name, got := range changed {
		if got == base {
			t.Errorf("%s must be bound into the digest", name)
		}
	}

	// The call data is what makes the operation the one being sponsored.
	other := goldenOp()
	other.CallData = []byte{0x99}
	if PaymasterDigest(other, goldenPaymaster, goldenEntryPoint, goldenChainID,
		goldenValidUntil, big.NewInt(goldenMaxGasCost)) == base {
		t.Error("the call data must be bound into the digest")
	}
}

// The paymaster's own signature is excluded, which is what lets it sign first
// and the account sign afterwards over the complete operation.
func TestPaymasterDigestExcludesItsOwnSignature(t *testing.T) {
	base := PaymasterDigest(goldenOp(), goldenPaymaster, goldenEntryPoint, goldenChainID,
		goldenValidUntil, big.NewInt(goldenMaxGasCost))

	signed := goldenOp()
	signed.PaymasterData = []byte("a completely different tail")
	if PaymasterDigest(signed, goldenPaymaster, goldenEntryPoint, goldenChainID,
		goldenValidUntil, big.NewInt(goldenMaxGasCost)) != base {
		t.Error("paymaster data must not be covered by its own digest")
	}

	withAccountSignature := goldenOp()
	withAccountSignature.Signature = []byte("account signature")
	if PaymasterDigest(withAccountSignature, goldenPaymaster, goldenEntryPoint, goldenChainID,
		goldenValidUntil, big.NewInt(goldenMaxGasCost)) != base {
		t.Error("the account signature must not be covered")
	}
}

func TestSignPaymasterDataLayout(t *testing.T) {
	key, err := crypto.ToECDSA(common.BigToHash(big.NewInt(3)).Bytes())
	if err != nil {
		t.Fatal(err)
	}
	op := goldenOp()
	maxGasCost := big.NewInt(goldenMaxGasCost)

	if err := SignPaymasterData(op, goldenPaymaster, goldenEntryPoint, goldenChainID,
		goldenValidUntil, maxGasCost, key); err != nil {
		t.Fatal(err)
	}
	if len(op.PaymasterData) != PaymasterDataLen {
		t.Fatalf("paymaster data = %d bytes, want %d", len(op.PaymasterData), PaymasterDataLen)
	}
	if op.Paymaster == nil || *op.Paymaster != goldenPaymaster {
		t.Fatal("the paymaster must be attached to the operation")
	}
	if got := new(big.Int).SetBytes(op.PaymasterData[0:6]).Uint64(); got != goldenValidUntil {
		t.Errorf("expiry = %d, want %d", got, goldenValidUntil)
	}
	if got := new(big.Int).SetBytes(op.PaymasterData[6:38]); got.Cmp(maxGasCost) != 0 {
		t.Errorf("ceiling = %s, want %s", got, maxGasCost)
	}

	// The signature must recover to the signer under the prefixed digest the
	// contract verifies.
	sig := append([]byte(nil), op.PaymasterData[38:]...)
	if v := sig[64]; v != 27 && v != 28 {
		t.Fatalf("recovery id = %d, want 27 or 28", v)
	}
	sig[64] -= 27
	digest := PaymasterDigest(op, goldenPaymaster, goldenEntryPoint, goldenChainID, goldenValidUntil, maxGasCost)
	pub, err := crypto.SigToPub(accounts.TextHash(digest.Bytes()), sig)
	if err != nil {
		t.Fatal(err)
	}
	if crypto.PubkeyToAddress(*pub) != crypto.PubkeyToAddress(key.PublicKey) {
		t.Fatal("the authorisation must recover to the paymaster signer")
	}
}

// Signing must not disturb the gas limits the digest was taken over.
func TestSignPaymasterDataLeavesTheOperationOtherwiseIntact(t *testing.T) {
	key, err := crypto.ToECDSA(common.BigToHash(big.NewInt(4)).Bytes())
	if err != nil {
		t.Fatal(err)
	}
	op := goldenOp()
	before := op.Hash(goldenEntryPoint, goldenChainID)

	if err := SignPaymasterData(op, goldenPaymaster, goldenEntryPoint, goldenChainID,
		goldenValidUntil, big.NewInt(goldenMaxGasCost), key); err != nil {
		t.Fatal(err)
	}
	if op.Hash(goldenEntryPoint, goldenChainID) == before {
		t.Fatal("attaching sponsorship must change the operation hash")
	}
	if op.PaymasterVerificationGasLimit.Cmp(big.NewInt(100_000)) != 0 ||
		op.PaymasterPostOpGasLimit.Cmp(big.NewInt(1)) != 0 {
		t.Fatal("the paymaster gas limits must be preserved")
	}
}
