package engine

import (
	"errors"
	"strings"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

// unknownMergeFixture drives handleMergeAndCIGates against a PR whose mergeable
// fields the test controls, on a stub clock. Steps are 5 minutes apart: well
// inside the merge-gate lapse (3 × the 10-minute poll) and below the 20-minute
// bound, so the bound is reached on the fifth step.
type unknownMergeFixture struct {
	t         *testing.T
	eng       *Engine
	client    *mockGitHubClient
	start     time.Time
	head      string
	mergeable *bool
	state     string
	fieldsErr error
	runs      []gh.CheckRun
	comments  []gh.Comment
}

func newUnknownMergeFixture(t *testing.T) *unknownMergeFixture {
	t.Helper()
	f := &unknownMergeFixture{t: t, start: time.Now(), head: "deadbeefcafe0001", state: "unknown"}
	f.client = &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, n int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 42, HeadSHA: f.head, State: "open"}, nil
		},
		fetchPRMergeableFieldsFn: func(owner, repo string, n int) (*bool, string, error) {
			return f.mergeable, f.state, f.fieldsErr
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) { return f.runs, nil },
	}
	stgs := []*stages.Stage{{Name: "Implement", Order: 1, Prompt: "implement"}, {Name: "Review", Order: 2, Prompt: "review"}}
	f.eng = testEngineWithStages(t, f.client, stgs)
	f.eng.cfg.PollSeconds = 600
	f.eng.cfg.MergeStateUnknownTimeout = 20 * time.Minute
	f.runs = nGreenRuns(20)
	return f
}

func nGreenRuns(n int) []gh.CheckRun {
	var out []gh.CheckRun
	for i := 0; i < n; i++ {
		out = append(out, gh.CheckRun{ID: int64(i + 1), Name: string(rune('a'+i%26)) + string(rune('0'+i/26)), Status: "completed", Conclusion: "success"})
	}
	return out
}

// at runs the merge/CI gates once at start+d and returns whether it claimed.
func (f *unknownMergeFixture) at(d time.Duration) bool {
	f.t.Helper()
	f.eng.SetClock(stubClock{t: f.start.Add(d)})
	pctx := makeMergeGatePctx(&gh.ProjectBoard{ProjectID: "PVT_1"}, map[string]bool{})
	pctx.item.Comments = f.comments
	claimed := f.eng.handleMergeAndCIGates(pctx)
	// Carry posted pause comments forward so dedup sees them on the next poll.
	f.client.mu.Lock()
	for _, c := range f.client.addCommentCalls[len(f.comments):] {
		f.comments = append(f.comments, gh.Comment{Body: c.body})
	}
	f.client.mu.Unlock()
	return claimed
}

func (f *unknownMergeFixture) pauseCount() int {
	f.client.mu.Lock()
	defer f.client.mu.Unlock()
	n := 0
	for _, l := range f.client.addLabelCalls {
		if l.labelName == "fabrik:paused" {
			n++
		}
	}
	return n
}

func (f *unknownMergeFixture) commentCount() int {
	f.client.mu.Lock()
	defer f.client.mu.Unlock()
	return len(f.client.addCommentCalls)
}

func (f *unknownMergeFixture) clock() *unknownMergeClock {
	c := &f.eng.unknownMergeClocks
	c.mu.Lock()
	defer c.mu.Unlock()
	if cur := c.m[f.eng.unknownMergeKey(gh.ProjectItem{Number: 20, Repo: "owner/repo"})]; cur != nil {
		cp := *cur
		return &cp
	}
	return nil
}

func TestUnknownMergeClock_StartsOnMergeableNull(t *testing.T) {
	f := newUnknownMergeFixture(t)
	f.mergeable, f.state = nil, ""
	if !f.at(0) {
		t.Fatal("unknown merge state must claim the item")
	}
	c := f.clock()
	if c == nil || c.value != "mergeable=null" || c.head != f.head {
		t.Fatalf("clock = %+v, want started on mergeable=null for head %s", c, f.head)
	}
}

func TestUnknownMergeClock_StartsOnStateUnknown(t *testing.T) {
	f := newUnknownMergeFixture(t)
	f.mergeable, f.state = boolPtr(true), "unknown"
	f.at(0)
	if c := f.clock(); c == nil || c.value != "mergeable_state=unknown" {
		t.Fatalf("clock = %+v, want started on mergeable_state=unknown", c)
	}
}

func TestUnknownMergeClock_ResetsOnComputedState(t *testing.T) {
	f := newUnknownMergeFixture(t)
	f.mergeable, f.state = nil, ""
	f.at(0)
	f.at(5 * time.Minute)
	f.mergeable, f.state = boolPtr(true), "clean"
	f.at(10 * time.Minute)
	if f.clock() != nil {
		t.Fatal("a computed state must end the clock")
	}
	// A later return to unknown starts a fresh clock: 10 more minutes is still short of the bound.
	f.mergeable, f.state = nil, ""
	f.at(15 * time.Minute)
	f.at(25 * time.Minute)
	if f.pauseCount() != 0 {
		t.Fatal("a fresh clock after a computed state must not inherit the old elapsed time")
	}
}

func TestUnknownMergeClock_ResetsOnHeadChange(t *testing.T) {
	f := newUnknownMergeFixture(t)
	f.mergeable, f.state = nil, ""
	f.at(0)
	f.at(15 * time.Minute)
	f.head = "feedfacefeed0002"
	f.at(20 * time.Minute) // would be the bound for the old head
	if f.pauseCount() != 0 {
		t.Fatal("a head change must restart the clock")
	}
	if c := f.clock(); c == nil || c.head != f.head || !c.since.Equal(f.start.Add(20*time.Minute)) {
		t.Fatalf("clock = %+v, want restarted at 20m for the new head", c)
	}
}

func TestUnknownMergeClock_ReadErrorAndCIPendingDoNotStart(t *testing.T) {
	f := newUnknownMergeFixture(t)
	f.fieldsErr = errors.New("boom")
	f.at(0)
	if f.clock() != nil {
		t.Fatal("a FetchPRMergeableFields error must not start the clock")
	}
	f.fieldsErr = nil
	f.mergeable, f.state = boolPtr(true), "blocked"
	f.runs = []gh.CheckRun{{ID: 1, Name: "ci", Status: "in_progress"}}
	f.at(5 * time.Minute)
	if f.clock() != nil {
		t.Fatal("a CI-checks-pending hold must not start the clock")
	}
}

func TestUnknownMergeClock_ReadErrorLeavesRunningClockAlone(t *testing.T) {
	f := newUnknownMergeFixture(t)
	f.mergeable, f.state = nil, ""
	f.at(0)
	f.fieldsErr = errors.New("boom")
	f.at(5 * time.Minute)
	f.fieldsErr = nil
	f.at(10 * time.Minute)
	if c := f.clock(); c == nil || !c.since.Equal(f.start) {
		t.Fatalf("clock = %+v, want the original start preserved across a read error", c)
	}
}

func TestUnknownMergeBound_FiresOnceAtBoundNotBefore(t *testing.T) {
	f := newUnknownMergeFixture(t)
	f.mergeable, f.state = nil, ""
	for _, d := range []time.Duration{0, 5, 10, 15} {
		f.at(d * time.Minute)
		if f.pauseCount() != 0 {
			t.Fatalf("paused at %dm, before the 20m bound", d)
		}
	}
	if !f.at(20 * time.Minute) {
		t.Fatal("must claim")
	}
	if f.pauseCount() != 1 || f.commentCount() != 1 {
		t.Fatalf("pauses=%d comments=%d, want 1/1 at the bound", f.pauseCount(), f.commentCount())
	}
	body := f.client.addCommentCalls[0].body
	for _, want := range []string{
		"GitHub has not reported a computed mergeability for PR #42 at head `deadbeefca`",
		"20m0s", "`mergeable=null`",
		"CI gate for stage **Implement** was never reached",
		"20 of 20 checks passed",
		pauseCauseUnknownMergeStateMarker,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("comment missing %q:\n%s", want, body)
		}
	}
	for _, bad := range []string{"address any failures", "timed out waiting for checks"} {
		if strings.Contains(body, bad) {
			t.Errorf("comment must not contain %q:\n%s", bad, body)
		}
	}
}

func TestUnknownMergeBound_NamesStateUnknownAndNonGreenChecks(t *testing.T) {
	f := newUnknownMergeFixture(t)
	f.mergeable, f.state = boolPtr(true), "unknown"
	f.runs = append(nGreenRuns(18),
		gh.CheckRun{ID: 100, Name: "x", Status: "completed", Conclusion: "failure"},
		gh.CheckRun{ID: 101, Name: "y", Status: "in_progress"})
	for _, d := range []time.Duration{0, 5, 10, 15, 20} {
		f.at(d * time.Minute)
	}
	body := f.client.addCommentCalls[0].body
	if !strings.Contains(body, "`mergeable_state=unknown`") || !strings.Contains(body, "18 of 20 checks passed, 1 failed, 1 pending") {
		t.Errorf("comment = %s", body)
	}
}

func TestUnknownMergeBound_NotOnFirstEvaluationAfterStart(t *testing.T) {
	// A fresh process has no clock: even if GitHub has been null for hours, the
	// first observation only starts it (ADR 2059 R2).
	f := newUnknownMergeFixture(t)
	f.eng.cfg.MergeStateUnknownTimeout = time.Nanosecond
	f.mergeable, f.state = nil, ""
	f.at(0)
	if f.pauseCount() != 0 {
		t.Fatal("the first evaluation after a start must never escalate")
	}
}

func TestUnknownMergeBound_NotReposted_ReappliedOnResume(t *testing.T) {
	f := newUnknownMergeFixture(t)
	f.mergeable, f.state = nil, ""
	for _, d := range []time.Duration{0, 5, 10, 15, 20} {
		f.at(d * time.Minute)
	}
	if f.pauseCount() != 1 || f.commentCount() != 1 {
		t.Fatalf("setup: pauses=%d comments=%d", f.pauseCount(), f.commentCount())
	}
	if f.clock() != nil {
		t.Fatal("the pause must clear the clock")
	}
	// A human resumes (the pause comment stays); the PR still reads null. The
	// clock restarts: nothing fires for a full bound, then the pause is reapplied
	// without a second comment.
	for _, d := range []time.Duration{25, 30, 35, 40} {
		f.at(d * time.Minute)
		if f.pauseCount() != 1 {
			t.Fatalf("re-paused at %dm, before a fresh bound elapsed", d)
		}
	}
	f.at(45 * time.Minute)
	if f.pauseCount() != 2 {
		t.Fatalf("pauses = %d, want the labels reapplied after a fresh bound", f.pauseCount())
	}
	if f.commentCount() != 1 {
		t.Fatalf("comments = %d, want the existing comment reused (#1408)", f.commentCount())
	}
}

func TestUnknownMergeBound_GapLapsesTheClock(t *testing.T) {
	f := newUnknownMergeFixture(t)
	f.mergeable, f.state = nil, ""
	f.at(0)
	f.at(5 * time.Minute)
	// Unobserved for 45m (a pause, non-admission): longer than the 30m lapse.
	f.at(50 * time.Minute)
	if f.pauseCount() != 0 {
		t.Fatal("an unobserved gap must restart the clock, not count as unknown time")
	}
	if c := f.clock(); c == nil || !c.since.Equal(f.start.Add(50*time.Minute)) {
		t.Fatalf("clock = %+v, want restarted at 50m", c)
	}
}

func TestUnknownMergeBound_WorkerInFlightDefers(t *testing.T) {
	f := newUnknownMergeFixture(t)
	f.mergeable, f.state = nil, ""
	for _, d := range []time.Duration{0, 5, 10, 15} {
		f.at(d * time.Minute)
	}
	f.eng.store.Apply(itemstate.WorkerEntered{Repo: "owner/repo", Number: 20, StageName: "Implement", StartedAt: time.Now()})
	f.at(20 * time.Minute)
	if f.pauseCount() != 0 {
		t.Fatal("a worker in flight must defer the pause")
	}
	f.eng.store.Apply(itemstate.WorkerExited{Repo: "owner/repo", Number: 20})
	f.at(25 * time.Minute)
	if f.pauseCount() != 1 {
		t.Fatal("once the worker has gone the next poll must pause")
	}
}

func TestUnknownMergeBound_CheckRunReadErrorStillPauses(t *testing.T) {
	f := newUnknownMergeFixture(t)
	f.client.fetchCheckRunsFn = func(owner, repo, sha string) ([]gh.CheckRun, error) { return nil, errors.New("boom") }
	f.mergeable, f.state = nil, ""
	for _, d := range []time.Duration{0, 5, 10, 15, 20} {
		f.at(d * time.Minute)
	}
	if f.pauseCount() != 1 {
		t.Fatal("a check-run read error must not block the pause")
	}
	if !strings.Contains(f.client.addCommentCalls[0].body, "check-run state could not be read") {
		t.Errorf("comment = %s", f.client.addCommentCalls[0].body)
	}
}

func TestUnknownMergeFragmentDoesNotCollideWithCIFragments(t *testing.T) {
	stage := &stages.Stage{Name: "Implement"}
	body := "🏭 **Fabrik — merge state unknown**\n\n" + unknownMergeFragment(42, "deadbeefcafe0001") + " for 20m."
	item := gh.ProjectItem{Comments: []gh.Comment{{Body: body}}}
	if hasCIGatePauseComment(item, stage) {
		t.Error("an unknown-merge-state comment must not read as a CI-gate pause")
	}
	ci := gh.ProjectItem{Comments: []gh.Comment{{Body: "🏭 **Fabrik — CI wait timeout**\n\nThe CI gate for stage **Implement** timed out waiting for checks to pass."}}}
	if hasPauseComment(ci, unknownMergeFragment(42, "deadbeefcafe0001")) {
		t.Error("a CI-timeout comment must not suppress the unknown-merge-state pause")
	}
	other := gh.ProjectItem{Comments: []gh.Comment{{Body: body}}}
	if hasPauseComment(other, unknownMergeFragment(42, "0123456789abcdef")) {
		t.Error("a comment for another head must not suppress a fresh pause")
	}
}
