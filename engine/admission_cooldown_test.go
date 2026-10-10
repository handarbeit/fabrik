package engine

import (
	"errors"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

// Tests for #2096 (ADR 2096): an expired cooldown admits an item once and is then
// consumed; the paused-item backstop fetches once per baseline; the merge gate
// records a short re-check cooldown for transient claims. Every time value is
// derived from one stubClock (never a mix of time.Now() and the stub), so a test
// cannot pass vacuously on clock-domain skew.

const cooldownTestRepo = "owner/repo"

// cooldownAdmissionEngine returns an engine on a stub clock with a real recheck interval.
func cooldownAdmissionEngine(t *testing.T, client *mockGitHubClient) (*Engine, time.Time) {
	t.Helper()
	eng := testEngine(t, client, &mockClaudeInvoker{})
	eng.cfg.PollSeconds = 60
	start := time.Now()
	eng.SetClock(stubClock{t: start})
	return eng, start
}

func cooldownAdmissionBoard(item gh.ProjectItem) *gh.ProjectBoard {
	return &gh.ProjectBoard{ProjectID: "PVT_1", Items: []gh.ProjectItem{item}}
}

func fetchCount(c *mockGitHubClient) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.fetchItemDetailsCalls)
}

// FR-011 / SC-002: an expiry admits the item exactly once; a new cooldown that
// expires admits it once more.
func TestAdmission_ExpiredCooldownAdmitsOnce(t *testing.T) {
	client := &mockGitHubClient{}
	eng, start := cooldownAdmissionEngine(t, client)
	item := gh.ProjectItem{Number: 70, Title: "Item", Status: "Research"}
	eng.store.Apply(itemstate.CooldownRecorded{Repo: cooldownTestRepo, Number: 70, Reason: "review-blocked", Until: start.Add(-time.Minute)})

	poll := func() int {
		before := fetchCount(client)
		eng.selectDeepFetchCandidates(cooldownAdmissionBoard(item), "", map[string]bool{}, map[string]bool{})
		return fetchCount(client) - before
	}
	if got := poll(); got != 1 {
		t.Fatalf("first poll after expiry: %d fetch(es), want 1", got)
	}
	for i := 0; i < 5; i++ {
		eng.SetClock(stubClock{t: start.Add(time.Duration(i+1) * time.Minute)})
		if got := poll(); got != 0 {
			t.Fatalf("poll %d after the expiry was used: %d fetch(es), want 0 (sticky expiry)", i+2, got)
		}
	}
	// A new cooldown records and expires: admitted once more, then quiet again.
	now := start.Add(10 * time.Minute)
	eng.SetClock(stubClock{t: now})
	eng.store.Apply(itemstate.CooldownRecorded{Repo: cooldownTestRepo, Number: 70, Reason: "feedback-pending", Until: now.Add(30 * time.Second)})
	if got := poll(); got != 0 {
		t.Fatalf("while the new cooldown is active: %d fetch(es), want 0", got)
	}
	eng.SetClock(stubClock{t: now.Add(30 * time.Second)}) // exactly at expiry counts as expired
	if got := poll(); got != 1 {
		t.Fatalf("at the new cooldown's expiry: %d fetch(es), want 1", got)
	}
	eng.SetClock(stubClock{t: now.Add(time.Minute)})
	if got := poll(); got != 0 {
		t.Fatalf("after the new expiry was used: %d fetch(es), want 0", got)
	}
}

// An expiry that coincides with another admission reason (cycleSet) is still used up.
func TestAdmission_ExpiryAlongsideCycleSetIsConsumed(t *testing.T) {
	client := &mockGitHubClient{}
	eng, start := cooldownAdmissionEngine(t, client)
	item := gh.ProjectItem{Number: 71, Title: "Item", Status: "Research"}
	eng.store.Apply(itemstate.CooldownRecorded{Repo: cooldownTestRepo, Number: 71, Reason: "dep-blocked", Until: start.Add(-time.Minute)})

	eng.selectDeepFetchCandidates(cooldownAdmissionBoard(item), "", map[string]bool{cooldownTestRepo + "#71": true}, map[string]bool{})
	if before := fetchCount(client); before != 1 {
		t.Fatalf("cycleSet admission: %d fetch(es), want 1", before)
	}
	eng.SetClock(stubClock{t: start.Add(time.Minute)})
	eng.selectDeepFetchCandidates(cooldownAdmissionBoard(item), "", map[string]bool{}, map[string]bool{})
	if got := fetchCount(client); got != 1 {
		t.Errorf("the expiry that coincided with cycleSet admission re-admitted the item: %d fetches total, want 1", got)
	}
}

// FR-008 / SC-004 / R4: an item admitted but dropped before joining
// deepFetchCandidates (here: awaiting-done fails itemMayNeedWork) keeps a periodic
// re-evaluation entry, re-armed on every admission, so it is never stranded.
func TestAdmission_DroppedItemKeepsPeriodicReEval(t *testing.T) {
	client := &mockGitHubClient{}
	eng, start := cooldownAdmissionEngine(t, client)
	item := gh.ProjectItem{Number: 72, Title: "Item", Status: "Research", Labels: []string{"fabrik:awaiting-done"}}
	eng.store.Apply(itemstate.CooldownRecorded{Repo: cooldownTestRepo, Number: 72, Reason: "periodic-re-eval", Until: start.Add(-time.Second)})

	interval := eng.githubRecheckInterval()
	for round := 1; round <= 3; round++ {
		now := start.Add(time.Duration(round-1) * interval)
		eng.SetClock(stubClock{t: now})
		eng.selectDeepFetchCandidates(cooldownAdmissionBoard(item), "", map[string]bool{}, map[string]bool{})
		snap, err := eng.store.Get(cooldownTestRepo, 72)
		if err != nil {
			t.Fatal(err)
		}
		want := now.Add(interval)
		if got := snap.CooldownAt("periodic-re-eval"); !got.Equal(want) {
			t.Fatalf("round %d: periodic-re-eval = %v, want re-armed to %v", round, got, want)
		}
		// Not yet due: an immediate re-poll must not re-admit (and so not re-arm).
		eng.selectDeepFetchCandidates(cooldownAdmissionBoard(item), "", map[string]bool{}, map[string]bool{})
		snap, _ = eng.store.Get(cooldownTestRepo, 72)
		if got := snap.CooldownAt("periodic-re-eval"); !got.Equal(want) {
			t.Fatalf("round %d: re-polled before the interval and the entry moved to %v", round, got)
		}
	}
}

// R4: a failed deep fetch also leaves the periodic re-evaluation armed.
func TestAdmission_FailedFetchKeepsPeriodicReEval(t *testing.T) {
	client := &mockGitHubClient{fetchItemDetailsFn: func(*gh.ProjectItem) error { return errors.New("boom") }}
	eng, start := cooldownAdmissionEngine(t, client)
	item := gh.ProjectItem{Number: 73, Title: "Item", Status: "Research"}
	eng.store.Apply(itemstate.CooldownRecorded{Repo: cooldownTestRepo, Number: 73, Reason: "periodic-re-eval", Until: start.Add(-time.Second)})

	_, deepFetched := eng.selectDeepFetchCandidates(cooldownAdmissionBoard(item), "", map[string]bool{}, map[string]bool{})
	if deepFetched != 0 {
		t.Fatalf("deepFetched = %d, want 0 on a failed fetch", deepFetched)
	}
	snap, _ := eng.store.Get(cooldownTestRepo, 73)
	want := start.Add(eng.githubRecheckInterval())
	if got := snap.CooldownAt("periodic-re-eval"); !got.Equal(want) {
		t.Errorf("periodic-re-eval after a failed fetch = %v, want %v", got, want)
	}
}

// R4: an idle item admitted by the periodic re-check is admitted again once per
// interval (the defer's re-stamp is simulated by recording the same cooldown).
func TestAdmission_PeriodicReCheckRecursEveryInterval(t *testing.T) {
	client := &mockGitHubClient{}
	eng, start := cooldownAdmissionEngine(t, client)
	item := gh.ProjectItem{Number: 74, Title: "Item", Status: "Research"}
	interval := eng.githubRecheckInterval()
	eng.store.Apply(itemstate.CooldownRecorded{Repo: cooldownTestRepo, Number: 74, Reason: "periodic-re-eval", Until: start.Add(interval)})

	admitted := 0
	for step := 0; step <= 30; step++ {
		now := start.Add(time.Duration(step) * time.Minute)
		eng.SetClock(stubClock{t: now})
		before := fetchCount(client)
		cands, _ := eng.selectDeepFetchCandidates(cooldownAdmissionBoard(item), "", map[string]bool{}, map[string]bool{})
		if fetchCount(client) > before {
			admitted++
			// What poll()'s defer does for a non-advanced candidate.
			for range cands {
				eng.store.Apply(itemstate.CooldownRecorded{Repo: cooldownTestRepo, Number: 74, Reason: "periodic-re-eval", Until: now.Add(interval)})
			}
		}
	}
	// interval is 10 minutes: admissions at minutes 10, 20, 30.
	if admitted != 3 {
		t.Errorf("admitted %d time(s) over 30 minutes, want 3 (one per periodic interval)", admitted)
	}
}

// FR-006 / SC-003: across 20 polls with an unchanged baseline the paused backstop
// makes at most one live fetch; a fresh self-write earns exactly one more; a failed
// fetch records nothing and is retried. Every poll sees an expired cooldown — the
// worst case the old sticky expiry produced.
func TestAdmission_PausedBackstopFetchesOncePerBaseline(t *testing.T) {
	// The real fetch re-anchors the baseline to GitHub's updatedAt, which is not the
	// self-write instant; a mock that leaves it alone could not tell "baseline at
	// decision time" from "baseline after the fetch".
	var fetchErr error
	client := &mockGitHubClient{}
	eng, start := cooldownAdmissionEngine(t, client)
	updatedAt := start.Add(-time.Second)
	client.fetchItemDetailsFn = func(it *gh.ProjectItem) error {
		if fetchErr != nil {
			return fetchErr
		}
		it.UpdatedAt = updatedAt
		return nil
	}
	item := gh.ProjectItem{Number: 75, Title: "Just paused", Status: "Research", Labels: []string{"fabrik:paused", "fabrik:awaiting-input"}}
	eng.store.Apply(itemstate.SelfWriteObserved{Repo: cooldownTestRepo, Number: 75}) // the pause write

	step := 0
	pollN := func(n int) int {
		before := fetchCount(client)
		for i := 0; i < n; i++ {
			step++
			now := start.Add(time.Duration(step) * 10 * time.Second)
			eng.SetClock(stubClock{t: now})
			eng.store.Apply(itemstate.CooldownRecorded{Repo: cooldownTestRepo, Number: 75, Reason: "periodic-re-eval", Until: now.Add(-time.Second)})
			cands, _ := eng.selectDeepFetchCandidates(cooldownAdmissionBoard(item), "", map[string]bool{}, map[string]bool{})
			if fetchErr == nil && len(cands) != 1 {
				t.Fatalf("poll %d: paused item must stay a candidate (#1379), got %d", step, len(cands))
			}
		}
		return fetchCount(client) - before
	}

	if got := pollN(20); got != 1 {
		t.Fatalf("20 polls, unchanged baseline: %d backstop fetches, want 1", got)
	}
	// A new self-write is a new baseline that may mask a new reply: one more fetch.
	eng.store.Apply(itemstate.SelfWriteObserved{Repo: cooldownTestRepo, Number: 75})
	if got := pollN(20); got != 1 {
		t.Fatalf("20 polls after a fresh self-write: %d backstop fetches, want 1", got)
	}
	// A failed fetch must not be recorded as served for the new baseline. (The
	// retry itself is then paced by the engine's own deep-fetch failure back-off in
	// itemMayNeedWork, which is unchanged.)
	eng.store.Apply(itemstate.SelfWriteObserved{Repo: cooldownTestRepo, Number: 75})
	snap, _ := eng.store.Get(cooldownTestRepo, 75)
	newBaseline := snap.State().LastSeenSourceUpdatedAt
	fetchErr = errors.New("boom")
	if got := pollN(1); got != 1 {
		t.Fatalf("failing backstop fetch: %d calls, want 1", got)
	}
	snap, _ = eng.store.Get(cooldownTestRepo, 75)
	if snap.PausedBackstopBaseline().Equal(newBaseline) {
		t.Error("a failed backstop fetch was recorded as served for the new baseline")
	}
}

// FR-001 / FR-003: the merge gate records merge-unsettled on a transient claim and
// nothing for a conflict.
func TestHandleMergeAndCIGates_RecordsMergeUnsettledCooldown(t *testing.T) {
	unsettled := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, n int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 42, HeadSHA: "deadbeef", State: "open"}, nil
		},
		fetchPRMergeableFieldsFn: func(owner, repo string, n int) (*bool, string, error) {
			return nil, "unknown", nil // mergeable not yet computed → PRMergeUnsettled
		},
	}
	stgs := []*stages.Stage{{Name: "Implement", Order: 1, Prompt: "implement"}, {Name: "Review", Order: 2, Prompt: "review"}}
	eng := testEngineWithStages(t, unsettled, stgs)
	eng.cfg.PollSeconds = 60
	start := time.Now()
	eng.SetClock(stubClock{t: start})

	pctx := makeMergeGatePctx(&gh.ProjectBoard{ProjectID: "PVT_1"}, map[string]bool{})
	if !eng.handleMergeAndCIGates(pctx) {
		t.Fatal("unsettled merge state must claim the item")
	}
	snap, _ := eng.store.Get(cooldownTestRepo, 20)
	if got, want := snap.CooldownAt(mergeUnsettledCooldownReason), start.Add(30*time.Second); !got.Equal(want) {
		t.Errorf("merge-unsettled = %v, want %v", got, want)
	}

	// A conflict is a different branch (rebase dispatch) and records no such cooldown.
	conflict := conflictingSettleClient()
	eng2 := testEngineWithStages(t, conflict, stgs)
	eng2.cfg.MaxRebaseCycles = 3
	eng2.SetClock(stubClock{t: start})
	if !eng2.handleMergeAndCIGates(makeMergeGatePctx(&gh.ProjectBoard{ProjectID: "PVT_1"}, map[string]bool{})) {
		t.Fatal("conflict must claim the item")
	}
	eng2.wg.Wait()
	snap2, err := eng2.store.Get(cooldownTestRepo, 20)
	if err == nil && !snap2.CooldownAt(mergeUnsettledCooldownReason).IsZero() {
		t.Error("a merge conflict must not record the merge-unsettled cooldown (FR-003)")
	}
}

func TestMergeGateRecheckInterval(t *testing.T) {
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	for _, tc := range []struct {
		poll int
		want time.Duration
	}{{0, time.Second}, {1, time.Second}, {2, time.Second}, {60, 30 * time.Second}, {180, 90 * time.Second}} {
		eng.cfg.PollSeconds = tc.poll
		if got := eng.mergeGateRecheckInterval(); got != tc.want {
			t.Errorf("PollSeconds=%d: %v, want %v", tc.poll, got, tc.want)
		}
	}
}
