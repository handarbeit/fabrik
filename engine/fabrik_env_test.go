package engine

import (
	"errors"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// TestResolveFabrikEnvOpts covers #1288's FABRIK_ROOT/FABRIK_PR resolution:
// the board value is trusted when non-zero, the REST fallback only fires for
// stages where a PR could plausibly exist, a failed or non-open/merged result
// is treated as "no PR" without failing, and FabrikRoot always reflects
// e.fabrikDir regardless of PR resolution.
//
// "Could plausibly exist" is resume-aware for a stage that creates its own
// draft PR (CreateDraftPR): on that stage's very first attempt (resume=false)
// no PR exists yet — Implement doesn't create it until after Claude
// completes — so the fallback must not fire there at all, or every first
// Implement invocation would burn a REST call that can never succeed.
//
// On a base:<branch> repo, closingIssuesReferences is structurally empty for
// every PR (not just the linked one), so FetchLinkedPR's branch-name match
// alone could trust an unrelated PR on a stale/repurposed fabrik/issue-N
// branch. Mirroring handleBrokenReviewLinkage, resolveFabrikEnvOpts confirms
// via FetchPRClosingIssues before trusting that match — these cases cover
// the confirmed, unconfirmed, and transient-error-fails-open paths.
func TestResolveFabrikEnvOpts(t *testing.T) {
	createDraftPRStage := &stages.Stage{Name: "Implement", CreateDraftPR: true, PostToPR: true}
	postToPRStage := &stages.Stage{Name: "Review", PostToPR: true}
	earlyStage := &stages.Stage{Name: "Research"}

	tests := []struct {
		name                 string
		item                 gh.ProjectItem
		stage                *stages.Stage
		resume               bool
		fetchLinkedPR        func(owner, repo string, issueNumber int) (*gh.PRDetails, error)
		fetchPRClosingIssues func(owner, repo string, prNumber int) ([]int, error)
		wantPRNumber         int
		wantFetchCalls       int
		wantClosingCalls     int
	}{
		{
			name:           "board value trusted, no fallback call",
			item:           gh.ProjectItem{Number: 1, Repo: "owner/repo", LinkedPRNumber: 42},
			stage:          createDraftPRStage,
			resume:         true,
			fetchLinkedPR:  func(string, string, int) (*gh.PRDetails, error) { return nil, errors.New("should not be called") },
			wantPRNumber:   42,
			wantFetchCalls: 0,
		},
		{
			name:  "early stage never calls fallback, PR unset",
			item:  gh.ProjectItem{Number: 2, Repo: "owner/repo", LinkedPRNumber: 0},
			stage: earlyStage,
			fetchLinkedPR: func(string, string, int) (*gh.PRDetails, error) {
				return nil, errors.New("should not be called")
			},
			wantPRNumber:   0,
			wantFetchCalls: 0,
		},
		{
			name:   "CreateDraftPR stage's first attempt never calls fallback, PR unset",
			item:   gh.ProjectItem{Number: 3, Repo: "owner/repo", LinkedPRNumber: 0},
			stage:  createDraftPRStage,
			resume: false,
			fetchLinkedPR: func(string, string, int) (*gh.PRDetails, error) {
				return nil, errors.New("should not be called")
			},
			wantPRNumber:   0,
			wantFetchCalls: 0,
		},
		{
			name:   "CreateDraftPR stage's resumed attempt does call fallback",
			item:   gh.ProjectItem{Number: 4, Repo: "owner/repo", LinkedPRNumber: 0},
			stage:  createDraftPRStage,
			resume: true,
			fetchLinkedPR: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
				return &gh.PRDetails{Number: 88, State: "open"}, nil
			},
			wantPRNumber:   88,
			wantFetchCalls: 1,
		},
		{
			name:   "PostToPR-only stage calls fallback on first attempt (PR already created by an earlier stage)",
			item:   gh.ProjectItem{Number: 5, Repo: "owner/repo", LinkedPRNumber: 0},
			stage:  postToPRStage,
			resume: false,
			fetchLinkedPR: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
				return &gh.PRDetails{Number: 55, State: "open"}, nil
			},
			wantPRNumber:   55,
			wantFetchCalls: 1,
		},
		{
			name:   "base:<branch> repo — board 0, closing keyword confirms linkage, PR trusted",
			item:   gh.ProjectItem{Number: 6, Repo: "owner/repo", LinkedPRNumber: 0, Labels: []string{"base:develop"}},
			stage:  createDraftPRStage,
			resume: true,
			fetchLinkedPR: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
				return &gh.PRDetails{Number: 55, State: "open"}, nil
			},
			fetchPRClosingIssues: func(owner, repo string, prNumber int) ([]int, error) {
				return []int{6}, nil
			},
			wantPRNumber:     55,
			wantFetchCalls:   1,
			wantClosingCalls: 1,
		},
		{
			name:   "base:<branch> repo — stale branch's unrelated PR lacks closing keyword, FABRIK_PR unset",
			item:   gh.ProjectItem{Number: 10, Repo: "owner/repo", LinkedPRNumber: 0, Labels: []string{"base:develop"}},
			stage:  createDraftPRStage,
			resume: true,
			fetchLinkedPR: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
				return &gh.PRDetails{Number: 99, State: "open"}, nil
			},
			fetchPRClosingIssues: func(owner, repo string, prNumber int) ([]int, error) {
				return []int{123}, nil // closes a different issue, not #10
			},
			wantPRNumber:     0,
			wantFetchCalls:   1,
			wantClosingCalls: 1,
		},
		{
			name:   "base:<branch> repo — FetchPRClosingIssues transient error fails open, PR trusted",
			item:   gh.ProjectItem{Number: 11, Repo: "owner/repo", LinkedPRNumber: 0, Labels: []string{"base:develop"}},
			stage:  createDraftPRStage,
			resume: true,
			fetchLinkedPR: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
				return &gh.PRDetails{Number: 55, State: "open"}, nil
			},
			fetchPRClosingIssues: func(owner, repo string, prNumber int) ([]int, error) {
				return nil, errors.New("transient network error")
			},
			wantPRNumber:     55,
			wantFetchCalls:   1,
			wantClosingCalls: 1,
		},
		{
			name:   "non-base repo — closing-keyword confirmation is skipped entirely",
			item:   gh.ProjectItem{Number: 12, Repo: "owner/repo", LinkedPRNumber: 0},
			stage:  createDraftPRStage,
			resume: true,
			fetchLinkedPR: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
				return &gh.PRDetails{Number: 55, State: "open"}, nil
			},
			fetchPRClosingIssues: func(owner, repo string, prNumber int) ([]int, error) {
				return nil, errors.New("should not be called")
			},
			wantPRNumber:     55,
			wantFetchCalls:   1,
			wantClosingCalls: 0,
		},
		{
			name:   "failed lookup leaves PR unset, non-fatal",
			item:   gh.ProjectItem{Number: 7, Repo: "owner/repo", LinkedPRNumber: 0},
			stage:  createDraftPRStage,
			resume: true,
			fetchLinkedPR: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
				return nil, errors.New("network error")
			},
			wantPRNumber:   0,
			wantFetchCalls: 1,
		},
		{
			name:   "closed PR treated as no PR",
			item:   gh.ProjectItem{Number: 8, Repo: "owner/repo", LinkedPRNumber: 0},
			stage:  createDraftPRStage,
			resume: true,
			fetchLinkedPR: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
				return &gh.PRDetails{Number: 66, State: "closed"}, nil
			},
			wantPRNumber:   0,
			wantFetchCalls: 1,
		},
		{
			name:   "merged PR treated as no PR",
			item:   gh.ProjectItem{Number: 9, Repo: "owner/repo", LinkedPRNumber: 0},
			stage:  createDraftPRStage,
			resume: true,
			fetchLinkedPR: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
				return &gh.PRDetails{Number: 77, State: "open", Merged: true}, nil
			},
			wantPRNumber:   0,
			wantFetchCalls: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			closingCalls := 0
			client := &mockGitHubClient{
				fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
					calls++
					return tt.fetchLinkedPR(owner, repo, issueNumber)
				},
				fetchPRClosingIssuesFn: func(owner, repo string, prNumber int) ([]int, error) {
					closingCalls++
					return tt.fetchPRClosingIssues(owner, repo, prNumber)
				},
			}
			eng := testEngine(t, client, &mockClaudeInvoker{})
			eng.fabrikDir = "/fabrik/root"

			fabrikRoot, prNumber := eng.resolveFabrikEnvOpts(tt.item, tt.stage, tt.resume)

			if fabrikRoot != "/fabrik/root" {
				t.Errorf("fabrikRoot = %q, want /fabrik/root", fabrikRoot)
			}
			if prNumber != tt.wantPRNumber {
				t.Errorf("prNumber = %d, want %d", prNumber, tt.wantPRNumber)
			}
			if calls != tt.wantFetchCalls {
				t.Errorf("FetchLinkedPR called %d times, want %d", calls, tt.wantFetchCalls)
			}
			if closingCalls != tt.wantClosingCalls {
				t.Errorf("FetchPRClosingIssues called %d times, want %d", closingCalls, tt.wantClosingCalls)
			}
		})
	}
}
