package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

const sampleConfig = `{
  "name": "local",
  "chainId": 31337,
  "rpc": "http://127.0.0.1:8545",
  "bundler": "http://127.0.0.1:4337",
  "rail": "0x0000000000000000000000000000000000001001",
  "token": "0x0000000000000000000000000000000000001002",
  "paymaster": "0x0000000000000000000000000000000000001003",
  "entryPoint": "0x0000000000000000000000000000000000002001",
  "safeSingleton": "0x0000000000000000000000000000000000002002",
  "safeProxyFactory": "0x0000000000000000000000000000000000002003",
  "safeModule": "0x0000000000000000000000000000000000002004",
  "safeModuleSetup": "0x0000000000000000000000000000000000002005",
  "multiSendCallOnly": "0x0000000000000000000000000000000000002006"
}`

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "domain.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestConfigBecomesAValidDomain(t *testing.T) {
	c, err := loadConfig(writeConfig(t, sampleConfig))
	if err != nil {
		t.Fatal(err)
	}
	d := c.domain()

	if err := d.Validate(); err != nil {
		t.Fatalf("the sample domain must be valid: %v", err)
	}
	if d.ChainID.Uint64() != 31337 {
		t.Errorf("chain id = %s", d.ChainID)
	}
	if d.Rail != common.HexToAddress("0x1001") || d.Contracts.MultiSendCallOnly != common.HexToAddress("0x2006") {
		t.Errorf("addresses did not survive: %+v", d)
	}
	// Finality defaults to true finality, never to something weaker.
	if d.Finality != "finalized" {
		t.Errorf("finality = %q, want finalized", d.Finality)
	}
}

func TestConfigCannotSelectWeakFinality(t *testing.T) {
	body := strings.Replace(sampleConfig, `"chainId": 31337,`, `"chainId": 31337, "finality": "latest",`, 1)
	c, err := loadConfig(writeConfig(t, body))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.domain().Validate(); err == nil {
		t.Fatal("a weaker finality mechanism must be rejected")
	}
}

func TestIncompleteConfigIsRejected(t *testing.T) {
	body := strings.Replace(sampleConfig,
		`"rail": "0x0000000000000000000000000000000000001001",`, "", 1)
	c, err := loadConfig(writeConfig(t, body))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.domain().Validate(); err == nil {
		t.Fatal("a missing rail address must be rejected")
	}
}

func TestConfigErrorsAreReadable(t *testing.T) {
	if _, err := loadConfig(filepath.Join(t.TempDir(), "absent.json")); err == nil ||
		!strings.Contains(err.Error(), "read config") {
		t.Fatalf("missing file: %v", err)
	}
	if _, err := loadConfig(writeConfig(t, "{not json")); err == nil ||
		!strings.Contains(err.Error(), "parse config") {
		t.Fatalf("malformed file: %v", err)
	}
}

func TestKeyFromEnv(t *testing.T) {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	hex := common.Bytes2Hex(crypto.FromECDSA(key))

	t.Setenv("RAILCTL_KEY", hex)
	got, err := keyFromEnv("RAILCTL_KEY")
	if err != nil {
		t.Fatal(err)
	}
	if crypto.PubkeyToAddress(got.PublicKey) != crypto.PubkeyToAddress(key.PublicKey) {
		t.Fatal("the key did not round trip")
	}

	// The 0x prefix is accepted too, since that is how keys are usually pasted.
	t.Setenv("RAILCTL_KEY", "0x"+hex)
	if _, err := keyFromEnv("RAILCTL_KEY"); err != nil {
		t.Fatalf("prefixed key: %v", err)
	}

	t.Setenv("RAILCTL_KEY", "")
	if _, err := keyFromEnv("RAILCTL_KEY"); err == nil {
		t.Error("an unset key must be reported")
	}
	t.Setenv("RAILCTL_KEY", "not-a-key")
	if _, err := keyFromEnv("RAILCTL_KEY"); err == nil {
		t.Error("a malformed key must be reported")
	}
}

// The halt points name the durable steps, and nothing else.
func TestHaltPoints(t *testing.T) {
	for _, valid := range []string{"", "intent", "sign", "submit"} {
		if !validHaltPoint(valid) {
			t.Errorf("%q must be accepted", valid)
		}
	}
	for _, invalid := range []string{"confirm", "send", "nonsense"} {
		if validHaltPoint(invalid) {
			t.Errorf("%q must be rejected", invalid)
		}
	}
}

// Every command and flag the harness drives is documented.
func TestUsageCoversTheCommandSurface(t *testing.T) {
	for _, name := range []string{
		"init", "account", "balance", "deposit", "settle", "withdraw", "status", "abandon",
		"-config", "-store", "-json", "-halt-after", "-profile",
		"-key-file", "-sponsor-key-file",
		"RAILCTL_KEY", "RAILCTL_PAYMASTER_KEY",
	} {
		if !strings.Contains(usage, name) {
			t.Errorf("usage does not mention %q", name)
		}
	}
}

// --- profiles ---

func testKeyHex(t *testing.T) string {
	t.Helper()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return common.Bytes2Hex(crypto.FromECDSA(key))
}

func writeKeyFile(t *testing.T, hex string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, []byte(hex+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// isolate points the home directory at a fresh directory, so no test can read
// or write the developer's real profiles.
func isolate(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return filepath.Join(home, ".juice-rail")
}

// Everything supplied explicitly must never require a home directory: the
// integration harness passes all four inputs and must not touch one.
func TestResolveNeedsNoProfileWhenEverythingIsGiven(t *testing.T) {
	t.Setenv("HOME", filepath.Join(t.TempDir(), "absent"))
	t.Setenv("RAILCTL_KEY", "0xaa")
	t.Setenv("RAILCTL_PAYMASTER_KEY", "0xbb")

	in, err := resolve(options{config: writeConfig(t, sampleConfig), store: "/tmp/x.db"})
	if err != nil {
		t.Fatal(err)
	}
	if in.domain.ChainID != 31337 || in.store != "/tmp/x.db" || in.key != "0xaa" || in.sponsorKey != "0xbb" {
		t.Fatalf("inputs did not come from the overrides: %+v", in)
	}
}

func TestResolveFallsBackToTheProfile(t *testing.T) {
	dir := isolate(t)
	key, sponsor := testKeyHex(t), testKeyHex(t)
	if err := initProfile([]string{"alice", writeConfig(t, sampleConfig)},
		options{keyFile: writeKeyFile(t, key), sponsorKey: writeKeyFile(t, sponsor)}); err != nil {
		t.Fatal(err)
	}

	// No flags, no environment: everything comes from the profile.
	t.Setenv("RAILCTL_KEY", "")
	t.Setenv("RAILCTL_PAYMASTER_KEY", "")
	in, err := resolve(options{})
	if err != nil {
		t.Fatal(err)
	}
	if in.domain.ChainID != 31337 {
		t.Errorf("domain = %+v", in.domain)
	}
	if want := filepath.Join(dir, "alice.db"); in.store != want {
		t.Errorf("store = %q, want %q", in.store, want)
	}
	if in.key != key || in.sponsorKey != sponsor {
		t.Error("keys did not come from the credentials file")
	}

	// The environment still wins over the profile.
	t.Setenv("RAILCTL_KEY", "0xcc")
	if in, err = resolve(options{}); err != nil || in.key != "0xcc" {
		t.Fatalf("env must take precedence: %q %v", in.key, err)
	}
}

// An override may say how a domain is reached, never which domain it is: the
// profile's store and sponsorship key belong to its own domain.
func TestResolveRefusesAnOverrideForAnotherDomain(t *testing.T) {
	isolate(t)
	t.Setenv("RAILCTL_KEY", "")
	t.Setenv("RAILCTL_PAYMASTER_KEY", "")
	if err := initProfile([]string{"alice", writeConfig(t, sampleConfig)},
		options{sponsorKey: writeKeyFile(t, testKeyHex(t))}); err != nil {
		t.Fatal(err)
	}

	// Same domain, different endpoint: legitimate, the profile supplies the rest.
	moved := strings.Replace(sampleConfig,
		`"rpc": "http://127.0.0.1:8545"`, `"rpc": "http://127.0.0.1:9999"`, 1)
	in, err := resolve(options{config: writeConfig(t, moved)})
	if err != nil {
		t.Fatalf("an override on the same domain must be accepted: %v", err)
	}
	if in.domain.RPC != "http://127.0.0.1:9999" {
		t.Errorf("the override must win: %q", in.domain.RPC)
	}

	for _, elsewhere := range []string{
		strings.Replace(sampleConfig, `"chainId": 31337`, `"chainId": 42161`, 1),
		strings.Replace(sampleConfig,
			`"rail": "0x0000000000000000000000000000000000001001"`,
			`"rail": "0x0000000000000000000000000000000000009999"`, 1),
	} {
		_, err := resolve(options{config: writeConfig(t, elsewhere)})
		if err == nil || !strings.Contains(err.Error(), "profile") {
			t.Fatalf("another domain must be refused, got %v", err)
		}
	}
}

// A missing profile must name the command that creates one.
func TestResolveWithoutAnyProfileNamesTheFix(t *testing.T) {
	isolate(t)
	t.Setenv("RAILCTL_KEY", "")
	t.Setenv("RAILCTL_PAYMASTER_KEY", "")

	_, err := resolve(options{})
	if !errors.Is(err, errNoProfile) {
		t.Fatalf("want errNoProfile, got %v", err)
	}
	if !strings.Contains(err.Error(), "railctl init") {
		t.Errorf("the error must name the fix: %v", err)
	}
}

func TestProfileNamesAreRestricted(t *testing.T) {
	for _, ok := range []string{"a", "alice", "bob-2", "prod_1", strings.Repeat("x", 32)} {
		if err := checkProfileName(ok); err != nil {
			t.Errorf("%q must be accepted: %v", ok, err)
		}
	}
	// A name becomes a filename and a JSON key, so traversal and surprises are
	// refused rather than escaped.
	for _, bad := range []string{"", "..", "../escape", "/absolute", "a/b", "Alice", "a b", "-lead", strings.Repeat("x", 33)} {
		if err := checkProfileName(bad); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
}

func TestInitWritesTheProfileAndRefusesToClobber(t *testing.T) {
	dir := isolate(t)
	domainFile := writeConfig(t, sampleConfig)
	sponsor := writeKeyFile(t, testKeyHex(t))

	if err := initProfile([]string{"alice", domainFile}, options{sponsorKey: sponsor}); err != nil {
		t.Fatal(err)
	}

	set, err := loadSettings(dir)
	if err != nil {
		t.Fatal(err)
	}
	if set.Current != "alice" {
		t.Errorf("current = %q, want alice", set.Current)
	}
	if set.Profiles["alice"].Domain != "local" {
		t.Errorf("profile = %+v", set.Profiles["alice"])
	}
	if set.Domains["local"].ChainID != 31337 {
		t.Errorf("domain = %+v", set.Domains["local"])
	}

	// A generated key is a real key, and credentials are private.
	cr, err := loadCredentials(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseKey(cr.Profiles["alice"], "generated"); err != nil {
		t.Errorf("generated key: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, credentialsFile))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("credentials mode = %#o, want 0600", perm)
	}

	if err := initProfile([]string{"alice", domainFile}, options{sponsorKey: sponsor}); err == nil {
		t.Error("an existing profile must not be overwritten")
	}

	// A second profile on the installed domain needs no sponsorship key, and
	// does not disturb the recorded current profile.
	if err := initProfile([]string{"bob", domainFile}, options{}); err != nil {
		t.Fatalf("second profile: %v", err)
	}
	if set, _ = loadSettings(dir); set.Current != "alice" {
		t.Errorf("current changed to %q", set.Current)
	}
}

// Redefining an installed domain would repoint existing profiles at another
// vault, which is the store's ErrWrongDomain one level up.
func TestInitRefusesAConflictingDomain(t *testing.T) {
	isolate(t)
	sponsor := writeKeyFile(t, testKeyHex(t))
	if err := initProfile([]string{"alice", writeConfig(t, sampleConfig)},
		options{sponsorKey: sponsor}); err != nil {
		t.Fatal(err)
	}

	elsewhere := strings.Replace(sampleConfig,
		`"rail": "0x0000000000000000000000000000000000001001"`,
		`"rail": "0x0000000000000000000000000000000000009999"`, 1)
	err := initProfile([]string{"bob", writeConfig(t, elsewhere)}, options{sponsorKey: sponsor})
	if err == nil || !strings.Contains(err.Error(), "already installed") {
		t.Fatalf("a changed domain must be refused, got %v", err)
	}

	// The same contents again are not a conflict.
	if err := initProfile([]string{"carol", writeConfig(t, sampleConfig)}, options{}); err != nil {
		t.Fatalf("an unchanged domain must be accepted: %v", err)
	}
}

func TestInitRefusesSecretsItCannotRead(t *testing.T) {
	isolate(t)
	domainFile := writeConfig(t, sampleConfig)

	// A new domain without a sponsorship key cannot work, and says so.
	err := initProfile([]string{"alice", domainFile}, options{})
	if err == nil || !strings.Contains(err.Error(), "-sponsor-key-file") {
		t.Fatalf("want the flag named, got %v", err)
	}
	// Key files are validated, not trusted.
	if _, err := readKeyFile(writeKeyFile(t, "not-a-key")); err == nil {
		t.Error("a malformed key file must be refused")
	}
	if _, err := readKeyFile(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("a missing key file must be refused")
	}
}

func TestCredentialsReadableByOthersAreRefused(t *testing.T) {
	dir := isolate(t)
	if err := initProfile([]string{"alice", writeConfig(t, sampleConfig)},
		options{sponsorKey: writeKeyFile(t, testKeyHex(t))}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, credentialsFile)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCredentials(dir); err == nil || !strings.Contains(err.Error(), "readable by others") {
		t.Fatalf("loose permissions must be refused, got %v", err)
	}
}

// A half-written file must never replace a good one, since for credentials
// that would destroy a key.
func TestWriteJSONReplacesAtomically(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	if err := writeJSON(path, settings{Current: "first"}, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(path, settings{Current: "second"}, 0o600); err != nil {
		t.Fatal(err)
	}
	var got settings
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("the file must always be valid JSON: %v", err)
	}
	if got.Current != "second" {
		t.Errorf("current = %q, want second", got.Current)
	}

	// A failure leaves the previous contents and no debris behind.
	if err := writeJSON(path, make(chan int), 0o600); err == nil {
		t.Fatal("an unencodable value must fail")
	}
	if raw, err = os.ReadFile(path); err != nil || !strings.Contains(string(raw), "second") {
		t.Fatalf("the good file must survive: %q %v", raw, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".railctl-") {
			t.Errorf("temporary file left behind: %s", e.Name())
		}
	}
}
