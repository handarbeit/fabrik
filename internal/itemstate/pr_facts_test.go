package itemstate

import (
	"testing"
	"time"
)

func seedLinkedPR(t *testing.T, s *Store, sha string) {
	t.Helper()
	applyExpect(t, s, PRHeadSHAUpdated{Repo: testRepo, Number: 1, LinkedPRNum: 7, SHA: sha}, LinkedPRChanged)
}

func TestPRFactsObservedIsQuietAndRecorded(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	seedLinkedPR(t, s, "aaa")
	yes := true
	at := time.Now()
	facts := PRFacts{State: "open", Draft: true, Mergeable: &yes, MergeableState: "clean", HeadSHA: "aaa", ObservedAt: at}
	_, changes, err := s.Apply(PRFactsObserved{Repo: testRepo, Number: 1, PRNumber: 7, Facts: facts})
	if err != nil || len(changes) != 1 {
		t.Fatalf("Apply: changes=%d err=%v", len(changes), err)
	}
	if changes[0].Fields != PRFactsChanged {
		t.Errorf("flags = %b, want only PRFactsChanged", changes[0].Fields)
	}
	if PRFactsChanged&(LinkedPRChanged|StatusChanged|LabelsChanged|CommentsChanged|LockChanged|AssigneesChanged|WorkerLifecycleChanged|CheckRunChanged) != 0 {
		t.Error("PRFactsChanged must not alias a wake flag")
	}
	st := getItem(t, s, testRepo, 1)
	if !st.LinkedPR.Facts.Draft || st.LinkedPR.Facts.MergeableState != "clean" || st.LinkedPR.Facts.Mergeable == nil || !*st.LinkedPR.Facts.Mergeable {
		t.Errorf("Facts = %+v", st.LinkedPR.Facts)
	}
	// The gate-read fields are untouched.
	if st.LinkedPR.State != "" || st.LinkedPR.MergeableState != "" || st.LinkedPR.Mergeable != nil {
		t.Errorf("gate-read fields written: %+v", st.LinkedPR)
	}
	if !st.LastSeenSourceUpdatedAt.IsZero() || !st.LastSeenPRUpdatedAt.IsZero() || !st.LinkedPR.LastCIProgressAt.IsZero() {
		t.Error("PRFactsObserved moved a freshness baseline or LastCIProgressAt")
	}
	// Identical re-observation is a no-op.
	if _, c, _ := s.Apply(PRFactsObserved{Repo: testRepo, Number: 1, PRNumber: 7, Facts: facts}); len(c) != 0 {
		t.Errorf("identical facts produced %d changes", len(c))
	}
	// A different PR number is dropped.
	if _, c, _ := s.Apply(PRFactsObserved{Repo: testRepo, Number: 1, PRNumber: 8, Facts: PRFacts{State: "open", ObservedAt: at.Add(time.Second)}}); len(c) != 0 {
		t.Error("facts for another PR were applied")
	}
}

func TestCIVerdictIsHeadScopedAndClearedOnHeadChange(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	seedLinkedPR(t, s, "aaa")
	at := time.Now()
	applyExpect(t, s, CIVerdictObserved{Repo: testRepo, Number: 1, Verdict: CIVerdictRecord{Verdict: "red", HeadSHA: "aaa", Failing: []string{"Test and vet"}, ObservedAt: at}}, PRFactsChanged)
	st := getItem(t, s, testRepo, 1)
	if st.LinkedPR.CIVerdict.Verdict != "red" || len(st.LinkedPR.CIVerdict.Failing) != 1 {
		t.Fatalf("CIVerdict = %+v", st.LinkedPR.CIVerdict)
	}
	if !st.LinkedPR.LastCIProgressAt.IsZero() {
		t.Error("CIVerdictObserved touched LastCIProgressAt")
	}
	// A verdict for a head that is not the current one is dropped.
	if _, c, _ := s.Apply(CIVerdictObserved{Repo: testRepo, Number: 1, Verdict: CIVerdictRecord{Verdict: "green", HeadSHA: "zzz", ObservedAt: at}}); len(c) != 0 {
		t.Error("verdict for a stale head was applied")
	}
	// A genuine head change clears it.
	applyExpect(t, s, PRHeadSHAUpdated{Repo: testRepo, Number: 1, SHA: "bbb"}, LinkedPRChanged)
	st = getItem(t, s, testRepo, 1)
	if st.LinkedPR.CIVerdict.Verdict != "" || st.LinkedPR.CIVerdict.HeadSHA != "" {
		t.Errorf("verdict survived a head change: %+v", st.LinkedPR.CIVerdict)
	}
}

func TestMergedOrClosedPRClearsVerdict(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	seedLinkedPR(t, s, "aaa")
	at := time.Now()
	applyExpect(t, s, CIVerdictObserved{Repo: testRepo, Number: 1, Verdict: CIVerdictRecord{Verdict: "red", HeadSHA: "aaa", ObservedAt: at}}, PRFactsChanged)
	applyExpect(t, s, PRFactsObserved{Repo: testRepo, Number: 1, PRNumber: 7, Facts: PRFacts{State: "closed", Merged: true, HeadSHA: "aaa", ObservedAt: at}}, PRFactsChanged)
	if v := getItem(t, s, testRepo, 1).LinkedPR.CIVerdict; v.Verdict != "" {
		t.Errorf("verdict kept for a merged PR: %+v", v)
	}
}

func TestPRFactsSnapshotsAreIsolated(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	seedLinkedPR(t, s, "aaa")
	yes := true
	at := time.Now()
	applyExpect(t, s, PRFactsObserved{Repo: testRepo, Number: 1, PRNumber: 7, Facts: PRFacts{State: "open", Mergeable: &yes, HeadSHA: "aaa", ObservedAt: at}}, PRFactsChanged)
	applyExpect(t, s, CIVerdictObserved{Repo: testRepo, Number: 1, Verdict: CIVerdictRecord{Verdict: "red", HeadSHA: "aaa", Failing: []string{"a", "b"}, ObservedAt: at}}, PRFactsChanged)
	snap, _ := s.Get(testRepo, 1)
	lpr := snap.LinkedPR()
	*lpr.Facts.Mergeable = false
	lpr.CIVerdict.Failing[0] = "mutated"
	again := getItem(t, s, testRepo, 1)
	if !*again.LinkedPR.Facts.Mergeable || again.LinkedPR.CIVerdict.Failing[0] != "a" {
		t.Errorf("snapshot mutation leaked into the store: %+v %+v", again.LinkedPR.Facts, again.LinkedPR.CIVerdict)
	}
}
