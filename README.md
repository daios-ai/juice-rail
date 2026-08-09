# juice-rail

juice-rail moves real money between accounts on an Ethereum-style network.
The money is **USDT0**, a token pegged to the dollar. juice-rail is three
things: a **vault**, a program deployed on the network that holds the tokens
and keeps one balance per account; a **Go library** that drives the vault;
and a small command-line tool, **`railctl`**, for using it by hand.

**One vault, many participants.** Independent installations (different
companies, different machines, no trust between them) each hold an account
in the same vault and pay each other through it. A payment one side made and
the other side received cannot be denied by either, because the referee is
the network itself, not anyone's server.

Your money is always in one of two pockets: tokens sitting **at your own
address**, like cash in hand, or a balance **inside the vault**, like money
on account. It moves three ways: **deposit** (your hand → your vault
balance), **settle** (your vault balance → another participant's),
**withdraw** (vault → any address's hand).

Every operation carries an identifier the vault remembers: repeating an
operation is a safe no-op, and reusing its identifier for different terms is
refused. No money can ever move twice.

A running vault has one **operator** and any number of **participants**. The
operator deploys the vault once and pays everyone's network fees through a
sponsor contract called the **paymaster**. Participants therefore never need
ETH, the network's fuel; only the operator holds it. A participant joins
with one key they make themselves and two things the operator gives them.

`requirements.md` is the specification. This file is how to run it.

## Install

Everyone installs the same way, operator and participants alike.

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
paymaster on it, and acts out six scenarios between accounts with fake
money: a deposit, a payment from one account to another, a withdrawal, a
repeated operation, a reused identifier with different terms, and a crash
halfway through.

```sh
go test -tags integration ./integration-tests/
```

Everything runs and checks itself in about a minute. Nothing leaves your
machine. The scenario code, `integration-tests/stories_test.go`, shows
`railctl` doing everything it can do. It is worth a skim once you reach
"Use railctl" below.

## Run a domain (you are the operator)

A **domain** is one deployed vault, named by (network, vault address).
Domains are independent: separate balances, separate identifiers, nothing
crosses between them. Each domain has exactly one operator. If someone
already operates the vault you want to use, skip to "Join a domain".

The vault runs on one of two networks: **Arbitrum Sepolia**, a test network
where everything is free and fake, or **Arbitrum One**, the real one. The
steps are the same on both; the differences are marked.

### 1. Create the two operator keys

A key is a long secret number; whoever holds it controls one address.
Foundry makes them:

```sh
cast wallet new   # run it twice, save both outputs somewhere safe
```

Call the first the **operator key**: it deploys everything and is the only
key that ever holds ETH. Call the second the **sponsorship key**: the
paymaster will pay fees only for operations approved by it, and you will
hand it to every participant you sponsor. Handing it out is a bounded risk
by design: the worst this key can do is spend your fee tank; it can never
touch anyone's money.

```sh
export OPERATOR_KEY=<operator private key>
export PAYMASTER_SIGNER=<sponsorship key's ADDRESS>
```

### 2. Get your two endpoints

- An **RPC endpoint**: your window onto the network. Free from
  [Alchemy](https://alchemy.com) or any node provider.
- A **bundler endpoint**: the mail service that carries fee-free operations
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

There is no real USDT0 on the test network, so deploy the mock, a play
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
addresses on every network; take them as given. The script prints two new
addresses, the vault (called `rail` from here on) and the paymaster, and
fills the paymaster's fee tank with the deposit.

### 6. Publish the domain file

Copy `deployments/arbitrum-sepolia.json` (or `arbitrum-one.json`) and fill
the blanks with what you now have: `rpc` and `bundler` from step 2, `token`
from step 4, `rail` and `paymaster` from step 5. The remaining addresses are
already correct: public infrastructure, never deployed from this repository.

Leave `finality` as `"finalized"`. It means a fact is reported only once the
network can never take it back; weaker settings are refused at startup.

The domain is live. Give each participant two things: **this file** (it
contains only public information) and **the sponsorship key**.

## Join a domain (you are a participant)

You deploy nothing and never touch ETH. You need:

1. **From the operator:** the domain file and the sponsorship key.
2. **Made by you, kept by you:** your account key.

```sh
cast wallet new   # your account key; the operator never sees it
```

That's all. Continue below.

## Use railctl

Operator and participants use it identically; the operator is just a
participant who also holds the other keys.

```sh
go build -o railctl ./cmd/railctl

export RAILCTL_CONFIG=<path to the domain file>
export RAILCTL_STORE=~/.juice-rail/<domain name>.db   # its records; one file per domain
export RAILCTL_KEY=<your account private key>
export RAILCTL_PAYMASTER_KEY=<the sponsorship key>
```

A first session between two participants, **Alice** and **Bob**, each on
their own machine with their own setup as above.

Both start the same way:

```sh
railctl account
```

It prints your address on this domain. The address exists before anything is
deployed there, so tokens sent to it are safe from day one. Bob tells Alice
his address; that is all Alice ever needs to know about Bob.

**Alice puts tokens in her hand**, meaning at her address. Test network:
mint play money (this uses the operator's setup from "Run a domain", since
anyone may mint the mock):

```sh
cast send $TOKEN "mint(address,uint256)" <Alice's railctl address> 1000000000 \
    --rpc-url $RPC --private-key $OPERATOR_KEY
```

Real network: transfer USDT0 to her address as to any other.

**Alice deposits**, moving tokens from hand to vault. She makes a fresh
identifier (32 random bytes) for the operation and moves ten dollars:

```sh
ID=$(openssl rand -hex 32)
railctl deposit $ID <Alice's railctl address> 10000000
railctl status $ID
```

`status` says `pending` until the network finalizes the operation, about
20 minutes on a real network. That is the safety model working, not a hang.
Then it says `confirmed`.

**Alice pays Bob.** One dollar moves from her vault balance to his, under a
fresh identifier:

```sh
ID=$(openssl rand -hex 32)
railctl settle $ID <Bob's address> 1000000
```

**Bob checks and cashes out.** Once Alice's payment is `confirmed`, on his
machine:

```sh
railctl balance                            # his vault balance: 1000000
ID=$(openssl rand -hex 32)
railctl withdraw $ID <any address Bob likes> 1000000
```

Alice and Bob never shared anything but addresses. The vault enforced that
Alice could only spend her own balance, and Bob can prove he was paid.

All commands:

| Command | What it does |
|---|---|
| `railctl account` | your address on this domain |
| `railctl balance [address]` | vault balance and tokens in hand |
| `railctl deposit <id> <account> <amount>` | pull tokens from your hand, credit an account's vault balance |
| `railctl settle <id> <creditor> <amount>` | move vault balance to another participant |
| `railctl withdraw <id> <to> <amount>` | send tokens from the vault out to any address |
| `railctl status <id>` | what became of an identifier |
| `railctl abandon <id>` | stop signing for an identifier |

Worth knowing:

- **Amounts are token base units.** USDT0 has six decimals: `1000000` is one
  dollar.
- **Identifiers must be fresh and unguessable.** Generate each one as above;
  never reuse one.
- **One store per domain.** The store file records its domain on first use
  and refuses to open for another. Second domain, second file.
- `-json` gives machine-readable output.

## Operate it

Every identifier is always in exactly one of four states:

| | |
|---|---|
| `unknown` | no record of this identifier |
| `pending` | it may still execute; keep waiting or retry |
| `confirmed` | it executed and the result can never be reversed |
| `failed` | it can never execute: the identifier is taken under other terms, or it was abandoned and every attempt is provably dead |

**Retrying is always safe.** Re-running the same command re-presents the
same signed operation; the vault refuses a second execution regardless.

**A stuck operation.** Re-run it. If it must be given up on, `abandon` it.
But that alone releases nothing: it stops future signing without killing
what is already signed. Wait for `failed`, which arrives once every attempt
is provably dead.

**A refused identifier.** If an operation is rejected because its identifier
is taken under different terms, that identifier is spent. Generate a fresh
one and retry; no funds are ever at risk from this.

**Operators: watch the fee tank.** When the paymaster's deposit runs out,
the whole domain stops until it is topped up. Nothing is lost (operations
wait), but nothing moves either.

**If you embed the library in your own application**, making it a *host*
(Juice is one), two rules apply: change your own ledger only on `confirmed`,
and release anything you reserved only on `failed`.

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
