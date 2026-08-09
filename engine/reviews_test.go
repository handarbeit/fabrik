package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/boardcache"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tui"
)

// reviewTestStages returns a two-stage pipeline for review gate tests.
func reviewTestStages() []*stages.Stage {
	waitTrue := true
	return []*stages.Stage{
		{Name: "Implement", Order: 1, Prompt: "implement", WaitForReviews: &waitTrue},
		{Name: "Review", Order: 2, Prompt: "review"},
	}
}

func reviewTestEngine(t *testing.T, client *mockGitHubClient) *Engine {
	t.Helper()
	return testEngineWithStages(t, client, reviewTestStages())
}

func TestReviewGateAuthorityVerdict(t *testing.T) {
	tests := []struct {
		name           string
		reviewDecision string
		reviews        []gh.PRReview
		wantSatisfied  bool
	}{
		{
			name:           "reviewDecision APPROVED satisfies regardless of reviews",
			reviewDecision: "APPROVED",
			reviews:        nil,
			wantSatisfied:  true,
		},
		{
			name:           "reviewDecision CHANGES_REQUESTED blocks",
			reviewDecision: "CHANGES_REQUESTED",
			reviews:        []gh.PRReview{{Author: "alice", State: "APPROVED"}},
			wantSatisfied:  false,
		},
		{
			name:           "reviewDecision REVIEW_REQUIRED blocks",
			reviewDecision: "REVIEW_REQUIRED",
			reviews:        nil,
			wantSatisfied:  false,
		},
		{
			name:           "empty reviewDecision, clean reviews satisfies (fallback)",
			reviewDecision: "",
			reviews:        []gh.PRReview{{Author: "alice", State: "APPROVED"}},
			wantSatisfied:  true,
		},
		{
			name:           "empty reviewDecision, outstanding CHANGES_REQUESTED blocks (fallback)",
			reviewDecision: "",
			reviews:        []gh.PRReview{{Author: "bob", State: "CHANGES_REQUESTED"}},
			wantSatisfied:  false,
		},
		{
			name:           "empty reviewDecision, DISMISSED CHANGES_REQUESTED satisfies (fallback)",
			reviewDecision: "",
			reviews:        []gh.PRReview{{Author: "bob", State: "DISMISSED"}},
			wantSatisfied:  true,
		},
		{
			name:           "empty reviewDecision, no reviews at all satisfies (fallback)",
			reviewDecision: "",
			reviews:        nil,
			wantSatisfied:  true,
		},
		{
			name:           "unrecognized reviewDecision value blocks conservatively, does not fall back",
			reviewDecision: "SOME_FUTURE_VALUE",
			reviews:        []gh.PRReview{{Author: "alice", State: "APPROVED"}},
			wantSatisfied:  false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			satisfied, reason := reviewGateAuthorityVerdict(tt.reviewDecision, tt.reviews)
			if satisfied != tt.wantSatisfied {
				t.Errorf("reviewGateAuthorityVerdict(%q, %v) satisfied = %v, want %v (reason: %q)",
					tt.reviewDecision, tt.reviews, satisfied, tt.wantSatisfied, reason)
			}
			if reason == "" {
				t.Errorf("reviewGateAuthorityVerdict(%q, %v): reason should never be empty", tt.reviewDecision, tt.reviews)
			}
		})
	}
}

func TestReviewTimeoutReason(t *testing.T) {
	tests := []struct {
		name                string
		outstanding         []string
		declaredOutstanding []string
		authorityReason     string
		wantContains        []string
		wantExcludes        []string
	}{
		{
			name:            "authorityReason takes precedence over everything else",
			outstanding:     []string{"alice"},
			authorityReason: "review authority is authoritative and CHANGES_REQUESTED is outstanding",
			wantContains:    []string{"authoritative gate blocking:", "CHANGES_REQUESTED"},
		},
		{
			name:         "outstanding reviewers named when present",
			outstanding:  []string{"alice", "bob"},
			wantContains: []string{"pending reviewers:", "alice", "bob"},
			wantExcludes: []string{"authoritative gate blocking:", "bots"},
		},
		{
			name:                "declared-but-unrequested reviewer named when outstanding is empty",
			declaredOutstanding: []string{"pruefer-bot"},
			wantContains:        []string{"declared reviewer(s) not yet responded", "pruefer-bot"},
			wantExcludes:        []string{"authoritative gate blocking:", "pending reviewers:", "no reviewers were requested"},
		},
		{
			name:                "requested reviewer takes precedence over declared",
			outstanding:         []string{"alice"},
			declaredOutstanding: []string{"pruefer-bot"},
			wantContains:        []string{"pending reviewers:", "alice"},
			wantExcludes:        []string{"declared reviewer(s)", "pruefer-bot"},
		},
		{
			name:         "no reviewers requested or declared states the three known facts, no bot speculation",
			outstanding:  nil,
			wantContains: []string{"no reviewers were requested", "no review has been received", "cannot determine whether one is coming"},
			wantExcludes: []string{"bot", "authoritative gate blocking:", "pending reviewers:", "declared reviewer(s)"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := reviewTimeoutReason(tt.outstanding, tt.declaredOutstanding, tt.authorityReason)
			for _, want := range tt.wantContains {
				if !strings.Contains(got, want) {
					t.Errorf("reviewTimeoutReason(%v, %v, %q) = %q, want substring %q", tt.outstanding, tt.declaredOutstanding, tt.authorityReason, got, want)
				}
			}
			for _, excl := range tt.wantExcludes {
				if strings.Contains(strings.ToLower(got), strings.ToLower(excl)) {
					t.Errorf("reviewTimeoutReason(%v, %v, %q) = %q, must not contain %q", tt.outstanding, tt.declaredOutstanding, tt.authorityReason, got, excl)
				}
			}
		})
	}
}

// --- review-authority:<mode> label override (#1261) -----------------------

func TestExtractReviewAuthorityOverride(t *testing.T) {
	eng := reviewTestEngine(t, &mockGitHubClient{})
	tests := []struct {
		name   string
		labels []string
		want   string
	}{
		{"no labels", nil, ""},
		{"no review-authority label", []string{"stage:Plan:complete", "fabrik:locked"}, ""},
		{"single advisory", []string{"review-authority:advisory"}, "advisory"},
		{"single authoritative", []string{"review-authority:authoritative"}, "authoritative"},
		{"label among others", []string{"stage:Plan", "review-authority:authoritative", "fabrik:locked"}, "authoritative"},
		{"both present resolves to authoritative", []string{"review-authority:advisory", "review-authority:authoritative"}, "authoritative"},
		{"malformed suffix ignored", []string{"review-authority:Authoritative"}, ""},
		{"unknown suffix ignored", []string{"review-authority:foo"}, ""},
		{"empty suffix ignored", []string{"review-authority:"}, ""},
		{"malformed alongside valid label falls back to valid", []string{"review-authority:foo", "review-authority:advisory"}, "advisory"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := eng.extractReviewAuthorityOverride(0, tc.labels)
			if got != tc.want {
				t.Errorf("extractReviewAuthorityOverride(%v) = %q, want %q", tc.labels, got, tc.want)
			}
		})
	}
}

func TestEffectiveReviewAuthority(t *testing.T) {
	eng := reviewTestEngine(t, &mockGitHubClient{})
	tests := []struct {
		name       string
		labels     []string
		stageValue string
		want       string
	}{
		{"no label, stage advisory (unset) → advisory behavior", nil, "", ""},
		{"no label, stage authoritative → authoritative behavior", nil, "authoritative", "authoritative"},
		{"label authoritative, stage advisory → authoritative behavior", []string{"review-authority:authoritative"}, "", "authoritative"},
		{"label advisory, stage authoritative → advisory behavior", []string{"review-authority:advisory"}, "authoritative", "advisory"},
		{"both labels, stage advisory → authoritative wins", []string{"review-authority:advisory", "review-authority:authoritative"}, "", "authoritative"},
		{"malformed label, stage authoritative → falls back to stage config", []string{"review-authority:bogus"}, "authoritative", "authoritative"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			item := gh.ProjectItem{Number: 10, Labels: tc.labels}
			stage := &stages.Stage{Name: "Implement", ReviewAuthority: tc.stageValue}
			got := eng.effectiveReviewAuthority(item, stage)
			if got != tc.want {
				t.Errorf("effectiveReviewAuthority(labels=%v, stage.ReviewAuthority=%q) = %q, want %q",
					tc.labels, tc.stageValue, got, tc.want)
			}
		})
	}
}

// --- expected-reviewers:<mode> label override (#1304) ---------------------

// expectedReviewersSlice compares *[]string by nil-ness and contents, since
// two separately-allocated slices with identical contents are not == but
// should compare equal for these tests.
func expectedReviewersSlice(got *[]string, want *[]string) bool {
	if (got == nil) != (want == nil) {
		return false
	}
	if got == nil {
		return true
	}
	if len(*got) != len(*want) {
		return false
	}
	for i := range *got {
		if (*got)[i] != (*want)[i] {
			return false
		}
	}
	return true
}

func TestExtractExpectedReviewersOverride(t *testing.T) {
	eng := reviewTestEngine(t, &mockGitHubClient{})
	declared := &[]string{expectedReviewersSyntheticName}
	none := &[]string{}
	tests := []struct {
		name   string
		labels []string
		want   *[]string
	}{
		{"no labels", nil, nil},
		{"no expected-reviewers label", []string{"stage:Plan:complete", "fabrik:locked"}, nil},
		{"single none", []string{"expected-reviewers:none"}, none},
		{"single declared", []string{"expected-reviewers:declared"}, declared},
		{"label among others", []string{"stage:Plan", "expected-reviewers:declared", "fabrik:locked"}, declared},
		{"both present resolves to declared", []string{"expected-reviewers:none", "expected-reviewers:declared"}, declared},
		{"malformed suffix ignored", []string{"expected-reviewers:Declared"}, nil},
		{"unknown suffix ignored", []string{"expected-reviewers:foo"}, nil},
		{"empty suffix ignored", []string{"expected-reviewers:"}, nil},
		{"malformed alongside valid label falls back to valid", []string{"expected-reviewers:foo", "expected-reviewers:none"}, none},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := eng.extractExpectedReviewersOverride(0, tc.labels)
			if !expectedReviewersSlice(got, tc.want) {
				t.Errorf("extractExpectedReviewersOverride(%v) = %v, want %v", tc.labels, got, tc.want)
			}
		})
	}
}

func TestEffectiveExpectedReviewers(t *testing.T) {
	eng := reviewTestEngine(t, &mockGitHubClient{})
	declared := &[]string{expectedReviewersSyntheticName}
	none := &[]string{}
	stageDeclared := &[]string{"pruefer-bot"}
	tests := []struct {
		name       string
		labels     []string
		stageValue *[]string
		want       *[]string
	}{
		{"no label, stage nil → nil (FR-5 default preserved)", nil, nil, nil},
		{"no label, stage declared → stage value unchanged", nil, stageDeclared, stageDeclared},
		{"label none, stage nil → none", []string{"expected-reviewers:none"}, nil, none},
		{"label declared, stage nil → synthetic name", []string{"expected-reviewers:declared"}, nil, declared},
		{"label none, stage declared → label wins (none)", []string{"expected-reviewers:none"}, stageDeclared, none},
		{"both labels, stage nil → declared wins", []string{"expected-reviewers:none", "expected-reviewers:declared"}, nil, declared},
		{"malformed label, stage declared → falls back to stage config", []string{"expected-reviewers:bogus"}, stageDeclared, stageDeclared},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			item := gh.ProjectItem{Number: 10, Labels: tc.labels}
			stage := &stages.Stage{Name: "Implement", ExpectedReviewers: tc.stageValue}
			got := eng.effectiveExpectedReviewers(item, stage)
			if !expectedReviewersSlice(got, tc.want) {
				t.Errorf("effectiveExpectedReviewers(labels=%v, stage.ExpectedReviewers=%v) = %v, want %v",
					tc.labels, tc.stageValue, got, tc.want)
			}
		})
	}
}

// review-authority:authoritative label on a stage whose YAML is advisory
// (unset) must produce authoritative behavior at checkReviewGate — the
// advance gate.
func TestCheckReviewGate_LabelOverride_AuthoritativeOnAdvisoryStage_Blocks(t *testing.T) {
	client := &mockGitHubClient{
		fetchPRReviewDecisionFn: func(owner, repo string, prNumber int) (string, error) {
			return "CHANGES_REQUESTED", nil
		},
	}
	eng := reviewTestEngine(t, client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:         10,
		Repo:           "owner/repo",
		LinkedPRNumber: 55,
		Labels:         []string{"review-authority:authoritative"},
		LinkedPRReviews: []gh.PRReview{
			{Author: "alice", State: "CHANGES_REQUESTED"},
		},
	}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true)} // ReviewAuthority unset (advisory)

	blocked, timedOut, _, _ := eng.checkReviewGate(board, item, stage)

	if !blocked {
		t.Error("expected review-authority:authoritative label to make an advisory stage block on CHANGES_REQUESTED")
	}
	if timedOut {
		t.Error("expected not timedOut on first evaluation")
	}
}

// review-authority:advisory label on a stage whose YAML is authoritative
// must produce advisory behavior at checkReviewGate — the label overrides
// the stage config for this issue only.
func TestCheckReviewGate_LabelOverride_AdvisoryOnAuthoritativeStage_Clears(t *testing.T) {
	client := &mockGitHubClient{
		fetchPRReviewDecisionFn: func(owner, repo string, prNumber int) (string, error) {
			t.Error("FetchPRReviewDecision must not be called once the label resolves to advisory")
			return "", nil
		},
	}
	eng := reviewTestEngine(t, client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:         10,
		Repo:           "owner/repo",
		LinkedPRNumber: 55,
		Labels:         []string{"review-authority:advisory"},
		LinkedPRReviews: []gh.PRReview{
			{Author: "alice", State: "CHANGES_REQUESTED"},
		},
	}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true), ReviewAuthority: "authoritative"}

	blocked, timedOut, _, _ := eng.checkReviewGate(board, item, stage)

	if blocked {
		t.Error("expected review-authority:advisory label to clear the gate despite an authoritative stage and CHANGES_REQUESTED review")
	}
	if timedOut {
		t.Error("expected not timedOut")
	}
}

// --- review_authority: authoritative matrix (checkReviewGate) -------------

// Advisory mode (default, unset ReviewAuthority) must clear the gate exactly
// as before even when a review is CHANGES_REQUESTED — reinforces "no
// behavior change for advisory (default) repos" from the issue requirements.
func TestCheckReviewGate_Advisory_ChangesRequestedReview_StillClears(t *testing.T) {
	client := &mockGitHubClient{
		fetchPRReviewDecisionFn: func(owner, repo string, prNumber int) (string, error) {
			t.Error("FetchPRReviewDecision must not be called in advisory mode")
			return "", nil
		},
	}
	eng := reviewTestEngine(t, client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:         10,
		Repo:           "owner/repo",
		LinkedPRNumber: 55,
		LinkedPRReviews: []gh.PRReview{
			{Author: "alice", State: "CHANGES_REQUESTED"},
		},
	}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true)} // ReviewAuthority unset

	blocked, timedOut, _, _ := eng.checkReviewGate(board, item, stage)

	if blocked {
		t.Error("advisory mode must clear regardless of CHANGES_REQUESTED verdict (unchanged pre-existing behavior)")
	}
	if timedOut {
		t.Error("expected not timedOut")
	}
}

func TestCheckReviewGate_Authoritative_ReviewDecisionApproved_Clears(t *testing.T) {
	client := &mockGitHubClient{
		fetchPRReviewDecisionFn: func(owner, repo string, prNumber int) (string, error) {
			if prNumber != 55 {
				t.Errorf("expected FetchPRReviewDecision called with PR #55, got #%d", prNumber)
			}
			return "APPROVED", nil
		},
	}
	eng := reviewTestEngine(t, client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:         10,
		Repo:           "owner/repo",
		LinkedPRNumber: 55,
		LinkedPRReviews: []gh.PRReview{
			{Author: "alice", State: "APPROVED"},
		},
	}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true), ReviewAuthority: "authoritative"}

	blocked, timedOut, _, _ := eng.checkReviewGate(board, item, stage)

	if blocked {
		t.Error("expected gate to clear when reviewDecision=APPROVED")
	}
	if timedOut {
		t.Error("expected not timedOut")
	}
}

func TestCheckReviewGate_Authoritative_ReviewDecisionChangesRequested_Blocks(t *testing.T) {
	client := &mockGitHubClient{
		fetchPRReviewDecisionFn: func(owner, repo string, prNumber int) (string, error) {
			return "CHANGES_REQUESTED", nil
		},
	}
	eng := reviewTestEngine(t, client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:         10,
		Repo:           "owner/repo",
		LinkedPRNumber: 55,
		LinkedPRReviews: []gh.PRReview{
			{Author: "alice", State: "CHANGES_REQUESTED"},
		},
	}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true), ReviewAuthority: "authoritative"}

	blocked, timedOut, _, _ := eng.checkReviewGate(board, item, stage)

	if !blocked {
		t.Error("expected gate to stay blocked when reviewDecision=CHANGES_REQUESTED")
	}
	if timedOut {
		t.Error("expected not timedOut on first evaluation")
	}
	if len(client.addLabelCalls) != 1 || client.addLabelCalls[0].labelName != "fabrik:awaiting-review" {
		t.Errorf("expected fabrik:awaiting-review label add, got %v", client.addLabelCalls)
	}
}

// No branch-protection review requirement (reviewDecision == "") — authoritative
// mode must not become a silent no-op; it falls back to Fabrik's own
// no-CHANGES_REQUESTED computation, which here is satisfied.
func TestCheckReviewGate_Authoritative_NoBranchProtection_NoChangesRequested_Clears(t *testing.T) {
	client := &mockGitHubClient{
		fetchPRReviewDecisionFn: func(owner, repo string, prNumber int) (string, error) {
			return "", nil // no branch-protection review requirement configured
		},
	}
	eng := reviewTestEngine(t, client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:         10,
		Repo:           "owner/repo",
		LinkedPRNumber: 55,
		LinkedPRReviews: []gh.PRReview{
			{Author: "alice", State: "APPROVED"},
		},
	}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true), ReviewAuthority: "authoritative"}

	blocked, timedOut, _, _ := eng.checkReviewGate(board, item, stage)

	if blocked {
		t.Error("expected gate to clear via fallback computation when no CHANGES_REQUESTED review exists")
	}
	if timedOut {
		t.Error("expected not timedOut")
	}
}

// Same no-branch-protection scenario, but with an active CHANGES_REQUESTED
// review — the fallback computation must still block, so authoritative mode
// is meaningful even on a repo with no branch protection configured.
func TestCheckReviewGate_Authoritative_NoBranchProtection_ChangesRequested_Blocks(t *testing.T) {
	client := &mockGitHubClient{
		fetchPRReviewDecisionFn: func(owner, repo string, prNumber int) (string, error) {
			return "", nil
		},
	}
	eng := reviewTestEngine(t, client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:         10,
		Repo:           "owner/repo",
		LinkedPRNumber: 55,
		LinkedPRReviews: []gh.PRReview{
			{Author: "bob", State: "CHANGES_REQUESTED"},
		},
	}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true), ReviewAuthority: "authoritative"}

	blocked, timedOut, _, _ := eng.checkReviewGate(board, item, stage)

	if !blocked {
		t.Error("expected gate to stay blocked via fallback computation on a repo with no branch protection")
	}
	if timedOut {
		t.Error("expected not timedOut on first evaluation")
	}
}

// A FetchPRReviewDecision fetch error must block conservatively, never
// silently fall back to advisory clearing.
func TestCheckReviewGate_Authoritative_FetchReviewDecisionError_BlocksConservatively(t *testing.T) {
	client := &mockGitHubClient{
		fetchPRReviewDecisionFn: func(owner, repo string, prNumber int) (string, error) {
			return "", fmt.Errorf("transient GraphQL failure")
		},
	}
	eng := reviewTestEngine(t, client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:         10,
		Repo:           "owner/repo",
		LinkedPRNumber: 55,
		LinkedPRReviews: []gh.PRReview{
			{Author: "alice", State: "APPROVED"},
		},
	}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true), ReviewAuthority: "authoritative"}

	blocked, timedOut, _, _ := eng.checkReviewGate(board, item, stage)

	if !blocked {
		t.Error("expected gate to block conservatively when FetchPRReviewDecision errors")
	}
	if timedOut {
		t.Error("expected not timedOut on first evaluation")
	}
}

// Finding 2 (#1375): a declared expected_reviewers bot must never defer a
// human's authoritative escalation. Here, the requested human (alice) has
// already responded with CHANGES_REQUESTED (so she's no longer in
// LinkedPRReviewRequests — GitHub drops a reviewer from the requested list
// once they submit), review_authority is authoritative (so authorityReason
// is non-empty), and the stage separately declares a bot via
// expected_reviewers that has not yet reviewed. Before the fix,
// reviewGateAllBots's declared-reviewer branch (len(outstanding)==0) would
// see only the declared bot outstanding and return allBots=true, routing
// through the Phase 1 bot re-prompt ladder instead of the human-escalation
// timeout path — deferring the human's CHANGES_REQUESTED verdict by a full
// ReviewWaitTimeout window. With the fix, authorityReason != "" forces
// allBots=false, so Phase 1 must not fire (no re-prompt comment, no
// DeleteReviewRequest/AddReviewRequest mutation, no fabrik:bot-reprompted
// label) — the item instead falls straight to the standard timeout branch,
// which is what ultimately carries the authoritative reason to a human via
// pauseForReviewTimeout. AC2 in the issue requires this scenario to fail
// against unpatched main.
func TestCheckReviewGate_DeclaredBotDoesNotDeferAuthoritativeHumanEscalation(t *testing.T) {
	client := &mockGitHubClient{
		fetchPRReviewDecisionFn: func(owner, repo string, prNumber int) (string, error) {
			return "CHANGES_REQUESTED", nil
		},
	}
	eng := reviewTestEngine(t, client)
	eng.cfg.ReviewWaitTimeout = 5 * time.Minute

	awaitingApplied := time.Now().Add(-10 * time.Minute)
	client.fetchLabelAppliedAtFn = func(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
		if labelName == "fabrik:awaiting-review" {
			return awaitingApplied, nil
		}
		return time.Time{}, nil
	}

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:                 10,
		Repo:                   "owner/repo",
		LinkedPRNumber:         42,
		Labels:                 []string{"fabrik:awaiting-review"},
		LinkedPRReviewRequests: nil, // alice already responded — GitHub dropped her from the requested list
		LinkedPRReviews: []gh.PRReview{
			{Author: "alice", State: "CHANGES_REQUESTED"},
		},
	}
	declared := []string{"some-declared-bot"}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true), ReviewAuthority: "authoritative", ExpectedReviewers: &declared}

	blocked, timedOut, _, _ := eng.checkReviewGate(board, item, stage)

	if blocked {
		t.Error("expected NOT blocked — timeout branch fires instead (authoritative escalation, not a bot re-prompt)")
	}
	if !timedOut {
		t.Error("expected timedOut — the human escalation must reach the standard pause path, not the bot ladder")
	}
	if len(client.addCommentCalls) != 0 {
		t.Errorf("expected NO bot re-prompt comment (declared bot must not preempt human escalation), got %d: %v", len(client.addCommentCalls), client.addCommentCalls)
	}
	if len(client.deleteReviewRequestCalls) != 0 {
		t.Errorf("expected NO DeleteReviewRequest calls, got %d", len(client.deleteReviewRequestCalls))
	}
	if len(client.addReviewRequestCalls) != 0 {
		t.Errorf("expected NO AddReviewRequest calls, got %d", len(client.addReviewRequestCalls))
	}
	for _, call := range client.addLabelCalls {
		if call.labelName == "fabrik:bot-reprompted" {
			t.Error("expected fabrik:bot-reprompted NOT to be applied — Phase 1 must not fire for an authoritative-blocked human escalation")
		}
	}
}

// Authoritative mode must also work on a base:<branch> repo, where reviews
// are REST-sourced and the resolved PR number (not item.LinkedPRNumber,
// always 0 there) is what FetchPRReviewDecision is called with.
func TestCheckReviewGate_NonDefaultBase_Authoritative_ChangesRequested_Blocks(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 77, State: "open"}, nil
		},
		fetchPRClosingIssuesFn: func(owner, repo string, prNumber int) ([]int, error) {
			return []int{10}, nil
		},
		fetchPRReviewsFn: func(owner, repo string, prNumber int) ([]gh.PRReview, error) {
			return []gh.PRReview{{Author: "bob", State: "CHANGES_REQUESTED"}}, nil
		},
		fetchPRReviewRequestsFn: func(owner, repo string, prNumber int) ([]gh.ReviewRequest, error) {
			return nil, nil
		},
		fetchPRReviewDecisionFn: func(owner, repo string, prNumber int) (string, error) {
			if prNumber != 77 {
				t.Errorf("expected FetchPRReviewDecision called with resolved PR #77, got #%d", prNumber)
			}
			return "", nil // no branch protection configured — exercises the fallback
		},
	}
	eng := reviewTestEngine(t, client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:         10,
		Repo:           "owner/repo",
		Labels:         []string{"base:develop"},
		LinkedPRNumber: 0,
	}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true), ReviewAuthority: "authoritative"}

	blocked, timedOut, _, _ := eng.checkReviewGate(board, item, stage)

	if !blocked {
		t.Error("expected gate to stay blocked on a base:<branch> repo with an outstanding CHANGES_REQUESTED review")
	}
	if timedOut {
		t.Error("expected not timedOut on first evaluation")
	}
}

// (a) No requested reviewers AND no reviews submitted → gate STAYS BLOCKED,
// waiting for self-assigning bot reviewers (Copilot, Gemini) to post. This
// is the common yolo case: the pipeline marks the PR ready and immediately
// evaluates the gate; bots are still processing and haven't submitted yet,
// so we wait.
func TestCheckReviewGate_NoReviewersNoReviews_Blocks(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:                 10,
		Repo:                   "owner/repo",
		LinkedPRReviewRequests: nil, // no requested reviewers (bots don't use this)
		LinkedPRReviews:        nil, // no reviews submitted yet
	}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true)}

	blocked, timedOut, _, _ := eng.checkReviewGate(board, item, stage)

	if !blocked {
		t.Error("expected blocked when no reviews submitted yet (bots may still be processing)")
	}
	if timedOut {
		t.Error("expected not timedOut on first evaluation")
	}
	if len(client.addLabelCalls) != 1 {
		t.Errorf("expected 1 label add (fabrik:awaiting-review), got %d", len(client.addLabelCalls))
	}
}

// FR-1 (#1334): when nothing is requested and nothing is declared via
// expected_reviewers, the blocking log line must state what is known — that
// nothing is requested and nothing is declared — and name expected_reviewers
// as the way to change that. It must not speculate that bot reviewers may
// still be processing (#1080).
func TestCheckReviewGate_NoReviewersNoDeclaration_LogsExpectedReviewersRemedy(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	eventsCh := make(chan tui.Event, 32)
	eng.events = eventsCh
	item := gh.ProjectItem{
		Number:                 10,
		Repo:                   "owner/repo",
		LinkedPRReviewRequests: nil, // no requested reviewers
		LinkedPRReviews:        nil, // no reviews submitted yet
	}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true)} // no ExpectedReviewers declared

	blocked, _, _, _ := eng.checkReviewGate(board, item, stage)
	if !blocked {
		t.Fatal("expected blocked when no reviews submitted yet")
	}

	close(eventsCh)
	var msg string
	var found bool
	for ev := range eventsCh {
		le, ok := ev.(tui.LogEvent)
		if !ok || le.Tag != "awaiting-review" {
			continue
		}
		if strings.Contains(le.Message, "waiting for initial review submission") {
			msg = le.Message
			found = true
		}
	}
	if !found {
		t.Fatal("expected a log event for 'waiting for initial review submission'")
	}
	if !strings.Contains(msg, "no reviewers requested") || !strings.Contains(msg, "none declared via expected_reviewers") {
		t.Errorf("expected log to state nothing requested and nothing declared via expected_reviewers, got: %s", msg)
	}
	if strings.Contains(msg, "bot reviewers may still be processing") {
		t.Errorf("log must not speculate that bot reviewers may still be processing when nothing is requested or declared, got: %s", msg)
	}
}

// (a2) No requested reviewers but at least one review submitted → gate clears.
// This covers the case where a bot like Copilot or Gemini has self-submitted
// without ever appearing in reviewRequests.
func TestCheckReviewGate_NoReviewersWithReview_Clears(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:                 10,
		Repo:                   "owner/repo",
		LinkedPRReviewRequests: nil, // no requested reviewers
		LinkedPRReviews: []gh.PRReview{
			{Author: "copilot-pull-request-reviewer", State: "COMMENTED", Body: "## Pull request overview\n\nLGTM."},
		},
	}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true)}

	blocked, timedOut, _, _ := eng.checkReviewGate(board, item, stage)

	if blocked {
		t.Error("expected not blocked once a review has been submitted")
	}
	if timedOut {
		t.Error("expected not timedOut")
	}
	if len(client.addLabelCalls) != 0 {
		t.Errorf("expected no label adds, got %d", len(client.addLabelCalls))
	}
}

// (a2) Gate disabled (nil WaitForReviews) → always returns false.
func TestCheckReviewGate_GateDisabled_ReturnsFalse(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 10,
		Repo:   "owner/repo",
		LinkedPRReviewRequests: []gh.ReviewRequest{
			{Login: "copilot"},
		},
	}
	// WaitForReviews is nil (not set)
	stage := &stages.Stage{Name: "Implement", WaitForReviews: nil}

	blocked, timedOut, _, _ := eng.checkReviewGate(board, item, stage)

	if blocked {
		t.Error("expected not blocked when WaitForReviews is nil")
	}
	if timedOut {
		t.Error("expected not timedOut when WaitForReviews is nil")
	}
}

// (b) Reviewer requested but no review submitted → block and apply label.
func TestCheckReviewGate_ReviewerRequested_NoReview_Blocks(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 10,
		Repo:   "owner/repo",
		// copilot is in reviewRequests → outstanding
		LinkedPRReviewRequests: []gh.ReviewRequest{
			{Login: "copilot"},
		},
		// No reviews submitted yet
		LinkedPRReviews: nil,
	}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true)}

	blocked, timedOut, _, _ := eng.checkReviewGate(board, item, stage)

	if !blocked {
		t.Error("expected blocked when reviewer has not submitted")
	}
	if timedOut {
		t.Error("expected not timedOut when reviewer has not submitted")
	}
	// Label should be applied on first block
	if len(client.addLabelCalls) != 1 {
		t.Fatalf("expected 1 add label call, got %d", len(client.addLabelCalls))
	}
	if client.addLabelCalls[0].labelName != "fabrik:awaiting-review" {
		t.Errorf("expected fabrik:awaiting-review label, got %q", client.addLabelCalls[0].labelName)
	}
}

// (b2) Already has awaiting-review label → still blocked but no duplicate label add.
func TestCheckReviewGate_AlreadyWaiting_NoLabelAdd(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	// FetchLabelAppliedAt returns recent time (no timeout)
	recentTime := time.Now().Add(-1 * time.Minute)
	client.fetchLabelAppliedAtFn = func(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
		return recentTime, nil
	}
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 10,
		Repo:   "owner/repo",
		Labels: []string{"fabrik:awaiting-review"},
		LinkedPRReviewRequests: []gh.ReviewRequest{
			{Login: "copilot"},
		},
	}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true)}

	blocked, timedOut, _, _ := eng.checkReviewGate(board, item, stage)

	if !blocked {
		t.Error("expected still blocked")
	}
	if timedOut {
		t.Error("expected not timedOut when recently applied label")
	}
	// No new label add (already present)
	if len(client.addLabelCalls) != 0 {
		t.Errorf("expected no label add when already waiting, got %d", len(client.addLabelCalls))
	}
}

// (c) All requested reviewers have submitted → advance (no reviewers in reviewRequests).
func TestCheckReviewGate_AllReviewersSubmitted_ReturnsFalse(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	// copilot no longer in reviewRequests (they submitted) and awaiting-review label present
	item := gh.ProjectItem{
		Number: 10,
		Repo:   "owner/repo",
		Labels: []string{"fabrik:awaiting-review"},
		// Empty reviewRequests = all reviewers submitted
		LinkedPRReviewRequests: nil,
		LinkedPRReviews: []gh.PRReview{
			{Author: "copilot", State: "APPROVED"},
		},
	}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true)}

	blocked, timedOut, _, _ := eng.checkReviewGate(board, item, stage)

	if blocked {
		t.Error("expected not blocked when all reviewers submitted")
	}
	if timedOut {
		t.Error("expected not timedOut when all reviewers submitted")
	}
	// Label should be removed
	if len(client.removeLabelCalls) != 1 {
		t.Fatalf("expected 1 remove label call, got %d", len(client.removeLabelCalls))
	}
	if client.removeLabelCalls[0].labelName != "fabrik:awaiting-review" {
		t.Errorf("expected removal of fabrik:awaiting-review, got %q", client.removeLabelCalls[0].labelName)
	}
}

// (d) Timeout elapsed → advance with warning, label removed.
func TestCheckReviewGate_TimeoutElapsed_ReturnsFalse(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	// Override timeout to 5 minutes; label was applied 10 minutes ago → timed out
	eng.cfg.ReviewWaitTimeout = 5 * time.Minute
	appliedAt := time.Now().Add(-10 * time.Minute)
	client.fetchLabelAppliedAtFn = func(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
		return appliedAt, nil
	}
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 10,
		Repo:   "owner/repo",
		Labels: []string{"fabrik:awaiting-review"},
		// Reviewer still outstanding (would block if not for timeout)
		LinkedPRReviewRequests: []gh.ReviewRequest{
			{Login: "copilot"},
		},
	}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true)}

	blocked, timedOut, _, _ := eng.checkReviewGate(board, item, stage)

	if blocked {
		t.Error("expected not blocked when timeout elapsed")
	}
	if !timedOut {
		t.Error("expected timedOut == true when timeout elapsed")
	}
	// Label should be removed after timeout
	if len(client.removeLabelCalls) != 1 {
		t.Fatalf("expected 1 remove label call, got %d", len(client.removeLabelCalls))
	}
	if client.removeLabelCalls[0].labelName != "fabrik:awaiting-review" {
		t.Errorf("expected removal of fabrik:awaiting-review, got %q", client.removeLabelCalls[0].labelName)
	}
	// FetchLabelAppliedAt should have been called for timeout check
	if len(client.fetchLabelAppliedAtCalls) != 1 {
		t.Errorf("expected FetchLabelAppliedAt to be called, got %d calls", len(client.fetchLabelAppliedAtCalls))
	}
}

// (e) Dismissed reviewer re-blocks gate: reviewer re-appears in reviewRequests.
func TestCheckReviewGate_DismissedReviewer_Reblocks(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	// Label was applied recently — not timed out
	recentTime := time.Now().Add(-1 * time.Minute)
	client.fetchLabelAppliedAtFn = func(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
		return recentTime, nil
	}
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	// Reviewer submitted once (appears in latestReviews) but review was dismissed
	// and they were re-added to reviewRequests — so they're outstanding again.
	item := gh.ProjectItem{
		Number: 10,
		Repo:   "owner/repo",
		Labels: []string{"fabrik:awaiting-review"},
		// copilot re-appears in reviewRequests after dismissal
		LinkedPRReviewRequests: []gh.ReviewRequest{
			{Login: "copilot"},
		},
		// Prior review (now dismissed)
		LinkedPRReviews: []gh.PRReview{
			{Author: "copilot", State: "APPROVED"},
		},
	}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true)}

	blocked, timedOut, _, _ := eng.checkReviewGate(board, item, stage)

	if !blocked {
		t.Error("expected re-blocked after reviewer dismissal and re-request")
	}
	if timedOut {
		t.Error("expected not timedOut on dismissed reviewer re-block")
	}
	// No new label (already present)
	if len(client.addLabelCalls) != 0 {
		t.Errorf("expected no new label add, got %d", len(client.addLabelCalls))
	}
}

func TestCheckReviewGate_DismissedReviewDoesNotClear(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	recentTime := time.Now().Add(-1 * time.Minute)
	client.fetchLabelAppliedAtFn = func(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
		return recentTime, nil
	}
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	// No outstanding review requests, but the only review is DISMISSED.
	// hasReviews must be false — the gate should stay blocked.
	item := gh.ProjectItem{
		Number:                 10,
		Repo:                   "owner/repo",
		Labels:                 []string{"fabrik:awaiting-review"},
		LinkedPRReviewRequests: nil,
		LinkedPRReviews:        []gh.PRReview{{Author: "alice", State: "DISMISSED"}},
	}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true)}

	blocked, timedOut, _, _ := eng.checkReviewGate(board, item, stage)

	if !blocked {
		t.Error("expected gate to stay blocked when only review is DISMISSED")
	}
	if timedOut {
		t.Error("expected not timedOut")
	}
}

// (f) buildReviewThreadComments returns inline thread comments with real DatabaseIDs.
func TestBuildReviewThreadComments(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	item := gh.ProjectItem{
		Number: 10,
		Repo:   "owner/repo",
		LinkedPRReviewThreadComments: []gh.Comment{
			{ID: "PRRC_1", DatabaseID: 101, Author: "copilot", Body: "Please fix the error handling.", ReviewThreadID: "RT_1"},
			{ID: "PRRC_2", DatabaseID: 102, Author: "human", Body: "Consider edge case.", ReviewThreadID: "RT_2"},
		},
	}

	comments := eng.buildReviewThreadComments(item)

	if len(comments) != 2 {
		t.Fatalf("expected 2 thread comments, got %d", len(comments))
	}
	if comments[0].DatabaseID == 0 {
		t.Error("thread comments must carry real DatabaseIDs so reactions work")
	}
	if comments[0].ReviewThreadID == "" {
		t.Error("thread comments must carry ReviewThreadID so threads can be resolved later")
	}
}

// (f2) buildReviewThreadComments skips comments already present in ProcessedComments.
func TestBuildReviewThreadComments_ProcessedCommentsSkip(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	item := gh.ProjectItem{
		Number: 10,
		Repo:   "owner/repo",
		LinkedPRReviewThreadComments: []gh.Comment{
			{ID: "PRRC_1", DatabaseID: 101, Author: "copilot", Body: "Already handled.", ReviewThreadID: "RT_1"},
			{ID: "PRRC_2", DatabaseID: 102, Author: "human", Body: "Not yet handled.", ReviewThreadID: "RT_2"},
		},
	}

	// Pre-populate ProcessedComments for comment PRRC_1 (simulates markCommentsProcessed).
	eng.store.Apply(itemstate.CommentProcessed{Repo: "owner/repo", Number: 10, CommentID: "PRRC_1", At: time.Now()})

	comments := eng.buildReviewThreadComments(item)

	if len(comments) != 1 {
		t.Fatalf("expected 1 comment (PRRC_1 should be skipped), got %d", len(comments))
	}
	if comments[0].ID != "PRRC_2" {
		t.Errorf("expected remaining comment to be PRRC_2, got %q", comments[0].ID)
	}
}

// currentHeadReviewThreadComments excludes comments whose parent thread
// GitHub has marked isOutdated (superseded by a later push) — the stale-SHA
// scoping required at both yolo-merge guard points (#1207).
func TestCurrentHeadReviewThreadComments_ExcludesOutdated(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	item := gh.ProjectItem{
		Number: 10,
		Repo:   "owner/repo",
		LinkedPRReviewThreadComments: []gh.Comment{
			{ID: "PRRC_current", DatabaseID: 101, Author: "pruefer", Body: "current-head finding", ReviewThreadID: "RT_current", IsOutdated: false},
			{ID: "PRRC_stale", DatabaseID: 102, Author: "pruefer", Body: "stale finding", ReviewThreadID: "RT_stale", IsOutdated: true},
		},
	}

	comments := eng.currentHeadReviewThreadComments(item)

	if len(comments) != 1 {
		t.Fatalf("expected 1 current-head comment, got %d", len(comments))
	}
	if comments[0].ID != "PRRC_current" {
		t.Errorf("expected remaining comment to be PRRC_current, got %q", comments[0].ID)
	}
}

// When every unresolved thread is outdated, currentHeadReviewThreadComments
// must return empty — a thread against a superseded commit must not block
// indefinitely at either guard point.
func TestCurrentHeadReviewThreadComments_AllOutdatedReturnsEmpty(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	item := gh.ProjectItem{
		Number: 10,
		Repo:   "owner/repo",
		LinkedPRReviewThreadComments: []gh.Comment{
			{ID: "PRRC_stale", DatabaseID: 102, Author: "pruefer", Body: "stale finding", ReviewThreadID: "RT_stale", IsOutdated: true},
		},
	}

	comments := eng.currentHeadReviewThreadComments(item)

	if len(comments) != 0 {
		t.Fatalf("expected 0 current-head comments, got %d", len(comments))
	}
}

// Finding 4 (#1375): buildReviewBodyComments must pick up a CHANGES_REQUESTED
// review's body even when it has zero inline thread comments — the exact
// shape GitHub's REST API guarantees (body required for REQUEST_CHANGES) and
// the exact shape the e2e harness's SubmitPRReview produces.
func TestBuildReviewBodyComments_ChangesRequestedWithBody(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	item := gh.ProjectItem{
		Number: 10,
		Repo:   "owner/repo",
		LinkedPRReviews: []gh.PRReview{
			{Author: "alice", State: "CHANGES_REQUESTED", Body: "please fix the error handling", DatabaseID: 555},
		},
	}

	comments := eng.buildReviewBodyComments(item)

	if len(comments) != 1 {
		t.Fatalf("expected 1 synthetic body comment, got %d", len(comments))
	}
	if comments[0].ID != "review-body:555" {
		t.Errorf("expected synthetic ID review-body:555, got %q", comments[0].ID)
	}
	if comments[0].DatabaseID != 0 {
		t.Errorf("expected synthetic comment to carry DatabaseID 0, got %d", comments[0].DatabaseID)
	}
	if comments[0].Body != "please fix the error handling" {
		t.Errorf("expected body to be carried through, got %q", comments[0].Body)
	}
}

// Pruefer review finding (#1375): buildReviewBodyComments must prefer the
// review's actual SubmittedAt over time.Now(), so a mixed batch of thread
// comments (real timestamps) and a review body sorts/reads correctly by
// chronology in the reinvoke prompt (engine/claude.go renders CreatedAt
// verbatim) instead of always appearing to be the newest item regardless of
// actual submission time.
func TestBuildReviewBodyComments_UsesSubmittedAt(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	submittedAt := time.Date(2026, 1, 15, 10, 30, 0, 0, time.UTC)
	item := gh.ProjectItem{
		Number: 10,
		Repo:   "owner/repo",
		LinkedPRReviews: []gh.PRReview{
			{Author: "alice", State: "CHANGES_REQUESTED", Body: "please fix this", DatabaseID: 555, SubmittedAt: submittedAt},
		},
	}

	comments := eng.buildReviewBodyComments(item)

	if len(comments) != 1 {
		t.Fatalf("expected 1 synthetic body comment, got %d", len(comments))
	}
	if !comments[0].CreatedAt.Equal(submittedAt) {
		t.Errorf("CreatedAt = %v, want the review's actual SubmittedAt %v", comments[0].CreatedAt, submittedAt)
	}
}

// When SubmittedAt is unavailable (zero value — e.g. a data source that
// doesn't carry it), buildReviewBodyComments must fall back to time.Now()
// rather than leaving the synthetic comment with a zero timestamp.
func TestBuildReviewBodyComments_FallsBackToNowWhenSubmittedAtZero(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	item := gh.ProjectItem{
		Number: 10,
		Repo:   "owner/repo",
		LinkedPRReviews: []gh.PRReview{
			{Author: "alice", State: "CHANGES_REQUESTED", Body: "please fix this", DatabaseID: 555},
		},
	}

	before := time.Now()
	comments := eng.buildReviewBodyComments(item)
	after := time.Now()

	if len(comments) != 1 {
		t.Fatalf("expected 1 synthetic body comment, got %d", len(comments))
	}
	if comments[0].CreatedAt.Before(before) || comments[0].CreatedAt.After(after) {
		t.Errorf("expected CreatedAt to fall back to time.Now() (between %v and %v), got %v", before, after, comments[0].CreatedAt)
	}
}

// buildReviewBodyComments must skip a review with DatabaseID == 0 (not yet
// fetched/unavailable — no stable ID to key dedup on).
func TestBuildReviewBodyComments_SkipsZeroDatabaseID(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	item := gh.ProjectItem{
		Number: 10,
		Repo:   "owner/repo",
		LinkedPRReviews: []gh.PRReview{
			{Author: "alice", State: "CHANGES_REQUESTED", Body: "please fix this", DatabaseID: 0},
		},
	}

	comments := eng.buildReviewBodyComments(item)

	if len(comments) != 0 {
		t.Fatalf("expected 0 comments for DatabaseID==0 review, got %d", len(comments))
	}
}

// buildReviewBodyComments must skip APPROVED reviews (a body there is
// typically "LGTM", not something to act on) and DISMISSED reviews (no
// longer an active verdict).
func TestBuildReviewBodyComments_SkipsApprovedAndDismissed(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	item := gh.ProjectItem{
		Number: 10,
		Repo:   "owner/repo",
		LinkedPRReviews: []gh.PRReview{
			{Author: "alice", State: "APPROVED", Body: "LGTM!", DatabaseID: 601},
			{Author: "bob", State: "DISMISSED", Body: "please fix this", DatabaseID: 602},
		},
	}

	comments := eng.buildReviewBodyComments(item)

	if len(comments) != 0 {
		t.Fatalf("expected 0 comments (APPROVED/DISMISSED skipped), got %d", len(comments))
	}
}

// buildReviewBodyComments must skip COMMENTED reviews even though they carry
// a non-empty body (Pruefer review finding, #1375). Automated reviewers like
// Copilot routinely submit a COMMENTED review whose body is a generic
// "Pull request overview" summary with zero inline comments — treating that
// as actionable feedback would trigger a reinvoke/Claude invocation on every
// wait_for_reviews stage (advisory included, not just authoritative), far
// beyond a CHANGES_REQUESTED verdict's scope. reviewGateAuthorityVerdict draws
// the identical line — only CHANGES_REQUESTED blocks its fallback verdict.
func TestBuildReviewBodyComments_SkipsCommented(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	item := gh.ProjectItem{
		Number: 10,
		Repo:   "owner/repo",
		LinkedPRReviews: []gh.PRReview{
			{Author: "copilot-pull-request-reviewer", State: "COMMENTED", Body: "## Pull request overview\n\nLGTM.", DatabaseID: 603},
		},
	}

	comments := eng.buildReviewBodyComments(item)

	if len(comments) != 0 {
		t.Fatalf("expected 0 comments (COMMENTED skipped), got %d", len(comments))
	}
}

// buildReviewBodyComments must skip a PENDING review — a draft review not yet
// submitted (Pruefer review finding, #1375). The actionable-state check is an
// allow-list (State == "CHANGES_REQUESTED" only, not a APPROVED/DISMISSED
// deny-list), so PENDING — and any other state neither data source is
// documented to return here — is excluded structurally, not by relying on it
// never occurring in practice.
func TestBuildReviewBodyComments_SkipsPending(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	item := gh.ProjectItem{
		Number: 10,
		Repo:   "owner/repo",
		LinkedPRReviews: []gh.PRReview{
			{Author: "alice", State: "PENDING", Body: "draft, not submitted yet", DatabaseID: 604},
		},
	}

	comments := eng.buildReviewBodyComments(item)

	if len(comments) != 0 {
		t.Fatalf("expected 0 comments (PENDING skipped), got %d", len(comments))
	}
}

// buildReviewBodyComments must skip a review with an empty body — nothing to
// act on.
func TestBuildReviewBodyComments_SkipsEmptyBody(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	item := gh.ProjectItem{
		Number: 10,
		Repo:   "owner/repo",
		LinkedPRReviews: []gh.PRReview{
			{Author: "alice", State: "CHANGES_REQUESTED", Body: "", DatabaseID: 701},
		},
	}

	comments := eng.buildReviewBodyComments(item)

	if len(comments) != 0 {
		t.Fatalf("expected 0 comments for empty-body review, got %d", len(comments))
	}
}

// buildReviewBodyComments must dedup via snap.CommentProcessed keyed on the
// synthetic review-body: ID (R7) — no GitHub reaction endpoint exists for a
// top-level review body, so this is the only idempotency guard.
func TestBuildReviewBodyComments_DedupViaCommentProcessed(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	item := gh.ProjectItem{
		Number: 10,
		Repo:   "owner/repo",
		LinkedPRReviews: []gh.PRReview{
			{Author: "alice", State: "CHANGES_REQUESTED", Body: "please fix this", DatabaseID: 801},
		},
	}

	eng.store.Apply(itemstate.CommentProcessed{Repo: "owner/repo", Number: 10, CommentID: "review-body:801", At: time.Now()})

	comments := eng.buildReviewBodyComments(item)

	if len(comments) != 0 {
		t.Fatalf("expected 0 comments (already processed), got %d", len(comments))
	}
}

// Pruefer review finding (#1375): on a base:<branch> item, item.LinkedPRReviews
// is always structurally empty (closedByPullRequestsReferences is empty for
// non-default-base PRs), so buildReviewBodyComments must REST-fallback —
// mirroring checkReviewGate's own base:<branch> branch — rather than silently
// returning nothing forever. item.LinkedPRNumber == 0 here (as it always is on
// a base:<branch> repo), so this also exercises the FetchLinkedPR resolution
// path.
func TestBuildReviewBodyComments_NonDefaultBase_RESTFallback(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 77, State: "open"}, nil
		},
		fetchPRReviewsFn: func(owner, repo string, prNumber int) ([]gh.PRReview, error) {
			if prNumber != 77 {
				t.Errorf("expected FetchPRReviews called with resolved PR #77, got #%d", prNumber)
			}
			return []gh.PRReview{
				{Author: "bob", State: "CHANGES_REQUESTED", Body: "please fix this", DatabaseID: 900},
			}, nil
		},
	}
	eng := reviewTestEngine(t, client)
	item := gh.ProjectItem{
		Number:         10,
		Repo:           "owner/repo",
		Labels:         []string{"base:develop"},
		LinkedPRNumber: 0,
		// LinkedPRReviews deliberately empty — this is the true GraphQL state
		// for a base:<branch> item; the REST fallback below is what must
		// populate the result instead.
	}

	comments := eng.buildReviewBodyComments(item)

	if len(comments) != 1 {
		t.Fatalf("expected 1 synthetic body comment via REST fallback, got %d", len(comments))
	}
	if comments[0].ID != "review-body:900" {
		t.Errorf("expected synthetic ID review-body:900, got %q", comments[0].ID)
	}
}

// When item.LinkedPRNumber is already resolved (nonzero), the REST fallback
// must reuse it rather than calling FetchLinkedPR again.
func TestBuildReviewBodyComments_NonDefaultBase_ReusesResolvedPRNumber(t *testing.T) {
	fetchLinkedPRCalls := 0
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			fetchLinkedPRCalls++
			return &gh.PRDetails{Number: 99, State: "open"}, nil
		},
		fetchPRReviewsFn: func(owner, repo string, prNumber int) ([]gh.PRReview, error) {
			if prNumber != 55 {
				t.Errorf("expected FetchPRReviews called with item.LinkedPRNumber #55, got #%d", prNumber)
			}
			return []gh.PRReview{
				{Author: "bob", State: "CHANGES_REQUESTED", Body: "please fix this", DatabaseID: 901},
			}, nil
		},
	}
	eng := reviewTestEngine(t, client)
	item := gh.ProjectItem{
		Number:         10,
		Repo:           "owner/repo",
		Labels:         []string{"base:develop"},
		LinkedPRNumber: 55,
	}

	comments := eng.buildReviewBodyComments(item)

	if len(comments) != 1 {
		t.Fatalf("expected 1 synthetic body comment, got %d", len(comments))
	}
	if fetchLinkedPRCalls != 0 {
		t.Errorf("expected FetchLinkedPR NOT called when LinkedPRNumber is already resolved, got %d calls", fetchLinkedPRCalls)
	}
}

// A FetchPRReviews error on the base:<branch> REST fallback must return nil,
// not panic or silently fall back to the (empty) GraphQL-sourced slice.
func TestBuildReviewBodyComments_NonDefaultBase_RESTFetchError_ReturnsNil(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 77, State: "open"}, nil
		},
		fetchPRReviewsFn: func(owner, repo string, prNumber int) ([]gh.PRReview, error) {
			return nil, errors.New("transient API error")
		},
	}
	eng := reviewTestEngine(t, client)
	item := gh.ProjectItem{
		Number:         10,
		Repo:           "owner/repo",
		Labels:         []string{"base:develop"},
		LinkedPRNumber: 0,
	}

	comments := eng.buildReviewBodyComments(item)

	if len(comments) != 0 {
		t.Errorf("expected no comments on FetchPRReviews error, got %v", comments)
	}
}

// buildReviewFeedbackComments combines inline thread comments and review-body
// comments into a single actionable set.
func TestBuildReviewFeedbackComments_CombinesThreadAndBody(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	item := gh.ProjectItem{
		Number: 10,
		Repo:   "owner/repo",
		LinkedPRReviewThreadComments: []gh.Comment{
			{ID: "PRRC_1", DatabaseID: 101, Author: "copilot", Body: "inline finding", ReviewThreadID: "RT_1"},
		},
		LinkedPRReviews: []gh.PRReview{
			{Author: "alice", State: "CHANGES_REQUESTED", Body: "please fix this", DatabaseID: 555},
		},
	}

	comments := eng.buildReviewFeedbackComments(item)

	if len(comments) != 2 {
		t.Fatalf("expected 2 comments (1 thread + 1 body), got %d", len(comments))
	}
}

// currentHeadReviewFeedbackComments includes review-body comments
// unconditionally (R8: no isOutdated signal exists for a body-only review)
// alongside current-head thread comments, excluding outdated ones.
func TestCurrentHeadReviewFeedbackComments_IncludesBodyRegardlessOfOutdated(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	item := gh.ProjectItem{
		Number: 10,
		Repo:   "owner/repo",
		LinkedPRReviewThreadComments: []gh.Comment{
			{ID: "PRRC_stale", DatabaseID: 102, Author: "pruefer", Body: "stale finding", ReviewThreadID: "RT_stale", IsOutdated: true},
		},
		LinkedPRReviews: []gh.PRReview{
			{Author: "alice", State: "CHANGES_REQUESTED", Body: "please fix this", DatabaseID: 555},
		},
	}

	comments := eng.currentHeadReviewFeedbackComments(item)

	if len(comments) != 1 {
		t.Fatalf("expected 1 comment (body only — outdated thread excluded), got %d", len(comments))
	}
	if comments[0].ID != "review-body:555" {
		t.Errorf("expected the remaining comment to be the review body, got %q", comments[0].ID)
	}
}

// (f3) catch-up loop skips dispatchReviewReinvoke when a goroutine is already
// in-flight for the item, and does NOT increment ReviewCycles.
func TestCatchUpLoop_InFlightGuard(t *testing.T) {
	threadComment := gh.Comment{
		ID:             "PRRC_guard_1",
		DatabaseID:     201,
		Author:         "copilot",
		Body:           "Please fix this.",
		ReviewThreadID: "RT_guard_1",
	}

	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{
				ProjectID: "PVT_1",
				Items: []gh.ProjectItem{
					{
						Number: 42,
						ItemID: "PVTI_42",
						Status: "Implement",
						Repo:   "owner/repo",
						Labels: []string{"stage:Implement:complete", "fabrik:yolo"},
					},
				},
			}, nil
		},
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			// Simulate FetchItemDetails populating review thread comments.
			item.LinkedPRReviewThreadComments = []gh.Comment{threadComment}
			return nil
		},
	}

	stgs := []*stages.Stage{
		{Name: "Implement", Order: 1, Prompt: "implement"},
		{Name: "Review", Order: 2, Prompt: "review"},
	}
	eng := testEngineWithStages(t, client, stgs)
	eng.cfg.MaxReviewCycles = 5

	// Pre-populate the store Worker to simulate a goroutine already running.
	// The dispatch guard uses snap.Worker() != nil (Store-backed) so only the
	// Store mutation is needed; no separate inFlight map entry is required.
	eng.store.Apply(itemstate.LocalLockAcquired{
		Repo:       "owner/repo",
		Number:     42,
		User:       "testuser",
		Worker:     &itemstate.WorkerHandle{StageName: "Implement", StartedAt: time.Now()},
		AcquiredAt: time.Now(),
	})

	ctx := context.Background()
	if _, err := eng.poll(ctx); err != nil {
		t.Fatalf("poll: %v", err)
	}
	// Drain any goroutines (none should have been launched, but be safe).
	eng.wg.Wait()

	// ReviewCycles must remain 0 — the inFlight guard must prevent the dispatch.
	snap42, _ := eng.store.Get("owner/repo", 42)
	if snap42.ReviewCycles("Implement") != 0 {
		t.Errorf("ReviewCycles(Implement) = %d; want 0 (dispatch must be suppressed when in-flight)", snap42.ReviewCycles("Implement"))
	}
}

// (g) pauseForReviewTimeout applies labels and posts a comment.
func TestPauseForReviewTimeout(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 10,
		Repo:   "owner/repo",
		Labels: []string{"fabrik:awaiting-review"},
	}
	stage := &stages.Stage{Name: "Review", WaitForReviews: boolPtr(true)}

	eng.pauseForReviewTimeout(board, item, stage)

	// Should have added fabrik:paused and fabrik:awaiting-input
	labelNames := make(map[string]bool)
	for _, call := range client.addLabelCalls {
		labelNames[call.labelName] = true
	}
	if !labelNames["fabrik:paused"] {
		t.Error("expected fabrik:paused label to be added")
	}
	if !labelNames["fabrik:awaiting-input"] {
		t.Error("expected fabrik:awaiting-input label to be added")
	}

	// Should have posted a comment
	if len(client.addCommentCalls) != 1 {
		t.Fatalf("expected 1 comment, got %d", len(client.addCommentCalls))
	}
}

// (h) pauseForReviewCycleLimit applies labels, posts a comment with cycle count.
func TestPauseForReviewCycleLimit(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 10,
		Repo:   "owner/repo",
	}
	stage := &stages.Stage{Name: "Review", WaitForReviews: boolPtr(true)}

	eng.pauseForReviewCycleLimit(board, item, stage, 5, 5)

	// Should have added fabrik:paused and fabrik:awaiting-input
	labelNames := make(map[string]bool)
	for _, call := range client.addLabelCalls {
		labelNames[call.labelName] = true
	}
	if !labelNames["fabrik:paused"] {
		t.Error("expected fabrik:paused label to be added")
	}
	if !labelNames["fabrik:awaiting-input"] {
		t.Error("expected fabrik:awaiting-input label to be added")
	}

	// Should have posted a comment mentioning the cycle count
	if len(client.addCommentCalls) != 1 {
		t.Fatalf("expected 1 comment, got %d", len(client.addCommentCalls))
	}
}

// (i1) ReviewCycles is per-stage: cycles consumed by one stage do not
// reduce the budget for a different stage on the same issue.
func TestReviewCycleCount_PerStageNotPerIssue(t *testing.T) {
	client := &mockGitHubClient{}
	stgs := []*stages.Stage{
		{Name: "Review", Order: 1, Prompt: "review"},
		{Name: "Validate", Order: 2, Prompt: "validate"},
	}
	eng := testEngineWithStages(t, client, stgs)

	// Simulate Review consuming 3 cycles out of 5.
	for i := 0; i < 3; i++ {
		eng.store.Apply(itemstate.ReviewCycleIncremented{Repo: "owner/repo", Number: 10, StageName: "Review"})
	}

	// Before Validate "runs", its counter must be 0 — proving Review's counter
	// did not bleed into Validate's key.
	snapBefore, _ := eng.store.Get("owner/repo", 10)
	if snapBefore.ReviewCycles("Validate") != 0 {
		t.Fatalf("Validate ReviewCycles = %d before Validate runs; want 0 (must be independent)", snapBefore.ReviewCycles("Validate"))
	}

	// Simulate Validate consuming one cycle and verify it uses its own counter
	// without disturbing Review's existing count.
	eng.store.Apply(itemstate.ReviewCycleIncremented{Repo: "owner/repo", Number: 10, StageName: "Validate"})
	snapAfter, _ := eng.store.Get("owner/repo", 10)
	reviewCount := snapAfter.ReviewCycles("Review")
	validateCount := snapAfter.ReviewCycles("Validate")

	if reviewCount != 3 {
		t.Errorf("Review ReviewCycles = %d after Validate increment; want 3", reviewCount)
	}
	if validateCount != 1 {
		t.Errorf("Validate ReviewCycles = %d; want 1 (must be independent of Review cycles)", validateCount)
	}
}

// (i2) clearFailedStage resets only the paused stage's ReviewCycles; a
// different stage's counter on the same issue is unaffected.
func TestClearFailedStage_ReviewCycleCount_ResetsOnlyCurrentStage(t *testing.T) {
	client := &mockGitHubClient{}
	stgs := []*stages.Stage{
		{Name: "Review", Order: 1, Prompt: "review", WaitForReviews: boolPtr(true)},
		{Name: "Validate", Order: 2, Prompt: "validate", WaitForReviews: boolPtr(true)},
	}
	eng := testEngineWithStages(t, client, stgs)
	item := gh.ProjectItem{
		Number: 10,
		Repo:   "owner/repo",
		Labels: []string{"stage:Review:failed", "fabrik:paused"},
	}

	// Simulate both stages having consumed cycles (Review hit limit; Validate consumed 2).
	for i := 0; i < 5; i++ {
		eng.store.Apply(itemstate.ReviewCycleIncremented{Repo: "owner/repo", Number: 10, StageName: "Review"})
	}
	for i := 0; i < 2; i++ {
		eng.store.Apply(itemstate.ReviewCycleIncremented{Repo: "owner/repo", Number: 10, StageName: "Validate"})
	}

	// User manually unpauses Review.
	reviewStage := &stages.Stage{Name: "Review", Order: 1}
	eng.clearFailedStage(item, reviewStage)

	// Review's counter must be reset to 0.
	snapAfter, _ := eng.store.Get("owner/repo", 10)
	afterReview := snapAfter.ReviewCycles("Review")
	afterValidate := snapAfter.ReviewCycles("Validate")

	if afterReview != 0 {
		t.Errorf("Review ReviewCycles = %d after clearFailedStage; want 0", afterReview)
	}
	// Validate's counter must be untouched — it has an independent budget.
	if afterValidate != 2 {
		t.Errorf("Validate ReviewCycles = %d after clearing Review; want 2 (independent)", afterValidate)
	}
}

// --- Bot-reviewer escalation ladder tests ---

// Phase 1: pure-bot outstanding, awaiting-review timeout elapsed, no reprompted label.
// Verifies DELETE+POST review requests, @mention PR comment, and label are applied.
// Gate returns (true, false) — still blocked.
func TestCheckReviewGate_BotPhase1_Reprompts(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	eng.cfg.ReviewWaitTimeout = 5 * time.Minute

	// awaiting-review label was applied 10 minutes ago — 1× timeout elapsed.
	awaitingApplied := time.Now().Add(-10 * time.Minute)
	client.fetchLabelAppliedAtFn = func(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
		if labelName == "fabrik:awaiting-review" {
			return awaitingApplied, nil
		}
		return time.Time{}, nil
	}

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:         10,
		Repo:           "owner/repo",
		LinkedPRNumber: 42,
		Labels:         []string{"fabrik:awaiting-review"},
		LinkedPRReviewRequests: []gh.ReviewRequest{
			{Login: "copilot-pull-request-reviewer", IsBot: true},
		},
		LinkedPRReviews: nil,
	}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true)}

	blocked, timedOut, _, _ := eng.checkReviewGate(board, item, stage)

	if !blocked {
		t.Error("expected still blocked after Phase 1 re-prompt")
	}
	if timedOut {
		t.Error("expected not timedOut after Phase 1 (still blocked)")
	}

	// DeleteReviewRequest + AddReviewRequest should each have been called once.
	if len(client.deleteReviewRequestCalls) != 1 {
		t.Errorf("expected 1 DeleteReviewRequest call, got %d", len(client.deleteReviewRequestCalls))
	}
	if len(client.addReviewRequestCalls) != 1 {
		t.Errorf("expected 1 AddReviewRequest call, got %d", len(client.addReviewRequestCalls))
	}

	// @mention comment should have been posted on the PR.
	if len(client.addCommentCalls) != 1 {
		t.Fatalf("expected 1 PR @mention comment, got %d", len(client.addCommentCalls))
	}
	if client.addCommentCalls[0].issueNumber != 42 {
		t.Errorf("expected comment on PR #42, got #%d", client.addCommentCalls[0].issueNumber)
	}
	// Copilot login must be mentioned as @copilot, not @copilot-pull-request-reviewer.
	if !strings.Contains(client.addCommentCalls[0].body, "@copilot") {
		t.Errorf("expected @copilot in reprompt comment body, got: %q", client.addCommentCalls[0].body)
	}
	if strings.Contains(client.addCommentCalls[0].body, "@copilot-pull-request-reviewer") {
		t.Errorf("reprompt comment must not contain @copilot-pull-request-reviewer, got: %q", client.addCommentCalls[0].body)
	}

	// fabrik:bot-reprompted label should have been applied once (not per-login).
	var foundReprompted bool
	for _, call := range client.addLabelCalls {
		if call.labelName == "fabrik:bot-reprompted" {
			foundReprompted = true
		}
	}
	if !foundReprompted {
		t.Error("expected fabrik:bot-reprompted label to be added")
	}
}

// Phase 1 idempotency: if fabrik:bot-reprompted already present, Phase 1 does not re-fire.
// When the reprompted label is present but not yet timed out, the gate stays blocked silently.
func TestCheckReviewGate_BotPhase1_Idempotent_StillBlocked(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	eng.cfg.ReviewWaitTimeout = 5 * time.Minute

	awaitingApplied := time.Now().Add(-10 * time.Minute)
	repromptedApplied := time.Now().Add(-2 * time.Minute) // 2 min ago — not yet timed out for Phase 2
	client.fetchLabelAppliedAtFn = func(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
		if labelName == "fabrik:awaiting-review" {
			return awaitingApplied, nil
		}
		if labelName == "fabrik:bot-reprompted" {
			return repromptedApplied, nil
		}
		return time.Time{}, nil
	}

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:         10,
		Repo:           "owner/repo",
		LinkedPRNumber: 42,
		Labels:         []string{"fabrik:awaiting-review", "fabrik:bot-reprompted"},
		LinkedPRReviewRequests: []gh.ReviewRequest{
			{Login: "copilot-pull-request-reviewer", IsBot: true},
		},
		LinkedPRReviews: nil,
	}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true)}

	blocked, timedOut, _, _ := eng.checkReviewGate(board, item, stage)

	if !blocked {
		t.Error("expected still blocked between Phase 1 and Phase 2")
	}
	if timedOut {
		t.Error("expected not timedOut between Phase 1 and Phase 2")
	}
	// No new review requests or comments — Phase 1 does not re-fire.
	if len(client.deleteReviewRequestCalls) != 0 {
		t.Errorf("expected no DeleteReviewRequest (Phase 1 idempotency), got %d", len(client.deleteReviewRequestCalls))
	}
	if len(client.addReviewRequestCalls) != 0 {
		t.Errorf("expected no AddReviewRequest (Phase 1 idempotency), got %d", len(client.addReviewRequestCalls))
	}
}

// Phase 2: bot-reprompted label timed out — gate fires (false, true) and cleans up labels.
// pauseForReviewTimeout then detects Phase 2 context and posts a contextual message.
func TestCheckReviewGate_BotPhase2_PausesForHuman(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	eng.cfg.ReviewWaitTimeout = 5 * time.Minute

	awaitingApplied := time.Now().Add(-15 * time.Minute)
	repromptedApplied := time.Now().Add(-10 * time.Minute) // 10 min > 5 min timeout → Phase 2
	client.fetchLabelAppliedAtFn = func(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
		if labelName == "fabrik:awaiting-review" {
			return awaitingApplied, nil
		}
		if labelName == "fabrik:bot-reprompted" {
			return repromptedApplied, nil
		}
		return time.Time{}, nil
	}

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:         10,
		Repo:           "owner/repo",
		LinkedPRNumber: 42,
		Labels:         []string{"fabrik:awaiting-review", "fabrik:bot-reprompted"},
		LinkedPRReviewRequests: []gh.ReviewRequest{
			{Login: "copilot-pull-request-reviewer", IsBot: true},
		},
		LinkedPRReviews: nil,
	}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true)}

	blocked, timedOut, _, _ := eng.checkReviewGate(board, item, stage)

	if blocked {
		t.Error("expected not blocked on Phase 2 (should return false, true)")
	}
	if !timedOut {
		t.Error("expected timedOut == true on Phase 2")
	}

	// fabrik:bot-reprompted and fabrik:awaiting-review should both be removed.
	removedLabels := make(map[string]bool)
	for _, call := range client.removeLabelCalls {
		removedLabels[call.labelName] = true
	}
	if !removedLabels["fabrik:bot-reprompted"] {
		t.Error("expected fabrik:bot-reprompted to be removed in Phase 2")
	}
	if !removedLabels["fabrik:awaiting-review"] {
		t.Error("expected fabrik:awaiting-review to be removed in Phase 2")
	}

	// Verify pauseForReviewTimeout posts a Phase 2 contextual message.
	eng2 := reviewTestEngine(t, client)
	eng2.pauseForReviewTimeout(board, item, stage) // item still has pre-cleanup labels

	var foundPhase2Comment bool
	for _, call := range client.addCommentCalls {
		if len(call.body) > 0 && containsAll(call.body, "after bot re-prompt", "copilot-pull-request-reviewer") {
			foundPhase2Comment = true
		}
	}
	if !foundPhase2Comment {
		t.Error("expected pauseForReviewTimeout to post a Phase 2 contextual message mentioning the bot and re-prompt")
	}
}

// Pure-bot stuck then bot responds before Phase 2 — gate clears naturally.
func TestCheckReviewGate_BotRespondsBeforePhase2_Clears(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	eng.cfg.ReviewWaitTimeout = 5 * time.Minute

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:         10,
		Repo:           "owner/repo",
		LinkedPRNumber: 42,
		Labels:         []string{"fabrik:awaiting-review"},
		// Bot submitted a review and no longer in reviewRequests.
		LinkedPRReviewRequests: nil,
		LinkedPRReviews: []gh.PRReview{
			{Author: "copilot-pull-request-reviewer", State: "COMMENTED"},
		},
	}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true)}

	blocked, timedOut, _, _ := eng.checkReviewGate(board, item, stage)

	if blocked {
		t.Error("expected not blocked when bot submitted a review")
	}
	if timedOut {
		t.Error("expected not timedOut when bot submitted a review")
	}
	// Gate clears naturally — no re-prompt calls.
	if len(client.deleteReviewRequestCalls) != 0 {
		t.Errorf("expected no DeleteReviewRequest when gate clears, got %d", len(client.deleteReviewRequestCalls))
	}
}

// Mixed bot+human outstanding at 1× timeout — existing pause path fires, no re-prompt.
func TestCheckReviewGate_MixedBotHuman_PausesWithoutReprompt(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	eng.cfg.ReviewWaitTimeout = 5 * time.Minute

	awaitingApplied := time.Now().Add(-10 * time.Minute) // 1× elapsed
	client.fetchLabelAppliedAtFn = func(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
		if labelName == "fabrik:awaiting-review" {
			return awaitingApplied, nil
		}
		return time.Time{}, nil
	}

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:         10,
		Repo:           "owner/repo",
		LinkedPRNumber: 42,
		Labels:         []string{"fabrik:awaiting-review"},
		LinkedPRReviewRequests: []gh.ReviewRequest{
			{Login: "copilot-pull-request-reviewer", IsBot: true},
			{Login: "alice", IsBot: false}, // human reviewer present
		},
		LinkedPRReviews: nil,
	}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true)}

	blocked, timedOut, _, _ := eng.checkReviewGate(board, item, stage)

	if blocked {
		t.Error("expected not blocked (timeout elapsed → timedOut)")
	}
	if !timedOut {
		t.Error("expected timedOut == true when mixed outstanding at 1× timeout")
	}
	// Phase 1 must NOT have fired for mixed outstanding.
	if len(client.deleteReviewRequestCalls) != 0 {
		t.Errorf("expected no DeleteReviewRequest for mixed outstanding, got %d", len(client.deleteReviewRequestCalls))
	}
	if len(client.addReviewRequestCalls) != 0 {
		t.Errorf("expected no AddReviewRequest for mixed outstanding, got %d", len(client.addReviewRequestCalls))
	}
	// No bot-reprompted label should have been applied.
	for _, call := range client.addLabelCalls {
		if call.labelName == "fabrik:bot-reprompted" {
			t.Errorf("unexpected bot-reprompted label added for mixed outstanding: %q", call.labelName)
		}
	}
}

// Pure-human outstanding at 1× timeout — existing pause path fires.
func TestCheckReviewGate_PureHuman_PausesWithoutReprompt(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	eng.cfg.ReviewWaitTimeout = 5 * time.Minute

	awaitingApplied := time.Now().Add(-10 * time.Minute)
	client.fetchLabelAppliedAtFn = func(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
		if labelName == "fabrik:awaiting-review" {
			return awaitingApplied, nil
		}
		return time.Time{}, nil
	}

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:         10,
		Repo:           "owner/repo",
		LinkedPRNumber: 42,
		Labels:         []string{"fabrik:awaiting-review"},
		LinkedPRReviewRequests: []gh.ReviewRequest{
			{Login: "alice", IsBot: false},
		},
		LinkedPRReviews: nil,
	}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true)}

	blocked, timedOut, _, _ := eng.checkReviewGate(board, item, stage)

	if blocked {
		t.Error("expected not blocked (timeout elapsed → timedOut)")
	}
	if !timedOut {
		t.Error("expected timedOut == true for pure-human at 1× timeout")
	}
	if len(client.deleteReviewRequestCalls) != 0 {
		t.Errorf("expected no DeleteReviewRequest for pure-human, got %d", len(client.deleteReviewRequestCalls))
	}
}

// pauseForReviewTimeout enhanced comment lists reviewers with bot/human tags.
func TestPauseForReviewTimeout_ListsReviewerTypes(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 10,
		Repo:   "owner/repo",
		Labels: []string{"fabrik:awaiting-review"},
		LinkedPRReviewRequests: []gh.ReviewRequest{
			{Login: "copilot-pull-request-reviewer", IsBot: true},
			{Login: "alice", IsBot: false},
		},
	}
	stage := &stages.Stage{Name: "Review", WaitForReviews: boolPtr(true)}

	eng.pauseForReviewTimeout(board, item, stage)

	if len(client.addCommentCalls) != 1 {
		t.Fatalf("expected 1 comment, got %d", len(client.addCommentCalls))
	}
	body := client.addCommentCalls[0].body
	if !containsAll(body, "copilot-pull-request-reviewer", "bot", "alice", "human") {
		t.Errorf("pause comment should list reviewers with bot/human tags; got:\n%s", body)
	}
	// Regression guard: when reviewers were requested, the message must keep
	// its existing "outstanding reviewers" wording and must not bleed into
	// the no-reviewers-requested remedies text (#1268).
	if !strings.Contains(body, "outstanding reviewers") {
		t.Errorf("pause comment should still say 'outstanding reviewers' when reviewers were requested; got:\n%s", body)
	}
	if strings.Contains(body, "wait_for_reviews: false") {
		t.Errorf("pause comment should not include the no-reviewers-requested remedies when reviewers were requested; got:\n%s", body)
	}
}

// On a timeout where no reviewer was ever requested, the pause comment must
// not claim to have waited on "outstanding reviewers" and must list the five
// remedies, including the COMMENTED self-review hatch (#1268) and the
// expected_reviewers: [] declaration (#1283, #1334).
func TestPauseForReviewTimeout_NoReviewersRequested_ListsRemedies(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:         10,
		Repo:           "owner/repo",
		Labels:         []string{"fabrik:awaiting-review"},
		LinkedPRNumber: 42,
	}
	stage := &stages.Stage{Name: "Review", WaitForReviews: boolPtr(true)}

	eng.pauseForReviewTimeout(board, item, stage)

	if len(client.addCommentCalls) != 1 {
		t.Fatalf("expected 1 comment, got %d", len(client.addCommentCalls))
	}
	body := client.addCommentCalls[0].body
	if strings.Contains(body, "outstanding reviewers") {
		t.Errorf("pause comment must not claim to wait on outstanding reviewers when none were requested; got:\n%s", body)
	}
	if !containsAll(body, "COMMENTED", "self-review", "expected_reviewers: []", "wait_for_reviews: false", "merge", "fabrik:paused") {
		t.Errorf("pause comment should list the five remedies including the COMMENTED self-review hatch and expected_reviewers: []; got:\n%s", body)
	}
	// FR-2: expected_reviewers: [] and wait_for_reviews: false must be
	// distinguished, not merged into one recommendation — the former narrows
	// waiting for unrequested reviewers while honoring an explicit request,
	// the latter disables the gate entirely.
	if !strings.Contains(body, "entirely") {
		t.Errorf("pause comment should distinguish wait_for_reviews: false as disabling the gate entirely; got:\n%s", body)
	}
	// FR-3: the old "cannot determine whether one is ever coming" framing
	// asserted an inherent limit that expected_reviewers (#1283) removed —
	// it must be replaced with declarable-not-unknowable framing.
	if strings.Contains(body, "cannot determine whether one is ever coming") {
		t.Errorf("pause comment must not claim Fabrik cannot determine whether a reviewer is coming — this is now declarable via expected_reviewers; got:\n%s", body)
	}
	if !strings.Contains(body, "declarable") {
		t.Errorf("pause comment should note that whether a reviewer is coming is declarable; got:\n%s", body)
	}
	// Advisory mode (no ReviewAuthority set): a self-COMMENT is known to
	// satisfy the gate outright, so the authoritative-mode caveat on remedy
	// (a) must not appear — stating it here would assert something Fabrik
	// doesn't actually know to be relevant (#1268 review thread).
	if strings.Contains(body, "another account") {
		t.Errorf("pause comment must not qualify the self-review remedy in advisory mode; got:\n%s", body)
	}
}

// FR-4: multiple declared reviewers with a partial response — the pause
// message must report per-reviewer status so the gap is diagnosable.
func TestPauseForReviewTimeout_ExpectedReviewers_PartialResponse_ReportsPerReviewerStatus(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:         10,
		Repo:           "owner/repo",
		LinkedPRNumber: 42,
		Labels:         []string{"fabrik:awaiting-review"},
		LinkedPRReviews: []gh.PRReview{
			{Author: "handarbeit-pruefer[bot]", State: "COMMENTED"},
		},
	}
	declared := []string{"handarbeit-pruefer", "gemini-code-assist"}
	stage := &stages.Stage{Name: "Review", WaitForReviews: boolPtr(true), ExpectedReviewers: &declared}

	eng.pauseForReviewTimeout(board, item, stage)

	if len(client.addCommentCalls) != 1 {
		t.Fatalf("expected 1 comment, got %d", len(client.addCommentCalls))
	}
	body := client.addCommentCalls[0].body
	if !containsAll(body, "`handarbeit-pruefer` reviewed", "`gemini-code-assist` did not respond") {
		t.Errorf("pause comment should report per-reviewer status; got:\n%s", body)
	}
}

// FR-4/FR-6: a declared reviewer that's never installed still times out, naming
// the bot — the message must not imply "no reviewer was ever requested" (the
// stale, pre-declaration framing this issue supersedes).
func TestPauseForReviewTimeout_ExpectedReviewers_NeverResponds_NamesBot(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:                 10,
		Repo:                   "owner/repo",
		LinkedPRNumber:         42,
		Labels:                 []string{"fabrik:awaiting-review"},
		LinkedPRReviewRequests: nil,
		LinkedPRReviews:        nil,
	}
	declared := []string{"nonexistent-reviewer"}
	stage := &stages.Stage{Name: "Review", WaitForReviews: boolPtr(true), ExpectedReviewers: &declared}

	eng.pauseForReviewTimeout(board, item, stage)

	body := client.addCommentCalls[0].body
	if !strings.Contains(body, "`nonexistent-reviewer` did not respond") {
		t.Errorf("pause comment should name the absent declared reviewer; got:\n%s", body)
	}
}

// FR-4: the Phase 2 "(after bot re-prompt)" message flavor must also fire for
// a declared-but-unrequested reviewer, not just a formally requested one —
// LinkedPRReviewRequests is always empty for a declared-only reviewer, so the
// Phase 2 detection must fold in declaredOutstandingNames too.
func TestPauseForReviewTimeout_ExpectedReviewers_Phase2_UsesReprompFlavor(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:                 10,
		Repo:                   "owner/repo",
		LinkedPRNumber:         42,
		Labels:                 []string{"fabrik:awaiting-review", "fabrik:bot-reprompted"},
		LinkedPRReviewRequests: nil, // declared-only reviewer never appears here
		LinkedPRReviews:        nil,
	}
	declared := []string{"handarbeit-pruefer"}
	stage := &stages.Stage{Name: "Review", WaitForReviews: boolPtr(true), ExpectedReviewers: &declared}

	eng.pauseForReviewTimeout(board, item, stage)

	body := client.addCommentCalls[0].body
	if !strings.Contains(body, "after bot re-prompt") {
		t.Errorf("expected the Phase 2 re-prompt message flavor to fire for a declared reviewer; got:\n%s", body)
	}
	if !strings.Contains(body, "handarbeit-pruefer") {
		t.Errorf("expected the declared reviewer to be named in the Phase 2 message; got:\n%s", body)
	}
}

// Authoritative-mode pause messaging must cover REVIEW_REQUIRED, not just an
// active CHANGES_REQUESTED review — it live-fetches the verdict via
// reviewGateAuthorityVerdict rather than scanning only for CHANGES_REQUESTED.
func TestPauseForReviewTimeout_Authoritative_ReviewRequired_MentionsVerdict(t *testing.T) {
	client := &mockGitHubClient{
		fetchPRReviewDecisionFn: func(owner, repo string, prNumber int) (string, error) {
			if prNumber != 77 {
				t.Errorf("expected FetchPRReviewDecision called with PR #77, got #%d", prNumber)
			}
			return "REVIEW_REQUIRED", nil
		},
	}
	eng := reviewTestEngine(t, client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:         10,
		Repo:           "owner/repo",
		Labels:         []string{"fabrik:awaiting-review"},
		LinkedPRNumber: 77,
	}
	stage := &stages.Stage{Name: "Review", WaitForReviews: boolPtr(true), ReviewAuthority: "authoritative"}

	eng.pauseForReviewTimeout(board, item, stage)

	if len(client.addCommentCalls) != 1 {
		t.Fatalf("expected 1 comment, got %d", len(client.addCommentCalls))
	}
	body := client.addCommentCalls[0].body
	if !strings.Contains(body, "REVIEW_REQUIRED") {
		t.Errorf("expected pause comment to mention the REVIEW_REQUIRED verdict, got:\n%s", body)
	}
	// This is the exact scenario where advice (a)'s unqualified "a COMMENTED
	// self-review satisfies the gate" would be actively wrong: branch
	// protection requires an approving review, and a self-COMMENT (GitHub
	// forbids self-approval) can never produce one (#1268 review thread).
	if !strings.Contains(body, "another account") {
		t.Errorf("pause comment must qualify the self-review remedy when authoritative mode requires approving reviews, got:\n%s", body)
	}
}

// A FetchPRReviewDecision fetch error in the pause path must be surfaced in
// the pause comment rather than silently falling back to the generic
// "timed out waiting for outstanding reviewers" text.
func TestPauseForReviewTimeout_Authoritative_FetchError_MentionsUnreadableVerdict(t *testing.T) {
	client := &mockGitHubClient{
		fetchPRReviewDecisionFn: func(owner, repo string, prNumber int) (string, error) {
			return "", fmt.Errorf("transient GraphQL failure")
		},
	}
	eng := reviewTestEngine(t, client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:         10,
		Repo:           "owner/repo",
		Labels:         []string{"fabrik:awaiting-review"},
		LinkedPRNumber: 77,
	}
	stage := &stages.Stage{Name: "Review", WaitForReviews: boolPtr(true), ReviewAuthority: "authoritative"}

	eng.pauseForReviewTimeout(board, item, stage)

	if len(client.addCommentCalls) != 1 {
		t.Fatalf("expected 1 comment, got %d", len(client.addCommentCalls))
	}
	body := client.addCommentCalls[0].body
	if !strings.Contains(body, "could not be") {
		t.Errorf("expected pause comment to mention the unreadable verdict, got:\n%s", body)
	}
	// The verdict is unknown, not confirmed non-blocking — Fabrik can't rule
	// out that an approval requirement exists, so the self-review caveat
	// must still be shown (#1268 review thread).
	if !strings.Contains(body, "another account") {
		t.Errorf("pause comment must qualify the self-review remedy when the verdict is unreadable, got:\n%s", body)
	}
}

// Authoritative mode with a confirmed non-blocking verdict (no branch
// protection review requirement configured) is the one case where Fabrik
// positively knows a self-review will satisfy the gate — the caveat on
// remedy (a) must not appear here, otherwise the message asserts an
// authoritative-approval requirement that doesn't exist (#1268 review
// thread).
func TestPauseForReviewTimeout_Authoritative_NoRequirement_NoUnqualifiedCaveat(t *testing.T) {
	client := &mockGitHubClient{
		fetchPRReviewDecisionFn: func(owner, repo string, prNumber int) (string, error) {
			return "", nil // no branch-protection review requirement configured
		},
	}
	eng := reviewTestEngine(t, client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:         10,
		Repo:           "owner/repo",
		Labels:         []string{"fabrik:awaiting-review"},
		LinkedPRNumber: 77,
	}
	stage := &stages.Stage{Name: "Review", WaitForReviews: boolPtr(true), ReviewAuthority: "authoritative"}

	eng.pauseForReviewTimeout(board, item, stage)

	if len(client.addCommentCalls) != 1 {
		t.Fatalf("expected 1 comment, got %d", len(client.addCommentCalls))
	}
	body := client.addCommentCalls[0].body
	if strings.Contains(body, "another account") {
		t.Errorf("pause comment must not qualify the self-review remedy when authoritative mode has no active review requirement, got:\n%s", body)
	}
}

// On a base:<branch> repo, LinkedPRNumber/LinkedPRReviews are always 0/empty
// (closedByPullRequestsReferences is structurally empty there) — the pause
// message must still resolve the real PR via the same REST fallback
// checkReviewGate/reviewGateBlocksLanding use, rather than silently omitting
// the authority explanation the way a naive item.LinkedPRNumber > 0 guard
// would.
func TestPauseForReviewTimeout_Authoritative_NonDefaultBase_MentionsVerdict(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 77, State: "open"}, nil
		},
		fetchPRReviewsFn: func(owner, repo string, prNumber int) ([]gh.PRReview, error) {
			return []gh.PRReview{{Author: "bob", State: "CHANGES_REQUESTED"}}, nil
		},
		fetchPRReviewDecisionFn: func(owner, repo string, prNumber int) (string, error) {
			if prNumber != 77 {
				t.Errorf("expected FetchPRReviewDecision called with resolved PR #77, got #%d", prNumber)
			}
			return "", nil // no branch protection configured — exercises the fallback
		},
	}
	eng := reviewTestEngine(t, client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:         10,
		Repo:           "owner/repo",
		Labels:         []string{"fabrik:awaiting-review", "base:develop"},
		LinkedPRNumber: 0,
	}
	stage := &stages.Stage{Name: "Review", WaitForReviews: boolPtr(true), ReviewAuthority: "authoritative"}

	eng.pauseForReviewTimeout(board, item, stage)

	if len(client.addCommentCalls) != 1 {
		t.Fatalf("expected 1 comment, got %d", len(client.addCommentCalls))
	}
	body := client.addCommentCalls[0].body
	if !strings.Contains(body, "bob requested changes") {
		t.Errorf("expected pause comment to mention the outstanding CHANGES_REQUESTED verdict resolved via the base:<branch> REST fallback, got:\n%s", body)
	}
	// Regression guard (#1268 review thread): bob submitted a review, so
	// pendingLine is "" (he's no longer a requested reviewer) but a review
	// did happen — the comment must not claim otherwise.
	if strings.Contains(body, "no review has been submitted") || strings.Contains(body, "No reviewer was ever requested") {
		t.Errorf("pause comment must not claim no review was submitted when bob's CHANGES_REQUESTED review exists, got:\n%s", body)
	}
}

// A requested reviewer who submits a formal review (e.g. CHANGES_REQUESTED)
// is dropped from GitHub's requested-reviewers list, so pendingLine goes back
// to "" even though a review was submitted. This is the default-branch
// GraphQL path (LinkedPRReviews populated directly, no REST fallback) — the
// no-reviewers-requested branch must not fire here, since a review did
// happen and the message would otherwise contradict the appended
// authorityLine quoting that same review's verdict (#1268 review thread).
func TestPauseForReviewTimeout_Authoritative_ReviewerRespondedButNotPending_NoFalseClaim(t *testing.T) {
	client := &mockGitHubClient{
		fetchPRReviewDecisionFn: func(owner, repo string, prNumber int) (string, error) {
			if prNumber != 77 {
				t.Errorf("expected FetchPRReviewDecision called with PR #77, got #%d", prNumber)
			}
			return "", nil // no branch protection configured — exercises Fabrik's own fallback
		},
	}
	eng := reviewTestEngine(t, client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:         10,
		Repo:           "owner/repo",
		Labels:         []string{"fabrik:awaiting-review"},
		LinkedPRNumber: 77,
		// LinkedPRReviewRequests intentionally empty: bob already submitted
		// and is no longer outstanding.
		LinkedPRReviews: []gh.PRReview{{Author: "bob", State: "CHANGES_REQUESTED"}},
	}
	stage := &stages.Stage{Name: "Review", WaitForReviews: boolPtr(true), ReviewAuthority: "authoritative"}

	eng.pauseForReviewTimeout(board, item, stage)

	if len(client.addCommentCalls) != 1 {
		t.Fatalf("expected 1 comment, got %d", len(client.addCommentCalls))
	}
	body := client.addCommentCalls[0].body
	if !strings.Contains(body, "bob requested changes") {
		t.Errorf("expected pause comment to mention the outstanding CHANGES_REQUESTED verdict, got:\n%s", body)
	}
	if strings.Contains(body, "no review has been submitted") || strings.Contains(body, "No reviewer was ever requested") {
		t.Errorf("pause comment must not claim no review was submitted when bob's CHANGES_REQUESTED review exists, got:\n%s", body)
	}
	if strings.Contains(body, "wait_for_reviews: false") {
		t.Errorf("pause comment should not offer the no-reviewer remedies when a review was actually submitted, got:\n%s", body)
	}
	// bob is no longer outstanding (he already responded), so the standard
	// "waiting for outstanding reviewers" wording is also false here — a
	// distinct claim from the two checked above (#1268 review thread,
	// follow-up finding).
	if strings.Contains(body, "outstanding reviewers") {
		t.Errorf("pause comment must not claim to wait on outstanding reviewers when bob already responded and nobody is pending, got:\n%s", body)
	}
	if !strings.Contains(body, "No reviewer is currently outstanding") {
		t.Errorf("pause comment should state plainly that nobody is currently outstanding, got:\n%s", body)
	}
}

// On a base:<branch> repo in authoritative mode, item.LinkedPRReviews is
// structurally empty (see comment on the REST-fallback block in
// pauseForReviewTimeout), so hasReviews must be resolved via a live
// FetchPRReviews call. If that call fails, the failure must not be silently
// swallowed into a stale hasReviews=false reading — that would trip the
// no-reviewers-requested branch and falsely claim "no review has been
// submitted" even though review state is actually unknown. The gate must
// fail conservatively, exactly as checkReviewGate's own REST-fallback does
// (#1268 review thread).
func TestPauseForReviewTimeout_Authoritative_NonDefaultBase_ReviewFetchFails_NoFalseClaim(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 77, State: "open"}, nil
		},
		fetchPRReviewsFn: func(owner, repo string, prNumber int) ([]gh.PRReview, error) {
			return nil, fmt.Errorf("simulated transient GitHub API failure")
		},
		fetchPRReviewDecisionFn: func(owner, repo string, prNumber int) (string, error) {
			return "", nil // no branch protection configured — exercises Fabrik's own fallback
		},
	}
	eng := reviewTestEngine(t, client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:         10,
		Repo:           "owner/repo",
		Labels:         []string{"fabrik:awaiting-review", "base:develop"},
		LinkedPRNumber: 0, // structurally empty on a base:<branch> item
	}
	stage := &stages.Stage{Name: "Review", WaitForReviews: boolPtr(true), ReviewAuthority: "authoritative"}

	eng.pauseForReviewTimeout(board, item, stage)

	if len(client.addCommentCalls) != 1 {
		t.Fatalf("expected 1 comment, got %d", len(client.addCommentCalls))
	}
	body := client.addCommentCalls[0].body
	if strings.Contains(body, "No reviewer was ever requested") || strings.Contains(body, "no review has been submitted") {
		t.Errorf("pause comment must not assert no review was submitted when the review fetch itself failed, got:\n%s", body)
	}
	if strings.Contains(body, "wait_for_reviews: false") {
		t.Errorf("pause comment should not offer the no-reviewer remedies when review state could not be determined, got:\n%s", body)
	}
	// Deliberate fallthrough: hasReviews is forced true defensively here (the
	// fetch failed, so review state is unknown-but-possibly-present), but
	// authorityLine is "" because the separate, successful reviewDecision
	// fetch was evaluated against that same stale, empty review list and
	// came back satisfied. The pendingLine=="" && hasReviews branch requires
	// authorityLine != "" precisely so it never asserts a submitted review
	// this ambiguous state can't actually confirm — it falls through to the
	// standard message instead, an existing imprecision this change doesn't
	// widen (#1268 review thread, follow-up finding).
	if !strings.Contains(body, "outstanding reviewers") {
		t.Errorf("expected the standard fallback wording when review state is ambiguous, got:\n%s", body)
	}
}

// removeAwaitingReviewLabel also removes the fabrik:bot-reprompted label.
func TestRemoveAwaitingReviewLabel_CleansRepromptedLabels(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	item := gh.ProjectItem{
		Number: 10,
		Repo:   "owner/repo",
		Labels: []string{"fabrik:awaiting-review", "fabrik:bot-reprompted"},
	}

	eng.removeAwaitingReviewLabel("owner", "repo", item)

	removedLabels := make(map[string]bool)
	for _, call := range client.removeLabelCalls {
		removedLabels[call.labelName] = true
	}
	if !removedLabels["fabrik:awaiting-review"] {
		t.Error("expected fabrik:awaiting-review to be removed")
	}
	if !removedLabels["fabrik:bot-reprompted"] {
		t.Error("expected fabrik:bot-reprompted to be removed")
	}
}

// TestRemoveAwaitingReviewLabel_ErrNotFound verifies fix #4 (issue #1024): a 404
// from RemoveLabelFromIssue must be treated as success (label already absent on
// GitHub) and the cache synced accordingly — mirroring removeAwaitingCILabel's
// ErrNotFound branch exactly. Before the fix, an ErrNotFound just logged a
// warning and left the cache believing the label was still present.
//
// The item is constructed with an empty Repo (relying on the defaultRepo()
// fallback to "owner/repo") so the item.Repo-vs-resolved-owner/repo cache key
// mismatch is also observable here — a test using an explicit non-empty
// item.Repo would not catch that bug class.
func TestRemoveAwaitingReviewLabel_ErrNotFound(t *testing.T) {
	var calls int
	client := &mockGitHubClient{
		removeLabelFromIssueFn: func(owner, repo string, issueNumber int, labelName string) error {
			if labelName == "fabrik:awaiting-review" {
				calls++
				return fmt.Errorf("GitHub API returned 404: label not found: %w", gh.ErrNotFound)
			}
			return nil
		},
	}
	eng := reviewTestEngine(t, client)
	cache := boardcache.NewCacheImpl(client, eng.store, func(string, ...any) {})
	cache.BootstrapFromProbe([]gh.BoardProbeItem{
		{ContentID: "I_010", ItemID: "PVTI_010", Number: 10, Repo: "owner/repo", Status: "Review"},
	}, "PVT_1")
	cache.ApplyLabelAdded(boardcache.ItemKey("owner/repo", 10), "fabrik:awaiting-review")
	eng.readClient = cache

	item := gh.ProjectItem{
		Number: 10,
		Labels: []string{"fabrik:awaiting-review"},
	}

	eng.removeAwaitingReviewLabel("owner", "repo", item)

	if calls != 1 {
		t.Errorf("expected exactly 1 RemoveLabelFromIssue call for ErrNotFound, got %d", calls)
	}

	labels, err := cache.FetchLabels("owner", "repo", 10)
	if err != nil {
		t.Fatalf("FetchLabels: %v", err)
	}
	for _, l := range labels {
		if l == "fabrik:awaiting-review" {
			t.Error("expected fabrik:awaiting-review to be removed from cache after ErrNotFound — cache is stuck believing the label is still present")
		}
	}
}

// TestBotRepromptedLabelLength guards against the botRepromptedLabel constant
// exceeding GitHub's 50-character REST API limit for label names.
func TestBotRepromptedLabelLength(t *testing.T) {
	if len(botRepromptedLabel) > 50 {
		t.Errorf("botRepromptedLabel is %d chars (max 50): %q", len(botRepromptedLabel), botRepromptedLabel)
	}
}

// containsAll checks that all substrings appear in s (case-sensitive).
func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

// boolPtr is a helper to create a *bool from a bool literal.
func boolPtr(b bool) *bool {
	return &b
}

func TestBotMentionHandle(t *testing.T) {
	cases := []struct {
		login string
		want  string
	}{
		{"copilot-pull-request-reviewer", "copilot"},
		{"copilot", "copilot"},
		{"Copilot-pull-request-reviewer", "copilot"},
		{"dependabot[bot]", "dependabot[bot]"},
		{"someuser", "someuser"},
	}
	for _, tc := range cases {
		got := botMentionHandle(tc.login)
		if got != tc.want {
			t.Errorf("botMentionHandle(%q) = %q, want %q", tc.login, got, tc.want)
		}
	}
}

// Phase 1: non-Copilot bot reviewer — reprompt comment must mention @<login> directly.
func TestCheckReviewGate_BotPhase1_NonCopilot_MentionsLogin(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	eng.cfg.ReviewWaitTimeout = 5 * time.Minute

	awaitingApplied := time.Now().Add(-10 * time.Minute)
	client.fetchLabelAppliedAtFn = func(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
		if labelName == "fabrik:awaiting-review" {
			return awaitingApplied, nil
		}
		return time.Time{}, nil
	}

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:         10,
		Repo:           "owner/repo",
		LinkedPRNumber: 42,
		Labels:         []string{"fabrik:awaiting-review"},
		LinkedPRReviewRequests: []gh.ReviewRequest{
			{Login: "dependabot[bot]", IsBot: true},
		},
		LinkedPRReviews: nil,
	}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true)}

	eng.checkReviewGate(board, item, stage)

	if len(client.addCommentCalls) != 1 {
		t.Fatalf("expected 1 PR @mention comment, got %d", len(client.addCommentCalls))
	}
	body := client.addCommentCalls[0].body
	if !strings.Contains(body, "@dependabot[bot]") {
		t.Errorf("expected @dependabot[bot] in reprompt comment body, got: %q", body)
	}
}

// --- expected_reviewers (declared unrequested reviewers) pure-function tests ---

func TestStripBotSuffix(t *testing.T) {
	cases := []struct{ login, want string }{
		{"handarbeit-pruefer[bot]", "handarbeit-pruefer"},
		{"handarbeit-pruefer[BOT]", "handarbeit-pruefer"},
		{"handarbeit-pruefer[Bot]", "handarbeit-pruefer"},
		{"handarbeit-pruefer", "handarbeit-pruefer"},
		{"copilot-pull-request-reviewer[bot]", "copilot-pull-request-reviewer"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := stripBotSuffix(tc.login); got != tc.want {
			t.Errorf("stripBotSuffix(%q) = %q, want %q", tc.login, got, tc.want)
		}
	}
}

func TestReviewerIdentityMatches(t *testing.T) {
	cases := []struct {
		declared, author string
		want             bool
	}{
		// Real-world Pruefer shape: declared without suffix, REST-sourced author with it.
		{"handarbeit-pruefer", "handarbeit-pruefer[bot]", true},
		// GraphQL-sourced author never carries the suffix.
		{"handarbeit-pruefer", "handarbeit-pruefer", true},
		// Case-insensitive.
		{"Handarbeit-Pruefer", "handarbeit-pruefer[bot]", true},
		// Copilot: declared canonical mention handle vs. the real login.
		{"copilot", "copilot-pull-request-reviewer[bot]", true},
		{"copilot", "copilot-pull-request-reviewer", true},
		// Unrelated identities must not match.
		{"handarbeit-pruefer", "gemini-code-assist", false},
		{"handarbeit-pruefer", "handarbeit-pruefer2[bot]", false},
	}
	for _, tc := range cases {
		if got := reviewerIdentityMatches(tc.declared, tc.author); got != tc.want {
			t.Errorf("reviewerIdentityMatches(%q, %q) = %v, want %v", tc.declared, tc.author, got, tc.want)
		}
	}
}

func TestDeclaredReviewersOutstanding(t *testing.T) {
	t.Run("no reviews — all declared outstanding", func(t *testing.T) {
		got := declaredReviewersOutstanding([]string{"handarbeit-pruefer", "gemini-code-assist"}, nil)
		want := []string{"handarbeit-pruefer", "gemini-code-assist"}
		if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("one declared reviewer responds — only the other remains outstanding", func(t *testing.T) {
		reviews := []gh.PRReview{
			{Author: "handarbeit-pruefer[bot]", State: "COMMENTED"},
		}
		got := declaredReviewersOutstanding([]string{"handarbeit-pruefer", "gemini-code-assist"}, reviews)
		if len(got) != 1 || got[0] != "gemini-code-assist" {
			t.Errorf("got %v, want [gemini-code-assist]", got)
		}
	})

	t.Run("DISMISSED review does not count as a response", func(t *testing.T) {
		reviews := []gh.PRReview{
			{Author: "handarbeit-pruefer[bot]", State: "DISMISSED"},
		}
		got := declaredReviewersOutstanding([]string{"handarbeit-pruefer"}, reviews)
		if len(got) != 1 || got[0] != "handarbeit-pruefer" {
			t.Errorf("got %v, want [handarbeit-pruefer] (DISMISSED must not clear)", got)
		}
	})

	t.Run("empty declared list returns nil", func(t *testing.T) {
		if got := declaredReviewersOutstanding(nil, []gh.PRReview{{Author: "x", State: "APPROVED"}}); got != nil {
			t.Errorf("got %v, want nil", got)
		}
	})
}

func TestReviewGateFastAdvance(t *testing.T) {
	none := []string{}
	declared := []string{"handarbeit-pruefer"}
	cases := []struct {
		name        string
		outstanding []string
		hasReviews  bool
		expected    *[]string
		want        bool
	}{
		{"undeclared stage never fast-advances", nil, false, nil, false},
		{"declared none, nothing requested, nothing reviewed — advances", nil, false, &none, true},
		{"declared none but a reviewer is requested — still waits", []string{"alice"}, false, &none, false},
		{"declared none but a review already exists — not the fast path's job", nil, true, &none, false},
		{"declared reviewers (non-empty) never fast-advance", nil, false, &declared, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := reviewGateFastAdvance(tc.outstanding, tc.hasReviews, tc.expected); got != tc.want {
				t.Errorf("reviewGateFastAdvance(%v, %v, %v) = %v, want %v", tc.outstanding, tc.hasReviews, tc.expected, got, tc.want)
			}
		})
	}
}

func TestReviewGateAllBots_DeclaredBranch(t *testing.T) {
	t.Run("outstanding empty, declared reviewer outstanding — allBots true", func(t *testing.T) {
		if !reviewGateAllBots(nil, nil, []string{"handarbeit-pruefer"}) {
			t.Error("expected allBots true when a declared reviewer is still outstanding")
		}
	})
	t.Run("outstanding empty, nothing declared outstanding — allBots false", func(t *testing.T) {
		if reviewGateAllBots(nil, nil, nil) {
			t.Error("expected allBots false when nothing is outstanding at all")
		}
	})
	t.Run("outstanding non-empty ignores declaredOutstanding — unchanged behavior", func(t *testing.T) {
		requests := []gh.ReviewRequest{{Login: "alice", IsBot: false}}
		if reviewGateAllBots(requests, []string{"alice"}, []string{"handarbeit-pruefer"}) {
			t.Error("expected allBots false — a human requested reviewer must not be masked by a declared bot")
		}
		requests = []gh.ReviewRequest{{Login: "some-bot", IsBot: true}}
		if !reviewGateAllBots(requests, []string{"some-bot"}, nil) {
			t.Error("expected allBots true — requested-reviewer classification unchanged when nothing is declared")
		}
	})
}

// --- expected_reviewers integration tests (checkReviewGate) ---

// FR-2: nothing requested, nothing reviewed, expected_reviewers: [] declared — gate advances immediately.
func TestCheckReviewGate_ExpectedReviewersNone_AdvancesImmediately(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:                 10,
		Repo:                   "owner/repo",
		LinkedPRNumber:         42,
		LinkedPRReviewRequests: nil,
		LinkedPRReviews:        nil,
	}
	none := []string{}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true), ExpectedReviewers: &none}

	blocked, timedOut, terminated, _ := eng.checkReviewGate(board, item, stage)

	if blocked || timedOut || terminated {
		t.Errorf("expected (false, false, false) fast-advance, got (%v, %v, %v)", blocked, timedOut, terminated)
	}
	if len(client.addLabelCalls) != 0 {
		t.Errorf("expected no fabrik:awaiting-review label applied on fast-advance, got %d label add(s)", len(client.addLabelCalls))
	}
}

// FR-2 exception: expected_reviewers: [] still waits for a reviewer actually requested on the PR.
func TestCheckReviewGate_ExpectedReviewersNone_ReviewerRequested_StillWaits(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:         10,
		Repo:           "owner/repo",
		LinkedPRNumber: 42,
		LinkedPRReviewRequests: []gh.ReviewRequest{
			{Login: "alice", IsBot: false},
		},
		LinkedPRReviews: nil,
	}
	none := []string{}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true), ExpectedReviewers: &none}

	blocked, timedOut, _, _ := eng.checkReviewGate(board, item, stage)

	if !blocked {
		t.Error("expected still blocked — a requested reviewer takes precedence over expected_reviewers: []")
	}
	if timedOut {
		t.Error("expected not timedOut on first evaluation")
	}
}

// The FR-2 fast path fires regardless of review_authority: with nothing
// requested, nothing reviewed, and expected_reviewers: [] declared, there is
// no verdict for authoritative mode to weigh yet (reviewGateAuthorityVerdict
// only ever runs once hasReviews is true) — gating the advance on
// review_authority would just wait out the timeout for a review the
// declaration says will never arrive. FetchPRReviewDecision must not be
// called on this path.
func TestCheckReviewGate_ExpectedReviewersNone_AuthoritativeMode_StillAdvancesImmediately(t *testing.T) {
	client := &mockGitHubClient{
		fetchPRReviewDecisionFn: func(owner, repo string, prNumber int) (string, error) {
			t.Fatal("FetchPRReviewDecision must not be called when nothing has been reviewed yet")
			return "", nil
		},
	}
	eng := reviewTestEngine(t, client)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:                 10,
		Repo:                   "owner/repo",
		LinkedPRNumber:         42,
		LinkedPRReviewRequests: nil,
		LinkedPRReviews:        nil,
	}
	none := []string{}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true), ExpectedReviewers: &none, ReviewAuthority: "authoritative"}

	blocked, timedOut, terminated, _ := eng.checkReviewGate(board, item, stage)

	if blocked || timedOut || terminated {
		t.Errorf("expected (false, false, false) fast-advance under authoritative mode, got (%v, %v, %v)", blocked, timedOut, terminated)
	}
}

// Regression test: the FR-2 fast path must not fire when no PR has been
// resolved at all. handleBrokenReviewLinkage returns (paused=false,
// prNumber=0) — falling through to normal gate logic rather than pausing —
// whenever FetchLinkedPR errors, finds nothing on the branch, or finds a PR
// that isn't open/is merged (see TestCheckReviewGate_BrokenLinkage_
// NoPRFound_FallsThrough, which exercises the identical no-PR shape). In that
// state outstanding/hasReviews read empty/false simply because there is no PR
// to read yet, not because a real PR genuinely has nothing requested or
// reviewed. Without the prNumber > 0 guard, a stage declaring
// expected_reviewers: [] would fast-advance past a not-yet-resolved PR
// entirely — the same fail-open shape the !fetchFailed guard exists to
// prevent, just reached via a different route (no PR at all, rather than a
// failed fetch for a PR that does exist).
func TestCheckReviewGate_ExpectedReviewersNone_NoPRResolved_DoesNotFastAdvance(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return nil, nil // no PR on branch yet
		},
	}
	eng := reviewTestEngine(t, client)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:         10,
		Repo:           "owner/repo",
		LinkedPRNumber: 0,
	}
	none := []string{}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true), ExpectedReviewers: &none}

	blocked, _, terminated, _ := eng.checkReviewGate(board, item, stage)

	if terminated {
		t.Error("expected not terminated — no PR found should fall through to normal logic, not pause")
	}
	if !blocked {
		t.Error("expected still blocked — the fast path must not fire when no PR has been resolved (prNumber == 0)")
	}
}

// FR-3/acceptance: the exact real-world shape (empty LinkedPRReviewRequests, declared
// identity) reaches Phase 1, posts an @mention comment directly, applies
// fabrik:bot-reprompted, and performs NO request-mutation calls (there is nothing to
// mutate for a reviewer that was never formally requested).
func TestCheckReviewGate_DeclaredReviewer_EmptyReviewRequests_Phase1PostsMentionDirectly(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	eng.cfg.ReviewWaitTimeout = 5 * time.Minute

	awaitingApplied := time.Now().Add(-10 * time.Minute)
	client.fetchLabelAppliedAtFn = func(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
		if labelName == "fabrik:awaiting-review" {
			return awaitingApplied, nil
		}
		return time.Time{}, nil
	}

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:                 10,
		Repo:                   "owner/repo",
		LinkedPRNumber:         42,
		Labels:                 []string{"fabrik:awaiting-review"},
		LinkedPRReviewRequests: nil, // real self-submitting bots never appear here
		LinkedPRReviews:        nil,
	}
	declared := []string{"handarbeit-pruefer"}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true), ExpectedReviewers: &declared}

	blocked, timedOut, _, _ := eng.checkReviewGate(board, item, stage)

	if !blocked {
		t.Error("expected still blocked after Phase 1 re-prompt")
	}
	if timedOut {
		t.Error("expected not timedOut after Phase 1 (still blocked)")
	}

	if len(client.deleteReviewRequestCalls) != 0 {
		t.Errorf("expected NO DeleteReviewRequest calls (nothing to mutate for a declared reviewer), got %d", len(client.deleteReviewRequestCalls))
	}
	if len(client.addReviewRequestCalls) != 0 {
		t.Errorf("expected NO AddReviewRequest calls (nothing to mutate for a declared reviewer), got %d", len(client.addReviewRequestCalls))
	}

	if len(client.addCommentCalls) != 1 {
		t.Fatalf("expected 1 PR @mention comment, got %d", len(client.addCommentCalls))
	}
	if client.addCommentCalls[0].issueNumber != 42 {
		t.Errorf("expected comment on PR #42, got #%d", client.addCommentCalls[0].issueNumber)
	}
	if !strings.Contains(client.addCommentCalls[0].body, "@handarbeit-pruefer") {
		t.Errorf("expected @handarbeit-pruefer in reprompt comment body, got: %q", client.addCommentCalls[0].body)
	}

	var foundReprompted bool
	for _, call := range client.addLabelCalls {
		if call.labelName == "fabrik:bot-reprompted" {
			foundReprompted = true
		}
	}
	if !foundReprompted {
		t.Error("expected fabrik:bot-reprompted label to be added — the first production-reachable use of this label")
	}
}

// Regression test: a declared identity that is a valid bare handle (passes
// validateExpectedReviewers, matches the live review author fine via
// reviewerIdentityMatches' copilot-prefix collapse) is not necessarily the
// same string GitHub resolves as a live @mention. "copilot-pull-request-
// reviewer" is exactly this case — a plausible App-slug declaration that
// clears the gate correctly but, before this fix, was mentioned verbatim as
// "@copilot-pull-request-reviewer" (which does not resolve) instead of
// "@copilot" (which does), silently defeating the notification this whole
// feature exists to send. No formal review request exists here at all, so
// this exercises only the declared-only Phase 1 loop, not the dedup-skip
// path covered by the adjacent "AlsoFormallyRequested" test.
func TestCheckReviewGate_DeclaredReviewer_MentionUsesCanonicalHandle(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	eng.cfg.ReviewWaitTimeout = 5 * time.Minute

	awaitingApplied := time.Now().Add(-10 * time.Minute)
	client.fetchLabelAppliedAtFn = func(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
		if labelName == "fabrik:awaiting-review" {
			return awaitingApplied, nil
		}
		return time.Time{}, nil
	}

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:                 10,
		Repo:                   "owner/repo",
		LinkedPRNumber:         42,
		Labels:                 []string{"fabrik:awaiting-review"},
		LinkedPRReviewRequests: nil, // real self-submitting bots never appear here
		LinkedPRReviews:        nil,
	}
	declared := []string{"copilot-pull-request-reviewer"}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true), ExpectedReviewers: &declared}

	blocked, timedOut, _, _ := eng.checkReviewGate(board, item, stage)

	if !blocked {
		t.Error("expected still blocked after Phase 1 re-prompt")
	}
	if timedOut {
		t.Error("expected not timedOut after Phase 1 (still blocked)")
	}

	if len(client.addCommentCalls) != 1 {
		t.Fatalf("expected 1 PR @mention comment, got %d", len(client.addCommentCalls))
	}
	if !strings.Contains(client.addCommentCalls[0].body, "@copilot") {
		t.Errorf("expected @copilot (the canonical, mention-resolvable handle) in reprompt comment body, got: %q", client.addCommentCalls[0].body)
	}
	if strings.Contains(client.addCommentCalls[0].body, "@copilot-pull-request-reviewer") {
		t.Errorf("mention must use the canonical handle, not the raw declared identity verbatim, got: %q", client.addCommentCalls[0].body)
	}
}

// A reviewer that is both formally requested (under GitHub's login form) and
// separately declared in expected_reviewers (under its mention-resolvable
// form) is the same reviewer — Phase 1 must re-prompt it once, not twice.
// Regression test for a login-form mismatch in the dedup check: it used to
// compare the declared identity against the raw requested login with plain
// case-insensitive equality, which never matches "copilot" against
// "copilot-pull-request-reviewer[bot]".
func TestCheckReviewGate_DeclaredReviewer_AlsoFormallyRequested_NotDoubleReprompted(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	eng.cfg.ReviewWaitTimeout = 5 * time.Minute

	awaitingApplied := time.Now().Add(-10 * time.Minute)
	client.fetchLabelAppliedAtFn = func(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
		if labelName == "fabrik:awaiting-review" {
			return awaitingApplied, nil
		}
		return time.Time{}, nil
	}

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:         10,
		Repo:           "owner/repo",
		LinkedPRNumber: 42,
		Labels:         []string{"fabrik:awaiting-review"},
		LinkedPRReviewRequests: []gh.ReviewRequest{
			{Login: "copilot-pull-request-reviewer", IsBot: true},
		},
		LinkedPRReviews: nil,
	}
	declared := []string{"copilot"}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true), ExpectedReviewers: &declared}

	blocked, timedOut, _, _ := eng.checkReviewGate(board, item, stage)

	if !blocked {
		t.Error("expected still blocked after Phase 1 re-prompt")
	}
	if timedOut {
		t.Error("expected not timedOut after Phase 1 (still blocked)")
	}

	if len(client.addCommentCalls) != 1 {
		t.Fatalf("expected exactly 1 @mention comment (requested and declared forms are the same reviewer), got %d: %v", len(client.addCommentCalls), client.addCommentCalls)
	}
	if !strings.Contains(client.addCommentCalls[0].body, "@copilot") {
		t.Errorf("expected @copilot in reprompt comment body, got: %q", client.addCommentCalls[0].body)
	}
}

// FR-3 any-of-N: with two declared reviewers, one submitting (under the REST-sourced
// [bot]-suffixed author form) clears the gate naturally without waiting for the other.
func TestCheckReviewGate_MultipleDeclaredReviewers_AnyOneResponds_Clears(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:                 10,
		Repo:                   "owner/repo",
		LinkedPRNumber:         42,
		LinkedPRReviewRequests: nil,
		LinkedPRReviews: []gh.PRReview{
			{Author: "handarbeit-pruefer[bot]", State: "COMMENTED"},
		},
	}
	declared := []string{"handarbeit-pruefer", "gemini-code-assist"}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true), ExpectedReviewers: &declared}

	blocked, timedOut, _, _ := eng.checkReviewGate(board, item, stage)

	if blocked {
		t.Error("expected gate to clear once any one declared reviewer has responded")
	}
	if timedOut {
		t.Error("expected not timedOut on natural clear")
	}
}

// FR-6: a declared reviewer that never appears must still time out (Phase 2), naming
// the bot — a declaration is not evidence the reviewer exists.
func TestCheckReviewGate_DeclaredReviewer_NeverResponds_Phase2PausesNamingBot(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	eng.cfg.ReviewWaitTimeout = 5 * time.Minute

	awaitingApplied := time.Now().Add(-15 * time.Minute)
	repromptedApplied := time.Now().Add(-10 * time.Minute) // 10 min > 5 min timeout → Phase 2
	client.fetchLabelAppliedAtFn = func(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
		if labelName == "fabrik:awaiting-review" {
			return awaitingApplied, nil
		}
		if labelName == "fabrik:bot-reprompted" {
			return repromptedApplied, nil
		}
		return time.Time{}, nil
	}

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:                 10,
		Repo:                   "owner/repo",
		LinkedPRNumber:         42,
		Labels:                 []string{"fabrik:awaiting-review", "fabrik:bot-reprompted"},
		LinkedPRReviewRequests: nil,
		LinkedPRReviews:        nil, // declared bot ("not installed") never shows up
	}
	declared := []string{"nonexistent-reviewer"}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true), ExpectedReviewers: &declared}

	blocked, timedOut, _, _ := eng.checkReviewGate(board, item, stage)

	if blocked {
		t.Error("expected not blocked on Phase 2 (should return false, true)")
	}
	if !timedOut {
		t.Error("expected timedOut == true on Phase 2")
	}

	removedLabels := make(map[string]bool)
	for _, call := range client.removeLabelCalls {
		removedLabels[call.labelName] = true
	}
	if !removedLabels["fabrik:bot-reprompted"] || !removedLabels["fabrik:awaiting-review"] {
		t.Error("expected both fabrik:bot-reprompted and fabrik:awaiting-review removed in Phase 2")
	}
}

// Broken-linkage guard: PR exists on branch but LinkedPRNumber == 0 → pause instead of loop.
func TestCheckReviewGate_BrokenLinkage_PRFound_Pauses(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 77, State: "open"}, nil
		},
	}
	eng := reviewTestEngine(t, client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:         10,
		Repo:           "owner/repo",
		LinkedPRNumber: 0, // no linkage via closingIssuesReferences
	}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true)}

	blocked, timedOut, terminated, _ := eng.checkReviewGate(board, item, stage)

	// Gate returns (false, false, true) — not blocked-for-reviews, not timed
	// out, but processing was terminated via a direct pause — the caller must
	// claim the item rather than reading this as "gate cleared".
	if blocked {
		t.Error("gate should not report blocked when broken linkage detected")
	}
	if timedOut {
		t.Error("gate should not report timedOut for broken linkage")
	}
	if !terminated {
		t.Error("gate should report terminated=true for broken-linkage pause")
	}

	// Issue MUST be paused.
	client.mu.Lock()
	labels := client.addLabelCalls
	client.mu.Unlock()
	hasPaused := false
	for _, l := range labels {
		if l.labelName == "fabrik:paused" {
			hasPaused = true
		}
	}
	if !hasPaused {
		t.Error("issue should have fabrik:paused label applied")
	}

	// fabrik:awaiting-review must NOT be applied.
	for _, l := range labels {
		if l.labelName == "fabrik:awaiting-review" {
			t.Error("fabrik:awaiting-review must not be applied for broken linkage")
		}
	}

	// Comment should mention the closing keyword and PR number.
	if len(client.addCommentCalls) == 0 {
		t.Fatal("expected a broken-linkage comment to be posted")
	}
	body := client.addCommentCalls[0].body
	if !strings.Contains(body, "Closes #10") {
		t.Errorf("comment should contain recovery closing keyword, got: %q", body)
	}
}

// Broken-linkage guard: LinkedPRNumber == 0 and no PR found on branch → falls through to normal logic.
func TestCheckReviewGate_BrokenLinkage_NoPRFound_FallsThrough(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return nil, nil // no PR on branch
		},
	}
	eng := reviewTestEngine(t, client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:         10,
		Repo:           "owner/repo",
		LinkedPRNumber: 0,
	}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true)}

	blocked, _, _, _ := eng.checkReviewGate(board, item, stage)

	// No PR found → falls through to normal reviewer logic. With no reviews, gate blocks.
	if !blocked {
		t.Error("gate should still block when no PR is found (normal logic applies)")
	}

	// Issue must NOT be paused for broken linkage.
	client.mu.Lock()
	labels := client.addLabelCalls
	client.mu.Unlock()
	for _, l := range labels {
		if l.labelName == "fabrik:paused" {
			t.Error("should not pause when no PR exists on branch")
		}
	}
}

// Broken-linkage guard on a base:<branch> repo: closedByPullRequestsReferences is
// structurally empty there, so LinkedPRNumber == 0 alone doesn't mean linkage is
// broken. When the PR body already contains the closing keyword (confirmed via
// FetchPRClosingIssues), the gate must fall through to normal reviewer logic instead
// of pausing. See #1046/#1047.
func TestCheckReviewGate_BrokenLinkage_NonDefaultBase_ConfirmedViaBody_ClearsNormally(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 77, State: "open"}, nil
		},
		fetchPRClosingIssuesFn: func(owner, repo string, prNumber int) ([]int, error) {
			return []int{10}, nil // PR body already contains "Closes #10"
		},
	}
	eng := reviewTestEngine(t, client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:         10,
		Repo:           "owner/repo",
		Labels:         []string{"base:develop"},
		LinkedPRNumber: 0,
	}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true)}

	blocked, timedOut, _, _ := eng.checkReviewGate(board, item, stage)

	// Falls through to normal reviewer logic: no reviewers requested, no reviews yet → blocks.
	if !blocked {
		t.Error("gate should fall through to normal blocking logic once linkage is confirmed via body")
	}
	if timedOut {
		t.Error("gate should not report timedOut")
	}

	client.mu.Lock()
	labels := client.addLabelCalls
	client.mu.Unlock()
	for _, l := range labels {
		if l.labelName == "fabrik:paused" {
			t.Error("should not pause when linkage is confirmed via PR body on a base:<branch> repo")
		}
	}
}

// Broken-linkage guard on a base:<branch> repo: PR exists but its body still lacks the
// closing keyword — this is genuinely broken linkage (base-independent), so the gate
// must pause exactly as it does on the default-branch path.
func TestCheckReviewGate_BrokenLinkage_NonDefaultBase_BodyMissing_StillPauses(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 77, State: "open"}, nil
		},
		fetchPRClosingIssuesFn: func(owner, repo string, prNumber int) ([]int, error) {
			return nil, nil // no closing keyword found in the PR body
		},
	}
	eng := reviewTestEngine(t, client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:         10,
		Repo:           "owner/repo",
		Labels:         []string{"base:develop"},
		LinkedPRNumber: 0,
	}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true)}

	blocked, timedOut, terminated, _ := eng.checkReviewGate(board, item, stage)

	if blocked {
		t.Error("gate should not report blocked when broken linkage detected")
	}
	if timedOut {
		t.Error("gate should not report timedOut for broken linkage")
	}
	if !terminated {
		t.Error("gate should report terminated=true for broken-linkage pause")
	}

	client.mu.Lock()
	labels := client.addLabelCalls
	client.mu.Unlock()
	hasPaused := false
	for _, l := range labels {
		if l.labelName == "fabrik:paused" {
			hasPaused = true
		}
		if l.labelName == "fabrik:awaiting-review" {
			t.Error("fabrik:awaiting-review must not be applied for broken linkage")
		}
	}
	if !hasPaused {
		t.Error("issue should have fabrik:paused label applied")
	}

	if len(client.addCommentCalls) == 0 {
		t.Fatal("expected a broken-linkage comment to be posted")
	}
	body := client.addCommentCalls[0].body
	if !strings.Contains(body, "Closes #10") {
		t.Errorf("comment should contain recovery closing keyword, got: %q", body)
	}
}

// On a base:<branch> repo, closedByPullRequestsReferences (and everything nested
// inside it — reviewRequests, latestReviews) is structurally empty, so
// item.LinkedPRReviewRequests/LinkedPRReviews are always empty regardless of the
// PR's actual review state. checkReviewGate must fetch reviews/requests via REST,
// keyed on the PR number handleBrokenReviewLinkage resolves, and clear the gate
// naturally once a review is in and no requests remain outstanding. See #1046/#1047/#1050.
func TestCheckReviewGate_NonDefaultBase_RESTReviewSubmitted_ClearsNaturally(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 77, State: "open"}, nil
		},
		fetchPRClosingIssuesFn: func(owner, repo string, prNumber int) ([]int, error) {
			return []int{10}, nil // PR body already contains "Closes #10"
		},
		fetchPRReviewsFn: func(owner, repo string, prNumber int) ([]gh.PRReview, error) {
			if prNumber != 77 {
				t.Errorf("expected FetchPRReviews to be called with resolved PR #77, got #%d", prNumber)
			}
			return []gh.PRReview{{Author: "reviewer1", State: "APPROVED"}}, nil
		},
		fetchPRReviewRequestsFn: func(owner, repo string, prNumber int) ([]gh.ReviewRequest, error) {
			if prNumber != 77 {
				t.Errorf("expected FetchPRReviewRequests to be called with resolved PR #77, got #%d", prNumber)
			}
			return nil, nil // no outstanding requests
		},
	}
	eng := reviewTestEngine(t, client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:         10,
		Repo:           "owner/repo",
		Labels:         []string{"base:develop"},
		LinkedPRNumber: 0,
	}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true)}

	blocked, timedOut, _, _ := eng.checkReviewGate(board, item, stage)

	if blocked {
		t.Error("expected gate to clear naturally once a REST-sourced review is submitted with no outstanding requests")
	}
	if timedOut {
		t.Error("expected not timedOut")
	}

	client.mu.Lock()
	labels := client.addLabelCalls
	client.mu.Unlock()
	for _, l := range labels {
		if l.labelName == "fabrik:paused" {
			t.Error("should not pause when the REST-sourced review data clears the gate")
		}
		if l.labelName == "fabrik:awaiting-review" {
			t.Error("fabrik:awaiting-review should not be applied when the gate clears immediately")
		}
	}
}

// Outstanding-reviewer detection must also work on base:<branch> repos: a requested
// reviewer who hasn't submitted keeps the gate blocked, mirroring default-branch behavior.
func TestCheckReviewGate_NonDefaultBase_RESTOutstandingReviewer_Blocks(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 77, State: "open"}, nil
		},
		fetchPRClosingIssuesFn: func(owner, repo string, prNumber int) ([]int, error) {
			return []int{10}, nil
		},
		fetchPRReviewsFn: func(owner, repo string, prNumber int) ([]gh.PRReview, error) {
			return nil, nil // no reviews submitted yet
		},
		fetchPRReviewRequestsFn: func(owner, repo string, prNumber int) ([]gh.ReviewRequest, error) {
			return []gh.ReviewRequest{{Login: "alice", IsBot: false}}, nil
		},
	}
	eng := reviewTestEngine(t, client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:         10,
		Repo:           "owner/repo",
		Labels:         []string{"base:develop"},
		LinkedPRNumber: 0,
	}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true)}

	blocked, timedOut, _, _ := eng.checkReviewGate(board, item, stage)

	if !blocked {
		t.Error("expected gate to stay blocked while a REST-sourced outstanding reviewer hasn't submitted")
	}
	if timedOut {
		t.Error("expected not timedOut on first evaluation")
	}

	client.mu.Lock()
	labels := client.addLabelCalls
	client.mu.Unlock()
	if len(labels) != 1 || labels[0].labelName != "fabrik:awaiting-review" {
		t.Errorf("expected exactly 1 fabrik:awaiting-review label add, got %v", labels)
	}
}

// Regression test: Phase 1's formal-request mutation loop (DELETE/POST
// review request + @mention comment) must fire for a REST-resolved
// outstanding bot reviewer on a base:<branch> repo, where
// item.LinkedPRReviewRequests is structurally empty. The loop used to
// iterate item.LinkedPRReviewRequests directly instead of the
// already-REST-resolved outstanding slice checkReviewGate threads through,
// so it silently no-opped here even though allBots/outstanding correctly
// detected the outstanding bot.
func TestCheckReviewGate_NonDefaultBase_BotPhase1_Reprompts(t *testing.T) {
	client := &mockGitHubClient{
		fetchPRReviewsFn: func(owner, repo string, prNumber int) ([]gh.PRReview, error) {
			return nil, nil
		},
		fetchPRReviewRequestsFn: func(owner, repo string, prNumber int) ([]gh.ReviewRequest, error) {
			return []gh.ReviewRequest{{Login: "copilot-pull-request-reviewer", IsBot: true}}, nil
		},
	}
	eng := reviewTestEngine(t, client)
	eng.cfg.ReviewWaitTimeout = 5 * time.Minute

	awaitingApplied := time.Now().Add(-10 * time.Minute)
	client.fetchLabelAppliedAtFn = func(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
		if labelName == "fabrik:awaiting-review" {
			return awaitingApplied, nil
		}
		return time.Time{}, nil
	}

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:         10,
		Repo:           "owner/repo",
		Labels:         []string{"base:develop", "fabrik:awaiting-review"},
		LinkedPRNumber: 77,
	}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true)}

	blocked, timedOut, _, _ := eng.checkReviewGate(board, item, stage)

	if !blocked {
		t.Error("expected still blocked after Phase 1 re-prompt")
	}
	if timedOut {
		t.Error("expected not timedOut after Phase 1 (still blocked)")
	}

	if len(client.deleteReviewRequestCalls) != 1 {
		t.Errorf("expected 1 DeleteReviewRequest call, got %d", len(client.deleteReviewRequestCalls))
	}
	if len(client.addReviewRequestCalls) != 1 {
		t.Errorf("expected 1 AddReviewRequest call, got %d", len(client.addReviewRequestCalls))
	}
	if len(client.addCommentCalls) != 1 {
		t.Fatalf("expected 1 PR @mention comment, got %d", len(client.addCommentCalls))
	}
	if client.addCommentCalls[0].issueNumber != 77 {
		t.Errorf("expected comment on PR #77, got #%d", client.addCommentCalls[0].issueNumber)
	}
	if !strings.Contains(client.addCommentCalls[0].body, "@copilot") {
		t.Errorf("expected @copilot in reprompt comment body, got: %q", client.addCommentCalls[0].body)
	}

	var foundReprompted bool
	for _, call := range client.addLabelCalls {
		if call.labelName == "fabrik:bot-reprompted" {
			foundReprompted = true
		}
	}
	if !foundReprompted {
		t.Error("expected fabrik:bot-reprompted label to be added")
	}
}

// A transient REST error on either the reviews or requested-reviewers endpoint must
// not falsely clear the gate — treat the poll as no-data-available and stay blocked,
// retrying on the next poll.
func TestCheckReviewGate_NonDefaultBase_RESTFetchError_StaysBlocked(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 77, State: "open"}, nil
		},
		fetchPRClosingIssuesFn: func(owner, repo string, prNumber int) ([]int, error) {
			return []int{10}, nil
		},
		fetchPRReviewsFn: func(owner, repo string, prNumber int) ([]gh.PRReview, error) {
			return nil, fmt.Errorf("transient GitHub API error")
		},
		fetchPRReviewRequestsFn: func(owner, repo string, prNumber int) ([]gh.ReviewRequest, error) {
			// Succeeds with no outstanding requests — but the reviews call failed, so
			// the gate must NOT trust this partial success and false-clear.
			return nil, nil
		},
	}
	eng := reviewTestEngine(t, client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:         10,
		Repo:           "owner/repo",
		Labels:         []string{"base:develop"},
		LinkedPRNumber: 0,
	}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true)}

	blocked, timedOut, _, _ := eng.checkReviewGate(board, item, stage)

	if !blocked {
		t.Error("expected gate to stay blocked on a partial REST fetch failure (conservative no-data fallback)")
	}
	if timedOut {
		t.Error("expected not timedOut on first evaluation")
	}
}

// A REST fetch failure must never be indistinguishable from "genuinely
// nothing requested or reviewed" for the FR-2 fast path: a stage that
// declares expected_reviewers: [] must NOT fast-advance while the actual
// review state is unknown, mirroring reviewGateBlocksLanding's identical
// !fetchFailed guard. Regression test for a gap where checkReviewGate's fast
// path had no such guard at all.
func TestCheckReviewGate_NonDefaultBase_RESTFetchError_ExpectedReviewersNone_DoesNotFastAdvance(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 77, State: "open"}, nil
		},
		fetchPRClosingIssuesFn: func(owner, repo string, prNumber int) ([]int, error) {
			return []int{10}, nil
		},
		fetchPRReviewsFn: func(owner, repo string, prNumber int) ([]gh.PRReview, error) {
			return nil, fmt.Errorf("transient GitHub API error")
		},
		fetchPRReviewRequestsFn: func(owner, repo string, prNumber int) ([]gh.ReviewRequest, error) {
			return nil, nil
		},
	}
	eng := reviewTestEngine(t, client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:         10,
		Repo:           "owner/repo",
		Labels:         []string{"base:develop"},
		LinkedPRNumber: 0,
	}
	none := []string{}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true), ExpectedReviewers: &none}

	blocked, timedOut, _, _ := eng.checkReviewGate(board, item, stage)

	if !blocked {
		t.Error("expected gate to stay blocked on a REST fetch failure even though expected_reviewers: [] is declared — a fetch failure must never look like 'nothing requested, nothing expected'")
	}
	if timedOut {
		t.Error("expected not timedOut on first evaluation")
	}
	if len(client.addLabelCalls) != 0 {
		for _, l := range client.addLabelCalls {
			if l.labelName != "fabrik:awaiting-review" {
				t.Errorf("unexpected label applied on fetch failure: %s", l.labelName)
			}
		}
	}
}

// A REST fetch failure must not manufacture a false "declared reviewer never
// responded" reading either: with reviews unknown (nil), every declared
// identity would otherwise look outstanding, which can flip
// reviewGateAllBots to true and fire a spurious Phase 1 @mention re-prompt
// for a bot that may already have reviewed. Sets up the timeout-elapsed
// state (mirroring TestCheckReviewGate_NonDefaultBase_BotPhase1_Reprompts)
// to prove Phase 1 does NOT fire from fetch-failure-derived data alone.
func TestCheckReviewGate_NonDefaultBase_RESTFetchError_DeclaredReviewers_NoSpuriousReprompt(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 77, State: "open"}, nil
		},
		fetchPRClosingIssuesFn: func(owner, repo string, prNumber int) ([]int, error) {
			return []int{10}, nil
		},
		fetchPRReviewsFn: func(owner, repo string, prNumber int) ([]gh.PRReview, error) {
			return nil, fmt.Errorf("transient GitHub API error")
		},
		fetchPRReviewRequestsFn: func(owner, repo string, prNumber int) ([]gh.ReviewRequest, error) {
			return nil, nil
		},
	}
	eng := reviewTestEngine(t, client)
	eng.cfg.ReviewWaitTimeout = 5 * time.Minute

	awaitingApplied := time.Now().Add(-10 * time.Minute)
	client.fetchLabelAppliedAtFn = func(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
		if labelName == "fabrik:awaiting-review" {
			return awaitingApplied, nil
		}
		return time.Time{}, nil
	}

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:         10,
		Repo:           "owner/repo",
		Labels:         []string{"base:develop", "fabrik:awaiting-review"},
		LinkedPRNumber: 77, // must be >0 for Phase 1 to be reachable at all — see checkAwaitingReviewTimeout's item.LinkedPRNumber > 0 guard
	}
	declared := []string{"handarbeit-pruefer"}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true), ExpectedReviewers: &declared}

	eng.checkReviewGate(board, item, stage)

	if len(client.addCommentCalls) != 0 {
		t.Errorf("expected NO @mention re-prompt comment from fetch-failure-derived data, got %d", len(client.addCommentCalls))
	}
	for _, call := range client.addLabelCalls {
		if call.labelName == "fabrik:bot-reprompted" {
			t.Error("expected fabrik:bot-reprompted NOT to be applied purely from a REST fetch failure")
		}
	}
}

// Default-branch items must never invoke the REST review-fetch helpers — the gate
// continues to use the GraphQL-sourced item.LinkedPRReviewRequests/LinkedPRReviews
// exclusively, exactly as before this change.
func TestCheckReviewGate_DefaultBranch_DoesNotCallRESTReviewFetch(t *testing.T) {
	var restReviewCalls, restRequestCalls int
	client := &mockGitHubClient{
		fetchPRReviewsFn: func(owner, repo string, prNumber int) ([]gh.PRReview, error) {
			restReviewCalls++
			return nil, fmt.Errorf("should not be called on default-branch items")
		},
		fetchPRReviewRequestsFn: func(owner, repo string, prNumber int) ([]gh.ReviewRequest, error) {
			restRequestCalls++
			return nil, fmt.Errorf("should not be called on default-branch items")
		},
	}
	eng := reviewTestEngine(t, client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:                 10,
		Repo:                   "owner/repo",
		LinkedPRNumber:         77, // linkage already resolved via GraphQL
		LinkedPRReviewRequests: nil,
		LinkedPRReviews: []gh.PRReview{
			{Author: "reviewer1", State: "APPROVED"},
		},
	}
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true)}

	blocked, timedOut, _, _ := eng.checkReviewGate(board, item, stage)

	if blocked {
		t.Error("expected gate to clear using the existing GraphQL-sourced data")
	}
	if timedOut {
		t.Error("expected not timedOut")
	}
	if restReviewCalls != 0 || restRequestCalls != 0 {
		t.Errorf("expected no REST review-fetch calls on a default-branch item, got reviews=%d requests=%d", restReviewCalls, restRequestCalls)
	}
}

// ---- #1460 R2/AC1/AC2: pauseForReviewCycleLimit resumability ----

// TestReviewCycleLimit_UnpauseResetsCounterAndAllowsMultipleCycles is the
// AC1/AC2 regression for #1460's confirmed site #1: before this fix,
// pauseForReviewCycleLimit never applied itemstate.EnginePaused, so removing
// fabrik:paused was a no-op — ReviewCycles stayed pinned at the limit and the
// very next catch-up pass re-paused immediately (reproduced live on #1208).
//
// Drives through the real catchUpPhase1Handlers chain (runPhase1Chain, not
// clearFailedStage directly), and — critically — drives the pause itself
// through the real dispatchWithCycleLimit/pauseForReviewCycleLimit call
// (cycleCount pre-set to exactly the limit, with actionable feedback present
// so the gate actually reaches the pause branch) rather than seeding
// itemstate.EnginePaused directly in the test. Seeding it directly would
// validate handleEngineUnpause's own reset logic (already covered by
// TestHandleEngineUnpause_PausedByEngine_ResetsAndReturnsFalse) but prove
// nothing about whether pauseForReviewCycleLimit itself applies EnginePaused
// — the actual R2 fix.
//
// AC3: reverting pauseForReviewCycleLimit's two itemstate.EnginePaused apply
// lines back to their pre-fix (absent) form turns this test red: Step 1's
// PausedByEngine assertion fails immediately (confirmed by neutralizing both
// lines and re-running — see PR description).
func TestReviewCycleLimit_UnpauseResetsCounterAndAllowsMultipleCycles(t *testing.T) {
	client := &mockGitHubClient{
		addCommentFn:         func(_, _ string, _ int, _ string) (int, error) { return 1, nil },
		addCommentReactionFn: func(_, _ string, _ int, _ string) error { return nil },
	}
	stgs := []*stages.Stage{{Name: "Implement", Order: 1, Prompt: "implement"}}
	eng := testEngineWithStages(t, client, stgs)
	eng.cfg.MaxReviewCycles = 2
	stage := stgs[0]
	board := &gh.ProjectBoard{}
	const repo = "owner/repo"
	const number = 30

	// Step 1: drive a REAL pause through the actual code path. ReviewCycles
	// pre-set to exactly the limit, with actionable feedback present, so
	// handleReviewGate's dispatchWithCycleLimit reaches the pause branch
	// (cycleCount >= maxCycles) and calls the real pauseForReviewCycleLimit.
	for i := 0; i < eng.cfg.MaxReviewCycles; i++ {
		eng.store.Apply(itemstate.ReviewCycleIncremented{Repo: repo, Number: number, StageName: "Implement"})
	}
	item := gh.ProjectItem{
		Number: number, Repo: repo, Labels: []string{"stage:Implement:complete"},
		LinkedPRReviewThreadComments: []gh.Comment{
			{ID: "PRRC_initial", DatabaseID: 199, Author: "copilot", Body: "fix this", ReviewThreadID: "RT_initial"},
		},
	}
	pctx := &phase1Ctx{ctx: context.Background(), board: board, item: item, stage: stage, hasComplete: true, advancedItems: make(map[string]bool)}
	claimed := runPhase1Chain(eng, pctx)
	eng.wg.Wait()
	if !claimed {
		t.Fatal("expected the cycle-limit pause branch to claim the item")
	}

	client.mu.Lock()
	pausedApplied := false
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			pausedApplied = true
		}
	}
	client.mu.Unlock()
	if !pausedApplied {
		t.Fatal("expected fabrik:paused to be applied by the real cycle-limit pause")
	}
	snap, _ := eng.store.Get(repo, number)
	if !snap.PausedByEngine("Implement") {
		t.Fatal("R2: pauseForReviewCycleLimit must apply itemstate.EnginePaused so wasPaused becomes true on resume — PausedByEngine(Implement) is false after the real pause")
	}

	// Step 2 (AC1): simulate the operator removing fabrik:paused (the item's
	// own label set no longer carries it) and run a pass with nothing new
	// actionable — isolates the reset itself from any same-pass re-dispatch.
	item.Labels = []string{"stage:Implement:complete"}
	item.LinkedPRReviewThreadComments = nil
	pctx = &phase1Ctx{ctx: context.Background(), board: board, item: item, stage: stage, hasComplete: true, advancedItems: make(map[string]bool)}
	runPhase1Chain(eng, pctx)
	eng.wg.Wait()

	snap, _ = eng.store.Get(repo, number)
	if got := snap.ReviewCycles("Implement"); got != 0 {
		t.Fatalf("AC1: ReviewCycles(Implement) after unpause = %d; want 0 — the counter must actually reset, not just the label go away", got)
	}
	if snap.PausedByEngine("Implement") {
		t.Error("PausedByEngine(Implement) must be cleared after the reset pass")
	}

	// Step 3 (AC2): the item must now perform more than one subsequent cycle
	// without re-pausing. Feed fresh actionable review feedback across two
	// more passes (distinct comment/thread IDs per pass, simulating new
	// reviewer activity each time — buildReviewThreadComments excludes an
	// already-processed comment ID, so reusing one ID would silently stop
	// being "actionable" rather than proving anything about the cycle limit)
	// and confirm ReviewCycles climbs while fabrik:paused is never re-added.
	// baseline records the addLabelCalls count so far (Step 1's genuine pause
	// legitimately added fabrik:paused once — only re-additions AFTER the
	// reset are the regression this checks for).
	client.mu.Lock()
	baseline := len(client.addLabelCalls)
	client.mu.Unlock()
	for cycle := 1; cycle <= 2; cycle++ {
		item.LinkedPRReviewThreadComments = []gh.Comment{
			{
				ID: fmt.Sprintf("PRRC_cycle_%d", cycle), DatabaseID: 200 + cycle,
				Author: "copilot", Body: "fix this", ReviewThreadID: fmt.Sprintf("RT_cycle_%d", cycle),
			},
		}
		pctx = &phase1Ctx{ctx: context.Background(), board: board, item: item, stage: stage, hasComplete: true, advancedItems: make(map[string]bool)}
		claimed := runPhase1Chain(eng, pctx)
		eng.wg.Wait()
		if !claimed {
			t.Fatalf("cycle %d: expected the review-reinvoke dispatch to claim the item", cycle)
		}
		snap, _ = eng.store.Get(repo, number)
		if got := snap.ReviewCycles("Implement"); got != cycle {
			t.Errorf("cycle %d: ReviewCycles(Implement) = %d; want %d — a genuinely reset counter must permit more than one further cycle", cycle, got, cycle)
		}
	}

	client.mu.Lock()
	defer client.mu.Unlock()
	for _, c := range client.addLabelCalls[baseline:] {
		if c.labelName == "fabrik:paused" {
			t.Errorf("AC2: fabrik:paused must not be re-added across 2 subsequent cycles, both well below MaxReviewCycles=%d", eng.cfg.MaxReviewCycles)
		}
	}
}

// TestPauseForReviewCycleLimit_ExistingComment_NoRepost is the AC4 regression
// for #1460's confirmed site #1: when a pause comment for this episode
// already exists (hasPauseComment matches the stable review-cycle-limit
// fragment), pauseForReviewCycleLimit must reapply fabrik:paused +
// fabrik:awaiting-input (and re-arm itemstate.EnginePaused — R2 must fire on
// the reapply branch too, not just the fresh-pause branch) without posting a
// duplicate comment. Mirrors
// TestSettleAwaitingCIScan_RaceWithMainLoop_CycleLimitPause_NoDuplicateComment's
// assertion style for the CI-gate precedent this reuses (PR #1445/ADR-1408).
func TestPauseForReviewCycleLimit_ExistingComment_NoRepost(t *testing.T) {
	client := &mockGitHubClient{}
	stgs := []*stages.Stage{{Name: "Implement", Order: 1, Prompt: "implement"}}
	eng := testEngineWithStages(t, client, stgs)
	stage := stgs[0]
	item := gh.ProjectItem{
		Number: 31, Repo: "owner/repo",
		Comments: []gh.Comment{
			{ID: "C1", Body: "🏭 **Fabrik — review cycle limit reached**\n\nThe stage **Implement** has been re-invoked to address " +
				"PR review feedback 3 time(s), which has reached the maximum configured limit (`FABRIK_MAX_REVIEW_CYCLES=3`)."},
		},
	}

	escalated := eng.pauseForReviewCycleLimit(&gh.ProjectBoard{}, item, stage, 3, 3)

	if escalated {
		t.Error("expected escalated=false when an existing pause comment is reused")
	}
	if len(client.addCommentCalls) != 0 {
		t.Errorf("AC4: expected no new comment posted when one already exists for this episode, got %d", len(client.addCommentCalls))
	}
	pausedApplied, awaitingInputApplied := false, false
	for _, c := range client.addLabelCalls {
		switch c.labelName {
		case "fabrik:paused":
			pausedApplied = true
		case "fabrik:awaiting-input":
			awaitingInputApplied = true
		}
	}
	if !pausedApplied || !awaitingInputApplied {
		t.Errorf("expected fabrik:paused and fabrik:awaiting-input to be reapplied, got paused=%v awaitingInput=%v", pausedApplied, awaitingInputApplied)
	}
	snap, _ := eng.store.Get("owner/repo", 31)
	if !snap.PausedByEngine("Implement") {
		t.Error("R2: expected itemstate.EnginePaused to be (re-)applied even on the reapply branch — otherwise a second unpause after this episode would be a no-op")
	}
}
