package gate

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- host-load probe -------------------------------------------------------

// loadGate is a gate with the load probe enabled (4 CPUs × factor 2 = threshold 8)
// and a scripted 1-minute load reading; Sleep records the waits.
func loadGate(t *testing.T, readings ...float64) (*Gate, *fakeExec, *[]time.Duration, *int) {
	t.Helper()
	g, fe, _, _ := testGate(t)
	g.CPUs = 4
	g.Cfg.LoadProbeFactor = 2
	g.Cfg.ProbeInterval = 30 * time.Second
	g.Cfg.ProbeWaitMax = 10 * time.Minute
	var sleeps []time.Duration
	g.Sleep = func(d time.Duration) { sleeps = append(sleeps, d) }
	reads := 0
	g.LoadAvg = func() (float64, bool) {
		i := reads
		reads++
		if i >= len(readings) {
			i = len(readings) - 1
		}
		return readings[i], true
	}
	return g, fe, &sleeps, &reads
}

func TestLoadProbeProceedsBelowThreshold(t *testing.T) {
	g, _, sleeps, reads := loadGate(t, 3.2)
	if err := g.ProbeHostLoad(context.Background()); err != nil {
		t.Fatal(err)
	}
	if *reads != 1 || len(*sleeps) != 0 {
		t.Errorf("below the threshold must proceed at once: reads=%d sleeps=%v", *reads, *sleeps)
	}
	if r := g.probes.Load; r == nil || r.Status != ProbeOK || r.Threshold != 8 || r.CPUs != 4 || r.Attempts != 1 {
		t.Errorf("record = %+v", r)
	}
	if !strings.Contains(g.Out.(interface{ String() string }).String(), "load 3.20 is within the threshold") {
		t.Errorf("the result must be printed: %q", g.Out)
	}
}

func TestLoadProbeWaitsAboveThresholdThenProceeds(t *testing.T) {
	g, _, sleeps, reads := loadGate(t, 20, 15, 3)
	if err := g.ProbeHostLoad(context.Background()); err != nil {
		t.Fatal(err)
	}
	if *reads != 3 || len(*sleeps) != 2 || (*sleeps)[0] != 30*time.Second {
		t.Errorf("want two 30s waits then proceed: reads=%d sleeps=%v", *reads, *sleeps)
	}
	r := g.probes.Load
	if r.Status != ProbeRecovered || r.WaitedSeconds != 60 || len(r.Readings) != 3 {
		t.Errorf("record = %+v", r)
	}
}

func TestLoadProbeExhaustionFailsPreflightNamingTheProbe(t *testing.T) {
	g, _, sleeps, reads := loadGate(t, 25)
	g.Cfg.ProbeWaitMax = 90 * time.Second // 3 pauses → 4 readings
	err := g.ProbeHostLoad(context.Background())
	ee, ok := err.(*ExitError)
	if !ok || ee.Code != ExitPreflightFailed || !strings.Contains(ee.Msg, "host-load probe") {
		t.Fatalf("want ExitPreflightFailed naming the probe, got %v", err)
	}
	if *reads != 4 || len(*sleeps) != 3 {
		t.Errorf("bounded loop: reads=%d sleeps=%v", *reads, *sleeps)
	}
	if g.probes.Load.Status != ProbeExceeded {
		t.Errorf("record = %+v", g.probes.Load)
	}
}

func TestLoadProbeAbsoluteThresholdOverridesFactor(t *testing.T) {
	g, _, _, _ := loadGate(t, 12)
	g.Cfg.LoadProbeThreshold = 50
	if err := g.ProbeHostLoad(context.Background()); err != nil {
		t.Fatal(err)
	}
	if g.probes.Load.Threshold != 50 {
		t.Errorf("threshold = %v", g.probes.Load.Threshold)
	}
}

func TestLoadProbeUnavailableDoesNotBlock(t *testing.T) {
	g, _, sleeps, _ := loadGate(t, 1)
	g.LoadAvg = func() (float64, bool) { return 0, false }
	if err := g.ProbeHostLoad(context.Background()); err != nil {
		t.Fatal(err)
	}
	if g.probes.Load.Status != ProbeUnavailable || len(*sleeps) != 0 {
		t.Errorf("record = %+v sleeps=%v", g.probes.Load, *sleeps)
	}
}

func TestProbesAreOffWithAZeroConfigAndWhenSkipped(t *testing.T) {
	g, fe, _, _ := testGate(t)
	g.LoadAvg = func() (float64, bool) { t.Fatal("a disabled probe must not read the load"); return 0, false }
	if err := g.ProbeHostLoad(context.Background()); err != nil || !g.probes.empty() {
		t.Errorf("zero config: err=%v probes=%+v", err, g.probes)
	}
	if err := g.ProbeBoardLag(context.Background()); err != nil || !g.probes.empty() || len(fe.calls) != 0 {
		t.Errorf("zero config lag: err=%v calls=%v", err, fe.lines())
	}

	g, fe, _, _ = loadGate(t, 99)
	g.Env = append(g.Env, "E2E_SKIP_PROBES=1")
	g.Cfg.LagProbeThreshold = 30 * time.Second
	if err := g.ProbeHostLoad(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := g.ProbeBoardLag(context.Background()); err != nil {
		t.Fatal(err)
	}
	if g.probes.Load.Status != ProbeSkipped || g.probes.Lag.Status != ProbeSkipped || len(fe.calls) != 0 {
		t.Errorf("E2E_SKIP_PROBES: probes=%+v/%+v calls=%v", g.probes.Load, g.probes.Lag, fe.lines())
	}
}

// ---- orphan listing --------------------------------------------------------

func TestParseOrphans(t *testing.T) {
	ps := strings.Join([]string{
		"  501     1 /var/folders/x/sim.test",
		"  502     1 e2e.test",
		"  503   777 sim.test",     // has a live parent: not an orphan
		"  504     1 /usr/bin/zsh", // not a test binary
		"  505     1 /tmp/go-build/fabrik.test",
		"garbage",
		"  506     1 mysim.test", // base name must match exactly
	}, "\n")
	got := parseOrphans(ps)
	if len(got) != 3 || got[0].PID != 501 || got[0].Name != "sim.test" || got[1].PID != 502 || got[2].PID != 505 || got[2].Name != "fabrik.test" {
		t.Fatalf("orphans = %+v", got)
	}
}

func TestLoadProbeListsOrphansWithCwdAndNeverKills(t *testing.T) {
	g, fe, _, _ := loadGate(t, 1)
	fe.handler = func(_ context.Context, c Cmd) Result {
		if c.Name == "ps" {
			writeStdout(c, "  601     1 sim.test\n  602     1 e2e.test\n")
		}
		return Result{}
	}
	g.ProcCwd = func(_ context.Context, pid int) string {
		if pid == 601 {
			return "/work/issue-7"
		}
		return "" // unresolvable → "unknown", not an error
	}
	if err := g.ProbeHostLoad(context.Background()); err != nil {
		t.Fatal(err)
	}
	o := g.probes.Load.Orphans
	if len(o) != 2 || o[0].Cwd != "/work/issue-7" || o[1].Cwd != "unknown" {
		t.Fatalf("orphans = %+v", o)
	}
	if e := g.Err.(interface{ String() string }).String(); !strings.Contains(e, "pid 601") || !strings.Contains(e, "/work/issue-7") || !strings.Contains(e, "NOT killed") {
		t.Errorf("the orphans must be reported: %q", e)
	}
	assertNoKill(t, fe)
}

func TestLoadProbeOrphansAreInformationalAndNeverGate(t *testing.T) {
	g, fe, _, _ := loadGate(t, 1)
	fe.handler = func(_ context.Context, c Cmd) Result {
		if c.Name == "ps" {
			return Result{ExitCode: 1} // ps failing is not an error either
		}
		return Result{}
	}
	if err := g.ProbeHostLoad(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func assertNoKill(t *testing.T, fe *fakeExec) {
	t.Helper()
	for _, c := range fe.calls {
		switch c.Name {
		case "kill", "pkill", "killall", "taskkill":
			t.Errorf("a probe must never signal a process: %s", argsLine(c))
		}
		for _, a := range c.Args {
			if a == "kill" || a == "-9" || a == "-KILL" {
				t.Errorf("a probe must never signal a process: %s", argsLine(c))
			}
		}
	}
}

// ---- board-lag probe -------------------------------------------------------

// lagFake scripts the bed's board over `gh`: an add creates ITEM_<n>; the listing
// shows an added item only once visibleAfter(listingReadsSinceThatAdd) is true;
// deletes are recorded together with whether their context was still live.
type lagFake struct {
	mu           sync.Mutex
	adds         int
	readsSince   int
	visibleAfter func(add, reads int) bool
	leftovers    []lagItem
	deleted      []string
	deleteCtxOK  []bool
	addFails     bool
	onList       func()
}

func (l *lagFake) handle(ctx context.Context, c Cmd) Result {
	if c.Name != "gh" {
		return Result{}
	}
	line := argsLine(c)
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case strings.Contains(line, "organization(login"):
		writeStdout(c, "PVT_1\n")
	case strings.Contains(line, "addProjectV2DraftIssue"):
		if l.addFails {
			return Result{ExitCode: 1}
		}
		l.adds++
		l.readsSince = 0
		writeStdout(c, fmt.Sprintf("ITEM_%d\n", l.adds))
	case strings.Contains(line, "deleteProjectV2Item"):
		for _, a := range c.Args {
			if i := strings.Index(a, `itemId:"`); i >= 0 {
				id := strings.SplitN(a[i+len(`itemId:"`):], `"`, 2)[0]
				l.deleted = append(l.deleted, id)
			}
		}
		l.deleteCtxOK = append(l.deleteCtxOK, ctx.Err() == nil)
	case strings.Contains(line, "items(first:100"):
		l.readsSince++
		if l.onList != nil {
			l.onList()
		}
		var nodes []string
		for _, it := range l.leftovers {
			nodes = append(nodes, fmt.Sprintf(`{"id":%q,"content":{"__typename":"DraftIssue","title":%q}}`, it.ID, it.Title))
		}
		for n := 1; n <= l.adds; n++ {
			if n == l.adds && l.visibleAfter != nil && l.visibleAfter(n, l.readsSince) {
				nodes = append(nodes, fmt.Sprintf(`{"id":"ITEM_%d","content":{"__typename":"DraftIssue","title":"x"}}`, n))
			}
		}
		writeStdout(c, `{"data":{"node":{"items":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[`+strings.Join(nodes, ",")+`]}}}}`)
	}
	return Result{}
}

// lagGate: lag threshold 30s, a 3s listing poll, a 30s re-probe pause bounded at
// 90s, a bed token, and a fake clock that only Sleep advances.
func lagGate(t *testing.T, lf *lagFake) (*Gate, *fakeExec) {
	t.Helper()
	g, fe, _, _ := testGate(t)
	fe.handler = lf.handle
	g.Cfg.LagProbeThreshold = 30 * time.Second
	g.Cfg.LagPollInterval = 3 * time.Second
	g.Cfg.ProbeInterval = 30 * time.Second
	g.Cfg.ProbeWaitMax = 90 * time.Second
	g.Cfg.GHAPITimeout = 2 * time.Second
	mustWrite(t, g.Cfg.TestBed+"/.env", "FABRIK_TOKEN=bed-token\n")
	clock := time.Unix(1_700_000_000, 0)
	g.Now = func() time.Time { return clock }
	g.Sleep = func(d time.Duration) { clock = clock.Add(d) }
	return g, fe
}

func TestLagProbeProceedsBelowThresholdAndRemovesItsItem(t *testing.T) {
	lf := &lagFake{visibleAfter: func(_, reads int) bool { return reads >= 3 }} // visible on the 3rd read: 6s
	g, fe := lagGate(t, lf)
	if err := g.ProbeBoardLag(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := g.probes.Lag
	if r.Status != ProbeOK || len(r.ReadingsSeconds) != 1 || r.ReadingsSeconds[0] != 6 || r.Attempts != 1 {
		t.Errorf("record = %+v", r)
	}
	if len(lf.deleted) != 1 || lf.deleted[0] != "ITEM_1" {
		t.Errorf("the probe item must be removed: %v", lf.deleted)
	}
	assertNoKill(t, fe)
	if !strings.Contains(g.Out.(interface{ String() string }).String(), "board-listing lag 6.0s is within the threshold") {
		t.Errorf("the result must be printed: %q", g.Out)
	}
}

func TestLagProbeWaitsAboveThresholdThenProceeds(t *testing.T) {
	// First measurement: never visible within 30s. Second (after a 30s wait): quick.
	lf := &lagFake{visibleAfter: func(add, reads int) bool { return add >= 2 && reads >= 1 }}
	g, _ := lagGate(t, lf)
	if err := g.ProbeBoardLag(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := g.probes.Lag
	if r.Status != ProbeRecovered || r.Attempts != 2 || r.WaitedSeconds != 30 {
		t.Errorf("record = %+v", r)
	}
	if r.ReadingsSeconds[0] <= 30 {
		t.Errorf("a give-up reading must be strictly above the threshold, got %v", r.ReadingsSeconds)
	}
	if len(lf.deleted) != 2 {
		t.Errorf("every measurement's item must be removed: %v", lf.deleted)
	}
}

func TestLagProbeExhaustionFailsPreflightNamingTheProbe(t *testing.T) {
	lf := &lagFake{visibleAfter: func(int, int) bool { return false }}
	g, _ := lagGate(t, lf)
	err := g.ProbeBoardLag(context.Background())
	ee, ok := err.(*ExitError)
	if !ok || ee.Code != ExitPreflightFailed || !strings.Contains(ee.Msg, "board-lag probe") {
		t.Fatalf("want ExitPreflightFailed naming the probe, got %v", err)
	}
	if lf.adds != 4 || len(lf.deleted) != 4 {
		t.Errorf("4 bounded measurements, each cleaned up: adds=%d deleted=%v", lf.adds, lf.deleted)
	}
	if g.probes.Lag.Status != ProbeExceeded {
		t.Errorf("record = %+v", g.probes.Lag)
	}
}

func TestLagProbeCleansUpOnCancelOnAFreshContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	lf := &lagFake{visibleAfter: func(int, int) bool { return false }}
	lf.onList = cancel // the gate is interrupted mid-measurement
	g, _ := lagGate(t, lf)
	err := g.ProbeBoardLag(ctx)
	if err == nil {
		t.Fatal("a cancelled probe must stop the run")
	}
	if len(lf.deleted) != 1 || lf.deleted[0] != "ITEM_1" {
		t.Fatalf("the item must be removed even on cancel: %v", lf.deleted)
	}
	if len(lf.deleteCtxOK) != 1 || !lf.deleteCtxOK[0] {
		t.Errorf("the cleanup must not run on the cancelled context: %v", lf.deleteCtxOK)
	}
}

func TestLagProbeSweepsLeftoversFromAnInterruptedRun(t *testing.T) {
	lf := &lagFake{
		visibleAfter: func(int, int) bool { return true },
		leftovers: []lagItem{
			{ID: "OLD_1", Title: lagProbeTitlePrefix + "123"},
			{ID: "KEEP", Title: "someone's real draft"},
		},
	}
	g, _ := lagGate(t, lf)
	if err := g.ProbeBoardLag(context.Background()); err != nil {
		t.Fatal(err)
	}
	if g.probes.Lag.SweptLeftovers != 1 || lf.deleted[0] != "OLD_1" {
		t.Errorf("swept=%d deleted=%v", g.probes.Lag.SweptLeftovers, lf.deleted)
	}
	for _, d := range lf.deleted {
		if d == "KEEP" {
			t.Error("a non-probe item must never be touched")
		}
	}
}

func TestLagProbeUnavailableDoesNotBlock(t *testing.T) {
	t.Run("no bed token", func(t *testing.T) {
		lf := &lagFake{}
		g, fe := lagGate(t, lf)
		os.Remove(g.Cfg.TestBed + "/.env")
		if err := g.ProbeBoardLag(context.Background()); err != nil || g.probes.Lag.Status != ProbeUnavailable || len(fe.calls) != 0 {
			t.Errorf("err=%v lag=%+v calls=%v", err, g.probes.Lag, fe.lines())
		}
	})
	t.Run("the add fails", func(t *testing.T) {
		lf := &lagFake{addFails: true}
		g, _ := lagGate(t, lf)
		if err := g.ProbeBoardLag(context.Background()); err != nil || g.probes.Lag.Status != ProbeUnavailable {
			t.Errorf("err=%v lag=%+v", err, g.probes.Lag)
		}
	})
}

func TestLagProbeGHCallsAreScopedToTheBedTokenAndBounded(t *testing.T) {
	lf := &lagFake{visibleAfter: func(int, int) bool { return true }}
	g, fe := lagGate(t, lf)
	if err := g.ProbeBoardLag(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, c := range fe.callsNamed("gh") {
		if envValue(c.Env, "GH_TOKEN") != "bed-token" || !c.Session || c.Timeout != g.Cfg.GHAPITimeout {
			t.Errorf("gh call must be bed-token scoped, in a Session, and time-bounded: %s (session=%v timeout=%v)", argsLine(c), c.Session, c.Timeout)
		}
	}
}

// ---- run wiring ------------------------------------------------------------

func TestRunProbeOrderLoadBeforePregateLagAfterPregateBothBeforeTheBed(t *testing.T) {
	rf := newRunFake()
	g, _ := newRunGate(t, rf, "E2E_TRAIN_MODE=off")
	lf := &lagFake{visibleAfter: func(int, int) bool { return true }}
	g.Cfg.LagProbeThreshold = 30 * time.Second
	g.Cfg.LoadProbeFactor = 2
	g.CPUs = 4
	g.LoadAvg = func() (float64, bool) { return 1, true }
	g.Preflights = append(g.Preflights, Preflight{Name: "host-load-probe", Run: func(ctx context.Context, g *Gate, _ *Plan) error {
		rf.note("probe:load")
		return g.ProbeHostLoad(ctx)
	}})
	g.LivePreflights = []Preflight{{Name: "board-lag-probe", Run: func(ctx context.Context, g *Gate, _ *Plan) error {
		rf.note("probe:lag")
		return g.ProbeBoardLag(ctx)
	}}}
	inner := g.Exec.(*startWriter).handler
	g.Exec.(*startWriter).handler = func(ctx context.Context, c Cmd) Result {
		if c.Name == "gh" && strings.Contains(argsLine(c), "ProjectV2") || c.Name == "gh" && strings.Contains(argsLine(c), "organization(login") {
			return lf.handle(ctx, c)
		}
		return inner(ctx, c)
	}
	if code := g.Run(context.Background(), nil); code != 0 {
		t.Fatalf("exit %d\n%s", code, g.Err.(interface{ String() string }).String())
	}
	ms := rf.milestones()
	iLoad, iSim, iLag, iBuild := strings.Index(ms, "probe:load"), strings.Index(ms, "pregate:sim"), strings.Index(ms, "probe:lag"), strings.Index(ms, "bed:build")
	if iLoad < 0 || iSim < 0 || iLag < 0 || iBuild < 0 || !(iLoad < iSim && iSim < iLag && iLag < iBuild) {
		t.Errorf("order must be load probe < pregate < lag probe < bed build: %s", ms)
	}
	out := g.Out.(interface{ String() string }).String()
	if !strings.Contains(out, "probing host load") || !strings.Contains(out, "probing board-listing lag") {
		t.Errorf("preflight output must show both probes: %s", out)
	}
}

func TestRunPregateFailureStillSpendsNothingLiveWithTheProbesEnabled(t *testing.T) {
	rf := newRunFake()
	rf.simRC = 1
	g, sw := newRunGate(t, rf, "E2E_TRAIN_MODE=off")
	g.Cfg.LagProbeThreshold = 30 * time.Second
	g.LivePreflights = DefaultLivePreflights()
	g.Preflights = nil
	var ghCalls int
	inner := sw.handler
	sw.handler = func(ctx context.Context, c Cmd) Result {
		if c.Name == "gh" {
			ghCalls++
		}
		return inner(ctx, c)
	}
	if code := g.Run(context.Background(), nil); code != ExitPregateFailed {
		t.Fatalf("exit %d", code)
	}
	if ghCalls != 0 {
		t.Errorf("the lag probe is a live write and must not run before the pre-gate passes: %d gh call(s)", ghCalls)
	}
}

func TestProbeResultsAreArchivedInTheLegDirectory(t *testing.T) {
	f := covFixture(t)
	g := f.g
	g.Cfg.LoadProbeFactor, g.CPUs = 2, 4
	g.LoadAvg = func() (float64, bool) { return 1.5, true }
	g.Cfg.LagProbeThreshold = 30 * time.Second
	g.Preflights = []Preflight{{Name: "host-load-probe", Run: func(ctx context.Context, g *Gate, _ *Plan) error { return g.ProbeHostLoad(ctx) }}}
	g.LivePreflights = DefaultLivePreflights()
	lf := &lagFake{visibleAfter: func(int, int) bool { return true }}
	fe := g.Exec.(*fakeExec)
	inner := fe.handler
	fe.handler = func(ctx context.Context, c Cmd) Result {
		if c.Name == "gh" && (strings.Contains(argsLine(c), "ProjectV2") || strings.Contains(argsLine(c), "organization(login")) {
			return lf.handle(ctx, c)
		}
		return inner(ctx, c)
	}
	f.lf.suiteOut = stream(pass("TestAlpha"), pass("TestBravo"))
	if code := g.Run(context.Background(), nil); code != 0 {
		t.Fatalf("exit %d", code)
	}
	matches, _ := filepath.Glob(filepath.Join(f.dir, "*", "archive", "*", "*", "probes.json"))
	if len(matches) != 1 {
		t.Fatalf("want one probes.json in the leg archive, got %v", matches)
	}
	b, _ := os.ReadFile(matches[0])
	for _, want := range []string{`"load"`, `"lag"`, `"threshold"`, `"readings_1m"`, `"readings_seconds"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("probes.json lacks %s:\n%s", want, b)
		}
	}
}

// ---- config ----------------------------------------------------------------

func TestLoadConfigProbeKnobs(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	c, err := LoadConfig(env(map[string]string{"HOME": "/h"}), "/r")
	if err != nil {
		t.Fatal(err)
	}
	if c.LagProbeThreshold != 30*time.Second || c.LoadProbeFactor != 2 || c.LoadProbeThreshold != 0 || c.ProbeWaitMax != 10*time.Minute || c.ProbeInterval != 30*time.Second {
		t.Errorf("defaults = %+v", c)
	}
	c, err = LoadConfig(env(map[string]string{
		"HOME": "/h", "E2E_LAG_PROBE_THRESHOLD": "45", "E2E_LOAD_PROBE_FACTOR": "1.5",
		"E2E_LOAD_PROBE_THRESHOLD": "12.5", "E2E_PROBE_WAIT_MAX": "120", "E2E_PROBE_INTERVAL": "10",
	}), "/r")
	if err != nil {
		t.Fatal(err)
	}
	if c.LagProbeThreshold != 45*time.Second || c.LoadProbeFactor != 1.5 || c.LoadProbeThreshold != 12.5 || c.ProbeWaitMax != 2*time.Minute || c.ProbeInterval != 10*time.Second {
		t.Errorf("overrides = %+v", c)
	}
	for _, bad := range []string{"E2E_LOAD_PROBE_FACTOR", "E2E_LOAD_PROBE_THRESHOLD"} {
		if _, err := LoadConfig(env(map[string]string{"HOME": "/h", bad: "lots"}), "/r"); err == nil {
			t.Errorf("%s=lots must be a hard error", bad)
		}
	}
	if _, err := LoadConfig(env(map[string]string{"HOME": "/h", "E2E_LOAD_PROBE_FACTOR": "-1"}), "/r"); err == nil {
		t.Error("a negative factor must be an error")
	}
}

// ---- never kills -----------------------------------------------------------

// The probes report; the operator decides. This pins, on probes.go's AST (comments
// are ignored), that the file carries no signalling code path at all: no
// syscall/os/signal/sessionreap import, no Kill/Signal call, no termPID, and no
// string naming a kill program.
func TestProbeSourceHasNoSignallingCodePath(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "probes.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, imp := range f.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		if p == "syscall" || p == "os/signal" || p == "golang.org/x/sys/unix" || strings.Contains(p, "sessionreap") {
			t.Errorf("probes.go imports %q — the probes must never signal anything", p)
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			switch x.Sel.Name {
			case "Kill", "Signal", "Terminate", "Escalate", "SignalGroup":
				t.Errorf("%s: selector .%s", fset.Position(x.Pos()), x.Sel.Name)
			}
		case *ast.Ident:
			if x.Name == "termPID" || x.Name == "killProcGroup" {
				t.Errorf("%s: identifier %s", fset.Position(x.Pos()), x.Name)
			}
		case *ast.BasicLit:
			if x.Kind == token.STRING {
				s, _ := strconv.Unquote(x.Value)
				switch s {
				case "kill", "pkill", "killall", "taskkill":
					t.Errorf("%s: string %q", fset.Position(x.Pos()), s)
				}
			}
		}
		return true
	})
}
