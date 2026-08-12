# juice-rail

A small money rail. Each participant has one ordinary blockchain account that
holds a stablecoin as money, and pays its own way. There is no rail contract,
no operator, and nobody who can freeze anyone else.

Three things you can do: **deposit**, **transfer**, **withdraw**.

---

## 1. The words

Each term below is used by the ones after it, so read them in order.

**Chain.** A public ledger of accounts and balances. We use Ethereum-style
chains: Arbitrum, and a throwaway local one for testing.

**Transaction.** The only way to change anything on a chain. Someone signs one
and sends it to the network.

**Gas.** What a transaction costs. It is always paid in the chain's own
currency — ETH on Ethereum and Arbitrum. You cannot pay gas in anything else.

**Token.** A balance sheet kept by a contract on the chain. Sending tokens is
an ordinary transaction, so the sender pays gas for it. The receiver does
nothing and pays nothing.

**Stablecoin.** A token meant to hold a steady value. Ours is USDT0. It is the
money in this system; amounts are in it and nothing else.

**Account.** One address on the chain, controlled by one private key. In
juice-rail an account holds two things:

```text
stablecoin   the money
gas currency the fuel it needs to send transactions
```

That is the whole design. Each account is its own vault.

**Domain.** One chain plus one stablecoin on it:

```text
domain = (chain, token address)
```

Two domains are two separate worlds. Money on one is not money on the other,
and there is no bridge here.

**Finality.** A chain can briefly reorganise: a block that looked settled can
disappear. Finality is the point past which it cannot. juice-rail treats
nothing as real before finality — not a submitted transaction, not one already
included in a block.

---

## 2. The three things you can do

**Deposit** is money arriving. Someone — an exchange, a wallet, another
participant — sends the stablecoin to your address. You do nothing. You send no
transaction and you need no gas. Once it finalizes, juice-rail records it.

**Transfer** is paying another participant. Your account sends the stablecoin
to theirs. You pay the gas; they pay nothing and need nothing.

**Withdraw** is the same transaction sent somewhere outside the rail — an
exchange, a wallet. The only difference is who is on the other end, so the
difference is a label, not a mechanism.

---

## 3. Gas, and why an account looks after its own

Only the account sending a transaction can pay for it. There are systems that
hide this by having somebody else submit your transactions for you, but then
you cannot pay unless that somebody is online and willing. juice-rail takes the
other trade: you fund your account once with a little gas currency, and after
that it keeps itself topped up. Nobody else is ever needed.

**Reserve.** The gas currency in your account. It is fuel, not money, and it is
never shown as part of your balance.

**Refill.** When the reserve runs low, the account sells a little of its own
stablecoin for gas currency and keeps going. This is maintenance, not a fourth
thing you can do, and the amounts are small.

**Venue.** Where the refill trades: a Uniswap V3 pool, named in the domain's
configuration. juice-rail keeps no prices of its own; it asks the venue.

**Quote.** What the venue says the gas will cost, asked fresh each time.

**Slippage.** The price can move between the quote and the trade. The refill
carries a limit — "spend at most this much" — and reverts rather than exceed
it.

The rule, in full:

```text
if the reserve is at or above MIN:     send it
otherwise, if a refill is affordable:  buy gas up to MAX, then send it
otherwise:                             wait, and say why
```

Waiting is a real outcome and always says which of two things is short:

```text
stablecoin too low, top up        send the stablecoin to the account
native currency too low, top up   send the gas currency to the account
```

Each message carries the amounts and the address to send to.

Nothing is signed when the account waits. Blocked is not lost.

Two honest limits. No fixed reserve survives every gas price, so there is a
configured ceiling on what a refill may cost, and above it the account waits.
And if the reserve ever empties completely, the only way back is somebody
sending gas currency in, exactly as at the start.

---

## 4. Doing the same thing twice

**Identifier.** You name each payment with a 32-byte identifier of your own
choosing. Running the same command twice with the same identifier is the same
payment, not two.

**Nonce.** Every account has a counter. Each transaction it sends carries the
next number, and the chain accepts one transaction per number, ever. juice-rail
writes down which payment owns which number *before* it signs anything.

That is the whole safety argument. A payment owns one number; the chain admits
one transaction per number; so a payment can happen at most once, no matter how
many times you retry, and no matter when the program is killed.

**Retry.** If a transaction is stuck, retry it. It goes out again with the same
number and the same amount to the same person — only the fee is raised, which
is how the network is meant to replace a stuck transaction.

**Status.** Always about the payment, never about one attempt:

```text
unknown    never heard of it
pending    it may still happen
confirmed  it happened, and finality says so
failed     it can never happen
```

Only the last two are permanent.

---

## 5. Try it locally

You need [Go](https://go.dev) 1.22+ and [Foundry](https://getfoundry.sh)
(`forge`, `anvil`).

```sh
git clone --recurse-submodules <this repo> && cd juice-rail
go build ./...
cd contracts && forge build && cd ..
go test ./...                                  # the library
go test -tags integration ./integration-tests  # the six stories on a local chain
```

The stories start their own throwaway chain, deploy a mock token and a mock
venue, and drive the real `railctl` binary through onboarding, a deposit, a
payment, a withdrawal, a refill and a crash.

---

## 6. Using it: railctl

`railctl` is the operations tool. It is not a wallet product.

### Create an account

Copy a domain file from `deployments/` and fill in its `rpc` — that is the one
field the chain does not fix for you. Then:

```sh
railctl init alice my-domain.json
```

This makes a key, checks the domain really works, and prints your address with
what to send it:

```text
profile alice on domain arbitrum-sepolia, account 0x3B06...E4fD

to make this account operational, fund it:
  1. send the stablecoin to 0x3B06...E4fD
  2. send at least 0.0004 of the native currency to the same address
  3. wait for finality
```

Step 2 is the only time you handle gas currency by hand.

### Everyday use

```sh
railctl balance                       # money, and the reserve, separately
railctl deposits                      # money that arrived and finalized
railctl transfer <id> <address> 25.00
railctl withdraw <id> <address> 25.00
railctl status <id>
railctl retry <id>                    # if a payment is stuck
```

Identifiers are yours to pick; `openssl rand -hex 32` is fine.

Amounts are written the way you say them: `25.00`, `0.50`, `1250`. There is no
floating point anywhere inside — amounts are whole numbers of the token's
smallest unit throughout.

When the reserve is too low, a payment does not go through. Instead the account
buys gas and tells you to come back:

```text
reserve low: refill 0x82295988... submitted 0x5eb29983...;
run the payment again once it is confirmed
```

Run the same command again after finality and it goes through. Add
`-no-refill` if you would rather it just refused.

Add `-json` to any command for machine-readable output.

### Where things are kept

```text
~/.juice-rail/config.json       domains and profiles
~/.juice-rail/credentials.json  keys, and nothing else (mode 600)
~/.juice-rail/<profile>.db      the records for one account
```

For automation, pass everything instead and the home directory is never read:
`-config <file> -store <file>` and `RAILCTL_KEY=<hex>`.

---

## 7. Configuring a domain

A domain file, field by field:

```json
{
  "name": "arbitrum-sepolia",
  "chainId": 421614,
  "rpc": "https://...",
  "token": "0x8e87...d568",
  "decimals": 6,
  "finality": "finalized",
  "fromBlock": 297184700,
  "venue": {
    "router":   "0x101F...663E",
    "quoter":   "0x2779...Ef0B",
    "weth":     "0x980B...c973",
    "feeTier":  500,
    "router02": true
  },
  "gas": {
    "min":         "200000000000000",
    "max":         "400000000000000",
    "feeBound":    "100000000000000",
    "slippageBps": 500,
    "paymentGas":  300000,
    "swapGas":     1500000
  }
}
```

- `finality` must be `finalized`. Counting confirmations is not supported,
  because something reported as confirmed must never come undone.
- `fromBlock` is where the search for deposits starts. Set it to the block your
  account was created in; there is nothing before that to find.
- `venue.router02` says which version of the Uniswap router is deployed. The
  two encode a swap slightly differently. Arbitrum One has the first;
  Arbitrum Sepolia has SwapRouter02.
- `gas.min` / `gas.max` are the reserve band, in wei. The account refills when
  its reserve is below `min`, and buys back up to `max`.
- `gas.feeBound` is the most a refill may cost. Above it the account waits.
  It must be no larger than `min`, or the reserve could fall to a level from
  which it cannot pay for its own refill.
- `gas.slippageBps` is how far past the quote a refill may go, in hundredths
  of a percent. `500` is 5%.
- `gas.paymentGas` / `gas.swapGas` bound the gas of a payment and of a refill.
  These are bounds, not estimates: unused gas is not charged, and an estimate
  would need the account to already hold the gas we are deciding whether to
  buy. Chains that charge for data, like rollups, need larger numbers.

`deployments/` holds a file per domain with everything but the `rpc`, which is
yours to fill in.

Arbitrum Sepolia has no USDT0 and no pool to buy gas from, so there is a
one-time setup script for staging:

```sh
cd contracts
forge script script/SepoliaBootstrap.s.sol --rpc-url $RPC --broadcast --private-key $KEY
```

It deploys a mock token and seeds a real Uniswap V3 pool, then prints the
addresses to put in the domain file.

---

## 8. Embedding the library

`go/rail` is the whole thing. One `Rail` is one account on one domain.

```go
r, err := rail.New(domain, store, chainClient, key)

r.Balances(ctx)                                    // money, reserve
r.ScanDeposits(ctx)                                // newly finalized money in
r.Prepare(ctx, id, rail.KindTransfer, to, amount)  // decide and write down
r.Send(ctx, id)                                    // sign and broadcast
r.Retry(ctx, id)                                   // same nonce, higher fee
r.Status(ctx, id)                                  // from finalized facts only
r.Refill(ctx, reserve)                             // buy gas, when asked to
```

`Prepare` returns `ErrNeedRefill` when the reserve is short, and having written
nothing down. Call `Refill`, wait for finality, then call `Prepare` again.

The library reads no file, no environment variable and no home directory.
Everything arrives at construction; finding configuration is the app's job.

It keeps four kinds of record, through a `Store` interface you can implement
over your own database — a SQLite one ships:

```text
intents      which payment owns which nonce, written before signing
submissions  each signed attempt, written before broadcasting
facts        finalized outcomes, cached because they cannot change
deposits     finalized money in, one record per log
```

One rule the library cannot enforce for you: **one signer per key.** Nonces are
chain state, so two programs holding the same key would race for them. If
juice-rail ever finds a spent nonce it has no record of, it stops and says so
rather than guessing.

---

## 9. What is checked

- The gas rule is a pure function of balances, costs and one quote. It is
  checked exhaustively — every combination on a grid, against a second
  implementation written independently.
- The lifecycle of a payment is enumerated: every shape its history can take,
  checking that a settled status never moves again.
- The six stories run against the compiled tool on a real chain.
- The whole flow has been run on Arbitrum Sepolia against a real Uniswap V3
  pool, including a refill.

What is assumed, and not checked here: finality is final, the token is a
correct ERC-20 with EIP-2612 permits, the venue delivers what it quotes or
reverts, gas stays below the configured bound, one signer per key, and the RPC
tells the truth.

---

## 10. Layout

```text
go/rail/          the library: accounts, the gas rule, the venue, status
go/sqlite/        the shipped record store
cmd/railctl/      the operations tool
contracts/        test and staging fixtures only; no rail contract exists
deployments/      one file per domain
integration-tests/ the six stories
requirements.md   what this is meant to be, and why
```
