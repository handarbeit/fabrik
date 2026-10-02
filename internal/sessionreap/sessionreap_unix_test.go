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

// worker starts a session leader whose child setpgrp()s itself (via perl; dash has no usable `set -m`) into a
// separate process group of the same session. The child traps INT/TERM into
// marker files and keeps running. Returns leader and child PIDs.
func worker(t *testing.T, dir string) (leader, child int) {
	t.Helper()
	script := "perl -e '$SIG{INT}=q(DEFAULT); $SIG{QUIT}=q(DEFAULT); setpgrp(0,0); exec @ARGV' sh -c \"trap 'echo x >> " + dir + "/sigs' INT TERM; echo \\$\\$ > " + dir + "/child.pid; while :; do sleep 0.05; done\" >/dev/null 2>&1 &\n" +
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

func TestEscalate_StopsWhenLeaderPIDIsRecycledMidEscalation(t *testing.T) {
	// The worker's leader is reaped during a grace window and its PID is reused
	// by an unrelated process that becomes a session leader. Members of that
	// session carry the same SID value; Escalate must not signal them.
	// PIDs above any kernel pid_max, so the real kill(2) in signalMembers is ESRCH.
	const sid, orphan, impostor = 90000001, 90000002, 90000003
	var mu sync.Mutex
	listing := 0
	o := Options{
		Log:  func(string, string, ...any) {},
		Poll: time.Millisecond,
		List: func() ([]int, error) {
			mu.Lock()
			defer mu.Unlock()
			listing++
			switch {
			case listing == 1: // first step: leader and one member
				return []int{sid, orphan}, nil
			case listing == 2: // leader reaped, member still running
				return []int{orphan}, nil
			default: // PID reused: a new leader with its own session member
				return []int{sid, impostor}, nil
			}
		},
		Getsid: func(pid int) (int, error) { return sid, nil },
		Comm:   func(int) string { return "x" },
	}
	sink := &logSink{}
	o.Log = sink.log
	done := make(chan struct{})
	go func() {
		defer close(done)
		Escalate(sid, "test", 50*time.Millisecond, 50*time.Millisecond, o)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Escalate did not stop")
	}
	if !strings.Contains(sink.joined(), "recycled PID") {
		t.Errorf("recycled-leader stop not logged: %q", sink.joined())
	}
	if strings.Contains(sink.joined(), "SIGTERM") || strings.Contains(sink.joined(), "SIGKILL") {
		t.Errorf("escalated after leader PID recycled: %q", sink.joined())
	}
}

func TestMembers_ExcludesZombies(t *testing.T) {
	// Seam: a Getsid-matching entry reported as a zombie is not a member.
	o := Options{
		List:   func() ([]int, error) { return []int{90000010, 90000011}, nil },
		Getsid: func(int) (int, error) { return 90000010, nil },
		Zombie: func(pid int) bool { return pid == 90000011 },
	}
	got, err := Members(90000010, o)
	if err != nil || len(got) != 1 || got[0] != 90000010 {
		t.Fatalf("Members = %v, %v; want only the non-zombie", got, err)
	}
}

func TestMembers_RealZombieIsNotAMember(t *testing.T) {
	// A session leader that exited and was deliberately never waited on stays a
	// zombie child of this process; Getsid still answers for it.
	cmd := exec.Command("sh", "-c", "exit 0")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() { _ = cmd.Wait() }) // reap at the end so the test leaves no zombie
	deadline := time.Now().Add(5 * time.Second)
	for !isZombie(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("pid %d never became a detectable zombie", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if sid, err := unix.Getsid(pid); err == nil && sid == pid {
		// the raw Getsid check alone would count it; Members must not
		m, merr := Members(pid, Options{})
		if merr != nil {
			t.Fatal(merr)
		}
		if len(m) != 0 {
			t.Errorf("Members = %v; a zombie must not count as a member", m)
		}
	}
}

// commandWorker models the real Claude CLI shape (#1989 validation finding): the
// worker is a session leader, and each Bash-tool command runs in a session of
// its own created by the command shell's setsid(), with the test tree below it
// in a separate process group of THAT session:
//
//	worker W (sid W) ── command shell C (sid C, setsid) ── child G (sid C, own pgid)
//
// The shell is started from the worker with `&`, so its PPID chain leads to W
// while W lives. perl does the setsid()/setpgrp() so the fixture does not depend
// on the shell: dash (the Linux CI /bin/sh) has no usable `set -m`, and it also
// ignores SIGINT for background jobs, which perl resets so G's INT trap works.
func commandWorker(t *testing.T, dir string) (cmd *exec.Cmd, worker, shell, child int) {
	t.Helper()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	childSh := write("child.sh", "trap 'echo x >> "+dir+"/sigs' INT TERM\necho $$ > "+dir+"/child.pid\nwhile :; do sleep 0.05; done\n")
	cmdSh := write("cmd.sh", "perl -e '$SIG{INT}=q(DEFAULT); $SIG{QUIT}=q(DEFAULT); setpgrp(0,0); exec @ARGV' sh "+childSh+" >/dev/null 2>&1 &\necho $$ > "+dir+"/shell.pid\nwait\n")
	workerSh := write("worker.sh", "perl -MPOSIX -e 'POSIX::setsid(); $SIG{INT}=q(DEFAULT); $SIG{QUIT}=q(DEFAULT); exec @ARGV' sh "+cmdSh+" >/dev/null 2>&1 &\nsleep 60\n")
	cmd = exec.Command("sh", workerSh)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = cmd.Wait() }()
	worker = cmd.Process.Pid
	shell, _ = strconv.Atoi(waitFile(t, filepath.Join(dir, "shell.pid")))
	child, _ = strconv.Atoi(waitFile(t, filepath.Join(dir, "child.pid")))
	t.Cleanup(func() {
		for _, p := range []int{child, shell, worker} {
			_ = syscall.Kill(p, syscall.SIGKILL)
		}
		_ = syscall.Kill(-worker, syscall.SIGKILL)
	})
	if got, _ := unix.Getsid(shell); got != shell || shell == worker {
		t.Fatalf("command shell sid=%d pid=%d worker=%d; fixture must put the shell in its own session", got, shell, worker)
	}
	if got, _ := unix.Getsid(child); got != shell {
		t.Fatalf("child sid=%d, want the shell's session %d", got, shell)
	}
	if pg, _ := unix.Getpgid(child); pg == shell {
		t.Fatalf("child shares the shell's process group; fixture is vacuous")
	}
	if got, _ := unix.Getsid(worker); got != worker {
		t.Fatalf("worker sid=%d, want its own pid %d", got, worker)
	}
	return cmd, worker, shell, child
}

func TestDiscover_FindsCommandSessionBelowWorker(t *testing.T) {
	_, worker, shell, _ := commandWorker(t, t.TempDir())
	got, err := Discover(worker, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].SID != shell {
		t.Fatalf("Discover = %+v, want exactly the command shell's session %d", got, shell)
	}
	if got[0].Start == "" {
		t.Errorf("a live leader must carry a start token on this platform")
	}
	if r, _ := Discover(worker, Options{WorkerSIDOnly: true}); len(r) != 0 {
		t.Errorf("WorkerSIDOnly must discover nothing, got %+v", r)
	}
}

func TestEscalate_ReapsBashToolCommandSessionGracefully(t *testing.T) {
	dir := t.TempDir()
	_, worker, shell, child := commandWorker(t, dir)
	bystander := exec.Command("sleep", "60")
	bystander.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := bystander.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = bystander.Wait() }()
	t.Cleanup(func() { _ = syscall.Kill(bystander.Process.Pid, syscall.SIGKILL) })

	sink := &logSink{}
	tr := Track(worker, Options{Log: sink.log}, nil) // discovery needs the start token Track records
	defer tr.Close()
	Escalate(worker, "max_wall_time", 400*time.Millisecond, 400*time.Millisecond, Options{Log: sink.log, Poll: 10 * time.Millisecond})

	if !waitDead(child, 3*time.Second) || !waitDead(shell, 3*time.Second) {
		t.Fatalf("command session survived (child alive=%v shell alive=%v); log:\n%s", alive(child), alive(shell), sink.joined())
	}
	if b, err := os.ReadFile(filepath.Join(dir, "sigs")); err != nil || len(strings.TrimSpace(string(b))) == 0 {
		t.Errorf("child never received a graceful signal before dying: %v", err)
	}
	if !alive(bystander.Process.Pid) {
		t.Error("a process in an unrelated session was killed")
	}
	if !strings.Contains(sink.joined(), "command session(s)") || !strings.Contains(sink.joined(), strconv.Itoa(shell)) {
		t.Errorf("R5 line does not name the command session %d:\n%s", shell, sink.joined())
	}
}

func TestEscalate_WorkerSIDOnlyLeavesBashToolCommandAlive(t *testing.T) {
	// Neutralised twin: the pre-command-session behaviour (worker SID only).
	// The worker dies but its command session survives untouched — the shape
	// that made the live orphans — so the test above is not vacuous.
	dir := t.TempDir()
	_, worker, shell, child := commandWorker(t, dir)
	Escalate(worker, "max_wall_time", 100*time.Millisecond, 100*time.Millisecond, Options{WorkerSIDOnly: true, Poll: 10 * time.Millisecond})
	if !waitDead(worker, 3*time.Second) {
		t.Fatal("worker survived")
	}
	if !alive(child) || !alive(shell) {
		t.Fatalf("worker-SID-only reap reached the command session (child alive=%v shell alive=%v); fixture or twin is broken", alive(child), alive(shell))
	}
}

func TestSweep_ReapsSampledCommandSessionAfterWorkerExit(t *testing.T) {
	for _, tc := range []struct {
		name     string
		sample   bool
		only     bool
		wantGone bool
	}{
		{"sampled", true, false, true},
		{"never sampled leaves the residual gap", false, false, false},
		{"worker-SID-only twin", true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			_, worker, shell, child := commandWorker(t, dir)
			o := Options{Poll: 10 * time.Millisecond, WorkerSIDOnly: tc.only}
			tr := Track(worker, o, nil)
			defer tr.Close()
			if tc.sample {
				if n := len(tr.Sample()); n != 1 && !tc.only {
					t.Fatalf("Sample found %d sessions, want 1", n)
				}
			}
			// The worker exits (its leader is reaped); the command shell is
			// reparented to PID 1 and the parent chain is gone.
			_ = syscall.Kill(worker, syscall.SIGKILL)
			if !waitDead(worker, 3*time.Second) {
				t.Fatal("worker survived SIGKILL")
			}
			sink := &logSink{}
			o.Log = sink.log
			n := Sweep(worker, "clean_exit", o)
			if tc.wantGone {
				if !waitDead(child, 3*time.Second) || !waitDead(shell, 3*time.Second) {
					t.Fatalf("sampled command session survived the post-exit sweep; log:\n%s", sink.joined())
				}
				if n < 2 || !strings.Contains(sink.joined(), "exit=clean_exit") {
					t.Errorf("Sweep returned %d, log %q; want >=2 members and an R5 clean_exit line", n, sink.joined())
				}
			} else if !alive(child) {
				t.Errorf("command session was reaped without a sample / under the worker-SID-only twin")
			}
		})
	}
}

func TestReap_RefusesCommandSessionWhoseLeaderPIDWasRecycled(t *testing.T) {
	_, worker, _, child := commandWorker(t, t.TempDir())
	// A live process holds PID == SID. With a start token that does not match,
	// it is a recycled PID that became a session leader — never ours.
	stale := Session{SID: worker, Start: "not-the-recorded-token"}
	if ValidSession(stale, Options{}) {
		t.Fatal("a live leader with a different start token was trusted")
	}
	if n, rem := Reap(stale, Options{}); n != 0 || rem != 0 {
		t.Fatalf("Reap on a recycled session signalled %d (remaining %d)", n, rem)
	}
	if !alive(worker) || !alive(child) {
		t.Fatal("recycled-leader session members were killed")
	}
	// The recorded token matches: the leader is the sampled one and is reaped.
	good := Session{SID: worker, Start: startToken(worker)}
	if good.Start == "" {
		t.Skip("no start token on this platform")
	}
	if !ValidSession(good, Options{}) {
		t.Fatal("a live leader with the recorded token was not trusted")
	}
	if n, _ := Reap(good, Options{}); n < 1 {
		t.Fatalf("Reap of a valid session signalled %d", n)
	}
	if !waitDead(worker, 3*time.Second) {
		t.Error("leader survived Reap")
	}
}

func TestDiscover_NeverUsesAnUnrelatedProcessTree(t *testing.T) {
	// workerIsOurs gates discovery on the worker's recorded start token: with a
	// different token (a recycled PID) nothing below that PID may be adopted.
	_, worker, _, child := commandWorker(t, t.TempDir())
	o := Options{}
	if o.workerIsOurs(worker, "stale") || o.workerIsOurs(worker, "") {
		t.Fatal("discovery trusted a PID whose start token does not match")
	}
	tr := Track(worker, Options{Start: func(int) string { return "" }}, nil)
	defer tr.Close()
	if got := tr.Sample(); len(got) != 0 {
		t.Fatalf("Sample with no worker token adopted %+v", got)
	}
	if !alive(child) {
		t.Fatal("child died")
	}
}

func TestEscalate_WithoutTrackerNeverDiscoversCommandSessions(t *testing.T) {
	// No registered Tracker means no proof the live holder of PID sid is still
	// the worker, so Escalate acts on the worker's own SID only.
	dir := t.TempDir()
	_, worker, shell, child := commandWorker(t, dir)
	Escalate(worker, "max_wall_time", 100*time.Millisecond, 100*time.Millisecond, Options{Poll: 10 * time.Millisecond})
	if !waitDead(worker, 3*time.Second) {
		t.Fatal("worker survived")
	}
	if !alive(child) || !alive(shell) {
		t.Fatalf("an untracked Escalate reached the command session (child alive=%v shell alive=%v)", alive(child), alive(shell))
	}
}
