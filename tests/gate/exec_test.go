package gate

import (
	"bytes"
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Ported from scripts/e2e/hang_hardening_test.sh: the with_timeout / run_reaped
// / bounded-drain cases, against the real OSExec and real child processes. The
// bash-specific cases (the `$(with_timeout …)` command-substitution pitfall, the
// deadline-watcher job pattern, disarm_deadline_signal) test workarounds for
// bash job control that the Go runner does not have, and are dropped.

func readPID(t *testing.T, path string) int {
	t.Helper()
	for i := 0; i < 100; i++ {
		if data, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no pid appeared in %s", path)
	return 0
}

func waitDead(t *testing.T, pid int, within time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if !pidAlive(pid) {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return !pidAlive(pid)
}

func TestExecTimeoutKillsAHangingCommand(t *testing.T) {
	start := time.Now()
	res := OSExec{}.Run(context.Background(), Cmd{Name: "sleep", Args: []string{"30"}, Session: true, Timeout: 500 * time.Millisecond, Grace: time.Second})
	if elapsed := time.Since(start); !res.TimedOut || elapsed > 10*time.Second {
		t.Errorf("want TimedOut within 10s, got %+v after %v", res, elapsed)
	}
	if res.ExitCode == 0 {
		t.Errorf("a killed command must not report success: %+v", res)
	}
}

// Non-vacuous: the same command with NO deadline is still running well past the
// bound the previous case used, so that case's early return is the timeout
// firing, not the command finishing on its own.
func TestExecWithoutATimeoutKeepsRunning(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Result, 1)
	go func() {
		done <- OSExec{}.Run(ctx, Cmd{Name: "sleep", Args: []string{"30"}, Session: true, Grace: time.Second})
	}()
	select {
	case res := <-done:
		t.Fatalf("an unbounded sleep 30 finished on its own: %+v", res)
	case <-time.After(1200 * time.Millisecond):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("cancel did not stop the command")
	}
}

func TestExecPassesExitCodesThroughUnmodified(t *testing.T) {
	for _, c := range []struct {
		name string
		args []string
		want int
	}{{"true", nil, 0}, {"false", nil, 1}, {"sh", []string{"-c", "exit 7"}, 7}, {"sh", []string{"-c", "kill -9 $$"}, 128 + 9}} {
		res := OSExec{}.Run(context.Background(), Cmd{Name: c.name, Args: c.args, Session: true, Timeout: 5 * time.Second})
		if res.ExitCode != c.want || res.TimedOut {
			t.Errorf("%s %v: got %+v, want exit %d and no timeout", c.name, c.args, res, c.want)
		}
	}
}

func TestExecNotFoundIs127(t *testing.T) {
	res := OSExec{}.Run(context.Background(), Cmd{Name: "definitely-not-a-real-binary-1994"})
	if res.ExitCode != 127 || res.Err == nil {
		t.Errorf("got %+v", res)
	}
}

func TestExecCapturesOutputUnderADeadline(t *testing.T) {
	var out syncBuf
	res := OSExec{}.Run(context.Background(), Cmd{Name: "sh", Args: []string{"-c", "echo before; sleep 30"}, Stdout: &out, Session: true, Timeout: 700 * time.Millisecond, Grace: time.Second})
	if !res.TimedOut || !strings.Contains(out.String(), "before") {
		t.Errorf("want the pre-kill output captured with the deadline enforced, got %+v out=%q", res, out.String())
	}
}

func TestExecStdoutAndStderrShareOnePipeInOrder(t *testing.T) {
	var out syncBuf
	res := OSExec{}.Run(context.Background(), Cmd{Name: "sh", Args: []string{"-c", "echo a; echo b >&2; echo c"}, Stdout: &out, Stderr: &out})
	if res.ExitCode != 0 || out.String() != "a\nb\nc\n" {
		t.Errorf("2>&1 ordering: got %q %+v", out.String(), res)
	}
}

// with_timeout's group-kill: a timed-out command's own background children are
// reaped, not left running headless.
func TestExecTimeoutReapsBackgroundChildren(t *testing.T) {
	pidfile := t.TempDir() + "/child_pid"
	res := OSExec{}.Run(context.Background(), Cmd{
		Name: "sh", Args: []string{"-c", "sleep 30 & echo $! > " + pidfile + "; wait"},
		Session: true, Timeout: time.Second, Grace: time.Second,
	})
	if !res.TimedOut {
		t.Fatalf("want a timeout, got %+v", res)
	}
	child := readPID(t, pidfile)
	if !waitDead(t, child, 3*time.Second) {
		t.Errorf("child %d still running after its parent's deadline fired — orphan leak", child)
	}
}

// run_reaped's job: cancelling the gate (a signal to the runner) reaps the whole
// session, not just the direct child. The neutralisation twin runs the SAME
// command without Session and shows the grandchild survives, proving the
// session reap is what does the work.
func TestExecCancelReapsTheSession(t *testing.T) {
	run := func(session bool) int {
		pidfile := t.TempDir() + "/child_pid"
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan Result, 1)
		go func() {
			done <- OSExec{}.Run(ctx, Cmd{Name: "sh", Args: []string{"-c", "sleep 30 & echo $! > " + pidfile + "; wait"}, Session: session, Grace: time.Second})
		}()
		child := readPID(t, pidfile)
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Fatal("cancel did not return")
		}
		return child
	}
	child := run(true)
	if !waitDead(t, child, 3*time.Second) {
		t.Errorf("session reap left grandchild %d running", child)
	}
	twin := run(false)
	defer func() {
		if p, err := os.FindProcess(twin); err == nil {
			p.Kill()
		}
	}()
	if waitDead(t, twin, 1500*time.Millisecond) {
		t.Skip("without a session the grandchild was reaped anyway on this platform; the twin cannot discriminate")
	}
}

// #1694: something that outlived `go test` and inherited its output pipe (the
// detached bed) must not wedge the post-suite tail. Real children, real pipe.
func TestExecBoundsAWedgedOutputPipe(t *testing.T) {
	for _, exit := range []int{0, 3} {
		pidfile := t.TempDir() + "/holder_pid"
		var out syncBuf
		start := time.Now()
		res := OSExec{}.Run(context.Background(), Cmd{
			// The "bed": a descendant that outlives its parent holding the write end.
			Name: "sh", Args: []string{"-c", "echo hello; sleep 60 & echo $! > " + pidfile + "; exit " + strconv.Itoa(exit)},
			Stdout: &out, Stderr: &out, Session: true, WaitDelay: 800 * time.Millisecond,
		})
		holder := readPID(t, pidfile)
		elapsed := time.Since(start)
		if !res.PipeWedged {
			t.Errorf("exit %d: the wedge was not detected: %+v", exit, res)
		}
		if res.ExitCode != exit {
			t.Errorf("exit %d: the suite's own exit code must survive an abandoned drain, got %d", exit, res.ExitCode)
		}
		if elapsed > 10*time.Second {
			t.Errorf("exit %d: took %v against a 0.8s bound — the bound is not enforced", exit, elapsed)
		}
		if !strings.Contains(out.String(), "hello") {
			t.Errorf("exit %d: output written before the wedge must survive: %q", exit, out.String())
		}
		if p, err := os.FindProcess(holder); err == nil {
			p.Kill()
		}
	}
}

// Non-vacuous premise for the case above: with NO bound, the same wedge really
// does block (bash: "consumer exited on its own despite a descendant holding
// the fifo open — this case no longer reproduces #1694's mechanism").
func TestExecWedgeReallyBlocksWithoutABound(t *testing.T) {
	pidfile := t.TempDir() + "/holder_pid"
	var out syncBuf
	done := make(chan Result, 1)
	go func() {
		done <- OSExec{}.Run(context.Background(), Cmd{
			Name: "sh", Args: []string{"-c", "echo hello; sleep 60 & echo $! > " + pidfile + "; exit 0"},
			Stdout: &out, Stderr: &out, Session: true, WaitDelay: 0,
		})
	}()
	holder := readPID(t, pidfile)
	select {
	case res := <-done:
		t.Fatalf("an unbounded drain returned despite a descendant holding the pipe: %+v", res)
	case <-time.After(1500 * time.Millisecond):
	}
	if p, err := os.FindProcess(holder); err == nil {
		p.Kill() // releases the pipe, so the unbounded wait ends
	}
	select {
	case res := <-done:
		if res.PipeWedged {
			t.Errorf("EOF arrived, so it was not wedged: %+v", res)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("still blocked after the holder was killed")
	}
}

// The healthy case: a normal drain costs nothing and reports no wedge.
func TestExecHealthyDrainIsPromptAndNotWedged(t *testing.T) {
	var out syncBuf
	start := time.Now()
	res := OSExec{}.Run(context.Background(), Cmd{Name: "sh", Args: []string{"-c", "echo bye"}, Stdout: &out, Session: true, WaitDelay: 30 * time.Second})
	if res.PipeWedged || res.ExitCode != 0 || time.Since(start) > 3*time.Second || out.String() != "bye\n" {
		t.Errorf("got %+v out=%q after %v", res, out.String(), time.Since(start))
	}
}

func TestExecRunsInDirWithEnv(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	res := OSExec{}.Run(context.Background(), Cmd{Name: "sh", Args: []string{"-c", "pwd -P; echo $GATE_TEST_VAR"}, Dir: dir, Env: append(os.Environ(), "GATE_TEST_VAR=hi"), Stdout: &out})
	real := realPath(dir)
	if res.ExitCode != 0 || out.String() != real+"\nhi\n" {
		t.Errorf("got %q %+v (want dir %s)", out.String(), res, real)
	}
}

func TestExecStartDetachesAndWritesToAFile(t *testing.T) {
	logPath := t.TempDir() + "/bed-run.log"
	f, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := (OSExec{}).Start(Cmd{Name: "sh", Args: []string{"-c", "echo Fabrik starting"}, Stdout: f, Stderr: f}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, _ := os.ReadFile(logPath); strings.Contains(string(data), "Fabrik starting") {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Error("the detached process never wrote its banner")
}

func TestExecStartRefusesANonFileStdout(t *testing.T) {
	if err := (OSExec{}).Start(Cmd{Name: "sh", Stdout: &bytes.Buffer{}}); err == nil {
		t.Error("Start must refuse a non-file Stdout")
	}
}
