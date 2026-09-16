package engine

import (
	"context"
	"path/filepath"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/githubauth"
)

// appAuthTestReconciler builds a real *githubauth.Reconciler against a fake
// GitHub App server, mirroring TestRun_ShutdownOnSignal_WithGitHubAppAuth_WaitsForRefreshLoop's
// pattern (github_app_auth_test.go) — Reconciler.botLogin is unexported and
// only ever set inside Reconcile()'s success path, so this is the only way
// to obtain a Reconciler with a known BotLogin() value in tests.
func appAuthTestReconciler(t *testing.T) *githubauth.Reconciler {
	t.Helper()
	dir := t.TempDir()
	keyPath := writeEngineTestAppKey(t, dir)
	srv := newFakeGitHubAppServer(t, 999, "handarbeit", "organization", RequiredGitHubAppPermissions(false))

	reconciler, err := githubauth.Reconcile(context.Background(), githubauth.Options{
		AppID: 42, AppInstallationID: 999, AppPrivateKeyPath: keyPath,
		AppStatePath: filepath.Join(dir, "app-state.json"),
		WatchedRepos: []string{"handarbeit/*"},
		BaseURL:      srv.URL,
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	return reconciler
}

// TestSelfLogin_PATMode_ReturnsCfgUser pins AC2 (byte-identical PAT-mode
// behavior): with no ghAppAuth configured, selfLogin() must return cfg.User
// exactly as every pre-#1754 call site's e.cfg.User comparison did.
func TestSelfLogin_PATMode_ReturnsCfgUser(t *testing.T) {
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	if got := eng.selfLogin(); got != "testuser" {
		t.Errorf("selfLogin() = %q, want cfg.User %q", got, "testuser")
	}
}

// TestSelfLogin_AppAuthMode_ReturnsBotLogin pins AC1's foundation: under App
// auth, selfLogin() must return the reconciler's BotLogin() ("<slug>[bot]"),
// not cfg.User (the operator's separate human login).
func TestSelfLogin_AppAuthMode_ReturnsBotLogin(t *testing.T) {
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	eng.ghAppAuth = appAuthTestReconciler(t)

	got := eng.selfLogin()
	want := eng.ghAppAuth.BotLogin()
	if got != want {
		t.Errorf("selfLogin() = %q, want BotLogin() %q", got, want)
	}
	if got != "fabrik[bot]" {
		t.Errorf("selfLogin() = %q, want %q", got, "fabrik[bot]")
	}
	if eng.cfg.User == got {
		t.Fatalf("test setup invalid: cfg.User (%q) must differ from BotLogin() (%q) to prove selfLogin() isn't just returning cfg.User by coincidence", eng.cfg.User, got)
	}
}

// TestDurablyAddressedReviewIDs_AppAuth_RecognizesSelfAuthoredMarker is the
// R6/AC1 regression test for S1 (issue #1754): before the fix,
// durablyAddressedReviewIDs compared comment authors against e.cfg.User, so
// under App auth — where the durable marker comment is actually posted
// under the bot login — the comparison never matched and "addressed" was
// always empty, permanently defeating #1555's durable review-suppression.
func TestDurablyAddressedReviewIDs_AppAuth_RecognizesSelfAuthoredMarker(t *testing.T) {
	client := &mockGitHubClient{
		fetchIssueCommentsFn: func(owner, repo string, issueNumber int) ([]gh.Comment, error) {
			return []gh.Comment{
				{
					DatabaseID: 5001,
					Author:     "fabrik[bot]",
					Body:       "🏭 **Fabrik — stage: Validate (review feedback addressed)**\n...\n\n<!-- fabrik:review-ids-addressed: 900 -->",
				},
			}, nil
		},
	}
	eng := reviewTestEngine(t, client)
	eng.ghAppAuth = appAuthTestReconciler(t)
	if got := eng.selfLogin(); got != "fabrik[bot]" {
		t.Fatalf("test setup invalid: selfLogin() = %q, want %q", got, "fabrik[bot]")
	}

	item := gh.ProjectItem{Number: 20, Repo: "owner/repo", LinkedPRNumber: 77}

	addressed := eng.durablyAddressedReviewIDs(item)

	if !addressed[900] {
		t.Errorf("expected review 900 to be recognized as durably addressed under App auth, got addressed=%+v", addressed)
	}
}

// TestFindBlockedComment_AppAuth_RecognizesSelfAuthoredComment is the
// R6/AC1 regression test for S2 (issue #1754): before the fix,
// findBlockedComment was called with e.cfg.User, so under App auth — where
// the blocked-on-dependencies comment is actually posted under the bot
// login — the already-blocked branch's in-place UpdateComment was dead
// code and the "waiting for #X, #Y" list was never refreshed.
func TestFindBlockedComment_AppAuth_RecognizesSelfAuthoredComment(t *testing.T) {
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	eng.ghAppAuth = appAuthTestReconciler(t)

	comments := []gh.Comment{
		{
			DatabaseID: 42,
			Author:     "fabrik[bot]",
			Body:       blockedCommentPrefix + "\n\nWaiting for the following issues to close: #1, #2",
		},
	}

	got := findBlockedComment(comments, eng.selfLogin())
	if got == nil {
		t.Fatal("expected findBlockedComment to recognize the bot-authored comment as self-authored under App auth, got nil")
	}
	if got.DatabaseID != 42 {
		t.Errorf("expected DatabaseID 42, got %d", got.DatabaseID)
	}
}

// TestPostComment_AppAuth_CacheWriteThroughAgreesWithBotLogin is the R6/AC3
// regression test for S3 (issue #1754): postComment's cache write-through
// must stamp Author with Fabrik's own posting identity (selfLogin()), not
// cfg.User, so a cached read and a refetched read of the same comment never
// disagree on whether gh.IsBotLogin classifies it — the divergence
// filterHuman (ADR-069) depends on not existing.
func TestPostComment_AppAuth_CacheWriteThroughAgreesWithBotLogin(t *testing.T) {
	client := &mockGitHubClient{
		addCommentFn: func(owner, repo string, issueNumber int, body string) (int, error) {
			return 999, nil
		},
	}
	eng, _ := testEngineWithCache(t, client, &mockClaudeInvoker{})
	eng.ghAppAuth = appAuthTestReconciler(t)

	item := gh.ProjectItem{Number: 1, Repo: "owner/repo"}
	if _, err := eng.postComment(item, "🏭 **Fabrik** hello", false, false); err != nil {
		t.Fatalf("postComment: %v", err)
	}

	snap, err := eng.store.Get("owner/repo", 1)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	comments := snap.State().Comments
	var found *gh.Comment
	for i := range comments {
		if comments[i].DatabaseID == 999 {
			found = &comments[i]
		}
	}
	if found == nil {
		t.Fatal("expected the posted comment to be present in the cache")
	}
	if found.Author != eng.selfLogin() {
		t.Errorf("cached comment Author = %q, want selfLogin() %q", found.Author, eng.selfLogin())
	}
	if !gh.IsBotLogin(found.Author) {
		t.Errorf("cached comment Author %q must be classified as a bot login by gh.IsBotLogin, matching what a live refetch would report", found.Author)
	}
}
