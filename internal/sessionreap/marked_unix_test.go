//go:build !windows

package sessionreap

import (
	"os"
	"os/exec"
	"sync"
	"syscall"
	"testing"
	"time"
)

const testMarkerKey = "FABRIK_WORKER_OWNER"

// markedSleeper starts a sleeping process in its own session (so the R4 caller
// session refusal does not apply) and returns it with a Wait goroutine so a
// signalled process is reaped rather than left a zombie.
func markedSleeper(t *testing.T) (*exec.Cmd, <-chan struct{}) {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-done })
	return cmd, done
}

func exited(done <-chan struct{}, within time.Duration) bool {
	select {
	case <-done:
		return true
	case <-time.After(within):
		return false
	}
}

// envTable is an Environ seam: pid -> environment. A pid with no entry reads as
// unreadable (the SIP / other-user case).
type envTable struct {
	mu sync.Mutex
	m  map[int][]string
}

func (e *envTable) set(pid int, env ...string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.m == nil {
		e.m = map[int][]string{}
	}
	e.m[pid] = env
}

func (e *envTable) get(pid int) ([]string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if env, ok := e.m[pid]; ok {
		return env, nil
	}
	return nil, ErrEnvUnreadable
}

func markers(v ...string) map[string]struct{} {
	m := map[string]struct{}{}
	for _, s := range v {
		m[s] = struct{}{}
	}
	return m
}

func TestReapMarked_SignalsMarkedAndLeavesUnmarked(t *testing.T) {
	marked, markedDone := markedSleeper(t)
	other, otherDone := markedSleeper(t)       // carries a different worker's marker
	unmarked, unmarkedDone := markedSleeper(t) // ppid-1-shaped bystander: no marker at all
	var tbl envTable
	tbl.set(marked.Process.Pid, "A=1", testMarkerKey+"=n1")
	tbl.set(other.Process.Pid, testMarkerKey+"=n2")
	tbl.set(unmarked.Process.Pid, "A=1")

	n := ReapMarked(testMarkerKey, markers("n1"), "test", 2*time.Second, Options{Environ: tbl.get})
	if n != 1 {
		t.Fatalf("ReapMarked = %d, want 1", n)
	}
	if !exited(markedDone, 5*time.Second) {
		t.Fatal("marked process was not reaped")
	}
	if exited(otherDone, 300*time.Millisecond) {
		t.Fatal("process marked for another owner was killed")
	}
	if exited(unmarkedDone, 10*time.Millisecond) {
		t.Fatal("unmarked process was killed on shape alone")
	}
}

// Non-vacuity: with the marker scan neutralised the same setup reaps nothing.
func TestReapMarked_IgnoreMarkersReapsNothing(t *testing.T) {
	marked, done := markedSleeper(t)
	var tbl envTable
	tbl.set(marked.Process.Pid, testMarkerKey+"=n1")
	if n := ReapMarked(testMarkerKey, markers("n1"), "test", 0, Options{Environ: tbl.get, IgnoreMarkers: true}); n != 0 {
		t.Fatalf("ReapMarked with IgnoreMarkers = %d, want 0", n)
	}
	if exited(done, 300*time.Millisecond) {
		t.Fatal("process was killed although the marker scan was neutralised")
	}
}

func TestReapMarked_UnreadableEnvironmentIsNotMarked(t *testing.T) {
	p, done := markedSleeper(t)
	_ = p
	if n := ReapMarked(testMarkerKey, markers("n1"), "test", 0, Options{Environ: (&envTable{}).get}); n != 0 {
		t.Fatalf("ReapMarked = %d, want 0", n)
	}
	if exited(done, 300*time.Millisecond) {
		t.Fatal("process with an unreadable environment was killed")
	}
}

func TestReapMarked_NeverSignalsItself(t *testing.T) {
	var tbl envTable
	tbl.set(os.Getpid(), testMarkerKey+"=n1")
	// Would be fatal to the test binary if the own-PID refusal were missing.
	if n := ReapMarked(testMarkerKey, markers("n1"), "test", 0, Options{Environ: tbl.get}); n != 0 {
		t.Fatalf("ReapMarked signalled %d process(es) including itself", n)
	}
}

func TestReapMarked_RecycledPIDIsSkipped(t *testing.T) {
	p, done := markedSleeper(t)
	var tbl envTable
	tbl.set(p.Process.Pid, testMarkerKey+"=n1")
	// The start token read at scan time differs from the one read just before the
	// signal: the PID now names a different process.
	var mu sync.Mutex
	calls := 0
	start := func(int) string {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls == 1 {
			return "scanned"
		}
		return "recycled"
	}
	if n := ReapMarked(testMarkerKey, markers("n1"), "test", 0, Options{Environ: tbl.get, Start: start}); n != 0 {
		t.Fatalf("ReapMarked = %d, want 0 for a recycled PID", n)
	}
	if exited(done, 300*time.Millisecond) {
		t.Fatal("recycled PID was signalled")
	}
}

func TestReapMarked_UnknownStartTokenFailsClosed(t *testing.T) {
	p, done := markedSleeper(t)
	var tbl envTable
	tbl.set(p.Process.Pid, testMarkerKey+"=n1")
	if n := ReapMarked(testMarkerKey, markers("n1"), "test", 0, Options{Environ: tbl.get, Start: func(int) string { return "" }}); n != 0 {
		t.Fatalf("ReapMarked = %d, want 0 when identity cannot be established", n)
	}
	if exited(done, 300*time.Millisecond) {
		t.Fatal("process with no start token was signalled")
	}
}

func TestReapMarked_RefusesCallersOwnSession(t *testing.T) {
	// Same session as the test binary: CheckSID refuses it even though marked.
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-done })
	var tbl envTable
	tbl.set(cmd.Process.Pid, testMarkerKey+"=n1")
	if n := ReapMarked(testMarkerKey, markers("n1"), "test", 0, Options{Environ: tbl.get}); n != 0 {
		t.Fatalf("ReapMarked = %d, want 0 for the caller's own session", n)
	}
	if exited(done, 300*time.Millisecond) {
		t.Fatal("process in the caller's own session was signalled")
	}
}

func TestReapMarked_EscalatesToSIGKILL(t *testing.T) {
	// A process that ignores SIGTERM must still die, after the grace window.
	cmd := exec.Command("sh", "-c", "trap '' TERM; while :; do sleep 1; done")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-done })
	time.Sleep(200 * time.Millisecond) // let the trap install
	var tbl envTable
	tbl.set(cmd.Process.Pid, testMarkerKey+"=n1")
	if n := ReapMarked(testMarkerKey, markers("n1"), "test", 300*time.Millisecond, Options{Environ: tbl.get, Poll: 20 * time.Millisecond}); n != 1 {
		t.Fatalf("ReapMarked = %d, want 1", n)
	}
	if !exited(done, 5*time.Second) {
		t.Fatal("SIGTERM-ignoring process survived the SIGKILL escalation")
	}
}

func TestMarkedPIDs_GroupsByMarker(t *testing.T) {
	a, _ := markedSleeper(t)
	b, _ := markedSleeper(t)
	var tbl envTable
	tbl.set(a.Process.Pid, testMarkerKey+"=n1")
	tbl.set(b.Process.Pid, testMarkerKey+"=n2")
	got, err := MarkedPIDs(testMarkerKey, markers("n1", "n2"), Options{Environ: tbl.get})
	if err != nil {
		t.Fatal(err)
	}
	if len(got["n1"]) != 1 || got["n1"][0] != a.Process.Pid || len(got["n2"]) != 1 || got["n2"][0] != b.Process.Pid {
		t.Fatalf("MarkedPIDs = %v", got)
	}
}
