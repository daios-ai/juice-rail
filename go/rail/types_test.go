package rail

import (
	"errors"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
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

func TestAmountsAreExactIntegers(t *testing.T) {
	d := testDomain() // six decimals, like the stablecoins this serves
	for _, tc := range []struct {
		text string
		want string
	}{
		{"1250", "1250000000"},
		{"1250.00", "1250000000"},
		{"0.000001", "1"},
		{".5", "500000"},
		{"0", "0"},
	} {
		got, err := d.ParseAmount(tc.text)
		if err != nil {
			t.Fatalf("%q: %v", tc.text, err)
		}
		if got.String() != tc.want {
			t.Fatalf("%q is %s base units, want %s", tc.text, got, tc.want)
		}
	}
	for _, bad := range []string{"", "  ", "1.2345678", "one", "1.2.3", "-5", "1e6"} {
		if _, err := d.ParseAmount(bad); !errors.Is(err, ErrBadInput) {
			t.Fatalf("%q was accepted as an amount", bad)
		}
	}
}

func TestAmountsAreShownTheWayPeopleWriteThem(t *testing.T) {
	d := testDomain()
	for _, tc := range []struct {
		units string
		want  string
	}{
		{"1250000000", "1250.00"},
		{"1250500000", "1250.50"},
		{"1", "0.000001"},
		{"0", "0.00"},
	} {
		v, _ := new(big.Int).SetString(tc.units, 10)
		if got := d.FormatAmount(v); got != tc.want {
			t.Fatalf("%s base units shows as %s, want %s", tc.units, got, tc.want)
		}
	}
	// The native currency needs no configuration: every EVM chain uses 18.
	for _, tc := range []struct {
		wei  string
		want string
	}{
		{"50000000000000000", "0.05"},
		{"68280001500000", "0.0000682800015"},
		{"0", "0.00"},
	} {
		v, _ := new(big.Int).SetString(tc.wei, 10)
		if got := FormatNative(v); got != tc.want {
			t.Fatalf("%s wei shows as %s, want %s", tc.wei, got, tc.want)
		}
	}
}

func TestAmountsRoundTrip(t *testing.T) {
	d := testDomain()
	for _, text := range []string{"0.00", "1.00", "12.34", "999999.999999"} {
		units, err := d.ParseAmount(text)
		if err != nil {
			t.Fatalf("%q: %v", text, err)
		}
		if got := d.FormatAmount(units); got != text {
			t.Fatalf("%q came back as %q", text, got)
		}
	}
}

func TestTheFundingChecklistNamesBothAssetsAndTheAddress(t *testing.T) {
	d := testDomain()
	got := d.FundingChecklist(bob)
	for _, want := range []string{
		bob.Hex(),
		"send the stablecoin",
		"native currency",
		FormatNative(d.Gas.Min), // the minimum is what makes an account operational
		"wait until the chain settles it",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("the checklist does not mention %q:\n%s", want, got)
		}
	}
}

func TestADomainWithoutUnitsIsRefused(t *testing.T) {
	d := testDomain()
	d.Decimals = 0
	if err := d.Validate(); err == nil {
		t.Fatal("a domain that does not know its own units was accepted")
	}
}

func TestAddressesMustBeWholeAddresses(t *testing.T) {
	if _, err := ParseAddress("0x1111111111111111111111111111111111111111", "recipient"); err != nil {
		t.Fatalf("a whole address was refused: %v", err)
	}
	for _, bad := range []string{"0x1111", "1111111111111111111111111111111111111111x", "", "bob"} {
		if _, err := ParseAddress(bad, "recipient"); err == nil {
			t.Fatalf("%q was accepted as an address", bad)
		}
	}
}

func TestKeysAreReadWithOrWithoutThePrefix(t *testing.T) {
	const hex = "4c0883a69102937d6231471b5dbb6204fe5129617082792ae468d01a3f362318"
	bare, err := ParseKey(hex, "account key")
	if err != nil {
		t.Fatalf("bare key: %v", err)
	}
	prefixed, err := ParseKey("0x"+hex, "account key")
	if err != nil {
		t.Fatalf("prefixed key: %v", err)
	}
	if crypto.PubkeyToAddress(bare.PublicKey) != crypto.PubkeyToAddress(prefixed.PublicKey) {
		t.Fatal("the same key read two ways gave two accounts")
	}
	for _, bad := range []string{"", "zz", hex[:10]} {
		if _, err := ParseKey(bad, "account key"); err == nil {
			t.Fatalf("%q was accepted as a key", bad)
		}
	}
}
