package sim

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/internal/localapi"
	"github.com/handarbeit/fabrik/tests/sim/simclaude"
)

// auditComments returns the issue's overseer audit comments.
func auditComments(t *testing.T, env *Env, num int) []string {
	t.Helper()
	comments, err := env.Sim.FetchIssueComments(env.Owner, env.Repo, num)
	if err != nil {
		t.Fatalf("FetchIssueComments: %v", err)
	}
	var out []string
	for _, c := range comments {
		if strings.Contains(c.Body, "overseer action:") {
			out = append(out, c.Body)
		}
	}
	return out
}

// TestOverseerAuditCommentDoesNotLiftAPause (#1969 R4): the audit comment an
// action posts lands after the pause, under the operator's own non-bot login in
// PAT mode, yet it must not resume the item or start a comment-review worker.
// The prefix check is the only protection; this drives the real engine poll.
func TestOverseerAuditCommentDoesNotLiftAPause(t *testing.T) {
	t.Parallel()
	env := NewEnv(t, EnvOptions{Stages: smokeStages()})
	env.Claude.ForStageComments("Specify", simclaude.CommentReviewCompleted())

	num := FileIssue(t, env, "overseer audit vs pause", "body", "Specify")
	for _, l := range []string{"fabrik:paused", "fabrik:awaiting-input"} {
		if err := env.Sim.Sim().AddLabelToIssue(env.Owner, env.Repo, num, l); err != nil {
			t.Fatalf("AddLabelToIssue(%s): %v", l, err)
		}
	}
	RunPolls(t, env, 2) // the daemon now knows the item
	stageCalls, commentCalls := env.Claude.StageCallCount("Specify"), env.Claude.CommentCallCount("Specify")

	act := env.Engine.OverseerActorForTest()
	res, err := act.SetAutonomy(localapi.SetAutonomyParams{Subscriber: "overseer-session", Issue: "acme/widgets#" + strconv.Itoa(num), Mode: "cruise"})
	if err != nil {
		t.Fatalf("SetAutonomy: %v", err)
	}
	if !res.Changed || res.AuditComment != localapi.AuditPosted {
		t.Fatalf("result = %+v", res)
	}
	audits := auditComments(t, env, num)
	if len(audits) != 1 || !strings.Contains(audits[0], "`overseer-session`") {
		t.Fatalf("audit comments = %q", audits)
	}

	// simgh authors the engine's own comments as "simgh-bot", which filterHuman
	// drops on the login alone. Under PAT mode the same comment is authored by
	// the operator's non-bot login, where only the body prefix protects it:
	// replay the real posted body under an operator login to exercise that.
	env.Clock.Advance(time.Minute)
	env.Sim.Sim().SeedComment(env.OwnerRepo, num, "the-operator", audits[0])
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("SeedComment: %v", err)
	}

	RunPolls(t, env, 6)
	labels := IssueLabels(t, env, num)
	if !hasLabel(labels, "fabrik:paused") || !hasLabel(labels, "fabrik:awaiting-input") {
		t.Fatalf("the audit comment lifted the pause; labels=%v", labels)
	}
	if !hasLabel(labels, "fabrik:cruise") {
		t.Errorf("the action's own label write is missing: %v", labels)
	}
	if got := env.Claude.CommentCallCount("Specify"); got != commentCalls {
		t.Errorf("comment-review worker ran for the audit comment: %d -> %d", commentCalls, got)
	}
	if got := env.Claude.StageCallCount("Specify"); got != stageCalls {
		t.Errorf("a stage worker ran for a paused item: %d -> %d", stageCalls, got)
	}
	if n := len(auditComments(t, env, num)); n != 2 {
		t.Errorf("%d audit-shaped comments after polling, want the posted one and the operator-authored replay", n)
	}
}

// TestOverseerAuditCommentStartsNoReviewCycleOnASettledItem: on an item that is
// not paused, the audit comment must not look like new feedback either — no
// comment-review invocation, no extra comments, no reaction churn.
func TestOverseerAuditCommentStartsNoReviewCycleOnASettledItem(t *testing.T) {
	t.Parallel()
	yolo := false
	env := NewEnv(t, EnvOptions{Stages: smokeStages(), Yolo: &yolo})
	env.Claude.ForStageComments("Specify", simclaude.CommentReviewCompleted())

	num := FileIssue(t, env, "overseer audit settled", "body", "Specify")
	WaitForIssueLabel(t, env, num, "stage:Specify:complete", 80)
	RunPolls(t, env, 2)
	commentCalls := env.Claude.CommentCallCount("Specify")

	act := env.Engine.OverseerActorForTest()
	if _, err := act.SetAutonomy(localapi.SetAutonomyParams{Subscriber: "overseer-session", Issue: strconv.Itoa(num), Mode: "yolo"}); err != nil {
		t.Fatalf("SetAutonomy: %v", err)
	}
	RunPolls(t, env, 6)
	if got := env.Claude.CommentCallCount("Specify"); got != commentCalls {
		t.Errorf("a comment-review cycle ran for the audit comment: %d -> %d", commentCalls, got)
	}
	if n := len(auditComments(t, env, num)); n != 1 {
		t.Errorf("%d audit comments, want 1", n)
	}
}
