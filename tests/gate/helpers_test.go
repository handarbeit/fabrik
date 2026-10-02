package gate

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeExec is a Commander whose behaviour a test scripts. It replaces the
// bash tests' PATH-shadowed fake `go` and stubbed functions.
type fakeExec struct {
	mu      sync.Mutex
	calls   []Cmd
	handler func(ctx context.Context, c Cmd) Result
	started []Cmd
}

func (f *fakeExec) Run(ctx context.Context, c Cmd) Result {
	f.mu.Lock()
	f.calls = append(f.calls, c)
	h := f.handler
	f.mu.Unlock()
	if h == nil {
		return Result{}
	}
	return h(ctx, c)
}

func (f *fakeExec) Start(c Cmd) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, c)
	return nil
}

// callsNamed returns the recorded calls to the named program.
func (f *fakeExec) callsNamed(name string) []Cmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Cmd
	for _, c := range f.calls {
		if c.Name == name {
			out = append(out, c)
		}
	}
	return out
}

// argsLine is the call as one string, for matching.
func argsLine(c Cmd) string { return c.Name + " " + strings.Join(c.Args, " ") }

func (f *fakeExec) lines() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		out = append(out, argsLine(c))
	}
	return out
}

func writeStdout(c Cmd, s string) {
	if c.Stdout != nil {
		io.WriteString(c.Stdout, s)
	}
}

func writeStderr(c Cmd, s string) {
	if c.Stderr != nil {
		io.WriteString(c.Stderr, s)
	}
}

// testGate builds a Gate over a temp bed with a fake Commander, a fixed
// environment (never the real one — these tests must not read E2E_*/FABRIK_*
// from the developer's shell) and captured output.
func testGate(t *testing.T, env ...string) (*Gate, *fakeExec, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	root := t.TempDir()
	bed := t.TempDir()
	fe := &fakeExec{}
	var out, errb bytes.Buffer
	cfg := Config{
		RepoRoot: root, TestBed: bed, EngineLog: bed + "/.fabrik/fabrik.log",
		PrueferDir: t.TempDir() + "/none", Timeout: "4h", Parallel: "4", ParallelOn: "2", BedPollSeconds: "60",
		GHAPITimeout: 30 * time.Second, PostSuiteDrainTimeout: 30 * time.Second, PostSuiteWatchdog: 300 * time.Second,
		StallWarn: 15 * time.Minute, StallCheckInterval: time.Hour,
		BannerWait: 3 * time.Second, StopWait: 3 * time.Second, KillGrace: time.Second,
		TmpDir: t.TempDir(),
	}
	g := &Gate{
		Cfg: cfg, Out: &out, Err: &errb, Exec: fe,
		Env:   append([]string{"HOME=" + t.TempDir()}, env...),
		Sleep: func(time.Duration) {}, Now: time.Now, Self: os.Getpid(),
		ProcCwd: func(context.Context, int) string { return "" },
	}
	g.Scheduler = SerialScheduler{}
	g.Preflights = DefaultPreflights()
	return g, fe, &out, &errb
}

func skipIfNoGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(dirOf(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func dirOf(p string) string {
	if i := strings.LastIndexByte(p, '/'); i > 0 {
		return p[:i]
	}
	return "."
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	if ee, ok := err.(*ExitError); ok {
		return ee.Code
	}
	return -1
}

// fmtSscanf parses 'sim parity: N covered, M live-only, K gap'.
func fmtSscanf(line string, a, b, c *int) (int, error) {
	return fmt.Sscanf(line, "sim parity: %d covered, %d live-only, %d gap", a, b, c)
}
