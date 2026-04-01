package config

import (
	"os"
	"path/filepath"
	"testing"
)

// chdir changes to dir for the duration of the test, restoring the original on cleanup.
func chdir(t *testing.T, dir string) {
	t.Helper()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(orig) })
}

func TestIsInGitignore_Present(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(".env\n"), 0644)
	chdir(t, dir)
	if !isInGitignore(".env") {
		t.Error("expected .env to be found in .gitignore")
	}
}

func TestIsInGitignore_GlobPattern(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("**/.env\n"), 0644)
	chdir(t, dir)
	if !isInGitignore(".env") {
		t.Error("expected **/.env pattern to match .env")
	}
}

func TestIsInGitignore_Commented(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("# .env\n"), 0644)
	chdir(t, dir)
	if isInGitignore(".env") {
		t.Error("commented-out .env should not count as ignored")
	}
}

func TestIsInGitignore_Absent(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("*.log\nnode_modules/\n"), 0644)
	chdir(t, dir)
	if isInGitignore(".env") {
		t.Error("expected .env not to be found in .gitignore")
	}
}

func TestIsInGitignore_NoGitignoreFile(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)
	if isInGitignore(".env") {
		t.Error("expected false when .gitignore does not exist")
	}
}

func TestLoadDotenv_NoEnvFile(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)
	if err := LoadDotenv(); err != nil {
		t.Errorf("expected nil when no .env file, got: %v", err)
	}
}

func TestLoadDotenv_EnvNotInGitignore(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".env"), []byte("FABRIK_OWNER=test\n"), 0600)
	os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("*.log\n"), 0644)
	chdir(t, dir)
	if err := LoadDotenv(); err == nil {
		t.Error("expected fatal error when .env is not in .gitignore")
	}
}

func TestLoadDotenv_Success(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".env"), []byte("FABRIK_OWNER=myorg\n"), 0600)
	os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(".env\n"), 0644)
	chdir(t, dir)
	os.Unsetenv("FABRIK_OWNER")
	t.Cleanup(func() { os.Unsetenv("FABRIK_OWNER") })
	if err := LoadDotenv(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := os.Getenv("FABRIK_OWNER"); got != "myorg" {
		t.Errorf("expected FABRIK_OWNER=myorg, got %q", got)
	}
}

func TestToken_FabrikTokenPreferred(t *testing.T) {
	t.Setenv("FABRIK_TOKEN", "fabrik-tok")
	t.Setenv("GITHUB_TOKEN", "github-tok")
	if got := Token(); got != "fabrik-tok" {
		t.Errorf("expected FABRIK_TOKEN to win, got %q", got)
	}
}

func TestToken_FallbackToGitHubToken(t *testing.T) {
	t.Setenv("FABRIK_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "github-tok")
	if got := Token(); got != "github-tok" {
		t.Errorf("expected GITHUB_TOKEN fallback, got %q", got)
	}
}
