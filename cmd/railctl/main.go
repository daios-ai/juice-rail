// Command railctl drives one juice-rail domain: create an account, deposit,
// settle, withdraw, and read status. It is the operational and test harness
// for the library, never a wallet product.
package main

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"os"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/daios-ai/juice-rail/go/erc4337"
	"github.com/daios-ai/juice-rail/go/rail"
	"github.com/daios-ai/juice-rail/go/sqlite"
)

const usage = `railctl drives one juice-rail domain.

usage:
  railctl [flags] account
  railctl [flags] balance [address]
  railctl [flags] deposit  <id> <account> <amount>
  railctl [flags] settle   <id> <creditor> <amount>
  railctl [flags] withdraw <id> <to> <amount>
  railctl [flags] status   <id>
  railctl [flags] abandon  <id>

flags:
  -config path   domain configuration (default $RAILCTL_CONFIG)
  -store path    durable records (default $RAILCTL_STORE)
  -json          machine-readable output
  -halt-after s  exit right after a durable step: intent, sign, or submit

environment:
  RAILCTL_KEY            rail signing key, hex; owns the rail's account
  RAILCTL_PAYMASTER_KEY  paymaster verifying key, hex; operator infrastructure

Amounts are token base units. Diagnostics go to stderr, data to stdout.
`

// config is one domain: (chain id, rail address) plus the addresses and
// endpoints that surround it. Every address is configuration, never code.
type config struct {
	Name              string `json:"name"`
	ChainID           uint64 `json:"chainId"`
	RPC               string `json:"rpc"`
	Bundler           string `json:"bundler"`
	Rail              string `json:"rail"`
	Token             string `json:"token"`
	Paymaster         string `json:"paymaster"`
	EntryPoint        string `json:"entryPoint"`
	SafeSingleton     string `json:"safeSingleton"`
	SafeProxyFactory  string `json:"safeProxyFactory"`
	SafeModule        string `json:"safeModule"`
	SafeModuleSetup   string `json:"safeModuleSetup"`
	MultiSendCallOnly string `json:"multiSendCallOnly"`
	Finality          string `json:"finality"`
}

func loadConfig(path string) (config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return config{}, fmt.Errorf("read config: %w", err)
	}
	var c config
	if err := json.Unmarshal(raw, &c); err != nil {
		return config{}, fmt.Errorf("parse config: %w", err)
	}
	return c, nil
}

// domain turns the configuration into the domain the rail binds to. Every
// address is configuration, so the same file shape serves a local deployment
// and a public one.
func (c config) domain() rail.Domain {
	finality := c.Finality
	if finality == "" {
		finality = "finalized"
	}
	return rail.Domain{
		Name:      c.Name,
		ChainID:   new(big.Int).SetUint64(c.ChainID),
		Rail:      common.HexToAddress(c.Rail),
		Token:     common.HexToAddress(c.Token),
		Paymaster: common.HexToAddress(c.Paymaster),
		Contracts: erc4337.Contracts{
			EntryPoint:        common.HexToAddress(c.EntryPoint),
			SafeSingleton:     common.HexToAddress(c.SafeSingleton),
			SafeProxyFactory:  common.HexToAddress(c.SafeProxyFactory),
			SafeModule:        common.HexToAddress(c.SafeModule),
			SafeModuleSetup:   common.HexToAddress(c.SafeModuleSetup),
			MultiSendCallOnly: common.HexToAddress(c.MultiSendCallOnly),
		},
		Finality: finality,
		Gas:      rail.DefaultGas,
	}
}

// validHaltPoint reports whether s names a durable step to stop after.
func validHaltPoint(s string) bool {
	switch s {
	case "", "intent", "sign", "submit":
		return true
	default:
		return false
	}
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "railctl:", err)
		os.Exit(1)
	}
}

type options struct {
	config    string
	store     string
	asJSON    bool
	haltAfter string
}

func run() error {
	var opt options
	flag.StringVar(&opt.config, "config", os.Getenv("RAILCTL_CONFIG"), "domain configuration file")
	flag.StringVar(&opt.store, "store", os.Getenv("RAILCTL_STORE"), "durable record store")
	flag.BoolVar(&opt.asJSON, "json", false, "machine-readable output")
	flag.StringVar(&opt.haltAfter, "halt-after", "", "exit after a durable step: intent, sign, submit")
	flag.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	flag.Parse()

	args := flag.Args()
	if len(args) == 0 {
		flag.Usage()
		return errors.New("no command")
	}
	if !validHaltPoint(opt.haltAfter) {
		return fmt.Errorf("unknown halt point %q", opt.haltAfter)
	}

	ctx := context.Background()
	app, err := open(ctx, opt)
	if err != nil {
		return err
	}
	defer app.close()

	switch args[0] {
	case "account":
		return app.account(ctx)
	case "balance":
		return app.balance(ctx, args[1:])
	case "deposit":
		return app.operation(ctx, rail.KindDeposit, args[1:])
	case "settle":
		return app.operation(ctx, rail.KindSettle, args[1:])
	case "withdraw":
		return app.operation(ctx, rail.KindWithdraw, args[1:])
	case "status":
		return app.status(ctx, args[1:])
	case "abandon":
		return app.abandon(ctx, args[1:])
	default:
		flag.Usage()
		return fmt.Errorf("unknown command %q", args[0])
	}
}

type app struct {
	opt   options
	rail  *rail.Rail
	store *sqlite.Store
	chain *ethclient.Client
}

func open(ctx context.Context, opt options) (*app, error) {
	if opt.config == "" {
		return nil, errors.New("no -config given")
	}
	if opt.store == "" {
		return nil, errors.New("no -store given")
	}
	c, err := loadConfig(opt.config)
	if err != nil {
		return nil, err
	}

	key, err := keyFromEnv("RAILCTL_KEY")
	if err != nil {
		return nil, err
	}
	paymasterKey, err := keyFromEnv("RAILCTL_PAYMASTER_KEY")
	if err != nil {
		return nil, err
	}

	chain, err := ethclient.DialContext(ctx, c.RPC)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", c.RPC, err)
	}
	domain := c.domain()
	store, err := sqlite.Open(opt.store, rail.DomainKey(domain.ChainID, domain.Rail))
	if err != nil {
		chain.Close()
		return nil, err
	}

	sponsor := rail.LocalSponsor{
		Paymaster:  domain.Paymaster,
		EntryPoint: domain.Contracts.EntryPoint,
		ChainID:    domain.ChainID,
		Key:        paymasterKey,
	}
	r, err := rail.New(domain, store, chain, erc4337.NewHTTPBundler(c.Bundler), sponsor, key)
	if err != nil {
		store.Close()
		chain.Close()
		return nil, err
	}
	return &app{opt: opt, rail: r, store: store, chain: chain}, nil
}

func (a *app) close() {
	a.store.Close()
	a.chain.Close()
}

func keyFromEnv(name string) (*ecdsa.PrivateKey, error) {
	v := strings.TrimPrefix(os.Getenv(name), "0x")
	if v == "" {
		return nil, fmt.Errorf("%s is not set", name)
	}
	k, err := crypto.HexToECDSA(v)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return k, nil
}

func (a *app) account(ctx context.Context) error {
	addr, err := a.rail.Account(ctx)
	if err != nil {
		return err
	}
	return a.emit(map[string]any{"account": addr.Hex()}, addr.Hex())
}

func (a *app) balance(ctx context.Context, args []string) error {
	addr, err := a.rail.Account(ctx)
	if err != nil {
		return err
	}
	if len(args) > 0 {
		addr = common.HexToAddress(args[0])
	}
	balance, err := a.rail.Balance(ctx, addr)
	if err != nil {
		return err
	}
	held, err := a.rail.TokenBalance(ctx, addr)
	if err != nil {
		return err
	}
	return a.emit(
		map[string]any{"account": addr.Hex(), "balance": balance.String(), "token": held.String()},
		fmt.Sprintf("%s balance %s token %s", addr.Hex(), balance, held),
	)
}

// operation runs one intent through its durable steps, stopping wherever
// -halt-after says. The steps are the same for all three kinds.
func (a *app) operation(ctx context.Context, kind rail.Kind, args []string) error {
	if len(args) != 3 {
		return fmt.Errorf("%s: want <id> <address> <amount>", kind)
	}
	id, err := rail.ParseID(args[0])
	if err != nil {
		return err
	}
	party := common.HexToAddress(args[1])
	amount, ok := new(big.Int).SetString(args[2], 10)
	if !ok {
		return fmt.Errorf("amount %q is not an integer", args[2])
	}

	terms := rail.Terms{Kind: kind, Amount: amount}
	if kind == rail.KindDeposit {
		terms.Account = party
	} else {
		self, err := a.rail.Account(ctx)
		if err != nil {
			return err
		}
		terms.Account, terms.Party = self, party
	}

	if err := a.rail.Prepare(ctx, id, terms); err != nil {
		return err
	}
	if a.opt.haltAfter == "intent" {
		return a.report(ctx, id, "intent recorded")
	}

	if err := a.rail.Sign(ctx, id); err != nil {
		return err
	}
	if a.opt.haltAfter == "sign" {
		return a.report(ctx, id, "attempt signed")
	}

	if err := a.rail.Send(ctx, id); err != nil {
		return err
	}
	return a.report(ctx, id, "submitted")
}

func (a *app) status(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("status: want <id>")
	}
	id, err := rail.ParseID(args[0])
	if err != nil {
		return err
	}
	return a.report(ctx, id, "")
}

func (a *app) abandon(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("abandon: want <id>")
	}
	id, err := rail.ParseID(args[0])
	if err != nil {
		return err
	}
	if err := a.rail.Abandon(ctx, id); err != nil {
		return err
	}
	return a.report(ctx, id, "abandoned: no further attempt will be signed")
}

// report prints the intent's status. Status is a property of the identifier,
// so every command ends the same way.
func (a *app) report(ctx context.Context, id rail.ID, note string) error {
	status, err := a.rail.Status(ctx, id)
	if err != nil {
		return err
	}
	out := map[string]any{"id": id.String(), "status": string(status)}
	line := fmt.Sprintf("%s %s", id, status)
	if note != "" {
		out["note"] = note
		line += " (" + note + ")"
	}
	return a.emit(out, line)
}

func (a *app) emit(structured map[string]any, line string) error {
	if !a.opt.asJSON {
		_, err := fmt.Fprintln(os.Stdout, line)
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(structured)
}
