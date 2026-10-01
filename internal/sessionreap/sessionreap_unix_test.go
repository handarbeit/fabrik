//go:build !windows

package sessionreap

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (l *logSink) log(tag, format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf("["+tag+"] "+format, args...))
}

func (l *logSink) joined() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "")
}

func alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

func waitDead(pid int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if !alive(pid) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return !alive(pid)
}

func waitFile(t *testing.T, p string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(p); err == nil && len(strings.TrimSpace(string(b))) > 0 {
			return strings.TrimSpace(string(b))
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s never appeared", p)
	return ""
}

// worker starts a session leader whose child lands, under job control, in a
// separate process group of the same session. The child traps INT/TERM into
// marker files and keeps running. Returns leader and child PIDs.
func worker(t *testing.T, dir string) (leader, child int) {
	t.Helper()
	script := "set -m\n" +
		"sh -c \"trap 'echo x >> " + dir + "/sigs' INT TERM; echo \\$\\$ > " + dir + "/child.pid; while :; do sleep 0.05; done\" >/dev/null 2>&1 &\n" +
		"sleep 60\n"
	cmd := exec.Command("sh", "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = cmd.Wait() }() // reap the leader so it never lingers as a zombie
	leader = cmd.Process.Pid
	child, _ = strconv.Atoi(waitFile(t, filepath.Join(dir, "child.pid")))
	t.Cleanup(func() {
		_ = syscall.Kill(-leader, syscall.SIGKILL)
		_ = syscall.Kill(child, syscall.SIGKILL)
	})
	if sid, _ := unix.Getsid(child); sid != leader {
		t.Fatalf("child sid %d != leader %d", sid, leader)
	}
	if pg, _ := unix.Getpgid(child); pg == leader {
		t.Fatalf("child shares the leader's process group; fixture is vacuous")
	}
	return leader, child
}

func TestCheckSID_RefusesUnsafe(t *testing.T) {
	own, err := unix.Getsid(0)
	if err != nil {
		t.Fatal(err)
	}
	for _, sid := range []int{-5, 0, 1, own} {
		if err := CheckSID(sid); !errors.Is(err, ErrUnsafeSID) {
			t.Errorf("CheckSID(%d) = %v, want ErrUnsafeSID", sid, err)
		}
	}
}

// A bystander in another session must survive every entry point, even with a
// lister that offers it and a Getsid that claims it belongs to the unsafe SID.
func TestUnsafeSIDNeverSignals(t *testing.T) {
	bystander := exec.Command("sleep", "30")
	if err := bystander.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bystander.Process.Kill(); _ = bystander.Wait() })
	own, _ := unix.Getsid(0)

	for _, sid := range []int{0, 1, own} {
		sink := &logSink{}
		o := Options{
			Log:    sink.log,
			List:   func() ([]int, error) { return []int{bystander.Process.Pid}, nil },
			Getsid: func(int) (int, error) { return sid, nil },
		}
		if n, err := Signal(sid, syscall.SIGKILL, o); n != 0 || !errors.Is(err, ErrUnsafeSID) {
			t.Errorf("Signal(sid=%d) = %d, %v", sid, n, err)
		}
		Escalate(sid, "test", 10*time.Millisecond, 10*time.Millisecond, o)
		if n := Sweep(sid, "clean_exit", o); n != 0 {
			t.Errorf("Sweep(sid=%d) = %d", sid, n)
		}
		if !alive(bystander.Process.Pid) {
			t.Fatalf("bystander killed for unsafe sid %d", sid)
		}
		if !strings.Contains(sink.joined(), "refused") {
			t.Errorf("sid %d: no refusal logged: %q", sid, sink.joined())
		}
	}
}

func TestSignal_GetsidRecheckSkipsLeavers(t *testing.T) {
	bystander := exec.Command("sleep", "30")
	if err := bystander.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bystander.Process.Kill(); _ = bystander.Wait() })

	calls := 0
	o := Options{
		List: func() ([]int, error) { return []int{bystander.Process.Pid}, nil },
		// Matches during enumeration, then the process "leaves" the session.
		Getsid: func(int) (int, error) {
			calls++
			if calls == 1 {
				return 4242, nil
			}
			return 1234, nil
		},
	}
	if n, err := Signal(4242, syscall.SIGKILL, o); n != 0 || err != nil {
		t.Fatalf("Signal = %d, %v; want 0, nil", n, err)
	}
	if !alive(bystander.Process.Pid) {
		t.Fatal("process that left the session was signalled")
	}
}

func TestEscalate_SignalsWholeSessionInOrder(t *testing.T) {
	dir := t.TempDir()
	leader, child := worker(t, dir)
	sink := &logSink{}
	o := Options{Log: sink.log, Poll: 10 * time.Millisecond}

	Escalate(leader, "max_wall_time", 400*time.Millisecond, 400*time.Millisecond, o)

	if !waitDead(child, 3*time.Second) {
		t.Fatalf("same-session child %d survived Escalate; log:\n%s", child, sink.joined())
	}
	if !waitDead(leader, 3*time.Second) {
		t.Fatalf("leader survived")
	}
	b, _ := os.ReadFile(filepath.Join(dir, "sigs"))
	if len(strings.Fields(string(b))) < 1 {
		t.Errorf("child never saw a graceful signal (SIGINT/SIGTERM) before SIGKILL; log:\n%s", sink.joined())
	}
	log := sink.joined()
	iInt := strings.Index(log, "SIGINT")
	if iInt < 0 || !strings.Contains(log, "member(s)") {
		t.Errorf("missing R5 SIGINT line: %q", log)
	}
	if iTerm := strings.Index(log, "SIGTERM"); iTerm >= 0 && iTerm < iInt {
		t.Errorf("SIGTERM logged before SIGINT: %q", log)
	}
}

func TestEscalate_ExitsEarlyWhenSessionEmpty(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = cmd.Wait() }()
	start := time.Now()
	Escalate(cmd.Process.Pid, "test", 5*time.Second, 5*time.Second, Options{Poll: 10 * time.Millisecond})
	if el := time.Since(start); el > 2*time.Second {
		t.Errorf("took %s; SIGINT killed the only member, grace must end early", el)
	}
	if !waitDead(cmd.Process.Pid, time.Second) {
		t.Error("member survived")
	}
}

func TestEscalate_ZeroGraceGoesStraightToSIGKILL(t *testing.T) {
	dir := t.TempDir()
	leader, child := worker(t, dir)
	Escalate(leader, "test", 0, 0, Options{})
	if !waitDead(child, 3*time.Second) || !waitDead(leader, 3*time.Second) {
		t.Fatal("session survived zero-grace escalation")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "sigs")); len(b) != 0 {
		t.Errorf("zero grace delivered a trappable signal: %q", b)
	}
}

func TestEscalate_ListErrorFallsBackToProcessGroup(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = cmd.Wait() }()
	sink := &logSink{}
	o := Options{Log: sink.log, List: func() ([]int, error) { return nil, errors.New("boom") }}
	Escalate(cmd.Process.Pid, "test", 0, 0, o)
	if !waitDead(cmd.Process.Pid, 3*time.Second) {
		t.Fatal("leader survived the process-group fallback")
	}
	if !strings.Contains(sink.joined(), "falling back") {
		t.Errorf("fallback not logged: %q", sink.joined())
	}
}

func TestSweep_KillsSameSessionChildAndLogsR5(t *testing.T) {
	dir := t.TempDir()
	leader, child := worker(t, dir)
	// Simulate the post-exit state: the leader is gone, its child remains.
	if err := syscall.Kill(leader, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if !waitDead(leader, 3*time.Second) {
		t.Fatal("leader did not die")
	}
	sink := &logSink{}
	n := Sweep(leader, "clean_exit", Options{Log: sink.log})
	if n < 1 {
		t.Fatalf("Sweep signalled %d, want >= 1; log %q", n, sink.joined())
	}
	if !waitDead(child, 3*time.Second) {
		t.Fatal("child survived Sweep")
	}
	log := sink.joined()
	if !strings.Contains(log, "exit=clean_exit") || !strings.Contains(log, fmt.Sprintf("signalled %d member(s)", n)) {
		t.Errorf("R5 line malformed: %q", log)
	}
}

func TestSweep_QuietWhenNothingToReap(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = cmd.Wait() }()
	pid := cmd.Process.Pid
	_ = syscall.Kill(pid, syscall.SIGKILL)
	waitDead(pid, 3*time.Second)
	sink := &logSink{}
	if n := Sweep(pid, "clean_exit", Options{Log: sink.log}); n != 0 || sink.joined() != "" {
		t.Errorf("Sweep = %d, log %q; want silent 0", n, sink.joined())
	}
}

func TestSweep_RefusesWhenLeaderPIDIsRecycled(t *testing.T) {
	// A live process whose PID equals the SID is, after the worker exited, a
	// recycled PID that became a session leader. Its session's members carry the
	// same SID value, so Sweep must refuse outright and touch nothing.
	leader, child := worker(t, t.TempDir())
	sink := &logSink{}
	if n := Sweep(leader, "after_stop", Options{Log: sink.log}); n != 0 {
		t.Errorf("Sweep signalled %d; a recycled leader PID must refuse the sweep", n)
	}
	if !alive(leader) || !alive(child) {
		t.Fatal("leader-PID holder or its session member was killed")
	}
	if !strings.Contains(sink.joined(), "refused") {
		t.Errorf("refusal not logged: %q", sink.joined())
	}
}

func TestFormatComms_TruncatesAndCaps(t *testing.T) {
	long := strings.Repeat("a", 100)
	got := formatComms([]string{"sh", long, "", "d", "e", "f", "g"})
	if !strings.Contains(got, strings.Repeat("a", maxCommLen)+"…") || strings.Contains(got, strings.Repeat("a", maxCommLen+1)) {
		t.Errorf("long name not truncated: %q", got)
	}
	if !strings.Contains(got, "(+2 more)") {
		t.Errorf("cap tail missing: %q", got)
	}
	if !strings.Contains(got, "?") {
		t.Errorf("empty name not rendered: %q", got)
	}
}

func TestMembers_RealProcessTable(t *testing.T) {
	dir := t.TempDir()
	leader, child := worker(t, dir)
	m, err := Members(leader, Options{})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[int]bool{}
	for _, p := range m {
		seen[p] = true
	}
	if !seen[leader] || !seen[child] {
		t.Errorf("Members(%d) = %v; want leader and child", leader, m)
	}
}
