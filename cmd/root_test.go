package cmd

import (
	"flag"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// chdirTest changes to dir for the duration of the test, restoring original on cleanup.
func chdirTest(t *testing.T, dir string) {
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

func resetFlags() {
	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
}

func writeStageFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestExecute_MissingRequiredFlags(t *testing.T) {
	resetFlags()
	os.Args = []string{"fabrik"}

	err := Execute()
	if err == nil {
		t.Fatal("expected error for missing required flags")
	}
}

func TestExecute_MissingToken(t *testing.T) {
	resetFlags()
	os.Args = []string{"fabrik", "--owner", "o", "--repo", "r", "--project", "1", "--user", "u"}
	// Clear GITHUB_TOKEN env var
	prev := os.Getenv("GITHUB_TOKEN")
	os.Unsetenv("GITHUB_TOKEN")
	defer os.Setenv("GITHUB_TOKEN", prev)

	err := Execute()
	if err == nil {
		t.Fatal("expected error for missing token")
	}
}

func TestExecute_MissingUser(t *testing.T) {
	resetFlags()
	os.Args = []string{"fabrik", "--owner", "o", "--repo", "r", "--project", "1", "--token", "tok"}

	err := Execute()
	if err == nil {
		t.Fatal("expected error for missing user")
	}
}

func TestExecute_TokenFromEnv(t *testing.T) {
	resetFlags()
	stagesDir := t.TempDir()
	// No stage files — should fail with "no stage configurations"
	os.Args = []string{"fabrik", "--owner", "o", "--repo", "r", "--project", "1", "--user", "u", "--stages", stagesDir}
	os.Setenv("GITHUB_TOKEN", "env-token")
	defer os.Unsetenv("GITHUB_TOKEN")

	err := Execute()
	if err == nil {
		t.Fatal("expected error (no stages)")
	}
	// The error should be about stages, not about token
	if err.Error() == "GitHub token required: use --token or set GITHUB_TOKEN env var" {
		t.Error("token from env should have been accepted")
	}
}

func TestExecute_NoStages(t *testing.T) {
	resetFlags()
	stagesDir := t.TempDir()
	os.Args = []string{"fabrik", "--owner", "o", "--repo", "r", "--project", "1", "--user", "u", "--token", "tok", "--stages", stagesDir}

	err := Execute()
	if err == nil {
		t.Fatal("expected error for empty stages dir")
	}
}

func TestExecute_BadStagesDir(t *testing.T) {
	resetFlags()
	os.Args = []string{"fabrik", "--owner", "o", "--repo", "r", "--project", "1", "--user", "u", "--token", "tok", "--stages", "/nonexistent/stages"}

	err := Execute()
	if err == nil {
		t.Fatal("expected error for nonexistent stages dir")
	}
}

func TestExecute_MissingOwner(t *testing.T) {
	resetFlags()
	os.Args = []string{"fabrik", "--repo", "r", "--project", "1"}

	err := Execute()
	if err == nil {
		t.Fatal("expected error for missing owner")
	}
}

func TestExecute_MissingRepo(t *testing.T) {
	resetFlags()
	os.Args = []string{"fabrik", "--owner", "o", "--project", "1"}

	err := Execute()
	if err == nil {
		t.Fatal("expected error for missing repo")
	}
}

func TestExecute_MissingProject(t *testing.T) {
	resetFlags()
	os.Args = []string{"fabrik", "--owner", "o", "--repo", "r", "--user", "u", "--token", "tok"}

	err := Execute()
	if err == nil {
		t.Fatal("expected error for missing project number")
	}
}

func TestExecute_MaxConcurrentFlag(t *testing.T) {
	resetFlags()
	stagesDir := t.TempDir()
	// No stage files — expect stages error, not a flag error
	os.Args = []string{"fabrik", "--owner", "o", "--repo", "r", "--project", "1", "--user", "u", "--token", "tok", "--stages", stagesDir, "--max-concurrent", "10"}

	err := Execute()
	if err == nil {
		t.Fatal("expected error (no stages)")
	}
	// Should fail on stages, not on the --max-concurrent flag itself
	if err.Error() == "unknown flag: --max-concurrent" {
		t.Error("--max-concurrent flag not registered")
	}
}

func TestExecute_ConfigYAMLApplied(t *testing.T) {
	// Test that config.yaml values satisfy required fields (owner/repo/project/user).
	// Execute() will fail at engine.New() (not inside a git repo) rather than at
	// the "missing required config" validation — that's the evidence config.yaml worked.
	dir := t.TempDir()
	chdirTest(t, dir)
	os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(".env\n"), 0644)
	stagesDir := filepath.Join(dir, "stages")
	os.MkdirAll(stagesDir, 0755)
	writeStageFile(t, stagesDir, "research.yaml", "name: Research\norder: 1\nprompt: test\n")
	configYAML := "owner: cfgorg\nrepo: cfgrepo\nproject: 7\nuser: cfguser\nstages: " + stagesDir + "\npoll: 15\n"
	os.MkdirAll(filepath.Join(dir, ".fabrik"), 0755)
	os.WriteFile(filepath.Join(dir, ".fabrik", "config.yaml"), []byte(configYAML), 0644)

	resetFlags()
	t.Setenv("GITHUB_TOKEN", "tok")
	for _, k := range []string{"FABRIK_OWNER", "FABRIK_REPO", "FABRIK_PROJECT_NUMBER", "FABRIK_USER", "FABRIK_STAGES"} {
		t.Setenv(k, "")
	}
	os.Args = []string{"fabrik"}

	err := Execute()
	// Should fail at engine.New() (not a git repo) — NOT at "missing required config"
	if err == nil {
		t.Fatal("expected error (not a git repo)")
	}
	const missingConfig = "missing required config: owner, repo, project"
	if err.Error() == missingConfig {
		t.Errorf("config.yaml values were not applied: got %q", err.Error())
	}
}

func TestExecute_EnvVarBeatsConfigYAML(t *testing.T) {
	dir := t.TempDir()
	chdirTest(t, dir)
	os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(".env\n"), 0644)
	stagesDir := filepath.Join(dir, "stages")
	os.MkdirAll(stagesDir, 0755)
	// config.yaml sets owner to cfgorg, but env var should win
	os.MkdirAll(filepath.Join(dir, ".fabrik"), 0755)
	os.WriteFile(filepath.Join(dir, ".fabrik", "config.yaml"), []byte("owner: cfgorg\n"), 0644)

	resetFlags()
	t.Setenv("FABRIK_OWNER", "envorg")
	defer t.Cleanup(func() { os.Unsetenv("FABRIK_OWNER") })
	os.Args = []string{"fabrik", "--repo", "r", "--project", "1", "--user", "u", "--token", "tok", "--stages", stagesDir}

	err := Execute()
	// Will fail on no stages — but NOT on missing owner (env var won over config.yaml)
	if err != nil && err.Error() == "missing required config: owner, repo, project (use flags or .env file)" {
		t.Error("env var FABRIK_OWNER should have satisfied owner requirement (beats config.yaml)")
	}
}

func TestExecute_ValidStagesReachesEngine(t *testing.T) {
	resetFlags()
	stagesDir := t.TempDir()
	writeStageFile(t, stagesDir, "research.yaml", `
name: Research
order: 1
prompt: "Do research"
`)
	os.Args = []string{
		"fabrik",
		"--owner", "o",
		"--repo", "r",
		"--project", "1",
		"--user", "u",
		"--token", "tok",
		"--stages", stagesDir,
		"--poll", "1",
		"--yolo",
	}

	// Use testReadyCh so we only send SIGINT after engine.Run has registered
	// its signal handlers, avoiding a race that can terminate the test process.
	readyCh := make(chan struct{})
	testReadyCh = readyCh
	defer func() { testReadyCh = nil }()

	done := make(chan error, 1)
	go func() {
		done <- Execute()
	}()

	<-readyCh
	p, _ := os.FindProcess(os.Getpid())
	p.Signal(syscall.SIGINT)

	select {
	case <-done:
		// Success — Execute returned
	case <-time.After(5 * time.Second):
		t.Fatal("Execute did not return after SIGINT")
	}
}
