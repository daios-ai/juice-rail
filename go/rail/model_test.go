package rail

import (
	"fmt"
	"testing"
)

// The small model of §14 target 2: an intent, the variants signed to carry it,
// and a chain that executes at most one of them.
//
// The model is exhaustive rather than sampled, and it calls the library's own
// decision procedure — classify and variantDead — so the properties are proved
// about the shipped logic, not about a copy of it.
//
// What it proves:
//
//	at-most-once  no reachable state executes two variants
//	soundness     a failed intent can never execute, now or later
//	absorption    confirmed and failed never change back
//	liveness      no intent parks forever: from any state, letting time pass
//	              after abandonment reaches a terminal status
const (
	maxVariants = 3
	maxTime     = 4
)

type state struct {
	prepared  bool
	abandoned bool
	// deadlines of the signed variants, in signing order.
	deadlines [maxVariants]uint8
	count     uint8
	// binding is the contract's write-once record: 0 unbound, i for the i-th
	// variant (1-based), -1 for terms this installation never signed — the
	// same account binding the identifier from somewhere else.
	binding int8
	boundAt uint8
	now     uint8
	// finalized is the time the finalized head has reached; it trails now.
	finalized uint8
}

type step struct {
	name  string
	state state
}

// successors enumerates every transition the world can take.
func (s state) successors() []step {
	var out []step
	add := func(name string, next state) { out = append(out, step{name, next}) }

	if !s.prepared {
		next := s
		next.prepared = true
		add("prepare", next)
		// Nothing else can happen before the write-ahead record exists: the
		// rail signs nothing it has not recorded.
		return out
	}

	// Signing: only while the intent is live, and only a variant that could
	// still execute. Replacing a relayer needs no waiting, so this is enabled
	// whenever a fresh deadline is available.
	if !s.abandoned && s.count < maxVariants {
		for d := s.now + 1; d <= maxTime; d++ {
			next := s
			next.deadlines[next.count] = d
			next.count++
			add(fmt.Sprintf("sign(deadline=%d)", d), next)
		}
	}
	if !s.abandoned {
		next := s
		next.abandoned = true
		add("abandon", next)
	}

	// Relaying: the contract binds the identifier to the first variant that
	// arrives in time. A replay of the bound variant changes nothing; anything
	// else reverts, which is also no change.
	for i := uint8(0); i < s.count; i++ {
		if s.binding == 0 && s.now < s.deadlines[i] {
			next := s
			next.binding = int8(i + 1)
			next.boundAt = s.now
			add(fmt.Sprintf("relay(variant=%d)", i+1), next)
		}
	}
	// The same account binding this identifier to other terms elsewhere.
	if s.binding == 0 {
		next := s
		next.binding = -1
		next.boundAt = s.now
		add("bind(other terms)", next)
	}

	if s.now < maxTime {
		next := s
		next.now++
		add("tick", next)
	}
	if s.finalized < s.now {
		next := s
		next.finalized = s.now
		add("finalize", next)
	}
	return out
}

// status is what the library reports, computed from what this state makes
// observable. A binding is only a fact once it is finalized; a variant is only
// dead once its deadline is finalized past.
func (s state) status() Status {
	var fact *Fact
	if s.binding != 0 && s.finalized >= s.boundAt {
		fact = &Fact{Executed: s.binding > 0}
	}
	everyDead := true
	for i := uint8(0); i < s.count; i++ {
		if !variantDead(Variant{ValidBefore: uint64(s.deadlines[i])}, uint64(s.finalized)) {
			everyDead = false
		}
	}
	return classify(s.prepared, fact, s.abandoned, everyDead)
}

// executed reports whether one of our own variants moved money.
func (s state) executed() bool { return s.binding > 0 }

// reachable enumerates the whole state space from the empty state.
func reachable(t *testing.T) map[state]bool {
	t.Helper()
	start := state{}
	seen := map[state]bool{start: true}
	queue := []state{start}
	for len(queue) > 0 {
		s := queue[0]
		queue = queue[1:]
		for _, next := range s.successors() {
			// At-most-once, checked on the transition that would break it:
			// nothing can execute a variant once the identifier is bound.
			if next.state.executed() && s.executed() && next.state.binding != s.binding {
				t.Fatalf("two variants executed: %+v -%s-> %+v", s, next.name, next.state)
			}
			if !seen[next.state] {
				seen[next.state] = true
				queue = append(queue, next.state)
			}
		}
	}
	return seen
}

func TestModelExecutesAtMostOnce(t *testing.T) {
	states := reachable(t)
	t.Logf("%d reachable states", len(states))
	if len(states) < 1000 {
		t.Fatalf("the model collapsed to %d states: it is not exercising anything", len(states))
	}
}

// A failed intent can never execute: nothing it can still do moves money, and
// everything it can do leaves it failed. With those two, failure is terminal
// by induction over any run.
func TestModelFailureIsTerminal(t *testing.T) {
	for s := range reachable(t) {
		if s.status() != StatusFailed {
			continue
		}
		for _, next := range s.successors() {
			if next.state.executed() && !s.executed() {
				t.Fatalf("a failed intent executed: %+v -%s-> %+v", s, next.name, next.state)
			}
			if got := next.state.status(); got != StatusFailed {
				t.Fatalf("failed became %s: %+v -%s-> %+v", got, s, next.name, next.state)
			}
		}
	}
}

// A confirmed intent stays confirmed: finalized facts cannot change, which is
// what lets a host alter its ledger on the strength of one.
func TestModelConfirmationIsFinal(t *testing.T) {
	for s := range reachable(t) {
		if s.status() != StatusConfirmed {
			continue
		}
		if !s.executed() {
			t.Fatalf("confirmed without an execution: %+v", s)
		}
		for _, next := range s.successors() {
			if got := next.state.status(); got != StatusConfirmed {
				t.Fatalf("confirmed became %s: %+v -%s-> %+v", got, s, next.name, next.state)
			}
		}
	}
}

// No intent parks forever. From any state, abandoning and letting time pass is
// enough to reach a terminal status, so a host waiting on one is never stuck.
func TestModelNoIntentParksForever(t *testing.T) {
	for s := range reachable(t) {
		if !s.prepared {
			continue // an unrecorded identifier is nobody's business
		}
		end := s
		end.abandoned = true
		end.now = maxTime
		end.finalized = maxTime
		switch got := end.status(); got {
		case StatusConfirmed, StatusFailed:
		default:
			t.Fatalf("from %+v, letting time pass gives %s, not a terminal status", s, got)
		}
	}
}

// Pending is never a lie: an intent reported pending has something that can
// still execute, or something already executed and not yet finalized.
func TestModelPendingMeansSomethingCanStillHappen(t *testing.T) {
	for s := range reachable(t) {
		if s.status() != StatusPending {
			continue
		}
		if s.binding != 0 {
			// Bound but not finalized: the fact is coming.
			if s.finalized >= s.boundAt {
				t.Fatalf("bound and finalized, yet pending: %+v", s)
			}
			continue
		}
		live := !s.abandoned // it may still sign a fresh variant
		for i := uint8(0); i < s.count; i++ {
			if s.now < s.deadlines[i] {
				live = true
			}
		}
		if !live {
			// Every variant is past its deadline in real time and no more can
			// be signed; only finality is still catching up.
			for i := uint8(0); i < s.count; i++ {
				if s.finalized >= s.deadlines[i] {
					continue
				}
				live = true
			}
			if !live && s.count > 0 {
				t.Fatalf("pending with nothing that can happen: %+v", s)
			}
		}
	}
}
