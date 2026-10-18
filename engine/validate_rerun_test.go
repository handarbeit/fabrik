package engine

import (
	"context"
	"errors"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// #2378: a forced Validate re-run is restored only by a Validate stage run; a
// Validate comment-review pass handles its comment but cannot complete the stage.

func rerunComment() []gh.Comment {
	return []gh.Comment{{ID: "C_1", DatabaseID: 1, Author: "human", Body: "please look at X"}}
}

func rerunStage(name string) *stages.Stage {
	return &stages.Stage{Name: name, Order: 1, Completion: stages.CompletionCriteria{Type: "claude"}}
}

// rerunEngine returns an engine whose comment pass reports completion and whose
// live label read returns labels (or an error when readErr is set).
func rerunEngine(t *testing.T, labels []string, readErr error) (*Engine, *mockGitHubClient) {
	t.Helper()
	skipIfNoGit(t)
	client := &mockGitHubClient{
		fetchLabelsFn: func(string, string, int) ([]string, error) {
			if readErr != nil {
				return nil, readErr
			}
			return labels, nil
		},
	}
	claude := &mockClaudeInvoker{
		invokeForCommentsFn: func(*stages.Stage, gh.ProjectItem, []gh.Comment, string, InvokeOptions) (string, bool, TokenUsage, error) {
			return "Acknowledged.\n\nFABRIK_STAGE_COMPLETE\n", true, TokenUsage{}, nil
		},
	}
	return testEngineWithRepo(t, client, claude), client
}

func runRerunComment(t *testing.T, eng *Engine, stageName string) (didNotRunKind, error) {
	t.Helper()
	item := gh.ProjectItem{Number: 2378, Body: "spec"}
	return eng.processCommentsClassified(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item, rerunStage(stageName), rerunComment())
}

func TestValidateRerun_CommentPassDoesNotCompleteValidate(t *testing.T) {
	eng, client := rerunEngine(t, []string{validateRerunLabel}, nil)
	kind, err := runRerunComment(t, eng, "Validate")
	if err != nil || kind != commentRunCompletionSuppressed {
		t.Fatalf("kind=%q err=%v, want %q and nil", kind, err, commentRunCompletionSuppressed)
	}
	added := labelsAdded(client)
	if added["stage:Validate:complete"] || added["fabrik:awaiting-ci"] {
		t.Errorf("labels added = %v: the comment pass restored a completion signal", added)
	}
	// FR-002: the comment is still processed.
	snap, _ := eng.store.Get("owner/repo", 2378)
	if snap.CommentProcessed("C_1").IsZero() {
		t.Error("the pending comment was not marked processed")
	}
	// FR-005: neither breaker counts it.
	if n := snap.NoOpCommentCycles("Validate"); n != 0 {
		t.Errorf("NoOpCommentCycles = %d, want 0", n)
	}
}

func TestValidateRerun_UnreadableLabelsFailClosed(t *testing.T) {
	eng, client := rerunEngine(t, nil, errors.New("boom"))
	kind, err := runRerunComment(t, eng, "Validate")
	if err != nil || kind != commentRunCompletionSuppressed {
		t.Fatalf("kind=%q err=%v, want a suppressed completion on an unreadable label set", kind, err)
	}
	if labelsAdded(client)["stage:Validate:complete"] {
		t.Error("completion label added although the marker could not be ruled out")
	}
}

func TestValidateRerun_WithoutMarkerCompletesAsBefore(t *testing.T) {
	eng, client := rerunEngine(t, []string{"fabrik:yolo"}, nil)
	kind, err := runRerunComment(t, eng, "Validate")
	if err != nil || kind == commentRunCompletionSuppressed {
		t.Fatalf("kind=%q err=%v: a pass without the marker must complete", kind, err)
	}
	if !labelsAdded(client)["stage:Validate:complete"] {
		t.Error("an ordinary Validate comment pass no longer restores the completion label")
	}
}

func TestValidateRerun_OtherStagesUnaffected(t *testing.T) {
	eng, client := rerunEngine(t, []string{validateRerunLabel}, nil)
	kind, err := runRerunComment(t, eng, "Review")
	if err != nil || kind == commentRunCompletionSuppressed {
		t.Fatalf("kind=%q err=%v: the marker only governs Validate", kind, err)
	}
	if !labelsAdded(client)["stage:Review:complete"] {
		t.Error("a Review comment pass was stopped from completing")
	}
}

func TestValidateRerun_SeamRestoresOldBehaviour(t *testing.T) {
	eng, client := rerunEngine(t, []string{validateRerunLabel}, nil)
	eng.SetValidateRerunGuardDisabledForTest(true)
	kind, err := runRerunComment(t, eng, "Validate")
	if err != nil || kind == commentRunCompletionSuppressed {
		t.Fatalf("kind=%q err=%v with the guard disabled", kind, err)
	}
	if !labelsAdded(client)["stage:Validate:complete"] {
		t.Error("the disabled seam still suppressed the completion")
	}
}

func TestValidateRerun_StrippingSetsMarkerBeforeCompletionLabelComesOff(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineWithStages(t, client, testStagesWithValidate())
	markerAdded := false
	client.addLabelToIssueFn = func(_, _ string, _ int, l string) error {
		if l == validateRerunLabel {
			markerAdded = true
		}
		return nil
	}
	client.removeLabelFromIssueFn = func(_, _ string, _ int, l string) error {
		if l == "stage:Validate:complete" && !markerAdded {
			t.Error("stage:Validate:complete removed before the forced re-run marker was added")
		}
		return nil
	}
	item := gh.ProjectItem{Number: 7, Labels: []string{"stage:Validate:complete"}}
	if !eng.handleValidateSHAInvalidation(item, "owner", "repo") {
		t.Fatal("handleValidateSHAInvalidation deferred")
	}
	if !markerAdded {
		t.Error("the marker was never added")
	}

	markerAdded = false
	if !eng.reenterValidate(item, "owner", "repo") {
		t.Fatal("reenterValidate deferred")
	}
	if !markerAdded {
		t.Error("reenterValidate did not add the marker")
	}
}

func TestValidateRerun_MarkerWriteFailureDefersTheStrip(t *testing.T) {
	client := &mockGitHubClient{
		addLabelToIssueFn: func(_, _ string, _ int, l string) error { return errors.New("boom") },
	}
	eng := testEngineWithStages(t, client, testStagesWithValidate())
	item := gh.ProjectItem{Number: 7, Labels: []string{"stage:Validate:complete"}}
	if eng.handleValidateSHAInvalidation(item, "owner", "repo") {
		t.Error("handleValidateSHAInvalidation reported success without the marker")
	}
	if eng.reenterValidate(item, "owner", "repo") {
		t.Error("reenterValidate reported success without the marker")
	}
	if len(client.removeLabelCalls) != 0 {
		t.Errorf("labels removed (%v) although the marker could not be written", client.removeLabelCalls)
	}
}

func TestValidateRerun_StageRunClearsMarker(t *testing.T) {
	for _, waitForCI := range []bool{false, true} {
		name := "direct"
		if waitForCI {
			name = "wait_for_ci"
		}
		t.Run(name, func(t *testing.T) {
			client := &mockGitHubClient{
				fetchLabelsFn: func(string, string, int) ([]string, error) {
					return []string{validateRerunLabel}, nil
				},
			}
			eng := testEngineWithStages(t, client, testStagesWithValidate())
			wf := waitForCI
			stage := &stages.Stage{Name: "Validate", WaitForCI: &wf}
			item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Labels: []string{validateRerunLabel}}
			eng.handleStageComplete(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item, stage)

			cleared := false
			for _, c := range client.removeLabelCalls {
				if c.labelName == validateRerunLabel {
					cleared = true
				}
			}
			if !cleared {
				t.Errorf("the marker was not cleared by a completing Validate stage run (removed: %v)", client.removeLabelCalls)
			}
			signal := labelsAdded(client)["stage:Validate:complete"]
			if waitForCI {
				signal = labelsAdded(client)["fabrik:awaiting-ci"]
			}
			if !signal {
				t.Error("no completion signal was written")
			}
		})
	}
}

func TestValidateRerun_StageFirstRouting(t *testing.T) {
	eng := testEngineWithStages(t, &mockGitHubClient{}, testStagesWithValidate())
	validate := &stages.Stage{Name: "Validate"}
	withComment := []gh.Comment{{Body: "🏭 **Fabrik — stage: Validate**\n*branch: x | commit: unknown | main: y | z*\nbody"}}
	base := gh.ProjectItem{Number: 1, Comments: withComment}

	if !eng.stageRanOnEntry(base, validate, false, false) {
		t.Fatal("precondition: an item with a Validate stage comment has run the stage")
	}
	marked := base
	marked.Labels = []string{validateRerunLabel}
	if eng.stageRanOnEntry(marked, validate, false, false) {
		t.Error("a forced re-run must send a pending comment behind the Validate stage run")
	}
	if !eng.stageRanOnEntry(marked, validate, true, false) || !eng.stageRanOnEntry(marked, validate, false, true) {
		t.Error("an engine-paused or resumed item keeps the comment path")
	}
	review := marked
	review.Comments = []gh.Comment{{Body: "🏭 **Fabrik — stage: Review**\nbody"}}
	if !eng.stageRanOnEntry(review, &stages.Stage{Name: "Review"}, false, false) {
		t.Error("the marker must not affect another stage")
	}
	eng.SetValidateRerunGuardDisabledForTest(true)
	if !eng.stageRanOnEntry(marked, validate, false, false) {
		t.Error("the disabled seam must restore the old routing")
	}
}
