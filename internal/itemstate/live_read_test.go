package itemstate

import (
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// Tests for the live-read mutations (#2350, ADR 2350): LabelsRead, PRReviewsRead,
// LinkedPRStateObserved, the FromLiveRead origin tag and the live-apply comment
// union.

func liveReadStore(t *testing.T) *Store {
	t.Helper()
	s := orderedStore(t)
	return s
}

func TestLabelsRead_AppliesFullSetAndKnownBaseline(t *testing.T) {
	s := liveReadStore(t)
	snap, _, err := s.Apply(LabelsRead{Repo: orderRepo, Number: 1, Labels: []string{"a", "b"}, FetchedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if got := snap.Labels(); len(got) != 2 {
		t.Fatalf("labels = %v, want a,b", got)
	}
	if snap.ObservedAt("labels").IsZero() {
		t.Fatal("labels observation not recorded")
	}
}

func TestLabelsRead_KeepsEngineWriteMadeAfterReadStarted(t *testing.T) {
	s := liveReadStore(t)
	start := time.Now()
	s.Apply(LocalLabelAdded{Repo: orderRepo, Number: 1, Label: "fabrik:paused"})
	// The read started before the engine write and does not contain the label.
	snap, _, _ := s.Apply(LabelsRead{Repo: orderRepo, Number: 1, Labels: []string{"x"}, FetchedAt: start})
	got := snap.Labels()
	found := false
	for _, l := range got {
		if l == "fabrik:paused" {
			found = true
		}
	}
	if !found {
		t.Fatalf("engine-written label cleared by an older read: %v", got)
	}
}

func TestLabelsRead_LateReadDoesNotRevertNewerSet(t *testing.T) {
	s := liveReadStore(t)
	now := time.Now()
	s.Apply(LabelsRead{Repo: orderRepo, Number: 1, Labels: []string{"new"}, FetchedAt: now})
	// A read that started long before (older than the grace) arrives late.
	snap, _, _ := s.Apply(LabelsRead{Repo: orderRepo, Number: 1, Labels: []string{"old"}, FetchedAt: now.Add(-10 * testGrace)})
	if got := snap.Labels(); len(got) != 1 || got[0] != "new" {
		t.Fatalf("late read reverted labels: %v", got)
	}
}

func TestPRReviewsRead_AppliesAndNewerReviewWins(t *testing.T) {
	s := liveReadStore(t)
	now := time.Now()
	snap, _, _ := s.Apply(PRReviewsRead{
		Repo: orderRepo, Number: 1, PRNumber: orderPR,
		Reviews:   []gh.PRReview{{Author: "bob", State: "APPROVED", DatabaseID: 2, SubmittedAt: now}},
		Requests:  []gh.ReviewRequest{{Login: "carol"}},
		FetchedAt: now,
	})
	lpr := snap.LinkedPR()
	if lpr == nil || len(lpr.Reviews) != 1 || len(lpr.ReviewRequests) != 1 || lpr.ReviewRequests[0].Login != "carol" {
		t.Fatalf("linked PR = %+v", lpr)
	}
	// A read that started long before arrives late: dropped.
	snap, _, _ = s.Apply(PRReviewsRead{
		Repo: orderRepo, Number: 1, PRNumber: orderPR,
		Requests:  nil,
		FetchedAt: now.Add(-10 * testGrace),
	})
	if lpr := snap.LinkedPR(); len(lpr.ReviewRequests) != 1 {
		t.Fatalf("late read reverted review requests: %+v", lpr.ReviewRequests)
	}
}

func TestLinkedPRStateObserved_SetsOnlyStateAndMerged(t *testing.T) {
	s := liveReadStore(t)
	now := time.Now()
	s.Apply(PRDetailsUpdated{Repo: orderRepo, Number: 1, PRNumber: orderPR, Title: "T", State: "open", Draft: true, At: now.Add(-time.Hour)})
	snap, _, _ := s.Apply(LinkedPRStateObserved{Repo: orderRepo, Number: 1, PRNumber: orderPR, State: "closed", Merged: true, At: now})
	lpr := snap.LinkedPR()
	if !lpr.Merged || lpr.State != "closed" {
		t.Fatalf("not applied: %+v", lpr)
	}
	if lpr.Title != "T" || !lpr.Draft {
		t.Fatalf("Title/Draft blanked: %+v", lpr)
	}
}

func TestLinkedPRStateObserved_LaggingListCannotRevert(t *testing.T) {
	s := liveReadStore(t)
	now := time.Now()
	s.Apply(LinkedPRStateObserved{Repo: orderRepo, Number: 1, PRNumber: orderPR, State: "closed", Merged: true, At: now})
	// A list read that started before the confirmation reports merged=false.
	snap, _, _ := s.Apply(PRDetailsUpdated{Repo: orderRepo, Number: 1, PRNumber: orderPR, State: "open", Merged: false, At: now.Add(-10 * testGrace), AtIsFetchStart: true})
	if lpr := snap.LinkedPR(); !lpr.Merged {
		t.Fatalf("lagging list read reverted the merge: %+v", lpr)
	}
}

func TestFromLiveRead_TagsOriginAndWakesNothing(t *testing.T) {
	s := liveReadStore(t)
	var got Change
	s.Subscribe(ObserverFunc(func(c Change, _ Snapshot) { got = c }))
	s.Apply(FromLiveRead{Inner: LabelsRead{Repo: orderRepo, Number: 1, Labels: []string{"z"}, FetchedAt: time.Now()}})
	if got.Origin != OriginLiveRead {
		t.Fatalf("origin = %v, want OriginLiveRead", got.Origin)
	}
	if got.Fields&LabelsChanged == 0 {
		t.Fatalf("fields = %v, want LabelsChanged", got.Fields)
	}
}

func TestLiveDeepFetch_KeepsCommentRecordedAfterReadStarted(t *testing.T) {
	s := liveReadStore(t)
	start := time.Now()
	// A comment the engine wrote after the read began.
	s.Apply(LocalCommentAdded{Repo: orderRepo, Number: 1, Comment: gh.Comment{ID: "late", CreatedAt: start.Add(time.Second)}})
	fresh := testProjectItem(orderRepo, 1)
	fresh.DeepFetchLive = true
	fresh.DeepFetchStartedAt = start
	fresh.Comments = []gh.Comment{{ID: "c1", CreatedAt: start.Add(-time.Hour)}}
	snap, _, _ := s.Apply(ItemDeepFetched{Repo: orderRepo, Number: 1, FreshState: fresh})
	ids := map[string]bool{}
	for _, c := range snap.State().Comments {
		ids[c.ID] = true
	}
	if !ids["c1"] || !ids["late"] {
		t.Fatalf("comments = %v, want c1 and late", ids)
	}
	// A cache-served (non-live) apply keeps today's replacement.
	cached := testProjectItem(orderRepo, 1)
	cached.Comments = []gh.Comment{{ID: "c1", CreatedAt: start.Add(-time.Hour)}}
	snap, _, _ = s.Apply(ItemDeepFetched{Repo: orderRepo, Number: 1, FreshState: cached})
	if len(snap.State().Comments) != 1 {
		t.Fatalf("non-live apply must replace wholesale: %v", snap.State().Comments)
	}
}
