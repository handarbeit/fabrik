package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/handarbeit/fabrik/warnings"
)

func setWarningsOverride(t *testing.T) {
	t.Helper()
	warnings.WarningsPathOverride = filepath.Join(t.TempDir(), "warnings.json")
	t.Cleanup(func() { warnings.WarningsPathOverride = "" })
}

// TestSweepStaleAllowAutoMergeWarnings_ClearsAbsentRepo is AC1: a recorded
// allow_auto_merge warning for a repo absent from the current board is
// cleared by the sweep. Neutralizing the sweep call (removing it from
// poll()) makes this fail, proving the test is non-vacuous per the issue's
// Verification note.
func TestSweepStaleAllowAutoMergeWarnings_ClearsAbsentRepo(t *testing.T) {
	setWarningsOverride(t)
	if err := warnings.Record(warnings.Entry{
		Key:  "allow_auto_merge:gone/repo",
		Type: "allow_auto_merge",
	}); err != nil {
		t.Fatal(err)
	}
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})

	eng.sweepStaleAllowAutoMergeWarnings(map[string]bool{"owner/repo": true})

	entries, _ := warnings.Load()
	if len(entries) != 0 {
		t.Fatalf("expected absent-repo warning cleared, got %v", entries)
	}
}

// TestSweepStaleAllowAutoMergeWarnings_PreservesPresentRepo is AC2: a
// warning for a repo still on the board is not cleared, regardless of its
// auto-merge state. This is the guard against over-clearing.
func TestSweepStaleAllowAutoMergeWarnings_PreservesPresentRepo(t *testing.T) {
	setWarningsOverride(t)
	if err := warnings.Record(warnings.Entry{
		Key:  "allow_auto_merge:owner/repo",
		Type: "allow_auto_merge",
	}); err != nil {
		t.Fatal(err)
	}
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})

	eng.sweepStaleAllowAutoMergeWarnings(map[string]bool{"owner/repo": true})

	entries, _ := warnings.Load()
	if len(entries) != 1 || entries[0].Key != "allow_auto_merge:owner/repo" {
		t.Fatalf("expected present-repo warning preserved, got %v", entries)
	}
}

// TestSweepStaleAllowAutoMergeWarnings_IgnoresOtherTypes is AC3.
func TestSweepStaleAllowAutoMergeWarnings_IgnoresOtherTypes(t *testing.T) {
	setWarningsOverride(t)
	if err := warnings.Record(warnings.Entry{
		Key:  "stage_drift:SomeStage",
		Type: "stage_drift",
	}); err != nil {
		t.Fatal(err)
	}
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})

	// Empty board — if the sweep mis-scoped by Type, this would clear the
	// stage_drift entry too.
	eng.sweepStaleAllowAutoMergeWarnings(map[string]bool{})

	entries, _ := warnings.Load()
	if len(entries) != 1 || entries[0].Key != "stage_drift:SomeStage" {
		t.Fatalf("expected unrelated warning type untouched, got %v", entries)
	}
}

// TestSweepStaleAllowAutoMergeWarnings_PreservesDismissed is AC4: a
// dismissed warning for a repo still on the board stays present and
// dismissed.
func TestSweepStaleAllowAutoMergeWarnings_PreservesDismissed(t *testing.T) {
	setWarningsOverride(t)
	key := "allow_auto_merge:owner/repo"
	if err := warnings.Record(warnings.Entry{Key: key, Type: "allow_auto_merge"}); err != nil {
		t.Fatal(err)
	}
	if err := warnings.Dismiss(key); err != nil {
		t.Fatal(err)
	}
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})

	eng.sweepStaleAllowAutoMergeWarnings(map[string]bool{"owner/repo": true})

	entries, _ := warnings.Load()
	if len(entries) != 1 || !entries[0].Dismissed {
		t.Fatalf("expected dismissed entry preserved and still dismissed, got %v", entries)
	}
}

// TestSweepStaleAllowAutoMergeWarnings_NoWriteWhenNothingStale is AC5: no
// API calls (mockGitHubClient with no fetchAllowAutoMergeFn would panic if
// called) and no write when nothing needs clearing.
func TestSweepStaleAllowAutoMergeWarnings_NoWriteWhenNothingStale(t *testing.T) {
	setWarningsOverride(t)
	if err := warnings.Record(warnings.Entry{Key: "allow_auto_merge:owner/repo", Type: "allow_auto_merge"}); err != nil {
		t.Fatal(err)
	}
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})

	before, err := os.Stat(warnings.WarningsPathOverride)
	if err != nil {
		t.Fatal(err)
	}

	eng.sweepStaleAllowAutoMergeWarnings(map[string]bool{"owner/repo": true})

	after, err := os.Stat(warnings.WarningsPathOverride)
	if err != nil {
		t.Fatal(err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Errorf("expected no write when nothing stale; mtime changed from %v to %v", before.ModTime(), after.ModTime())
	}
}

// TestSweepStaleAllowAutoMergeWarnings_LogsOncePerClear is AC6: exactly one
// log line per clear, naming the key and the reason.
func TestSweepStaleAllowAutoMergeWarnings_LogsOncePerClear(t *testing.T) {
	setWarningsOverride(t)
	if err := warnings.Record(warnings.Entry{Key: "allow_auto_merge:gone/repo", Type: "allow_auto_merge"}); err != nil {
		t.Fatal(err)
	}
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})

	out := captureStdout(func() {
		eng.sweepStaleAllowAutoMergeWarnings(map[string]bool{"owner/repo": true})
	})

	if got := strings.Count(out, "allow_auto_merge:gone/repo"); got != 1 {
		t.Fatalf("expected exactly one log line naming the key, got %d in: %q", got, out)
	}
	if !strings.Contains(out, "no longer on the board") {
		t.Errorf("expected log line to name the reason, got: %q", out)
	}
}

// TestSweepStaleAllowAutoMergeWarnings_ExemptsConfiguredRepo covers the
// single-repo-mode edge case surfaced in Research: checkAllowAutoMerge for
// the engine's own configured repo fires unconditionally at startup, but a
// transient poll with zero open items for that repo must not durably clear
// a legitimate warning for it.
func TestSweepStaleAllowAutoMergeWarnings_ExemptsConfiguredRepo(t *testing.T) {
	setWarningsOverride(t)
	if err := warnings.Record(warnings.Entry{Key: "allow_auto_merge:owner/repo", Type: "allow_auto_merge"}); err != nil {
		t.Fatal(err)
	}
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{}) // Owner="owner", Repo="repo"

	// seenRepos is empty — the board has no open items for the configured
	// repo right now — but the warning must survive via the defaultRepo()
	// union.
	eng.sweepStaleAllowAutoMergeWarnings(map[string]bool{})

	entries, _ := warnings.Load()
	if len(entries) != 1 {
		t.Fatalf("expected configured repo's warning exempted from sweep, got %v", entries)
	}
}

// TestSweepStaleAllowAutoMergeWarnings_MultiRepoModeNoExemption confirms
// multi-repo mode (Owner=="" && Repo=="") has no always-present repo:
// defaultRepo() returns "" and is correctly a no-op union.
func TestSweepStaleAllowAutoMergeWarnings_MultiRepoModeNoExemption(t *testing.T) {
	setWarningsOverride(t)
	if err := warnings.Record(warnings.Entry{Key: "allow_auto_merge:gone/repo", Type: "allow_auto_merge"}); err != nil {
		t.Fatal(err)
	}
	eng := NewWithDeps(
		Config{
			ProjectNum:    1,
			User:          "testuser",
			Token:         "token",
			MaxConcurrent: 5,
			Stages:        testStages(),
		},
		&mockGitHubClient{},
		&mockClaudeInvoker{},
		NewWorktreeManager(t.TempDir()),
	)

	eng.sweepStaleAllowAutoMergeWarnings(map[string]bool{"other/repo": true})

	entries, _ := warnings.Load()
	if len(entries) != 0 {
		t.Fatalf("expected multi-repo mode to have no exemption, got %v", entries)
	}
}

// TestSweepStaleAllowAutoMergeWarnings_MultiRepoModeEmptyBoardSkipsSweep
// guards against the destructive-wipe shape flagged in review: in multi-repo
// mode there is no defaultRepo() to fall back on, so a genuinely empty
// seenRepos (e.g. retryOnEmpty in github/project.go exhausting retries on a
// persistent indexer hiccup) would otherwise make every recorded
// allow_auto_merge warning look "absent" and clear them all in one pass. The
// sweep must skip entirely — not just skip clearing a specific repo — when
// it has zero signal to compare against.
func TestSweepStaleAllowAutoMergeWarnings_MultiRepoModeEmptyBoardSkipsSweep(t *testing.T) {
	setWarningsOverride(t)
	if err := warnings.Record(warnings.Entry{Key: "allow_auto_merge:owner/repo", Type: "allow_auto_merge"}); err != nil {
		t.Fatal(err)
	}
	eng := NewWithDeps(
		Config{
			ProjectNum:    1,
			User:          "testuser",
			Token:         "token",
			MaxConcurrent: 5,
			Stages:        testStages(),
		},
		&mockGitHubClient{},
		&mockClaudeInvoker{},
		NewWorktreeManager(t.TempDir()),
	)

	before, err := os.Stat(warnings.WarningsPathOverride)
	if err != nil {
		t.Fatal(err)
	}

	eng.sweepStaleAllowAutoMergeWarnings(map[string]bool{})

	entries, _ := warnings.Load()
	if len(entries) != 1 || entries[0].Key != "allow_auto_merge:owner/repo" {
		t.Fatalf("expected warning preserved when the board is empty in multi-repo mode, got %v", entries)
	}
	after, err := os.Stat(warnings.WarningsPathOverride)
	if err != nil {
		t.Fatal(err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Errorf("expected no write when the sweep is skipped; mtime changed from %v to %v", before.ModTime(), after.ModTime())
	}
}
