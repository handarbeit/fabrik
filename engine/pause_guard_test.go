package engine

import (
	"context"
	"fmt"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// Pause guard tests (#2341, ADR 2341): a fabrik:paused label seen at an automatic
// pre-merge advance or landing decision stops it, and with the pause removed the
// same call proceeds.

var (
	pausedYolo   = []string{"fabrik:yolo", "fabrik:paused", "fabrik:awaiting-input"}
	unpausedYolo = []string{"fabrik:yolo"}
)

func pauseClient(labels []string) *mockGitHubClient {
	c := landingClient()
	c.fetchLabelsFn = func(owner, repo string, n int) ([]string, error) { return labels, nil }
	return c
}

func assertNoPauseWrites(t *testing.T, client *mockGitHubClient) {
	t.Helper()
	assertNoLanding(t, client)
	if n := len(client.addCommentCalls); n != 0 {
		t.Errorf("a refusal must post no comment, got %d", n)
	}
	if n := len(client.removeLabelCalls); n != 0 {
		t.Errorf("a refusal must remove no label, got %d", n)
	}
}

func TestRefuseIfPaused(t *testing.T) {
	eng := testEngineForMerge(t, &mockGitHubClient{})
	if !eng.refuseIfPaused(gh.ProjectItem{Number: 1, Labels: pausedYolo}, "landing") {
		t.Error("paused item must be refused")
	}
	if eng.refuseIfPaused(gh.ProjectItem{Number: 1, Labels: unpausedYolo}, "landing") {
		t.Error("unpaused item must not be refused")
	}
	if eng.refuseIfPaused(gh.ProjectItem{Number: 1}, "landing") {
		t.Error("nil labels must not be refused")
	}
	eng.SetPauseGuardDisabledForTest(true)
	if eng.refuseIfPaused(gh.ProjectItem{Number: 1, Labels: pausedYolo}, "landing") {
		t.Error("disabled seam must not refuse")
	}
}

func TestPauseGuard_AttemptMergeOnValidate_AutoMerge(t *testing.T) {
	for _, tc := range []struct {
		name   string
		paused bool
	}{{"paused", true}, {"unpaused", false}} {
		t.Run(tc.name, func(t *testing.T) {
			labels := unpausedYolo
			if tc.paused {
				labels = pausedYolo
			}
			client := pauseClient(labels)
			eng := testEngineForMerge(t, client)
			item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Labels: labels}

			enabled, deferred, err := eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{}, item, validateStage())
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.paused {
				if enabled || !deferred {
					t.Errorf("want enabled=false deferred=true, got enabled=%v deferred=%v", enabled, deferred)
				}
				assertNoPauseWrites(t, client)
				return
			}
			if !enabled || deferred {
				t.Errorf("unpaused: want enabled=true deferred=false, got enabled=%v deferred=%v", enabled, deferred)
			}
			if len(client.enablePullRequestAutoMergeCalls) != 1 {
				t.Errorf("unpaused: want 1 EnablePullRequestAutoMerge, got %d", len(client.enablePullRequestAutoMergeCalls))
			}
		})
	}
}

// A pause visible only on the GraphQL re-read (the REST-fresh snapshot lacks it)
// is also refused: the second check, after FetchItemDetails.
func TestPauseGuard_AttemptMergeOnValidate_PauseOnlyOnReread(t *testing.T) {
	client := landingClient()
	client.fetchItemDetailsFn = func(item *gh.ProjectItem) error {
		item.Labels = pausedYolo
		return nil
	}
	eng := testEngineForMerge(t, client)
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Labels: unpausedYolo}

	_, deferred, err := eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{}, item, validateStage())
	if err != nil || !deferred {
		t.Fatalf("want deferred, nil error; got deferred=%v err=%v", deferred, err)
	}
	assertNoPauseWrites(t, client)
}

// The REST-fresh snapshot shows the pause but the GraphQL re-read lags and does not:
// the check ahead of the re-read still refuses.
func TestPauseGuard_AttemptMergeOnValidate_PauseOnlyOnSnapshot(t *testing.T) {
	client := landingClient()
	client.fetchItemDetailsFn = func(item *gh.ProjectItem) error {
		item.Labels = unpausedYolo
		return nil
	}
	eng := testEngineForMerge(t, client)
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Labels: pausedYolo}

	_, deferred, err := eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{}, item, validateStage())
	if err != nil || !deferred {
		t.Fatalf("want deferred, nil error; got deferred=%v err=%v", deferred, err)
	}
	assertNoPauseWrites(t, client)
}

func TestPauseGuard_LandValidated_DirectMergeFallback(t *testing.T) {
	for _, paused := range []bool{true, false} {
		t.Run(fmt.Sprintf("paused=%v", paused), func(t *testing.T) {
			client := landingClient()
			client.enablePullRequestAutoMergeFn = func(owner, repo string, prNumber int, strategy string) error {
				return fmt.Errorf("%w: clean status", gh.ErrAutoMergeAlreadyClean)
			}
			// The pause appears only on the last live read before the merge.
			client.fetchItemDetailsFn = func(item *gh.ProjectItem) error {
				if paused {
					item.Labels = pausedYolo
				}
				return nil
			}
			eng := testEngineForMerge(t, client)
			// Call landValidated directly so the gates ahead of it are not under test.
			item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Labels: unpausedYolo}

			enabled, deferred, err := eng.landValidated(context.Background(), &gh.ProjectBoard{}, item, "owner", "repo")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if paused {
				if enabled || !deferred || len(client.mergePRCalls) != 0 {
					t.Errorf("paused: want refused (deferred, no MergePR); enabled=%v deferred=%v merges=%d", enabled, deferred, len(client.mergePRCalls))
				}
				return
			}
			if !enabled || len(client.mergePRCalls) != 1 {
				t.Errorf("unpaused: want direct merge; enabled=%v merges=%d", enabled, len(client.mergePRCalls))
			}
		})
	}
}

func TestPauseGuard_LandValidated_EntryAndQueued(t *testing.T) {
	for _, paused := range []bool{true, false} {
		t.Run(fmt.Sprintf("paused=%v", paused), func(t *testing.T) {
			labels := unpausedYolo
			if paused {
				labels = pausedYolo
			}
			client := &mockGitHubClient{}
			eng := testEngineWithStages(t, client, testStagesWithValidateAndHolding())
			eng.cfg.MergeTrain = "on"
			item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Repo: "owner/repo", Labels: labels}

			_, deferred, err := eng.landValidated(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item, "owner", "repo")
			if paused {
				if err != nil || !deferred {
					t.Fatalf("paused: want deferred, nil error; got deferred=%v err=%v", deferred, err)
				}
				assertNoPauseWrites(t, client)
				if len(client.updateStatusCalls) != 0 {
					t.Error("a paused item must not be moved to Queued")
				}
				return
			}
			if err != nil || deferred {
				t.Fatalf("unpaused: want advance to Queued; deferred=%v err=%v", deferred, err)
			}
			if len(client.updateStatusCalls) != 1 {
				t.Errorf("unpaused: want 1 status update (Queued), got %d", len(client.updateStatusCalls))
			}
		})
	}
}

func TestPauseGuard_AdvanceToQueued_ReturnsSentinel(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineWithStages(t, client, testStagesWithValidateAndHolding())
	eng.cfg.MergeTrain = "on"
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Repo: "owner/repo", Labels: pausedYolo}

	err := eng.advanceToQueued(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item, "owner", "repo")
	if err == nil || err.Error() != errLandingPaused.Error() {
		t.Fatalf("want errLandingPaused, got %v", err)
	}
	if len(client.updateStatusCalls) != 0 || len(client.addLabelCalls) != 0 {
		t.Error("a refusal must move nothing and add no label")
	}
}

// handleStageComplete at the stage-completion call site: the pause is visible only
// on the live label refresh (the item is a pre-run snapshot).
func TestPauseGuard_HandleStageComplete_Validate(t *testing.T) {
	for _, paused := range []bool{true, false} {
		t.Run(fmt.Sprintf("paused=%v", paused), func(t *testing.T) {
			live := unpausedYolo
			if paused {
				live = pausedYolo
			}
			client := pauseClient(live)
			eng := testEngineWithStages(t, client, testStagesWithValidate())
			item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Labels: unpausedYolo}

			eng.handleStageComplete(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item, validateStage())

			if paused {
				if n := len(client.updateStatusCalls); n != 0 {
					t.Errorf("paused item advanced (%d status update(s))", n)
				}
				if len(client.enablePullRequestAutoMergeCalls) != 0 || len(client.mergePRCalls) != 0 {
					t.Error("paused item was landed")
				}
				// The completion label is still applied; nothing else is.
				for _, c := range client.addLabelCalls {
					if c.labelName != "stage:Validate:complete" {
						t.Errorf("unexpected label %q added while paused", c.labelName)
					}
				}
				if len(client.addCommentCalls) != 0 {
					t.Error("a refusal must post no comment")
				}
				return
			}
			if len(client.enablePullRequestAutoMergeCalls) != 1 {
				t.Errorf("unpaused: want auto-merge enabled once, got %d", len(client.enablePullRequestAutoMergeCalls))
			}
		})
	}
}

func TestPauseGuard_HandleStageComplete_NonValidateAdvance(t *testing.T) {
	for _, paused := range []bool{true, false} {
		t.Run(fmt.Sprintf("paused=%v", paused), func(t *testing.T) {
			live := unpausedYolo
			if paused {
				live = pausedYolo
			}
			client := pauseClient(live)
			eng := testEngineWithStages(t, client, testStagesWithValidate())
			item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Labels: unpausedYolo}

			eng.handleStageComplete(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item, &stages.Stage{Name: "Research", Order: 1})

			if got := len(client.updateStatusCalls); paused && got != 0 {
				t.Errorf("paused item advanced (%d status update(s))", got)
			} else if !paused && got != 1 {
				t.Errorf("unpaused item: want 1 advance, got %d", got)
			}
		})
	}
}

func TestPauseGuard_RunCatchUpPhase2_NonValidateAdvance(t *testing.T) {
	for _, paused := range []bool{true, false} {
		t.Run(fmt.Sprintf("paused=%v", paused), func(t *testing.T) {
			live := unpausedYolo
			if paused {
				live = pausedYolo
			}
			client := pauseClient(live)
			eng := testEngineWithStages(t, client, testStagesWithValidate())
			item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Labels: unpausedYolo}
			advanced := map[string]bool{}

			eng.runCatchUpPhase2(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item, &stages.Stage{Name: "Research", Order: 1}, advanced)

			if paused {
				if len(client.updateStatusCalls) != 0 || len(advanced) != 0 {
					t.Errorf("paused item advanced: status updates=%d advanced=%v", len(client.updateStatusCalls), advanced)
				}
				if len(client.addLabelCalls) != 0 || len(client.addCommentCalls) != 0 {
					t.Error("a refusal must write no label and post no comment")
				}
				return
			}
			if len(client.updateStatusCalls) != 1 {
				t.Errorf("unpaused item: want 1 advance, got %d", len(client.updateStatusCalls))
			}
		})
	}
}

func TestPauseGuard_RunCatchUpPhase2_ValidateLanding(t *testing.T) {
	client := pauseClient(pausedYolo)
	eng := testEngineForMerge(t, client)
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Labels: unpausedYolo}

	eng.runCatchUpPhase2(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item, validateStage(), map[string]bool{})

	assertNoPauseWrites(t, client)
}

// advanceToNextStage itself is deliberately unguarded: it also carries the Done move
// of an already-merged item, which a pause must not strand.
func TestPauseGuard_AdvanceToNextStage_StillMovesPausedItem(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineWithStages(t, client, testStagesWithValidate())
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Labels: pausedYolo}

	if err := eng.advanceToNextStage(&gh.ProjectBoard{ProjectID: "PVT_1"}, item, validateStage()); err != nil {
		t.Fatalf("advanceToNextStage: %v", err)
	}
	if len(client.updateStatusCalls) != 1 {
		t.Errorf("a paused, merged item must still move to Done; got %d status update(s)", len(client.updateStatusCalls))
	}
}

// Non-vacuity: with the guard neutralised, the paused landing happens.
func TestPauseGuard_NonVacuity(t *testing.T) {
	client := pauseClient(pausedYolo)
	eng := testEngineForMerge(t, client)
	eng.SetPauseGuardDisabledForTest(true)
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Labels: pausedYolo}

	enabled, _, err := eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{}, item, validateStage())
	if err != nil || !enabled {
		t.Fatalf("with the guard disabled the landing must proceed; enabled=%v err=%v", enabled, err)
	}
}
