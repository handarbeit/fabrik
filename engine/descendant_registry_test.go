package engine

import (
	"testing"
	"time"
)

func TestDescendantRegistry_MissingFileIsEmpty(t *testing.T) {
	t.Chdir(t.TempDir())
	entries, err := allTrackedDescendants()
	if err != nil {
		t.Fatalf("allTrackedDescendants: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected empty registry, got %d entries", len(entries))
	}
}

func TestDescendantRegistry_UpsertLoadRoundTrip(t *testing.T) {
	t.Chdir(t.TempDir())
	d := trackedDescendant{
		PID: 4242, Comm: "sleep", LStart: "Wed Sep 18 12:00:00 2026",
		WorkerPID: 100, IssueNumber: 7, Repo: "acme/widgets", Stage: "Implement",
		DiscoveredAt: time.Now(),
	}
	if err := upsertTrackedDescendant(d); err != nil {
		t.Fatalf("upsertTrackedDescendant: %v", err)
	}
	got, err := descendantsForWorker(100)
	if err != nil {
		t.Fatalf("descendantsForWorker: %v", err)
	}
	if len(got) != 1 || got[0].PID != 4242 || got[0].Comm != "sleep" {
		t.Fatalf("unexpected entries: %+v", got)
	}

	// Upsert with same PID replaces rather than duplicates.
	d.Comm = "renamed"
	if err := upsertTrackedDescendant(d); err != nil {
		t.Fatalf("upsertTrackedDescendant (replace): %v", err)
	}
	got, err = descendantsForWorker(100)
	if err != nil {
		t.Fatalf("descendantsForWorker: %v", err)
	}
	if len(got) != 1 || got[0].Comm != "renamed" {
		t.Fatalf("expected replace not duplicate, got %+v", got)
	}
}

func TestDescendantRegistry_RemoveTrackedDescendants(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, pid := range []int{1, 2, 3} {
		if err := upsertTrackedDescendant(trackedDescendant{PID: pid, WorkerPID: 999}); err != nil {
			t.Fatalf("upsertTrackedDescendant(%d): %v", pid, err)
		}
	}
	if err := removeTrackedDescendants([]int{2}); err != nil {
		t.Fatalf("removeTrackedDescendants: %v", err)
	}
	got, err := descendantsForWorker(999)
	if err != nil {
		t.Fatalf("descendantsForWorker: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 remaining entries, got %d: %+v", len(got), got)
	}
	for _, e := range got {
		if e.PID == 2 {
			t.Fatalf("removed PID 2 still present: %+v", got)
		}
	}
}

func TestDescendantRegistry_ConcurrentUpsertSafe(t *testing.T) {
	t.Chdir(t.TempDir())
	done := make(chan struct{})
	for i := 0; i < 20; i++ {
		go func(pid int) {
			_ = upsertTrackedDescendant(trackedDescendant{PID: pid, WorkerPID: 1})
			done <- struct{}{}
		}(i)
	}
	for i := 0; i < 20; i++ {
		<-done
	}
	got, err := descendantsForWorker(1)
	if err != nil {
		t.Fatalf("descendantsForWorker: %v", err)
	}
	if len(got) != 20 {
		t.Fatalf("expected 20 entries from concurrent upserts, got %d", len(got))
	}
}
