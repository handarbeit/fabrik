package engine

import (
	"context"
	"fmt"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

// ── nonDefaultBaseLabelValue ─────────────────────────────────────────────────

func TestNonDefaultBaseLabelValue(t *testing.T) {
	tests := []struct {
		name       string
		labels     []string
		pinnedBase string
		want       string
	}{
		{"no labels", nil, "main", ""},
		{"no base label", []string{"fabrik:yolo", "model:opus"}, "main", ""},
		{"matching base label agrees with pinned", []string{"base:main"}, "main", ""},
		{"empty-value base label", []string{"base:"}, "main", ""},
		{"contradicting base label", []string{"base:develop"}, "main", "develop"},
		{"multiple labels, one contradicting", []string{"fabrik:yolo", "base:develop", "model:opus"}, "main", "develop"},
		{"multiple base labels, first contradicting wins", []string{"base:develop", "base:staging"}, "main", "develop"},
		{"multiple base labels, first agrees, second contradicts", []string{"base:main", "base:develop"}, "main", "develop"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := nonDefaultBaseLabelValue(tt.labels, tt.pinnedBase)
			if got != tt.want {
				t.Errorf("nonDefaultBaseLabelValue(%v, %q) = %q, want %q", tt.labels, tt.pinnedBase, got, tt.want)
			}
		})
	}
}

// ── isDefaultPartitionKey ────────────────────────────────────────────────────

func TestIsDefaultPartitionKey(t *testing.T) {
	if !isDefaultPartitionKey("owner", "repo", "owner/repo") {
		t.Error("expected true for trainKey == owner/repo (the default-partition sentinel form)")
	}
	if !isDefaultPartitionKey("owner", "repo", mergeTrainKey("owner/repo", defaultPartitionBase)) {
		t.Error("expected true for mergeTrainKey(repoKey, defaultPartitionBase)")
	}
	if isDefaultPartitionKey("owner", "repo", "owner/repo:develop") {
		t.Error("expected false for an explicit base: partition key")
	}
	if isDefaultPartitionKey("owner", "repo", mergeTrainKey("owner/repo", "develop")) {
		t.Error("expected false for mergeTrainKey(repoKey, \"develop\")")
	}
}

// ── refuseIfBaseContradictsMembers ───────────────────────────────────────────

// defaultTrainKey returns the trainKey a default-partition worker for owner/repo
// would actually be dispatched under — mirrors prepareTrainWorker/mergeTrainKey.
func defaultTrainKey(owner, repo string) string {
	return mergeTrainKey(owner+"/"+repo, defaultPartitionBase)
}

func TestRefuseIfBaseContradictsMembers_NonDefaultPartition_NeverFetchesLabels(t *testing.T) {
	client := &mockGitHubClient{
		fetchLabelsFn: func(owner, repo string, issueNumber int) ([]string, error) {
			t.Fatalf("FetchLabels must not be called when the pinned base is not the repository default (R5 zero-cost skip)")
			return nil, nil
		},
	}
	eng := trainTestEngine(t, client, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))
	members := []trainMember{makeQueuedMember(1, 10, "Issue One")}

	refused := eng.refuseIfBaseContradictsMembers("owner", "repo", "develop", mergeTrainKey("owner/repo", "develop"), members)
	if refused {
		t.Error("expected no refusal for a non-default-base partition")
	}
}

func TestRefuseIfBaseContradictsMembers_DefaultPartition_NoContradiction_ReturnsFalse(t *testing.T) {
	client := &mockGitHubClient{
		fetchLabelsFn: func(owner, repo string, issueNumber int) ([]string, error) {
			return []string{"fabrik:yolo"}, nil
		},
	}
	eng := trainTestEngine(t, client, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))
	members := []trainMember{
		makeQueuedMember(1, 10, "Issue One"),
		makeQueuedMember(2, 11, "Issue Two"),
	}

	refused := eng.refuseIfBaseContradictsMembers("owner", "repo", "main", defaultTrainKey("owner", "repo"), members)
	if refused {
		t.Error("expected no refusal when every member's live labels agree with the pinned default base")
	}
}

func TestRefuseIfBaseContradictsMembers_AllMembersGenuinelyDefaultBase_ReturnsFalse(t *testing.T) {
	client := &mockGitHubClient{
		fetchLabelsFn: func(owner, repo string, issueNumber int) ([]string, error) {
			return nil, nil // no base: label at all
		},
	}
	eng := trainTestEngine(t, client, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))
	members := []trainMember{
		makeQueuedMember(1, 10, "Issue One"),
		makeQueuedMember(2, 11, "Issue Two"),
	}

	refused := eng.refuseIfBaseContradictsMembers("owner", "repo", "main", defaultTrainKey("owner", "repo"), members)
	if refused {
		t.Error("expected no refusal when no member declares any base: label")
	}
}

// TestRefuseIfBaseContradictsMembers_CachedEmptyButLiveContradicts is the #1688
// shape and the point of the issue (R2, Acceptance #2): the member's cached
// item.Labels carries nothing, but a live FetchLabels read reveals a
// contradicting base:develop label. The check must still refuse.
func TestRefuseIfBaseContradictsMembers_CachedEmptyButLiveContradicts(t *testing.T) {
	client := &mockGitHubClient{
		fetchLabelsFn: func(owner, repo string, issueNumber int) ([]string, error) {
			return []string{"base:develop"}, nil
		},
	}
	eng := trainTestEngine(t, client, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))

	m := makeQueuedMember(1, 10, "Issue One")
	if len(m.item.Labels) != 0 {
		t.Fatalf("test setup invariant broken: cached item.Labels must be empty, got %v", m.item.Labels)
	}

	refused := eng.refuseIfBaseContradictsMembers("owner", "repo", "main", defaultTrainKey("owner", "repo"), []trainMember{m})
	if !refused {
		t.Error("expected refusal: live labels contradict the pinned default base even though the cached snapshot is empty")
	}
}

func TestRefuseIfBaseContradictsMembers_FetchLabelsError_FailsClosed(t *testing.T) {
	client := &mockGitHubClient{
		fetchLabelsFn: func(owner, repo string, issueNumber int) ([]string, error) {
			return nil, fmt.Errorf("API rate limited")
		},
	}
	eng := trainTestEngine(t, client, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))
	members := []trainMember{makeQueuedMember(1, 10, "Issue One")}

	refused := eng.refuseIfBaseContradictsMembers("owner", "repo", "main", defaultTrainKey("owner", "repo"), members)
	if !refused {
		t.Error("expected fail-closed refusal when the live label read itself errors")
	}
}

func TestRefuseIfBaseContradictsMembers_UsesLiveClientNotReadClient(t *testing.T) {
	// A regression guard for the single most important implementation constraint
	// (Research's finding): the check must call e.client.FetchLabels, which is
	// always live, never e.readClient.FetchLabels, which boardcache.CacheImpl
	// would serve from its cached snapshot. There is no readClient wired into
	// these unit tests at all (trainTestEngine only configures e.client), so a
	// call reaching for e.readClient would panic on a nil interface — this test
	// simply confirms the check completes without doing so.
	client := &mockGitHubClient{
		fetchLabelsFn: func(owner, repo string, issueNumber int) ([]string, error) {
			return []string{"base:develop"}, nil
		},
	}
	eng := trainTestEngine(t, client, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))
	members := []trainMember{makeQueuedMember(1, 10, "Issue One")}

	refused := eng.refuseIfBaseContradictsMembers("owner", "repo", "main", defaultTrainKey("owner", "repo"), members)
	if !refused {
		t.Error("expected refusal via the live client path")
	}
}

// ── landMergeTrainBatch integration ──────────────────────────────────────────

// TestLandMergeTrainBatch_BaseContradiction_RefusesToOpenPR is Acceptance #1: pinned
// base == repo default while a member declares base:develop must refuse to open the
// integration PR and leave members in Queued.
func TestLandMergeTrainBatch_BaseContradiction_RefusesToOpenPR(t *testing.T) {
	survivors := []trainMember{
		makeQueuedMember(1, 10, "Issue One"),
		makeQueuedMember(2, 11, "Issue Two"),
	}

	createPRCalled := false
	client := &mockGitHubClient{
		listPRsFn: func(owner, repo string) ([]gh.PRDetails, error) {
			return nil, nil // no existing integration PR
		},
		createPRFn: func(owner, repo, title, head, base, body string) (int, error) {
			createPRCalled = true
			return 100, nil
		},
		fetchLabelsFn: func(owner, repo string, issueNumber int) ([]string, error) {
			if issueNumber == 2 {
				return []string{"base:develop"}, nil
			}
			return nil, nil
		},
	}
	claude := &mockClaudeInvoker{}
	wm := NewWorktreeManager(t.TempDir())
	eng := trainTestEngine(t, client, claude, wm)

	trainKey := defaultTrainKey("owner", "repo")
	state := &mergeTrainWorkerState{trialName: "merge-train-main-12345", projectID: "PVT_test"}
	eng.mergeTrainInFlight.Store(trainKey, state)

	eng.landMergeTrainBatch(context.Background(), state, "owner", "repo", "main", trainKey, survivors, wm)

	if createPRCalled {
		t.Error("CreatePR must not be called when a member's live label contradicts the pinned default base")
	}

	client.mu.Lock()
	advanced := len(client.updateStatusCalls)
	closed := len(client.closeIssueCalls)
	client.mu.Unlock()
	if advanced != 0 {
		t.Errorf("expected no board status updates after refusal, got %d", advanced)
	}
	if closed != 0 {
		t.Errorf("expected no issue/PR closures after refusal, got %d", closed)
	}
}

// TestLandMergeTrainBatch_AgreeingNonDefaultBase_ProceedsNormally is Acceptance #3:
// pinned base equals the members' declared non-default base — the check must never
// fire (it only triggers on the default partition), and the PR opens normally.
func TestLandMergeTrainBatch_AgreeingNonDefaultBase_ProceedsNormally(t *testing.T) {
	survivors := []trainMember{makeQueuedMember(1, 10, "Issue One")}

	createPRCalled := false
	client := &mockGitHubClient{
		listPRsFn: func(owner, repo string) ([]gh.PRDetails, error) { return nil, nil },
		createPRFn: func(owner, repo, title, head, base, body string) (int, error) {
			createPRCalled = true
			return 100, nil
		},
		fetchLabelsFn: func(owner, repo string, issueNumber int) ([]string, error) {
			t.Fatalf("FetchLabels must not be called for a non-default-base partition (R5)")
			return nil, nil
		},
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			tr := true
			return &tr, "clean", nil
		},
		fetchPRDetailsFn: func(owner, repo string, prNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: prNumber, MergeableState: "clean"}, nil
		},
		mergePRFn:    func(owner, repo string, prNumber int) error { return nil },
		addCommentFn: func(owner, repo string, n int, body string) (int, error) { return 1, nil },
		closeIssueFn: func(owner, repo string, n int) error { return nil },
	}
	claude := &mockClaudeInvoker{}
	wm := NewWorktreeManager(t.TempDir())
	eng := trainTestEngine(t, client, claude, wm)

	trainKey := mergeTrainKey("owner/repo", "develop")
	state := &mergeTrainWorkerState{trialName: "merge-train-develop-12345", projectID: "PVT_test"}
	eng.mergeTrainInFlight.Store(trainKey, state)

	eng.landMergeTrainBatch(context.Background(), state, "owner", "repo", "develop", trainKey, survivors, wm)

	if !createPRCalled {
		t.Error("expected CreatePR to be called normally when pinned base matches the members' declared base")
	}
}

// TestLandMergeTrainBatch_AllDefaultBase_ProceedsNormally is Acceptance #4: all
// members are genuinely default-base (no contradicting label found live) — the PR
// opens normally with no new refusal output.
func TestLandMergeTrainBatch_AllDefaultBase_ProceedsNormally(t *testing.T) {
	survivors := []trainMember{
		makeQueuedMember(1, 10, "Issue One"),
		makeQueuedMember(2, 11, "Issue Two"),
	}

	createPRCalled := false
	client := &mockGitHubClient{
		listPRsFn: func(owner, repo string) ([]gh.PRDetails, error) { return nil, nil },
		createPRFn: func(owner, repo, title, head, base, body string) (int, error) {
			createPRCalled = true
			return 100, nil
		},
		fetchLabelsFn: func(owner, repo string, issueNumber int) ([]string, error) {
			return nil, nil // no base: label — genuinely default
		},
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			tr := true
			return &tr, "clean", nil
		},
		fetchPRDetailsFn: func(owner, repo string, prNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: prNumber, MergeableState: "clean"}, nil
		},
		mergePRFn:    func(owner, repo string, prNumber int) error { return nil },
		addCommentFn: func(owner, repo string, n int, body string) (int, error) { return 1, nil },
		closeIssueFn: func(owner, repo string, n int) error { return nil },
	}
	claude := &mockClaudeInvoker{}
	wm := NewWorktreeManager(t.TempDir())
	eng := trainTestEngine(t, client, claude, wm)

	trainKey := defaultTrainKey("owner", "repo")
	state := &mergeTrainWorkerState{trialName: "merge-train-main-12345", projectID: "PVT_test"}
	eng.mergeTrainInFlight.Store(trainKey, state)

	eng.landMergeTrainBatch(context.Background(), state, "owner", "repo", "main", trainKey, survivors, wm)

	if !createPRCalled {
		t.Error("expected CreatePR to be called normally when no member declares a contradicting base")
	}
}

// TestLandMergeTrainBatch_BaseContradiction_SelfHeals is Acceptance #5: once the
// inputs agree on a later poll, the train proceeds normally.
func TestLandMergeTrainBatch_BaseContradiction_SelfHeals(t *testing.T) {
	survivors := []trainMember{makeQueuedMember(1, 10, "Issue One")}
	trainKey := defaultTrainKey("owner", "repo")

	contradicting := true
	createPRCalled := false
	client := &mockGitHubClient{
		listPRsFn: func(owner, repo string) ([]gh.PRDetails, error) { return nil, nil },
		createPRFn: func(owner, repo, title, head, base, body string) (int, error) {
			createPRCalled = true
			return 100, nil
		},
		fetchLabelsFn: func(owner, repo string, issueNumber int) ([]string, error) {
			if contradicting {
				return []string{"base:develop"}, nil
			}
			return nil, nil
		},
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			tr := true
			return &tr, "clean", nil
		},
		fetchPRDetailsFn: func(owner, repo string, prNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: prNumber, MergeableState: "clean"}, nil
		},
		mergePRFn:    func(owner, repo string, prNumber int) error { return nil },
		addCommentFn: func(owner, repo string, n int, body string) (int, error) { return 1, nil },
		closeIssueFn: func(owner, repo string, n int) error { return nil },
	}
	wm := NewWorktreeManager(t.TempDir())
	eng := trainTestEngine(t, client, &mockClaudeInvoker{}, wm)
	state := &mergeTrainWorkerState{trialName: "merge-train-main-12345", projectID: "PVT_test"}
	eng.mergeTrainInFlight.Store(trainKey, state)

	// First poll: contradiction present — refused.
	eng.landMergeTrainBatch(context.Background(), state, "owner", "repo", "main", trainKey, survivors, wm)
	if createPRCalled {
		t.Fatal("expected first poll to refuse (contradiction present)")
	}

	// Second poll: inputs now agree — proceeds normally.
	contradicting = false
	eng.landMergeTrainBatch(context.Background(), state, "owner", "repo", "main", trainKey, survivors, wm)
	if !createPRCalled {
		t.Error("expected second poll to proceed once labels no longer contradict (self-healing)")
	}
}

// ── landSingleton integration ────────────────────────────────────────────────

func TestLandSingleton_BaseContradiction_RefusesToOpenPR(t *testing.T) {
	m := makeQueuedMember(9, 90, "Issue Nine")

	createPRCalled := false
	client := &mockGitHubClient{
		createPRFn: func(owner, repo, title, head, base, body string) (int, error) {
			createPRCalled = true
			return 900, nil
		},
		fetchLabelsFn: func(owner, repo string, issueNumber int) ([]string, error) {
			return []string{"base:develop"}, nil
		},
	}
	wm := NewWorktreeManager(t.TempDir())
	eng := trainTestEngine(t, client, &mockClaudeInvoker{}, wm)
	state := &mergeTrainWorkerState{projectID: "PVT_test"}
	p := trialParams{
		owner: "owner", repo: "repo", baseBranch: "main",
		trainKey: defaultTrainKey("owner", "repo"),
		wm:       wm, holdingStg: holdingStage(eng.cfg),
	}

	eng.landSingleton(context.Background(), state, p, m, "merge-train-singleton-base")

	if createPRCalled {
		t.Error("CreatePR must not be called when the member's live label contradicts the pinned default base")
	}

	client.mu.Lock()
	advanced := len(client.updateStatusCalls)
	closed := len(client.closeIssueCalls)
	client.mu.Unlock()
	if advanced != 0 {
		t.Errorf("expected no board status updates after refusal, got %d", advanced)
	}
	if closed != 0 {
		t.Errorf("expected no issue/PR closures after refusal, got %d", closed)
	}
}

func TestLandSingleton_AgreeingBase_ProceedsNormally(t *testing.T) {
	m := makeQueuedMember(9, 90, "Issue Nine")

	createPRCalled := false
	client := &mockGitHubClient{
		createPRFn: func(owner, repo, title, head, base, body string) (int, error) {
			createPRCalled = true
			return 900, nil
		},
		fetchLabelsFn: func(owner, repo string, issueNumber int) ([]string, error) {
			return nil, nil
		},
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			tr := true
			return &tr, "clean", nil
		},
		fetchPRDetailsFn: func(owner, repo string, prNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: prNumber, MergeableState: "clean"}, nil
		},
		mergePRFn:    func(owner, repo string, prNumber int) error { return nil },
		addCommentFn: func(owner, repo string, n int, body string) (int, error) { return 1, nil },
		closeIssueFn: func(owner, repo string, n int) error { return nil },
	}
	wm := NewWorktreeManager(t.TempDir())
	eng := trainTestEngine(t, client, &mockClaudeInvoker{}, wm)
	state := &mergeTrainWorkerState{projectID: "PVT_test"}
	p := trialParams{
		owner: "owner", repo: "repo", baseBranch: "main",
		trainKey: defaultTrainKey("owner", "repo"),
		wm:       wm, holdingStg: holdingStage(eng.cfg),
	}

	eng.landSingleton(context.Background(), state, p, m, "merge-train-singleton-ok")

	if !createPRCalled {
		t.Error("expected CreatePR to be called normally when no contradiction is found")
	}
}
