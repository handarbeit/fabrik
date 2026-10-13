//go:build !windows

package testlife

import (
	"bufio"
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/internal/sessionreap"
	"github.com/handarbeit/fabrik/internal/workerenv"
	"github.com/handarbeit/fabrik/internal/workerowner"
)

// TestMain doubles as the helper process the tests below start: the test binary
// re-execs itself with TESTLIFE_HELPER set, installs Guard exactly as a package
// TestMain would, and then idles forever — only a watchdog can end it.
func TestMain(m *testing.M) {
	switch os.Getenv("TESTLIFE_HELPER") {
	case "guard":
		Guard(helperOptions())
		os.Stdout.WriteString("ready\n")
		for {
			time.Sleep(time.Hour)
		}
	case "parent":
		// Starts a guarded child via ReExecCommand, reports its PID, then idles.
		cmd, _ := ReExecCommand(time.Minute)
		cmd.Env = append(cmd.Env, "TESTLIFE_HELPER=guard", "TESTLIFE_PARENT_POLL=100ms")
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		if err := cmd.Start(); err != nil {
			os.Stderr.WriteString("start: " + err.Error() + "\n")
			os.Exit(3)
		}
		os.Stdout.WriteString(strconv.Itoa(cmd.Process.Pid) + "\n")
		for {
			time.Sleep(time.Hour)
		}
	}
	os.Exit(m.Run())
}

func helperOptions() Options {
	o := Options{Name: "testlife-helper"}
	if d, err := time.ParseDuration(os.Getenv("TESTLIFE_OWNER_POLL")); err == nil {
		o.OwnerPoll = d
	}
	if d, err := time.ParseDuration(os.Getenv("TESTLIFE_PARENT_POLL")); err == nil {
		o.ParentPoll = d
	}
	for _, d := range strings.Split(os.Getenv("TESTLIFE_DISABLE"), ",") {
		switch d {
		case "ceiling":
			o.DisableCeiling = true
		case "owner":
			o.DisableOwnerWatch = true
		case "parent":
			o.DisableParentWatch = true
		}
	}
	return o
}

type helper struct {
	cmd    *exec.Cmd
	stderr *bytes.Buffer
	mu     sync.Mutex
	done   chan struct{}
	err    error
}

func (h *helper) exited(within time.Duration) bool {
	select {
	case <-h.done:
		return true
	case <-time.After(within):
		return false
	}
}

func (h *helper) stderrString() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.stderr.String()
}

type syncBuf struct {
	mu *sync.Mutex
	b  *bytes.Buffer
}

func (s syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }

// startHelper starts the helper in mode with extra env, and waits until it has
// installed its Guard.
func startHelper(t *testing.T, mode string, env ...string) *helper {
	t.Helper()
	h := &helper{stderr: &bytes.Buffer{}, done: make(chan struct{})}
	h.cmd = exec.Command(os.Args[0])
	h.cmd.Env = append(os.Environ(), append([]string{"TESTLIFE_HELPER=" + mode}, env...)...)
	h.cmd.Stderr = syncBuf{&h.mu, h.stderr}
	out, err := h.cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := h.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { h.err = h.cmd.Wait(); close(h.done) }()
	t.Cleanup(func() { _ = h.cmd.Process.Kill(); <-h.done })
	line := make(chan string, 1)
	go func() {
		s, _ := bufio.NewReader(out).ReadString('\n')
		line <- strings.TrimSpace(s)
	}()
	select {
	case l := <-line:
		if mode == "guard" && l != "ready" {
			t.Fatalf("helper said %q, stderr: %s", l, h.stderrString())
		}
		h.cmd.Args = append(h.cmd.Args, l) // keep the first stdout line for "parent" mode
	case <-time.After(10 * time.Second):
		t.Fatal("helper never reported ready")
	}
	return h
}

// standIn starts a process that plays the worker, and an owner file naming it.
func standIn(t *testing.T) (ownerPath string, kill func()) {
	t.Helper()
	cmd := exec.Command("sleep", "120")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-done })
	tok := sessionreap.StartToken(cmd.Process.Pid)
	if tok == "" {
		t.Skip("no start token on this platform")
	}
	path := filepath.Join(t.TempDir(), "worker-owners", "w.json")
	if err := workerowner.Write(path, workerowner.Info{PID: cmd.Process.Pid, StartToken: tok}); err != nil {
		t.Fatal(err)
	}
	return path, func() { _ = cmd.Process.Kill(); <-done }
}

// Acceptance 1: the worker is SIGKILLed; the guarded binary exits within seconds
// (owner poll 100ms here; the production bound is the 5s default plus exit time).
func TestGuard_ExitsWhenOwnerIsKilled(t *testing.T) {
	owner, kill := standIn(t)
	h := startHelper(t, "guard", workerenv.OwnerVar+"="+owner, "TESTLIFE_OWNER_POLL=100ms")
	if h.exited(700 * time.Millisecond) {
		t.Fatalf("helper exited while its owner was alive: %s", h.stderrString())
	}
	kill()
	if !h.exited(10 * time.Second) {
		t.Fatal("helper outlived its owner")
	}
	if !strings.Contains(h.stderrString(), "owning worker") {
		t.Fatalf("exit message does not name the owner: %q", h.stderrString())
	}
}

func TestGuard_NeutralisedOwnerWatchOutlivesOwner(t *testing.T) {
	owner, kill := standIn(t)
	h := startHelper(t, "guard", workerenv.OwnerVar+"="+owner, "TESTLIFE_OWNER_POLL=100ms", "TESTLIFE_DISABLE=owner")
	kill()
	if h.exited(1500 * time.Millisecond) {
		t.Fatal("helper exited with the owner watch neutralised: the owner test is vacuous")
	}
}

// Acceptance 2: the launching shell has exited (the helper is orphaned to init
// by shape), but the worker lives — the binary keeps running.
func TestGuard_SurvivesLaunchingShellWhileOwnerLives(t *testing.T) {
	owner, _ := standIn(t)
	cmd := exec.Command("sh", "-c", `"$0" >/dev/null 2>&1 & echo $!`, os.Args[0])
	cmd.Env = append(os.Environ(), "TESTLIFE_HELPER=guard", workerenv.OwnerVar+"="+owner, "TESTLIFE_OWNER_POLL=100ms")
	out, err := cmd.Output() // returns when the shell has exited
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("pid %q: %v", out, err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	time.Sleep(1500 * time.Millisecond)
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("background run died with its worker alive and its shell gone: %v", err)
	}
}

// A recycled owner PID is gone: the PID is alive but holds a different process.
func TestGuard_ExitsWhenOwnerPIDIsRecycled(t *testing.T) {
	owner, _ := standIn(t)
	info, err := workerowner.Read(owner)
	if err != nil {
		t.Fatal(err)
	}
	info.StartToken += "-recycled"
	if err := workerowner.Write(owner, info); err != nil {
		t.Fatal(err)
	}
	h := startHelper(t, "guard", workerenv.OwnerVar+"="+owner, "TESTLIFE_OWNER_POLL=100ms")
	if !h.exited(10 * time.Second) {
		t.Fatal("helper kept running although the owner's PID is held by a different process")
	}
}

// Acceptance 3: the ceiling names itself in the exit message.
func TestGuard_CeilingExitsWithClearMessage(t *testing.T) {
	h := startHelper(t, "guard", CeilingEnv+"=400ms")
	if !h.exited(10 * time.Second) {
		t.Fatal("helper outlived its ceiling")
	}
	msg := h.stderrString()
	if !strings.Contains(msg, "hard wall-time ceiling of 400ms") || !strings.Contains(msg, CeilingEnv) {
		t.Fatalf("exit message does not name the ceiling: %q", msg)
	}
	var ee *exec.ExitError
	if h.err == nil || !errorsAs(h.err, &ee) || ee.ExitCode() != ExitCode {
		t.Fatalf("exit status = %v, want %d", h.err, ExitCode)
	}
}

func TestGuard_NeutralisedCeilingDoesNotExit(t *testing.T) {
	h := startHelper(t, "guard", CeilingEnv+"=300ms", "TESTLIFE_DISABLE=ceiling")
	if h.exited(1500 * time.Millisecond) {
		t.Fatal("helper exited with the ceiling neutralised: the ceiling test is vacuous")
	}
}

// The re-exec'd helper dies with its parent (the 25664/49066 mechanism: the
// parent test binary was killed and the helpers ran on with no timeout).
func TestReExecCommand_ChildExitsWhenParentIsKilled(t *testing.T) {
	childPID, parent := startParentAndChild(t, "TESTLIFE_UNUSED=1")
	_ = parent.cmd.Process.Kill()
	if !waitGone(childPID, 10*time.Second) {
		t.Fatal("re-exec'd helper outlived its killed parent")
	}
}

func TestReExecCommand_NeutralisedParentWatchLeavesChild(t *testing.T) {
	childPID, parent := startParentAndChild(t, "TESTLIFE_DISABLE=parent")
	_ = parent.cmd.Process.Kill()
	if waitGone(childPID, 1500*time.Millisecond) {
		t.Fatal("child exited with the parent watch neutralised: the parent test is vacuous")
	}
}

func startParentAndChild(t *testing.T, env string) (int, *helper) {
	t.Helper()
	p := startHelper(t, "parent", env)
	childPID, err := strconv.Atoi(p.cmd.Args[len(p.cmd.Args)-1])
	if err != nil {
		t.Fatalf("parent did not report a child pid: %v (%v)", err, p.cmd.Args)
	}
	t.Cleanup(func() { _ = syscall.Kill(childPID, syscall.SIGKILL) })
	time.Sleep(500 * time.Millisecond) // let the child install its Guard
	if err := syscall.Kill(childPID, 0); err != nil {
		t.Fatalf("child not running before the parent was killed: %v", err)
	}
	return childPID, p
}

func waitGone(pid int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
			return true
		}
		// A child orphaned to init may linger as a zombie briefly on some hosts.
		if sessionreap.StartToken(pid) == "" {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return false
}

func TestReExecCommand_SetsTimeoutCeilingAndParent(t *testing.T) {
	cmd, cancel := ReExecCommand(2*time.Minute, "-test.run=^TestX$")
	defer cancel()
	if cmd.Args[0] != os.Args[0] {
		t.Fatalf("argv0 = %q", cmd.Args[0])
	}
	args := strings.Join(cmd.Args[1:], " ")
	if !strings.Contains(args, "-test.run=^TestX$") || !strings.Contains(args, "-test.timeout=2m0s") {
		t.Fatalf("args = %q", args)
	}
	env := strings.Join(cmd.Env, "\n")
	if !strings.Contains(env, CeilingEnv+"=2m30s") {
		t.Fatalf("env missing ceiling: %s", env)
	}
	want := ParentEnv + "=" + strconv.Itoa(os.Getpid()) + ":" + sessionreap.StartToken(os.Getpid())
	if sessionreap.StartToken(os.Getpid()) != "" && !strings.Contains(env, want) {
		t.Fatalf("env missing %s", want)
	}
}

// In-process cases: nothing resolvable, nothing readable -> never exit.
func TestGuard_UnresolvedOrUnreadableOwnerNeverExits(t *testing.T) {
	cases := map[string]Options{
		"no marker": {Getenv: func(string) string { return "" }},
		"file never appears": {
			Getenv: func(k string) string {
				return map[string]string{workerenv.OwnerVar: filepath.Join(t.TempDir(), "nope.json")}[k]
			},
			ResolveWindow: 300 * time.Millisecond,
		},
	}
	// Token unreadable on a live PID: Unknown, not Gone.
	owner, _ := standIn(t)
	cases["token unreadable"] = Options{
		Getenv: func(k string) string { return map[string]string{workerenv.OwnerVar: owner}[k] },
		Start:  func(int) string { return "" },
	}
	for name, o := range cases {
		t.Run(name, func(t *testing.T) {
			exits := make(chan int, 1)
			o.Exit = func(c int) { exits <- c }
			o.Stderr = &bytes.Buffer{}
			o.OwnerPoll = 20 * time.Millisecond
			o.Ceiling = time.Hour
			stop := Guard(o)
			defer stop()
			select {
			case <-exits:
				t.Fatal("Guard exited on an unresolved or unreadable owner")
			case <-time.After(600 * time.Millisecond):
			}
		})
	}
}

func TestCeilingEnvParsing(t *testing.T) {
	var buf bytes.Buffer
	o := Options{Stderr: &buf, Getenv: func(k string) string { return map[string]string{CeilingEnv: "nonsense"}[k] }}
	if got := o.ceiling(); got != DefaultCeiling {
		t.Fatalf("ceiling = %v, want default", got)
	}
	if !strings.Contains(buf.String(), "invalid") {
		t.Fatalf("no warning for a bad %s: %q", CeilingEnv, buf.String())
	}
	o.Getenv = func(k string) string { return map[string]string{CeilingEnv: "90m"}[k] }
	if got := o.ceiling(); got != 90*time.Minute {
		t.Fatalf("ceiling = %v", got)
	}
}

func errorsAs(err error, target **exec.ExitError) bool {
	ee, ok := err.(*exec.ExitError)
	if ok {
		*target = ee
	}
	return ok
}
