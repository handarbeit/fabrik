package engine

import (
	"errors"
	"strings"
	"sync"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// logLines collects the engine's log lines through the per-Engine sink.
func logLines(e *Engine) func() []string {
	var mu sync.Mutex
	var lines []string
	e.SetLogSinkForTest(func(_ int, tag, msg string) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, tag+"|"+strings.TrimSpace(msg))
	})
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), lines...)
	}
}

func authoritativeReviewStage() *stages.Stage {
	return &stages.Stage{Name: "Review", WaitForReviews: boolPtr(true), ReviewAuthority: "authoritative"}
}

// A blocking verdict holds the Phase 2 advance: the hold names the advance (not a
// landing decision), applies fabrik:awaiting-review and records the cooldown.
func TestReviewGateHoldsAdvance_BlockedHolds(t *testing.T) {
	client := &mockGitHubClient{
		fetchPRReviewsFn: func(string, string, int) ([]gh.PRReview, error) {
			return []gh.PRReview{{Author: "alice", State: "CHANGES_REQUESTED"}}, nil
		},
		fetchPRReviewDecisionFn: func(string, string, int) (string, error) { return "CHANGES_REQUESTED", nil },
	}
	eng := reviewTestEngine(t, client)
	lines := logLines(eng)
	item := gh.ProjectItem{Number: 10, Repo: "owner/repo", LinkedPRNumber: 77}

	if !eng.reviewGateHoldsAdvance(item, authoritativeReviewStage()) {
		t.Fatal("a blocking authoritative verdict must hold the advance")
	}
	if !containsLabel(addedLabelNames(client.addLabelCalls), "fabrik:awaiting-review") {
		t.Errorf("hold must apply fabrik:awaiting-review, got %v", client.addLabelCalls)
	}
	var held bool
	for _, l := range lines() {
		if strings.Contains(l, "holding advance on PR #77") {
			held = true
		}
		if strings.Contains(l, "landing decision") || strings.Contains(l, "advancing") {
			t.Errorf("hold poll logged the wrong decision: %q", l)
		}
	}
	if !held {
		t.Errorf("expected an advance-worded hold line, got %v", lines())
	}
	snap, ok := eng.store.Peek("owner/repo", 10)
	if ok && snap.CooldownAt("review-blocked").IsZero() {
		t.Error("hold must record a review-blocked cooldown")
	}
}

// A read failure holds (fail closed), never advances.
func TestReviewGateHoldsAdvance_ReadFailureHolds(t *testing.T) {
	client := &mockGitHubClient{
		fetchPRReviewsFn: func(string, string, int) ([]gh.PRReview, error) { return nil, errors.New("boom") },
	}
	eng := reviewTestEngine(t, client)
	item := gh.ProjectItem{Number: 10, Repo: "owner/repo", LinkedPRNumber: 77}
	if !eng.reviewGateHoldsAdvance(item, authoritativeReviewStage()) {
		t.Fatal("an unreadable review state must hold the advance")
	}
}

// A cleared verdict advances and the log says so, never "blocking".
func TestReviewGateHoldsAdvance_ClearAdvancesAndLogsIt(t *testing.T) {
	client := &mockGitHubClient{
		fetchPRReviewsFn: func(string, string, int) ([]gh.PRReview, error) {
			return []gh.PRReview{{Author: "alice", State: "APPROVED"}}, nil
		},
		fetchPRReviewDecisionFn: func(string, string, int) (string, error) { return "APPROVED", nil },
	}
	eng := reviewTestEngine(t, client)
	lines := logLines(eng)
	item := gh.ProjectItem{Number: 10, Repo: "owner/repo", LinkedPRNumber: 77}
	if eng.reviewGateHoldsAdvance(item, authoritativeReviewStage()) {
		t.Fatal("a satisfied verdict must not hold")
	}
	if len(client.addLabelCalls) != 0 {
		t.Errorf("a clear verdict writes no label, got %v", client.addLabelCalls)
	}
	var clear bool
	for _, l := range lines() {
		if strings.Contains(l, "blocking") || strings.Contains(l, "holding") {
			t.Errorf("advancing poll logged a hold: %q", l)
		}
		if strings.Contains(l, "review gate clear") {
			clear = true
		}
	}
	if !clear {
		t.Errorf("expected a gate-clear line, got %v", lines())
	}
}

// Stages without wait_for_reviews are untouched: no reads, no log, no hold.
func TestReviewGateHoldsAdvance_NotOptedInIsUntouched(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	lines := logLines(eng)
	item := gh.ProjectItem{Number: 10, Repo: "owner/repo", LinkedPRNumber: 77}
	if eng.reviewGateHoldsAdvance(item, &stages.Stage{Name: "Plan"}) {
		t.Fatal("a stage without wait_for_reviews must not be held")
	}
	if got := lines(); len(got) != 0 {
		t.Errorf("expected no log output, got %v", got)
	}
	if len(client.addLabelCalls) != 0 {
		t.Errorf("expected no label writes, got %v", client.addLabelCalls)
	}
}

// The seam restores the pre-#2491 behaviour (the non-vacuity proof).
func TestReviewGateHoldsAdvance_SeamNeutralises(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	eng.SetReviewGateAdvanceDisabledForTest(true)
	item := gh.ProjectItem{Number: 10, Repo: "owner/repo", LinkedPRNumber: 77}
	if eng.reviewGateHoldsAdvance(item, authoritativeReviewStage()) {
		t.Fatal("with the gate neutralised the advance is never held")
	}
}

// The landing and overseer callers keep their existing wording byte for byte.
func TestReviewGateHold_LandingWordingUnchanged(t *testing.T) {
	client := &mockGitHubClient{}
	eng := reviewTestEngine(t, client)
	item := gh.ProjectItem{Number: 10, Repo: "owner/repo", LinkedPRNumber: 77}
	h := eng.reviewGateHold(item, &stages.Stage{Name: "Review", WaitForReviews: boolPtr(true)}, "owner", "repo")
	want := "holding landing decision on PR #77 — waiting for initial review submission\n"
	if h.Msg != want {
		t.Errorf("Msg = %q, want %q", h.Msg, want)
	}
}
