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

func TestIsInGitignore_SimilarName(t *testing.T) {
	dir := t.TempDir()
	// .envrc and .env.example should NOT match ".env"
	os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(".envrc\n.env.example\n"), 0644)
	chdir(t, dir)
	if isInGitignore(".env") {
		t.Error(".envrc / .env.example entries must not falsely match .env")
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

func TestLoadProjectConfig_MissingFile(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)
	pc, err := LoadProjectConfig()
	if err != nil {
		t.Fatalf("expected nil error for missing file, got: %v", err)
	}
	if pc.Owner != "" || pc.Repo != "" || pc.ProjectNum != nil {
		t.Error("expected zero-value struct for missing config file")
	}
}

func TestLoadProjectConfig_ValidYAML(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)
	if err := os.MkdirAll(filepath.Join(dir, ".fabrik"), 0755); err != nil {
		t.Fatal(err)
	}
	yaml := `
owner: myorg
repo: myrepo
project: 42
user: alice
stages: ./.fabrik/stages
poll: 60
max_concurrent: 3
max_retries: 5
yolo: true
auto_upgrade: true
tui: true
terminal: iTerm.app
debug_output: true
version: "2.0.0"
`
	os.WriteFile(filepath.Join(dir, ".fabrik", "config.yaml"), []byte(yaml), 0644)

	pc, err := LoadProjectConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pc.Owner != "myorg" {
		t.Errorf("owner: want myorg, got %q", pc.Owner)
	}
	if pc.Repo != "myrepo" {
		t.Errorf("repo: want myrepo, got %q", pc.Repo)
	}
	if pc.ProjectNum == nil || *pc.ProjectNum != 42 {
		t.Errorf("project: want 42, got %v", pc.ProjectNum)
	}
	if pc.User != "alice" {
		t.Errorf("user: want alice, got %q", pc.User)
	}
	if pc.Poll == nil || *pc.Poll != 60 {
		t.Errorf("poll: want 60, got %v", pc.Poll)
	}
	if pc.MaxConcurrent == nil || *pc.MaxConcurrent != 3 {
		t.Errorf("max_concurrent: want 3, got %v", pc.MaxConcurrent)
	}
	if pc.MaxRetries == nil || *pc.MaxRetries != 5 {
		t.Errorf("max_retries: want 5, got %v", pc.MaxRetries)
	}
	if !pc.Yolo {
		t.Error("yolo: want true")
	}
	if !pc.AutoUpgrade {
		t.Error("auto_upgrade: want true")
	}
	if pc.TUI == nil || !*pc.TUI {
		t.Error("tui: want true")
	}
	if pc.Terminal != "iTerm.app" {
		t.Errorf("terminal: want iTerm.app, got %q", pc.Terminal)
	}
	if !pc.DebugOutput {
		t.Error("debug_output: want true")
	}
	if pc.Version != "2.0.0" {
		t.Errorf("version: want 2.0.0, got %q", pc.Version)
	}
}

func TestLoadProjectConfig_UnknownKeysIgnored(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)
	if err := os.MkdirAll(filepath.Join(dir, ".fabrik"), 0755); err != nil {
		t.Fatal(err)
	}
	yaml := "owner: org\nunknown_key: value\n"
	os.WriteFile(filepath.Join(dir, ".fabrik", "config.yaml"), []byte(yaml), 0644)

	pc, err := LoadProjectConfig()
	if err != nil {
		t.Fatalf("unknown keys should be ignored, got error: %v", err)
	}
	if pc.Owner != "org" {
		t.Errorf("owner: want org, got %q", pc.Owner)
	}
}

func TestLoadProjectConfig_PointerFieldsAbsent(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)
	if err := os.MkdirAll(filepath.Join(dir, ".fabrik"), 0755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, ".fabrik", "config.yaml"), []byte("owner: org\n"), 0644)

	pc, err := LoadProjectConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pc.ProjectNum != nil {
		t.Error("project: want nil when absent")
	}
	if pc.Poll != nil {
		t.Error("poll: want nil when absent")
	}
	if pc.MaxConcurrent != nil {
		t.Error("max_concurrent: want nil when absent")
	}
	if pc.MaxRetries != nil {
		t.Error("max_retries: want nil when absent")
	}
	if pc.TUI != nil {
		t.Error("tui: want nil when absent")
	}
}

func TestLoadProjectConfig_InvalidYAML(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)
	if err := os.MkdirAll(filepath.Join(dir, ".fabrik"), 0755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, ".fabrik", "config.yaml"), []byte(":\n  - invalid: [yaml\n"), 0644)

	_, err := LoadProjectConfig()
	if err == nil {
		t.Error("expected error for invalid YAML")
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
