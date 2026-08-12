# JUICE-RAIL

Version: 0.3 (only MAJOR.MINOR)

INSTRUCTIONS: THIS FILE CONTAINS THE JUICE-RAIL REQUIREMENTS.
BEFORE ADDING ANYTHING, ALWAYS CHECK WHETHER THE EXISTING TEXT CAN BE REWRITTEN.
TONE MUST BE TERSE AND ALWAYS MINIMAL.

0. Rules

MINIMALITY IS CRUCIAL HERE: ONLY WHAT IS SMALL ENOUGH TO ENUMERATE CAN BE
TRUSTED WITH MONEY.

`juice-rail` is a minimal, self-contained stablecoin rail: a Go library for
host applications, plus a tiny standalone app as test and operations
harness. Juice is one host among others.

* The host is authoritative for its own ledger; the rail moves external
  money and reports finalized facts.
* A rail account is an EOA holding the stablecoin (money) and the native gas
  asset (operating reserve). Each account is its own vault.
* Money movement uses ordinary EVM mechanisms — ERC-20 balances and
  transfers, EOA nonces — never reproduced in a custom contract.
* An account funds its own gas from its own stablecoin. One account never
  pays another account's gas; no relayer, bundler, paymaster, shared
  treasury or privileged operator exists.
* Receiving money requires nothing from the recipient.
* Every operation is at-most-once; retry never duplicates.
* On any shortage nothing is signed or submitted; the account waits. Blocked
  is not lost. Every failure is loud.
* No stored observation of the chain is authoritative; only finalized facts
  may be cached. Write-ahead records are authoritative commitments.
* No floating-point accounting or stored exchange-rate state exists inside
  the rail.

1. Domains

A domain is one settlement environment:

```text
domain = (chain ID, stablecoin token address)
```

Per-domain configuration: RPC, finality mechanism, first block to observe,
swap venue, gas policy (MIN, MAX, slippage bound s in basis points, refill
fee bound B, and the gas bounds of a payment and of a refill).

Domains have independent balances and state. No bridging or cross-domain
netting. The configured stablecoin (USDT0) is the accounting asset; amounts
use its base units.

2. Accounts and onboarding

A rail account is one EOA whose key is local to its participant's client.
No central service submits its transactions. Host identity is outside the
rail.

On chain the account is fully described by:

```text
USDT0 balance    the user's money
ETH balance      the operating reserve
nonce            serialization of outgoing transactions
```

There is no rail balance mapping: the token ledger is the ledger.

Onboarding:

```text
1. create the EOA
2. fund it with USDT0
3. fund it with ETH >= the reserve minimum
4. wait for finality
```

This is the only point where ETH is normally acquired explicitly.

Accepted risk: key loss or compromise is terminal. Rotation is a transfer
of both balances to a fresh EOA.

3. Gas reserve

ORDINARY MIN/MAX HYSTERESIS, AND NOTHING ELSE:

```text
if ETH >= MIN: execute
otherwise:     refill to MAX, recheck, then execute
```

The level is compared to MIN. Nothing is projected, estimated or modelled:
covering the next transaction is what MIN is for. One threshold, one place, a
pure function. A second reserve rule anywhere is a defect; delete it. A
reserve under MIN is not a fault, it is the reorder point doing its job.

A payment is what takes the balance under MIN, and the refill comes after it,
so MIN must cover both:

```text
MIN >= (payment gas + swap gas) × p_ref
```

p_ref is a reference fee cap: a high percentile of the domain's historical
base fee, doubled per the formula below, plus tip. No constant MIN suffices
for every fee level, so refill transaction cost <= B is a trust-base
assumption and external ETH top-up is the recovery beyond it. MAX provides
runway so the account rarely rebalances.

Transaction fees follow go-ethereum's default, which the excess-refund rule
makes free to overstate:

```text
tip     = node suggestion (eth_maxPriorityFeePerGas), floor 1 wei
fee cap = tip + 2 × base fee
cost    = fee cap × gas bound
```

At most one outgoing transaction is in flight per account.

Refill is maintenance, not a verb. Sizing, from the venue quote:

```text
Δ = MAX − (e − g_s)     e ETH balance, g_s swap gas cost
q = quoted USDT0 input for exactly Δ ETH
x = q + ⌈q·s / 10000⌉   s slippage bound in basis points
```

One transaction: EIP-2612 permit for x plus an exact-output swap of Δ with
input bound x, pinned addresses, deadline, native-ETH output (unwrapped).
The allowance residue x − input is at most x, one refill's input bound; it
is held only against the pinned immutable router and is overwritten by the
next permit.

Gas bounds are configuration, not estimates: estimating requires the ETH the
policy is deciding whether to buy. Unused gas is not charged, so a generous
bound costs only a more conservative reserve, and a refill may overshoot MAX
harmlessly.

Shortages block before signing:

```text
ETH   < swap cost           wait for cheaper gas or external top-up
USDT0 < x + pending amount  wait for a deposit or better conditions
```

Venue requirements: immutable contracts, exact-output swaps, native-ETH
delivery, deep stablecoin/WETH liquidity on the domain. Pins, Uniswap V3 at
the 0.05% tier (verified on-chain before use):

```text
Arbitrum One      router 0xE592427A0AEce92De3Edee1F18E0157C05861564
                  quoter 0x61fFE014bA17989E743c5F6cB21bF9697530B21e
                  WETH   0x82aF49447D8a07e3bd95BD0d56f35241523fBab1
Arbitrum Sepolia  router 0x101F443B4d1b059569D643917553c771E1b9663E
                  quoter 0x2779a0CC1c3e0E44D2542EC3e79E3864Ae93Ef0B
                  WETH   0x980B62Da83eFf3D4576C647993b0c1D7faf17c73
```

Arbitrum One carries the first SwapRouter; Arbitrum Sepolia carries
SwapRouter02, which moves the deadline from the swap parameters into
multicall. Both encodings are supported and selected per domain. Neither
pool address is configured: quoting proves both that the pool exists and
that it has depth.

Arbitrum Sepolia has no USDT0 and no pool. A one-time bootstrap script
deploys an EIP-2612 MockUSDT0 and creates and seeds a real pool for it.

4. Deposit

A deposit is passive: any external party transfers USDT0 to the account
address and pays its own gas. The rail watches finalized ERC-20 `Transfer`
events. Deposit identity is

```text
(domain, transaction hash, log index)
```

so one finalized transfer is recognized exactly once. An exchange
withdrawal to the account address is a deposit; no second hop exists.

5. Transfer

`USDT0.transfer(recipient, amount)` submitted by the sender's account. The
sender pays gas; the recipient needs nothing, not even ETH.

6. Withdrawal

The same primitive with an external destination, supplied per operation;
there is no stored withdrawal address. The distinction from transfer is
semantic: the destination is outside the rail.

A withdrawal may take the whole balance. Leaving is not paying and does not
consult the reserve policy: a payment wants the reserve kept up, an exit does
not, and buying gas on the way out is money spent to leave. The reserve stays
behind, reachable with the account's key and by no verb here.

7. Identity, retry and confirmation

Before signing, the client durably records the intent:

```text
(operation ID, kind, destination, amount, nonce)
```

An intent binds to exactly one nonce. A retry reuses the same nonce and the
same economic terms; only transaction fee fields may change (Ethereum's
replacement mechanism). The chain admits one transaction per nonce, so an
intent executes at most once.

```text
confirmed   a finalized successful transaction matching the intent
failed      a finalized revert, or finalized state proves the nonce can no
            longer carry the intent
pending     otherwise
```

An ERC-20 `Transfer` carries no intent ID: the sender reports the
transaction hash to the host, which confirms against the finalized event.

After a crash or restore, the client assigns no new nonce until every nonce
below the finalized account nonce is matched to an intent by calldata. One signer per key; concurrent signers are excluded by
assumption.

8. Library

Go. A `Rail` instance binds one domain at construction; a host using
several domains holds several instances.

THE LIBRARY IS THE WHOLE RAIL. All chain access, all amount handling in the
token's own units, all checks that a domain is what it claims, and the
payment flow itself. A host adds configuration discovery and rendering, and
nothing else; logic a host has to write is logic missing from the library.

```text
account   balances (USDT0, ETH reserve)   nonce
pay (transfer, withdrawal), retry        prepare / send / refill beneath it
deposit observation   status   finality
domain check   amount parsing and formatting   funding checklist
```

Durable state, write-once or append-only, nothing else:

```text
1. write-ahead intent records: which operation owns which nonce
2. signed submissions, appended before broadcast
3. cache of finalized facts, including observed deposits
4. observation cursors: deposits scanned, nonces reconciled
```

The account key and the gas policy are inputs, not records.

Storage is an interface over exactly these records; a SQLite implementation
ships; hosts may substitute their own. The library reads no file,
environment variable or home directory; inputs arrive at construction.
Configuration discovery belongs to the app.

9. User experience

Participants see USDT0 amounts; ETH is never shown as money.

```text
Balance    1,250.00 USDT

Deposit    Transfer    Withdraw
```

Refill cost appears as a network cost reducing the USDT0 balance. After
onboarding the user normally never handles ETH.

10. Alternatives

Signed operations carried by designated relayers (V2): rejected; every
payment depended on an external ETH-holding carrier the design could not
name — a privileged operator in effect.

Custody contract with internal balances: rejected; duplicates the token's
ledger and requires contract verification without adding a property.

ERC-4337 / Safe / paymaster / bundler: rejected; a large trusted surface to
circumvent a gas model this design simply accepts.

Shared gas treasury: rejected; cross-subsidy and a shared liveness
dependency.

EIP-3009 deposit binding: unnecessary; a deposit is a plain transfer to the
account's own address.

Unlimited standing swap allowance: rejected; per-refill permit bounds any
residue to one refill's input bound.

Estimating gas to size the reserve: rejected; the estimate needs the ETH the
decision is about. Configured bounds only.

11. User stories

Exactly seven. Each runs against the standalone app on a local chain stack,
and the whole sequence is also driven once on a staging domain against a
real venue.

```text
1. onboard      create, fund USDT0 + ETH, finality, operational
2. deposit      external transfer credits exactly once by (tx, log index)
3. transfer     A pays B; B needs nothing; amounts move exactly once
4. withdraw     to an external destination
5. refill       reserve dips below MIN; sized refill executes; insufficient
                USDT0 blocks loudly with nothing signed
6. recovery     killed after signing; restart reconciles nonces from
                finalized history; retry duplicates nothing
7. leave        the whole balance goes out in one transfer, buying no gas
```

The standalone app is an operations and test harness, never a wallet
product.

12. Verification

There is no contract to verify. The gas policy is a pure decision function
over (ETH, USDT0, quote, pending operation) and is tested exhaustively; the
library is conventionally tested and exercised by the seven stories.

Trust base:

```text
finality is final
the pinned token is a correct ERC-20 with EIP-2612 permit
the pinned venue delivers exact output or reverts atomically
refill transaction cost <= B for liveness; beyond B, external top-up
one signer per key
RPC honest
```

13. Plan

Standalone-first: the library and app prove all seven stories before host
integration.

Layout:

```text
go/                  Rail implementation, gas policy, venue, submission,
                     confirmation
cmd/railctl/         standalone app
contracts/           test and staging fixtures only; no rail contract
deployments/         per-domain pinned addresses and gas policy
integration-tests/   complete local-chain scenarios
```

Every Go production file has a corresponding `_test.go`. `go build ./...`
and `go test ./...` pass without network access once dependencies are
fetched.
