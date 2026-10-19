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
	call := regexp.MustCompile(`\be\.(echo\.Reserve\w*|reserveLabel\w*|reserveComment)\(`)
	bound := regexp.MustCompile(`^\s*(\w+(, \w+)* :?= |return )`)
	// file:substring of the call, for the post-create PR echoes.
	exempt := map[string]string{
		"pr.go":       `"pull_request", "opened"`,
		"prcreate.go": `"pull_request", "opened"`,
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "echo_registry.go" {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			if !call.MatchString(line) || strings.HasPrefix(strings.TrimSpace(line), "//") || strings.Contains(line, "func (e *Engine)") {
				continue
			}
			if bound.MatchString(line) {
				continue
			}
			if sub, ok := exempt[f]; ok && strings.Contains(line, sub) {
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
