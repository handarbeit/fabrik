package engine

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// R1 (#2550): after a transport-class failure of a lock/state label write the
// engine asks GitHub what happened. Three outcomes: applied, not applied, and a
// failed re-read.

func reconcileEngine(t *testing.T, labels []string, readErr error) (*Engine, *mockGitHubClient, *atomic.Int32) {
	t.Helper()
	var reads atomic.Int32
	client := &mockGitHubClient{
		fetchLabelsFn: func(string, string, int) ([]string, error) {
			reads.Add(1)
			return labels, readErr
		},
	}
	return testEngineWithRepo(t, client, &mockClaudeInvoker{}), client, &reads
}

func TestReconcileLabelWrite_Outcomes(t *testing.T) {
	item := gh.ProjectItem{Number: 70, Repo: "owner/repo"}
	cases := []struct {
		name        string
		labels      []string
		readErr     error
		wantPresent bool
		writeErr    error
		want        labelWriteOutcome
		wantReads   int32
	}{
		{"add landed", []string{"fabrik:editing"}, nil, true, errTimeout, labelWriteApplied, 1},
		{"add not landed", []string{"bug"}, nil, true, errTimeout, labelWriteNotApplied, 1},
		{"add, re-read failed", nil, errTimeout, true, errTimeout, labelWriteUnknown, 1},
		{"removal landed (label gone)", []string{"bug"}, nil, false, errTimeout, labelWriteApplied, 1},
		{"removal not landed (label still there)", []string{"fabrik:editing"}, nil, false, errTimeout, labelWriteNotApplied, 1},
		{"removal, re-read failed", nil, errTimeout, false, errTimeout, labelWriteUnknown, 1},
		{"non-transport error is an answer, no re-read", []string{"fabrik:editing"}, nil, true, errors.New("GitHub API returned 422: nope"), labelWriteUnreconciled, 0},
		{"nil error", []string{"fabrik:editing"}, nil, true, nil, labelWriteUnreconciled, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eng, _, reads := reconcileEngine(t, tc.labels, tc.readErr)
			if got := eng.reconcileLabelWrite(item, "fabrik:editing", tc.wantPresent, tc.writeErr); got != tc.want {
				t.Fatalf("outcome = %v, want %v", got, tc.want)
			}
			if got := reads.Load(); got != tc.wantReads {
				t.Errorf("FetchLabels calls = %d, want %d", got, tc.wantReads)
			}
		})
	}
}

func TestReconcileLabelWrite_NeutralisedSeamMakesNoRead(t *testing.T) {
	eng, _, reads := reconcileEngine(t, []string{"fabrik:editing"}, nil)
	eng.SetAmbiguousWriteReconcileDisabledForTest(true)
	if got := eng.reconcileLabelWrite(gh.ProjectItem{Number: 1, Repo: "owner/repo"}, "fabrik:editing", true, errTimeout); got != labelWriteUnreconciled {
		t.Fatalf("outcome = %v, want unreconciled", got)
	}
	if reads.Load() != 0 {
		t.Error("the neutralised reconcile still read labels")
	}
}

// The comment pass (Step 1): a fabrik:editing add that landed behind a timeout
// is the pass's own lock — no refusal, no cooldown.
func commentPassWithEditingAddTimeout(t *testing.T, labels []string, readErr error) (kind string, eng *Engine, item gh.ProjectItem) {
	t.Helper()
	skipIfNoGit(t)
	eng, client, _ := reconcileEngine(t, labels, readErr)
	client.addLabelToIssueFn = func(_, _ string, _ int, label string) error {
		if label == "fabrik:editing" {
			return errTimeout
		}
		return nil
	}
	eng.cfg.PollSeconds = 30
	item = gh.ProjectItem{Number: 71, Body: "spec"}
	comments := []gh.Comment{{ID: "C_1", DatabaseID: 1, Author: "alice", Body: "please fix"}}
	k, err := eng.processCommentsClassified(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item, &stages.Stage{Name: "Review", Order: 1}, comments)
	if err != nil && k == didNotRunEditingWrite {
		t.Fatalf("unexpected error with refusal kind: %v", err)
	}
	return string(k), eng, item
}

func TestCommentPass_EditingAddLandedBehindTimeout_Proceeds(t *testing.T) {
	kind, eng, item := commentPassWithEditingAddTimeout(t, []string{"fabrik:editing"}, nil)
	if kind == string(didNotRunEditingWrite) {
		t.Fatal("pass refused although GitHub applied fabrik:editing")
	}
	if snap, err := eng.store.Get("owner/repo", item.Number); err == nil && !snap.CooldownAt(lockWriteRetryReason).IsZero() {
		t.Error("lock-write-failed cooldown recorded for a write that landed")
	}
}

func TestCommentPass_EditingAddNotLanded_RefusesUncharged(t *testing.T) {
	for name, readErr := range map[string]error{"absent": nil, "re-read failed": errTimeout} {
		t.Run(name, func(t *testing.T) {
			kind, eng, item := commentPassWithEditingAddTimeout(t, nil, readErr)
			if kind != string(didNotRunEditingWrite) {
				t.Fatalf("kind = %q, want didNotRunEditingWrite", kind)
			}
			snap, err := eng.store.Get("owner/repo", item.Number)
			if err != nil {
				t.Fatal(err)
			}
			if snap.CooldownAt(lockWriteRetryReason).IsZero() {
				t.Error("no lock-write-failed cooldown recorded")
			}
			if got := eng.commentBreakerCount(item); got != 0 {
				t.Errorf("breaker charged: %d", got)
			}
		})
	}
}

// The stage lock: a lock add that landed behind a timeout proceeds as holder and
// the in_progress marker that landed is removed by release().
func TestAcquireLockAndVerify_AmbiguousAddsLandedProceedAsHolder(t *testing.T) {
	var removed []string
	client := &mockGitHubClient{
		addLabelToIssueFn: func(_, _ string, _ int, label string) error { return errTimeout },
		fetchLabelsFn: func(string, string, int) ([]string, error) {
			return []string{"fabrik:locked:testuser", "stage:Research:in_progress"}, nil
		},
		removeLabelFromIssueFn: func(_, _ string, _ int, label string) error {
			removed = append(removed, label)
			return nil
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	release, _, workerDone, ok, lockErr := eng.acquireLockAndVerify(context.Background(), gh.ProjectItem{Number: 72}, &stages.Stage{Name: "Research"}, "owner", "repo", "owner/repo", "fabrik:locked:testuser")
	if !ok || lockErr != nil {
		t.Fatalf("ok=%v lockErr=%v, want holder", ok, lockErr)
	}
	if workerDone == nil {
		t.Error("no heartbeat channel: the lock was not treated as acquired")
	}
	release()
	got := map[string]bool{}
	for _, l := range removed {
		got[l] = true
	}
	if !got["fabrik:locked:testuser"] || !got["stage:Research:in_progress"] {
		t.Errorf("release removed %v; both landed labels must be removed", removed)
	}
}

// A removal that fails with a timeout is reconciled with exactly one live
// re-read, and the label is gone from the Store when GitHub shows it absent.
func TestApplyLabelRemove_AmbiguousFailureLanded(t *testing.T) {
	var reads atomic.Int32
	client := &mockGitHubClient{
		removeLabelFromIssueFn: func(string, string, int, string) error { return errTimeout },
		fetchLabelsFn: func(string, string, int) ([]string, error) {
			reads.Add(1)
			return []string{"bug"}, nil
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	eng.removeLabel(gh.ProjectItem{Number: 73, Repo: "owner/repo"}, "fabrik:locked:testuser")
	if reads.Load() != 1 {
		t.Errorf("FetchLabels calls = %d, want 1", reads.Load())
	}
}

// removeEditingLabel keeps its retry loop and reconciles once after it gives up.
func TestRemoveEditingLabel_AmbiguousFailureReconcilesAfterRetries(t *testing.T) {
	var removes, reads atomic.Int32
	client := &mockGitHubClient{
		removeLabelFromIssueFn: func(string, string, int, string) error { removes.Add(1); return errTimeout },
		fetchLabelsFn: func(string, string, int) ([]string, error) {
			reads.Add(1)
			return nil, nil // label absent: the removal landed
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	eng.removeEditingLabel("owner", "repo", 74)
	if removes.Load() < 1 {
		t.Fatal("removal never attempted")
	}
	if reads.Load() != 1 {
		t.Errorf("FetchLabels calls = %d, want exactly 1 after the retries", reads.Load())
	}
}
