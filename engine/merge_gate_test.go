package engine

import (
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

func TestCheckMergeabilityGate_WaitForCIFalse_ClearsImmediately(t *testing.T) {
	called := false
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			called = true
			return nil, nil
		},
	}
	eng := testEngineForMerge(client)
	item := gh.ProjectItem{Number: 1}
	stage := &stages.Stage{Name: "Validate"} // WaitForCI is nil

	blocked, conflict := eng.checkMergeabilityGate(item, stage)
	if blocked || conflict {
		t.Errorf("expected clear when wait_for_ci not set, got blocked=%v conflict=%v", blocked, conflict)
	}
	if called {
		t.Error("should not call FetchLinkedPR when wait_for_ci is not set")
	}
}

func TestCheckMergeabilityGate_NoPR_ClearsGate(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return nil, nil
		},
	}
	eng := testEngineForMerge(client)
	tr := true
	item := gh.ProjectItem{Number: 1}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	blocked, conflict := eng.checkMergeabilityGate(item, stage)
	if blocked || conflict {
		t.Errorf("expected clear when no PR found, got blocked=%v conflict=%v", blocked, conflict)
	}
}

func TestCheckMergeabilityGate_Mergeable_ClearsGate_RemovesStaleLabel(t *testing.T) {
	tr := true
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 42, HeadSHA: "sha"}, nil
		},
		fetchPRMergeableFn: func(owner, repo string, prNumber int) (*bool, error) {
			return &tr, nil
		},
	}
	eng := testEngineForMerge(client)
	item := gh.ProjectItem{Number: 1, Labels: []string{"fabrik:rebase-needed"}}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	blocked, conflict := eng.checkMergeabilityGate(item, stage)
	if blocked || conflict {
		t.Errorf("expected clear when mergeable=true, got blocked=%v conflict=%v", blocked, conflict)
	}
	found := false
	for _, c := range client.removeLabelCalls {
		if c.labelName == "fabrik:rebase-needed" {
			found = true
		}
	}
	if !found {
		t.Error("expected stale fabrik:rebase-needed label to be removed when PR becomes mergeable")
	}
}

func TestCheckMergeabilityGate_NilMergeable_BlockedNoConflict(t *testing.T) {
	tr := true
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 42, HeadSHA: "sha"}, nil
		},
		fetchPRMergeableFn: func(owner, repo string, prNumber int) (*bool, error) {
			return nil, nil // GitHub hasn't computed yet
		},
	}
	eng := testEngineForMerge(client)
	item := gh.ProjectItem{Number: 1}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	blocked, conflict := eng.checkMergeabilityGate(item, stage)
	if !blocked || conflict {
		t.Errorf("expected blocked=true conflict=false for mergeable=null, got blocked=%v conflict=%v", blocked, conflict)
	}
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:rebase-needed" {
			t.Error("should not add fabrik:rebase-needed when mergeable is unknown")
		}
	}
}

func TestCheckMergeabilityGate_FalseMergeable_AppliesLabelAndSignalsConflict(t *testing.T) {
	fa := false
	tr := true
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 42, HeadSHA: "sha"}, nil
		},
		fetchPRMergeableFn: func(owner, repo string, prNumber int) (*bool, error) {
			return &fa, nil
		},
	}
	eng := testEngineForMerge(client)
	item := gh.ProjectItem{Number: 1}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	blocked, conflict := eng.checkMergeabilityGate(item, stage)
	if !blocked || !conflict {
		t.Errorf("expected blocked=true conflict=true for mergeable=false, got blocked=%v conflict=%v", blocked, conflict)
	}
	found := false
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:rebase-needed" {
			found = true
		}
	}
	if !found {
		t.Error("expected fabrik:rebase-needed to be added on confirmed conflict")
	}
}

func TestCheckMergeabilityGate_FalseMergeable_LabelIdempotent(t *testing.T) {
	fa := false
	tr := true
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 42, HeadSHA: "sha"}, nil
		},
		fetchPRMergeableFn: func(owner, repo string, prNumber int) (*bool, error) {
			return &fa, nil
		},
	}
	eng := testEngineForMerge(client)
	// Label already present — should not be re-added.
	item := gh.ProjectItem{Number: 1, Labels: []string{"fabrik:rebase-needed"}}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	_, conflict := eng.checkMergeabilityGate(item, stage)
	if !conflict {
		t.Error("expected conflict=true")
	}
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:rebase-needed" {
			t.Error("should not re-add fabrik:rebase-needed when already present")
		}
	}
}

func TestCheckMergeabilityGate_FetchPRError_BlocksForRetry(t *testing.T) {
	tr := true
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return nil, errStubTransient
		},
	}
	eng := testEngineForMerge(client)
	item := gh.ProjectItem{Number: 1}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	blocked, conflict := eng.checkMergeabilityGate(item, stage)
	if !blocked || conflict {
		t.Errorf("expected blocked=true conflict=false on transient FetchLinkedPR error, got blocked=%v conflict=%v", blocked, conflict)
	}
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:rebase-needed" {
			t.Error("should not add fabrik:rebase-needed on transient API error (no confirmed conflict)")
		}
	}
}

func TestCheckMergeabilityGate_FetchMergeableError_BlocksForRetry(t *testing.T) {
	tr := true
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 42, HeadSHA: "sha"}, nil
		},
		fetchPRMergeableFn: func(owner, repo string, prNumber int) (*bool, error) {
			return nil, errStubTransient
		},
	}
	eng := testEngineForMerge(client)
	item := gh.ProjectItem{Number: 1}
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	blocked, conflict := eng.checkMergeabilityGate(item, stage)
	if !blocked || conflict {
		t.Errorf("expected blocked=true conflict=false on transient mergeable-fetch error, got blocked=%v conflict=%v", blocked, conflict)
	}
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:rebase-needed" {
			t.Error("should not add fabrik:rebase-needed on transient API error (no confirmed conflict)")
		}
	}
}

// errStubTransient is a sentinel used in merge-gate tests to simulate a
// transient GitHub API error.
var errStubTransient = &stubError{"simulated transient error"}

type stubError struct{ msg string }

func (e *stubError) Error() string { return e.msg }
