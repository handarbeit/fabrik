package engine

import (
	"errors"
	"testing"

	"github.com/handarbeit/fabrik/boardcache"
	gh "github.com/handarbeit/fabrik/github"
)

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
