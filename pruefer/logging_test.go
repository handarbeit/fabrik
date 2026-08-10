package pruefer

import (
	"os"
	"path/filepath"
	"testing"
)

// resetLogf ensures the package-level Logf hook is restored to nil after a
// test that assigns it, so global-state leakage can't affect other tests in
// this package (Logf is a package-level var, not test-scoped).
func resetLogf(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { Logf = nil })
}

func TestNewDaemon_AssignsNonNilLogf(t *testing.T) {
	resetLogf(t)
	Logf = nil

	dir := t.TempDir()
	cfg := Config{LogFile: filepath.Join(dir, "pruefer.log")}

	_, closeLog := NewDaemon(cfg, nil, nil, nil, "bot")
	defer closeLog()

	if Logf == nil {
		t.Fatal("NewDaemon did not assign pruefer.Logf; want a non-nil function")
	}
}

func TestNewDaemon_DefaultPathResolvedAgainstCwd(t *testing.T) {
	resetLogf(t)
	Logf = nil

	dir := t.TempDir()
	origWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(origWd) })

	cfg := Config{LogFile: DefaultLogPath}
	_, closeLog := NewDaemon(cfg, nil, nil, nil, "bot")
	defer closeLog()

	Logf(0, "poll", "hello\n")

	wantPath := filepath.Join(dir, DefaultLogPath)
	if _, err := os.Stat(wantPath); err != nil {
		t.Errorf("expected log file at %s (default path resolved against cwd): %v", wantPath, err)
	}
}

func TestNewDaemon_ExplicitPathOverride(t *testing.T) {
	resetLogf(t)
	Logf = nil

	dir := t.TempDir()
	logPath := filepath.Join(dir, "custom-subdir", "override.log")
	cfg := Config{LogFile: logPath}

	_, closeLog := NewDaemon(cfg, nil, nil, nil, "bot")
	defer closeLog()

	Logf(0, "poll", "hello\n")

	if _, err := os.Stat(logPath); err != nil {
		t.Errorf("expected log file at explicit override path %s: %v", logPath, err)
	}
}

func TestNewDaemon_EmptyLogFileDisablesFileLogging(t *testing.T) {
	resetLogf(t)
	Logf = nil

	cfg := Config{LogFile: ""}
	_, closeLog := NewDaemon(cfg, nil, nil, nil, "bot")
	defer closeLog()

	if Logf != nil {
		t.Error("NewDaemon assigned pruefer.Logf despite an empty LogFile; want it left nil (file logging disabled)")
	}
}

func TestWireLogf_EmptyLogFileWithTUIDiscardsInsteadOfNil(t *testing.T) {
	resetLogf(t)
	Logf = nil

	// tui=true is passed directly (rather than exercised via NewDaemon's own
	// useTUI(cfg) call) because useTUI requires a real terminal, which a
	// unit test doesn't have.
	closeLog := wireLogf(Config{LogFile: ""}, true)
	defer closeLog()

	if Logf == nil {
		t.Fatal("wireLogf(LogFile=\"\", tui=true) left Logf nil; want a discard function assigned so log.go's raw stderr fallback never corrupts the TUI")
	}

	origStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w
	t.Cleanup(func() { os.Stderr = origStderr })

	Logf(103, "warn", "diff fetch failed: %v\n", "boom")

	w.Close()
	os.Stderr = origStderr
	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	if n != 0 {
		t.Errorf("expected no stderr output from the discard Logf in TUI mode, got %q", string(buf[:n]))
	}
}

func TestWireLogf_LogFileOpenFailureWithTUIDiscardsInsteadOfNil(t *testing.T) {
	resetLogf(t)
	Logf = nil

	// A path with a NUL byte is guaranteed to fail os.OpenFile on every
	// platform, forcing newFileLogger's error branch without relying on
	// filesystem permissions (which behave inconsistently across CI
	// environments, e.g. root-owned runners bypassing 0000 dirs).
	badPath := "bad\x00path.log"
	closeLog := wireLogf(Config{LogFile: badPath}, true)
	defer closeLog()

	if Logf == nil {
		t.Fatal("wireLogf with an unopenable LogFile and tui=true left Logf nil; want a discard function assigned so log.go's raw stderr fallback never corrupts the TUI")
	}
}

func TestWireLogf_EmptyLogFileWithoutTUILeavesLogfNil(t *testing.T) {
	resetLogf(t)
	Logf = nil

	closeLog := wireLogf(Config{LogFile: ""}, false)
	defer closeLog()

	if Logf != nil {
		t.Error("wireLogf(LogFile=\"\", tui=false) assigned Logf; want it left nil (plain mode keeps the existing stderr-fallback behavior)")
	}
}

func TestLogf_FallsBackToStderrWhenLogfIsNil(t *testing.T) {
	// No Daemon/NewDaemon involved at all — this is AC5's "package used
	// without a Daemon" scenario. Logf must be nil for this test to be
	// meaningful; guard against leakage from a prior test.
	if Logf != nil {
		t.Fatal("Logf is unexpectedly non-nil at start of test — a prior test leaked global state")
	}

	origStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w
	t.Cleanup(func() { os.Stderr = origStderr })

	logf(103, "warn", "diff fetch failed: %v\n", "boom")

	w.Close()
	os.Stderr = origStderr
	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	got := string(buf[:n])

	if got != "[pr#103 warn] diff fetch failed: boom\n" {
		t.Errorf("stderr output = %q, want the unmodified stderr-fallback format", got)
	}
}
