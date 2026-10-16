package engine

import (
	"context"
	"errors"
	"fmt"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// Fail-closed pause guard tests (#2341, review of PR #2355): a fabrik:paused label is
// the operator's stop button, so labels that could not be re-read must never count
// as "not paused". The engines below leave the test-only fail-open seam off.

var errLabelRead = errors.New("rate limited")

func unreadableLabelsClient() *mockGitHubClient {
	c := landingClient()
	c.fetchLabelsFn = func(owner, repo string, n int) ([]string, error) { return nil, errLabelRead }
	return c
}

// failClosedEngine is testEngineWithStages with the fail-open seam switched off.
func failClosedEngine(t *testing.T, client *mockGitHubClient) *Engine {
	t.Helper()
	eng := testEngineWithStages(t, client, testStagesWithValidate())
	eng.SetPauseGuardUnreadFailOpenForTest(false)
	return eng
}

// A yolo Validate item whose live label read errors at completion neither merges nor
// advances: the pre-run snapshot may predate a human's pause.
func TestPauseGuard_FailClosed_HandleStageComplete_Validate(t *testing.T) {
	client := unreadableLabelsClient()
	eng := failClosedEngine(t, client)
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Labels: unpausedYolo}

	eng.handleStageComplete(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item, validateStage())

	// The completion label is still applied (and the stale-failure cleanup runs
	// ahead of the guard, as it always has); no landing, no advance, no comment.
	if len(client.enablePullRequestAutoMergeCalls) != 0 || len(client.mergePRCalls) != 0 || len(client.enqueuePullRequestCalls) != 0 {
		t.Error("an item whose labels were unreadable was landed")
	}
	if n := len(client.addCommentCalls); n != 0 {
		t.Errorf("a refusal must post no comment, got %d", n)
	}
	if n := len(client.updateStatusCalls); n != 0 {
		t.Errorf("an item whose labels were unreadable advanced (%d status update(s))", n)
	}
	for _, c := range client.addLabelCalls {
		if c.labelName != "stage:Validate:complete" {
			t.Errorf("unexpected label %q added on a refusal", c.labelName)
		}
	}
}

func TestPauseGuard_FailClosed_HandleStageComplete_NonValidateAdvance(t *testing.T) {
	client := unreadableLabelsClient()
	eng := failClosedEngine(t, client)
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Labels: unpausedYolo}

	eng.handleStageComplete(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item, &stages.Stage{Name: "Research", Order: 1})

	if n := len(client.updateStatusCalls); n != 0 {
		t.Errorf("an item whose labels were unreadable advanced (%d status update(s))", n)
	}
}

func TestPauseGuard_FailClosed_RunCatchUpPhase2(t *testing.T) {
	t.Run("validate landing", func(t *testing.T) {
		client := unreadableLabelsClient()
		eng := failClosedEngine(t, client)
		item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Labels: unpausedYolo}

		eng.runCatchUpPhase2(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item, validateStage(), map[string]bool{})

		assertNoPauseWrites(t, client)
	})
	t.Run("non-validate advance", func(t *testing.T) {
		client := unreadableLabelsClient()
		eng := failClosedEngine(t, client)
		item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Labels: unpausedYolo}
		advanced := map[string]bool{}

		eng.runCatchUpPhase2(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item, &stages.Stage{Name: "Research", Order: 1}, advanced)

		if len(client.updateStatusCalls) != 0 || len(advanced) != 0 {
			t.Errorf("an item whose labels were unreadable advanced: status updates=%d advanced=%v", len(client.updateStatusCalls), advanced)
		}
		if len(client.addLabelCalls) != 0 || len(client.addCommentCalls) != 0 {
			t.Error("a refusal must write no label and post no comment")
		}
	})
}

// With the labels readable and unpaused, the same calls proceed (the control).
func TestPauseGuard_FailClosed_ReadableUnpaused_Proceeds(t *testing.T) {
	client := pauseClient(unpausedYolo)
	eng := failClosedEngine(t, client)
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Labels: unpausedYolo}

	eng.handleStageComplete(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item, validateStage())

	if len(client.enablePullRequestAutoMergeCalls) != 1 {
		t.Errorf("readable, unpaused: want auto-merge enabled once, got %d", len(client.enablePullRequestAutoMergeCalls))
	}
}

// reenableAutoMergeAfterRebase (the rebase reinvoke's after hook) re-enables
// auto-merge, or merges directly, so it checks the live labels and fails closed.
func TestPauseGuard_ReenableAutoMergeAfterRebase(t *testing.T) {
	cases := []struct {
		name      string
		labels    func(owner, repo string, n int) ([]string, error)
		wantLands bool
	}{
		{"paused", func(string, string, int) ([]string, error) {
			return []string{"fabrik:paused", "fabrik:awaiting-input"}, nil
		}, false},
		{"unreadable", func(string, string, int) ([]string, error) { return nil, errLabelRead }, false},
		{"unpaused", func(string, string, int) ([]string, error) { return []string{"fabrik:auto-merge-enabled"}, nil }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &mockGitHubClient{
				fetchLabelsFn: tc.labels,
				fetchLinkedPRFn: func(owner, repo string, n int) (*gh.PRDetails, error) {
					return &gh.PRDetails{Number: 42, HeadSHA: "sha42"}, nil
				},
				enablePullRequestAutoMergeFn: func(owner, repo string, prNumber int, strategy string) error {
					return fmt.Errorf("%w: clean", gh.ErrAutoMergeAlreadyClean)
				},
				mergePRFn: func(owner, repo string, prNumber int) error { return nil },
			}
			eng := failClosedEngine(t, client)
			item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Labels: []string{"fabrik:auto-merge-enabled"}}

			eng.reenableAutoMergeAfterRebase(item)

			if tc.wantLands {
				if len(client.enablePullRequestAutoMergeCalls) != 1 || len(client.mergePRCalls) != 1 {
					t.Errorf("unpaused: want auto-merge re-enabled and the clean fallback merged, got enable=%d merge=%d",
						len(client.enablePullRequestAutoMergeCalls), len(client.mergePRCalls))
				}
				return
			}
			if len(client.enablePullRequestAutoMergeCalls) != 0 || len(client.mergePRCalls) != 0 {
				t.Errorf("must neither re-enable auto-merge nor merge: enable=%d merge=%d",
					len(client.enablePullRequestAutoMergeCalls), len(client.mergePRCalls))
			}
			if len(client.addLabelCalls) != 0 || len(client.removeLabelCalls) != 0 || len(client.addCommentCalls) != 0 {
				t.Error("a refusal must write no label and post no comment")
			}
		})
	}
}

// reEnqueueOrPause is the merge-queue landing for a yolo PR that left the queue.
func TestPauseGuard_ReEnqueueOrPause(t *testing.T) {
	cases := []struct {
		name        string
		labels      func(owner, repo string, n int) ([]string, error)
		wantEnqueue bool
	}{
		{"paused", func(string, string, int) ([]string, error) {
			return []string{"fabrik:paused", "fabrik:awaiting-input"}, nil
		}, false},
		{"unreadable", func(string, string, int) ([]string, error) { return nil, errLabelRead }, false},
		{"unpaused", func(string, string, int) ([]string, error) { return []string{"fabrik:auto-merge-enabled"}, nil }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &mockGitHubClient{
				fetchLabelsFn:   tc.labels,
				fetchPRMergedFn: func(owner, repo string, prNumber int) (bool, error) { return false, nil },
			}
			eng := failClosedEngine(t, client)
			eng.cfg.MaxEnqueueCycles = 5
			item := gh.ProjectItem{Number: 42, Repo: "owner/repo", Labels: []string{"fabrik:auto-merge-enabled"}}
			settle := PRSettleResult{Status: PRMergeReady, PR: &gh.PRDetails{Number: 10, HeadSHA: "abc12345"}}

			eng.reEnqueueOrPause(&gh.ProjectBoard{ProjectID: "PVT_1"}, item, &stages.Stage{Name: "Validate"}, settle)

			if got := len(client.enqueuePullRequestCalls); tc.wantEnqueue && got != 1 {
				t.Errorf("unpaused: want 1 enqueue, got %d", got)
			} else if !tc.wantEnqueue && got != 0 {
				t.Errorf("must not enqueue, got %d", got)
			}
			if !tc.wantEnqueue {
				snap, err := eng.store.Get("owner/repo", 42)
				if err == nil && (snap.EnqueueCycles("Validate") != 0 || snap.LastEnqueuedSHA() != "") {
					t.Error("a refusal must record no enqueue cycle or SHA")
				}
				if len(client.addLabelCalls) != 0 || len(client.addCommentCalls) != 0 {
					t.Error("a refusal must write no label and post no comment")
				}
			}
		})
	}
}

// Non-vacuity: with the guard neutralised, the paused re-enqueue and the paused
// post-rebase merge both happen.
func TestPauseGuard_MergeGate_NonVacuity(t *testing.T) {
	paused := func(string, string, int) ([]string, error) { return []string{"fabrik:paused"}, nil }

	client := &mockGitHubClient{
		fetchLabelsFn:   paused,
		fetchPRMergedFn: func(owner, repo string, prNumber int) (bool, error) { return false, nil },
	}
	eng := failClosedEngine(t, client)
	eng.SetPauseGuardDisabledForTest(true)
	eng.cfg.MaxEnqueueCycles = 5
	item := gh.ProjectItem{Number: 42, Repo: "owner/repo", Labels: []string{"fabrik:auto-merge-enabled"}}
	eng.reEnqueueOrPause(&gh.ProjectBoard{ProjectID: "PVT_1"}, item, &stages.Stage{Name: "Validate"},
		PRSettleResult{Status: PRMergeReady, PR: &gh.PRDetails{Number: 10, HeadSHA: "abc12345"}})
	if len(client.enqueuePullRequestCalls) != 1 {
		t.Errorf("with the guard disabled the paused PR is re-enqueued; got %d", len(client.enqueuePullRequestCalls))
	}

	client2 := &mockGitHubClient{
		fetchLabelsFn:                paused,
		fetchLinkedPRFn:              func(owner, repo string, n int) (*gh.PRDetails, error) { return &gh.PRDetails{Number: 42, HeadSHA: "abc12345"}, nil },
		enablePullRequestAutoMergeFn: func(owner, repo string, prNumber int, strategy string) error { return nil },
	}
	eng2 := failClosedEngine(t, client2)
	eng2.SetPauseGuardDisabledForTest(true)
	eng2.reenableAutoMergeAfterRebase(gh.ProjectItem{Number: 1, Labels: []string{"fabrik:auto-merge-enabled"}})
	if len(client2.enablePullRequestAutoMergeCalls) != 1 {
		t.Errorf("with the guard disabled the paused PR gets auto-merge re-enabled; got %d", len(client2.enablePullRequestAutoMergeCalls))
	}
}

// Non-vacuity for the fail-closed rule itself: the fail-open seam restores the old
// "stale snapshot decides" behaviour and the unreadable landing goes ahead.
func TestPauseGuard_FailClosed_NonVacuity(t *testing.T) {
	client := unreadableLabelsClient()
	eng := failClosedEngine(t, client)
	eng.SetPauseGuardUnreadFailOpenForTest(true)
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Labels: unpausedYolo}

	eng.handleStageComplete(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item, validateStage())

	if len(client.enablePullRequestAutoMergeCalls) != 1 {
		t.Errorf("with fail-closed neutralised the unreadable item lands; got %d auto-merge call(s)", len(client.enablePullRequestAutoMergeCalls))
	}
}
