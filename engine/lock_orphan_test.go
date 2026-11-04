package engine

import (
	"errors"
	"sync"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/attention"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

// R2/R3/R4 (#2550): the runtime orphan-lock reclaim and its shared clock.

type orphanHarness struct {
	mu        sync.Mutex
	labels    map[int][]string // live labels per issue
	removed   []string         // "N:label"
	readErr   error
	rmErr     error
	appliedAt time.Time
}

func newOrphanEngine(t *testing.T, h *orphanHarness) (*Engine, *forgeClock) {
	t.Helper()
	client := &mockGitHubClient{
		fetchLabelsFn: func(_, _ string, n int) ([]string, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			if h.readErr != nil {
				return nil, h.readErr
			}
			return append([]string(nil), h.labels[n]...), nil
		},
		fetchLabelAppliedAtFn: func(_, _ string, _ int, _ string) (time.Time, error) {
			return h.appliedAt, nil
		},
		removeLabelFromIssueFn: func(_, _ string, n int, l string) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			if h.rmErr != nil {
				return h.rmErr
			}
			h.removed = append(h.removed, itemKey(n, l))
			var keep []string
			for _, x := range h.labels[n] {
				if x != l {
					keep = append(keep, x)
				}
			}
			h.labels[n] = keep
			return nil
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	clk := &forgeClock{t: time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)}
	eng.SetClock(clk)
	return eng, clk
}

func seedOrphanItem(t *testing.T, e *Engine, n int, labels ...string) {
	t.Helper()
	it := gh.ProjectItem{ID: "I", Number: n, Repo: "owner/repo", Status: "Review", Labels: labels, ItemID: "PVTI"}
	e.store.Apply(itemstate.IssueOpened{Item: it})
	e.store.Apply(itemstate.ItemDeepFetched{Repo: "owner/repo", Number: n, FreshState: it})
	if snap, ok := e.store.Peek("owner/repo", n); !ok || !snap.LabelsKnown() {
		t.Fatalf("seed: labels not known for #%d", n)
	}
}

func TestSettleOrphanedLocks_GraceBoundary(t *testing.T) {
	h := &orphanHarness{labels: map[int][]string{1: {"fabrik:editing"}}}
	eng, clk := newOrphanEngine(t, h)
	eng.SetLockOrphanGraceForTest(3 * time.Minute)
	seedOrphanItem(t, eng, 1, "fabrik:editing")

	eng.settleOrphanedLocks() // first sight starts the clock
	clk.Advance(3*time.Minute - time.Second)
	eng.settleOrphanedLocks()
	if len(h.removed) != 0 {
		t.Fatalf("reclaimed %v just before the grace", h.removed)
	}
	clk.Advance(time.Second) // exactly the grace
	eng.settleOrphanedLocks()
	if len(h.removed) != 1 || h.removed[0] != "1:fabrik:editing" {
		t.Fatalf("removed = %v, want one fabrik:editing reclaim at the grace", h.removed)
	}
}

func TestSettleOrphanedLocks_WorkerNeverReclaimed(t *testing.T) {
	h := &orphanHarness{labels: map[int][]string{2: {"fabrik:editing"}}}
	eng, clk := newOrphanEngine(t, h)
	seedOrphanItem(t, eng, 2, "fabrik:editing")
	eng.store.Apply(itemstate.LocalLockAcquired{Repo: "owner/repo", Number: 2, User: "u", AcquiredAt: clk.Now(),
		Worker: &itemstate.WorkerHandle{StageName: "Review", StartedAt: clk.Now()}})
	eng.settleOrphanedLocks()
	clk.Advance(time.Hour)
	eng.settleOrphanedLocks()
	if len(h.removed) != 0 {
		t.Fatalf("reclaimed %v under a live worker", h.removed)
	}
}

func TestSettleOrphanedLocks_WorkerAppearingResetsTheClock(t *testing.T) {
	h := &orphanHarness{labels: map[int][]string{3: {"fabrik:editing"}}}
	eng, clk := newOrphanEngine(t, h)
	eng.SetLockOrphanGraceForTest(3 * time.Minute)
	seedOrphanItem(t, eng, 3, "fabrik:editing")
	eng.settleOrphanedLocks()
	clk.Advance(2 * time.Minute)
	eng.store.Apply(itemstate.LocalLockAcquired{Repo: "owner/repo", Number: 3, User: "u", AcquiredAt: clk.Now(),
		Worker: &itemstate.WorkerHandle{StageName: "Review", StartedAt: clk.Now()}})
	eng.settleOrphanedLocks() // clock forgotten
	eng.store.Apply(itemstate.WorkerExited{Repo: "owner/repo", Number: 3})
	clk.Advance(2 * time.Minute) // 4 minutes since first sight, but only 2 since the worker left
	eng.settleOrphanedLocks()
	clk.Advance(time.Minute)
	eng.settleOrphanedLocks()
	if len(h.removed) != 0 {
		t.Fatalf("reclaimed %v although the worker only just left", h.removed)
	}
}

func TestSettleOrphanedLocks_OnlyOwnLocks(t *testing.T) {
	h := &orphanHarness{labels: map[int][]string{4: {"fabrik:locked:someone-else", "fabrik:locked:testuser", "stage:Review:in_progress", "fabrik:reworking:Review"}}}
	eng, clk := newOrphanEngine(t, h)
	eng.SetLockOrphanGraceForTest(time.Minute)
	seedOrphanItem(t, eng, 4, h.labels[4]...)
	eng.settleOrphanedLocks()
	clk.Advance(time.Hour)
	eng.settleOrphanedLocks()
	if len(h.removed) != 1 || h.removed[0] != "4:fabrik:locked:testuser" {
		t.Fatalf("removed = %v, want only this daemon's own lock", h.removed)
	}
}

func TestSettleOrphanedLocks_ErrNotFoundIsSuccess(t *testing.T) {
	h := &orphanHarness{labels: map[int][]string{5: {"fabrik:editing"}}, rmErr: gh.ErrNotFound}
	eng, clk := newOrphanEngine(t, h)
	eng.SetLockOrphanGraceForTest(time.Minute)
	seedOrphanItem(t, eng, 5, "fabrik:editing")
	eng.settleOrphanedLocks()
	clk.Advance(2 * time.Minute)
	eng.settleOrphanedLocks()
	if _, ok := eng.lockOrphans.oldest("owner/repo", 5); ok {
		t.Error("clock still tracks the label after an ErrNotFound removal")
	}
}

func TestSettleOrphanedLocks_FailedReadOrRemovalRetriesNextPollWithoutRequeue(t *testing.T) {
	h := &orphanHarness{labels: map[int][]string{6: {"fabrik:editing"}}, readErr: errTimeout}
	eng, clk := newOrphanEngine(t, h)
	eng.SetLockOrphanGraceForTest(time.Minute)
	seedOrphanItem(t, eng, 6, "fabrik:editing")
	eng.settleOrphanedLocks()
	clk.Advance(2 * time.Minute)
	eng.settleOrphanedLocks() // live read fails
	if len(h.removed) != 0 {
		t.Fatalf("removed %v without a live confirmation", h.removed)
	}
	if snap, _ := eng.store.Peek("owner/repo", 6); !snap.CooldownAt(lockWriteRetryReason).IsZero() {
		t.Error("cooldown recorded for a reclaim that did not happen")
	}
	h.mu.Lock()
	h.readErr, h.rmErr = nil, errors.New("boom")
	h.mu.Unlock()
	eng.settleOrphanedLocks() // removal fails
	if len(h.removed) != 0 {
		t.Fatalf("removed %v despite a failed write", h.removed)
	}
	h.mu.Lock()
	h.rmErr = nil
	h.mu.Unlock()
	eng.settleOrphanedLocks() // retried on the next poll
	if len(h.removed) != 1 {
		t.Fatalf("removed = %v, want the retry to succeed", h.removed)
	}
}

func TestSettleOrphanedLocks_LiveAbsentForgetsTheClock(t *testing.T) {
	h := &orphanHarness{labels: map[int][]string{7: nil}} // Store stale: GitHub no longer has it
	eng, clk := newOrphanEngine(t, h)
	eng.SetLockOrphanGraceForTest(time.Minute)
	seedOrphanItem(t, eng, 7, "fabrik:editing")
	eng.settleOrphanedLocks()
	clk.Advance(2 * time.Minute)
	eng.settleOrphanedLocks()
	if len(h.removed) != 0 {
		t.Fatalf("removed %v for a label GitHub does not have", h.removed)
	}
}

func TestSettleOrphanedLocks_YoungEditingLabelRestartsTheClock(t *testing.T) {
	h := &orphanHarness{labels: map[int][]string{8: {"fabrik:editing"}}}
	eng, clk := newOrphanEngine(t, h)
	eng.SetLockOrphanGraceForTest(3 * time.Minute)
	seedOrphanItem(t, eng, 8, "fabrik:editing")
	eng.settleOrphanedLocks()
	clk.Advance(4 * time.Minute)
	// Another instance placed it 30s ago.
	h.appliedAt = clk.Now().Add(-30 * time.Second)
	eng.settleOrphanedLocks()
	if len(h.removed) != 0 {
		t.Fatalf("reclaimed %v although the label is 30s old", h.removed)
	}
	clk.Advance(3 * time.Minute)
	eng.settleOrphanedLocks()
	if len(h.removed) != 1 {
		t.Fatalf("removed = %v, want the reclaim once the label itself is past grace", h.removed)
	}
}

// R3 + FR-009: the reclaim re-admits the item and writes nothing else.
func TestSettleOrphanedLocks_ReadmitsAndChargesNothing(t *testing.T) {
	h := &orphanHarness{labels: map[int][]string{9: {"fabrik:editing"}}}
	eng, clk := newOrphanEngine(t, h)
	eng.SetLockOrphanGraceForTest(time.Minute)
	seedOrphanItem(t, eng, 9, "fabrik:editing")
	eng.settleOrphanedLocks()
	clk.Advance(2 * time.Minute)
	eng.settleOrphanedLocks()

	eng.mayNeedWorkMu.Lock()
	queued := eng.mayNeedWork["owner/repo#9"]
	eng.mayNeedWorkMu.Unlock()
	if !queued {
		t.Error("item not re-admitted (mayNeedWork) after the reclaim")
	}
	snap, _ := eng.store.Peek("owner/repo", 9)
	if snap.CooldownAt(lockWriteRetryReason).IsZero() || snap.CooldownAt(lockWriteRetryReason).After(clk.Now()) {
		t.Errorf("want an already-due %s cooldown, got %v", lockWriteRetryReason, snap.CooldownAt(lockWriteRetryReason))
	}
	if snap.Attempts("Review") != 0 || !snap.LastAttemptAt("Review").IsZero() {
		t.Error("the reclaim recorded a stage attempt")
	}
	if got := eng.commentBreakerCount(gh.ProjectItem{Number: 9, Repo: "owner/repo"}); got != 0 {
		t.Errorf("comment breaker charged: %d", got)
	}
	for _, l := range snap.Labels() {
		if l == "fabrik:paused" || l == "stage:Review:failed" {
			t.Errorf("unexpected label %q", l)
		}
	}
}

func TestSettleOrphanedLocks_NeutralisedForgeHeldAndDrainingRemoveNothing(t *testing.T) {
	h := &orphanHarness{labels: map[int][]string{10: {"fabrik:editing"}}}
	eng, clk := newOrphanEngine(t, h)
	eng.SetLockOrphanGraceForTest(time.Minute)
	eng.SetLockOrphanReclaimDisabledForTest(true)
	seedOrphanItem(t, eng, 10, "fabrik:editing")
	eng.settleOrphanedLocks()
	clk.Advance(time.Hour)
	eng.settleOrphanedLocks()
	if len(h.removed) != 0 {
		t.Fatalf("neutralised reclaim removed %v", h.removed)
	}
	// The clock keeps running while the reclaim is off: attention still sees it.
	if fact := eng.lockOrphanFact(&itemstate.ItemState{Repo: "owner/repo", Number: 10}); fact == nil || fact.Label != "fabrik:editing" {
		t.Errorf("lockOrphanFact = %+v, want the editing orphan", fact)
	}
	eng.SetLockOrphanReclaimDisabledForTest(false)
	eng.SetForgeHoldThresholdsForTest(1, time.Nanosecond)
	eng.ObserveForgeResult(errTimeout)
	clk.Advance(time.Second)
	eng.ObserveForgeResult(errTimeout)
	if !eng.forgeHeld() {
		t.Fatal("hold did not engage")
	}
	eng.settleOrphanedLocks()
	if len(h.removed) != 0 {
		t.Fatalf("reclaimed %v while the forge hold is engaged", h.removed)
	}
}

// R4 end to end: the engine's attention input carries the fact and the
// classifier reports stalled past grace, not inside it.
func TestAttentionInput_CarriesTheLockOrphanFact(t *testing.T) {
	h := &orphanHarness{labels: map[int][]string{11: {"fabrik:editing"}}}
	eng, clk := newOrphanEngine(t, h)
	eng.SetLockOrphanReclaimDisabledForTest(true) // keep the label so the verdict is observable
	eng.SetLockOrphanGraceForTest(3 * time.Minute)
	seedOrphanItem(t, eng, 11, "fabrik:editing")
	eng.settleOrphanedLocks()

	verdict := func() attention.Result {
		var res attention.Result
		eng.store.Scan(func(st *itemstate.ItemState) {
			if st.Number != 11 {
				return
			}
			in := attention.Input{Now: eng.now(), Status: "Review", StageKind: attention.KindManaged,
				Labels: st.Labels, StatusEnteredAt: clk.Now().Add(-time.Hour),
				Cfg: attention.Config{StallThreshold: 30 * time.Minute}}
			eng.fillWorkerInputs(&in, st, eng.now(), time.Time{})
			res = attention.Classify(in)
		})
		return res
	}
	if res := verdict(); res.Code == attention.CodeLockOrphaned {
		t.Fatalf("stalled inside the grace: %s", res.Summary)
	}
	clk.Advance(4 * time.Minute)
	if res := verdict(); res.State != attention.Stalled || res.Code != attention.CodeLockOrphaned {
		t.Fatalf("got %s/%s, want stalled/%s", res.State, res.Code, attention.CodeLockOrphaned)
	}
}
