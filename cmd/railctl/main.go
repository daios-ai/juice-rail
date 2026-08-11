// Command railctl drives one juice-rail domain: create an account, deposit,
// transfer, withdraw, relay other people's signed operations, and read status.
// It is the operational and test harness for the library, never a wallet
// product.
//
// Configuration discovery lives here and nowhere else: the library takes every
// input at construction.
package main

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/daios-ai/juice-rail/go/rail"
	"github.com/daios-ai/juice-rail/go/sqlite"
)

const usage = `railctl drives one juice-rail domain.

usage:
  railctl init <profile> <domain-file>
  railctl [flags] account
  railctl [flags] balance  [address]
  railctl [flags] deposit  <id> <credited-account> <amount>
  railctl [flags] transfer <id> <recipient> <amount>
  railctl [flags] withdraw <id> <destination> <amount>
  railctl [flags] relay    <variant-file|->
  railctl [flags] status   <id>
  railctl [flags] abandon  <id>

flags:
  -profile name  which profile to act as (default $RAILCTL_PROFILE, else the
                 current profile recorded by init)
  -json          machine-readable output
  -halt-after s  exit right after a durable step: intent, sign, or submit

operation flags:
  -fee n         the relay fee, in token base units (default 0)
  -relayer addr  who may submit it, and earns the fee (default: yourself)
  -valid-for d   how long the signed operation lives (default 1h)
  -out path      write the signed operation instead of submitting it, for a
                 relayer to carry; "-" writes to stdout

init flags:
  -key-file p    import an account key instead of generating one

overrides, for automation; each one skips its part of the profile:
  -config path   domain configuration (default $RAILCTL_CONFIG)
  -store path    durable records (default $RAILCTL_STORE)
  RAILCTL_KEY    account key, hex; it signs money and pays gas when relaying

Profiles live in ~/.juice-rail: config.json holds domains and profiles,
credentials.json holds keys and nothing else. Secrets are passed as file
paths, never as flag values, because a flag value is world-readable.

Amounts are token base units. Diagnostics go to stderr, data to stdout.
`

// config is one domain: (chain id, rail address) plus how to reach it. Every
// address is configuration, never code.
type config struct {
	Name     string `json:"name"`
	ChainID  uint64 `json:"chainId"`
	RPC      string `json:"rpc"`
	Rail     string `json:"rail"`
	Token    string `json:"token"`
	Finality string `json:"finality"`
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

// domain turns the configuration into the domain the rail binds to.
func (c config) domain() rail.Domain {
	finality := c.Finality
	if finality == "" {
		finality = "finalized"
	}
	return rail.Domain{
		Name:     c.Name,
		ChainID:  new(big.Int).SetUint64(c.ChainID),
		Rail:     common.HexToAddress(c.Rail),
		Token:    common.HexToAddress(c.Token),
		Finality: finality,
	}
}

// --- profiles: ~/.juice-rail ---

const (
	settingsFile    = "config.json"
	credentialsFile = "credentials.json"
)

// settings records the domains installed and the profiles that use them. It
// holds no secrets, so it needs no special permissions.
type settings struct {
	Current  string             `json:"current"`
	Domains  map[string]config  `json:"domains"`
	Profiles map[string]profile `json:"profiles"`
}

type profile struct {
	Domain string `json:"domain"`
}

// credentials holds keys and nothing else, so one file carries one permission
// rule.
type credentials struct {
	Profiles map[string]string `json:"profiles"`
}

// profileNames are also filenames and JSON keys, so they are restricted rather
// than escaped.
var profileName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

func checkProfileName(name string) error {
	if !profileName.MatchString(name) {
		return fmt.Errorf("profile %q: use 1 to 32 characters of a-z, 0-9, - or _, starting with a letter or digit", name)
	}
	return nil
}

var errNoProfile = errors.New(`no profile configured: run "railctl init <name> <domain-file>"`)

// railDir is only ever consulted when an override is missing, so automation
// that passes every input never needs a home directory at all.
func railDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	return filepath.Join(home, ".juice-rail"), nil
}

// loadSettings reads config.json. A missing file is not an error: it means no
// profile exists yet, which the caller reports with the command that fixes it.
func loadSettings(dir string) (settings, error) {
	var s settings
	raw, err := os.ReadFile(filepath.Join(dir, settingsFile))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return s, fmt.Errorf("read profiles: %w", err)
	}
	if err == nil {
		if err := json.Unmarshal(raw, &s); err != nil {
			return s, fmt.Errorf("parse profiles: %w", err)
		}
	}
	if s.Domains == nil {
		s.Domains = map[string]config{}
	}
	if s.Profiles == nil {
		s.Profiles = map[string]profile{}
	}
	return s, nil
}

// loadCredentials reads the keys, refusing a file others can read.
func loadCredentials(dir string) (credentials, error) {
	var c credentials
	path := filepath.Join(dir, credentialsFile)
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			c.Profiles = map[string]string{}
			return c, nil
		}
		return c, fmt.Errorf("read credentials: %w", err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return c, fmt.Errorf("%s is readable by others (%#o): chmod 600 it", path, perm)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return c, fmt.Errorf("read credentials: %w", err)
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, fmt.Errorf("parse credentials: %w", err)
	}
	if c.Profiles == nil {
		c.Profiles = map[string]string{}
	}
	return c, nil
}

// writeJSON replaces path atomically. A crash part way through credentials.json
// would otherwise destroy a key, and with it access to money.
func writeJSON(path string, v any, mode os.FileMode) error {
	blob, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	blob = append(blob, '\n')

	tmp, err := os.CreateTemp(filepath.Dir(path), ".railctl-*")
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	defer os.Remove(tmp.Name())

	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if _, err := tmp.Write(blob); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return os.Rename(tmp.Name(), path)
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
	profile   string
	keyFile   string
	asJSON    bool
	haltAfter string
	fee       string
	relayer   string
	validFor  time.Duration
	out       string
}

func run() error {
	var opt options
	flag.StringVar(&opt.config, "config", os.Getenv("RAILCTL_CONFIG"), "domain configuration file")
	flag.StringVar(&opt.store, "store", os.Getenv("RAILCTL_STORE"), "durable record store")
	flag.StringVar(&opt.profile, "profile", "", "profile to act as")
	flag.StringVar(&opt.keyFile, "key-file", "", "init: import an account key from this file")
	flag.BoolVar(&opt.asJSON, "json", false, "machine-readable output")
	flag.StringVar(&opt.haltAfter, "halt-after", "", "exit after a durable step: intent, sign, submit")
	flag.StringVar(&opt.fee, "fee", "0", "relay fee in token base units")
	flag.StringVar(&opt.relayer, "relayer", "", "who may submit this operation (default: yourself)")
	flag.DurationVar(&opt.validFor, "valid-for", rail.DefaultValidFor, "how long the signed operation lives")
	flag.StringVar(&opt.out, "out", "", `write the signed operation here instead of submitting it ("-" for stdout)`)
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

	// init creates the configuration the other commands read, so it runs
	// before any attempt to bind a domain.
	if args[0] == "init" {
		return initProfile(args[1:], opt)
	}

	ctx := context.Background()
	app, err := open(ctx, opt)
	if err != nil {
		return err
	}
	defer app.close()

	switch args[0] {
	case "account":
		return app.account()
	case "balance":
		return app.balance(ctx, args[1:])
	case "deposit":
		return app.operation(ctx, rail.KindDeposit, args[1:])
	case "transfer":
		return app.operation(ctx, rail.KindTransfer, args[1:])
	case "withdraw":
		return app.operation(ctx, rail.KindWithdraw, args[1:])
	case "relay":
		return app.relay(ctx, args[1:])
	case "status":
		return app.status(ctx, args[1:])
	case "abandon":
		return app.abandon(ctx, args[1:])
	default:
		flag.Usage()
		return fmt.Errorf("unknown command %q", args[0])
	}
}

// inputs is everything needed to bind a domain, whatever it came from.
type inputs struct {
	domain config
	store  string
	key    string
}

// resolve fills the inputs from the first source that has each one: an
// explicit flag, then the environment, then the profile. The profile is read
// only if something is still missing, so a caller that passes everything never
// touches a home directory.
func resolve(opt options) (inputs, error) {
	in := inputs{store: opt.store, key: os.Getenv("RAILCTL_KEY")}
	haveDomain := false
	if opt.config != "" {
		c, err := loadConfig(opt.config)
		if err != nil {
			return inputs{}, err
		}
		in.domain, haveDomain = c, true
	}
	if haveDomain && in.store != "" && in.key != "" {
		return in, nil
	}

	dir, err := railDir()
	if err != nil {
		return inputs{}, err
	}
	set, err := loadSettings(dir)
	if err != nil {
		return inputs{}, err
	}

	name := opt.profile
	if name == "" {
		name = os.Getenv("RAILCTL_PROFILE")
	}
	if name == "" {
		name = set.Current
	}
	if name == "" {
		return inputs{}, errNoProfile
	}
	if err := checkProfileName(name); err != nil {
		return inputs{}, err
	}
	p, ok := set.Profiles[name]
	if !ok {
		return inputs{}, fmt.Errorf("no profile %q: run %q", name, "railctl init "+name+" <domain-file>")
	}
	domain, ok := set.Domains[p.Domain]
	if !ok {
		return inputs{}, fmt.Errorf("profile %q names domain %q, which is not installed", name, p.Domain)
	}
	// An override may refine how a domain is reached, never which domain it
	// is. The store belongs to the profile's domain, so a different (chain id,
	// rail address) would bind records to the wrong chain.
	if haveDomain {
		if in.domain.ChainID != domain.ChainID ||
			common.HexToAddress(in.domain.Rail) != common.HexToAddress(domain.Rail) {
			return inputs{}, fmt.Errorf(
				"-config is domain (%d, %s) but profile %q is on (%d, %s): pass -store and RAILCTL_KEY as well",
				in.domain.ChainID, in.domain.Rail, name, domain.ChainID, domain.Rail)
		}
	} else {
		in.domain = domain
	}
	if in.store == "" {
		in.store = filepath.Join(dir, name+".db")
	}
	if in.key == "" {
		cr, err := loadCredentials(dir)
		if err != nil {
			return inputs{}, err
		}
		in.key = cr.Profiles[name]
	}
	if in.key == "" {
		return inputs{}, fmt.Errorf("profile %q has no key", name)
	}
	return in, nil
}

// initProfile installs a domain and creates one profile that uses it.
func initProfile(args []string, opt options) error {
	if len(args) != 2 {
		return errors.New("init: want <profile> <domain-file>")
	}
	name, domainFile := args[0], args[1]
	if err := checkProfileName(name); err != nil {
		return err
	}
	c, err := loadConfig(domainFile)
	if err != nil {
		return err
	}
	if c.Name == "" {
		return fmt.Errorf("%s: the domain file has no name", domainFile)
	}
	if err := c.domain().Validate(); err != nil {
		return fmt.Errorf("%s: %w", domainFile, err)
	}

	dir, err := railDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	set, err := loadSettings(dir)
	if err != nil {
		return err
	}
	if _, exists := set.Profiles[name]; exists {
		return fmt.Errorf("profile %q already exists", name)
	}
	// A domain is (chain id, rail address): silently redefining it would
	// repoint every profile that uses it at another vault.
	if installed, ok := set.Domains[c.Name]; ok && installed != c {
		return fmt.Errorf("domain %q is already installed with different contents; pick another name in the domain file", c.Name)
	}
	cr, err := loadCredentials(dir)
	if err != nil {
		return err
	}

	key := ""
	if opt.keyFile != "" {
		if key, err = readKeyFile(opt.keyFile); err != nil {
			return err
		}
	} else {
		fresh, err := crypto.GenerateKey()
		if err != nil {
			return fmt.Errorf("generate key: %w", err)
		}
		key = common.Bytes2Hex(crypto.FromECDSA(fresh))
	}
	owner, err := parseKey(key, "account key")
	if err != nil {
		return err
	}

	// The token must carry EIP-3009 authorisations or no deposit can ever be
	// made on this domain. Better to learn that here than at the first
	// deposit.
	if err := checkDomainToken(c, owner); err != nil {
		return err
	}

	set.Domains[c.Name] = c
	set.Profiles[name] = profile{Domain: c.Name}
	if set.Current == "" {
		set.Current = name
	}
	cr.Profiles[name] = key

	// Credentials first: a profile without its key is recoverable by hand, a
	// key without its profile is harmless.
	if err := writeJSON(filepath.Join(dir, credentialsFile), cr, 0o600); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(dir, settingsFile), set, 0o644); err != nil {
		return err
	}

	fmt.Fprintf(os.Stdout, "profile %s on domain %s, account %s\n",
		name, c.Name, crypto.PubkeyToAddress(owner.PublicKey).Hex())
	fmt.Fprintf(os.Stderr, "run \"railctl -profile %s account\" for the address to fund\n", name)
	return nil
}

// checkDomainToken dials the domain once to make sure its token can take a
// deposit at all.
func checkDomainToken(c config, key *ecdsa.PrivateKey) error {
	ctx := context.Background()
	chain, err := ethclient.DialContext(ctx, c.RPC)
	if err != nil {
		return fmt.Errorf("dial %s: %w", c.RPC, err)
	}
	defer chain.Close()

	r, err := rail.New(c.domain(), noStore{}, chain, key)
	if err != nil {
		return err
	}
	return r.CheckToken(ctx)
}

// readKeyFile takes a key from a file, so no secret ever appears in a command
// line, where any user on the machine could read it.
func readKeyFile(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read key file: %w", err)
	}
	key := strings.TrimSpace(string(raw))
	if _, err := parseKey(key, path); err != nil {
		return "", err
	}
	return key, nil
}

type app struct {
	opt   options
	rail  *rail.Rail
	store *sqlite.Store
	chain *ethclient.Client
}

func open(ctx context.Context, opt options) (*app, error) {
	in, err := resolve(opt)
	if err != nil {
		return nil, err
	}
	key, err := parseKey(in.key, "account key")
	if err != nil {
		return nil, err
	}
	chain, err := ethclient.DialContext(ctx, in.domain.RPC)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", in.domain.RPC, err)
	}
	domain := in.domain.domain()
	store, err := sqlite.Open(in.store, rail.DomainKey(domain.ChainID, domain.Rail))
	if err != nil {
		chain.Close()
		return nil, err
	}
	r, err := rail.New(domain, store, chain, key)
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

// parseAddress refuses anything that is not a whole address. common.HexToAddress
// pads and truncates in silence, so a half-pasted destination would become a
// real address nobody holds the key to, and the signature would make it
// authoritative. Nothing downstream can catch that: the contract cannot know an
// address was a typo.
func parseAddress(s, what string) (common.Address, error) {
	if !common.IsHexAddress(s) {
		return common.Address{}, fmt.Errorf("%s %q is not an Ethereum address", what, s)
	}
	return common.HexToAddress(s), nil
}

// parseKey reads a hex key, with or without the 0x prefix that is how keys are
// usually pasted. source names where it came from, so the error does too.
func parseKey(hex, source string) (*ecdsa.PrivateKey, error) {
	v := strings.TrimPrefix(hex, "0x")
	if v == "" {
		return nil, fmt.Errorf("%s is not set", source)
	}
	k, err := crypto.HexToECDSA(v)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", source, err)
	}
	return k, nil
}

func (a *app) account() error {
	addr := a.rail.Account()
	return a.emit(map[string]any{"account": addr.Hex()}, addr.Hex())
}

func (a *app) balance(ctx context.Context, args []string) error {
	addr := a.rail.Account()
	if len(args) > 0 {
		var err error
		if addr, err = parseAddress(args[0], "address"); err != nil {
			return err
		}
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
	party, err := parseAddress(args[1], map[rail.Kind]string{
		rail.KindDeposit:  "credited account",
		rail.KindTransfer: "recipient",
		rail.KindWithdraw: "destination",
	}[kind])
	if err != nil {
		return err
	}
	amount, ok := new(big.Int).SetString(args[2], 10)
	if !ok {
		return fmt.Errorf("amount %q is not an integer", args[2])
	}
	fee, ok := new(big.Int).SetString(a.opt.fee, 10)
	if !ok {
		return fmt.Errorf("fee %q is not an integer", a.opt.fee)
	}
	relayer := a.rail.Account()
	if a.opt.relayer != "" {
		if relayer, err = parseAddress(a.opt.relayer, "relayer"); err != nil {
			return err
		}
	}
	if relayer != a.rail.Account() && a.opt.out == "" {
		return fmt.Errorf("only %s may submit this operation: write it out with -out and hand it over", relayer)
	}

	terms := rail.Terms{Kind: kind, Account: a.rail.Account(), Party: party, Amount: amount}
	if err := a.rail.Prepare(ctx, id, terms); err != nil {
		return err
	}
	if a.opt.haltAfter == "intent" {
		return a.report(ctx, id, "intent recorded")
	}

	v, err := a.rail.Sign(ctx, id, fee, relayer, a.opt.validFor)
	if err != nil {
		return err
	}
	if a.opt.out != "" {
		if err := a.writeVariant(v); err != nil {
			return err
		}
		return a.report(ctx, id, "signed for "+relayer.Hex())
	}
	if a.opt.haltAfter == "sign" {
		return a.report(ctx, id, "variant signed")
	}

	if _, err := a.rail.Relay(ctx, v); err != nil && !errors.Is(err, rail.ErrExecuted) {
		return err
	}
	// No note: a submission is not an outcome, and after a retry the intent
	// may already have executed without this run presenting anything.
	return a.report(ctx, id, "")
}

// writeVariant hands a signed operation to whoever will carry it.
func (a *app) writeVariant(v rail.Variant) error {
	blob, err := rail.EncodeVariant(a.rail.Domain(), v)
	if err != nil {
		return err
	}
	blob = append(blob, '\n')
	if a.opt.out == "-" {
		_, err := os.Stdout.Write(blob)
		return err
	}
	return os.WriteFile(a.opt.out, blob, 0o600)
}

// relay carries someone else's signed operation, paying its gas. Nothing about
// the operation is taken on trust: the signature, the binding, the deadline
// and the balance are all checked, and the exact call is simulated, before any
// gas is spent.
func (a *app) relay(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("relay: want <variant-file|->")
	}
	var (
		raw []byte
		err error
	)
	if args[0] == "-" {
		raw, err = io.ReadAll(os.Stdin)
	} else {
		raw, err = os.ReadFile(args[0])
	}
	if err != nil {
		return fmt.Errorf("read variant: %w", err)
	}
	env, err := rail.DecodeVariant(raw)
	if err != nil {
		return err
	}
	v, err := a.rail.Accept(env)
	if err != nil {
		return err
	}
	hash, err := a.rail.Relay(ctx, v)
	if errors.Is(err, rail.ErrExecuted) {
		return a.emit(
			map[string]any{"id": v.ID.String(), "account": v.Account.Hex(), "submitted": false, "note": "already executed"},
			fmt.Sprintf("%s already executed", v.ID),
		)
	}
	if err != nil {
		return err
	}
	return a.emit(
		map[string]any{"id": v.ID.String(), "account": v.Account.Hex(), "submitted": true, "tx": hash.Hex()},
		fmt.Sprintf("%s submitted %s", v.ID, hash.Hex()),
	)
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
	return a.report(ctx, id, "abandoned: no further variant will be signed")
}

// report prints the intent's status. Status is a property of the intent, so
// every command ends the same way.
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

// noStore satisfies rail.Store for the one command that binds a rail without
// keeping records: the token check at init reads the chain and nothing else.
type noStore struct{}

var errNoStore = errors.New("railctl: this command keeps no records")

func (noStore) PutIntent(rail.Ref, rail.Intent) error      { return errNoStore }
func (noStore) Intent(rail.Ref) (rail.Intent, bool, error) { return rail.Intent{}, false, errNoStore }
func (noStore) AppendVariant(rail.Ref, rail.Variant) error { return errNoStore }
func (noStore) Variants(rail.Ref) ([]rail.Variant, error)  { return nil, errNoStore }
func (noStore) Abandon(rail.Ref) error                     { return errNoStore }
func (noStore) Abandoned(rail.Ref) (bool, error)           { return false, errNoStore }
func (noStore) PutFact(rail.Ref, rail.Fact) error          { return errNoStore }
func (noStore) Fact(rail.Ref) (rail.Fact, bool, error)     { return rail.Fact{}, false, errNoStore }
func (noStore) Pending() ([]rail.Ref, error)               { return nil, errNoStore }
func (noStore) Close() error                               { return nil }
