package rail

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// goldenVariant is pinned identically in contracts/test/JuiceRail.t.sol. If the
// encoding drifts on either side, both suites fail rather than one silently
// signing what the other will not accept.
func goldenVariant(kind Kind) Variant {
	var id ID
	id[31] = 1
	return Variant{
		Terms: Terms{
			Kind:    kind,
			Account: common.HexToAddress("0x1111111111111111111111111111111111111111"),
			Party:   common.HexToAddress("0x2222222222222222222222222222222222222222"),
			Amount:  big.NewInt(1000000),
		},
		ID:          id,
		Fee:         big.NewInt(2500),
		Relayer:     common.HexToAddress("0x3333333333333333333333333333333333333333"),
		ValidBefore: 1893456000,
	}
}

const (
	goldenDomainSeparator = "0xade9308a35ed9af153baebcacd628f248895fc8f1867fc9fd97ce0eae27c8b75"
	goldenDepositHash     = "0xa5287ef541fb6292d106a3f21b644a59f8ec509ad3732353a65f16a3a9f96ba7"
	goldenTransferHash    = "0xf80f55d5187157c5cfe78049bb8b41af257966d1fa196f4dd848633ee0dd91f5"
	goldenWithdrawHash    = "0xaf98a560cc4df4da8606057478c0753cc1727069e9a028d0fb4326d47e575b01"
)

func TestGoldenVectorsMatchTheContract(t *testing.T) {
	got := DomainSeparator(big.NewInt(31337), common.HexToAddress("0x00000000000000000000000000000000000000A1"))
	if got.Hex() != goldenDomainSeparator {
		t.Fatalf("domain separator %s, want %s", got.Hex(), goldenDomainSeparator)
	}
	for kind, want := range map[Kind]string{
		KindDeposit:  goldenDepositHash,
		KindTransfer: goldenTransferHash,
		KindWithdraw: goldenWithdrawHash,
	} {
		h, err := TermsHash(goldenVariant(kind))
		if err != nil {
			t.Fatal(err)
		}
		if h.Hex() != want {
			t.Errorf("%s terms hash %s, want %s", kind, h.Hex(), want)
		}
	}
}

// The kinds share a field layout but not a type hash, so one identifier can
// never mean two kinds.
func TestTermsHashSeparatesTheKinds(t *testing.T) {
	seen := map[common.Hash]Kind{}
	for _, kind := range []Kind{KindDeposit, KindTransfer, KindWithdraw} {
		h, err := TermsHash(goldenVariant(kind))
		if err != nil {
			t.Fatal(err)
		}
		if other, clash := seen[h]; clash {
			t.Fatalf("%s and %s hash alike", kind, other)
		}
		seen[h] = kind
	}
	if _, err := TermsHash(Variant{Terms: Terms{Kind: 9}}); err == nil {
		t.Fatal("an unknown kind must not hash")
	}
}

// Every field is in the hash, so no field can be changed after signing.
func TestTermsHashCoversEveryField(t *testing.T) {
	base, err := TermsHash(goldenVariant(KindTransfer))
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Variant){
		"id":          func(v *Variant) { v.ID[0] = 9 },
		"account":     func(v *Variant) { v.Account = addr(9) },
		"party":       func(v *Variant) { v.Party = addr(9) },
		"amount":      func(v *Variant) { v.Amount = big.NewInt(999) },
		"fee":         func(v *Variant) { v.Fee = big.NewInt(999) },
		"relayer":     func(v *Variant) { v.Relayer = addr(9) },
		"validBefore": func(v *Variant) { v.ValidBefore++ },
	} {
		v := goldenVariant(KindTransfer)
		mutate(&v)
		h, err := TermsHash(v)
		if err != nil {
			t.Fatal(err)
		}
		if h == base {
			t.Errorf("%s is not covered by the terms hash", name)
		}
	}
}

func TestDomainSeparatorBindsChainAndDeployment(t *testing.T) {
	one := DomainSeparator(big.NewInt(42161), addr(1))
	otherChain := DomainSeparator(big.NewInt(421614), addr(1))
	otherRail := DomainSeparator(big.NewInt(42161), addr(2))
	if one == otherChain || one == otherRail {
		t.Fatal("terms signed for one domain would execute on another")
	}
}

func TestSignaturesRoundTrip(t *testing.T) {
	key := testKey(t, 3)
	want := crypto.PubkeyToAddress(key.PublicKey)
	d := common.HexToHash("0xabc")

	sig, err := signDigest(key, d)
	if err != nil {
		t.Fatal(err)
	}
	if len(sig) != 65 {
		t.Fatalf("signature is %d bytes, want 65", len(sig))
	}
	if sig[64] != 27 && sig[64] != 28 {
		t.Fatalf("v is %d, want 27 or 28: the contracts reject anything else", sig[64])
	}
	got, err := RecoverSigner(d, sig)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("recovered %s, want %s", got, want)
	}
	if other, err := RecoverSigner(common.HexToHash("0xabd"), sig); err == nil && other == want {
		t.Fatal("a signature must not recover the signer for another digest")
	}
	if _, err := RecoverSigner(d, sig[:64]); err == nil {
		t.Fatal("a short signature must be refused")
	}
}

func TestAuthDigestBindsThePayeeAndTheNonce(t *testing.T) {
	tokenDomain := common.HexToHash("0xd0d0")
	from, to := addr(1), addr(2)
	nonce := common.HexToHash("0x01")

	base, err := authDigest(tokenDomain, from, to, big.NewInt(100), 500, nonce)
	if err != nil {
		t.Fatal(err)
	}
	for name, other := range map[string]func() (common.Hash, error){
		"another token": func() (common.Hash, error) {
			return authDigest(common.HexToHash("0xd0d1"), from, to, big.NewInt(100), 500, nonce)
		},
		"another payee": func() (common.Hash, error) {
			return authDigest(tokenDomain, from, addr(3), big.NewInt(100), 500, nonce)
		},
		"another value": func() (common.Hash, error) {
			return authDigest(tokenDomain, from, to, big.NewInt(101), 500, nonce)
		},
		"another deadline": func() (common.Hash, error) {
			return authDigest(tokenDomain, from, to, big.NewInt(100), 501, nonce)
		},
		"another operation": func() (common.Hash, error) {
			return authDigest(tokenDomain, from, to, big.NewInt(100), 500, common.HexToHash("0x02"))
		},
	} {
		got, err := other()
		if err != nil {
			t.Fatal(err)
		}
		if got == base {
			t.Errorf("the authorisation does not bind %s", name)
		}
	}
}
