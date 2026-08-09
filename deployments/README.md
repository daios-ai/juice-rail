# Domains

A **domain** is one `JuiceRail` deployment, identified by `(chain id, contract
address)`. Every project deploys its own copy per network it uses. Domains are
independent: separate balances, separate identifiers, no bridging.

Each file here is one domain's configuration, as `railctl -config` consumes it.
`rail` and `paymaster` are filled in from `contracts/script/Deploy.s.sol`;
`rpc` and `bundler` are operator endpoints. The Safe suite, the EntryPoint and
`token` are the canonical deployments on that network.

Local test fixtures write their own temporary configuration at deploy time and
are never tracked here.

`bytecode/` holds the official creation bytecode of the Safe v1.4.1 suite and
Safe4337Module v0.3.0, used to stand up a local stack. It is deployed
unmodified; nothing in this repository recompiles the Safe contracts.
