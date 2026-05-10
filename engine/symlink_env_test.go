package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSymlinkEnvIfEnabled_SourcePresent(t *testing.T) {
	fabrikDir := t.TempDir()
	workDir := t.TempDir()

	srcEnv := filepath.Join(fabrikDir, ".env")
	if err := os.WriteFile(srcEnv, []byte("API_KEY=secret\n"), 0600); err != nil {
		t.Fatal(err)
	}

	eng := NewWithDeps(Config{SymlinkEnv: true}, nil, nil, nil)
	eng.fabrikDir = fabrikDir
	eng.symlinkEnvIfEnabled(1, workDir)

	dst := filepath.Join(workDir, ".env")
	fi, err := os.Lstat(dst)
	if err != nil {
		t.Fatalf("expected .env symlink in worktree, got: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("expected .env to be a symlink")
	}

	// Verify the symlink resolves to the source content.
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("reading symlink target: %v", err)
	}
	if string(data) != "API_KEY=secret\n" {
		t.Errorf("unexpected content %q", data)
	}
}

func TestSymlinkEnvIfEnabled_SourceAbsent(t *testing.T) {
	fabrikDir := t.TempDir()
	workDir := t.TempDir()

	eng := NewWithDeps(Config{SymlinkEnv: true}, nil, nil, nil)
	eng.fabrikDir = fabrikDir
	eng.symlinkEnvIfEnabled(1, workDir)

	dst := filepath.Join(workDir, ".env")
	if _, err := os.Lstat(dst); !os.IsNotExist(err) {
		t.Errorf("expected no .env in worktree when source absent, got: %v", err)
	}
}

func TestSymlinkEnvIfEnabled_DestExists(t *testing.T) {
	fabrikDir := t.TempDir()
	workDir := t.TempDir()

	srcEnv := filepath.Join(fabrikDir, ".env")
	if err := os.WriteFile(srcEnv, []byte("API_KEY=source\n"), 0600); err != nil {
		t.Fatal(err)
	}

	// Pre-existing .env in worktree with different content.
	dst := filepath.Join(workDir, ".env")
	if err := os.WriteFile(dst, []byte("API_KEY=existing\n"), 0600); err != nil {
		t.Fatal(err)
	}

	eng := NewWithDeps(Config{SymlinkEnv: true}, nil, nil, nil)
	eng.fabrikDir = fabrikDir
	eng.symlinkEnvIfEnabled(1, workDir)

	// Existing file must remain untouched.
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "API_KEY=existing\n" {
		t.Errorf("pre-existing .env was modified: %q", data)
	}
	// Must not be a symlink — it was a regular file before.
	fi, _ := os.Lstat(dst)
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Error("pre-existing regular file was replaced by a symlink")
	}
}

func TestSymlinkEnvIfEnabled_Disabled(t *testing.T) {
	fabrikDir := t.TempDir()
	workDir := t.TempDir()

	srcEnv := filepath.Join(fabrikDir, ".env")
	if err := os.WriteFile(srcEnv, []byte("API_KEY=secret\n"), 0600); err != nil {
		t.Fatal(err)
	}

	eng := NewWithDeps(Config{SymlinkEnv: false}, nil, nil, nil)
	eng.fabrikDir = fabrikDir
	eng.symlinkEnvIfEnabled(1, workDir)

	dst := filepath.Join(workDir, ".env")
	if _, err := os.Lstat(dst); !os.IsNotExist(err) {
		t.Errorf("expected no .env when SymlinkEnv disabled, got: %v", err)
	}
}

func TestEnsureEnvExcluded_WritesEntry(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)

	eng := NewWithDeps(Config{SymlinkEnv: true}, nil, nil, nil)
	eng.fabrikDir = repoDir
	eng.ensureEnvExcluded(1, repoDir)

	// Verify .env appears in the exclude file.
	out, err := readGitExclude(t, repoDir)
	if err != nil {
		t.Fatalf("reading exclude: %v", err)
	}
	if !strings.Contains(out, ".env") {
		t.Errorf("expected .env in git exclude, got:\n%s", out)
	}
}

func TestEnsureEnvExcluded_Idempotent(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)

	eng := NewWithDeps(Config{SymlinkEnv: true}, nil, nil, nil)
	eng.fabrikDir = repoDir
	eng.ensureEnvExcluded(1, repoDir)
	eng.ensureEnvExcluded(1, repoDir)

	out, err := readGitExclude(t, repoDir)
	if err != nil {
		t.Fatalf("reading exclude: %v", err)
	}
	count := strings.Count(out, ".env\n")
	if count != 1 {
		t.Errorf("expected exactly one .env entry, got %d:\n%s", count, out)
	}
}

func TestEnsureEnvExcluded_Disabled(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)

	eng := NewWithDeps(Config{SymlinkEnv: false}, nil, nil, nil)
	eng.fabrikDir = repoDir
	eng.ensureEnvExcluded(1, repoDir)

	out, _ := readGitExclude(t, repoDir)
	if strings.Contains(out, ".env") {
		t.Errorf("expected no .env in exclude when disabled, got:\n%s", out)
	}
}

// readGitExclude returns the contents of the per-repo git exclude file.
func readGitExclude(t *testing.T, repoDir string) (string, error) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoDir, ".git", "info", "exclude"))
	return string(data), err
}
