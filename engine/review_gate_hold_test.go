package engine

import (
	"errors"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// reviewGateHold is the read-only core of reviewGateBlocksLanding (#2126): it must
// reach the same verdict and never write a label, while the wrapper keeps applying
// fabrik:awaiting-review exactly as before.
func TestReviewGateHold_VerdictsApplyNoLabel(t *testing.T) {
	stage := &stages.Stage{Name: "Review", WaitForReviews: boolPtr(true)}
	authoritative := &stages.Stage{Name: "Review", WaitForReviews: boolPtr(true), ReviewAuthority: "authoritative"}
	item := gh.ProjectItem{Number: 10, Repo: "owner/repo", LinkedPRNumber: 77}

	tests := []struct {
		name        string
		stage       *stages.Stage
		client      *mockGitHubClient
		wantBlocked bool
		wantMsg     string
	}{
		{
			name:   "not opted in",
			stage:  &stages.Stage{Name: "Review"},
			client: &mockGitHubClient{},
		},
		{
			name:  "outstanding reviewer",
			stage: stage,
			client: &mockGitHubClient{
				fetchPRReviewRequestsFn: func(string, string, int) ([]gh.ReviewRequest, error) {
					return []gh.ReviewRequest{{Login: "alice"}}, nil
				},
			},
			wantBlocked: true,
			wantMsg:     "waiting for reviewers: alice",
		},
		{
			name:  "review fetch failure blocks conservatively",
			stage: stage,
			client: &mockGitHubClient{
				fetchPRReviewsFn: func(string, string, int) ([]gh.PRReview, error) { return nil, errors.New("boom") },
			},
			wantBlocked: true,
			wantMsg:     "review state unreadable",
		},
		{
			name:        "nothing requested or reviewed yet",
			stage:       stage,
			client:      &mockGitHubClient{},
			wantBlocked: true,
			wantMsg:     "waiting for initial review submission",
		},
		{
			name:  "reviewed and nothing outstanding clears",
			stage: stage,
			client: &mockGitHubClient{
				fetchPRReviewsFn: func(string, string, int) ([]gh.PRReview, error) {
					return []gh.PRReview{{Author: "alice", State: "APPROVED"}}, nil
				},
			},
		},
		{
			name:  "authoritative verdict blocks",
			stage: authoritative,
			client: &mockGitHubClient{
				fetchPRReviewsFn: func(string, string, int) ([]gh.PRReview, error) {
					return []gh.PRReview{{Author: "alice", State: "CHANGES_REQUESTED"}}, nil
				},
				fetchPRReviewDecisionFn: func(string, string, int) (string, error) { return "CHANGES_REQUESTED", nil },
			},
			wantBlocked: true,
			wantMsg:     "authoritative gate still blocking",
		},
		{
			name:  "authoritative verdict unreadable blocks",
			stage: authoritative,
			client: &mockGitHubClient{
				fetchPRReviewsFn: func(string, string, int) ([]gh.PRReview, error) {
					return []gh.PRReview{{Author: "alice", State: "APPROVED"}}, nil
				},
				fetchPRReviewDecisionFn: func(string, string, int) (string, error) { return "", errors.New("boom") },
			},
			wantBlocked: true,
			wantMsg:     "review verdict unreadable",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			eng := reviewTestEngine(t, tc.client)
			h := eng.reviewGateHold(item, tc.stage, "owner", "repo")
			if h.Blocked != tc.wantBlocked {
				t.Fatalf("Blocked = %v, want %v (msg %q)", h.Blocked, tc.wantBlocked, h.Msg)
			}
			if tc.wantBlocked && !strings.Contains(h.Msg, tc.wantMsg) {
				t.Errorf("Msg = %q, want it to contain %q", h.Msg, tc.wantMsg)
			}
			if len(tc.client.addLabelCalls) != 0 {
				t.Errorf("reviewGateHold must not write labels, got %v", tc.client.addLabelCalls)
			}
		})
	}
}

func TestReviewGateHold_NoLinkedPRIsNotBlocked(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(string, string, int) (*gh.PRDetails, error) { return nil, nil },
	}
	eng := reviewTestEngine(t, client)
	item := gh.ProjectItem{Number: 10, Repo: "owner/repo"}
	h := eng.reviewGateHold(item, &stages.Stage{Name: "Review", WaitForReviews: boolPtr(true)}, "owner", "repo")
	if h.Blocked {
		t.Errorf("an item with no PR must not be held, got %q", h.Msg)
	}
}

// The wrapper still applies the label for a blocking verdict.
func TestReviewGateBlocksLanding_WrapperStillAppliesLabel(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	item := gh.ProjectItem{Number: 10, Repo: "owner/repo", LinkedPRNumber: 77}
	if !eng.reviewGateBlocksLanding(item, &stages.Stage{Name: "Review", WaitForReviews: boolPtr(true)}, "owner", "repo") {
		t.Fatal("expected the landing gate to block with nothing reviewed")
	}
	if !hasAddLabelCall(client, "fabrik:awaiting-review") {
		t.Error("wrapper must apply fabrik:awaiting-review when it blocks")
	}
}
