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
func TestResolveFabrikEnvOpts(t *testing.T) {
	prStage := &stages.Stage{Name: "Implement", CreateDraftPR: true, PostToPR: true}
	earlyStage := &stages.Stage{Name: "Research"}

	tests := []struct {
		name           string
		item           gh.ProjectItem
		stage          *stages.Stage
		fetchLinkedPR  func(owner, repo string, issueNumber int) (*gh.PRDetails, error)
		wantPRNumber   int
		wantFetchCalls int
	}{
		{
			name:           "board value trusted, no fallback call",
			item:           gh.ProjectItem{Number: 1, Repo: "owner/repo", LinkedPRNumber: 42},
			stage:          prStage,
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
			name:  "base:<branch> repo — board 0, fallback resolves open PR",
			item:  gh.ProjectItem{Number: 3, Repo: "owner/repo", LinkedPRNumber: 0},
			stage: prStage,
			fetchLinkedPR: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
				return &gh.PRDetails{Number: 55, State: "open"}, nil
			},
			wantPRNumber:   55,
			wantFetchCalls: 1,
		},
		{
			name:  "failed lookup leaves PR unset, non-fatal",
			item:  gh.ProjectItem{Number: 4, Repo: "owner/repo", LinkedPRNumber: 0},
			stage: prStage,
			fetchLinkedPR: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
				return nil, errors.New("network error")
			},
			wantPRNumber:   0,
			wantFetchCalls: 1,
		},
		{
			name:  "closed PR treated as no PR",
			item:  gh.ProjectItem{Number: 5, Repo: "owner/repo", LinkedPRNumber: 0},
			stage: prStage,
			fetchLinkedPR: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
				return &gh.PRDetails{Number: 66, State: "closed"}, nil
			},
			wantPRNumber:   0,
			wantFetchCalls: 1,
		},
		{
			name:  "merged PR treated as no PR",
			item:  gh.ProjectItem{Number: 6, Repo: "owner/repo", LinkedPRNumber: 0},
			stage: prStage,
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
			client := &mockGitHubClient{
				fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
					calls++
					return tt.fetchLinkedPR(owner, repo, issueNumber)
				},
			}
			eng := testEngine(t, client, &mockClaudeInvoker{})
			eng.fabrikDir = "/fabrik/root"

			fabrikRoot, prNumber := eng.resolveFabrikEnvOpts(tt.item, tt.stage)

			if fabrikRoot != "/fabrik/root" {
				t.Errorf("fabrikRoot = %q, want /fabrik/root", fabrikRoot)
			}
			if prNumber != tt.wantPRNumber {
				t.Errorf("prNumber = %d, want %d", prNumber, tt.wantPRNumber)
			}
			if calls != tt.wantFetchCalls {
				t.Errorf("FetchLinkedPR called %d times, want %d", calls, tt.wantFetchCalls)
			}
		})
	}
}
