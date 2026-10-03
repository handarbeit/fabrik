//go:build !windows

package engine

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/localapi"
)

func lifecycleEngine(t *testing.T) *Engine {
	t.Helper()
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{}, nil
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	eng.cfg.PollSeconds = 300
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".fabrik"), 0o755); err != nil {
		t.Fatal(err)
	}
	eng.fabrikDir = dir
	return eng
}

// waitForAPI blocks until the daemon answers on its socket.
func waitForAPI(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if err := localapi.Call(context.Background(), path, localapi.MethodHello, nil, nil); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("local API never came up at %s", path)
}

// A stale socket from a previous exec is replaced at startup; the API serves;
// SIGHUP closes and unlinks the socket strictly before the exec; and a second
// daemon in another directory gets its own path.
func TestRun_LocalAPILifecycleAcrossSighupRestart(t *testing.T) {
	eng := lifecycleEngine(t)
	path := localapi.SocketPath(eng.fabrikDir)

	// Leave a stale socket file at the path, as a crashed/exec'd process would.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	stale, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	stale.Close()
	if fi, err := os.Lstat(path); err != nil || fi.Mode()&os.ModeSocket == 0 {
		t.Fatalf("setup: stale socket missing: %v", err)
	}

	readyCh := make(chan struct{})
	eng.cfg.ReadyCh = readyCh

	var socketGoneAtExec bool
	execCalled := make(chan struct{})
	eng.sighupExecFn = func(string, []string, []string) error {
		_, statErr := os.Lstat(path)
		socketGoneAtExec = errors.Is(statErr, os.ErrNotExist)
		close(execCalled)
		return nil
	}

	done := make(chan error, 1)
	go func() { done <- eng.Run() }()
	<-readyCh
	waitForAPI(t, path) // the stale file was replaced and the daemon serves

	var health localapi.HealthResult
	if err := localapi.Call(context.Background(), path, localapi.MethodHealth, nil, &health); err != nil {
		t.Fatalf("health over the real socket: %v", err)
	}
	if health.AsOf.IsZero() {
		t.Error("health carries no as_of")
	}

	// A different directory is a different socket.
	if other := localapi.SocketPath(t.TempDir()); other == path {
		t.Errorf("two daemons must not share a socket: %s", other)
	}

	p, _ := os.FindProcess(os.Getpid())
	p.Signal(syscall.SIGHUP)
	select {
	case <-execCalled:
	case <-time.After(10 * time.Second):
		t.Fatal("exec was never reached after SIGHUP")
	}
	if !socketGoneAtExec {
		t.Error("the socket must be closed and unlinked BEFORE the exec (an exec runs no deferred cleanup)")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("socket file still present after shutdown: %v", err)
	}
}

// Clean shutdown (context cancel, no re-exec) also removes the socket, and a
// failure to bind never stops the daemon.
func TestRun_LocalAPIRemovedOnCleanShutdownAndBindFailureIsNonFatal(t *testing.T) {
	eng := lifecycleEngine(t)
	path := localapi.SocketPath(eng.fabrikDir)
	readyCh := make(chan struct{})
	eng.cfg.ReadyCh = readyCh
	done := make(chan error, 1)
	go func() { done <- eng.Run() }()
	<-readyCh
	waitForAPI(t, path)
	p, _ := os.FindProcess(os.Getpid())
	p.Signal(syscall.SIGINT)
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not stop on SIGINT")
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("socket file still present after clean shutdown: %v", err)
	}

	// Bind failure: a regular file where the socket should be.
	eng2 := lifecycleEngine(t)
	bad := localapi.SocketPath(eng2.fabrikDir)
	if err := os.MkdirAll(filepath.Dir(bad), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, []byte("not a socket"), 0o600); err != nil {
		t.Fatal(err)
	}
	ready2 := make(chan struct{})
	eng2.cfg.ReadyCh = ready2
	done2 := make(chan error, 1)
	go func() { done2 <- eng2.Run() }()
	<-ready2
	time.Sleep(500 * time.Millisecond) // let Run get past startLocalAPI
	select {
	case err := <-done2:
		t.Fatalf("a failed API bind must not stop the daemon, Run returned %v", err)
	default:
	}
	if b, _ := os.ReadFile(bad); string(b) != "not a socket" {
		t.Error("the regular file was modified")
	}
	p.Signal(syscall.SIGINT)
	select {
	case <-done2:
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not stop on SIGINT")
	}
}
