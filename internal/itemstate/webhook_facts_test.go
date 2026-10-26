package itemstate

import (
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

func seedWithComments(t *testing.T) *Store {
	t.Helper()
	s := newStoreWithItem(t, testRepo, 1)
	applyExpect(t, s, IssueCommentCreated{Repo: testRepo, Number: 1, Comment: gh.Comment{ID: "IC_1", DatabaseID: 11, Body: "old", Author: "a", Reactions: []gh.ReactionGroup{{Content: "ROCKET"}}}}, CommentsChanged)
	applyExpect(t, s, IssueCommentCreated{Repo: testRepo, Number: 1, Comment: gh.Comment{ID: "IC_2", DatabaseID: 12, Body: "other"}}, CommentsChanged)
	return s
}

func TestIssueCommentEdited_ReplacesBodyOnly(t *testing.T) {
	s := seedWithComments(t)
	at := time.Now()
	applyExpect(t, s, IssueCommentEdited{Repo: testRepo, Number: 1, DatabaseID: 11, Body: "new", At: at}, CommentsChanged)
	st := getItem(t, s, testRepo, 1)
	c := st.Comments[0]
	if c.Body != "new" || c.Author != "a" || c.ID != "IC_1" || len(c.Reactions) != 1 {
		t.Fatalf("edit changed more than the body: %+v", c)
	}
	if st.Comments[1].Body != "other" {
		t.Fatalf("other comment touched: %+v", st.Comments[1])
	}
	// Same body again: no Change.
	if _, ch, _ := s.Apply(IssueCommentEdited{Repo: testRepo, Number: 1, DatabaseID: 11, Body: "new", At: at.Add(time.Second)}); len(ch) != 0 {
		t.Fatalf("unchanged body produced %d changes", len(ch))
	}
}

func TestIssueCommentEdited_UnknownAndStale(t *testing.T) {
	s := seedWithComments(t)
	at := time.Now()
	if _, ch, _ := s.Apply(IssueCommentEdited{Repo: testRepo, Number: 1, DatabaseID: 99, Body: "x", At: at}); len(ch) != 0 {
		t.Fatalf("unknown comment produced changes")
	}
	applyExpect(t, s, IssueCommentEdited{Repo: testRepo, Number: 1, DatabaseID: 11, Body: "v2", At: at}, CommentsChanged)
	// An older edit delivered late must not undo the newer one.
	if _, ch, _ := s.Apply(IssueCommentEdited{Repo: testRepo, Number: 1, DatabaseID: 11, Body: "v1", At: at.Add(-time.Minute)}); len(ch) != 0 {
		t.Fatalf("stale edit applied")
	}
	if got := getItem(t, s, testRepo, 1).Comments[0].Body; got != "v2" {
		t.Fatalf("body = %q, want v2", got)
	}
}

func TestIssueCommentDeleted(t *testing.T) {
	s := seedWithComments(t)
	applyExpect(t, s, IssueCommentDeleted{Repo: testRepo, Number: 1, DatabaseID: 11}, CommentsChanged)
	st := getItem(t, s, testRepo, 1)
	if len(st.Comments) != 1 || st.Comments[0].DatabaseID != 12 {
		t.Fatalf("comments after delete = %+v", st.Comments)
	}
	if _, ch, _ := s.Apply(IssueCommentDeleted{Repo: testRepo, Number: 1, DatabaseID: 11}); len(ch) != 0 {
		t.Fatalf("second delete produced changes")
	}
}

func seedWithThreads(t *testing.T) *Store {
	t.Helper()
	s := newStoreWithItem(t, testRepo, 1)
	for _, c := range []gh.Comment{
		{ID: "PRRC_1", Body: "t1", ReviewThreadID: "T1"},
		{ID: "PRRC_2", Body: "t1 reply", ReviewThreadID: "T1"},
		{ID: "PRRC_3", Body: "t2", ReviewThreadID: "T2"},
	} {
		applyExpect(t, s, ReviewThreadCommentAdded{Repo: testRepo, IssueNumber: 1, Comment: c}, CommentsChanged)
	}
	return s
}

func TestReviewThreadResolved(t *testing.T) {
	s := seedWithThreads(t)
	at := time.Now()
	snap := applyExpect(t, s, ReviewThreadResolved{Repo: testRepo, IssueNumber: 1, ThreadID: "T1", At: at}, LinkedPRChanged|CommentsChanged)
	lpr := snap.State().LinkedPR
	if len(lpr.ThreadComments) != 1 || lpr.ThreadComments[0].ReviewThreadID != "T2" {
		t.Fatalf("thread comments = %+v", lpr.ThreadComments)
	}
	if lpr.ResolvedThreadCount != 1 {
		t.Fatalf("ResolvedThreadCount = %d, want 1", lpr.ResolvedThreadCount)
	}
	// Replay: no change, count not bumped again.
	if _, ch, _ := s.Apply(ReviewThreadResolved{Repo: testRepo, IssueNumber: 1, ThreadID: "T1", At: at}); len(ch) != 0 {
		t.Fatalf("replay produced changes")
	}
	if got := getItem(t, s, testRepo, 1).LinkedPR.ResolvedThreadCount; got != 1 {
		t.Fatalf("count after replay = %d", got)
	}
}

func TestReviewThreadResolved_UnknownThreadAndNoLinkedPR(t *testing.T) {
	s := seedWithThreads(t)
	if _, ch, _ := s.Apply(ReviewThreadResolved{Repo: testRepo, IssueNumber: 1, ThreadID: "NOPE", At: time.Now()}); len(ch) != 0 {
		t.Fatalf("unknown thread produced changes")
	}
	if got := getItem(t, s, testRepo, 1).LinkedPR.ResolvedThreadCount; got != 0 {
		t.Fatalf("count = %d", got)
	}
	s2 := newStoreWithItem(t, testRepo, 2)
	if _, ch, _ := s2.Apply(ReviewThreadResolved{Repo: testRepo, IssueNumber: 2, ThreadID: "T1", At: time.Now()}); len(ch) != 0 {
		t.Fatalf("item without a linked PR produced changes")
	}
	if getItem(t, s2, testRepo, 2).LinkedPR != nil {
		t.Fatalf("a resolve created a LinkedPR")
	}
}

func TestReviewThreadUnresolved_InvalidatesAndOrders(t *testing.T) {
	s := seedWithThreads(t)
	applyExpect(t, s, ItemDeepFetched{Repo: testRepo, Number: 1, FreshState: testProjectItem(testRepo, 1)}, 0)
	st := getItem(t, s, testRepo, 1)
	if st.LastDeepFetchAt.IsZero() {
		t.Fatal("seed deep fetch did not stamp LastDeepFetchAt")
	}
	at := time.Now()
	applyExpect(t, s, ReviewThreadUnresolved{Repo: testRepo, IssueNumber: 1, ThreadID: "T1", At: at}, DeepFetchChanged|CommentsChanged)
	if !getItem(t, s, testRepo, 1).LastDeepFetchAt.IsZero() {
		t.Fatalf("deep fetch not invalidated")
	}
	// A resolve that was delivered after, but happened before, the unresolve is dropped.
	if _, ch, _ := s.Apply(ReviewThreadResolved{Repo: testRepo, IssueNumber: 1, ThreadID: "T1", At: at.Add(-time.Minute)}); len(ch) != 0 {
		t.Fatalf("reordered resolve applied")
	}
	if n := len(getItem(t, s, testRepo, 1).LinkedPR.ThreadComments); n != 3 {
		t.Fatalf("thread comments = %d, want 3", n)
	}
}

func TestReviewThreadCommentEditedAndDeleted(t *testing.T) {
	s := seedWithThreads(t)
	at := time.Now()
	applyExpect(t, s, ReviewThreadCommentEdited{Repo: testRepo, IssueNumber: 1, NodeID: "PRRC_3", Body: "edited", At: at}, LinkedPRChanged|CommentsChanged)
	st := getItem(t, s, testRepo, 1)
	if st.LinkedPR.ThreadComments[2].Body != "edited" || st.LinkedPR.ThreadComments[2].ReviewThreadID != "T2" {
		t.Fatalf("edit = %+v", st.LinkedPR.ThreadComments[2])
	}
	applyExpect(t, s, ReviewThreadCommentDeleted{Repo: testRepo, IssueNumber: 1, NodeID: "PRRC_1"}, LinkedPRChanged|CommentsChanged)
	if n := len(getItem(t, s, testRepo, 1).LinkedPR.ThreadComments); n != 2 {
		t.Fatalf("after delete: %d comments", n)
	}
	if _, ch, _ := s.Apply(ReviewThreadCommentDeleted{Repo: testRepo, IssueNumber: 1, NodeID: "PRRC_1"}); len(ch) != 0 {
		t.Fatalf("second delete produced changes")
	}
}

func openWithLabels(t *testing.T, s *Store, repo string, n int, labels ...string) {
	t.Helper()
	it := testProjectItem(repo, n)
	it.Labels = labels
	if _, _, err := s.Apply(IssueOpened{Item: it}); err != nil {
		t.Fatal(err)
	}
}

func TestRepoLabelChanged_RenameFansOutPerRepo(t *testing.T) {
	s := NewStore(nil)
	openWithLabels(t, s, testRepo, 1, "bug", "old", "x")
	openWithLabels(t, s, testRepo, 2, "old")
	openWithLabels(t, s, testRepo, 3, "other")
	openWithLabels(t, s, testRepo, 4, "old", "new") // already carries the target
	openWithLabels(t, s, "owner/elsewhere", 1, "old")

	var seen []Change
	s.Subscribe(ObserverFunc(func(c Change, _ Snapshot) { seen = append(seen, c) }))
	_, changes, err := s.Apply(RepoLabelChanged{Repo: testRepo, From: "old", To: "new", At: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 3 || len(seen) != 3 {
		t.Fatalf("changes=%d observed=%d, want 3", len(changes), len(seen))
	}
	for _, c := range changes {
		if c.Fields&LabelsChanged == 0 || len(c.LabelDeltas) != 0 || c.Origin != OriginWebhook {
			t.Fatalf("change = %+v", c)
		}
	}
	got := func(n int) []string { return getItem(t, s, testRepo, n).Labels }
	if l := got(1); len(l) != 3 || l[0] != "bug" || l[1] != "new" || l[2] != "x" {
		t.Fatalf("item 1 labels = %v (rename must be in place)", l)
	}
	if l := got(2); len(l) != 1 || l[0] != "new" {
		t.Fatalf("item 2 labels = %v", l)
	}
	if l := got(3); len(l) != 1 || l[0] != "other" {
		t.Fatalf("item 3 labels = %v", l)
	}
	if l := got(4); len(l) != 1 || l[0] != "new" {
		t.Fatalf("item 4 labels = %v (must not duplicate)", l)
	}
	if l := getItem(t, s, "owner/elsewhere", 1).Labels; len(l) != 1 || l[0] != "old" {
		t.Fatalf("other repo touched: %v", l)
	}
}

func TestRepoLabelChanged_DeleteAndStale(t *testing.T) {
	s := NewStore(nil)
	openWithLabels(t, s, testRepo, 1, "gone", "keep")
	now := time.Now()
	// A newer observation of the label than the delivery: dropped.
	applyExpect(t, s, IssueLabeled{Repo: testRepo, Number: 1, Label: "gone", At: now}, 0)
	if _, ch, _ := s.Apply(RepoLabelChanged{Repo: testRepo, From: "gone", At: now.Add(-time.Hour)}); len(ch) != 0 {
		t.Fatalf("stale delete applied")
	}
	if _, ch, _ := s.Apply(RepoLabelChanged{Repo: testRepo, From: "gone", At: now.Add(time.Second)}); len(ch) != 1 {
		t.Fatalf("delete produced %d changes", len(ch))
	}
	if l := getItem(t, s, testRepo, 1).Labels; len(l) != 1 || l[0] != "keep" {
		t.Fatalf("labels = %v", l)
	}
	if _, ch, _ := s.Apply(RepoLabelChanged{Repo: testRepo, From: "", To: "x", At: now}); len(ch) != 0 {
		t.Fatalf("empty From produced changes")
	}
}

func openWithMilestone(t *testing.T, s *Store, repo string, n int, ms *gh.Milestone) {
	t.Helper()
	it := testProjectItem(repo, n)
	it.Milestone, it.MilestoneKnown = ms, true
	if _, _, err := s.Apply(IssueOpened{Item: it}); err != nil {
		t.Fatal(err)
	}
}

func TestRepoMilestoneChanged(t *testing.T) {
	s := NewStore(nil)
	openWithMilestone(t, s, testRepo, 1, &gh.Milestone{Title: "v1", Number: 2})
	openWithMilestone(t, s, testRepo, 2, &gh.Milestone{Title: "v1", Number: 3}) // same title, other milestone
	openWithMilestone(t, s, testRepo, 3, nil)
	now := time.Now()

	_, ch, _ := s.Apply(RepoMilestoneChanged{Repo: testRepo, Number: 2, Title: "v1-renamed", At: now})
	if len(ch) != 1 || ch[0].Fields&MilestoneChanged == 0 {
		t.Fatalf("edit changes = %+v", ch)
	}
	if ch[0].Fields&(LabelsChanged|CommentsChanged|LinkedPRChanged) != 0 {
		t.Fatalf("milestone edit raised a wake flag: %b", ch[0].Fields)
	}
	if m := getItem(t, s, testRepo, 1).Milestone; m == nil || m.Title != "v1-renamed" || m.Number != 2 {
		t.Fatalf("item 1 milestone = %+v", m)
	}
	if m := getItem(t, s, testRepo, 2).Milestone; m == nil || m.Title != "v1" {
		t.Fatalf("matched by title, not number: %+v", m)
	}
	if _, ch, _ := s.Apply(RepoMilestoneChanged{Repo: testRepo, Number: 2, Deleted: true, At: now.Add(time.Second)}); len(ch) != 1 {
		t.Fatalf("delete changes = %d", len(ch))
	}
	st := getItem(t, s, testRepo, 1)
	if st.Milestone != nil || !st.MilestoneKnown {
		t.Fatalf("after delete: %+v known=%v", st.Milestone, st.MilestoneKnown)
	}
	// Stale: an older edit cannot resurrect a milestone... and cannot touch a nil one.
	if _, ch, _ := s.Apply(RepoMilestoneChanged{Repo: testRepo, Number: 2, Title: "late", At: now.Add(-time.Hour)}); len(ch) != 0 {
		t.Fatalf("edit of a cleared milestone produced changes")
	}
}

func TestSubIssueLinks_EitherSideAppliedOnce(t *testing.T) {
	s := NewStore(nil)
	openWithLabels(t, s, testRepo, 1)
	openWithLabels(t, s, testRepo, 2)
	parent, child := IssueRef{testRepo, 1}, IssueRef{testRepo, 2}
	at := time.Now()

	for i := 0; i < 2; i++ { // twice: a replay is a no-op
		_, ch, _ := s.Apply(SubIssueLinkedBelow{Repo: testRepo, Number: 1, Child: child, At: at})
		if i == 0 && (len(ch) != 1 || ch[0].Fields != SubIssueLinksChanged) {
			t.Fatalf("add changes = %+v", ch)
		}
		if i == 1 && len(ch) != 0 {
			t.Fatalf("replay produced changes")
		}
	}
	applyExpect(t, s, SubIssueLinkedUnder{Repo: testRepo, Number: 2, Parent: parent, At: at}, SubIssueLinksChanged)
	p, c := getItem(t, s, testRepo, 1), getItem(t, s, testRepo, 2)
	if len(p.SubIssues) != 1 || p.SubIssues[0] != child || c.ParentIssue == nil || *c.ParentIssue != parent {
		t.Fatalf("links: parent=%+v child=%+v", p.SubIssues, c.ParentIssue)
	}

	applyExpect(t, s, SubIssueLinkedBelow{Repo: testRepo, Number: 1, Child: child, Remove: true, At: at.Add(time.Second)}, SubIssueLinksChanged)
	applyExpect(t, s, SubIssueLinkedUnder{Repo: testRepo, Number: 2, Parent: parent, Remove: true, At: at.Add(time.Second)}, SubIssueLinksChanged)
	if _, ch, _ := s.Apply(SubIssueLinkedUnder{Repo: testRepo, Number: 2, Parent: parent, Remove: true, At: at.Add(time.Second)}); len(ch) != 0 {
		t.Fatalf("second remove produced changes")
	}
	p, c = getItem(t, s, testRepo, 1), getItem(t, s, testRepo, 2)
	if len(p.SubIssues) != 0 || c.ParentIssue != nil {
		t.Fatalf("links remain: %+v %+v", p.SubIssues, c.ParentIssue)
	}
	// A stale add (older than the remove) is dropped.
	if _, ch, _ := s.Apply(SubIssueLinkedBelow{Repo: testRepo, Number: 1, Child: child, At: at}); len(ch) != 0 {
		t.Fatalf("stale add applied")
	}
}

func TestSnapshotIsolatesSubIssues(t *testing.T) {
	s := NewStore(nil)
	openWithLabels(t, s, testRepo, 1)
	applyExpect(t, s, SubIssueLinkedBelow{Repo: testRepo, Number: 1, Child: IssueRef{testRepo, 2}, At: time.Now()}, SubIssueLinksChanged)
	applyExpect(t, s, SubIssueLinkedUnder{Repo: testRepo, Number: 1, Parent: IssueRef{testRepo, 9}, At: time.Now()}, SubIssueLinksChanged)
	snap, _ := s.Peek(testRepo, 1)
	st := snap.State()
	st.SubIssues[0].Number = 99
	st.ParentIssue.Number = 99
	again, _ := s.Peek(testRepo, 1)
	if a := again.State(); a.SubIssues[0].Number != 2 || a.ParentIssue.Number != 9 {
		t.Fatalf("snapshot aliases store state: %+v %+v", a.SubIssues, a.ParentIssue)
	}
}

func TestSubIssueLinksChangedIsNotAWakeFlagValue(t *testing.T) {
	// The flag must be a distinct bit so engine/observers.go's wake sets never
	// include it by accident of a shared value.
	for _, f := range []ChangeFlags{StatusChanged, LabelsChanged, LockChanged, LinkedPRChanged, CommentsChanged, AssigneesChanged, WorkerLifecycleChanged, PRFactsChanged} {
		if f&SubIssueLinksChanged != 0 {
			t.Fatalf("SubIssueLinksChanged overlaps %b", f)
		}
	}
}

// seedWithWebhookThreadComments holds the shapes #2444 is about: comments stored
// from a pull_request_review_comment delivery have no ReviewThreadID.
func seedWithWebhookThreadComments(t *testing.T) *Store {
	t.Helper()
	s := newStoreWithItem(t, testRepo, 1)
	for _, c := range []gh.Comment{
		{ID: "PRRC_w1", DatabaseID: 101, Body: "webhook-added, thread T1"},
		{ID: "PRRC_w2", DatabaseID: 102, Body: "webhook-added, thread T1 (db id only)"},
		{ID: "PRRC_f1", DatabaseID: 103, Body: "deep-fetched, thread T1", ReviewThreadID: "T1"},
		{ID: "PRRC_o1", DatabaseID: 104, Body: "webhook-added, thread T2"},
		{ID: "PRRC_t2", DatabaseID: 105, Body: "deep-fetched, thread T2", ReviewThreadID: "T2"},
	} {
		applyExpect(t, s, ReviewThreadCommentAdded{Repo: testRepo, IssueNumber: 1, Comment: c}, CommentsChanged)
	}
	return s
}

func resolveT1() ReviewThreadResolved {
	return ReviewThreadResolved{
		Repo: testRepo, IssueNumber: 1, ThreadID: "T1", At: time.Now(),
		// PRRC_t2 is listed too: it is tagged with another thread, so it must stay.
		CommentNodeIDs:     []string{"PRRC_w1", "PRRC_f1", "PRRC_t2"},
		CommentDatabaseIDs: []int{101, 102, 103},
	}
}

func threadCommentIDs(t *testing.T, s *Store) []string {
	t.Helper()
	var ids []string
	for _, c := range getItem(t, s, testRepo, 1).LinkedPR.ThreadComments {
		ids = append(ids, c.ID)
	}
	return ids
}

func TestReviewThreadResolved_DropsThreadlessCommentsByIdentity(t *testing.T) {
	s := seedWithWebhookThreadComments(t)
	applyExpect(t, s, resolveT1(), LinkedPRChanged|CommentsChanged)
	// w1 (node ID), w2 (database ID only) and f1 (thread ID) are gone; the
	// thread-less comment of another thread and the T2-tagged comment remain,
	// the latter although the payload listed its node ID (FR-004).
	got := threadCommentIDs(t, s)
	if len(got) != 2 || got[0] != "PRRC_o1" || got[1] != "PRRC_t2" {
		t.Fatalf("remaining comments = %v, want [PRRC_o1 PRRC_t2]", got)
	}
	if n := getItem(t, s, testRepo, 1).LinkedPR.ResolvedThreadCount; n != 1 {
		t.Fatalf("ResolvedThreadCount = %d, want 1", n)
	}
	// Replay changes nothing.
	if _, ch, _ := s.Apply(resolveT1()); len(ch) != 0 {
		t.Fatalf("replay produced changes")
	}
	if n := getItem(t, s, testRepo, 1).LinkedPR.ResolvedThreadCount; n != 1 {
		t.Fatalf("count after replay = %d", n)
	}
}

func TestReviewThreadResolved_NoListedCommentsKeepsThreadlessOnes(t *testing.T) {
	s := seedWithWebhookThreadComments(t)
	m := resolveT1()
	m.CommentNodeIDs, m.CommentDatabaseIDs = nil, nil
	applyExpect(t, s, m, LinkedPRChanged|CommentsChanged) // PRRC_f1 still matches by thread ID
	if got := threadCommentIDs(t, s); len(got) != 4 {
		t.Fatalf("remaining comments = %v, want the 4 others", got)
	}
}

func TestReviewThreadResolved_ThreadlessOnlyThread(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	applyExpect(t, s, ReviewThreadCommentAdded{Repo: testRepo, IssueNumber: 1, Comment: gh.Comment{ID: "PRRC_w1", DatabaseID: 101}}, CommentsChanged)
	applyExpect(t, s, ReviewThreadResolved{Repo: testRepo, IssueNumber: 1, ThreadID: "T1", At: time.Now(), CommentNodeIDs: []string{"PRRC_w1"}}, LinkedPRChanged|CommentsChanged)
	if got := threadCommentIDs(t, s); len(got) != 0 {
		t.Fatalf("remaining comments = %v", got)
	}
	// A zero DatabaseID never matches a listed zero/absent one.
	applyExpect(t, s, ReviewThreadCommentAdded{Repo: testRepo, IssueNumber: 1, Comment: gh.Comment{ID: "PRRC_z"}}, CommentsChanged)
	if _, ch, _ := s.Apply(ReviewThreadResolved{Repo: testRepo, IssueNumber: 1, ThreadID: "T9", At: time.Now(), CommentDatabaseIDs: []int{0}}); len(ch) != 0 {
		t.Fatalf("zero database ID matched")
	}
}

func TestReviewThreadResolved_StaleDeliveryStillOrdered(t *testing.T) {
	s := seedWithWebhookThreadComments(t)
	at := time.Now()
	s.Apply(ReviewThreadUnresolved{Repo: testRepo, IssueNumber: 1, ThreadID: "T1", At: at})
	m := resolveT1()
	m.At = at.Add(-time.Minute)
	if _, ch, _ := s.Apply(m); len(ch) != 0 {
		t.Fatalf("stale resolve applied")
	}
}

func TestReviewThreadResolved_IdentityMatchSeam(t *testing.T) {
	s := seedWithWebhookThreadComments(t)
	s.SetThreadResolveByIdentityDisabledForTest(true)
	applyExpect(t, s, resolveT1(), LinkedPRChanged|CommentsChanged)
	// Only the thread-ID-tagged comment went.
	if got := threadCommentIDs(t, s); len(got) != 4 {
		t.Fatalf("remaining comments = %v, want 4 (thread-less ones kept)", got)
	}
}
