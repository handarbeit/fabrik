// Package testlife bounds the lifetime of a test binary (#2128, ADR-2128), so a
// `sim.test` that outlives the run that started it cannot burn CPU for hours —
// one ran 32 hours — and so every wait is by positive evidence, never by
// process-tree shape.
//
// Guard, called first thing in a package's TestMain, starts three independent
// mechanisms:
//
//   - Ceiling (R2): after a bounded wall time the binary exits with a message
//     naming the ceiling. It ignores -test.timeout, so a timeout of 0 or a huge
//     one cannot defeat it.
//   - Owner watch (R1): when FABRIK_WORKER_OWNER names an owner file (the engine
//     sets it for every Claude worker, and every descendant inherits it), the
//     binary exits once the recorded worker is provably gone — its PID absent,
//     or held by a process with a different start token (a recycled PID). It
//     never exits on parent death: a `go test -c` binary run directly, or a
//     background run whose launching shell exited, legitimately has ppid 1.
//     Anything unresolved or unreadable means "keep running".
//   - Parent watch: when SIM_TEST_PARENT=<pid>:<start token> is set — only by
//     ReExecCommand, for a helper the test itself re-execs — the helper exits
//     once that parent is gone. This covers the re-exec'd helpers that survived
//     a `go test -timeout` panic of their parent.
//
// The package is a leaf: stdlib plus internal/sessionreap, internal/workerenv
// and internal/workerowner. It never signals another process.
package testlife

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/handarbeit/fabrik/internal/sessionreap"
	"github.com/handarbeit/fabrik/internal/workerenv"
	"github.com/handarbeit/fabrik/internal/workerowner"
)

const (
	// DefaultCeiling is the hard wall-time ceiling of a test binary. It is
	// sized well above the slowest legitimate full run: `go test -race
	// ./tests/sim/` measured ~11 minutes at -parallel 4 on a 28-core host,
	// scripts/sim/run.sh allows SIM_TIMEOUT=20m, and host load of 34-41 stretched
	// runs further. 60 minutes keeps a healthy run clear of it while making a
	// 32-hour orphan impossible.
	DefaultCeiling = 60 * time.Minute

	// CeilingEnv overrides the ceiling (a time.Duration string, e.g. "90m").
	CeilingEnv = "SIM_TEST_CEILING"

	// ParentEnv is set by ReExecCommand to "<pid>:<start token>" of the
	// re-executing parent.
	ParentEnv = "SIM_TEST_PARENT"

	// ExitCode is the process exit status for every watchdog exit.
	ExitCode = 2

	defaultOwnerPoll    = 5 * time.Second
	defaultParentPoll   = time.Second
	defaultResolveEvery = 500 * time.Millisecond
	defaultResolveFor   = 30 * time.Second
)

// Options configures Guard. The zero value is production behaviour.
type Options struct {
	// Name prefixes every message, e.g. "tests/sim".
	Name string

	Ceiling       time.Duration // 0: CeilingEnv, else DefaultCeiling
	OwnerPoll     time.Duration // 0: 5s; worst-case exit latency after the worker dies is this plus exit time
	ParentPoll    time.Duration // 0: 1s
	ResolveWindow time.Duration // 0: 30s; how long to wait for the engine to write the owner file

	// Seams. Zero values use the real process, environment and clock.
	Getenv func(string) string
	Exit   func(code int)
	Stderr io.Writer
	Start  func(pid int) string // process start-time token; nil: sessionreap.StartToken

	// Neutralisation seams: a test sets one to prove the mechanism it covers is
	// load-bearing. Production never sets them.
	DisableCeiling     bool
	DisableOwnerWatch  bool
	DisableParentWatch bool
}

func (o Options) getenv(k string) string {
	if o.Getenv != nil {
		return o.Getenv(k)
	}
	return os.Getenv(k)
}

func (o Options) stderr() io.Writer {
	if o.Stderr != nil {
		return o.Stderr
	}
	return os.Stderr
}

func (o Options) name() string {
	if o.Name != "" {
		return o.Name
	}
	return "test binary"
}

func dur(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

func (o Options) ceiling() time.Duration {
	if o.Ceiling > 0 {
		return o.Ceiling
	}
	if v := strings.TrimSpace(o.getenv(CeilingEnv)); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
		fmt.Fprintf(o.stderr(), "%s: ignoring invalid %s=%q; using %s\n", o.name(), CeilingEnv, v, DefaultCeiling)
	}
	return DefaultCeiling
}

// Guard starts the watchdogs and returns a stop func that ends them (a TestMain
// that exits via os.Exit need not call it). It returns immediately.
func Guard(o Options) (stop func()) {
	exit := o.Exit
	if exit == nil {
		exit = os.Exit
	}
	var once sync.Once
	bail := func(format string, args ...any) {
		once.Do(func() {
			fmt.Fprintf(o.stderr(), "%s: %s\n", o.name(), fmt.Sprintf(format, args...))
			exit(ExitCode)
		})
	}
	done := make(chan struct{})
	var stopOnce sync.Once
	stop = func() { stopOnce.Do(func() { close(done) }) }

	if !o.DisableCeiling {
		c := o.ceiling()
		go func() {
			t := time.NewTimer(c)
			defer t.Stop()
			select {
			case <-t.C:
				bail("test binary exceeded its hard wall-time ceiling of %s (override with %s); exiting", c, CeilingEnv)
			case <-done:
			}
		}()
	}
	if !o.DisableOwnerWatch {
		if path := o.getenv(workerenv.OwnerVar); path != "" {
			go watchOwner(o, path, done, bail)
		}
	}
	if !o.DisableParentWatch {
		if pid, tok, ok := parseParent(o.getenv(ParentEnv)); ok {
			go watchIdentity(o, workerowner.Info{PID: pid, StartToken: tok}, "re-exec parent", dur(o.ParentPoll, defaultParentPoll), done, bail)
		}
	}
	return stop
}

func parseParent(v string) (pid int, tok string, ok bool) {
	p, t, found := strings.Cut(v, ":")
	if !found || t == "" {
		return 0, "", false
	}
	n, err := strconv.Atoi(p)
	if err != nil || n <= 1 {
		return 0, "", false
	}
	return n, t, true
}

// watchOwner resolves the owner file — the engine writes it just after starting
// the worker, so it may not exist yet — then watches the identity it names. An
// unresolvable file means no owner watch, never an exit.
func watchOwner(o Options, path string, done <-chan struct{}, bail func(string, ...any)) {
	every := defaultResolveEvery
	if p := dur(o.OwnerPoll, defaultOwnerPoll); p < every {
		every = p
	}
	deadline := time.Now().Add(dur(o.ResolveWindow, defaultResolveFor))
	for {
		info, err := workerowner.Read(path)
		if err == nil {
			watchIdentity(o, info, "owning worker", dur(o.OwnerPoll, defaultOwnerPoll), done, bail)
			return
		}
		if time.Now().After(deadline) {
			return
		}
		select {
		case <-done:
			return
		case <-time.After(every):
		}
	}
}

// watchIdentity polls info and exits once it is provably gone. Alive and Unknown
// both keep the binary running.
func watchIdentity(o Options, info workerowner.Info, what string, poll time.Duration, done <-chan struct{}, bail func(string, ...any)) {
	opts := sessionreap.Options{Start: o.Start}
	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		if workerowner.Check(info, opts) == workerowner.Gone {
			bail("%s (pid %d) is gone; exiting", what, info.PID)
			return
		}
		select {
		case <-done:
			return
		case <-t.C:
		}
	}
}

// ReExecCommand returns a command that re-executes the current test binary with
// args, bounded three ways so it cannot outlive its parent test: an explicit
// -test.timeout (a re-exec'd binary otherwise has none — go test passes one only
// to the binary it starts), a context deadline, and SIM_TEST_PARENT naming this
// process so the child's Guard exits when the parent is gone. The returned
// cancel must be called when the command is done.
func ReExecCommand(timeout time.Duration, args ...string) (*exec.Cmd, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	full := append(append([]string(nil), args...), "-test.timeout="+timeout.String())
	cmd := exec.CommandContext(ctx, os.Args[0], full...)
	cmd.WaitDelay = 5 * time.Second
	env := append([]string(nil), os.Environ()...)
	// The child's own ceiling is a little past its timeout, so the ordinary
	// -test.timeout panic stays the first thing to fire.
	env = append(env, CeilingEnv+"="+(timeout+30*time.Second).String())
	if tok := sessionreap.StartToken(os.Getpid()); tok != "" {
		env = append(env, ParentEnv+"="+strconv.Itoa(os.Getpid())+":"+tok)
	}
	cmd.Env = env
	return cmd, cancel
}
