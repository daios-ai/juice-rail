package rail

import "math/big"

// Action is what the gas policy says to do next.
type Action uint8

const (
	// ActionExecute: the reserve covers the payment. Send it.
	ActionExecute Action = iota
	// ActionQuote: a refill is needed and its size is known, but its price is
	// not. Quote Delta, then decide again.
	ActionQuote
	// ActionRefill: buy Delta of native currency for at most MaxInput.
	ActionRefill
	// ActionWait: a shortage. Nothing may be signed; Reason says which.
	ActionWait
)

// Reason names a shortage. Each maps to one error, so a blocked account always
// says exactly what it is short of.
type Reason uint8

const (
	ReasonNone Reason = iota
	// ReasonNeedToken: not enough stablecoin for the payment, or for the
	// refill that would enable it.
	ReasonNeedToken
	// ReasonNeedNative: the reserve cannot pay for its own refill. Only an
	// external top-up recovers from this.
	ReasonNeedNative
	// ReasonFeesAboveBound: gas costs more than the configured bound.
	ReasonFeesAboveBound
)

// Err turns a shortage into the error the caller sees.
func (r Reason) Err() error {
	switch r {
	case ReasonNeedToken:
		return ErrInsufficientStablecoin
	case ReasonNeedNative:
		return ErrInsufficientNative
	case ReasonFeesAboveBound:
		return ErrFeesAboveBound
	default:
		return nil
	}
}

// PolicyInput is everything the decision depends on. Costs are upper bounds
// (fee cap times a fixed gas limit) rather than estimates, so the decision errs
// towards keeping the account able to act.
type PolicyInput struct {
	// Gas is the native balance; Token is the stablecoin balance.
	Gas, Token *big.Int
	// SwapCost is what a refill's own transaction may cost.
	SwapCost *big.Int
	// Quote is the stablecoin input the venue quoted for exactly Delta. Nil
	// until the caller has asked.
	Quote *big.Int
	// PendingAmount is the payment about to be made, which a refill may not
	// spend.
	PendingAmount *big.Int
}

// Decision is the policy's whole output.
type Decision struct {
	Action Action
	// Delta is the native currency a refill must deliver.
	Delta *big.Int
	// MaxInput is the most stablecoin a refill may spend.
	MaxInput *big.Int
	Reason   Reason
}

// Decide is the entire gas policy: ordinary min/max hysteresis, as a total
// function of balances, one cost and one venue quote. It reads no clock, no
// chain and no configuration beyond its arguments.
//
// It looks at the level of the reserve, not at what the next transaction will
// cost. MIN is the buffer that covers that, which is what a reorder point is
// for.
func Decide(p GasPolicy, in PolicyInput) Decision {
	gas := orZero(in.Gas)
	token := orZero(in.Token)
	swapCost := orZero(in.SwapCost)
	pending := orZero(in.PendingAmount)

	// The payment itself must be affordable. Nothing else is worth deciding.
	if token.Cmp(pending) < 0 {
		return Decision{Action: ActionWait, Reason: ReasonNeedToken}
	}
	// Above the reorder point: go.
	if gas.Cmp(p.Min) >= 0 {
		return Decision{Action: ActionExecute}
	}
	// Below it, buy back up to the maximum. The refill is itself a transaction,
	// so it must be affordable and worth making.
	if swapCost.Cmp(p.FeeBound) > 0 {
		return Decision{Action: ActionWait, Reason: ReasonFeesAboveBound}
	}
	if gas.Cmp(swapCost) < 0 {
		return Decision{Action: ActionWait, Reason: ReasonNeedNative}
	}
	// Counting the refill's own gas as spent. This is positive: the reserve is
	// under MIN, and MAX is above it.
	delta := new(big.Int).Sub(p.Max, new(big.Int).Sub(gas, swapCost))
	if in.Quote == nil {
		return Decision{Action: ActionQuote, Delta: delta}
	}
	maxInput := withSlippage(in.Quote, p.SlippageBps)
	// The refill may not spend what the payment needs.
	if token.Cmp(new(big.Int).Add(maxInput, pending)) < 0 {
		return Decision{Action: ActionWait, Reason: ReasonNeedToken}
	}
	return Decision{Action: ActionRefill, Delta: delta, MaxInput: maxInput}
}

// withSlippage is the input bound x = q + ceil(q*s/10000). Integer arithmetic
// throughout, rounded up, so the bound is never less than the quote allows.
func withSlippage(quote *big.Int, bps uint32) *big.Int {
	q := orZero(quote)
	margin := new(big.Int).Mul(q, big.NewInt(int64(bps)))
	margin.Add(margin, big.NewInt(9_999))
	margin.Div(margin, big.NewInt(10_000))
	return new(big.Int).Add(q, margin)
}

func orZero(v *big.Int) *big.Int {
	if v == nil {
		return new(big.Int)
	}
	return v
}
