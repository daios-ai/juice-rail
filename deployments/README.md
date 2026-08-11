# Domains

A **domain** is one `JuiceRail` deployment, identified by `(chain id, contract
address)`. Every project deploys its own copy per network it uses. Domains are
independent: separate balances, separate identifiers, no bridging.

Each file here is one domain's configuration, as `railctl -config` consumes it,
and as `railctl init` installs it:

```text
name      what the profile calls this domain
chainId   the chain
rail      the JuiceRail deployment, from contracts/script/Deploy.s.sol
token     the stablecoin, which must implement EIP-3009
rpc       your endpoint
finality  "finalized", the only supported mechanism
```

There is nothing else to configure: no operator contract, no bundler and no
sponsorship. A relayer is anyone who holds the chain's native currency and is
willing to be named in a signed operation.

`token` must implement EIP-3009 `receiveWithAuthorization`, or no deposit can
ever be made. `railctl init` checks this against the live token before it
writes a profile. On Arbitrum One that is USDT0 at the address pinned above; on
a test network, deploy `contracts/test/MockUSDT0.sol`.

Local test fixtures write their own temporary configuration at deploy time and
are never tracked here.
