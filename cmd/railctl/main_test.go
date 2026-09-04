package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/rpc"
)

func testConfig() config {
	return config{
		Name:     "local",
		ChainID:  31337,
		RPC:      "http://127.0.0.1:8545",
		Token:    "0x00000000000000000000000000000000000000a0",
		Decimals: 6,
		Finality: rpc.FinalizedBlockNumber,
		Venue: venueConfig{
			Router:  "0x00000000000000000000000000000000000000b0",
			Quoter:  "0x00000000000000000000000000000000000000c0",
			WETH:    "0x00000000000000000000000000000000000000d0",
			FeeTier: 500,
		},
		Gas: gasConfig{
			Min:         "20000000000000000",
			Max:         "50000000000000000",
			SlippageBps: 50,
			FeeBound:    "10000000000000000",
		},
	}
}

func TestConfigBecomesAUsableDomain(t *testing.T) {
	d, err := testConfig().domain()
	if err != nil {
		t.Fatalf("domain: %v", err)
	}
	if err := d.Validate(); err != nil {
		t.Fatalf("the domain is not usable: %v", err)
	}
	if d.Gas.Min.String() != "20000000000000000" || d.Venue.FeeTier != 500 {
		t.Fatalf("domain came out as %+v", d)
	}
}

// The settlement tag is a trust decision, so a file that leaves it out is
// refused rather than given a default nobody chose.
func TestConfigWithoutASettlementTagIsRefused(t *testing.T) {
	var c config
	if err := json.Unmarshal([]byte(`{"finality": "latest"}`), &c); err != nil || c.Finality != rpc.LatestBlockNumber {
		t.Fatalf("the tag does not decode from its name: %v %v", c.Finality, err)
	}
	c = testConfig()
	c.Finality = 0
	d, err := c.domain()
	if err != nil {
		t.Fatalf("domain: %v", err)
	}
	if err := d.Validate(); err == nil {
		t.Fatal("a domain with no settlement tag was accepted")
	}
}

func TestConfigRefusesAnUnreadableReserve(t *testing.T) {
	c := testConfig()
	c.Gas.Min = "0.02"
	if _, err := c.domain(); err == nil {
		t.Fatal("a fractional reserve was accepted; the reserve is whole wei")
	}
	c = testConfig()
	c.Gas.FeeBound = ""
	if _, err := c.domain(); err == nil {
		t.Fatal("a missing fee bound was accepted")
	}
}

func TestProfileNamesAreRestricted(t *testing.T) {
	for _, good := range []string{"alice", "a", "bob-2", "test_domain", "0"} {
		if err := checkProfileName(good); err != nil {
			t.Fatalf("%q: %v", good, err)
		}
	}
	for _, bad := range []string{"", "-alice", "Alice", "a/b", "../etc", strings.Repeat("a", 33)} {
		if err := checkProfileName(bad); err == nil {
			t.Fatalf("%q was accepted as a profile name", bad)
		}
	}
}

func TestResolvePrefersExplicitInputsOverTheHomeDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "domain.json")
	blob, _ := json.Marshal(testConfig())
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("HOME", filepath.Join(dir, "no-such-home"))
	t.Setenv("RAILCTL_KEY", "4c0883a69102937d6231471b5dbb6204fe5129617082792ae468d01a3f362318")

	in, err := resolve(options{config: path, store: filepath.Join(dir, "rail.db")})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if in.domain.Name != "local" || in.key == "" || in.store == "" {
		t.Fatalf("resolved as %+v", in)
	}
}

func TestResolveSaysHowToCreateAProfile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("RAILCTL_KEY", "")
	t.Setenv("RAILCTL_PROFILE", "")
	if _, err := resolve(options{}); err == nil || !strings.Contains(err.Error(), "railctl init") {
		t.Fatalf("with nothing configured the error is %v", err)
	}
}

func TestCredentialsMustNotBeReadableByOthers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, credentialsFile)
	if err := os.WriteFile(path, []byte(`{"profiles":{}}`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := loadCredentials(dir); err == nil {
		t.Fatal("a world-readable key file was accepted")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if _, err := loadCredentials(dir); err != nil {
		t.Fatalf("a private key file was refused: %v", err)
	}
}

func TestWriteJSONReplacesAtomicallyAndKeepsItsMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "creds.json")
	if err := writeJSON(path, credentials{Profiles: map[string]string{"a": "1"}}, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := writeJSON(path, credentials{Profiles: map[string]string{"a": "2"}}, 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("key file is %#o", info.Mode().Perm())
	}
	var got credentials
	raw, _ := os.ReadFile(path)
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.Profiles["a"] != "2" {
		t.Fatalf("file holds %v", got.Profiles)
	}
	// No temporary files survive.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("%d files left behind", len(entries))
	}
}

func TestHaltPointsAreTheDurableSteps(t *testing.T) {
	for _, good := range []string{"", "intent", "submit"} {
		if !validHaltPoint(good) {
			t.Fatalf("%q is a durable step", good)
		}
	}
	for _, bad := range []string{"sign", "confirm", "anything"} {
		if validHaltPoint(bad) {
			t.Fatalf("%q was accepted as a durable step", bad)
		}
	}
}
