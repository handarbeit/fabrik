package sim

import (
	"context"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tests/sim/simclaude"
)

// TestCIBackstop_ResumedAfterCleanStopPause_NotRepaused is the sim twin of the
// #2059 incident sequence (seen on #2052 / PR #2053): an item carrying
// fabrik:awaiting-ci is paused by a daemon clean stop, sits paused past
// CIBackstopTimeout, the daemon restarts, and a human's steering comment
// resumes it. The backstop must not re-pause it — paused time and downtime do
// not count (R1), the first evaluation after the restart runs the live chain
// (R2), and the comment worker the resume dispatches is not raced (R3) — so the
// comment worker's fix lands, CI goes green and the gate clears.
//
// Time is arranged so the label anchor is genuinely over the timeout by the real
// wall clock the backstop compares against (the sim clock starts 2h in the past
// and CIBackstopTimeout is 1h), while the resume is recent; both facts are
// asserted before the poll that matters, so the test cannot pass vacuously. The
// per-guard neutralisation tests live in engine/ci_settle_test.go; with the R1
// anchor removed this twin fails once the first post-restart poll has satisfied
// R2 and the worker has exited (see the PR body's neutralisation record).
func TestCIBackstop_ResumedAfterCleanStopPause_NotRepaused(t *testing.T) {
	t.Parallel()
	const backstop = time.Hour

	env := NewEnv(t, EnvOptions{
		Stages:    ciFixStages(),
		StartTime: time.Now().Add(-2 * time.Hour),
		ConfigureCfg: func(cfg *engine.Config) {
			cfg.MaxCiFixCycles = 5
			cfg.CIBackstopTimeout = backstop
			// Keep the stage gate's own (also awaiting-ci-anchored) timeout out of
			// the way; it is a separate, out-of-scope anchor (see ADR 2059).
			cfg.CIWaitTimeout = 6 * time.Hour
		},
	})
	env.Sim.Sim().SeedRepoAccess(env.OwnerRepo, gh.RepoAccess{AllowAutoMerge: false, CanPush: true})
	env.Sim.Sim().SeedRequiredContexts(env.OwnerRepo, "main", []string{ciFixSentinel})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	// active is the Env whose Sim the script writes to: RestartEnv builds a new
	// Sim from a snapshot, so the script must follow the restart.
	active := env

	// The steering comment's worker fixes the outstanding problem: it turns the
	// required check green on the PR head.
	env.Claude.ForStageComments("Validate", simclaude.CommentScript(
		func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
			pr, err := active.Sim.FetchLinkedPR(active.Owner, active.Repo, issue.Number)
			if err != nil {
				return "", false, engine.TokenUsage{}, err
			}
			active.Sim.Sim().SeedCheckRun(active.OwnerRepo, pr.HeadSHA, gh.CheckRun{Name: ciFixSentinel, Status: "completed", Conclusion: "success"})
			if serr := active.Sim.Sim().Err(); serr != nil {
				return "", false, engine.TokenUsage{}, serr
			}
			return "FABRIK_STAGE_COMPLETE\n", true, engine.TokenUsage{InputTokens: 500, OutputTokens: 100, TurnsUsed: 2, MaxTurns: 50}, nil
		}))

	num := FileIssue(t, env, "backstop resume", "Prove the CI backstop does not re-pause a just-resumed item.", "Implement")
	WaitForIssueLabel(t, env, num, "fabrik:awaiting-ci", 80)

	appliedAt, err := env.Sim.FetchLabelAppliedAt(env.Owner, env.Repo, num, "fabrik:awaiting-ci")
	if err != nil {
		t.Fatalf("FetchLabelAppliedAt: %v", err)
	}
	if age := time.Since(appliedAt); age < backstop {
		t.Fatalf("non-vacuity: fabrik:awaiting-ci is only %s old, need > %s so the label anchor alone would trip the backstop", age, backstop)
	}

	// The daemon clean stop pauses the item mid-Validate; awaiting-ci stays.
	for _, l := range []string{"fabrik:paused", "fabrik:awaiting-input"} {
		if err := env.Sim.Sim().AddLabelToIssue(env.Owner, env.Repo, num, l); err != nil {
			t.Fatalf("AddLabelToIssue(%s): %v", l, err)
		}
	}
	env.Sim.Sim().SeedComment(env.OwnerRepo, num, "fabrik-dev-daemon[bot]",
		"🏭 **Fabrik — paused by a daemon clean stop**\n\nStage **Validate** was interrupted by a daemon clean stop (SIGINT/SIGTERM). Remove `fabrik:paused` to resume.")
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seeding pause: %v", err)
	}

	// Overnight: the clock catches up to the wall clock, then the daemon restarts
	// (clearing the engine's in-memory first-evaluation marker).
	env.Clock.Set(time.Now().Add(-10 * time.Minute))
	restarted := RestartEnv(t, env)
	active = restarted

	// A human resumes it with a steering comment.
	restarted.Clock.Advance(time.Minute)
	if err := restarted.Sim.Sim().RemoveLabelFromIssue(restarted.Owner, restarted.Repo, num, "fabrik:paused"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if err := restarted.Sim.Sim().RemoveLabelFromIssue(restarted.Owner, restarted.Repo, num, "fabrik:awaiting-input"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	restarted.Clock.Advance(time.Second)
	restarted.Sim.Sim().SeedComment(restarted.OwnerRepo, num, "a-human-operator", "Please also fix the flaky check, then re-run.")
	if err := restarted.Sim.Sim().Err(); err != nil {
		t.Fatalf("seeding resume comment: %v", err)
	}

	removedAt, err := restarted.Sim.FetchLabelRemovedAt(restarted.Owner, restarted.Repo, num, "fabrik:paused")
	if err != nil {
		t.Fatalf("FetchLabelRemovedAt: %v", err)
	}
	if age := time.Since(removedAt); age >= backstop {
		t.Fatalf("non-vacuity: the resume is %s old; it must be under %s for this to be the incident shape", age, backstop)
	}

	// Drive polls. At no point may the item be re-paused; the comment worker
	// must run and the gate must clear on green.
	for i := 0; i < 60; i++ {
		RunPoll(t, restarted)
		labels := IssueLabels(t, restarted, num)
		if hasLabel(labels, "fabrik:paused") {
			t.Fatalf("poll %d: item was re-paused after a legitimate resume; labels=%v", i+1, labels)
		}
		if !hasLabel(labels, "fabrik:awaiting-ci") {
			break
		}
	}
	if hasLabel(IssueLabels(t, restarted, num), "fabrik:awaiting-ci") {
		t.Fatal("fabrik:awaiting-ci never cleared after the comment worker turned CI green")
	}
	if got := restarted.Claude.CommentCallCount("Validate"); got < 1 {
		t.Errorf("CommentCallCount(Validate) = %d, want the resume's comment worker to have run", got)
	}
	if hasLabel(IssueLabels(t, restarted, num), "fabrik:paused") {
		t.Error("item ended paused")
	}
}
