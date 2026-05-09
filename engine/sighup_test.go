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

	// Override exec so the test process is not actually replaced.
	var execCalled atomic.Bool
	var capturedBin string
	var capturedArgs []string
	eng.sighupExecFn = func(argv0 string, argv []string, envv []string) error {
		execCalled.Store(true)
		capturedBin = argv0
		capturedArgs = argv
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

	if !execCalled.Load() {
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
}
