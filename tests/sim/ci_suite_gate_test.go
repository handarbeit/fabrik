package sim

import (
	"strings"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/tests/sim/simgh/ghfault"
)

// Scenarios for #1822: the CI gate must not clear on a partial check set.
//
// A job queued for a runner (a needs:-dependent job, or one waiting on a busy
// runner) has no check run yet, so an all-green *prefix* of a workflow reads as
// a complete pass — and GitHub itself reports mergeable_state "clean" whenever
// no *required* check is outstanding. The check suite is created at queue time
// and stays non-completed until every job has run; the engine now consults it.
//
// Every scenario registers ciFixSentinel as the required context. That is
// load-bearing in the same way it is for TestCIFixReinvoke: with no required
// context the derived mergeable_state is vacuously "clean" before anything is
// seeded, racing the gate closed before the scenario can act. The sentinel
// going green then models the incident's shape exactly — the required check
// passes, the (non-required) E2E job has not been scheduled yet, and GitHub
// says "clean".

const suiteGateSuiteID = 8100

// newSuiteGateEnv builds the shared environment. pollSeconds sets how far each
// poll advances the injected clock, which is what makes a multi-minute window
// cheap to cross.
func newSuiteGateEnv(t *testing.T, start time.Time, pollSeconds int, configure func(*engine.Config)) *Env {
	t.Helper()
	env := NewEnv(t, EnvOptions{
		Stages: ciFixStages(),
		// StartTime near real now for the same reason as TestCIFixReinvoke: the
		// awaiting-ci backstop compares real time.Since against a clock-stamped
		// label time, so a far-past start would fire it instantly. Scenarios that
		// want that (the stuck-suite one) pass a backdated start on purpose.
		StartTime: start,
		ConfigureCfg: func(cfg *engine.Config) {
			cfg.MaxCiFixCycles = 5
			cfg.PollSeconds = pollSeconds
			if configure != nil {
				configure(cfg)
			}
		},
	})
	// A CI-fix reinvoke pushes a new commit whose own check run passes, so a
	// scenario that goes red can complete afterwards.
	env.Claude.ForStageComments("Validate", ciFixCommentScript(env.Sim, env.OwnerRepo, "success"))
	env.Sim.Sim().SeedRepoAccess(env.OwnerRepo, gh.RepoAccess{AllowAutoMerge: false, CanPush: true})
	env.Sim.Sim().SeedRequiredContexts(env.OwnerRepo, "main", []string{ciFixSentinel})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	return env
}

// reachAwaitingCI drives the item to the point where the initial Validate
// dispatch has finished and the gate is the only thing between it and Done,
// returning the PR head SHA. Nothing has been seeded for that SHA yet.
func reachAwaitingCI(t *testing.T, env *Env, title string) (int, string) {
	t.Helper()
	num := FileIssue(t, env, title, "Prove #1822's suite-aware CI gate.", "Implement")
	WaitForIssueLabel(t, env, num, "fabrik:awaiting-ci", 80)
	pr, err := env.Sim.FetchLinkedPR(env.Owner, env.Repo, num)
	if err != nil || pr == nil || pr.Number == 0 {
		t.Fatalf("expected a linked PR, got %+v (err %v)", pr, err)
	}
	return num, pr.HeadSHA
}

// seedGreenPrefix records the incident's all-green prefix: the required
// sentinel and a build job, both completed/success.
func seedGreenPrefix(t *testing.T, env *Env, sha string) {
	t.Helper()
	sim := env.Sim.Sim()
	sim.SeedCheckRun(env.OwnerRepo, sha, gh.CheckRun{Name: ciFixSentinel, Status: "completed", Conclusion: "success"}).
		SeedCheckRun(env.OwnerRepo, sha, gh.CheckRun{Name: "Build (app)", Status: "completed", Conclusion: "success"})
	if err := sim.Err(); err != nil {
		t.Fatalf("seeding green prefix: %v", err)
	}
}

// inertSuites seeds the two installed-but-inert App suites seen in the
// incident: queued, zero runs, hours old.
func inertSuites(t *testing.T, env *Env, sha string) {
	t.Helper()
	old := env.Clock.Now().Add(-4 * time.Hour)
	env.Sim.Sim().
		SeedCheckSuite(env.OwnerRepo, sha, gh.CheckSuite{AppSlug: "cursor", Status: "queued", CreatedAt: old}).
		SeedCheckSuite(env.OwnerRepo, sha, gh.CheckSuite{AppSlug: "claude", Status: "queued", CreatedAt: old})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seeding inert suites: %v", err)
	}
}

// assertGateHolding fails if the gate has cleared or the item has landed.
func assertGateHolding(t *testing.T, env *Env, num int, when string) {
	t.Helper()
	item := projectItem(t, env, num)
	if hasLabel(item.Labels, "stage:Validate:complete") {
		t.Fatalf("%s: stage:Validate:complete granted — the gate cleared on a partial check set\n\n%s", when, diagnostics(env))
	}
	if !hasLabel(item.Labels, "fabrik:awaiting-ci") {
		t.Fatalf("%s: fabrik:awaiting-ci no longer present\n\n%s", when, diagnostics(env))
	}
	if item.IsClosed {
		t.Fatalf("%s: item closed — a PR landed over a partial check set", when)
	}
}

// TestCISuiteGate_IncidentReproduction is Req 7: a green prefix of check runs,
// a suite still in progress, then a failing check run created later. The gate
// must not have cleared in between, and must classify the SHA red once the
// failure lands (observable as the CI-fix reinvoke dispatching).
//
// Without suite awareness the gate cleared on the very first poll: the required
// sentinel is green so mergeable_state is "clean", and every check run that
// exists is green.
func TestCISuiteGate_IncidentReproduction(t *testing.T) {
	t.Parallel()
	env := newSuiteGateEnv(t, time.Now(), 30, nil)
	num, sha1 := reachAwaitingCI(t, env, "sim ci suite gate: incident")

	seedGreenPrefix(t, env, sha1)
	inertSuites(t, env, sha1)
	env.Sim.Sim().SeedCheckSuite(env.OwnerRepo, sha1,
		gh.CheckSuite{ID: suiteGateSuiteID, AppSlug: "github-actions", Status: "in_progress", LatestCheckRunsCount: 2})
	// Four simulated minutes on, the queued E2E job finally gets a runner, runs,
	// and fails; the workflow's suite completes red.
	failAt := env.Clock.Now().Add(4 * time.Minute)
	env.Sim.Sim().
		SeedCheckRunsAt(env.OwnerRepo, sha1, failAt,
			gh.CheckRun{Name: "E2E (Playwright)", Status: "completed", Conclusion: "failure"}).
		SeedCheckSuitesAt(env.OwnerRepo, sha1, failAt,
			gh.CheckSuite{ID: suiteGateSuiteID, AppSlug: "github-actions", Status: "completed", Conclusion: "failure", LatestCheckRunsCount: 3})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	// Phase 1: six polls = three simulated minutes, before the failure lands.
	// The gate must hold on every one of them.
	for i := 0; i < 6; i++ {
		RunPoll(t, env)
		assertGateHolding(t, env, num, "before the E2E check run exists")
	}
	if got := env.Claude.CommentCallCount("Validate"); got != 0 {
		t.Fatalf("CI-fix reinvoke fired %d time(s) before any failure existed", got)
	}
	// Call budget: the suite read is only spent on a would-be-green verdict, at
	// most once per poll for this item — the same cadence as the live check-run
	// refresh, not a per-poll multiple. Zero would mean the gate held for some
	// other reason.
	reads := len(env.Sim.Log().ByMethod("FetchCheckSuites"))
	if reads == 0 {
		t.Fatal("the gate never read check suites — it held for some other reason")
	}
	t.Logf("FetchCheckSuites calls over 6 held polls: %d", reads)
	if reads > 6 {
		t.Errorf("FetchCheckSuites called %d times over 6 polls — want at most one per poll", reads)
	}

	// Phase 2: the failure lands. The SHA must be classified red — which is what
	// dispatches the CI-fix reinvoke — even though the suite is the thing that was
	// holding the gate a moment ago.
	AdvanceUntil(t, env, func(env *Env) bool {
		cur, err := env.Sim.FetchLinkedPR(env.Owner, env.Repo, num)
		return err == nil && cur != nil && cur.HeadSHA != sha1
	}, 80)
	if env.Claude.CommentCallCount("Validate") == 0 {
		t.Fatal("head moved without a CI-fix reinvoke")
	}
	if hasLabel(IssueLabels(t, env, num), "stage:Validate:complete") && !projectItem(t, env, num).IsClosed {
		// Allowed only once the fix's own SHA is green; the waits below confirm it.
		t.Log("stage:Validate:complete granted after the fix commit")
	}
	WaitForIssueClosed(t, env, num, 80)
}

// TestCISuiteGate_InertSuiteDoesNotDeadlock is Req 3: installed-but-inert App
// suites (queued, zero runs, hours old) must never hold the gate. Also the
// counterweight to the incident test — a gate that held on "every suite must be
// completed" would fail here.
func TestCISuiteGate_InertSuiteDoesNotDeadlock(t *testing.T) {
	t.Parallel()
	env := newSuiteGateEnv(t, time.Now(), 30, nil)
	num, sha := reachAwaitingCI(t, env, "sim ci suite gate: inert")

	seedGreenPrefix(t, env, sha)
	inertSuites(t, env, sha)
	env.Sim.Sim().SeedCheckSuite(env.OwnerRepo, sha,
		gh.CheckSuite{AppSlug: "github-actions", Status: "completed", Conclusion: "success", LatestCheckRunsCount: 2})

	WaitForIssueLabel(t, env, num, "stage:Validate:complete", 40)
	WaitForIssueClosed(t, env, num, 40)
}

// TestCISuiteGate_PostPushWindowHoldsThenClears is Req 4: a real suite that
// exists with zero runs right after a push (its first job has not registered)
// holds the gate for the post-push dwell, measured from the suite's own
// created_at, and only then reads as inert.
//
// The seeded App is deliberately *not* github-actions (#1829): a github-actions
// suite is never inert regardless of age (see
// TestCISuiteGate_ActionsSuiteNeverInertOnAge below) — the dwell-then-inert
// mechanism this test pins now applies only to every other App.
func TestCISuiteGate_PostPushWindowHoldsThenClears(t *testing.T) {
	t.Parallel()
	env := newSuiteGateEnv(t, time.Now(), 30, func(cfg *engine.Config) { cfg.PostPushDwell = 90 * time.Second })
	num, sha := reachAwaitingCI(t, env, "sim ci suite gate: post-push")

	seedGreenPrefix(t, env, sha)
	// Created "now", no runs yet: the window in which a real suite is
	// indistinguishable from an inert one except by age.
	env.Sim.Sim().SeedCheckSuite(env.OwnerRepo, sha, gh.CheckSuite{AppSlug: "some-other-ci-app", Status: "queued"})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	// 30s and 60s old: inside the 90s dwell.
	for i := 0; i < 2; i++ {
		RunPoll(t, env)
		assertGateHolding(t, env, num, "inside the post-push dwell")
	}
	// Past the dwell a suite that never registered a run is inert: no deadlock.
	WaitForIssueLabel(t, env, num, "stage:Validate:complete", 20)
	WaitForIssueClosed(t, env, num, 40)
}

// TestCISuiteGate_ActionsSuiteNeverInertOnAge is #1829's own regression guard:
// a github-actions suite with zero check runs must hold the gate no matter how
// far past the post-push dwell it ages — the incident's 17-minute wait for a
// runner is the shape this proves, at simulated-clock speed — and must only
// clear once GitHub actually settles the suite to completed. Together with
// TestCISuiteGate_PostPushWindowHoldsThenClears (still-inert-eventually for
// every other App) this pins both halves of the #1829 fix: never clears by age
// alone, but does clear once really done (no deadlock regression against
// #1822's own acceptance criteria).
func TestCISuiteGate_ActionsSuiteNeverInertOnAge(t *testing.T) {
	t.Parallel()
	env := newSuiteGateEnv(t, time.Now(), 30, func(cfg *engine.Config) { cfg.PostPushDwell = 90 * time.Second })
	num, sha := reachAwaitingCI(t, env, "sim ci suite gate: actions never inert on age")

	seedGreenPrefix(t, env, sha)
	const actionsSuiteID = 8300
	env.Sim.Sim().SeedCheckSuite(env.OwnerRepo, sha,
		gh.CheckSuite{ID: actionsSuiteID, AppSlug: "github-actions", Status: "queued"})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	// Several simulated minutes — well past the 90s dwell that would have made
	// this suite read as inert before #1829. The gate must hold throughout.
	for i := 0; i < 10; i++ {
		RunPoll(t, env)
		assertGateHolding(t, env, num, "well past the post-push dwell, still zero runs")
	}

	// The runner finally picks up the job and the workflow completes green.
	env.Sim.Sim().SeedCheckSuitesAfter(env.OwnerRepo, sha, 30*time.Second,
		gh.CheckSuite{ID: actionsSuiteID, AppSlug: "github-actions", Status: "completed", Conclusion: "success", LatestCheckRunsCount: 3})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seeding completion: %v", err)
	}
	WaitForIssueLabel(t, env, num, "stage:Validate:complete", 20)
	WaitForIssueClosed(t, env, num, 40)
}

// TestCISuiteGate_SuiteReadFaultHolds is Req 5: when the suite read errors the
// gate holds rather than clearing, and resumes once the read recovers.
func TestCISuiteGate_SuiteReadFaultHolds(t *testing.T) {
	t.Parallel()
	env := newSuiteGateEnv(t, time.Now(), 30, nil)
	num, sha := reachAwaitingCI(t, env, "sim ci suite gate: read fault")

	seedGreenPrefix(t, env, sha)
	env.Sim.Faults().FailAlways("FetchCheckSuites", ghfault.ServerError())

	for i := 0; i < 5; i++ {
		RunPoll(t, env)
		assertGateHolding(t, env, num, "with the suite read failing")
	}
	env.Sim.Faults().Clear("FetchCheckSuites")
	WaitForIssueLabel(t, env, num, "stage:Validate:complete", 40)
	WaitForIssueClosed(t, env, num, 40)
}

// TestCISuiteGate_StuckSuiteEscalatesAtBackstop is Req 6: a suite stuck
// in_progress forever is bounded by CIBackstopTimeout and escalates the same way
// a stuck check run does — a pause, not a hang — and the pause comment names
// the suite so an operator can tell which workflow is stuck. What this pins is
// the escalation and its wording; the hold itself is pinned by the scenarios
// above (the backdated clock makes the backstop fire before the hold could be
// observed clearing).
func TestCISuiteGate_StuckSuiteEscalatesAtBackstop(t *testing.T) {
	t.Parallel()
	// Backdated start: the backstop compares real time.Since against the
	// clock-stamped awaiting-ci label time, exactly as timeout_test.go exploits.
	env := newSuiteGateEnv(t, time.Now().Add(-24*time.Hour), 30, func(cfg *engine.Config) {
		cfg.CIBackstopTimeout = time.Hour
	})
	num, sha := reachAwaitingCI(t, env, "sim ci suite gate: stuck")

	seedGreenPrefix(t, env, sha)
	env.Sim.Sim().SeedCheckSuite(env.OwnerRepo, sha,
		gh.CheckSuite{AppSlug: "github-actions", Status: "in_progress", LatestCheckRunsCount: 2})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	WaitForIssueLabel(t, env, num, "fabrik:paused", 40)
	item := projectItem(t, env, num)
	if item.IsClosed || hasLabel(item.Labels, "stage:Validate:complete") {
		t.Fatalf("a stuck suite must escalate, not clear or land: %v", item.Labels)
	}
	comments, err := env.Sim.FetchIssueComments(env.Owner, env.Repo, num)
	if err != nil {
		t.Fatalf("FetchIssueComments: %v", err)
	}
	var found bool
	for _, c := range comments {
		if strings.HasPrefix(c.Body, "🏭 **Fabrik — CI wait timeout**") && strings.Contains(c.Body, "github-actions (in_progress, 2 runs)") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no CI wait timeout comment naming the stuck suite\n\n%s", diagnostics(env))
	}
}

// TestMergeTrainSingletonFastPath_OutstandingSuite_BuildsTrialInstead is Req 2
// for the merge train: a single Queued member whose own head has a green, complete
// check run but a suite still in progress must not take the fast path (which lands
// the member's own PR with no trial). Without suite awareness the same setup lands
// via MergePRAtHeadSHA with zero draft PRs — see the sibling scenario in
// mergetrain_singleton_fastpath_test.go — so a trial being built here is evidence
// the suite hold fired, not an artifact of a scenario the fast path could never
// have taken.
func TestMergeTrainSingletonFastPath_OutstandingSuite_BuildsTrialInstead(t *testing.T) {
	t.Parallel()
	env := mergeTrainEnv(t, mergeTrainEnvOptions{})

	num, prNum := QueueMember(t, env, "fastpath-suite", map[string]string{"fastpath-suite.txt": "s\n"})
	pr, err := env.Sim.Sim().FetchLinkedPR(env.Owner, env.Repo, num)
	if err != nil || pr == nil {
		t.Fatalf("could not resolve queued member's own linked PR: %v", err)
	}
	env.Sim.Sim().SeedCheckRun(env.OwnerRepo, pr.HeadSHA, greenCheckRun("")).
		SeedCheckSuite(env.OwnerRepo, pr.HeadSHA,
			gh.CheckSuite{AppSlug: "github-actions", Status: "in_progress", LatestCheckRunsCount: 1})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	startTrialVerdictSeeder(t, env, allGreenVerdict)

	RunPoll(t, env)
	WaitForProjectStatus(t, env, num, "Done", 20)
	WaitForIssueClosed(t, env, num, 5)

	if got := len(env.Sim.Log().ByMethod("MergePRAtHeadSHA")); got != 0 {
		t.Errorf("fast path merged the member's own PR (%d MergePRAtHeadSHA call(s)) over a running suite", got)
	}
	if got := draftPRCount(env); got < 1 {
		t.Errorf("expected the ordinary trial path to build a draft CI PR, got %d", got)
	}
	for _, m := range env.Sim.Log().ByMethod("MergePR") {
		if m.Args.Number == prNum {
			t.Errorf("merged the member's own PR #%d directly", prNum)
		}
	}
}
