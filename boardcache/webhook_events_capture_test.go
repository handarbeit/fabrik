package boardcache

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

// Tests for the #2352 webhook events, fed by verbatim captured deliveries
// (boardcache/testdata/webhooks). Shapes with no capture (review-comment
// edited/deleted) are DERIVED in test code from the captured review-thread
// comment and are labelled as such.

const captureRepo = "example-org/fabrik-test-alpha"

func loadCapture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "webhooks", name+".json"))
	if err != nil {
		t.Fatalf("read capture %s: %v", name, err)
	}
	return b
}

// noFetchFallback fails the test if the Store ever reaches for GitHub.
type noFetchFallback struct{ t *testing.T }

func (f noFetchFallback) FetchItem(repo string, number int) (gh.ProjectItem, error) {
	f.t.Errorf("FR-011: apply path fetched %s#%d from GitHub", repo, number)
	return gh.ProjectItem{}, os.ErrNotExist
}

type captureEnv struct {
	c       *CacheImpl
	mc      *mockClient
	mu      sync.Mutex
	changes []itemstate.Change
	logs    []string
}

func newCaptureEnv(t *testing.T) *captureEnv {
	t.Helper()
	e := &captureEnv{mc: &mockClient{}}
	st := itemstate.NewStore(noFetchFallback{t})
	st.Subscribe(itemstate.ObserverFunc(func(ch itemstate.Change, _ itemstate.Snapshot) {
		e.mu.Lock()
		e.changes = append(e.changes, ch)
		e.mu.Unlock()
	}))
	e.c = NewCacheImpl(e.mc, st, func(f string, a ...any) {
		e.mu.Lock()
		e.logs = append(e.logs, strings.TrimSpace(fmt.Sprintf(f, a...)))
		e.mu.Unlock()
	})
	t.Cleanup(func() {
		m := e.mc
		if m.fetchItemDetailsCount+m.fetchCheckRunsCount+m.fetchLinkedPRCount+m.fetchLabelsCount+
			m.fetchPRClosingIssuesCount+m.fetchPRReviewsCount+m.fetchPRReviewRequestsCount+
			m.fetchPRReviewDecisionCount+m.fetchPRsForSHACount+m.fetchProjectItemCount != 0 {
			t.Errorf("FR-011: a delta handler made a GitHub read: %+v", m)
		}
	})
	return e
}

func (e *captureEnv) open(t *testing.T, number int, labels ...string) {
	t.Helper()
	it := gh.ProjectItem{ID: "I_" + string(rune('A'+number%26)), Number: number, Repo: captureRepo, Title: "t", Labels: labels}
	if _, _, err := e.c.store.Apply(itemstate.IssueOpened{Item: it}); err != nil {
		t.Fatal(err)
	}
}

func (e *captureEnv) state(t *testing.T, number int) itemstate.ItemState {
	t.Helper()
	snap, ok := e.c.store.Peek(captureRepo, number)
	if !ok {
		t.Fatalf("%s#%d not in store", captureRepo, number)
	}
	return snap.State()
}

func (e *captureEnv) changesWith(flag itemstate.ChangeFlags) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, c := range e.changes {
		if c.Fields&flag != 0 {
			n++
		}
	}
	return n
}

func (e *captureEnv) reset() {
	e.mu.Lock()
	e.changes = nil
	e.mu.Unlock()
}


// ---- wire pins: the fields each handler reads ----

func TestCapturedPayloadsDecodeThroughTheHandlerStructs(t *testing.T) {
	var thread reviewThreadEventPayload
	mustDecode(t, "pull_request_review_thread_resolved", &thread)
	if thread.Action != "resolved" || thread.Thread.NodeID != "PRRT_kwDOSmuv9s6tyk3b" || thread.PullRequest.Number != 8769 ||
		thread.Repository.FullName != captureRepo || thread.UpdatedAt == "" {
		t.Fatalf("thread pins: %+v", thread)
	}
	if _, err := time.Parse(time.RFC3339, string(thread.UpdatedAt)); err != nil {
		t.Fatalf("thread updated_at (non-UTC offset) must parse as RFC3339: %v", err)
	}

	var label labelEventPayload
	mustDecode(t, "label_edited", &label)
	if label.Action != "edited" || label.Changes.Name.From != "probe-2352-label" || label.Label.Name != "probe-2352-renamed" || label.Repository.FullName != captureRepo {
		t.Fatalf("label pins: %+v", label)
	}
	var labelDel labelEventPayload
	mustDecode(t, "label_deleted", &labelDel)
	if labelDel.Action != "deleted" || labelDel.Label.Name == "" {
		t.Fatalf("label deleted pins: %+v", labelDel)
	}

	var ms milestoneEventPayload
	mustDecode(t, "milestone_edited", &ms)
	if ms.Action != "edited" || ms.Milestone.Number != 2 || ms.Milestone.Title != "probe-2352-b-edited" {
		t.Fatalf("milestone pins: %+v", ms)
	}
	var msDel milestoneEventPayload
	mustDecode(t, "milestone_deleted", &msDel)
	if msDel.Action != "deleted" || msDel.Milestone.Number != 1 {
		t.Fatalf("milestone deleted pins: %+v", msDel)
	}

	var sub, par subIssuesEventPayload
	mustDecode(t, "sub_issues_sub_issue_added", &sub)
	mustDecode(t, "sub_issues_parent_issue_added", &par)
	if sub.Action != "sub_issue_added" || sub.ParentIssue.Number != 8766 || sub.SubIssue.Number != 8768 ||
		sub.SubIssueRepo.FullName != captureRepo || sub.Repository.FullName != captureRepo {
		t.Fatalf("sub-issue (parent side) pins: %+v", sub)
	}
	if par.Action != "parent_issue_added" || par.ParentIssue.Number != 8766 || par.SubIssue.Number != 8768 ||
		par.ParentIssueRepo.FullName != captureRepo || par.Repository.FullName != captureRepo {
		t.Fatalf("sub-issue (child side) pins: %+v", par)
	}

	var cm issueCommentPayload
	mustDecode(t, "issue_comment_edited", &cm)
	if cm.Action != "edited" || cm.Comment.DatabaseID != 6259989993 || cm.Comment.Body != "probe comment edited" ||
		cm.Comment.UpdatedAt == "" || cm.Issue.Number != 8766 || cm.Repository.FullName != captureRepo {
		t.Fatalf("issue_comment pins: %+v", cm)
	}
}

func mustDecode(t *testing.T, name string, v any) {
	t.Helper()
	if err := json.Unmarshal(loadCapture(t, name), v); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
}

// ---- issue_comment edited / deleted ----

func TestCapturedIssueCommentEditedThenDeleted(t *testing.T) {
	e := newCaptureEnv(t)
	e.open(t, 8766)
	e.c.store.Apply(itemstate.IssueCommentCreated{Repo: captureRepo, Number: 8766, Comment: gh.Comment{
		ID: "IC_kwDOSmuv9s8AAAABdR_d6Q", DatabaseID: 6259989993, Author: "arbeithand", Body: "probe comment",
		Reactions: []gh.ReactionGroup{{Content: "ROCKET"}}}})
	e.reset()

	e.c.ApplyDelta("issue_comment", loadCapture(t, "issue_comment_edited"))
	c := e.state(t, 8766).Comments
	if len(c) != 1 || c[0].Body != "probe comment edited" || c[0].Author != "arbeithand" || len(c[0].Reactions) != 1 {
		t.Fatalf("after edit: %+v", c)
	}
	if e.changesWith(itemstate.CommentsChanged) != 1 {
		t.Fatalf("edit must raise CommentsChanged once")
	}

	e.c.ApplyDelta("issue_comment", loadCapture(t, "issue_comment_deleted"))
	if c := e.state(t, 8766).Comments; len(c) != 0 {
		t.Fatalf("after delete: %+v", c)
	}
	// Replays and unknown items: no panic, no further change.
	e.reset()
	e.c.ApplyDelta("issue_comment", loadCapture(t, "issue_comment_deleted"))
	e.c.ApplyDelta("issue_comment", loadCapture(t, "issue_comment_edited"))
	if len(e.changes) != 0 {
		t.Fatalf("replay changed state: %+v", e.changes)
	}
}

func TestCapturedIssueCommentEditOnUnknownItemIsIgnored(t *testing.T) {
	e := newCaptureEnv(t) // nothing seeded; noFetchFallback fails the test on a fetch
	e.c.ApplyDelta("issue_comment", loadCapture(t, "issue_comment_edited"))
	e.c.ApplyDelta("issue_comment", loadCapture(t, "issue_comment_deleted"))
	if _, ok := e.c.store.Peek(captureRepo, 8766); ok {
		t.Fatalf("an ignored delivery created the item")
	}
}

func TestIssueCommentEditDoesNotResetProcessedState(t *testing.T) {
	// The processed (🚀) state lives in StageState.ProcessedComments and on the
	// comment's reactions; an edit must touch neither.
	e := newCaptureEnv(t)
	e.open(t, 8766)
	e.c.store.Apply(itemstate.IssueCommentCreated{Repo: captureRepo, Number: 8766, Comment: gh.Comment{
		ID: "IC_x", DatabaseID: 6259989993, Body: "probe comment", Reactions: []gh.ReactionGroup{{Content: "ROCKET"}}}})
	before := e.state(t, 8766)
	e.c.ApplyDelta("issue_comment", loadCapture(t, "issue_comment_edited"))
	after := e.state(t, 8766)
	if len(after.Comments[0].Reactions) != len(before.Comments[0].Reactions) ||
		len(after.StageState.ProcessedComments) != len(before.StageState.ProcessedComments) {
		t.Fatalf("edit changed processed state: before=%+v after=%+v", before.Comments[0], after.Comments[0])
	}
}

// ---- pull_request_review_thread ----

func seedThreadItem(t *testing.T, e *captureEnv) {
	t.Helper()
	e.open(t, 8766)
	testSetLinkedPR(e.c, captureRepo, 8766, 8769)
	for _, cm := range []gh.Comment{
		{ID: "PRRC_kwDOSmuv9s8AAAABAOxKXA", Body: "probe thread", ReviewThreadID: "PRRT_kwDOSmuv9s6tyk3b", FromPR: 8769},
		{ID: "PRRC_other", Body: "another thread", ReviewThreadID: "PRRT_other", FromPR: 8769},
	} {
		e.c.store.Apply(itemstate.ReviewThreadCommentAdded{Repo: captureRepo, IssueNumber: 8766, Comment: cm})
	}
	e.reset()
}

func TestCapturedThreadResolvedIsSeenWithoutRefetch(t *testing.T) {
	e := newCaptureEnv(t)
	seedThreadItem(t, e)
	e.c.ApplyDelta("pull_request_review_thread", loadCapture(t, "pull_request_review_thread_resolved"))
	lpr := e.state(t, 8766).LinkedPR
	if len(lpr.ThreadComments) != 1 || lpr.ThreadComments[0].ReviewThreadID != "PRRT_other" || lpr.ResolvedThreadCount != 1 {
		t.Fatalf("after resolve: %+v", lpr)
	}
	if e.changesWith(itemstate.CommentsChanged|itemstate.LinkedPRChanged) != 1 {
		t.Fatalf("resolve must raise a waking change once; changes=%+v", e.changes)
	}
	// Replay is a no-op.
	e.reset()
	e.c.ApplyDelta("pull_request_review_thread", loadCapture(t, "pull_request_review_thread_resolved"))
	if len(e.changes) != 0 || e.state(t, 8766).LinkedPR.ResolvedThreadCount != 1 {
		t.Fatalf("replay changed state")
	}
}

func TestCapturedThreadUnresolvedInvalidatesDeepCache(t *testing.T) {
	e := newCaptureEnv(t)
	seedThreadItem(t, e)
	testSetDeepFetched(e.c, captureRepo, 8766)
	if e.state(t, 8766).LastDeepFetchAt.IsZero() {
		t.Fatal("seed: not deep fetched")
	}
	e.c.ApplyDelta("pull_request_review_thread", loadCapture(t, "pull_request_review_thread_unresolved"))
	if !e.state(t, 8766).LastDeepFetchAt.IsZero() {
		t.Fatalf("unresolved must invalidate the deep cache so the next fetch rebuilds the thread")
	}
	if e.changesWith(itemstate.CommentsChanged) != 1 {
		t.Fatalf("unresolved must wake the poll")
	}
}

func TestCapturedThreadForUnknownPRIsIgnored(t *testing.T) {
	e := newCaptureEnv(t)
	e.open(t, 8766) // item exists but no linked PR 8769
	e.reset()
	e.c.ApplyDelta("pull_request_review_thread", loadCapture(t, "pull_request_review_thread_resolved"))
	if len(e.changes) != 0 {
		t.Fatalf("unknown PR changed state: %+v", e.changes)
	}
}

// ---- pull_request_review_comment edited / deleted (derived shapes) ----

// reviewCommentFrom derives a pull_request_review_comment delivery from the
// captured thread's first comment: there is no captured edited/deleted delivery
// for review comments, so action and body are set here (labelled DERIVED).
func reviewCommentFrom(t *testing.T, action, body string) []byte {
	t.Helper()
	var d map[string]json.RawMessage
	if err := json.Unmarshal(loadCapture(t, "pull_request_review_thread_resolved"), &d); err != nil {
		t.Fatal(err)
	}
	var th struct {
		Comments []map[string]any `json:"comments"`
	}
	if err := json.Unmarshal(d["thread"], &th); err != nil {
		t.Fatal(err)
	}
	cm := th.Comments[0]
	cm["body"] = body
	out := map[string]any{
		"action":       action,
		"comment":      cm,
		"pull_request": map[string]any{"number": 8769},
		"repository":   map[string]any{"full_name": captureRepo},
	}
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestDerivedReviewCommentEditedThenDeleted(t *testing.T) {
	e := newCaptureEnv(t)
	seedThreadItem(t, e)
	e.c.ApplyDelta("pull_request_review_comment", reviewCommentFrom(t, "edited", "probe thread, edited"))
	cs := e.state(t, 8766).LinkedPR.ThreadComments
	if len(cs) != 2 || cs[0].Body != "probe thread, edited" || cs[0].ReviewThreadID != "PRRT_kwDOSmuv9s6tyk3b" {
		t.Fatalf("after edit: %+v", cs)
	}
	e.c.ApplyDelta("pull_request_review_comment", reviewCommentFrom(t, "deleted", "probe thread, edited"))
	cs = e.state(t, 8766).LinkedPR.ThreadComments
	if len(cs) != 1 || cs[0].ID != "PRRC_other" {
		t.Fatalf("after delete: %+v", cs)
	}
}

// ---- label ----

func TestCapturedLabelRenameAndDelete(t *testing.T) {
	e := newCaptureEnv(t)
	e.open(t, 1, "probe-2352-label", "bug")
	e.open(t, 2, "unrelated")
	e.open(t, 3, "probe-2352-label")
	e.reset()

	e.c.ApplyDelta("label", loadCapture(t, "label_created")) // created: nothing
	if len(e.changes) != 0 {
		t.Fatalf("label created changed items: %+v", e.changes)
	}
	e.c.ApplyDelta("label", loadCapture(t, "label_edited"))
	if l := e.state(t, 1).Labels; len(l) != 2 || l[0] != "probe-2352-renamed" || l[1] != "bug" {
		t.Fatalf("item 1 labels = %v", l)
	}
	if l := e.state(t, 3).Labels; len(l) != 1 || l[0] != "probe-2352-renamed" {
		t.Fatalf("item 3 labels = %v", l)
	}
	if l := e.state(t, 2).Labels; len(l) != 1 || l[0] != "unrelated" {
		t.Fatalf("item 2 labels = %v", l)
	}
	if e.changesWith(itemstate.LabelsChanged) != 2 {
		t.Fatalf("rename must wake once per carrying item; got %d", e.changesWith(itemstate.LabelsChanged))
	}
	e.c.ApplyDelta("label", loadCapture(t, "label_deleted"))
	if l := e.state(t, 1).Labels; len(l) != 1 || l[0] != "bug" {
		t.Fatalf("after delete item 1 = %v", l)
	}
	if l := e.state(t, 3).Labels; len(l) != 0 {
		t.Fatalf("after delete item 3 = %v", l)
	}
}

func TestLabelEditedWithoutNameChangeRenamesNothing(t *testing.T) {
	e := newCaptureEnv(t)
	e.open(t, 1, "probe-2352-renamed")
	e.reset()
	var d map[string]any
	if err := json.Unmarshal(loadCapture(t, "label_edited"), &d); err != nil {
		t.Fatal(err)
	}
	delete(d, "changes") // DERIVED: a colour/description edit carries no changes.name
	b, _ := json.Marshal(d)
	e.c.ApplyDelta("label", b)
	if len(e.changes) != 0 {
		t.Fatalf("colour edit changed items: %+v", e.changes)
	}
}

// ---- milestone ----

func TestCapturedMilestoneEditAndDelete(t *testing.T) {
	e := newCaptureEnv(t)
	for _, n := range []int{1, 2, 3} {
		e.open(t, n)
	}
	e.c.store.Apply(itemstate.IssueMilestoneUpdated{Repo: captureRepo, Number: 1, Milestone: &gh.Milestone{Title: "probe-2352-b", Number: 2}})
	e.c.store.Apply(itemstate.IssueMilestoneUpdated{Repo: captureRepo, Number: 2, Milestone: &gh.Milestone{Title: "probe-2352-b", Number: 7}}) // same title, other number
	e.c.store.Apply(itemstate.IssueMilestoneUpdated{Repo: captureRepo, Number: 3, Milestone: &gh.Milestone{Title: "probe-2352-edited", Number: 1}})
	e.reset()

	e.c.ApplyDelta("milestone", loadCapture(t, "milestone_created"))
	if len(e.changes) != 0 {
		t.Fatalf("milestone created changed items")
	}
	e.c.ApplyDelta("milestone", loadCapture(t, "milestone_edited"))
	if m := e.state(t, 1).Milestone; m == nil || m.Title != "probe-2352-b-edited" || m.Number != 2 {
		t.Fatalf("item 1 milestone = %+v", m)
	}
	if m := e.state(t, 2).Milestone; m == nil || m.Title != "probe-2352-b" {
		t.Fatalf("matched by title instead of number: %+v", m)
	}
	if e.changesWith(itemstate.LabelsChanged|itemstate.CommentsChanged|itemstate.LinkedPRChanged|itemstate.StatusChanged) != 0 {
		t.Fatalf("milestone edit raised a wake flag")
	}
	e.c.ApplyDelta("milestone", loadCapture(t, "milestone_deleted")) // number 1
	if st := e.state(t, 3); st.Milestone != nil || !st.MilestoneKnown {
		t.Fatalf("after delete item 3: %+v known=%v", st.Milestone, st.MilestoneKnown)
	}
}

// ---- sub_issues ----

func TestCapturedSubIssuePairAppliesOnceInEitherOrder(t *testing.T) {
	for _, order := range [][]string{
		{"sub_issues_sub_issue_added", "sub_issues_parent_issue_added"},
		{"sub_issues_parent_issue_added", "sub_issues_sub_issue_added"},
		{"sub_issues_parent_issue_added", "sub_issues_parent_issue_added"},
		{"sub_issues_sub_issue_added"},
	} {
		e := newCaptureEnv(t)
		e.open(t, 8766)
		e.open(t, 8768)
		e.reset()
		for _, f := range order {
			e.c.ApplyDelta("sub_issues", loadCapture(t, f))
		}
		p, c := e.state(t, 8766), e.state(t, 8768)
		wantChild := itemstate.IssueRef{Repo: captureRepo, Number: 8768}
		wantParent := itemstate.IssueRef{Repo: captureRepo, Number: 8766}
		if len(p.SubIssues) != 1 || p.SubIssues[0] != wantChild || c.ParentIssue == nil || *c.ParentIssue != wantParent {
			t.Fatalf("order %v: parent.SubIssues=%+v child.ParentIssue=%+v", order, p.SubIssues, c.ParentIssue)
		}
		if n := e.changesWith(itemstate.SubIssueLinksChanged); n != 2 {
			t.Fatalf("order %v: %d link changes, want exactly 2 (one per endpoint)", order, n)
		}
		if n := e.changesWith(itemstate.LabelsChanged | itemstate.CommentsChanged | itemstate.LinkedPRChanged); n != 0 {
			t.Fatalf("a link change raised a wake flag")
		}

		for _, f := range []string{"sub_issues_parent_issue_removed", "sub_issues_sub_issue_removed"} {
			e.c.ApplyDelta("sub_issues", loadCapture(t, f))
		}
		p, c = e.state(t, 8766), e.state(t, 8768)
		if len(p.SubIssues) != 0 || c.ParentIssue != nil {
			t.Fatalf("order %v: links remain after removal: %+v %+v", order, p.SubIssues, c.ParentIssue)
		}
	}
}

func TestCapturedSubIssueWithOneEndpointCachedStillApplies(t *testing.T) {
	e := newCaptureEnv(t)
	e.open(t, 8766) // the child is not cached
	e.c.ApplyDelta("sub_issues", loadCapture(t, "sub_issues_parent_issue_added"))
	if p := e.state(t, 8766); len(p.SubIssues) != 1 {
		t.Fatalf("parent links = %+v", p.SubIssues)
	}
	if _, ok := e.c.store.Peek(captureRepo, 8768); ok {
		t.Fatalf("the uncached endpoint was created")
	}
}
