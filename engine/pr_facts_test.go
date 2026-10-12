package engine

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/attention"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

const prFactsSHA = "aaaa1111"

type prFactsEnv struct {
	e      *Engine
	client *mockGitHubClient
	clock  *settableClock
	runs   []gh.CheckRun
	reads  atomic.Int32
}

type settableClock struct{ t time.Time }

func (c *settableClock) Now() time.Time { return c.t }

func newPRFactsEnv(t *testing.T) *prFactsEnv {
	t.Helper()
	env := &prFactsEnv{clock: &settableClock{t: time.Now()}}
	env.client = &mockGitHubClient{
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			env.reads.Add(1)
			return env.runs, nil
		},
		fetchPRDetailsFn: func(owner, repo string, n int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: n, State: "open", HeadSHA: prFactsSHA, MergeableState: "clean"}, nil
		},
	}
	env.e = NewWithDeps(Config{
		Owner: "owner", Repo: "repo", ProjectNum: 1, PollSeconds: 30, MaxConcurrent: 3,
		StallThreshold: 30 * time.Minute, Stages: apiStages(),
	}, env.client, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))
	env.e.SetClock(env.clock)
	env.e.health.markStarted(env.clock.t.Add(-time.Hour))
	return env
}

// seedPR puts an Implement item with a linked open PR #50 at prFactsSHA into the
// store, as the probe would, and returns the board listing it.
func (env *prFactsEnv) seedPR(t *testing.T, number int, draft bool) *gh.ProjectBoard {
	t.Helper()
	env.e.seedAPIItem(t, number, "Implement")
	env.e.store.Apply(itemstate.PRHeadSHAUpdated{Repo: "owner/repo", Number: number, LinkedPRNum: 50 + number, SHA: prFactsSHA})
	env.e.store.Apply(itemstate.PRFactsObserved{Repo: "owner/repo", Number: number, PRNumber: 50 + number,
		Facts: itemstate.PRFacts{State: "open", Draft: draft, MergeableState: "clean", HeadSHA: prFactsSHA, ObservedAt: env.clock.t}})
	return env.board(number)
}

func (env *prFactsEnv) board(numbers ...int) *gh.ProjectBoard {
	b := &gh.ProjectBoard{}
	for _, n := range numbers {
		b.Items = append(b.Items, gh.ProjectItem{Number: n, Repo: "owner/repo", Status: "Implement"})
	}
	return b
}

func failedRun(name string, id int64) gh.CheckRun {
	return gh.CheckRun{ID: id, Name: name, Status: "completed", Conclusion: "failure"}
}

func TestSettlePRFacts_RedHeadWithNoGateAndNoWebhook(t *testing.T) {
	env := newPRFactsEnv(t)
	board := env.seedPR(t, 7, false)
	env.runs = []gh.CheckRun{failedRun("Test and vet", 1), passRun("lint", 2)}

	// Before the refresher runs: the PR number and head are known, CI is not.
	before := status(t, env.e, "7")
	if before.PR.CI.Valid {
		t.Fatalf("ci = %+v before any refresh, want unknown", before.PR.CI)
	}

	env.e.settlePRFacts(board)
	st := status(t, env.e, "7")
	if !st.PR.CI.Valid || st.PR.CI.V != "red" {
		t.Fatalf("ci = %+v, want red", st.PR.CI)
	}
	if len(st.PR.CIFailing) != 1 || st.PR.CIFailing[0] != "Test and vet" {
		t.Errorf("ci_failing = %v, want [Test and vet]", st.PR.CIFailing)
	}
	if !st.PR.CIHeadSHA.Valid || st.PR.CIHeadSHA.V != prFactsSHA || !st.PR.CIAsOf.Valid {
		t.Errorf("ci head/as-of: %+v %+v", st.PR.CIHeadSHA, st.PR.CIAsOf)
	}
	if !st.PR.PRState.Valid || st.PR.PRState.V != "open" || !st.PR.Draft.Valid || st.PR.Draft.V ||
		!st.PR.MergeableState.Valid || st.PR.MergeableState.V != "clean" {
		t.Errorf("pr facts: %+v", st.PR)
	}
	if st.Attention.CIFailing == nil || st.Attention.CIFailing[0] != "Test and vet" ||
		!strings.Contains(st.Attention.Summary, "CI is red on Test and vet") {
		t.Errorf("attention did not surface the red CI: %+v", st.Attention)
	}
	var sawReason bool
	for _, r := range st.Attention.Reasons {
		sawReason = sawReason || r.Code == attention.CodeCIRed
	}
	if !sawReason {
		t.Errorf("attention reasons lack %s: %+v", attention.CodeCIRed, st.Attention.Reasons)
	}

	// The same scenario fails with the refresher off: it is non-vacuous.
	off := newPRFactsEnv(t)
	offBoard := off.seedPR(t, 7, false)
	off.runs = env.runs
	off.e.SetPRFactsRefreshDisabledForTest(true)
	off.e.settlePRFacts(offBoard)
	if got := status(t, off.e, "7"); got.PR.CI.Valid || strings.Contains(got.Attention.Summary, "CI is red") {
		t.Errorf("with the refresher disabled ci=%+v summary=%q", got.PR.CI, got.Attention.Summary)
	}
}

func TestSettlePRFacts_RerunGreenSupersedesRed(t *testing.T) {
	env := newPRFactsEnv(t)
	board := env.seedPR(t, 7, false)
	env.runs = []gh.CheckRun{failedRun("Test and vet", 1)}
	env.e.settlePRFacts(board)
	if v := status(t, env.e, "7").PR.CI; v.V != "red" {
		t.Fatalf("ci = %+v, want red", v)
	}
	// A rerun on the same head: the older failure stays in the list but the
	// gates' latest-per-name rule makes it green.
	env.runs = []gh.CheckRun{failedRun("Test and vet", 1), passRun("Test and vet", 2)}
	env.clock.t = env.clock.t.Add(10 * time.Second)
	env.e.settlePRFacts(board)
	st := status(t, env.e, "7")
	if st.PR.CI.V != "green" || len(st.PR.CIFailing) != 0 {
		t.Errorf("after rerun ci=%+v failing=%v, want green", st.PR.CI, st.PR.CIFailing)
	}
	if strings.Contains(st.Attention.Summary, "CI is red") {
		t.Errorf("green CI still surfaced: %q", st.Attention.Summary)
	}
}

func TestSettlePRFacts_HeadChangeDropsTheOldVerdict(t *testing.T) {
	env := newPRFactsEnv(t)
	board := env.seedPR(t, 7, false)
	env.runs = []gh.CheckRun{failedRun("Test and vet", 1)}
	env.e.settlePRFacts(board)
	// A push moves the head; the old head's red must not render against it.
	env.e.store.Apply(itemstate.PRHeadSHAUpdated{Repo: "owner/repo", Number: 7, SHA: "bbbb2222"})
	st := status(t, env.e, "7")
	if st.PR.CI.Valid || st.PR.CIHeadSHA.Valid || len(st.PR.CIFailing) != 0 {
		t.Errorf("verdict carried across a head change: %+v", st.PR)
	}
	if !st.PR.HeadSHA.Valid || st.PR.HeadSHA.V != "bbbb2222" {
		t.Errorf("head_sha = %+v", st.PR.HeadSHA)
	}
	if strings.Contains(st.Attention.Summary, "CI is red") {
		t.Errorf("attention kept a stale head's red: %q", st.Attention.Summary)
	}
}

func TestSettlePRFacts_FreshnessIsReportedAsStale(t *testing.T) {
	env := newPRFactsEnv(t)
	board := env.seedPR(t, 7, false)
	env.runs = []gh.CheckRun{passRun("Test and vet", 1)}
	env.e.settlePRFacts(board)

	st := status(t, env.e, "7")
	if !st.PR.CIStale.Valid || st.PR.CIStale.V || !st.PR.FactsStale.Valid || st.PR.FactsStale.V {
		t.Errorf("fresh facts reported stale: ci=%+v facts=%+v", st.PR.CIStale, st.PR.FactsStale)
	}
	// One poll interval (30 s) and a bit later with no refresh: stale, still
	// shown, not defaulted.
	env.clock.t = env.clock.t.Add(31 * time.Second)
	st = status(t, env.e, "7")
	if !st.PR.CIStale.V || !st.PR.FactsStale.V {
		t.Errorf("aged facts not stale: ci=%+v facts=%+v", st.PR.CIStale, st.PR.FactsStale)
	}
	if st.PR.CI.V != "green" {
		t.Errorf("stale ci = %+v, want the value kept", st.PR.CI)
	}
	// A stale red is not surfaced as attention.
	env.runs = []gh.CheckRun{failedRun("Test and vet", 1)}
	env.e.settlePRFacts(board)
	env.clock.t = env.clock.t.Add(31 * time.Second)
	if got := status(t, env.e, "7"); strings.Contains(got.Attention.Summary, "CI is red") {
		t.Errorf("stale red surfaced: %q", got.Attention.Summary)
	}
}

func TestSettlePRFacts_NeverObservedIsUnknownNotStale(t *testing.T) {
	env := newPRFactsEnv(t)
	env.e.seedAPIItem(t, 7, "Implement")
	env.e.store.Apply(itemstate.PRHeadSHAUpdated{Repo: "owner/repo", Number: 7, LinkedPRNum: 57, SHA: prFactsSHA})
	pr := status(t, env.e, "7").PR
	if pr.PRState.Valid || pr.Draft.Valid || pr.CI.Valid || pr.FactsStale.Valid || pr.CIStale.Valid || pr.FactsAsOf.Valid {
		t.Errorf("never-observed PR must be all unknown: %+v", pr)
	}
}

func TestSettlePRFacts_RESTFallbackFillsFactsTheProbeCannotSee(t *testing.T) {
	env := newPRFactsEnv(t)
	env.e.seedAPIItem(t, 7, "Implement", "base:release")
	env.e.store.Apply(itemstate.PRDetailsUpdated{Repo: "owner/repo", Number: 7, PRNumber: 57, State: "open"})
	env.client.fetchPRDetailsFn = func(owner, repo string, n int) (*gh.PRDetails, error) {
		return &gh.PRDetails{Number: n, State: "open", Draft: true, HeadSHA: prFactsSHA, MergeableState: "UNKNOWN"}, nil
	}
	env.runs = []gh.CheckRun{passRun("Test and vet", 1)}
	env.e.settlePRFacts(env.board(7))
	pr := status(t, env.e, "7").PR
	if !pr.PRState.Valid || pr.PRState.V != "open" || !pr.Draft.V || pr.MergeableState.Valid {
		t.Errorf("fallback facts: %+v", pr)
	}
	if pr.CI.V != "green" {
		t.Errorf("ci = %+v, want green", pr.CI)
	}
}

func TestSettlePRFacts_MergedPRGetsNoVerdict(t *testing.T) {
	env := newPRFactsEnv(t)
	board := env.seedPR(t, 7, false)
	env.runs = []gh.CheckRun{failedRun("Test and vet", 1)}
	env.e.settlePRFacts(board)
	env.clock.t = env.clock.t.Add(time.Second)
	env.e.store.Apply(itemstate.PRFactsObserved{Repo: "owner/repo", Number: 7, PRNumber: 57,
		Facts: itemstate.PRFacts{State: "closed", Merged: true, HeadSHA: prFactsSHA, ObservedAt: env.clock.t}})
	env.reads.Store(0)
	env.e.settlePRFacts(board)
	st := status(t, env.e, "7")
	if st.PR.CI.Valid {
		t.Errorf("merged PR shows a verdict: %+v", st.PR.CI)
	}
	if env.reads.Load() != 0 {
		t.Errorf("check runs read for a merged PR (%d calls)", env.reads.Load())
	}
}

func TestSettlePRFacts_ReadErrorKeepsTheRecordUnstamped(t *testing.T) {
	env := newPRFactsEnv(t)
	board := env.seedPR(t, 7, false)
	env.runs = []gh.CheckRun{failedRun("Test and vet", 1)}
	env.e.settlePRFacts(board)
	first := status(t, env.e, "7").PR.CIAsOf
	env.client.fetchCheckRunsFn = func(owner, repo, sha string) ([]gh.CheckRun, error) { return nil, errors.New("boom") }
	env.clock.t = env.clock.t.Add(20 * time.Second)
	env.e.settlePRFacts(board)
	pr := status(t, env.e, "7").PR
	if pr.CI.V != "red" || !pr.CIAsOf.V.Equal(first.V) {
		t.Errorf("a failed read restamped or dropped the record: %+v (was %+v)", pr.CIAsOf, first)
	}
}

func TestSettlePRFacts_PerPollBudgetRotatesOldestFirst(t *testing.T) {
	env := newPRFactsEnv(t)
	var numbers []int
	for n := 10; n < 10+prFactsCheckBudgetPerPoll+4; n++ {
		env.seedPR(t, n, false)
		numbers = append(numbers, n)
	}
	board := env.board(numbers...)
	env.runs = []gh.CheckRun{failedRun("Test and vet", 1)}
	env.e.settlePRFacts(board)
	if got := env.reads.Load(); got != prFactsCheckBudgetPerPoll {
		t.Fatalf("check-run reads in one poll = %d, want the budget %d", got, prFactsCheckBudgetPerPoll)
	}
	// The next poll starts with the items the first one left unrefreshed.
	env.clock.t = env.clock.t.Add(time.Second)
	env.reads.Store(0)
	env.e.settlePRFacts(board)
	var refreshed int
	for _, n := range numbers {
		if st := env.e.store; st != nil {
			if snap, ok := st.Peek("owner/repo", n); ok {
				if lpr := snap.State().LinkedPR; lpr != nil && lpr.CIVerdict.Verdict != "" {
					refreshed++
				}
			}
		}
	}
	if refreshed != len(numbers) {
		t.Errorf("after two polls %d of %d items have a verdict", refreshed, len(numbers))
	}
}

func TestAttention_RedCIWorkerWording(t *testing.T) {
	env := newPRFactsEnv(t)
	board := env.seedPR(t, 7, false)
	env.runs = []gh.CheckRun{failedRun("Test and vet", 1)}
	env.e.settlePRFacts(board)

	classify := func() attention.Result {
		snap, _ := env.e.store.Peek("owner/repo", 7)
		st := snap.State()
		return attention.Classify(env.e.attentionInput(&st, env.clock.t, 30*time.Minute, time.Time{}, env.e.peekBlockerLookup()))
	}
	// No worker: ranked.
	if r := classify(); r.Rank != attention.RankCIRed {
		t.Errorf("unaddressed red CI rank = %d, want %d (%s)", r.Rank, attention.RankCIRed, r.Summary)
	}
	// A stage run is working: the summary says so and does not claim a CI fix.
	env.e.store.Apply(itemstate.WorkerEntered{Repo: "owner/repo", Number: 7, StageName: "Implement", StartedAt: env.clock.t, Kind: itemstate.WorkerKindStageRun})
	r := classify()
	if !strings.Contains(r.Summary, "CI is red on Test and vet; a worker is running") || r.Rank == attention.RankCIRed {
		t.Errorf("stage-run worker: rank=%d summary=%q", r.Rank, r.Summary)
	}
	env.e.store.Apply(itemstate.WorkerExited{Repo: "owner/repo", Number: 7})
	env.e.store.Apply(itemstate.WorkerEntered{Repo: "owner/repo", Number: 7, StageName: "Implement", StartedAt: env.clock.t, Kind: itemstate.WorkerKindCIFixReinvoke})
	if r := classify(); !strings.Contains(r.Summary, "a CI-fix reinvoke is addressing it") {
		t.Errorf("ci-fix worker summary = %q", r.Summary)
	}
}

func TestAttention_RedCIOnDraftIsReportedNotRanked(t *testing.T) {
	env := newPRFactsEnv(t)
	board := env.seedPR(t, 7, true)
	env.runs = []gh.CheckRun{failedRun("Test and vet", 1)}
	env.e.settlePRFacts(board)
	snap, _ := env.e.store.Peek("owner/repo", 7)
	st := snap.State()
	r := attention.Classify(env.e.attentionInput(&st, env.clock.t, 30*time.Minute, time.Time{}, env.e.peekBlockerLookup()))
	if !strings.Contains(r.Summary, "(draft PR)") || r.Rank == attention.RankCIRed {
		t.Errorf("draft: rank=%d summary=%q", r.Rank, r.Summary)
	}
}
