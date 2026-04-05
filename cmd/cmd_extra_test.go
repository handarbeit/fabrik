package cmd

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/handarbeit/fabrik/config"
)

// ── upgrade ───────────────────────────────────────────────────────────────────

func TestRunUpgrade_WritesPluginFiles(t *testing.T) {
	dir := t.TempDir()
	chdirTest(t, dir)

	if err := runUpgrade(nil); err != nil {
		t.Fatalf("runUpgrade: %v", err)
	}

	// .fabrik/plugin/ should have been created with at least one file
	entries, err := os.ReadDir(filepath.Join(dir, ".fabrik", "plugin"))
	if err != nil {
		t.Fatalf(".fabrik/plugin not created: %v", err)
	}
	if len(entries) == 0 {
		t.Error("expected at least one entry in .fabrik/plugin/")
	}
}

func TestRunUpgrade_Idempotent(t *testing.T) {
	dir := t.TempDir()
	chdirTest(t, dir)

	// Run twice — second run should overwrite without error
	if err := runUpgrade(nil); err != nil {
		t.Fatalf("first runUpgrade: %v", err)
	}
	if err := runUpgrade(nil); err != nil {
		t.Fatalf("second runUpgrade: %v", err)
	}
}

func TestRefreshPlugin_WritesFiles(t *testing.T) {
	dir := t.TempDir()
	chdirTest(t, dir)

	n, err := refreshPlugin()
	if err != nil {
		t.Fatalf("refreshPlugin: %v", err)
	}
	if n == 0 {
		t.Error("expected refreshPlugin to write at least one file")
	}
}

// ── init buildConfigWithValues ────────────────────────────────────────────────

func TestBuildConfigWithValues_AllFields(t *testing.T) {
	result := buildConfigWithValues("myorg", "myrepo", "42", "myuser")

	if !strings.Contains(result, "owner: myorg") {
		t.Errorf("owner not in output: %s", result)
	}
	if !strings.Contains(result, "repo: myrepo") {
		t.Errorf("repo not in output: %s", result)
	}
	if !strings.Contains(result, "project: 42") {
		t.Errorf("project not in output: %s", result)
	}
	if !strings.Contains(result, "user: myuser") {
		t.Errorf("user not in output: %s", result)
	}
}

func TestBuildConfigWithValues_EmptyFields_KeepsComments(t *testing.T) {
	result := buildConfigWithValues("", "", "", "")

	// Empty strings should leave commented lines untouched
	if strings.Contains(result, "owner: ") && !strings.Contains(result, "# owner:") {
		t.Error("expected owner to remain commented when empty")
	}
	// The output should still be valid YAML-template text
	if len(result) == 0 {
		t.Error("result should not be empty")
	}
}

func TestBuildConfigWithValues_PartialFields(t *testing.T) {
	result := buildConfigWithValues("acme", "", "5", "")

	if !strings.Contains(result, "owner: acme") {
		t.Errorf("owner not replaced: %s", result)
	}
	if !strings.Contains(result, "project: 5") {
		t.Errorf("project not replaced: %s", result)
	}
	// repo and user should remain commented
	if strings.Contains(result, "\nrepo: ") {
		t.Errorf("repo should remain commented when empty: %s", result)
	}
}

// ── streamfilter ─────────────────────────────────────────────────────────────

func TestRunStreamFilter_ValidJSON(t *testing.T) {
	// Pipe valid JSON through the filter
	input := `{"type":"result","result":{"usage":{"input_tokens":10,"output_tokens":5}}}`

	origStdin := os.Stdin
	origStdout := os.Stdout
	defer func() {
		os.Stdin = origStdin
		os.Stdout = origStdout
	}()

	// Create a pipe for stdin
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdin = r

	// Create a pipe for stdout
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = outW

	// Write input and close
	w.WriteString(input + "\n")
	w.Close()

	RunStreamFilter()
	outW.Close()

	var buf bytes.Buffer
	io.Copy(&buf, outR)
	outR.Close()

	// Should not panic and should produce some output
	_ = buf.String()
}

func TestRunStreamFilter_MalformedJSON(t *testing.T) {
	origStdin := os.Stdin
	origStdout := os.Stdout
	defer func() {
		os.Stdin = origStdin
		os.Stdout = origStdout
	}()

	r, w, _ := os.Pipe()
	os.Stdin = r
	outR, outW, _ := os.Pipe()
	os.Stdout = outW

	w.WriteString("not json at all\n{\"valid\": true}\n")
	w.Close()

	RunStreamFilter()
	outW.Close()
	io.Copy(io.Discard, outR)
	outR.Close()
}

// ── root.go helpers ───────────────────────────────────────────────────────────

func TestDetectTerminalFromEnv(t *testing.T) {
	cases := []struct {
		env  string
		want string
	}{
		{"Apple_Terminal", "terminal"},
		{"iTerm.app", "iterm2"},
		{"ghostty", "ghostty"},
		{"WarpTerminal", "warp"},
		{"alacritty", "alacritty"},
		{"unknown", ""},
		{"", ""},
	}
	for _, tc := range cases {
		os.Setenv("TERM_PROGRAM", tc.env)
		got := detectTerminalFromEnv()
		if got != tc.want {
			t.Errorf("TERM_PROGRAM=%q: got %q, want %q", tc.env, got, tc.want)
		}
	}
	os.Unsetenv("TERM_PROGRAM")
}

func TestBuildProjectInfo_HomeRelative(t *testing.T) {
	dir := t.TempDir()
	chdirTest(t, dir)

	cfg := &Config{Owner: "acme", Repo: "myrepo"}
	info := buildProjectInfo(cfg, config.ProjectConfig{})

	if info.Repo != "acme/myrepo" {
		t.Errorf("Repo = %q, want %q", info.Repo, "acme/myrepo")
	}
	// CWD should not be empty
	if info.CWD == "" {
		t.Error("CWD should not be empty")
	}
}

func TestExecute_InvalidFabrikProjectNumber(t *testing.T) {
	resetFlags()
	dir := t.TempDir()
	chdirTest(t, dir)
	os.WriteFile(".gitignore", []byte(".env\n"), 0644)
	t.Setenv("FABRIK_PROJECT_NUMBER", "notanumber")
	t.Setenv("GITHUB_TOKEN", "tok")
	os.Args = []string{"fabrik", "--owner", "o", "--repo", "r", "--user", "u"}

	err := Execute()
	if err == nil {
		t.Fatal("expected error for invalid FABRIK_PROJECT_NUMBER")
	}
}

func TestWriteConfigTemplate_CreatesNewFile(t *testing.T) {
	dir := t.TempDir()
	chdirTest(t, dir)
	if err := os.MkdirAll(".fabrik", 0755); err != nil {
		t.Fatal(err)
	}

	if err := writeConfigTemplate(false); err != nil {
		t.Fatalf("writeConfigTemplate: %v", err)
	}
	content, err := os.ReadFile(".fabrik/config.yaml")
	if err != nil {
		t.Fatalf("config.yaml not created: %v", err)
	}
	if len(content) == 0 {
		t.Error("config.yaml should not be empty")
	}
}

func TestBuildProjectInfo_RepoFormat(t *testing.T) {
	dir := t.TempDir()
	chdirTest(t, dir)
	cfg := &Config{Owner: "org", Repo: "repo"}
	info := buildProjectInfo(cfg, config.ProjectConfig{Version: "v1.2.3"})
	if info.Repo != "org/repo" {
		t.Errorf("Repo = %q, want org/repo", info.Repo)
	}
	if info.Version != "v1.2.3" {
		t.Errorf("Version = %q, want v1.2.3", info.Version)
	}
}
