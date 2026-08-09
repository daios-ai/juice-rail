//go:build integration

package integration

import (
	"math/big"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// The six user stories. These are the only use cases; each runs against the
// compiled railctl binary on its own fresh chain, deployment and store.

// 1. bootstrap: the operator stands up a domain, funds sponsorship, and
// derives account addresses before anything is deployed.
func TestStory1Bootstrap(t *testing.T) {
	h := newHarness(t)
	alice := h.operator("alice", railKeyHex)

	account := alice.account()
	if account == (common.Address{}) {
		t.Fatal("an account address must be derivable")
	}
	if again := alice.account(); again != account {
		t.Fatalf("the address must be stable before deployment: %s then %s", account, again)
	}

	// Nothing exists on chain yet, and nothing is owed.
	if code, err := h.client.CodeAt(t.Context(), account, nil); err != nil || len(code) != 0 {
		t.Fatalf("the account must still be counterfactual: %d bytes of code, %v", len(code), err)
	}
	if got := h.railBalance(account); got.Sign() != 0 {
		t.Fatalf("a fresh account has no balance, got %s", got)
	}
	if got := h.heldByRail(); got.Sign() != 0 {
		t.Fatalf("a fresh domain holds nothing, got %s", got)
	}

	// Money can arrive before the account exists.
	h.mintTo(account, tokens(100))
	if got := h.tokenBalance(account); got.Cmp(tokens(100)) != 0 {
		t.Fatalf("tokens must reach a counterfactual account, got %s", got)
	}

	// Sponsorship is the operator's, not the account holder's.
	if h.paymasterDeposit().Sign() <= 0 {
		t.Fatal("the paymaster must be funded")
	}
	if balance, err := h.client.BalanceAt(t.Context(), account, nil); err != nil || balance.Sign() != 0 {
		t.Fatalf("an account holder must need no ETH, got %s %v", balance, err)
	}
}

// 2. deposit: a payer funds an account; the status is pending until finality
// and confirmed only after, and the host credits exactly once.
func TestStory2Deposit(t *testing.T) {
	h := newHarness(t)
	alice := h.operator("alice", railKeyHex)
	account := alice.account()
	h.mintTo(account, tokens(100))

	const id = "0x0000000000000000000000000000000000000000000000000000000000000002"
	alice.mustRun("deposit", id, account.Hex(), tokens(60).String())

	// Accepted by the bundler, but nothing has been mined.
	if got := alice.status(id); got != "pending" {
		t.Fatalf("before inclusion: status = %s, want pending", got)
	}

	// Included, but not yet final: still pending, and the host must not credit.
	h.mine(1)
	if got := h.railBalance(account); got.Cmp(tokens(60)) != 0 {
		t.Fatalf("the deposit must have executed on chain, got %s", got)
	}
	if got := alice.status(id); got != "pending" {
		t.Fatalf("executed but unfinalized: status = %s, want pending", got)
	}

	// Final: only now may a host move its own ledger.
	h.finalise()
	if got := alice.status(id); got != "confirmed" {
		t.Fatalf("finalized: status = %s, want confirmed", got)
	}

	if got := h.railBalance(account); got.Cmp(tokens(60)) != 0 {
		t.Fatalf("balance = %s, want 60", got)
	}
	if got := h.heldByRail(); got.Cmp(tokens(60)) != 0 {
		t.Fatalf("held = %s, want 60", got)
	}
	if got := h.railEvents(id); got != 1 {
		t.Fatalf("the identifier must move money exactly once, got %d events", got)
	}
	if got := h.tokenBalance(account); got.Cmp(tokens(40)) != 0 {
		t.Fatalf("the payer's remaining tokens = %s, want 40", got)
	}
	assertSolvent(t, h, account)
}

// 3. settle: one account pays another under an agreed identifier; conservation
// holds, and the same settlement on a different domain is refused.
func TestStory3Settle(t *testing.T) {
	h := newHarness(t)
	alice := h.operator("alice", railKeyHex)
	bob := h.operator("bob", otherRailKeyHex)

	debtor, creditor := alice.account(), bob.account()
	h.mintTo(debtor, tokens(100))

	const depositID = "0x0000000000000000000000000000000000000000000000000000000000000031"
	alice.mustRun("deposit", depositID, debtor.Hex(), tokens(100).String())
	h.settleAndFinalise()

	before := h.heldByRail()
	const settlementID = "0x0000000000000000000000000000000000000000000000000000000000000032"
	alice.mustRun("settle", settlementID, creditor.Hex(), tokens(40).String())
	h.settleAndFinalise()

	if got := alice.status(settlementID); got != "confirmed" {
		t.Fatalf("status = %s, want confirmed", got)
	}
	if got := h.railBalance(debtor); got.Cmp(tokens(60)) != 0 {
		t.Fatalf("debtor = %s, want 60", got)
	}
	if got := h.railBalance(creditor); got.Cmp(tokens(40)) != 0 {
		t.Fatalf("creditor = %s, want 40", got)
	}
	// A settlement moves balances between accounts and no tokens at all.
	if got := h.heldByRail(); got.Cmp(before) != 0 {
		t.Fatalf("held changed from %s to %s", before, got)
	}

	assertSolvent(t, h, debtor, creditor)

	// A debt owed here cannot be paid anywhere else. A second settlement is
	// agreed on this domain, but the debtor executes it on another one —
	// where tokens are free.
	const owedHere = "0x0000000000000000000000000000000000000000000000000000000000000033"
	alice.mustRun("-halt-after", "intent", "settle", owedHere, creditor.Hex(), tokens(40).String())
	if got := alice.status(owedHere); got != "pending" {
		t.Fatalf("the agreed settlement starts pending, got %s", got)
	}

	other := newHarnessOn(t, defaultChainID+1)
	otherAlice := other.operator("alice", railKeyHex)
	other.mintTo(otherAlice.account(), tokens(100))
	otherAlice.mustRun("deposit", depositID, otherAlice.account().Hex(), tokens(100).String())
	other.settleAndFinalise()
	otherAlice.mustRun("settle", owedHere, creditor.Hex(), tokens(40).String())
	other.settleAndFinalise()

	// It executed there, under the same identifier, and paid the creditor there.
	if got := otherAlice.status(owedHere); got != "confirmed" {
		t.Fatalf("the payment must execute on its own domain, got %s", got)
	}
	if got := other.railBalance(creditor); got.Cmp(tokens(40)) != 0 {
		t.Fatalf("the other domain credited %s, want 40", got)
	}

	// Here, where the debt is owed, the very same identifier is still unpaid.
	// Mining past finality does not change that.
	h.settleAndFinalise()
	if got := alice.status(owedHere); got != "pending" {
		t.Fatalf("a payment made elsewhere must not settle the debt here, got %s", got)
	}
	if got := h.railEvents(owedHere); got != 0 {
		t.Fatalf("nothing may have executed here, got %d events", got)
	}
	if got := h.railBalance(creditor); got.Cmp(tokens(40)) != 0 {
		t.Fatalf("the creditor's balance here must be unchanged, got %s", got)
	}
	if h.chainID.Cmp(other.chainID) == 0 {
		t.Fatal("the two domains must be distinct")
	}

	// Records cannot be shared between domains either: identifiers mean one
	// thing per domain, so a store claimed by one refuses the other rather
	// than quietly merging two ledgers.
	shared := filepath.Join(t.TempDir(), "shared.db")
	here := &operator{h: h, name: "shared-here", key: railKeyHex, store: shared}
	there := &operator{h: other, name: "shared-there", key: railKeyHex, store: shared}
	here.mustRun("account")
	if out, err := there.run("account"); err == nil {
		t.Fatalf("one store must not serve two domains, got %q", out)
	} else if !strings.Contains(err.Error(), "different domain") {
		t.Fatalf("the refusal must say why, got %v", err)
	}

	assertSolvent(t, h, debtor, creditor)
	assertSolvent(t, other, otherAlice.account(), creditor)
}

// 4. withdraw: money leaves the contract, balance and holdings fall equally,
// and a terminally failed withdrawal is what releases a host's reserve.
func TestStory4Withdraw(t *testing.T) {
	h := newHarness(t)
	alice := h.operator("alice", railKeyHex)
	account := alice.account()
	recipient := common.HexToAddress("0x00000000000000000000000000000000000000d1")
	h.mintTo(account, tokens(100))

	const depositID = "0x0000000000000000000000000000000000000000000000000000000000000041"
	alice.mustRun("deposit", depositID, account.Hex(), tokens(100).String())
	h.settleAndFinalise()

	const withdrawalID = "0x0000000000000000000000000000000000000000000000000000000000000042"
	alice.mustRun("withdraw", withdrawalID, recipient.Hex(), tokens(25).String())
	h.settleAndFinalise()

	if got := alice.status(withdrawalID); got != "confirmed" {
		t.Fatalf("status = %s, want confirmed", got)
	}
	if got := h.railBalance(account); got.Cmp(tokens(75)) != 0 {
		t.Fatalf("balance = %s, want 75", got)
	}
	if got := h.heldByRail(); got.Cmp(tokens(75)) != 0 {
		t.Fatalf("held = %s, want 75", got)
	}
	if got := h.tokenBalance(recipient); got.Cmp(tokens(25)) != 0 {
		t.Fatalf("the recipient received %s, want 25", got)
	}

	// A withdrawal that cannot execute stays pending, not failed: a host must
	// not release a reserve merely because an attempt did not land.
	const stuckID = "0x0000000000000000000000000000000000000000000000000000000000000043"
	h.shim.dropOperations(true)
	alice.mustRun("withdraw", stuckID, recipient.Hex(), tokens(10).String())
	h.settleAndFinalise()
	if got := alice.status(stuckID); got != "pending" {
		t.Fatalf("an unlanded withdrawal must stay pending, got %s", got)
	}

	// Only abandonment plus the death of every attempt makes it terminal.
	alice.mustRun("abandon", stuckID)
	if got := alice.status(stuckID); got != "pending" {
		t.Fatalf("abandonment alone must not release: status = %s", got)
	}
	h.advanceTime(10 * time.Minute)
	h.finalise()
	if got := alice.status(stuckID); got != "failed" {
		t.Fatalf("after every attempt is dead: status = %s, want failed", got)
	}
	if got := h.railBalance(account); got.Cmp(tokens(75)) != 0 {
		t.Fatalf("a failed withdrawal must move nothing, balance = %s", got)
	}
	if got := h.railEvents(stuckID); got != 0 {
		t.Fatalf("a failed withdrawal must emit nothing, got %d events", got)
	}
	assertSolvent(t, h, account)
}

// 5. retry and conflict: resubmitting moves money once and reports the same
// outcome; the same identifier with different terms fails loudly.
func TestStory5RetryAndConflict(t *testing.T) {
	h := newHarness(t)
	alice := h.operator("alice", railKeyHex)
	account := alice.account()
	h.mintTo(account, tokens(100))

	const id = "0x0000000000000000000000000000000000000000000000000000000000000051"

	// The impatient operator repeats the identical command.
	alice.mustRun("deposit", id, account.Hex(), tokens(60).String())
	alice.mustRun("deposit", id, account.Hex(), tokens(60).String())
	alice.mustRun("deposit", id, account.Hex(), tokens(60).String())
	h.settleAndFinalise()

	if got := alice.status(id); got != "confirmed" {
		t.Fatalf("status = %s, want confirmed", got)
	}
	if got := h.railEvents(id); got != 1 {
		t.Fatalf("money must move once, got %d events", got)
	}
	if got := h.railBalance(account); got.Cmp(tokens(60)) != 0 {
		t.Fatalf("balance = %s, want 60", got)
	}

	// Repeating after confirmation is still safe and still reports the same.
	alice.mustRun("deposit", id, account.Hex(), tokens(60).String())
	h.settleAndFinalise()
	if got := h.railEvents(id); got != 1 {
		t.Fatalf("a post-confirmation repeat must move nothing, got %d events", got)
	}
	if got := alice.status(id); got != "confirmed" {
		t.Fatalf("status = %s, want confirmed", got)
	}

	// The same identifier with different terms is refused before anything is
	// signed, and the operator is told to use a fresh one.
	out, err := alice.run("deposit", id, account.Hex(), tokens(61).String())
	if err == nil {
		t.Fatalf("a conflicting identifier must fail loudly, got %q", out)
	}
	if !strings.Contains(err.Error(), "different terms") {
		t.Fatalf("the failure must name the cause, got %v", err)
	}
	if got := h.railBalance(account); got.Cmp(tokens(60)) != 0 {
		t.Fatalf("a refused conflict must move nothing, balance = %s", got)
	}

	// A fresh identifier for the corrected terms works.
	const retryID = "0x0000000000000000000000000000000000000000000000000000000000000052"
	alice.mustRun("deposit", retryID, account.Hex(), tokens(40).String())
	h.settleAndFinalise()
	if got := h.railBalance(account); got.Cmp(tokens(100)) != 0 {
		t.Fatalf("balance = %s, want 100", got)
	}
	assertSolvent(t, h, account)
}

// 6. crash recovery: killed after a durable step, a restart resumes to
// exactly-once, and a stuck attempt is replaced only on proof it is dead.
func TestStory6CrashRecovery(t *testing.T) {
	h := newHarness(t)
	alice := h.operator("alice", railKeyHex)
	account := alice.account()
	h.mintTo(account, tokens(100))

	// Killed after the write-ahead record: nothing was signed or submitted.
	const afterIntent = "0x0000000000000000000000000000000000000000000000000000000000000061"
	alice.mustRun("-halt-after", "intent", "deposit", afterIntent, account.Hex(), tokens(10).String())
	if got := h.shim.submissions(); got != 0 {
		t.Fatalf("nothing may be submitted yet, got %d", got)
	}
	if got := alice.status(afterIntent); got != "pending" {
		t.Fatalf("status = %s, want pending", got)
	}
	// A restart carries on from the record and completes it.
	alice.mustRun("deposit", afterIntent, account.Hex(), tokens(10).String())
	h.settleAndFinalise()
	if got := alice.status(afterIntent); got != "confirmed" {
		t.Fatalf("after restart: status = %s, want confirmed", got)
	}
	if got := h.railEvents(afterIntent); got != 1 {
		t.Fatalf("exactly one execution, got %d", got)
	}

	// Killed after signing, before submission: the attempt survives the crash
	// and is re-presented rather than signed again.
	const afterSign = "0x0000000000000000000000000000000000000000000000000000000000000062"
	before := h.shim.submissions()
	alice.mustRun("-halt-after", "sign", "deposit", afterSign, account.Hex(), tokens(20).String())
	if got := h.shim.submissions(); got != before {
		t.Fatalf("signing must not submit, got %d submissions", got-before)
	}
	alice.mustRun("deposit", afterSign, account.Hex(), tokens(20).String())
	h.settleAndFinalise()
	if got := alice.status(afterSign); got != "confirmed" {
		t.Fatalf("after restart: status = %s, want confirmed", got)
	}
	if got := h.railEvents(afterSign); got != 1 {
		t.Fatalf("exactly one execution, got %d", got)
	}

	// Killed after submission, with the operation never landing: a restart
	// must re-present the same attempt while it may still execute, and sign a
	// fresh one only once expiry at finality proves it dead.
	const afterSubmit = "0x0000000000000000000000000000000000000000000000000000000000000063"
	h.shim.dropOperations(true)
	alice.mustRun("-halt-after", "submit", "deposit", afterSubmit, account.Hex(), tokens(30).String())
	h.settleAndFinalise()

	alice.mustRun("deposit", afterSubmit, account.Hex(), tokens(30).String())
	if got := alice.status(afterSubmit); got != "pending" {
		t.Fatalf("a dropped attempt leaves the intent pending, got %s", got)
	}

	// The sponsorship expires and the chain finalizes past it; only now may a
	// replacement be signed, and this time the bundler lets it through.
	h.advanceTime(10 * time.Minute)
	h.finalise()
	h.shim.dropOperations(false)
	alice.mustRun("deposit", afterSubmit, account.Hex(), tokens(30).String())
	h.settleAndFinalise()

	if got := alice.status(afterSubmit); got != "confirmed" {
		t.Fatalf("after recovery: status = %s, want confirmed", got)
	}
	if got := h.railEvents(afterSubmit); got != 1 {
		t.Fatalf("recovery must still execute exactly once, got %d events", got)
	}

	// Everything reconciles: three deposits, one execution each.
	if got := h.railBalance(account); got.Cmp(tokens(60)) != 0 {
		t.Fatalf("balance = %s, want 60", got)
	}
	if got := h.heldByRail(); got.Cmp(tokens(60)) != 0 {
		t.Fatalf("held = %s, want 60", got)
	}
	if got, want := h.tokenBalance(account), tokens(40); got.Cmp(want) != 0 {
		t.Fatalf("remaining tokens = %s, want %s", got, want)
	}
	assertSolvent(t, h, account)
}

// Solvency is the invariant a host relies on, so every story ends with the
// contract holding at least what it owes.
func assertSolvent(t *testing.T, h *harness, accounts ...common.Address) {
	t.Helper()
	owed := new(big.Int)
	for _, a := range accounts {
		owed.Add(owed, h.railBalance(a))
	}
	if h.heldByRail().Cmp(owed) < 0 {
		t.Fatalf("held %s is less than owed %s", h.heldByRail(), owed)
	}
}
