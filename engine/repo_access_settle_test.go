package engine

import (
	"strings"
	"testing"

	"github.com/handarbeit/fabrik/warnings"
)

// This file mirrors allow_auto_merge_settle_test.go's coverage for
// sweepStaleRepoAccessWarnings — the "repo_access" warning type's own
// counterpart to the #1348 durable-state-leak fix, raised as a review
// finding on #1750: a repo_access warning had no sweep at all, so it would
// be immortal once its subject repo left the board.

// TestSweepStaleRepoAccessWarnings_ClearsAbsentRepo is the non-vacuous
// proof: neutralizing the sweep call in poll() (or this function itself)
// would leave the warning present, since resolveRepoAccess is never called
// again for a repo no longer referenced by any board item.
func TestSweepStaleRepoAccessWarnings_ClearsAbsentRepo(t *testing.T) {
	setWarningsOverride(t)
	if err := warnings.Record(warnings.Entry{
		Key:  "repo_access:gone/repo",
		Type: "repo_access",
	}); err != nil {
		t.Fatal(err)
	}
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})

	eng.sweepStaleRepoAccessWarnings(map[string]bool{"owner/repo": true})

	entries, _ := warnings.Load()
	if len(entries) != 0 {
		t.Fatalf("expected absent-repo warning cleared, got %v", entries)
	}
}

// TestSweepStaleRepoAccessWarnings_PreservesPresentRepo guards against
// over-clearing: a repo_access warning for a repo still on the board must
// survive the sweep.
func TestSweepStaleRepoAccessWarnings_PreservesPresentRepo(t *testing.T) {
	setWarningsOverride(t)
	if err := warnings.Record(warnings.Entry{
		Key:  "repo_access:owner/repo",
		Type: "repo_access",
	}); err != nil {
		t.Fatal(err)
	}
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})

	eng.sweepStaleRepoAccessWarnings(map[string]bool{"owner/repo": true})

	entries, _ := warnings.Load()
	if len(entries) != 1 || entries[0].Key != "repo_access:owner/repo" {
		t.Fatalf("expected present-repo warning preserved, got %v", entries)
	}
}

// TestSweepStaleRepoAccessWarnings_IgnoresOtherTypes confirms the sweep is
// scoped strictly to Type == "repo_access" — an allow_auto_merge warning
// (or any other type) for the same absent repo must be untouched by this
// sweep (sweepStaleAllowAutoMergeWarnings owns that one).
func TestSweepStaleRepoAccessWarnings_IgnoresOtherTypes(t *testing.T) {
	setWarningsOverride(t)
	if err := warnings.Record(warnings.Entry{
		Key:  "allow_auto_merge:gone/repo",
		Type: "allow_auto_merge",
	}); err != nil {
		t.Fatal(err)
	}
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})

	eng.sweepStaleRepoAccessWarnings(map[string]bool{"owner/repo": true})

	entries, _ := warnings.Load()
	if len(entries) != 1 || entries[0].Key != "allow_auto_merge:gone/repo" {
		t.Fatalf("expected unrelated warning type untouched, got %v", entries)
	}
}

// TestSweepStaleRepoAccessWarnings_ExemptsConfiguredRepo mirrors the
// single-repo-mode edge case: resolveRepoAccess for the engine's own
// configured repo fires unconditionally at Run() startup, so a transient
// poll with zero open items for that repo must not durably clear a
// legitimate warning for it.
func TestSweepStaleRepoAccessWarnings_ExemptsConfiguredRepo(t *testing.T) {
	setWarningsOverride(t)
	if err := warnings.Record(warnings.Entry{
		Key:  "repo_access:owner/repo",
		Type: "repo_access",
	}); err != nil {
		t.Fatal(err)
	}
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{}) // Owner="owner", Repo="repo"

	eng.sweepStaleRepoAccessWarnings(map[string]bool{})

	entries, _ := warnings.Load()
	if len(entries) != 1 {
		t.Fatalf("expected configured repo's warning exempted from sweep, got %v", entries)
	}
}

// TestSweepStaleRepoAccessWarnings_LogsOncePerClear confirms the cleared
// entry is logged once, naming the key and the reason — matching
// sweepStaleAllowAutoMergeWarnings' AC6.
func TestSweepStaleRepoAccessWarnings_LogsOncePerClear(t *testing.T) {
	setWarningsOverride(t)
	if err := warnings.Record(warnings.Entry{Key: "repo_access:gone/repo", Type: "repo_access"}); err != nil {
		t.Fatal(err)
	}
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})

	out := captureStdout(func() {
		eng.sweepStaleRepoAccessWarnings(map[string]bool{"owner/repo": true})
	})

	if got := strings.Count(out, "repo_access:gone/repo"); got != 1 {
		t.Fatalf("expected exactly one log line naming the key, got %d in: %q", got, out)
	}
	if !strings.Contains(out, "no longer on the board") {
		t.Errorf("expected log line to name the reason, got: %q", out)
	}
}
