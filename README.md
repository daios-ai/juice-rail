# juice-rail

A settlement rail for micropayments: an on-chain vault that holds USDT0 and
keeps one balance per account, plus a Go library that drives it, plus a small
CLI (`railctl`) to run it standalone. Juice is one host among others.

Money moves three ways — **deposit** in, **settle** between two accounts,
**withdraw** out. Every operation carries an identifier the vault remembers:
repeating an operation is a safe no-op, and reusing its identifier for
different terms is refused. The host stays authoritative for its own ledger;
the rail moves external money and reports finalized facts.

Account holders never need ETH. A sponsor contract (the *paymaster*) pays the
chain's fees; only the operator who runs it holds ETH.

`requirements.md` is the specification. This file is how to run it.

## Install

| | |
|---|---|
| Go | 1.22 or later |
| [Foundry](https://getfoundry.sh) | `forge` builds the contracts, `anvil` runs a local chain |
| [Halmos](https://github.com/a16z/halmos) | optional, only for the formal proofs |

```sh
git clone --recurse-submodules https://github.com/daios-ai/juice-rail
cd juice-rail && go build ./...
```

Already cloned without submodules? `git submodule update --init --recursive`.

## Try it locally first

You don't need a real network, real money, or any account with any provider.
The six user stories stand up a complete local chain — vault, paymaster,
accounts — and drive the real `railctl` binary through every flow:

```sh
go test -tags integration ./integration-tests/
```

You'll see deposits, settlements, withdrawals, retries, conflicting
identifiers, and crash recovery run end to end in about a minute.
`integration-tests/stories_test.go` is a worked example of every command; read
it before your first real deployment.

## Deploy a real domain

A **domain** is one vault deployment, named by `(chain id, contract address)`.
Every project deploys its own per network. Domains are independent: separate
balances, separate identifiers, no bridging between them.

Do this once per domain. Testnet (Arbitrum Sepolia) and mainnet (Arbitrum One)
are the same steps; differences are marked.

### 1. What you need

- **An RPC endpoint** — your window onto the chain. Free from
  [Alchemy](https://alchemy.com) or any node provider.
- **A bundler endpoint** — the service that carries gasless operations to the
  chain. Free on testnet from [Pimlico](https://pimlico.io) or Alchemy.
- **One operator key with a little ETH** — the only ETH in the whole system.
  It deploys the contracts and fills the paymaster's gas tank. On testnet,
  get it free from a faucet; 0.2 ETH is plenty.

### 2. Testnet only: deploy a test token

There is no real USDT0 on Sepolia, so deploy the mock (anyone can mint it):

```sh
cd contracts
forge create test/MockUSDT0.sol:MockUSDT0 --rpc-url $RPC --private-key $OPERATOR_KEY
```

Note the printed address — it is your `TOKEN` below. On mainnet, `TOKEN` is
the canonical USDT0 address; you deploy nothing.

### 3. Deploy the vault and paymaster

```sh
cd contracts
export TOKEN=<token address from step 2, or canonical USDT0>
export ENTRY_POINT=0x0000000071727De22E5E9d8BAf0edAc6f37da032
export MULTI_SEND=0x9641d764fc13c8B624c04430C7356C1C7C8102e2
export PAYMASTER_SIGNER=<address that authorises sponsorship>
export PAYMASTER_DEPOSIT=100000000000000000   # 0.1 ETH into the gas tank

forge script script/Deploy.s.sol --rpc-url $RPC --private-key $OPERATOR_KEY --broadcast
```

The script prints the rail and paymaster addresses and, with
`PAYMASTER_DEPOSIT` set, funds the paymaster in the same run.

### 4. Write the domain file

Start from `deployments/arbitrum-sepolia.json` (or `arbitrum-one.json`) and
fill the blanks: `rpc` and `bundler` are your endpoints from step 1, `token`
from step 2, `rail` and `paymaster` from step 3. The Safe and EntryPoint
addresses are already correct — they are the canonical deployments on that
network; this repository never deploys or recompiles them.

`finality` stays `"finalized"`. Only true finality is accepted: a confirmed
fact must never be reversible, so confirmation-count policies are refused at
startup.

That's it. The domain is live.

## Use railctl

```sh
go build -o railctl ./cmd/railctl

export RAILCTL_CONFIG=deployments/arbitrum-sepolia.json
export RAILCTL_STORE=~/.juice-rail/arbitrum-sepolia.db
export RAILCTL_KEY=<hex key owning this rail's account>
export RAILCTL_PAYMASTER_KEY=<hex paymaster signing key>
```

`RAILCTL_KEY` can be a fresh key with zero ETH — that is the point.

| Command | What it does |
|---|---|
| `railctl account` | this rail's address on the domain |
| `railctl balance [address]` | rail balance and token holding |
| `railctl deposit <id> <account> <amount>` | pull tokens in and credit an account |
| `railctl settle <id> <creditor> <amount>` | move balance to another account |
| `railctl withdraw <id> <to> <amount>` | send tokens out |
| `railctl status <id>` | what became of an identifier |
| `railctl abandon <id>` | stop signing for an identifier |

A first session: `railctl account` prints your address — it exists before the
account does, so send tokens there right away; the account is created on its
first operation. Then `deposit`, then `balance`, then `status <id>`.

Worth knowing:

- **`pending` is honest.** On a real network an operation stays `pending`
  until the chain finalizes it — about 20 minutes on Arbitrum. That is the
  safety model working, not a hang.
- **Amounts are token base units.** USDT0 has six decimals: `1000000` is one
  dollar.
- **Identifiers are 32-byte hex** and must be unguessable until submission —
  the host mints them.
- **One store per domain.** The store records its domain on first use and
  refuses to open for another. Run a second domain with a second store file.
- `-json` gives machine-readable output.

## Operate it

**The four statuses:**

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

**A stuck operation.** Re-run the command. If it must be given up on,
`abandon` it — but that alone releases nothing: it stops future signing
without killing what is already signed. Wait for `failed`, which arrives once
every attempt has expired past finality.

**A refused identifier.** If an operation is rejected for different terms,
that identifier is spent. Mint a fresh one and retry; no funds are ever at
risk from this.

**Watch the paymaster's gas tank.** When its deposit runs out, everything
stops until it is topped up. Nothing is lost — operations wait — but nothing
moves either.

## Verify the build

```sh
go test ./...                                  # library, offline
go test -race ./...                            # same, under the race detector
cd contracts && forge test                     # 53 unit, fuzz and invariant tests
go test -tags integration ./integration-tests/ # the six user stories, needs anvil
```

The stories are opt-in: without `-tags integration` they do not run, and if
`anvil` is missing they fail rather than pass quietly.

The formal proofs need Halmos in its own environment, so it cannot disturb
the rest of your Python setup:

```sh
python3 -m venv ~/.venvs/halmos && ~/.venvs/halmos/bin/pip install halmos
cd contracts && ~/.venvs/halmos/bin/halmos --contract JuiceRailSymbolicTest
```

## Layout

```
contracts/   JuiceRail, the verifying paymaster, tests, deploy script
go/rail      the library: intents, terms, status, the store interface
go/erc4337   operation packing, Safe derivation, sponsorship, bundler client
go/sqlite    the shipped store
cmd/railctl  this app
deployments/ per-domain configuration and the published Safe bytecode
```
