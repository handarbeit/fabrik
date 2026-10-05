package engine

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// infraSeamValidator is a trainValidateFn whose result per call is scripted.
type infraSeamValidator struct {
	mu      sync.Mutex
	script  []TrainCIResult // result per call; the last repeats
	calls   int
	infraAt int // informational
}

func (v *infraSeamValidator) fn(_ context.Context, members []trainMember) (TrainCIResult, *trainCIDiagnostic) {
	v.mu.Lock()
	defer v.mu.Unlock()
	i := v.calls
	v.calls++
	if i >= len(v.script) {
		i = len(v.script) - 1
	}
	switch v.script[i] {
	case TrainCIRed:
		return TrainCIRed, &trainCIDiagnostic{
			FailedChecks: []gh.CheckRun{{Name: "ci/test", Status: "completed", Conclusion: "failure"}},
			PRNum:        900, TrialSHA: "seam-trial-sha",
		}
	case TrainCIInfra:
		return TrainCIInfra, &trainCIDiagnostic{Note: "CI never started: workflow run 77 (CI) — startup_failure", PRNum: 900, TrialSHA: "seam-trial-sha"}
	}
	return v.script[i], nil
}

func (v *infraSeamValidator) count() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.calls
}

func trialCount(e *Engine, trainKey string) int {
	e.mergeTrainTrialsMu.Lock()
	defer e.mergeTrainTrialsMu.Unlock()
	return len(e.mergeTrainTrials[trainKey])
}

// assertNothingChargedOrMoved is the common R2/R4 assertion: no ejection, no
// pause, no landing, no board move, and the members' Queued status untouched.
func assertNothingChargedOrMoved(t *testing.T, eng *Engine, client *mockGitHubClient, n int) {
	t.Helper()
	for i := 1; i <= n; i++ {
		if c := ejectionCommentCount(client, i); c != 0 {
			t.Errorf("member #%d was ejected (%d comment(s)) — an infrastructure failure must not charge members", i, c)
		}
		eng.mergeTrainEjectionsMu.Lock()
		cnt := eng.mergeTrainEjectionCounts[fmt.Sprintf("owner/repo#%d", i)]
		eng.mergeTrainEjectionsMu.Unlock()
		if cnt != 0 {
			t.Errorf("member #%d counted %d toward MaxMergeTrainEjections", i, cnt)
		}
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.mergePRCalls) != 0 {
		t.Errorf("a trial abandoned for CI infrastructure must not land: merges = %d", len(client.mergePRCalls))
	}
	if len(client.updateStatusCalls) != 0 {
		t.Errorf("members must stay in Queued: %d board move(s)", len(client.updateStatusCalls))
	}
	for _, c := range client.addLabelCalls {
		if strings.Contains(c.labelName, "paused") {
			t.Errorf("a member was paused (%v)", c)
		}
	}
	for _, c := range client.addCommentCalls {
		if strings.Contains(c.body, "batch dissolved") {
			t.Errorf("an infrastructure abandon must not post a 'batch dissolved' comment: %q", c.body)
		}
	}
}

func infraSeamEngine(t *testing.T, script ...TrainCIResult) (*Engine, *mockGitHubClient, *infraSeamValidator, *WorktreeManager) {
	t.Helper()
	skipIfNoGit(t)
	_, _, _, wm := setupTrainRepo(t)
	eng, client, _ := seamTrainEngine(t, wm, func(map[int]bool) bool { return false })
	v := &infraSeamValidator{script: script}
	eng.trainValidateFn = v.fn
	eng.SetCIInfraTimingForTest(0, 0, time.Minute, -1)
	return eng, client, v, wm
}

func runInfraWorker(t *testing.T, eng *Engine, n int) {
	t.Helper()
	state := &mergeTrainWorkerState{assembling: true, projectID: "PVT_test"}
	eng.mergeTrainInFlight.Store(mergeTrainKey("owner/repo", "main"), state)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eng.runMergeTrainWorker(ctx, state, "owner", "repo", "main", makeSeamBatch(n))
}

// R2/R4: the worker abandons an infra trial with members Queued, nothing
// charged, the in-flight marker cleared, and the partition on cooldown.
func TestMergeTrainInfra_WorkerAbandonsWithoutCharging(t *testing.T) {
	eng, client, v, _ := infraSeamEngine(t, TrainCIInfra)
	runInfraWorker(t, eng, 3)

	if v.count() != 1 {
		t.Fatalf("validations = %d, want exactly 1 — an infra trial must not bisect or re-form", v.count())
	}
	assertNothingChargedOrMoved(t, eng, client, 3)
	key := mergeTrainKey("owner/repo", "main")
	if got := trialCount(eng, key); got != 0 {
		t.Errorf("runaway guard recorded %d trial(s), want 0 — an infra abandon is not a trial", got)
	}
	if _, held := eng.infraCooldownRemaining(key); !held {
		t.Error("the partition must be on cooldown after an infra abandon")
	}
	if _, ok := eng.mergeTrainInFlight.Load(key); ok {
		t.Error("in-flight marker not cleared")
	}
}

// The cooldown gates re-dispatch: no worker is started inside it.
func TestMergeTrainInfra_CooldownHoldsOffRedispatch(t *testing.T) {
	eng, _, v, _ := infraSeamEngine(t, TrainCIInfra)
	runInfraWorker(t, eng, 2)
	if v.count() != 1 {
		t.Fatalf("setup: validations = %d", v.count())
	}
	eng.dispatchMergeTrainWorker(context.Background(), makeSeamBatch(2), "PVT_test", "main")
	eng.wg.Wait()
	if v.count() != 1 {
		t.Fatalf("a worker ran inside the cooldown (validations = %d)", v.count())
	}
	if _, ok := eng.mergeTrainInFlight.Load(mergeTrainKey("owner/repo", "main")); ok {
		t.Error("a worker marker was registered inside the cooldown")
	}
}

func TestMergeTrainInfra_CooldownLapsesAndDispatchResumes(t *testing.T) {
	eng, _, v, _ := infraSeamEngine(t, TrainCIInfra, TrainCIGreen)
	eng.SetCIInfraTimingForTest(0, 0, 30*time.Millisecond, -1)
	runInfraWorker(t, eng, 2)
	time.Sleep(60 * time.Millisecond)
	if _, held := eng.infraCooldownRemaining(mergeTrainKey("owner/repo", "main")); held {
		t.Fatal("cooldown should have lapsed")
	}
	runInfraWorker(t, eng, 2)
	if v.count() != 2 {
		t.Fatalf("validations = %d, want a fresh trial after the cooldown", v.count())
	}
}

// R2: a bisection sub-trial that hits CI infrastructure aborts the episode; it
// is not read as a green half (which would degrade to one-at-a-time landing) and
// nothing is ejected. The red full-batch trial is real and stays counted; the
// infra sub-trial is not.
func TestMergeTrainInfra_BisectSubTrialAbortsEpisode(t *testing.T) {
	eng, client, v, _ := infraSeamEngine(t, TrainCIRed, TrainCIInfra)
	runInfraWorker(t, eng, 4)

	if v.count() != 2 {
		t.Fatalf("validations = %d, want 2 (the red trial and the abandoned sub-trial) — the episode must stop there", v.count())
	}
	assertNothingChargedOrMoved(t, eng, client, 4)
	if got := trialCount(eng, mergeTrainKey("owner/repo", "main")); got != 1 {
		t.Errorf("recorded trials = %d, want 1 (only the red trial counts)", got)
	}
	if _, held := eng.infraCooldownRemaining(mergeTrainKey("owner/repo", "main")); !held {
		t.Error("cooldown not set after an aborted bisection")
	}
}

// R2: the one-at-a-time fallback stops at the first infra singleton.
func TestMergeTrainInfra_OneAtATimeStopsAtInfra(t *testing.T) {
	// red full, green half A, green half B (non-isolable) → fallback → infra singleton.
	eng, client, v, _ := infraSeamEngine(t, TrainCIRed, TrainCIGreen, TrainCIGreen, TrainCIInfra)
	runInfraWorker(t, eng, 3)

	if v.count() != 4 {
		t.Fatalf("validations = %d, want 4 (red, 2 green halves, 1 infra singleton) — the fallback must stop at the first infra", v.count())
	}
	assertNothingChargedOrMoved(t, eng, client, 3)
	if _, held := eng.infraCooldownRemaining(mergeTrainKey("owner/repo", "main")); !held {
		t.Error("cooldown not set")
	}
}

// R2: a rebase re-validation that hits infra leaves the members Queued without
// the 'batch dissolved' comment a red re-validation earns.
func TestMergeTrainInfra_RebaseRevalidationLeavesMembersQueued(t *testing.T) {
	eng, client, v, wm := infraSeamEngine(t, TrainCIInfra)
	client.fetchCommitsBehindFn = func(_, _, _, _ string) (int, error) { return 1, nil } // main moved

	survivors := []trainMember{makeQueuedMember(1, 101, "One"), makeQueuedMember(2, 102, "Two")}
	state := &mergeTrainWorkerState{trialName: "merge-train-main-1", projectID: "PVT_test"}
	key := mergeTrainKey("owner/repo", "main")
	eng.mergeTrainInFlight.Store(key, state)
	p := trialParams{owner: "owner", repo: "repo", baseBranch: "main", trainKey: key, wm: wm, nextTrialName: trialNameGen("merge-train-main-1")}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eng.landGreenBatch(ctx, state, p, survivors)

	if v.count() != 1 {
		t.Fatalf("validations = %d, want 1", v.count())
	}
	assertNothingChargedOrMoved(t, eng, client, 2)
	client.mu.Lock()
	closes := len(client.closeIssueCalls)
	client.mu.Unlock()
	if closes != 0 {
		t.Errorf("an infra re-validation must not close PRs via dissolveBatch (closes = %d)", closes)
	}
	if _, held := eng.infraCooldownRemaining(key); !held {
		t.Error("cooldown not set")
	}
}

// The wrapper does not charge an infra trial, but still charges red and pending.
func TestAssembleAndValidate_InfraIsNotRecordedAsATrial(t *testing.T) {
	for _, tc := range []struct {
		result TrainCIResult
		want   int
	}{{TrainCIInfra, 0}, {TrainCIRed, 1}, {TrainCIPending, 1}, {TrainCIGreen, 0}} {
		eng, _, _, wm := infraSeamEngine(t, tc.result)
		p := trialParams{owner: "owner", repo: "repo", baseBranch: "main", trainKey: "owner/repo:main", wm: wm, nextTrialName: trialNameGen("t")}
		eng.assembleAndValidate(context.Background(), p, []trainMember{makeQueuedMember(1, 101, "One")}, "t")
		if got := trialCount(eng, "owner/repo:main"); got != tc.want {
			t.Errorf("result %v: recorded trials = %d, want %d", tc.result, got, tc.want)
		}
	}
}

// ── R3: only the owning worker acts on the trial PR ─────────────────────────

// A second dispatch for an in-flight partition returns on the marker and never
// reaches prepareTrainWorker -> reconstructTrainState (the stale-PR sweep), so
// the sweep cannot act on a trial PR while its worker is closing/reopening it.
func TestMergeTrainInfra_SweepNeverRunsForAnInFlightTrain(t *testing.T) {
	skipIfNoGit(t)
	_, _, _, wm := setupTrainRepo(t)
	eng, client, _ := seamTrainEngine(t, wm, func(map[int]bool) bool { return false })

	var mu sync.Mutex
	listCalls := 0
	client.listPRsFn = func(owner, repo string) ([]gh.PRDetails, error) {
		mu.Lock()
		listCalls++
		mu.Unlock()
		return nil, nil
	}
	inCI := make(chan struct{})
	release := make(chan struct{})
	eng.trainValidateFn = func(_ context.Context, _ []trainMember) (TrainCIResult, *trainCIDiagnostic) {
		close(inCI) // the worker is now "inside pollTrainCI"
		<-release
		return TrainCIPending, nil
	}

	batch := makeSeamBatch(2)
	eng.dispatchMergeTrainWorker(context.Background(), batch, "PVT_test", "")
	select {
	case <-inCI:
	case <-time.After(20 * time.Second):
		t.Fatal("worker never reached the CI poll")
	}
	mu.Lock()
	before := listCalls
	mu.Unlock()
	if before != 1 {
		t.Fatalf("setup: the worker's own reconstruct ran %d times, want 1", before)
	}

	key := mergeTrainKey("owner/repo", "")
	if _, ok := eng.mergeTrainInFlight.Load(key); !ok {
		t.Fatal("in-flight marker must be registered while the worker polls CI")
	}
	for i := 0; i < 3; i++ {
		eng.dispatchMergeTrainWorker(context.Background(), batch, "PVT_test", "")
	}
	mu.Lock()
	after := listCalls
	mu.Unlock()
	if after != before {
		t.Fatalf("a second dispatch reached the stale-PR sweep (ListPRs calls %d -> %d)", before, after)
	}
	close(release)
	eng.wg.Wait()
}

// The crash-in-window case: a closed-unmerged trial PR with no worker. The
// sweep neither reopens, closes, lands nor escalates it; the batch re-forms.
func TestMergeTrainInfra_SweepOnAClosedTrialPRWithNoWorker(t *testing.T) {
	skipIfNoGit(t)
	_, _, _, wm := setupTrainRepo(t)
	closedPR := gh.PRDetails{
		Number: 600, State: "closed", Merged: false,
		HeadRefName: "fabrik/merge-train/merge-train-main-1",
		Body:        "batch: #1, #2\n" + mergeTrainBatchMarker,
	}
	eng, client, rv := seamTrainEngine(t, wm, func(map[int]bool) bool { return false })
	client.listPRsFn = func(owner, repo string) ([]gh.PRDetails, error) { return []gh.PRDetails{closedPR}, nil }
	state := &mergeTrainWorkerState{assembling: true, projectID: "PVT_test"}
	eng.mergeTrainInFlight.Store(mergeTrainKey("owner/repo", "main"), state)

	if eng.reconstructTrainState(context.Background(), state, reconstructParams(wm), makeSeamBatch(2)) {
		t.Fatal("a closed-unmerged trial PR must not be 'handled' — reconstruct should let a fresh train form")
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.closeIssueCalls) != 0 || len(client.reopenIssueCalls) != 0 || len(client.mergePRCalls) != 0 ||
		len(client.updateStatusCalls) != 0 || len(client.addCommentCalls) != 0 || rv.count() != 0 {
		t.Errorf("sweep acted on a closed trial PR: closes=%d reopens=%d merges=%d moves=%d comments=%d validations=%d",
			len(client.closeIssueCalls), len(client.reopenIssueCalls), len(client.mergePRCalls),
			len(client.updateStatusCalls), len(client.addCommentCalls), rv.count())
	}
}

// Structural pin: neither the stale-PR sweep nor the closed-unmerged-trial
// escalation is reachable from the polling loop that closes and reopens the
// trial PR, and only the sanctioned callers retrigger a PR.
func TestMergeTrainInfra_OwnershipIsStructural(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	callers := map[string]map[string]bool{} // callee -> set of enclosing funcs
	inPoll := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range af.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				name := sel.Sel.Name
				if callers[name] == nil {
					callers[name] = map[string]bool{}
				}
				callers[name][fd.Name.Name] = true
				if fd.Name.Name == "pollTrainCI" {
					inPoll[name] = true
				}
				return true
			})
		}
	}
	for _, forbidden := range []string{"reconstructTrainState", "escalateClosedUnmergedTrial", "findIntegrationPR", "dissolveBatch", "cleanupTrialArtifacts", "landMergeTrainBatch"} {
		if inPoll[forbidden] {
			t.Errorf("pollTrainCI (which closes and reopens the trial PR) must not call %s", forbidden)
		}
	}
	var got []string
	for fn := range callers["retriggerPR"] {
		got = append(got, fn)
	}
	sort.Strings(got)
	if fmt.Sprint(got) != "[ciStartupCheck pollTrainCI]" {
		t.Errorf("retriggerPR callers = %v, want exactly [ciStartupCheck pollTrainCI]", got)
	}
}
