package rail

import (
	"bytes"
	"context"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// eip2612Typehash is the constant fixed by EIP-2612 itself. Pinning it here
// means a change to the struct string is caught in this repository rather than
// by a token rejecting every permit.
const eip2612Typehash = "0x6e71edae12b1b97f4d1f60370fef10105fa2faae0126114a169c64845d6126c9"

func TestPermitTypehashIsTheOneInTheStandard(t *testing.T) {
	if got := permitTypehash.Hex(); got != eip2612Typehash {
		t.Fatalf("permit type hash is %s, want %s", got, eip2612Typehash)
	}
}

func TestTransferEventIsTheErc20One(t *testing.T) {
	want := "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"
	if got := topicTransfer.Hex(); got != want {
		t.Fatalf("transfer topic is %s, want %s", got, want)
	}
}

func TestPermitSignatureRecoversToTheAccount(t *testing.T) {
	key := testKey(t)
	owner := crypto.PubkeyToAddress(key.PublicKey)
	separator := crypto.Keccak256Hash([]byte("some token domain"))
	spender := common.HexToAddress("0x00000000000000000000000000000000000000b0")
	value, nonce, deadline := big.NewInt(1_234_567), big.NewInt(3), uint64(1_700_000_000)

	v, r, s, err := signPermit(key, separator, owner, spender, value, nonce, deadline)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if v != 27 && v != 28 {
		t.Fatalf("recovery byte is %d, want 27 or 28", v)
	}

	// Recompute the digest the way a token does, and check the signature is the
	// owner's.
	var encoded []byte
	encoded = append(encoded, permitTypehash.Bytes()...)
	encoded = append(encoded, common.LeftPadBytes(owner.Bytes(), 32)...)
	encoded = append(encoded, common.LeftPadBytes(spender.Bytes(), 32)...)
	encoded = append(encoded, common.LeftPadBytes(value.Bytes(), 32)...)
	encoded = append(encoded, common.LeftPadBytes(nonce.Bytes(), 32)...)
	encoded = append(encoded, common.LeftPadBytes(new(big.Int).SetUint64(deadline).Bytes(), 32)...)
	digest := crypto.Keccak256Hash([]byte{0x19, 0x01}, separator.Bytes(), crypto.Keccak256(encoded))

	sig := append(append(append([]byte{}, r[:]...), s[:]...), v-27)
	pub, err := crypto.SigToPub(digest.Bytes(), sig)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if crypto.PubkeyToAddress(*pub) != owner {
		t.Fatalf("the permit recovers to %s, not %s", crypto.PubkeyToAddress(*pub), owner)
	}
}

func TestTransferCalldataIsAPlainErc20Transfer(t *testing.T) {
	data, err := transferCalldata(bob, big.NewInt(12_500_000))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if !bytes.Equal(data[:4], tokenABI.Methods["transfer"].ID) {
		t.Fatalf("calldata calls %x, want transfer", data[:4])
	}
	args, err := tokenABI.Methods["transfer"].Inputs.Unpack(data[4:])
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if args[0].(common.Address) != bob || args[1].(*big.Int).Int64() != 12_500_000 {
		t.Fatalf("calldata pays %v %v", args[0], args[1])
	}
}

// refillParts decodes a refill back into its three steps.
func refillParts(t *testing.T, d Domain, data []byte) [][]byte {
	t.Helper()
	api := routerABI
	if d.Venue.Router02 {
		api = router02ABI
	}
	args, err := api.Methods["multicall"].Inputs.Unpack(data[4:])
	if err != nil {
		t.Fatalf("decode multicall: %v", err)
	}
	batch := args[len(args)-1].([][]byte)
	if len(batch) != 3 {
		t.Fatalf("a refill is %d calls, want permit, swap and unwrap", len(batch))
	}
	if d.Venue.Router02 {
		if got := args[0].(*big.Int); got.Sign() == 0 {
			t.Fatal("SwapRouter02 carries the deadline in multicall; it is missing")
		}
	}
	return batch
}

func TestRefillIsPermitSwapUnwrapInOneTransaction(t *testing.T) {
	for _, router02 := range []bool{false, true} {
		name := "SwapRouter"
		if router02 {
			name = "SwapRouter02"
		}
		t.Run(name, func(t *testing.T) {
			d := testDomain()
			d.Venue.Router02 = router02
			chain := newFakeChain(d)
			api := routerABI
			if router02 {
				api = router02ABI
			}

			delta := big.NewInt(40_000_000_000_000_000)
			maxInput := big.NewInt(123_000_000)
			deadline := uint64(1_700_003_600)
			data, err := refillCalldata(context.Background(), chain, d, testKey(t),
				crypto.PubkeyToAddress(testKey(t).PublicKey), delta, maxInput, deadline)
			if err != nil {
				t.Fatalf("encode refill: %v", err)
			}
			batch := refillParts(t, d, data)

			// 1. authorise the router for exactly the input bound
			permit, err := api.Methods["selfPermit"].Inputs.Unpack(batch[0][4:])
			if err != nil {
				t.Fatalf("decode permit: %v", err)
			}
			if permit[0].(common.Address) != d.Token || permit[1].(*big.Int).Cmp(maxInput) != 0 {
				t.Fatalf("permit authorises %v of %v", permit[1], permit[0])
			}
			if permit[2].(*big.Int).Uint64() != deadline {
				t.Fatalf("permit expires at %v, want %d", permit[2], deadline)
			}

			// 2. buy exactly delta, paying at most the bound, into the router
			swap, err := api.Methods["exactOutputSingle"].Inputs.Unpack(batch[1][4:])
			if err != nil {
				t.Fatalf("decode swap: %v", err)
			}
			fields := swapFields(t, swap[0], router02)
			if fields.tokenIn != d.Token || fields.tokenOut != d.Venue.WETH {
				t.Fatalf("swap trades %s for %s", fields.tokenIn, fields.tokenOut)
			}
			if fields.fee != uint64(d.Venue.FeeTier) {
				t.Fatalf("swap uses fee tier %d, want %d", fields.fee, d.Venue.FeeTier)
			}
			if fields.recipient != d.Venue.Router {
				t.Fatalf("swap output goes to %s, want the router itself", fields.recipient)
			}
			if fields.amountOut.Cmp(delta) != 0 || fields.amountInMax.Cmp(maxInput) != 0 {
				t.Fatalf("swap buys %s for at most %s", fields.amountOut, fields.amountInMax)
			}

			// 3. unwrap exactly what was bought, to the account
			unwrap, err := api.Methods["unwrapWETH9"].Inputs.Unpack(batch[2][4:])
			if err != nil {
				t.Fatalf("decode unwrap: %v", err)
			}
			if unwrap[0].(*big.Int).Cmp(delta) != 0 {
				t.Fatalf("unwrap floor is %v, want %s", unwrap[0], delta)
			}
			if unwrap[1].(common.Address) != crypto.PubkeyToAddress(testKey(t).PublicKey) {
				t.Fatalf("unwrap pays %v, not the account", unwrap[1])
			}
		})
	}
}

type swapView struct {
	tokenIn, tokenOut, recipient common.Address
	fee                          uint64
	amountOut, amountInMax       *big.Int
}

// swapFields reads the swap parameters out of either router's struct shape.
func swapFields(t *testing.T, v any, router02 bool) swapView {
	t.Helper()
	if router02 {
		p := v.(struct {
			TokenIn           common.Address `json:"tokenIn"`
			TokenOut          common.Address `json:"tokenOut"`
			Fee               *big.Int       `json:"fee"`
			Recipient         common.Address `json:"recipient"`
			AmountOut         *big.Int       `json:"amountOut"`
			AmountInMaximum   *big.Int       `json:"amountInMaximum"`
			SqrtPriceLimitX96 *big.Int       `json:"sqrtPriceLimitX96"`
		})
		return swapView{p.TokenIn, p.TokenOut, p.Recipient, p.Fee.Uint64(), p.AmountOut, p.AmountInMaximum}
	}
	p := v.(struct {
		TokenIn           common.Address `json:"tokenIn"`
		TokenOut          common.Address `json:"tokenOut"`
		Fee               *big.Int       `json:"fee"`
		Recipient         common.Address `json:"recipient"`
		Deadline          *big.Int       `json:"deadline"`
		AmountOut         *big.Int       `json:"amountOut"`
		AmountInMaximum   *big.Int       `json:"amountInMaximum"`
		SqrtPriceLimitX96 *big.Int       `json:"sqrtPriceLimitX96"`
	})
	if p.Deadline.Sign() == 0 {
		t.Fatal("the SwapRouter carries the deadline in its parameters; it is missing")
	}
	return swapView{p.TokenIn, p.TokenOut, p.Recipient, p.Fee.Uint64(), p.AmountOut, p.AmountInMaximum}
}

func TestQuoteAsksTheVenueForTheExactOutput(t *testing.T) {
	d := testDomain()
	chain := newFakeChain(d)
	delta := big.NewInt(1_000_000_000_000_000_000)
	quote, err := quoteRefill(context.Background(), chain, d, delta)
	if err != nil {
		t.Fatalf("quote: %v", err)
	}
	if quote.Cmp(chain.quote(delta)) != 0 {
		t.Fatalf("quote is %s, want %s", quote, chain.quote(delta))
	}
}

func TestTransferLogMatchesOnlyTheExactPayment(t *testing.T) {
	d := testDomain()
	from, to, amount := testAccount, bob, big.NewInt(500)
	topics := []common.Hash{
		topicTransfer,
		common.BytesToHash(from.Bytes()),
		common.BytesToHash(to.Bytes()),
	}
	data := common.LeftPadBytes(amount.Bytes(), 32)

	if !transferLog(d.Token, topics, data, d.Token, from, to, amount) {
		t.Fatal("the payment's own log was not recognised")
	}
	for _, tc := range []struct {
		name   string
		change func() (common.Address, []common.Hash, []byte)
	}{
		{"another token", func() (common.Address, []common.Hash, []byte) { return bob, topics, data }},
		{"another recipient", func() (common.Address, []common.Hash, []byte) {
			other := append([]common.Hash{}, topics...)
			other[2] = common.BytesToHash(exchange.Bytes())
			return d.Token, other, data
		}},
		{"another amount", func() (common.Address, []common.Hash, []byte) {
			return d.Token, topics, common.LeftPadBytes(big.NewInt(499).Bytes(), 32)
		}},
		{"another event", func() (common.Address, []common.Hash, []byte) {
			other := append([]common.Hash{}, topics...)
			other[0] = crypto.Keccak256Hash([]byte("Approval(address,address,uint256)"))
			return d.Token, other, data
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			address, tp, dt := tc.change()
			if transferLog(address, tp, dt, d.Token, from, to, amount) {
				t.Fatal("a log that is not the payment was accepted as proof of it")
			}
		})
	}
}
