package main

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/daios-ai/juice-rail/go/rail"
)

// fakeNode answers the two reads the token check makes, over loopback. It is
// the only network any test here touches.
func fakeNode(t *testing.T, hasEip3009 bool) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params []json.RawMessage
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode rpc request: %v", err)
			return
		}
		reply := func(result string) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":%q}`, req.ID, result)
		}
		if req.Method != "eth_call" {
			reply("0x")
			return
		}
		var call struct {
			Input string `json:"input"`
			Data  string `json:"data"`
		}
		if err := json.Unmarshal(req.Params[0], &call); err != nil {
			t.Errorf("decode call: %v", err)
			return
		}
		data := call.Input
		if data == "" {
			data = call.Data
		}
		if len(data) < 10 {
			reply("0x")
			return
		}
		switch data[:10] {
		case selector("DOMAIN_SEPARATOR()"):
			reply("0x" + strings.Repeat("11", 32))
		case selector("authorizationState(address,bytes32)"):
			if !hasEip3009 {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32000,"message":"execution reverted"}}`, req.ID)
				return
			}
			reply("0x" + strings.Repeat("00", 32))
		default:
			reply("0x")
		}
	}))
	t.Cleanup(server.Close)
	return server.URL
}

func selector(sig string) string {
	return "0x" + hex.EncodeToString(crypto.Keccak256([]byte(sig))[:4])
}

func testConfig(rpc string) config {
	return config{
		Name:     "local",
		ChainID:  31337,
		RPC:      rpc,
		Rail:     "0x00000000000000000000000000000000000000A1",
		Token:    "0x00000000000000000000000000000000000000B2",
		Finality: "finalized",
	}
}

func writeConfigFile(t *testing.T, dir string, c config) string {
	t.Helper()
	path := filepath.Join(dir, "domain.json")
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestConfigBecomesADomain(t *testing.T) {
	c := testConfig("http://localhost:1")
	d := c.domain()
	if d.ChainID.Uint64() != 31337 || d.Rail != common.HexToAddress(c.Rail) || d.Token != common.HexToAddress(c.Token) {
		t.Fatalf("domain %+v does not match the configuration", d)
	}
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}

	// A missing finality means the only supported one, never a weaker policy.
	c.Finality = ""
	if got := c.domain().Finality; got != "finalized" {
		t.Fatalf("finality defaulted to %q", got)
	}
	c.Finality = "6-confirmations"
	if err := c.domain().Validate(); err == nil {
		t.Fatal("a confirmation-count policy must be refused: confirmed may never revert")
	}
}

func TestHaltPointsAreTheDurableSteps(t *testing.T) {
	for _, ok := range []string{"", "intent", "sign", "submit"} {
		if !validHaltPoint(ok) {
			t.Errorf("%q must be a halt point", ok)
		}
	}
	for _, bad := range []string{"relay", "confirm", "nonsense"} {
		if validHaltPoint(bad) {
			t.Errorf("%q must not be a halt point", bad)
		}
	}
}

func TestProfileNamesAreRestricted(t *testing.T) {
	for _, ok := range []string{"alice", "a", "bob-2", "under_score", strings.Repeat("a", 32)} {
		if err := checkProfileName(ok); err != nil {
			t.Errorf("%q must be allowed: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "Alice", "-alice", "../etc", "a/b", strings.Repeat("a", 33), "space bar"} {
		if err := checkProfileName(bad); err == nil {
			t.Errorf("%q must be refused: it is a filename and a JSON key", bad)
		}
	}
}

func TestKeysComeFromFilesAndTheEnvironment(t *testing.T) {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	hexKey := common.Bytes2Hex(crypto.FromECDSA(key))

	if _, err := parseKey(hexKey, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := parseKey("0x"+hexKey, "test"); err != nil {
		t.Fatalf("a 0x prefix is how keys are pasted: %v", err)
	}
	if _, err := parseKey("", "test"); err == nil {
		t.Fatal("an empty key must be refused")
	}
	if _, err := parseKey("nonsense", "test"); err == nil {
		t.Fatal("a malformed key must be refused")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "key")
	if err := os.WriteFile(path, []byte(hexKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readKeyFile(path)
	if err != nil || got != hexKey {
		t.Fatalf("read %q (%v), want %q", got, err, hexKey)
	}
	if _, err := readKeyFile(filepath.Join(dir, "absent")); err == nil {
		t.Fatal("a missing key file must be refused")
	}
}

func TestResolvePrefersFlagsThenEnvironmentThenProfile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("RAILCTL_KEY", "")
	rpc := fakeNode(t, true)

	// Nothing configured at all.
	if _, err := resolve(options{}); !errors.Is(err, errNoProfile) {
		t.Fatalf("want errNoProfile, got %v", err)
	}

	// Everything passed explicitly: the home directory is never read.
	key, _ := crypto.GenerateKey()
	hexKey := common.Bytes2Hex(crypto.FromECDSA(key))
	t.Setenv("RAILCTL_KEY", hexKey)
	cfgPath := writeConfigFile(t, t.TempDir(), testConfig(rpc))
	in, err := resolve(options{config: cfgPath, store: "/tmp/x.db"})
	if err != nil {
		t.Fatal(err)
	}
	if in.store != "/tmp/x.db" || in.key != hexKey || in.domain.ChainID != 31337 {
		t.Fatalf("overrides ignored: %+v", in)
	}

	// A profile fills in what is missing.
	t.Setenv("RAILCTL_KEY", "")
	if err := initProfile([]string{"alice", cfgPath}, options{}); err != nil {
		t.Fatal(err)
	}
	in, err = resolve(options{profile: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if in.store != filepath.Join(home, ".juice-rail", "alice.db") {
		t.Fatalf("store %q, want the profile's own", in.store)
	}
	if in.key == "" || in.domain.Name != "local" {
		t.Fatalf("the profile did not fill in: %+v", in)
	}

	// An override may refine how a domain is reached, never which domain.
	other := testConfig(rpc)
	other.Rail = "0x00000000000000000000000000000000000000FF"
	otherPath := writeConfigFile(t, t.TempDir(), other)
	if _, err := resolve(options{profile: "alice", config: otherPath}); err == nil {
		t.Fatal("a -config for another deployment must be refused")
	}

	if _, err := resolve(options{profile: "bob"}); err == nil {
		t.Fatal("an unknown profile must be refused")
	}
}

func TestInitRefusesATokenWithoutEip3009(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfgPath := writeConfigFile(t, t.TempDir(), testConfig(fakeNode(t, false)))

	if err := initProfile([]string{"alice", cfgPath}, options{}); err == nil {
		t.Fatal("a token that cannot carry an authorisation must be refused at init")
	}
	if _, err := os.Stat(filepath.Join(home, ".juice-rail", "config.json")); err == nil {
		t.Fatal("a refused init must leave no profile behind")
	}
}

func TestInitIsIdempotentlyRefusedAndKeepsSecretsPrivate(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("RAILCTL_KEY", "")
	cfgPath := writeConfigFile(t, t.TempDir(), testConfig(fakeNode(t, true)))

	if err := initProfile([]string{"alice", cfgPath}, options{}); err != nil {
		t.Fatal(err)
	}
	if err := initProfile([]string{"alice", cfgPath}, options{}); err == nil {
		t.Fatal("an existing profile must not be silently replaced")
	}

	info, err := os.Stat(filepath.Join(home, ".juice-rail", "credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("credentials are readable by others: %#o", perm)
	}

	// A second profile on the same domain is fine; redefining the domain under
	// the same name is not.
	if err := initProfile([]string{"bob", cfgPath}, options{}); err != nil {
		t.Fatal(err)
	}
	moved := testConfig(fakeNode(t, true))
	moved.Rail = "0x00000000000000000000000000000000000000FF"
	if err := initProfile([]string{"carol", writeConfigFile(t, t.TempDir(), moved)}, options{}); err == nil {
		t.Fatal("silently repointing a domain would move everyone's money elsewhere")
	}
}

func TestInitImportsAKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("RAILCTL_KEY", "")

	key, _ := crypto.GenerateKey()
	hexKey := common.Bytes2Hex(crypto.FromECDSA(key))
	keyPath := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyPath, []byte(hexKey), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := writeConfigFile(t, t.TempDir(), testConfig(fakeNode(t, true)))

	if err := initProfile([]string{"alice", cfgPath}, options{keyFile: keyPath}); err != nil {
		t.Fatal(err)
	}
	cr, err := loadCredentials(filepath.Join(home, ".juice-rail"))
	if err != nil {
		t.Fatal(err)
	}
	if cr.Profiles["alice"] != hexKey {
		t.Fatal("the imported key was not the one recorded")
	}
}

func TestWriteJSONReplacesAtomically(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.json")

	if err := writeJSON(path, map[string]string{"a": "1"}, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(path, map[string]string{"a": "2"}, 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("the file is not valid JSON after replacement: %v", err)
	}
	if got["a"] != "2" {
		t.Fatalf("got %v, want the second write", got)
	}
	info, _ := os.Stat(path)
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("permissions %#o, want 0600", perm)
	}
	// No temporary file may be left where a key could be read from it.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("%d files left behind, want 1", len(entries))
	}
}

func TestCredentialsRefuseLooseFilePermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, credentialsFile)
	if err := os.WriteFile(path, []byte(`{"profiles":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCredentials(dir); err == nil {
		t.Fatal("a world-readable key file must be refused")
	}
}

// The usage text is the interface: every command must be in it.
func TestUsageCoversEveryCommand(t *testing.T) {
	for _, command := range []string{
		"init", "account", "balance", "deposit", "transfer", "withdraw", "relay", "status", "abandon",
	} {
		if !strings.Contains(usage, command) {
			t.Errorf("usage does not mention %q", command)
		}
	}
	for _, flagName := range []string{"-fee", "-relayer", "-valid-for", "-out", "-halt-after", "-profile", "-json"} {
		if !strings.Contains(usage, flagName) {
			t.Errorf("usage does not mention %q", flagName)
		}
	}
	// The V1 vocabulary is gone: no operator sponsorship, no settlement.
	for _, stale := range []string{"paymaster", "bundler", "sponsor", "settle "} {
		if strings.Contains(usage, stale) {
			t.Errorf("usage still mentions %q", stale)
		}
	}
}

func TestNoStoreRefusesEveryRecord(t *testing.T) {
	var s rail.Store = noStore{}
	ref := rail.Ref{}
	if err := s.PutIntent(ref, rail.Intent{}); err == nil {
		t.Fatal("the token check keeps no records")
	}
	if _, _, err := s.Intent(ref); err == nil {
		t.Fatal("the token check keeps no records")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

// A destination is signed and then executed faithfully, so a half-pasted
// address must be refused here or not at all.
func TestAddressesAreParsedStrictly(t *testing.T) {
	full := "0x00000000000000000000000000000000000000A1"
	got, err := parseAddress(full, "recipient")
	if err != nil || got != common.HexToAddress(full) {
		t.Fatalf("parsed %s (%v), want %s", got, err, full)
	}
	if _, err := parseAddress(strings.TrimPrefix(full, "0x"), "recipient"); err != nil {
		t.Fatalf("an address without 0x is still an address: %v", err)
	}
	for _, bad := range []string{
		"",
		"0x",
		"0x1234", // truncated: would silently become 0x00..1234
		"0x00000000000000000000000000000000000000A1FF", // too long
		"0xZZ00000000000000000000000000000000000000",   // not hex
		"not an address",
	} {
		if got, err := parseAddress(bad, "recipient"); err == nil {
			t.Errorf("%q was accepted as %s", bad, got)
		}
	}
}
