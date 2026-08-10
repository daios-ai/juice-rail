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

These steps drive `forge` and `cast`, which read the environment, and an
`export`ed value dies with its terminal. Save each one as you go and reload it
in a new terminal:

```sh
echo 'export TOKEN=0x...' >> ~/deploy.env   # save as you go
source ~/deploy.env                         # reload later
```

This is for deployment only. `railctl` itself needs no exports; see
"Use railctl".

### 1. Create the two operator keys

Run this command twice:

```sh
cast wallet new
```

Each run prints a fresh pair like this:

```text
Address:     0xAB12...      an account number; fine to share
Private key: 0x59C6...      the secret that controls it; save it
                            like a bank password
```

Save both pairs. The first is your **operator key**: it sets everything up
and holds the fee money. The second is your **sponsorship key**: fees are
paid only for operations it approves, and you hand it to every participant.
That's safe: at worst it can spend your fee tank, never anyone's money.

Addresses are 42 characters, private keys 66. An "invalid length" error
later means you swapped them.

```sh
export OPERATOR_KEY=<Private key line of the FIRST pair>
export PAYMASTER_SIGNER=<Address line of the SECOND pair>
```

### 2. Get your two web links

The vault lives on a public network of computers. Talking to it takes two
services, rented as personal web links; free plans are enough.

**The reading link** is how this software reads the network. Go to
[alchemy.com](https://alchemy.com), sign up with an email, and create an
"app", choosing the network **Arbitrum Sepolia** (test) or **Arbitrum One**
(real). Copy the link the dashboard hands you; it looks like

```text
https://arb-sepolia.g.alchemy.com/v2/<long code>
```

**The carrying link** is how it sends in fee-free operations. Go to
[pimlico.io](https://pimlico.io), sign up, and copy your link. It looks like

```text
https://api.pimlico.io/v2/421614/rpc?apikey=<long code>
```

The number in the middle is the network: `421614` is the test network,
`42161` the real one.

Two companies because carriers set their own conditions: Alchemy's demands
a returnable 0.1 ETH deposit (step 5), Pimlico's currently doesn't. Nothing
depends on either; switching providers is one line in the domain file.

```sh
export RPC=<the reading link>   # both links go into a file in step 6
```

### 3. Put fee money on the operator address

Fees are paid in ETH from your operator address (the Address line of the
first pair). Setup fees are under 0.001; the real costs are the step-5 fee
tank (0.01 to start) and the 0.1 deposit if your carrier wants one.

Test network ETH is free from faucets, which all demand proof you're
human. The generous ones (Alchemy's) want ~$5 of real ETH parked at your
address on Ethereum mainnet and give ~0.1 per day;
[l2faucet.com/arbitrum](https://l2faucet.com/arbitrum) just checks your
device and gives less. Faucets need only your Address; anything asking for
a private key is a scam.

Real network: withdraw ETH from any exchange to the operator address,
network **Arbitrum One**, the same way you would send USDT.

### 4. Test network only: create a play token

There is no real USDT0 on the test network, so deploy the mock, a play
version of the token that anyone may mint:

```sh
cd contracts
forge create test/MockUSDT0.sol:MockUSDT0 --rpc-url $RPC --private-key $OPERATOR_KEY --broadcast
export TOKEN=<the "Deployed to" address it prints>
```

Without `--broadcast` forge only rehearses and deploys nothing.

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

If submissions later fail with "stake/unstake delay too low", your carrier
wants the 0.1 ETH deposit:

```sh
cast send <paymaster address> "addStake(uint32)" 86400 --value 100000000000000000
```

It is never spent and reclaimable after a one-day wait.

### 6. Publish the domain file

Copy `deployments/arbitrum-sepolia.json` (or `arbitrum-one.json`) to a
place **outside this repository**, for example `~/.juice-rail/`, and fill
the blanks there: `rpc` is your reading link, `bundler` your carrying link,
`token` from step 4, `rail` and `paymaster` from step 5. The remaining
addresses are already correct: public infrastructure, never deployed from
this repository.

The filled file contains your account codes; that's why it lives outside
the repository.

Leave `finality` as `"finalized"`. It means a fact is reported only once the
network can never take it back; weaker settings are refused at startup.

The domain is live. Give each participant your filled domain file and the
sponsorship key.

## Join a domain (you are a participant)

You deploy nothing and never touch ETH. You need:

1. **From the operator:** the domain file and the sponsorship key.
2. **Made by you, kept by you:** nothing. `railctl init` below makes your key.

## Use railctl

Operator and participants use it identically; the operator is just a
participant who also holds the other keys.

Set up once. Put the sponsorship key in a file first, because a key passed as
a flag value would be visible to every user on the machine:

```sh
go build -o railctl ./cmd/railctl

$EDITOR sponsor.key   # paste the sponsorship key the operator gave you
railctl -sponsor-key-file sponsor.key init alice arbitrum-sepolia.json
```

That generates your account key, installs the domain, and records `alice` as
the profile to use. From then on, in any terminal, with nothing exported:

```sh
railctl balance
```

It all lives in `~/.juice-rail/`: `config.json` holds domains and profiles,
`credentials.json` holds keys and nothing else at mode `0600`, and each
profile gets its own `<profile>.db` of records. Add a second person with
another `init` (the domain is already installed, so no sponsorship key is
needed) and pick them with `-profile bob`.

Two conventions worth knowing: flags come before the command, as in
`railctl -profile bob balance`; and to import an existing key instead of
generating one, `init` takes `-key-file`.

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
| `railctl init <profile> <domain-file>` | set up a profile once |
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
- **Re-running a command is safe** and reports the same status. Money moves
  once whatever you do.
- **One store per profile.** The store file records its domain on first use
  and refuses to open for another.
- `-json` gives machine-readable output. `-config`, `-store`, `RAILCTL_KEY`
  and `RAILCTL_PAYMASTER_KEY` still override the profile, for automation.

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

**"Expired" on every retry.** Signed operations expire after five minutes,
and a replacement is signed only once the network's final record proves the
old one can never execute, roughly 25 minutes on Arbitrum. Wait, re-run,
and it proceeds.

**Operators: watch the fee tank.** When the paymaster's deposit runs out,
the whole domain stops until it is topped up. Nothing is lost (operations
wait), but nothing moves either. Two numbers to watch:

```sh
# ETH still in your operator hand
cast balance --ether <operator address> --rpc-url $RPC

# the fee tank, in wei (divide by 10^18 for ETH)
cast call 0x0000000071727De22E5E9d8BAf0edAc6f37da032 \
    "balanceOf(address)(uint256)" <paymaster address> --rpc-url $RPC
```

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
