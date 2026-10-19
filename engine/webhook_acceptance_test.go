package engine

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

// intakeEngine wires an engine the way setUpIngestion does for an attached
// transport: engine-level echo registry wired into the cache once, plus an intake
// applying through the cache. It returns the changes the Store emitted.
func intakeEngine(t *testing.T) (*Engine, *intake, *[]itemstate.Change, *sync.Mutex) {
	t.Helper()
	eng, cache := testEngineWithCache(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	eng.echo = newEchoRegistry()
	cache.SetMatchEchoFn(eng.echo.MatchEcho)
	cache.SetMatchEchoReportedFn(eng.echo.MatchEchoReported)
	cache.SetMatchEchoViaFn(eng.echo.MatchEchoVia)

	var mu sync.Mutex
	var got []itemstate.Change
	cache.Subscribe(itemstate.ObserverFunc(func(ch itemstate.Change, _ itemstate.Snapshot) {
		mu.Lock()
		got = append(got, ch)
		mu.Unlock()
	}))
	in := newIntake(newDeliveryLedger("", time.Now(), nil), func(et string, p []byte) { cache.ApplyDelta(et, p) }, nil, time.Now)
	eng.intake = in
	return eng, in, &got, &mu
}

const labeledPayload = `{"action":"labeled","repository":{"full_name":"owner/repo"},"issue":{"number":1},"label":{"name":"fabrik:paused"},"sender":{"login":"bot[bot]"}}`

// FR-008: echo matching works for a Hookdeck delivery, which used to be a no-op.
func TestIntake_HookdeckDeliveryEchoIsAttributedToTheEngine(t *testing.T) {
	eng, in, got, mu := intakeEngine(t)

	// The engine reserves before it writes ...
	eng.reserveLabelAdd(gh.ProjectItem{Number: 1, Repo: "owner/repo"}, "fabrik:paused", "")
	// ... and the echo arrives over Hookdeck.
	in.Submit(delivery{ID: "hd-echo", EventType: "issues", Payload: []byte(labeledPayload), Transport: "hookdeck"})
	in.Drain()

	mu.Lock()
	defer mu.Unlock()
	var found *itemstate.Change
	for i := range *got {
		if len((*got)[i].LabelDeltas) > 0 {
			found = &(*got)[i]
		}
	}
	if found == nil || !found.EchoOfEngine {
		t.Fatalf("hookdeck-delivered echo was not attributed to the engine: %+v", found)
	}
	if eng.echo.len() != 0 {
		t.Fatal("the matched echo was left pending")
	}
}

// A human applying the same label later is still treated as human: the engine's
// own echo was consumed, and a failed write's reservation was cancelled.
func TestIntake_IdenticalLaterHumanEventIsStillHuman(t *testing.T) {
	eng, in, got, mu := intakeEngine(t)

	// A write that failed: reserved, then cancelled.
	eng.reserveLabelAdd(gh.ProjectItem{Number: 1, Repo: "owner/repo"}, "fabrik:paused", "").Cancel()

	in.Submit(delivery{ID: "human-1", EventType: "issues", Payload: []byte(labeledPayload), Transport: "webhook"})
	in.Drain()

	mu.Lock()
	defer mu.Unlock()
	for _, ch := range *got {
		if len(ch.LabelDeltas) > 0 && ch.EchoOfEngine {
			t.Fatal("a failed write's orphan swallowed a later identical human event")
		}
	}
}

// Two deliveries for one item reach the Store in arrival order through the real
// cache apply path.
func TestIntake_TwoDeliveriesForOneItemApplyInOrder(t *testing.T) {
	_, in, got, mu := intakeEngine(t)
	mk := func(action, label string) []byte {
		return []byte(`{"action":"` + action + `","repository":{"full_name":"owner/repo"},"issue":{"number":1},"label":{"name":"` + label + `"},"sender":{"login":"alice"}}`)
	}
	in.Submit(delivery{ID: "1", EventType: "issues", Payload: mk("labeled", "first-label")})
	in.Submit(delivery{ID: "2", EventType: "issues", Payload: mk("unlabeled", "first-label")})
	in.Submit(delivery{ID: "3", EventType: "issues", Payload: mk("labeled", "second-label")})
	in.Drain()

	mu.Lock()
	defer mu.Unlock()
	var seq []string
	for _, ch := range *got {
		for _, d := range ch.LabelDeltas {
			seq = append(seq, d.Label)
		}
	}
	if len(seq) != 3 || seq[0] != "first-label" || seq[1] != "first-label" || seq[2] != "second-label" {
		t.Fatalf("label deltas reached the Store as %v, want [first-label first-label second-label] in arrival order", seq)
	}
}

// Echo reservations must precede the write they describe. Every Reserve*/reserve*
// call in the engine's production sources has its result bound (so the failure
// path can Cancel it) — except the two PR-creation sites whose echo key carries a
// number known only after the create call returns.
func TestEchoReservationsAreBoundForCancel(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	call := regexp.MustCompile(`\b\w+\.(echo\.Reserve\w*|reserveLabel\w*|reserveComment)\(`)
	bound := regexp.MustCompile(`^\s*(\w+(, \w+)* :?= |return )`)
	// A reservation assigned to variables (the reservation is the first one, as in
	// `res, x := ...`) must reach that variable's Cancel() on the failure path (a
	// helper that returns the reservation, or the post-create PR echoes, are
	// exempt).
	boundVar := regexp.MustCompile(`^\s*(\w+)(?:, \w+)* :?= \w+\.(echo\.Reserve|reserveLabel|reserveComment)`)
	// file:substring of the call, for the post-create PR echoes; each file may
	// have exactly one such site, so a second unbound reservation there is caught.
	exempt := map[string]string{
		"pr.go":       `"pull_request", "opened"`,
		"prcreate.go": `"pull_request", "opened"`,
	}
	exemptUsed := map[string]int{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "echo_registry.go" {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(string(data), "\n")
		for i, line := range lines {
			if !call.MatchString(line) || strings.HasPrefix(strings.TrimSpace(line), "//") || strings.Contains(line, "func (e *Engine)") {
				continue
			}
			if m := boundVar.FindStringSubmatch(line); m != nil {
				if !cancelsBefore(lines, i, m[1]) {
					t.Errorf("%s:%d: %s is reserved here but never Cancel()led before the next reservation or the end of the function — a failed write would leave an orphan echo: %s", f, i+1, m[1], strings.TrimSpace(line))
				}
				continue
			}
			if bound.MatchString(line) {
				continue
			}
			if sub, ok := exempt[f]; ok && strings.Contains(line, sub) && exemptUsed[f] == 0 {
				exemptUsed[f]++
				continue
			}
			t.Errorf("%s:%d: echo reservation result is not bound, so a failed write cannot cancel it: %s", f, i+1, strings.TrimSpace(line))
		}
	}
}

// No engine source may bypass the registry: the pre-#2351 per-transport echo API
// and the single-manager field are gone.
func TestNoPerTransportEchoOrSingleManagerLeft(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	banned := []string{"RegisterEcho(", "RegisterEchoVia(", "RegisterEchoIfSubscribed(", "e.webhookMgr", "eventIngestionManager", "pendingEchoes"}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		data, _ := os.ReadFile(f)
		for _, b := range banned {
			for i, line := range strings.Split(string(data), "\n") {
				if strings.Contains(line, b) && !strings.HasPrefix(strings.TrimSpace(line), "//") {
					t.Errorf("%s:%d still references %q", f, i+1, b)
				}
			}
		}
	}
}

// cancelsBefore reports whether lines after index i call v.Cancel() before the
// variable is reserved again or the enclosing function ends.
func cancelsBefore(lines []string, i int, v string) bool {
	reassign := regexp.MustCompile(`^\s*` + regexp.QuoteMeta(v) + ` :?= e\.(echo\.Reserve|reserveLabel|reserveComment)`)
	for _, l := range lines[i+1:] {
		if strings.HasPrefix(l, "func ") {
			return false
		}
		if strings.Contains(l, v+".Cancel()") {
			return true
		}
		if reassign.MatchString(l) {
			return false
		}
	}
	return false
}

// A delivery that carries no GitHub time is ordered by when the intake RECEIVED
// it (#2351, amending #2348): one queued behind a slow apply loses to an engine
// write that landed while it waited, instead of being stamped at apply time and
// overwriting that fresher state.
func TestIntake_QueuedDeliveryLosesToALaterEngineWrite(t *testing.T) {
	const labelKey = "owner/repo#1"
	run := func(t *testing.T, useReceiptTime bool) (labelSurvived bool) {
		eng, cache := testEngineWithCache(t, &mockGitHubClient{}, &mockClaudeInvoker{})
		// Deliveries are received "a minute ago" by the intake's clock, so they are
		// older than the engine write below however fast the test runs.
		in := newIntake(newDeliveryLedger("", time.Now(), nil), nil, nil, time.Now)
		in.receivedNow = func() time.Time { return time.Now().Add(-time.Minute) }
		eng.intake = in

		gate := make(chan struct{})
		var first sync.Once
		wait := func() { first.Do(func() { <-gate }) }
		if useReceiptTime {
			in.applyAt = func(et string, p []byte, rcv time.Time) { wait(); cache.ApplyDeltaAt(et, p, rcv) }
		} else {
			in.apply = func(et string, p []byte) { wait(); cache.ApplyDelta(et, p) }
		}
		mk := func(action, label string) []byte {
			return []byte(`{"action":"` + action + `","repository":{"full_name":"owner/repo"},"issue":{"number":1},"label":{"name":"` + label + `"},"sender":{"login":"alice"}}`)
		}
		// D1 holds the item's queue; D2 (unlabeled x) waits behind it.
		in.Submit(delivery{ID: "d1", EventType: "issues", Payload: mk("labeled", "other")})
		in.Submit(delivery{ID: "d2", EventType: "issues", Payload: mk("unlabeled", "x")})

		// The engine adds x while D2 is still queued, then the queue moves on.
		cache.ApplyLabelAdded(labelKey, "x")
		close(gate)
		in.Drain()

		snap, ok := eng.store.Peek("owner/repo", 1)
		if !ok {
			t.Fatal("item missing from the store")
		}
		for _, l := range snap.State().Labels {
			if l == "x" {
				return true
			}
		}
		return false
	}

	if !run(t, true) {
		t.Fatal("a delivery queued behind a slow apply, received before an engine write, removed the label that write added")
	}
	// Non-vacuity: stamped at apply time the same delivery wins.
	if run(t, false) {
		t.Fatal("non-vacuity: an apply-time stamped delivery should have removed the label")
	}
}
