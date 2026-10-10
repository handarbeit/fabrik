package sim

import (
	"fmt"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/tests/sim/simgh"
)

// Sim twins for #2096 (ADR 2096): the merge gate re-checks a transient claim on
// the next poll instead of waiting for the 10 × PollSeconds periodic re-evaluation
// (mentioned for context: #1943), and an expired cooldown admits an item once, not
// on every later poll (mentioned for context: #2062).
//
// The #1943 shape is a Validate-complete yolo item with a PR whose CI is pending
// and NO fabrik:awaiting-ci — so the dedicated awaiting-ci settle scan (ADR 1270)
// is not what re-checks it; only the poll's cooldown pre-filter can. The incident's
// route into that label state is not reproducible through the pipeline, so the
// state is seeded directly (the construction precedent of review_gate_helpers.go
// and paused_merged_pr_recovery_test.go): the item sits at Validate carrying
// fabrik:yolo and stage:Validate:complete, with an open PR.

const (
	mergeRecheckPollSeconds = 30
	mergeRecheckCheckID     = int64(9100)
)

func newMergeRecheckEnv(t *testing.T) *Env {
	t.Helper()
	env := NewEnv(t, EnvOptions{
		Stages:    ciFixStages(),
		Yolo:      boolPtr(false),
		StartTime: time.Now(),
		ConfigureCfg: func(cfg *engine.Config) {
			cfg.PollSeconds = mergeRecheckPollSeconds
			cfg.MaxCiFixCycles = 5
		},
	})
	// Deterministic direct merge once the gate clears.
	env.Sim.Sim().SeedRepoAccess(env.OwnerRepo, gh.RepoAccess{AllowAutoMerge: false, CanPush: true})
	env.Sim.Sim().SeedRequiredContexts(env.OwnerRepo, "main", []string{ciFixSentinel})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	return env
}

// TestMergeGateRecheck_TransientClaimClearsWithinTwoPolls: CI goes green with no
// label change and no updatedAt change; the item must land within two polls of the
// flip, not after the 10-poll periodic interval.
func TestMergeGateRecheck_TransientClaimClearsWithinTwoPolls(t *testing.T) {
	t.Parallel()
	env := newMergeRecheckEnv(t)

	title := "merge gate recheck (#1943 shape)"
	num := FileIssue(t, env, title, "Prove a pending-CI merge-gate claim is re-checked next poll.", "Validate", "fabrik:yolo", "stage:Validate:complete")
	branch := fmt.Sprintf("fabrik/issue-%d", num)
	env.Sim.Sim().SeedCommitFrom(env.OwnerRepo, branch, "", map[string]string{fmt.Sprintf("sim/merge-recheck-%d.md", num): "# merge recheck\n"}, "sim: "+title)
	env.Sim.Sim().SeedPR(env.OwnerRepo, simgh.PRSeed{Title: title, Head: branch, IssueNumber: num})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	pr, err := env.Sim.FetchLinkedPR(env.Owner, env.Repo, num)
	if err != nil || pr == nil || pr.Number == 0 {
		t.Fatalf("expected a linked PR, got %+v (err %v)", pr, err)
	}

	// CI is pending now and turns green four polls from now — by clock alone.
	flipAt := env.Clock.Now().Add(4 * mergeRecheckPollSeconds * time.Second)
	env.Sim.Sim().
		SeedCheckRun(env.OwnerRepo, pr.HeadSHA, gh.CheckRun{ID: mergeRecheckCheckID, Name: ciFixSentinel, Status: "in_progress"}).
		SeedCheckRunsAt(env.OwnerRepo, pr.HeadSHA, flipAt, gh.CheckRun{ID: mergeRecheckCheckID, Name: ciFixSentinel, Status: "completed", Conclusion: "success"})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seeding CI: %v", err)
	}

	// Non-vacuity: the flip is invisible to labels and to the issue's updatedAt, and
	// nothing wakes the item but the poll's own admission.
	baseline := projectItem(t, env, num).UpdatedAt
	flipped := -1
	for poll := 1; poll <= 20; poll++ {
		RunPoll(t, env)
		item := projectItem(t, env, num)
		if flipped < 0 && !env.Clock.Now().Before(flipAt) {
			flipped = poll
		}
		if item.IsClosed {
			if flipped < 0 {
				t.Fatalf("poll %d: landed before CI went green\n\n%s", poll, diagnostics(env))
			}
			if lag := poll - flipped; lag > 2 {
				t.Fatalf("landed %d polls after CI went green, want <= 2\n\n%s", lag, diagnostics(env))
			}
			if !linkedPRMerged(env, num) {
				t.Fatal("issue closed but the PR is not merged")
			}
			return
		}
		// The engine's own claim writes nothing; the item must look untouched.
		if !item.UpdatedAt.Equal(baseline) {
			t.Fatalf("poll %d: issue updatedAt moved (%v -> %v); the scenario no longer isolates the periodic re-evaluation", poll, baseline, item.UpdatedAt)
		}
	}
	t.Fatalf("item never landed within 20 polls (CI went green at poll %d)\n\n%s", flipped, diagnostics(env))
}
