package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/boardcache"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

// Tests for #2096 (ADR 2096): an expired cooldown admits an item once and is then
// consumed; a paused item reads through the cache (#2180); the merge gate
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

// FR-006 / SC-003 under the #2180 rule: a paused item admitted by an expired
// periodic cooldown on every poll reads through the board cache, so across 20
// polls with an unchanged board it costs zero live fetches; a reply masked by a
// self-write earns exactly one, after which the cache is settled again; a failed
// fetch is not recorded as served and is retried at the next admission. Every
// poll sees an expired cooldown — the worst case the old sticky expiry produced.
func TestAdmission_PausedReadsThroughCache(t *testing.T) {
	var fetchErr error
	client := &mockGitHubClient{}
	eng, start := cooldownAdmissionEngine(t, client)
	cache := boardcache.NewCacheImpl(client, eng.store, func(string, ...any) {})
	eng.readClient = cache

	updatedAt := start.Add(-time.Minute)
	total := 1 // GitHub's issue comment total
	client.fetchItemDetailsFn = func(it *gh.ProjectItem) error {
		if fetchErr != nil {
			return fetchErr
		}
		it.UpdatedAt = updatedAt
		it.Comments = nil
		for i := 1; i <= total; i++ {
			it.Comments = append(it.Comments, gh.Comment{DatabaseID: i})
		}
		return nil
	}
	board := func() *gh.ProjectBoard {
		return cooldownAdmissionBoard(gh.ProjectItem{
			Number: 75, Repo: cooldownTestRepo, Title: "Just paused", Status: "Research",
			Labels:    []string{"fabrik:paused", "fabrik:awaiting-input"},
			UpdatedAt: updatedAt, IssueLayerUpdatedAt: updatedAt,
			IssueCommentTotal: total, IssueCommentTotalKnown: true,
		})
	}
	eng.store.Apply(itemstate.ItemDeepFetched{Repo: cooldownTestRepo, Number: 75, FreshState: gh.ProjectItem{
		Number: 75, Repo: cooldownTestRepo, Status: "Research",
		Labels:    []string{"fabrik:paused", "fabrik:awaiting-input"},
		Comments:  []gh.Comment{{DatabaseID: 1}},
		UpdatedAt: updatedAt, IssueLayerUpdatedAt: updatedAt,
		IssueCommentTotal: 1, IssueCommentTotalKnown: true,
	}})
	eng.store.Apply(itemstate.SelfWriteObserved{Repo: cooldownTestRepo, Number: 75, At: updatedAt.Add(time.Second)}) // the pause write (GitHub time)

	step := 0
	pollN := func(n int) int {
		before := fetchCount(client)
		for i := 0; i < n; i++ {
			step++
			now := start.Add(time.Duration(step) * 10 * time.Second)
			eng.SetClock(stubClock{t: now})
			eng.store.Apply(itemstate.CooldownRecorded{Repo: cooldownTestRepo, Number: 75, Reason: "periodic-re-eval", Until: now.Add(-time.Second)})
			cands, _ := eng.selectDeepFetchCandidates(board(), "", map[string]bool{}, map[string]bool{})
			if fetchErr == nil && len(cands) != 1 {
				t.Fatalf("poll %d: paused item must stay a candidate (#1379), got %d", step, len(cands))
			}
		}
		return fetchCount(client) - before
	}

	if got := pollN(20); got != 0 {
		t.Fatalf("20 polls, unchanged board: %d live fetches, want 0", got)
	}
	// A human replies just before the engine's next self-write: the comment total
	// moves while no updatedAt does.
	total = 2
	eng.store.Apply(itemstate.SelfWriteObserved{Repo: cooldownTestRepo, Number: 75, At: updatedAt.Add(2 * time.Second)})
	if got := pollN(20); got != 1 {
		t.Fatalf("20 polls after a masked reply: %d live fetches, want 1", got)
	}
	// A failed fetch records nothing: the cache stays stale, so the masked
	// comment is still found at the next admission that can reach GitHub. (The
	// retry is not the next poll: admission consumed the expiry and re-armed
	// periodic-re-eval, and the failure backoff holds the item off.)
	total = 3
	fetchErr = errors.New("boom")
	if got := pollN(1); got != 1 {
		t.Fatalf("failing fetch: %d calls, want 1", got)
	}
	snap, _ := eng.store.Get(cooldownTestRepo, 75)
	b := board().Items[0]
	if cache.IsItemCacheFresh(cooldownTestRepo, 75, boardcache.EvidenceFromItem(&b)) {
		t.Errorf("a failed fetch must leave the cache stale (baseline %v)", snap.State().LastSeenSourceUpdatedAt)
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

// A failed non-Validate advance must not mark the item advanced: the poll defer
// skips advanced items, and with the expired cooldown already consumed the
// periodic-re-eval re-stamp is the only thing that retries the advance (#2096).
func TestRunCatchUpPhase2_FailedAdvanceIsNotMarkedAdvanced(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineWithStages(t, client, testStagesWithValidate())
	eng.cfg.Yolo = true
	eng.statusField = nil // advanceToNextStage fails: "status field metadata not available"

	advanced := map[string]bool{}
	eng.runCatchUpPhase2(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"},
		gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Repo: "owner/repo", Status: "Research"}, &stages.Stage{Name: "Research"}, advanced)
	if len(client.updateStatusCalls) != 0 {
		t.Fatalf("advance unexpectedly reached the board: %+v", client.updateStatusCalls)
	}
	if advanced["owner/repo#1"] {
		t.Error("a failed advance was marked advanced, so the poll defer would skip the periodic re-arm and nothing would retry it")
	}
}

// Review follow-up (#2096): a stage that exits incomplete leaves LastAttemptAt
// inside stageRetryBackoff, which records no cooldown of its own. dispatchCandidates
// records "retry-backoff" for the remaining wait, so the expiry re-admits the item
// once, rather than leaving it to the periodic re-evaluation (10 x PollSeconds).
func TestAdmission_RetryBackoffExpiryReadmitsOnce(t *testing.T) {
	client := &mockGitHubClient{}
	eng, start := cooldownAdmissionEngine(t, client)
	eng.cfg.PollSeconds = 180
	eng.cfg.RetryBackoff = 60 * time.Second
	item := gh.ProjectItem{Number: 76, Title: "Item", Status: "Research"}
	// An incomplete attempt 50s ago (LastAttemptAt is wall-clock): ~10s of backoff left.
	eng.store.Apply(itemstate.StageAttempted{Repo: cooldownTestRepo, Number: 76, StageName: "Research", At: time.Now().Add(-50 * time.Second)})
	// The poll's own re-arm, as the defer would stamp it after the dispatch poll.
	eng.store.Apply(itemstate.CooldownRecorded{Repo: cooldownTestRepo, Number: 76, Reason: "periodic-re-eval", Until: start.Add(eng.githubRecheckInterval())})

	board := cooldownAdmissionBoard(item)
	// The poll after the worker exits: admitted (label churn), refused by the backoff.
	if n, _ := eng.dispatchCandidates(context.Background(), board, []gh.ProjectItem{item}); n != 0 {
		t.Fatalf("dispatched %d item(s) inside the retry backoff, want 0", n)
	}
	snap, _ := eng.store.Get(cooldownTestRepo, 76)
	until := snap.CooldownAt("retry-backoff")
	if remaining := until.Sub(start); remaining <= 0 || remaining > 11*time.Second {
		t.Fatalf("retry-backoff cooldown = start+%v, want the ~10s left of the backoff", remaining)
	}

	poll := func() int {
		before := fetchCount(client)
		eng.selectDeepFetchCandidates(board, "", map[string]bool{}, map[string]bool{})
		return fetchCount(client) - before
	}
	eng.SetClock(stubClock{t: until.Add(-time.Second)})
	if got := poll(); got != 0 {
		t.Fatalf("before the backoff ends: %d fetch(es), want 0", got)
	}
	// First poll after LastAttemptAt + backoff: re-admitted, far inside the periodic interval.
	eng.SetClock(stubClock{t: until})
	if got := poll(); got != 1 {
		t.Fatalf("first poll after the backoff: %d fetch(es), want 1", got)
	}
	if until.Sub(start) >= eng.githubRecheckInterval() {
		t.Fatal("test setup: backoff must be shorter than the periodic interval")
	}
	eng.SetClock(stubClock{t: until.Add(time.Minute)})
	if got := poll(); got != 0 {
		t.Fatalf("after the expiry was used: %d fetch(es), want 0 (consumed once)", got)
	}
}

// A refusal for any other reason records no retry-backoff cooldown.
func TestAdmission_NoRetryBackoffCooldownWithoutAttempt(t *testing.T) {
	client := &mockGitHubClient{}
	eng, _ := cooldownAdmissionEngine(t, client)
	eng.cfg.RetryBackoff = 60 * time.Second
	item := gh.ProjectItem{Number: 77, Title: "Item", Status: "Research", Labels: []string{"stage:Research:complete"}}
	eng.store.Apply(itemstate.ItemDeepFetched{Repo: cooldownTestRepo, Number: 77, FreshState: item})
	eng.dispatchCandidates(context.Background(), cooldownAdmissionBoard(item), []gh.ProjectItem{item})
	snap, _ := eng.store.Get(cooldownTestRepo, 77)
	if got := snap.CooldownAt("retry-backoff"); !got.IsZero() {
		t.Errorf("retry-backoff recorded for an item not in backoff: %v", got)
	}
}

// Review follow-up (#2096): dispatch refused because a cleared worker's process is
// still alive records a one-poll cooldown, so it is re-checked next poll.
func TestAdmission_SentinelLiveRefusalRecordsCooldown(t *testing.T) {
	client := &mockGitHubClient{}
	eng, start := cooldownAdmissionEngine(t, client)
	item := gh.ProjectItem{Number: 78, Title: "Item", Status: "Research"}

	origSupported, origList := claudeNameFlagSupported, listProcessArgvFn
	t.Cleanup(func() { claudeNameFlagSupported, listProcessArgvFn = origSupported, origList })
	claudeNameFlagSupported = true
	sentinel := sessionNameSentinel(cooldownTestRepo, 78, "Research")
	listProcessArgvFn = func() ([]procArgvEntry, error) {
		return []procArgvEntry{{PID: 4242, Argv: []string{"claude", sentinel}}}, nil
	}

	if n, _ := eng.dispatchCandidates(context.Background(), cooldownAdmissionBoard(item), []gh.ProjectItem{item}); n != 0 {
		t.Fatalf("dispatched %d item(s) with a live sentinel, want 0", n)
	}
	snap, _ := eng.store.Get(cooldownTestRepo, 78)
	want := start.Add(eng.mergeGateRecheckInterval())
	if got := snap.CooldownAt("sentinel-live"); !got.Equal(want) {
		t.Errorf("sentinel-live cooldown = %v, want %v", got, want)
	}
}

// #2114 review: a slot-deferred item is re-queued into mayNeedWork, so the next
// poll admits it through cycleSet alone (no expired cooldown consumed, and the
// deferral skipped its periodic-re-eval stamp). If that poll's deep-fetch fails
// the item used to be dropped with no cooldown and no cycleSet entry — stranded
// until an unrelated change. A cycleSet-admitted drop must leave a future
// re-admission, and the failure backoff then governs how soon it comes back.
func TestAdmission_CycleSetAdmittedFailedFetchIsRearmedAndRecovers(t *testing.T) {
	var fetchErr = errors.New("boom")
	client := &mockGitHubClient{}
	client.fetchItemDetailsFn = func(*gh.ProjectItem) error { return fetchErr }
	eng, start := cooldownAdmissionEngine(t, client)
	item := gh.ProjectItem{Number: 76, Title: "Deferred", Status: "Research"}
	key := issueKey(item, eng.defaultRepo())
	interval := eng.githubRecheckInterval()
	// Known to the store with no cooldown, as a slot-deferred item is.
	eng.store.Apply(itemstate.SelfWriteObserved{Repo: cooldownTestRepo, Number: 76, At: start})

	cands, _ := eng.selectDeepFetchCandidates(cooldownAdmissionBoard(item), "", map[string]bool{key: true}, map[string]bool{})
	if len(cands) != 0 {
		t.Fatalf("failed fetch returned %d candidate(s), want 0", len(cands))
	}
	snap, _ := eng.store.Get(cooldownTestRepo, 76)
	if got, want := snap.CooldownAt("periodic-re-eval"), start.Add(interval); !got.Equal(want) {
		t.Fatalf("periodic-re-eval after a cycleSet-admitted failed fetch = %v, want %v (item would be stranded)", got, want)
	}

	// Nothing re-admits it before the interval, with no cycleSet entry.
	before := fetchCount(client)
	eng.selectDeepFetchCandidates(cooldownAdmissionBoard(item), "", map[string]bool{}, map[string]bool{})
	if fetchCount(client) != before {
		t.Fatal("item re-admitted before its re-armed interval")
	}

	// Once the interval has passed (and the failure backoff with it) and GitHub
	// recovers, the item is admitted again on an ordinary poll.
	fetchErr = nil
	eng.SetClock(stubClock{t: start.Add(interval + time.Second)})
	eng.store.Apply(itemstate.DeepFetchFailed{Repo: cooldownTestRepo, Number: 76, At: time.Now().Add(-2 * interval)})
	cands, _ = eng.selectDeepFetchCandidates(cooldownAdmissionBoard(item), "", map[string]bool{}, map[string]bool{})
	if len(cands) != 1 {
		t.Fatalf("after the interval the item produced %d candidate(s), want 1 (stranded)", len(cands))
	}
}

// #2114 review: the other drop site. A cycleSet-admitted item refused by
// itemMayNeedWork (here: inside the deep-fetch failure backoff) must also be
// left with a future re-admission.
func TestAdmission_CycleSetAdmittedBackoffDropIsRearmed(t *testing.T) {
	client := &mockGitHubClient{}
	eng, start := cooldownAdmissionEngine(t, client)
	item := gh.ProjectItem{Number: 77, Title: "Deferred", Status: "Research"}
	key := issueKey(item, eng.defaultRepo())
	eng.store.Apply(itemstate.DeepFetchFailed{Repo: cooldownTestRepo, Number: 77, At: time.Now()})

	cands, _ := eng.selectDeepFetchCandidates(cooldownAdmissionBoard(item), "", map[string]bool{key: true}, map[string]bool{})
	if len(cands) != 0 || fetchCount(client) != 0 {
		t.Fatalf("item inside the failure backoff must not be fetched (candidates=%d fetches=%d)", len(cands), fetchCount(client))
	}
	snap, _ := eng.store.Get(cooldownTestRepo, 77)
	if got, want := snap.CooldownAt("periodic-re-eval"), start.Add(eng.githubRecheckInterval()); !got.Equal(want) {
		t.Errorf("periodic-re-eval after a cycleSet-admitted backoff drop = %v, want %v", got, want)
	}
}
