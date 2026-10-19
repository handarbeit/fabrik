package engine

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/boardcache"
	gh "github.com/handarbeit/fabrik/github"
)

// aged returns a registry whose clock the test advances.
func agedRegistry() (*echoRegistry, *time.Time) {
	now := time.Now()
	r := newEchoRegistry()
	r.nowFn = func() time.Time { return now }
	return r, &now
}

func TestEchoRegistry_ReserveMatchConsumes(t *testing.T) {
	r := newEchoRegistry()
	r.Reserve("issues", "labeled", "owner/repo#1+x")
	if !r.has(echoKey("issues", "labeled", "owner/repo#1+x")) {
		t.Fatal("reservation not recorded")
	}
	if !r.MatchEchoReported("issues", "labeled", "owner/repo#1+x") {
		t.Fatal("MatchEchoReported should match the reservation")
	}
	if r.len() != 0 {
		t.Fatal("a match must consume the entry")
	}
	if r.MatchEchoReported("issues", "labeled", "owner/repo#1+x") {
		t.Fatal("a consumed echo must not match again")
	}
}

func TestEchoRegistry_NilIsInert(t *testing.T) {
	var r *echoRegistry
	res := r.Reserve("issues", "labeled", "k")
	res.Cancel()
	r.ReserveVia("issues", "labeled", "k", "x").Cancel()
	r.ReserveIfSubscribed("projects_v2_item", "edited", "k").Cancel()
	if ok, via := r.MatchEchoVia("issues", "labeled", "k"); ok || via != "" {
		t.Fatalf("nil registry matched: %v %q", ok, via)
	}
	if r.sweep(time.Second) != 0 || r.has("k") || r.len() != 0 {
		t.Fatal("nil registry must be inert")
	}
}

// FR-007: the reservation is cancelled when the write fails, so no orphan can
// swallow a later identical human event.
func TestEchoRegistry_CancelLeavesNothingToConsumeALaterHumanEvent(t *testing.T) {
	r := newEchoRegistry()
	res := r.Reserve("issues", "labeled", "owner/repo#1+fabrik:paused")
	res.Cancel() // the write failed
	if r.len() != 0 {
		t.Fatalf("pending after cancel = %d, want 0", r.len())
	}
	// A human applies the same label later: it must read as external.
	if r.MatchEchoReported("issues", "labeled", "owner/repo#1+fabrik:paused") {
		t.Fatal("a cancelled reservation swallowed a later identical human event")
	}
}

func TestEchoRegistry_CancelIsGenerationScoped(t *testing.T) {
	r := newEchoRegistry()
	first := r.Reserve("issues", "labeled", "k")
	second := r.Reserve("issues", "labeled", "k") // a newer write of the same fact
	first.Cancel()
	if !r.has(echoKey("issues", "labeled", "k")) {
		t.Fatal("cancelling a superseded reservation removed the newer one")
	}
	second.Cancel()
	if r.len() != 0 {
		t.Fatal("cancelling the live reservation must remove it")
	}
	second.Cancel() // idempotent
}

func TestEchoRegistry_CancelAfterMatchIsHarmless(t *testing.T) {
	r := newEchoRegistry()
	res := r.Reserve("issues", "labeled", "k")
	r.MatchEcho("issues", "labeled", "k")
	r.Reserve("issues", "labeled", "k") // a later write, same key
	res.Cancel()
	if !r.has(echoKey("issues", "labeled", "k")) {
		t.Fatal("a stale reservation's Cancel removed a later write's entry")
	}
}

func TestEchoRegistry_ViaSemantics(t *testing.T) {
	t.Run("match returns the requester and consumes the entry", func(t *testing.T) {
		r := newEchoRegistry()
		r.ReserveVia("issues", "labeled", "o/r#1+x", "overseer-a")
		ok, via := r.MatchEchoVia("issues", "labeled", "o/r#1+x")
		if !ok || via != "overseer-a" {
			t.Fatalf("MatchEchoVia = %v, %q", ok, via)
		}
		if r.len() != 0 {
			t.Fatalf("entries left: %d", r.len())
		}
	})
	t.Run("plain reserve has no requester", func(t *testing.T) {
		r := newEchoRegistry()
		r.Reserve("issues", "labeled", "o/r#1+x")
		if ok, via := r.MatchEchoVia("issues", "labeled", "o/r#1+x"); !ok || via != "" {
			t.Fatalf("MatchEchoVia = %v, %q", ok, via)
		}
	})
	t.Run("an autonomous reservation clears a stale requester", func(t *testing.T) {
		r := newEchoRegistry()
		r.ReserveVia("issues", "labeled", "o/r#1+x", "overseer-a")
		r.Reserve("issues", "labeled", "o/r#1+x")
		if ok, via := r.MatchEchoVia("issues", "labeled", "o/r#1+x"); !ok || via != "" {
			t.Fatalf("MatchEchoVia = %v, %q; an autonomous write must not inherit the overseer", ok, via)
		}
	})
	t.Run("ReserveIfSubscribed clears a stale requester", func(t *testing.T) {
		r := newEchoRegistry()
		r.ReserveVia("issues", "labeled", "o/r#1+x", "overseer-a")
		r.ReserveIfSubscribed("issues", "labeled", "o/r#1+x")
		if ok, via := r.MatchEchoVia("issues", "labeled", "o/r#1+x"); !ok || via != "" {
			t.Fatalf("MatchEchoVia = %v, %q", ok, via)
		}
	})
}

func TestEchoRegistry_ReserveIfSubscribedConsultsTransports(t *testing.T) {
	r := newEchoRegistry()
	set := &transportSet{}
	r.setSubscribed(set.SubscribesTo)

	// No transport receives projects_v2_item: nothing is reserved.
	if res := r.ReserveIfSubscribed("projects_v2_item", "edited", "PVTI_1"); res != nil || r.len() != 0 {
		t.Fatal("reserved an echo no transport can deliver")
	}

	// A transport that dropped the event at runtime (gh-forward fallback).
	ghLike := newFakeTransport("gh")
	ghLike.sub = func(ev string) bool { return ev != "projects_v2_item" }
	set.Add(ghLike)
	if r.ReserveIfSubscribed("projects_v2_item", "edited", "PVTI_1") != nil {
		t.Fatal("reserved projects_v2_item though the only transport dropped it")
	}

	// Hookdeck receives everything.
	hm, _ := newTestHookdeckManager(t)
	set.Add(hm)
	if r.ReserveIfSubscribed("projects_v2_item", "edited", "PVTI_1") == nil || r.len() != 1 {
		t.Fatal("did not reserve projects_v2_item though hookdeck receives it")
	}
}

func TestEchoRegistry_SweepExpiresAndReportsMisses(t *testing.T) {
	r, now := agedRegistry()
	var got []time.Time
	r.setMissFn(func(at []time.Time) { got = append(got, at...) })

	r.Reserve("issues", "labeled", "a")
	*now = now.Add(webhookEchoTimeout - time.Second)
	r.Reserve("issues", "labeled", "b")
	*now = now.Add(2 * time.Second) // a is now past the timeout, b is not
	if n := r.sweep(webhookEchoTimeout); n != 1 {
		t.Fatalf("swept %d, want 1", n)
	}
	if len(got) != 1 || r.len() != 1 || !r.has(echoKey("issues", "labeled", "b")) {
		t.Fatalf("missFn saw %d, pending=%d", len(got), r.len())
	}
}

// ADR 042: the miss observer turns the transport Unhealthy at the threshold, and
// ignores misses while the subprocess is starting up.
func TestWebhookManager_NoteEchoMisses(t *testing.T) {
	newMgr := func(t *testing.T, state WebhookHealthState) *webhookManager {
		t.Helper()
		wm, _ := newTestWebhookManager(t)
		wm.mu.Lock()
		wm.state = state
		wm.mu.Unlock()
		return wm
	}
	state := func(wm *webhookManager) WebhookHealthState {
		wm.mu.Lock()
		defer wm.mu.Unlock()
		return wm.state
	}
	now := time.Now()

	t.Run("below the threshold stays healthy and remembers the misses", func(t *testing.T) {
		wm := newMgr(t, WebhookStreamHealthy)
		wm.noteEchoMisses([]time.Time{now})
		if state(wm) != WebhookStreamHealthy {
			t.Fatal("went unhealthy below the threshold")
		}
		wm.mu.Lock()
		n := len(wm.missHistory)
		wm.mu.Unlock()
		if n != 1 {
			t.Fatalf("missHistory = %d, want 1", n)
		}
	})
	t.Run("threshold misses in the window → Unhealthy and history cleared", func(t *testing.T) {
		wm := newMgr(t, WebhookStreamHealthy)
		wm.noteEchoMisses([]time.Time{now, now, now})
		if state(wm) != WebhookStreamUnhealthy {
			t.Fatalf("state = %s, want unhealthy", state(wm))
		}
		wm.mu.Lock()
		n := len(wm.missHistory)
		wm.mu.Unlock()
		if n != 0 {
			t.Fatalf("missHistory = %d after firing, want 0", n)
		}
	})
	t.Run("misses older than the window are pruned", func(t *testing.T) {
		wm := newMgr(t, WebhookStreamHealthy)
		old := now.Add(-(webhookEchoWindow + time.Minute))
		wm.noteEchoMisses([]time.Time{old, old})
		wm.noteEchoMisses([]time.Time{now})
		if state(wm) != WebhookStreamHealthy {
			t.Fatal("stale misses counted toward the threshold")
		}
		wm.mu.Lock()
		n := len(wm.missHistory)
		wm.mu.Unlock()
		if n != 1 {
			t.Fatalf("missHistory = %d, want 1 (old ones pruned)", n)
		}
	})
	t.Run("ignored while the subprocess is starting up", func(t *testing.T) {
		wm := newMgr(t, WebhookStreamStartingUp)
		wm.noteEchoMisses([]time.Time{now, now, now, now})
		if state(wm) != WebhookStreamStartingUp {
			t.Fatalf("state = %s: startup-time misses must not fire", state(wm))
		}
	})
}

// A matched echo is never a miss: the sweep finds nothing to report.
func TestEchoRegistry_MatchedEchoIsNotAMiss(t *testing.T) {
	r, now := agedRegistry()
	missed := 0
	r.setMissFn(func(at []time.Time) { missed += len(at) })
	r.Reserve("issues", "labeled", "a")
	r.MatchEcho("issues", "labeled", "a")
	*now = now.Add(time.Hour)
	if r.sweep(webhookEchoTimeout) != 0 || missed != 0 {
		t.Fatal("a matched echo was reported as a miss")
	}
}

// --- acceptance: echoes are reserved before the write they describe (R3) ----

// An echo that arrives INSIDE the write call (a sub-millisecond direct receiver)
// is matched: the reservation already exists when the write is issued.
func TestEngineWrites_ReserveEchoBeforeTheWrite(t *testing.T) {
	item := gh.ProjectItem{Number: 1, Repo: "owner/repo"}
	labelKey := echoKey("issues", "labeled", boardcache.ItemKey("owner/repo", 1)+"+fabrik:paused")
	unlabelKey := echoKey("issues", "unlabeled", boardcache.ItemKey("owner/repo", 1)+"+fabrik:paused")
	commentKey := echoKey("issue_comment", "created", boardcache.ItemKey("owner/repo", 1))
	bodyKey := echoKey("issues", "edited", boardcache.ItemKey("owner/repo", 1))

	var mu sync.Mutex
	pendingDuringWrite := map[string]bool{}
	var eng *Engine
	note := func(name string, key string) {
		mu.Lock()
		defer mu.Unlock()
		pendingDuringWrite[name] = eng.echo.has(key)
	}
	client := &mockGitHubClient{
		addLabelToIssueFn: func(_, _ string, _ int, _ string) error { note("add", labelKey); return nil },
		removeLabelFromIssueFn: func(_, _ string, _ int, _ string) error {
			note("remove", unlabelKey)
			return nil
		},
		addCommentFn: func(_, _ string, _ int, _ string) (int, error) { note("comment", commentKey); return 7, nil },
		updateIssueBodyFn: func(_, _ string, _ int, _ string) error {
			note("body", bodyKey)
			return nil
		},
	}
	eng, _ = testEngineWithCache(t, client, &mockClaudeInvoker{})
	eng.echo = newEchoRegistry()

	eng.addLabel(item, "fabrik:paused")
	eng.removeLabel(item, "fabrik:paused")
	if _, err := eng.postComment(item, "hi", false, true); err != nil {
		t.Fatal(err)
	}
	if err := eng.updateIssueBody(item, "owner", "repo", "body"); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	for _, name := range []string{"add", "remove", "comment", "body"} {
		if !pendingDuringWrite[name] {
			t.Errorf("%s: no echo was reserved when the write was issued — a fast echo would arrive unregistered", name)
		}
	}
}

// And the echo is really attributable when it arrives mid-write: the matcher the
// cache calls finds the reservation.
func TestEngineWrites_EchoArrivingInsideTheWriteIsMatched(t *testing.T) {
	var eng *Engine
	matchedDuringWrite := false
	client := &mockGitHubClient{
		addLabelToIssueFn: func(_, _ string, _ int, _ string) error {
			// The webhook for this very write arrives before the write call returns.
			matchedDuringWrite = eng.echo.MatchEchoReported("issues", "labeled", boardcache.ItemKey("owner/repo", 1)+"+fabrik:paused")
			return nil
		},
	}
	eng, _ = testEngineWithCache(t, client, &mockClaudeInvoker{})
	eng.echo = newEchoRegistry()
	eng.addLabel(gh.ProjectItem{Number: 1, Repo: "owner/repo"}, "fabrik:paused")
	if !matchedDuringWrite {
		t.Fatal("an echo arriving before the write returned was not matched as the engine's own")
	}
	if eng.echo.len() != 0 {
		t.Fatal("the matched echo was not consumed")
	}
}

// A write that fails leaves no pending echo, for every write kind.
func TestEngineWrites_FailedWriteLeavesNoPendingEcho(t *testing.T) {
	boom := errors.New("boom")
	client := &mockGitHubClient{
		addLabelToIssueFn:      func(_, _ string, _ int, _ string) error { return boom },
		removeLabelFromIssueFn: func(_, _ string, _ int, _ string) error { return boom },
		addCommentFn:           func(_, _ string, _ int, _ string) (int, error) { return 0, boom },
		updateIssueBodyFn:      func(_, _ string, _ int, _ string) error { return boom },
	}
	eng, _ := testEngineWithCache(t, client, &mockClaudeInvoker{})
	eng.echo = newEchoRegistry()
	item := gh.ProjectItem{Number: 1, Repo: "owner/repo"}

	eng.addLabel(item, "fabrik:paused")
	eng.removeLabel(item, "fabrik:paused")
	if _, err := eng.postComment(item, "hi", false, true); err == nil {
		t.Fatal("expected the comment write to fail")
	}
	if err := eng.updateIssueBody(item, "owner", "repo", "x"); err == nil {
		t.Fatal("expected the body write to fail")
	}
	if err := eng.addLabelChecked(item, "fabrik:cruise"); err == nil {
		t.Fatal("expected the checked label write to fail")
	}
	if err := eng.removeLabelChecked(item, "fabrik:cruise"); err == nil {
		t.Fatal("expected the checked label removal to fail")
	}
	if n := eng.echo.len(); n != 0 {
		t.Fatalf("%d pending echo(es) left after failed writes", n)
	}
	// A human doing the same thing later is not mistaken for the engine.
	if eng.echo.MatchEchoReported("issues", "labeled", boardcache.ItemKey("owner/repo", 1)+"+fabrik:paused") {
		t.Fatal("a failed write's echo swallowed a later identical human event")
	}
}

// A label removal GitHub reports as already absent (ErrNotFound) sends no
// webhook, so the reservation is dropped.
func TestEngineWrites_AlreadyAbsentLabelLeavesNoPendingEcho(t *testing.T) {
	client := &mockGitHubClient{
		removeLabelFromIssueFn: func(_, _ string, _ int, _ string) error { return gh.ErrNotFound },
	}
	eng, _ := testEngineWithCache(t, client, &mockClaudeInvoker{})
	eng.echo = newEchoRegistry()
	eng.removeLabel(gh.ProjectItem{Number: 1, Repo: "owner/repo"}, "fabrik:paused")
	if n := eng.echo.len(); n != 0 {
		t.Fatalf("%d pending echo(es) after an already-absent removal", n)
	}
}

// A poll-only engine (no transport, so no registry) reserves nothing and does
// not panic.
func TestEngineWrites_PollOnlyEngineReservesNothing(t *testing.T) {
	eng, _ := testEngineWithCache(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	if eng.echo != nil {
		t.Fatal("precondition: testEngine has no echo registry")
	}
	eng.addLabel(gh.ProjectItem{Number: 1, Repo: "owner/repo"}, "fabrik:paused")
	if _, err := eng.postComment(gh.ProjectItem{Number: 1, Repo: "owner/repo"}, "x", false, true); err != nil {
		t.Fatal(err)
	}
}

// A failed write's Cancel must not take an earlier write's pending echo with it:
// write A reserves, write B of the same key reserves and fails.
func TestEchoRegistry_CancelOfALaterReservationRestoresTheEarlierOne(t *testing.T) {
	r := newEchoRegistry()
	r.ReserveVia("issues", "labeled", "k", "alice")
	r.Reserve("issues", "labeled", "k").Cancel()
	ok, via := r.MatchEchoVia("issues", "labeled", "k")
	if !ok || via != "alice" {
		t.Fatalf("A's echo after B's failed write: matched=%v via=%q, want matched by alice", ok, via)
	}
}

// Same, when the failing reservation is not the newest: C displaced B, then B's
// write fails. B must be unlinked from behind C and A's echo must still match
// once C's has gone.
func TestEchoRegistry_CancelOfAnInnerReservationLeavesTheOthers(t *testing.T) {
	r := newEchoRegistry()
	r.ReserveVia("issues", "labeled", "k", "alice") // A
	b := r.Reserve("issues", "labeled", "k")        // B
	r.Reserve("issues", "labeled", "k")             // C (current)
	b.Cancel()
	// C is still the current entry.
	if !r.has(echoKey("issues", "labeled", "k")) {
		t.Fatal("cancelling an inner reservation removed the current entry")
	}
	// Cancel C: A (not B) must come back.
	r.mu.Lock()
	cur := r.pending[echoKey("issues", "labeled", "k")]
	r.mu.Unlock()
	(&echoReservation{r: r, key: echoKey("issues", "labeled", "k"), gen: cur.gen}).Cancel()
	ok, via := r.MatchEchoVia("issues", "labeled", "k")
	if !ok || via != "alice" {
		t.Fatalf("after cancelling B and C: matched=%v via=%q, want A (alice)", ok, via)
	}
}
