# juice-rail

juice-rail moves real money between accounts on an Ethereum-style network.
The money is a dollar-pegged token such as **USDT0**. juice-rail is three
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
balance), **transfer** (your vault balance → another participant's),
**withdraw** (vault → any address you name).

Your account is just an ordinary Ethereum address. You sign what you want to
happen; someone else delivers it.

**Nobody is in charge.** Every movement of money needs the account holder's
signature and nothing else. The vault has no owner, no administrator and no
off switch. Whoever deployed it has no more power over your balance than a
stranger does.

**You never need ETH.** Delivering a signed instruction to the network costs
the network's own fuel, ETH, which ordinary participants should never have to
think about. So a **relayer** pays that cost and is paid back in the token
itself, out of the same operation. You name your relayer and agree its fee
before you sign; nobody else can carry your instruction, and nobody can
change what it says. Being a relayer takes no permission and no contract: it
is anyone holding ETH who is willing to be named.

```text
you sign        amount, who gets it, the fee, and which relayer may deliver it
relayer pays    the network fee, in ETH, up front
the vault pays  the relayer its fee, in tokens, out of your balance
```

Every operation carries an identifier the vault remembers: repeating an
operation is a safe no-op, and reusing its identifier for different terms is
refused. No money can ever move twice.

`requirements.md` is the specification. This file is how to run it.

## Install

```sh
git clone --recurse-submodules https://github.com/daios-ai/juice-rail
cd juice-rail && go build ./...
```

| | |
|---|---|
| Go | 1.22 or later |
| [Foundry](https://getfoundry.sh) | contract tools: `forge`, `cast`, `anvil` |
| [Halmos](https://github.com/a16z/halmos) | optional, only for the formal proofs |

Already cloned without submodules? `git submodule update --init --recursive`.

## Try it locally first

You need no real network, no real money and no account with any provider.
This command creates a pretend network on your machine, puts the vault on it,
and acts out the six scenarios: a deposit, a payment, a withdrawal, a
repeated operation, a reused identifier with different terms, and a crash
halfway through.

```sh
cd contracts && forge build && cd ..
go test -tags integration ./integration-tests/
```

It finishes in seconds and checks itself. Nothing leaves your machine. The
scenario code, `integration-tests/stories_test.go`, shows `railctl` doing
everything it can do.

## A local playground

To drive it by hand, start a pretend network and leave it running:

```sh
anvil
```

In another terminal, deploy a play token and the vault. `anvil` prints ten
funded test keys; use the first as `$DEPLOYER_KEY`.

```sh
cd contracts
export RPC=http://127.0.0.1:8545
forge create test/MockUSDT0.sol:MockUSDT0 --rpc-url $RPC --private-key $DEPLOYER_KEY --broadcast
export TOKEN=<address it printed>
forge script script/Deploy.s.sol --rpc-url $RPC --private-key $DEPLOYER_KEY --broadcast
```

Write the domain file, which is all anyone needs to join:

```json
{
  "name": "local",
  "chainId": 31337,
  "rpc": "http://127.0.0.1:8545",
  "rail": "<the JuiceRail address>",
  "token": "<the MockUSDT0 address>",
  "finality": "finalized"
}
```

Create two participants and one relayer. `init` makes a fresh key, checks the
token can carry signed authorisations, and remembers everything under
`~/.juice-rail`:

```sh
railctl init alice local.json
railctl init bob   local.json
railctl -key-file anvil-second-key.txt init relayer local.json
```

Flags go before the command, always.

The relayer is the only one that needs ETH, because it is the only one that
talks to the network directly. Alice and Bob need none, ever.

Give Alice some play money and put it in the vault. She signs; the relayer
delivers:

```sh
cast send $TOKEN "mint(address,uint256)" $(railctl -profile alice account) 100000000 \
  --rpc-url $RPC --private-key $DEPLOYER_KEY

ID=0x$(openssl rand -hex 32)
railctl -profile alice -fee 20000 -relayer $(railctl -profile relayer account) \
        -out deposit.json deposit $ID $(railctl -profile alice account) 60000000
railctl -profile relayer relay deposit.json
```

Amounts are in the token's smallest unit; USDT0 has six decimals, so
`60000000` is 60.00 and the fee above is 0.02.

`deposit.json` is the signed instruction. It is worth reading: it says exactly
what will happen and who may make it happen.

Check on it, pay Bob, and let Bob take his money out to any address:

```sh
railctl -profile alice status $ID          # pending until the network finalises it
railctl -profile alice balance

PAY=0x$(openssl rand -hex 32)
railctl -profile alice -fee 10000 -relayer $(railctl -profile relayer account) \
        -out pay.json transfer $PAY $(railctl -profile bob account) 10000000
railctl -profile relayer relay pay.json

OUT=0x$(openssl rand -hex 32)
railctl -profile bob -fee 10000 -relayer $(railctl -profile relayer account) \
        -out out.json withdraw $OUT 0xSomeExchangeDepositAddress 5000000
railctl -profile relayer relay out.json
```

On a real network, `pending` turns into `confirmed` by itself once the
network finalises the block, which on Arbitrum takes about twenty minutes.
That wait is the safety model working, not a hang: money that is only
probably yours is not yours.

## Going live

The same steps, minus the play token. Deploy the vault against the real
stablecoin:

```sh
cd contracts
TOKEN=0xFd086bC7CD5C481DCC9C85ebE478A1C0b69FCbb9 \
  forge script script/Deploy.s.sol --rpc-url $RPC --private-key $YOUR_KEY --broadcast
```

Fill `rail` into a copy of `deployments/arbitrum-one.json` and hand that file
to whoever is joining. That is the whole of it: no operator contract, no
sponsorship to fund, nothing to keep running. Deploying costs you gas once,
and afterwards you have no power over anyone's money, including your own
users'.

Someone has to relay. That can be you, a participant, or several unrelated
parties competing; every operation names the one that may carry it. A relayer
needs ETH at its address, and earns its quoted fee in tokens when the
operation executes.

## Use railctl

| command | what it does |
|---|---|
| `init <profile> <domain-file>` | join a domain: make a key, remember the settings |
| `account` | your address on this domain |
| `balance [address]` | vault balance and tokens in hand |
| `deposit <id> <account> <amount>` | put tokens into the vault, crediting any account |
| `transfer <id> <recipient> <amount>` | pay another account inside the vault |
| `withdraw <id> <destination> <amount>` | send tokens out to any address |
| `relay <file>` | deliver someone's signed instruction, paying its gas |
| `status <id>` | what happened to an operation |
| `abandon <id>` | stop signing anything further for this operation |

Flags go **before** the command. The ones that matter:

| flag | |
|---|---|
| `-fee n` | what the relayer earns, in token units |
| `-relayer addr` | who may deliver it (default: yourself, which needs ETH) |
| `-valid-for d` | how long the signature lives (default 1h) |
| `-out path` | write the signed instruction instead of sending it; `-` for stdout |
| `-profile name` | act as another identity |
| `-json` | machine-readable output |

Identifiers must be fresh and unguessable: `openssl rand -hex 32`. Re-running
any command with the same identifier and the same terms is safe.

State lives in `~/.juice-rail`: `config.json` (domains and profiles),
`credentials.json` (keys, mode 0600), and one database per profile. For
automation, `-config`, `-store` and `RAILCTL_KEY` replace all of it, and then
no home directory is read at all.

## Operate it

| status | meaning |
|---|---|
| `unknown` | never heard of this identifier |
| `pending` | may still happen; a failed attempt is not a failure |
| `confirmed` | it happened, and the network can no longer change its mind |
| `failed` | it can never happen; whatever you reserved can be released |

Only `confirmed` and `failed` are final. Retry is always safe, so retry is the
whole recovery procedure. If a relayer takes your instruction and disappears,
sign another one naming a different relayer and send that instead; the vault
executes at most one of them, so there is no waiting and no risk of paying
twice.

Check a relayer's fuel with `cast balance <address> --rpc-url $RPC`.

## Verify the build

```sh
go build ./... && go test ./...            # library, records, CLI, state machine
go test -race ./...
cd contracts && forge test                 # unit, fuzz and invariant tests
FOUNDRY_PROFILE=deep forge test            # the same, much harder
halmos --contract JuiceRailSymbolicTest    # nine proofs about the vault
go test -tags integration ./integration-tests/
```

The scenario tests are opt-in and fail loudly if `anvil` is missing, rather
than passing quietly. `go test ./...` needs no network.

## Layout

```text
contracts/           JuiceRail, its tests, invariants and proofs
go/rail/             the library: intents, signing, relaying, confirmation
go/sqlite/           the durable records
cmd/railctl/         the command-line tool
deployments/         one file per domain
integration-tests/   the six scenarios, on a local network
```
