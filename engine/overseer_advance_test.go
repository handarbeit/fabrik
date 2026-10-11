package engine

import (
	"errors"
	"strings"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/internal/localapi"
	"github.com/handarbeit/fabrik/stages"
)

// advEnv seeds item #5 in status with cached and live labels both set to
// labels, and a live column read that answers status. Every live read the
// success path makes is stubbed, so a refusal can never pass for the wrong
// reason (an unstubbed FetchLabels fails closed with an error).
func advEnv(t *testing.T, status string, labels ...string) (*Engine, *mockGitHubClient, *overseerActor) {
	t.Helper()
	client := &mockGitHubClient{}
	liveStatus(client, status)
	liveLabels(client, labels...)
	eng, _, act := overseerEngine(t, client, ovItem{number: 5, status: status, labels: labels})
	return eng, client, act
}

func advanceReq() localapi.AdvanceParams {
	return localapi.AdvanceParams{Subscriber: "my-session", Issue: "5"}
}

func TestOverseerAdvance_MovesOneStageThroughTheEnginesAdvance(t *testing.T) {
	eng, client, act := advEnv(t, "Specify", "stage:Specify:complete")

	res, err := act.Advance(advanceReq())
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if !res.Changed || res.Action != localapi.MethodAdvance || res.Issue != "owner/repo#5" || res.AuditComment != localapi.AuditPosted {
		t.Errorf("result = %+v", res)
	}
	want := []updateStatusCall{{ovProjectID, "PVTI_5", "FIELD_1", "OPT_Implement"}}
	if len(client.updateStatusCalls) != 1 || client.updateStatusCalls[0] != want[0] {
		t.Errorf("status writes = %v, want %v", client.updateStatusCalls, want)
	}
	if len(client.addLabelCalls)+len(client.removeLabelCalls) != 0 {
		t.Errorf("advance must write no labels: %v %v", client.addLabelCalls, client.removeLabelCalls)
	}
	bodies := commentBodies(client)
	if len(bodies) != 1 || !strings.HasPrefix(bodies[0], "🏭 **Fabrik — overseer action: advance**") ||
		!strings.Contains(bodies[0], "from Specify to Implement") || !strings.Contains(bodies[0], "`my-session`") {
		t.Errorf("audit comment = %q", bodies)
	}
	if got := statusOf(t, eng, 5); got != "Implement" {
		t.Errorf("cache status = %q, want Implement", got)
	}
	// The live reads the action promises: one column read and one details read.
	if len(client.fetchProjectItemStatusCalls) != 1 {
		t.Errorf("FetchProjectItemStatus calls = %v, want exactly one", client.fetchProjectItemStatusCalls)
	}
	if len(client.fetchItemDetailsCalls) < 1 {
		t.Error("the dependency and feedback gates must read the item live")
	}
	if len(res.Notes) != 0 {
		t.Errorf("no autonomy note expected, got %v", res.Notes)
	}
}

func TestOverseerAdvance_AutonomyIsNotedNotRefusedOrChanged(t *testing.T) {
	for _, mode := range []string{"cruise", "yolo"} {
		t.Run(mode, func(t *testing.T) {
			_, client, act := advEnv(t, "Specify", "stage:Specify:complete", "fabrik:"+mode)
			res, err := act.Advance(advanceReq())
			if err != nil {
				t.Fatalf("Advance: %v", err)
			}
			if !strings.Contains(strings.Join(res.Notes, " "), "fabrik:"+mode) {
				t.Errorf("notes = %v", res.Notes)
			}
			if len(client.addLabelCalls)+len(client.removeLabelCalls) != 0 {
				t.Errorf("autonomy labels must be untouched")
			}
		})
	}
}

func TestOverseerAdvance_RefusalsSayWhyAndWriteNothing(t *testing.T) {
	tests := []struct {
		name   string
		status string
		labels []string
		setup  func(eng *Engine, c *mockGitHubClient)
		want   string
	}{
		{name: "parking column points to promote", status: "Backlog", want: "fabrik_promote"},
		{name: "Validate points to the PR merge", status: "Validate", labels: []string{"stage:Validate:complete"}, want: "Validate → use the PR merge"},
		{name: "cleanup column", status: "Done", want: "final column"},
		{name: "holding stage", status: "Queued", want: "holding stage"},
		{name: "column with no stage", status: "Mystery", want: "no configured stage"},
		{name: "stage not complete", status: "Specify", want: "stage not complete"},
		{name: "in-progress label means a worker is running", status: "Specify", labels: []string{"stage:Specify:in_progress"}, want: "a worker is running"},
		{name: "editing", status: "Specify", labels: []string{"stage:Specify:complete", "fabrik:editing"}, want: "worker is in flight"},
		{name: "locked", status: "Specify", labels: []string{"stage:Specify:complete", "fabrik:locked:someone"}, want: "worker is in flight"},
		{name: "paused", status: "Specify", labels: []string{"stage:Specify:complete", "fabrik:paused", "fabrik:awaiting-input"}, want: "a human comment resumes it"},
		{name: "awaiting input alone", status: "Specify", labels: []string{"stage:Specify:complete", "fabrik:awaiting-input"}, want: "a human comment resumes it"},
		{name: "blocked label", status: "Specify", labels: []string{"stage:Specify:complete", "fabrik:blocked"}, want: "fabrik:blocked"},
		{name: "CI wait", status: "Specify", labels: []string{"stage:Specify:complete", "fabrik:awaiting-ci"}, want: "CI wait"},
		{name: "review wait label", status: "Specify", labels: []string{"stage:Specify:complete", "fabrik:awaiting-review"}, want: "review wait"},
		{name: "rebase needed", status: "Specify", labels: []string{"stage:Specify:complete", "fabrik:rebase-needed"}, want: "rebase"},
		{name: "auto-merge enabled", status: "Specify", labels: []string{"stage:Specify:complete", "fabrik:auto-merge-enabled"}, want: "auto-merge"},
		{
			name: "unresolved dependency read live", status: "Specify", labels: []string{"stage:Specify:complete"},
			setup: func(_ *Engine, c *mockGitHubClient) {
				c.fetchItemDetailsFn = func(it *gh.ProjectItem) error {
					it.BlockedBy = []gh.Dependency{{Number: 9, Repo: "owner/repo", State: "OPEN"}}
					return nil
				}
			},
			want: "waiting for #9",
		},
		{
			name: "unprocessed human comment", status: "Specify", labels: []string{"stage:Specify:complete"},
			setup: func(_ *Engine, c *mockGitHubClient) {
				c.fetchItemDetailsFn = func(it *gh.ProjectItem) error {
					it.Comments = []gh.Comment{{ID: "C_1", DatabaseID: 1, Body: "wait, one more thing", Author: "alice", CreatedAt: time.Now()}}
					return nil
				}
			},
			want: "unprocessed comments or review feedback",
		},
		{
			name: "live details read fails", status: "Specify", labels: []string{"stage:Specify:complete"},
			setup: func(_ *Engine, c *mockGitHubClient) {
				c.fetchItemDetailsFn = func(*gh.ProjectItem) error { return errors.New("boom") }
			},
			want: "refusing rather than skip a gate",
		},
		{
			name: "live column read fails", status: "Specify", labels: []string{"stage:Specify:complete"},
			setup: func(_ *Engine, c *mockGitHubClient) {
				c.fetchProjectItemStatusFn = func(string) (string, error) { return "", errors.New("boom") }
			},
			want: "could not confirm",
		},
		{
			name: "live label read fails", status: "Specify", labels: []string{"stage:Specify:complete"},
			setup: func(_ *Engine, c *mockGitHubClient) {
				c.fetchLabelsFn = func(_, _ string, _ int) ([]string, error) { return nil, errors.New("boom") }
			},
			want: "refusing rather than advance from a possibly stale cache",
		},
		{
			name: "cache says Specify but the live column is Validate", status: "Specify", labels: []string{"stage:Specify:complete"},
			setup: func(_ *Engine, c *mockGitHubClient) { liveStatus(c, "Validate") },
			want:  "use the PR merge",
		},
		{
			name: "cache says complete but the live labels do not", status: "Specify", labels: []string{"stage:Specify:complete"},
			setup: func(_ *Engine, c *mockGitHubClient) { liveLabels(c) },
			want:  "stage not complete",
		},
		{
			name: "cache says clear but a pause landed", status: "Specify", labels: []string{"stage:Specify:complete"},
			setup: func(_ *Engine, c *mockGitHubClient) {
				liveLabels(c, "stage:Specify:complete", "fabrik:paused", "fabrik:awaiting-input")
			},
			want: "a human comment resumes it",
		},
		{
			name: "next stage has no board column", status: "Specify", labels: []string{"stage:Specify:complete"},
			setup: func(eng *Engine, _ *mockGitHubClient) {
				eng.mu.Lock()
				delete(eng.statusField.Options, "Implement")
				eng.mu.Unlock()
			},
			want: "no \"Implement\" column",
		},
		{
			name: "worker in flight with a stale completion label", status: "Specify", labels: []string{"stage:Specify:complete"},
			setup: func(eng *Engine, _ *mockGitHubClient) {
				eng.store.Apply(itemstate.WorkerEntered{Repo: "owner/repo", Number: 5, StageName: "Specify", StartedAt: time.Now()})
			},
			want: "worker is in flight",
		},
		{
			name: "worker in flight, stage not complete", status: "Specify",
			setup: func(eng *Engine, _ *mockGitHubClient) {
				eng.store.Apply(itemstate.WorkerEntered{Repo: "owner/repo", Number: 5, StageName: "Specify", StartedAt: time.Now()})
			},
			want: "a worker is running",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			eng, client, act := advEnv(t, tc.status, tc.labels...)
			if tc.setup != nil {
				tc.setup(eng, client)
			}
			_, err := act.Advance(advanceReq())
			pe, st := refusedErr(t, err)
			if !strings.Contains(pe.Message, tc.want) {
				t.Errorf("message %q does not contain %q", pe.Message, tc.want)
			}
			if st.Issue != "owner/repo#5" {
				t.Errorf("a refusal must carry the item's state, got %+v", st)
			}
			if len(st.ValidTargets) != 0 {
				t.Errorf("advance never lists valid targets: %v", st.ValidTargets)
			}
			wantNoWrites(t, client)
		})
	}
}

func TestOverseerAdvance_RefusesClosedItemAndPullRequest(t *testing.T) {
	client := &mockGitHubClient{}
	_, _, act := overseerEngine(t, client,
		ovItem{number: 5, status: "Specify", closed: true},
		ovItem{number: 6, status: "Specify", pr: true})
	if _, err := act.Advance(localapi.AdvanceParams{Subscriber: "s", Issue: "5"}); err == nil {
		t.Error("a closed issue must be refused")
	}
	if _, err := act.Advance(localapi.AdvanceParams{Subscriber: "s", Issue: "6"}); err == nil {
		t.Error("a pull request must be refused")
	}
	wantNoWrites(t, client)
}

func TestOverseerAdvance_RequiresAnAttributableRequester(t *testing.T) {
	_, client, act := advEnv(t, "Specify", "stage:Specify:complete")
	if _, err := act.Advance(localapi.AdvanceParams{Issue: "5"}); err == nil {
		t.Fatal("a missing requester must be rejected")
	}
	wantNoWrites(t, client)
}

// When the cache lags, the live column is what is advanced from: an item the
// engine already moved to Implement advances to Validate, never straight from a
// stale Specify.
func TestOverseerAdvance_LiveColumnDecidesWhatIsCurrent(t *testing.T) {
	_, client, act := advEnv(t, "Specify", "stage:Specify:complete")
	liveStatus(client, "Implement")
	liveLabels(client, "stage:Implement:complete")
	if _, err := act.Advance(advanceReq()); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if len(client.updateStatusCalls) != 1 || client.updateStatusCalls[0].optionID != "OPT_Validate" {
		t.Errorf("status writes = %v, want one write to Validate", client.updateStatusCalls)
	}
}

// A second advance right after the first is refused: the live column is then
// Implement with no completion label, so the item can never end up two stages
// ahead.
func TestOverseerAdvance_SecondRequestDoesNotDoubleAdvance(t *testing.T) {
	_, client, act := advEnv(t, "Specify", "stage:Specify:complete")
	if _, err := act.Advance(advanceReq()); err != nil {
		t.Fatalf("first Advance: %v", err)
	}
	liveStatus(client, "Implement")
	liveLabels(client, "stage:Specify:complete")
	_, err := act.Advance(advanceReq())
	pe, _ := refusedErr(t, err)
	if !strings.Contains(pe.Message, "stage not complete") {
		t.Errorf("second advance: %q", pe.Message)
	}
	if len(client.updateStatusCalls) != 1 {
		t.Errorf("status writes = %v, want exactly one", client.updateStatusCalls)
	}
}

// The review wait is not covered by stage:X:complete (it is added before the
// reviewers are recorded as outstanding), so the overseer evaluates it itself —
// through the read-only core, writing nothing.
func TestOverseerAdvance_ReviewWaitHoldsAndAppliesNoLabel(t *testing.T) {
	_, client, act := advEnv(t, "Implement", "stage:Implement:complete")
	act.e.cfg.Stages = stagesWithReviewWait()
	client.fetchPRReviewRequestsFn = func(string, string, int) ([]gh.ReviewRequest, error) {
		return []gh.ReviewRequest{{Login: "alice"}}, nil
	}
	client.fetchLinkedPRFn = func(string, string, int) (*gh.PRDetails, error) {
		return &gh.PRDetails{Number: 77, State: "open"}, nil
	}
	_, err := act.Advance(advanceReq())
	pe, _ := refusedErr(t, err)
	if !strings.Contains(pe.Message, "review wait") || !strings.Contains(pe.Message, "alice") {
		t.Errorf("message = %q", pe.Message)
	}
	wantNoWrites(t, client)
}

func TestOverseerAdvance_ReviewWaitClearedAllowsAdvance(t *testing.T) {
	_, client, act := advEnv(t, "Implement", "stage:Implement:complete")
	act.e.cfg.Stages = stagesWithReviewWait()
	client.fetchPRReviewsFn = func(string, string, int) ([]gh.PRReview, error) {
		return []gh.PRReview{{Author: "alice", State: "APPROVED"}}, nil
	}
	client.fetchLinkedPRFn = func(string, string, int) (*gh.PRDetails, error) {
		return &gh.PRDetails{Number: 77, State: "open"}, nil
	}
	if _, err := act.Advance(advanceReq()); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if len(client.updateStatusCalls) != 1 {
		t.Errorf("status writes = %v", client.updateStatusCalls)
	}
}

func stagesWithReviewWait() []*stages.Stage {
	ss := apiStages()
	for _, s := range ss {
		if s.Name == "Implement" {
			s.WaitForReviews = boolPtr(true)
		}
	}
	return ss
}

// The move stands when the audit comment cannot be posted, and the result says so.
func TestOverseerAdvance_AuditCommentFailureLeavesTheMove(t *testing.T) {
	_, client, act := advEnv(t, "Specify", "stage:Specify:complete")
	client.addCommentFn = func(string, string, int, string) (int, error) { return 0, errors.New("nope") }
	res, err := act.Advance(advanceReq())
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if res.AuditComment != localapi.AuditFailed || !res.Changed {
		t.Errorf("result = %+v", res)
	}
	if len(client.updateStatusCalls) != 1 {
		t.Errorf("status writes = %v", client.updateStatusCalls)
	}
}

// A failed status write returns an error and posts no audit comment.
func TestOverseerAdvance_StatusWriteFailureWritesNoComment(t *testing.T) {
	_, client, act := advEnv(t, "Specify", "stage:Specify:complete")
	client.updateProjectItemStatusFn = func(string, string, string, string) error { return errors.New("rate limited") }
	if _, err := act.Advance(advanceReq()); err == nil {
		t.Fatal("expected an error")
	}
	if got := commentBodies(client); len(got) != 0 {
		t.Errorf("comment posted after a failed move: %v", got)
	}
}

// R3/FR-016: promote's pipeline-column refusal names fabrik_advance and offers
// no valid_targets; a parking column with a bad target still lists them.
func TestOverseerPromote_PipelineColumnRefusalPointsToAdvance(t *testing.T) {
	client := &mockGitHubClient{}
	liveStatus(client, "Specify")
	_, _, act := overseerEngine(t, client, ovItem{number: 5, status: "Specify", labels: []string{"stage:Specify:complete"}})
	_, err := act.Promote(localapi.PromoteParams{Subscriber: "s", Issue: "5", To: "Implement"})
	pe, st := refusedErr(t, err)
	if !strings.Contains(pe.Message, "already in the pipeline column") || !strings.Contains(pe.Message, "fabrik_advance") || len(st.ValidTargets) != 0 || strings.Contains(pe.Message, "valid:") {
		t.Errorf("message %q state %+v", pe.Message, st)
	}
	wantNoWrites(t, client)

	client2 := &mockGitHubClient{}
	liveStatus(client2, "Backlog")
	_, _, act2 := overseerEngine(t, client2, ovItem{number: 5, status: "Backlog"})
	_, err = act2.Promote(localapi.PromoteParams{Subscriber: "s", Issue: "5", To: "Queued"})
	pe2, st2 := refusedErr(t, err)
	if len(st2.ValidTargets) == 0 || strings.Contains(pe2.Message, "fabrik_advance") {
		t.Errorf("parking-column refusal changed: %q %+v", pe2.Message, st2)
	}
}
