package engine

import (
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/boardcache"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

// probeDriftHarness drives runProbeAndDeepFetch for one item whose probe value
// and deep-fetch value are controlled by the test (#2080).
type probeDriftHarness struct {
	eng        *Engine
	cache      *boardcache.CacheImpl
	probePR    atomic.Int64
	deepPR     atomic.Int64
	deepFetchs atomic.Int64
	status     string
	closed     bool
}

// newProbeDriftHarness seeds a warm (already deep-fetched) item #1 at status
// with cachedPR as its cached linkage. The probe reports probePR and the deep
// fetch reports deepPR; both are adjustable afterwards.
func newProbeDriftHarness(t *testing.T, status string, closed bool, cachedPR, probePR, deepPR int) *probeDriftHarness {
	t.Helper()
	h := &probeDriftHarness{status: status, closed: closed}
	h.probePR.Store(int64(probePR))
	h.deepPR.Store(int64(deepPR))
	warm := time.Now().Add(-30 * time.Minute)

	client := &mockGitHubClient{
		probeProjectBoardFn: func(string, string, int, string) ([]gh.BoardProbeItem, string, error) {
			return []gh.BoardProbeItem{{
				ItemID: "PVTI_001", ContentID: "I_001", Number: 1, Repo: "owner/repo",
				Status: h.status, IsClosed: h.closed, EffectiveUpdatedAt: warm,
				LinkedPRNumber: int(h.probePR.Load()),
			}}, "PVT_1", nil
		},
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			h.deepFetchs.Add(1)
			item.LinkedPRNumber = int(h.deepPR.Load())
			return nil
		},
	}
	h.eng = testEngineWithCleanup(t, client, &mockClaudeInvoker{})
	h.cache = boardcache.NewCacheImpl(client, h.eng.store, func(string, ...any) {})
	testBootstrapFromBoard(h.cache, &gh.ProjectBoard{
		ProjectID: "PVT_1",
		Items: []gh.ProjectItem{{
			ID: "I_001", ItemID: "PVTI_001", Number: 1, Repo: "owner/repo",
			Status: status, UpdatedAt: warm, IsClosed: closed, LinkedPRNumber: cachedPR,
		}},
	})
	h.eng.readClient = h.cache
	h.eng.store.Apply(itemstate.ItemDeepFetched{
		Repo: "owner/repo", Number: 1,
		FreshState: gh.ProjectItem{ID: "I_001", Number: 1, Repo: "owner/repo", Status: status, UpdatedAt: warm, LinkedPRNumber: cachedPR},
	})
	return h
}

func (h *probeDriftHarness) poll(n int) {
	for i := 0; i < n; i++ {
		h.eng.runProbeAndDeepFetch(h.cache)
	}
}

func (h *probeDriftHarness) cachedPR(t *testing.T) int {
	t.Helper()
	snap, err := h.eng.store.Get("owner/repo", 1)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if lp := snap.State().LinkedPR; lp != nil {
		return lp.Number
	}
	return 0
}

// TestProbeDrift_ClosedPRDeepFetchKeepsFindingIt_InvalidatesOnce is the #1655
// shape: the cache holds PR 1654 (found by a REST lookup), the probe reports 0
// because closedByPullRequestsReferences omits a closed PR, and the deep fetch
// reports 0 too. The disagreement never converges by itself, so the ledger must
// stop the per-poll invalidation after the first one.
func TestProbeDrift_ClosedPRDeepFetchKeepsFindingIt_InvalidatesOnce(t *testing.T) {
	h := newProbeDriftHarness(t, "Research", false, 1654, 0, 0)
	h.poll(25)
	if got := h.deepFetchs.Load(); got != 1 {
		t.Errorf("deep fetches across 25 polls = %d, want exactly 1 (one invalidation, then converged)", got)
	}
	if got := h.cachedPR(t); got != 1654 {
		t.Errorf("cached PR = %d, want 1654 left untouched (a deep-fetch 0 is not authoritative)", got)
	}
}

// TestProbeDrift_PersistentDisagreementLoggedOnce pins R1's "logged once per
// item per pair of values".
func TestProbeDrift_PersistentDisagreementLoggedOnce(t *testing.T) {
	h := newProbeDriftHarness(t, "Research", false, 1654, 0, 0)
	out := captureStdout(func() { h.poll(10) })
	if n := strings.Count(out, "linkage disagreement persists"); n != 1 {
		t.Errorf("persistent-disagreement lines = %d, want 1\n%s", n, out)
	}
	if n := strings.Count(out, "invalidating deep cache"); n != 1 {
		t.Errorf("invalidation lines = %d, want 1\n%s", n, out)
	}
}

// TestProbeDrift_GenuineLinkChange_InvalidatesOnceThenSettles: PR A → PR B.
func TestProbeDrift_GenuineLinkChange_InvalidatesOnceThenSettles(t *testing.T) {
	h := newProbeDriftHarness(t, "Research", false, 5, 7, 7)
	h.poll(10)
	if got := h.deepFetchs.Load(); got != 1 {
		t.Errorf("deep fetches = %d, want 1", got)
	}
	if got := h.cachedPR(t); got != 7 {
		t.Errorf("cached PR = %d, want 7", got)
	}
}

// TestProbeDrift_ReturnToEarlierPair_InvalidatesAgain: A → B → A. The pair is
// forgotten when the values agree, so coming back to an earlier pair is a
// genuine new change and invalidates.
func TestProbeDrift_ReturnToEarlierPair_InvalidatesAgain(t *testing.T) {
	h := newProbeDriftHarness(t, "Research", false, 5, 7, 7)
	h.poll(3)
	h.probePR.Store(5)
	h.deepPR.Store(5)
	h.poll(3)
	h.probePR.Store(7)
	h.deepPR.Store(7)
	h.poll(3)
	if got := h.deepFetchs.Load(); got != 3 {
		t.Errorf("deep fetches = %d, want 3 (5→7, 7→5, 5→7)", got)
	}
}

// TestProbeDrift_FreshLedgerCostsOneInvalidation: a restart loses the ledger.
func TestProbeDrift_FreshLedgerCostsOneInvalidation(t *testing.T) {
	h := newProbeDriftHarness(t, "Research", false, 1654, 0, 0)
	h.poll(5)
	h.eng.probeDrift = probeDriftLedger{}
	h.poll(5)
	if got := h.deepFetchs.Load(); got != 2 {
		t.Errorf("deep fetches = %d, want 2 (one per ledger lifetime)", got)
	}
}

// TestProbeDrift_TerminalFlaggedItem_NeverInvalidates (R2): a flagged terminal
// item in a cleanup stage skips the drift check entirely.
func TestProbeDrift_TerminalFlaggedItem_NeverInvalidates(t *testing.T) {
	h := newProbeDriftHarness(t, "Done", true, 1654, 0, 0)
	h.eng.store.Apply(itemstate.TerminalFlagSet{Repo: "owner/repo", Number: 1, Terminal: true})
	before := getLastDeepFetch(t, h)
	out := captureStdout(func() { h.poll(10) })
	if strings.Contains(out, "linkage drift") {
		t.Errorf("terminal item logged linkage drift:\n%s", out)
	}
	if got := h.deepFetchs.Load(); got != 0 {
		t.Errorf("deep fetches = %d, want 0", got)
	}
	if after := getLastDeepFetch(t, h); !after.Equal(before) {
		t.Errorf("LastDeepFetchAt changed %v → %v: the deep cache was invalidated", before, after)
	}
}

// TestProbeDrift_ClosedDoneNoLabelNoWorktree_NeverInvalidates (R2): an item
// that never got the terminal flag (no stage:Done:complete label) is still
// exempt via the probe-only predicate.
func TestProbeDrift_ClosedDoneNoLabelNoWorktree_NeverInvalidates(t *testing.T) {
	h := newProbeDriftHarness(t, "Done", true, 1654, 0, 0)
	out := captureStdout(func() { h.poll(10) })
	if strings.Contains(out, "linkage drift") {
		t.Errorf("closed Done item logged linkage drift:\n%s", out)
	}
	if got := h.deepFetchs.Load(); got != 0 {
		t.Errorf("deep fetches = %d, want 0", got)
	}
}

// TestProbeDrift_ClosedDoneWorktreePresent_StillDrifts: with the worktree on
// disk the item is not terminal yet, so it keeps normal drift handling.
func TestProbeDrift_ClosedDoneWorktreePresent_StillDrifts(t *testing.T) {
	h := newProbeDriftHarness(t, "Done", true, 1654, 0, 0)
	h.eng.mu.Lock()
	wm := h.eng.worktreeManagers["owner/repo"]
	h.eng.mu.Unlock()
	if wm == nil {
		t.Fatal("no worktree manager registered for owner/repo")
	}
	if err := os.MkdirAll(wm.WorktreeDir(1), 0o755); err != nil {
		t.Fatal(err)
	}
	// Bootstrap may have seeded the terminal flag before the worktree existed.
	h.eng.store.Apply(itemstate.TerminalFlagSet{Repo: "owner/repo", Number: 1, Terminal: false})
	h.poll(1)
	if got := h.deepFetchs.Load(); got != 1 {
		t.Errorf("deep fetches = %d, want 1 (worktree present → not terminal → drift handled)", got)
	}
}

// TestProbeDrift_OpenItemInDoneStillDrifts: an open item is never exempt.
func TestProbeDrift_OpenItemInDoneStillDrifts(t *testing.T) {
	h := newProbeDriftHarness(t, "Done", false, 1654, 0, 0)
	h.poll(1)
	if got := h.deepFetchs.Load(); got != 1 {
		t.Errorf("deep fetches = %d, want 1", got)
	}
}

func getLastDeepFetch(t *testing.T, h *probeDriftHarness) time.Time {
	t.Helper()
	snap, err := h.eng.store.Get("owner/repo", 1)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return snap.State().LastDeepFetchAt
}

// TestProbeDrift_InvalidationAndDeepFetchDoNotWake (R3): a genuine link change
// (which sets LinkedPRChanged, a wake flag) written by the probe loop must not
// request an immediate poll.
func TestProbeDrift_InvalidationAndDeepFetchDoNotWake(t *testing.T) {
	h := newProbeDriftHarness(t, "Research", false, 5, 7, 7)
	wakeCh := make(chan struct{}, 16)
	h.eng.store.Subscribe(newWakeChObserver(wakeCh))
	h.poll(3)
	if got := h.deepFetchs.Load(); got != 1 {
		t.Fatalf("deep fetches = %d, want 1 (the invalidation must have happened)", got)
	}
	if n := len(wakeCh); n != 0 {
		t.Errorf("probe-loop writes requested %d wake(s), want 0", n)
	}
	// Control: the same linkage change from a non-probe writer still wakes.
	h.eng.store.Apply(itemstate.PRDetailsUpdated{Repo: "owner/repo", Number: 1, PRNumber: 9})
	if n := len(wakeCh); n != 1 {
		t.Errorf("non-probe linkage write requested %d wake(s), want 1", n)
	}
}

// ---- ledger unit tests (R4) ----

func TestProbeDriftLedger_LoopWarningFiresOnceAtThreshold(t *testing.T) {
	var l probeDriftLedger
	now := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	warns := 0
	for i := 0; i < probeDriftLoopThreshold+5; i++ {
		// Alternate the pair so every observation is a new pair (a true loop).
		v := l.observe("o/r#1", i%2, (i+1)%2+10, now.Add(time.Duration(i)*time.Minute))
		if !v.Invalidate {
			t.Fatalf("observation %d did not invalidate", i)
		}
		if v.Warn {
			warns++
			if v.Count != probeDriftLoopThreshold+1 {
				t.Errorf("warned at count %d, want %d", v.Count, probeDriftLoopThreshold+1)
			}
		}
	}
	if warns != 1 {
		t.Errorf("warnings = %d, want exactly 1", warns)
	}
}

func TestProbeDriftLedger_FewGenuineChangesDoNotWarn(t *testing.T) {
	var l probeDriftLedger
	now := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	for i := 0; i < probeDriftLoopThreshold; i++ {
		if v := l.observe("o/r#1", i, i+1, now.Add(time.Duration(i)*time.Minute)); v.Warn {
			t.Fatalf("warned at invalidation %d (threshold %d)", i+1, probeDriftLoopThreshold)
		}
	}
}

func TestProbeDriftLedger_WarningReArmsAfterWindowEmpties(t *testing.T) {
	var l probeDriftLedger
	now := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	loop := func(start time.Time) (warns int) {
		for i := 0; i < probeDriftLoopThreshold+3; i++ {
			if l.observe("o/r#1", i, i+100, start.Add(time.Duration(i)*time.Second)).Warn {
				warns++
			}
		}
		return warns
	}
	if got := loop(now); got != 1 {
		t.Fatalf("first loop warnings = %d, want 1", got)
	}
	// A new loop a full window later: the old timestamps have aged out.
	if got := loop(now.Add(2 * probeDriftLoopWindow)); got != 1 {
		t.Errorf("second loop warnings = %d, want 1 (re-armed)", got)
	}
}

func TestProbeDriftLedger_RepeatedPairSkipsAndLogsOnce(t *testing.T) {
	var l probeDriftLedger
	now := time.Now()
	if v := l.observe("o/r#1", 1654, 0, now); !v.Invalidate {
		t.Fatal("first observation must invalidate")
	}
	v := l.observe("o/r#1", 1654, 0, now)
	if v.Invalidate || !v.LogSkip {
		t.Errorf("first repeat = %+v, want skip with LogSkip", v)
	}
	if v := l.observe("o/r#1", 1654, 0, now); v.Invalidate || v.LogSkip {
		t.Errorf("second repeat = %+v, want silent skip", v)
	}
	l.forget("o/r#1")
	if v := l.observe("o/r#1", 1654, 0, now); !v.Invalidate {
		t.Error("after forget the pair must invalidate again")
	}
}

// TestProbeDrift_EngineLoopWarning: end to end through the probe loop with an
// injected clock — strictly alternating pairs trip the R4 warning once.
func TestProbeDrift_EngineLoopWarning(t *testing.T) {
	h := newProbeDriftHarness(t, "Research", false, 5, 7, 7)
	h.eng.SetClock(stubClock{t: time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)})
	out := captureStdout(func() {
		for i := 0; i < probeDriftLoopThreshold+4; i++ {
			// Flip between two linkages every poll: probe and deep fetch agree,
			// but the link changes each time, so each poll is a genuine drift.
			next := 7
			if i%2 == 1 {
				next = 5
			}
			h.probePR.Store(int64(next))
			h.deepPR.Store(int64(next))
			h.poll(1)
		}
	})
	if n := strings.Count(out, "linkage drift loop"); n != 1 {
		t.Errorf("loop warnings = %d, want 1\n%s", n, out)
	}
}

// ---- neutralisation (#2080): each fix, switched off, brings the old behavior back ----

func TestProbeDrift_Neutralised_ConvergenceRestoresPerPollInvalidation(t *testing.T) {
	h := newProbeDriftHarness(t, "Research", false, 1654, 0, 0)
	h.eng.SetProbeDriftNeutralisationForTest(true, false)
	h.poll(25)
	if got := h.deepFetchs.Load(); got != 25 {
		t.Errorf("with convergence neutralised, deep fetches over 25 polls = %d, want 25 (the pre-fix loop)", got)
	}
}

func TestProbeDrift_Neutralised_TerminalSkipRestoresInvalidation(t *testing.T) {
	h := newProbeDriftHarness(t, "Done", true, 1654, 0, 0)
	h.eng.store.Apply(itemstate.TerminalFlagSet{Repo: "owner/repo", Number: 1, Terminal: true})
	before := getLastDeepFetch(t, h)
	h.eng.SetProbeDriftNeutralisationForTest(false, true)
	h.poll(1)
	if after := getLastDeepFetch(t, h); after.Equal(before) {
		t.Error("with the terminal skip neutralised the terminal item should have been invalidated by drift (pre-fix behavior)")
	}
}
