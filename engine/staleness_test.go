package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/selfupgrade"
	"github.com/handarbeit/fabrik/warnings"
)

// TestMaybeCheckSourceStaleness_ReachableWhileBusy is the AC1 fixture: a
// merge-train worker is permanently in flight (the exact "continuously busy
// board" shape from the incident — dispatched stays 0 every poll, but so does
// idleCount, since HasInFlightWorker() is true the whole time). It asserts
// the source_staleness warning is still recorded after enough real eng.poll()
// cycles to cross stalenessCheckPollInterval — proving the new check does not
// share idleUpgradeThreshold's gate.
//
// AC2 companion (not automatable without two code revisions in one test):
// moving the e.maybeCheckSourceStaleness() call site in poll.go inside the
// `if dispatched == 0 { ... idleCount ... }` block turns this test red, since
// idleCount never reaches idleUpgradeThreshold in this fixture. Verified
// manually during implementation; stated here per the acceptance criteria.
func TestMaybeCheckSourceStaleness_ReachableWhileBusy(t *testing.T) {
	warnings.WarningsPathOverride = filepath.Join(t.TempDir(), "warnings.json")
	defer func() { warnings.WarningsPathOverride = "" }()

	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{ProjectID: "PVT_1", Items: nil}, nil
		},
	}
	eng := NewWithDeps(Config{
		Owner:         "owner",
		Repo:          "repo",
		ProjectNum:    1,
		User:          "testuser",
		Token:         "token",
		MaxConcurrent: 5,
		AutoUpgrade:   true,
		Version:       "dev(8a5ef0f1234567890abcdef1234567890abcdef)",
		Stages:        testStages(),
	}, client, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))

	// A merge-train worker that never exits — the board is permanently busy.
	eng.store.EnterRepoWorker("owner/repo")

	commitTime := time.Now().Add(-15 * time.Hour)
	compareCalls := 0
	eng.stalenessCompareFn = func(selfupgrade.DevBuildConfig) (selfupgrade.DevBuildStatus, error) {
		compareCalls++
		return selfupgrade.DevBuildStatus{
			Applicable:      true,
			LocalRef:        "8a5ef0f1234567890abcdef1234567890abcdef",
			LocalCommitTime: commitTime,
			RemoteAhead:     true,
			CommitsBehind:   75,
			NeedsRebuild:    true,
		}, nil
	}

	for i := 0; i < stalenessCheckPollInterval; i++ {
		if _, err := eng.poll(context.Background()); err != nil {
			t.Fatalf("poll %d: %v", i, err)
		}
	}

	if eng.idleCount != 0 {
		t.Fatalf("idleCount = %d, want 0 — fixture must never go idle for this test to prove anything", eng.idleCount)
	}
	if compareCalls != 1 {
		t.Fatalf("stalenessCompareFn called %d times over %d polls, want exactly 1 (fires on poll #1, next fire is %d polls later)", compareCalls, stalenessCheckPollInterval, stalenessCheckPollInterval)
	}

	entries, err := warnings.Load()
	if err != nil {
		t.Fatal(err)
	}
	var found *warnings.Entry
	for i := range entries {
		if entries[i].Key == sourceStalenessWarningKey {
			found = &entries[i]
		}
	}
	if found == nil {
		t.Fatalf("expected a %q warning to be recorded while workers stayed continuously in flight; entries: %+v", sourceStalenessWarningKey, entries)
	}
	if !strings.Contains(found.Detail, "75 commit") {
		t.Errorf("Detail = %q, want it to name the commits-behind count", found.Detail)
	}
	if !strings.Contains(found.Detail, "8a5ef0f") {
		t.Errorf("Detail = %q, want it to name the running SHA", found.Detail)
	}
	if !strings.Contains(found.Detail, "15h") {
		t.Errorf("Detail = %q, want it to name the commit age", found.Detail)
	}
}

// TestMaybeCheckSourceStaleness_FiresExactlyEveryInterval pins the gate's
// exact cadence: it must fire on call 1 (startup coverage) and again exactly
// stalenessCheckPollInterval calls later — not stalenessCheckPollInterval+1,
// which was the off-by-one a prior version of this gate had (the reset value
// left after a fire needs to be stalenessCheckPollInterval-1, since the fire
// call itself already counts as the first call of the next interval).
func TestMaybeCheckSourceStaleness_FiresExactlyEveryInterval(t *testing.T) {
	warnings.WarningsPathOverride = filepath.Join(t.TempDir(), "warnings.json")
	defer func() { warnings.WarningsPathOverride = "" }()

	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	eng.cfg.Version = "dev(abc1234)"
	eng.stalenessCompareFn = func(selfupgrade.DevBuildConfig) (selfupgrade.DevBuildStatus, error) {
		return selfupgrade.DevBuildStatus{Applicable: true, NeedsRebuild: false}, nil
	}

	var fireCalls []int
	for call := 1; call <= 2*stalenessCheckPollInterval+1; call++ {
		before := eng.pollsUntilStalenessCheck
		eng.maybeCheckSourceStaleness()
		// A fire is distinguishable from a decrement by the gate resetting
		// the counter instead of decrementing it by exactly one.
		if before == 0 {
			fireCalls = append(fireCalls, call)
		}
	}

	want := []int{1, 1 + stalenessCheckPollInterval, 1 + 2*stalenessCheckPollInterval}
	if len(fireCalls) != len(want) {
		t.Fatalf("fired on calls %v, want %v", fireCalls, want)
	}
	for i, w := range want {
		if fireCalls[i] != w {
			t.Errorf("fire %d happened on call %d, want call %d (gap between fires must be exactly stalenessCheckPollInterval=%d calls)", i, fireCalls[i], w, stalenessCheckPollInterval)
		}
	}
}

// TestCheckSourceStaleness_ClearsOnUpToDate verifies R5's self-clearing
// behavior: a pre-existing warning is removed once the comparison reports
// the checkout is current.
func TestCheckSourceStaleness_ClearsOnUpToDate(t *testing.T) {
	warnings.WarningsPathOverride = filepath.Join(t.TempDir(), "warnings.json")
	defer func() { warnings.WarningsPathOverride = "" }()

	if err := warnings.Record(warnings.Entry{
		Key:   sourceStalenessWarningKey,
		Type:  "source_staleness",
		Title: "stale from a previous check",
	}); err != nil {
		t.Fatal(err)
	}

	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	eng.cfg.Version = "dev(abc1234)"
	eng.stalenessCompareFn = func(selfupgrade.DevBuildConfig) (selfupgrade.DevBuildStatus, error) {
		return selfupgrade.DevBuildStatus{Applicable: true, NeedsRebuild: false}, nil
	}

	eng.checkSourceStaleness()

	entries, err := warnings.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Key == sourceStalenessWarningKey {
			t.Fatalf("expected %q to be cleared once up to date, still present: %+v", sourceStalenessWarningKey, e)
		}
	}
}

// TestCheckSourceStaleness_FreshCheckUpToDateRecordsNothing simulates a
// daemon restart (a brand new Engine, a fresh warnings file) whose first
// staleness check finds the checkout already current — nothing should be
// recorded. Combined with TestCheckSourceStaleness_ClearsOnUpToDate, this
// covers R5's "clears itself when the daemon is restarted onto current code"
// even when the old process's warning entry never made it to disk before
// the restart.
func TestCheckSourceStaleness_FreshCheckUpToDateRecordsNothing(t *testing.T) {
	warnings.WarningsPathOverride = filepath.Join(t.TempDir(), "warnings.json")
	defer func() { warnings.WarningsPathOverride = "" }()

	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	eng.cfg.Version = "dev(abc1234)"
	eng.stalenessCompareFn = func(selfupgrade.DevBuildConfig) (selfupgrade.DevBuildStatus, error) {
		return selfupgrade.DevBuildStatus{Applicable: true, NeedsRebuild: false}, nil
	}

	// pollsUntilStalenessCheck's zero value fires immediately — the "check at
	// startup" behavior a restart relies on.
	eng.maybeCheckSourceStaleness()

	entries, err := warnings.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected no warnings recorded for an up-to-date fresh check, got: %+v", entries)
	}
}

// TestCheckSourceStaleness_NonDevVersionSkipsEntirely is the AC5 case: a
// release build never calls into the comparator at all — no git fetch, no
// warning, regardless of what the comparator would have returned.
func TestCheckSourceStaleness_NonDevVersionSkipsEntirely(t *testing.T) {
	warnings.WarningsPathOverride = filepath.Join(t.TempDir(), "warnings.json")
	defer func() { warnings.WarningsPathOverride = "" }()

	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	eng.cfg.Version = "v1.2.3" // release build, not "dev(...)"

	called := false
	eng.stalenessCompareFn = func(selfupgrade.DevBuildConfig) (selfupgrade.DevBuildStatus, error) {
		called = true
		return selfupgrade.DevBuildStatus{}, nil
	}

	eng.checkSourceStaleness()

	if called {
		t.Error("expected the comparator (and therefore any git fetch) to never be invoked for a release version")
	}
	entries, err := warnings.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected no warnings for a release version, got: %+v", entries)
	}
}

// TestCheckSourceStaleness_RealCompareDevBuild drives checkSourceStaleness
// through the real, un-stubbed selfupgrade.CompareDevBuild against an actual
// git checkout that is genuinely two commits behind its origin — proving the
// engine call site is wired to the real comparator (not just its own test
// seam) and that going through it end-to-end never touches go build/exec:
// the checkout's placeholder files are asserted unchanged afterward.
func TestCheckSourceStaleness_RealCompareDevBuild(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH")
	}
	warnings.WarningsPathOverride = filepath.Join(t.TempDir(), "warnings.json")
	defer func() { warnings.WarningsPathOverride = "" }()

	runGit := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s: %v", args, out, err)
		}
	}

	// The origin remote's URL must contain "handarbeit/fabrik" to satisfy
	// IsSourceCheckout — using that literal path segment on local disk lets a
	// real (non-network) git remote clear the same substring gate production
	// traffic does.
	originDir := filepath.Join(t.TempDir(), "handarbeit", "fabrik")
	if err := os.MkdirAll(originDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(originDir, "go.mod"), []byte("module staletest\n\ngo 1.21\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit(originDir, "init", "-b", "main")
	runGit(originDir, "config", "user.email", "test@test.com")
	runGit(originDir, "config", "user.name", "Test")
	runGit(originDir, "add", "-A")
	runGit(originDir, "commit", "-m", "initial")

	localDir := t.TempDir()
	runGit(localDir, "clone", originDir, ".")
	runGit(localDir, "config", "user.email", "test@test.com")
	runGit(localDir, "config", "user.name", "Test")

	localSHACmd := exec.Command("git", "rev-parse", "HEAD")
	localSHACmd.Dir = localDir
	out, err := localSHACmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	localSHA := strings.TrimSpace(string(out))

	runGit(originDir, "commit", "--allow-empty", "-m", "second")
	runGit(originDir, "commit", "--allow-empty", "-m", "third")

	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	eng.fabrikDir = localDir
	eng.cfg.Version = "dev(" + localSHA[:7] + ")"
	eng.cfg.AutoUpgrade = true
	// stalenessCompareFn left nil — production defaults to the real
	// selfupgrade.CompareDevBuild.

	goModBefore, err := os.ReadFile(filepath.Join(localDir, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}

	eng.checkSourceStaleness()

	goModAfter, err := os.ReadFile(filepath.Join(localDir, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if string(goModBefore) != string(goModAfter) {
		t.Error("checkSourceStaleness modified checkout files — it must never build")
	}

	entries, err := warnings.Load()
	if err != nil {
		t.Fatal(err)
	}
	var found *warnings.Entry
	for i := range entries {
		if entries[i].Key == sourceStalenessWarningKey {
			found = &entries[i]
		}
	}
	if found == nil {
		t.Fatalf("expected a %q warning for a checkout 2 commits behind origin, entries: %+v", sourceStalenessWarningKey, entries)
	}
	if !strings.Contains(found.Detail, "2 commits") {
		t.Errorf("Detail = %q, want it to name the commits-behind count (2)", found.Detail)
	}
	if !strings.Contains(found.Detail, localSHA[:7]) {
		t.Errorf("Detail = %q, want it to name the running SHA (%s)", found.Detail, localSHA[:7])
	}
	if found.FixAction != "shell_command" {
		t.Errorf("FixAction = %q, want %q since AutoUpgrade is enabled", found.FixAction, "shell_command")
	}
}
