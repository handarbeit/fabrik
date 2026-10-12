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

// An engine without the board probe never records a head outside a gate: the
// verdict is then keyed to the head the facts were read for, and is dropped
// when the engine records a different head later (#2122).
func TestCIVerdictUsesTheFactsHeadUntilTheEngineRecordsOne(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	applyExpect(t, s, PRDetailsUpdated{Repo: testRepo, Number: 1, PRNumber: 7}, LinkedPRChanged|PRStateChanged)
	at := time.Now()
	if _, _, err := s.Apply(PRFactsObserved{Repo: testRepo, Number: 1, PRNumber: 7, Facts: PRFacts{State: "open", HeadSHA: "aaa", ObservedAt: at}}); err != nil {
		t.Fatal(err)
	}
	if got := getItem(t, s, testRepo, 1).LinkedPR.ObservedHead(); got != "aaa" {
		t.Fatalf("ObservedHead = %q, want the facts head", got)
	}
	if _, _, err := s.Apply(CIVerdictObserved{Repo: testRepo, Number: 1, Verdict: CIVerdictRecord{Verdict: "red", HeadSHA: "aaa", ObservedAt: at}}); err != nil {
		t.Fatal(err)
	}
	if v := getItem(t, s, testRepo, 1).LinkedPR.CIVerdict; v.Verdict != "red" {
		t.Fatalf("verdict not stored against the facts head: %+v", v)
	}
	if _, _, err := s.Apply(PRHeadSHAUpdated{Repo: testRepo, Number: 1, LinkedPRNum: 7, SHA: "bbb"}); err != nil {
		t.Fatal(err)
	}
	lpr := getItem(t, s, testRepo, 1).LinkedPR
	if lpr.CIVerdict.Verdict != "" || lpr.ObservedHead() != "bbb" {
		t.Errorf("a verdict for another head survived the engine recording one: %+v", lpr)
	}
}

// When the engine-recorded head and the facts' head are both known and differ
// there is no agreed head: a verdict for either is dropped, none is accepted,
// and one is accepted again once they agree (#2122).
func TestCIVerdictNeedsAnAgreedHead(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	applyExpect(t, s, PRDetailsUpdated{Repo: testRepo, Number: 1, PRNumber: 7}, LinkedPRChanged|PRStateChanged)
	at := time.Now()
	mustApply := func(m Mutation) {
		t.Helper()
		if _, _, err := s.Apply(m); err != nil {
			t.Fatal(err)
		}
	}
	mustApply(PRHeadSHAUpdated{Repo: testRepo, Number: 1, LinkedPRNum: 7, SHA: "aaa"})
	mustApply(PRFactsObserved{Repo: testRepo, Number: 1, PRNumber: 7, Facts: PRFacts{State: "open", HeadSHA: "aaa", ObservedAt: at}})
	mustApply(CIVerdictObserved{Repo: testRepo, Number: 1, Verdict: CIVerdictRecord{Verdict: "green", HeadSHA: "aaa", ObservedAt: at}})
	if v := getItem(t, s, testRepo, 1).LinkedPR.CIVerdict; v.Verdict != "green" {
		t.Fatalf("precondition: verdict = %+v", v)
	}

	// Facts now name head B while the engine still records A.
	mustApply(PRFactsObserved{Repo: testRepo, Number: 1, PRNumber: 7, Facts: PRFacts{State: "open", HeadSHA: "bbb", ObservedAt: at.Add(time.Second)}})
	lpr := getItem(t, s, testRepo, 1).LinkedPR
	if lpr.AgreedHead() != "" || lpr.CIVerdict.Verdict != "" {
		t.Errorf("disagreeing heads: agreed=%q verdict=%+v, want none", lpr.AgreedHead(), lpr.CIVerdict)
	}
	for _, head := range []string{"aaa", "bbb"} {
		mustApply(CIVerdictObserved{Repo: testRepo, Number: 1, Verdict: CIVerdictRecord{Verdict: "green", HeadSHA: head, ObservedAt: at}})
		if v := getItem(t, s, testRepo, 1).LinkedPR.CIVerdict; v.Verdict != "" {
			t.Errorf("a verdict for %s was accepted while the heads disagree: %+v", head, v)
		}
	}

	// The engine learns head B: they agree, and a verdict for B is accepted.
	mustApply(PRHeadSHAUpdated{Repo: testRepo, Number: 1, LinkedPRNum: 7, SHA: "bbb"})
	mustApply(CIVerdictObserved{Repo: testRepo, Number: 1, Verdict: CIVerdictRecord{Verdict: "red", HeadSHA: "bbb", ObservedAt: at}})
	if v := getItem(t, s, testRepo, 1).LinkedPR.CIVerdict; v.Verdict != "red" {
		t.Errorf("verdict for the agreed head not stored: %+v", v)
	}
}
