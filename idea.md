## Conversation summary

### 1. Goal

We established that today’s work is the **Money Rail for Juice**.

The goal is to build the external money/settlement layer that connects Juice credits and inter-kernel obligations to real USDT0 settlement.

---

### 2. Juice kernel context

You shared the **Juice Kernel Requirements v0.12.0**.

The important architectural context for the rail is:

* Juice is a Go production kernel for callable actions.
* Execution runs through a single `Call()` primitive.
* The roles of process owner/payer, immediate caller, and action owner/payee are explicitly separated.
* Processes and traces contain bounded wallets using `available` and `locked`.
* `action.price` is a subtree bound.
* Monetary transitions and their corresponding audit records must be atomic.
* Juice already has local ledger operations for deposits, withdrawals, transfers, and federation settlement concepts.
* The kernel deliberately keeps execution semantics separate from supervision and external infrastructure.

The key implication for the Money Rail is that **the host application remains authoritative for its own internal ledger**, while the rail is an external settlement mechanism.

---

### 3. Money Rail specification

You then shared the **Juice USDT0 Account-Abstraction Rail Technical Specification**.

Its goals are to:

* accept USDT0 deposits for specific Juice users;
* hold settlement liquidity for Juice kernels;
* settle balances between kernels;
* make deposits, settlements, and withdrawals idempotent;
* avoid requiring account holders to manage ETH;
* keep the host authoritative for local credits and bilateral obligations;
* remain independently testable from Juice and any configured network.

### Fixed technology stack

The current specification fixes:

```text
Networks:         configured EVM networks
Initial mainnet:  Arbitrum One (42161)
Initial testnet:  Arbitrum Sepolia (421614)
Asset:            USDT0
Decimals:         6
Account model:    ERC-4337
EntryPoint:       v0.7
Smart account:    Safe v1.4.1-2
4337 module:      Safe4337Module v0.3.0
Ownership:        1 owner / threshold 1
Contracts:        OpenZeppelin 5.x
Solidity tooling: Foundry
Go Ethereum:      go-ethereum
```

The accounting unit is deliberately trivial:

```text
1 Juice credit = 1 USDT0 base unit
1,000,000 Juice credits = 1 USDT0
```

No floating point or exchange rate exists in the rail.

---

### 4. Rail architecture

The intended architecture is:

```text
Host (e.g. Juice kernel)
    │
    ▼
Go rail library
    │
    ├── ERC-4337 bundler
    └── verifying paymaster
             │
             ▼
      JuiceRail per domain
             │
             ▼
           USDT0
```

A **domain** is one `JuiceRail` deployment, identified by `(chain ID, contract address)`. Every project deploys its own copy on every network it uses. Domains are independent settlement domains: separate balances, identifiers, and liability — no bridging, no netting, no cross-domain state. Adding a domain is configuration and deployment, not a redesign.

Per-domain configuration:

```text
chain ID
token address          (USDT0 per network; a mock on testnets)
rail contract address
EntryPoint address
bundler URL
paymaster address + signer
finality mechanism
```

Gas and Safes: every rail operation — deposit, settle, withdraw — is a contract call, and whoever submits it needs gas unless sponsored. Gasless submission goes through ERC-4337: the actor’s funds sit in a Safe controlled by its rail key. Safe addresses are derivable before deployment, so USDT0 can arrive before the account exists on-chain. A plain EOA may act by paying its own gas. Only the deposit’s credited account and a withdrawal’s recipient are pure addresses needing nothing. A bare USDT0 transfer to the contract never credits anything. The paymaster’s ETH stake at the EntryPoint is operator infrastructure.

---

### 5. `JuiceRail` contract

One **non-upgradeable, adminless** contract artifact, deployed per (project, network).

Its state is:

```solidity
IERC20 public immutable usdt0;

mapping(address account => uint256) public balanceOf;
mapping(bytes32 id => bytes32 termsHash) public operations;   // one mapping, all kinds
```

One binding rule covers all three operations; the terms hash contains everything identifying the intent — **kind, domain, parties, amount**:

```text
same id + same terms      → no-op
same id + different terms → revert   (including reuse across kinds)
```

There is no stored `totalLiability`: no operation reads it, and solvency (`held >= sum of balances`) is checkable off-chain from events — stored derived state is banned in the contract exactly as off-chain.

#### Deposit

A payer calls approve + deposit; the contract pulls the USDT0 and credits a specific account balance. The payer’s address does **not** determine who is credited: the host has already bound the unpredictable deposit ID to a local user and amount.

#### Settlement

A debtor account moves part of its balance to a creditor account. A debtor can debit only itself; settlement preserves the sum of balances. The terms include the debtor (§16).

#### Withdrawal

An account withdraws against only its own balance:

```text
decreases account balance
transfers the same amount of USDT0 out
```

---

### 6. Gas sponsorship

The rail uses a verifying ERC-4337 paymaster.

Signing order resolves the self-reference (a signature cannot cover itself): the **paymaster signs first**, over the UserOperation with **both signature fields excluded** (its own slot, and the account’s — not yet present), plus:

* chain ID;
* EntryPoint;
* paymaster address;
* expiration;
* maximum gas cost.

The **account then signs** over the complete operation including the paymaster signature (canonical VerifyingPaymaster construction). The authorization expires after five minutes.

The paymaster is restricted to operations ultimately performing:

```text
USDT0 approve + JuiceRail.deposit
JuiceRail.settle
JuiceRail.withdraw
```

It must reject unrelated operations and never have access to rail balances or USDT0 itself.

---

### 7. Go boundary

A `Rail` instance is bound to **one domain at construction**; methods carry no network parameter, so mixing domains in one call path is impossible. A host using several domains holds several instances.

```go
Account
Balance

PrepareDeposit
SubmitDeposit
DepositStatus

Settle
SettlementStatus

Withdraw
WithdrawalStatus
```

The implementation operates in terms of opaque operations and `Transfer` statuses — properties of the **intent (the ID)**, never of one submission:

```text
unknown     no record of the ID
pending     may still execute — a revert is NOT failure; a reverted intent can be retried
confirmed   a finalized JuiceRail event matching (ID, terms) exists on the domain
failed      the intent can never execute: the ID is finalized-bound to different terms,
            or abandonment is recorded AND every signed op for the ID is finalized-dead
            (nonce consumed or authorization expired past finality — the re-sign
            rule's death test, reused)
```

The host releases reserved funds only on `failed`. Abandonment is a durable write-once decision — “never sign this ID again”. It stops future signing but kills nothing already signed, so release still waits for finalized proof that every signed op is dead.

Submission is idempotent by **operation ID**, uniform across all three kinds. The rail persists a write-ahead record (ID, terms, signed op) before submission, and the UserOperation hash and eventual transaction hash before reporting successful submission — both are **hints for where to look on-chain, never the definition of status**.

Operation identity is (domain, operation ID). Signatures and terms hashes bind the domain.

---

### 8. Confirmation model

One of the strongest requirements is that **bundler acceptance is not confirmation**.

A transfer becomes `Confirmed` only when a **finalized `JuiceRail` event matching (ID, terms)** exists on the domain — queried by the indexed ID, regardless of which transaction carried it. A no-op replay’s receipt carries no event, so a receipt-centric check would wait forever on an intent that already executed; the status lives with the ID, not with a submission.

Finality is the domain’s configured **finality mechanism** (default: the `finalized` tag). Only true finality qualifies; confirmation-count policies are rejected — the design has no reconciliation machinery, so confirmed must never revert.

Until then it remains pending. **No stored observation of the chain is authoritative**; the only cache is of finalized facts, which cannot change. The rail’s own decisions — write-ahead intent, signed ops, abandonment — are authoritative by construction: commitments, not claims about the chain.

Only after this confirmation does the host alter its own authoritative ledger.

---

### 9. Juice integration flows

#### User deposit

```text
Juice creates deposit ID
→ stores user + exact amount
→ rail prepares operation
→ payer signs (via its Safe, or an EOA paying its own gas)
→ rail submits
→ rail confirms exact event + finality
→ Juice credits user
```

The deposit ID becomes Juice’s `external_key`, giving the Juice ledger another idempotency boundary.

#### Kernel settlement

```text
Juice determines settlement ID + amount
→ the agreement binds (domain, creditor account, amount, ID) from signed federation data
→ debtor Safe executes settlement
→ rail confirms on exactly that domain
→ Juice applies bilateral cash settlement idempotently
```

#### Withdrawal

```text
Juice reserves/debits credits
→ kernel Safe submits withdrawal
→ rail confirms
→ Juice finalizes local withdrawal
```

Only `failed` — never a mere revert — releases the reserved credits (§7): the ID is provably dead, or abandonment is recorded and every signed op is finalized-dead.

---

### 10. Main rail invariants

The specification establishes:

```text
USDT0 held by JuiceRail >= sum(account balances)

settlement preserves the sum of account balances

withdrawal decreases held USDT0 and the account balance equally

an operation ID moves money at most once, across all kinds

conflicting ID reuse always fails

account A cannot debit account B

direct USDT0 transfers do not mint balances

unconfirmed blockchain operations never alter the host's ledger
```

---

## 11. Development strategy we agreed on

You raised the question of whether to hardwire this immediately into Juice or first build the Money Rail as a standalone system.

The recommendation was strongly in favor of **standalone-first**.

Not as a throwaway prototype, but as the real production rail implemented behind the intended interface.

The proposed development shape is:

```text
Standalone rail
      │
      ├── contracts
      ├── Safe / ERC-4337
      ├── paymaster
      ├── Go implementation
      ├── persistence / recovery
      └── CLI test harness

Juice
      │
      └── Rail interface + FakeRail initially
```

Then Juice switches from `FakeRail` to the production rail implementation once the rail has independently passed its tests.

This is also consistent with the specification itself, which explicitly says the contract repository must run independently from Juice.

---

## 12. User stories

The rail has **exactly six user stories. These are THE ONLY use cases; all work focuses on them.** Each runs against the live standalone app binary on a local chain stack — compiled surface only, like Juice’s flow tests.

```text
1. bootstrap         operator stands up a fresh domain, funds the paymaster, derives
                     Safe addresses pre-deployment; account holders need no ETH;
                     zero balances
2. deposit           a payer (own Safe gasless, or an EOA paying its own gas) funds
                     account A under a fresh deposit ID; status is pending until
                     finality, confirmed only after; host credits exactly once
3. settle            A pays B per agreed (domain, creditor account, amount, ID); conservation
                     holds; the same settlement on a different domain is refused
4. withdraw          B withdraws to an external address; balance and held USDT0 drop
                     equally; a failed withdrawal — terminal per §7, never a mere
                     revert — releases the host's reserve
5. retry & conflict  resubmitting the same operation moves money once and reports the same
                     outcome; the same ID with different terms fails loudly, nothing moves
6. crash recovery    killed after submission, restart resumes to exactly-once; an expired
                     op is re-signed only on finalized proof the old one is dead
```

The standalone app (`railctl`-style: account, deposit, balance, settle, withdraw, status) is the harness for all six — a production test and operational tool, never a wallet product.

Contract invariants (A cannot debit B; direct USDT0 transfers mint nothing) belong to the contract test suite, not the flow suite. Juice-specific flows belong to Juice’s own tests once it adopts the rail.

---

## 13. Proposed rollout sequence

We discussed the following staged development/deployment model:

```text
1. JuiceRail contract
2. Local ERC-4337/Safe/paymaster environment
3. Standalone Go rail
4. Arbitrum Sepolia
5. Security review / frozen release
6. Limited mainnet rail
7. Juice integration
```

For Juice integration itself, we discussed progressively enabling:

```text
disabled
→ shadow / observe-only
→ operator-only
→ limited users / amounts
→ general availability
```

Deposits, withdrawals, and kernel settlement can also be activated independently.

---

## 14. Repository decision

You asked whether the Money Rail deserved its own repository.

The answer was **yes**.

Your current repository layout is:

```text
juice/           kernel
juice-ui/        UI for the kernel
juice-services/  basic services shipped with Juice
juice-agent/     Juice agents
```

The proposed addition is:

```text
juice-rail/
```

Giving:

```text
juice/
juice-ui/
juice-services/
juice-agent/
juice-rail/
```

The reasoning is that the rail has its own:

* smart contracts;
* ERC-4337 integration;
* Safe infrastructure;
* paymaster;
* Go client;
* persistence/reconciliation logic;
* chain deployment lifecycle;
* security/audit boundary.

It is infrastructure for Juice rather than an ordinary callable Juice service.

---

## 15. Proposed responsibility split

### `juice-rail/`

Would own roughly:

```text
contracts/
    JuiceRail
    verifying paymaster

go/
    production Rail implementation
    ERC-4337 client
    chain confirmation
    persistence/recovery

cmd/railctl/
    standalone operational/test CLI

deployments/
    pinned Safe/EntryPoint/module/rail deployment data

integration-tests/
    complete AA + contract scenarios
```

### `juice/`

Would own only Juice-specific semantics:

```text
Rail interface
FakeRail
rail configuration

deposit state
withdrawal state
settlement state

local Juice ledger transitions
federation integration
```

So the dependency direction becomes:

```text
juice
   │
   ▼
Rail abstraction
   │
   ▼
juice-rail
   │
   ▼
ERC-4337 / selected EVM network / USDT0
```

The important architectural conclusion of the conversation is therefore:

> **Build `juice-rail` as an independently functional money system first, prove deposits, withdrawals, inter-kernel transfers, retries, recovery, and finality there, while developing Juice against the same `Rail` abstraction. Then integrate the proven rail into the kernel.**

---

## 16. Minimality audit

We reviewed the design under one rule: **if there is state, the transitions must be minimal so they can be verified.**

### Every intent executes or reverts

No intent may no-op against another intent’s binding. This is why the terms hash contains everything identifying the intent (§5): a settlement omitting the debtor would let a second debtor reusing an ID no-op silently — no transfer, no event, no error — and wait on `pending` forever. Foreign reuse must revert: loud and terminal.

### Re-signing rule

Paymaster authorizations expire after five minutes, so a stuck operation must eventually be re-signed — but a careless re-sign is a double-spend.

```text
re-presenting the same signed op    → always safe
signing a new op for the same id    → only on finalized evidence
                                       the previous op can never execute
```

The per-id durable state is an append-only list of signed ops, at most one not known-dead-at-finality. The contract binding backstops violations: a wrongful second op reverts rather than pays.

### Stated assumptions

```text
burned IDs      a different-terms front-run permanently blocks an ID; funds are
                never lost, but the host must be able to issue a fresh ID for retry
token exactness held >= sum of balances assumes USDT0 transfers exact amounts;
                a balance-delta check on deposit turns the assumption into a guarantee
```

### Resulting verification surface

```text
on-chain:   3 balance transitions + 1 binding rule over one mapping;
            1 invariant (held >= sum of balances) by 3-case induction
off-chain:  write-ahead, guarded re-sign, and abandonment — all write-once;
            status derived from the chain by ID; no stored observation is
            authoritative (finalized facts may be cached); stored decisions —
            intent, signed ops, abandonment — are authoritative commitments
host:       1 ledger transition per id, idempotent by external key;
            reserves released only on failed (terminal), never on a revert
```

Every layer is at-most-once under the same identifier. Retry is the recovery protocol: every retry is safe, every failure is loud.

---

## 17. Library-first reframing

`juice-rail` is a **general micropayment rail library** for any project; Juice is one consumer among others. The repo ships two deliverables:

```text
library    Go rail library + JuiceRail contract + paymaster
app        tiny standalone app using the library; independent; test/ops harness
```

The library defines a minimal storage interface — write-ahead operation records, the append-only signed-op log, the cache of finalized facts — and ships a SQLite implementation. The standalone app uses SQLite as-is; Juice plugs in its own store so rail records commit atomically with kernel records.

The contract keeps the name `JuiceRail` as a brand. The library vocabulary is generic: **account**, not kernel — for Juice an account is a kernel’s Safe; for another project, whatever party holds a balance. The **host application** is authoritative for its own ledger; the rail moves external money and reports finalized facts.

---

## 18. Settlement domain law

The debtor cannot choose the domain unilaterally — otherwise a real debt could be “paid” on a testnet deployment with free tokens. Rule:

```text
a settlement agreement binds (domain, creditor account, amount, settlement ID);
the creditor's host accepts the settlement only on the confirmed event
from exactly that domain
```

The confirmation model already checks the exact event on a specific contract; this makes it a stated law rather than a consequence.

Division of documents: `idea.md` is the design log; `requirements.md` is the specification — terse, minimal, no history.

---

## 19. Formal verification targets

Off-chain records are write-once or append-only; on-chain, the `operations` mapping is write-once and **only balances mutate**, under three transitions and one invariant. Verification therefore targets transitions and invariants, not transition graphs. Four targets, in decreasing value:

```text
1. JuiceRail      complete verification (Foundry invariants + symbolic checker):
                  3 transitions, write-once binding, conservation, self-debit-only,
                  held >= sum of balances
2. paymaster      the hardest target: its sponsorship predicate parses hostile
                  nested calldata ("these bytes are exactly one of the three
                  operation shapes, nothing else"); delegatecall and free-form
                  batching forbidden outright. Blast radius of a bypass: the
                  operator's ETH stake + rail outage — never account balances
3. re-sign rule   small model (TLA+-style): crash, restart, finalize, re-sign;
                  proves exactly-once AND liveness (no ID parks forever) — the
                  contract backstops safety but not liveness
4. trust base     stated assumptions all proofs are conditional on:
                  finality is final; token transfers exact amounts (discharged
                  by balance-delta check); Safe/EntryPoint as audited; RPC honest
```

The Go library and standalone app are not formally verified: they are guarded by the contract, exercised by the six user stories (§12), and tested conventionally.
