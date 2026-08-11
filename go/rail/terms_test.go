package rail

import (
	"encoding/json"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func TestKindsRoundTripByName(t *testing.T) {
	for _, kind := range []Kind{KindDeposit, KindTransfer, KindWithdraw} {
		got, err := ParseKind(kind.String())
		if err != nil || got != kind {
			t.Fatalf("%s round trips to %v (%v)", kind, got, err)
		}
	}
	if _, err := ParseKind("settle"); err == nil {
		t.Fatal("an unknown kind must be refused, not guessed at")
	}
}

func TestParseIDDemandsThirtyTwoBytes(t *testing.T) {
	id := testID(7)
	got, err := ParseID(id.String())
	if err != nil || got != id {
		t.Fatalf("round trip gave %v (%v)", got, err)
	}
	if _, err := ParseID("0x01"); err == nil {
		t.Fatal("a short identifier must be refused")
	}
	if _, err := ParseID("nonsense"); err == nil {
		t.Fatal("a non-hex identifier must be refused")
	}
}

func TestTermsValidation(t *testing.T) {
	good := testTerms(KindTransfer, addr(1), addr(2), 10)
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, terms := range map[string]Terms{
		"unknown kind": {Kind: 9, Account: addr(1), Party: addr(2), Amount: big.NewInt(1)},
		"no amount":    {Kind: KindTransfer, Account: addr(1), Party: addr(2)},
		"negative":     {Kind: KindTransfer, Account: addr(1), Party: addr(2), Amount: big.NewInt(-1)},
		"no account":   {Kind: KindTransfer, Party: addr(2), Amount: big.NewInt(1)},
		"no party":     {Kind: KindTransfer, Account: addr(1), Amount: big.NewInt(1)},
	} {
		if err := terms.Validate(); err == nil {
			t.Errorf("%s: must be refused", name)
		}
	}
}

func TestTermsEqualityIsExact(t *testing.T) {
	base := testTerms(KindTransfer, addr(1), addr(2), 10)
	if !base.Equal(testTerms(KindTransfer, addr(1), addr(2), 10)) {
		t.Fatal("identical terms differ")
	}
	for name, other := range map[string]Terms{
		"kind":    testTerms(KindWithdraw, addr(1), addr(2), 10),
		"account": testTerms(KindTransfer, addr(9), addr(2), 10),
		"party":   testTerms(KindTransfer, addr(1), addr(9), 10),
		"amount":  testTerms(KindTransfer, addr(1), addr(2), 11),
	} {
		if base.Equal(other) {
			t.Errorf("terms differing in %s compare equal", name)
		}
	}
	if base.Equal(Terms{Kind: KindTransfer, Account: addr(1), Party: addr(2)}) {
		t.Error("terms without an amount compare equal")
	}
}

func TestVariantValidation(t *testing.T) {
	good := testVariant(KindTransfer, addr(1), addr(2), 10, 1, addr(3), 100)
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	deposit := testVariant(KindDeposit, addr(1), addr(2), 10, 1, addr(3), 100)
	if err := deposit.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Variant){
		"no relayer":            func(v *Variant) { v.Relayer = common.Address{} },
		"no deadline":           func(v *Variant) { v.ValidBefore = 0 },
		"negative fee":          func(v *Variant) { v.Fee = big.NewInt(-1) },
		"no fee":                func(v *Variant) { v.Fee = nil },
		"short signature":       func(v *Variant) { v.TermsSig = make([]byte, 64) },
		"authorisation misused": func(v *Variant) { v.AuthSig = make([]byte, 65) },
	} {
		v := testVariant(KindTransfer, addr(1), addr(2), 10, 1, addr(3), 100)
		mutate(&v)
		if err := v.Validate(); err == nil {
			t.Errorf("%s: must be refused", name)
		}
	}
	// A deposit without its token authorisation cannot pull anything.
	d := testVariant(KindDeposit, addr(1), addr(2), 10, 1, addr(3), 100)
	d.AuthSig = nil
	if err := d.Validate(); err == nil {
		t.Error("a deposit without a token authorisation must be refused")
	}
}

func TestVariantTotalIsAmountPlusFee(t *testing.T) {
	v := testVariant(KindTransfer, addr(1), addr(2), 10, 3, addr(3), 100)
	if v.Total().Int64() != 13 {
		t.Fatalf("total %s, want 13", v.Total())
	}
	if v.Ref() != (Ref{Account: addr(1), ID: testID(1)}) {
		t.Fatalf("ref %v", v.Ref())
	}
}

func TestVariantsRoundTripThroughTheWire(t *testing.T) {
	for _, kind := range []Kind{KindDeposit, KindTransfer, KindWithdraw} {
		v := testVariant(kind, addr(1), addr(2), 1_000_000, 2_500, addr(3), 1893456000)
		for i := range v.TermsSig {
			v.TermsSig[i] = byte(i)
		}
		blob, err := EncodeVariant(testDomain(), v)
		if err != nil {
			t.Fatal(err)
		}
		env, err := DecodeVariant(blob)
		if err != nil {
			t.Fatal(err)
		}
		if env.ChainID.Cmp(testDomain().ChainID) != 0 || env.Rail != testDomain().Rail {
			t.Fatalf("the envelope lost its domain: %+v", env)
		}
		got := env.Variant
		if !got.Terms.Equal(v.Terms) || got.ID != v.ID || got.Fee.Cmp(v.Fee) != 0 ||
			got.Relayer != v.Relayer || got.ValidBefore != v.ValidBefore ||
			string(got.TermsSig) != string(v.TermsSig) || string(got.AuthSig) != string(v.AuthSig) {
			t.Fatalf("%s round trip changed the variant:\n got %+v\nwant %+v", kind, got, v)
		}

		// The durable record carries the same variant without the domain,
		// which the store it lives in already fixes.
		record, err := MarshalVariant(v)
		if err != nil {
			t.Fatal(err)
		}
		back, err := UnmarshalVariant(record)
		if err != nil {
			t.Fatal(err)
		}
		if !back.Terms.Equal(v.Terms) || back.ValidBefore != v.ValidBefore {
			t.Fatalf("the durable record changed the variant: %+v", back)
		}
	}
}

func TestDecodeVariantRefusesNonsense(t *testing.T) {
	v := testVariant(KindTransfer, addr(1), addr(2), 10, 1, addr(3), 100)
	blob, err := EncodeVariant(testDomain(), v)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(blob, &fields); err != nil {
		t.Fatal(err)
	}

	for name, mutate := range map[string]func(map[string]any){
		"bad chain id":  func(m map[string]any) { m["chainId"] = "not a number" },
		"bad kind":      func(m map[string]any) { m["kind"] = "settle" },
		"bad amount":    func(m map[string]any) { m["amount"] = "1.5" },
		"bad fee":       func(m map[string]any) { m["fee"] = "" },
		"bad signature": func(m map[string]any) { m["termsSig"] = "0xzz" },
		"short id":      func(m map[string]any) { m["id"] = "0x01" },
		"unsigned":      func(m map[string]any) { m["termsSig"] = "0x00" },
	} {
		copied := map[string]any{}
		for k, val := range fields {
			copied[k] = val
		}
		mutate(copied)
		raw, err := json.Marshal(copied)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := DecodeVariant(raw); err == nil {
			t.Errorf("%s: must be refused", name)
		}
	}
	if _, err := DecodeVariant([]byte("{")); err == nil {
		t.Error("malformed JSON must be refused")
	}
}

// The wire form is what a relayer reads, so its field names are part of the
// interface.
func TestWireFormatIsStable(t *testing.T) {
	v := testVariant(KindTransfer, addr(1), addr(2), 10, 1, addr(3), 100)
	blob, err := EncodeVariant(testDomain(), v)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{
		"chainId", "rail", "kind", "id", "account", "party",
		"amount", "fee", "relayer", "validBefore", "termsSig",
	} {
		if !strings.Contains(string(blob), `"`+field+`"`) {
			t.Errorf("the wire form lost %q:\n%s", field, blob)
		}
	}
	if strings.Contains(string(blob), "authSig") {
		t.Error("only a deposit carries a token authorisation")
	}
}
