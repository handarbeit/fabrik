package sim

import (
	"fmt"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/tests/sim/simgh"
)

// Sim twin of #2573 (ADR 2573): an item waiting on CI whose PR keeps receiving
// check events. The cache serves the board with UpdatedAt raised to the webhook
// receipt time; the awaiting-CI settle scan applied that copy back into the Store,
// so every light reconcile compared a receipt time with GitHub's own value and
// reported drift. The deep-fetch apply no longer writes it.

const receiptDriftTicks = 12

func newReceiptDriftEnv(t *testing.T) *Env {
	t.Helper()
	// No StartTime: the sim's default start is in the past, so the wall-clock
	// receipt time is later than GitHub's virtual updatedAt (asserted below).
	env := NewEnv(t, EnvOptions{
		Stages:     ciFixStages(),
		BoardCache: true,
		ConfigureCfg: func(cfg *engine.Config) {
			cfg.MaxCiFixCycles = 5
			cfg.PollSeconds = 60
			// The label anchor is virtual time long before the wall clock; keep the
			// CI timeouts out of the way so the item stays at awaiting-ci.
			cfg.CIBackstopTimeout = 10000 * time.Hour
			cfg.CIWaitTimeout = 10000 * time.Hour
		},
	})
	t.Cleanup(env.Engine.RegisterObservers())
	env.Claude.ForStageComments("Validate", ciFixCommentScript(env.Sim, env.OwnerRepo, "success"))
	env.Sim.Sim().SeedRepoAccess(env.OwnerRepo, gh.RepoAccess{AllowAutoMerge: false, CanPush: true})
	env.Sim.Sim().SeedRequiredContexts(env.OwnerRepo, "main", []string{ciFixSentinel})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	return env
}

// waitingOnCI files a Validate item with an open PR whose required check stays
// in progress and that carries fabrik:awaiting-ci, so the awaiting-CI settle scan
// owns it on every poll. It returns the number and the PR head SHA.
func waitingOnCI(t *testing.T, env *Env, title string) (int, string) {
	t.Helper()
	num := FileIssue(t, env, title, "Waits on a busy PR.", "Validate", "fabrik:awaiting-ci")
	branch := fmt.Sprintf("fabrik/issue-%d", num)
	env.Sim.Sim().SeedCommitFrom(env.OwnerRepo, branch, "", map[string]string{fmt.Sprintf("sim/receipt-drift-%d.md", num): "# receipt drift\n"}, "sim: "+title)
	env.Sim.Sim().SeedPR(env.OwnerRepo, simgh.PRSeed{Title: title, Head: branch, IssueNumber: num})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	pr, err := env.Sim.FetchLinkedPR(env.Owner, env.Repo, num)
	if err != nil || pr == nil || pr.HeadSHA == "" {
		t.Fatalf("expected a linked PR with a head, got %+v (err %v)", pr, err)
	}
	env.Sim.Sim().SeedCheckRun(env.OwnerRepo, pr.HeadSHA, gh.CheckRun{Name: ciFixSentinel, Status: "in_progress"})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seeding CI: %v", err)
	}
	return num, pr.HeadSHA
}

// checkEvent delivers a completed check_run webhook for the PR head: a local
// receipt time bump with no change on GitHub's issue or PR. It is a side check
// (not the required sentinel), so the item keeps waiting on CI.
func checkEvent(env *Env, sha string, id int) {
	env.BoardCache.ApplyDelta("check_run", []byte(fmt.Sprintf(
		`{"action":"completed","check_run":{"id":%d,"name":"side-check","status":"completed","conclusion":"success","head_sha":%q},`+
			`"repository":{"full_name":%q}}`, id, sha, env.OwnerRepo)))
}

// receiptDriftRun polls and light-reconciles N ticks with a check event before
// every poll, returning the drift each tick reported and whether the served board
// carried a receipt-inflated UpdatedAt at least once (the precondition).
func receiptDriftRun(t *testing.T, env *Env, num int, sha string, before func(tick int)) (drift []int, inflated bool) {
	t.Helper()
	for i := 0; i < receiptDriftTicks; i++ {
		if before != nil {
			before(i)
		}
		checkEvent(env, sha, 9000+i)
		RunPoll(t, env)
		board, err := env.BoardCache.FetchProjectBoard(env.Owner, env.Repo, env.ProjectNum, env.cfg.OwnerType)
		if err != nil {
			t.Fatalf("FetchProjectBoard: %v", err)
		}
		for _, it := range board.Items {
			if it.Number == num && it.UpdatedAt.After(env.Clock.Now()) {
				inflated = true // the receipt (wall clock) is later than GitHub's virtual updatedAt
			}
		}
		env.Clock.Advance(45 * time.Second) // reconcile runs on its own timer, after the 30 s engine-write shield
		d, _, _, err := env.BoardCache.LightReconcile(env.Owner, env.Repo, env.ProjectNum, env.cfg.OwnerType)
		if err != nil {
			t.Fatalf("LightReconcile: %v", err)
		}
		drift = append(drift, d)
	}
	return drift, inflated
}

func sum(xs []int) int {
	n := 0
	for _, x := range xs {
		n += x
	}
	return n
}

func TestReceiptTimeDrift_WaitingItemWithBusyPRReportsNoDrift(t *testing.T) {
	t.Parallel()
	env := newReceiptDriftEnv(t)
	num, sha := waitingOnCI(t, env, "receipt time drift (#2573)")
	drift, inflated := receiptDriftRun(t, env, num, sha, nil)
	if !inflated {
		t.Fatalf("precondition: the served UpdatedAt never carried a receipt time later than GitHub's; the twin would be vacuous")
	}
	if !hasLabel(IssueLabels(t, env, num), "fabrik:awaiting-ci") {
		t.Fatalf("item left fabrik:awaiting-ci; the scenario no longer exercises the settle scan")
	}
	if n := sum(drift); n != 0 {
		t.Errorf("light reconcile reported drift %v for an unchanged item with a busy PR, want none\n\n%s", drift, diagnostics(env))
	}
}

// Non-vacuity: with the apply restored, the receipt time is written back and the
// drift returns.
func TestReceiptTimeDrift_NeutralisedDriftsEveryTick(t *testing.T) {
	t.Parallel()
	env := newReceiptDriftEnv(t)
	env.Engine.SetReceiptTimeApplyForTest(true)
	num, sha := waitingOnCI(t, env, "receipt time drift neutralised (#2573)")
	drift, _ := receiptDriftRun(t, env, num, sha, nil)
	ticks := 0
	for _, d := range drift {
		if d > 0 {
			ticks++
		}
	}
	// The first tick can still agree (nothing has been applied yet).
	if ticks < receiptDriftTicks-2 {
		t.Fatalf("non-vacuity: with the fix off drift was reported on %d of %d ticks (%v), want nearly every tick\n\n%s",
			ticks, receiptDriftTicks, drift, diagnostics(env))
	}
}

// Real drift still shows: a label GitHub gained with no webhook is reported, with
// and without the continuous check-event stream.
func TestReceiptTimeDrift_RealMissedChangeStillReported(t *testing.T) {
	t.Parallel()
	for _, stream := range []bool{true, false} {
		stream := stream
		t.Run(fmt.Sprintf("checkStream=%v", stream), func(t *testing.T) {
			t.Parallel()
			env := newReceiptDriftEnv(t)
			num, sha := waitingOnCI(t, env, fmt.Sprintf("receipt time real drift %v (#2573)", stream))
			if stream {
				receiptDriftRun(t, env, num, sha, nil)
			} else {
				RunPolls(t, env, 2)
			}
			env.Clock.Advance(45 * time.Second)
			// A change on GitHub the cache never heard about.
			env.Sim.Sim().SeedLabelAdded(env.OwnerRepo, num, "missed-by-webhook")
			if stream {
				checkEvent(env, sha, 9999)
			}
			d, _, _, err := env.BoardCache.LightReconcile(env.Owner, env.Repo, env.ProjectNum, env.cfg.OwnerType)
			if err != nil {
				t.Fatalf("LightReconcile: %v", err)
			}
			if d == 0 {
				t.Fatalf("a field GitHub changed without a webhook was not reported as drift\n\n%s", diagnostics(env))
			}
		})
	}
}
