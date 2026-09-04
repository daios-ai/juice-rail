# Domains

One file per domain. A domain is one chain and one stablecoin on it:

```text
domain = (chain id, token address)
```

Records are bound to that pair, so a store opened for one domain refuses to
serve another.

Fill in `rpc` before use; it is the one field that is yours rather than the
chain's. Everything else is fixed by the deployment.

`finality` is the block tag the rail treats as settled: `latest`, `safe` or
`finalized`. Both Arbitrum files say `latest`: the sequencer's confirmation is
trusted, which is what choosing Arbitrum means. A domain that wants Ethereum
finality says `finalized` and waits for it. Confirmation counting is not
supported.

`fromBlock` is where the search for money coming in starts. Set it to the
block your account was created in: there is nothing before that to find, and
on a busy chain the difference is thousands of queries. The value shipped here
is the chain head when the file was written, which is a safe floor for an
account created after that and wrong for one created before.

The `gas` block is the operating reserve, in wei. `min` must be at least
`feeBound`, or the reserve could fall to a level from which it cannot pay for
its own refill. `railctl init` checks the whole file against the chain before
writing a profile: the token must answer as an ERC-20, carry EIP-2612 permits
and report the `decimals` this file claims, and the venue must be able to
price a refill.

## arbitrum-one

Real money. The token is USD₮0; its EIP-2612 `permit` was verified against the
live contract. The venue is the canonical Uniswap V3 SwapRouter and QuoterV2,
trading against the USDT0/WETH pool at the 0.05% tier
(`0x641C00A822e8b671738d32a431a4Fb6074E5c79d`). That router is the first
version, so `router02` is false.

The reserve band is roughly 0.001 to 0.003 ETH. At about 1900 USDT to the ETH,
a full refill costs under six USDT and buys many payments of runway.

## arbitrum-sepolia

Staging. Arbitrum Sepolia has no USDT0 and no pool to buy gas from, so both
were created for this repository by `contracts/script/SepoliaBootstrap.s.sol`:
a freely mintable mock token, and a real Uniswap V3 pool seeded with 0.004 WETH
against it.

```sh
cd contracts
forge script script/SepoliaBootstrap.s.sol --rpc-url $RPC --broadcast --private-key $KEY
```

It prints the token and pool addresses to put in the domain file. Run it again
to start over with a fresh token.

Two things differ from Arbitrum One and matter. The router there is
SwapRouter02, which encodes a swap slightly differently, so `router02` is true.
And the pool is shallow, so a refill moves the price several percent; the
slippage allowance is 5% rather than 1%, and the reserve band is small enough
that refills stay a fraction of the pool.

The mock token is not USDT0 and anyone can mint it. Staging proves the
workflow, the timing and the router encoding — not the market.
