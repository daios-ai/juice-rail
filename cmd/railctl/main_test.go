package main

import (
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
		"account", "balance", "deposit", "settle", "withdraw", "status", "abandon",
		"-config", "-store", "-json", "-halt-after",
		"RAILCTL_KEY", "RAILCTL_PAYMASTER_KEY",
	} {
		if !strings.Contains(usage, name) {
			t.Errorf("usage does not mention %q", name)
		}
	}
}
