package engine

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

func TestSaveSessionIDDirect(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.session")

	saveSessionIDDirect(path, "sess_abc123")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading session file: %v", err)
	}
	if string(data) != "sess_abc123" {
		t.Errorf("session ID = %q, want sess_abc123", string(data))
	}
}

func TestSaveSessionIDDirect_Empty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.session")

	saveSessionIDDirect(path, "")

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("session file should not exist for empty session ID")
	}
}

func TestSessionDir(t *testing.T) {
	dir := SessionDir(42)
	if !strings.Contains(dir, "issue-42") {
		t.Errorf("SessionDir(42) = %q, expected to contain issue-42", dir)
	}
	if !strings.Contains(dir, ".fabrik/sessions") {
		t.Errorf("SessionDir(42) = %q, expected to contain .fabrik/sessions", dir)
	}
}

func TestLogDir(t *testing.T) {
	dir := LogDir(42)
	if !strings.Contains(dir, "issue-42") {
		t.Errorf("LogDir(42) = %q, expected to contain issue-42", dir)
	}
	if !strings.Contains(dir, ".fabrik/logs") {
		t.Errorf("LogDir(42) = %q, expected to contain .fabrik/logs", dir)
	}
}

func TestLogDirForItem_SingleRepo(t *testing.T) {
	issue := gh.ProjectItem{Number: 42}
	dir := logDirForItem(issue)
	if !strings.Contains(dir, "issue-42") {
		t.Errorf("logDirForItem single-repo = %q, expected issue-42", dir)
	}
	if !strings.Contains(dir, ".fabrik/logs") {
		t.Errorf("logDirForItem single-repo = %q, expected .fabrik/logs", dir)
	}
	// Should match flat LogDir for single-repo.
	if dir != LogDir(42) {
		t.Errorf("logDirForItem single-repo = %q, want %q", dir, LogDir(42))
	}
}

func TestLogDirForItem_MultiRepo(t *testing.T) {
	issue := gh.ProjectItem{Number: 7, Repo: "myorg/myrepo"}
	dir := logDirForItem(issue)
	if !strings.Contains(dir, "myorg-myrepo") {
		t.Errorf("logDirForItem multi-repo = %q, expected myorg-myrepo", dir)
	}
	if !strings.Contains(dir, "issue-7") {
		t.Errorf("logDirForItem multi-repo = %q, expected issue-7", dir)
	}
	if !strings.Contains(dir, ".fabrik/logs") {
		t.Errorf("logDirForItem multi-repo = %q, expected .fabrik/logs", dir)
	}
	// Must differ from flat LogDir.
	if dir == LogDir(7) {
		t.Errorf("logDirForItem multi-repo should differ from flat LogDir, got %q", dir)
	}
}

func TestFormatStatsFooter(t *testing.T) {
	tests := []struct {
		name      string
		stats     TokenUsage
		completed bool
		wantEmpty bool
		wantSubs  []string
	}{
		{
			name:      "zero stats returns empty",
			stats:     TokenUsage{},
			completed: true,
			wantEmpty: true,
		},
		{
			name:      "with turns and tokens, completed",
			stats:     TokenUsage{TurnsUsed: 15, MaxTurns: 30, InputTokens: 47000, OutputTokens: 8000},
			completed: true,
			wantSubs:  []string{"15/30 turns", "47k input", "8k output"},
		},
		{
			name:      "with turns and tokens, incomplete",
			stats:     TokenUsage{TurnsUsed: 30, MaxTurns: 30, InputTokens: 47000, OutputTokens: 8000},
			completed: false,
			wantSubs:  []string{"30/30 turns", "Stage incomplete."},
		},
		{
			name:      "no max turns",
			stats:     TokenUsage{TurnsUsed: 10, InputTokens: 5000, OutputTokens: 1000},
			completed: true,
			wantSubs:  []string{"10 turns", "5k input", "1k output"},
		},
		{
			name:      "only input tokens",
			stats:     TokenUsage{InputTokens: 5000},
			completed: true,
			wantEmpty: false,
			wantSubs:  []string{"5k input"},
		},
		{
			name:      "only output tokens",
			stats:     TokenUsage{OutputTokens: 2000},
			completed: true,
			wantEmpty: false,
			wantSubs:  []string{"2k output"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatStatsFooter(tt.stats, tt.completed)
			if tt.wantEmpty {
				if got != "" {
					t.Errorf("expected empty footer, got %q", got)
				}
				return
			}
			for _, sub := range tt.wantSubs {
				if !strings.Contains(got, sub) {
					t.Errorf("footer %q missing %q", got, sub)
				}
			}
		})
	}
}

func TestSessionFile(t *testing.T) {
	path := sessionFile(42, "Research")
	if !strings.HasSuffix(path, "Research.session") {
		t.Errorf("sessionFile = %q, expected to end with Research.session", path)
	}
}

func TestReadSessionID_FileAbsent(t *testing.T) {
	t.Chdir(t.TempDir())
	got := ReadSessionID("", 999, "Research")
	if got != "" {
		t.Errorf("expected empty string for absent file, got %q", got)
	}
}

func TestReadSessionID_FileEmpty(t *testing.T) {
	tmp := t.TempDir()
	t.Chdir(tmp)
	dir := filepath.Join(tmp, ".fabrik", "sessions", "issue-1")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Research.session"), []byte(""), 0600); err != nil {
		t.Fatal(err)
	}
	got := ReadSessionID("", 1, "Research")
	if got != "" {
		t.Errorf("expected empty string for empty file, got %q", got)
	}
}

func TestReadSessionID_ValidID(t *testing.T) {
	tmp := t.TempDir()
	t.Chdir(tmp)
	dir := filepath.Join(tmp, ".fabrik", "sessions", "issue-42")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	wantID := "abc123def456"
	if err := os.WriteFile(filepath.Join(dir, "Plan.session"), []byte(wantID), 0600); err != nil {
		t.Fatal(err)
	}
	got := ReadSessionID("", 42, "Plan")
	if got != wantID {
		t.Errorf("expected %q, got %q", wantID, got)
	}
}

func TestReadSessionID_WhitespacePaddedID(t *testing.T) {
	tmp := t.TempDir()
	t.Chdir(tmp)
	dir := filepath.Join(tmp, ".fabrik", "sessions", "issue-7")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	wantID := "session-xyz"
	if err := os.WriteFile(filepath.Join(dir, "Implement.session"), []byte("  "+wantID+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got := ReadSessionID("", 7, "Implement")
	if got != wantID {
		t.Errorf("expected %q after trimming, got %q", wantID, got)
	}
}

func TestReadSessionID_MultiRepo(t *testing.T) {
	tmp := t.TempDir()
	t.Chdir(tmp)
	dir := filepath.Join(tmp, ".fabrik", "sessions", "myorg-myrepo", "issue-55")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	wantID := "multi-repo-session"
	if err := os.WriteFile(filepath.Join(dir, "Implement.session"), []byte(wantID), 0600); err != nil {
		t.Fatal(err)
	}
	got := ReadSessionID("myorg/myrepo", 55, "Implement")
	if got != wantID {
		t.Errorf("expected %q, got %q", wantID, got)
	}
}

func TestMigrateHomeToProject_Basic(t *testing.T) {
	// Set up a fake home dir with sessions and logs.
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	// Write a session file in the old home-dir location.
	oldSessDir := filepath.Join(fakeHome, ".fabrik", "sessions", "myorg-myrepo", "issue-10")
	if err := os.MkdirAll(oldSessDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldSessDir, "Research.session"), []byte("sess_abc"), 0600); err != nil {
		t.Fatal(err)
	}

	// Write a log dir in the old home-dir location.
	oldLogDir := filepath.Join(fakeHome, ".fabrik", "logs", "myorg-myrepo", "issue-10")
	if err := os.MkdirAll(oldLogDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldLogDir, "Research.log"), []byte("log data"), 0600); err != nil {
		t.Fatal(err)
	}

	// Project dir is separate from home dir.
	projectDir := t.TempDir()

	var logs []string
	migrateHomeToProject(projectDir, func(msg string) { logs = append(logs, msg) })

	// Session should have moved.
	newSessFile := filepath.Join(projectDir, ".fabrik", "sessions", "myorg-myrepo", "issue-10", "Research.session")
	data, err := os.ReadFile(newSessFile)
	if err != nil {
		t.Fatalf("reading migrated session file: %v", err)
	}
	if string(data) != "sess_abc" {
		t.Errorf("session content = %q, want sess_abc", string(data))
	}

	// Log should have moved.
	newLogFile := filepath.Join(projectDir, ".fabrik", "logs", "myorg-myrepo", "issue-10", "Research.log")
	logData, err := os.ReadFile(newLogFile)
	if err != nil {
		t.Fatalf("reading migrated log file: %v", err)
	}
	if string(logData) != "log data" {
		t.Errorf("log content = %q, want 'log data'", string(logData))
	}

	// Old source dirs should be gone (emptied and removed).
	if _, err := os.Stat(filepath.Join(fakeHome, ".fabrik", "sessions")); !os.IsNotExist(err) {
		t.Error("old sessions dir should have been removed after migration")
	}
}

func TestMigrateHomeToProject_Idempotent(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	projectDir := t.TempDir()

	// Write session in old home-dir location.
	oldSessDir := filepath.Join(fakeHome, ".fabrik", "sessions", "issue-5")
	if err := os.MkdirAll(oldSessDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldSessDir, "Plan.session"), []byte("sess_old"), 0600); err != nil {
		t.Fatal(err)
	}

	// Pre-create destination so migration must skip it.
	newSessDir := filepath.Join(projectDir, ".fabrik", "sessions", "issue-5")
	if err := os.MkdirAll(newSessDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(newSessDir, "Plan.session"), []byte("sess_existing"), 0600); err != nil {
		t.Fatal(err)
	}

	var logs []string
	migrateHomeToProject(projectDir, func(msg string) { logs = append(logs, msg) })

	// Destination should still have the original content (not overwritten).
	data, err := os.ReadFile(filepath.Join(newSessDir, "Plan.session"))
	if err != nil {
		t.Fatalf("reading session file: %v", err)
	}
	if string(data) != "sess_existing" {
		t.Errorf("session content = %q, want sess_existing (skipped)", string(data))
	}

	// A warning should have been logged.
	found := false
	for _, l := range logs {
		if strings.Contains(l, "already exists") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected 'already exists' warning, got: %v", logs)
	}
}

func TestMigrateHomeToProject_SamePath(t *testing.T) {
	// When HOME == fabrikDir, migration should be a no-op (same-path guard).
	sameDir := t.TempDir()
	t.Setenv("HOME", sameDir)

	// Write a session file.
	sessDir := filepath.Join(sameDir, ".fabrik", "sessions", "issue-1")
	if err := os.MkdirAll(sessDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessDir, "Research.session"), []byte("sess_x"), 0600); err != nil {
		t.Fatal(err)
	}

	// Run migration with fabrikDir == fakeHome (same-path case).
	migrateHomeToProject(sameDir, nil)

	// File should still be in the original location, not moved.
	data, err := os.ReadFile(filepath.Join(sessDir, "Research.session"))
	if err != nil {
		t.Fatalf("session file should still exist: %v", err)
	}
	if string(data) != "sess_x" {
		t.Errorf("session content = %q, want sess_x", string(data))
	}
}

func TestMigrateSessions_Basic(t *testing.T) {
	skipIfNoGit(t)

	// Set up a temp worktree root with a fake git repo at the namespaced path.
	wtRoot := t.TempDir()
	issueDir := filepath.Join(wtRoot, "myorg-myrepo", "issue-10")
	if err := os.MkdirAll(issueDir, 0755); err != nil {
		t.Fatal(err)
	}
	// Initialize a bare git repo and set the remote so ownerRepoDirFromURL works.
	if out, err := exec.Command("git", "-C", issueDir, "init").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	if out, err := exec.Command("git", "-C", issueDir, "remote", "add", "origin", "git@github.com:myorg/myrepo.git").CombinedOutput(); err != nil {
		t.Fatalf("git remote add: %v\n%s", err, out)
	}

	// Set up a session root with a flat issue-10/ directory.
	sessRoot := t.TempDir()
	oldSessDir := filepath.Join(sessRoot, "issue-10")
	if err := os.MkdirAll(oldSessDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldSessDir, "Research.session"), []byte("sess_abc"), 0600); err != nil {
		t.Fatal(err)
	}

	var logs []string
	migrateSessions(sessRoot, wtRoot, func(msg string) { logs = append(logs, msg) })

	// Old path should be gone.
	if _, err := os.Stat(oldSessDir); !os.IsNotExist(err) {
		t.Errorf("old session dir %s should have been removed", oldSessDir)
	}

	// New path should exist with the session file.
	newSessDir := filepath.Join(sessRoot, "myorg-myrepo", "issue-10")
	data, err := os.ReadFile(filepath.Join(newSessDir, "Research.session"))
	if err != nil {
		t.Fatalf("reading migrated session file: %v", err)
	}
	if string(data) != "sess_abc" {
		t.Errorf("session file content = %q, want sess_abc", string(data))
	}

	// Should have logged a migration message.
	found := false
	for _, l := range logs {
		if strings.Contains(l, "migrated") && strings.Contains(l, "issue-10") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected migration log message, got: %v", logs)
	}
}

func TestMigrateSessions_NoWorktree(t *testing.T) {
	sessRoot := t.TempDir()
	wtRoot := t.TempDir()

	// Create a session dir with no corresponding worktree.
	oldSessDir := filepath.Join(sessRoot, "issue-99")
	if err := os.MkdirAll(oldSessDir, 0700); err != nil {
		t.Fatal(err)
	}

	var logs []string
	migrateSessions(sessRoot, wtRoot, func(msg string) { logs = append(logs, msg) })

	// Session should remain in place.
	if _, err := os.Stat(oldSessDir); err != nil {
		t.Errorf("session dir should remain in place when no worktree: %v", err)
	}

	// Should have logged a warning.
	found := false
	for _, l := range logs {
		if strings.Contains(l, "warn") && strings.Contains(l, "issue-99") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected warning log, got: %v", logs)
	}
}

func TestMigrateSessions_TargetAlreadyExists(t *testing.T) {
	skipIfNoGit(t)

	wtRoot := t.TempDir()
	issueDir := filepath.Join(wtRoot, "myorg-myrepo", "issue-20")
	if err := os.MkdirAll(issueDir, 0755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", issueDir, "init").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	if out, err := exec.Command("git", "-C", issueDir, "remote", "add", "origin", "https://github.com/myorg/myrepo.git").CombinedOutput(); err != nil {
		t.Fatalf("git remote add: %v\n%s", err, out)
	}

	sessRoot := t.TempDir()
	// Create flat old dir.
	oldSessDir := filepath.Join(sessRoot, "issue-20")
	if err := os.MkdirAll(oldSessDir, 0700); err != nil {
		t.Fatal(err)
	}
	// Pre-create the migration target so it already exists.
	newSessDir := filepath.Join(sessRoot, "myorg-myrepo", "issue-20")
	if err := os.MkdirAll(newSessDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(newSessDir, "existing.session"), []byte("existing"), 0600); err != nil {
		t.Fatal(err)
	}

	var logs []string
	migrateSessions(sessRoot, wtRoot, func(msg string) { logs = append(logs, msg) })

	// Old path should still exist (skipped, not renamed).
	if _, err := os.Stat(oldSessDir); err != nil {
		t.Errorf("old session dir should remain when target exists: %v", err)
	}

	// Warning should have been logged.
	found := false
	for _, l := range logs {
		if strings.Contains(l, "warn") && strings.Contains(l, "already exists") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected 'already exists' warning, got: %v", logs)
	}
}

func TestMigrateSessions_EmptyDir(t *testing.T) {
	sessRoot := t.TempDir()
	wtRoot := t.TempDir()

	// migrateSessions on an empty session root should not panic.
	migrateSessions(sessRoot, wtRoot, nil)
}

func TestMigrateSessions_NonexistentRoot(t *testing.T) {
	// migrateSessions on a nonexistent root should return gracefully.
	migrateSessions("/nonexistent/path", t.TempDir(), nil)
}
