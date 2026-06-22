package engine

import (
	"errors"
	"strings"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

// settleItem returns a minimal ProjectItem for settling tests.
func settleItem(n int) gh.ProjectItem {
	return gh.ProjectItem{Number: n, Repo: "owner/repo"}
}

// ── No PR ─────────────────────────────────────────────────────────────────────

func TestSettle_NoPR_ReturnsNoPR(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return nil, nil
		},
	}
	eng := testEngineForMerge(t, client)
	r := eng.settlePRMergeState(settleItem(1), &stages.Stage{Name: "Validate"})
	if r.Status != PRMergeNoPR {
		t.Errorf("expected PRMergeNoPR, got %v", r.Status)
	}
}

func TestSettle_ZeroPR_ReturnsNoPR(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 0}, nil
		},
	}
	eng := testEngineForMerge(t, client)
	r := eng.settlePRMergeState(settleItem(1), &stages.Stage{Name: "Validate"})
	if r.Status != PRMergeNoPR {
		t.Errorf("expected PRMergeNoPR for PR.Number==0, got %v", r.Status)
	}
}

func TestSettle_FetchLinkedPRError_ReturnsUnsettled(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return nil, errors.New("network error")
		},
	}
	eng := testEngineForMerge(t, client)
	r := eng.settlePRMergeState(settleItem(1), &stages.Stage{Name: "Validate"})
	if r.Status != PRMergeUnsettled {
		t.Errorf("expected PRMergeUnsettled on FetchLinkedPR error, got %v", r.Status)
	}
}

// ── Terminal states ───────────────────────────────────────────────────────────

func TestSettle_PRMerged_ReturnsTerminal(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 5, Merged: true, State: "closed"}, nil
		},
	}
	eng := testEngineForMerge(t, client)
	r := eng.settlePRMergeState(settleItem(1), &stages.Stage{Name: "Validate"})
	if r.Status != PRMergeTerminal {
		t.Errorf("expected PRMergeTerminal for merged PR, got %v", r.Status)
	}
	if r.PR == nil || r.PR.Number != 5 {
		t.Error("expected PR details in result")
	}
}

func TestSettle_PRClosedNotMerged_ReturnsTerminal(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 5, Merged: false, State: "closed"}, nil
		},
	}
	eng := testEngineForMerge(t, client)
	r := eng.settlePRMergeState(settleItem(1), &stages.Stage{Name: "Validate"})
	if r.Status != PRMergeTerminal {
		t.Errorf("expected PRMergeTerminal for closed PR, got %v", r.Status)
	}
}

// ── Stale list-endpoint merged flag (ConvergenceRace regression) ──────────────
//
// FetchLinkedPR reads the PR *list* endpoint, whose `merged` flag reports false
// for several seconds after a merge. A PR the engine just merged (e.g. the
// direct-merge fallback) arrives here as {State:"closed", Merged:false}. settle
// must re-confirm against the authoritative single-PR endpoint before classifying
// "closed without merging" — otherwise the issue is wrongly paused.

func TestSettle_ClosedButMerged_ConfirmedViaSinglePR(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			// List endpoint lies: merged=false right after a merge.
			return &gh.PRDetails{Number: 42, Merged: false, State: "closed"}, nil
		},
		fetchPRMergedFn: func(owner, repo string, prNumber int) (bool, error) {
			// Single-PR endpoint is authoritative: the PR was in fact merged.
			return true, nil
		},
	}
	eng := testEngineForMerge(t, client)
	r := eng.settlePRMergeState(settleItem(1), &stages.Stage{Name: "Validate"})
	if r.Status != PRMergeTerminal {
		t.Errorf("expected PRMergeTerminal for stale-list/merged-single PR, got %v", r.Status)
	}
	if r.PR == nil || !r.PR.Merged {
		t.Error("expected r.PR.Merged=true after single-PR re-confirmation")
	}
	if strings.Contains(r.Reason, "closed without merging") {
		t.Errorf("merged PR must NOT be classified as closed-without-merging; got reason %q", r.Reason)
	}
}

func TestSettle_ClosedNotMerged_StillTerminal(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 42, Merged: false, State: "closed"}, nil
		},
		fetchPRMergedFn: func(owner, repo string, prNumber int) (bool, error) {
			// Single-PR endpoint confirms it really was closed without merging.
			return false, nil
		},
	}
	eng := testEngineForMerge(t, client)
	r := eng.settlePRMergeState(settleItem(1), &stages.Stage{Name: "Validate"})
	if r.Status != PRMergeTerminal {
		t.Errorf("expected PRMergeTerminal for genuinely-closed PR, got %v", r.Status)
	}
	if !strings.Contains(r.Reason, "closed without merging") {
		t.Errorf("expected closed-without-merging reason, got %q", r.Reason)
	}
}

func TestSettle_ClosedMergedUnconfirmable_ReturnsUnsettled(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 42, Merged: false, State: "closed"}, nil
		},
		fetchPRMergedFn: func(owner, repo string, prNumber int) (bool, error) {
			// Single-PR endpoint unreachable — must not pause on an unconfirmable state.
			return false, errors.New("api timeout")
		},
	}
	eng := testEngineForMerge(t, client)
	r := eng.settlePRMergeState(settleItem(1), &stages.Stage{Name: "Validate"})
	if r.Status != PRMergeUnsettled {
		t.Errorf("expected PRMergeUnsettled when merged-state unconfirmable, got %v", r.Status)
	}
}

// ── Merge queue (ADR-058 D4 FR-1) ─────────────────────────────────────────────

// TestSettle_MergedBeatsInMergeQueue is the #913 merged-first ordering invariant:
// a PR that is BOTH merged and (stale-state) in the queue must classify as
// PRMergeTerminal, never PRMergeQueued. A queue merge is terminal, not "queued".
func TestSettle_MergedBeatsInMergeQueue(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 5, Merged: true, State: "closed", IsInMergeQueue: true}, nil
		},
	}
	eng := testEngineForMerge(t, client)
	r := eng.settlePRMergeState(settleItem(1), &stages.Stage{Name: "Validate"})
	if r.Status != PRMergeTerminal {
		t.Errorf("merged-first invariant violated: expected PRMergeTerminal, got %v", r.Status)
	}
}

// TestSettle_InMergeQueue_ReturnsQueued covers the settle.PR.IsInMergeQueue source.
func TestSettle_InMergeQueue_ReturnsQueued(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 5, State: "open", IsInMergeQueue: true}, nil
		},
	}
	eng := testEngineForMerge(t, client)
	r := eng.settlePRMergeState(settleItem(1), &stages.Stage{Name: "Validate"})
	if r.Status != PRMergeQueued {
		t.Errorf("expected PRMergeQueued for in-queue PR, got %v", r.Status)
	}
	if r.PR == nil || r.PR.Number != 5 {
		t.Error("expected PR details in PRMergeQueued result")
	}
}

// TestSettle_InMergeQueue_PollNativeSignal_CacheMiss verifies PRMergeQueued is
// derived from the GraphQL-authoritative ProjectItem field even when settle.PR
// does NOT carry the flag (a boardcache miss: FetchLinkedPR falls back to REST,
// which never decodes isInMergeQueue). The poll-native field must still drive it.
func TestSettle_InMergeQueue_PollNativeSignal_CacheMiss(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			// Cache-miss REST fallback: in-queue flag absent.
			return &gh.PRDetails{Number: 5, State: "open", IsInMergeQueue: false}, nil
		},
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			t.Error("FetchPRMergeableFields must not be called once the queue hand-off short-circuits")
			f := true
			return &f, "clean", nil
		},
	}
	eng := testEngineForMerge(t, client)
	item := gh.ProjectItem{Number: 1, Repo: "owner/repo", LinkedPRIsInMergeQueue: true}
	r := eng.settlePRMergeState(item, &stages.Stage{Name: "Validate"})
	if r.Status != PRMergeQueued {
		t.Errorf("expected PRMergeQueued from poll-native signal on cache miss, got %v", r.Status)
	}
}

// TestSettle_MergeQueueEntryState_ReturnsQueued covers the MergeQueueEntry.State
// source (QUEUED / AWAITING_CHECKS) when neither IsInMergeQueue flag is set.
func TestSettle_MergeQueueEntryState_ReturnsQueued(t *testing.T) {
	for _, state := range []string{"QUEUED", "AWAITING_CHECKS"} {
		client := &mockGitHubClient{
			fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
				return &gh.PRDetails{Number: 5, State: "open", MergeQueueEntry: &gh.MergeQueueEntry{State: state}}, nil
			},
		}
		eng := testEngineForMerge(t, client)
		r := eng.settlePRMergeState(settleItem(1), &stages.Stage{Name: "Validate"})
		if r.Status != PRMergeQueued {
			t.Errorf("MergeQueueEntry.State=%q: expected PRMergeQueued, got %v", state, r.Status)
		}
	}
}

// TestSettle_InMergeQueue_BeatsConflicting verifies the queue check runs BEFORE
// the mergeable-fields fetch: an in-queue PR that would otherwise classify as
// Conflicting still returns PRMergeQueued (the queue owns it).
func TestSettle_InMergeQueue_BeatsConflicting(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 5, State: "open", IsInMergeQueue: true}, nil
		},
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			f := false
			return &f, "dirty", nil // would be Conflicting if reached
		},
	}
	eng := testEngineForMerge(t, client)
	r := eng.settlePRMergeState(settleItem(1), &stages.Stage{Name: "Validate"})
	if r.Status != PRMergeQueued {
		t.Errorf("expected PRMergeQueued (queue check before mergeable fields), got %v", r.Status)
	}
}

// ── Transient/null mergeable states ──────────────────────────────────────────

func TestSettle_MergeableNil_ReturnsUnsettled(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 5, State: "open", HeadSHA: "sha1"}, nil
		},
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			return nil, "", nil // GitHub still computing
		},
	}
	eng := testEngineForMerge(t, client)
	r := eng.settlePRMergeState(settleItem(1), &stages.Stage{Name: "Validate"})
	if r.Status != PRMergeUnsettled {
		t.Errorf("expected PRMergeUnsettled when mergeable=nil, got %v", r.Status)
	}
}

func TestSettle_MergeableStateUnknown_ReturnsUnsettled(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 5, State: "open", HeadSHA: "sha1"}, nil
		},
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			t2 := true
			return &t2, "unknown", nil
		},
	}
	eng := testEngineForMerge(t, client)
	r := eng.settlePRMergeState(settleItem(1), &stages.Stage{Name: "Validate"})
	if r.Status != PRMergeUnsettled {
		t.Errorf("expected PRMergeUnsettled for mergeable_state=unknown, got %v", r.Status)
	}
	if r.MergeableState != "unknown" {
		t.Errorf("expected MergeableState=unknown, got %q", r.MergeableState)
	}
}

func TestSettle_FetchMergeableFieldsError_ReturnsUnsettled(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 5, State: "open", HeadSHA: "sha1"}, nil
		},
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			return nil, "", errors.New("timeout")
		},
	}
	eng := testEngineForMerge(t, client)
	r := eng.settlePRMergeState(settleItem(1), &stages.Stage{Name: "Validate"})
	if r.Status != PRMergeUnsettled {
		t.Errorf("expected PRMergeUnsettled on FetchPRMergeableFields error, got %v", r.Status)
	}
}

// ── Conflict ──────────────────────────────────────────────────────────────────

func TestSettle_MergeableFalse_ReturnsConflicting(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 5, State: "open", HeadSHA: "sha1"}, nil
		},
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			return boolPtr(false), "dirty", nil
		},
	}
	eng := testEngineForMerge(t, client)
	r := eng.settlePRMergeState(settleItem(1), &stages.Stage{Name: "Validate"})
	if r.Status != PRMergeConflicting {
		t.Errorf("expected PRMergeConflicting for mergeable=false, got %v", r.Status)
	}
	if r.MergeableState != "dirty" {
		t.Errorf("expected MergeableState=dirty, got %q", r.MergeableState)
	}
}

// ── ADR-033 shortcut (clean/unstable) ─────────────────────────────────────────

func TestSettle_MergeableStateClean_ReturnsReady(t *testing.T) {
	fetchCheckRunsCalled := false
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 5, State: "open", HeadSHA: "sha1"}, nil
		},
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			return boolPtr(true), "clean", nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			fetchCheckRunsCalled = true
			return nil, nil
		},
	}
	eng := testEngineForMerge(t, client)
	r := eng.settlePRMergeState(settleItem(1), &stages.Stage{Name: "Validate"})
	if r.Status != PRMergeReady {
		t.Errorf("expected PRMergeReady for mergeable_state=clean, got %v", r.Status)
	}
	if fetchCheckRunsCalled {
		t.Error("FetchCheckRuns must NOT be called when mergeable_state=clean (ADR-033 shortcut)")
	}
}

func TestSettle_MergeableStateUnstable_ReturnsReady(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 5, State: "open", HeadSHA: "sha1"}, nil
		},
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			return boolPtr(true), "unstable", nil
		},
	}
	eng := testEngineForMerge(t, client)
	r := eng.settlePRMergeState(settleItem(1), &stages.Stage{Name: "Validate"})
	if r.Status != PRMergeReady {
		t.Errorf("expected PRMergeReady for mergeable_state=unstable, got %v", r.Status)
	}
}

// ── Post-push dwell and registration delay ────────────────────────────────────

func TestSettle_PostPushHadChecks_ReturnsUnsettled(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 5, State: "open", HeadSHA: "sha-new"}, nil
		},
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			return boolPtr(true), "blocked", nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			return nil, nil // no checks yet for new SHA
		},
	}
	eng := testEngineForMerge(t, client)
	// Pre-seed: issue has previously had check runs (hadChecks=true).
	eng.store.Apply(itemstate.PRChecksObserved{Repo: "owner/repo", Number: 1})

	r := eng.settlePRMergeState(settleItem(1), &stages.Stage{Name: "Validate"})
	if r.Status != PRMergeUnsettled {
		t.Errorf("expected PRMergeUnsettled for post-push registration delay (hadChecks), got %v", r.Status)
	}
}

func TestSettle_PostPushDwellActive_ReturnsUnsettled(t *testing.T) {
	// Apply two PRHeadSHAUpdated events to stamp LastHeadSHAUpdate (store only stamps
	// when SHA actually changes from a non-empty value).
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 5, State: "open", HeadSHA: "sha-new"}, nil
		},
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			return boolPtr(true), "", nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			return nil, nil
		},
	}
	eng := testEngineForMerge(t, client)
	eng.cfg.PostPushDwell = 30 * time.Second

	// Seed initial SHA, then the new SHA — the second apply stamps LastHeadSHAUpdate.
	eng.store.Apply(itemstate.PRHeadSHAUpdated{Repo: "owner/repo", Number: 1, SHA: "sha-initial"})
	eng.store.Apply(itemstate.PRHeadSHAUpdated{Repo: "owner/repo", Number: 1, SHA: "sha-new"})

	// Call immediately — well within the 30s dwell.
	r := eng.settlePRMergeState(settleItem(1), &stages.Stage{Name: "Validate"})
	if r.Status != PRMergeUnsettled {
		t.Errorf("expected PRMergeUnsettled during post-push dwell window, got %v", r.Status)
	}
}

func TestSettle_PostPushDwellElapsed_ReturnsReady(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 5, State: "open", HeadSHA: "sha-new"}, nil
		},
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			return boolPtr(true), "", nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			return nil, nil
		},
	}
	eng := testEngineForMerge(t, client)
	// Sub-nanosecond dwell so it elapses immediately after stamping.
	eng.cfg.PostPushDwell = 1 * time.Nanosecond

	eng.store.Apply(itemstate.PRHeadSHAUpdated{Repo: "owner/repo", Number: 1, SHA: "sha-initial"})
	eng.store.Apply(itemstate.PRHeadSHAUpdated{Repo: "owner/repo", Number: 1, SHA: "sha-new"})
	// Brief sleep so even low-resolution clocks see the dwell elapsed.
	time.Sleep(20 * time.Millisecond)

	r := eng.settlePRMergeState(settleItem(1), &stages.Stage{Name: "Validate"})
	if r.Status != PRMergeReady {
		t.Errorf("expected PRMergeReady after post-push dwell elapsed, got %v", r.Status)
	}
}

// ── R3: BLOCKED + no checks ───────────────────────────────────────────────────

func TestSettle_BlockedNoChecks_ReturnsUnsettled(t *testing.T) {
	// mergeableState=blocked, no checks, no hadChecks, no dwell — R3 path.
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 5, State: "open", HeadSHA: "sha1"}, nil
		},
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			return boolPtr(true), "blocked", nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			return nil, nil
		},
	}
	eng := testEngineForMerge(t, client)
	r := eng.settlePRMergeState(settleItem(1), &stages.Stage{Name: "Validate"})
	if r.Status != PRMergeUnsettled {
		t.Errorf("expected PRMergeUnsettled for BLOCKED+no-checks (R3), got %v", r.Status)
	}
	if r.MergeableState != "blocked" {
		t.Errorf("expected MergeableState=blocked in result, got %q", r.MergeableState)
	}
}

// ── No CI configured ─────────────────────────────────────────────────────────

func TestSettle_NoCheckRuns_NoCI_ReturnsReady(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 5, State: "open", HeadSHA: "sha1"}, nil
		},
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			return boolPtr(true), "", nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			return nil, nil
		},
	}
	eng := testEngineForMerge(t, client)
	// No hadChecks, no dwell, mergeableState="" → no CI configured.
	r := eng.settlePRMergeState(settleItem(1), &stages.Stage{Name: "Validate"})
	if r.Status != PRMergeReady {
		t.Errorf("expected PRMergeReady for no check runs (no CI), got %v", r.Status)
	}
}

// ── FetchCheckRuns error ──────────────────────────────────────────────────────

func TestSettle_FetchCheckRunsError_ReturnsUnsettled(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 5, State: "open", HeadSHA: "sha1"}, nil
		},
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			return boolPtr(true), "blocked", nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			return nil, errors.New("API error")
		},
	}
	eng := testEngineForMerge(t, client)
	r := eng.settlePRMergeState(settleItem(1), &stages.Stage{Name: "Validate"})
	if r.Status != PRMergeUnsettled {
		t.Errorf("expected PRMergeUnsettled on FetchCheckRuns error, got %v", r.Status)
	}
}

// ── Check run classification ──────────────────────────────────────────────────

func TestSettle_AllChecksGreen_ReturnsReady(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 5, State: "open", HeadSHA: "sha1"}, nil
		},
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			return boolPtr(true), "blocked", nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			return []gh.CheckRun{
				{Name: "build", Status: "completed", Conclusion: "success"},
				{Name: "test", Status: "completed", Conclusion: "success"},
			}, nil
		},
	}
	eng := testEngineForMerge(t, client)
	r := eng.settlePRMergeState(settleItem(1), &stages.Stage{Name: "Validate"})
	if r.Status != PRMergeReady {
		t.Errorf("expected PRMergeReady for all-green checks, got %v", r.Status)
	}
	if len(r.CheckRuns) != 2 {
		t.Errorf("expected 2 check runs in result, got %d", len(r.CheckRuns))
	}
}

func TestSettle_ChecksPending_ReturnsUnsettled(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 5, State: "open", HeadSHA: "sha1"}, nil
		},
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			return boolPtr(true), "blocked", nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			return []gh.CheckRun{
				{Name: "build", Status: "completed", Conclusion: "success"},
				{Name: "test", Status: "in_progress"},
			}, nil
		},
	}
	eng := testEngineForMerge(t, client)
	r := eng.settlePRMergeState(settleItem(1), &stages.Stage{Name: "Validate"})
	if r.Status != PRMergeUnsettled {
		t.Errorf("expected PRMergeUnsettled for pending checks, got %v", r.Status)
	}
}

func TestSettle_ChecksFailed_ReturnsBlocked(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 5, State: "open", HeadSHA: "sha1"}, nil
		},
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			return boolPtr(true), "blocked", nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			return []gh.CheckRun{
				{Name: "build", Status: "completed", Conclusion: "failure"},
				{Name: "test", Status: "completed", Conclusion: "success"},
			}, nil
		},
	}
	eng := testEngineForMerge(t, client)
	r := eng.settlePRMergeState(settleItem(1), &stages.Stage{Name: "Validate"})
	if r.Status != PRMergeBlocked {
		t.Errorf("expected PRMergeBlocked for failed checks, got %v", r.Status)
	}
	if len(r.CheckRuns) != 2 {
		t.Errorf("expected 2 check runs in result, got %d", len(r.CheckRuns))
	}
}

func TestSettle_ChecksFailedWithTimedOut_ReturnsBlocked(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 5, State: "open", HeadSHA: "sha1"}, nil
		},
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			return boolPtr(true), "blocked", nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			return []gh.CheckRun{
				{Name: "ci", Status: "completed", Conclusion: "timed_out"},
			}, nil
		},
	}
	eng := testEngineForMerge(t, client)
	r := eng.settlePRMergeState(settleItem(1), &stages.Stage{Name: "Validate"})
	if r.Status != PRMergeBlocked {
		t.Errorf("expected PRMergeBlocked for timed_out check, got %v", r.Status)
	}
}

// ── PRChecksObserved applied on non-empty check runs ─────────────────────────

func TestSettle_NonEmptyCheckRuns_AppliesPRChecksObserved(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 5, State: "open", HeadSHA: "sha1"}, nil
		},
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			return boolPtr(true), "blocked", nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			return []gh.CheckRun{{Name: "build", Status: "completed", Conclusion: "success"}}, nil
		},
	}
	eng := testEngineForMerge(t, client)
	eng.settlePRMergeState(settleItem(1), &stages.Stage{Name: "Validate"})

	// Verify that HasHadChecks was recorded (PRChecksObserved applied).
	snap, err := eng.store.Get("owner/repo", 1)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	lpr := snap.LinkedPR()
	if lpr == nil || !lpr.HasHadChecks {
		t.Error("expected HasHadChecks=true after settle with non-empty check runs")
	}
}
