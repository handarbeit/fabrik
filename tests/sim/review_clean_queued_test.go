package sim

import (
	"fmt"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
)

// #2525 sim twin: the clean-review skip (#2489) must also apply to a merge-train
// member sitting in the holding stage (Queued). The holding stage declares no
// expected_reviewers, so the member is judged against the stage it was validated
// in (the reroute target). Without that, the Queued review-finding scan reads the
// clean review as an unprocessed finding and ejects the member back to Validate.
// Each scenario also runs with the holding-stage context neutralised
// (SetHoldingStageReviewContextDisabledForTest), the pre-fix behaviour.

const (
	queuedCleanMarker = "<!-- pruefer:findings inline=0 body=0 coverage=complete -->"
	queuedFindingsTag = "ejected from merge-train — 1 unprocessed review finding"
)

// queuedReviewEnv is a merge-train env whose Validate stage declares cleanReviewer.
func queuedReviewEnv(t *testing.T, neutralised bool) *Env {
	t.Helper()
	env := mergeTrainEnv(t, mergeTrainEnvOptions{
		ValidateWaitForCI: true,
		StartTime:         time.Now(),
		ConfigureCfg: func(cfg *engine.Config) {
			exp := []string{cleanReviewer}
			for _, s := range cfg.Stages {
				if s.Name == "Validate" {
					s.ExpectedReviewers = &exp
				}
			}
		},
	})
	env.Engine.SetHoldingStageReviewContextDisabledForTest(neutralised)
	t.Cleanup(env.Engine.RegisterObservers())
	return env
}

// queuedReviewMember queues a yolo, Validate-complete member with review at head.
func queuedReviewMember(t *testing.T, env *Env, marker string, review gh.PRReview) int {
	t.Helper()
	num := queuedCommentMember(t, env, marker)
	prNum := projectItemPR(t, env, num)
	head, err := env.Sim.Sim().HeadSHA(env.OwnerRepo, fmt.Sprintf("fabrik/issue-%d", num))
	if err != nil {
		t.Fatalf("HeadSHA: %v", err)
	}
	review.CommitID = head
	env.Clock.Advance(time.Second)
	env.Sim.Sim().SeedReview(env.OwnerRepo, prNum, review)
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("SeedReview: %v", err)
	}
	return num
}

func projectItemPR(t *testing.T, env *Env, num int) int {
	t.Helper()
	pr, err := env.Sim.FetchLinkedPR(env.Owner, env.Repo, num)
	if err != nil || pr == nil {
		t.Fatalf("no linked PR for #%d: %v", num, err)
	}
	return pr.Number
}

func cleanQueuedReview() gh.PRReview {
	return gh.PRReview{
		Author: cleanReviewer, State: "COMMENTED", DatabaseID: 7800001,
		Body: "Reviewed the change; no issues found.\n\nPRUEFER_COVERAGE: complete\n\n" + queuedCleanMarker,
	}
}

// A yolo member whose only review is the declared reviewer's clean-marker review
// lands with no ejection and no pause.
func TestQueuedCleanReview_LandsWithoutEjection(t *testing.T) {
	t.Parallel()
	env := queuedReviewEnv(t, false)
	num := queuedReviewMember(t, env, "qcr-green", cleanQueuedReview())
	startTrialVerdictSeeder(t, env, allGreenVerdict)

	AdvanceUntil(t, env, func(env *Env) bool { return projectItem(t, env, num).IsClosed }, 60)

	if hasCommentContaining(t, env, num, "ejected from merge-train") {
		t.Error("a clean declared-reviewer review must not eject a Queued member")
	}
	if isPaused(t, env, num) {
		t.Error("the member must not be paused")
	}
}

// Red control: with the holding-stage context neutralised the same member is
// ejected for the clean review, as observed on #2491.
func TestQueuedCleanReview_NeutralisedEjects(t *testing.T) {
	t.Parallel()
	env := queuedReviewEnv(t, true)
	num := queuedReviewMember(t, env, "qcr-red", cleanQueuedReview())
	startTrialVerdictSeeder(t, env, allGreenVerdict)

	RunPoll(t, env)
	if !hasCommentContaining(t, env, num, queuedFindingsTag) {
		t.Fatal("with the fix neutralised the clean review must eject the Queued member (the #2525 loop)")
	}
}

// Real findings on a Queued member still eject with the fix on.
func TestQueuedCleanReview_RealFindingsStillEject(t *testing.T) {
	t.Parallel()
	cases := map[string]func(*gh.PRReview){
		"non-clean marker": func(r *gh.PRReview) {
			r.Body = "found one\n\n<!-- pruefer:findings inline=1 body=0 coverage=complete -->"
		},
		"marker-less body":  func(r *gh.PRReview) { r.Body = "looks fine to me" },
		"change request":    func(r *gh.PRReview) { r.State = "CHANGES_REQUESTED" },
		"undeclared author": func(r *gh.PRReview) { r.Author = "stranger" },
	}
	for name, mod := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := queuedReviewEnv(t, false)
			r := cleanQueuedReview()
			mod(&r)
			num := queuedReviewMember(t, env, "qcr-"+fmt.Sprint(len(name)), r)
			startTrialVerdictSeeder(t, env, allGreenVerdict)

			RunPoll(t, env)
			if !hasCommentContaining(t, env, num, queuedFindingsTag) {
				t.Errorf("%s: a real finding must still eject the Queued member", name)
			}
		})
	}
}
