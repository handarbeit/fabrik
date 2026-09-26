package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

func mergedPRClient() *mockGitHubClient {
	return &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, n int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 77, State: "closed", Merged: true}, nil
		},
	}
}

func postMergeItem() gh.ProjectItem {
	return gh.ProjectItem{Number: 20, ItemID: "PVTI_20", Status: "Validate", Labels: []string{"stage:Validate:complete"}}
}

var lateComment = gh.Comment{ID: "IC_late", DatabaseID: 901, Author: "maintainer", Body: "one more change please"}

// TestItemPRAlreadyLanded covers the positive-evidence-only contract.
func TestItemPRAlreadyLanded(t *testing.T) {
	cases := []struct {
		name       string
		labels     []string
		pr         *gh.PRDetails
		prErr      error
		merged     bool
		mergedErr  error
		wantLanded bool
		wantPR     int
	}{
		{name: "merged", pr: &gh.PRDetails{Number: 5, State: "closed", Merged: true}, wantLanded: true, wantPR: 5},
		{name: "closed, lagging flag, confirmed merged", pr: &gh.PRDetails{Number: 5, State: "closed"}, merged: true, wantLanded: true, wantPR: 5},
		{name: "closed not merged, no label", pr: &gh.PRDetails{Number: 5, State: "closed"}},
		{name: "open", pr: &gh.PRDetails{Number: 5, State: "open"}},
		{name: "no PR"},
		{name: "read error falls through", prErr: errors.New("boom")},
		{name: "merged-state confirm error falls through", pr: &gh.PRDetails{Number: 5, State: "closed"}, mergedErr: errors.New("boom")},
		{name: "train member credited label", labels: []string{"fabrik:credited-pr:88"}, pr: &gh.PRDetails{Number: 5, State: "closed"}, wantLanded: true, wantPR: 88},
		{name: "train member awaiting verification", labels: []string{"fabrik:awaiting-landing-verification"}, pr: &gh.PRDetails{Number: 5, State: "closed"}, wantLanded: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &mockGitHubClient{
				fetchLinkedPRFn: func(owner, repo string, n int) (*gh.PRDetails, error) { return tc.pr, tc.prErr },
				fetchPRMergedFn: func(owner, repo string, n int) (bool, error) { return tc.merged, tc.mergedErr },
			}
			eng := testEngineForMerge(t, client)
			item := postMergeItem()
			item.Labels = append(item.Labels, tc.labels...)
			landed, pr := eng.itemPRAlreadyLanded(item)
			if landed != tc.wantLanded {
				t.Errorf("landed = %v, want %v", landed, tc.wantLanded)
			}
			if tc.wantPR != 0 && pr != tc.wantPR {
				t.Errorf("pr = %d, want %d", pr, tc.wantPR)
			}
		})
	}
}

// TestPostMergeGuard_NoWorkerNoSideEffects: a comment picked up after the merge
// invokes no worker, adds no 👀/editing label/🚀, feeds no breaker, and gets one
// reply on the issue and one on the PR (#1862 AC4).
func TestPostMergeGuard_NoWorkerNoSideEffects(t *testing.T) {
	client := mergedPRClient()
	claude := &mockClaudeInvoker{}
	eng := testEngineWithRepo(t, client, claude)
	item := postMergeItem()

	kind, err := eng.processCommentsClassified(context.Background(), &gh.ProjectBoard{}, item, &stages.Stage{Name: "Validate"}, []gh.Comment{lateComment})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if kind != didNotRunPostMerge {
		t.Errorf("kind = %q, want %q", kind, didNotRunPostMerge)
	}
	if len(claude.calls) != 0 || len(claude.forCommentsCalls) != 0 {
		t.Errorf("worker invoked: %d stage call(s), %d comment call(s)", len(claude.calls), len(claude.forCommentsCalls))
	}
	if len(client.addCommentReactionCalls) != 0 {
		t.Errorf("no reaction (👀 or 🚀) may be added, got %+v", client.addCommentReactionCalls)
	}
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:editing" {
			t.Error("fabrik:editing must not be applied")
		}
	}
	if n := eng.commentBreakerCount(item); n != 0 {
		t.Errorf("comment breaker counter = %d, want 0 (guard must not feed the breaker)", n)
	}
	if len(client.addCommentCalls) != 2 {
		t.Fatalf("want 2 replies (issue + PR), got %d", len(client.addCommentCalls))
	}
	nums := map[int]bool{}
	for _, c := range client.addCommentCalls {
		nums[c.issueNumber] = true
		for _, want := range []string{"🏭 **Fabrik", "already landed", "not** applied", "new issue", postMergeCommentMarker("IC_late")} {
			if !strings.Contains(c.body, want) {
				t.Errorf("reply missing %q: %s", want, c.body)
			}
		}
	}
	if !nums[20] || !nums[77] {
		t.Errorf("replies must target the issue (#20) and the PR (#77), got %v", nums)
	}
}

// TestPostMergeGuard_ReplyDedupedByMarker: the comment stays un-🚀'd, so a second
// pass must not reply again once the marker is visible in item.Comments.
func TestPostMergeGuard_ReplyDedupedByMarker(t *testing.T) {
	client := mergedPRClient()
	eng := testEngineWithRepo(t, client, &mockClaudeInvoker{})
	item := postMergeItem()
	stage := &stages.Stage{Name: "Validate"}

	if _, err := eng.processCommentsClassified(context.Background(), &gh.ProjectBoard{}, item, stage, []gh.Comment{lateComment}); err != nil {
		t.Fatal(err)
	}
	first := len(client.addCommentCalls)
	if first == 0 {
		t.Fatal("first pass must post the reply")
	}

	// Next poll: our own reply is now part of item.Comments.
	item.Comments = []gh.Comment{lateComment, {ID: "IC_reply", Author: "arbeithand", Body: client.addCommentCalls[0].body}}
	if _, err := eng.processCommentsClassified(context.Background(), &gh.ProjectBoard{}, item, stage, []gh.Comment{lateComment}); err != nil {
		t.Fatal(err)
	}
	if got := len(client.addCommentCalls); got != first {
		t.Errorf("second pass posted %d more reply(ies); the marker must dedupe", got-first)
	}
}

// TestPostMergeGuard_BotAndSyntheticGetNoReply: nobody to tell, but still no worker.
func TestPostMergeGuard_BotAndSyntheticGetNoReply(t *testing.T) {
	client := mergedPRClient()
	claude := &mockClaudeInvoker{}
	eng := testEngineWithRepo(t, client, claude)
	batch := []gh.Comment{
		{ID: "IC_bot", DatabaseID: 902, Author: "copilot[bot]", Body: "summary"},
		{ID: "review-body:5", Author: "maintainer", Body: "synthetic, no DatabaseID"},
	}
	kind, err := eng.processCommentsClassified(context.Background(), &gh.ProjectBoard{}, postMergeItem(), &stages.Stage{Name: "Validate"}, batch)
	if err != nil || kind != didNotRunPostMerge {
		t.Fatalf("kind=%q err=%v, want post-merge/nil", kind, err)
	}
	if len(client.addCommentCalls) != 0 {
		t.Errorf("bot/synthetic-only batch must not be replied to, got %d comment(s)", len(client.addCommentCalls))
	}
	if len(claude.calls)+len(claude.forCommentsCalls) != 0 {
		t.Error("worker invoked")
	}
}

// TestPostMergeGuard_OpenPRProcessesNormally: an unmerged PR is untouched by the guard.
func TestPostMergeGuard_OpenPRProcessesNormally(t *testing.T) {
	skipIfNoGit(t)
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, n int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 77, State: "open"}, nil
		},
	}
	claude := &mockClaudeInvoker{}
	eng := testEngineWithRepo(t, client, claude)

	kind, _ := eng.processCommentsClassified(context.Background(), &gh.ProjectBoard{}, postMergeItem(), &stages.Stage{Name: "Validate"}, []gh.Comment{lateComment})
	if kind == didNotRunPostMerge {
		t.Error("guard fired on an open PR")
	}
	for _, c := range client.addCommentCalls {
		if strings.Contains(c.body, "comment not applied") {
			t.Error("'not applied' reply posted for an open PR")
		}
	}
}
