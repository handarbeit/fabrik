package itemstate

import (
	"sync"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

const testGrace = time.Minute

// engineWriteStore returns a Store holding issue 1 with labels, status Implement.
func engineWriteStore(t *testing.T, labels ...string) *Store {
	t.Helper()
	s := NewStore(nil)
	s.SetEngineWriteGraceForTest(testGrace)
	pi := testProjectItem(testRepo, 1)
	pi.Labels = labels
	s.Apply(IssueOpened{Item: pi})
	return s
}

// listing builds the shallow listing item for issue 1.
func listing(status string, labels ...string) gh.ProjectItem {
	pi := testProjectItem(testRepo, 1)
	pi.Status = status
	pi.Labels = labels
	return pi
}

func labelsOf(t *testing.T, s *Store) []string {
	t.Helper()
	snap, err := s.Get(testRepo, 1)
	if err != nil {
		t.Fatal(err)
	}
	return snap.State().Labels
}

func hasLabel(ls []string, l string) bool {
	for _, x := range ls {
		if x == l {
			return true
		}
	}
	return false
}

func reconcile(s *Store, pi gh.ProjectItem, fetchedAt time.Time) {
	s.Apply(ShallowBoardItemUpdated{Repo: testRepo, Number: 1, Item: pi, FetchedAt: fetchedAt})
}

func TestEngineWrite_AddedLabelSurvivesPreWriteListing(t *testing.T) {
	s := engineWriteStore(t, "bug")
	fetched := time.Now()
	s.Apply(LocalLabelAdded{Repo: testRepo, Number: 1, Label: "stage:Review:complete"})
	seen := collectChanges(s)

	reconcile(s, listing("Implement", "bug"), fetched)

	if !hasLabel(labelsOf(t, s), "stage:Review:complete") {
		t.Fatalf("engine-added label reverted: %v", labelsOf(t, s))
	}
	for _, c := range seen() {
		if len(c.LabelDeltas) != 0 {
			t.Fatalf("kept label produced label deltas: %+v", c)
		}
	}
}

func TestEngineWrite_RemovedLabelSurvivesPreWriteListing(t *testing.T) {
	s := engineWriteStore(t, "bug", "fabrik:awaiting-ci")
	fetched := time.Now()
	s.Apply(LocalLabelRemoved{Repo: testRepo, Number: 1, Label: "fabrik:awaiting-ci"})
	seen := collectChanges(s)

	reconcile(s, listing("Implement", "bug", "fabrik:awaiting-ci"), fetched)

	if hasLabel(labelsOf(t, s), "fabrik:awaiting-ci") {
		t.Fatalf("engine-removed label restored: %v", labelsOf(t, s))
	}
	for _, c := range seen() {
		if len(c.LabelDeltas) != 0 {
			t.Fatalf("kept label produced label deltas: %+v", c)
		}
	}
}

func TestEngineWrite_LaggingListingStartedAfterWriteStillShielded(t *testing.T) {
	s := engineWriteStore(t, "bug")
	s.Apply(LocalLabelAdded{Repo: testRepo, Number: 1, Label: "x"})
	reconcile(s, listing("Implement", "bug"), time.Now().Add(time.Second)) // within grace
	if !hasLabel(labelsOf(t, s), "x") {
		t.Fatalf("label reverted by a listing within the grace margin")
	}
}

func TestEngineWrite_ListingAfterGraceContradictsAndWins(t *testing.T) {
	s := engineWriteStore(t, "bug")
	s.Apply(LocalLabelAdded{Repo: testRepo, Number: 1, Label: "x"})
	// An external actor removed it after the write; the listing was fetched
	// after the grace margin.
	reconcile(s, listing("Implement", "bug"), time.Now().Add(2*testGrace))
	if hasLabel(labelsOf(t, s), "x") {
		t.Fatalf("external removal not applied: %v", labelsOf(t, s))
	}
}

func TestEngineWrite_NeverWrittenLabelStillReconciled(t *testing.T) {
	s := engineWriteStore(t, "bug")
	s.Apply(LocalLabelAdded{Repo: testRepo, Number: 1, Label: "x"})
	fetched := time.Now()
	// "bug" was never written by the engine: a listing without it removes it, and
	// a listing with "y" adds it, even while "x" is shielded.
	reconcile(s, listing("Implement", "y"), fetched)
	got := labelsOf(t, s)
	if hasLabel(got, "bug") || !hasLabel(got, "y") || !hasLabel(got, "x") {
		t.Fatalf("labels = %v, want y and x without bug", got)
	}
}

func TestEngineWrite_ZeroFetchedAtKeepsLegacyBehaviour(t *testing.T) {
	s := engineWriteStore(t, "bug")
	s.Apply(LocalLabelAdded{Repo: testRepo, Number: 1, Label: "x"})
	reconcile(s, listing("Implement", "bug"), time.Time{})
	if hasLabel(labelsOf(t, s), "x") {
		t.Fatalf("zero FetchedAt must not protect: %v", labelsOf(t, s))
	}
}

func TestEngineWrite_ConfirmingListingClearsRecord(t *testing.T) {
	s := engineWriteStore(t, "bug")
	s.Apply(LocalLabelAdded{Repo: testRepo, Number: 1, Label: "x"})
	reconcile(s, listing("Implement", "bug", "x"), time.Now().Add(time.Second))
	snap, _ := s.Get(testRepo, 1)
	if n := len(snap.State().EngineWriteAt); n != 0 {
		t.Fatalf("record not cleared by confirming listing: %v", snap.State().EngineWriteAt)
	}
}

func TestEngineWrite_PreWriteListingKeepsRecord(t *testing.T) {
	s := engineWriteStore(t, "bug")
	fetched := time.Now()
	s.Apply(LocalLabelAdded{Repo: testRepo, Number: 1, Label: "x"})
	reconcile(s, listing("Implement", "bug"), fetched)
	snap, _ := s.Get(testRepo, 1)
	if _, ok := snap.State().EngineWriteAt["label:x"]; !ok {
		t.Fatalf("record dropped by a listing that predates the write")
	}
}

func TestEngineWrite_ExpiredRecordsPruned(t *testing.T) {
	s := engineWriteStore(t, "bug")
	s.SetEngineWriteGraceForTest(10 * time.Millisecond)
	s.Apply(LocalLabelAdded{Repo: testRepo, Number: 1, Label: "x"})
	time.Sleep(30 * time.Millisecond)
	s.Apply(LocalLabelAdded{Repo: testRepo, Number: 1, Label: "y"}) // prunes x
	snap, _ := s.Get(testRepo, 1)
	rec := snap.State().EngineWriteAt
	if _, ok := rec["label:x"]; ok || len(rec) != 1 {
		t.Fatalf("expired record not pruned: %v", rec)
	}
}

func TestEngineWrite_WebhookRemovalAfterWriteRespectedAgainstStaleListing(t *testing.T) {
	s := engineWriteStore(t, "bug")
	fetched := time.Now()
	s.Apply(LocalLabelAdded{Repo: testRepo, Number: 1, Label: "x"})
	s.Apply(IssueUnlabeled{Repo: testRepo, Number: 1, Label: "x", Sender: "alice"})
	// The stale listing still shows x: the cache (local presence) says absent.
	reconcile(s, listing("Implement", "bug", "x"), fetched)
	if hasLabel(labelsOf(t, s), "x") {
		t.Fatalf("stale listing re-added a label removed by webhook: %v", labelsOf(t, s))
	}
}

func TestEngineWrite_OtherLabelsOnSameItemNotShielded(t *testing.T) {
	s := engineWriteStore(t, "bug", "other")
	fetched := time.Now()
	s.Apply(LocalLabelAdded{Repo: testRepo, Number: 1, Label: "x"})
	reconcile(s, listing("Implement", "bug"), fetched) // "other" removed externally
	got := labelsOf(t, s)
	if hasLabel(got, "other") || !hasLabel(got, "x") {
		t.Fatalf("labels = %v", got)
	}
}

func TestEngineWrite_StatusSurvivesPreWriteListing(t *testing.T) {
	s := engineWriteStore(t)
	fetched := time.Now()
	s.Apply(LocalStatusUpdated{Repo: testRepo, Number: 1, NewStatus: "Review"})
	reconcile(s, listing("Implement"), fetched)
	snap, _ := s.Get(testRepo, 1)
	if snap.State().Status != "Review" {
		t.Fatalf("status reverted to %q", snap.State().Status)
	}
	reconcile(s, listing("Implement"), time.Now().Add(2*testGrace))
	snap, _ = s.Get(testRepo, 1)
	if snap.State().Status != "Implement" {
		t.Fatalf("later contradicting listing not applied: %q", snap.State().Status)
	}
}

func TestEngineWrite_ClosedStateSurvivesPreWriteListing(t *testing.T) {
	s := engineWriteStore(t)
	fetched := time.Now()
	s.Apply(LocalIssueClosed{Repo: testRepo, Number: 1})
	reconcile(s, listing("Implement"), fetched)
	snap, _ := s.Get(testRepo, 1)
	if !snap.State().IsClosed || snap.State().State != "closed" {
		t.Fatalf("closed state reverted: %+v", snap.State().State)
	}
	reconcile(s, listing("Implement"), time.Now().Add(2*testGrace))
	snap, _ = s.Get(testRepo, 1)
	if snap.State().IsClosed {
		t.Fatalf("later listing showing open not applied")
	}
}

func TestEngineWrite_WebhookClosedDoesNotRecord(t *testing.T) {
	s := engineWriteStore(t)
	s.Apply(IssueClosed{Repo: testRepo, Number: 1})
	snap, _ := s.Get(testRepo, 1)
	if len(snap.State().EngineWriteAt) != 0 {
		t.Fatalf("webhook close recorded as engine write")
	}
}

func TestEngineWrite_RedundantWriteIsNotAChange(t *testing.T) {
	s := engineWriteStore(t, "bug")
	seen := collectChanges(s)
	s.Apply(LocalLabelAdded{Repo: testRepo, Number: 1, Label: "bug"})
	if n := len(seen()); n != 0 {
		t.Fatalf("redundant write produced %d changes", n)
	}
	// ...but it is still recorded, so a stale listing cannot revert it.
	snap, _ := s.Get(testRepo, 1)
	if _, ok := snap.State().EngineWriteAt["label:bug"]; !ok {
		t.Fatalf("redundant write not recorded")
	}
}

func TestEngineWrite_NoWakeFlag(t *testing.T) {
	s := engineWriteStore(t, "bug")
	fetched := time.Now()
	s.Apply(LocalLabelAdded{Repo: testRepo, Number: 1, Label: "x"})
	seen := collectChanges(s)
	reconcile(s, listing("Implement", "bug"), fetched)
	for _, c := range seen() {
		if c.Fields&(LabelsChanged|StatusChanged|StateChanged) != 0 {
			t.Fatalf("kept write flagged as a change: %+v", c)
		}
	}
}

func TestEngineWrite_SnapshotCloneIsolated(t *testing.T) {
	s := engineWriteStore(t, "bug")
	s.Apply(LocalLabelAdded{Repo: testRepo, Number: 1, Label: "x"})
	snap, _ := s.Get(testRepo, 1)
	st := snap.State()
	st.EngineWriteAt["label:zzz"] = time.Now()
	again, _ := s.Get(testRepo, 1)
	if _, ok := again.State().EngineWriteAt["label:zzz"]; ok {
		t.Fatalf("snapshot map aliases store state")
	}
}

func TestEngineWrite_EffectiveShallowMatchesApply(t *testing.T) {
	s := engineWriteStore(t, "bug")
	fetched := time.Now()
	s.Apply(LocalLabelAdded{Repo: testRepo, Number: 1, Label: "x"})
	pi := listing("Implement", "bug")

	eff, active, ok := s.EffectiveShallow(pi, fetched)
	if !ok || !active || !hasLabel(eff.Labels, "x") {
		t.Fatalf("eff=%v active=%v ok=%v", eff.Labels, active, ok)
	}
	// After the grace, nothing shields the item.
	eff, active, _ = s.EffectiveShallow(pi, time.Now().Add(2*testGrace))
	if active || hasLabel(eff.Labels, "x") {
		t.Fatalf("eff=%v active=%v after grace", eff.Labels, active)
	}
	if _, _, ok := s.EffectiveShallow(gh.ProjectItem{Repo: testRepo, Number: 99}, fetched); ok {
		t.Fatalf("uncached item reported ok")
	}
}

func TestEngineWrite_ConcurrentWriteAndReconcile(t *testing.T) {
	s := engineWriteStore(t, "bug")
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				s.Apply(LocalLabelAdded{Repo: testRepo, Number: 1, Label: "x"})
				s.Apply(LocalLabelRemoved{Repo: testRepo, Number: 1, Label: "x"})
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				reconcile(s, listing("Implement", "bug"), time.Now())
				s.EffectiveShallow(listing("Implement", "bug"), time.Now())
			}
		}()
	}
	wg.Wait()
}

// ---- Probe apply (#2178) ----

// probeListing builds the probe listing item for issue 1.
func probeListing(status string, closed bool) gh.BoardProbeItem {
	return gh.BoardProbeItem{
		Repo:               testRepo,
		Number:             1,
		Status:             status,
		IsClosed:           closed,
		EffectiveUpdatedAt: time.Unix(1000, 0),
	}
}

// probe applies a probe listing the way the probe loop does: wrapped in
// FromProbe.
func probe(s *Store, pi gh.BoardProbeItem, fetchedAt time.Time) {
	s.Apply(FromProbe{Inner: ProbeBoardItemUpdated{Repo: testRepo, Number: 1, Item: pi, FetchedAt: fetchedAt}})
}

func stateOf(t *testing.T, s *Store) ItemState {
	t.Helper()
	snap, err := s.Get(testRepo, 1)
	if err != nil {
		t.Fatal(err)
	}
	return snap.State()
}

func TestEngineWriteProbe_StatusSurvivesPreWriteListing(t *testing.T) {
	s := engineWriteStore(t)
	fetched := time.Now()
	s.Apply(LocalStatusUpdated{Repo: testRepo, Number: 1, NewStatus: "Review"})
	before := stateOf(t, s)
	seen := collectChanges(s)

	probe(s, probeListing("Implement", false), fetched)

	after := stateOf(t, s)
	if after.Status != "Review" {
		t.Fatalf("status reverted to %q", after.Status)
	}
	if !after.StatusEnteredAt.Equal(before.StatusEnteredAt) {
		t.Fatalf("StatusEnteredAt reset: %v -> %v", before.StatusEnteredAt, after.StatusEnteredAt)
	}
	for _, c := range seen() {
		if c.Fields&(StatusChanged|StateChanged) != 0 {
			t.Fatalf("kept status flagged as a change: %+v", c)
		}
	}
}

func TestEngineWriteProbe_ClosedStateSurvivesPreWriteListing(t *testing.T) {
	s := engineWriteStore(t)
	fetched := time.Now()
	s.Apply(LocalIssueClosed{Repo: testRepo, Number: 1})
	seen := collectChanges(s)

	probe(s, probeListing("Implement", false), fetched)

	if st := stateOf(t, s); !st.IsClosed || st.State != "closed" {
		t.Fatalf("closed state reverted: %+v", st.State)
	}
	for _, c := range seen() {
		if c.Fields&(StatusChanged|StateChanged) != 0 {
			t.Fatalf("kept closed state flagged as a change: %+v", c)
		}
	}
}

func TestEngineWriteProbe_ListingAfterGraceOverwrites(t *testing.T) {
	s := engineWriteStore(t)
	s.Apply(LocalStatusUpdated{Repo: testRepo, Number: 1, NewStatus: "Review"})
	s.Apply(LocalIssueClosed{Repo: testRepo, Number: 1})
	before := stateOf(t, s)
	seen := collectChanges(s)

	probe(s, probeListing("Implement", false), time.Now().Add(2*testGrace))

	after := stateOf(t, s)
	if after.Status != "Implement" || after.IsClosed || after.State != "open" {
		t.Fatalf("post-grace listing not applied: status=%q closed=%v", after.Status, after.IsClosed)
	}
	if !after.StatusEnteredAt.After(before.StatusEnteredAt) {
		t.Fatalf("StatusEnteredAt not reset on a real change")
	}
	var fields ChangeFlags
	for _, c := range seen() {
		fields |= c.Fields
	}
	if fields&StatusChanged == 0 || fields&StateChanged == 0 {
		t.Fatalf("flags = %v, want StatusChanged and StateChanged", fields)
	}
}

func TestEngineWriteProbe_ZeroFetchedAtOverwrites(t *testing.T) {
	s := engineWriteStore(t)
	s.Apply(LocalStatusUpdated{Repo: testRepo, Number: 1, NewStatus: "Review"})
	s.Apply(LocalIssueClosed{Repo: testRepo, Number: 1})
	probe(s, probeListing("Implement", false), time.Time{})
	if st := stateOf(t, s); st.Status != "Implement" || st.IsClosed {
		t.Fatalf("zero FetchedAt must not protect: status=%q closed=%v", st.Status, st.IsClosed)
	}
}

func TestEngineWriteProbe_StatusAndClosedDecidedIndependently(t *testing.T) {
	// A Status write does not shield a genuine external close.
	s := engineWriteStore(t)
	fetched := time.Now()
	s.Apply(LocalStatusUpdated{Repo: testRepo, Number: 1, NewStatus: "Review"})
	probe(s, probeListing("Review", true), fetched)
	if st := stateOf(t, s); st.Status != "Review" || !st.IsClosed {
		t.Fatalf("external close shielded by a status write: status=%q closed=%v", st.Status, st.IsClosed)
	}

	// A close does not shield a genuine external Status move.
	s = engineWriteStore(t)
	fetched = time.Now()
	s.Apply(LocalIssueClosed{Repo: testRepo, Number: 1})
	probe(s, probeListing("Validate", true), fetched)
	if st := stateOf(t, s); st.Status != "Validate" || !st.IsClosed {
		t.Fatalf("external status move shielded by a close: status=%q closed=%v", st.Status, st.IsClosed)
	}
}

func TestEngineWriteProbe_DoesNotClearLabelRecord(t *testing.T) {
	// The probe carries no labels, so "absent from the listing" must not be read
	// as confirming an engine label removal.
	s := engineWriteStore(t, "bug", "fabrik:awaiting-ci")
	fetched := time.Now()
	s.Apply(LocalLabelRemoved{Repo: testRepo, Number: 1, Label: "fabrik:awaiting-ci"})

	probe(s, probeListing("Implement", false), fetched.Add(time.Second))

	if _, ok := stateOf(t, s).EngineWriteAt[engineWriteLabelKey("fabrik:awaiting-ci")]; !ok {
		t.Fatalf("probe dropped the label record")
	}
	// A later lagging reconcile listing is still shielded.
	reconcile(s, listing("Implement", "bug", "fabrik:awaiting-ci"), fetched)
	if hasLabel(labelsOf(t, s), "fabrik:awaiting-ci") {
		t.Fatalf("engine-removed label restored: %v", labelsOf(t, s))
	}
}

func TestEngineWriteProbe_ConfirmingListingClearsStatusAndClosedRecords(t *testing.T) {
	s := engineWriteStore(t)
	s.Apply(LocalStatusUpdated{Repo: testRepo, Number: 1, NewStatus: "Review"})
	s.Apply(LocalIssueClosed{Repo: testRepo, Number: 1})
	probe(s, probeListing("Review", true), time.Now().Add(time.Second))
	if n := len(stateOf(t, s).EngineWriteAt); n != 0 {
		t.Fatalf("records not cleared by confirming probe: %v", stateOf(t, s).EngineWriteAt)
	}
}

func TestEngineWriteProbe_PreWriteListingKeepsRecords(t *testing.T) {
	s := engineWriteStore(t)
	fetched := time.Now()
	s.Apply(LocalStatusUpdated{Repo: testRepo, Number: 1, NewStatus: "Review"})
	probe(s, probeListing("Implement", false), fetched)
	if _, ok := stateOf(t, s).EngineWriteAt[engineWriteStatusKey]; !ok {
		t.Fatalf("record dropped by a listing that predates the write")
	}
}

func TestEngineWriteProbe_NoEventForKeptValues(t *testing.T) {
	s := engineWriteStore(t)
	fetched := time.Now()
	s.Apply(LocalStatusUpdated{Repo: testRepo, Number: 1, NewStatus: "Review"})
	seen := collectChanges(s)
	probe(s, probeListing("Implement", false), fetched)
	for _, c := range seen() {
		if c.Fields&^EngineWriteRecordedChanged != 0 {
			t.Fatalf("kept probe produced a change: %+v", c)
		}
	}
}

func TestEngineWriteProbe_EffectiveProbeMatchesApply(t *testing.T) {
	s := engineWriteStore(t)
	fetched := time.Now()
	s.Apply(LocalStatusUpdated{Repo: testRepo, Number: 1, NewStatus: "Review"})
	s.Apply(LocalIssueClosed{Repo: testRepo, Number: 1})
	pi := probeListing("Implement", false)

	for _, at := range []time.Time{fetched, time.Now().Add(2 * testGrace), {}} {
		status, closed, ok := s.EffectiveProbe(testRepo, 1, pi, at)
		if !ok {
			t.Fatal("cached item reported not ok")
		}
		// Apply on a copy of the same history and compare.
		c := engineWriteStore(t)
		c.Apply(LocalStatusUpdated{Repo: testRepo, Number: 1, NewStatus: "Review"})
		c.Apply(LocalIssueClosed{Repo: testRepo, Number: 1})
		probe(c, pi, at)
		got := stateOf(t, c)
		if status != got.Status || closed != got.IsClosed {
			t.Fatalf("fetchedAt %v: EffectiveProbe=(%q,%v) apply left (%q,%v)", at, status, closed, got.Status, got.IsClosed)
		}
	}
	if _, _, ok := s.EffectiveProbe(testRepo, 99, pi, fetched); ok {
		t.Fatalf("uncached item reported ok")
	}
}
