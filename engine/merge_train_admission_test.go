package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

// admissionEngine is trainTestEngine with wait_for_ci enabled on the reroute target
// (Implement in that fixture), which is the #1821 gate's precondition.
func admissionEngine(t *testing.T, client *mockGitHubClient) *Engine {
	t.Helper()
	eng := trainTestEngine(t, client, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))
	yes := true
	for _, s := range eng.cfg.Stages {
		if s.Name == "Implement" {
			s.WaitForCI = &yes
		}
	}
	return eng
}

func admissionMembers(nums ...int) []trainMember {
	var ms []trainMember
	for _, n := range nums {
		ms = append(ms, trainMember{
			item:    makeTrainItem(n, fmt.Sprintf("Issue %d", n)),
			prNum:   100 + n,
			headSHA: fmt.Sprintf("sha%d", n),
		})
	}
	return ms
}

func redRun() gh.CheckRun {
	return gh.CheckRun{Name: "e2e", Status: "completed", Conclusion: "failure", OutputSummary: "spec exploded"}
}

func greenRun() gh.CheckRun {
	return gh.CheckRun{Name: "e2e", Status: "completed", Conclusion: "success"}
}

func pendingRun() gh.CheckRun {
	return gh.CheckRun{Name: "e2e", Status: "in_progress"}
}

func numbersOf(ms []trainMember) []int {
	var out []int
	for _, m := range ms {
		out = append(out, m.item.Number)
	}
	return out
}

func sameInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func pauseLabelsFor(client *mockGitHubClient, n int) []string {
	client.mu.Lock()
	defer client.mu.Unlock()
	var out []string
	for _, c := range client.addLabelCalls {
		if c.issueNumber == n && (c.labelName == "fabrik:paused" || c.labelName == "fabrik:awaiting-input") {
			out = append(out, c.labelName)
		}
	}
	return out
}

func TestAdmitTrainMembers_RedDeferred(t *testing.T) {
	client := &mockGitHubClient{
		fetchCheckRunsFn: func(_, _, sha string) ([]gh.CheckRun, error) {
			return []gh.CheckRun{redRun()}, nil
		},
	}
	eng := admissionEngine(t, client)
	got := eng.admitTrainMembers(context.Background(), &mergeTrainWorkerState{projectID: "PVT_1"}, "owner", "repo", admissionMembers(1))
	if len(got) != 0 {
		t.Fatalf("expected red member excluded, got %v", numbersOf(got))
	}
	if len(client.updateStatusCalls) != 1 || client.updateStatusCalls[0].optionID != "opt-implement" {
		t.Fatalf("expected reroute to Implement, got %+v", client.updateStatusCalls)
	}
	client.mu.Lock()
	comments := client.addCommentCalls
	client.mu.Unlock()
	if len(comments) != 1 {
		t.Fatalf("expected exactly 1 comment, got %d", len(comments))
	}
	body := comments[0].body
	for _, want := range []string{"deferred (own CI failing)", "**e2e**", "spec exploded", "not a merge-train interaction", "not** been paused"} {
		if !strings.Contains(body, want) {
			t.Errorf("comment missing %q:\n%s", want, body)
		}
	}
	for _, bad := range []string{"review-thread finding", "integration PR", "moved base branch"} {
		if strings.Contains(body, bad) {
			t.Errorf("comment must not contain %q:\n%s", bad, body)
		}
	}
	if pl := pauseLabelsFor(client, 1); len(pl) != 0 {
		t.Errorf("deferral must never pause, got %v", pl)
	}
}

func TestAdmitTrainMembers_AmbiguityAdmits(t *testing.T) {
	cases := map[string]func(string, string, string) ([]gh.CheckRun, error){
		"pending":   func(_, _, _ string) ([]gh.CheckRun, error) { return []gh.CheckRun{pendingRun()}, nil },
		"green":     func(_, _, _ string) ([]gh.CheckRun, error) { return []gh.CheckRun{greenRun()}, nil },
		"zero-runs": func(_, _, _ string) ([]gh.CheckRun, error) { return nil, nil },
		"error":     func(_, _, _ string) ([]gh.CheckRun, error) { return nil, fmt.Errorf("boom") },
		// Pending wins over failed inside ClassifyCheckRuns: a rerun in progress masks a stale failure.
		"pending-masks-failure": func(_, _, _ string) ([]gh.CheckRun, error) {
			return []gh.CheckRun{redRun(), {Name: "rerun", Status: "in_progress"}}, nil
		},
	}
	for name, fn := range cases {
		t.Run(name, func(t *testing.T) {
			client := &mockGitHubClient{fetchCheckRunsFn: fn}
			eng := admissionEngine(t, client)
			got := eng.admitTrainMembers(context.Background(), &mergeTrainWorkerState{projectID: "PVT_1"}, "owner", "repo", admissionMembers(1, 2))
			if !sameInts(numbersOf(got), []int{1, 2}) {
				t.Fatalf("expected both admitted, got %v", numbersOf(got))
			}
			if len(client.updateStatusCalls) != 0 || len(client.addCommentCalls) != 0 {
				t.Errorf("admit must have no side effects: status=%d comments=%d", len(client.updateStatusCalls), len(client.addCommentCalls))
			}
		})
	}
}

func TestAdmitTrainMembers_MixedBatchDefersOnlyRed(t *testing.T) {
	client := &mockGitHubClient{
		fetchCheckRunsFn: func(_, _, sha string) ([]gh.CheckRun, error) {
			if sha == "sha2" {
				return []gh.CheckRun{redRun()}, nil
			}
			return []gh.CheckRun{greenRun()}, nil
		},
	}
	eng := admissionEngine(t, client)
	got := eng.admitTrainMembers(context.Background(), &mergeTrainWorkerState{projectID: "PVT_1"}, "owner", "repo", admissionMembers(1, 2, 3))
	if !sameInts(numbersOf(got), []int{1, 3}) {
		t.Fatalf("expected [1 3] admitted in order, got %v", numbersOf(got))
	}
	if len(client.updateStatusCalls) != 1 {
		t.Errorf("expected exactly one reroute, got %d", len(client.updateStatusCalls))
	}
}

func TestAdmitTrainMembers_AllRedReturnsEmpty(t *testing.T) {
	client := &mockGitHubClient{
		fetchCheckRunsFn: func(_, _, _ string) ([]gh.CheckRun, error) { return []gh.CheckRun{redRun()}, nil },
	}
	eng := admissionEngine(t, client)
	got := eng.admitTrainMembers(context.Background(), &mergeTrainWorkerState{projectID: "PVT_1"}, "owner", "repo", admissionMembers(1, 2))
	if len(got) != 0 {
		t.Fatalf("expected empty batch, got %v", numbersOf(got))
	}
	if len(client.updateStatusCalls) != 2 {
		t.Errorf("expected both rerouted, got %d", len(client.updateStatusCalls))
	}
}

func TestAdmitTrainMembers_FailedRerouteExcludesWithoutSideEffects(t *testing.T) {
	client := &mockGitHubClient{
		fetchCheckRunsFn:          func(_, _, _ string) ([]gh.CheckRun, error) { return []gh.CheckRun{redRun()}, nil },
		updateProjectItemStatusFn: func(string, string, string, string) error { return fmt.Errorf("boom") },
	}
	eng := admissionEngine(t, client)
	eng.markPendingReviewEject("owner/repo", 1, 2)

	got := eng.admitTrainMembers(context.Background(), &mergeTrainWorkerState{projectID: "PVT_1"}, "owner", "repo", admissionMembers(1))
	if len(got) != 0 {
		t.Fatalf("a confirmed-red member must be excluded even when the reroute fails, got %v", numbersOf(got))
	}
	if len(client.addCommentCalls) != 0 {
		t.Errorf("no comment when reroute fails, got %d", len(client.addCommentCalls))
	}
	if _, ok := eng.takePendingReviewEject("owner/repo", 1); !ok {
		t.Errorf("pending review-eject signal must be left intact when the reroute fails")
	}
	if len(eng.mergeTrainCIDeferred) != 0 {
		t.Errorf("no dedupe record when reroute fails, got %v", eng.mergeTrainCIDeferred)
	}
}

func TestAdmitTrainMembers_DedupeBySHA(t *testing.T) {
	client := &mockGitHubClient{
		fetchCheckRunsFn: func(_, _, _ string) ([]gh.CheckRun, error) { return []gh.CheckRun{redRun()}, nil },
	}
	eng := admissionEngine(t, client)
	state := &mergeTrainWorkerState{projectID: "PVT_1"}
	ms := admissionMembers(1)

	eng.admitTrainMembers(context.Background(), state, "owner", "repo", ms)
	eng.admitTrainMembers(context.Background(), state, "owner", "repo", ms)
	if len(client.updateStatusCalls) != 2 {
		t.Errorf("a repeat deferral must still reroute, got %d reroutes", len(client.updateStatusCalls))
	}
	if len(client.addCommentCalls) != 1 {
		t.Fatalf("same SHA must not re-comment, got %d comments", len(client.addCommentCalls))
	}

	ms[0].headSHA = "newsha"
	eng.admitTrainMembers(context.Background(), state, "owner", "repo", ms)
	if len(client.addCommentCalls) != 2 {
		t.Errorf("a changed SHA must comment again, got %d comments", len(client.addCommentCalls))
	}
}

func TestAdmitTrainMembers_NonRedAdmitClearsDedupe(t *testing.T) {
	red := true
	client := &mockGitHubClient{
		fetchCheckRunsFn: func(_, _, _ string) ([]gh.CheckRun, error) {
			if red {
				return []gh.CheckRun{redRun()}, nil
			}
			return []gh.CheckRun{greenRun()}, nil
		},
	}
	eng := admissionEngine(t, client)
	state := &mergeTrainWorkerState{projectID: "PVT_1"}
	ms := admissionMembers(1)

	eng.admitTrainMembers(context.Background(), state, "owner", "repo", ms)
	red = false
	if got := eng.admitTrainMembers(context.Background(), state, "owner", "repo", ms); len(got) != 1 {
		t.Fatalf("green member must be admitted, got %v", numbersOf(got))
	}
	red = true
	eng.admitTrainMembers(context.Background(), state, "owner", "repo", ms)
	if len(client.addCommentCalls) != 2 {
		t.Errorf("a regression after recovery must be announced afresh, got %d comments", len(client.addCommentCalls))
	}
}

func TestAdmitTrainMembers_NeverPausesOrCountsEjection(t *testing.T) {
	client := &mockGitHubClient{
		fetchCheckRunsFn: func(_, _, _ string) ([]gh.CheckRun, error) { return []gh.CheckRun{redRun()}, nil },
	}
	eng := admissionEngine(t, client)
	eng.cfg.MaxMergeTrainEjections = 1
	state := &mergeTrainWorkerState{projectID: "PVT_1"}
	ms := admissionMembers(1)
	for i := 0; i < 3; i++ {
		ms[0].headSHA = fmt.Sprintf("sha-%d", i)
		eng.admitTrainMembers(context.Background(), state, "owner", "repo", ms)
	}
	if pl := pauseLabelsFor(client, 1); len(pl) != 0 {
		t.Errorf("deferral must never pause (even at MaxMergeTrainEjections=1), got %v", pl)
	}
	if len(eng.mergeTrainEjectionCounts) != 0 {
		t.Errorf("deferral must not count as an ejection, got %v", eng.mergeTrainEjectionCounts)
	}
}

func TestAdmitTrainMembers_SkippedWithoutWaitForCI(t *testing.T) {
	calls := 0
	client := &mockGitHubClient{
		fetchCheckRunsFn: func(_, _, _ string) ([]gh.CheckRun, error) {
			calls++
			return []gh.CheckRun{redRun()}, nil
		},
	}
	// trainTestEngine's Implement stage has no wait_for_ci.
	eng := trainTestEngine(t, client, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))
	got := eng.admitTrainMembers(context.Background(), &mergeTrainWorkerState{projectID: "PVT_1"}, "owner", "repo", admissionMembers(1, 2))
	if !sameInts(numbersOf(got), []int{1, 2}) {
		t.Fatalf("expected all admitted, got %v", numbersOf(got))
	}
	if calls != 0 {
		t.Errorf("expected no FetchCheckRuns calls without wait_for_ci, got %d", calls)
	}
}

func TestAdmitTrainMembers_ConsumesPendingReviewEjectOnDeferral(t *testing.T) {
	client := &mockGitHubClient{
		fetchCheckRunsFn: func(_, _, _ string) ([]gh.CheckRun, error) { return []gh.CheckRun{redRun()}, nil },
	}
	eng := admissionEngine(t, client)
	eng.markPendingReviewEject("owner/repo", 1, 2)
	eng.admitTrainMembers(context.Background(), &mergeTrainWorkerState{projectID: "PVT_1"}, "owner", "repo", admissionMembers(1))
	if _, ok := eng.takePendingReviewEject("owner/repo", 1); ok {
		t.Errorf("pending review-eject signal must be consumed on a successful deferral")
	}
	if len(client.addCommentCalls) != 1 {
		t.Errorf("a double-flagged member must get exactly one comment, got %d", len(client.addCommentCalls))
	}
}

func TestAdmitTrainMembers_RequiredContextRedDeferred(t *testing.T) {
	client := &mockGitHubClient{
		fetchCheckRunsFn: func(_, _, _ string) ([]gh.CheckRun, error) { return []gh.CheckRun{greenRun()}, nil },
		fetchCombinedStatusFn: func(_, _, _ string) ([]gh.CommitStatus, error) {
			return []gh.CommitStatus{{Context: "local/test", State: "failure"}}, nil
		},
	}
	eng := admissionEngine(t, client)
	// A required status context absent from the check runs and failing in the combined
	// status makes classifyLandingCI return red with no failing check runs to render.
	eng.cfg.RequiredStatusContexts = map[string][]string{"owner/repo": {"local/test"}}
	got := eng.admitTrainMembers(context.Background(), &mergeTrainWorkerState{projectID: "PVT_1"}, "owner", "repo", admissionMembers(1))
	if len(got) != 0 {
		t.Fatalf("expected required-context red to defer, got %v", numbersOf(got))
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.addCommentCalls) != 1 || !strings.Contains(client.addCommentCalls[0].body, "required status context") {
		t.Errorf("expected comment to fall back to the classifier detail, got %+v", client.addCommentCalls)
	}
}
