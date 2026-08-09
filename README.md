# juice-rail

juice-rail moves real money — **USDT0**, a dollar-pegged token — between
accounts on an Ethereum-style network. It is three things: a **vault**, a
program deployed on the network that holds the tokens and keeps one balance
per account; a **Go library** that drives the vault; and a small command-line
tool, **`railctl`**, for using it by hand.

Your money is always in one of two pockets: tokens sitting **at your own
address** — cash in hand — or a balance **inside the vault** — money on
account. It moves three ways: **deposit** (hand → vault), **settle** (your
vault balance → someone else's), **withdraw** (vault → any address's hand).

Every operation carries an identifier the vault remembers: repeating an
operation is a safe no-op, and reusing its identifier for different terms is
refused. No money can ever move twice.

Account holders never need ETH, the network's fuel. A sponsor contract — the
**paymaster** — pays the network fees for them; only the operator who runs
the paymaster holds ETH.

`requirements.md` is the specification. This file is how to run it.

## Install

| | |
|---|---|
| Go | 1.22 or later |
| [Foundry](https://getfoundry.sh) | contract tools: `forge`, `cast`, `anvil` |
| [Halmos](https://github.com/a16z/halmos) | optional, only for the formal proofs |

```sh
git clone --recurse-submodules https://github.com/daios-ai/juice-rail
cd juice-rail && go build ./...
```

Already cloned without submodules? `git submodule update --init --recursive`.

## Try it locally first

You need no real network, no real money, no account with any provider. This
command creates a pretend network on your machine, puts the vault and
paymaster on it, and acts out six scenarios with fake money — a deposit, a
payment between two accounts, a withdrawal, a repeated operation, a reused
identifier with different terms, and a crash halfway through:

```sh
go test -tags integration ./integration-tests/
```

Everything runs and checks itself in about a minute. Nothing leaves your
machine. The scenario code, `integration-tests/stories_test.go`, shows
`railctl` doing everything it can do — worth a skim once you reach
"Use railctl" below.

## Deploy for real

The vault runs on one of two networks: **Arbitrum Sepolia**, a test network
where everything is free and fake, or **Arbitrum One**, the real one. The
steps are the same on both; the differences are marked. Do them once per
network you use.

### 1. Create the two operator keys

A key is a long secret number; whoever holds it controls one address.
Foundry makes them:

```sh
cast wallet new   # run it twice, save both outputs somewhere safe
```

Call the first one the **operator key**: it deploys everything and is the
only key that will ever hold ETH. Call the second the **paymaster key**: the
paymaster will pay fees only for operations approved by it. You use its
*address* now and its *private key* later, in "Use railctl".

```sh
export OPERATOR_KEY=<operator private key>
export PAYMASTER_SIGNER=<paymaster address>
```

### 2. Get your two endpoints

- An **RPC endpoint** — your window onto the network. Free from
  [Alchemy](https://alchemy.com) or any node provider.
- A **bundler endpoint** — the mail service that carries fee-free operations
  to the network. Free on the test network from [Pimlico](https://pimlico.io)
  or Alchemy.

```sh
export RPC=<your RPC endpoint URL>   # the bundler URL is used in step 5
```

### 3. Put ETH on the operator address

Deploying costs fees, paid in ETH from the operator address (printed when
you created the key). Test network: a faucet gives it away free; 0.2 is
plenty. Real network: transfer it there.

### 4. Test network only: create a play token

There is no real USDT0 on the test network, so deploy the mock — a play
version of the token that anyone may mint:

```sh
cd contracts
forge create test/MockUSDT0.sol:MockUSDT0 --rpc-url $RPC --private-key $OPERATOR_KEY
export TOKEN=<the printed address>
```

Real network: `export TOKEN=<the canonical USDT0 address>`, nothing to
deploy.

### 5. Deploy the vault and paymaster

```sh
cd contracts
export ENTRY_POINT=0x0000000071727De22E5E9d8BAf0edAc6f37da032
export MULTI_SEND=0x9641d764fc13c8B624c04430C7356C1C7C8102e2
export PAYMASTER_DEPOSIT=100000000000000000   # 0.1 ETH into the paymaster's fee tank

forge script script/Deploy.s.sol --rpc-url $RPC --private-key $OPERATOR_KEY --broadcast
```

`ENTRY_POINT` and `MULTI_SEND` are public infrastructure contracts, the same
addresses on every network — take them as given. The script prints two new
addresses — the vault (called `rail` from here on) and the paymaster — and
fills the paymaster's fee tank with the deposit.

### 6. Write the domain file

A **domain** is one deployed vault, named by (network, vault address).
Domains are independent: separate balances, separate identifiers, nothing
crosses between them.

Copy `deployments/arbitrum-sepolia.json` (or `arbitrum-one.json`) and fill
the blanks with what you now have: `rpc` and `bundler` from step 2, `token`
from step 4, `rail` and `paymaster` from step 5. The remaining addresses are
already correct — public infrastructure, never deployed from this
repository.

Leave `finality` as `"finalized"`. It means a fact is reported only once the
network can never take it back; weaker settings are refused at startup.

The domain is live.

## Use railctl

Build the tool and make yourself an account key — this one needs no ETH,
ever:

```sh
go build -o railctl ./cmd/railctl
cast wallet new   # your account key

export RAILCTL_CONFIG=deployments/arbitrum-sepolia.json   # your domain file
export RAILCTL_STORE=~/.juice-rail/arbitrum-sepolia.db    # its records; one file per domain
export RAILCTL_KEY=<your account private key>
export RAILCTL_PAYMASTER_KEY=<the paymaster PRIVATE key from deploy step 1>
```

The last line is the wire that makes operations free: it must be the private
key of the `PAYMASTER_SIGNER` address you deployed with.

A first session, in order:

```sh
railctl account
```

Prints your address on this domain. It exists before anything is deployed
there — tokens sent to it are safe from day one.

Put tokens in your hand, meaning at that address. Test network — mint
yourself play money (a billion base units is a thousand play dollars):

```sh
cast send $TOKEN "mint(address,uint256)" <your railctl address> 1000000000 \
    --rpc-url $RPC --private-key $OPERATOR_KEY
```

Real network: transfer USDT0 to the address as to any other.

Make an identifier — 32 random bytes — and move one dollar from hand to
vault:

```sh
ID=$(openssl rand -hex 32)
railctl deposit $ID <your railctl address> 1000000
railctl status $ID
```

`status` says `pending` until the network finalizes the operation — about
20 minutes on a real network. That is the safety model working, not a hang.
Then it says `confirmed`, and `railctl balance` shows both pockets: your
vault balance and the tokens still in hand.

All commands:

| Command | What it does |
|---|---|
| `railctl account` | your address on this domain |
| `railctl balance [address]` | vault balance and tokens in hand |
| `railctl deposit <id> <account> <amount>` | pull tokens from your hand, credit an account's vault balance |
| `railctl settle <id> <creditor> <amount>` | move vault balance to another account |
| `railctl withdraw <id> <to> <amount>` | send tokens from the vault out to any address |
| `railctl status <id>` | what became of an identifier |
| `railctl abandon <id>` | stop signing for an identifier |

Worth knowing:

- **Amounts are token base units.** USDT0 has six decimals: `1000000` is one
  dollar.
- **Identifiers must be fresh and unguessable** — generate each one as
  above, never reuse one.
- **One store per domain.** The store file records its domain on first use
  and refuses to open for another. Second domain, second file.
- `-json` gives machine-readable output.

## Operate it

Every identifier is always in exactly one of four states:

| | |
|---|---|
| `unknown` | no record of this identifier |
| `pending` | it may still execute — keep waiting or retry |
| `confirmed` | it executed and the result can never be reversed |
| `failed` | it can never execute: the identifier is taken under other terms, or it was abandoned and every attempt is provably dead |

**Retrying is always safe.** Re-running the same command re-presents the
same signed operation; the vault refuses a second execution regardless.

**A stuck operation.** Re-run it. If it must be given up on, `abandon` it —
but that alone releases nothing: it stops future signing without killing
what is already signed. Wait for `failed`, which arrives once every attempt
is provably dead.

**A refused identifier.** If an operation is rejected because its identifier
is taken under different terms, that identifier is spent. Generate a fresh
one and retry; no funds are ever at risk from this.

**Watch the paymaster's fee tank.** When its deposit runs out, everything
stops until it is topped up. Nothing is lost — operations wait — but nothing
moves either.

**If you embed the library in your own application** — a *host*; Juice is
one — two rules: change your own ledger only on `confirmed`, and release
anything you reserved only on `failed`.

## Verify the build

```sh
go test ./...                                  # library, offline
go test -race ./...                            # same, under the race detector
cd contracts && forge test                     # 53 unit, fuzz and invariant tests
go test -tags integration ./integration-tests/ # the six scenarios, needs anvil
```

The scenarios are opt-in: without `-tags integration` they do not run, and
if `anvil` is missing they fail rather than pass quietly.

The formal proofs need Halmos in its own environment, so it cannot disturb
the rest of your Python setup:

```sh
python3 -m venv ~/.venvs/halmos && ~/.venvs/halmos/bin/pip install halmos
cd contracts && ~/.venvs/halmos/bin/halmos --contract JuiceRailSymbolicTest
```

## Layout

```
contracts/   the vault (JuiceRail), the paymaster, tests, deploy script
go/rail      the library: intents, terms, status, the store interface
go/erc4337   operation packing, account derivation, sponsorship, bundler client
go/sqlite    the shipped store
cmd/railctl  this tool
deployments/ per-domain configuration and published contract bytecode
```
