package rail

import (
	"math/big"
	"testing"
)

// The gas policy is the one decision in this library that money depends on and
// no contract guards, so it is checked exhaustively against an independent
// implementation written in plain integers.

type oracleInput struct {
	gas, token, swapCost, quote, pending int64
	quoted                               bool
}

type oraclePolicy struct {
	min, max, bound int64
	bps             int64
}

func oracle(p oraclePolicy, in oracleInput) Decision {
	if in.token < in.pending {
		return Decision{Action: ActionWait, Reason: ReasonNeedToken}
	}
	if in.gas >= p.min {
		return Decision{Action: ActionExecute}
	}
	if in.swapCost > p.bound {
		return Decision{Action: ActionWait, Reason: ReasonFeesAboveBound}
	}
	if in.gas < in.swapCost {
		return Decision{Action: ActionWait, Reason: ReasonNeedGas}
	}
	delta := p.max - (in.gas - in.swapCost)
	if !in.quoted {
		return Decision{Action: ActionQuote, Delta: big.NewInt(delta)}
	}
	margin := (in.quote*p.bps + 9_999) / 10_000
	maxInput := in.quote + margin
	if in.token < maxInput+in.pending {
		return Decision{Action: ActionWait, Reason: ReasonNeedToken}
	}
	return Decision{Action: ActionRefill, Delta: big.NewInt(delta), MaxInput: big.NewInt(maxInput)}
}

func TestDecideMatchesAnIndependentImplementation(t *testing.T) {
	policies := []oraclePolicy{
		{min: 20, max: 50, bound: 10, bps: 50},
		{min: 5, max: 6, bound: 5, bps: 0},
		{min: 13, max: 21, bound: 13, bps: 10_000},
		{min: 1, max: 100, bound: 1, bps: 1},
	}
	values := []int64{0, 1, 2, 3, 5, 8, 13, 21, 55}

	cases := 0
	for _, p := range policies {
		policy := GasPolicy{
			Min: big.NewInt(p.min), Max: big.NewInt(p.max),
			FeeBound: big.NewInt(p.bound), SlippageBps: uint32(p.bps),
		}
		for _, gas := range values {
			for _, token := range values {
				for _, swapCost := range values {
					for _, pending := range values {
						for _, quote := range append([]int64{-1}, values...) {
							in := oracleInput{
								gas: gas, token: token,
								swapCost: swapCost, pending: pending,
								quote: quote, quoted: quote >= 0,
							}
							checkOne(t, policy, p, in)
							cases++
						}
					}
				}
			}
		}
	}
	if cases < 20_000 {
		t.Fatalf("only %d cases enumerated", cases)
	}
}

func checkOne(t *testing.T, policy GasPolicy, p oraclePolicy, in oracleInput) {
	t.Helper()
	input := PolicyInput{
		Gas:           big.NewInt(in.gas),
		Token:         big.NewInt(in.token),
		SwapCost:      big.NewInt(in.swapCost),
		PendingAmount: big.NewInt(in.pending),
	}
	if in.quoted {
		input.Quote = big.NewInt(in.quote)
	}
	before := []string{
		input.Gas.String(), input.Token.String(),
		input.SwapCost.String(), input.PendingAmount.String(),
	}

	got := Decide(policy, input)
	want := oracle(p, in)

	if got.Action != want.Action || got.Reason != want.Reason {
		t.Fatalf("%+v: action %d reason %d, want %d %d", in, got.Action, got.Reason, want.Action, want.Reason)
	}
	if want.Delta != nil && (got.Delta == nil || got.Delta.Cmp(want.Delta) != 0) {
		t.Fatalf("%+v: buys %v, want %v", in, got.Delta, want.Delta)
	}
	if want.MaxInput != nil && (got.MaxInput == nil || got.MaxInput.Cmp(want.MaxInput) != 0) {
		t.Fatalf("%+v: input bound %v, want %v", in, got.MaxInput, want.MaxInput)
	}

	// The decision reads its inputs and changes none of them.
	after := []string{
		input.Gas.String(), input.Token.String(),
		input.SwapCost.String(), input.PendingAmount.String(),
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("%+v: the decision altered its own input", in)
		}
	}
	// Deciding twice decides the same.
	if again := Decide(policy, input); again.Action != got.Action || again.Reason != got.Reason {
		t.Fatalf("%+v: the decision is not deterministic", in)
	}

	switch got.Action {
	case ActionQuote:
		// Asking a price only makes sense for an amount worth buying.
		if got.Delta.Sign() <= 0 {
			t.Fatalf("%+v: quoting %s of gas", in, got.Delta)
		}
	case ActionExecute:
		// Executing means the payment is affordable and the reserve is at or
		// above its reorder point.
		if in.token < in.pending || in.gas < p.min {
			t.Fatalf("%+v: executed below the reorder point", in)
		}
	case ActionRefill:
		if got.Delta.Sign() <= 0 {
			t.Fatalf("%+v: refilling by %s", in, got.Delta)
		}
		// A refill never commits more than the quote plus its slippage margin,
		// and never eats the payment it is meant to enable.
		if got.MaxInput.Cmp(big.NewInt(in.quote)) < 0 {
			t.Fatalf("%+v: the input bound is below the quote", in)
		}
		if in.token < got.MaxInput.Int64()+in.pending {
			t.Fatalf("%+v: refilled with money the payment needs", in)
		}
	case ActionWait:
		if got.Reason == ReasonNone {
			t.Fatalf("%+v: waiting for no stated reason", in)
		}
		if got.Reason.Err() == nil {
			t.Fatalf("%+v: a shortage with no error to report", in)
		}
	}
}

func TestSlippageRoundsUpAndStaysInteger(t *testing.T) {
	for _, tc := range []struct {
		quote int64
		bps   uint32
		want  int64
	}{
		{quote: 0, bps: 50, want: 0},
		{quote: 1, bps: 1, want: 2}, // any margin at all rounds up to one unit
		{quote: 10_000, bps: 1, want: 10_001},
		{quote: 100, bps: 0, want: 100}, // no slippage allowance, no margin
		{quote: 200, bps: 10_000, want: 400},
	} {
		if got := withSlippage(big.NewInt(tc.quote), tc.bps); got.Int64() != tc.want {
			t.Fatalf("quote %d at %d bps is %s, want %d", tc.quote, tc.bps, got, tc.want)
		}
	}
}

func TestDecideTreatsMissingNumbersAsZero(t *testing.T) {
	policy := GasPolicy{Min: big.NewInt(10), Max: big.NewInt(20), FeeBound: big.NewInt(10)}
	// Nothing set at all: no money, no gas, nothing to pay for.
	if d := Decide(policy, PolicyInput{}); d.Action != ActionQuote {
		t.Fatalf("an empty account decides %d, want to price a refill", d.Action)
	}
}

func TestTheReorderPointIsTheWholeTrigger(t *testing.T) {
	p := GasPolicy{
		Min: big.NewInt(100), Max: big.NewInt(300), FeeBound: big.NewInt(100),
	}
	in := func(gas int64) PolicyInput {
		return PolicyInput{
			Gas: big.NewInt(gas), Token: big.NewInt(1_000_000),
			SwapCost: big.NewInt(10), PendingAmount: big.NewInt(1),
		}
	}
	for _, tc := range []struct {
		gas  int64
		want Action
	}{
		{gas: 101, want: ActionExecute}, // above
		{gas: 100, want: ActionExecute}, // exactly at it: still above
		{gas: 99, want: ActionQuote},    // one below: refill
		{gas: 10, want: ActionQuote},    // far below, still able to pay for it
	} {
		if got := Decide(p, in(tc.gas)); got.Action != tc.want {
			t.Fatalf("a reserve of %d decides %d, want %d", tc.gas, got.Action, tc.want)
		}
	}

	// However expensive the payment, it plays no part in the trigger: the
	// decision looks at the level and nothing else.
	if got := Decide(p, in(100)); got.Action != ActionExecute {
		t.Fatal("something other than the level entered the trigger")
	}

	// A refill puts the reserve back at the maximum, its own gas included.
	d := Decide(p, PolicyInput{
		Gas: big.NewInt(50), Token: big.NewInt(1_000_000),
		SwapCost: big.NewInt(10), Quote: big.NewInt(7),
	})
	if d.Action != ActionRefill {
		t.Fatalf("decided %d, want a refill", d.Action)
	}
	if landing := 50 - 10 + d.Delta.Int64(); landing != p.Max.Int64() {
		t.Fatalf("a refill lands the reserve at %d, want %s", landing, p.Max)
	}
}
