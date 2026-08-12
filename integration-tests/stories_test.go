//go:build integration

package integration

import (
	"math/big"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// The seven user stories. Each runs against the compiled binary on its own local
// chain, and ends on the property the story is about.

// 1. onboard: a new account is created, funded once with money and gas, and is
// then operational. That single funding step is the only time a participant
// handles the native currency by hand.
func TestStory1Onboard(t *testing.T) {
	h := newHarness(t)
	home := t.TempDir()

	cmd := exec.Command(railctlBinary, "init", "alice", h.configPath)
	cmd.Env = append(os.Environ(), "HOME="+home)
	var checklist strings.Builder
	cmd.Stderr = &checklist
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("init: %v: %s", err, checklist.String())
	}
	if !strings.Contains(string(out), "profile alice on domain local") {
		t.Fatalf("init said %q", out)
	}
	// Onboarding is a checklist, and it says both things that must be sent.
	for _, want := range []string{"send the stablecoin", "native currency", "wait for finality"} {
		if !strings.Contains(checklist.String(), want) {
			t.Fatalf("the funding checklist does not mention %q:\n%s", want, checklist.String())
		}
	}

	alice := h.participant("alice", aliceKeyHex)
	account := alice.account()
	// A fresh account holds nothing.
	if h.tokenBalance(account).Sign() != 0 || h.reserve(account).Sign() != 0 {
		t.Fatal("a new account was not empty")
	}

	h.mintTo(account, tokens(1000))
	h.fund(account, wei(reserveMax))
	h.settleAndFinalise()

	balance := alice.mustRun("balance")
	if !strings.Contains(balance, "balance 1000.00") || !strings.Contains(balance, "reserve 0.05") {
		t.Fatalf("after funding, balance reads %q", balance)
	}

	// Operational means it can pay.
	id := freshID(t)
	got := alice.pay("transfer", id, addressOf(t, bobKeyHex), "1.00")
	if !strings.Contains(got.Note, "submitted") {
		t.Fatalf("a funded account could not pay: %+v", got)
	}
	h.settleAndFinalise()
	if status := alice.status(id); status != "confirmed" {
		t.Fatalf("the first payment is %s, want confirmed", status)
	}
}

// 2. deposit: money arrives with no transaction from this account at all, and
// each finalized transfer is recognised exactly once.
func TestStory2Deposit(t *testing.T) {
	h := newHarness(t)
	alice := h.participant("alice", aliceKeyHex)
	account := alice.account()

	payer := addressOf(t, payerKeyHex)
	h.mintTo(payer, tokens(500))
	h.fund(payer, ether(1))
	h.settleAndFinalise()

	// Two payments in from outside: an exchange and a wallet, say.
	h.transferFrom(payerKeyHex, account, tokens(120))
	h.transferFrom(payerKeyHex, account, tokens(30))
	h.mine(1)

	// Inclusion is not confirmation.
	if got := alice.deposits(); len(got) != 0 {
		t.Fatalf("%d deposits before finality", len(got))
	}
	h.finalise()

	got := alice.deposits()
	if len(got) != 2 {
		t.Fatalf("%d deposits after finality, want 2", len(got))
	}
	if got[0].Amount != "120.00" || got[1].Amount != "30.00" {
		t.Fatalf("deposits read %+v", got)
	}
	if got[0].Tx == got[1].Tx && got[0].LogIndex == got[1].LogIndex {
		t.Fatal("two deposits share one identity")
	}
	// Asking again finds nothing new: the log that carried it is its identity.
	if again := alice.deposits(); len(again) != 2 {
		t.Fatalf("rescanning turned 2 deposits into %d", len(again))
	}
	if h.tokenBalance(account).Cmp(tokens(150)) != 0 {
		t.Fatalf("the account holds %s", h.tokenBalance(account))
	}
	// The account never sent anything to receive its money.
	if h.nonce(account) != 0 {
		t.Fatal("receiving money cost the account a transaction")
	}
}

// 3. transfer: A pays B. B needs nothing at all, not even gas, and the money
// moves exactly once however many times the command is repeated.
func TestStory3Transfer(t *testing.T) {
	h := newHarness(t)
	alice, bob := h.participant("alice", aliceKeyHex), h.participant("bob", bobKeyHex)
	from, to := alice.account(), bob.account()

	h.mintTo(from, tokens(1000))
	h.fund(from, wei(reserveMax))
	h.settleAndFinalise()

	id := freshID(t)
	alice.pay("transfer", id, to, "250.00")
	h.settleAndFinalise()

	if status := alice.status(id); status != "confirmed" {
		t.Fatalf("payment is %s, want confirmed", status)
	}
	if h.tokenBalance(to).Cmp(tokens(250)) != 0 {
		t.Fatalf("the recipient holds %s", h.tokenBalance(to))
	}
	// The recipient holds no native currency and never needed any.
	if h.reserve(to).Sign() != 0 {
		t.Fatalf("the recipient holds %s wei to receive money", h.reserve(to))
	}
	if h.nonce(to) != 0 {
		t.Fatal("receiving cost the recipient a transaction")
	}

	// Repeating the command is a replay, not a second payment.
	alice.pay("transfer", id, to, "250.00")
	h.settleAndFinalise()
	if n := h.transfers(from, to); n != 1 {
		t.Fatalf("the money moved %d times", n)
	}
	// The same identifier may not mean something else.
	refusal := alice.mustFail("transfer", id, to.Hex(), "1.00")
	if !strings.Contains(refusal, "already recorded") {
		t.Fatalf("reusing an identifier was refused with %q", refusal)
	}
	// And the recipient can spend what it received, once it has gas of its own.
	h.fund(to, wei(reserveMax))
	h.settleAndFinalise()
	back := freshID(t)
	bob.pay("transfer", back, from, "10.00")
	h.settleAndFinalise()
	if bob.status(back) != "confirmed" {
		t.Fatal("the recipient could not spend what it received")
	}
}

// 4. withdraw: the same primitive, sent outside the rail. The destination is
// supplied per operation; nothing about it is stored.
func TestStory4Withdraw(t *testing.T) {
	h := newHarness(t)
	alice := h.participant("alice", aliceKeyHex)
	account := alice.account()
	exchange := common.HexToAddress("0x00000000000000000000000000000000000ec4a5")

	h.mintTo(account, tokens(1000))
	h.fund(account, wei(reserveMax))
	h.settleAndFinalise()

	id := freshID(t)
	alice.pay("withdraw", id, exchange, "300.00")
	h.settleAndFinalise()

	if status := alice.status(id); status != "confirmed" {
		t.Fatalf("withdrawal is %s, want confirmed", status)
	}
	if h.tokenBalance(exchange).Cmp(tokens(300)) != 0 {
		t.Fatalf("the destination received %s", h.tokenBalance(exchange))
	}
	if h.tokenBalance(account).Cmp(tokens(700)) != 0 {
		t.Fatalf("the account kept %s", h.tokenBalance(account))
	}
	if n := h.transfers(account, exchange); n != 1 {
		t.Fatalf("the withdrawal moved %d times", n)
	}
}

// 5. refill: a reserve too low for the next payment is topped up from the
// account's own money, and a shortage blocks loudly with nothing signed.
func TestStory5Refill(t *testing.T) {
	h := newHarness(t)
	alice := h.participant("alice", aliceKeyHex)
	account := alice.account()
	to := addressOf(t, bobKeyHex)

	h.mintTo(account, tokens(1000))
	// Enough gas to buy gas, not enough to pay from.
	h.setReserve(account, wei("15000000000000000")) // 0.015
	h.settleAndFinalise()

	before := h.tokenBalance(account)
	id := freshID(t)

	// The payment does not go through; the account buys gas instead, and says
	// so. Nothing about the payment was signed.
	first := alice.pay("transfer", id, to, "25.00")
	if first.Refill == "" || first.Tx == "" {
		t.Fatalf("a low reserve did not produce a refill: %+v", first)
	}
	if alice.status(id) != "unknown" {
		t.Fatal("a payment that never ran left a record behind")
	}
	h.settleAndFinalise()

	reserve := h.reserve(account)
	if reserve.Cmp(wei(reserveMin)) <= 0 {
		t.Fatalf("after refilling the reserve is %s, still under the minimum %s", reserve, reserveMin)
	}
	// It bought its gas with its own money, and no more than the quote plus its
	// slippage margin. The whole reserve range at the venue's price is the
	// loosest bound that still means something.
	spent := new(big.Int).Sub(before, h.tokenBalance(account))
	worstCase := big.NewInt(0).Div(new(big.Int).Mul(wei(reserveMax), big.NewInt(venuePrice)), ether(1))
	worstCase.Mul(worstCase, big.NewInt(10_000+slippageBps))
	worstCase.Div(worstCase, big.NewInt(10_000))
	if spent.Sign() <= 0 || spent.Cmp(worstCase) > 0 {
		t.Fatalf("the refill spent %s, outside (0, %s]", spent, worstCase)
	}
	// What the venue may still take is bounded by that same input bound.
	if left := h.allowance(account); left.Cmp(worstCase) > 0 {
		t.Fatalf("the venue may still take %s, more than one refill's input bound %s", left, worstCase)
	}

	// Now the payment goes through.
	second := alice.pay("transfer", id, to, "25.00")
	if second.Refill != "" {
		t.Fatalf("the payment asked for a second refill: %+v", second)
	}
	h.settleAndFinalise()
	if status := alice.status(id); status != "confirmed" {
		t.Fatalf("the payment after a refill is %s, want confirmed", status)
	}
	if h.reserve(account).Cmp(wei(reserveMin)) < 0 {
		t.Fatalf("after paying the reserve is %s, under the minimum", h.reserve(account))
	}

	// A second account with money enough for the payment but not for the
	// payment and the gas it would need: blocked, loudly, with nothing signed.
	poor := h.participant("poor", payerKeyHex)
	poorAccount := poor.account()
	h.mintTo(poorAccount, tokens(100))
	h.setReserve(poorAccount, wei("15000000000000000"))
	h.settleAndFinalise()

	refusal := poor.mustFail("transfer", freshID(t), to.Hex(), "5.00")
	if !strings.Contains(refusal, "stablecoin too low") {
		t.Fatalf("a shortage was reported as %q", refusal)
	}
	if h.nonce(poorAccount) != 0 {
		t.Fatal("a blocked payment sent a transaction")
	}
}

// 6. recovery: a process killed at any durable step resumes to exactly once,
// and a stuck payment is retried under the same nonce and the same terms.
func TestStory6Recovery(t *testing.T) {
	h := newHarness(t)
	alice := h.participant("alice", aliceKeyHex)
	account := alice.account()
	to := addressOf(t, bobKeyHex)

	h.mintTo(account, tokens(1000))
	h.fund(account, wei(reserveMax))
	h.settleAndFinalise()

	// Killed after the write-ahead record and before anything was signed.
	first := freshID(t)
	alice.mustRun("-halt-after", "intent", "transfer", first, to.Hex(), "40.00")
	if h.nonce(account) != 0 {
		t.Fatal("an intent that was only recorded still sent something")
	}
	if alice.status(first) != "pending" {
		t.Fatal("a recorded intent is not pending")
	}
	// A fresh process finishes it, on the nonce the record already owns.
	alice.mustRun("retry", first)
	h.settleAndFinalise()
	if status := alice.status(first); status != "confirmed" {
		t.Fatalf("the resumed payment is %s, want confirmed", status)
	}
	if n := h.transfers(account, to); n != 1 {
		t.Fatalf("the resumed payment moved money %d times", n)
	}

	// Killed after broadcasting, before anything finalized.
	second := freshID(t)
	alice.mustRun("-halt-after", "submit", "transfer", second, to.Hex(), "60.00")
	h.mine(1)
	// A fresh process retries what it does not yet know the outcome of. The
	// nonce is the same, so at most one of the two can ever execute.
	alice.mustRun("retry", second)
	h.settleAndFinalise()

	if status := alice.status(second); status != "confirmed" {
		t.Fatalf("after a crash and a retry the payment is %s, want confirmed", status)
	}
	if n := h.transfers(account, to); n != 2 {
		t.Fatalf("two payments moved money %d times", n)
	}
	if h.tokenBalance(to).Cmp(tokens(100)) != 0 {
		t.Fatalf("the recipient holds %s, want 100", h.tokenBalance(to))
	}
	// Repeating the whole command changes nothing.
	alice.pay("transfer", second, to, "60.00")
	h.settleAndFinalise()
	if n := h.transfers(account, to); n != 2 {
		t.Fatalf("a repeated command moved money %d times", n)
	}
}

// Domains are independent: records made on one chain are refused on another.
func TestDomainsAreIndependent(t *testing.T) {
	first := newHarness(t)
	alice := first.participant("alice", aliceKeyHex)
	account := alice.account()
	first.mintTo(account, tokens(100))
	first.fund(account, wei(reserveMax))
	first.settleAndFinalise()

	id := freshID(t)
	alice.pay("transfer", id, addressOf(t, bobKeyHex), "10.00")
	first.settleAndFinalise()

	second := newHarnessOn(t, defaultChainID+1)
	crossed := &participant{h: second, name: "alice", key: aliceKeyHex, store: alice.store}
	refusal := crossed.mustFail("status", id)
	if !strings.Contains(refusal, "different domain") {
		t.Fatalf("using one domain's records on another was refused with %q", refusal)
	}
}

// 7. leave: a participant sends the whole balance out in one transfer, without
// buying gas on the way.
func TestStory7WithdrawAll(t *testing.T) {
	h := newHarness(t)
	alice := h.participant("alice", aliceKeyHex)
	account := alice.account()
	exchange := common.HexToAddress("0x00000000000000000000000000000000000ec4a5")

	h.mintTo(account, tokens(500))
	h.fund(account, wei(reserveMax))
	h.settleAndFinalise()

	alice.pay("transfer", freshID(t), addressOf(t, bobKeyHex), "5.00")
	h.settleAndFinalise()

	whole := h.tokenBalance(account)
	id := freshID(t)
	out := alice.pay("withdraw", id, exchange, "all")
	if out.Refill != "" {
		t.Fatalf("leaving bought gas on the way out: %+v", out)
	}
	h.settleAndFinalise()

	if status := alice.status(id); status != "confirmed" {
		t.Fatalf("leaving is %s, want confirmed", status)
	}
	if h.tokenBalance(exchange).Cmp(whole) != 0 {
		t.Fatalf("the destination received %s, want %s", h.tokenBalance(exchange), whole)
	}
	if h.tokenBalance(account).Sign() != 0 {
		t.Fatalf("the account kept %s of stablecoin", h.tokenBalance(account))
	}
	if n := h.transfers(account, exchange); n != 1 {
		t.Fatalf("leaving moved money %d times", n)
	}
	// The reserve stays behind. It is reachable with the key, not with a verb.
	if h.reserve(account).Sign() == 0 {
		t.Fatal("the reserve vanished; leaving does not spend it")
	}
}
