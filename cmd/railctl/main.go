// Command railctl drives one juice-rail account: create it, watch deposits,
// pay, withdraw and read status. It is the operational and test harness for
// the library, never a wallet product.
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
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/daios-ai/juice-rail/go/rail"
	"github.com/daios-ai/juice-rail/go/sqlite"
)

const usage = `railctl drives one juice-rail account.

usage:
  railctl init <profile> <domain-file>
  railctl [flags] account
  railctl [flags] balance
  railctl [flags] deposits
  railctl [flags] transfer <id> <recipient>   <amount>
  railctl [flags] withdraw <id> <destination> <amount>
  railctl [flags] retry    <id>
  railctl [flags] status   <id>

flags:
  -profile name  which profile to act as (default $RAILCTL_PROFILE, else the
                 current profile recorded by init)
  -json          machine-readable output
  -halt-after s  exit right after a durable step: intent or submit
  -no-refill     refuse a payment that needs a refill instead of buying gas

init flags:
  -key-file p    import an account key instead of generating one

overrides, for automation; each one skips its part of the profile:
  -config path   domain configuration (default $RAILCTL_CONFIG)
  -store path    durable records (default $RAILCTL_STORE)
  RAILCTL_KEY    the account key, hex; it spends the money and pays the gas

Profiles live in ~/.juice-rail: config.json holds domains and profiles,
credentials.json holds keys and nothing else. Secrets are passed as file
paths, never as flag values, because a flag value is world-readable.

Amounts are decimal token units, for example 12.50. Diagnostics go to
stderr, data to stdout.
`

// config is one domain: the chain, the token accounted on it, how to reach it,
// where to buy gas and how much gas to keep. Every address is configuration,
// never code.
type config struct {
	Name      string      `json:"name"`
	ChainID   uint64      `json:"chainId"`
	RPC       string      `json:"rpc"`
	Token     string      `json:"token"`
	Decimals  uint8       `json:"decimals"`
	Finality  string      `json:"finality"`
	FromBlock uint64      `json:"fromBlock"`
	Venue     venueConfig `json:"venue"`
	Gas       gasConfig   `json:"gas"`
}

type venueConfig struct {
	Router   string `json:"router"`
	Quoter   string `json:"quoter"`
	WETH     string `json:"weth"`
	FeeTier  uint32 `json:"feeTier"`
	Router02 bool   `json:"router02"`
}

// gasConfig is the operating reserve, in wei. Text, so no precision is lost
// passing through JSON.
type gasConfig struct {
	Min         string `json:"min"`
	Max         string `json:"max"`
	SlippageBps uint32 `json:"slippageBps"`
	FeeBound    string `json:"feeBound"`
	PaymentGas  uint64 `json:"paymentGas"`
	SwapGas     uint64 `json:"swapGas"`
}

// gasDecimals is how the native currency is displayed. It is never money here,
// only fuel.
const gasDecimals = 18

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
func (c config) domain() (rail.Domain, error) {
	finality := c.Finality
	if finality == "" {
		finality = "finalized"
	}
	gas := rail.GasPolicy{
		SlippageBps: c.Gas.SlippageBps,
		PaymentGas:  c.Gas.PaymentGas,
		SwapGas:     c.Gas.SwapGas,
	}
	var err error
	if gas.Min, err = wei(c.Gas.Min, "gas.min"); err != nil {
		return rail.Domain{}, err
	}
	if gas.Max, err = wei(c.Gas.Max, "gas.max"); err != nil {
		return rail.Domain{}, err
	}
	if gas.FeeBound, err = wei(c.Gas.FeeBound, "gas.feeBound"); err != nil {
		return rail.Domain{}, err
	}
	return rail.Domain{
		Name:      c.Name,
		ChainID:   new(big.Int).SetUint64(c.ChainID),
		Token:     common.HexToAddress(c.Token),
		Finality:  finality,
		FromBlock: c.FromBlock,
		Venue: rail.Venue{
			Router:   common.HexToAddress(c.Venue.Router),
			Quoter:   common.HexToAddress(c.Venue.Quoter),
			WETH:     common.HexToAddress(c.Venue.WETH),
			FeeTier:  c.Venue.FeeTier,
			Router02: c.Venue.Router02,
		},
		Gas: gas,
	}, nil
}

func (c config) decimals() uint8 {
	if c.Decimals == 0 {
		return 6
	}
	return c.Decimals
}

func wei(s, what string) (*big.Int, error) {
	if s == "" {
		return nil, fmt.Errorf("%s is not set", what)
	}
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return nil, fmt.Errorf("%s %q is not a whole number of wei", what, s)
	}
	return v, nil
}

// --- amounts ---

// parseUnits reads a decimal amount into base units, exactly. There is no
// floating point anywhere in this program: money is integers.
func parseUnits(s string, decimals uint8) (*big.Int, error) {
	text := strings.TrimSpace(s)
	if text == "" {
		return nil, errors.New("amount is empty")
	}
	whole, frac, _ := strings.Cut(text, ".")
	if whole == "" {
		whole = "0"
	}
	if len(frac) > int(decimals) {
		return nil, fmt.Errorf("amount %q has more than %d decimal places", s, decimals)
	}
	digits := whole + frac + strings.Repeat("0", int(decimals)-len(frac))
	for _, r := range digits {
		if r < '0' || r > '9' {
			return nil, fmt.Errorf("amount %q is not a decimal number", s)
		}
	}
	v, ok := new(big.Int).SetString(digits, 10)
	if !ok {
		return nil, fmt.Errorf("amount %q is not a decimal number", s)
	}
	return v, nil
}

// formatUnits renders base units for people: two decimal places at least, and
// no trailing noise beyond that.
func formatUnits(v *big.Int, decimals uint8) string {
	if v == nil {
		v = new(big.Int)
	}
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
	whole, frac := new(big.Int).QuoRem(v, scale, new(big.Int))
	digits := fmt.Sprintf("%0*s", int(decimals), frac.String())
	digits = strings.TrimRight(digits, "0")
	for len(digits) < 2 {
		digits += "0"
	}
	return whole.String() + "." + digits
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

// profile names are also filenames and JSON keys, so they are restricted
// rather than escaped.
var profileName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

func checkProfileName(name string) error {
	if !profileName.MatchString(name) {
		return fmt.Errorf("profile %q: use 1 to 32 characters of a-z, 0-9, - or _, starting with a letter or digit", name)
	}
	return nil
}

var errNoProfile = errors.New(`no profile configured: run "railctl init <name> <domain-file>"`)

// railDir is only consulted when an override is missing, so automation that
// passes every input never needs a home directory at all.
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

// writeJSON replaces path atomically. A crash part way through
// credentials.json would otherwise destroy a key, and with it access to money.
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
	case "", "intent", "submit":
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
	noRefill  bool
}

func run() error {
	var opt options
	flag.StringVar(&opt.config, "config", os.Getenv("RAILCTL_CONFIG"), "domain configuration file")
	flag.StringVar(&opt.store, "store", os.Getenv("RAILCTL_STORE"), "durable record store")
	flag.StringVar(&opt.profile, "profile", "", "profile to act as")
	flag.StringVar(&opt.keyFile, "key-file", "", "init: import an account key from this file")
	flag.BoolVar(&opt.asJSON, "json", false, "machine-readable output")
	flag.StringVar(&opt.haltAfter, "halt-after", "", "exit after a durable step: intent, submit")
	flag.BoolVar(&opt.noRefill, "no-refill", false, "refuse a payment that needs a refill")
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

	// init creates the configuration the other commands read, so it runs before
	// any attempt to bind a domain.
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
		return app.balance(ctx)
	case "deposits":
		return app.deposits(ctx)
	case "transfer":
		return app.pay(ctx, rail.KindTransfer, args[1:])
	case "withdraw":
		return app.pay(ctx, rail.KindWithdraw, args[1:])
	case "retry":
		return app.retry(ctx, args[1:])
	case "status":
		return app.status(ctx, args[1:])
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
	// An override may refine how a domain is reached, never which domain it is.
	// The store belongs to the profile's domain, so a different (chain id,
	// token) would bind records to the wrong ledger.
	if haveDomain {
		if in.domain.ChainID != domain.ChainID ||
			common.HexToAddress(in.domain.Token) != common.HexToAddress(domain.Token) {
			return inputs{}, fmt.Errorf(
				"-config is domain (%d, %s) but profile %q is on (%d, %s): pass -store and RAILCTL_KEY as well",
				in.domain.ChainID, in.domain.Token, name, domain.ChainID, domain.Token)
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

// initProfile installs a domain, creates one profile that uses it, and says
// what has to be sent before the account can act.
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
	domain, err := c.domain()
	if err != nil {
		return fmt.Errorf("%s: %w", domainFile, err)
	}
	if err := domain.Validate(); err != nil {
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
	// A domain is (chain id, token): silently redefining it would repoint every
	// profile that uses it at another ledger.
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

	// Prove the domain works before any money depends on it: the token must
	// carry permits and the venue must be able to price a refill, or this
	// account could never keep itself in gas.
	if err := checkDomain(c, domain, owner); err != nil {
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

	address := crypto.PubkeyToAddress(owner.PublicKey).Hex()
	fmt.Fprintf(os.Stdout, "profile %s on domain %s, account %s\n", name, c.Name, address)
	fmt.Fprintf(os.Stderr, `
to make this account operational, fund it:
  1. send the stablecoin to %s
  2. send at least %s of the native currency to the same address
  3. wait for finality

after that the account keeps its own gas: it buys more with its own
stablecoin whenever the reserve runs low.
`, address, formatUnits(domain.Gas.Max, gasDecimals))
	return nil
}

// checkDomain dials the domain once to make sure it can be operated at all.
func checkDomain(c config, domain rail.Domain, key *ecdsa.PrivateKey) error {
	ctx := context.Background()
	chain, err := ethclient.DialContext(ctx, c.RPC)
	if err != nil {
		return fmt.Errorf("dial %s: %w", c.RPC, err)
	}
	defer chain.Close()

	if err := sameChain(ctx, chain, domain.ChainID); err != nil {
		return err
	}
	r, err := rail.New(domain, noStore{}, chain, key)
	if err != nil {
		return err
	}
	return r.CheckDomain(ctx)
}

// sameChain refuses an endpoint for a different chain. Records are bound to a
// chain id, and money sent on the wrong one is simply gone.
func sameChain(ctx context.Context, chain *ethclient.Client, want *big.Int) error {
	got, err := chain.ChainID(ctx)
	if err != nil {
		return fmt.Errorf("read chain id: %w", err)
	}
	if got.Cmp(want) != 0 {
		return fmt.Errorf("the endpoint serves chain %s, the domain is chain %s", got, want)
	}
	return nil
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
	opt      options
	rail     *rail.Rail
	store    *sqlite.Store
	chain    *ethclient.Client
	decimals uint8
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
	domain, err := in.domain.domain()
	if err != nil {
		return nil, err
	}
	chain, err := ethclient.DialContext(ctx, in.domain.RPC)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", in.domain.RPC, err)
	}
	if err := sameChain(ctx, chain, domain.ChainID); err != nil {
		chain.Close()
		return nil, err
	}
	store, err := sqlite.Open(in.store, rail.DomainKey(domain.ChainID, domain.Token))
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
	return &app{opt: opt, rail: r, store: store, chain: chain, decimals: in.domain.decimals()}, nil
}

func (a *app) close() {
	a.store.Close()
	a.chain.Close()
}

// parseAddress refuses anything that is not a whole address.
// common.HexToAddress pads and truncates in silence, so a half-pasted
// destination would become a real address nobody holds the key to. Nothing
// downstream can catch that: the chain cannot know an address was a typo.
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

// balance shows the money, and the reserve separately. The reserve is fuel,
// never spendable balance.
func (a *app) balance(ctx context.Context) error {
	token, gas, err := a.rail.Balances(ctx)
	if err != nil {
		return err
	}
	return a.emit(
		map[string]any{
			"account": a.rail.Account().Hex(),
			"balance": formatUnits(token, a.decimals),
			"units":   token.String(),
			"reserve": formatUnits(gas, gasDecimals),
		},
		fmt.Sprintf("balance %s  reserve %s", formatUnits(token, a.decimals), formatUnits(gas, gasDecimals)),
	)
}

// deposits catches up with finalized incoming transfers and lists them.
// Receiving needs no transaction from this account at all.
func (a *app) deposits(ctx context.Context) error {
	if _, err := a.rail.ScanDeposits(ctx); err != nil {
		return err
	}
	all, err := a.rail.Deposits()
	if err != nil {
		return err
	}
	rows := make([]map[string]any, 0, len(all))
	lines := make([]string, 0, len(all))
	for _, d := range all {
		rows = append(rows, map[string]any{
			"tx": d.TxHash.Hex(), "logIndex": d.LogIndex, "from": d.From.Hex(),
			"amount": formatUnits(d.Amount, a.decimals), "block": d.BlockNumber,
		})
		lines = append(lines, fmt.Sprintf("%s from %s (%s#%d)",
			formatUnits(d.Amount, a.decimals), d.From.Hex(), d.TxHash.Hex(), d.LogIndex))
	}
	if a.opt.asJSON {
		return a.emit(map[string]any{"deposits": rows}, "")
	}
	if len(lines) == 0 {
		return a.emit(nil, "no deposits")
	}
	_, err = fmt.Fprintln(os.Stdout, strings.Join(lines, "\n"))
	return err
}

// pay runs one payment through its durable steps. A reserve too low for the
// payment is not an error: it buys gas and says to come back once that is
// confirmed.
func (a *app) pay(ctx context.Context, kind rail.Kind, args []string) error {
	if len(args) != 3 {
		return fmt.Errorf("%s: want <id> <address> <amount>", kind)
	}
	id, err := rail.ParseID(args[0])
	if err != nil {
		return err
	}
	what := "recipient"
	if kind == rail.KindWithdraw {
		what = "destination"
	}
	to, err := parseAddress(args[1], what)
	if err != nil {
		return err
	}
	amount, err := parseUnits(args[2], a.decimals)
	if err != nil {
		return err
	}

	err = a.rail.Prepare(ctx, id, kind, to, amount)
	if errors.Is(err, rail.ErrNeedRefill) {
		if a.opt.noRefill {
			return err
		}
		return a.refill(ctx, amount)
	}
	if err != nil {
		return err
	}
	if a.opt.haltAfter == "intent" {
		return a.report(ctx, id, "intent recorded")
	}
	hash, err := a.rail.Send(ctx, id)
	if err != nil {
		return err
	}
	// A zero hash means the operation had already finished: there was nothing
	// left to send, which is what a repeated command should find.
	note := "already settled"
	if hash != (common.Hash{}) {
		note = "submitted " + hash.Hex()
	}
	if a.opt.haltAfter == "submit" {
		return a.emit(
			map[string]any{"id": id.String(), "tx": hash.Hex(), "submitted": hash != common.Hash{}},
			fmt.Sprintf("%s %s", id, note))
	}
	return a.report(ctx, id, note)
}

// refill buys native currency with the account's own stablecoin. It is
// maintenance, not a payment, so it reports separately and leaves the payment
// for the caller to repeat once the reserve is really there.
func (a *app) refill(ctx context.Context, reserve *big.Int) error {
	id, hash, err := a.rail.Refill(ctx, reserve)
	if err != nil {
		return err
	}
	return a.emit(
		map[string]any{"refill": id.String(), "tx": hash.Hex(), "submitted": true},
		fmt.Sprintf("reserve low: refill %s submitted %s; run the payment again once it is confirmed", id, hash.Hex()),
	)
}

// retry re-sends a recorded operation under the same nonce and terms, paying
// more only in transaction fees.
func (a *app) retry(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("retry: want <id>")
	}
	id, err := rail.ParseID(args[0])
	if err != nil {
		return err
	}
	hash, err := a.rail.Retry(ctx, id)
	if err != nil {
		return err
	}
	note := "nothing to retry"
	if hash != (common.Hash{}) {
		note = "resubmitted " + hash.Hex()
	}
	return a.report(ctx, id, note)
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
		if line == "" {
			return nil
		}
		_, err := fmt.Fprintln(os.Stdout, line)
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(structured)
}

// noStore satisfies rail.Store for the one command that binds a rail without
// keeping records: the domain check at init reads the chain and nothing else.
type noStore struct{}

var errNoStore = errors.New("railctl: this command keeps no records")

func (noStore) PutIntent(common.Address, rail.Intent) error { return errNoStore }
func (noStore) Intent(common.Address, rail.ID) (rail.Intent, bool, error) {
	return rail.Intent{}, false, errNoStore
}
func (noStore) IntentByNonce(common.Address, uint64) (rail.Intent, bool, error) {
	return rail.Intent{}, false, errNoStore
}
func (noStore) Pending(common.Address) ([]rail.Intent, error) { return nil, errNoStore }
func (noStore) AppendSubmission(common.Address, rail.ID, rail.Submission) error {
	return errNoStore
}
func (noStore) Submissions(common.Address, rail.ID) ([]rail.Submission, error) {
	return nil, errNoStore
}
func (noStore) PutFact(common.Address, rail.ID, rail.Fact) error { return errNoStore }
func (noStore) Fact(common.Address, rail.ID) (rail.Fact, bool, error) {
	return rail.Fact{}, false, errNoStore
}
func (noStore) PutDeposit(common.Address, rail.Deposit) error   { return errNoStore }
func (noStore) Deposits(common.Address) ([]rail.Deposit, error) { return nil, errNoStore }
func (noStore) Cursor(common.Address) (uint64, bool, error)     { return 0, false, errNoStore }
func (noStore) PutCursor(common.Address, uint64) error          { return errNoStore }
func (noStore) NonceFloor(common.Address) (uint64, bool, error) { return 0, false, errNoStore }
func (noStore) PutNonceFloor(common.Address, uint64) error      { return errNoStore }
func (noStore) Close() error                                    { return nil }
