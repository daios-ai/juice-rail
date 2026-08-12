package rail

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// Everything the rail says to a foreign contract is encoded here: the token,
// the swap venue and the permit signature. The rest of the library moves
// money without naming a selector.

const (
	// The ERC-20 surface a rail account uses, plus the EIP-2612 permit reads.
	tokenABIJSON = `[
      {"type":"function","name":"balanceOf","inputs":[{"name":"account","type":"address"}],
        "outputs":[{"type":"uint256"}],"stateMutability":"view"},
      {"type":"function","name":"transfer","inputs":[
        {"name":"to","type":"address"},{"name":"amount","type":"uint256"}],
        "outputs":[{"type":"bool"}],"stateMutability":"nonpayable"},
      {"type":"function","name":"allowance","inputs":[
        {"name":"owner","type":"address"},{"name":"spender","type":"address"}],
        "outputs":[{"type":"uint256"}],"stateMutability":"view"},
      {"type":"function","name":"nonces","inputs":[{"name":"owner","type":"address"}],
        "outputs":[{"type":"uint256"}],"stateMutability":"view"},
      {"type":"function","name":"decimals","inputs":[],
        "outputs":[{"type":"uint8"}],"stateMutability":"view"},
      {"type":"function","name":"DOMAIN_SEPARATOR","inputs":[],
        "outputs":[{"type":"bytes32"}],"stateMutability":"view"}]`

	// Uniswap V3 SwapRouter: permit, swap and unwrap, batched by multicall.
	routerABIJSON = `[
      {"type":"function","name":"multicall","inputs":[{"name":"data","type":"bytes[]"}],
        "outputs":[{"type":"bytes[]"}],"stateMutability":"payable"},
      {"type":"function","name":"selfPermit","inputs":[
        {"name":"token","type":"address"},{"name":"value","type":"uint256"},
        {"name":"deadline","type":"uint256"},{"name":"v","type":"uint8"},
        {"name":"r","type":"bytes32"},{"name":"s","type":"bytes32"}],
        "outputs":[],"stateMutability":"payable"},
      {"type":"function","name":"exactOutputSingle","inputs":[
        {"name":"params","type":"tuple","components":[
          {"name":"tokenIn","type":"address"},{"name":"tokenOut","type":"address"},
          {"name":"fee","type":"uint24"},{"name":"recipient","type":"address"},
          {"name":"deadline","type":"uint256"},{"name":"amountOut","type":"uint256"},
          {"name":"amountInMaximum","type":"uint256"},{"name":"sqrtPriceLimitX96","type":"uint160"}]}],
        "outputs":[{"type":"uint256"}],"stateMutability":"payable"},
      {"type":"function","name":"unwrapWETH9","inputs":[
        {"name":"amountMinimum","type":"uint256"},{"name":"recipient","type":"address"}],
        "outputs":[],"stateMutability":"payable"}]`

	// SwapRouter02 differs in exactly two places: the deadline moves from the
	// swap parameters into multicall.
	router02ABIJSON = `[
      {"type":"function","name":"multicall","inputs":[
        {"name":"deadline","type":"uint256"},{"name":"data","type":"bytes[]"}],
        "outputs":[{"type":"bytes[]"}],"stateMutability":"payable"},
      {"type":"function","name":"selfPermit","inputs":[
        {"name":"token","type":"address"},{"name":"value","type":"uint256"},
        {"name":"deadline","type":"uint256"},{"name":"v","type":"uint8"},
        {"name":"r","type":"bytes32"},{"name":"s","type":"bytes32"}],
        "outputs":[],"stateMutability":"payable"},
      {"type":"function","name":"exactOutputSingle","inputs":[
        {"name":"params","type":"tuple","components":[
          {"name":"tokenIn","type":"address"},{"name":"tokenOut","type":"address"},
          {"name":"fee","type":"uint24"},{"name":"recipient","type":"address"},
          {"name":"amountOut","type":"uint256"},{"name":"amountInMaximum","type":"uint256"},
          {"name":"sqrtPriceLimitX96","type":"uint160"}]}],
        "outputs":[{"type":"uint256"}],"stateMutability":"payable"},
      {"type":"function","name":"unwrapWETH9","inputs":[
        {"name":"amountMinimum","type":"uint256"},{"name":"recipient","type":"address"}],
        "outputs":[],"stateMutability":"payable"}]`

	// QuoterV2 prices an exact output. It is not a view function, so it is
	// asked by simulation and never submitted.
	quoterABIJSON = `[
      {"type":"function","name":"quoteExactOutputSingle","inputs":[
        {"name":"params","type":"tuple","components":[
          {"name":"tokenIn","type":"address"},{"name":"tokenOut","type":"address"},
          {"name":"amount","type":"uint256"},{"name":"fee","type":"uint24"},
          {"name":"sqrtPriceLimitX96","type":"uint160"}]}],
        "outputs":[
          {"name":"amountIn","type":"uint256"},{"name":"sqrtPriceX96After","type":"uint160"},
          {"name":"initializedTicksCrossed","type":"uint32"},{"name":"gasEstimate","type":"uint256"}],
        "stateMutability":"nonpayable"}]`
)

var (
	tokenABI    = mustABI(tokenABIJSON)
	routerABI   = mustABI(routerABIJSON)
	router02ABI = mustABI(router02ABIJSON)
	quoterABI   = mustABI(quoterABIJSON)

	// topicTransfer is the ERC-20 Transfer event, which is how a deposit is
	// observed and a payment is confirmed.
	topicTransfer = crypto.Keccak256Hash([]byte("Transfer(address,address,uint256)"))

	// permitTypehash is the EIP-2612 struct type.
	permitTypehash = crypto.Keccak256Hash([]byte(
		"Permit(address owner,address spender,uint256 value,uint256 nonce,uint256 deadline)"))
)

func mustABI(s string) abi.ABI {
	a, err := abi.JSON(strings.NewReader(s))
	if err != nil {
		panic(fmt.Sprintf("rail: bad ABI: %v", err))
	}
	return a
}

// exactOutputParams is the V1 SwapRouter parameter struct.
type exactOutputParams struct {
	TokenIn           common.Address
	TokenOut          common.Address
	Fee               *big.Int
	Recipient         common.Address
	Deadline          *big.Int
	AmountOut         *big.Int
	AmountInMaximum   *big.Int
	SqrtPriceLimitX96 *big.Int
}

// exactOutputParams02 is the same without the deadline.
type exactOutputParams02 struct {
	TokenIn           common.Address
	TokenOut          common.Address
	Fee               *big.Int
	Recipient         common.Address
	AmountOut         *big.Int
	AmountInMaximum   *big.Int
	SqrtPriceLimitX96 *big.Int
}

type quoteParams struct {
	TokenIn           common.Address
	TokenOut          common.Address
	Amount            *big.Int
	Fee               *big.Int
	SqrtPriceLimitX96 *big.Int
}

// --- reads ---

func call(ctx context.Context, chain Chain, to common.Address, a abi.ABI, method string, args ...any) ([]byte, error) {
	in, err := a.Pack(method, args...)
	if err != nil {
		return nil, err
	}
	out, err := chain.CallContract(ctx, ethereum.CallMsg{To: &to, Data: in}, nil)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", method, err)
	}
	return out, nil
}

func callWord(ctx context.Context, chain Chain, to common.Address, a abi.ABI, method string, args ...any) ([]byte, error) {
	out, err := call(ctx, chain, to, a, method, args...)
	if err != nil {
		return nil, err
	}
	if len(out) != 32 {
		return nil, fmt.Errorf("%s: want 32 bytes, got %d", method, len(out))
	}
	return out, nil
}

func tokenUint(ctx context.Context, chain Chain, token common.Address, method string, args ...any) (*big.Int, error) {
	out, err := callWord(ctx, chain, token, tokenABI, method, args...)
	if err != nil {
		return nil, err
	}
	return new(big.Int).SetBytes(out), nil
}

// transferCalldata is the payment itself: an ordinary ERC-20 transfer.
func transferCalldata(to common.Address, amount *big.Int) ([]byte, error) {
	return tokenABI.Pack("transfer", to, amount)
}

// quoteRefill asks the venue what buying delta of native currency costs.
func quoteRefill(ctx context.Context, chain Chain, d Domain, delta *big.Int) (*big.Int, error) {
	out, err := call(ctx, chain, d.Venue.Quoter, quoterABI, "quoteExactOutputSingle", quoteParams{
		TokenIn:           d.Token,
		TokenOut:          d.Venue.WETH,
		Amount:            delta,
		Fee:               big.NewInt(int64(d.Venue.FeeTier)),
		SqrtPriceLimitX96: new(big.Int),
	})
	if err != nil {
		return nil, fmt.Errorf("quote refill: %w", err)
	}
	values, err := quoterABI.Unpack("quoteExactOutputSingle", out)
	if err != nil || len(values) == 0 {
		return nil, fmt.Errorf("quote refill: undecodable answer")
	}
	amountIn, ok := values[0].(*big.Int)
	if !ok || amountIn.Sign() <= 0 {
		return nil, fmt.Errorf("quote refill: the venue priced %s at nothing", delta)
	}
	return amountIn, nil
}

// refillCalldata builds the whole refill: authorise the router for at most
// maxInput, buy exactly delta of wrapped native currency, unwrap it to the
// account. One transaction, so there is no window in which the authorisation
// exists without the swap.
//
// The swap's recipient is the router itself, which is what unwrapWETH9 then
// unwraps. Naming the router by address rather than by either router version's
// sentinel keeps one encoding valid for both.
func refillCalldata(ctx context.Context, chain Chain, d Domain, key *ecdsa.PrivateKey, account common.Address, delta, maxInput *big.Int, deadline uint64) ([]byte, error) {
	permit, err := permitCall(ctx, chain, d, key, account, maxInput, deadline)
	if err != nil {
		return nil, err
	}
	router := routerABI
	if d.Venue.Router02 {
		router = router02ABI
	}
	fee := big.NewInt(int64(d.Venue.FeeTier))

	var swap []byte
	if d.Venue.Router02 {
		swap, err = router.Pack("exactOutputSingle", exactOutputParams02{
			TokenIn: d.Token, TokenOut: d.Venue.WETH, Fee: fee,
			Recipient: d.Venue.Router, AmountOut: delta, AmountInMaximum: maxInput,
			SqrtPriceLimitX96: new(big.Int),
		})
	} else {
		swap, err = router.Pack("exactOutputSingle", exactOutputParams{
			TokenIn: d.Token, TokenOut: d.Venue.WETH, Fee: fee,
			Recipient: d.Venue.Router, Deadline: new(big.Int).SetUint64(deadline),
			AmountOut: delta, AmountInMaximum: maxInput,
			SqrtPriceLimitX96: new(big.Int),
		})
	}
	if err != nil {
		return nil, fmt.Errorf("encode swap: %w", err)
	}
	unwrap, err := router.Pack("unwrapWETH9", delta, account)
	if err != nil {
		return nil, fmt.Errorf("encode unwrap: %w", err)
	}

	batch := [][]byte{permit, swap, unwrap}
	if d.Venue.Router02 {
		return router.Pack("multicall", new(big.Int).SetUint64(deadline), batch)
	}
	return router.Pack("multicall", batch)
}

// permitCall signs an EIP-2612 authorisation for the router and encodes the
// call that presents it. The value is the refill's input bound: the swap
// consumes at most that, and the next refill's permit replaces whatever is
// left over.
func permitCall(ctx context.Context, chain Chain, d Domain, key *ecdsa.PrivateKey, account common.Address, value *big.Int, deadline uint64) ([]byte, error) {
	separator, err := callWord(ctx, chain, d.Token, tokenABI, "DOMAIN_SEPARATOR")
	if err != nil {
		return nil, fmt.Errorf("token %s does not implement EIP-2612: %w", d.Token, err)
	}
	nonce, err := tokenUint(ctx, chain, d.Token, "nonces", account)
	if err != nil {
		return nil, fmt.Errorf("token %s does not implement EIP-2612: %w", d.Token, err)
	}
	v, r, s, err := signPermit(key, common.BytesToHash(separator), account, d.Venue.Router, value, nonce, deadline)
	if err != nil {
		return nil, err
	}
	router := routerABI
	if d.Venue.Router02 {
		router = router02ABI
	}
	return router.Pack("selfPermit", d.Token, value, new(big.Int).SetUint64(deadline), v, r, s)
}

// signPermit produces the EIP-2612 signature over the token's own EIP-712
// domain. Reading the separator from the token means no name or version has to
// be configured, and none can be misconfigured.
func signPermit(key *ecdsa.PrivateKey, separator common.Hash, owner, spender common.Address, value, nonce *big.Int, deadline uint64) (uint8, [32]byte, [32]byte, error) {
	var (
		r [32]byte
		s [32]byte
	)
	var structData []byte
	structData = append(structData, permitTypehash.Bytes()...)
	structData = append(structData, common.LeftPadBytes(owner.Bytes(), 32)...)
	structData = append(structData, common.LeftPadBytes(spender.Bytes(), 32)...)
	structData = append(structData, common.LeftPadBytes(value.Bytes(), 32)...)
	structData = append(structData, common.LeftPadBytes(nonce.Bytes(), 32)...)
	structData = append(structData, common.LeftPadBytes(new(big.Int).SetUint64(deadline).Bytes(), 32)...)

	digest := crypto.Keccak256Hash([]byte{0x19, 0x01}, separator.Bytes(), crypto.Keccak256(structData))
	sig, err := crypto.Sign(digest.Bytes(), key)
	if err != nil {
		return 0, r, s, fmt.Errorf("sign permit: %w", err)
	}
	copy(r[:], sig[:32])
	copy(s[:], sig[32:64])
	// crypto.Sign reports recovery as 0 or 1; Ethereum signatures carry 27 or 28.
	return sig[64] + 27, r, s, nil
}

// transferLog reports whether a log is exactly this token transfer. A token
// that signals failure by returning false instead of reverting would otherwise
// look like a successful payment.
func transferLog(address common.Address, topics []common.Hash, data []byte, token, from, to common.Address, amount *big.Int) bool {
	if address != token || len(topics) != 3 || topics[0] != topicTransfer {
		return false
	}
	if common.BytesToAddress(topics[1].Bytes()) != from || common.BytesToAddress(topics[2].Bytes()) != to {
		return false
	}
	return len(data) >= 32 && new(big.Int).SetBytes(data[len(data)-32:]).Cmp(amount) == 0
}
