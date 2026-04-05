package cmd

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// setupValidStages creates a temp dir with a minimal stages setup so Execute
// can proceed past stage loading but still reach the engine start.
func setupValidStages(t *testing.T) (dir, stagesDir string) {
	t.Helper()
	dir = t.TempDir()
	stagesDir = filepath.Join(dir, "stages")
	if err := os.MkdirAll(stagesDir, 0755); err != nil {
		t.Fatal(err)
	}
	writeStageFile(t, stagesDir, "research.yaml", "name: Research\norder: 1\nprompt: test\n")
	// .gitignore needed for config.LoadDotenv
	os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(".env\n"), 0644)
	return dir, stagesDir
}

// executeAndStop runs Execute() in a goroutine, waits for the engine to start via
// testReadyCh, sends SIGINT to stop it, then waits for completion.
// This is necessary because Execute() starts the engine poll loop which runs
// indefinitely until interrupted.
func executeAndStop(t *testing.T) error {
	t.Helper()
	readyCh := make(chan struct{})
	testReadyCh = readyCh
	defer func() { testReadyCh = nil }()

	done := make(chan error, 1)
	go func() { done <- Execute() }()

	select {
	case err := <-done:
		// Execute returned early (e.g., config error or engine.New failure) — no SIGINT needed.
		return err
	case <-readyCh:
		// Engine started — send SIGINT to stop it.
	case <-time.After(10 * time.Second):
		t.Fatal("Execute did not start within 10 seconds")
	}

	p, _ := os.FindProcess(os.Getpid())
	p.Signal(syscall.SIGINT)

	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("Execute did not stop after SIGINT")
		return nil
	}
}

func TestExecute_EnvPoll_ValidValue(t *testing.T) {
	dir, stagesDir := setupValidStages(t)
	chdirTest(t, dir)
	resetFlags()
	t.Setenv("FABRIK_POLL", "60")
	t.Setenv("GITHUB_TOKEN", "tok")
	os.Args = []string{"fabrik", "--owner", "o", "--repo", "r", "--project", "1", "--user", "u", "--stages", stagesDir}
	executeAndStop(t)
}

func TestExecute_EnvPoll_InvalidValue(t *testing.T) {
	dir, stagesDir := setupValidStages(t)
	chdirTest(t, dir)
	resetFlags()
	t.Setenv("FABRIK_POLL", "bad")
	t.Setenv("GITHUB_TOKEN", "tok")
	os.Args = []string{"fabrik", "--owner", "o", "--repo", "r", "--project", "1", "--user", "u", "--stages", stagesDir}
	// Invalid value uses default — engine still starts
	executeAndStop(t)
}

func TestExecute_EnvMaxConcurrent_Valid(t *testing.T) {
	dir, stagesDir := setupValidStages(t)
	chdirTest(t, dir)
	resetFlags()
	t.Setenv("FABRIK_MAX_CONCURRENT", "10")
	t.Setenv("GITHUB_TOKEN", "tok")
	os.Args = []string{"fabrik", "--owner", "o", "--repo", "r", "--project", "1", "--user", "u", "--stages", stagesDir}
	executeAndStop(t)
}

func TestExecute_EnvMaxConcurrent_Invalid(t *testing.T) {
	dir, stagesDir := setupValidStages(t)
	chdirTest(t, dir)
	resetFlags()
	t.Setenv("FABRIK_MAX_CONCURRENT", "bad")
	t.Setenv("GITHUB_TOKEN", "tok")
	os.Args = []string{"fabrik", "--owner", "o", "--repo", "r", "--project", "1", "--user", "u", "--stages", stagesDir}
	executeAndStop(t)
}

func TestExecute_EnvYolo(t *testing.T) {
	dir, stagesDir := setupValidStages(t)
	chdirTest(t, dir)
	resetFlags()
	t.Setenv("FABRIK_YOLO", "true")
	t.Setenv("GITHUB_TOKEN", "tok")
	os.Args = []string{"fabrik", "--owner", "o", "--repo", "r", "--project", "1", "--user", "u", "--stages", stagesDir}
	executeAndStop(t)
}

func TestExecute_EnvTerminal(t *testing.T) {
	dir, stagesDir := setupValidStages(t)
	chdirTest(t, dir)
	resetFlags()
	t.Setenv("FABRIK_TERMINAL", "iterm2")
	t.Setenv("GITHUB_TOKEN", "tok")
	os.Args = []string{"fabrik", "--owner", "o", "--repo", "r", "--project", "1", "--user", "u", "--stages", stagesDir}
	executeAndStop(t)
}

func TestExecute_EnvMaxRetries_Valid(t *testing.T) {
	dir, stagesDir := setupValidStages(t)
	chdirTest(t, dir)
	resetFlags()
	t.Setenv("FABRIK_MAX_RETRIES", "5")
	t.Setenv("GITHUB_TOKEN", "tok")
	os.Args = []string{"fabrik", "--owner", "o", "--repo", "r", "--project", "1", "--user", "u", "--stages", stagesDir}
	executeAndStop(t)
}

func TestExecute_EnvPluginDir(t *testing.T) {
	dir, stagesDir := setupValidStages(t)
	chdirTest(t, dir)
	resetFlags()
	t.Setenv("FABRIK_PLUGIN_DIR", "/tmp/plugin")
	t.Setenv("GITHUB_TOKEN", "tok")
	os.Args = []string{"fabrik", "--owner", "o", "--repo", "r", "--project", "1", "--user", "u", "--stages", stagesDir}
	executeAndStop(t)
}

func TestExecute_StreamFilterSubcommand(t *testing.T) {
	resetFlags()
	r, w, _ := os.Pipe()
	origStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = origStdin }()
	w.Close()

	os.Args = []string{"fabrik", "stream-filter"}
	if err := Execute(); err != nil {
		t.Fatalf("Execute stream-filter: %v", err)
	}
}
