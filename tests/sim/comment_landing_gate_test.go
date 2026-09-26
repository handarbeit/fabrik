package sim

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tests/sim/simclaude"
	"github.com/handarbeit/fabrik/tests/sim/simgh"
)

// #1862 sim scenarios: the Validate landing gate on unprocessed comments and the
// post-merge comment guard. Reproduces #1832 — a human comments on a
// Validate-complete cruise item and swaps fabrik:cruise for fabrik:yolo, and the
// engine must not merge (or advance to Queued) until the comment is processed.
//
// Assertions are scoped to engine decisions (mutation log ordering, worker
// invocation counts, reactions, comments posted), not GitHub merge semantics —
// see tests/sim/README.md and simgh/FIDELITY.md.

const gateHumanComment = "please also handle the edge case in the README"

// gateWorkerProbe is a Validate comment-review script that records the world as
// the comment worker sees it — how many merges have happened, the item's board
// column, and the SHA of the rework commit it makes — then delegates to the
// ordinary committing script.
type gateWorkerProbe struct {
	mu        sync.Mutex
	merges    int
	queued    int
	reworkSHA string
	workDir   string
	calls     int
}

func (p *gateWorkerProbe) script(env *Env, num int) simclaude.CommentScript {
	return func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.calls++
		p.merges = len(env.Sim.Log().ByMethod("MergePR"))
		p.queued = queuedMoves(env)
		out, completed, usage, err := simclaude.DefaultCommentScript(ctx, stage, issue, comments, workDir, opts)
		if err == nil {
			// A real comment worker commits AND pushes (the engine's comment path
			// pushes nothing itself), so the rework only reaches the PR if it does.
			if pushOut, perr := exec.Command("git", "-C", workDir, "push", "origin", "HEAD").CombinedOutput(); perr != nil {
				return "", false, engine.TokenUsage{}, fmt.Errorf("push rework: %v\n%s", perr, pushOut)
			}
			if sha, gerr := exec.Command("git", "-C", workDir, "rev-parse", "HEAD").Output(); gerr == nil {
				p.reworkSHA = strings.TrimSpace(string(sha))
				p.workDir = workDir
			}
		}
		return out, completed, usage, err
	}
}

// reachValidateCompleteUnderCruise files an issue with fabrik:cruise and drives
// it to stage:Validate:complete with the PR still open and unmerged.
func reachValidateCompleteUnderCruise(t *testing.T, env *Env, title string) int {
	t.Helper()
	num := FileIssue(t, env, title, "body", "Specify", "fabrik:cruise")
	for _, name := range []string{"Specify", "Research", "Plan", "Implement", "Review", "Validate"} {
		WaitForIssueLabel(t, env, num, "stage:"+name+":complete", 80)
	}
	pr, err := env.Sim.FetchLinkedPR(env.Owner, env.Repo, num)
	if err != nil || pr == nil || pr.Merged {
		t.Fatalf("expected an open linked PR at Validate-complete, got pr=%+v err=%v", pr, err)
	}
	return num
}

// swapCruiseForYolo is the ordinary way to approve with a last request.
func swapCruiseForYolo(t *testing.T, env *Env, num int) {
	t.Helper()
	if err := env.Sim.RemoveLabelFromIssue(env.Owner, env.Repo, num, "fabrik:cruise"); err != nil {
		t.Fatalf("remove cruise: %v", err)
	}
	if err := env.Sim.AddLabelToIssue(env.Owner, env.Repo, num, "fabrik:yolo"); err != nil {
		t.Fatalf("add yolo: %v", err)
	}
}

// queuedMoves counts board-status moves into the holding stage ("Queued") in the
// mutation log. Asserted on the log rather than the item's final column: with
// wait_for_ci off (this scenario's Validate) a successful advanceToQueued
// returns into handleStageComplete's ordinary advance, which is pre-existing
// behavior this scenario does not cover.
func queuedMoves(env *Env) int {
	n := 0
	for _, e := range env.Sim.Log().ByMethod("UpdateProjectItemStatus") {
		if e.Failed() {
			continue
		}
		if strings.HasSuffix(e.Args.ID, ":Queued") {
			n++
			continue
		}
		for _, v := range e.Args.Values {
			if strings.HasSuffix(v, ":Queued") {
				n++
				break
			}
		}
	}
	return n
}

func linkedPRMerged(env *Env, num int) bool {
	pr, err := env.Sim.FetchLinkedPR(env.Owner, env.Repo, num)
	return err == nil && pr != nil && pr.Merged
}

func gateEnv(t *testing.T) *Env {
	t.Helper()
	env := NewEnv(t, EnvOptions{Stages: smokeStages(), Yolo: boolPtr(false)})
	// Deterministic direct-merge fallback, as TestSmoke_FullPipelineToDone.
	env.Sim.Sim().SeedRepoAccess(env.OwnerRepo, gh.RepoAccess{AllowAutoMerge: false, CanPush: true})
	return env
}

// TestCommentLandingGate_HoldsMergeUntilProcessed is #1862 AC1: no merge until
// the comment is processed, and the rework lands in the merged result.
func TestCommentLandingGate_HoldsMergeUntilProcessed(t *testing.T) {
	t.Parallel()
	env := gateEnv(t)
	probe := &gateWorkerProbe{}
	realNum := FileIssue(t, env, "comment gate (#1832 repro)", "body", "Specify", "fabrik:cruise")
	env.Claude.ForStageComments("Validate", probe.script(env, realNum))
	for _, name := range []string{"Specify", "Research", "Plan", "Implement", "Review", "Validate"} {
		WaitForIssueLabel(t, env, realNum, "stage:"+name+":complete", 80)
	}
	pr, err := env.Sim.FetchLinkedPR(env.Owner, env.Repo, realNum)
	if err != nil || pr == nil || pr.Merged {
		t.Fatalf("expected an open linked PR at Validate-complete, got %+v err=%v", pr, err)
	}
	preHead := pr.HeadSHA

	// #1832: a human comments, then swaps cruise for yolo.
	env.Sim.Sim().SeedComment(env.OwnerRepo, realNum, "maintainer", gateHumanComment)
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("SeedComment: %v", err)
	}
	swapCruiseForYolo(t, env, realNum)

	AdvanceUntil(t, env, func(env *Env) bool { return linkedPRMerged(env, realNum) }, 60)

	probe.mu.Lock()
	defer probe.mu.Unlock()
	if probe.calls != 1 {
		t.Fatalf("Validate comment worker ran %d time(s), want exactly 1", probe.calls)
	}
	if probe.merges != 0 {
		t.Errorf("the PR was merged %d time(s) BEFORE the comment worker ran — the landing gate did not hold (#1862 AC1)", probe.merges)
	}
	merged, _ := env.Sim.FetchLinkedPR(env.Owner, env.Repo, realNum)
	if merged.HeadSHA == preHead {
		t.Errorf("merged head %s equals the pre-comment head %s — the rework (%s) never reached the merged PR", merged.HeadSHA, preHead, probe.reworkSHA)
	}
	if out, err := exec.Command("git", "-C", probe.workDir, "merge-base", "--is-ancestor", probe.reworkSHA, merged.HeadSHA).CombinedOutput(); err != nil {
		t.Errorf("rework commit %s is not an ancestor of the merged head %s: %v\n%s", probe.reworkSHA, merged.HeadSHA, err, out)
	}
	assertProcessedNotBounced(t, env, realNum)
}

// commentRocketed reports whether the engine added a 🚀 to the comment with the
// given body. Read from the mutation log rather than the comment's reaction
// groups: simgh stores the reaction content exactly as the REST call passed it
// ("rocket"), while the engine reads GraphQL's "ROCKET", so a reaction-group
// check would silently never match.
func commentRocketed(t *testing.T, env *Env, num int, body string) bool {
	t.Helper()
	comments, err := env.Sim.FetchIssueComments(env.Owner, env.Repo, num)
	if err != nil {
		t.Fatalf("FetchIssueComments: %v", err)
	}
	for _, c := range comments {
		if c.Body != body {
			continue
		}
		for _, e := range env.Sim.Log().ByMethod("AddCommentReaction") {
			if e.Args.Number != c.DatabaseID || e.Failed() {
				continue
			}
			for _, v := range e.Args.Values {
				if strings.EqualFold(v, "rocket") {
					return true
				}
			}
		}
	}
	return false
}

// assertProcessedNotBounced: the comment was answered by the worker (🚀) and the
// post-merge guard's "not applied" reply never fired.
func assertProcessedNotBounced(t *testing.T, env *Env, num int) {
	t.Helper()
	comments, err := env.Sim.FetchIssueComments(env.Owner, env.Repo, num)
	if err != nil {
		t.Fatalf("FetchIssueComments: %v", err)
	}
	for _, c := range comments {
		if strings.Contains(c.Body, "comment not applied") {
			t.Errorf("post-merge 'not applied' reply posted although the comment was processed before the merge: %q", c.Body)
		}
	}
	if !commentRocketed(t, env, num, gateHumanComment) {
		t.Error("the comment was never 🚀'd — it was not processed")
	}
}

// TestCommentLandingGate_HoldsMergeUntilProcessed_NonVacuous is the control: the
// identical timeline without a comment merges straight away, so the hold above
// is caused by the comment and by nothing else.
func TestCommentLandingGate_HoldsMergeUntilProcessed_NonVacuous(t *testing.T) {
	t.Parallel()
	env := gateEnv(t)
	num := reachValidateCompleteUnderCruise(t, env, "comment gate control (no comment)")
	swapCruiseForYolo(t, env, num)
	AdvanceUntil(t, env, func(env *Env) bool { return linkedPRMerged(env, num) }, 10)
	if got := env.Claude.CommentCallCount("Validate"); got != 0 {
		t.Errorf("control ran the comment worker %d time(s); there was no comment", got)
	}
}

// TestCommentLandingGate_HoldsAdvanceToQueued is #1862 AC2: with merge_train: on,
// no advance to Queued until the comment is processed.
func TestCommentLandingGate_HoldsAdvanceToQueued(t *testing.T) {
	t.Parallel()
	env := mergeTrainEnv(t, mergeTrainEnvOptions{ConfigureCfg: func(cfg *engine.Config) { cfg.Yolo = false }})
	probe := &gateWorkerProbe{}
	num := FileIssue(t, env, "comment gate (train)", "body", "Specify", "fabrik:cruise")
	env.Claude.ForStageComments("Validate", probe.script(env, num))
	for _, name := range []string{"Specify", "Research", "Plan", "Implement", "Review", "Validate"} {
		WaitForIssueLabel(t, env, num, "stage:"+name+":complete", 80)
	}
	if st := projectItem(t, env, num).Status; st != "Validate" {
		t.Fatalf("status = %q at Validate-complete under cruise, want Validate", st)
	}

	env.Sim.Sim().SeedComment(env.OwnerRepo, num, "maintainer", gateHumanComment)
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("SeedComment: %v", err)
	}
	swapCruiseForYolo(t, env, num)

	AdvanceUntil(t, env, func(env *Env) bool { return queuedMoves(env) > 0 }, 60)

	probe.mu.Lock()
	defer probe.mu.Unlock()
	if probe.calls != 1 {
		t.Fatalf("Validate comment worker ran %d time(s), want exactly 1", probe.calls)
	}
	if probe.queued != 0 {
		t.Errorf("the item had already been moved to Queued %d time(s) when the comment worker ran — it advanced with the comment unprocessed (#1862 AC2)", probe.queued)
	}
}

// TestCommentLandingGate_HoldsAdvanceToQueued_NonVacuous: without a comment the
// same setup advances to Queued at once.
func TestCommentLandingGate_HoldsAdvanceToQueued_NonVacuous(t *testing.T) {
	t.Parallel()
	env := mergeTrainEnv(t, mergeTrainEnvOptions{ConfigureCfg: func(cfg *engine.Config) { cfg.Yolo = false }})
	num := reachValidateCompleteUnderCruise(t, env, "comment gate train control")
	swapCruiseForYolo(t, env, num)
	AdvanceUntil(t, env, func(env *Env) bool { return queuedMoves(env) > 0 }, 10)
	if got := env.Claude.CommentCallCount("Validate"); got != 0 {
		t.Errorf("control ran the comment worker %d time(s); there was no comment", got)
	}
}

// TestCommentLandingGate_NonActionableCommentsNeverBlock is #1862 AC3: an engine
// comment, a bot service notice and a bot review summary never hold a landing.
func TestCommentLandingGate_NonActionableCommentsNeverBlock(t *testing.T) {
	t.Parallel()
	env := gateEnv(t)
	num := reachValidateCompleteUnderCruise(t, env, "comment gate non-actionable")

	sim := env.Sim.Sim()
	sim.SeedComment(env.OwnerRepo, num, "arbeithand", "🏭 **Fabrik — stage: Validate**\nreport")
	sim.SeedComment(env.OwnerRepo, num, "gemini-code-assist[bot]", "You have reached your daily quota limit.")
	if err := sim.Err(); err != nil {
		t.Fatalf("SeedComment: %v", err)
	}
	swapCruiseForYolo(t, env, num)

	AdvanceUntil(t, env, func(env *Env) bool { return linkedPRMerged(env, num) }, 10)
	if got := env.Claude.CommentCallCount("Validate"); got != 0 {
		t.Errorf("comment worker ran %d time(s) for engine/bot-only comments", got)
	}
}

// TestPostMergeGuard_LateCommentIsNotApplied is #1862 AC4: a comment picked up
// after the merge makes no worker invocation, pushes nothing, is answered with
// the "not applied" reply, and gets no 🚀.
func TestPostMergeGuard_LateCommentIsNotApplied(t *testing.T) {
	t.Parallel()
	env := gateEnv(t)
	num := reachValidateCompleteUnderCruise(t, env, "post-merge guard")
	pr, _ := env.Sim.FetchLinkedPR(env.Owner, env.Repo, num)
	prNum := pr.Number

	// Move the sim clock so the merge/reopen/comment below carry a fresh
	// updatedAt: stamped at the same instant as the last poll's own writes they
	// would look unchanged and the item would never be re-admitted.
	env.Clock.Advance(time.Second)

	// The PR merges out from under the item (a human merge / a race) with the
	// issue still open at Validate, then a comment arrives.
	if err := env.Sim.MergePR(env.Owner, env.Repo, prNum); err != nil {
		t.Fatalf("MergePR: %v", err)
	}
	if err := env.Sim.ReopenIssue(env.Owner, env.Repo, num); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	env.Sim.Sim().SeedComment(env.OwnerRepo, num, "maintainer", gateHumanComment)
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("SeedComment: %v", err)
	}
	headBefore, _ := env.Sim.Sim().HeadSHA(env.OwnerRepo, "fabrik/issue-"+strconv.Itoa(num))

	// The comment is picked up once the item is re-admitted (a sim artifact of
	// updatedAt granularity, not engine behavior); then keep polling to prove the
	// un-🚀'd comment is answered exactly once, not on every poll.
	AdvanceUntil(t, env, func(env *Env) bool { return hasCommentContaining(t, env, num, "comment not applied") }, 60)
	RunPolls(t, env, 4)

	if got := env.Claude.CommentCallCount("Validate"); got != 0 {
		t.Errorf("comment worker ran %d time(s) after the merge — must never invoke a worker (AC4)", got)
	}
	if headAfter, _ := env.Sim.Sim().HeadSHA(env.OwnerRepo, "fabrik/issue-"+strconv.Itoa(num)); headAfter != headBefore {
		t.Errorf("branch tip moved %s -> %s after the merge — nothing may be pushed (AC4)", headBefore, headAfter)
	}
	comments, _ := env.Sim.FetchIssueComments(env.Owner, env.Repo, num)
	if commentRocketed(t, env, num, gateHumanComment) {
		t.Error("the un-applied comment got a 🚀 — it must not look processed (AC4)")
	}
	replies := 0
	for _, c := range comments {
		if strings.Contains(c.Body, "comment not applied") {
			replies++
		}
	}
	if replies != 1 {
		t.Errorf("issue carries %d 'not applied' reply(ies), want exactly 1 (durable dedupe across polls)", replies)
	}
	if entries := env.Sim.Log().Find(simgh.And(simgh.MethodIs("AddComment"), simgh.OnIssue(prNum))); len(entries) == 0 {
		t.Error("no reply was posted on the PR")
	}
}

// TestPostMergeGuard_MergeTrainMemberIsNotApplied is AC4 for a merge-train
// member: its own PR stays closed-not-merged after the train lands it via an
// integration PR, so the durable "already landed" signal is the credited-PR
// label (ADR-1616), not the member PR's merge state. The member's own PR is
// deliberately still open here, so the label is the only evidence.
func TestPostMergeGuard_MergeTrainMemberIsNotApplied(t *testing.T) {
	t.Parallel()
	env := gateEnv(t)
	num := reachValidateCompleteUnderCruise(t, env, "post-merge guard (train member)")
	pr, _ := env.Sim.FetchLinkedPR(env.Owner, env.Repo, num)

	env.Clock.Advance(time.Second)
	if err := env.Sim.AddLabelToIssue(env.Owner, env.Repo, num, "fabrik:credited-pr:"+strconv.Itoa(pr.Number+1000)); err != nil {
		t.Fatalf("add credited label: %v", err)
	}
	env.Sim.Sim().SeedComment(env.OwnerRepo, num, "maintainer", gateHumanComment)
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("SeedComment: %v", err)
	}

	AdvanceUntil(t, env, func(env *Env) bool { return hasCommentContaining(t, env, num, "comment not applied") }, 60)
	RunPolls(t, env, 3)

	if got := env.Claude.CommentCallCount("Validate"); got != 0 {
		t.Errorf("comment worker ran %d time(s) for a landed merge-train member (AC4)", got)
	}
	if commentRocketed(t, env, num, gateHumanComment) {
		t.Error("the un-applied comment got a 🚀 (AC4)")
	}
	if n := countCommentsContaining(t, env, num, "comment not applied"); n != 1 {
		t.Errorf("%d 'not applied' reply(ies), want exactly 1", n)
	}
}

func countCommentsContaining(t *testing.T, env *Env, num int, substr string) int {
	t.Helper()
	n := 0
	for _, c := range commentsOn(t, env, num) {
		if strings.Contains(c.Body, substr) {
			n++
		}
	}
	return n
}
