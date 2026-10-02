package gate

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/handarbeit/fabrik/internal/sessionreap"
)

// Cmd describes one subprocess. Everything the gate runs goes through a
// Commander so tests can substitute stubs.
type Cmd struct {
	Name string
	Args []string
	Dir  string
	// Env is the FULL child environment; nil means inherit the gate's own.
	Env []string

	Stdout io.Writer // nil discards
	Stderr io.Writer // nil discards

	// Session runs the child in a session of its own (so its PID is its SID)
	// and reaps the whole session — including the command sessions Claude-style
	// tooling setsid()s into — when ctx is cancelled or Timeout expires. It
	// replaces bash's run_reaped / with_timeout process-group kills (#1989's
	// internal/sessionreap is the shared helper). Used for anything that can
	// fork a tree: go test, gh, the pre-gate. Short git/build commands run in the
	// gate's own session so a tty prompt (ssh passphrase) still works.
	Session bool
	// Timeout kills the child (and, with Session, its tree) after this long;
	// 0 means no deadline. Result.TimedOut reports it.
	Timeout time.Duration
	// WaitDelay bounds how long Run waits for the child's output pipes to close
	// after the child itself has exited (#1694): a descendant that outlived
	// `go test` and inherited the write end — the detached bed was the real-world
	// case — would otherwise wedge the post-suite tail forever. Past it Run
	// stops waiting and reports Result.PipeWedged. 0 means wait for EOF
	// unboundedly.
	WaitDelay time.Duration
	// Grace is the SIGTERM → SIGKILL window when reaping; 0 uses 10s.
	Grace time.Duration
}

// Result is how a Cmd ended.
type Result struct {
	// ExitCode follows shell convention: the child's own code, 128+signum if it
	// was killed by a signal, 127 if it could not be started.
	ExitCode int
	// TimedOut: Cmd.Timeout expired and the child was killed (bash returned 124).
	TimedOut bool
	// PipeWedged: the child exited but something that inherited its output pipe
	// kept it open past WaitDelay, so Run stopped waiting for EOF.
	PipeWedged bool
	// Err is set when the child could not be started or Wait failed oddly.
	Err error
}

// Commander runs subprocesses.
type Commander interface {
	// Run starts c, waits for it, and reports how it ended. It honours ctx:
	// cancellation reaps the child.
	Run(ctx context.Context, c Cmd) Result
	// Start launches c detached — its own session, no Wait, surviving the gate
	// (the bed engine). The child's Stdout/Stderr must be *os.File or nil.
	Start(c Cmd) error
}

// OSExec is the real Commander.
type OSExec struct {
	// Log, if set, receives sessionreap's own diagnostics.
	Log sessionreap.Logger
}

const defaultGrace = 10 * time.Second

func (o OSExec) Run(ctx context.Context, c Cmd) Result {
	cmd := exec.Command(c.Name, c.Args...)
	cmd.Dir = c.Dir
	cmd.Env = c.Env

	// A non-*os.File writer is fed through a pipe we own and copy ourselves,
	// rather than exec's own copier goroutines, so that "the child has exited
	// but something still holds the pipe" is observable and bounded (#1694)
	// whatever the child's exit status was: exec.Cmd.WaitDelay reports it only
	// for a SUCCESSFUL exit.
	var copiers []*pipeCopy
	var parentEnds []*os.File
	attach := func(dst io.Writer) any {
		if dst == nil {
			return nil
		}
		if f, ok := dst.(*os.File); ok {
			return f
		}
		pc, err := startCopy(dst)
		if err != nil {
			return nil
		}
		copiers = append(copiers, pc)
		parentEnds = append(parentEnds, pc.w)
		return pc.w
	}
	closeParentEnds := func() {
		for _, f := range parentEnds {
			f.Close()
		}
	}
	stdoutEnd := attach(c.Stdout)
	var stderrEnd any
	if sameWriter(c.Stdout, c.Stderr) {
		stderrEnd = stdoutEnd // 2>&1: one pipe keeps the two streams in order
	} else {
		stderrEnd = attach(c.Stderr)
	}
	if f, ok := stdoutEnd.(*os.File); ok {
		cmd.Stdout = f
	}
	if f, ok := stderrEnd.(*os.File); ok {
		cmd.Stderr = f
	}
	if c.Session {
		setSession(cmd)
	}
	if err := cmd.Start(); err != nil {
		closeParentEnds()
		for _, pc := range copiers {
			pc.finish(0)
		}
		return Result{ExitCode: 127, Err: err}
	}
	// The child holds its own copy of each write end now; ours must close or the
	// copiers never see EOF.
	closeParentEnds()

	opts := sessionreap.Options{Log: o.Log}
	var tracker *sessionreap.Tracker
	var stopTrack context.CancelFunc
	if c.Session {
		tracker = sessionreap.Track(cmd.Process.Pid, opts, nil)
		var tctx context.Context
		tctx, stopTrack = context.WithCancel(context.Background())
		go tracker.Run(tctx, 2*time.Second)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var timer <-chan time.Time
	if c.Timeout > 0 {
		t := time.NewTimer(c.Timeout)
		defer t.Stop()
		timer = t.C
	}

	var res Result
	var waitErr error
	select {
	case waitErr = <-done:
	case <-ctx.Done():
		o.kill(cmd, c, "context cancelled")
		waitErr = <-done
	case <-timer:
		res.TimedOut = true
		o.kill(cmd, c, "timeout")
		waitErr = <-done
	}
	if stopTrack != nil {
		stopTrack()
		tracker.Close()
	}

	for _, pc := range copiers {
		if pc.finish(c.WaitDelay) {
			res.PipeWedged = true
		}
	}
	res.ExitCode = exitCodeOf(cmd, waitErr)
	return res
}

// pipeCopy copies the read end of a pipe to dst until EOF.
type pipeCopy struct {
	r, w *os.File
	done chan struct{}
}

func startCopy(dst io.Writer) (*pipeCopy, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	pc := &pipeCopy{r: r, w: w, done: make(chan struct{})}
	go func() {
		defer close(pc.done)
		_, _ = io.Copy(dst, r)
	}()
	return pc, nil
}

// finish waits for EOF — up to delay when delay > 0 — and reports whether it
// had to give up (some descendant that outlived the child still holds the write
// end). Giving up closes the read end, so nothing is left blocked on it.
func (pc *pipeCopy) finish(delay time.Duration) (wedged bool) {
	if delay <= 0 {
		<-pc.done
		pc.r.Close()
		return false
	}
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-pc.done:
	case <-t.C:
		wedged = true
		pc.r.Close()
		<-pc.done
	}
	pc.r.Close()
	return wedged
}

func sameWriter(a, b io.Writer) (same bool) {
	defer func() {
		if recover() != nil {
			same = false
		}
	}()
	return a != nil && b != nil && a == b
}

// kill reaps a child that must stop now. With Session the whole session goes
// (SIGTERM → grace → SIGKILL; bash's with_timeout/run_reaped sent only TERM,
// the SIGKILL backstop and the command-session reach are deliberate deltas,
// recorded in ADR-1994); without it just the child.
func (o OSExec) kill(cmd *exec.Cmd, c Cmd, reason string) {
	grace := c.Grace
	if grace == 0 {
		grace = defaultGrace
	}
	if c.Session {
		sessionreap.Escalate(cmd.Process.Pid, reason, 0, grace, sessionreap.Options{Log: o.Log})
		return
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	go func() {
		time.Sleep(grace)
		_ = cmd.Process.Kill()
	}()
}

func (o OSExec) Start(c Cmd) error {
	cmd := exec.Command(c.Name, c.Args...)
	cmd.Dir = c.Dir
	cmd.Env = c.Env
	if f, ok := c.Stdout.(*os.File); ok {
		cmd.Stdout = f
	} else if c.Stdout != nil {
		return fmt.Errorf("Start: Stdout must be an *os.File")
	}
	if f, ok := c.Stderr.(*os.File); ok {
		cmd.Stderr = f
	} else if c.Stderr != nil {
		return fmt.Errorf("Start: Stderr must be an *os.File")
	}
	setSession(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// exitCodeOf converts a Wait result to a shell-style exit code.
func exitCodeOf(cmd *exec.Cmd, waitErr error) int {
	if cmd.ProcessState == nil {
		if waitErr != nil {
			return 127
		}
		return 0
	}
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return cmd.ProcessState.ExitCode()
}

// output runs c and returns its stdout and stderr and exit code — the shape
// bash `$(cmd)` had. Used for short probes (git, pgrep, lsof, gh).
func output(ctx context.Context, x Commander, c Cmd) (stdout, stderr string, res Result) {
	var so, se bytes.Buffer
	if c.Stdout == nil {
		c.Stdout = &so
	}
	if c.Stderr == nil {
		c.Stderr = &se
	}
	res = x.Run(ctx, c)
	return so.String(), se.String(), res
}

// syncBuf is a goroutine-safe buffer (Run's output copiers run concurrently).
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
