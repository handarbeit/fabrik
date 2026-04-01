package cmd

import (
	"flag"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

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
