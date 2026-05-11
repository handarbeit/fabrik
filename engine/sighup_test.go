//go:build !windows

package engine

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

func TestRun_SighupRestart(t *testing.T) {
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{}, nil
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})
	eng.cfg.PollSeconds = 300 // long poll so we don't hit a second tick

	// Set up a temp fabrikDir so Run() can create the lock file.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".fabrik"), 0755); err != nil {
		t.Fatal(err)
	}
	eng.fabrikDir = dir

	// Use ReadyCh so we only send SIGHUP after signal.Notify is registered.
	readyCh := make(chan struct{})
	eng.cfg.ReadyCh = readyCh

	// seq is a monotonic counter; each hook records the value at call time so we
	// can assert cleanup happened strictly before exec.
	var seq atomic.Int32
	var cleanupSeq, execSeq int32

	// Set cleanupHook directly (bypassing SetCleanupHook) to capture order.
	eng.cleanupHook = func() {
		cleanupSeq = seq.Add(1)
	}

	// Override exec so the test process is not actually replaced.
	var capturedBin string
	var capturedArgs []string
	var capturedEnv []string
	eng.sighupExecFn = func(argv0 string, argv []string, envv []string) error {
		execSeq = seq.Add(1)
		capturedBin = argv0
		capturedArgs = argv
		capturedEnv = envv
		return nil
	}

	done := make(chan error, 1)
	go func() {
		done <- eng.Run()
	}()

	// Wait for Run to register signal handlers before sending SIGHUP.
	<-readyCh
	p, _ := os.FindProcess(os.Getpid())
	p.Signal(syscall.SIGHUP)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not shut down after SIGHUP in time")
	}

	if execSeq == 0 {
		t.Fatal("sighupExecFn was not called")
	}
	if capturedBin == "" {
		t.Error("exec called with empty binary path")
	}
	if len(capturedArgs) == 0 {
		t.Error("exec called with empty args")
	}
	if !eng.sighupRequested.Load() {
		t.Error("sighupRequested flag not set")
	}
	if cleanupSeq == 0 {
		t.Fatal("cleanupHook was not called before exec")
	}
	if cleanupSeq >= execSeq {
		t.Errorf("cleanupHook must be called before exec: cleanupSeq=%d execSeq=%d", cleanupSeq, execSeq)
	}

	// Verify FABRIK_SIGHUP_RESTART=1 is injected so cmd/root.go can detect
	// a re-exec'd process and skip the interactive plugin skills prompt.
	found := false
	for _, kv := range capturedEnv {
		if kv == "FABRIK_SIGHUP_RESTART=1" {
			found = true
			break
		}
	}
	if !found {
		t.Error("exec env did not contain FABRIK_SIGHUP_RESTART=1")
	}
}
