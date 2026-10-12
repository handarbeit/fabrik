package sim

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/localapi"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tests/sim/simclaude"
	"github.com/handarbeit/fabrik/tests/sim/simgh"
)

// #2122 scenarios, through a real Engine: an item in Implement or Review with a
// linked PR whose head check run is failing, no CI gate active (no
// fabrik:awaiting-ci, no wait_for_ci stage) and no webhook. Before this change
// the daemon held only the head SHA, so the status tool reported every PR fact
// and the CI verdict as unknown and attention said "waiting for dispatch".

func prFactsStages() []*stages.Stage {
	return []*stages.Stage{
		{Name: "Implement", Order: 1},
		{Name: "Review", Order: 2},
		{Name: "Done", Order: 3, CleanupWorktree: true},
	}
}

const prFactsFailingCheck = "Test and vet"

// seedPRFactsItem files an item at stage with an open linked PR whose head check
// run is failing, and returns the issue number and the PR head SHA.
func seedPRFactsItem(t *testing.T, env *Env, stage string, draft bool) (int, string) {
	t.Helper()
	num := FileIssue(t, env, "pr facts", "body", stage)
	branch := fmt.Sprintf("fabrik/issue-%d", num)
	sim := env.Sim.Sim()
	sim.SeedCommit(env.OwnerRepo, branch, map[string]string{"feature.txt": "work\n"}, "feat: work")
	sim.SeedPR(env.OwnerRepo, simghPRSeed(branch, num, draft))
	sha, err := sim.HeadSHA(env.OwnerRepo, branch)
	if err != nil {
		t.Fatal(err)
	}
	sim.SeedCheckRun(env.OwnerRepo, sha, gh.CheckRun{Name: prFactsFailingCheck, Status: "completed", Conclusion: "failure"})
	sim.SeedCheckRun(env.OwnerRepo, sha, gh.CheckRun{Name: "lint", Status: "completed", Conclusion: "success"})
	if err := sim.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	return num, sha
}

func prFactsStatus(t *testing.T, env *Env, num int) *localapi.StatusResult {
	t.Helper()
	st, err := env.Engine.LocalAPIBackend().Status(localapi.StatusParams{Issue: strconv.Itoa(num)})
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func newPRFactsEnv(t *testing.T, boardCache bool) (*Env, func()) {
	t.Helper()
	env := NewEnv(t, EnvOptions{Stages: prFactsStages(), MaxConcurrent: 2, Yolo: boolPtr(false), BoardCache: boardCache})
	if boardCache {
		t.Cleanup(env.Engine.RegisterObservers())
	}
	// Every slot held: the item is dispatchable but has no worker, which is the
	// "nothing is addressing it" state the issue observed.
	release := env.Engine.HoldGeneralWorkerSlotsForTest()
	released := false
	t.Cleanup(func() {
		if !released {
			release()
		}
	})
	return env, func() { released = true; release() }
}

func TestSimRedCIOnLinkedPRIsVisibleOutsideAnyCIGate(t *testing.T) {
	for _, stage := range []string{"Implement", "Review"} {
		for _, boardCache := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/boardCache=%v", stage, boardCache), func(t *testing.T) {
				env, _ := newPRFactsEnv(t, boardCache)
				num, sha := seedPRFactsItem(t, env, stage, false)

				RunPoll(t, env)
				st := prFactsStatus(t, env, num)
				if !st.PR.CI.Valid || st.PR.CI.V != "red" {
					t.Fatalf("ci = %+v, want red (pr=%+v)", st.PR.CI, st.PR)
				}
				if len(st.PR.CIFailing) != 1 || st.PR.CIFailing[0] != prFactsFailingCheck {
					t.Errorf("ci_failing = %v", st.PR.CIFailing)
				}
				if !st.PR.CIHeadSHA.Valid || st.PR.CIHeadSHA.V != sha || !st.PR.HeadSHA.Valid || st.PR.HeadSHA.V != sha {
					t.Errorf("heads: ci=%+v pr=%+v want %s", st.PR.CIHeadSHA, st.PR.HeadSHA, sha)
				}
				if !st.PR.PRState.Valid || st.PR.PRState.V != "open" || !st.PR.Draft.Valid || st.PR.Draft.V {
					t.Errorf("state/draft not known: %+v", st.PR)
				}
				if st.PR.CIStale.V || st.PR.FactsStale.V {
					t.Errorf("fresh facts reported stale: %+v", st.PR)
				}
				a := st.Attention
				if a.State != "waiting" || !strings.Contains(a.Summary, "CI is red on "+prFactsFailingCheck) {
					t.Errorf("attention = %+v", a)
				}
				if len(a.CIFailing) != 1 {
					t.Errorf("attention ci_failing = %v", a.CIFailing)
				}
				board, err := env.Engine.LocalAPIBackend().Board(localapi.BoardParams{})
				if err != nil {
					t.Fatal(err)
				}
				var ranked bool
				for _, it := range board.Attention {
					if it.Issue == st.Issue {
						ranked = true
					}
				}
				if !ranked {
					t.Errorf("the item is missing from the ranked attention view: %+v", board.Attention)
				}
				if !boardCache {
					return
				}
				// With the board probe the facts need no per-PR REST read once the
				// item is known to the store (the first poll may use the bounded
				// fallback for an item the probe has not met yet).
				first := len(env.Sim.Log().ByMethod("FetchPRDetails"))
				RunPolls(t, env, 3)
				if n := len(env.Sim.Log().ByMethod("FetchPRDetails")); n != first {
					t.Errorf("FetchPRDetails grew from %d to %d over three polls; the probe should supply the facts", first, n)
				}
				if st := prFactsStatus(t, env, num); st.PR.CI.V != "red" || st.PR.FactsStale.V {
					t.Errorf("after more polls: ci=%+v stale=%+v", st.PR.CI, st.PR.FactsStale)
				}
			})
		}
	}
}

// The same scenario fails without the poll-side refresher: the assertions above
// are not satisfied by something else.
func TestSimRedCIOnLinkedPR_RefresherDisabledStaysUnknown(t *testing.T) {
	env, _ := newPRFactsEnv(t, true)
	env.Engine.SetPRFactsRefreshDisabledForTest(true)
	num, _ := seedPRFactsItem(t, env, "Implement", false)
	RunPoll(t, env)
	st := prFactsStatus(t, env, num)
	if st.PR.CI.Valid || st.PR.PRState.Valid || strings.Contains(st.Attention.Summary, "CI is red") {
		t.Errorf("with the refresher off: ci=%+v state=%+v summary=%q", st.PR.CI, st.PR.PRState, st.Attention.Summary)
	}
}

func TestSimStatusToolMakesNoGitHubCall(t *testing.T) {
	env, _ := newPRFactsEnv(t, true)
	num, _ := seedPRFactsItem(t, env, "Implement", false)
	RunPoll(t, env)
	before := env.Sim.Log().Len()
	for i := 0; i < 3; i++ {
		prFactsStatus(t, env, num)
		if _, err := env.Engine.LocalAPIBackend().Board(localapi.BoardParams{}); err != nil {
			t.Fatal(err)
		}
	}
	if after := env.Sim.Log().Len(); after != before {
		t.Errorf("status/board made %d GitHub call(s)", after-before)
	}
}

func TestSimRedCIClearsOnRerunAndDropsOnNewHead(t *testing.T) {
	env, _ := newPRFactsEnv(t, true)
	num, sha := seedPRFactsItem(t, env, "Implement", false)
	RunPoll(t, env)
	if st := prFactsStatus(t, env, num); st.PR.CI.V != "red" {
		t.Fatalf("ci = %+v", st.PR.CI)
	}

	// A rerun on the same head goes green: no label or stage change involved.
	env.Sim.Sim().SeedCheckRun(env.OwnerRepo, sha, gh.CheckRun{Name: prFactsFailingCheck, Status: "completed", Conclusion: "success"})
	RunPoll(t, env)
	st := prFactsStatus(t, env, num)
	if st.PR.CI.V != "green" || len(st.PR.CIFailing) != 0 || strings.Contains(st.Attention.Summary, "CI is red") {
		t.Errorf("after rerun: ci=%+v failing=%v summary=%q", st.PR.CI, st.PR.CIFailing, st.Attention.Summary)
	}

	// A push moves the head. The old head's green must not render as the new
	// head's verdict.
	branch := fmt.Sprintf("fabrik/issue-%d", num)
	env.Sim.Sim().SeedCommit(env.OwnerRepo, branch, map[string]string{"more.txt": "x\n"}, "feat: more")
	newSHA, _ := env.Sim.Sim().HeadSHA(env.OwnerRepo, branch)
	env.Sim.Sim().SeedCheckRun(env.OwnerRepo, newSHA, gh.CheckRun{Name: prFactsFailingCheck, Status: "completed", Conclusion: "failure"})
	RunPoll(t, env)
	st = prFactsStatus(t, env, num)
	if st.PR.CI.V != "red" || st.PR.CIHeadSHA.V != newSHA || st.PR.HeadSHA.V != newSHA {
		t.Errorf("after push: ci=%+v ci_head=%+v head=%+v want red on %s", st.PR.CI, st.PR.CIHeadSHA, st.PR.HeadSHA, newSHA)
	}
}

func TestSimPRFactsAgeIntoStale(t *testing.T) {
	env, _ := newPRFactsEnv(t, true)
	num, _ := seedPRFactsItem(t, env, "Implement", false)
	RunPoll(t, env)
	if st := prFactsStatus(t, env, num); st.PR.FactsStale.V || st.PR.CIStale.V || !st.PR.CIAsOf.Valid {
		t.Fatalf("fresh: %+v", st.PR)
	}
	// No poll for longer than one interval: the values stay but are marked stale,
	// and a stale red is not surfaced as an attention item.
	env.Clock.Advance(env.PollInterval * 3)
	st := prFactsStatus(t, env, num)
	if !st.PR.FactsStale.Valid || !st.PR.FactsStale.V || !st.PR.CIStale.V {
		t.Errorf("aged facts not stale: %+v", st.PR)
	}
	if st.PR.CI.V != "red" {
		t.Errorf("a stale verdict keeps its value, got %+v", st.PR.CI)
	}
	if strings.Contains(st.Attention.Summary, "CI is red") {
		t.Errorf("stale red surfaced: %q", st.Attention.Summary)
	}
}

func TestSimRedCIWithAWorkerInFlightSaysSo(t *testing.T) {
	env := NewEnv(t, EnvOptions{Stages: prFactsStages(), MaxConcurrent: 2, Yolo: boolPtr(false), BoardCache: true})
	t.Cleanup(env.Engine.RegisterObservers())
	started := make(chan struct{})
	release := make(chan struct{})
	env.Claude.ForStage("Implement", func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, nc []gh.Comment, resume bool, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return simclaude.DefaultScript(ctx, stage, issue, nc, resume, workDir, opts)
	})
	num, _ := seedPRFactsItem(t, env, "Implement", false)
	env.Clock.Advance(env.PollInterval)
	if err := env.Engine.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(30 * time.Second):
		t.Fatal("the Implement worker never started")
	}
	defer close(release)
	st := prFactsStatus(t, env, num)
	if !strings.Contains(st.Attention.Summary, "CI is red on "+prFactsFailingCheck+"; a worker is running") {
		t.Errorf("summary with a worker in flight = %q", st.Attention.Summary)
	}
}

func simghPRSeed(branch string, issue int, draft bool) simgh.PRSeed {
	return simgh.PRSeed{Title: "feature", Head: branch, IssueNumber: issue, Draft: draft}
}
