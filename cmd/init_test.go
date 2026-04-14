package cmd

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/verveguy/fabrik/stages"
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
		owner, project, ownerType, err := parseProjectURL(tc.rawURL)
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

	err = runInit([]string{"--user", "verveguy", "https://github.com/users/verveguy/projects/5"})
	if err != nil {
		t.Fatalf("runInit: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, ".fabrik", "config.yaml"))
	if err != nil {
		t.Fatalf("config.yaml not written: %v", err)
	}
	content := string(data)

	if !strings.Contains(content, "owner: verveguy") {
		t.Errorf("expected 'owner: verveguy', got:\n%s", content)
	}
	if !strings.Contains(content, "project: 5") {
		t.Errorf("expected 'project: 5', got:\n%s", content)
	}
	if !strings.Contains(content, "owner_type: user") {
		t.Errorf("expected 'owner_type: user', got:\n%s", content)
	}
	if !strings.Contains(content, "user: verveguy") {
		t.Errorf("expected 'user: verveguy', got:\n%s", content)
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
