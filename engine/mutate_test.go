package engine

import (
	"errors"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/boardcache"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

// lastSeenSourceUpdatedAt returns the current probe staleness baseline for
// owner/repo#number, failing the test if the item isn't in the store.
func lastSeenSourceUpdatedAt(t *testing.T, eng *Engine, repo string, number int) time.Time {
	t.Helper()
	snap, err := eng.store.Get(repo, number)
	if err != nil {
		t.Fatalf("store.Get(%q, %d): %v", repo, number, err)
	}
	return snap.State().LastSeenSourceUpdatedAt
}

// ── cache() ──────────────────────────────────────────────────────────────

func TestCache_ReturnsNilWithoutCacheImpl(t *testing.T) {
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	if c := eng.cache(); c != nil {
		t.Errorf("expected nil cache when readClient is not *boardcache.CacheImpl, got %v", c)
	}
}

func TestCache_ReturnsCacheImplWhenWired(t *testing.T) {
	eng, cache := testEngineWithCache(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	if got := eng.cache(); got != cache {
		t.Errorf("expected cache() to return the wired *CacheImpl, got %v want %v", got, cache)
	}
}

// ── applyLabelAdd / addLabel ─────────────────────────────────────────────

func TestApplyLabelAdd_WriteThroughAndEcho(t *testing.T) {
	client := &mockGitHubClient{}
	eng, cache := testEngineWithCache(t, client, &mockClaudeInvoker{})
	wm, _ := newTestWebhookManager(t)
	eng.webhookMgr = wm

	item := gh.ProjectItem{Number: 1, Repo: "owner/repo"}
	eng.applyLabelAdd(item, "fabrik:paused", true)

	labels, err := cache.FetchLabels("owner", "repo", 1)
	if err != nil {
		t.Fatalf("FetchLabels: %v", err)
	}
	if !containsLabel(labels, "fabrik:paused") {
		t.Errorf("expected fabrik:paused in cache, got %v", labels)
	}

	wm.mu.Lock()
	_, gotEcho := wm.pendingEchoes[echoKey("issues", "labeled", boardcache.ItemKey("owner/repo", 1)+"+"+"fabrik:paused")]
	wm.mu.Unlock()
	if !gotEcho {
		t.Error("expected webhook echo to be registered when echo=true")
	}
}

func TestApplyLabelAdd_NoEchoWhenSuppressed(t *testing.T) {
	client := &mockGitHubClient{}
	eng, cache := testEngineWithCache(t, client, &mockClaudeInvoker{})
	wm, _ := newTestWebhookManager(t)
	eng.webhookMgr = wm

	item := gh.ProjectItem{Number: 1, Repo: "owner/repo"}
	eng.applyLabelAdd(item, "fabrik:paused", false)

	labels, err := cache.FetchLabels("owner", "repo", 1)
	if err != nil {
		t.Fatalf("FetchLabels: %v", err)
	}
	if !containsLabel(labels, "fabrik:paused") {
		t.Errorf("expected cache write-through regardless of echo, got %v", labels)
	}

	wm.mu.Lock()
	_, gotEcho := wm.pendingEchoes[echoKey("issues", "labeled", boardcache.ItemKey("owner/repo", 1)+"+"+"fabrik:paused")]
	wm.mu.Unlock()
	if gotEcho {
		t.Error("expected no webhook echo when echo=false")
	}
}

func TestApplyLabelAdd_ErrorSkipsWriteThrough(t *testing.T) {
	client := &mockGitHubClient{
		addLabelToIssueFn: func(owner, repo string, issueNumber int, labelName string) error {
			return errors.New("api error")
		},
	}
	eng, cache := testEngineWithCache(t, client, &mockClaudeInvoker{})

	item := gh.ProjectItem{Number: 1, Repo: "owner/repo"}
	eng.applyLabelAdd(item, "fabrik:paused", true)

	labels, err := cache.FetchLabels("owner", "repo", 1)
	if err != nil {
		t.Fatalf("FetchLabels: %v", err)
	}
	if containsLabel(labels, "fabrik:paused") {
		t.Errorf("expected cache unchanged on API failure, got %v", labels)
	}
}

func TestAddLabel_UsesResolvedRepoNotItemRepo(t *testing.T) {
	client := &mockGitHubClient{}
	eng, cache := testEngineWithCache(t, client, &mockClaudeInvoker{})

	// Empty item.Repo — must fall back to e.defaultRepo() ("owner/repo" in tests).
	item := gh.ProjectItem{Number: 1}
	eng.addLabel(item, "fabrik:paused")

	labels, err := cache.FetchLabels("owner", "repo", 1)
	if err != nil {
		t.Fatalf("FetchLabels: %v", err)
	}
	if !containsLabel(labels, "fabrik:paused") {
		t.Errorf("expected fabrik:paused under the resolved owner/repo cache key, got %v", labels)
	}
}

// ── applyLabelRemove / removeLabel ───────────────────────────────────────

func TestApplyLabelRemove_WriteThroughAndEcho(t *testing.T) {
	client := &mockGitHubClient{}
	eng, cache := testEngineWithCache(t, client, &mockClaudeInvoker{})
	wm, _ := newTestWebhookManager(t)
	eng.webhookMgr = wm
	cache.ApplyLabelAdded(boardcache.ItemKey("owner/repo", 1), "fabrik:blocked")

	item := gh.ProjectItem{Number: 1, Repo: "owner/repo"}
	eng.applyLabelRemove(item, "fabrik:blocked", true)

	labels, err := cache.FetchLabels("owner", "repo", 1)
	if err != nil {
		t.Fatalf("FetchLabels: %v", err)
	}
	if containsLabel(labels, "fabrik:blocked") {
		t.Errorf("expected fabrik:blocked removed from cache, got %v", labels)
	}

	wm.mu.Lock()
	_, gotEcho := wm.pendingEchoes[echoKey("issues", "unlabeled", boardcache.ItemKey("owner/repo", 1)+"+"+"fabrik:blocked")]
	wm.mu.Unlock()
	if !gotEcho {
		t.Error("expected webhook echo to be registered on successful removal")
	}
}

func TestApplyLabelRemove_ErrNotFoundSyncsCacheButNeverEchoes(t *testing.T) {
	client := &mockGitHubClient{
		removeLabelFromIssueFn: func(owner, repo string, issueNumber int, labelName string) error {
			return gh.ErrNotFound
		},
	}
	eng, cache := testEngineWithCache(t, client, &mockClaudeInvoker{})
	wm, _ := newTestWebhookManager(t)
	eng.webhookMgr = wm
	cache.ApplyLabelAdded(boardcache.ItemKey("owner/repo", 1), "fabrik:editing")

	item := gh.ProjectItem{Number: 1, Repo: "owner/repo"}
	// echo=true requested, but ErrNotFound must never echo — this is the
	// removeEditingLabel behavior Requirement 1 calls out explicitly.
	eng.applyLabelRemove(item, "fabrik:editing", true)

	labels, err := cache.FetchLabels("owner", "repo", 1)
	if err != nil {
		t.Fatalf("FetchLabels: %v", err)
	}
	if containsLabel(labels, "fabrik:editing") {
		t.Errorf("expected cache synced (label absent) on ErrNotFound idempotent removal, got %v", labels)
	}

	wm.mu.Lock()
	_, gotEcho := wm.pendingEchoes[echoKey("issues", "unlabeled", boardcache.ItemKey("owner/repo", 1)+"+"+"fabrik:editing")]
	wm.mu.Unlock()
	if gotEcho {
		t.Error("expected no webhook echo on ErrNotFound removal, even with echo=true requested")
	}
}

func TestApplyLabelRemove_NonNotFoundErrorSkipsWriteThrough(t *testing.T) {
	client := &mockGitHubClient{
		removeLabelFromIssueFn: func(owner, repo string, issueNumber int, labelName string) error {
			return errors.New("network error")
		},
	}
	eng, cache := testEngineWithCache(t, client, &mockClaudeInvoker{})
	cache.ApplyLabelAdded(boardcache.ItemKey("owner/repo", 1), "fabrik:blocked")

	item := gh.ProjectItem{Number: 1, Repo: "owner/repo"}
	eng.applyLabelRemove(item, "fabrik:blocked", true)

	labels, err := cache.FetchLabels("owner", "repo", 1)
	if err != nil {
		t.Fatalf("FetchLabels: %v", err)
	}
	if !containsLabel(labels, "fabrik:blocked") {
		t.Errorf("expected cache unchanged on non-ErrNotFound API failure, got %v", labels)
	}
}

// ── postComment / postItemComment ────────────────────────────────────────

func TestPostComment_WriteThroughEchoAndReact(t *testing.T) {
	client := &mockGitHubClient{
		addCommentFn: func(owner, repo string, issueNumber int, body string) (int, error) {
			return 42, nil
		},
	}
	eng, cache := testEngineWithCache(t, client, &mockClaudeInvoker{})
	wm, _ := newTestWebhookManager(t)
	eng.webhookMgr = wm

	item := gh.ProjectItem{Number: 1, Repo: "owner/repo"}
	// Warm up the deep-fetch cache so FetchItemDetails returns from cache after
	// the write-through (mirrors TestCommentWriteThrough's setup).
	if err := cache.FetchItemDetails(&item); err != nil {
		t.Fatalf("FetchItemDetails warm-up: %v", err)
	}
	dbID, err := eng.postComment(item, "hello", true, true)
	if err != nil {
		t.Fatalf("postComment: %v", err)
	}
	if dbID != 42 {
		t.Errorf("expected dbID 42, got %d", dbID)
	}

	itemAfter := gh.ProjectItem{Number: 1, Repo: "owner/repo"}
	if err := cache.FetchItemDetails(&itemAfter); err != nil {
		t.Fatalf("FetchItemDetails: %v", err)
	}
	if !containsCommentByID(itemAfter.Comments, 42) {
		t.Errorf("expected comment 42 in cache, got %v", itemAfter.Comments)
	}

	wm.mu.Lock()
	_, gotEcho := wm.pendingEchoes[echoKey("issue_comment", "created", boardcache.ItemKey("owner/repo", 1))]
	wm.mu.Unlock()
	if !gotEcho {
		t.Error("expected webhook echo to be registered when echo=true")
	}

	if len(client.addCommentReactionCalls) != 1 {
		t.Errorf("expected 1 rocket-react call when react=true, got %d", len(client.addCommentReactionCalls))
	}
}

func TestPostComment_NoEchoNoReactWhenSuppressed(t *testing.T) {
	client := &mockGitHubClient{
		addCommentFn: func(owner, repo string, issueNumber int, body string) (int, error) {
			return 42, nil
		},
	}
	eng, _ := testEngineWithCache(t, client, &mockClaudeInvoker{})
	wm, _ := newTestWebhookManager(t)
	eng.webhookMgr = wm

	item := gh.ProjectItem{Number: 1, Repo: "owner/repo"}
	if _, err := eng.postComment(item, "hello", false, false); err != nil {
		t.Fatalf("postComment: %v", err)
	}

	wm.mu.Lock()
	_, gotEcho := wm.pendingEchoes[echoKey("issue_comment", "created", boardcache.ItemKey("owner/repo", 1))]
	wm.mu.Unlock()
	if gotEcho {
		t.Error("expected no webhook echo when echo=false")
	}
	if len(client.addCommentReactionCalls) != 0 {
		t.Errorf("expected no rocket-react call when react=false, got %d", len(client.addCommentReactionCalls))
	}
}

func TestPostComment_ErrorSkipsWriteThrough(t *testing.T) {
	apiErr := errors.New("rate limited")
	client := &mockGitHubClient{
		addCommentFn: func(owner, repo string, issueNumber int, body string) (int, error) {
			return 0, apiErr
		},
	}
	eng, _ := testEngineWithCache(t, client, &mockClaudeInvoker{})

	item := gh.ProjectItem{Number: 1, Repo: "owner/repo"}
	dbID, err := eng.postComment(item, "hello", true, true)
	if !errors.Is(err, apiErr) {
		t.Errorf("expected apiErr, got %v", err)
	}
	if dbID != 0 {
		t.Errorf("expected dbID 0 on error, got %d", dbID)
	}
	if len(client.addCommentReactionCalls) != 0 {
		t.Error("expected no reaction attempt when AddComment failed")
	}
}

func TestPostItemComment_AlwaysEchoesAndSwallowsError(t *testing.T) {
	apiErr := errors.New("rate limited")
	client := &mockGitHubClient{
		addCommentFn: func(owner, repo string, issueNumber int, body string) (int, error) {
			return 0, apiErr
		},
	}
	eng, _ := testEngineWithCache(t, client, &mockClaudeInvoker{})

	item := gh.ProjectItem{Number: 1, Repo: "owner/repo"}
	if got := eng.postItemComment(item, "hello", false); got != 0 {
		t.Errorf("expected 0 on AddComment failure, got %d", got)
	}
}

// ── pauseIssue ────────────────────────────────────────────────────────────

// Pattern A: 10 of the 11 pauseFor* functions — awaiting-input added, comment
// rocket-reacted, neither label nor comment echoed, auto-merge untouched.
func TestPauseIssue_PatternA(t *testing.T) {
	client := &mockGitHubClient{
		addCommentFn: func(owner, repo string, issueNumber int, body string) (int, error) {
			return 7, nil
		},
	}
	eng, cache := testEngineWithCache(t, client, &mockClaudeInvoker{})
	wm, _ := newTestWebhookManager(t)
	eng.webhookMgr = wm

	item := gh.ProjectItem{Number: 1, Repo: "owner/repo"}
	eng.pauseIssue(item, "paused message", pauseOpts{
		awaitingInput: true,
		reactRocket:   true,
		labelEcho:     false,
		commentEcho:   false,
	})

	labels, _ := cache.FetchLabels("owner", "repo", 1)
	if !containsLabel(labels, "fabrik:paused") || !containsLabel(labels, "fabrik:awaiting-input") {
		t.Errorf("expected both fabrik:paused and fabrik:awaiting-input in cache, got %v", labels)
	}
	if len(client.addCommentReactionCalls) != 1 {
		t.Errorf("expected 1 rocket-react call, got %d", len(client.addCommentReactionCalls))
	}

	wm.mu.Lock()
	_, labelEchoed := wm.pendingEchoes[echoKey("issues", "labeled", boardcache.ItemKey("owner/repo", 1)+"+"+"fabrik:paused")]
	_, commentEchoed := wm.pendingEchoes[echoKey("issue_comment", "created", boardcache.ItemKey("owner/repo", 1))]
	wm.mu.Unlock()
	if labelEchoed {
		t.Error("Pattern A must not echo label writes")
	}
	if commentEchoed {
		t.Error("Pattern A must not echo the comment write")
	}
}

// Pattern B: escalatePRCreationFailure / escalateFailedStage — no
// awaiting-input, comment rocket-reacted, both label and comment echoed,
// fabrik:paused added *before* the comment is posted (labelFirst) — matching
// the original hand-inlined order at these two call sites.
func TestPauseIssue_PatternB(t *testing.T) {
	var order []string
	client := &mockGitHubClient{
		addCommentFn: func(owner, repo string, issueNumber int, body string) (int, error) {
			order = append(order, "comment")
			return 7, nil
		},
		addLabelToIssueFn: func(owner, repo string, issueNumber int, labelName string) error {
			order = append(order, "label:"+labelName)
			return nil
		},
	}
	eng, cache := testEngineWithCache(t, client, &mockClaudeInvoker{})
	wm, _ := newTestWebhookManager(t)
	eng.webhookMgr = wm

	item := gh.ProjectItem{Number: 1, Repo: "owner/repo"}
	eng.pauseIssue(item, "escalation message", pauseOpts{
		awaitingInput: false,
		reactRocket:   true,
		labelEcho:     true,
		commentEcho:   true,
		labelFirst:    true,
	})

	labels, _ := cache.FetchLabels("owner", "repo", 1)
	if !containsLabel(labels, "fabrik:paused") {
		t.Errorf("expected fabrik:paused in cache, got %v", labels)
	}
	if containsLabel(labels, "fabrik:awaiting-input") {
		t.Errorf("Pattern B must not add fabrik:awaiting-input, got %v", labels)
	}

	if want := []string{"label:fabrik:paused", "comment"}; len(order) != len(want) || order[0] != want[0] || order[1] != want[1] {
		t.Errorf("Pattern B must add fabrik:paused before posting the comment, got order %v", order)
	}

	wm.mu.Lock()
	_, labelEchoed := wm.pendingEchoes[echoKey("issues", "labeled", boardcache.ItemKey("owner/repo", 1)+"+"+"fabrik:paused")]
	_, commentEchoed := wm.pendingEchoes[echoKey("issue_comment", "created", boardcache.ItemKey("owner/repo", 1))]
	wm.mu.Unlock()
	if !labelEchoed {
		t.Error("Pattern B must echo the label write")
	}
	if !commentEchoed {
		t.Error("Pattern B must echo the comment write")
	}
}

// Pattern C: pauseForBrokenLinkage — no awaiting-input, no rocket reaction, no
// comment echo, but the label write is still echoed (via addPausedLabelToItem),
// and fabrik:paused is added *before* the comment is posted (labelFirst) —
// matching the original hand-inlined order at this call site.
func TestPauseIssue_PatternC(t *testing.T) {
	var order []string
	client := &mockGitHubClient{
		addCommentFn: func(owner, repo string, issueNumber int, body string) (int, error) {
			order = append(order, "comment")
			return 7, nil
		},
		addLabelToIssueFn: func(owner, repo string, issueNumber int, labelName string) error {
			order = append(order, "label:"+labelName)
			return nil
		},
	}
	eng, cache := testEngineWithCache(t, client, &mockClaudeInvoker{})
	wm, _ := newTestWebhookManager(t)
	eng.webhookMgr = wm

	item := gh.ProjectItem{Number: 1, Repo: "owner/repo"}
	eng.pauseIssue(item, "broken linkage message", pauseOpts{
		awaitingInput: false,
		reactRocket:   false,
		labelEcho:     true,
		commentEcho:   false,
		labelFirst:    true,
	})

	labels, _ := cache.FetchLabels("owner", "repo", 1)
	if !containsLabel(labels, "fabrik:paused") {
		t.Errorf("expected fabrik:paused in cache, got %v", labels)
	}
	if containsLabel(labels, "fabrik:awaiting-input") {
		t.Errorf("Pattern C must not add fabrik:awaiting-input, got %v", labels)
	}
	if len(client.addCommentReactionCalls) != 0 {
		t.Errorf("Pattern C must not rocket-react, got %d calls", len(client.addCommentReactionCalls))
	}

	if want := []string{"label:fabrik:paused", "comment"}; len(order) != len(want) || order[0] != want[0] || order[1] != want[1] {
		t.Errorf("Pattern C must add fabrik:paused before posting the comment, got order %v", order)
	}

	wm.mu.Lock()
	_, labelEchoed := wm.pendingEchoes[echoKey("issues", "labeled", boardcache.ItemKey("owner/repo", 1)+"+"+"fabrik:paused")]
	_, commentEchoed := wm.pendingEchoes[echoKey("issue_comment", "created", boardcache.ItemKey("owner/repo", 1))]
	wm.mu.Unlock()
	if !labelEchoed {
		t.Error("Pattern C must still echo the label write")
	}
	if commentEchoed {
		t.Error("Pattern C must not echo the comment write")
	}
}

// Pattern A order: the 10 pauseFor* functions historically posted their
// comment before adding fabrik:paused — pauseIssue's default (labelFirst:
// false) must preserve that order.
func TestPauseIssue_PatternA_CommentBeforeLabel(t *testing.T) {
	var order []string
	client := &mockGitHubClient{
		addCommentFn: func(owner, repo string, issueNumber int, body string) (int, error) {
			order = append(order, "comment")
			return 7, nil
		},
		addLabelToIssueFn: func(owner, repo string, issueNumber int, labelName string) error {
			order = append(order, "label:"+labelName)
			return nil
		},
	}
	eng, _ := testEngineWithCache(t, client, &mockClaudeInvoker{})

	item := gh.ProjectItem{Number: 1, Repo: "owner/repo"}
	eng.pauseIssue(item, "paused message", pauseOpts{
		awaitingInput: true,
		reactRocket:   true,
	})

	if want := []string{"comment", "label:fabrik:paused", "label:fabrik:awaiting-input"}; len(order) != len(want) {
		t.Fatalf("Pattern A order mismatch, got %v want %v", order, want)
	} else {
		for i := range want {
			if order[i] != want[i] {
				t.Errorf("Pattern A order mismatch at index %d: got %v want %v", i, order, want)
				break
			}
		}
	}
}

func TestPauseIssue_RemoveAutoMerge(t *testing.T) {
	client := &mockGitHubClient{
		addCommentFn: func(owner, repo string, issueNumber int, body string) (int, error) {
			return 7, nil
		},
	}
	eng, cache := testEngineWithCache(t, client, &mockClaudeInvoker{})
	cache.ApplyLabelAdded(boardcache.ItemKey("owner/repo", 1), "fabrik:auto-merge-enabled")

	item := gh.ProjectItem{Number: 1, Repo: "owner/repo"}
	eng.pauseIssue(item, "convergence failed", pauseOpts{
		awaitingInput:   true,
		reactRocket:     true,
		removeAutoMerge: true,
	})

	labels, _ := cache.FetchLabels("owner", "repo", 1)
	if containsLabel(labels, "fabrik:auto-merge-enabled") {
		t.Errorf("expected fabrik:auto-merge-enabled removed from cache, got %v", labels)
	}
	if !containsLabel(labels, "fabrik:paused") || !containsLabel(labels, "fabrik:awaiting-input") {
		t.Errorf("expected pause labels present, got %v", labels)
	}
}

// githubWriteAt is the GitHub-side stamp the mock reports for an engine write
// (#1945). It is deliberately far from the host clock, so an assertion that the
// baseline equals it cannot be satisfied by a time.Now() fallback.
var githubWriteAt = time.Date(2026, 10, 19, 4, 0, 7, 0, time.UTC)

// ── SelfWriteObserved probe-staleness baseline (#1090) ───────────────────────
//
// These tests exercise the same three engine/mutate.go call sites as above,
// but assert against the itemstate.Store's LastSeenSourceUpdatedAt baseline
// rather than the boardcache write-through — the two are deliberately
// independent mechanisms (see engine/mutate.go comments), so a passing
// write-through test above does not imply the baseline advanced correctly.

func TestApplyLabelAdd_AdvancesStaleness(t *testing.T) {
	client := &mockGitHubClient{}
	client.fixedWriteStamps(githubWriteAt)
	eng, _ := testEngineWithCache(t, client, &mockClaudeInvoker{})

	item := gh.ProjectItem{Number: 1, Repo: "owner/repo"}
	eng.applyLabelAdd(item, "fabrik:paused", true)

	if got := lastSeenSourceUpdatedAt(t, eng, "owner/repo", 1); !got.Equal(githubWriteAt) {
		t.Errorf("LastSeenSourceUpdatedAt = %v; want GitHub's stamp %v after a successful label add", got, githubWriteAt)
	}
}

func TestApplyLabelAdd_ErrorDoesNotAdvanceStaleness(t *testing.T) {
	client := &mockGitHubClient{
		addLabelToIssueFn: func(owner, repo string, issueNumber int, labelName string) error {
			return errors.New("api error")
		},
	}
	eng, _ := testEngineWithCache(t, client, &mockClaudeInvoker{})

	item := gh.ProjectItem{Number: 1, Repo: "owner/repo"}
	eng.applyLabelAdd(item, "fabrik:paused", true)

	if got := lastSeenSourceUpdatedAt(t, eng, "owner/repo", 1); !got.IsZero() {
		t.Errorf("LastSeenSourceUpdatedAt = %v; want unchanged (zero) on a failed label add", got)
	}
}

// TestApplyLabelRemove_SuccessAdvancesStaleness covers the genuine-removal
// path (echo=true), where syncLabelRemoval must advance the baseline.
func TestApplyLabelRemove_SuccessAdvancesStaleness(t *testing.T) {
	client := &mockGitHubClient{}
	client.fixedWriteStamps(githubWriteAt)
	eng, cache := testEngineWithCache(t, client, &mockClaudeInvoker{})
	cache.ApplyLabelAdded(boardcache.ItemKey("owner/repo", 1), "fabrik:blocked")

	item := gh.ProjectItem{Number: 1, Repo: "owner/repo"}
	eng.applyLabelRemove(item, "fabrik:blocked", true)

	if got := lastSeenSourceUpdatedAt(t, eng, "owner/repo", 1); !got.Equal(githubWriteAt) {
		t.Errorf("LastSeenSourceUpdatedAt = %v; want GitHub's stamp %v after a successful label removal", got, githubWriteAt)
	}
}

// TestApplyLabelRemove_ErrNotFoundDoesNotAdvanceStaleness covers the
// idempotent-no-op path (echo=false on gh.ErrNotFound): no real GitHub change
// occurred, so the staleness baseline must not advance, mirroring the
// adjacent RegisterEcho suppression.
func TestApplyLabelRemove_ErrNotFoundDoesNotAdvanceStaleness(t *testing.T) {
	client := &mockGitHubClient{
		removeLabelFromIssueFn: func(owner, repo string, issueNumber int, labelName string) error {
			return gh.ErrNotFound
		},
	}
	eng, cache := testEngineWithCache(t, client, &mockClaudeInvoker{})
	cache.ApplyLabelAdded(boardcache.ItemKey("owner/repo", 1), "fabrik:editing")

	item := gh.ProjectItem{Number: 1, Repo: "owner/repo"}
	eng.applyLabelRemove(item, "fabrik:editing", true)

	if got := lastSeenSourceUpdatedAt(t, eng, "owner/repo", 1); !got.IsZero() {
		t.Errorf("LastSeenSourceUpdatedAt = %v; want unchanged (zero) on an ErrNotFound no-op removal", got)
	}
}

func TestPostComment_SuccessAdvancesStaleness(t *testing.T) {
	client := &mockGitHubClient{
		addCommentFn: func(owner, repo string, issueNumber int, body string) (int, error) {
			return 42, nil
		},
	}
	client.fixedWriteStamps(githubWriteAt)
	eng, _ := testEngineWithCache(t, client, &mockClaudeInvoker{})

	item := gh.ProjectItem{Number: 1, Repo: "owner/repo"}
	if _, err := eng.postComment(item, "hello", false, true); err != nil {
		t.Fatalf("postComment: %v", err)
	}

	if got := lastSeenSourceUpdatedAt(t, eng, "owner/repo", 1); !got.Equal(githubWriteAt) {
		t.Errorf("LastSeenSourceUpdatedAt = %v; want GitHub's stamp %v after a successful comment post", got, githubWriteAt)
	}
}

func TestPostComment_ErrorDoesNotAdvanceStaleness(t *testing.T) {
	client := &mockGitHubClient{
		addCommentFn: func(owner, repo string, issueNumber int, body string) (int, error) {
			return 0, errors.New("rate limited")
		},
	}
	eng, _ := testEngineWithCache(t, client, &mockClaudeInvoker{})

	item := gh.ProjectItem{Number: 1, Repo: "owner/repo"}
	if _, err := eng.postComment(item, "hello", false, true); err == nil {
		t.Fatal("expected an error from postComment")
	}

	if got := lastSeenSourceUpdatedAt(t, eng, "owner/repo", 1); !got.IsZero() {
		t.Errorf("LastSeenSourceUpdatedAt = %v; want unchanged (zero) on a failed comment post", got)
	}
}

// TestApplyLabelAdd_AdvancesStalenessWithoutWebhookMgr verifies the "must work
// identically in polling-only mode" requirement (#1090): the baseline advance
// must not be gated on e.webhookMgr, unlike the adjacent RegisterEcho call.
func TestApplyLabelAdd_AdvancesStalenessWithoutWebhookMgr(t *testing.T) {
	client := &mockGitHubClient{}
	client.fixedWriteStamps(githubWriteAt)
	eng, _ := testEngineWithCache(t, client, &mockClaudeInvoker{})
	eng.webhookMgr = nil // polling-only mode

	item := gh.ProjectItem{Number: 1, Repo: "owner/repo"}
	eng.applyLabelAdd(item, "fabrik:paused", true)

	if got := lastSeenSourceUpdatedAt(t, eng, "owner/repo", 1); !got.Equal(githubWriteAt) {
		t.Errorf("LastSeenSourceUpdatedAt = %v; want GitHub's stamp %v in polling-only mode (webhookMgr == nil)", got, githubWriteAt)
	}
}

// ── removeLabelChecked (#1969) ───────────────────────────────────────────

func TestRemoveLabelChecked(t *testing.T) {
	cases := []struct {
		name        string
		clientErr   error
		wantErr     bool
		wantRemoved bool // cache write-through happened
	}{
		{"success", nil, false, true},
		{"already absent is success", gh.ErrNotFound, false, true},
		{"other error is returned and writes nothing through", errors.New("network error"), true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &mockGitHubClient{
				removeLabelFromIssueFn: func(owner, repo string, issueNumber int, labelName string) error {
					return tc.clientErr
				},
			}
			eng, cache := testEngineWithCache(t, client, &mockClaudeInvoker{})
			cache.ApplyLabelAdded(boardcache.ItemKey("owner/repo", 1), "fabrik:yolo")

			err := eng.removeLabelChecked(gh.ProjectItem{Number: 1, Repo: "owner/repo"}, "fabrik:yolo")
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr && !errors.Is(err, tc.clientErr) {
				t.Errorf("err = %v, want it to wrap %v", err, tc.clientErr)
			}
			labels, _ := cache.FetchLabels("owner", "repo", 1)
			if has := containsLabel(labels, "fabrik:yolo"); has == tc.wantRemoved {
				t.Errorf("label present=%v after call, wantRemoved=%v", has, tc.wantRemoved)
			}
		})
	}
}

// ── requester attribution (#2125) ────────────────────────────────────────

func TestLabelCheckedBy_CarriesRequesterOnlyOnSuccess(t *testing.T) {
	var addErr error
	client := &mockGitHubClient{
		addLabelToIssueFn: func(owner, repo string, issueNumber int, labelName string) error { return addErr },
	}
	eng, _ := testEngineWithCache(t, client, &mockClaudeInvoker{})
	var got []itemstate.Change
	eng.store.Subscribe(itemstate.ObserverFunc(func(c itemstate.Change, _ itemstate.Snapshot) { got = append(got, c) }))
	item := gh.ProjectItem{Number: 1, Repo: "owner/repo"}

	// A failed write applies no mutation, so nothing is left to mislabel a later write.
	addErr = errors.New("network error")
	if err := eng.addLabelCheckedBy(item, "fabrik:cruise", "x"); err == nil {
		t.Fatal("expected error")
	}
	if len(got) != 0 {
		t.Fatalf("failed write produced changes: %+v", got)
	}

	addErr = nil
	if err := eng.addLabelCheckedBy(item, "fabrik:cruise", "x"); err != nil {
		t.Fatal(err)
	}
	if err := eng.removeLabelCheckedBy(item, "fabrik:cruise", "x"); err != nil {
		t.Fatal(err)
	}
	// The same label written by the engine itself afterwards is not attributed.
	if err := eng.addLabelChecked(item, "fabrik:cruise"); err != nil {
		t.Fatal(err)
	}
	var reqs []string
	for _, c := range got {
		if len(c.LabelDeltas) == 1 {
			reqs = append(reqs, c.Requester)
		}
	}
	if len(reqs) != 3 || reqs[0] != "x" || reqs[1] != "x" || reqs[2] != "" {
		t.Fatalf("requesters = %q (changes %+v)", reqs, got)
	}
}

func TestRemoveLabelCheckedBy_NotFoundRegistersNoEcho(t *testing.T) {
	client := &mockGitHubClient{
		removeLabelFromIssueFn: func(owner, repo string, issueNumber int, labelName string) error { return gh.ErrNotFound },
	}
	eng, _ := testEngineWithCache(t, client, &mockClaudeInvoker{})
	wm, _ := newTestWebhookManager(t)
	wm.mu.Lock()
	wm.state = WebhookStreamHealthy
	wm.mu.Unlock()
	eng.webhookMgr = wm

	if err := eng.removeLabelCheckedBy(gh.ProjectItem{Number: 1, Repo: "owner/repo"}, "fabrik:yolo", "x"); err != nil {
		t.Fatal(err)
	}
	wm.mu.Lock()
	defer wm.mu.Unlock()
	if len(wm.pendingEchoes) != 0 || len(wm.echoVia) != 0 {
		t.Fatalf("echo registered for an already-absent label: %v %v", wm.pendingEchoes, wm.echoVia)
	}
}

func TestAddLabelCheckedBy_RegistersEchoWithRequester(t *testing.T) {
	eng, _ := testEngineWithCache(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	wm, _ := newTestWebhookManager(t)
	wm.mu.Lock()
	wm.state = WebhookStreamHealthy
	wm.mu.Unlock()
	eng.webhookMgr = wm

	if err := eng.addLabelCheckedBy(gh.ProjectItem{Number: 1, Repo: "owner/repo"}, "fabrik:cruise", "x"); err != nil {
		t.Fatal(err)
	}
	ok, via := wm.MatchEchoVia("issues", "labeled", "owner/repo#1+fabrik:cruise")
	if !ok || via != "x" {
		t.Fatalf("MatchEchoVia = %v, %q", ok, via)
	}
}

// A write that yields no GitHub timestamp must not advance the baseline, and
// must never fall back to the host clock (#1945, FR-003, SC-005).
func TestSelfWrite_NoGitHubStampLeavesBaseline(t *testing.T) {
	client := &mockGitHubClient{} // reports a zero stamp
	eng, _ := testEngineWithCache(t, client, &mockClaudeInvoker{})
	item := gh.ProjectItem{Number: 1, Repo: "owner/repo"}
	eng.applyLabelAdd(item, "fabrik:paused", true)
	if _, err := eng.postComment(item, "hello", false, true); err != nil {
		t.Fatalf("postComment: %v", err)
	}
	if got := lastSeenSourceUpdatedAt(t, eng, "owner/repo", 1); !got.IsZero() {
		t.Errorf("LastSeenSourceUpdatedAt = %v; want zero when GitHub reported no stamp", got)
	}
}

// A client without the WriteStampSource capability behaves as "no stamp".
func TestSelfWrite_ClientWithoutCapabilityLeavesBaseline(t *testing.T) {
	inner := &mockGitHubClient{}
	inner.fixedWriteStamps(githubWriteAt)
	eng, _ := testEngineWithCache(t, inner, &mockClaudeInvoker{})
	eng.client = struct{ GitHubClient }{inner} // hides the stamp methods
	eng.applyLabelAdd(gh.ProjectItem{Number: 1, Repo: "owner/repo"}, "fabrik:paused", true)
	if got := lastSeenSourceUpdatedAt(t, eng, "owner/repo", 1); !got.IsZero() {
		t.Errorf("LastSeenSourceUpdatedAt = %v; want zero for a client with no WriteStampSource", got)
	}
}
