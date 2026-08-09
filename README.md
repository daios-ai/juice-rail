# juice-rail

A settlement rail for micropayments: an on-chain vault that holds USDT0 and
keeps one balance per account, plus a Go library that drives it. Juice is one
host among others.

Money moves three ways — **deposit** in, **settle** between two accounts,
**withdraw** out. Every operation carries an identifier the vault remembers, so
a repeat is a no-op and a reused identifier with different terms is refused.
The host stays authoritative for its own ledger; the rail moves external money
and reports finalized facts.

`requirements.md` is the specification. This file is how to run it.

## Requirements

| | |
|---|---|
| Go | 1.22 or later |
| [Foundry](https://getfoundry.sh) | `forge`, `anvil` — contracts and the story tests |
| [Halmos](https://github.com/a16z/halmos) | optional, for the proofs |

```sh
git clone --recurse-submodules https://github.com/daios-ai/juice-rail
cd juice-rail && go build ./...
```

Already cloned without submodules: `git submodule update --init --recursive`.

## Verifying a build

```sh
go test ./...                                  # library, offline
go test -race ./...                            # same, under the race detector
cd contracts && forge test                     # 53 unit, fuzz and invariant tests
go test -tags integration ./integration-tests/ # the six user stories, needs anvil
```

The stories build `railctl` and drive it against a real EntryPoint and Safe on
a local chain. They are opt-in: without `-tags integration` they do not run,
and if `anvil` is missing they fail rather than pass quietly.

The proofs need Halmos in its own environment, so it cannot disturb the rest of
your Python setup:

```sh
python3 -m venv ~/.venvs/halmos && ~/.venvs/halmos/bin/pip install halmos
cd contracts && ~/.venvs/halmos/bin/halmos --contract JuiceRailSymbolicTest
```

## Deploying a domain

A **domain** is one `JuiceRail` deployment, named by `(chain id, contract
address)`. Every project deploys its own per network. Domains are independent:
separate balances, separate identifiers, no bridging between them.

The Safe contracts and the EntryPoint are the canonical deployments already on
the network; this repository never deploys or recompiles them.

```sh
cd contracts
export TOKEN=<USDT0 on this network>
export ENTRY_POINT=0x0000000071727De22E5E9d8BAf0edAc6f37da032
export MULTI_SEND=0x9641d764fc13c8B624c04430C7356C1C7C8102e2
export PAYMASTER_SIGNER=<address that authorises sponsorship>
export PAYMASTER_DEPOSIT=100000000000000000   # optional, wei to stake

forge script script/Deploy.s.sol --rpc-url $RPC --broadcast
```

Copy the printed rail and paymaster addresses into a domain file. Start from
`deployments/arbitrum-one.json` or `deployments/arbitrum-sepolia.json`; `rpc`
and `bundler` are your own endpoints.

```json
{
  "name": "arbitrum-one",
  "chainId": 42161,
  "rpc": "https://…",
  "bundler": "https://…",
  "rail": "0x…",
  "token": "0x…",
  "paymaster": "0x…",
  "entryPoint": "0x…",
  "safeSingleton": "0x…",
  "safeProxyFactory": "0x…",
  "safeModule": "0x…",
  "safeModuleSetup": "0x…",
  "multiSendCallOnly": "0x…",
  "finality": "finalized"
}
```

Only true finality is accepted. A confirmed fact must never be reversible, so
confirmation-count policies are refused at startup.

## railctl

```sh
go build -o railctl ./cmd/railctl

export RAILCTL_CONFIG=deployments/arbitrum-one.json
export RAILCTL_STORE=~/.juice-rail/arbitrum-one.db
export RAILCTL_KEY=<hex key owning this rail's account>
export RAILCTL_PAYMASTER_KEY=<hex paymaster signing key>
```

| Command | What it does |
|---|---|
| `railctl account` | this rail's address on the domain |
| `railctl balance [address]` | rail balance and token holding |
| `railctl deposit <id> <account> <amount>` | pull tokens in and credit an account |
| `railctl settle <id> <creditor> <amount>` | move balance to another account |
| `railctl withdraw <id> <to> <amount>` | send tokens out |
| `railctl status <id>` | what became of an identifier |
| `railctl abandon <id>` | stop signing for an identifier |

`-json` gives machine-readable output. Amounts are token base units: USDT0 has
six decimals, so `1000000` is one dollar. Identifiers are 32-byte hex and must
be unguessable until submission — the host mints them.

**One store per domain.** The store records its domain the first time it is
used and refuses to open for another, because identifiers only mean one thing
within a domain. Run a second domain with a second store file.

## Operating it

**Fund the paymaster.** It pays the chain's fees so account holders never need
to hold the chain's currency. When its stake runs out, everything stops until
it is topped up. Watch it.

**Account addresses exist before the accounts do.** `railctl account` prints
the address immediately; tokens sent there before it is deployed are safe, and
the account is created on its first operation.

**What the four statuses mean:**

| | |
|---|---|
| `unknown` | no record of this identifier |
| `pending` | it may still execute — a revert is not failure, and a reverted operation can be retried |
| `confirmed` | it executed and the result can never be reversed |
| `failed` | it can never execute: the identifier is finalized under other terms, or it was abandoned and every attempt is provably dead |

A host must move its own ledger only on `confirmed`, and release anything it
reserved only on `failed`.

**Retrying is always safe.** Re-running the same command re-presents the same
signed operation; a fresh one is signed only once the previous can no longer
execute. The vault refuses a second execution regardless.

**A stuck operation.** Re-run the command. If it must be given up on, `abandon`
it — but that alone does not release anything: it stops future signing without
killing what is already signed. Wait for `failed`, which arrives once every
attempt has expired past finality.

**A refused identifier.** If a `deposit`/`settle`/`withdraw` is rejected for
different terms, that identifier is spent. Mint a fresh one and retry; no funds
are ever at risk from this.

## Layout

```
contracts/   JuiceRail, the verifying paymaster, tests, deploy script
go/rail      the library: intents, terms, status, the store interface
go/erc4337   operation packing, Safe derivation, sponsorship, bundler client
go/sqlite    the shipped store
cmd/railctl  this app
deployments/ per-domain configuration and the published Safe bytecode
```
