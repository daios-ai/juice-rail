package rail

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"github.com/daios-ai/juice-rail/go/erc4337"
)

// Golden terms hashes produced by the JuiceRail contract itself (chain id
// 31337, rail 0x5615dEB798BB3E4dFa0139dFa1b3D433Cc23b72f). If the Go
// preimages ever drift from the contract's, these stop matching.
func TestTermsHashMatchesTheContract(t *testing.T) {
	chainID := big.NewInt(31337)
	railAddr := common.HexToAddress("0x5615dEB798BB3E4dFa0139dFa1b3D433Cc23b72f")
	a := common.BigToAddress(big.NewInt(0xA1))
	b := common.BigToAddress(big.NewInt(0xB2))

	for _, tc := range []struct {
		name  string
		terms Terms
		want  common.Hash
	}{
		{
			"deposit",
			Terms{Kind: KindDeposit, Account: a, Amount: big.NewInt(1_000_000)},
			common.HexToHash("0x7f26c1490ab2f0c7a411e7bc52b2b501fafb19746bb4ee415b7b006f3889f8e6"),
		},
		{
			"settle",
			Terms{Kind: KindSettle, Account: a, Party: b, Amount: big.NewInt(40)},
			common.HexToHash("0x43cc9673a84b9ff294a11a73878f4fcc8b748e291707d88d9a7df17a030488e7"),
		},
		{
			"withdraw",
			Terms{Kind: KindWithdraw, Account: a, Party: b, Amount: big.NewInt(25)},
			common.HexToHash("0xbc5f9b264bfdc9e02aea1be61a15f8d716104b71689938e4ba963c049ba1d1a6"),
		},
	} {
		if got := tc.terms.Hash(chainID, railAddr); got != tc.want {
			t.Errorf("%s terms hash = %s, want %s", tc.name, got, tc.want)
		}
	}
}

// The identifier means one intent and one only: every field, the kind and the
// domain included, changes the hash.
func TestTermsHashSeparatesEveryField(t *testing.T) {
	chainID := big.NewInt(31337)
	railAddr := common.BigToAddress(big.NewInt(0x1111))
	base := Terms{Kind: KindSettle, Account: common.BigToAddress(big.NewInt(1)),
		Party: common.BigToAddress(big.NewInt(2)), Amount: big.NewInt(5)}

	seen := map[common.Hash]string{base.Hash(chainID, railAddr): "base"}
	variants := map[string]struct {
		terms   Terms
		chainID *big.Int
		rail    common.Address
	}{
		"kind":     {Terms{KindWithdraw, base.Account, base.Party, base.Amount}, chainID, railAddr},
		"debtor":   {Terms{base.Kind, common.BigToAddress(big.NewInt(9)), base.Party, base.Amount}, chainID, railAddr},
		"creditor": {Terms{base.Kind, base.Account, common.BigToAddress(big.NewInt(9)), base.Amount}, chainID, railAddr},
		"amount":   {Terms{base.Kind, base.Account, base.Party, big.NewInt(6)}, chainID, railAddr},
		"chain":    {base, big.NewInt(42161), railAddr},
		"rail":     {base, chainID, common.BigToAddress(big.NewInt(0x2222))},
	}
	for name, v := range variants {
		h := v.terms.Hash(v.chainID, v.rail)
		if other, clash := seen[h]; clash {
			t.Errorf("%s collides with %s", name, other)
		}
		seen[h] = name
	}
}

// A deposit binds no counterparty, so its preimage is one word shorter. It
// must not be confusable with a settle whose creditor happens to be zero.
func TestDepositAndSettleNeverShareTerms(t *testing.T) {
	chainID := big.NewInt(1)
	railAddr := common.BigToAddress(big.NewInt(3))
	a := common.BigToAddress(big.NewInt(7))

	deposit := Terms{Kind: KindDeposit, Account: a, Amount: big.NewInt(1)}
	settle := Terms{Kind: KindSettle, Account: a, Party: a, Amount: big.NewInt(1)}
	if deposit.Hash(chainID, railAddr) == settle.Hash(chainID, railAddr) {
		t.Fatal("deposit and settle must never hash alike")
	}
}

func TestTermsValidation(t *testing.T) {
	a := common.BigToAddress(big.NewInt(1))
	for name, terms := range map[string]Terms{
		"unknown kind":     {Kind: 9, Account: a, Amount: big.NewInt(1)},
		"nil amount":       {Kind: KindDeposit, Account: a},
		"negative amount":  {Kind: KindDeposit, Account: a, Amount: big.NewInt(-1)},
		"no account":       {Kind: KindDeposit, Amount: big.NewInt(1)},
		"settle no party":  {Kind: KindSettle, Account: a, Amount: big.NewInt(1)},
		"deposit w/ party": {Kind: KindDeposit, Account: a, Party: a, Amount: big.NewInt(1)},
	} {
		if err := terms.Validate(); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	valid := Terms{Kind: KindSettle, Account: a, Party: a, Amount: big.NewInt(0)}
	if err := valid.Validate(); err != nil {
		t.Errorf("a zero-amount settle is well formed: %v", err)
	}
}

func testDomain() Domain {
	return Domain{
		Name:      "test",
		ChainID:   big.NewInt(31337),
		Rail:      common.BigToAddress(big.NewInt(0x1001)),
		Token:     common.BigToAddress(big.NewInt(0x1002)),
		Paymaster: common.BigToAddress(big.NewInt(0x1003)),
		Contracts: erc4337.Contracts{
			EntryPoint:        common.BigToAddress(big.NewInt(0x2001)),
			SafeSingleton:     common.BigToAddress(big.NewInt(0x2002)),
			SafeProxyFactory:  common.BigToAddress(big.NewInt(0x2003)),
			SafeModule:        common.BigToAddress(big.NewInt(0x2004)),
			SafeModuleSetup:   common.BigToAddress(big.NewInt(0x2005)),
			MultiSendCallOnly: common.BigToAddress(big.NewInt(0x2006)),
		},
		Finality: "finalized",
		Gas:      DefaultGas,
	}
}

// The call data must land on the canonical lengths and offsets the paymaster
// predicate reads. If either side drifts, sponsorship stops.
func TestCallDataMatchesTheSponsoredShapes(t *testing.T) {
	d := testDomain()
	id := testID(9)
	party := common.BigToAddress(big.NewInt(0xBEEF))
	amount := big.NewInt(1_000_000)

	batch, err := Terms{Kind: KindDeposit, Account: party, Amount: amount}.CallData(id, d)
	if err != nil {
		t.Fatal(err)
	}
	if len(batch) != 612 {
		t.Fatalf("deposit call data = %d bytes, want 612", len(batch))
	}
	if got := common.BytesToHash(batch[474:506]); got != id.Hash() {
		t.Errorf("identifier at offset 474 = %s", got)
	}
	if got := common.BytesToAddress(batch[506:538]); got != party {
		t.Errorf("account at offset 506 = %s", got)
	}
	if got := new(big.Int).SetBytes(batch[538:570]); got.Cmp(amount) != 0 {
		t.Errorf("amount at offset 538 = %s", got)
	}

	for _, kind := range []Kind{KindSettle, KindWithdraw} {
		cd, err := Terms{Kind: kind, Account: party, Party: party, Amount: amount}.CallData(id, d)
		if err != nil {
			t.Fatal(err)
		}
		if len(cd) != 292 {
			t.Fatalf("%s call data = %d bytes, want 292", kind, len(cd))
		}
		if got := common.BytesToHash(cd[168:200]); got != id.Hash() {
			t.Errorf("%s identifier at offset 168 = %s", kind, got)
		}
		if got := new(big.Int).SetBytes(cd[232:264]); got.Cmp(amount) != 0 {
			t.Errorf("%s amount at offset 232 = %s", kind, got)
		}
	}
}

func TestParseID(t *testing.T) {
	id := testID(3)
	got, err := ParseID(id.String())
	if err != nil || got != id {
		t.Fatalf("round trip: %s %v", got, err)
	}
	for _, bad := range []string{"", "0x", "0xzz", "0x00"} {
		if _, err := ParseID(bad); err == nil {
			t.Errorf("%q must not parse", bad)
		}
	}
}
