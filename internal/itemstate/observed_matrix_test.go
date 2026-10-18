package itemstate

import (
	"sync"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// The per-fact × per-producer-pair matrix of the observed-at rule (#2348, FR-015,
// SC-001): for every fact and every ordered pair of producers, an older
// observation applied after a newer one leaves the Store unchanged, and the
// reverse order applies both.

// producer applies value (true = the newer fact's value, false = the older one)
// observed at the authority time `at`. engine producers ignore at (they write at
// the current time) and so cannot play the older role in the reverse order.
type producer struct {
	name   string
	engine bool
	apply  func(s *Store, newer bool, at time.Time)
}

type factCase struct {
	name      string
	setup     func(s *Store)
	producers []producer
	// isNew reports that the Store holds the newer fact's value.
	isNew func(it ItemState) bool
}

const (
	orderRepo = testRepo
	orderPR   = 5
)

func orderedStore(t *testing.T) *Store {
	t.Helper()
	s := NewStore(nil)
	s.SetEngineWriteGraceForTest(testGrace)
	pi := testProjectItem(orderRepo, 1)
	pi.ItemID = "PVTI_1"
	pi.Labels = nil
	pi.Status = "Plan"
	pi.LinkedPRNumber = orderPR
	pi.Milestone, pi.MilestoneKnown = nil, true
	s.Apply(IssueOpened{Item: pi})
	s.Apply(PRHeadSHAUpdated{Repo: orderRepo, Number: 1, LinkedPRNum: orderPR, SHA: "sha-init"})
	return s
}

// fetchStart is the time a listing or live read must start for its authority time
// (start minus the grace) to be `at`.
func fetchStart(at time.Time) time.Time { return at.Add(testGrace) }

func statusCase() factCase {
	return factCase{
		name: "status",
		producers: []producer{
			{name: "listing", apply: func(s *Store, newer bool, at time.Time) {
				pi := listing(pick(newer, "Review", "Plan"))
				pi.LabelsAbsent = true
				reconcile(s, pi, fetchStart(at))
			}},
			{name: "probe", apply: func(s *Store, newer bool, at time.Time) {
				s.Apply(ProbeBoardItemUpdated{Repo: orderRepo, Number: 1, FetchedAt: fetchStart(at),
					Item: gh.BoardProbeItem{Status: pick(newer, "Review", "Plan"), EffectiveUpdatedAt: time.Unix(1000, 0)}})
			}},
			{name: "webhook", apply: func(s *Store, newer bool, at time.Time) {
				s.Apply(ProjectV2ItemEdited{ItemID: "PVTI_1", NewStatus: pick(newer, "Review", "Plan"), Observed: at, At: at})
			}},
			{name: "engine", engine: true, apply: func(s *Store, newer bool, _ time.Time) {
				s.Apply(LocalStatusUpdated{Repo: orderRepo, Number: 1, NewStatus: pick(newer, "Review", "Plan")})
			}},
		},
		isNew: func(it ItemState) bool { return it.Status == "Review" },
	}
}

func closedCase() factCase {
	return factCase{
		name: "closed",
		producers: []producer{
			{name: "listing", apply: func(s *Store, newer bool, at time.Time) {
				pi := listing("Plan")
				pi.LabelsAbsent = true
				pi.IsClosed = newer
				reconcile(s, pi, fetchStart(at))
			}},
			{name: "probe", apply: func(s *Store, newer bool, at time.Time) {
				s.Apply(ProbeBoardItemUpdated{Repo: orderRepo, Number: 1, FetchedAt: fetchStart(at),
					Item: gh.BoardProbeItem{Status: "Plan", IsClosed: newer, EffectiveUpdatedAt: time.Unix(1000, 0)}})
			}},
			{name: "deepfetch", apply: func(s *Store, newer bool, at time.Time) {
				fresh := testProjectItem(orderRepo, 1)
				fresh.LabelsAbsent = true
				fresh.Status = "Plan"
				fresh.IsClosed = newer
				fresh.DeepFetchLive, fresh.DeepFetchStartedAt = true, fetchStart(at)
				s.Apply(ItemDeepFetched{Repo: orderRepo, Number: 1, FreshState: fresh})
			}},
			{name: "webhook", apply: func(s *Store, newer bool, at time.Time) {
				if newer {
					s.Apply(IssueClosed{Repo: orderRepo, Number: 1, At: at})
				} else {
					s.Apply(IssueReopened{Repo: orderRepo, Number: 1, At: at})
				}
			}},
			// The engine only ever closes: it is the newer value or nothing.
			{name: "engine", engine: true, apply: func(s *Store, newer bool, _ time.Time) {
				if newer {
					s.Apply(LocalIssueClosed{Repo: orderRepo, Number: 1})
				}
			}},
		},
		isNew: func(it ItemState) bool { return it.IsClosed },
	}
}

func labelCase() factCase {
	const l = "fabrik:editing"
	return factCase{
		name: "label",
		producers: []producer{
			{name: "listing", apply: func(s *Store, newer bool, at time.Time) {
				pi := listing("Plan")
				if newer {
					pi.Labels = []string{l}
				} else {
					pi.Labels = nil
				}
				reconcile(s, pi, fetchStart(at))
			}},
			{name: "deepfetch", apply: func(s *Store, newer bool, at time.Time) {
				fresh := testProjectItem(orderRepo, 1)
				fresh.Status = "Plan"
				fresh.Labels = nil
				if newer {
					fresh.Labels = []string{l}
				}
				fresh.DeepFetchLive, fresh.DeepFetchStartedAt = true, fetchStart(at)
				s.Apply(ItemDeepFetched{Repo: orderRepo, Number: 1, FreshState: fresh})
			}},
			{name: "webhook", apply: func(s *Store, newer bool, at time.Time) {
				if newer {
					s.Apply(IssueLabeled{Repo: orderRepo, Number: 1, Label: l, At: at})
				} else {
					s.Apply(IssueUnlabeled{Repo: orderRepo, Number: 1, Label: l, At: at})
				}
			}},
			{name: "engine", engine: true, apply: func(s *Store, newer bool, _ time.Time) {
				if newer {
					s.Apply(LocalLabelAdded{Repo: orderRepo, Number: 1, Label: l})
				} else {
					s.Apply(LocalLabelRemoved{Repo: orderRepo, Number: 1, Label: l})
				}
			}},
		},
		isNew: func(it ItemState) bool { return hasLabel(it.Labels, l) },
	}
}

func headCase() factCase {
	head := func(newer bool) string { return pick(newer, "sha-new", "sha-old") }
	fetch := func(name string) producer {
		return producer{name: name, apply: func(s *Store, newer bool, at time.Time) {
			s.Apply(PRHeadSHAUpdated{Repo: orderRepo, Number: 1, LinkedPRNum: orderPR, SHA: head(newer), At: fetchStart(at), AtIsFetchStart: true})
		}}
	}
	return factCase{
		name: "head",
		producers: []producer{
			fetch("listing"), fetch("probe"), fetch("deepfetch"),
			{name: "webhook", apply: func(s *Store, newer bool, at time.Time) {
				s.Apply(PRHeadSHAUpdated{Repo: orderRepo, Number: 1, LinkedPRNum: orderPR, SHA: head(newer), At: at})
			}},
		},
		isNew: func(it ItemState) bool { return it.LinkedPR != nil && it.LinkedPR.HeadSHA == "sha-new" },
	}
}

func linkedPRCase() factCase {
	title := func(newer bool) string { return pick(newer, "new title", "old title") }
	return factCase{
		name: "linkedpr",
		producers: []producer{
			{name: "pr-read", apply: func(s *Store, newer bool, at time.Time) {
				s.Apply(PRDetailsUpdated{Repo: orderRepo, Number: 1, PRNumber: orderPR, Title: title(newer), State: "open", At: fetchStart(at), AtIsFetchStart: true})
			}},
			{name: "webhook", apply: func(s *Store, newer bool, at time.Time) {
				s.Apply(PRDetailsUpdated{Repo: orderRepo, Number: 1, PRNumber: orderPR, Title: title(newer), State: "open", At: at})
			}},
		},
		isNew: func(it ItemState) bool { return it.LinkedPR != nil && it.LinkedPR.Title == "new title" },
	}
}

func reviewsCase() factCase {
	review := func(newer bool, at time.Time) gh.PRReview {
		return gh.PRReview{Author: "alice", DatabaseID: pickInt(newer, 2, 1), State: pick(newer, "APPROVED", "CHANGES_REQUESTED"), SubmittedAt: at}
	}
	return factCase{
		name: "reviews",
		producers: []producer{
			{name: "deepfetch", apply: func(s *Store, newer bool, at time.Time) {
				fresh := testProjectItem(orderRepo, 1)
				fresh.Status = "Plan"
				fresh.LabelsAbsent = true
				fresh.LinkedPRNumber = orderPR
				fresh.LinkedPRReviews = []gh.PRReview{review(newer, at)}
				fresh.DeepFetchLive, fresh.DeepFetchStartedAt = true, fetchStart(at)
				s.Apply(ItemDeepFetched{Repo: orderRepo, Number: 1, FreshState: fresh})
			}},
			{name: "webhook", apply: func(s *Store, newer bool, at time.Time) {
				s.Apply(PRReviewSubmitted{Repo: orderRepo, Number: 1, Review: review(newer, at), At: at})
			}},
		},
		isNew: func(it ItemState) bool {
			return it.LinkedPR != nil && len(it.LinkedPR.Reviews) == 1 && it.LinkedPR.Reviews[0].State == "APPROVED"
		},
	}
}

func milestoneCase() factCase {
	ms := func(newer bool) *gh.Milestone {
		return &gh.Milestone{Title: pick(newer, "v2", "v1"), Number: pickInt(newer, 2, 1)}
	}
	return factCase{
		name: "milestone",
		producers: []producer{
			{name: "listing", apply: func(s *Store, newer bool, at time.Time) {
				pi := listing("Plan")
				pi.LabelsAbsent = true
				pi.Milestone, pi.MilestoneKnown = ms(newer), true
				reconcile(s, pi, fetchStart(at))
			}},
			{name: "deepfetch", apply: func(s *Store, newer bool, at time.Time) {
				fresh := testProjectItem(orderRepo, 1)
				fresh.Status = "Plan"
				fresh.LabelsAbsent = true
				fresh.Milestone, fresh.MilestoneKnown = ms(newer), true
				fresh.DeepFetchLive, fresh.DeepFetchStartedAt = true, fetchStart(at)
				s.Apply(ItemDeepFetched{Repo: orderRepo, Number: 1, FreshState: fresh})
			}},
			{name: "webhook", apply: func(s *Store, newer bool, at time.Time) {
				s.Apply(IssueMilestoneUpdated{Repo: orderRepo, Number: 1, Milestone: ms(newer), At: at})
			}},
		},
		isNew: func(it ItemState) bool { return it.Milestone != nil && it.Milestone.Title == "v2" },
	}
}

func blockedByCase() factCase {
	dep := gh.Dependency{Repo: orderRepo, Number: 9}
	return factCase{
		name: "blockedBy",
		producers: []producer{
			{name: "deepfetch", apply: func(s *Store, newer bool, at time.Time) {
				fresh := testProjectItem(orderRepo, 1)
				fresh.Status = "Plan"
				fresh.LabelsAbsent = true
				if newer {
					fresh.BlockedBy = []gh.Dependency{dep}
				}
				fresh.DeepFetchLive, fresh.DeepFetchStartedAt = true, fetchStart(at)
				s.Apply(ItemDeepFetched{Repo: orderRepo, Number: 1, FreshState: fresh})
			}},
			// The edge write is the newer value or nothing.
			{name: "engine", engine: true, apply: func(s *Store, newer bool, _ time.Time) {
				if newer {
					s.Apply(BlockedByEdgeAdded{Repo: orderRepo, Number: 1, Dep: dep})
				}
			}},
		},
		isNew: func(it ItemState) bool { return len(it.BlockedBy) == 1 },
	}
}

func pick(newer bool, a, b string) string {
	if newer {
		return a
	}
	return b
}

func pickInt(newer bool, a, b int) int {
	if newer {
		return a
	}
	return b
}

func TestObservedOrdering_Matrix(t *testing.T) {
	base := time.Now()
	for _, fc := range []factCase{statusCase(), closedCase(), labelCase(), headCase(), linkedPRCase(), reviewsCase(), milestoneCase(), blockedByCase()} {
		fc := fc
		for _, po := range fc.producers { // the producer of the older observation
			for _, pn := range fc.producers { // the producer of the newer observation
				if po.engine && pn.engine {
					continue
				}
				po, pn := po, pn
				// An engine producer writes at the current time, whichever role it plays.
				oldAt, newAt := base.Add(-10*time.Minute), base.Add(10*time.Minute)
				if po.engine {
					oldAt = base
				}
				if pn.engine {
					newAt = base
				}
				t.Run(fc.name+"/older="+po.name+"/newer="+pn.name, func(t *testing.T) {
					// The older observation arrives after the newer one: dropped.
					if !po.engine { // an engine write always applies, whatever came before it
						s := orderedStore(t)
						pn.apply(s, true, newAt)
						seen := collectChanges(s)
						po.apply(s, false, oldAt)
						if !fc.isNew(stateOf(t, s)) {
							t.Fatalf("older %s overwrote newer %s", po.name, pn.name)
						}
						// A deep fetch always records its own completion (DeepFetchChanged);
						// the dropped fact itself must be silent.
						if got := seen(); len(got) != 0 && po.name != "deepfetch" {
							t.Fatalf("dropped write emitted changes: %+v", got)
						}
					}
					// The reverse order applies both.
					s := orderedStore(t)
					po.apply(s, false, oldAt)
					pn.apply(s, true, newAt)
					if !fc.isNew(stateOf(t, s)) {
						t.Fatalf("newer %s did not apply after older %s", pn.name, po.name)
					}
				})
			}
		}
	}
}

// An observation at exactly the recorded time applies (a tie is not older).
func TestObservedOrdering_TieApplies(t *testing.T) {
	s := orderedStore(t)
	at := time.Now()
	s.Apply(ProjectV2ItemEdited{ItemID: "PVTI_1", NewStatus: "Review", Observed: at, At: at})
	s.Apply(ProjectV2ItemEdited{ItemID: "PVTI_1", NewStatus: "Validate", Observed: at, At: at})
	if got := stateOf(t, s).Status; got != "Validate" {
		t.Fatalf("tie did not apply: Status = %q", got)
	}
}

// A zero observation time carries no ordering information: it applies and records
// nothing, so the mutations existing code and tests build bare keep working.
func TestObservedOrdering_ZeroTimeAppliesAndRecordsNothing(t *testing.T) {
	s := orderedStore(t)
	s.Apply(IssueLabeled{Repo: orderRepo, Number: 1, Label: "x"})
	s.Apply(IssueUnlabeled{Repo: orderRepo, Number: 1, Label: "x"})
	s.Apply(IssueLabeled{Repo: orderRepo, Number: 1, Label: "x"})
	st := stateOf(t, s)
	if !hasLabel(st.Labels, "x") || len(st.ObservedAt) != 0 {
		t.Fatalf("labels %v, ObservedAt %v", st.Labels, st.ObservedAt)
	}
}

// The first observation of a fact always applies, however old.
func TestObservedOrdering_FirstObservationApplies(t *testing.T) {
	s := orderedStore(t)
	s.Apply(IssueLabeled{Repo: orderRepo, Number: 1, Label: "x", At: time.Unix(1, 0)})
	if !hasLabel(stateOf(t, s).Labels, "x") {
		t.Fatal("a first observation with an old time was dropped")
	}
}

// Ordering is per label: a newer change to one label never blocks another.
func TestObservedOrdering_PerLabelGranularity(t *testing.T) {
	s := orderedStore(t)
	now := time.Now()
	s.Apply(IssueLabeled{Repo: orderRepo, Number: 1, Label: "a", At: now.Add(time.Minute)})
	s.Apply(IssueLabeled{Repo: orderRepo, Number: 1, Label: "b", At: now.Add(-time.Hour)})
	if got := stateOf(t, s).Labels; !hasLabel(got, "a") || !hasLabel(got, "b") {
		t.Fatalf("labels = %v", got)
	}
}

// A late webhook echo of an earlier engine write must not re-add a label the
// engine has since removed, and must not be shielded afterwards (S2, S4).
func TestObservedOrdering_LateEchoDoesNotResurrectRemovedLabel(t *testing.T) {
	s := orderedStore(t)
	// GitHub times have one-second resolution, so a late event not flagged as an
	// echo is older by at least a second; a same-second echo is the echo rule's.
	t0 := time.Now().Add(-2 * time.Second).Truncate(time.Second)
	s.Apply(LocalLabelAdded{Repo: orderRepo, Number: 1, Label: "fabrik:editing"})
	s.Apply(LocalLabelRemoved{Repo: orderRepo, Number: 1, Label: "fabrik:editing"})
	// The echo of the add arrives late, flagged as an engine echo and not.
	s.Apply(IssueLabeled{Repo: orderRepo, Number: 1, Label: "fabrik:editing", At: time.Now(), EchoOfEngine: true})
	s.Apply(IssueLabeled{Repo: orderRepo, Number: 1, Label: "fabrik:editing", At: t0})
	if hasLabel(stateOf(t, s).Labels, "fabrik:editing") {
		t.Fatal("late echo resurrected the removed label")
	}
	// A listing fetched after the grace sees the label absent and keeps it so.
	reconcile(s, listing("Plan"), time.Now().Add(2*testGrace))
	if hasLabel(stateOf(t, s).Labels, "fabrik:editing") {
		t.Fatal("label present after a post-grace listing")
	}
}

// A replayed head observation does not regress the head, check runs or verdict.
func TestObservedOrdering_ReplayedHeadKeepsCIState(t *testing.T) {
	s := orderedStore(t)
	t1 := time.Now()
	s.Apply(PRHeadSHAUpdated{Repo: orderRepo, Number: 1, LinkedPRNum: orderPR, SHA: "sha-a", At: t1})
	s.Apply(PRHeadSHAUpdated{Repo: orderRepo, Number: 1, LinkedPRNum: orderPR, SHA: "sha-b", At: t1.Add(time.Minute)})
	s.Apply(CheckRunCompleted{SHA: "sha-b", Run: gh.CheckRun{ID: 1, Name: "ci", Status: "completed", Conclusion: "success"}})
	s.Apply(PRHeadSHAUpdated{Repo: orderRepo, Number: 1, LinkedPRNum: orderPR, SHA: "sha-a", At: t1}) // replay
	st := stateOf(t, s)
	if st.LinkedPR.HeadSHA != "sha-b" || len(st.LinkedPR.CheckRuns) != 1 {
		t.Fatalf("head %q, check runs %v", st.LinkedPR.HeadSHA, st.LinkedPR.CheckRuns)
	}
}

// A link-only head write (empty SHA) never wipes the head or its check runs.
func TestObservedOrdering_LinkOnlyHeadWriteKeepsHead(t *testing.T) {
	s := orderedStore(t)
	s.Apply(PRHeadSHAUpdated{Repo: orderRepo, Number: 1, LinkedPRNum: orderPR, SHA: "sha-a", At: time.Now()})
	s.Apply(CheckRunCompleted{SHA: "sha-a", Run: gh.CheckRun{ID: 1, Name: "ci", Status: "completed", Conclusion: "success"}})
	s.Apply(PRHeadSHAUpdated{Repo: orderRepo, Number: 1, LinkedPRNum: orderPR})
	st := stateOf(t, s)
	if st.LinkedPR.HeadSHA != "sha-a" || len(st.LinkedPR.CheckRuns) != 1 {
		t.Fatalf("head %q, check runs %v", st.LinkedPR.HeadSHA, st.LinkedPR.CheckRuns)
	}
}

// A partial payload writes only the fields it carries.
func TestObservedOrdering_PartialOpenedKeepsPopulatedItem(t *testing.T) {
	s := orderedStore(t)
	before := stateOf(t, s)
	for i := 0; i < 2; i++ { // the second is a duplicate delivery
		s.Apply(IssueOpened{Partial: true, At: time.Now(), Item: gh.ProjectItem{Repo: orderRepo, Number: 1, ID: "I_1", Title: "stale title"}})
	}
	after := stateOf(t, s)
	if after.Status != before.Status || after.ItemID != before.ItemID || after.Title != before.Title ||
		!after.UpdatedAt.Equal(before.UpdatedAt) || len(after.Comments) != len(before.Comments) {
		t.Fatalf("partial payload overwrote a populated item: %+v", after)
	}
}

func TestObservedOrdering_PartialOpenedPopulatesNewItem(t *testing.T) {
	s := NewStore(nil)
	s.Apply(IssueOpened{Partial: true, At: time.Now(), Item: gh.ProjectItem{Repo: orderRepo, Number: 1, ID: "I_1", Title: "T", Body: "B", Labels: []string{"bug"}}})
	st := stateOf(t, s)
	if st.Title != "T" || !hasLabel(st.Labels, "bug") || st.State != "open" {
		t.Fatalf("new item not populated: %+v", st)
	}
}

// A payload label that a newer observation removed is not re-added.
func TestObservedOrdering_PartialOpenedRespectsNewerLabelRemoval(t *testing.T) {
	s := orderedStore(t)
	t0 := time.Now()
	s.Apply(IssueLabeled{Repo: orderRepo, Number: 1, Label: "x", At: t0.Add(-time.Minute)})
	s.Apply(IssueUnlabeled{Repo: orderRepo, Number: 1, Label: "x", At: t0})
	s.Apply(IssueOpened{Partial: true, At: t0.Add(-2 * time.Minute), Item: gh.ProjectItem{Repo: orderRepo, Number: 1, Labels: []string{"x"}}})
	if hasLabel(stateOf(t, s).Labels, "x") {
		t.Fatal("older opened payload re-added a removed label")
	}
}

// Reconcile's removal never drops an item added after its listing's fetch began.
func TestObservedOrdering_RemoveIfNotAddedAfter(t *testing.T) {
	s := orderedStore(t)
	fetchStarted := time.Now().Add(-time.Minute)
	s.Apply(IssueOpened{Item: func() gh.ProjectItem { pi := testProjectItem(orderRepo, 2); return pi }()})
	if !s.RemoveIfNotAddedAfter(orderRepo, 2, fetchStarted) {
		t.Fatal("item added after the fetch started was removed")
	}
	if _, ok := s.Peek(orderRepo, 2); !ok {
		t.Fatal("item missing")
	}
	if s.RemoveIfNotAddedAfter(orderRepo, 2, time.Now().Add(time.Minute)) {
		t.Fatal("item added before the fetch started was kept")
	}
	if _, ok := s.Peek(orderRepo, 2); ok {
		t.Fatal("item still present")
	}
	// Zero fetch time: today's unconditional removal.
	s.Apply(IssueOpened{Item: testProjectItem(orderRepo, 3)})
	if s.RemoveIfNotAddedAfter(orderRepo, 3, time.Time{}) {
		t.Fatal("zero fetch time kept the item")
	}
}

// The seam restores the pre-#2348 behaviour.
func TestObservedOrdering_SeamDisablesRule(t *testing.T) {
	s := orderedStore(t)
	s.SetObservedOrderingDisabledForTest(true)
	now := time.Now()
	s.Apply(IssueLabeled{Repo: orderRepo, Number: 1, Label: "x", At: now})
	s.Apply(IssueUnlabeled{Repo: orderRepo, Number: 1, Label: "x", At: now.Add(-time.Hour)})
	s.Apply(IssueLabeled{Repo: orderRepo, Number: 1, Label: "x", At: now.Add(-2 * time.Hour)})
	if !hasLabel(stateOf(t, s).Labels, "x") || len(stateOf(t, s).ObservedAt) != 0 {
		t.Fatal("seam left records or reordered writes")
	}
	s.Apply(IssueOpened{Item: testProjectItem(orderRepo, 4)})
	if s.RemoveIfNotAddedAfter(orderRepo, 4, time.Now().Add(-time.Hour)) {
		t.Fatal("seam kept an item")
	}
}

// Two webhook deliveries handled concurrently end in the state of the newer one,
// whichever finishes first.
func TestObservedOrdering_ConcurrentWebhooks(t *testing.T) {
	for i := 0; i < 50; i++ {
		s := orderedStore(t)
		now := time.Now()
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			s.Apply(IssueLabeled{Repo: orderRepo, Number: 1, Label: "x", At: now.Add(time.Second)})
		}()
		go func() {
			defer wg.Done()
			s.Apply(IssueUnlabeled{Repo: orderRepo, Number: 1, Label: "x", At: now})
		}()
		wg.Wait()
		if !hasLabel(stateOf(t, s).Labels, "x") {
			t.Fatalf("iteration %d: older unlabeled won over newer labeled", i)
		}
	}
}

// Webhook and deep-fetch producers yield the same review list for the same
// reviews (R5), and a superseded review is absent from both.
func TestObservedOrdering_ReviewListsHaveOneShape(t *testing.T) {
	t0 := time.Now()
	old := gh.PRReview{Author: "alice", DatabaseID: 1, State: "CHANGES_REQUESTED", SubmittedAt: t0.Add(-time.Hour)}
	cur := gh.PRReview{Author: "alice", DatabaseID: 2, State: "APPROVED", SubmittedAt: t0}
	bob := gh.PRReview{Author: "bob", DatabaseID: 3, State: "COMMENTED", SubmittedAt: t0}

	web := orderedStore(t)
	for _, r := range []gh.PRReview{old, cur, bob} {
		web.Apply(PRReviewSubmitted{Repo: orderRepo, Number: 1, Review: r})
	}
	deep := orderedStore(t)
	fresh := testProjectItem(orderRepo, 1)
	fresh.LinkedPRNumber = orderPR
	fresh.LinkedPRReviews = []gh.PRReview{cur, bob}
	deep.Apply(ItemDeepFetched{Repo: orderRepo, Number: 1, FreshState: fresh})

	w, d := stateOf(t, web).LinkedPR.Reviews, stateOf(t, deep).LinkedPR.Reviews
	if len(w) != 2 || len(d) != 2 {
		t.Fatalf("webhook %v, deep fetch %v", w, d)
	}
	for i := range w {
		if w[i].Author != d[i].Author || w[i].State != d[i].State {
			t.Fatalf("lists differ: webhook %v, deep fetch %v", w, d)
		}
	}
}

// A dismissed review replaces its earlier entry in place.
func TestObservedOrdering_DismissedReviewUpdatesInPlace(t *testing.T) {
	s := orderedStore(t)
	r := gh.PRReview{Author: "alice", DatabaseID: 1, State: "CHANGES_REQUESTED", SubmittedAt: time.Now()}
	s.Apply(PRReviewSubmitted{Repo: orderRepo, Number: 1, Review: r})
	r.State = "DISMISSED"
	s.Apply(PRReviewSubmitted{Repo: orderRepo, Number: 1, Review: r})
	got := stateOf(t, s).LinkedPR.Reviews
	if len(got) != 1 || got[0].State != "DISMISSED" {
		t.Fatalf("reviews = %v", got)
	}
}

// Reviews are ordered per author: a newer review by one reviewer must not drop a
// delivery that is older but belongs to another, whichever order they arrive in
// (the Pruefer finding on PRReviewSubmitted).
func TestObservedOrdering_ReviewsAreOrderedPerAuthor(t *testing.T) {
	now := time.Now()
	alice := gh.PRReview{Author: "alice", DatabaseID: 1, State: "CHANGES_REQUESTED", SubmittedAt: now}
	bob := gh.PRReview{Author: "bob", DatabaseID: 2, State: "APPROVED", SubmittedAt: now.Add(time.Minute)}

	s := orderedStore(t)
	s.Apply(PRReviewSubmitted{Repo: orderRepo, Number: 1, Review: bob, At: bob.SubmittedAt})
	s.Apply(PRReviewSubmitted{Repo: orderRepo, Number: 1, Review: alice, At: alice.SubmittedAt})
	if got := stateOf(t, s).LinkedPR.Reviews; len(got) != 2 {
		t.Fatalf("alice's older review was dropped by bob's newer one: %+v", got)
	}

	// A dismissal of alice's review, ordered by receipt, clears her verdict even
	// though bob reviewed after she did.
	dismissed := alice
	dismissed.State = "DISMISSED"
	s.Apply(PRReviewSubmitted{Repo: orderRepo, Number: 1, Review: dismissed, At: time.Now()})
	for _, r := range stateOf(t, s).LinkedPR.Reviews {
		if r.Author == "alice" && r.State != "DISMISSED" {
			t.Fatalf("dismissal not applied: %+v", r)
		}
	}

	// The same author's older review is still dropped behind her newer one.
	s2 := orderedStore(t)
	newer := gh.PRReview{Author: "alice", DatabaseID: 3, State: "APPROVED", SubmittedAt: now.Add(time.Minute)}
	s2.Apply(PRReviewSubmitted{Repo: orderRepo, Number: 1, Review: newer, At: newer.SubmittedAt})
	s2.Apply(PRReviewSubmitted{Repo: orderRepo, Number: 1, Review: alice, At: alice.SubmittedAt})
	if got := stateOf(t, s2).LinkedPR.Reviews; len(got) != 1 || got[0].State != "APPROVED" {
		t.Fatalf("older review of the same author overwrote the newer one: %+v", got)
	}
}

// A live deep fetch issued before a webhook review landed must not wipe or undo
// that author's review, while still applying the other authors' reads.
func TestObservedOrdering_DeepFetchKeepsNewerWebhookReview(t *testing.T) {
	s := orderedStore(t)
	now := time.Now()
	started := now.Add(-time.Minute) // the fetch was issued before the webhook
	bob := gh.PRReview{Author: "bob", DatabaseID: 2, State: "APPROVED", SubmittedAt: now}
	s.Apply(PRReviewSubmitted{Repo: orderRepo, Number: 1, Review: bob, At: now.Add(2 * testGrace)})

	fresh := testProjectItem(orderRepo, 1)
	fresh.Status = "Plan"
	fresh.LabelsAbsent = true
	fresh.LinkedPRNumber = orderPR
	fresh.LinkedPRReviews = []gh.PRReview{{Author: "alice", DatabaseID: 1, State: "COMMENTED", SubmittedAt: started}}
	fresh.DeepFetchLive, fresh.DeepFetchStartedAt = true, started
	s.Apply(ItemDeepFetched{Repo: orderRepo, Number: 1, FreshState: fresh})

	got := stateOf(t, s).LinkedPR.Reviews
	if len(got) != 2 {
		t.Fatalf("want alice (fetch) and bob (webhook), got %+v", got)
	}
}

// A head write that is FillOnly sets the head only while the Store has none; with
// one recorded it links the PR and keeps the head and its check runs.
func TestObservedOrdering_FillOnlyHeadKeepsRecordedHead(t *testing.T) {
	s := orderedStore(t)
	s.Apply(PRHeadSHAUpdated{Repo: orderRepo, Number: 1, LinkedPRNum: orderPR, SHA: "sha-b", At: time.Now()})
	s.Apply(CheckRunCompleted{SHA: "sha-b", Run: gh.CheckRun{ID: 1, Name: "ci", Status: "completed", Conclusion: "success"}})
	s.Apply(PRHeadSHAUpdated{Repo: orderRepo, Number: 1, LinkedPRNum: orderPR, SHA: "sha-a", FillOnly: true})
	st := stateOf(t, s)
	if st.LinkedPR.HeadSHA != "sha-b" || len(st.LinkedPR.CheckRuns) != 1 {
		t.Fatalf("head %q, check runs %v", st.LinkedPR.HeadSHA, st.LinkedPR.CheckRuns)
	}
}

// A webhook's GitHub time has one-second resolution and an engine write is
// recorded with sub-second precision: a human change in the same second as an
// engine write must apply, not be dropped as older. One second earlier is still
// older.
func TestObservedOrdering_WebhookSameSecondAsEngineWriteApplies(t *testing.T) {
	s := orderedStore(t)
	s.Apply(LocalLabelAdded{Repo: orderRepo, Number: 1, Label: "x"})
	s.Apply(LocalLabelRemoved{Repo: orderRepo, Number: 1, Label: "x"})
	rec := stateOf(t, s).EngineWriteAt[engineWriteLabelKey("x")]
	if rec.Equal(rec.Truncate(time.Second)) {
		t.Skip("engine write landed exactly on a second boundary")
	}
	s.Apply(IssueLabeled{Repo: orderRepo, Number: 1, Label: "x", At: rec.Truncate(time.Second)})
	if !hasLabel(stateOf(t, s).Labels, "x") {
		t.Fatal("a webhook in the same second as an engine write was dropped")
	}
	s.Apply(IssueUnlabeled{Repo: orderRepo, Number: 1, Label: "x", At: rec.Truncate(time.Second).Add(-time.Second)})
	if !hasLabel(stateOf(t, s).Labels, "x") {
		t.Fatal("a webhook a second older than the record was applied")
	}
}
