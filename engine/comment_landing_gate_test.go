package engine

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

var humanComment = gh.Comment{ID: "IC_h", DatabaseID: 11, Author: "maintainer", Body: "please also handle X"}

func landingClient() *mockGitHubClient {
	return &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 10, HeadSHA: "sha1"}, nil
		},
	}
}

func validateStage() *stages.Stage { return &stages.Stage{Name: "Validate"} }

// assertNoLanding fails if any of the three landing actions fired.
func assertNoLanding(t *testing.T, client *mockGitHubClient) {
	t.Helper()
	if n := len(client.enablePullRequestAutoMergeCalls); n != 0 {
		t.Errorf("EnablePullRequestAutoMerge called %d time(s) while a comment was pending", n)
	}
	if n := len(client.mergePRCalls); n != 0 {
		t.Errorf("MergePR called %d time(s) while a comment was pending", n)
	}
	if n := len(client.enqueuePullRequestCalls); n != 0 {
		t.Errorf("EnqueuePullRequest called %d time(s) while a comment was pending", n)
	}
	for _, c := range client.updateStatusCalls {
		if c.optionID == "OPT_BatchHold" {
			t.Error("item advanced to the holding stage while a comment was pending")
		}
	}
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:auto-merge-enabled" || c.labelName == "stage:Validate:complete" {
			t.Errorf("label %q applied while a comment was pending", c.labelName)
		}
	}
}

// TestCommentGate_HoldsAutoMerge: an unprocessed human comment blocks enabling
// auto-merge and returns deferred=true (#1862, AC1).
func TestCommentGate_HoldsAutoMerge(t *testing.T) {
	client := landingClient()
	eng := testEngineForMerge(t, client)
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Comments: []gh.Comment{humanComment}}

	enabled, deferred, err := eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{}, item, validateStage())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if enabled || !deferred {
		t.Errorf("want enabled=false deferred=true, got enabled=%v deferred=%v", enabled, deferred)
	}
	assertNoLanding(t, client)
}

// TestCommentGate_HoldsDirectMerge: with auto-merge unavailable (direct-merge
// fallback) the comment still blocks MergePR.
func TestCommentGate_HoldsDirectMerge(t *testing.T) {
	client := landingClient()
	client.enablePullRequestAutoMergeFn = func(owner, repo string, prNumber int, strategy string) error {
		return fmt.Errorf("%w: clean status", gh.ErrAutoMergeAlreadyClean)
	}
	eng := testEngineForMerge(t, client)
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Comments: []gh.Comment{humanComment}}

	_, deferred, err := eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{}, item, validateStage())
	if err != nil || !deferred {
		t.Fatalf("want deferred, nil error; got deferred=%v err=%v", deferred, err)
	}
	assertNoLanding(t, client)
}

// TestCommentGate_DirectMergeFallbackRechecks: a comment that arrives after the
// first read but before MergePR (seen only by the fallback's own re-read) still
// blocks the merge.
func TestCommentGate_DirectMergeFallbackRechecks(t *testing.T) {
	reads := 0
	client := landingClient()
	client.enablePullRequestAutoMergeFn = func(owner, repo string, prNumber int, strategy string) error {
		return fmt.Errorf("%w: clean status", gh.ErrAutoMergeAlreadyClean)
	}
	client.fetchItemDetailsFn = func(item *gh.ProjectItem) error {
		reads++
		if reads >= 2 {
			item.Comments = []gh.Comment{humanComment}
		}
		return nil
	}
	eng := testEngineForMerge(t, client)
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1"}

	_, deferred, err := eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{}, item, validateStage())
	if err != nil || !deferred {
		t.Fatalf("want deferred, nil error; got deferred=%v err=%v", deferred, err)
	}
	if len(client.mergePRCalls) != 0 {
		t.Errorf("MergePR called %d time(s); the fallback re-check must block it", len(client.mergePRCalls))
	}
}

// TestCommentGate_HoldsAdvanceToQueued: with merge_train: on the comment blocks
// the advance to the holding stage (AC2).
func TestCommentGate_HoldsAdvanceToQueued(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineWithStages(t, client, testStagesWithValidateAndHolding())
	eng.cfg.MergeTrain = "on"
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Repo: "owner/repo", Comments: []gh.Comment{humanComment}}

	_, deferred, err := eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item, validateStage())
	if err != nil || !deferred {
		t.Fatalf("want deferred, nil error; got deferred=%v err=%v", deferred, err)
	}
	assertNoLanding(t, client)
}

// TestCommentGate_ReadsLiveComments: the comment gate must see a comment that is
// absent from the (stale) snapshot passed in but present on the live re-read —
// the stage-completion call site's shape.
func TestCommentGate_ReadsLiveComments(t *testing.T) {
	client := landingClient()
	client.fetchItemDetailsFn = func(item *gh.ProjectItem) error {
		item.Comments = []gh.Comment{humanComment}
		return nil
	}
	eng := testEngineForMerge(t, client)
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1"}

	_, deferred, err := eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{}, item, validateStage())
	if err != nil || !deferred {
		t.Fatalf("want deferred, nil error; got deferred=%v err=%v", deferred, err)
	}
	assertNoLanding(t, client)
}

// TestCommentGate_DoesNotBlockNonActionable: engine comments, bot service
// notices, 🚀'd comments and store-watermarked comments never block landing
// (AC3), so a comment nobody will process cannot hold a landing forever.
func TestCommentGate_DoesNotBlockNonActionable(t *testing.T) {
	rocketed := gh.Comment{ID: "IC_r", Author: "maintainer", Body: "done already",
		Reactions: []gh.ReactionGroup{{Content: "ROCKET", Count: 1}}}
	cases := map[string]gh.Comment{
		"engine comment": {ID: "IC_e", Author: "arbeithand", Body: "🏭 **Fabrik — stage: Validate**\nreport"},
		"bot notice":     {ID: "IC_b", Author: "gemini-code-assist[bot]", Body: "You have reached your daily quota limit."},
		"rocketed":       rocketed,
		"watermarked":    {ID: "IC_w", Author: "maintainer", Body: "handled in memory"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			client := landingClient()
			eng := testEngineForMerge(t, client)
			eng.store.Apply(itemstate.CommentProcessed{Repo: "owner/repo", Number: 1, CommentID: "IC_w", At: time.Now()})
			item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Comments: []gh.Comment{c}}

			enabled, deferred, err := eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{}, item, validateStage())
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !enabled || deferred {
				t.Errorf("%s must not block landing; got enabled=%v deferred=%v", name, enabled, deferred)
			}
		})
	}
}

// TestCommentGate_FailedLiveReadHolds: when the live re-read fails the comment
// state is unknown and landing is held (conservative), not merged.
func TestCommentGate_FailedLiveReadHolds(t *testing.T) {
	client := landingClient()
	client.fetchItemDetailsFn = func(item *gh.ProjectItem) error { return errors.New("rate limited") }
	eng := testEngineForMerge(t, client)
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1"}

	enabled, deferred, err := eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{}, item, validateStage())
	if err != nil || enabled || !deferred {
		t.Fatalf("want held (deferred, no error); got enabled=%v deferred=%v err=%v", enabled, deferred, err)
	}
	assertNoLanding(t, client)
}

// TestCommentGate_RecordsReevalCooldown: a hold records the comment-pending
// cooldown so the item is re-admitted on a timer.
func TestCommentGate_RecordsReevalCooldown(t *testing.T) {
	client := landingClient()
	eng := testEngineForMerge(t, client)
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Comments: []gh.Comment{humanComment}}

	if _, _, err := eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{}, item, validateStage()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	snap, err := eng.store.Get("owner/repo", 1)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if snap.CooldownAt("comment-pending").IsZero() {
		t.Error("expected a comment-pending cooldown to be recorded on hold")
	}
}

// TestCommentGate_EarlyReturnsUnchanged: cruise and auto-merge-enabled return
// before the gate exactly as before.
func TestCommentGate_EarlyReturnsUnchanged(t *testing.T) {
	eng := testEngineForMerge(t, landingClient())
	cruise := gh.ProjectItem{Number: 1, Labels: []string{"fabrik:cruise"}, Comments: []gh.Comment{humanComment}}
	if enabled, deferred, err := eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{}, cruise, validateStage()); enabled || deferred || err != nil {
		t.Errorf("cruise: got (%v,%v,%v), want (false,false,nil)", enabled, deferred, err)
	}
	enabledItem := gh.ProjectItem{Number: 2, Labels: []string{"fabrik:auto-merge-enabled"}, Comments: []gh.Comment{humanComment}}
	if enabled, deferred, err := eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{}, enabledItem, validateStage()); !enabled || deferred || err != nil {
		t.Errorf("auto-merge-enabled: got (%v,%v,%v), want (true,false,nil)", enabled, deferred, err)
	}
}

// TestCommentGate_HandleStageCompleteDoesNotAdvance: at the stage-completion
// call site (wait_for_ci: false + yolo) a deferred landing must stop before
// advanceToNextStage — otherwise an unmerged item would advance to Done (#1862).
func TestCommentGate_HandleStageCompleteDoesNotAdvance(t *testing.T) {
	client := landingClient()
	// The comment is only visible on the live re-read, as at this call site the
	// item is a pre-invocation snapshot.
	client.fetchItemDetailsFn = func(item *gh.ProjectItem) error {
		item.Comments = []gh.Comment{humanComment}
		return nil
	}
	eng := testEngineWithStages(t, client, testStagesWithValidate())
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Labels: []string{"fabrik:yolo"}}

	eng.handleStageComplete(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item, validateStage())

	// stage:Validate:complete is deliberately still applied (as for guard 1's
	// deferral) so Phase 2 re-enters the landing decision; what must not happen
	// is an advance or any landing action.
	if n := len(client.updateStatusCalls); n != 0 {
		t.Errorf("item advanced (%d status update(s)) while a comment was pending", n)
	}
	if len(client.enablePullRequestAutoMergeCalls) != 0 || len(client.mergePRCalls) != 0 {
		t.Error("landing action fired while a comment was pending")
	}
}
