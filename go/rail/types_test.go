package rail

import (
	"errors"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func TestKindsRoundTripByName(t *testing.T) {
	for _, kind := range []Kind{KindTransfer, KindWithdraw, KindRefill} {
		got, err := ParseKind(kind.String())
		if err != nil || got != kind {
			t.Fatalf("%s parsed back as %s (%v)", kind, got, err)
		}
	}
	if _, err := ParseKind("deposit"); !errors.Is(err, ErrBadInput) {
		t.Fatal("a deposit is not an operation: nothing is sent to receive one")
	}
}

func TestIdentifiersAreThirtyTwoBytes(t *testing.T) {
	text := "0x" + strings.Repeat("ab", 32)
	got, err := ParseID(text)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.String() != text {
		t.Fatalf("identifier round-tripped as %s", got)
	}
	for _, bad := range []string{"", "0x", "0xzz", "0x" + strings.Repeat("ab", 31)} {
		if _, err := ParseID(bad); err == nil {
			t.Fatalf("%q was accepted as an identifier", bad)
		}
	}
}

func TestIntentValidation(t *testing.T) {
	good := Intent{
		ID: ID{1}, Kind: KindTransfer, To: bob,
		Amount: big.NewInt(5), Calldata: []byte{1}, Nonce: 0,
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("a well-formed intent was refused: %v", err)
	}
	for _, tc := range []struct {
		name  string
		spoil func(*Intent)
	}{
		{"no kind", func(in *Intent) { in.Kind = 0 }},
		{"no destination", func(in *Intent) { in.To = common.Address{} }},
		{"zero amount", func(in *Intent) { in.Amount = big.NewInt(0) }},
		{"negative amount", func(in *Intent) { in.Amount = big.NewInt(-1) }},
		{"no calldata", func(in *Intent) { in.Calldata = nil }},
		{"refill buying nothing", func(in *Intent) { in.Kind = KindRefill; in.Delta = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := good
			tc.spoil(&in)
			if err := in.Validate(); err == nil {
				t.Fatal("a malformed intent was accepted")
			}
		})
	}
}

func TestSameTermsIgnoresNothingThatMatters(t *testing.T) {
	base := Intent{ID: ID{1}, Kind: KindTransfer, To: bob, Amount: big.NewInt(5), Nonce: 2}
	same := base
	same.Calldata = []byte{9}
	same.FromBlock = 99
	if !base.SameTerms(same) {
		t.Fatal("the same operation was seen as two")
	}
	for _, spoil := range []func(*Intent){
		func(in *Intent) { in.ID = ID{2} },
		func(in *Intent) { in.Kind = KindWithdraw },
		func(in *Intent) { in.To = exchange },
		func(in *Intent) { in.Amount = big.NewInt(6) },
		func(in *Intent) { in.Nonce = 3 },
	} {
		other := base
		spoil(&other)
		if base.SameTerms(other) {
			t.Fatal("two different operations were seen as one")
		}
	}
}

func TestDomainKeyNamesTheChainAndTheToken(t *testing.T) {
	token := common.HexToAddress("0xAbC0000000000000000000000000000000000001")
	key := DomainKey(big.NewInt(42161), token)
	if key != "42161:"+strings.ToLower(token.Hex()) {
		t.Fatalf("domain key is %q", key)
	}
	// Case is not identity: the same token written two ways is one domain.
	if DomainKey(big.NewInt(42161), common.HexToAddress(strings.ToLower(token.Hex()))) != key {
		t.Fatal("the same domain produced two keys")
	}
	if DomainKey(big.NewInt(1), token) == key {
		t.Fatal("two chains produced one key")
	}
}

func TestGasLimitsFallBackToTheDefaults(t *testing.T) {
	payment, swap := GasPolicy{}.limits()
	if payment != DefaultPaymentGas || swap != DefaultSwapGas {
		t.Fatalf("unset limits are %d and %d", payment, swap)
	}
	payment, swap = GasPolicy{PaymentGas: 1, SwapGas: 2}.limits()
	if payment != 1 || swap != 2 {
		t.Fatalf("configured limits came back as %d and %d", payment, swap)
	}
}

func TestGasPolicyMustBeAbleToRefillItself(t *testing.T) {
	sound := GasPolicy{
		Min: big.NewInt(20), Max: big.NewInt(50), FeeBound: big.NewInt(10), SlippageBps: 50,
	}
	if err := sound.Validate(); err != nil {
		t.Fatalf("a sound policy was refused: %v", err)
	}
	for _, tc := range []struct {
		name  string
		spoil func(*GasPolicy)
	}{
		{"no minimum", func(p *GasPolicy) { p.Min = nil }},
		{"maximum at the minimum", func(p *GasPolicy) { p.Max = big.NewInt(20) }},
		{"no fee bound", func(p *GasPolicy) { p.FeeBound = nil }},
		{"minimum below the fee bound", func(p *GasPolicy) { p.FeeBound = big.NewInt(21) }},
		{"slippage above the whole price", func(p *GasPolicy) { p.SlippageBps = 10_001 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := sound
			tc.spoil(&p)
			if err := p.Validate(); err == nil {
				t.Fatal("an unworkable policy was accepted")
			}
		})
	}
}
