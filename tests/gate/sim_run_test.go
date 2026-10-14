package gate

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/internal/testlife"
)

// jsonLine renders one test2json line.
func jsonLine(action, test, output string, elapsed float64) string {
	b, _ := json.Marshal(map[string]any{
		"Time": "2026-10-14T16:11:02.123Z", "Action": action, "Package": "github.com/x/tests/sim",
		"Test": test, "Output": output, "Elapsed": elapsed,
	})
	return string(b) + "\n"
}

// fakeClock is a settable clock for the observation time.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// simGate is a pre-gate Gate whose sim run executes simRun.
func simGate(t *testing.T, simRun func(ctx context.Context, c Cmd) Result) (*Gate, *fakeExec, *syncBuf, *fakeClock) {
	t.Helper()
	g, fe, _, _ := testGate(t)
	out := &syncBuf{}
	g.Out = out
	clk := &fakeClock{t: time.Date(2026, 10, 14, 16, 0, 0, 0, time.UTC)}
	g.Now = clk.Now
	g.Cfg.CoverageDir = filepath.Join(t.TempDir(), "cov")
	g.Cfg.CoverageKeepSHAs = 5
	fe.handler = func(ctx context.Context, c Cmd) Result {
		switch {
		case c.Name == "git" && len(c.Args) > 0 && c.Args[0] == "rev-parse":
			writeStdout(c, testHEAD+"\n")
		case c.Name == "git" && len(c.Args) > 0 && c.Args[0] == "status":
		case strings.HasSuffix(c.Name, "scripts/sim/run.sh"):
			return simRun(ctx, c)
		}
		return Result{}
	}
	return g, fe, out, clk
}

// Acceptance 1 (R2, R4): a test that never finishes is listed by the heartbeat
// with its elapsed time and a SLOW note, and the failure summary names it, the
// ceiling message and the dump.
func TestSimSuite_HeartbeatAndStillRunningForATestThatNeverFinishes(t *testing.T) {
	released := make(chan struct{})
	var clk *fakeClock
	g, _, out, clock := simGate(t, func(ctx context.Context, c Cmd) Result {
		writeStdout(c, "== sim suite: go test -count=1 ./tests/sim/... ==\n")
		writeStdout(c, jsonLine("run", "TestFast", "=== RUN   TestFast\n", 0))
		writeStdout(c, jsonLine("pass", "TestFast", "", 0.4))
		writeStdout(c, jsonLine("run", "TestHang", "=== RUN   TestHang\n", 0))
		writeStdout(c, jsonLine("run", "TestHang/sub", "=== RUN   TestHang/sub\n", 0))
		writeStdout(c, jsonLine("run", "TestQueued", "=== RUN   TestQueued\n", 0))
		writeStdout(c, jsonLine("pause", "TestQueued", "=== PAUSE TestQueued\n", 0))
		clk.Advance(6 * time.Minute) // TestHang has now run 6m, past the 5m no-history threshold
		select {
		case <-released:
		case <-ctx.Done():
		}
		dump := envValue(c.Env, testlife.DumpEnv)
		writeStdout(c, jsonLine("output", "TestHang", "tests/sim: test binary exceeded its hard wall-time ceiling of 40m0s (override with SIM_TEST_CEILING); exiting\n", 0))
		writeStdout(c, jsonLine("output", "TestHang", "tests/sim: goroutine dump written to "+dump+"\n", 0))
		writeStdout(c, jsonLine("output", "TestHang", testlife.DumpHeaderPrefix+" (tests/sim) pid 1: x ===\n", 0))
		writeStdout(c, jsonLine("output", "TestHang", "goroutine 17 [chan receive, 6 minutes]:\n", 0))
		writeStdout(c, jsonLine("fail", "", "", 2640))
		return Result{ExitCode: 1}
	})
	clk = clock
	g.Cfg.PregateHeartbeat = 20 * time.Millisecond

	done := make(chan error, 1)
	go func() {
		done <- g.runPregateStep(context.Background(), "sim suite", g.runSimSuite, "sim failed")
	}()

	deadline := time.After(10 * time.Second)
	for !strings.Contains(out.String(), "SLOW") {
		select {
		case <-deadline:
			t.Fatalf("no SLOW heartbeat; output so far:\n%s", out.String())
		case <-time.After(10 * time.Millisecond):
		}
	}
	close(released)
	err := <-done
	if exitCode(err) != ExitPregateFailed {
		t.Fatalf("err = %v", err)
	}
	s := out.String()
	for _, want := range []string{
		"== sim heartbeat:", "1 running, 1 queued behind -parallel",
		"running 6m0s", "TestHang", "SLOW: over 5m0s (no history)",
		"== sim suite failed: 0 test(s) failed, 1 still running when it ended ==",
		"still running after 6m0s", "hard wall-time ceiling of 40m0s",
		"goroutine dump: ", ".sim.dump.txt", "archive: ",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("output lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "TestFast  ") && strings.Contains(s, "running 0s TestFast") {
		t.Errorf("a finished test is listed as running:\n%s", s)
	}
	// The raw stream and the rendered timeline were archived.
	pre := filepath.Join(g.Cfg.CoverageDir, "pregate", testHEAD+".sim")
	for _, ext := range []string{".jsonl", ".log", ".timings.json"} {
		if _, err := os.Stat(pre + ext); err != nil {
			t.Errorf("missing archive file %s: %v", ext, err)
		}
	}
	if b, _ := os.ReadFile(pre + ".log"); !strings.Contains(string(b), "RUN  TestHang") || !strings.Contains(string(b), "goroutine 17 [chan receive") {
		t.Errorf("rendered log lacks the timeline or the dump:\n%s", b)
	}
	// A hang must never become the next baseline.
	if _, err := os.Stat(filepath.Join(g.Cfg.CoverageDir, "pregate", "sim-timings.json")); err == nil {
		t.Error("a failed run wrote the timing baseline")
	}
}

// R2: the SLOW threshold is 3× the last recorded duration (at least a minute).
func TestSimThreshold(t *testing.T) {
	base := map[string]float64{"TestA": 100, "TestTiny": 0.2}
	if d, why := simThreshold(base, "TestA"); d != 5*time.Minute || !strings.Contains(why, "3× its last") {
		t.Errorf("TestA = %v %q", d, why)
	}
	if d, _ := simThreshold(base, "TestTiny"); d != time.Minute {
		t.Errorf("TestTiny = %v, want the 1m floor", d)
	}
	if d, why := simThreshold(base, "TestNew"); d != 5*time.Minute || why != "no history" {
		t.Errorf("TestNew = %v %q", d, why)
	}
}

// R7 + baseline: a passing run prints the slowest tests, archives, and records
// the durations that the next run's threshold uses.
func TestSimSuite_PassPrintsSlowestAndRecordsBaseline(t *testing.T) {
	g, _, out, _ := simGate(t, func(_ context.Context, c Cmd) Result {
		for i := 1; i <= 12; i++ {
			name := fmt.Sprintf("TestN%02d", i)
			writeStdout(c, jsonLine("run", name, "", 0))
			writeStdout(c, jsonLine("pass", name, "", float64(i)))
		}
		return Result{}
	})
	if err := g.runPregateStep(context.Background(), "sim suite", g.runSimSuite, "x"); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if !strings.Contains(s, "10 slowest tests") || !strings.Contains(s, "TestN12") || strings.Contains(s, "TestN02") {
		t.Errorf("want the 10 slowest (N12..N03), got:\n%s", s)
	}
	if i, j := strings.Index(s, "TestN12"), strings.Index(s, "TestN11"); i < 0 || j < i {
		t.Errorf("slowest must come first:\n%s", s)
	}
	base := g.readSimBaseline()
	if base["TestN12"] != 12 || len(base) != 12 {
		t.Errorf("baseline = %v", base)
	}
	if !strings.Contains(s, ".sim.*") {
		t.Errorf("the archive path is not printed:\n%s", s)
	}
}

// With the ledger disabled the evidence is still kept, in a temp directory.
func TestSimSuite_LedgerDisabledArchivesToTempDir(t *testing.T) {
	g, _, out, _ := simGate(t, func(_ context.Context, c Cmd) Result {
		writeStdout(c, jsonLine("run", "TestA", "", 0))
		writeStdout(c, jsonLine("pass", "TestA", "", 1))
		return Result{}
	})
	g.Cfg.CoverageDir = ""
	if err := g.runPregateStep(context.Background(), "sim suite", g.runSimSuite, "x"); err != nil {
		t.Fatal(err)
	}
	m, _ := filepath.Glob(filepath.Join(g.Cfg.TmpDir, "fabrik-sim-*", testHEAD+".sim.jsonl"))
	if len(m) != 1 {
		t.Fatalf("temp archive = %v\n%s", m, out.String())
	}
	if g.simBaselinePath() != "" {
		t.Error("no baseline without the ledger")
	}
}

// A failed test's own output reaches the terminal; a passing test's does not.
func TestSimSuite_TerminalShowsOnlyFailuresOutput(t *testing.T) {
	g, _, out, _ := simGate(t, func(_ context.Context, c Cmd) Result {
		writeStdout(c, jsonLine("run", "TestPassing", "", 0))
		writeStdout(c, jsonLine("output", "TestPassing", "2026-10-14T16:11:02.123Z [TestPassing] [poll] nothing to do\n", 0))
		writeStdout(c, jsonLine("pass", "TestPassing", "", 1))
		writeStdout(c, jsonLine("run", "TestBroken", "", 0))
		writeStdout(c, jsonLine("output", "TestBroken", "    broken_test.go:9: expected 1 got 2\n", 0))
		writeStdout(c, jsonLine("fail", "TestBroken", "", 1))
		return Result{ExitCode: 1}
	})
	_ = g.runPregateStep(context.Background(), "sim suite", g.runSimSuite, "x")
	s := out.String()
	if strings.Contains(s, "nothing to do") {
		t.Errorf("a passing test's output was echoed:\n%s", s)
	}
	if !strings.Contains(s, "expected 1 got 2") || !strings.Contains(s, "failed: TestBroken") {
		t.Errorf("a failing test's output / name is missing:\n%s", s)
	}
}

// The retry scanner sees decoded text: a panic or data race inside the JSON
// stream still vetoes the TSan retry (without the decode every line starts
// `{"Time"` and the line-anchored veto could never match), and the testlife
// ceiling is a veto too. A TSan abort alone, in JSON, is still retried.
func TestSimSuite_JSONStreamKeepsTheRetryVeto(t *testing.T) {
	tsan := "ThreadSanitizer: CHECK failed: tsan_rtl.cpp:94 \"((0 && \"unexpected\"))\"\n"
	cases := []struct {
		name  string
		lines []string
		runs  int
	}{
		{"tsan alone retries", []string{tsan}, 2},
		{"panic vetoes", []string{tsan, "panic: boom\n"}, 1},
		{"data race vetoes", []string{tsan, "WARNING: DATA RACE\n"}, 1},
		{"goroutine dump vetoes", []string{tsan, "goroutine 5 [running]:\n"}, 1},
		{"ceiling vetoes", []string{tsan, "tests/sim: test binary exceeded its hard wall-time ceiling of 30m0s; exiting\n"}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := 0
			g, _, _, _ := simGate(t, func(_ context.Context, c Cmd) Result {
				n++
				if n == 1 {
					writeStdout(c, jsonLine("run", "TestX", "", 0))
					for _, l := range tc.lines {
						writeStdout(c, jsonLine("output", "TestX", l, 0))
					}
					return Result{ExitCode: 1}
				}
				return Result{}
			})
			_ = g.runPregateStep(context.Background(), "sim suite", g.runSimSuite, "x")
			if n != tc.runs {
				t.Fatalf("sim ran %d times, want %d", n, tc.runs)
			}
		})
	}
}

// A retry is a second attempt with its own archive files.
func TestSimSuite_RetryAttemptDoesNotOverwriteTheFirst(t *testing.T) {
	n := 0
	g, _, _, _ := simGate(t, func(_ context.Context, c Cmd) Result {
		n++
		if n == 1 {
			writeStdout(c, "ThreadSanitizer: CHECK failed: tsan_rtl.cpp:94\n")
			return Result{ExitCode: 66}
		}
		writeStdout(c, jsonLine("pass", "TestA", "", 1))
		return Result{}
	})
	if err := g.runPregateStep(context.Background(), "sim suite", g.runSimSuite, "x"); err != nil {
		t.Fatal(err)
	}
	pre := filepath.Join(g.Cfg.CoverageDir, "pregate", testHEAD+".sim")
	first, err1 := os.ReadFile(pre + ".jsonl")
	second, err2 := os.ReadFile(pre + ".a2.jsonl")
	if err1 != nil || err2 != nil || !strings.Contains(string(first), "tsan_rtl.cpp") || !strings.Contains(string(second), `"Action":"pass"`) {
		t.Fatalf("attempt files: %v %v\n%q\n%q", err1, err2, first, second)
	}
}

// The gate hands the binary where to write its dump, and the cut-release entry
// forwards extra go test arguments.
func TestSimSuite_CommandShape(t *testing.T) {
	g, fe, _, _ := simGate(t, func(_ context.Context, c Cmd) Result { return Result{} })
	if err := g.RunSim(context.Background(), []string{"-run", "TestFoo"}); err != nil {
		t.Fatal(err)
	}
	var sim Cmd
	for _, c := range fe.calls {
		if strings.HasSuffix(c.Name, "scripts/sim/run.sh") {
			sim = c
		}
	}
	if got := strings.Join(sim.Args, " "); got != "--all -json -run TestFoo" {
		t.Errorf("args = %q", got)
	}
	if !sim.Session {
		t.Error("the sim must run in its own session so its tree can be reaped")
	}
	if d := envValue(sim.Env, testlife.DumpEnv); !strings.HasSuffix(d, testHEAD+".sim.dump.txt") {
		t.Errorf("dump path = %q", d)
	}
}

// ---- tracker ----

func TestSimTracker_ContRestartsTheClockAndPauseStopsIt(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	tr := newSimTracker(clk.Now)
	tr.observe(Event{Action: "run", Test: "TestA"})
	clk.Advance(time.Minute)
	tr.observe(Event{Action: "pause", Test: "TestA"})
	if len(tr.runningNow()) != 0 || tr.queuedCount() != 1 {
		t.Fatalf("a paused test is queued, not running: %v", tr.runningNow())
	}
	clk.Advance(10 * time.Minute) // queued behind -parallel: not run time
	tr.observe(Event{Action: "cont", Test: "TestA"})
	clk.Advance(30 * time.Second)
	r := tr.runningNow()
	if len(r) != 1 || r[0].Elapsed != 30*time.Second {
		t.Fatalf("running = %v, want TestA at 30s", r)
	}
	tr.observe(Event{Action: "run", Test: "TestA/sub"}) // subtests fold into the parent
	if len(tr.runningNow()) != 1 {
		t.Fatalf("subtests must not be listed: %v", tr.runningNow())
	}
	tr.observe(Event{Action: "pass", Test: "TestA"})
	if len(tr.runningNow()) != 0 {
		t.Fatal("a finished test is still running")
	}
}

// ---- reaper ----

type fakeTable struct {
	mu      sync.Mutex
	members []int
	comm    map[int]string
	tok     map[int]string
	killed  []int
}

func (f *fakeTable) ops() simReaperOps {
	return simReaperOps{
		Members: func(int) ([]int, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			return append([]int(nil), f.members...), nil
		},
		StartToken: func(pid int) string {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.tok[pid]
		},
		Comm: func(pid int) string { f.mu.Lock(); defer f.mu.Unlock(); return f.comm[pid] },
		Kill: func(pid int) error { f.mu.Lock(); defer f.mu.Unlock(); f.killed = append(f.killed, pid); return nil },
	}
}

func newReaper(f *fakeTable, clk *fakeClock) *simReaper {
	return newSimReaper(f.ops(), clk.Now, 32*time.Minute, func(string, ...any) {})
}

func TestSimReaper_KillsOnlyAfterCeilingPlusGraceByStartToken(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	f := &fakeTable{members: []int{10, 11, 12, 13}, comm: map[int]string{10: "/bin/bash", 11: "go", 12: "/work/x/sim.test", 13: "/work/y/other"}, tok: map[int]string{10: "a", 11: "b", 12: "c", 13: "d"}}
	r := newReaper(f, clk)
	r.check(10)
	clk.Advance(31 * time.Minute)
	r.check(10)
	if len(f.killed) != 0 {
		t.Fatalf("killed before ceiling+grace: %v", f.killed)
	}
	clk.Advance(2 * time.Minute)
	r.check(10)
	if len(f.killed) != 1 || f.killed[0] != 12 {
		t.Fatalf("killed = %v, want only the .test binary 12", f.killed)
	}
	if got := r.killed(); len(got) != 1 || got[0] != 12 {
		t.Fatalf("killed() = %v", got)
	}
}

func TestSimReaper_RecycledPIDIsNeverKilled(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	f := &fakeTable{members: []int{12}, comm: map[int]string{12: "sim.test"}, tok: map[int]string{12: "old"}}
	r := newReaper(f, clk)
	r.check(10)
	clk.Advance(time.Hour)
	f.mu.Lock()
	f.tok[12] = "new" // the PID now names a different process
	f.mu.Unlock()
	r.check(10)
	if len(f.killed) != 0 {
		t.Fatalf("a recycled PID was killed: %v", f.killed)
	}
	// ...and the new process starts its own clock.
	clk.Advance(time.Hour)
	r.check(10)
	if len(f.killed) != 1 {
		t.Fatalf("the new process, seen long enough, should be reaped: %v", f.killed)
	}
}

func TestSimReaper_UnreadableIdentityNeverKills(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	f := &fakeTable{members: []int{12}, comm: map[int]string{12: "sim.test"}, tok: map[int]string{}}
	r := newReaper(f, clk)
	r.check(10)
	clk.Advance(5 * time.Hour)
	r.check(10)
	if len(f.killed) != 0 {
		t.Fatalf("killed a process with no readable identity: %v", f.killed)
	}
}

// A binary that exits (and stops being a member) is forgotten, so a later one
// with the same PID but a new token is timed afresh.
func TestSimReaper_ForgetsMembersThatLeft(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	f := &fakeTable{members: []int{12}, comm: map[int]string{12: "sim.test"}, tok: map[int]string{12: "a"}}
	r := newReaper(f, clk)
	r.check(10)
	f.mu.Lock()
	f.members = nil
	f.mu.Unlock()
	r.check(10)
	if len(r.seen) != 0 {
		t.Fatalf("seen = %v", r.seen)
	}
}

// End to end through runSimSuite: the child's PID reaches the reaper, which
// kills a stuck binary, and the failure summary says so.
func TestSimSuite_ReaperIsWiredToTheChildSession(t *testing.T) {
	f := &fakeTable{members: []int{4242}, comm: map[int]string{4242: "sim.test"}, tok: map[int]string{4242: "t"}}
	var sid int
	var sidMu sync.Mutex
	var clk *fakeClock
	g, _, out, clock := simGate(t, func(ctx context.Context, c Cmd) Result {
		c.OnStart(777)
		deadline := time.After(10 * time.Second)
		for {
			f.mu.Lock()
			n := len(f.killed)
			f.mu.Unlock()
			if n > 0 {
				return Result{ExitCode: 137}
			}
			clk.Advance(time.Hour)
			select {
			case <-deadline:
				return Result{ExitCode: 1}
			case <-ctx.Done():
				return Result{ExitCode: 1}
			case <-time.After(5 * time.Millisecond):
			}
		}
	})
	clk = clock
	g.Cfg.PregateReapPoll = 5 * time.Millisecond
	g.Cfg.PregateReapGrace = time.Minute
	g.simReaperOpsFn = func() *simReaperOps {
		o := f.ops()
		inner := o.Members
		o.Members = func(s int) ([]int, error) { sidMu.Lock(); sid = s; sidMu.Unlock(); return inner(s) }
		return &o
	}
	_ = g.runPregateStep(context.Background(), "sim suite", g.runSimSuite, "x")
	sidMu.Lock()
	defer sidMu.Unlock()
	if sid != 777 {
		t.Errorf("reaper watched session %d, want the child's 777", sid)
	}
	if !strings.Contains(out.String(), "SIGKILLed test binary pid(s) [4242]") && !strings.Contains(out.String(), "SIGKILLed") {
		t.Errorf("failure summary does not mention the reap:\n%s", out.String())
	}
}

// ---- archive retention ----

func TestPrunePregateSimArchive_KeepsNewestHeadsAndNeverTheRecord(t *testing.T) {
	g, _, _, _ := testGate(t)
	g.Cfg.CoverageKeepSHAs = 2
	dir := t.TempDir()
	now := time.Now()
	write := func(name string, age time.Duration) {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
	}
	for i, h := range []string{"aaa", "bbb", "ccc", "ddd"} { // aaa newest
		age := time.Duration(i+1) * time.Hour
		write(h+".sim.jsonl", age)
		write(h+".sim.log", age)
		write(h+".json", age)          // the pre-gate record
		write(h+".retries.jsonl", age) // the retry note
	}
	write("sim-timings.json", 100*time.Hour)
	write("zzz.sim.jsonl", 200*time.Hour) // oldest, but it is the current head
	g.prunePregateSimArchive(dir, "zzz")

	exists := func(n string) bool { _, err := os.Stat(filepath.Join(dir, n)); return err == nil }
	for _, n := range []string{"aaa.sim.jsonl", "aaa.sim.log", "bbb.sim.jsonl", "zzz.sim.jsonl", "sim-timings.json"} {
		if !exists(n) {
			t.Errorf("%s was pruned", n)
		}
	}
	for _, n := range []string{"ccc.sim.jsonl", "ccc.sim.log", "ddd.sim.jsonl"} {
		if exists(n) {
			t.Errorf("%s survived", n)
		}
	}
	for _, h := range []string{"aaa", "bbb", "ccc", "ddd"} {
		if !exists(h+".json") || !exists(h+".retries.jsonl") {
			t.Errorf("coverage record of %s was touched", h)
		}
	}
}
