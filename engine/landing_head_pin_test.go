package engine

import (
	"context"
	"errors"
	"fmt"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// The tests in this file pin #2342: every ordinary landing write names the head
// the gates judged, and a head that moved is a silent deferral.

// landingFixture returns a mock whose live item read reports head `judged`
// while the cache-backed linked-PR read reports a (possibly different) head.
func landingFixture(judged, cachedPR string, queue bool) *mockGitHubClient {
	return &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 42, HeadSHA: cachedPR, IsMergeQueueEnabled: queue}, nil
		},
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			item.LinkedPRHeadSHA = judged
			return nil
		},
	}
}

func assertSilentDeferral(t *testing.T, eng *Engine, client *mockGitHubClient, enabled, deferred bool, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("head-moved must not be an error, got %v", err)
	}
	if enabled || !deferred {
		t.Fatalf("(enabled=%v, deferred=%v), want (false, true)", enabled, deferred)
	}
	if len(client.addLabelCalls) != 0 || len(client.addCommentCalls) != 0 {
		t.Errorf("deferral must add no label or comment; labels=%v comments=%d", client.addLabelCalls, len(client.addCommentCalls))
	}
	if len(client.removeLabelCalls) != 0 {
		t.Errorf("deferral must remove no label; got %v", client.removeLabelCalls)
	}
	snap, ok := eng.store.Peek("owner/repo", 1)
	if !ok || snap.CooldownAt(mergeUnsettledCooldownReason).IsZero() {
		t.Errorf("deferral must record a %s admission cooldown so the next poll re-evaluates", mergeUnsettledCooldownReason)
	}
}

func landItem() gh.ProjectItem { return gh.ProjectItem{Number: 1, ItemID: "PVTI_1"} }

var pinValidateStage = &stages.Stage{Name: "Validate"}

func TestLanding_AutoMergePinnedToJudgedHead_NotTheCachedPRHead(t *testing.T) {
	client := landingFixture("judgedA", "staleCachedZ", false)
	eng := testEngineForMerge(t, client)

	enabled, deferred, err := eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{}, landItem(), pinValidateStage)
	if err != nil || !enabled || deferred {
		t.Fatalf("(enabled=%v, deferred=%v, err=%v), want (true, false, nil)", enabled, deferred, err)
	}
	if len(client.enablePullRequestAutoMergeCalls) != 1 {
		t.Fatalf("EnablePullRequestAutoMerge calls = %d, want 1", len(client.enablePullRequestAutoMergeCalls))
	}
	if got := client.enablePullRequestAutoMergeCalls[0].headOID; got != "judgedA" {
		t.Errorf("auto-merge pinned to %q, want the judged head %q (never the cached PR head)", got, "judgedA")
	}
}

func TestLanding_AutoMergeHeadMoved_DefersSilently(t *testing.T) {
	client := landingFixture("A", "A", false)
	client.enablePullRequestAutoMergeFn = func(owner, repo string, prNumber int, strategy string) error {
		return fmt.Errorf("enabling auto-merge: %w", gh.ErrHeadMoved)
	}
	eng := testEngineForMerge(t, client)

	enabled, deferred, err := eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{}, landItem(), pinValidateStage)
	assertSilentDeferral(t, eng, client, enabled, deferred, err)
	if n := len(client.mergePRAtHeadSHACalls) + len(client.mergePRCalls); n != 0 {
		t.Errorf("a moved head on enable must not fall back to a direct merge, got %d merge call(s)", n)
	}
}

func TestLanding_AlreadyCleanFallbackMerge_PinnedAndHeadMovedDefers(t *testing.T) {
	client := landingFixture("A", "A", false)
	client.enablePullRequestAutoMergeFn = func(owner, repo string, prNumber int, strategy string) error {
		return fmt.Errorf("%w: Pull request is in clean status", gh.ErrAutoMergeAlreadyClean)
	}
	client.mergePRAtHeadSHAFn = func(owner, repo string, prNumber int, sha string) error {
		return fmt.Errorf("merging PR at expected head %s: %w", sha, gh.ErrHeadMoved)
	}
	eng := testEngineForMerge(t, client)

	enabled, deferred, err := eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{}, landItem(), pinValidateStage)
	assertSilentDeferral(t, eng, client, enabled, deferred, err)
	if len(client.mergePRAtHeadSHACalls) != 1 || client.mergePRAtHeadSHACalls[0].expectedHeadSHA != "A" {
		t.Fatalf("direct-merge fallback must be pinned to the judged head A, got %+v", client.mergePRAtHeadSHACalls)
	}
}

func TestLanding_FallbackLiveReReadSeesNewHead_DefersWithoutMerging(t *testing.T) {
	client := landingFixture("A", "A", false)
	calls := 0
	client.fetchItemDetailsFn = func(item *gh.ProjectItem) error {
		calls++
		item.LinkedPRHeadSHA = "A"
		if calls > 1 { // the fallback's own re-read
			item.LinkedPRHeadSHA = "B"
		}
		return nil
	}
	client.enablePullRequestAutoMergeFn = func(owner, repo string, prNumber int, strategy string) error {
		return fmt.Errorf("%w: clean", gh.ErrAutoMergeAlreadyClean)
	}
	eng := testEngineForMerge(t, client)

	enabled, deferred, err := eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{}, landItem(), pinValidateStage)
	assertSilentDeferral(t, eng, client, enabled, deferred, err)
	if n := len(client.mergePRAtHeadSHACalls); n != 0 {
		t.Errorf("a head seen to move must not be merged, got %d merge call(s)", n)
	}
}

func TestLanding_EnqueuePinnedAndHeadMovedDefers(t *testing.T) {
	client := landingFixture("A", "staleZ", true)
	client.enqueuePullRequestFn = func(owner, repo string, prNumber int, sha string) error {
		return fmt.Errorf("enqueueing: %w", gh.ErrHeadMoved)
	}
	eng := testEngineForMerge(t, client)

	enabled, deferred, err := eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{}, landItem(), pinValidateStage)
	assertSilentDeferral(t, eng, client, enabled, deferred, err)
	if len(client.enqueuePullRequestCalls) != 1 || client.enqueuePullRequestCalls[0].expectedHeadOID != "A" {
		t.Fatalf("enqueue must be pinned to the judged head A (not the cached Z), got %+v", client.enqueuePullRequestCalls)
	}
}

func TestLanding_NonHeadRefusalsKeepTheirHandling(t *testing.T) {
	client := landingFixture("A", "A", false)
	client.enablePullRequestAutoMergeFn = func(owner, repo string, prNumber int, strategy string) error {
		return fmt.Errorf("%w: clean", gh.ErrAutoMergeAlreadyClean)
	}
	client.mergePRAtHeadSHAFn = func(owner, repo string, prNumber int, sha string) error {
		return fmt.Errorf("merge: %w", gh.ErrNotMergeableCI)
	}
	eng := testEngineForMerge(t, client)

	_, deferred, err := eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{}, landItem(), pinValidateStage)
	if err == nil || !errors.Is(err, gh.ErrNotMergeableCI) || deferred {
		t.Fatalf("a non-head refusal must surface unchanged; err=%v deferred=%v", err, deferred)
	}
}

func TestLanding_NoJudgedHead_NeverIssuesAnUnpinnedWrite(t *testing.T) {
	client := landingFixture("", "cachedZ", false)
	eng := testEngineForMerge(t, client)
	// The mock fills an empty item head from the linked PR; script the opposite.
	client.fetchItemDetailsFn = func(item *gh.ProjectItem) error { item.LinkedPRHeadSHA = ""; return nil }
	client.fetchLinkedPRFn = func(owner, repo string, n int) (*gh.PRDetails, error) {
		return &gh.PRDetails{Number: 42, HeadSHA: "cachedZ"}, nil
	}
	client.noHeadBackfill = true

	enabled, deferred, err := eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{}, landItem(), pinValidateStage)
	if err != nil || enabled || !deferred {
		t.Fatalf("(enabled=%v, deferred=%v, err=%v), want (false, true, nil)", enabled, deferred, err)
	}
	if n := len(client.enablePullRequestAutoMergeCalls) + len(client.mergePRAtHeadSHACalls) + len(client.enqueuePullRequestCalls); n != 0 {
		t.Errorf("no write may be issued without a judged head, got %d", n)
	}
}

func TestLanding_CIJudgedHeadMismatch_Defers_Match_Lands(t *testing.T) {
	for _, tc := range []struct {
		name, ciHead string
		wantLand     bool
	}{{"ci judged a superseded head", "old", false}, {"ci judged the same head", "A", true}} {
		t.Run(tc.name, func(t *testing.T) {
			client := landingFixture("A", "A", false)
			eng := testEngineForMerge(t, client)
			eng.noteCIJudgedHead(landItem(), tc.ciHead)

			enabled, deferred, err := eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{}, landItem(), pinValidateStage)
			if tc.wantLand {
				if err != nil || !enabled || deferred {
					t.Fatalf("(enabled=%v, deferred=%v, err=%v), want a landing", enabled, deferred, err)
				}
				return
			}
			assertSilentDeferral(t, eng, client, enabled, deferred, err)
			if len(client.enablePullRequestAutoMergeCalls) != 0 {
				t.Error("no auto-merge may be enabled on a head the CI gate did not judge")
			}
		})
	}
}

func TestLanding_UnmovedHead_WriteSequenceUnchanged(t *testing.T) {
	client := landingFixture("A", "A", false)
	eng := testEngineForMerge(t, client)

	enabled, deferred, err := eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{}, landItem(), pinValidateStage)
	if err != nil || !enabled || deferred {
		t.Fatalf("(enabled=%v, deferred=%v, err=%v), want a landing", enabled, deferred, err)
	}
	if len(client.addLabelCalls) != 1 || client.addLabelCalls[0].labelName != "fabrik:auto-merge-enabled" {
		t.Errorf("labels = %+v, want only fabrik:auto-merge-enabled", client.addLabelCalls)
	}
	if len(client.addCommentCalls) != 0 {
		t.Errorf("an unmoved landing posts no comment, got %d", len(client.addCommentCalls))
	}
}
