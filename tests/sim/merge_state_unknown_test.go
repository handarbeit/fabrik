package sim

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/tests/sim/simgh"
)

// Sim twins for #2302 (ADR 2302): the merge gate bounds an unknown merge state
// (mergeable null) on its own short clock instead of the 4h CIBackstopTimeout, and
// the CIBackstopTimeout pause names the gate that actually held the item. Related
// reports, mentioned for context: #1809, #1943.
//
// The unknown state is the sim's clock-driven SeedMergeableUnknownUntil, not the
// read-count recompute window: "null for 40 minutes" must not depend on how many
// reads the engine makes per poll.

const (
	unknownMergePollSeconds = 300 // 5 min per poll; the merge-gate lapse is 3 polls
	unknownMergeBound       = 30 * time.Minute
	unknownMergeCheckID     = int64(9200)
)

func newUnknownMergeEnv(t *testing.T, start time.Time, backstop time.Duration) *Env {
	t.Helper()
	env := NewEnv(t, EnvOptions{
		Stages:    ciFixStages(),
		Yolo:      boolPtr(false),
		StartTime: start,
		ConfigureCfg: func(cfg *engine.Config) {
			cfg.PollSeconds = unknownMergePollSeconds
			cfg.MaxCiFixCycles = 5
			cfg.MergeStateUnknownTimeout = unknownMergeBound
			if backstop > 0 {
				cfg.CIBackstopTimeout = backstop
				cfg.CIWaitTimeout = 6 * time.Hour // keep the stage gate's own dwell out of the way
			}
		},
	})
	env.Sim.Sim().SeedRepoAccess(env.OwnerRepo, gh.RepoAccess{AllowAutoMerge: false, CanPush: true})
	env.Sim.Sim().SeedRequiredContexts(env.OwnerRepo, "main", []string{ciFixSentinel})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	return env
}

// seedUnknownMergeItem files a Validate item with an open PR and a green (or
// pending) required check, and keeps the PR's mergeability unresolved until
// unknownUntil (zero = never put into the unknown window).
func seedUnknownMergeItem(t *testing.T, env *Env, title string, checkStatus, checkConclusion string, unknownUntil time.Time, labels ...string) (int, *gh.PRDetails) {
	t.Helper()
	num := FileIssue(t, env, title, "Prove the unknown-merge-state bound.", "Validate", labels...)
	branch := fmt.Sprintf("fabrik/issue-%d", num)
	env.Sim.Sim().SeedCommitFrom(env.OwnerRepo, branch, "", map[string]string{fmt.Sprintf("sim/merge-unknown-%d.md", num): "# merge unknown\n"}, "sim: "+title)
	env.Sim.Sim().SeedPR(env.OwnerRepo, simgh.PRSeed{Title: title, Head: branch, IssueNumber: num})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	pr, err := env.Sim.FetchLinkedPR(env.Owner, env.Repo, num)
	if err != nil || pr == nil || pr.Number == 0 {
		t.Fatalf("expected a linked PR, got %+v (err %v)", pr, err)
	}
	env.Sim.Sim().SeedCheckRun(env.OwnerRepo, pr.HeadSHA, gh.CheckRun{ID: unknownMergeCheckID, Name: ciFixSentinel, Status: checkStatus, Conclusion: checkConclusion})
	if !unknownUntil.IsZero() {
		env.Sim.Sim().SeedMergeableUnknownUntil(env.OwnerRepo, pr.Number, unknownUntil)
	}
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seeding CI: %v", err)
	}
	return num, pr
}

// pollUntilPaused drives polls until the item carries fabrik:paused or maxPolls
// is reached, returning how long after t0 (sim clock) the pause was observed.
func pollUntilPaused(t *testing.T, env *Env, num int, t0 time.Time, maxPolls int) (time.Duration, bool) {
	t.Helper()
	for i := 0; i < maxPolls; i++ {
		RunPoll(t, env)
		if hasLabel(IssueLabels(t, env, num), "fabrik:paused") {
			return env.Clock.Now().Sub(t0), true
		}
	}
	return 0, false
}

func pauseCommentOn(t *testing.T, env *Env, num int, fragment string) string {
	t.Helper()
	for _, c := range commentsOn(t, env, num) {
		if strings.Contains(c.Body, fragment) {
			return c.Body
		}
	}
	return ""
}

func assertUnknownMergeComment(t *testing.T, env *Env, num int, pr *gh.PRDetails) {
	t.Helper()
	body := pauseCommentOn(t, env, num, "GitHub has not reported a computed mergeability")
	if body == "" {
		t.Fatalf("no unknown-merge-state pause comment\n\n%s", diagnostics(env))
	}
	for _, want := range []string{
		fmt.Sprintf("for PR #%d at head `%s`", pr.Number, pr.HeadSHA[:10]),
		"`mergeable=null`",
		"CI gate for stage **Validate** was never reached",
		"1 of 1 checks passed",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("pause comment missing %q:\n%s", want, body)
		}
	}
	for _, bad := range []string{"address any failures", "timed out waiting for checks to pass"} {
		if strings.Contains(body, bad) {
			t.Errorf("pause comment must not contain %q:\n%s", bad, body)
		}
	}
}

// A Validate item carrying fabrik:awaiting-ci, all checks green, whose PR reads
// mergeable=null on every read, is paused after the new bound — not after the 4h
// CIBackstopTimeout — and the comment names the unknown value and the green checks.
func TestMergeStateUnknown_PersistentNullPausesAfterBound_AwaitingCI(t *testing.T) {
	t.Parallel()
	env := newUnknownMergeEnv(t, time.Now(), 0)
	t0 := env.Clock.Now()
	num, pr := seedUnknownMergeItem(t, env, "persistent null (awaiting-ci)", "completed", "success",
		t0.Add(100*time.Hour), "fabrik:awaiting-ci")

	after, paused := pollUntilPaused(t, env, num, t0, 14)
	if !paused {
		t.Fatalf("item never paused within 70 minutes of a persistent mergeable=null\n\n%s", diagnostics(env))
	}
	if after < unknownMergeBound || after > unknownMergeBound+2*unknownMergePollSeconds*time.Second {
		t.Fatalf("paused after %s, want about the %s bound (never the 4h backstop)", after, unknownMergeBound)
	}
	assertUnknownMergeComment(t, env, num, pr)

	// Stays one pause, one comment, across further polls.
	RunPolls(t, env, 4)
	n := 0
	for _, c := range commentsOn(t, env, num) {
		if strings.Contains(c.Body, "GitHub has not reported a computed mergeability") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d unknown-merge-state comments, want exactly 1 (no repost)", n)
	}
}

// The null clears to a computed state before the bound: the item proceeds and
// nothing is paused.
func TestMergeStateUnknown_NullClearingBeforeBound_NoPause(t *testing.T) {
	t.Parallel()
	env := newUnknownMergeEnv(t, time.Now(), 0)
	t0 := env.Clock.Now()
	num, _ := seedUnknownMergeItem(t, env, "null clears (awaiting-ci)", "completed", "success",
		t0.Add(12*time.Minute), "fabrik:awaiting-ci")

	for i := 0; i < 14; i++ {
		RunPoll(t, env)
		if hasLabel(IssueLabels(t, env, num), "fabrik:paused") {
			t.Fatalf("poll %d: paused although the null cleared before the bound\n\n%s", i+1, diagnostics(env))
		}
	}
	if hasLabel(IssueLabels(t, env, num), "fabrik:awaiting-ci") {
		t.Errorf("fabrik:awaiting-ci never cleared once mergeability was computed and CI green\n\n%s", diagnostics(env))
	}
	if pauseCommentOn(t, env, num, "GitHub has not reported a computed mergeability") != "" {
		t.Error("a pause comment was posted for a null that cleared")
	}
}

// A Validate-complete yolo item with NO fabrik:awaiting-ci (the #1943 shape) is
// bounded too: before this change nothing at all bounded it.
func TestMergeStateUnknown_PersistentNullBoundsYoloWithoutAwaitingCI(t *testing.T) {
	t.Parallel()
	env := newUnknownMergeEnv(t, time.Now(), 0)
	t0 := env.Clock.Now()
	num, pr := seedUnknownMergeItem(t, env, "persistent null (yolo, no awaiting-ci)", "completed", "success",
		t0.Add(100*time.Hour), "fabrik:yolo", "stage:Validate:complete")
	if hasLabel(IssueLabels(t, env, num), "fabrik:awaiting-ci") {
		t.Fatal("non-vacuity: the scenario must not carry fabrik:awaiting-ci")
	}

	after, paused := pollUntilPaused(t, env, num, t0, 14)
	if !paused {
		t.Fatalf("a Validate-complete item under a persistent null was never bounded\n\n%s", diagnostics(env))
	}
	if after < unknownMergeBound || after > unknownMergeBound+2*unknownMergePollSeconds*time.Second {
		t.Fatalf("paused after %s, want about the %s bound", after, unknownMergeBound)
	}
	assertUnknownMergeComment(t, env, num, pr)
	if linkedPRMerged(env, num) {
		t.Error("the PR must not merge while its mergeability is unknown")
	}
}

// An item held by "CI checks pending" past CIBackstopTimeout gets a backstop
// comment naming the merge gate's real reason — not "waiting for checks to pass".
func TestMergeStateUnknown_BackstopCommentNamesTheMergeGateReason(t *testing.T) {
	t.Parallel()
	const backstop = time.Hour
	// The backstop compares the label's age against the real wall clock, so the
	// sim clock starts 2h in the past (the ci_backstop_resume_test.go technique).
	env := newUnknownMergeEnv(t, time.Now().Add(-2*time.Hour), backstop)
	num, _ := seedUnknownMergeItem(t, env, "backstop names the merge gate", "in_progress", "",
		time.Time{}, "fabrik:awaiting-ci")

	appliedAt, err := env.Sim.FetchLabelAppliedAt(env.Owner, env.Repo, num, "fabrik:awaiting-ci")
	if err != nil {
		t.Fatalf("FetchLabelAppliedAt: %v", err)
	}
	if age := time.Since(appliedAt); age < backstop {
		t.Fatalf("non-vacuity: fabrik:awaiting-ci is only %s old, need > %s", age, backstop)
	}

	if _, paused := pollUntilPaused(t, env, num, env.Clock.Now(), 8); !paused {
		t.Fatalf("the CIBackstopTimeout backstop never escalated\n\n%s", diagnostics(env))
	}
	body := pauseCommentOn(t, env, num, "The awaiting-CI wait for stage")
	if body == "" {
		t.Fatalf("no backstop comment naming the holder\n\n%s", diagnostics(env))
	}
	for _, want := range []string{
		"held by the merge gate: CI checks pending",
		"0 of 1 checks passed, 1 pending", // the pending check is counted, not passed
		"The CI gate itself was never reached",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("backstop comment missing %q:\n%s", want, body)
		}
	}
	for _, bad := range []string{"timed out waiting for checks to pass", "address any failures"} {
		if strings.Contains(body, bad) {
			t.Errorf("backstop comment must not contain %q:\n%s", bad, body)
		}
	}
}
