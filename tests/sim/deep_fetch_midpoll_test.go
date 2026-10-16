package sim

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tests/sim/simgh"
)

// #2344: the deep-fetch result is built from a snapshot taken before the fetch.
// A write that lands between that snapshot and the apply (here: a worker's own
// board move, written through the cache the way UpdateItemStatus does) must
// survive the apply. BoardCache is required: the probe loop and the cache's
// own deep-fetch apply are exactly where the old write-back reverted it.

func deepFetchMidPollStages() []*stages.Stage {
	return []*stages.Stage{
		{Name: "Specify", Order: 1},
		{Name: "Plan", Order: 2},
		{Name: "Done", Order: 3, CleanupWorktree: true},
	}
}

// runMidPollWrite settles an item at Specify, then arranges for a status write
// to land inside the deep fetch a later external change triggers. It returns the
// store's Status for the item afterwards and how many status changes the store
// published while the poll ran.
func runMidPollWrite(t *testing.T, legacy bool) (status string, statusChanges, hookFired int) {
	t.Helper()
	env := NewEnv(t, EnvOptions{
		Stages:     deepFetchMidPollStages(),
		Yolo:       boolPtr(false),
		StartTime:  time.Now(),
		BoardCache: true,
	})
	t.Cleanup(env.Engine.RegisterObservers())
	num := FileIssue(t, env, "mid-poll write (#2344)", "body", "Specify")
	WaitForIssueLabel(t, env, num, "stage:Specify:complete", 40)
	RunPolls(t, env, 2)

	if legacy {
		env.Engine.SetLegacyDeepFetchApplyForTest(true)
	}
	item := projectItem(t, env, num)
	if got, _ := env.Engine.StoreItemForTest(env.OwnerRepo, num); got.Status != "Specify" {
		t.Fatalf("setup: store holds Status %q, want Specify", got.Status)
	}

	var mu sync.Mutex
	var changes int
	env.Engine.SubscribeStoreForTest(itemstate.ObserverFunc(func(c itemstate.Change, _ itemstate.Snapshot) {
		if c.Number == num && c.Fields&itemstate.StatusChanged != 0 {
			mu.Lock()
			changes++
			mu.Unlock()
		}
	}))

	var fired atomic.Int32
	env.Sim.Faults().BeforeCall("FetchItemDetails", func(a simgh.Args) {
		if a.Number != num || !fired.CompareAndSwap(0, 1) {
			return
		}
		// A worker's own move, written through the engine's store after the
		// listing/probe snapshot was taken and before the fetch result is applied.
		env.Engine.SimulateCacheStatusWriteThroughForTest(env.OwnerRepo, num, "Plan")
	})

	// An external change makes the cached copy stale, so the next poll refetches.
	if err := env.Sim.Sim().AddLabelToIssue(env.Owner, env.Repo, num, "operator-note"); err != nil {
		t.Fatal(err)
	}
	_ = item
	RunPoll(t, env)

	got, ok := env.Engine.StoreItemForTest(env.OwnerRepo, num)
	if !ok {
		t.Fatal("item missing from the store")
	}
	mu.Lock()
	defer mu.Unlock()
	return got.Status, changes, int(fired.Load())
}

func TestDeepFetch_WriteBetweenSnapshotAndApplySurvives(t *testing.T) {
	t.Parallel()
	status, changes, fired := runMidPollWrite(t, false)
	if fired != 1 {
		t.Fatalf("the deep fetch was not triggered (%d hook calls): this scenario no longer reproduces the window, so a pass would be vacuous", fired)
	}
	if status != "Plan" {
		t.Errorf("store Status = %q after the poll, want the mid-poll write (Plan)", status)
	}
	if changes != 1 {
		t.Errorf("store published %d status changes, want exactly the one write (no reverting event)", changes)
	}
}

// Non-vacuity: with the legacy apply the same scenario reverts the write.
func TestDeepFetch_WriteBetweenSnapshotAndApply_LegacyApplyReverts(t *testing.T) {
	t.Parallel()
	status, changes, fired := runMidPollWrite(t, true)
	if fired != 1 {
		t.Fatalf("the deep fetch was not triggered (%d hook calls): the twin would be vacuous", fired)
	}
	if status == "Plan" && changes == 1 {
		t.Errorf("legacy apply did not revert the mid-poll write (Status %q, %d changes): the twin cannot tell the fix from the old behaviour", status, changes)
	}
}

var _ = gh.ProjectItem{}
