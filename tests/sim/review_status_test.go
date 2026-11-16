package sim

import (
	"strings"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tests/sim/simgh/ghfault"
)

// Sim twin of the fabrik/review commit status (#2471, ADR 2471). Each scenario
// that proves a behaviour also runs with that behaviour neutralised through its
// per-Engine seam (the non-vacuity control), and must flip.

type reviewStatusOpts struct {
	repoWide    bool // config review_status
	stageKey    *bool
	noReviewers bool
	startTime   time.Time
	stage       string // the enabling stage ("Review" or "Validate")
	ciBackstop  time.Duration
}

func newReviewStatusEnv(t *testing.T, o reviewStatusOpts) *Env {
	t.Helper()
	tr := true
	var exp *[]string
	if !o.noReviewers {
		exp = &[]string{"pruefer-bot"}
	}
	stage := &stages.Stage{Name: "Review", Order: 2, WaitForReviews: &tr, ExpectedReviewers: exp, ReviewStatus: o.stageKey}
	if o.stage == "Validate" {
		stage = &stages.Stage{Name: "Validate", Order: 2, WaitForReviews: &tr, WaitForCI: &tr, ExpectedReviewers: exp, ReviewStatus: o.stageKey}
	}
	start := o.startTime
	if start.IsZero() {
		start = time.Now()
	}
	// No auto-advance: the item must stay in its enabling stage while its review
	// verdict changes (yolo would move it to Done once the gate clears).
	noYolo := false
	return NewEnv(t, EnvOptions{
		Yolo: &noYolo,
		Stages: []*stages.Stage{
			{Name: "Implement", Order: 1, CreateDraftPR: true, MarkPRReadyOnComplete: true},
			stage,
			{Name: "Done", Order: 3, CleanupWorktree: true},
		},
		StartTime: start,
		ConfigureCfg: func(cfg *engine.Config) {
			cfg.ReviewWaitTimeout = 15 * time.Minute
			cfg.MaxReviewCycles = 5
			cfg.ReviewStatus = o.repoWide
			if o.ciBackstop > 0 {
				cfg.CIBackstopTimeout = o.ciBackstop
				cfg.CIWaitTimeout = 6 * time.Hour // out of the way; the backstop is the subject
			}
		},
	})
}

// reviewStatusOf is the fabrik/review state on sha ("" when the context is absent).
func reviewStatusOf(t *testing.T, env *Env, sha string) string {
	t.Helper()
	sts, err := env.Sim.FetchCombinedStatus(env.Owner, env.Repo, sha)
	if err != nil {
		t.Fatalf("FetchCombinedStatus: %v", err)
	}
	for _, st := range sts {
		if st.Context == gh.ReviewStatusContext {
			return st.State
		}
	}
	return ""
}

func statusWrites(env *Env) int { return len(env.Sim.Log().ByMethod("CreateCommitStatus")) }

func requireStatus(t *testing.T, env *Env, sha, want, what string) {
	t.Helper()
	if got := reviewStatusOf(t, env, sha); got != want {
		t.Fatalf("%s: fabrik/review on %.7s = %q, want %q\n%s", what, sha, got, want, diagnostics(env))
	}
}

// AC1/AC2: pending → success on a clean review of the head, a push resets the new
// head to pending, and a change request at the head is failure. With the verdict
// head-blind (neutralised) the push does not reset the status.
func TestReviewStatus_TransitionsAndPushReset(t *testing.T) {
	t.Parallel()
	tr := true
	for _, tc := range []struct {
		name      string
		headBlind bool
	}{{"as shipped", false}, {"neutralised (head-blind verdict)", true}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newReviewStatusEnv(t, reviewStatusOpts{stageKey: &tr})
			env.Engine.SetReviewStatusHeadBlindForTest(tc.headBlind)
			num, prNum := seedReviewGateItem(t, env, "Review", "rs-transitions")
			a := headOf(t, env, prNum)

			RunPolls(t, env, 3)
			requireStatus(t, env, a, "pending", "no review yet")

			seedBareReview(t, env, prNum, "pruefer-bot[bot]", "COMMENTED", a)
			RunPolls(t, env, 3)
			requireStatus(t, env, a, "success", "clean review of the head")

			b := pushFix(t, env, num, prNum)
			RunPolls(t, env, 3)
			if tc.headBlind {
				if got := reviewStatusOf(t, env, b); got != "success" {
					t.Fatalf("head-blind control: new head status = %q; the neutralised verdict should wrongly say success", got)
				}
				return
			}
			requireStatus(t, env, b, "pending", "after a push the new head is pending")
			requireStatus(t, env, a, "success", "the old head keeps its own status")

			seedBareReview(t, env, prNum, "pruefer-bot[bot]", "CHANGES_REQUESTED", b)
			RunPolls(t, env, 3)
			requireStatus(t, env, b, "failure", "change request at the head")
		})
	}
}

// A change request on an earlier commit is pending for the new head, never failure.
func TestReviewStatus_OldChangeRequestIsPendingNotFailure(t *testing.T) {
	t.Parallel()
	tr := true
	env := newReviewStatusEnv(t, reviewStatusOpts{stageKey: &tr})
	num, prNum := seedReviewGateItem(t, env, "Review", "rs-old-cr")
	a := headOf(t, env, prNum)
	seedBareReview(t, env, prNum, "pruefer-bot[bot]", "CHANGES_REQUESTED", a)
	RunPolls(t, env, 3)
	requireStatus(t, env, a, "failure", "change request at the head")
	b := pushFix(t, env, num, prNum)
	RunPolls(t, env, 3)
	requireStatus(t, env, b, "pending", "change request on an earlier commit")
}

// An unchanged state is not rewritten.
func TestReviewStatus_UnchangedWritesNothing(t *testing.T) {
	t.Parallel()
	tr := true
	env := newReviewStatusEnv(t, reviewStatusOpts{stageKey: &tr})
	seedReviewGateItem(t, env, "Review", "rs-unchanged")
	RunPolls(t, env, 8)
	if n := statusWrites(env); n != 1 {
		t.Fatalf("status writes after 8 polls with nothing changing = %d, want 1\n%s", n, diagnostics(env))
	}
}

// AC4: off by default; the repo-wide key enables it, an explicit stage false
// beats the repo-wide key, and the engine seam turns it off.
func TestReviewStatus_OffByDefaultAndOptIn(t *testing.T) {
	t.Parallel()
	f := false
	tr := true
	for _, tc := range []struct {
		name      string
		opts      reviewStatusOpts
		seam      bool
		wantWrite bool
	}{
		{"default off", reviewStatusOpts{}, false, false},
		{"stage true", reviewStatusOpts{stageKey: &tr}, false, true},
		{"repo-wide on", reviewStatusOpts{repoWide: true}, false, true},
		{"stage false beats repo-wide", reviewStatusOpts{repoWide: true, stageKey: &f}, false, false},
		{"no expected reviewers: nothing to wait on", reviewStatusOpts{stageKey: &tr, noReviewers: true}, false, false},
		{"seam neutralises the feature", reviewStatusOpts{stageKey: &tr}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newReviewStatusEnv(t, tc.opts)
			env.Engine.SetReviewStatusDisabledForTest(tc.seam)
			seedReviewGateItem(t, env, "Review", "rs-optin")
			RunPolls(t, env, 4)
			if got := statusWrites(env) > 0; got != tc.wantWrite {
				t.Fatalf("status written = %v, want %v\n%s", got, tc.wantWrite, diagnostics(env))
			}
		})
	}
}

// FR-012: a refused write never pauses the item or blocks anything; it is
// retried every poll, and written once the refusal ends.
func TestReviewStatus_RefusedWriteNeverBlocks(t *testing.T) {
	t.Parallel()
	tr := true
	env := newReviewStatusEnv(t, reviewStatusOpts{stageKey: &tr})
	env.Sim.Faults().FailAlways("CreateCommitStatus", ghfault.Wrap(gh.ErrForbidden, "Resource not accessible by integration"))
	num, prNum := seedReviewGateItem(t, env, "Review", "rs-refused")
	a := headOf(t, env, prNum)
	RunPolls(t, env, 5)
	labels := IssueLabels(t, env, num)
	for _, bad := range []string{"fabrik:paused", "fabrik:awaiting-input", "stage:Review:failed"} {
		if hasLabel(labels, bad) {
			t.Fatalf("a refused status write applied %s\n%s", bad, diagnostics(env))
		}
	}
	if got := reviewStatusOf(t, env, a); got != "" {
		t.Fatalf("refused write left status %q", got)
	}
	if attempts := env.Sim.Faults().CallCount("CreateCommitStatus"); attempts < 3 {
		t.Fatalf("write attempts = %d, want one per poll (retried)", attempts)
	}
	env.Sim.Faults().Clear("CreateCommitStatus")
	RunPolls(t, env, 3)
	requireStatus(t, env, a, "pending", "after the refusal ends")
}

// R3 / FR-006: the CIBackstopTimeout bound is unchanged, but while the
// fabrik/review status is pending the pause comment names the review wait, not CI.
// Neutralised (attribution off) the same item gets the CI wording.
func TestReviewStatus_BackstopCommentNamesTheReviewWait(t *testing.T) {
	t.Parallel()
	tr := true
	for _, tc := range []struct {
		name       string
		attributed bool
	}{{"as shipped", true}, {"neutralised (no attribution)", false}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newReviewStatusEnv(t, reviewStatusOpts{stageKey: &tr, stage: "Validate", startTime: time.Now().Add(-3 * time.Hour), ciBackstop: time.Hour})
			env.Engine.SetReviewWaitAttributionDisabledForTest(!tc.attributed)
			env.Sim.Sim().SeedRepoAccess(env.OwnerRepo, gh.RepoAccess{AllowAutoMerge: false, CanPush: true})
			// A required check that never runs: the PR is blocked with no check runs and the
			// merge gate claims the item every poll, so CIBackstopTimeout is what bounds it.
			env.Sim.Sim().SeedRequiredContexts(env.OwnerRepo, "main", []string{"ci/never-runs"})
			num, prNum := seedReviewGateItem(t, env, "Validate", "rs-backstop", "fabrik:awaiting-ci")
			a := headOf(t, env, prNum)
			if err := env.Sim.Sim().RemoveLabelFromIssue(env.Owner, env.Repo, num, "stage:Validate:complete"); err != nil {
				t.Fatalf("RemoveLabel: %v", err)
			}

			AdvanceUntil(t, env, func(env *Env) bool {
				return hasLabel(IssueLabels(t, env, num), "fabrik:paused")
			}, 12)
			requireStatus(t, env, a, "pending", "no review of the head")

			const reviewWording = "The review wait for stage **Validate** timed out"
			const ciWording = "The awaiting-CI wait for stage **Validate** timed out"
			gotReview, gotCI := false, false
			for _, c := range projectItem(t, env, num).Comments {
				gotReview = gotReview || strings.Contains(c.Body, reviewWording)
				gotCI = gotCI || strings.Contains(c.Body, ciWording)
			}
			if tc.attributed && (!gotReview || gotCI) {
				t.Fatalf("pause comment should name the review wait (review=%v ci=%v)\n%s", gotReview, gotCI, diagnostics(env))
			}
			if !tc.attributed && (gotReview || !gotCI) {
				t.Fatalf("control: neutralised attribution should give the CI wording (review=%v ci=%v)\n%s", gotReview, gotCI, diagnostics(env))
			}
		})
	}
}
