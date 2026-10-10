package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// Red-singleton auto-repair tests (#2045, ADR-2045). Every behavioural test has a
// neutralisation counterpart: the same scenario with the cap at 0 (feature disabled) must
// produce today's ADR-1545 pause instead.

func autoRepairEngine(t *testing.T, client *mockGitHubClient, capN int) *Engine {
	t.Helper()
	eng := NewWithDeps(
		Config{
			Owner: "owner", Repo: "repo", ProjectNum: 1, User: "testuser", Token: "token",
			MaxConcurrent: 5, MaxMergeTrainEjections: 3, MergeTrain: "on",
			CIBackstopTimeout:          100 * time.Millisecond,
			MaxTrainAutoRepairAttempts: capN,
			Stages: []*stages.Stage{
				{Name: "Research", Order: 1, Prompt: "r"},
				{Name: "Validate", Order: 3, Prompt: "v"},
				{Name: "Queued", Order: 10, HoldingStage: true, MaxTurns: 10},
				{Name: "Done", Order: 99, Prompt: "c"},
			},
		},
		client, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()),
	)
	eng.statusField = &gh.StatusField{
		FieldID: "sf-test-1",
		Options: map[string]string{"Done": "opt-done", "Queued": "opt-queued", "Validate": "opt-validate"},
	}
	// Default: the member is NOT already fixed (live head one commit behind the live base).
	return eng
}

func repairMember() trainMember {
	return trainMember{item: makeTrainItem(1, "Issue 1"), prNum: 10, headSHA: "head-1"}
}

func repairParams(baseSHA string) trialParams {
	return trialParams{owner: "owner", repo: "repo", baseBranch: "main", baseSHA: baseSHA}
}

func repairDiag() *trainCIDiagnostic {
	return &trainCIDiagnostic{
		FailedChecks: []gh.CheckRun{{Name: "ci/build", Status: "completed", Conclusion: "failure", OutputText: "TS2322: type mismatch"}},
		PRNum:        77, TrialSHA: "trial-sha",
	}
}

// behindClient makes memberFixedAgainstLiveBase answer "not fixed" (head behind the live base).
func behindClient() *mockGitHubClient {
	return &mockGitHubClient{
		fetchPRDetailsFn: func(owner, repo string, n int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: n, State: "open", HeadSHA: "head-1", MergeableState: "clean"}, nil
		},
		fetchCommitsBehindFn: func(owner, repo, base, head string) (int, error) { return 2, nil },
	}
}

func withLiveBase(eng *Engine, sha string) {
	eng.trainLiveBaseFn = func(trialParams) (string, error) { return sha, nil }
}

func addedLabels(c *mockGitHubClient, issue int) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, l := range c.addLabelCalls {
		if l.issueNumber == issue {
			out = append(out, l.labelName)
		}
	}
	return out
}

func removedLabels(c *mockGitHubClient, issue int) map[string]bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]bool{}
	for _, l := range c.removeLabelCalls {
		if l.issueNumber == issue {
			out[l.labelName] = true
		}
	}
	return out
}

func comments(c *mockGitHubClient) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, a := range c.addCommentCalls {
		out = append(out, a.body)
	}
	return out
}

func repairHasLabel(labels []string, want string) bool {
	for _, l := range labels {
		if l == want {
			return true
		}
	}
	return false
}

func TestEjectRedSingleton_AutoRepair_ReentersValidateWithoutPause(t *testing.T) {
	client := behindClient()
	eng := autoRepairEngine(t, client, 1)
	withLiveBase(eng, "base-live")

	eng.ejectRedSingleton("PVT_1", "owner", "repo", repairMember(), repairParams("base-1"), repairDiag())

	if len(client.updateStatusCalls) != 1 || client.updateStatusCalls[0].optionID != "opt-validate" {
		t.Fatalf("expected one reroute to Validate, got %+v", client.updateStatusCalls)
	}
	removed := removedLabels(client, 1)
	for _, l := range []string{"stage:Validate:complete", "stage:Validate:failed", "fabrik:paused", "fabrik:awaiting-input", "fabrik:awaiting-ci", "fabrik:auto-merge-enabled"} {
		if !removed[l] {
			t.Errorf("expected %s cleared for Validate re-entry", l)
		}
	}
	if added := addedLabels(client, 1); repairHasLabel(added, "fabrik:paused") || repairHasLabel(added, "fabrik:awaiting-input") {
		t.Errorf("auto-repair must not pause, got labels %v", added)
	}
	cs := comments(client)
	if len(cs) != 1 || !strings.HasPrefix(cs[0], "🏭 **Fabrik merge-train — auto-repair started**") {
		t.Fatalf("expected one auto-repair comment with the engine prefix, got %q", cs)
	}
	for _, want := range []string{"attempt 1 of 1", "ci/build"} {
		if !strings.Contains(cs[0], want) {
			t.Errorf("comment missing %q: %s", want, cs[0])
		}
	}
	if got := eng.autoRepairAttemptsAt("owner/repo#1", "base-1"); got != 1 {
		t.Errorf("attempts at base-1 = %d, want 1", got)
	}

	// The Validate dispatch gets the diagnostic as a context file.
	dir := t.TempDir()
	eng.writeMergeTrainRepair(repairMember().item, true, dir)
	data, err := os.ReadFile(filepath.Join(dir, mergeTrainRepairFile))
	if err != nil {
		t.Fatalf("expected %s to be written for the repair dispatch: %v", mergeTrainRepairFile, err)
	}
	for _, want := range []string{"TS2322: type mismatch", "trial-sha", "base-1", "head-1", "Never revert"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("context file missing %q:\n%s", want, data)
		}
	}
}

// Neutralisation: with the feature off the same scenario pauses exactly as ADR-1545 did.
func TestEjectRedSingleton_AutoRepair_DisabledPauses(t *testing.T) {
	client := behindClient()
	prReads := 0
	inner := client.fetchPRDetailsFn
	client.fetchPRDetailsFn = func(owner, repo string, n int) (*gh.PRDetails, error) {
		prReads++
		return inner(owner, repo, n)
	}
	eng := autoRepairEngine(t, client, 0)
	withLiveBase(eng, "base-live")

	eng.ejectRedSingleton("PVT_1", "owner", "repo", repairMember(), repairParams("base-1"), repairDiag())

	added := addedLabels(client, 1)
	if !repairHasLabel(added, "fabrik:paused") || !repairHasLabel(added, "fabrik:awaiting-input") {
		t.Fatalf("cap 0 must pause, got labels %v", added)
	}
	if removedLabels(client, 1)["stage:Validate:complete"] {
		t.Error("cap 0 must not clear Validate completion")
	}
	cs := comments(client)
	if len(cs) != 1 || !strings.Contains(cs[0], "validation failed") || strings.Contains(cs[0], "Auto-repair") {
		t.Fatalf("cap 0 must post today's comment unchanged, got %q", cs)
	}
	client.mu.Lock()
	n := prReads
	client.mu.Unlock()
	if n != 0 {
		t.Errorf("cap 0 must skip R3 entirely, made %d PR reads", n)
	}
	if eng.autoRepairAttemptsAt("owner/repo#1", "base-1") != 0 {
		t.Error("cap 0 must record no attempt")
	}
}

func TestEjectRedSingleton_AutoRepair_SecondFailureAtSameBasePausesWithHistory(t *testing.T) {
	client := behindClient()
	eng := autoRepairEngine(t, client, 1)
	withLiveBase(eng, "base-live")

	eng.ejectRedSingleton("PVT_1", "owner", "repo", repairMember(), repairParams("base-1"), repairDiag())
	eng.ejectRedSingleton("PVT_1", "owner", "repo", repairMember(), repairParams("base-1"), repairDiag())

	added := addedLabels(client, 1)
	if !repairHasLabel(added, "fabrik:paused") {
		t.Fatalf("second failure at the cap must pause, got %v", added)
	}
	cs := comments(client)
	if len(cs) != 2 {
		t.Fatalf("expected the repair comment then the pause comment, got %d", len(cs))
	}
	pause := cs[1]
	for _, want := range []string{"validation failed", "Auto-repair attempts made (1)", "base-1", "head-1", "ci/build", "fabrik:revalidate"} {
		if !strings.Contains(pause, want) {
			t.Errorf("pause comment missing %q: %s", want, pause)
		}
	}
	if got := eng.autoRepairAttemptsAt("owner/repo#1", "base-1"); got != 1 {
		t.Errorf("a paused attempt must not be counted again, got %d", got)
	}
}

func TestEjectRedSingleton_AutoRepair_NewBaseSHAResetsCount(t *testing.T) {
	client := behindClient()
	eng := autoRepairEngine(t, client, 1)
	withLiveBase(eng, "base-live")

	eng.ejectRedSingleton("PVT_1", "owner", "repo", repairMember(), repairParams("base-1"), repairDiag())
	eng.ejectRedSingleton("PVT_1", "owner", "repo", repairMember(), repairParams("base-2"), repairDiag())

	if repairHasLabel(addedLabels(client, 1), "fabrik:paused") {
		t.Fatal("a new base SHA must allow a fresh repair, not a pause")
	}
	if a, b := eng.autoRepairAttemptsAt("owner/repo#1", "base-1"), eng.autoRepairAttemptsAt("owner/repo#1", "base-2"); a != 1 || b != 1 {
		t.Errorf("attempts per base = %d, %d, want 1, 1", a, b)
	}
}

func fixedClient() *mockGitHubClient {
	return &mockGitHubClient{
		fetchPRDetailsFn: func(owner, repo string, n int) (*gh.PRDetails, error) {
			// The head moved since the trial (head-2 != the snapshot head-1).
			return &gh.PRDetails{Number: n, State: "open", HeadSHA: "head-2", MergeableState: "clean"}, nil
		},
		fetchCommitsBehindFn: func(owner, repo, base, head string) (int, error) {
			if base != "base-live" || head != "head-2" {
				return 0, fmt.Errorf("unexpected ancestry query %s..%s", base, head)
			}
			return 0, nil
		},
		fetchCheckRunsFn: greenCompletedCheckRunFn,
	}
}

func TestEjectRedSingleton_AutoRepair_AlreadyFixedStaysQueuedOncePerBase(t *testing.T) {
	client := fixedClient()
	eng := autoRepairEngine(t, client, 1)
	withLiveBase(eng, "base-live")

	eng.ejectRedSingleton("PVT_1", "owner", "repo", repairMember(), repairParams("base-1"), repairDiag())

	if len(client.updateStatusCalls) != 0 || len(comments(client)) != 0 || len(addedLabels(client, 1)) != 0 || len(removedLabels(client, 1)) != 0 {
		t.Fatalf("an already-fixed member must be left completely untouched: status=%+v comments=%q added=%v", client.updateStatusCalls, comments(client), addedLabels(client, 1))
	}
	if eng.autoRepairAttemptsAt("owner/repo#1", "base-1") != 0 {
		t.Error("the R3 requeue must not consume the repair budget")
	}

	// Second red at the same base SHA: the requeue is spent, so it goes to repair...
	eng.ejectRedSingleton("PVT_1", "owner", "repo", repairMember(), repairParams("base-1"), repairDiag())
	if len(client.updateStatusCalls) != 1 || repairHasLabel(addedLabels(client, 1), "fabrik:paused") {
		t.Fatalf("second red at the same base must auto-repair, got status=%+v labels=%v", client.updateStatusCalls, addedLabels(client, 1))
	}
	// ...and a third goes to the pause.
	eng.ejectRedSingleton("PVT_1", "owner", "repo", repairMember(), repairParams("base-1"), repairDiag())
	if !repairHasLabel(addedLabels(client, 1), "fabrik:paused") {
		t.Fatal("third red at the same base must pause")
	}
}

// Neutralisation: cap 0 disables R3 too — the fixed member is rerouted and paused as before.
func TestEjectRedSingleton_AutoRepair_AlreadyFixedIgnoredWhenDisabled(t *testing.T) {
	client := fixedClient()
	eng := autoRepairEngine(t, client, 0)
	withLiveBase(eng, "base-live")

	eng.ejectRedSingleton("PVT_1", "owner", "repo", repairMember(), repairParams("base-1"), repairDiag())

	if !repairHasLabel(addedLabels(client, 1), "fabrik:paused") || len(client.updateStatusCalls) != 1 {
		t.Fatalf("cap 0 must reroute and pause even a fixed member, got status=%+v labels=%v", client.updateStatusCalls, addedLabels(client, 1))
	}
}

func TestMemberFixedAgainstLiveBase_FailsClosed(t *testing.T) {
	base := func(c *mockGitHubClient) { c.fetchCheckRunsFn = greenCompletedCheckRunFn }
	cases := []struct {
		name  string
		setup func(c *mockGitHubClient, eng *Engine)
		want  bool
	}{
		{"fixed", func(c *mockGitHubClient, eng *Engine) {}, true},
		{"head behind the live base", func(c *mockGitHubClient, eng *Engine) {
			c.fetchCommitsBehindFn = func(string, string, string, string) (int, error) { return 3, nil }
		}, false},
		{"ancestry read error", func(c *mockGitHubClient, eng *Engine) {
			c.fetchCommitsBehindFn = func(string, string, string, string) (int, error) { return 0, errors.New("boom") }
		}, false},
		{"PR read error", func(c *mockGitHubClient, eng *Engine) {
			c.fetchPRDetailsFn = func(string, string, int) (*gh.PRDetails, error) { return nil, errors.New("boom") }
		}, false},
		{"live base unresolvable", func(c *mockGitHubClient, eng *Engine) {
			eng.trainLiveBaseFn = func(trialParams) (string, error) { return "", errors.New("no ref") }
		}, false},
		{"CI not green", func(c *mockGitHubClient, eng *Engine) {
			c.fetchCheckRunsFn = func(string, string, string) ([]gh.CheckRun, error) {
				return []gh.CheckRun{{Name: "ci", Status: "completed", Conclusion: "failure"}}, nil
			}
		}, false},
		{"no check runs", func(c *mockGitHubClient, eng *Engine) {
			c.fetchCheckRunsFn = func(string, string, string) ([]gh.CheckRun, error) { return nil, nil }
		}, false},
		{"PR closed", func(c *mockGitHubClient, eng *Engine) {
			c.fetchPRDetailsFn = func(string, string, int) (*gh.PRDetails, error) {
				return &gh.PRDetails{State: "closed", HeadSHA: "head-2", MergeableState: "clean"}, nil
			}
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := fixedClient()
			base(client)
			eng := autoRepairEngine(t, client, 1)
			withLiveBase(eng, "base-live")
			tc.setup(client, eng)
			got, why := eng.memberFixedAgainstLiveBase(repairParams("base-1"), repairMember())
			if got != tc.want {
				t.Errorf("fixed = %v (%s), want %v", got, why, tc.want)
			}
		})
	}
}

func TestEjectRedSingleton_AutoRepair_RerouteFailureLeavesNothing(t *testing.T) {
	client := behindClient()
	client.updateProjectItemStatusFn = func(string, string, string, string) error { return errors.New("boom") }
	eng := autoRepairEngine(t, client, 1)
	withLiveBase(eng, "base-live")

	eng.ejectRedSingleton("PVT_1", "owner", "repo", repairMember(), repairParams("base-1"), repairDiag())

	if len(comments(client)) != 0 || len(addedLabels(client, 1)) != 0 || len(removedLabels(client, 1)) != 0 {
		t.Fatalf("a failed reroute must post, label and clear nothing")
	}
	if eng.autoRepairAttemptsAt("owner/repo#1", "base-1") != 0 {
		t.Error("a failed reroute must not count an attempt")
	}
	if eng.peekPendingRepair("owner/repo#1") != nil {
		t.Error("a failed reroute must leave no pending repair context")
	}
}

func TestEjectRedSingleton_AutoRepair_ClearFailureFallsBackToTriggerLabel(t *testing.T) {
	client := behindClient()
	client.removeLabelFromIssueFn = func(owner, repo string, n int, label string) error {
		if label == "stage:Validate:complete" {
			return errors.New("boom")
		}
		return nil
	}
	eng := autoRepairEngine(t, client, 1)
	withLiveBase(eng, "base-live")

	eng.ejectRedSingleton("PVT_1", "owner", "repo", repairMember(), repairParams("base-1"), repairDiag())

	added := addedLabels(client, 1)
	if !repairHasLabel(added, "fabrik:revalidate") || repairHasLabel(added, "fabrik:paused") {
		t.Fatalf("a failed clear must apply the trigger label (settle scan retries) and not pause, got %v", added)
	}
	if eng.autoRepairAttemptsAt("owner/repo#1", "base-1") != 1 || len(comments(client)) != 1 {
		t.Error("with re-entry secured via the trigger, the attempt is counted and posted once")
	}
}

func TestEjectRedSingleton_AutoRepair_ClearAndTriggerFailurePausesUncounted(t *testing.T) {
	client := behindClient()
	client.removeLabelFromIssueFn = func(owner, repo string, n int, label string) error {
		if label == "stage:Validate:complete" {
			return errors.New("boom")
		}
		return nil
	}
	client.addLabelToIssueFn = func(owner, repo string, n int, label string) error {
		if label == "fabrik:revalidate" {
			return errors.New("boom")
		}
		return nil
	}
	eng := autoRepairEngine(t, client, 1)
	withLiveBase(eng, "base-live")

	eng.ejectRedSingleton("PVT_1", "owner", "repo", repairMember(), repairParams("base-1"), repairDiag())

	if !repairHasLabel(addedLabels(client, 1), "fabrik:paused") {
		t.Fatal("when re-entry cannot be secured the member must be paused, never stranded")
	}
	if eng.autoRepairAttemptsAt("owner/repo#1", "base-1") != 0 || eng.peekPendingRepair("owner/repo#1") != nil {
		t.Error("nothing may be counted or left pending")
	}
	if len(client.updateStatusCalls) != 1 {
		t.Errorf("the member must not be rerouted twice, got %d status moves", len(client.updateStatusCalls))
	}
	cs := comments(client)
	if len(cs) != 1 || !strings.Contains(cs[0], "validation failed") {
		t.Fatalf("expected exactly the pause comment, got %q", cs)
	}
}

// A reroute target not literally named "Validate" keeps the pause (handleRevalidateLabel is
// hardcoded to Validate).
func TestEjectRedSingleton_AutoRepair_NonValidateTargetPauses(t *testing.T) {
	client := behindClient()
	eng := trainTestEngine(t, client, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir())) // target: Implement
	eng.cfg.MaxTrainAutoRepairAttempts = 1
	withLiveBase(eng, "base-live")

	eng.ejectRedSingleton("PVT_1", "owner", "repo", repairMember(), repairParams("base-1"), repairDiag())

	if !repairHasLabel(addedLabels(client, 1), "fabrik:paused") {
		t.Fatal("a non-Validate reroute target must keep the pause")
	}
	if eng.autoRepairAttemptsAt("owner/repo#1", "base-1") != 0 {
		t.Error("no attempt may be recorded")
	}
}

// ejectMember (bisection-isolated poisoners) is untouched by auto-repair: it never reroutes
// or clears Validate gates, and still counts toward MaxMergeTrainEjections.
func TestEjectMember_NotAutoRepairedWhenEnabled(t *testing.T) {
	client := behindClient()
	eng := autoRepairEngine(t, client, 1)
	item := makeTrainItem(1, "Issue 1")

	eng.ejectMember("owner", "repo", item, "isolated by halving bisection", repairDiag(), nil, true)

	if len(client.updateStatusCalls) != 0 || removedLabels(client, 1)["stage:Validate:complete"] {
		t.Error("ejectMember must not reroute or re-enter Validate")
	}
	if eng.autoRepairAttemptsAt("owner/repo#1", "base") != 0 {
		t.Error("ejectMember must not record auto-repair attempts")
	}
	eng.mergeTrainEjectionsMu.Lock()
	n := eng.mergeTrainEjectionCounts["owner/repo#1"]
	eng.mergeTrainEjectionsMu.Unlock()
	if n != 1 {
		t.Errorf("ejection count = %d, want 1", n)
	}
}

func TestWriteMergeTrainRepair_OnlyForRepairDispatch(t *testing.T) {
	client := behindClient()
	eng := autoRepairEngine(t, client, 1)
	item := repairMember().item
	eng.setPendingRepair("owner/repo#1", &repairContext{diag: repairDiag(), baseSHA: "b", memberHeadSHA: "h", attempt: 1, cap: 1, at: eng.now()})
	dir := t.TempDir()
	path := filepath.Join(dir, mergeTrainRepairFile)

	// A different stage never consumes or writes it.
	eng.writeMergeTrainRepair(item, false, dir)
	if _, err := os.Stat(path); err == nil {
		t.Fatal("file must not be written for a non-Validate / comment invocation")
	}
	// The repair dispatch writes it.
	eng.writeMergeTrainRepair(item, true, dir)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file must be written for the repair dispatch: %v", err)
	}
	// A retry of the same Validate (incomplete run, turn-limit slice, tools-denied) still gets
	// it: the pending context is not consumed by the first write.
	os.Remove(path)
	eng.writeMergeTrainRepair(item, true, dir)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("a retried repair dispatch must be given the file again: %v", err)
	}
	// Once the repair flow ends (Validate completed) a later Validate run removes the stale file.
	eng.dropPendingRepair("owner/repo#1")
	eng.writeMergeTrainRepair(item, true, dir)
	if _, err := os.Stat(path); err == nil {
		t.Fatal("stale file must be removed on a later Validate run")
	}
	// A later stage also removes a leftover file.
	os.WriteFile(path, []byte("stale"), 0644)
	eng.writeMergeTrainRepair(item, false, dir)
	if _, err := os.Stat(path); err == nil {
		t.Fatal("stale file must be removed for later stages")
	}
}

// A pending context that outlived its repair flow (item paused or blocked, then a later
// Validate dispatch) must not be written: it carries outdated base/head SHAs and failures.
func TestWriteMergeTrainRepair_DiscardsStaleContext(t *testing.T) {
	item := repairMember().item
	path := func(dir string) string { return filepath.Join(dir, mergeTrainRepairFile) }

	t.Run("expired", func(t *testing.T) {
		eng := autoRepairEngine(t, behindClient(), 1)
		eng.setPendingRepair("owner/repo#1", &repairContext{diag: repairDiag(), baseSHA: "b", memberHeadSHA: "h", attempt: 1, cap: 1, at: eng.now().Add(-repairContextTTL - time.Minute)})
		dir := t.TempDir()
		eng.writeMergeTrainRepair(item, true, dir)
		if _, err := os.Stat(path(dir)); err == nil {
			t.Fatal("an expired repair context must not be written")
		}
		if eng.peekPendingRepair("owner/repo#1") != nil {
			t.Fatal("an expired repair context must be consumed, not left pending")
		}
	})
	t.Run("head moved", func(t *testing.T) {
		eng := autoRepairEngine(t, behindClient(), 1)
		eng.setPendingRepair("owner/repo#1", &repairContext{diag: repairDiag(), baseSHA: "b", memberHeadSHA: "h", attempt: 1, cap: 1, at: eng.now()})
		moved := item
		moved.LinkedPRHeadSHA = "h-new"
		dir := t.TempDir()
		eng.writeMergeTrainRepair(moved, true, dir)
		if _, err := os.Stat(path(dir)); err == nil {
			t.Fatal("a repair context for a superseded head must not be written")
		}
	})
	// The repair run itself rebases/merges and pushes, so after the context was first written a
	// moved head is the repair's own work: a retry must still be given the file.
	t.Run("head moved by the repair itself after delivery", func(t *testing.T) {
		eng := autoRepairEngine(t, behindClient(), 1)
		eng.setPendingRepair("owner/repo#1", &repairContext{diag: repairDiag(), baseSHA: "b", memberHeadSHA: "h", attempt: 1, cap: 1, at: eng.now()})
		first := item
		first.LinkedPRHeadSHA = "h"
		dir := t.TempDir()
		eng.writeMergeTrainRepair(first, true, dir)
		if _, err := os.Stat(path(dir)); err != nil {
			t.Fatalf("first dispatch must be given the file: %v", err)
		}
		retry := item
		retry.LinkedPRHeadSHA = "h-after-repair-push"
		os.Remove(path(dir))
		eng.writeMergeTrainRepair(retry, true, dir)
		if _, err := os.Stat(path(dir)); err != nil {
			t.Fatalf("a retry after the repair's own push must still be given the file: %v", err)
		}
	})
	t.Run("fresh and same head", func(t *testing.T) {
		eng := autoRepairEngine(t, behindClient(), 1)
		eng.setPendingRepair("owner/repo#1", &repairContext{diag: repairDiag(), baseSHA: "b", memberHeadSHA: "h", attempt: 1, cap: 1, at: eng.now()})
		same := item
		same.LinkedPRHeadSHA = "h"
		dir := t.TempDir()
		eng.writeMergeTrainRepair(same, true, dir)
		if _, err := os.Stat(path(dir)); err != nil {
			t.Fatalf("a fresh context for the same head must be written: %v", err)
		}
	})
}

// A missing diagnostic (e.g. after a restart) never blocks the repair: Validate is still
// re-entered, and with no pending context the dispatch simply has no file.
func TestEjectRedSingleton_AutoRepair_MissingDiagnosticStillRepairs(t *testing.T) {
	client := behindClient()
	eng := autoRepairEngine(t, client, 1)
	withLiveBase(eng, "base-live")

	eng.ejectRedSingleton("PVT_1", "owner", "repo", repairMember(), repairParams("base-1"), nil)
	if repairHasLabel(addedLabels(client, 1), "fabrik:paused") || !removedLabels(client, 1)["stage:Validate:complete"] {
		t.Fatal("a nil diagnostic must not block the repair")
	}

	eng.dropPendingRepair("owner/repo#1") // simulates a restart between eject and dispatch
	dir := t.TempDir()
	eng.writeMergeTrainRepair(repairMember().item, true, dir)
	if _, err := os.Stat(filepath.Join(dir, mergeTrainRepairFile)); err == nil {
		t.Error("no pending context means no file")
	}
}

func TestResetEjectionCount_ForgetsAutoRepair(t *testing.T) {
	eng := autoRepairEngine(t, behindClient(), 1)
	eng.recordAutoRepairAttempt("owner/repo#1", repairAttempt{BaseSHA: "b", At: time.Now()})
	eng.takeRequeue("owner/repo#1", "b")
	eng.resetEjectionCount("owner", "repo", 1)
	if eng.autoRepairAttemptsAt("owner/repo#1", "b") != 0 || !eng.takeRequeue("owner/repo#1", "b") {
		t.Error("landing must forget attempts and spent requeues")
	}
}

// Cross-goroutine access (train worker writes, poll goroutine consumes) is race-clean.
func TestAutoRepairState_ConcurrentAccess(t *testing.T) {
	eng := autoRepairEngine(t, behindClient(), 1)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("owner/repo#%d", i%2)
			eng.recordAutoRepairAttempt(key, repairAttempt{BaseSHA: "b", At: time.Now()})
			eng.setPendingRepair(key, &repairContext{baseSHA: "b"})
			eng.takeRequeue(key, "b")
			_ = eng.autoRepairAttemptsAt(key, "b")
			_ = eng.renderRepairAttemptHistory(key)
			eng.peekPendingRepair(key)
		}(i)
	}
	wg.Wait()
}

// Completing Validate ends the repair flow: handleStageComplete drops the pending context, so
// a later, unrelated Validate run is not handed the repair file. Other stages leave it alone.
func TestHandleStageComplete_DropsPendingRepairOnValidate(t *testing.T) {
	client := behindClient()
	eng := autoRepairEngine(t, client, 1)
	item := repairMember().item
	item.Labels = []string{"fabrik:yolo"}
	pending := func() bool { return eng.peekPendingRepair("owner/repo#1") != nil }
	set := func() {
		eng.setPendingRepair("owner/repo#1", &repairContext{diag: repairDiag(), baseSHA: "b", memberHeadSHA: "h", attempt: 1, cap: 1, at: eng.now()})
	}

	set()
	eng.handleStageComplete(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item, &stages.Stage{Name: "Review"})
	if !pending() {
		t.Fatal("a non-Validate stage completing must not drop the pending repair context")
	}
	eng.handleStageComplete(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item, &stages.Stage{Name: "Validate"})
	if pending() {
		t.Fatal("Validate completing must drop the pending repair context")
	}
}
