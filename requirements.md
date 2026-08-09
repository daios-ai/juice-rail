JUICE-RAIL
==========

Version: 0.1 (only MAJOR.MINOR)

INSTRUCTIONS: THIS FILE CONSTAINS THE JUICE-RAIL REQUIREMENTS.
BEFORE ADDING ANYTHING, ALWAYS CHECK WHETHER THE EXISTING TEXT CAN BE REWRITTEN.
TONE MUST BE TERSE AND ALWAYS MINIMAL.

0. Rules

MINIMALITY IS CRUCIAL HERE TO ALLOW FOR FORMAL VERIFICATION!

`juice-rail` is a minimal, self-contained money rail: a Go library and
contracts that move USDT0 on EVM networks for a host application, plus a
tiny standalone app using the library as its test and operations harness.
Juice is one host among others.

- The users' persona: not crypto-savvy. They know how to move USTD from
  one account to the other on Ethereum, but that's it. They should NEVER
  be thinking about ETH.
- The host is authoritative for its own ledger; the rail moves external
  money and reports finalized facts.
- If there is state, transitions are minimal so they can be verified.
- Every operation is at-most-once under its ID at every layer.
- Every intent executes or reverts; no intent no-ops against another's
  binding.
- No stored observation of the chain is authoritative; only finalized
  facts may be cached (they cannot change). Stored decisions — intent,
  signed ops, abandonment — are authoritative commitments.
- Retry is the recovery protocol: every retry is safe, every failure loud.

1. Stack and accounting

```text
Networks:         configured EVM networks
Initial mainnet:  Arbitrum One (42161)
Initial testnet:  Arbitrum Sepolia (421614)
Asset:            USDT0 (6 decimals)
Account model:    ERC-4337, EntryPoint v0.7
Smart account:    Safe v1.4.1-2 + Safe4337Module v0.3.0, 1 owner / threshold 1
Contracts:        Solidity, OpenZeppelin 5.x, Foundry
Client:           Go, go-ethereum
```

`1 credit = 1 USDT0 base unit`. No floating point, no exchange rate.

2. Domains

A **domain** is one `JuiceRail` deployment, identified by
`(chain ID, contract address)`. Every project deploys its own copy per
network it uses. Domains are independent: separate balances, IDs, and
liability; no bridging, no netting, no cross-domain state.

Per-domain configuration: chain ID, token address, rail contract address,
EntryPoint address, bundler URL, paymaster address + signer, finality
mechanism.

3. Contract

One non-upgradeable, adminless artifact `JuiceRail`.

```solidity
IERC20 public immutable usdt0;
mapping(address account => uint256) public balanceOf;
mapping(bytes32 id => bytes32 termsHash) public operations;   // all kinds
```

Terms hashes are exact per kind (`debtor` = `msg.sender` at execution):

```text
deposit:  keccak256(abi.encode(DEPOSIT,  chainid, address(this), account, amount))
settle:   keccak256(abi.encode(SETTLE,   chainid, address(this), debtor, creditor, amount))
withdraw: keccak256(abi.encode(WITHDRAW, chainid, address(this), account, to, amount))
```

```text
same id + same terms      → no-op
same id + different terms → revert    (including reuse across kinds)
```

The only state-changing paths:

```text
deposit(id, account, amount)    pulls amount USDT0 (approve + transferFrom,
                                balance-delta checked), credits account; the
                                payer's address never determines the credited
                                account
settle(id, creditor, amount)    moves amount from msg.sender to creditor;
                                a debtor debits only itself
withdraw(id, to, amount)        debits msg.sender, transfers amount out
```

An executing operation emits exactly one event with the indexed `id` and
full terms; a no-op replay emits nothing. There is no stored liability
total: solvency is checkable off-chain from events.

Invariants:

```text
held USDT0 >= sum of balances
settle preserves the sum of balances; deposit/withdraw change held and the
  sum equally
an ID moves money at most once, across all kinds; conflicting reuse reverts
account A cannot debit account B
direct USDT0 transfers mint nothing (the token balance is read only
  transiently inside deposit to measure the pulled amount; no stored
  state ever derives from the standing balance)
```

4. Gas sponsorship

A verifying ERC-4337 paymaster sponsors gas. Sponsorship predicate: the
UserOperation performs exactly one of

```text
USDT0 approve + JuiceRail.deposit
JuiceRail.settle
JuiceRail.withdraw
```

and nothing else. Delegatecall is permitted only to the pinned
MultiSendCallOnly library, carrying exactly the approve+deposit pair (the
Safe must delegatecall it so the approval originates from the Safe); every
other target is a plain call. Free-form batching is rejected.

Signing order (a signature cannot cover itself): the paymaster signs first,
over the UserOperation with both signature fields excluded plus (chain ID,
EntryPoint, paymaster address, expiration, maximum gas cost); the account
signs second over the complete operation. Authorization expires after five
minutes. A predicate bypass risks the operator's ETH stake and rail outage,
never account balances.

The paymaster's owner (the operator) has exactly two powers: rotate the
verifying signer and manage its own EntryPoint stake. `JuiceRail` alone
is adminless.

Every rail operation is a contract call. Gasless submission requires the
actor's funds in a Safe controlled by its rail key (address derivable
before deployment, so USDT0 may arrive first). An EOA may act paying its
own gas. A deposit's credited account and a withdrawal's recipient are
plain addresses. A bare USDT0 transfer to the contract never credits.

5. Library

Go. A `Rail` instance binds one domain at construction; methods carry no
network parameter — a host using several domains holds several instances.

```go
Account  Balance
PrepareDeposit  SubmitDeposit  DepositStatus
Settle  SettlementStatus
Withdraw  WithdrawalStatus
```

Statuses are properties of the intent (the ID), never of one submission:

```text
unknown     no record of the ID
pending     may still execute; a revert is not failure
confirmed   a finalized JuiceRail event matching (ID, terms) exists
failed      the intent can never execute: the ID is finalized-bound to
            different terms, or abandonment is recorded AND every signed
            op for the ID is finalized-dead
```

Durable state per ID — write-once or append-only, nothing else:

```text
1. write-ahead record (ID, terms) before any signature
2. append-only list of signed ops; a new signature only on finalized
   evidence the previous op can never execute (nonce consumed, or
   authorization expired past finality)
3. abandonment: "never sign this ID again" — stops future signing,
   kills nothing already signed
4. cache of finalized facts
```

The signed op is durable before submission; a transaction hash exists only
after inclusion and is recorded when observed. Both hashes are hints for
locating events, never status. Storage is an interface over exactly the
four records above; a SQLite implementation ships; hosts may substitute
their own store.

6. Confirmation

Bundler acceptance is not confirmation. `confirmed` means: a finalized
`JuiceRail` event matching (ID, terms) exists on the domain, queried by
the indexed ID regardless of which transaction carried it. Finality is
the domain's configured mechanism (default: the `finalized` tag); true
finality only — confirmation-count policies are rejected, since confirmed
must never revert.

The host alters its ledger only on `confirmed` and releases reserves only
on `failed`.

7. Host integration

```text
deposit      host creates the ID and binds user + amount; payer signs;
             rail submits; host credits on confirmed. The ID is the
             host's idempotency key (Juice: external_key).
settlement   the agreement binds (domain, creditor account, amount, ID);
             the creditor's host accepts only the confirmed event from
             exactly that domain — a real debt cannot be paid on a
             testnet deployment.
withdrawal   host reserves credits; rail submits; host finalizes on
             confirmed and releases the reserve only on failed.
```

A burned ID (different-terms front-run) loses no funds; the host issues a
fresh ID. Accepted risk: burning costs gas per round and needs
pre-inclusion visibility, absent on the initial networks (private
sequencer feed). Commit-reveal only if a public-mempool network is ever
configured.

8. User stories

Exactly six; THESE ARE THE ONLY USE CASES. Each runs against the live
standalone app binary on a local chain stack — compiled surface only.

```text
1. bootstrap         operator stands up a fresh domain, funds the paymaster,
                     derives Safe addresses pre-deployment; account holders
                     need no ETH; zero balances
2. deposit           a payer (own Safe gasless, or an EOA paying gas) funds
                     account A under a fresh deposit ID; pending until
                     finality, confirmed only after; host credits exactly once
3. settle            A pays B per agreed (domain, creditor account, amount,
                     ID); conservation holds; the same settlement on a
                     different domain is refused
4. withdraw          B withdraws to an external address; balance and held
                     USDT0 drop equally; a failed withdrawal — terminal,
                     never a mere revert — releases the host's reserve
5. retry & conflict  resubmitting the same operation moves money once and
                     reports the same outcome; the same ID with different
                     terms fails loudly, nothing moves
6. crash recovery    killed after submission, restart resumes to
                     exactly-once; an expired op is re-signed only on
                     finalized proof the old one is dead
```

The standalone app (account, deposit, balance, settle, withdraw, status)
is the harness for all six — an ops and test tool, never a wallet product.
Contract invariants belong to the contract suite; host-specific flows to
the host's own tests.

9. Verification

Off-chain records are write-once or append-only; on-chain, `operations`
is write-once and only balances mutate. Verification targets transitions
and invariants, not transition graphs.

```text
1. JuiceRail      complete verification (Foundry invariants + symbolic
                  checker): 3 transitions, write-once binding, conservation,
                  self-debit-only, held >= sum of balances
2. paymaster      the hardest target: the sponsorship predicate parses
                  hostile nested calldata; delegatecall and free-form
                  batching forbidden outright
3. re-sign rule   small model: crash, restart, finalize, re-sign; proves
                  exactly-once AND liveness (no ID parks forever)
4. trust base     finality is final; token transfers exact amounts
                  (discharged by the balance-delta check); Safe/EntryPoint
                  as audited; RPC honest
```

The Go library and app are not formally verified: guarded by the contract,
exercised by the six stories, tested conventionally.

10. Plan

Standalone-first: the library and app prove all six stories before any
host integrates; hosts develop against the `Rail` interface with a fake.

Rollout: contract → local AA environment → Go rail → Arbitrum Sepolia →
security review / frozen release → limited mainnet → host integration
(shadow → operator-only → limited → general availability).

Layout:

```text
contracts/           JuiceRail, verifying paymaster
go/                  Rail implementation, 4337 client, confirmation,
                     persistence
cmd/railctl/         standalone app
deployments/         pinned per-domain deployment data
integration-tests/   full AA + contract scenarios
```

Every Go production file has a corresponding `_test.go`. `go build ./...`
and `go test ./...` pass without network access once dependencies are
fetched; chain scenarios run against a local stack.
