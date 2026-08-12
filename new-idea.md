# JuiceRail — Self-Funded EVM Rail

## Goal

Provide a minimal money rail with exactly three user-facing verbs:

```text
deposit
transfer
withdraw
```

The normal UX is denominated entirely in USDT0. ETH exists only as operating fuel and should disappear from normal use after onboarding.

The rail must require no bundler, paymaster, relayer, active operator, shared gas treasury, or custom chain for transaction liveness.

## Constraints

1. On current Ethereum-style EVM networks, the account submitting a transaction must pay native gas. On Ethereum this is ETH.
2. Receiving an ERC-20 transfer requires no action by the recipient; the sender submits the transaction. ERC-20 directly provides balances and transfers between addresses.
3. Therefore every JuiceRail account that must independently send money must maintain its own native-gas reserve.
4. Joining JuiceRail requires funding the JuiceRail account with **both USDT0 and ETH on the configured domain**.
5. After onboarding, JuiceRail automatically maintains the ETH reserve from the account's own USDT0.
6. One account never pays another account's gas.
7. No floating-point accounting. USDT0 remains the user-visible monetary asset.
8. Only finalized chain facts are authoritative.
9. Retry must never duplicate a transfer.
10. The design should use ordinary EVM mechanisms wherever possible rather than reproduce them in JuiceRail.

## Rationale

This deliberately accepts Ethereum's native-gas model instead of constructing infrastructure to circumvent it.

The standard EVM pattern is already sufficient:

```text
one account
├── ERC-20 asset     value
└── native asset     execution fuel
```

Ethereum transactions are initiated by EOAs and require gas; ERC-20 already defines account balances and transfers.

Maintaining a small native-asset reserve is also an established operational pattern. Fireblocks, for example, monitors token-holding vault accounts and refuels their native gas asset when it falls below a threshold. JuiceRail applies the same **min/max inventory principle**, but each account funds its own reserve from its own USDT0 instead of depending on a central Gas Station.

The consequence is important:

> **Each JuiceRail account is its own vault.**

There is no need for ERC-4337, Safe4337Module, a bundler, a paymaster, a shared ETH pool, or a JuiceRail custody contract merely to implement deposit, transfer, and withdrawal.

## Minimal design

### Domain

A domain identifies one EVM settlement environment:

```text
chain ID
USDT0 token address
native gas asset          ETH initially
RPC
finality mechanism
swap venue/configuration
```

Domains are independent. Funds and gas on one domain do not satisfy another.

### Rail account

Every participant has one standard EVM EOA controlled by its local JuiceRail software.

```text
RailAccount
    address
    private key

On chain:
    USDT0 balance
    ETH balance
```

The private key is local to that participant. No central JuiceRail service is required to submit its transactions.

The EOA's USDT0 balance is the user's money.

The EOA's ETH balance is the user's operating reserve.

No corresponding JuiceRail balance mapping exists: duplicating the token's own ledger would add state without adding a required property.

### Onboarding

Joining has one explicit cost:

```text
1. create JuiceRail EOA
2. user sends USDT0 to that address
3. user sends ETH to that address
4. wait for finality
5. account becomes operational
```

The UI should present this as **funding a Juice account**, with a simple checklist for the two required assets.

The initial ETH deposit must satisfy the configured operating-reserve minimum.

This is the only point where ETH must normally be acquired explicitly by the user.

## Gas reserve

Each account maintains:

```text
MIN < ETH reserve target < MAX
```

The account spends ETH normally.

Before an outgoing transaction:

```text
if ETH < MIN:
    refill ETH to approximately MAX
    then execute

otherwise:
    execute
```

MIN is the trigger; MAX is where a refill puts the reserve back. The
transaction's own cost is not projected against MIN — a payment may leave the
reserve under MIN, and the next outgoing operation refills it.

`MIN` is a safety reserve, not merely a percentage of wealth. It must conservatively cover the transactions required to replenish the reserve plus operating margin.

`MAX` provides runway and hysteresis so the account does not constantly rebalance.

### Refill

Refill is maintenance, not a fourth banking verb:

```text
account USDT0
      ↓
configured DEX
      ↓
account ETH

ETH → MAX
```

The account itself submits the swap and therefore pays its own gas.

The USDT0 spent acquiring ETH is the economic cost of operating the account. The normal UI may hide ETH and simply reflect this as a reduction in the user's USDT0 wealth/network cost.

The refill is sized at refill time from the venue's quote, the slippage bound `s`, and the swap's own gas cost `g_s`:

```text
Δ = MAX − (e − g_s)      ETH the swap must deliver
q = quoted USDT0 input for exactly Δ ETH
x = q · (1 + s)          maximum USDT0 input
```

The swap requests exactly `Δ` ETH with maximum input `x`.

If `u < x + amount(pending payment)`, nothing is signed or submitted: the account waits for a deposit or better conditions.

Initial domains use direct Uniswap V3 USDT0/WETH at the 0.05% fee tier:

```text
Arbitrum One     router 0xE592427A0AEce92De3Edee1F18E0157C05861564
                 quoter 0x61fFE014bA17989E743c5F6cB21bF9697530B21e
                 pool   0x641C00A822e8b671738d32a431a4Fb6074E5c79d
                 WETH   0x82aF49447D8a07e3bd95BD0d56f35241523fBab1
Arbitrum Sepolia factory 0x248AB79Bbb9bC29bB72f7Cd42F17e054Fc40188e
                 router 0x101F443B4d1b059569D643917553c771E1b9663E
                 quoter 0x2779a0CC1c3e0E44D2542EC3e79E3864Ae93Ef0B
                 WETH   0x980B62Da83eFf3D4576C647993b0c1D7faf17c73
```

On Sepolia, deploy an EIP-2612 MockUSDT0 and create, seed and pin its pool.

A refill must have:

```text
bounded input
bounded slippage
pinned token addresses
pinned/approved swap target
deadline
```

JuiceRail contains no exchange-rate ledger and takes no ETH-price exposure.

If automated refill becomes impossible while sufficient ETH remains, the client retries or uses another configured route. If the account actually exhausts ETH, external ETH top-up is the recovery mechanism.

## Deposit

A deposit is passive:

```text
Kraken / Coinbase / external wallet
                │
                │ USDT0
                ▼
        user's JuiceRail EOA
```

The external sender submits the transaction and pays its gas.

JuiceRail watches for the finalized ERC-20 `Transfer` event to the account. No JuiceRail transaction is required to receive the deposit. ERC-20 transfers are the standard token movement mechanism.

Deposit identity is:

```text
(domain, transaction hash, log index)
```

so one finalized token transfer is recognized exactly once.

After onboarding, ordinary deposits require only USDT0; the existing ETH reserve keeps the account operational.

## Transfer

A transfer means a payment to another JuiceRail account:

```text
Alice JuiceRail EOA
        │
        │ USDT0.transfer(Bob)
        ▼
Bob JuiceRail EOA
```

Alice's account submits the transaction.

Alice pays the ETH gas.

Bob pays nothing to receive it.

There is no intermediary ledger movement, relayer, sweep, or operator.

Transfer and withdrawal deliberately use the standard ERC-20 `transfer` primitive.

## Withdrawal

A withdrawal is the same on-chain primitive with an external destination:

```text
Alice JuiceRail EOA
        │
        │ USDT0.transfer(external address)
        ▼
Kraken / Coinbase / wallet
```

Alice's account submits the transaction and pays the ETH gas.

The distinction between `transfer` and `withdraw` is semantic:

```text
transfer    destination is another JuiceRail account
withdraw    destination is outside JuiceRail
```

No separate blockchain mechanism is required.

The destination needs no ETH merely to receive USDT0.

## Submission and retry

Outgoing operations use the EOA's native transaction nonce.

Before signing, JuiceRail durably records:

```text
operation ID
kind
destination
amount
account nonce
```

A retry of an unconfirmed transaction reuses the **same nonce and same economic terms**; only transaction fee fields may change.

This uses Ethereum's existing nonce/replacement mechanism rather than implementing a second on-chain operation-ID system.

Once the transaction is finalized, the operation is confirmed.

A finalized revert is failed.

A transaction that has not finalized remains pending unless finalized chain state proves its nonce can no longer execute.

## User-visible balance

The normal interface remains:

```text
Balance: 1,250.00 USDT

Deposit
Transfer
Withdraw
```

ETH is not included as spendable USDT balance.

It is an operating reserve belonging to that same account.

When USDT0 is converted into ETH for gas, the cost belongs entirely to that account. There is no cross-subsidy and no dependence on which user happened to trigger a shared treasury refill.

## Minimal state

JuiceRail itself needs only durable local operational records:

```text
account key/address
outgoing operation write-ahead records
finalized transaction facts
gas-policy configuration
```

Authoritative monetary state is already on-chain:

```text
USDT0.balanceOf(account)
ETH balance of account
transaction nonce
finalized transactions/events
```

No custom `JuiceRail` contract is required for the three verbs.

## Invariants

```text
a participant can execute without another Juice participant being online

a participant spends only its own ETH for gas

a participant spends only its own USDT0

receiving USDT0 requires no ETH from the recipient

transfer and withdrawal use ordinary ERC-20 transfers

gas refill consumes only the same account's USDT0

normal operation never depends on a shared ETH reserve

retry cannot intentionally change an operation's economic terms

only finalized chain facts become confirmed

loss of every external JuiceRail service does not prevent an adequately
funded participant from constructing and broadcasting its own transaction
```

The design therefore accepts one unavoidable requirement—**initial ETH funding**—in exchange for removing the much larger liveness and trust dependencies introduced by sponsored execution.

## Analysis

The replenishment mechanism is sound as stated. Three notes complete it:

1. No fixed MIN guarantees operation under every gas price; the design
   already names external ETH top-up as the recovery, so this is a stated
   assumption, not a defect. Insufficient USDT0 for a refill is ordinary
   insufficient funds: fail loudly.
2. Outgoing transactions serialize; after a refill finalizes, recheck
   balances before sending. Implementation rules, nothing more.
3. Two spec obligations outside the refill mechanism: an ERC-20 `Transfer`
   carries no intent ID, so payment attribution needs the sender to report
   the transaction hash to the host; and after a restore, reconcile nonces
   against finalized history before signing.
