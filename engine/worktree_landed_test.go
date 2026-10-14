package engine

import (
	"fmt"
	"sync/atomic"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

func landedOptsFor(t *testing.T, e *Engine, item gh.ProjectItem) ensureOpts {
	t.Helper()
	var eo ensureOpts
	for _, o := range e.worktreeAdoptionOptions(item) {
		o(&eo)
	}
	if eo.landedPRHeads == nil || eo.prBaseProbe == nil {
		t.Fatal("worktreeAdoptionOptions must supply both options")
	}
	return eo
}

func seedLinkedPR(t *testing.T, e *Engine, issue, pr int, head string, merged bool) {
	t.Helper()
	e.store.Reset([]gh.ProjectItem{{ID: "I_1", Number: issue, Repo: "owner/repo", Status: "Validate"}})
	if _, _, err := e.store.Apply(itemstate.PRHeadSHAUpdated{Repo: "owner/repo", Number: issue, LinkedPRNum: pr, SHA: head}); err != nil {
		t.Fatal(err)
	}
	state := "open"
	if merged {
		state = "closed"
	}
	if _, _, err := e.store.Apply(itemstate.PRDetailsUpdated{Repo: "owner/repo", Number: issue, PRNumber: pr, State: state, Merged: merged}); err != nil {
		t.Fatal(err)
	}
}

func TestWorktreeAdoptionOptions_MergedLinkedPRHeadIsReported(t *testing.T) {
	client := &mockGitHubClient{}
	e := testEngine(t, client, &mockClaudeInvoker{})
	seedLinkedPR(t, e, 5, 50, "abc123", true)

	eo := landedOptsFor(t, e, gh.ProjectItem{Number: 5, Repo: "owner/repo"})
	if got := eo.landedPRHeads(); len(got) != 1 || got[0] != "abc123" {
		t.Errorf("landed heads = %v, want [abc123]", got)
	}
}

func TestWorktreeAdoptionOptions_OpenLinkedPRHeadIsNotLanded(t *testing.T) {
	e := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	seedLinkedPR(t, e, 5, 50, "abc123", false)

	eo := landedOptsFor(t, e, gh.ProjectItem{Number: 5, Repo: "owner/repo"})
	if got := eo.landedPRHeads(); len(got) != 0 {
		t.Errorf("landed heads = %v, want none", got)
	}
}

func TestWorktreeAdoptionOptions_CreditedPRIsReadOnlyWhenInvoked(t *testing.T) {
	var calls atomic.Int32
	client := &mockGitHubClient{fetchPRDetailsFn: func(owner, repo string, n int) (*gh.PRDetails, error) {
		calls.Add(1)
		if n != 77 {
			t.Errorf("FetchPRDetails(#%d), want the credited #77", n)
		}
		return &gh.PRDetails{Number: 77, State: "closed", Merged: true, HeadSHA: "feed99"}, nil
	}}
	e := testEngine(t, client, &mockClaudeInvoker{})
	item := gh.ProjectItem{Number: 5, Repo: "owner/repo", Labels: []string{"fabrik:credited-pr:77"}}

	eo := landedOptsFor(t, e, item)
	if calls.Load() != 0 {
		t.Fatalf("building the options made %d GitHub calls, want 0 (lazy)", calls.Load())
	}
	if got := eo.landedPRHeads(); len(got) != 1 || got[0] != "feed99" {
		t.Errorf("landed heads = %v, want [feed99]", got)
	}
	if calls.Load() != 1 {
		t.Errorf("FetchPRDetails calls = %d, want 1", calls.Load())
	}
}

func TestWorktreeAdoptionOptions_CreditedPRNotMergedOrErrorYieldsNothing(t *testing.T) {
	for name, fn := range map[string]func(owner, repo string, n int) (*gh.PRDetails, error){
		"not merged": func(string, string, int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 77, State: "open", HeadSHA: "feed99"}, nil
		},
		"error": func(string, string, int) (*gh.PRDetails, error) { return nil, fmt.Errorf("boom") },
		"nil":   func(string, string, int) (*gh.PRDetails, error) { return nil, nil },
	} {
		t.Run(name, func(t *testing.T) {
			e := testEngine(t, &mockGitHubClient{fetchPRDetailsFn: fn}, &mockClaudeInvoker{})
			eo := landedOptsFor(t, e, gh.ProjectItem{Number: 5, Repo: "owner/repo", Labels: []string{"fabrik:credited-pr:77"}})
			if got := eo.landedPRHeads(); len(got) != 0 {
				t.Errorf("landed heads = %v, want none", got)
			}
		})
	}
}

func TestWorktreeAdoptionOptions_PRBase(t *testing.T) {
	t.Run("open linked PR reports its base", func(t *testing.T) {
		client := &mockGitHubClient{fetchPRDetailsFn: func(string, string, int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 50, State: "open", BaseRef: "release/1.x"}, nil
		}}
		e := testEngine(t, client, &mockClaudeInvoker{})
		eo := landedOptsFor(t, e, gh.ProjectItem{Number: 5, Repo: "owner/repo", LinkedPRNumber: 50})
		if base, ok := eo.prBaseProbe(); !ok || base != "release/1.x" {
			t.Errorf("probe = (%q, %v), want (release/1.x, true)", base, ok)
		}
	})
	t.Run("no linked PR is unknown without a call", func(t *testing.T) {
		client := &mockGitHubClient{fetchPRDetailsFn: func(string, string, int) (*gh.PRDetails, error) {
			t.Error("unexpected FetchPRDetails")
			return nil, nil
		}}
		e := testEngine(t, client, &mockClaudeInvoker{})
		eo := landedOptsFor(t, e, gh.ProjectItem{Number: 5, Repo: "owner/repo"})
		if _, ok := eo.prBaseProbe(); ok {
			t.Error("probe should be unknown with no linked PR")
		}
	})
	t.Run("merged or errored is unknown", func(t *testing.T) {
		client := &mockGitHubClient{fetchPRDetailsFn: func(string, string, int) (*gh.PRDetails, error) {
			return nil, fmt.Errorf("boom")
		}}
		e := testEngine(t, client, &mockClaudeInvoker{})
		eo := landedOptsFor(t, e, gh.ProjectItem{Number: 5, Repo: "owner/repo", LinkedPRNumber: 50})
		if _, ok := eo.prBaseProbe(); ok {
			t.Error("probe should be unknown on error")
		}
		seedLinkedPR(t, e, 5, 50, "abc", true)
		if _, ok := eo.prBaseProbe(); ok {
			t.Error("probe should be unknown for a merged PR")
		}
	})
}
