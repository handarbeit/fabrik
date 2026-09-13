package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	fabrikplugin "github.com/handarbeit/fabrik/plugin"
	"github.com/handarbeit/fabrik/stages"
)

func skipIfNoGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH")
	}
}

func TestRunInit_WritesFiles(t *testing.T) {
	dir := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(orig) //nolint

	if err := runInit([]string{}); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	entries, err := os.ReadDir(filepath.Join(dir, ".fabrik", "stages"))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}

	// Count embedded source files to verify all were written.
	embedded, err := fs.ReadDir(stages.DefaultStages, "examples")
	if err != nil {
		t.Fatalf("reading embedded stages: %v", err)
	}
	embeddedFiles := 0
	for _, e := range embedded {
		if !e.IsDir() {
			embeddedFiles++
		}
	}
	writtenFiles := 0
	for _, e := range entries {
		if !e.IsDir() {
			writtenFiles++
		}
	}
	if writtenFiles != embeddedFiles {
		t.Fatalf("expected %d file(s) written, got %d", embeddedFiles, writtenFiles)
	}

	// Verify each written file matches the embedded source exactly.
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		written, err := os.ReadFile(filepath.Join(dir, ".fabrik", "stages", e.Name()))
		if err != nil {
			t.Fatalf("reading written file %s: %v", e.Name(), err)
		}
		source, err := stages.DefaultStages.ReadFile("examples/" + e.Name())
		if err != nil {
			t.Fatalf("reading embedded source %s: %v", e.Name(), err)
		}
		if string(written) != string(source) {
			t.Errorf("file %s: content mismatch", e.Name())
		}
	}
}

func TestRunInit_WritesPluginFiles(t *testing.T) {
	dir := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(orig); err != nil {
			t.Fatalf("restoring working directory: %v", err)
		}
	})

	if err := runInit([]string{}); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	// Collect all embedded plugin file paths.
	var embeddedPaths []string
	if err := fs.WalkDir(fabrikplugin.FabrikPlugin, "fabrik-workflows", func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !d.IsDir() {
			embeddedPaths = append(embeddedPaths, p)
		}
		return nil
	}); err != nil {
		t.Fatalf("walking embedded plugin: %v", err)
	}
	if len(embeddedPaths) == 0 {
		t.Fatal("no embedded plugin files found")
	}

	// Verify each embedded file was written to .fabrik/plugin/ with matching content.
	for _, p := range embeddedPaths {
		rel, err := filepath.Rel("fabrik-workflows", p)
		if err != nil {
			t.Fatalf("computing relative path for embedded plugin file %s: %v", p, err)
		}
		destPath := filepath.Join(dir, ".fabrik", "plugin", rel)

		written, err := os.ReadFile(destPath)
		if err != nil {
			t.Errorf("plugin file %s not written: %v", rel, err)
			continue
		}
		source, err := fabrikplugin.FabrikPlugin.ReadFile(p)
		if err != nil {
			t.Fatalf("reading embedded plugin file %s: %v", p, err)
		}
		if string(written) != string(source) {
			t.Errorf("plugin file %s: content mismatch", rel)
		}
	}
}

func TestRunInit_SkipsExistingFiles(t *testing.T) {
	dir := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(orig) //nolint

	// First init — writes all files.
	if err := runInit([]string{}); err != nil {
		t.Fatalf("first runInit: %v", err)
	}

	// Overwrite one file with sentinel content.
	stagesDir := filepath.Join(dir, ".fabrik", "stages")
	entries, err := os.ReadDir(stagesDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no files written by first init")
	}
	sentinel := []byte("sentinel content")
	targetPath := filepath.Join(stagesDir, entries[0].Name())
	if err := os.WriteFile(targetPath, sentinel, 0644); err != nil {
		t.Fatal(err)
	}

	// Second init — should skip the existing file.
	if err := runInit([]string{}); err != nil {
		t.Fatalf("second runInit: %v", err)
	}

	got, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(sentinel) {
		t.Errorf("existing file was overwritten; want sentinel, got %q", string(got))
	}
}

func TestRunInit_ForceOverwrites(t *testing.T) {
	dir := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(orig) //nolint

	// First init.
	if err := runInit([]string{}); err != nil {
		t.Fatalf("first runInit: %v", err)
	}

	stagesDir := filepath.Join(dir, ".fabrik", "stages")
	entries, err := os.ReadDir(stagesDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no files written by first init")
	}

	// Overwrite one file with sentinel.
	sentinel := []byte("sentinel content")
	targetPath := filepath.Join(stagesDir, entries[0].Name())
	if err := os.WriteFile(targetPath, sentinel, 0644); err != nil {
		t.Fatal(err)
	}

	// Second init with --force — should overwrite.
	if err := runInit([]string{"--force"}); err != nil {
		t.Fatalf("force runInit: %v", err)
	}

	got, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) == string(sentinel) {
		t.Error("--force did not overwrite existing file")
	}
}

func TestRunInit_WritesConfigYAML(t *testing.T) {
	dir := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(orig) //nolint

	if err := runInit([]string{}); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, ".fabrik", "config.yaml"))
	if err != nil {
		t.Fatalf("config.yaml not written: %v", err)
	}
	content := string(data)
	// All required fields should be commented out in non-interactive mode
	if !strings.Contains(content, "# owner:") {
		t.Error("expected '# owner:' in config.yaml template")
	}
	if !strings.Contains(content, "# repo:") {
		t.Error("expected '# repo:' in config.yaml template")
	}
	if !strings.Contains(content, "# project:") {
		t.Error("expected '# project:' in config.yaml template")
	}
	if !strings.Contains(content, "# user:") {
		t.Error("expected '# user:' in config.yaml template")
	}
}

func TestRunInit_ConfigYAMLNotOverwrittenWithoutForce(t *testing.T) {
	dir := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(orig) //nolint

	// First init writes the template
	if err := runInit([]string{}); err != nil {
		t.Fatalf("first runInit: %v", err)
	}

	// Overwrite with sentinel
	configPath := filepath.Join(dir, ".fabrik", "config.yaml")
	sentinel := []byte("owner: sentinel\n")
	if err := os.WriteFile(configPath, sentinel, 0644); err != nil {
		t.Fatal(err)
	}

	// Second init without --force should skip
	if err := runInit([]string{}); err != nil {
		t.Fatalf("second runInit: %v", err)
	}

	got, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(sentinel) {
		t.Errorf("config.yaml was overwritten without --force; want sentinel, got %q", string(got))
	}
}

func TestRunInit_ConfigYAMLOverwrittenWithForce(t *testing.T) {
	dir := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(orig) //nolint

	// Write sentinel first
	os.MkdirAll(filepath.Join(dir, ".fabrik"), 0755)
	configPath := filepath.Join(dir, ".fabrik", "config.yaml")
	if err := os.WriteFile(configPath, []byte("owner: sentinel\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Init with --force should overwrite
	if err := runInit([]string{"--force"}); err != nil {
		t.Fatalf("force runInit: %v", err)
	}

	got, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) == "owner: sentinel\n" {
		t.Error("--force did not overwrite config.yaml")
	}
	if !strings.Contains(string(got), "# owner:") {
		t.Error("expected template content after --force overwrite")
	}
}

func TestRunInit_RejectsInvalidURL(t *testing.T) {
	cases := []struct {
		args []string
		desc string
	}{
		{[]string{"not-a-url"}, "non-URL string"},
		{[]string{"https://example.com/users/foo/projects/1"}, "wrong host"},
		{[]string{"https://github.com/repos/foo/projects/1"}, "wrong kind segment"},
		{[]string{"https://github.com/users/foo/issues/1"}, "projects segment missing"},
		{[]string{"https://github.com/users/foo/projects/abc"}, "non-integer project number"},
		{[]string{"https://github.com/users/foo/projects/0"}, "zero project number"},
		{[]string{"one", "two"}, "too many positional args"},
	}
	for _, tc := range cases {
		if err := runInit(tc.args); err == nil {
			t.Errorf("expected error for %s (%v), got nil", tc.desc, tc.args)
		}
	}
}

func TestRunInit_CreateBoardFlagValidation(t *testing.T) {
	cases := []struct {
		args []string
		desc string
	}{
		{[]string{"--create-board", "https://github.com/orgs/foo/projects/1"}, "create-board combined with project URL"},
		{[]string{"--create-board"}, "create-board missing --owner and --repo"},
		{[]string{"--create-board", "--owner", "acme"}, "create-board missing --repo"},
		{[]string{"--create-board", "--repo", "widgets"}, "create-board missing --owner"},
	}
	for _, tc := range cases {
		if err := runInit(tc.args); err == nil {
			t.Errorf("expected error for %s (%v), got nil", tc.desc, tc.args)
		}
	}
}

// TestRunInit_CreateBoardRefusedWhenAlreadyConfigured is the regression test
// for the review finding on PR #1718: without a pre-flight check,
// --create-board would create a brand-new GitHub Project even when
// .fabrik/config.yaml already points at one, then either silently skip
// writing the new board's details (writeConfigTemplate's own
// no-op-without-force) or, on a repeat run, create yet another duplicate
// board. The refusal must fire before any network call — this test supplies
// no token and no reachable GitHub client, so a network attempt would fail
// with a different, token-related error rather than the refusal message.
func TestRunInit_CreateBoardRefusedWhenAlreadyConfigured(t *testing.T) {
	dir := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(orig) //nolint

	if err := os.MkdirAll(".fabrik", 0755); err != nil {
		t.Fatal(err)
	}
	existing := "owner: acme\nproject: 5\n"
	if err := os.WriteFile(".fabrik/config.yaml", []byte(existing), 0644); err != nil {
		t.Fatal(err)
	}

	err = runInit([]string{"--create-board", "--owner", "acme", "--repo", "widgets"})
	if err == nil {
		t.Fatal("expected --create-board to be refused when .fabrik/config.yaml is already configured, got nil")
	}
	if !strings.Contains(err.Error(), "already configures") {
		t.Errorf("error %q does not explain the refusal", err.Error())
	}

	got, readErr := os.ReadFile(".fabrik/config.yaml")
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != existing {
		t.Errorf("existing config.yaml was modified despite the refusal; want %q, got %q", existing, string(got))
	}
}

// TestRunInit_CreateBoardForceOverridesAlreadyConfiguredRefusal confirms
// --force opts back into the pre-#1718-review behavior: the refusal above is
// bypassed and --create-board proceeds (immediately hitting the
// token-loading step here, since no real GitHub credentials are available in
// this test — proving the guard, not the full create flow, is what --force
// disables).
func TestRunInit_CreateBoardForceOverridesAlreadyConfiguredRefusal(t *testing.T) {
	dir := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(orig) //nolint

	t.Setenv("FABRIK_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")

	if err := os.MkdirAll(".fabrik", 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(".fabrik/config.yaml", []byte("owner: acme\nproject: 5\n"), 0644); err != nil {
		t.Fatal(err)
	}

	err = runInit([]string{"--force", "--create-board", "--owner", "acme", "--repo", "widgets"})
	if err == nil {
		t.Fatal("expected an error (no GitHub token available in test), got nil")
	}
	if strings.Contains(err.Error(), "already configures") {
		t.Errorf("--force should have bypassed the already-configured refusal, got: %v", err)
	}
}

func TestParseProjectURL(t *testing.T) {
	cases := []struct {
		rawURL        string
		wantOwner     string
		wantProject   string
		wantOwnerType string
		wantErr       bool
	}{
		// User project URL (no views)
		{
			"https://github.com/users/alice/projects/5",
			"alice", "5", "user", false,
		},
		// User project URL with /views suffix
		{
			"https://github.com/users/alice/projects/5/views/1",
			"alice", "5", "user", false,
		},
		// Org project URL (no views)
		{
			"https://github.com/orgs/acme/projects/3",
			"acme", "3", "organization", false,
		},
		// Org project URL with /views suffix
		{
			"https://github.com/orgs/acme/projects/3/views/2",
			"acme", "3", "organization", false,
		},
		// Invalid: wrong host
		{"https://example.com/users/alice/projects/5", "", "", "", true},
		// Invalid: wrong kind segment
		{"https://github.com/repos/alice/projects/5", "", "", "", true},
		// Invalid: non-integer project number
		{"https://github.com/users/alice/projects/abc", "", "", "", true},
		// Invalid: zero project number
		{"https://github.com/users/alice/projects/0", "", "", "", true},
		// Invalid: too few segments
		{"https://github.com/users/alice", "", "", "", true},
	}
	for _, tc := range cases {
		owner, project, ownerType, err := parseProjectURL(tc.rawURL, "")
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseProjectURL(%q): expected error, got nil", tc.rawURL)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseProjectURL(%q): unexpected error: %v", tc.rawURL, err)
			continue
		}
		if owner != tc.wantOwner {
			t.Errorf("parseProjectURL(%q): owner = %q, want %q", tc.rawURL, owner, tc.wantOwner)
		}
		if project != tc.wantProject {
			t.Errorf("parseProjectURL(%q): project = %q, want %q", tc.rawURL, project, tc.wantProject)
		}
		if ownerType != tc.wantOwnerType {
			t.Errorf("parseProjectURL(%q): ownerType = %q, want %q", tc.rawURL, ownerType, tc.wantOwnerType)
		}
	}
}

// TestParseProjectURL_GHESHost covers parseProjectURL's ghesHost parameter
// directly: a configured GHES host is accepted alongside github.com (not
// instead of it — a single engine may still manage a github.com project),
// and a host matching neither is rejected with an error naming both.
func TestParseProjectURL_GHESHost(t *testing.T) {
	cases := []struct {
		desc          string
		rawURL        string
		ghesHost      string
		wantOwner     string
		wantProject   string
		wantOwnerType string
		wantErr       bool
		wantErrSubstr string
	}{
		{
			desc:   "GHES host URL accepted when configured",
			rawURL: "https://github.example.com/orgs/acme/projects/7", ghesHost: "github.example.com",
			wantOwner: "acme", wantProject: "7", wantOwnerType: "organization",
		},
		{
			desc:   "GHES host URL with /views suffix accepted when configured",
			rawURL: "https://github.example.com/users/alice/projects/5/views/2", ghesHost: "github.example.com",
			wantOwner: "alice", wantProject: "5", wantOwnerType: "user",
		},
		{
			desc:   "github.com URL still accepted when a GHES host is configured",
			rawURL: "https://github.com/orgs/acme/projects/7", ghesHost: "github.example.com",
			wantOwner: "acme", wantProject: "7", wantOwnerType: "organization",
		},
		{
			desc:   "host matching neither github.com nor the configured GHES host is rejected, naming both",
			rawURL: "https://wrong-host.example.com/orgs/acme/projects/7", ghesHost: "github.example.com",
			wantErr: true, wantErrSubstr: "host must be github.com or github.example.com",
		},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			owner, project, ownerType, err := parseProjectURL(tc.rawURL, tc.ghesHost)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if tc.wantErrSubstr != "" && !strings.Contains(err.Error(), tc.wantErrSubstr) {
					t.Errorf("error %q does not contain %q", err.Error(), tc.wantErrSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if owner != tc.wantOwner || project != tc.wantProject || ownerType != tc.wantOwnerType {
				t.Errorf("got (%q, %q, %q), want (%q, %q, %q)", owner, project, ownerType, tc.wantOwner, tc.wantProject, tc.wantOwnerType)
			}
		})
	}
}

func TestRunInit_URLPopulatesConfig(t *testing.T) {
	dir := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(orig) //nolint

	if err := runInit([]string{"https://github.com/orgs/acme/projects/7"}); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, ".fabrik", "config.yaml"))
	if err != nil {
		t.Fatalf("config.yaml not written: %v", err)
	}
	content := string(data)

	if !strings.Contains(content, "owner: acme") {
		t.Errorf("expected 'owner: acme' in config, got:\n%s", content)
	}
	if !strings.Contains(content, "project: 7") {
		t.Errorf("expected 'project: 7' in config, got:\n%s", content)
	}
	if !strings.Contains(content, "owner_type: organization") {
		t.Errorf("expected 'owner_type: organization' in config, got:\n%s", content)
	}
	// repo should remain commented (multi-repo default)
	if strings.Contains(content, "\nrepo: ") {
		t.Errorf("repo should remain commented when URL is provided, got:\n%s", content)
	}
}

func TestRunInit_UserFlag(t *testing.T) {
	dir := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(orig) //nolint

	err = runInit([]string{"--user", "acme", "https://github.com/users/acme/projects/5"})
	if err != nil {
		t.Fatalf("runInit: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, ".fabrik", "config.yaml"))
	if err != nil {
		t.Fatalf("config.yaml not written: %v", err)
	}
	content := string(data)

	if !strings.Contains(content, "owner: acme") {
		t.Errorf("expected 'owner: acme', got:\n%s", content)
	}
	if !strings.Contains(content, "project: 5") {
		t.Errorf("expected 'project: 5', got:\n%s", content)
	}
	if !strings.Contains(content, "owner_type: user") {
		t.Errorf("expected 'owner_type: user', got:\n%s", content)
	}
	if !strings.Contains(content, "user: acme") {
		t.Errorf("expected 'user: acme', got:\n%s", content)
	}
}

// TestRunInit_GHESHost_EnvVar_URLPopulatesConfig covers acceptance [1]:
// FABRIK_GHES_HOST plus a matching GHES project URL parses successfully and
// persists ghes_host into the written config, so it doesn't need to be
// supplied again on every subsequent `fabrik` invocation.
func TestRunInit_GHESHost_EnvVar_URLPopulatesConfig(t *testing.T) {
	dir := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(orig) //nolint

	t.Setenv("FABRIK_GHES_HOST", "github.example.com")

	if err := runInit([]string{"https://github.example.com/orgs/acme/projects/7"}); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, ".fabrik", "config.yaml"))
	if err != nil {
		t.Fatalf("config.yaml not written: %v", err)
	}
	content := string(data)

	if !strings.Contains(content, "owner: acme") {
		t.Errorf("expected 'owner: acme' in config, got:\n%s", content)
	}
	if !strings.Contains(content, "project: 7") {
		t.Errorf("expected 'project: 7' in config, got:\n%s", content)
	}
	if !strings.Contains(content, "ghes_host: github.example.com") {
		t.Errorf("expected 'ghes_host: github.example.com' in config, got:\n%s", content)
	}
}

// TestRunInit_GHESHostFlag_URLPopulatesConfig covers the --ghes-host flag
// form of acceptance [1] (not just the env var).
func TestRunInit_GHESHostFlag_URLPopulatesConfig(t *testing.T) {
	dir := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(orig) //nolint

	if err := runInit([]string{"--ghes-host", "github.example.com", "https://github.example.com/orgs/acme/projects/7"}); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, ".fabrik", "config.yaml"))
	if err != nil {
		t.Fatalf("config.yaml not written: %v", err)
	}
	content := string(data)

	if !strings.Contains(content, "ghes_host: github.example.com") {
		t.Errorf("expected 'ghes_host: github.example.com' in config, got:\n%s", content)
	}
}

// TestRunInit_NoGHESHost_GithubComURL_Unchanged covers acceptance [2]: with
// no GHES host configured, a github.com URL behaves exactly as before this
// change — including the config NOT gaining a ghes_host line. This is the
// regression that matters most, since every existing user is on this path.
func TestRunInit_NoGHESHost_GithubComURL_Unchanged(t *testing.T) {
	dir := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(orig) //nolint

	t.Setenv("FABRIK_GHES_HOST", "")

	if err := runInit([]string{"https://github.com/orgs/acme/projects/7"}); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, ".fabrik", "config.yaml"))
	if err != nil {
		t.Fatalf("config.yaml not written: %v", err)
	}
	content := string(data)

	if !strings.Contains(content, "owner: acme") {
		t.Errorf("expected 'owner: acme' in config, got:\n%s", content)
	}
	if !strings.Contains(content, "project: 7") {
		t.Errorf("expected 'project: 7' in config, got:\n%s", content)
	}
	if !strings.Contains(content, "# ghes_host:") {
		t.Errorf("expected ghes_host to remain commented, got:\n%s", content)
	}
	if strings.Contains(content, "\nghes_host: ") {
		t.Errorf("ghes_host should not be written when unconfigured, got:\n%s", content)
	}
}

// TestRunInit_GHESHost_MismatchedHostRejected covers acceptance [3]: a URL
// whose host matches neither github.com nor the configured GHES host is
// rejected, and the error names both accepted hosts rather than only
// github.com — a GHES operator who typos the host should not be told the
// answer is "github.com".
func TestRunInit_GHESHost_MismatchedHostRejected(t *testing.T) {
	dir := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(orig) //nolint

	t.Setenv("FABRIK_GHES_HOST", "github.example.com")

	err = runInit([]string{"https://typo-host.example.com/orgs/acme/projects/7"})
	if err == nil {
		t.Fatal("expected error for mismatched host, got nil")
	}
	if !strings.Contains(err.Error(), "github.com") || !strings.Contains(err.Error(), "github.example.com") {
		t.Errorf("expected error to name both accepted hosts, got: %v", err)
	}

	// No config.yaml should have been written — the URL parse failure must
	// happen before any filesystem writes.
	if _, statErr := os.Stat(filepath.Join(dir, ".fabrik", "config.yaml")); statErr == nil {
		t.Error("config.yaml should not be written when URL parsing fails")
	}
}

func TestRunInit_IdempotentDestDir(t *testing.T) {
	dir := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(orig) //nolint

	// Running init twice should not error even if .fabrik/stages already exists.
	if err := runInit([]string{}); err != nil {
		t.Fatalf("first runInit: %v", err)
	}
	if err := runInit([]string{}); err != nil {
		t.Fatalf("second runInit: %v", err)
	}
}

// TestRunInit_DriftClean verifies a freshly-init'd .fabrik/stages/ still
// carries the advisory knobs (e.g. kill_grace) and produces zero drift
// warnings against the embedded defaults it was seeded from.
func TestRunInit_DriftClean(t *testing.T) {
	dir := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(orig) //nolint

	if err := runInit([]string{}); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	implementYAML, err := os.ReadFile(filepath.Join(dir, ".fabrik", "stages", "implement.yaml"))
	if err != nil {
		t.Fatalf("reading implement.yaml: %v", err)
	}
	if !strings.Contains(string(implementYAML), "kill_grace:") {
		t.Errorf("expected kill_grace: in freshly-init'd implement.yaml, got:\n%s", string(implementYAML))
	}

	userStages, err := stages.LoadAll(filepath.Join(dir, ".fabrik", "stages"))
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}

	var out strings.Builder
	stages.WarnStageDrift(userStages, "v0.0.99", &out)
	if got := out.String(); got != "" {
		t.Errorf("expected zero drift warnings for freshly-init'd stages, got: %q", got)
	}
}

// TestWriteGitExclude_WriteFailurePropagates verifies that a failure writing
// .git/info/exclude is surfaced as an error rather than silently discarded.
// Pre-fix, writeGitExclude ignored the os.WriteFile error entirely.
func TestWriteGitExclude_WriteFailurePropagates(t *testing.T) {
	dir := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(orig) //nolint

	if err := os.Mkdir(".git", 0755); err != nil {
		t.Fatal(err)
	}
	// Make .git/info a regular file (not a directory), so os.Stat(".git/info")
	// succeeds (the "not a git repo" early-return doesn't fire) but
	// os.WriteFile(".git/info/exclude", ...) fails with ENOTDIR.
	if err := os.WriteFile(".git/info", []byte("not a directory"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := writeGitExclude(); err == nil {
		t.Fatal("expected writeGitExclude to return an error when the write fails")
	}
}

// TestRunInit_HaltsOnGitExcludeFailure verifies runInit propagates a
// writeGitExclude failure instead of reporting success.
func TestRunInit_HaltsOnGitExcludeFailure(t *testing.T) {
	dir := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(orig) //nolint

	if err := os.Mkdir(".git", 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(".git/info", []byte("not a directory"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := runInit([]string{}); err == nil {
		t.Fatal("expected runInit to fail when writeGitExclude fails")
	}
}

// createBoardTestServer returns an httptest server that fakes just enough of
// the GraphQL surface createBoardCore drives, routing on a substring of the
// query text (each call site's query shape is distinct enough to
// disambiguate). ownerTypename is "Organization" or "User", letting tests
// exercise both R7 branches. createCalled, if non-nil, records whether
// createProjectV2 fired — used to prove a local (no-network) failure never
// creates a board (the orphan-resource review finding on PR #1718).
func createBoardTestServer(t *testing.T, ownerTypename string, statusOptions []map[string]interface{}, createCalled *bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Query string `json:"query"`
		}
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &body)

		var resp map[string]interface{}
		switch {
		case strings.Contains(body.Query, "repositoryOwner(login:"):
			resp = map[string]interface{}{
				"data": map[string]interface{}{
					"repositoryOwner": map[string]interface{}{
						"__typename": ownerTypename,
						"id":         "O_OWNER1",
					},
				},
			}
		case strings.Contains(body.Query, "repository(owner: $owner, name: $name)"):
			resp = map[string]interface{}{
				"data": map[string]interface{}{
					"repository": map[string]interface{}{"id": "R_REPO1"},
				},
			}
		case strings.Contains(body.Query, "createProjectV2(input:"):
			if createCalled != nil {
				*createCalled = true
			}
			resp = map[string]interface{}{
				"data": map[string]interface{}{
					"createProjectV2": map[string]interface{}{
						"projectV2": map[string]interface{}{"id": "PVT_NEW1", "number": 42},
					},
				},
			}
		case strings.Contains(body.Query, "shortDescription: $shortDescription"):
			resp = map[string]interface{}{
				"data": map[string]interface{}{
					"updateProjectV2": map[string]interface{}{
						"projectV2": map[string]interface{}{"id": "PVT_NEW1"},
					},
				},
			}
		case strings.Contains(body.Query, "field(name: \"Status\")"):
			resp = map[string]interface{}{
				"data": map[string]interface{}{
					"node": map[string]interface{}{
						"field": map[string]interface{}{
							"id":      "FIELD_STATUS",
							"options": statusOptions,
						},
					},
				},
			}
		case strings.Contains(body.Query, "updateProjectV2Field(input:"):
			resp = map[string]interface{}{
				"data": map[string]interface{}{
					"updateProjectV2Field": map[string]interface{}{
						"projectV2Field": map[string]interface{}{"id": "FIELD_STATUS"},
					},
				},
			}
		default:
			t.Fatalf("unexpected GraphQL query: %s", body.Query)
		}
		json.NewEncoder(w).Encode(resp)
	}))
}

func writeMinimalStage(t *testing.T, dir, name string, order int) {
	t.Helper()
	content := fmt.Sprintf("name: %s\norder: %d\nprompt: do the thing\n", name, order)
	if err := os.WriteFile(filepath.Join(dir, strings.ToLower(name)+".yaml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestCreateBoardCore_Success(t *testing.T) {
	srv := createBoardTestServer(t, "Organization", []map[string]interface{}{
		{"id": "OPT_1", "name": "Todo", "color": "GRAY", "description": ""},
		{"id": "OPT_2", "name": "In Progress", "color": "BLUE", "description": ""},
		{"id": "OPT_3", "name": "Done", "color": "GREEN", "description": ""},
	}, nil)
	defer srv.Close()

	stagesDir := t.TempDir()
	writeMinimalStage(t, stagesDir, "Specify", 0)
	writeMinimalStage(t, stagesDir, "Implement", 1)

	client := gh.NewClientWithBaseURL("token", srv.URL)
	project, ownerType, err := createBoardCore(client, "acme", "widgets", "", stagesDir)
	if err != nil {
		t.Fatalf("createBoardCore: %v", err)
	}
	if project != "42" {
		t.Errorf("project = %q, want 42", project)
	}
	if ownerType != "organization" {
		t.Errorf("ownerType = %q, want organization", ownerType)
	}
}

func TestCreateBoardCore_RefusesUserOwned(t *testing.T) {
	srv := createBoardTestServer(t, "User", nil, nil)
	defer srv.Close()

	stagesDir := t.TempDir()
	writeMinimalStage(t, stagesDir, "Specify", 0)

	client := gh.NewClientWithBaseURL("token", srv.URL)
	if _, _, err := createBoardCore(client, "someuser", "widgets", "", stagesDir); err == nil {
		t.Fatal("expected refusal for user-owned target")
	} else if !strings.Contains(err.Error(), "organization-only") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestCreateBoardCore_NoStageConfigs is the orphan-resource review finding
// (PR #1718): a missing/empty stage configs directory is a purely local,
// no-network condition, and must be caught before CreateProjectV2 ever
// fires — otherwise a real project gets created on GitHub with nothing
// pointing back at it (runInit never reaches writeConfigTemplate on error)
// and no guard against a retry creating a second, separate project.
func TestCreateBoardCore_NoStageConfigs(t *testing.T) {
	var created bool
	srv := createBoardTestServer(t, "Organization", []map[string]interface{}{
		{"id": "OPT_1", "name": "Todo", "color": "GRAY", "description": ""},
	}, &created)
	defer srv.Close()

	stagesDir := t.TempDir()
	// No stage YAML files written — requiredStageColumnNames returns empty.

	client := gh.NewClientWithBaseURL("token", srv.URL)
	if _, _, err := createBoardCore(client, "acme", "widgets", "", stagesDir); err == nil {
		t.Fatal("expected error when no stage configs are present")
	}
	if created {
		t.Error("createProjectV2 must not be called when there are no stage configs — this would orphan a real board")
	}
}

// TestCreateBoardCore_PostCreateFailureNamesTheOrphanedBoard covers the
// remaining orphan-resource case that can't be avoided by reordering (a
// network failure genuinely occurring after the project already exists):
// the returned error must name the board's number/URL and steer the
// operator at the existing `fabrik init <project-url>` link-to-existing-
// board flow, rather than reporting a bare underlying error with no trace
// of the board GitHub actually created (review finding, PR #1718).
func TestCreateBoardCore_PostCreateFailureNamesTheOrphanedBoard(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Query string `json:"query"`
		}
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &body)

		var resp map[string]interface{}
		switch {
		case strings.Contains(body.Query, "repositoryOwner(login:"):
			resp = map[string]interface{}{
				"data": map[string]interface{}{
					"repositoryOwner": map[string]interface{}{"__typename": "Organization", "id": "O_OWNER1"},
				},
			}
		case strings.Contains(body.Query, "repository(owner: $owner, name: $name)"):
			resp = map[string]interface{}{
				"data": map[string]interface{}{"repository": map[string]interface{}{"id": "R_REPO1"}},
			}
		case strings.Contains(body.Query, "createProjectV2(input:"):
			resp = map[string]interface{}{
				"data": map[string]interface{}{
					"createProjectV2": map[string]interface{}{
						"projectV2": map[string]interface{}{"id": "PVT_NEW1", "number": 42},
					},
				},
			}
		case strings.Contains(body.Query, "shortDescription: $shortDescription"):
			resp = map[string]interface{}{
				"data": map[string]interface{}{"updateProjectV2": map[string]interface{}{"projectV2": map[string]interface{}{"id": "PVT_NEW1"}}},
			}
		case strings.Contains(body.Query, "field(name: \"Status\")"):
			// Simulate a transient failure fetching the Status field of the
			// board that was already created above.
			w.WriteHeader(500)
			w.Write([]byte("server error"))
			return
		default:
			t.Fatalf("unexpected GraphQL query: %s", body.Query)
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	stagesDir := t.TempDir()
	writeMinimalStage(t, stagesDir, "Specify", 0)

	client := gh.NewClientWithBaseURL("token", srv.URL)
	_, _, err := createBoardCore(client, "acme", "widgets", "", stagesDir)
	if err == nil {
		t.Fatal("expected error from FetchStatusField failure")
	}
	for _, want := range []string{"#42", "https://github.com/orgs/acme/projects/42", "fabrik init", "do not re-run --create-board"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name the orphaned board and steer to the link-existing-board flow (missing %q): %v", want, err)
		}
	}
}
