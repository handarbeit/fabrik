package sim

import (
	"context"
	"fmt"
	"os/exec"
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

// #1863 sim scenarios: an unprocessed human comment on a Queued merge-train member
// ejects it off Queued so the ordinary comment path can act on it, and the member
// re-queues once Validate completes again. The eject is not train churn — it never
// pauses the member and never counts toward MaxMergeTrainEjections.
//
// Members are seeded with QueueMember, which bypasses the pipeline, so each scenario
// adds stage:Validate:complete (the state a real member carries when it reaches
// Queued) and a Validate comment script that commits a rework to the member branch.

const queuedHumanComment = "please also handle the edge case in the README"

// queuedCommentEnv is a merge-train env whose Validate stage waits for CI (so a member
// re-queues only after its rework's CI is green, and the fresh-batch admission gate is
// live). The clock starts at real now so the awaiting-ci backstop (real time.Since against
// the clock-stamped label time) does not fire on the first settle pass.
func queuedCommentEnv(t *testing.T, configure ...func(*engine.Config)) *Env {
	t.Helper()
	env := mergeTrainEnv(t, mergeTrainEnvOptions{
		ValidateWaitForCI: true,
		StartTime:         time.Now(),
		ConfigureCfg: func(cfg *engine.Config) {
			for _, c := range configure {
				c(cfg)
			}
		},
	})
	// Stands in for the cache write-through and its reactive observers, neither of which
	// tests/sim wires in — see cacheWriteThrough.
	t.Cleanup(env.Engine.RegisterObservers())
	return env
}

// delayedGreenVerdict is allGreenVerdict held back briefly, so a comment flagged by the
// same poll's settle scan is always pending before the worker reaches its checkpoint.
func delayedGreenVerdict(members []int) []gh.CheckRun {
	time.Sleep(100 * time.Millisecond)
	return allGreenVerdict(members)
}

// countComments counts comments on num whose body contains substr.
func countComments(t *testing.T, env *Env, num int, substr string) int {
	t.Helper()
	n := 0
	for _, c := range commentsOn(t, env, num) {
		if strings.Contains(c.Body, substr) {
			n++
		}
	}
	return n
}

// assertReworkOnMain checks the comment worker's rework for num is on main.
func assertReworkOnMain(t *testing.T, env *Env, num int) {
	t.Helper()
	bare, err := env.Sim.Sim().RepoBareDir(env.OwnerRepo)
	if err != nil {
		t.Fatalf("RepoBareDir: %v", err)
	}
	if _, err := gitShowFile(t, bare, "main", ".simclaude/Validate-comment.md"); err != nil {
		t.Errorf("the comment worker's rework never reached main: %v", err)
	}
}

// assertNotTrainChurn: the member was neither paused nor charged an ejection — the
// churn wording ("merge-train — ejected", "has left the Queued column") never appears.
func assertNotTrainChurn(t *testing.T, env *Env, num int) {
	t.Helper()
	labels := IssueLabels(t, env, num)
	if hasLabel(labels, "fabrik:paused") || hasLabel(labels, "fabrik:awaiting-input") {
		t.Errorf("#%d must not be paused by a comment eject, labels: %v", num, labels)
	}
	if hasCommentContaining(t, env, num, "merge-train — ejected**") || hasCommentContaining(t, env, num, "has left the Queued column") {
		t.Errorf("#%d got train-churn ejection wording for a comment eject", num)
	}
}

// reworkRecorder is a Validate comment script that does what a real comment worker does —
// commits a rework, pushes it, and (Validate has wait_for_ci here) the member's PR gets a
// green check run on the new head — recording each call for the scenario to assert on.
type reworkRecorder struct {
	mu    sync.Mutex
	calls []int
	env   *Env
}

func (r *reworkRecorder) script(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
	// QueueMember seeds the member's branch on the remote without a local worktree, so the
	// worktree the comment path creates starts from main; put it on the member's real tip
	// (a fresh worktree, nothing local to lose) before making the rework on top of it.
	branch := fmt.Sprintf("fabrik/issue-%d", issue.Number)
	if o, gerr := exec.Command("git", "-C", workDir, "fetch", "origin", branch).CombinedOutput(); gerr != nil {
		return "", false, engine.TokenUsage{}, fmt.Errorf("fetch member branch: %v\n%s", gerr, o)
	}
	if o, gerr := exec.Command("git", "-C", workDir, "reset", "--hard", "FETCH_HEAD").CombinedOutput(); gerr != nil {
		return "", false, engine.TokenUsage{}, fmt.Errorf("reset to member tip: %v\n%s", gerr, o)
	}
	out, completed, usage, err := simclaude.DefaultCommentScript(ctx, stage, issue, comments, workDir, opts)
	if err != nil {
		return out, completed, usage, err
	}
	if pushOut, perr := exec.Command("git", "-C", workDir, "push", "origin", "HEAD").CombinedOutput(); perr != nil {
		return "", false, engine.TokenUsage{}, fmt.Errorf("push rework: %v\n%s", perr, pushOut)
	}
	sha, gerr := exec.Command("git", "-C", workDir, "rev-parse", "HEAD").Output()
	if gerr != nil {
		return "", false, engine.TokenUsage{}, gerr
	}
	r.env.Sim.Sim().SeedCheckRun(r.env.OwnerRepo, strings.TrimSpace(string(sha)), greenCheckRun(""))
	r.mu.Lock()
	r.calls = append(r.calls, issue.Number)
	r.mu.Unlock()
	return out, completed, usage, nil
}

func (r *reworkRecorder) count(num int) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.calls {
		if c == num {
			n++
		}
	}
	return n
}

// queuedCommentMember queues a member as a real one would look on entering Queued:
// Validate-complete, with yolo so it re-queues automatically once its comment is
// processed. Registers the Validate comment script on env's Claude.
func queuedCommentMember(t *testing.T, env *Env, marker string) int {
	t.Helper()
	num, _ := QueueMember(t, env, marker, map[string]string{marker + ".txt": marker + "\n"})
	for _, l := range []string{"stage:Validate:complete", "fabrik:yolo"} {
		if err := env.Sim.AddLabelToIssue(env.Owner, env.Repo, num, l); err != nil {
			t.Fatalf("add %s: %v", l, err)
		}
	}
	seedMemberCI(t, env, num, greenCheckRun(""))
	return num
}

// seedQueuedComment posts a comment on num and advances the sim clock so the item's
// updatedAt moves (a comment stamped at the last poll's own instant would look
// unchanged and never be re-admitted).
func seedQueuedComment(t *testing.T, env *Env, num int, author, body string) {
	t.Helper()
	env.Clock.Advance(time.Second)
	env.Sim.Sim().SeedComment(env.OwnerRepo, num, author, body)
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("SeedComment: %v", err)
	}
}

// cacheWriteThrough stands in for boardcache.CacheImpl's status write-through (tests/sim
// never wires the cache in), so the next poll admits the rerouted member. The scenario
// must also have registered the engine's reactive observers (env.Engine.RegisterObservers,
// see NewEnv's doc comment): the StatusChanged this raises reaches the poll's cycleSet only
// through the mayNeedWork observer.
func cacheWriteThrough(env *Env, num int, status string) {
	env.Engine.SimulateCacheStatusWriteThroughForTest(env.OwnerRepo, num, status)
}

func ejectCommentPosted(t *testing.T, env *Env, num int) bool {
	return hasCommentContaining(t, env, num, "ejected (unprocessed comment)")
}

// TestQueuedCommentEject_WorkerInFlight_BatchContinues is Acceptance 2: both members are
// in the live worker's batch, the human comment on #B is flagged by the settle scan as a
// pending signal, the worker applies it at its checkpoint (discarding the trial), and #A
// — the rest of the batch — continues and lands. #B's comment is then processed by the
// ordinary path and #B re-queues and lands with the change.
func TestQueuedCommentEject_WorkerInFlight_BatchContinues(t *testing.T) {
	t.Parallel()
	env := queuedCommentEnv(t)
	rework := &reworkRecorder{env: env}
	env.Claude.ForStageComments("Validate", rework.script)
	a := queuedCommentMember(t, env, "qc-a")
	b := queuedCommentMember(t, env, "qc-b")
	seedQueuedComment(t, env, b, "maintainer", queuedHumanComment)
	startTrialVerdictSeeder(t, env, delayedGreenVerdict)

	RunPoll(t, env)
	if !ejectCommentPosted(t, env, b) {
		t.Fatal("expected #B to be ejected for its unprocessed comment")
	}
	if ejectCommentPosted(t, env, a) {
		t.Error("#A has no comment and must not be ejected")
	}
	cacheWriteThrough(env, b, "Validate")

	AdvanceUntil(t, env, func(env *Env) bool {
		return projectItem(t, env, a).IsClosed && projectItem(t, env, b).IsClosed
	}, 120)

	if got := rework.count(b); got != 1 {
		t.Errorf("#B's comment worker ran %d time(s), want exactly 1", got)
	}
	if got := rework.count(a); got != 0 {
		t.Errorf("#A had no comment but its comment worker ran %d time(s)", got)
	}
	if !commentRocketed(t, env, b, queuedHumanComment) {
		t.Error("#B's comment was never 🚀'd — it was not processed")
	}
	if n := countComments(t, env, b, "ejected (unprocessed comment)"); n != 1 {
		t.Errorf("#B got %d eject comments, want exactly 1", n)
	}
	assertReworkOnMain(t, env, b)
	assertNotTrainChurn(t, env, a)
	assertNotTrainChurn(t, env, b)
}

// TestQueuedCommentEject_NotInLiveBatch_EjectedDirectly is Acceptance 1: #B is Queued but
// beyond the live worker's dispatched batch (MaxBatchSize 1, queue sort disabled so #A
// is the batch), so the scan ejects it directly; its comment is processed, it re-queues
// and lands, and #A's batch is unaffected.
func TestQueuedCommentEject_NotInLiveBatch_EjectedDirectly(t *testing.T) {
	t.Parallel()
	env := queuedCommentEnv(t, func(cfg *engine.Config) { cfg.MaxBatchSize = 1 })
	env.Engine.SetMergeTrainQueueSortDisabledForTest(true)
	rework := &reworkRecorder{env: env}
	env.Claude.ForStageComments("Validate", rework.script)
	a := queuedCommentMember(t, env, "qc-direct-a")
	b := queuedCommentMember(t, env, "qc-direct-b")
	seedQueuedComment(t, env, b, "maintainer", queuedHumanComment)
	startTrialVerdictSeeder(t, env, allGreenVerdict)

	RunPoll(t, env)
	if !ejectCommentPosted(t, env, b) {
		t.Fatal("expected #B (beyond the batch cap) to be ejected directly by the scan")
	}
	cacheWriteThrough(env, b, "Validate")

	AdvanceUntil(t, env, func(env *Env) bool {
		return projectItem(t, env, a).IsClosed && projectItem(t, env, b).IsClosed
	}, 120)

	if got := rework.count(b); got != 1 {
		t.Errorf("#B's comment worker ran %d time(s), want exactly 1", got)
	}
	if got := rework.count(a); got != 0 {
		t.Errorf("#A had no comment but its comment worker ran %d time(s)", got)
	}
	if !commentRocketed(t, env, b, queuedHumanComment) {
		t.Error("#B's comment was never 🚀'd — it was not processed")
	}
	assertReworkOnMain(t, env, b)
	assertNotTrainChurn(t, env, b)
}

// TestQueuedCommentEject_NonActionableComments_NeverEject is Acceptance 3: an engine
// comment, a bot review summary and a bot service notice never eject a Queued member —
// it lands exactly as it would with no comment at all, and no comment worker runs.
func TestQueuedCommentEject_NonActionableComments_NeverEject(t *testing.T) {
	t.Parallel()
	env := queuedCommentEnv(t)
	rework := &reworkRecorder{env: env}
	env.Claude.ForStageComments("Validate", rework.script)
	num := queuedCommentMember(t, env, "qc-bot")
	sim := env.Sim.Sim()
	env.Clock.Advance(time.Second)
	sim.SeedComment(env.OwnerRepo, num, "arbeithand", "🏭 **Fabrik — stage: Validate**\nreport")
	sim.SeedComment(env.OwnerRepo, num, "copilot[bot]", "Looks fine to me.")
	sim.SeedComment(env.OwnerRepo, num, "gemini-code-assist[bot]", "You have reached your daily quota limit.")
	if err := sim.Err(); err != nil {
		t.Fatalf("SeedComment: %v", err)
	}
	startTrialVerdictSeeder(t, env, allGreenVerdict)

	AdvanceUntil(t, env, func(env *Env) bool { return projectItem(t, env, num).IsClosed }, 40)

	if ejectCommentPosted(t, env, num) {
		t.Error("a bot/engine comment must never eject a Queued member")
	}
	if got := rework.count(num); got != 0 {
		t.Errorf("comment worker ran %d time(s) for bot/engine-only comments", got)
	}
}

// TestQueuedCommentEject_FailedReroute_RetriesNextPoll: a failed status move posts
// nothing and the next poll retries (ADR-1208 §4 ordering).
func TestQueuedCommentEject_FailedReroute_RetriesNextPoll(t *testing.T) {
	t.Parallel()
	env := queuedCommentEnv(t)
	num := queuedCommentMember(t, env, "qc-failreroute")
	seedQueuedComment(t, env, num, "maintainer", queuedHumanComment)
	startTrialVerdictSeeder(t, env, delayedGreenVerdict)

	// Narrowed on the member's itemID, as mergetrain_redsingleton_test.go does, and
	// one-shot so the retry can succeed.
	itemID := projectItem(t, env, num).ItemID
	env.Sim.Faults().FailWhen("UpdateProjectItemStatus",
		func(a simgh.Args) bool { return len(a.Values) > 0 && a.Values[0] == itemID },
		1, errInjectedRerouteFault)

	RunPoll(t, env)
	if got := projectItem(t, env, num).Status; got != "Queued" {
		t.Fatalf("status = %q after a failed reroute, want Queued", got)
	}
	if ejectCommentPosted(t, env, num) {
		t.Error("a failed reroute must post nothing")
	}

	RunPoll(t, env)
	WaitForProjectStatus(t, env, num, "Validate", 10)
	if n := countComments(t, env, num, "ejected (unprocessed comment)"); n != 1 {
		t.Errorf("eject comment posted %d times, want exactly 1", n)
	}
}
