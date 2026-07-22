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
	t.Setenv("FABRIK_PLUGIN_DIR", t.TempDir())
	t.Setenv("GITHUB_TOKEN", "tok")
	os.Args = []string{"fabrik", "--owner", "o", "--repo", "r", "--project", "1", "--user", "u", "--stages", stagesDir}
	executeAndStop(t)
}

func TestExecute_EnvTUIFalse(t *testing.T) {
	dir, stagesDir := setupValidStages(t)
	chdirTest(t, dir)
	resetFlags()
	t.Setenv("FABRIK_TUI", "false")
	t.Setenv("GITHUB_TOKEN", "tok")
	os.Args = []string{"fabrik", "--owner", "o", "--repo", "r", "--project", "1", "--user", "u", "--stages", stagesDir}
	executeAndStop(t)
}

func TestExecute_EnvSymlinkEnv(t *testing.T) {
	dir, stagesDir := setupValidStages(t)
	chdirTest(t, dir)
	resetFlags()
	t.Setenv("FABRIK_SYMLINK_ENV", "true")
	t.Setenv("GITHUB_TOKEN", "tok")
	os.Args = []string{"fabrik", "--owner", "o", "--repo", "r", "--project", "1", "--user", "u", "--stages", stagesDir}
	executeAndStop(t)
}

func TestExecute_FlagSymlinkEnv(t *testing.T) {
	dir, stagesDir := setupValidStages(t)
	chdirTest(t, dir)
	resetFlags()
	t.Setenv("GITHUB_TOKEN", "tok")
	os.Args = []string{"fabrik", "--owner", "o", "--repo", "r", "--project", "1", "--user", "u", "--stages", stagesDir, "--symlink-env"}
	executeAndStop(t)
}

func TestExecute_EnvWorktreeBoundaryAudit(t *testing.T) {
	dir, stagesDir := setupValidStages(t)
	chdirTest(t, dir)
	resetFlags()
	t.Setenv("FABRIK_WORKTREE_BOUNDARY_AUDIT", "true")
	t.Setenv("GITHUB_TOKEN", "tok")
	os.Args = []string{"fabrik", "--owner", "o", "--repo", "r", "--project", "1", "--user", "u", "--stages", stagesDir}
	executeAndStop(t)
}

func TestExecute_FlagWorktreeBoundaryAudit(t *testing.T) {
	dir, stagesDir := setupValidStages(t)
	chdirTest(t, dir)
	resetFlags()
	t.Setenv("GITHUB_TOKEN", "tok")
	os.Args = []string{"fabrik", "--owner", "o", "--repo", "r", "--project", "1", "--user", "u", "--stages", stagesDir, "--worktree-boundary-audit"}
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

// executeWithConfigHook runs Execute() with testResolvedConfigHook installed,
// capturing the fully-resolved Config right before engine.New would be
// called. The hook makes Execute return immediately once resolution
// completes, so no engine startup or SIGINT dance is needed.
func executeWithConfigHook(t *testing.T) Config {
	t.Helper()
	var captured Config
	var called bool
	testResolvedConfigHook = func(cfg Config) {
		captured = cfg
		called = true
	}
	defer func() { testResolvedConfigHook = nil }()

	if err := Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !called {
		t.Fatal("testResolvedConfigHook was not invoked")
	}
	return captured
}

func TestExecute_MergeTrainConfigOnly(t *testing.T) {
	dir, stagesDir := setupValidStages(t)
	chdirTest(t, dir)
	os.MkdirAll(filepath.Join(dir, ".fabrik"), 0755)
	os.WriteFile(filepath.Join(dir, ".fabrik", "config.yaml"), []byte("merge_train: on\n"), 0644)
	resetFlags()
	t.Setenv("GITHUB_TOKEN", "tok")
	os.Args = []string{"fabrik", "--owner", "o", "--repo", "r", "--project", "1", "--user", "u", "--stages", stagesDir}

	cfg := executeWithConfigHook(t)
	if got := mergeTrainMode(cfg.MergeTrain); got != "on" {
		t.Errorf("resolved merge train mode = %q, want on (config.yaml should enable it)", got)
	}
}

func TestExecute_MergeTrainFlagBeatsConfig(t *testing.T) {
	dir, stagesDir := setupValidStages(t)
	chdirTest(t, dir)
	os.MkdirAll(filepath.Join(dir, ".fabrik"), 0755)
	os.WriteFile(filepath.Join(dir, ".fabrik", "config.yaml"), []byte("merge_train: on\n"), 0644)
	resetFlags()
	t.Setenv("GITHUB_TOKEN", "tok")
	os.Args = []string{"fabrik", "--owner", "o", "--repo", "r", "--project", "1", "--user", "u", "--stages", stagesDir, "--merge-train", "off"}

	cfg := executeWithConfigHook(t)
	if got := mergeTrainMode(cfg.MergeTrain); got != "off" {
		t.Errorf("resolved merge train mode = %q, want off (explicit flag should beat config.yaml)", got)
	}
}

func TestExecute_MergeTrainEnvBeatsConfig(t *testing.T) {
	dir, stagesDir := setupValidStages(t)
	chdirTest(t, dir)
	os.MkdirAll(filepath.Join(dir, ".fabrik"), 0755)
	os.WriteFile(filepath.Join(dir, ".fabrik", "config.yaml"), []byte("merge_train: on\n"), 0644)
	resetFlags()
	t.Setenv("GITHUB_TOKEN", "tok")
	t.Setenv("FABRIK_MERGE_TRAIN", "off")
	os.Args = []string{"fabrik", "--owner", "o", "--repo", "r", "--project", "1", "--user", "u", "--stages", stagesDir}

	cfg := executeWithConfigHook(t)
	if got := mergeTrainMode(cfg.MergeTrain); got != "off" {
		t.Errorf("resolved merge train mode = %q, want off (env var should beat config.yaml)", got)
	}
}

func TestExecute_MergeTrainInvalidConfigValue(t *testing.T) {
	dir, stagesDir := setupValidStages(t)
	chdirTest(t, dir)
	os.MkdirAll(filepath.Join(dir, ".fabrik"), 0755)
	os.WriteFile(filepath.Join(dir, ".fabrik", "config.yaml"), []byte("merge_train: maybe\n"), 0644)
	resetFlags()
	t.Setenv("GITHUB_TOKEN", "tok")
	os.Args = []string{"fabrik", "--owner", "o", "--repo", "r", "--project", "1", "--user", "u", "--stages", stagesDir}

	cfg := executeWithConfigHook(t)
	if cfg.MergeTrain != "maybe" {
		t.Errorf("cfg.MergeTrain = %q, want raw config.yaml value %q to flow through unvalidated", cfg.MergeTrain, "maybe")
	}
	if got := mergeTrainMode(cfg.MergeTrain); got != "off" {
		t.Errorf("mergeTrainMode(%q) = %q, want off (invalid value falls back to default)", cfg.MergeTrain, got)
	}
}

func TestMergeTrainMode(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "off"},
		{"on", "on"},
		{"ON", "on"},
		{"off", "off"},
		{"maybe", "off"},
	}
	for _, c := range cases {
		if got := mergeTrainMode(c.in); got != c.want {
			t.Errorf("mergeTrainMode(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestArchiveAfter(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"", 168 * time.Hour},
		{"24h", 24 * time.Hour},
		{"12h", 12 * time.Hour},
		{"0", 0},                 // bare "0" is accepted by time.ParseDuration; legal: archive immediately
		{"0s", 0},                // legal: archive immediately once eligible
		{"-1h", 168 * time.Hour}, // negative -> falls back to default
		{"not-a-duration", 168 * time.Hour},
	}
	for _, c := range cases {
		if got := archiveAfter(c.in); got != c.want {
			t.Errorf("archiveAfter(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestArchiveDoneMode(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "on"},
		{"on", "on"},
		{"ON", "on"},
		{"off", "off"},
		{"OFF", "off"},
		{"maybe", "on"},
	}
	for _, c := range cases {
		if got := archiveDoneMode(c.in); got != c.want {
			t.Errorf("archiveDoneMode(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestExecute_ArchiveAfterEnvBeatsDefault(t *testing.T) {
	dir, stagesDir := setupValidStages(t)
	chdirTest(t, dir)
	resetFlags()
	t.Setenv("GITHUB_TOKEN", "tok")
	t.Setenv("FABRIK_ARCHIVE_AFTER", "12h")
	os.Args = []string{"fabrik", "--owner", "o", "--repo", "r", "--project", "1", "--user", "u", "--stages", stagesDir}

	cfg := executeWithConfigHook(t)
	if got := archiveAfter(cfg.ArchiveAfter); got != 12*time.Hour {
		t.Errorf("resolved archive-after = %v, want 12h from FABRIK_ARCHIVE_AFTER", got)
	}
}

func TestExecute_ArchiveAfterFlagBeatsEnv(t *testing.T) {
	dir, stagesDir := setupValidStages(t)
	chdirTest(t, dir)
	resetFlags()
	t.Setenv("GITHUB_TOKEN", "tok")
	t.Setenv("FABRIK_ARCHIVE_AFTER", "12h")
	os.Args = []string{"fabrik", "--owner", "o", "--repo", "r", "--project", "1", "--user", "u", "--stages", stagesDir, "--archive-after", "1h"}

	cfg := executeWithConfigHook(t)
	if got := archiveAfter(cfg.ArchiveAfter); got != time.Hour {
		t.Errorf("resolved archive-after = %v, want 1h (explicit flag should beat env)", got)
	}
}

func TestExecute_ArchiveDoneEnvBeatsDefault(t *testing.T) {
	dir, stagesDir := setupValidStages(t)
	chdirTest(t, dir)
	resetFlags()
	t.Setenv("GITHUB_TOKEN", "tok")
	t.Setenv("FABRIK_ARCHIVE_DONE", "off")
	os.Args = []string{"fabrik", "--owner", "o", "--repo", "r", "--project", "1", "--user", "u", "--stages", stagesDir}

	cfg := executeWithConfigHook(t)
	if got := archiveDoneMode(cfg.ArchiveDone); got != "off" {
		t.Errorf("resolved archive-done = %q, want off from FABRIK_ARCHIVE_DONE", got)
	}
}

func TestExecute_MaxBatchSizeConfigOnly(t *testing.T) {
	dir, stagesDir := setupValidStages(t)
	chdirTest(t, dir)
	os.MkdirAll(filepath.Join(dir, ".fabrik"), 0755)
	os.WriteFile(filepath.Join(dir, ".fabrik", "config.yaml"), []byte("max_batch_size: 8\n"), 0644)
	resetFlags()
	t.Setenv("GITHUB_TOKEN", "tok")
	os.Args = []string{"fabrik", "--owner", "o", "--repo", "r", "--project", "1", "--user", "u", "--stages", stagesDir}

	cfg := executeWithConfigHook(t)
	if cfg.MaxBatchSize != 8 {
		t.Errorf("cfg.MaxBatchSize = %d, want 8 from config.yaml", cfg.MaxBatchSize)
	}
}

func TestExecute_MaxBatchSizeEnvBeatsConfig(t *testing.T) {
	dir, stagesDir := setupValidStages(t)
	chdirTest(t, dir)
	os.MkdirAll(filepath.Join(dir, ".fabrik"), 0755)
	os.WriteFile(filepath.Join(dir, ".fabrik", "config.yaml"), []byte("max_batch_size: 8\n"), 0644)
	resetFlags()
	t.Setenv("GITHUB_TOKEN", "tok")
	t.Setenv("FABRIK_MAX_BATCH_SIZE", "12")
	os.Args = []string{"fabrik", "--owner", "o", "--repo", "r", "--project", "1", "--user", "u", "--stages", stagesDir}

	cfg := executeWithConfigHook(t)
	if cfg.MaxBatchSize != 12 {
		t.Errorf("cfg.MaxBatchSize = %d, want 12 (env var should beat config.yaml)", cfg.MaxBatchSize)
	}
}

func TestExecute_MaxBatchSizeFlagBeatsConfig(t *testing.T) {
	dir, stagesDir := setupValidStages(t)
	chdirTest(t, dir)
	os.MkdirAll(filepath.Join(dir, ".fabrik"), 0755)
	os.WriteFile(filepath.Join(dir, ".fabrik", "config.yaml"), []byte("max_batch_size: 8\n"), 0644)
	resetFlags()
	t.Setenv("GITHUB_TOKEN", "tok")
	os.Args = []string{"fabrik", "--owner", "o", "--repo", "r", "--project", "1", "--user", "u", "--stages", stagesDir, "--max-batch-size", "3"}

	cfg := executeWithConfigHook(t)
	if cfg.MaxBatchSize != 3 {
		t.Errorf("cfg.MaxBatchSize = %d, want 3 (explicit flag should beat config.yaml)", cfg.MaxBatchSize)
	}
}

func TestExecute_MaxBatchSizeInvalidConfigValue(t *testing.T) {
	dir, stagesDir := setupValidStages(t)
	chdirTest(t, dir)
	os.MkdirAll(filepath.Join(dir, ".fabrik"), 0755)
	os.WriteFile(filepath.Join(dir, ".fabrik", "config.yaml"), []byte("max_batch_size: 0\n"), 0644)
	resetFlags()
	t.Setenv("GITHUB_TOKEN", "tok")
	os.Args = []string{"fabrik", "--owner", "o", "--repo", "r", "--project", "1", "--user", "u", "--stages", stagesDir}

	cfg := executeWithConfigHook(t)
	if cfg.MaxBatchSize != 0 {
		t.Errorf("cfg.MaxBatchSize = %d, want 0 (invalid config.yaml value falls back to default)", cfg.MaxBatchSize)
	}
}

func TestExecute_TrainTrialWindowConfigOnly(t *testing.T) {
	dir, stagesDir := setupValidStages(t)
	chdirTest(t, dir)
	os.MkdirAll(filepath.Join(dir, ".fabrik"), 0755)
	os.WriteFile(filepath.Join(dir, ".fabrik", "config.yaml"), []byte("train_trial_window: 45\n"), 0644)
	resetFlags()
	t.Setenv("GITHUB_TOKEN", "tok")
	os.Args = []string{"fabrik", "--owner", "o", "--repo", "r", "--project", "1", "--user", "u", "--stages", stagesDir}

	cfg := executeWithConfigHook(t)
	if cfg.TrainTrialWindowMinutes != 45 {
		t.Errorf("cfg.TrainTrialWindowMinutes = %d, want 45 from config.yaml", cfg.TrainTrialWindowMinutes)
	}
}

func TestExecute_MaxBisectValidationsConfigZeroMeansDerive(t *testing.T) {
	dir, stagesDir := setupValidStages(t)
	chdirTest(t, dir)
	os.MkdirAll(filepath.Join(dir, ".fabrik"), 0755)
	os.WriteFile(filepath.Join(dir, ".fabrik", "config.yaml"), []byte("max_bisect_validations: 0\n"), 0644)
	resetFlags()
	t.Setenv("GITHUB_TOKEN", "tok")
	os.Args = []string{"fabrik", "--owner", "o", "--repo", "r", "--project", "1", "--user", "u", "--stages", stagesDir}

	cfg := executeWithConfigHook(t)
	if cfg.MaxBisectValidations != 0 {
		t.Errorf("cfg.MaxBisectValidations = %d, want 0 (config.yaml 0 means derive default, not invalid)", cfg.MaxBisectValidations)
	}
}

func TestExecute_MaxBisectValidationsConfigNegativeIsInvalid(t *testing.T) {
	dir, stagesDir := setupValidStages(t)
	chdirTest(t, dir)
	os.MkdirAll(filepath.Join(dir, ".fabrik"), 0755)
	os.WriteFile(filepath.Join(dir, ".fabrik", "config.yaml"), []byte("max_bisect_validations: -1\n"), 0644)
	resetFlags()
	t.Setenv("GITHUB_TOKEN", "tok")
	os.Args = []string{"fabrik", "--owner", "o", "--repo", "r", "--project", "1", "--user", "u", "--stages", stagesDir}

	cfg := executeWithConfigHook(t)
	if cfg.MaxBisectValidations != 0 {
		t.Errorf("cfg.MaxBisectValidations = %d, want 0 (negative config.yaml value falls back to default)", cfg.MaxBisectValidations)
	}
}
