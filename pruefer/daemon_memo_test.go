package pruefer

import (
	"context"
	"errors"
	"fmt"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

// #1952: the PR evaluation memo, exercised through Daemon.poll against
// counting fakes.

const memoRepo = "owner/repo"

func memoPR(n int, head, updated string) gh.PRDetails {
	return gh.PRDetails{Number: n, Author: "alice", HeadSHA: head, UpdatedAt: updated, BaseRef: "main", Title: fmt.Sprintf("pr %d", n)}
}

func newMemoDaemon(t *testing.T, client *fakeLister, claude ClaudeInvoker, cfg Config) *Daemon {
	t.Helper()
	clone, _ := fakeClone(t, nil)
	return withDerivedRepos(&Daemon{
		Clients:  map[string]GitHubLister{"owner": client},
		Claude:   claude,
		Clone:    clone,
		Config:   cfg,
		BotLogin: "pruefer-bot[bot]",
		Tracker:  NewReviewTracker(),
		memo:     newPRMemo(),
	}, memoRepo)
}

// perPRCalls is the count of the calls the memo exists to avoid: the
// comment read, the reviews read and the diff fetch, all per PR.
func perPRCalls(c *fakeLister) (comments, reviews, diffs int) {
	c.fakeCommenter.mu.Lock()
	comments = c.fakeCommenter.commentFetches
	c.fakeCommenter.mu.Unlock()
	c.fakeReviewer.mu.Lock()
	reviews, diffs = c.fakeReviewer.reviewsFetches, c.fakeReviewer.diffCalls
	c.fakeReviewer.mu.Unlock()
	return
}

func configFetches(c *fakeLister) int { return len(c.fileAtRefCallArgs()) }

func okClaude() *mockClaudeInvoker {
	return &mockClaudeInvoker{fn: func(ReviewRequest) (ReviewResult, error) { return ReviewResult{Text: "ok"}, nil }}
}

func setPRs(c *fakeLister, prs ...gh.PRDetails) {
	c.mu.Lock()
	c.prsByRepo[memoRepo] = prs
	c.mu.Unlock()
}

// AC1 + AC4: a cold start evaluates every open PR once; a poll where every PR
// is reviewed and unchanged makes no per-PR calls at all — only the per-repo
// listing and the single per-repo config fingerprint read.
func TestDaemonMemo_ColdStartEvaluatesAll_UnchangedPollMakesNoPerPRCalls(t *testing.T) {
	client := newFakeLister()
	setPRs(client, memoPR(1, "a", "t1"), memoPR(2, "b", "t1"), memoPR(3, "c", "t1"))
	claude := okClaude()
	d := newMemoDaemon(t, client, claude, Config{ConcurrencyCap: 3})

	d.poll(context.Background())
	if got := client.submitCallCount(); got != 3 {
		t.Fatalf("cold start reviewed %d PRs, want 3 (every open PR evaluated once)", got)
	}
	comments0, reviews0, diffs0 := perPRCalls(client)
	if comments0 != 3 || reviews0 != 3 {
		t.Fatalf("cold start per-PR reads = comments %d reviews %d, want 3/3", comments0, reviews0)
	}
	cfg0, list0 := configFetches(client), client.listOpenPRsCallCount()

	d.poll(context.Background())

	comments1, reviews1, diffs1 := perPRCalls(client)
	if comments1 != comments0 || reviews1 != reviews0 || diffs1 != diffs0 {
		t.Errorf("unchanged poll made per-PR calls: comments %d→%d reviews %d→%d diffs %d→%d",
			comments0, comments1, reviews0, reviews1, diffs0, diffs1)
	}
	if got := client.submitCallCount(); got != 3 {
		t.Errorf("unchanged poll submitted reviews: %d total, want 3", got)
	}
	if got := configFetches(client) - cfg0; got != 1 {
		t.Errorf("unchanged poll made %d repo-config reads, want exactly 1 per repo (not per PR)", got)
	}
	if got := client.listOpenPRsCallCount() - list0; got != 1 {
		t.Errorf("unchanged poll made %d ListOpenPRs calls, want 1", got)
	}
}

// AC2: a new head SHA is evaluated and reviewed. The head alone changing —
// with updated_at deliberately held equal — must still evaluate.
func TestDaemonMemo_HeadShaChangeIsReviewed(t *testing.T) {
	client := newFakeLister()
	setPRs(client, memoPR(1, "a", "t1"))
	d := newMemoDaemon(t, client, okClaude(), Config{})
	d.poll(context.Background())
	d.poll(context.Background())
	if got := client.submitCallCount(); got != 1 {
		t.Fatalf("setup: %d reviews, want 1", got)
	}

	setPRs(client, memoPR(1, "b", "t1"))
	d.poll(context.Background())
	if got := client.submitCallCount(); got != 2 {
		t.Errorf("reviews after head change = %d, want 2", got)
	}
	if last := client.submitCalls[len(client.submitCalls)-1]; last.commitSHA != "b" {
		t.Errorf("review pinned to %q, want the new head %q", last.commitSHA, "b")
	}
}

// AC3: a /pruefer review comment advances updated_at; the memoised PR is
// evaluated and force-reviewed even though its head SHA is unchanged.
func TestDaemonMemo_ForceReviewCommentIsHonoured(t *testing.T) {
	client := newFakeLister()
	setPRs(client, memoPR(1, "a", "t1"))
	d := newMemoDaemon(t, client, okClaude(), Config{})
	d.poll(context.Background())
	d.poll(context.Background())
	if got := client.submitCallCount(); got != 1 {
		t.Fatalf("setup: %d reviews, want 1", got)
	}

	client.comments = append(client.comments, gh.Comment{DatabaseID: 42, Body: "/pruefer review"})
	setPRs(client, memoPR(1, "a", "t2")) // the comment bumped updated_at
	d.poll(context.Background())
	if got := client.submitCallCount(); got != 2 {
		t.Errorf("reviews after /pruefer review = %d, want 2 (forced re-review of the same head)", got)
	}
}

// Pruefer's own review bumps updated_at: exactly one extra evaluation (a
// cheap already-reviewed skip), after which the PR is memoised again.
func TestDaemonMemo_OwnReviewCostsExactlyOneReEvaluation(t *testing.T) {
	client := newFakeLister()
	setPRs(client, memoPR(1, "a", "t1"))
	d := newMemoDaemon(t, client, okClaude(), Config{})
	d.poll(context.Background()) // reviews

	setPRs(client, memoPR(1, "a", "t2")) // GitHub bumped updated_at for our review
	c0, _, _ := perPRCalls(client)
	d.poll(context.Background())
	c1, _, _ := perPRCalls(client)
	if c1-c0 != 1 {
		t.Errorf("post-review poll made %d comment reads, want exactly 1 re-evaluation", c1-c0)
	}
	if got := client.submitCallCount(); got != 1 {
		t.Errorf("re-evaluation submitted another review: %d total", got)
	}
	d.poll(context.Background())
	c2, _, _ := perPRCalls(client)
	if c2 != c1 {
		t.Errorf("third poll re-evaluated again (%d → %d comment reads); the skip should have been memoised", c1, c2)
	}
}

// AC7: an errored evaluation is not memoised and retries next poll.
func TestDaemonMemo_ErroredEvaluationIsNotMemoised(t *testing.T) {
	client := newFakeLister()
	setPRs(client, memoPR(1, "a", "t1"))
	fail := true
	claude := &mockClaudeInvoker{fn: func(ReviewRequest) (ReviewResult, error) {
		if fail {
			return ReviewResult{}, errors.New("claude boom")
		}
		return ReviewResult{Text: "ok"}, nil
	}}
	d := newMemoDaemon(t, client, claude, Config{})

	d.poll(context.Background())
	if d.memo.Len() != 0 {
		t.Fatal("an errored evaluation was memoised")
	}
	fail = false
	d.poll(context.Background())
	if got := client.submitCallCount(); got != 1 {
		t.Errorf("retry after error submitted %d reviews, want 1", got)
	}
	if claude.callCount() != 2 {
		t.Errorf("claude called %d times, want 2 (failed once, retried once)", claude.callCount())
	}
}

// AC7: a swallowed error (a failed comment lookup behind a cadence
// on-request skip) is not memoised either — it retries next poll.
func TestDaemonMemo_SwallowedErrorSkipIsNotMemoised(t *testing.T) {
	client := newFakeLister()
	setPRs(client, memoPR(1, "a", "t1"))
	client.fetchErr = errors.New("comments 502")
	d := newMemoDaemon(t, client, okClaude(), Config{Cadence: CadenceOnRequest})

	d.poll(context.Background())
	if d.memo.Len() != 0 {
		t.Fatal("a skip decided on a failed comment lookup was memoised")
	}
	c0, _, _ := perPRCalls(client)
	d.poll(context.Background())
	if c1, _, _ := perPRCalls(client); c1 == c0 {
		t.Error("the PR was not re-evaluated after a swallowed error")
	}
}

// AC7: a cadence skip is not frozen past an operator reload of its input.
func TestDaemonMemo_CadenceSkipNotFrozenAcrossReload(t *testing.T) {
	client := newFakeLister()
	setPRs(client, memoPR(1, "a", "t1"))
	cfg := Config{Cadence: CadenceOnRequest}
	d := newMemoDaemon(t, client, okClaude(), cfg)

	d.poll(context.Background())
	d.poll(context.Background())
	if got := client.submitCallCount(); got != 0 {
		t.Fatalf("on-request cadence reviewed %d PRs, want 0", got)
	}
	if d.memo.Len() != 1 {
		t.Fatal("setup: the cadence skip should be memoised")
	}
	c0, _, _ := perPRCalls(client)
	d.poll(context.Background())
	if c1, _, _ := perPRCalls(client); c1 != c0 {
		t.Fatal("setup: memoised cadence skip still re-evaluated")
	}

	reloaded := cfg
	reloaded.Cadence = CadenceEveryPush
	d.ApplyReload(reloaded, nil, nil)
	d.poll(context.Background())
	if got := client.submitCallCount(); got != 1 {
		t.Errorf("after reloading cadence to every-push, reviews = %d, want 1 — a stale skip withheld the review", got)
	}
}

// AC7: an operator-config reload of an exclusion invalidates a memoised skip
// in the other direction too (an excluded PR must not be re-reviewed from a
// stale "eligible" verdict... and a stale "skipped" must not persist).
func TestDaemonMemo_ExclusionReloadInvalidatesMemo(t *testing.T) {
	client := newFakeLister()
	setPRs(client, memoPR(1, "a", "t1"))
	d := newMemoDaemon(t, client, okClaude(), Config{ExcludedAuthors: []string{"alice"}})
	d.poll(context.Background())
	if got := client.submitCallCount(); got != 0 {
		t.Fatalf("excluded author reviewed: %d", got)
	}
	d.ApplyReload(Config{}, nil, nil)
	d.poll(context.Background())
	if got := client.submitCallCount(); got != 1 {
		t.Errorf("after dropping the exclusion, reviews = %d, want 1", got)
	}
}

// AC7: a change to .pruefer/config.yaml at the base ref — which moves nothing
// on the PR — re-evaluates an otherwise-memoised skip.
func TestDaemonMemo_RepoConfigChangeInvalidatesMemo(t *testing.T) {
	client := newFakeLister()
	setPRs(client, memoPR(1, "a", "t1"))
	client.repoConfigData = []byte("excluded_authors: [alice]\n")
	d := newMemoDaemon(t, client, okClaude(), Config{})

	d.poll(context.Background())
	d.poll(context.Background())
	if got := client.submitCallCount(); got != 0 {
		t.Fatalf("repo-config-excluded author reviewed: %d", got)
	}
	c0, _, _ := perPRCalls(client)
	d.poll(context.Background())
	if c1, _, _ := perPRCalls(client); c1 != c0 {
		t.Fatal("setup: memoised skip still re-evaluated with an unchanged repo config")
	}

	client.repoConfigData = nil // the base branch dropped the exclusion; the PR did not move
	d.poll(context.Background())
	if got := client.submitCallCount(); got != 1 {
		t.Errorf("after the repo config changed, reviews = %d, want 1", got)
	}
}

// A repo-config fetch error makes the fingerprint unknown, so nothing is
// memoised and nothing is matched: the PR is evaluated every poll, as before.
func TestDaemonMemo_RepoConfigFetchErrorNeverMemoises(t *testing.T) {
	client := newFakeLister()
	p := memoPR(1, "a", "t1")
	p.Draft = true // a skip: would be memoised if the fingerprint were known
	setPRs(client, p)
	client.repoConfigErr = errors.New("contents 502")
	d := newMemoDaemon(t, client, okClaude(), Config{})

	d.poll(context.Background())
	c0, _, _ := perPRCalls(client)
	d.poll(context.Background())
	if d.memo.Len() != 0 {
		t.Error("an entry was recorded while the repo config was unreadable")
	}
	if c1, _, _ := perPRCalls(client); c1 == c0 {
		t.Error("PR was memo-skipped while the repo config was unreadable")
	}
}

// AC7 / R3: a failed ListOpenPRs neither records nor evicts.
func TestDaemonMemo_FailedListingNeitherRecordsNorEvicts(t *testing.T) {
	client := newFakeLister()
	setPRs(client, memoPR(1, "a", "t1"), memoPR(2, "b", "t1"))
	d := newMemoDaemon(t, client, okClaude(), Config{})
	d.poll(context.Background())
	if d.memo.Len() != 2 {
		t.Fatalf("setup: memo has %d entries, want 2", d.memo.Len())
	}

	client.listErrByRepo[memoRepo] = errors.New("rate limited")
	d.poll(context.Background())
	if d.memo.Len() != 2 {
		t.Errorf("a failed listing changed the memo: %d entries, want 2 (no eviction)", d.memo.Len())
	}

	delete(client.listErrByRepo, memoRepo)
	c0, _, _ := perPRCalls(client)
	d.poll(context.Background())
	if c1, _, _ := perPRCalls(client); c1 != c0 {
		t.Error("entries retained across a failed listing should still skip on the next good poll")
	}
}

// Eviction: a PR missing from a *successful* listing is dropped; a repo that
// leaves the derived set takes its entries with it.
func TestDaemonMemo_EvictsClosedPRsAndRemovedRepos(t *testing.T) {
	client := newFakeLister()
	setPRs(client, memoPR(1, "a", "t1"), memoPR(2, "b", "t1"))
	d := newMemoDaemon(t, client, okClaude(), Config{})
	d.poll(context.Background())

	setPRs(client, memoPR(1, "a", "t1"))
	d.poll(context.Background())
	if d.memo.Len() != 1 {
		t.Errorf("memo has %d entries after PR #2 closed, want 1", d.memo.Len())
	}

	d.ApplyDerivedRepos(testDerivedRepoSet(), map[string]GitHubLister{"owner": client})
	d.poll(context.Background())
	if d.memo.Len() != 0 {
		t.Errorf("memo has %d entries after the repo left the derived set, want 0", d.memo.Len())
	}
}

// A Daemon with no memo (every pre-#1952 literal) behaves exactly as before:
// every PR is evaluated every poll and no repo-config fingerprint is read.
func TestDaemonMemo_NilMemoEvaluatesEveryPoll(t *testing.T) {
	client := newFakeLister()
	setPRs(client, memoPR(1, "a", "t1"))
	d := newMemoDaemon(t, client, okClaude(), Config{})
	d.memo = nil
	d.poll(context.Background())
	d.poll(context.Background())
	if c, _, _ := perPRCalls(client); c != 2 {
		t.Errorf("nil memo: %d comment reads over two polls, want 2", c)
	}
}

func TestRepoConfigFingerprint(t *testing.T) {
	c := newFakeReviewer()
	if got := repoConfigFingerprint(c, "o", "r", "main"); got != "absent" {
		t.Errorf("no file: fingerprint = %q, want %q", got, "absent")
	}
	c.repoConfigData = []byte("excluded_authors: [a]\n")
	a := repoConfigFingerprint(c, "o", "r", "main")
	c.repoConfigData = []byte("excluded_authors: [b]\n")
	b := repoConfigFingerprint(c, "o", "r", "main")
	if a == "" || a == "absent" || a == b {
		t.Errorf("content fingerprints must be non-empty and differ per content: %q vs %q", a, b)
	}
	c.repoConfigErr = errors.New("502")
	if got := repoConfigFingerprint(c, "o", "r", "main"); got != "" {
		t.Errorf("fetch error: fingerprint = %q, want empty (unknown — never memoised or matched)", got)
	}
}
