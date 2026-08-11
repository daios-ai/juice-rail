//go:build integration

package integration

import (
	"math/big"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// The six user stories of the specification, each driven entirely through the
// compiled binary. Participants never hold ether; a relayer does, and earns a
// stablecoin fee for it.

// fundRail gives an account a rail balance the only way there is: a deposit
// carried by a relayer.
func (h *harness) fundRail(who, relayer *participant, amount int64) {
	h.t.Helper()
	id := freshID(h.t)
	h.mintTo(who.account(), big.NewInt(amount))
	path := who.sign("deposit", id, who.account(), amount, 0, relayer.account())
	relayer.mustRun("relay", path)
	h.settleAndFinalise()
	if got := who.status(id); got != "confirmed" {
		h.t.Fatalf("funding %s: status %s, want confirmed", who.name, got)
	}
}

func mustEqual(t *testing.T, what string, got *big.Int, want int64) {
	t.Helper()
	if got.Cmp(big.NewInt(want)) != 0 {
		t.Fatalf("%s is %s, want %d", what, got, want)
	}
}

// 1. bootstrap: an operator stands up a fresh domain. There is nothing else to
// deploy, nobody to fund, and no privileged party left behind.
func TestStory1Bootstrap(t *testing.T) {
	h := newHarness(t)
	alice := h.participant("alice", aliceKeyHex)

	// The account is the key's own address, known before the chain has ever
	// heard of it, so money can arrive first.
	if got, want := alice.account(), addressOf(t, aliceKeyHex); got != want {
		t.Fatalf("account %s, want the key's own address %s", got, want)
	}
	h.assertNoEther("alice", alice.account())

	mustEqual(t, "a fresh account's balance", h.railBalance(alice.account()), 0)
	mustEqual(t, "a fresh domain's holding", h.heldByRail(), 0)

	// Tokens arriving at the address are the account holder's, not the rail's.
	h.mintTo(alice.account(), tokens(100))
	out := alice.mustRun("balance")
	if !strings.Contains(out, "balance 0 token 100000000") {
		t.Fatalf("balance reported %q", out)
	}
	mustEqual(t, "the rail's holding", h.heldByRail(), 0)
	h.assertSolvent(alice.account())
}

// 2. deposit: stablecoin at an ordinary address becomes a rail balance. The
// payer signs; a relayer pays the gas and takes its fee from the deposit.
func TestStory2Deposit(t *testing.T) {
	h := newHarness(t)
	alice := h.participant("alice", aliceKeyHex)
	relayer := h.participant("relayer", relayerKeyHex)

	// This is also the exchange path: withdraw to your own address first, then
	// sign it into the rail.
	h.mintTo(alice.account(), tokens(100))
	id := freshID(t)
	path := alice.sign("deposit", id, alice.account(), 60_000_000, 20_000, relayer.account())

	// Signing moves nothing.
	if got := alice.status(id); got != "pending" {
		t.Fatalf("status %s before submission, want pending", got)
	}
	if n := h.railEvents(alice.account(), id); n != 0 {
		t.Fatalf("%d events before submission", n)
	}

	relayer.mustRun("relay", path)
	if got := alice.status(id); got != "pending" {
		t.Fatalf("status %s while unmined: submission is not confirmation", got)
	}
	h.mine(1)
	if got := alice.status(id); got != "pending" {
		t.Fatalf("status %s while unfinalized: inclusion is not confirmation", got)
	}
	h.finalise()
	if got := alice.status(id); got != "confirmed" {
		t.Fatalf("status %s after finality, want confirmed", got)
	}

	mustEqual(t, "alice's rail balance", h.railBalance(alice.account()), 60_000_000)
	mustEqual(t, "the relayer's fee", h.railBalance(relayer.account()), 20_000)
	mustEqual(t, "the rail's holding", h.heldByRail(), 60_020_000)
	mustEqual(t, "alice's remaining tokens", h.tokenBalance(alice.account()), 100_000_000-60_020_000)
	if n := h.railEvents(alice.account(), id); n != 1 {
		t.Fatalf("%d events, want exactly 1", n)
	}
	h.assertNoEther("alice", alice.account())
	h.assertSolvent(alice.account(), relayer.account())
}

// 3. transfer: A pays B inside the rail. Conservation holds and the relayer is
// paid from money already backed.
func TestStory3Transfer(t *testing.T) {
	h := newHarness(t)
	alice := h.participant("alice", aliceKeyHex)
	bob := h.participant("bob", bobKeyHex)
	relayer := h.participant("relayer", relayerKeyHex)
	h.fundRail(alice, relayer, 100_000_000)

	heldBefore := h.heldByRail()
	relayerBefore := h.railBalance(relayer.account())

	id := freshID(t)
	path := alice.sign("transfer", id, bob.account(), 10_000_000, 10_000, relayer.account())
	relayer.mustRun("relay", path)
	h.settleAndFinalise()

	if got := alice.status(id); got != "confirmed" {
		t.Fatalf("status %s, want confirmed", got)
	}
	mustEqual(t, "alice's balance", h.railBalance(alice.account()), 100_000_000-10_000_000-10_000)
	mustEqual(t, "bob's balance", h.railBalance(bob.account()), 10_000_000)
	mustEqual(t, "the relayer's balance", h.railBalance(relayer.account()),
		relayerBefore.Int64()+10_000)
	if h.heldByRail().Cmp(heldBefore) != 0 {
		t.Fatalf("a transfer moved tokens: held %s, was %s", h.heldByRail(), heldBefore)
	}
	if n := h.railEvents(alice.account(), id); n != 1 {
		t.Fatalf("%d events, want exactly 1", n)
	}
	h.assertNoEther("alice", alice.account())
	h.assertNoEther("bob", bob.account())
	h.assertSolvent(alice.account(), bob.account(), relayer.account())
}

// 4. withdraw: money leaves the rail to an address the account holder signed,
// which may be an exchange rather than the account itself.
func TestStory4Withdraw(t *testing.T) {
	h := newHarness(t)
	bob := h.participant("bob", bobKeyHex)
	relayer := h.participant("relayer", relayerKeyHex)
	h.fundRail(bob, relayer, 50_000_000)

	// Somewhere else entirely: the destination is a signed term, so it needs
	// no relationship with the rail at all.
	exchange := crypto.PubkeyToAddress(mustKey(t, relayer2KeyHex).PublicKey)
	heldBefore := h.heldByRail()
	relayerBefore := h.railBalance(relayer.account())

	id := freshID(t)
	path := bob.sign("withdraw", id, exchange, 20_000_000, 10_000, relayer.account())
	relayer.mustRun("relay", path)
	h.settleAndFinalise()

	if got := bob.status(id); got != "confirmed" {
		t.Fatalf("status %s, want confirmed", got)
	}
	mustEqual(t, "the destination's tokens", h.tokenBalance(exchange), 20_000_000)
	mustEqual(t, "bob's balance", h.railBalance(bob.account()), 50_000_000-20_000_000-10_000)
	mustEqual(t, "the relayer's balance", h.railBalance(relayer.account()), relayerBefore.Int64()+10_000)
	mustEqual(t, "the rail's holding", h.heldByRail(), heldBefore.Int64()-20_000_000)
	h.assertNoEther("bob", bob.account())
	h.assertSolvent(bob.account(), relayer.account())
}

// 5. retry and conflict: replaying moves money once and reports the same
// outcome; the same identifier under different terms fails loudly and moves
// nothing.
func TestStory5RetryAndConflict(t *testing.T) {
	h := newHarness(t)
	alice := h.participant("alice", aliceKeyHex)
	bob := h.participant("bob", bobKeyHex)
	relayer := h.participant("relayer", relayerKeyHex)
	h.fundRail(alice, relayer, 100_000_000)

	id := freshID(t)
	path := alice.sign("transfer", id, bob.account(), 10_000_000, 10_000, relayer.account())
	relayer.mustRun("relay", path)
	h.settleAndFinalise()
	after := h.railBalance(alice.account())

	// Presenting the same signed operation again costs nothing and changes
	// nothing.
	out := relayer.mustRun("relay", path)
	if !strings.Contains(out, "already executed") {
		t.Fatalf("a replay reported %q", out)
	}
	// Re-running the whole command is equally safe.
	alice.sign("transfer", id, bob.account(), 10_000_000, 10_000, relayer.account())
	h.settleAndFinalise()

	if n := h.railEvents(alice.account(), id); n != 1 {
		t.Fatalf("%d events after two replays, want exactly 1", n)
	}
	if h.railBalance(alice.account()).Cmp(after) != 0 {
		t.Fatal("a replay moved money a second time")
	}
	if got := alice.status(id); got != "confirmed" {
		t.Fatalf("status %s, want the same outcome as before", got)
	}

	// The same identifier with different terms is refused by the write-ahead
	// record, before anything is signed.
	refusal := alice.mustFail("-fee", "10000", "-relayer", relayer.account().Hex(), "-out",
		h.dir+"/conflict.json", "transfer", id, bob.account().Hex(), "99000000")
	if !strings.Contains(refusal, "already recorded") {
		t.Fatalf("conflicting terms were refused with %q", refusal)
	}

	// Even from another installation of the same key, which has no record to
	// consult, the chain refuses: the binding is per account and write-once.
	elsewhere := h.participant("alice-elsewhere", aliceKeyHex)
	other := elsewhere.sign("transfer", id, bob.account(), 99_000_000, 10_000, relayer.account())
	refusal = relayer.mustFail("relay", other)
	if !strings.Contains(refusal, "different terms") {
		t.Fatalf("a conflicting variant was refused with %q", refusal)
	}
	if n := h.railEvents(alice.account(), id); n != 1 {
		t.Fatalf("%d events after the conflict, want exactly 1", n)
	}
	if h.railBalance(alice.account()).Cmp(after) != 0 {
		t.Fatal("the conflict moved money")
	}
	h.assertSolvent(alice.account(), bob.account(), relayer.account())
}

// 6. crash recovery: a restart resumes to exactly-once, an unresponsive
// relayer is replaced without waiting, and an abandoned intent whose variants
// have all expired is terminally failed.
func TestStory6CrashRecovery(t *testing.T) {
	h := newHarness(t)
	alice := h.participant("alice", aliceKeyHex)
	bob := h.participant("bob", bobKeyHex)
	relayer := h.participant("relayer", relayerKeyHex)
	relayer2 := h.participant("relayer2", relayer2KeyHex)
	h.fundRail(alice, relayer, 100_000_000)
	before := h.railBalance(alice.account())

	// Killed after the intent is durable but before anything is signed.
	id := freshID(t)
	alice.mustRun("-halt-after", "intent", "transfer", id, bob.account().Hex(), "10000000")
	if got := alice.status(id); got != "pending" {
		t.Fatalf("status %s after the write-ahead record, want pending", got)
	}

	// Killed after signing, before submission: the restart must reuse the
	// signed variant rather than sign a second one.
	path := alice.sign("transfer", id, bob.account(), 10_000_000, 10_000, relayer.account())
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	alice.sign("transfer", id, bob.account(), 10_000_000, 10_000, relayer.account())
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatal("a restart signed a second variant where the first was still live")
	}

	// The relayer disappears. Another is named at once: no waiting, because
	// the contract will execute at most one variant.
	replacement := alice.sign("transfer", id, bob.account(), 10_000_000, 20_000, relayer2.account())
	relayer2.mustRun("relay", replacement)
	h.settleAndFinalise()

	if got := alice.status(id); got != "confirmed" {
		t.Fatalf("status %s, want confirmed", got)
	}
	mustEqual(t, "alice's balance", h.railBalance(alice.account()),
		before.Int64()-10_000_000-20_000)
	mustEqual(t, "the replacement relayer's fee", h.railBalance(relayer2.account()), 20_000)

	// The first relayer wakes up and presents its variant. It is refused, and
	// nothing moves twice.
	refusal := relayer.mustFail("relay", path)
	if !strings.Contains(refusal, "different terms") {
		t.Fatalf("the losing variant was refused with %q", refusal)
	}
	if n := h.railEvents(alice.account(), id); n != 1 {
		t.Fatalf("%d events, want exactly 1", n)
	}

	// An abandoned intent is terminal only once every variant it signed is
	// finalized-dead. Until then it stays pending, because a signed operation
	// abandonment does not kill can still execute.
	dead := freshID(t)
	alice.sign("transfer", dead, bob.account(), 1_000_000, 10_000, relayer.account(), "-valid-for", "60s")
	alice.mustRun("abandon", dead)
	if got := alice.status(dead); got != "pending" {
		t.Fatalf("status %s: abandonment kills nothing already signed", got)
	}
	h.advanceTime(2 * time.Minute)
	if got := alice.status(dead); got != "pending" {
		t.Fatalf("status %s: expiry is terminal only once finalized", got)
	}
	h.finalise()
	if got := alice.status(dead); got != "failed" {
		t.Fatalf("status %s after expiry past finality, want failed", got)
	}
	if n := h.railEvents(alice.account(), dead); n != 0 {
		t.Fatalf("%d events for an intent that never executed", n)
	}
	h.assertSolvent(alice.account(), bob.account(), relayer.account(), relayer2.account())
}

// A domain is (chain id, contract address): the same agreement cannot be
// discharged on another deployment.
func TestDomainsAreIndependent(t *testing.T) {
	here := newHarness(t)
	there := newHarnessOn(t, defaultChainID+1)

	alice := here.participant("alice", aliceKeyHex)
	relayer := here.participant("relayer", relayerKeyHex)
	here.fundRail(alice, relayer, 50_000_000)

	// The same identifier and terms, signed for this domain, are not a
	// payment on the other one.
	id := freshID(t)
	bob := here.participant("bob", bobKeyHex)
	path := alice.sign("transfer", id, bob.account(), 10_000_000, 10_000, relayer.account())

	elsewhere := there.participant("relayer", relayerKeyHex)
	refusal := elsewhere.mustFail("relay", path)
	if !strings.Contains(refusal, "different domain") {
		t.Fatalf("a variant from another domain was refused with %q", refusal)
	}
	if n := there.railEvents(common.HexToAddress(alice.account().Hex()), id); n != 0 {
		t.Fatalf("%d events on the other domain", n)
	}
}
