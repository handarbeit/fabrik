//go:build !windows

package testlife

import (
	"bufio"
	"bytes"
	"io"
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
		if os.Getenv("TESTLIFE_MAINPARENT") == "1" {
			// Arm the child through os.Getppid instead of an explicit parent.
			var env []string
			for _, e := range cmd.Env {
				if strings.HasPrefix(e, ParentEnv+"=") || strings.HasPrefix(e, workerenv.OwnerVar+"=") {
					continue
				}
				env = append(env, e)
			}
			cmd.Env = env
		}
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
		case "mainparent":
			o.DisableMainParentWatch = true
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
	dump := filepath.Join(t.TempDir(), "dump.txt")
	h := startHelper(t, "guard", CeilingEnv+"=400ms", DumpEnv+"="+dump)
	if !h.exited(10 * time.Second) {
		t.Fatal("helper outlived its ceiling")
	}
	msg := h.stderrString()
	if !strings.Contains(msg, DumpHeaderPrefix) || !strings.Contains(msg, "goroutine dump written to "+dump) {
		t.Fatalf("ceiling exit carries no goroutine dump: %q", msg)
	}
	if b, err := os.ReadFile(dump); err != nil || !strings.Contains(string(b), "goroutine ") {
		t.Fatalf("dump file = %q, %v", b, err)
	}
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

func startParentAndChild(t *testing.T, env ...string) (int, *helper) {
	t.Helper()
	p := startHelper(t, "parent", env...)
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

// recorder is a fake Exit/Kill pair. blockExit makes Exit never return, the
// "exit does not return" of the incident.
type recorder struct {
	exits, kills chan int
	release      chan struct{}
}

func newRecorder(blockExit bool) *recorder {
	r := &recorder{exits: make(chan int, 4), kills: make(chan int, 4), release: make(chan struct{})}
	if !blockExit {
		close(r.release)
	}
	return r
}

func (r *recorder) opts(o Options) Options {
	o.Exit = func(c int) { r.exits <- c; <-r.release }
	o.Kill = func() { r.kills <- 1 }
	o.Stderr = &bytes.Buffer{}
	o.KillGrace = 150 * time.Millisecond
	o.DisableDump = true // the dump tests build their own Options; do not litter the temp dir
	return o
}

func (r *recorder) cleanup(t *testing.T) { t.Cleanup(func() { close(r.release) }) }

// sleeper starts a process and returns its PID and a kill func.
func sleeper(t *testing.T) (int, func()) {
	t.Helper()
	cmd := exec.Command("sleep", "120")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-done })
	if sessionreap.StartToken(cmd.Process.Pid) == "" {
		t.Skip("no start token on this platform")
	}
	return cmd.Process.Pid, func() { _ = cmd.Process.Kill(); <-done }
}

func noEnv(string) string { return "" }

// bailTriggers drive each of the three bail paths with a fake Exit. Each returns
// the Options that make the trigger fire quickly.
func bailTriggers() map[string]func(t *testing.T) Options {
	return map[string]func(t *testing.T) Options{
		"ceiling": func(t *testing.T) Options {
			return Options{Getenv: noEnv, Ceiling: 50 * time.Millisecond, DisableMainParentWatch: true}
		},
		"owner watch": func(t *testing.T) Options {
			owner, _ := standIn(t)
			info, _ := workerowner.Read(owner)
			info.StartToken += "-recycled"
			if err := workerowner.Write(owner, info); t.Failed() || err != nil {
				t.Fatal(err)
			}
			return Options{
				Getenv:    func(k string) string { return map[string]string{workerenv.OwnerVar: owner}[k] },
				OwnerPoll: 20 * time.Millisecond, Ceiling: time.Hour,
			}
		},
		"re-exec parent watch": func(t *testing.T) Options {
			pid, _ := sleeper(t)
			return Options{
				Getenv: func(k string) string {
					return map[string]string{ParentEnv: strconv.Itoa(pid) + ":not-its-token"}[k]
				},
				ParentPoll: 20 * time.Millisecond, Ceiling: time.Hour,
			}
		},
		"main parent watch": func(t *testing.T) Options {
			pid, kill := sleeper(t)
			kill()
			return Options{
				Getenv:  noEnv,
				Getppid: func() int { return pid },
				Start: func(p int) string {
					if p == pid {
						return "tok"
					}
					return sessionreap.StartToken(p)
				},
				ParentPoll: 20 * time.Millisecond, Ceiling: time.Hour,
			}
		},
	}
}

// R3 / FR-007: with an Exit that never returns, every bail path still ends in a
// forced kill, and not before the grace.
func TestBail_ExitThatNeverReturnsIsKilled(t *testing.T) {
	for name, mk := range bailTriggers() {
		t.Run(name, func(t *testing.T) {
			r := newRecorder(true)
			r.cleanup(t)
			stop := Guard(r.opts(mk(t)))
			defer stop()
			select {
			case c := <-r.exits:
				if c != ExitCode {
					t.Fatalf("exit code = %d, want %d", c, ExitCode)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("trigger never fired")
			}
			select {
			case <-r.kills:
			case <-time.After(10 * time.Second):
				t.Fatal("exit never returned and the process was not killed")
			}
		})
	}
}

// FR-008: with the fallback neutralised, the same hang is never killed — the
// test above is non-vacuous.
func TestBail_NeutralisedKillFallbackNeverKills(t *testing.T) {
	r := newRecorder(true)
	r.cleanup(t)
	o := r.opts(bailTriggers()["ceiling"](t))
	o.DisableKillFallback = true
	stop := Guard(o)
	defer stop()
	<-r.exits
	select {
	case <-r.kills:
		t.Fatal("kill fired with the fallback neutralised: the fallback test is vacuous")
	case <-time.After(700 * time.Millisecond):
	}
}

// FR-005: a prompt exit is unchanged, and the fallback never acts.
func TestBail_ReturningExitDisarmsFallback(t *testing.T) {
	r := newRecorder(false)
	var buf bytes.Buffer
	o := r.opts(bailTriggers()["ceiling"](t))
	o.Stderr = &buf
	stop := Guard(o)
	defer stop()
	if c := <-r.exits; c != ExitCode {
		t.Fatalf("exit code = %d", c)
	}
	if !strings.Contains(buf.String(), "hard wall-time ceiling") {
		t.Fatalf("message = %q", buf.String())
	}
	select {
	case <-r.kills:
		t.Fatal("fallback fired although exit returned")
	case <-time.After(600 * time.Millisecond):
	}
}

func TestBail_KillNotBeforeGrace(t *testing.T) {
	r := newRecorder(true)
	r.cleanup(t)
	o := r.opts(bailTriggers()["ceiling"](t))
	o.KillGrace = 600 * time.Millisecond
	stop := Guard(o)
	defer stop()
	<-r.exits
	select {
	case <-r.kills:
		t.Fatal("killed before the grace elapsed")
	case <-time.After(300 * time.Millisecond):
	}
	select {
	case <-r.kills:
	case <-time.After(5 * time.Second):
		t.Fatal("never killed after the grace")
	}
}

func mainParentOpts(getppid func() int, start func(int) string) Options {
	return Options{Getenv: noEnv, Getppid: getppid, Start: start, ParentPoll: 20 * time.Millisecond, Ceiling: time.Hour}
}

// US4: a live parent, an unchanged identity and an unreadable identity never bail.
func TestMainParent_LiveOrUnreadableNeverBails(t *testing.T) {
	pid, _ := sleeper(t)
	var calls int
	var mu sync.Mutex
	cases := map[string]Options{
		"live parent":           mainParentOpts(func() int { return pid }, nil),
		"unreadable at install": mainParentOpts(func() int { return pid }, func(int) string { return "" }),
		"ppid 1":                mainParentOpts(func() int { return 1 }, nil),
		"unreadable later": mainParentOpts(func() int { return pid }, func(p int) string {
			mu.Lock()
			defer mu.Unlock()
			calls++
			if calls == 1 {
				return sessionreap.StartToken(p)
			}
			return ""
		}),
	}
	for name, o := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRecorder(false)
			stop := Guard(r.opts(o))
			defer stop()
			select {
			case <-r.exits:
				t.Fatal("bailed although the parent was alive or unreadable")
			case <-time.After(500 * time.Millisecond):
			}
		})
	}
}

func TestMainParent_GoneParentBails(t *testing.T) {
	pid, kill := sleeper(t)
	r := newRecorder(false)
	var buf bytes.Buffer
	o := r.opts(mainParentOpts(func() int { return pid }, nil))
	o.Stderr = &buf
	stop := Guard(o)
	defer stop()
	select {
	case <-r.exits:
		t.Fatal("bailed with a live parent")
	case <-time.After(300 * time.Millisecond):
	}
	kill()
	select {
	case <-r.exits:
	case <-time.After(10 * time.Second):
		t.Fatal("did not bail after the parent was killed")
	}
	if !strings.Contains(buf.String(), "parent process") {
		t.Fatalf("message = %q", buf.String())
	}
}

// A recycled parent PID (same PID, different start token) is gone.
func TestMainParent_RecycledPIDBails(t *testing.T) {
	pid, _ := sleeper(t)
	var mu sync.Mutex
	first := true
	r := newRecorder(false)
	o := r.opts(mainParentOpts(func() int { return pid }, func(p int) string {
		mu.Lock()
		defer mu.Unlock()
		if first {
			first = false
			return "captured-at-install"
		}
		return sessionreap.StartToken(p)
	}))
	stop := Guard(o)
	defer stop()
	select {
	case <-r.exits:
	case <-time.After(10 * time.Second):
		t.Fatal("a recycled parent PID was mistaken for the parent")
	}
}

// FR-014: neutralising the watch, or an owner marker, leaves a gone parent alone.
func TestMainParent_NeutralisedOrOwnerMarkedDoesNotBail(t *testing.T) {
	for name, mod := range map[string]func(*Options){
		"neutralised": func(o *Options) { o.DisableMainParentWatch = true },
		"owner marker": func(o *Options) {
			o.Getenv = func(k string) string {
				if k == workerenv.OwnerVar {
					return filepath.Join(t.TempDir(), "never.json")
				}
				return ""
			}
			o.ResolveWindow = 50 * time.Millisecond
		},
	} {
		t.Run(name, func(t *testing.T) {
			pid, kill := sleeper(t)
			kill()
			r := newRecorder(false)
			o := mainParentOpts(func() int { return pid }, func(p int) string { return "tok" })
			mod(&o)
			stop := Guard(r.opts(o))
			defer stop()
			select {
			case <-r.exits:
				t.Fatal("bailed: the main parent watch should not be armed")
			case <-time.After(500 * time.Millisecond):
			}
		})
	}
}

// Real processes: killing the process that started a binary ends the binary.
func TestMainParent_ChildExitsWhenParentIsKilled(t *testing.T) {
	childPID, parent := startParentAndChild(t, "TESTLIFE_MAINPARENT=1")
	_ = parent.cmd.Process.Kill()
	if !waitGone(childPID, 10*time.Second) {
		t.Fatal("binary outlived the process that started it")
	}
}

func TestMainParent_NeutralisedLeavesChild(t *testing.T) {
	childPID, parent := startParentAndChild(t, "TESTLIFE_MAINPARENT=1", "TESTLIFE_DISABLE=mainparent")
	_ = parent.cmd.Process.Kill()
	if waitGone(childPID, 1500*time.Millisecond) {
		t.Fatal("child exited with the main parent watch neutralised: the test is vacuous")
	}
}

func TestMainParent_ChildSurvivesLiveParent(t *testing.T) {
	childPID, _ := startParentAndChild(t, "TESTLIFE_MAINPARENT=1")
	if waitGone(childPID, 1500*time.Millisecond) {
		t.Fatal("child exited while its parent was alive")
	}
}

func errorsAs(err error, target **exec.ExitError) bool {
	ee, ok := err.(*exec.ExitError)
	if ok {
		*target = ee
	}
	return ok
}

// dumpOpts is the ceiling trigger with a dump-capable recorder.
func dumpOpts(t *testing.T, r *recorder) (Options, *bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	o := r.opts(bailTriggers()["ceiling"](t))
	o.DisableDump = false
	o.Stderr = &buf
	o.DumpPath = filepath.Join(t.TempDir(), "dump.txt")
	return o, &buf, o.DumpPath
}

// blocked is a goroutine the dump must show, parked in a recognisable function.
func testlifeBlockedGoroutineMarker(release <-chan struct{}) { <-release }

// R3 (#2247): the ceiling writes every goroutine's stack to a file and stderr
// before it exits, and the message names the file.
func TestBail_CeilingWritesGoroutineDumpBeforeExit(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	go testlifeBlockedGoroutineMarker(release)

	r := newRecorder(false)
	o, buf, path := dumpOpts(t, r)
	exited := make(chan struct{})
	inner := o.Exit
	o.Exit = func(c int) {
		// The dump must already be on disk when exit is requested.
		if _, err := os.Stat(path); err != nil {
			t.Errorf("dump not written before exit: %v", err)
		}
		close(exited)
		inner(c)
	}
	stop := Guard(o)
	defer stop()
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		t.Fatal("ceiling never fired")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for name, got := range map[string]string{"file": string(b), "stderr": buf.String()} {
		if !strings.Contains(got, DumpHeaderPrefix) || !strings.Contains(got, "testlifeBlockedGoroutineMarker") {
			t.Errorf("%s lacks the header or the blocked goroutine:\n%.400s", name, got)
		}
	}
	if !strings.Contains(buf.String(), "goroutine dump written to "+path) {
		t.Errorf("message does not name the dump path: %q", buf.String()[:min(len(buf.String()), 400)])
	}
}

// The neutralisation seam: with the dump disabled nothing is written, so the
// test above is non-vacuous.
func TestBail_DisableDumpWritesNothing(t *testing.T) {
	r := newRecorder(false)
	o, buf, path := dumpOpts(t, r)
	o.DisableDump = true
	stop := Guard(o)
	defer stop()
	<-r.exits
	if _, err := os.Stat(path); err == nil {
		t.Fatal("dump file written with the dump disabled")
	}
	if strings.Contains(buf.String(), DumpHeaderPrefix) {
		t.Fatal("dump on stderr with the dump disabled")
	}
}

// A dump that does not finish inside its budget is skipped; exit still happens
// and the self-kill grace is not extended.
func TestBail_SlowDumpIsSkippedAndExitStillHappens(t *testing.T) {
	r := newRecorder(false)
	o, buf, path := dumpOpts(t, r)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	o.Profile = func(io.Writer) error { <-release; return nil }
	o.DumpBudget = 50 * time.Millisecond
	o.KillGrace = time.Second
	stop := Guard(o)
	defer stop()
	select {
	case <-r.exits:
	case <-time.After(10 * time.Second):
		t.Fatal("a stuck dump blocked the exit")
	}
	if !strings.Contains(buf.String(), "goroutine dump skipped") {
		t.Fatalf("no skip note: %q", buf.String())
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("dump file written although the profile never finished")
	}
}

// An unwritable path is reported and ignored.
func TestBail_UnwritableDumpPathStillExits(t *testing.T) {
	r := newRecorder(false)
	o, buf, _ := dumpOpts(t, r)
	o.DumpPath = filepath.Join(t.TempDir(), "no-such-dir", "dump.txt")
	stop := Guard(o)
	defer stop()
	select {
	case <-r.exits:
	case <-time.After(10 * time.Second):
		t.Fatal("exit never requested")
	}
	if !strings.Contains(buf.String(), "goroutine dump not written") {
		t.Fatalf("failure not reported: %q", buf.String())
	}
	if !strings.Contains(buf.String(), DumpHeaderPrefix) {
		t.Fatal("stderr dump should still be attempted")
	}
}

// The default path is in the temp dir, never the working directory, and DumpEnv
// overrides it.
func TestDumpPath_DefaultsOutsideWorkingDirectory(t *testing.T) {
	o := Options{Getenv: noEnv, Name: "tests/sim"}
	got := o.dumpPath()
	if filepath.Dir(got) != filepath.Clean(os.TempDir()) {
		t.Fatalf("default dump path %q is not in %q", got, os.TempDir())
	}
	if !strings.Contains(filepath.Base(got), "tests_sim-goroutines-") {
		t.Fatalf("unexpected name %q", got)
	}
	o.Getenv = func(k string) string { return map[string]string{DumpEnv: "/x/y.txt"}[k] }
	if o.dumpPath() != "/x/y.txt" {
		t.Fatal("DumpEnv ignored")
	}
}
