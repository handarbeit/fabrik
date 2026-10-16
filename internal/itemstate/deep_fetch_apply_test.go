package itemstate

import (
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// A deep-fetch result is built from the poll-start listing: its Status, labels
// and closed state are as old as the listing (#2344). These tests apply a write
// that landed after that listing and then the deep-fetch result.

func staleListing() gh.ProjectItem {
	pi := testProjectItem(testRepo, 1) // Status "Implement", labels [bug]
	pi.Title = "deep title"
	return pi
}

func TestDeepFetchApply_StatusMovedAfterListingSurvives(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		s := newStoreWithItem(t, testRepo, 1)
		s.SetLegacyDeepFetchApplyForTest(legacy)
		stale := staleListing()
		s.Apply(LocalStatusUpdated{Repo: testRepo, Number: 1, NewStatus: "Review"})
		seen := collectChanges(s)

		s.Apply(ItemDeepFetched{Repo: testRepo, Number: 1, FreshState: stale})

		got := getItem(t, s, testRepo, 1)
		reverted := got.Status != "Review"
		if reverted != legacy {
			t.Fatalf("legacy=%v: Status = %q (reverted=%v)", legacy, got.Status, reverted)
		}
		statusEvents := 0
		for _, c := range seen() {
			if c.Fields&StatusChanged != 0 {
				statusEvents++
			}
		}
		if (statusEvents > 0) != legacy {
			t.Errorf("legacy=%v: %d spurious StatusChanged events", legacy, statusEvents)
		}
	}
}

func TestDeepFetchApply_LabelChangeAfterListingSurvives(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		s := newStoreWithItem(t, testRepo, 1)
		s.SetLegacyDeepFetchApplyForTest(legacy)
		stale := staleListing()
		s.Apply(LocalLabelAdded{Repo: testRepo, Number: 1, Label: "stage:Implement:complete"})
		s.Apply(LocalLabelRemoved{Repo: testRepo, Number: 1, Label: "bug"})

		s.Apply(ItemDeepFetched{Repo: testRepo, Number: 1, FreshState: stale})

		ls := getItem(t, s, testRepo, 1).Labels
		kept := hasLabel(ls, "stage:Implement:complete") && !hasLabel(ls, "bug")
		if kept == legacy {
			t.Errorf("legacy=%v: labels = %v (survived=%v)", legacy, ls, kept)
		}
	}
}

func TestDeepFetchApply_ClosedAfterListingSurvives(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		s := newStoreWithItem(t, testRepo, 1)
		s.SetLegacyDeepFetchApplyForTest(legacy)
		stale := staleListing()
		s.Apply(LocalIssueClosed{Repo: testRepo, Number: 1})

		s.Apply(ItemDeepFetched{Repo: testRepo, Number: 1, FreshState: stale})

		closed := getItem(t, s, testRepo, 1).IsClosed
		if closed == legacy {
			t.Errorf("legacy=%v: IsClosed = %v", legacy, closed)
		}
	}
}

func TestDeepFetchApply_StillWritesDeepOwnedFields(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	pi := staleListing()
	pi.Body = "new body"
	pi.Comments = []gh.Comment{{ID: "c1", Body: "hello"}}
	pi.LinkedPRNumber = 7
	pi.LinkedPRIsInMergeQueue = true
	pi.LinkedPRIsMergeQueueEnabled = true
	pi.LinkedPRMergeQueueEntry = &gh.MergeQueueEntry{State: "QUEUED", Position: 2}
	_, changes, err := s.Apply(ItemDeepFetched{Repo: testRepo, Number: 1, FreshState: pi})
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].Fields&DeepFetchChanged == 0 || changes[0].Path != PathDeepFetch {
		t.Fatalf("changes = %+v", changes)
	}
	got := getItem(t, s, testRepo, 1)
	if got.Title != "deep title" || got.Body != "new body" || len(got.Comments) != 1 {
		t.Errorf("deep fields not written: %+v", got)
	}
	lp := got.LinkedPR
	if lp == nil || lp.Number != 7 || !lp.IsInMergeQueue || !lp.IsMergeQueueEnabled || lp.MergeQueueEntry == nil || lp.MergeQueueEntry.Position != 2 {
		t.Errorf("linked PR = %+v", lp)
	}
	if lp.MergeQueueEntry == pi.LinkedPRMergeQueueEntry {
		t.Error("MergeQueueEntry shares the caller's pointer")
	}
}

func TestDeepFetchApply_FirstPopulation(t *testing.T) {
	t.Run("new record", func(t *testing.T) {
		s := NewStore(nil)
		pi := staleListing()
		pi.IsClosed = true
		s.Apply(ItemDeepFetched{Repo: testRepo, Number: 1, FreshState: pi})
		got := getItem(t, s, testRepo, 1)
		if got.Status != "Implement" || !got.LabelsKnown || !hasLabel(got.Labels, "bug") || !got.IsClosed || got.Repo != testRepo || got.Number != 1 {
			t.Errorf("first population incomplete: %+v", got)
		}
	})
	t.Run("probe-seeded item gets its first labels once", func(t *testing.T) {
		s := NewStore(nil)
		seed := gh.ProjectItem{Repo: testRepo, Number: 1, Status: "Plan", LabelsAbsent: true}
		s.Apply(IssueOpened{Item: seed})
		if getItem(t, s, testRepo, 1).LabelsKnown {
			t.Fatal("seed already label-known")
		}
		seen := collectChanges(s)
		pi := staleListing()
		pi.Status = "Plan"
		s.Apply(ItemDeepFetched{Repo: testRepo, Number: 1, FreshState: pi})
		got := getItem(t, s, testRepo, 1)
		if !got.LabelsKnown || !hasLabel(got.Labels, "bug") {
			t.Errorf("labels not populated: %+v", got.Labels)
		}
		for _, c := range seen() {
			if len(c.LabelDeltas) != 0 {
				t.Errorf("first label population emitted deltas: %+v", c.LabelDeltas)
			}
		}
		// A second, stale apply must not overwrite what is now known.
		s.Apply(LocalLabelAdded{Repo: testRepo, Number: 1, Label: "fabrik:paused"})
		s.Apply(ItemDeepFetched{Repo: testRepo, Number: 1, FreshState: pi})
		if !hasLabel(getItem(t, s, testRepo, 1).Labels, "fabrik:paused") {
			t.Error("second deep fetch reverted a known label set")
		}
	})
	t.Run("empty status is filled", func(t *testing.T) {
		s := NewStore(nil)
		s.Apply(IssueOpened{Item: gh.ProjectItem{Repo: testRepo, Number: 1}})
		pi := staleListing()
		s.Apply(ItemDeepFetched{Repo: testRepo, Number: 1, FreshState: pi})
		if got := getItem(t, s, testRepo, 1).Status; got != "Implement" {
			t.Errorf("Status = %q", got)
		}
	})
}

func TestDeepFetchApply_BaselinesNeverMovedBackByCacheHit(t *testing.T) {
	early := time.Unix(1000, 0)
	for _, legacy := range []bool{false, true} {
		s := newStoreWithItem(t, testRepo, 1)
		s.SetLegacyDeepFetchApplyForTest(legacy)
		s.Apply(SelfWriteObserved{Repo: testRepo, Number: 1})
		before := getItem(t, s, testRepo, 1)
		late := before.LastSeenSourceUpdatedAt
		if !late.After(early) {
			t.Fatalf("setup: baseline = %v, want a self-write time after %v", late, early)
		}
		pi := staleListing()
		pi.UpdatedAt = early
		pi.LinkedPRUpdatedAt = early
		s.Apply(ItemDeepFetched{Repo: testRepo, Number: 1, FreshState: pi}) // DeepFetchLive false: cache hit
		got := getItem(t, s, testRepo, 1)
		moved := !got.LastSeenSourceUpdatedAt.Equal(late)
		if moved != legacy {
			t.Errorf("legacy=%v: baseline = %v", legacy, got.LastSeenSourceUpdatedAt)
		}
		if !got.UpdatedAt.Equal(before.UpdatedAt) && !legacy && got.UpdatedAt.Before(before.UpdatedAt) {
			t.Errorf("UpdatedAt moved backwards: %v -> %v", before.UpdatedAt, got.UpdatedAt)
		}
	}
}

// A live refetch is held to the same rule as a cache hit: a baseline that
// SelfWriteObserved raised while the fetch was in flight must not be lowered by
// the response (#2344, R4 as written).
func TestDeepFetchApply_LiveRefetchNeverLowersBaselines(t *testing.T) {
	early := time.Unix(1000, 0)
	for _, legacy := range []bool{false, true} {
		s := newStoreWithItem(t, testRepo, 1)
		s.SetLegacyDeepFetchApplyForTest(legacy)
		s.Apply(SelfWriteObserved{Repo: testRepo, Number: 1})
		late := getItem(t, s, testRepo, 1).LastSeenSourceUpdatedAt
		pi := staleListing()
		pi.UpdatedAt = early
		pi.LinkedPRUpdatedAt = early
		pi.DeepFetchLive = true
		s.Apply(ItemDeepFetched{Repo: testRepo, Number: 1, FreshState: pi})
		got := getItem(t, s, testRepo, 1)
		lowered := !got.LastSeenSourceUpdatedAt.Equal(late)
		if lowered != legacy {
			t.Errorf("legacy=%v: baseline = %v, was %v", legacy, got.LastSeenSourceUpdatedAt, late)
		}
	}
}

func TestDeepFetchApply_LiveRefetchRaisesBaselines(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	s.Apply(SelfWriteObserved{Repo: testRepo, Number: 1})
	future := time.Now().Add(time.Hour)
	pi := staleListing()
	pi.UpdatedAt = future
	pi.LinkedPRUpdatedAt = future
	pi.DeepFetchLive = true
	s.Apply(ItemDeepFetched{Repo: testRepo, Number: 1, FreshState: pi})
	got := getItem(t, s, testRepo, 1)
	if !got.LastSeenSourceUpdatedAt.Equal(future) || !got.LastSeenPRUpdatedAt.Equal(future) {
		t.Errorf("a live refetch must raise the baselines: got %v / %v want %v", got.LastSeenSourceUpdatedAt, got.LastSeenPRUpdatedAt, future)
	}
}

// liveFetch is a live deep-fetch result whose request started at startedAt.
func liveFetch(startedAt time.Time, labels []string, closed bool) gh.ProjectItem {
	pi := staleListing()
	pi.Labels = labels
	pi.IsClosed = closed
	pi.DeepFetchLive = true
	pi.DeepFetchStartedAt = startedAt
	return pi
}

// A live fetch is held to the engine-write protection every listing path has
// (ADR 2159/2178): an engine write recorded inside the grace window of the
// request's start may not be in the response, so the local value is kept. The
// legacy apply has no such rule and reverts it.
func TestDeepFetchApply_LiveFetchKeepsEngineWritesInsideGrace(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		s := newStoreWithItem(t, testRepo, 1)
		s.SetLegacyDeepFetchApplyForTest(legacy)
		startedAt := time.Now()
		s.Apply(LocalLabelAdded{Repo: testRepo, Number: 1, Label: "stage:Implement:complete"})
		s.Apply(LocalLabelRemoved{Repo: testRepo, Number: 1, Label: "bug"})
		s.Apply(LocalIssueClosed{Repo: testRepo, Number: 1})
		seen := collectChanges(s)

		s.Apply(ItemDeepFetched{Repo: testRepo, Number: 1, FreshState: liveFetch(startedAt, []string{"bug"}, false)})

		got := getItem(t, s, testRepo, 1)
		kept := hasLabel(got.Labels, "stage:Implement:complete") && !hasLabel(got.Labels, "bug") && got.IsClosed
		if kept == legacy {
			t.Errorf("legacy=%v: labels=%v closed=%v (kept=%v)", legacy, got.Labels, got.IsClosed, kept)
		}
		if !legacy {
			for _, c := range seen() {
				if c.Fields&(LabelsChanged|StateChanged) != 0 || len(c.LabelDeltas) != 0 {
					t.Errorf("kept engine writes produced a reverting event: %+v", c)
				}
			}
		}
	}
}

// Outside the window the response is trusted and wins, as for the listings.
func TestDeepFetchApply_LiveFetchAppliesOutsideGrace(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	s.Apply(LocalLabelAdded{Repo: testRepo, Number: 1, Label: "stage:Implement:complete"})
	s.Apply(LocalLabelRemoved{Repo: testRepo, Number: 1, Label: "bug"})
	s.Apply(LocalIssueClosed{Repo: testRepo, Number: 1})
	startedAt := time.Now().Add(DefaultEngineWriteGrace + time.Second)

	s.Apply(ItemDeepFetched{Repo: testRepo, Number: 1, FreshState: liveFetch(startedAt, []string{"bug"}, false)})

	got := getItem(t, s, testRepo, 1)
	if hasLabel(got.Labels, "stage:Implement:complete") || !hasLabel(got.Labels, "bug") || got.IsClosed {
		t.Errorf("a fetch started after the grace window must win: labels=%v closed=%v", got.Labels, got.IsClosed)
	}
}

// A live read with no start stamp (a producer that does not set it) protects
// nothing, like a zero FetchedAt on the listings.
func TestDeepFetchApply_LiveFetchWithoutStartStampIsUnprotected(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	s.Apply(LocalLabelAdded{Repo: testRepo, Number: 1, Label: "stage:Implement:complete"})
	s.Apply(ItemDeepFetched{Repo: testRepo, Number: 1, FreshState: liveFetch(time.Time{}, []string{"bug"}, false)})
	if hasLabel(getItem(t, s, testRepo, 1).Labels, "stage:Implement:complete") {
		t.Errorf("a zero DeepFetchStartedAt must not protect")
	}
}

// A live fetch does not read Status, so it neither keeps nor confirms Status.
func TestDeepFetchApply_LiveFetchDoesNotConsumeStatusRecord(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	startedAt := time.Now()
	s.Apply(LocalStatusUpdated{Repo: testRepo, Number: 1, NewStatus: "Review"})
	s.Apply(ItemDeepFetched{Repo: testRepo, Number: 1, FreshState: liveFetch(startedAt.Add(time.Minute), []string{"bug"}, false)})
	got := getItem(t, s, testRepo, 1)
	if _, ok := got.EngineWriteAt[engineWriteStatusKey]; !ok {
		t.Errorf("a deep fetch must not drop the Status engine-write record: %v", got.EngineWriteAt)
	}
}

// A live fetch read labels and closed state from GitHub just now; they are how
// a change without a webhook reaches the Store, so they are written (Status
// never is: the deep fetch does not read it).
func TestDeepFetchApply_LiveFetchWritesLabelsAndClosedButNotStatus(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	s.Apply(LocalStatusUpdated{Repo: testRepo, Number: 1, NewStatus: "Review"})
	pi := staleListing()
	pi.Labels = []string{"bug", "added-externally"}
	pi.IsClosed = true
	pi.DeepFetchLive = true
	s.Apply(ItemDeepFetched{Repo: testRepo, Number: 1, FreshState: pi})
	got := getItem(t, s, testRepo, 1)
	if !hasLabel(got.Labels, "added-externally") || !got.IsClosed {
		t.Errorf("live labels/closed not written: labels=%v closed=%v", got.Labels, got.IsClosed)
	}
	if got.Status != "Review" {
		t.Errorf("a deep fetch must not write Status; got %q", got.Status)
	}
}
