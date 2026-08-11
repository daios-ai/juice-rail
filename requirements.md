# JUICE-RAIL

Version: 0.2 (only MAJOR.MINOR)

INSTRUCTIONS: THIS FILE CONTAINS THE JUICE-RAIL REQUIREMENTS.
BEFORE ADDING ANYTHING, ALWAYS CHECK WHETHER THE EXISTING TEXT CAN BE REWRITTEN.
TONE MUST BE TERSE AND ALWAYS MINIMAL.

0. Rules

MINIMALITY IS CRUCIAL HERE TO ALLOW FOR FORMAL VERIFICATION!

`juice-rail` is a minimal, self-contained stablecoin rail: a Go library and
contracts for host applications, plus a tiny standalone app as test and
operations harness. Juice is one host among others.

* The host is authoritative for its own ledger; the rail moves external money
  and reports finalized facts.
* A rail account is an Ethereum address.
* The account holder authorizes money movement; transaction submission grants
  no authority over funds.
* No privileged operator is required for continued operation.
* Ordinary participants never need the chain's native gas currency.
* Every rail balance is fully backed by stablecoin held by the contract.
* If there is state, transitions are minimal so they can be verified.
* Every operation is at-most-once under (account, ID).
* Resubmitting the executed operation is a harmless replay; conflicting reuse
  reverts.
* Retry is the recovery protocol: every retry is safe, every failure loud.
* No stored observation of the chain is authoritative; only finalized facts
  may be cached. Stored decisions (intent, signed operations, abandonment)
  are authoritative commitments.
* Prefer established mechanisms over custom protocols.

1. Domains and accounting

A domain is one `JuiceRail` deployment:

```text
domain = (chain ID, JuiceRail contract address)
```

Domains have independent balances, IDs and liability. No bridging, netting or
cross-domain state.

The configured stablecoin is the accounting asset. Amounts use its base units.
No floating point or exchange rate exists inside JuiceRail.

Per-domain configuration: chain ID, token address, rail contract address, RPC
and finality mechanism.

2. Accounts and authorization

A JuiceRail account is an Ethereum address. The holder's EOA signature
(ecrecover) is the only debit authority. EIP-1271 is not supported:
verification executes no foreign code.

Host identity is outside JuiceRail. A host may bind its own identity to a
rail address.

Money operations use EIP-712. The domain separator binds (chain ID, contract
address); each kind is a distinct struct type, so the type hash discriminates
kinds. Signed terms per kind:

```text
deposit    ID, payer, credited account, amount, fee, relayer, validity
transfer   ID, sender, recipient, amount, fee, relayer, validity
withdraw   ID, account, destination, amount, fee, relayer, validity
```

`validity` is a deadline; past it the operation cannot execute.

Accepted risk: key loss or compromise is terminal for an account. Rotation is
a signed transfer of the balance, less its relay fee, to a fresh address.

3. Contract

One non-upgradeable, adminless artifact `JuiceRail`.

```solidity
IERC20 public immutable token;
mapping(address account => uint256) public balanceOf;
mapping(address account => mapping(bytes32 id => bytes32 termsHash)) public operations;
```

`operations` is keyed by the authorizing (debited) account, so no other
account can bind or burn an ID. It is written once, at execution, with the
EIP-712 struct hash of the executed terms.

The only money operations:

```text
deposit     external stablecoin → account; fee → relayer
transfer    account → account; fee → relayer
withdraw    account → signed destination; fee → relayer
```

The fee credits the relayer's rail balance. An executing operation emits one
event with indexed account and ID and full terms; a replay emits nothing.
There is no stored liability total: solvency is checkable off-chain from
events.

Invariants:

```text
held stablecoin >= sum of balances
deposit increases held and the sum of balances equally
transfer preserves the sum of balances
withdraw decreases held and the sum equally
(account, ID) moves money at most once; conflicting reuse reverts
an account cannot debit another account
the transaction sender alone has no debit authority
direct token transfers credit nothing (the token balance is read only
  transiently inside deposit to measure the pulled amount)
```

4. Relaying and gas

A relayer submits a signed operation and fronts native gas. Only the
designated relayer may submit; submitting for oneself means naming oneself.
A signed operation is a bearer instrument until its deadline, so naming its
carrier keeps a leaked authorization inert to everyone else.

The relayer quotes a stablecoin fee before signing. The account holder signs
relayer and fee as operation terms; no other relayer can take the fee.

On success the debited account pays amount + fee and the fee credits the
relayer's rail balance.

JuiceRail contains no gas oracle, native-token accounting, exchange
mechanism, fixed fee or fee auction.

Before submission the relayer verifies signature, ID and balance and
simulates the exact transaction. If execution nevertheless fails, the relayer
bears the native-gas cost; relayers price this residual risk into fees.

5. Deposit

The payer signs twice:

```text
1. EIP-3009 receiveWithAuthorization for amount + fee, payee JuiceRail
2. the EIP-712 deposit terms
```

The payee restriction stops the authorization executing outside deposit and
stranding funds; `transferWithAuthorization` is rejected. The terms signer
and the authorization's `from` must be one address; the rail deadline is the
authorization's `validBefore`, with `validAfter` zero.

The contract checks the (payer, ID) binding, pulls the tokens, checks its own
balance delta equals amount + fee, credits the account and pays the fee. The
payer's address never determines the credited account.

An exchange deposit is two-stage:

```text
exchange → payer Ethereum address
payer → JuiceRail by EIP-3009 deposit
```

A bare token transfer to the contract credits no account.

The participant needs no native gas currency.

6. Transfer

The sender signs the transfer terms; the designated relayer submits.

```text
sender     -= amount + fee
recipient  += amount
relayer    += fee
```

The transaction sender cannot alter signed terms or debit an account without
its holder's signature.

7. Withdrawal

The account holder signs the withdrawal, including its external destination;
an exchange address may be the destination directly.

```text
account      -= amount + fee
held         -= amount
destination  receives amount externally
relayer      += fee
```

The destination is a signed term; there is no stored withdrawal-address
state.

8. Operation identity, retry and replacement

Operation identity is (domain, account, ID). The ID binds at execution, to
the executed terms.

```text
resubmit the executed operation       → no-op
other terms under a bound (acct, ID)  → revert
```

The binding is checked before any other validation: an executed operation's
replay no-ops even after its deadline or token-nonce consumption.

An intent may have several signed variants under one ID, differing only in
relayer, fee and validity; the write-ahead record (§10) fixes the core terms:

```text
deposit    payer, credited account, amount
transfer   sender, recipient, amount
withdraw   account, destination, amount
``` The contract executes at most one variant, so
replacing an unresponsive relayer needs no waiting: sign a new variant. A
losing variant reverts at its relayer's gas cost.

9. Confirmation

Submission and inclusion are not confirmation.

`confirmed` means a finalized `JuiceRail` event matching (account, ID) and
the intent's core terms exists on the exact domain, located by the indexed
fields regardless of which transaction carried it. True finality only;
confirmation-count policies are rejected, since confirmed must never revert.

The host alters its ledger only on confirmed and releases reserves only on
failed.

10. Library

Go. A `Rail` instance binds one domain at construction; a host using several
domains holds several instances.

```text
account  balance
prepare / sign / submit   (deposit, transfer, withdrawal)
status  event observation  finality
```

Statuses are properties of the intent (account, ID), never of one submission:

```text
unknown     no record of the intent
pending     may still execute; a revert is not failure
confirmed   a finalized matching event exists
failed      the intent can never execute: the ID is finalized-bound to other
            core terms, or abandonment is recorded AND every signed variant's
            validity expired past finality
```

Durable state per intent, write-once or append-only, nothing else:

```text
1. write-ahead record (account, ID, core terms) before any signature
2. append-only list of signed variants
3. abandonment: never sign this intent again; kills nothing already signed
4. cache of finalized facts
```

A signed variant is durable before submission. Transaction hashes are hints
for locating events, never status. Storage is an interface over exactly these
four records; a SQLite implementation ships; hosts may substitute their own.

A separate relayer service is not required: any party with native gas may
relay a valid operation.

The library reads no file, environment variable or home directory. Inputs
arrive at construction; configuration discovery belongs to the app.

11. User experience

Normal participants see stablecoin amounts and relay fees only.

```text
DEPOSIT
Deposit       100.00
Fee             0.02

TRANSFER
Send Bob       10.00
Fee             0.01
Total           10.01

WITHDRAW
Withdraw       20.00
Fee             0.01
You receive    20.00
```

Ordinary participants need not understand or hold native gas currency.

12. Alternatives

Safe + ERC-4337 paymaster: rejected; adds Safe, EntryPoint, paymaster,
allowances and separate gas reserves.

Separate stablecoin gas reserve: rejected; a funded rail account must not
depend on a second user balance to transact.

Privileged operator: rejected; disappearance of one party must not freeze
other accounts.

Fixed gas fee or custom auction: rejected; relayers quote fees externally.

On-chain P2P identity: rejected; identity belongs to the embedding host.

EIP-1271 contract signatures: rejected; authorization must not execute
foreign code.

Permissionless submission: rejected; only the designated relayer may carry a
signed operation, and liveness needs no more than signing another variant.

Stored withdrawal address: rejected; the destination is a signed term.

Waiting out a stalled relayer before re-signing: rejected; the write-once
(account, ID) binding already serializes variants.

Shared exchange deposit address: rejected; a plain token transfer cannot bind
the deposit to a rail account.

13. User stories

Exactly six. Each runs against the standalone app on a local chain stack.

```text
1. bootstrap         deploy a fresh domain; zero balances; no privileged
                     operator
2. deposit           wallet or exchange-originated stablecoin credits an
                     account; the payer holds no native gas
3. transfer          A signs payment to B; relayer submits; amount and fee
                     move exactly once
4. withdraw          account signs withdrawal to a signed destination; held
                     and balances drop equally; fee pays the relayer
5. retry & conflict  resubmission moves nothing twice; a losing variant and
                     conflicting reuse revert loudly
6. crash recovery    killed after submission, restart resumes to
                     exactly-once; an unresponsive relayer is replaced by
                     signing a new variant, no waiting
```

The standalone app is an operations and test harness, never a wallet product.

14. Verification

Off-chain records are write-once or append-only; on-chain, `operations` is
write-once and only balances mutate. Verification targets transitions and
invariants, not transition graphs.

```text
1. JuiceRail    complete verification (Foundry invariants + symbolic
                checker): 3 transitions, per-account write-once binding,
                conservation, signature-gated debits, held >= sum of balances
2. variants     small model: several signed variants under one intent execute
                at most once; abandonment plus expiry past finality is
                terminal (no intent parks forever)
3. trust base   finality is final; the pinned token implements EIP-3009 as
                specified; the balance-delta check discharges exact incoming
                amounts; outgoing transfers deliver exact amounts; ecrecover;
                RPC honest
```

The Go library and app are not formally verified: guarded by the contract,
exercised by the six stories, tested conventionally.

15. Plan

Standalone-first: the library and app prove all six stories before host
integration.

Layout:

```text
contracts/           JuiceRail
go/                  Rail implementation, signatures, EVM submission,
                     confirmation
cmd/railctl/         standalone app
deployments/         per-domain deployment data
integration-tests/   complete local-chain scenarios
```

Every Go production file has a corresponding `_test.go`. `go build ./...` and
`go test ./...` pass without network access once dependencies are fetched.
