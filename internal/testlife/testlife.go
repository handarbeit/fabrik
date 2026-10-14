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
//     never infers death from process-tree shape (ppid 1): a worker-launched
//     `go test -c` binary whose shell exited legitimately has ppid 1.
//     Anything unresolved or unreadable means "keep running".
//   - Parent watch: when SIM_TEST_PARENT=<pid>:<start token> is set — only by
//     ReExecCommand, for a helper the test itself re-execs — the helper exits
//     once that parent is gone. This covers the re-exec'd helpers that survived
//     a `go test -timeout` panic of their parent.
//   - Main parent watch (#2226): a binary with no explicit parent and no owner
//     marker watches the process that started it (PID plus start token captured
//     at install), so a `sim.test` reparented to init when its `go test` is gone
//     exits too. An owner marker takes precedence — the owner watch is then the
//     authoritative lifetime — and an unreadable identity means "keep running".
//
// All three share one bail path with a bounded exit (#2226): before it asks the
// process to exit it arms a watchdog that SIGKILLs the process itself after
// KillGrace, so an exit that blocks or spins (seen under -race) cannot leave an
// orphan.
//
// Goroutine dump (#2247): after the watchdog is armed and the reason printed,
// the bail path writes every goroutine's stack (runtime/pprof goroutine profile,
// debug=2) to a file (DumpEnv, else the temp directory — never the working
// directory, which is the package source tree) and then to stderr, so a hang and
// a merely slow run no longer look the same. The dump is built in its own
// goroutine and waited for at most DumpBudget (inside KillGrace): a stop-the-world
// profile under a stuck runtime is skipped, never allowed to delay the exit. A
// failed dump is reported and ignored.
//
// The package is a leaf: stdlib plus internal/sessionreap, internal/workerenv
// and internal/workerowner. It never signals another process.
package testlife

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/handarbeit/fabrik/internal/sessionreap"
	"github.com/handarbeit/fabrik/internal/workerenv"
	"github.com/handarbeit/fabrik/internal/workerowner"
)

const (
	// DefaultCeiling is the hard wall-time ceiling of a test binary. It bounds
	// a runaway, so it is no longer than it needs to be: an hour of a runaway
	// sim.test at ~80% CPU is the host-load damage this guard exists to prevent.
	// It still sits well clear of a healthy run: `go test -race ./tests/sim/`
	// measured ~11 minutes at -parallel 4 on a 28-core host, CI's full suite
	// takes ~7-8 minutes, and scripts/sim/run.sh allows SIM_TIMEOUT=20m, so an
	// ordinary -test.timeout panic fires first and keeps its goroutine dump.
	// 30 minutes also makes a 32-hour orphan impossible. SIM_TEST_CEILING
	// overrides it for a deliberately long run.
	DefaultCeiling = 30 * time.Minute

	// CeilingEnv overrides the ceiling (a time.Duration string, e.g. "90m").
	CeilingEnv = "SIM_TEST_CEILING"

	// ParentEnv is set by ReExecCommand to "<pid>:<start token>" of the
	// re-executing parent.
	ParentEnv = "SIM_TEST_PARENT"

	// DumpEnv names the file the goroutine dump is written to when the bail path
	// fires (#2247); the process ID is inserted before its extension so binaries
	// sharing one value do not overwrite each other. Unset: a file in the temp
	// directory.
	DumpEnv = "SIM_TEST_DUMP_FILE"

	// DumpHeaderPrefix begins the first line of every dump, so a reader of the
	// output stream can find it.
	DumpHeaderPrefix = "=== testlife goroutine dump"

	// ExitCode is the process exit status for every watchdog exit.
	ExitCode = 2

	defaultOwnerPoll    = 5 * time.Second
	defaultParentPoll   = time.Second
	defaultKillGrace    = 5 * time.Second
	defaultResolveEvery = 500 * time.Millisecond
	defaultResolveFor   = 30 * time.Second
	defaultDumpBudget   = 3 * time.Second
)

// Options configures Guard. The zero value is production behaviour.
type Options struct {
	// Name prefixes every message, e.g. "tests/sim".
	Name string

	Ceiling       time.Duration // 0: CeilingEnv, else DefaultCeiling
	OwnerPoll     time.Duration // 0: 5s; worst-case exit latency after the worker dies is this plus exit time
	ParentPoll    time.Duration // 0: 1s
	ResolveWindow time.Duration // 0: 30s; how long to wait for the engine to write the owner file

	KillGrace time.Duration // 0: 5s; how long exit may take before the process kills itself

	DumpPath   string        // "": DumpEnv with the PID inserted before its extension, else <tmp>/<name>-goroutines-<pid>.txt
	DumpBudget time.Duration // 0: 3s (never more than 3/5 of KillGrace); how long the bail path waits for the dump

	// Seams. Zero values use the real process, environment and clock.
	Getenv  func(string) string
	Exit    func(code int)
	Kill    func()     // forced termination of this process; nil: SIGKILL to os.Getpid()
	Getppid func() int // nil: os.Getppid
	Stderr  io.Writer
	Start   func(pid int) string  // process start-time token; nil: sessionreap.StartToken
	Profile func(io.Writer) error // writes the goroutine dump; nil: pprof goroutine profile, debug=2

	// Neutralisation seams: a test sets one to prove the mechanism it covers is
	// load-bearing. Production never sets them.
	DisableCeiling     bool
	DisableOwnerWatch  bool
	DisableParentWatch bool

	DisableKillFallback    bool
	DisableMainParentWatch bool
	DisableDump            bool
}

// selfKill terminates this process and only this process: os.Process.Kill is
// SIGKILL on unix and TerminateProcess on Windows, and the PID is our own.
func selfKill() {
	if p, err := os.FindProcess(os.Getpid()); err == nil {
		_ = p.Kill()
	}
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

// dumpPath is where the goroutine dump goes. Never the working directory: a
// test binary's cwd is its package directory, and a file there would dirty the
// tree the gate records a pass against.
func (o Options) dumpPath() string {
	if o.DumpPath != "" {
		return o.DumpPath
	}
	if v := strings.TrimSpace(o.getenv(DumpEnv)); v != "" {
		// `go test ./...` runs several test binaries at once, all inheriting this
		// one path: a shared name would let the second hung binary truncate the
		// first's dump. The PID goes before the extension, so every binary keeps
		// its own file and the gate (which reads the "written to" line) finds each.
		ext := filepath.Ext(v)
		return fmt.Sprintf("%s.%d%s", strings.TrimSuffix(v, ext), os.Getpid(), ext)
	}
	name := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.':
			return r
		}
		return '_'
	}, o.name())
	return filepath.Join(os.TempDir(), fmt.Sprintf("%s-goroutines-%d.txt", name, os.Getpid()))
}

// dumpBudget is how long the bail path waits for the dump. It stays inside the
// self-kill grace so the dump can never extend it.
func (o Options) dumpBudget() time.Duration {
	b := dur(o.DumpBudget, defaultDumpBudget)
	if !o.DisableKillFallback {
		if max := dur(o.KillGrace, defaultKillGrace) * 3 / 5; b > max {
			b = max
		}
	}
	return b
}

// dump writes the goroutine dump to a file and then stderr (#2247). Every
// failure is reported and swallowed: the caller is about to exit and must.
func (o Options) dump(reason string) {
	if o.DisableDump {
		return
	}
	path := o.dumpPath()
	profile := o.Profile
	if profile == nil {
		profile = func(w io.Writer) error {
			p := pprof.Lookup("goroutine")
			if p == nil {
				return errors.New("no goroutine profile")
			}
			return p.WriteTo(w, 2)
		}
	}
	type result struct {
		data []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		var buf bytes.Buffer
		fmt.Fprintf(&buf, "%s (%s) pid %d: %s ===\n", DumpHeaderPrefix, o.name(), os.Getpid(), reason)
		err := profile(&buf)
		ch <- result{buf.Bytes(), err}
	}()
	budget := time.NewTimer(o.dumpBudget())
	defer budget.Stop()
	var r result
	select {
	case r = <-ch:
	case <-budget.C:
		fmt.Fprintf(o.stderr(), "%s: goroutine dump skipped: not produced within %s\n", o.name(), o.dumpBudget())
		return
	}
	if r.err != nil {
		fmt.Fprintf(o.stderr(), "%s: goroutine dump incomplete: %v\n", o.name(), r.err)
	}
	if err := os.WriteFile(path, r.data, 0o644); err != nil {
		fmt.Fprintf(o.stderr(), "%s: goroutine dump not written to %s: %v\n", o.name(), path, err)
	} else {
		fmt.Fprintf(o.stderr(), "%s: goroutine dump written to %s\n", o.name(), path)
	}
	// File first: stderr may be a broken pipe once the parent is gone.
	_, _ = o.stderr().Write(r.data) // a broken pipe changes nothing: we are exiting
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
			// Armed before anything that can block (the message, the exit), so a
			// hang in either cannot stop the process being killed (#2226). Only a
			// fake exit returns; that disarms it.
			var wd *time.Timer
			if !o.DisableKillFallback {
				kill := o.Kill
				if kill == nil {
					kill = selfKill
				}
				wd = time.AfterFunc(dur(o.KillGrace, defaultKillGrace), kill)
			}
			reason := fmt.Sprintf(format, args...)
			fmt.Fprintf(o.stderr(), "%s: %s\n", o.name(), reason)
			o.dump(reason)
			exit(ExitCode)
			if wd != nil {
				wd.Stop()
			}
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
	if info, ok := mainParent(o); ok {
		go watchIdentity(o, info, "parent process", dur(o.ParentPoll, defaultParentPoll), done, bail)
	}
	return stop
}

// mainParent returns the identity of the process that started this binary, when
// the main parent watch applies: not neutralised, no explicit ReExecCommand
// parent (that watch covers it), no owner marker (the owner watch is then the
// authoritative lifetime), and a parent that is not init with a readable start
// token. Anything else means no watch, never an exit.
func mainParent(o Options) (workerowner.Info, bool) {
	if o.DisableMainParentWatch || o.getenv(workerenv.OwnerVar) != "" {
		return workerowner.Info{}, false
	}
	if _, _, ok := parseParent(o.getenv(ParentEnv)); ok {
		return workerowner.Info{}, false
	}
	getppid := o.Getppid
	if getppid == nil {
		getppid = os.Getppid
	}
	ppid := getppid()
	if ppid <= 1 {
		return workerowner.Info{}, false
	}
	start := o.Start
	if start == nil {
		start = sessionreap.StartToken
	}
	tok := start(ppid)
	if tok == "" {
		return workerowner.Info{}, false
	}
	return workerowner.Info{PID: ppid, StartToken: tok}, true
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
