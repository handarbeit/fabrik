package engine

import (
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

// TestPushUnblockObserver_BlockerClosesViaReconcile is the production scenario:
// the blocker's IsClosed flips via ShallowBoardItemUpdated (the mutation Reconcile
// emits), not via IssueClosed. Verifies the observer still fires.
func TestPushUnblockObserver_BlockerClosesViaReconcile(t *testing.T) {
	store := itemstate.NewStore(nil)

	// Seed blocker Q (open) and dependent R (blocked, with Q in BlockedBy as OPEN).
	store.Apply(itemstate.IssueOpened{Item: gh.ProjectItem{Number: 662, Repo: "handarbeit/fabrik"}})
	store.Apply(itemstate.IssueOpened{Item: gh.ProjectItem{
		Number:    663,
		Repo:      "handarbeit/fabrik",
		Status:    "Specify",
		Labels:    []string{"fabrik:blocked", "fabrik:yolo"},
		BlockedBy: []gh.Dependency{{Number: 662, Repo: "handarbeit/fabrik", State: "OPEN"}},
	}})

	removeCh := make(chan int, 1)
	obs := &PushUnblockObserver{
		Store:  store,
		Remove: func(owner, repo string, n int) { removeCh <- n },
	}
	store.Subscribe(obs)

	// Close Q via the Reconcile path: ShallowBoardItemUpdated with IsClosed=true.
	store.Apply(itemstate.ShallowBoardItemUpdated{
		Repo:   "handarbeit/fabrik",
		Number: 662,
		Item: gh.ProjectItem{
			Number:   662,
			Repo:     "handarbeit/fabrik",
			IsClosed: true,
		},
	})

	select {
	case n := <-removeCh:
		if n != 663 {
			t.Errorf("expected Remove called for issue 663, got %d", n)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout: Remove was not called after blocker closed via ShallowBoardItemUpdated")
	}
}
