<p align="center">
  <img src="docs/juice-logo.svg" alt="Juice logo" width="110" />
</p>

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

**Settlement.** Each domain names the block tag past which juice-rail treats a
fact as real: `latest`, `safe` or `finalized`. Nothing is real before that —
not a submitted transaction, not one already included in a block. The Arbitrum
domains say `latest`: the sequencer's confirmation is trusted, which is what
choosing Arbitrum means, and a fact settles in about a second. A domain that
wants Ethereum finality says `finalized` and waits for it.

---

## 2. The three things you can do

**Deposit** is money arriving. Someone — an exchange, a wallet, another
participant — sends the stablecoin to your address. You do nothing. You send no
transaction and you need no gas. Once it settles, juice-rail records it.

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

Leaving is the mirror of that. `withdraw ... all` sends the whole balance out
and does not buy gas to do it — spending money to leave would be perverse. The
reserve stays behind, a couple of units of gas currency. It is your key and
your address, so any wallet can sweep it; juice-rail simply has no verb that
does, because all three of its verbs move the stablecoin.

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
confirmed  it happened, and the settled chain says so
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
go test -tags integration ./integration-tests  # the seven stories on a local chain
```

The stories start their own throwaway chain, deploy a mock token and a mock
venue, and drive the real `railctl` binary through onboarding, a deposit, a
payment, a withdrawal, a refill, a crash, and leaving.

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
  3. wait until the chain settles it
```

Step 2 is the only time you handle gas currency by hand.

### Everyday use

```sh
railctl balance                       # money, and the reserve, separately
railctl deposits                      # money that arrived and settled
railctl transfer <id> <address> 25.00
railctl withdraw <id> <address> 25.00
railctl withdraw <id> <address> all   # send the whole balance out
railctl status <id>
railctl retry <id>                    # if a payment is stuck
```

Identifiers are yours to pick; `openssl rand -hex 32` is fine.

`balance` shows the money twice: as it is now, and as it stood at the last
settled block. The difference between the two lines is money in flight.

```text
balance 48.78  reserve 0.000456589
settled 48.78  reserve 0.000456589  at block 302237886
```

`status` adds, once an operation has settled, which transaction carried it
and where — and for a refill, what buying the gas actually cost, which is
less than the amount it was allowed to spend.

```text
0x0abf…35da confirmed
  settled in 0x3222b8ec…a7b73b at block 297406682
  cost 1.2194
```

`deposits` ends with the block it searched up to, so an empty list means no
money arrived rather than that nothing was looked for.

Amounts are written the way you say them: `25.00`, `0.50`, `1250`. There is no
floating point anywhere inside — amounts are whole numbers of the token's
smallest unit throughout.

When the reserve is too low, a payment does not go through. Instead the account
buys gas and tells you to come back:

```text
reserve low: refill 0x82295988... submitted 0x5eb29983...;
run the payment again once it is confirmed
```

Run the same command again once the refill has settled and it goes through. Add
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
  "finality": "latest",
  "fromBlock": 297184700,
  "venue": {
    "router":   "0x101F...663E",
    "quoter":   "0x2779...Ef0B",
    "weth":     "0x980B...c973",
    "feeTier":  500,
    "router02": true
  },
  "gas": {
    "min":         "400000000000000",
    "max":         "600000000000000",
    "feeBound":    "400000000000000",
    "slippageBps": 500,
    "paymentGas":  150000,
    "swapGas":     500000
  }
}
```

- `decimals` is how many base units make one token. It is checked against the
  token itself at `init`, because mistaking six for eighteen would misstate
  every amount by a factor of a trillion.
- `finality` is the block tag treated as settled: `latest`, `safe` or
  `finalized`. The Arbitrum files say `latest`. Counting confirmations is not
  supported.
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

r.Balances(ctx)                                 // money, reserve
r.ScanDeposits(ctx)                             // newly settled money in
r.Pay(ctx, id, rail.KindTransfer, to, amount)   // the whole payment flow
r.WithdrawAll(ctx, id, to)                      // send the whole balance out
r.Retry(ctx, id)                                // same nonce, higher fee
r.Status(ctx, id)                               // from settled facts only
r.Outcome(ctx, id)                              // the settled tx, and its block
r.RefillCost(ctx, refillID)                     // what buying gas actually cost
r.SettledBalances(ctx)                          // balances at a settled block N
r.DepositsScannedTo()                           // deposits observed through ...
```

`Pay` returns an `Outcome`. Either the payment went out, or the reserve was
too low and the account bought gas instead:

```go
if out.Refilled() {
    // out.RefillTx is buying gas. Wait for it to settle, then call Pay
    // again with the same identifier.
}
```

Nothing else is needed to run a rail. The steps behind `Pay` —
`Prepare`, `Send`, `Refill` — stay public for hosts that want to stop between
them, but no host has to reimplement the flow, and none should.

A refill's recorded amount is the most it was allowed to spend, not what it
spent. If you keep your own ledger and need to book the cost of gas against a
user, ask `RefillCost` — it reads the winning transaction and answers exactly.

### Checking your books against the chain

If you keep a ledger of your own, check it against a settled block, never
against the present moment: money in flight makes the present moment wrong
with nothing amiss. The procedure:

```text
1. ScanDeposits          advance your view of incoming money
2. SettledBalances       the chain's answer at block N
3. DepositsScannedTo     below N? repeat step 1
4. Outcome, per operation you have not yet settled
5. compare               chain balance at N == your ledger at N
```

Count only what settled at or before N — deposits and outcomes both carry
their block. Deduct settled refills at their `RefillCost`, or the books will
be off by the price of gas. After a restart, ask `Pending()` too, so a refill
you never saw recorded is not missed.

This audits the present, promptly. Asking what the balance was at some past
block needs an archive node, which the public endpoints here do not provide.

Amounts are the domain's business, not yours: `domain.ParseAmount("12.50")`,
`domain.FormatAmount(v)` and `rail.FormatNative(v)` handle units, and every
error the library returns is already written in them. `domain.FundingChecklist(addr)`
gives you the onboarding text.

The library reads no file, no environment variable and no home directory.
Everything arrives at construction; finding configuration is the app's job.

It keeps four kinds of record, through a `Store` interface you can implement
over your own database — a SQLite one ships:

```text
intents      which payment owns which nonce, written before signing
submissions  each signed attempt, written before broadcasting
facts        settled outcomes, cached because they cannot change
deposits     settled money in, one record per log
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
- The seven stories run against the compiled tool on a real chain.
- The whole flow has been run on Arbitrum Sepolia against a real Uniswap V3
  pool, including a refill.

What is assumed, and not checked here: the domain's settlement tag is final, the token is a
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
integration-tests/ the seven stories
requirements.md   what this is meant to be, and why
```

---

## 11. Licence, and use at your own risk

Apache License 2.0. The full text is in `LICENSE`. Copyright 2026
Pedro A. Ortega.

**Use at your own risk.** This software moves money. It has not been
audited. It is provided as is, without warranty of any kind, and neither the
author nor any contributor is liable for any loss arising from its use,
including loss of funds. Sections 7 and 8 of the licence say this in the
words that bind; it is repeated here so that nobody has to reach section 7
to find it out. If you run this, the keys, the configuration and the money
are yours to look after.

Section 9 lists what is checked and what is assumed. Read it before trusting
this with anything you mind losing.

### Third-party code

The Go module links `github.com/ethereum/go-ethereum`, whose library
packages are LGPL-3.0-or-later. Everything else that compiles in is
permissive: MIT, BSD-2-Clause, BSD-3-Clause, Apache-2.0, and one package in
the public domain. Distributing source carries no LGPL obligation;
distributing a compiled binary carries those in LGPL-3.0 section 4, which
shipping the source satisfies.

`contracts/` builds against OpenZeppelin (MIT) and forge-std (MIT or
Apache-2.0), both git submodules, both used only for tests and the staging
bootstrap. Neither is part of the Go module and neither is on any production
path.

Apache-2.0 flows one way into GPL-3.0 and AGPL-3.0, so a copyleft host may
embed this library. It is not compatible with GPL-2.0-only.
