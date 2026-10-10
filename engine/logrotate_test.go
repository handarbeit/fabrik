package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// simulateStart mimics what Run() does at startup: rotate, open a fresh file
// (truncating only on the fallback), write the banner and one marker line.
func simulateStart(t *testing.T, path string, keep, run int) (warnings []string, truncate bool) {
	t.Helper()
	warnings, truncate = rotateEngineLog(path, keep)
	flags := os.O_CREATE | os.O_WRONLY
	if truncate {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(path, flags, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	now := time.Date(2026, 10, 10, 12, 0, run, 0, time.UTC)
	fmt.Fprint(f, startBanner(now, "v1.2.3", 4242, "fresh start"))
	fmt.Fprintf(f, "marker run-%d\n", run)
	return warnings, truncate
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}

func TestRotateEngineLog_FirstStartIsNoop(t *testing.T) {
	// No .fabrik directory at all, and then an empty one.
	missing := filepath.Join(t.TempDir(), ".fabrik", "fabrik.log")
	if w, trunc := rotateEngineLog(missing, 5); len(w) != 0 || trunc {
		t.Errorf("missing dir: warnings=%v truncate=%v, want none", w, trunc)
	}
	path := filepath.Join(t.TempDir(), "fabrik.log")
	if w, trunc := rotateEngineLog(path, 5); len(w) != 0 || trunc {
		t.Errorf("no file: warnings=%v truncate=%v, want none", w, trunc)
	}
	if _, err := os.Stat(path + ".1"); err == nil {
		t.Error("no backup should exist after a first start")
	}
}

func TestRotateEngineLog_ThreeStartsKeepEachRunWithItsBanner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fabrik.log")
	for run := 1; run <= 3; run++ {
		if w, trunc := simulateStart(t, path, 5, run); len(w) != 0 || trunc {
			t.Fatalf("run %d: warnings=%v truncate=%v", run, w, trunc)
		}
	}
	for file, run := range map[string]int{path: 3, path + ".1": 2, path + ".2": 1} {
		got := readFile(t, file)
		lines := strings.Split(got, "\n")
		if !strings.Contains(lines[0], "[startup] fabrik v1.2.3 pid=4242") {
			t.Errorf("%s line 1 is not the banner: %q", filepath.Base(file), lines[0])
		}
		if !strings.Contains(lines[0], fmt.Sprintf("started=2026-10-10T12:00:%02dZ", run)) {
			t.Errorf("%s banner has the wrong start time for run %d: %q", filepath.Base(file), run, lines[0])
		}
		if !strings.Contains(got, fmt.Sprintf("marker run-%d\n", run)) || strings.Count(got, "marker run-") != 1 {
			t.Errorf("%s should hold exactly run %d, got %q", filepath.Base(file), run, got)
		}
	}
	if _, err := os.Stat(path + ".3"); err == nil {
		t.Error("fabrik.log.3 should not exist after three runs")
	}
}

func TestRotateEngineLog_CapsBackups(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fabrik.log")
	const keep = 3
	for run := 1; run <= keep+4; run++ {
		simulateStart(t, path, keep, run)
	}
	last := keep + 4
	for k := 1; k <= keep; k++ {
		want := fmt.Sprintf("marker run-%d\n", last-k)
		if got := readFile(t, fmt.Sprintf("%s.%d", path, k)); !strings.Contains(got, want) {
			t.Errorf("fabrik.log.%d = %q, want run %d", k, got, last-k)
		}
	}
	if _, err := os.Stat(fmt.Sprintf("%s.%d", path, keep+1)); err == nil {
		t.Errorf("fabrik.log.%d must not exist: retention is capped at %d", keep+1, keep)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != keep+1 {
		t.Errorf("directory holds %d files, want %d", len(entries), keep+1)
	}
}

func TestRotateEngineLog_GapInChainStillShifts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fabrik.log")
	for p, c := range map[string]string{path: "cur", path + ".1": "one", path + ".3": "three"} {
		if err := os.WriteFile(p, []byte(c), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if w, trunc := rotateEngineLog(path, 5); len(w) != 0 || trunc {
		t.Fatalf("warnings=%v truncate=%v, want none", w, trunc)
	}
	for p, want := range map[string]string{path + ".1": "cur", path + ".2": "one", path + ".4": "three"} {
		if got := readFile(t, p); got != want {
			t.Errorf("%s = %q, want %q", filepath.Base(p), got, want)
		}
	}
	if _, err := os.Stat(path + ".3"); err == nil {
		t.Error("fabrik.log.3 should be empty slot after the shift")
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("fabrik.log should be gone until the caller opens a fresh one")
	}
}

// A non-empty directory in the .1 slot makes the final rename fail: the caller
// must be told to truncate so the file still holds exactly one run.
func TestRotateEngineLog_UnrotatableLogFallsBackToTruncate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fabrik.log")
	if err := os.WriteFile(path, []byte("old run\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// With keep=1 the only slot is .1: a non-empty directory there can neither
	// be removed nor replaced by the log file.
	if err := os.MkdirAll(filepath.Join(path+".1", "child"), 0755); err != nil {
		t.Fatal(err)
	}
	w, trunc := simulateStart(t, path, 1, 1)
	if !trunc {
		t.Fatalf("expected truncate fallback, warnings=%v", w)
	}
	if len(w) == 0 {
		t.Error("expected a warning for the failed rotation")
	}
	got := readFile(t, path)
	if strings.Contains(got, "old run") || !strings.Contains(got, "marker run-1") {
		t.Errorf("fresh log must hold only the new run, got %q", got)
	}
}

// A slot that cannot be removed or shifted warns, but the rest of the chain
// still rotates and a fresh log is opened.
func TestRotateEngineLog_UnremovableOldestSlotWarnsAndContinues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fabrik.log")
	if err := os.WriteFile(path, []byte("cur"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".1", []byte("one"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(path+".2", "child"), 0755); err != nil { // oldest slot (keep=2): not removable by os.Remove
		t.Fatal(err)
	}
	w, trunc := rotateEngineLog(path, 2)
	if len(w) < 2 {
		t.Errorf("expected warnings for the unremovable oldest slot and the blocked shift, got %v", w)
	}
	// .1 cannot move into .2, but the log itself still replaces .1.
	if trunc {
		t.Error("the log rotated into .1 fine, so no truncate fallback is needed")
	}
	if got := readFile(t, path+".1"); got != "cur" {
		t.Errorf("fabrik.log.1 = %q, want the previous log", got)
	}
}

func TestStartBanner(t *testing.T) {
	now := time.Date(2026, 10, 10, 17, 4, 5, 0, time.FixedZone("x", 3600))
	got := startBanner(now, "dev(82b23bc)+dirty", 77, "SIGHUP restart")
	want := "2026-10-10T16:04:05Z [startup] fabrik dev(82b23bc)+dirty pid=77 started=2026-10-10T16:04:05Z reason=SIGHUP restart\n"
	if got != want {
		t.Errorf("banner =\n%q\nwant\n%q", got, want)
	}
	if strings.Count(got, "\n") != 1 {
		t.Error("banner must be exactly one line")
	}
	if !regexp.MustCompile(`reason=unknown\n$`).MatchString(startBanner(now, "v1", 1, "")) {
		t.Error("empty reason must render as reason=unknown")
	}
}

// Run() rotates a pre-existing log to .1 and opens the new one with the banner
// as its very first line.
func TestRun_RotatesLogAndWritesBannerFirst(t *testing.T) {
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{}, nil
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	eng.cfg.PollSeconds = 300
	eng.cfg.Version = "v9.9.9"
	eng.cfg.StartReason = "SIGHUP restart"

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".fabrik"), 0755); err != nil {
		t.Fatal(err)
	}
	eng.fabrikDir = dir
	logPath := filepath.Join(dir, ".fabrik", "fabrik.log")
	if err := os.WriteFile(logPath, []byte("previous run\n"), 0600); err != nil {
		t.Fatal(err)
	}

	readyCh := make(chan struct{})
	eng.cfg.ReadyCh = readyCh
	done := make(chan error, 1)
	go func() { done <- eng.Run() }()
	awaitRunReady(t, readyCh, done)
	p, _ := os.FindProcess(os.Getpid())
	p.Signal(syscall.SIGINT)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not shut down in time")
	}

	if got := readFile(t, logPath+".1"); got != "previous run\n" {
		t.Errorf("fabrik.log.1 = %q, want the previous run's content", got)
	}
	cur := readFile(t, logPath)
	first := strings.SplitN(cur, "\n", 2)[0]
	for _, want := range []string{"[startup] fabrik v9.9.9", fmt.Sprintf("pid=%d", os.Getpid()), "reason=SIGHUP restart"} {
		if !strings.Contains(first, want) {
			t.Errorf("banner %q missing %q", first, want)
		}
	}
	if strings.Contains(cur, "previous run") {
		t.Error("fabrik.log must hold only the current run")
	}
	if fi, err := os.Stat(logPath); err != nil || fi.Mode().Perm() != 0600 {
		t.Errorf("fabrik.log mode = %v (err %v), want 0600", fi.Mode().Perm(), err)
	}
}
