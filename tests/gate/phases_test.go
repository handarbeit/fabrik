package gate

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/tests/e2e/registry"
)

var (
	shared  = registry.IsolationShared
	defBase = registry.IsolationDefaultBaseTrain
	excl    = registry.IsolationExclusive
)

func phaseSummary(ps []Phase) []string {
	var out []string
	for _, p := range ps {
		out = append(out, p.Name+"@"+p.Parallel+" "+strings.Join(p.Args, " "))
	}
	return out
}

func TestPlanPhasesOrderAndParallelism(t *testing.T) {
	live := []string{"TestBase", "TestExcl", "TestShareA", "TestShareB", "TestSwitchTrainMode"}
	classes := map[string]registry.Isolation{
		"TestShareA": shared, "TestShareB": shared, "TestBase": defBase, "TestExcl": excl, "TestSwitchTrainMode": excl,
	}
	got := phaseSummary(PlanPhases(Cell{Auth: "app", Train: "on", Parallel: "4", Args: []string{"-v"}}, live, classes))
	want := []string{
		"shared@4 -run ^(TestShareA|TestShareB)$ -v",
		"default-base-train@1 -run ^(TestBase)$ -v",
		"exclusive@1 -run ^(TestExcl)$ -v", // TestSwitchTrainMode is the leg's restart step, never a phase member
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("phases\n got: %q\nwant: %q", got, want)
	}
}

// A shared test must never be scheduled after an exclusive one, whatever the
// selection: the invariant behind "a shared test never inherits exclusive state".
func TestPlanPhasesSharedNeverFollowsExclusive(t *testing.T) {
	live := []string{"TestA", "TestB", "TestC", "TestD"}
	classes := map[string]registry.Isolation{"TestA": excl, "TestB": shared, "TestC": defBase, "TestD": shared}
	for _, args := range [][]string{nil, {"-run", "TestA|TestB"}, {"-run", "TestB|TestC|TestD"}, {"-skip", "TestD"}} {
		seenSerial := false
		for _, p := range PlanPhases(Cell{Parallel: "8", Args: args}, live, classes) {
			if p.Name != string(shared) {
				seenSerial = true
				if p.Parallel != "1" {
					t.Errorf("%v: phase %s runs at -parallel %s, want serial", args, p.Name, p.Parallel)
				}
			} else if seenSerial {
				t.Errorf("%v: the shared phase runs after a serial one", args)
			}
		}
	}
}

func TestPlanPhasesSelection(t *testing.T) {
	live := []string{"TestA", "TestB", "TestC", "TestSwitchTrainMode"}
	classes := map[string]registry.Isolation{"TestA": shared, "TestB": excl, "TestC": shared, "TestSwitchTrainMode": excl}
	cases := []struct {
		name string
		args []string
		want []string
	}{
		{"a caller -run selects only part", []string{"-run", "TestB|TestC"}, []string{"shared@4 -run ^(TestC)$", "exclusive@1 -run ^(TestB)$"}},
		{"a --resume rewrite", []string{"-run", "^(TestA|TestB)$"}, []string{"shared@4 -run ^(TestA)$", "exclusive@1 -run ^(TestB)$"}},
		{"only exclusive selected: no shared phase", []string{"-run", "TestB"}, []string{"exclusive@1 -run ^(TestB)$"}},
		{"a subtest filter survives on every phase", []string{"-run", "TestA|TestB/case"}, []string{"shared@4 -run ^(TestA)$/case", "exclusive@1 -run ^(TestB)$/case"}},
		{"-skip is folded into the selection", []string{"-skip", "TestA"}, []string{"shared@4 -run ^(TestC)$", "exclusive@1 -run ^(TestB)$"}},
		{"nothing selected but the restart test", []string{"-run", "TestSwitchTrainMode"}, nil},
	}
	for _, c := range cases {
		got := phaseSummary(PlanPhases(Cell{Parallel: "4", Args: c.args}, live, classes))
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s\n got: %q\nwant: %q", c.name, got, c.want)
		}
	}
}

func TestPlanPhasesFallbacks(t *testing.T) {
	cell := Cell{Parallel: "4", Args: []string{"-v", "-run", "X"}}
	// No registry: one undivided phase, the cell's own arguments untouched.
	if got := PlanPhases(cell, []string{"TestX"}, nil); len(got) != 1 || got[0].Name != "" || !reflect.DeepEqual(got[0].Args, cell.Args) || got[0].Parallel != "4" {
		t.Errorf("nil classes: %+v", got)
	}
	// An unparsable -run is left for go test to report.
	bad := Cell{Parallel: "4", Args: []string{"-run", "("}}
	if got := PlanPhases(bad, []string{"TestX"}, map[string]registry.Isolation{"TestX": shared}); len(got) != 1 || got[0].Name != "" {
		t.Errorf("bad -run: %+v", got)
	}
	// A test the registry does not know runs serially, last.
	got := PlanPhases(Cell{Parallel: "4"}, []string{"TestKnown", "TestUnknown"}, map[string]registry.Isolation{"TestKnown": shared})
	if len(got) != 2 || got[1].Name != string(excl) || !reflect.DeepEqual(got[1].Tests, []string{"TestUnknown"}) {
		t.Errorf("unknown test: %+v", got)
	}
}

func TestPhaseLogPath(t *testing.T) {
	if got := phaseLogPath("/a/go-test.json", Phase{}); got != "/a/go-test.json" {
		t.Errorf("undivided phase keeps the base path: %s", got)
	}
	if got := phaseLogPath("/a/go-test.json", Phase{Name: "shared"}); got != "/a/go-test.shared.json" {
		t.Errorf("got %s", got)
	}
	if got := phaseLogPath(retryLogPath("/a/go-test.json", 1), Phase{Name: "exclusive"}); got != "/a/go-test.retry-1.exclusive.json" {
		t.Errorf("got %s", got)
	}
}

func TestPeakConcurrent(t *testing.T) {
	ev := func(a, test string) Event { return Event{Action: a, Test: test} }
	cases := []struct {
		name string
		in   []Event
		want int
	}{
		{"empty", nil, 0},
		{"serial", []Event{ev("run", "A"), ev("pass", "A"), ev("run", "B"), ev("pass", "B")}, 1},
		{"two overlap", []Event{ev("run", "A"), ev("run", "B"), ev("pass", "A"), ev("pass", "B")}, 2},
		// t.Parallel(): run, pause (queued, not running), cont (released).
		{"queued tests are not running", []Event{
			ev("run", "A"), ev("pause", "A"), ev("run", "B"), ev("pause", "B"), ev("run", "C"), ev("pause", "C"),
			ev("cont", "A"), ev("cont", "B"), ev("pass", "A"), ev("cont", "C"), ev("pass", "B"), ev("pass", "C"),
		}, 2},
		{"subtests fold into the parent", []Event{ev("run", "A"), ev("run", "A/x"), ev("run", "A/y"), ev("pass", "A/x"), ev("pass", "A")}, 1},
		{"a killed test stays counted", []Event{ev("run", "A"), ev("run", "B")}, 2},
		{"a duplicate run does not double count", []Event{ev("run", "A"), ev("run", "A"), ev("pass", "A")}, 1},
	}
	for _, c := range cases {
		if got := PeakConcurrent(c.in); got != c.want {
			t.Errorf("%s: peak = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestLegSummaryFormat(t *testing.T) {
	s := LegSummary{
		Label: "app/on",
		Phases: []PhaseStat{
			{Name: "shared", Tests: 30, Parallel: "4", Wall: 90 * time.Minute, PeakConcurrent: 4, BudgetBefore: 4000, BudgetAfter: 3000},
			{Name: "exclusive", Tests: 3, Parallel: "1", Wall: 30 * time.Minute, PeakConcurrent: 1, BudgetBefore: 3000, BudgetAfter: 2900},
		},
		PeakLoad1m: 7.25, LoadSamples: 12, HasLoad: true, BedMaxConcurrent: 10,
	}
	out := s.Format()
	for _, want := range []string{
		"== leg summary (leg: app/on) ==",
		"shared", "30 test(s)", "-parallel=4", "wall 1h30m0s", "peak concurrent 4", "GraphQL 1000 pts",
		"exclusive", "GraphQL 100 pts",
		"total               wall 2h0m0s  peak concurrent 4  GraphQL 1100 pts  peak host load 7.25 (1m, 12 samples)",
		"bed max_concurrent  10",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q:\n%s", want, out)
		}
	}
	// An unreadable phase budget is reported as such, never as zero spend.
	s.Phases[1].BudgetAfter = -1
	out = s.Format()
	if !strings.Contains(out, "GraphQL n/a") || !strings.Contains(out, "(a phase's spend was not computable)") {
		t.Errorf("unknown spend must be flagged:\n%s", out)
	}
	if got := (LegSummary{Label: "x", Phases: []PhaseStat{{Parallel: "4"}}}).Format(); !strings.Contains(got, "suite") || !strings.Contains(got, "peak host load n/a") {
		t.Errorf("undivided phase / no load:\n%s", got)
	}
}

// ---- RunLeg over phases (fake Commander: no bed, no network) ----

func phaseGate(t *testing.T, classes map[string]registry.Isolation, script ...legAttempt) (*Gate, *legFake, *strings.Builder) {
	t.Helper()
	g, lf, eb := retryGate(t, script...)
	g.liveTests = []string{"TestBase", "TestExcl", "TestShareA", "TestShareB"}
	g.isolation = classes
	return g, lf, eb
}

var threeClasses = map[string]registry.Isolation{"TestShareA": shared, "TestShareB": shared, "TestBase": defBase, "TestExcl": excl}

func suiteRuns(lf *legFake) []string {
	var out []string
	for _, c := range lf.suiteCmds {
		par := ""
		for i, a := range c.Args {
			if a == "-parallel" {
				par = c.Args[i+1]
			}
		}
		out = append(out, par+" "+runArgOf(c))
	}
	return out
}

func TestLegRunsExclusiveLastAfterOneRestart(t *testing.T) {
	g, lf, _ := phaseGate(t, threeClasses,
		legAttempt{out: streams(pass("TestShareA"), pass("TestShareB"))},
		legAttempt{out: pass("TestBase")},
		legAttempt{out: pass("TestExcl")},
	)
	if err := g.RunLeg(context.Background(), defaultCell); err != nil {
		t.Fatal(err)
	}
	if len(lf.switchCmds) != 1 {
		t.Errorf("restarts = %d, want one per cell, none between phases", len(lf.switchCmds))
	}
	want := []string{"4 ^(TestShareA|TestShareB)$", "1 ^(TestBase)$", "1 ^(TestExcl)$"}
	if got := suiteRuns(lf); !reflect.DeepEqual(got, want) {
		t.Errorf("suite invocations\n got: %q\nwant: %q", got, want)
	}
}

func TestLegRunsEveryPhaseAndReturnsTheFirstFailure(t *testing.T) {
	g, lf, _ := phaseGate(t, threeClasses,
		legAttempt{out: streams(pass("TestShareA"), fail("TestShareB")), rc: 1},
		legAttempt{out: fail("TestBase"), rc: 1},
		legAttempt{out: pass("TestExcl")},
	)
	err := g.RunLeg(context.Background(), defaultCell)
	if ee, ok := err.(*ExitError); !ok || ee.Code != 1 {
		t.Fatalf("err = %v, want exit 1", err)
	}
	if len(lf.suiteCmds) != 3 {
		t.Errorf("a failed phase must not leave the next uncovered: %d invocations", len(lf.suiteCmds))
	}
}

func TestLegFirstNonZeroExitWins(t *testing.T) {
	g, _, _ := phaseGate(t, threeClasses,
		legAttempt{out: pass("TestShareA")},
		legAttempt{out: fail("TestBase"), rc: 3 + 4},
		legAttempt{out: fail("TestExcl"), rc: 1},
	)
	if ee, ok := g.RunLeg(context.Background(), defaultCell).(*ExitError); !ok || ee.Code != 7 {
		t.Fatalf("err = %v, want the first non-zero exit (7)", ee)
	}
}

func TestLegStopsWhenAPhaseIsIncomplete(t *testing.T) {
	// TestShareB is still running when the "run" dies (a timeout kill): the bed
	// state is unknown, so the serial phases do not run on top of it.
	g, lf, errb := phaseGate(t, threeClasses,
		legAttempt{out: streams(pass("TestShareA"), `{"Action":"run","Test":"TestShareB"}`), rc: 2},
	)
	_ = g.RunLeg(context.Background(), defaultCell)
	if len(lf.suiteCmds) != 1 {
		t.Errorf("invocations = %d, want 1", len(lf.suiteCmds))
	}
	if !strings.Contains(g.Out.(interface{ String() string }).String(), "not running the remaining phase(s)") {
		t.Errorf("the early stop must be explained; stderr: %s", errb)
	}
}

func TestLegRetryPreservesClassification(t *testing.T) {
	g, lf, _ := phaseGate(t, threeClasses,
		legAttempt{out: streams(inc("TestShareA", "race"), pass("TestShareB"))},
		legAttempt{out: pass("TestBase")},
		legAttempt{out: inc("TestExcl", "race")},
		// retry 1: the shared test, then the exclusive one, in phase order
		legAttempt{out: pass("TestShareA")},
		legAttempt{out: pass("TestExcl")},
	)
	if err := g.RunLeg(context.Background(), defaultCell); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"4 ^(TestShareA|TestShareB)$", "1 ^(TestBase)$", "1 ^(TestExcl)$",
		"4 ^(TestShareA)$", "1 ^(TestExcl)$",
	}
	if got := suiteRuns(lf); !reflect.DeepEqual(got, want) {
		t.Errorf("suite invocations\n got: %q\nwant: %q", got, want)
	}
	if got := g.leftInconclusiveTests(); len(got) != 0 {
		t.Errorf("both retried tests passed: %v", got)
	}
}

// A retried shared test that fails must not leave a retried exclusive test
// unretried: like the first run, every phase of the attempt runs.
func TestLegRetryRunsEveryPhaseAfterAFailedOne(t *testing.T) {
	g, lf, _ := phaseGate(t, threeClasses,
		legAttempt{out: streams(inc("TestShareA", "race"), pass("TestShareB"))},
		legAttempt{out: pass("TestBase")},
		legAttempt{out: inc("TestExcl", "race")},
		// retry 1: the shared test fails, the exclusive one must still be retried
		legAttempt{out: fail("TestShareA"), rc: 1},
		legAttempt{out: pass("TestExcl")},
	)
	err := g.RunLeg(context.Background(), defaultCell)
	if ee, ok := err.(*ExitError); !ok || ee.Code != 1 {
		t.Fatalf("err = %v, want exit 1 (the failed retry)", err)
	}
	want := []string{
		"4 ^(TestShareA|TestShareB)$", "1 ^(TestBase)$", "1 ^(TestExcl)$",
		"4 ^(TestShareA)$", "1 ^(TestExcl)$",
	}
	if got := suiteRuns(lf); !reflect.DeepEqual(got, want) {
		t.Errorf("suite invocations\n got: %q\nwant: %q", got, want)
	}
	if got := g.leftInconclusiveTests(); len(got) != 0 {
		t.Errorf("the exclusive test was retried and passed: %v", got)
	}
}

func TestLegWithoutARegistryIsOneInvocation(t *testing.T) {
	lf := &legFake{suiteOut: passStream}
	g, _, _ := newLegGate(t, lf)
	if err := g.RunLeg(context.Background(), defaultCell); err != nil {
		t.Fatal(err)
	}
	if len(lf.suiteCmds) != 1 || strings.Contains(strings.Join(lf.suiteCmds[0].Args, " "), "-run") {
		t.Errorf("the pre-#1977 shape must be unchanged: %v", suiteRuns(lf))
	}
}

func TestLegSummaryIsPrinted(t *testing.T) {
	g, _, _ := phaseGate(t, threeClasses,
		legAttempt{out: streams(pass("TestShareA"), pass("TestShareB"))},
		legAttempt{out: pass("TestBase")},
		legAttempt{out: pass("TestExcl")},
	)
	if err := g.RunLeg(context.Background(), defaultCell); err != nil {
		t.Fatal(err)
	}
	out := g.Out.(interface{ String() string }).String()
	for _, want := range []string{"== phase 1/3: shared", "== phase 3/3: exclusive", "== leg summary (leg: pat/off) ==", "peak concurrent"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q:\n%s", want, out)
		}
	}
}

// A killed leg keeps what had finished: the first phase's outcomes are in the
// ledger even though the second phase never completed.
func TestLegPhasesFeedOneRecorder(t *testing.T) {
	f := covFixture(t)
	mustWrite(t, f.g.Cfg.RepoRoot+"/tests/e2e/registry/registry.json", `{"version":1,"tests":[
{"name":"TestAlpha","parity":"gap"},{"name":"TestBravo","parity":"gap","exclusive":true,"exclusive_reason":"restarts the bed"}]}`)
	f.lf.suiteScript = []legAttempt{
		{out: stream(pass("TestAlpha"))},
		{out: stream(`{"Action":"run","Test":"TestBravo"}`), rc: 2},
	}
	f.g.Run(context.Background(), nil)
	if len(f.lf.suiteCmds) != 2 {
		t.Fatalf("invocations = %d, want 2 phases", len(f.lf.suiteCmds))
	}
	h := f.g.cov.inputs.hashes
	s := f.ledger(t).Load()
	if !s.Covered("pat/off", "TestAlpha", h["TestAlpha"]) {
		t.Errorf("the finished phase's PASS must be recorded: %+v", s.Latest)
	}
	if s.Covered("pat/off", "TestBravo", h["TestBravo"]) {
		t.Error("a test still running when its phase died must not be recorded as covered")
	}
}

// ---- load sampler and bed concurrency ----

func TestLoadSamplerKeepsThePeak(t *testing.T) {
	g, _, _, _ := testGate(t)
	loads := []float64{1.5, 9.25, 3}
	var i int
	g.LoadAvg = func() (float64, bool) {
		v := loads[i%len(loads)]
		i++
		return v, true
	}
	a := &legArchive{g: g}
	a.sampleLoad()
	a.sampleLoad()
	a.sampleLoad()
	if peak, n, ok := a.LoadStats(); !ok || peak != 9.25 || n != 3 {
		t.Errorf("LoadStats = %v %d %v", peak, n, ok)
	}
	g.LoadAvg = func() (float64, bool) { return 0, false }
	b := &legArchive{g: g}
	b.sampleLoad()
	if _, _, ok := b.LoadStats(); ok {
		t.Error("an unavailable probe must report no load")
	}
}

func TestBedMaxConcurrentPrecedence(t *testing.T) {
	write := func(env, cfg string) string {
		bed := t.TempDir()
		if env != "" {
			mustWrite(t, bed+"/.env", env)
		}
		if cfg != "" {
			mustWrite(t, bed+"/.fabrik/config.yaml", cfg)
		}
		return bed
	}
	cases := []struct {
		name, env, cfg string
		want           int
		src            string
	}{
		{"default", "", "", 5, "default"},
		{"config.yaml", "", "poll: 60\nmax_concurrent: 10\n", 10, "config.yaml"},
		{".env beats config.yaml", "FABRIK_MAX_CONCURRENT=12\n", "max_concurrent: 10\n", 12, ".env"},
		{"an invalid .env value is the default, as in the engine", "FABRIK_MAX_CONCURRENT=lots\n", "max_concurrent: 10\n", 5, "default"},
		{"a non-positive config value is the default", "", "max_concurrent: 0\n", 5, "default"},
		{"a trailing comment is fine", "", "max_concurrent: 8 # raised for #1977\n", 8, "config.yaml"},
		{"an indented key is not a top-level one", "", "other:\n  max_concurrent: 9\n", 5, "default"},
	}
	for _, c := range cases {
		n, src := BedMaxConcurrent(write(c.env, c.cfg))
		if n != c.want || src != c.src {
			t.Errorf("%s: got %d (%s), want %d (%s)", c.name, n, src, c.want, c.src)
		}
	}
}

func TestNoteBedConcurrencyWarnsWithoutBlocking(t *testing.T) {
	g, _, _, errb := testGate(t)
	mustWrite(t, g.Cfg.TestBed+"/.fabrik/config.yaml", "max_concurrent: 4\n")
	dir := t.TempDir()
	arch := &legArchive{g: g, dir: dir}
	phases := []Phase{{Name: "shared", Parallel: "8"}, {Name: "exclusive", Parallel: "1"}}
	if n := g.noteBedConcurrency(arch, Cell{Auth: "pat", Train: "on"}, phases); n != 4 {
		t.Errorf("returned %d", n)
	}
	if !strings.Contains(errb.String(), "max_concurrent is 4") || !strings.Contains(errb.String(), "-parallel=8") {
		t.Errorf("expected a warning, got %q", errb)
	}
	data, err := os.ReadFile(filepath.Join(dir, "bed-concurrency.json"))
	if err != nil || !strings.Contains(string(data), `"max_concurrent": 4`) || !strings.Contains(string(data), `"below_parallel": true`) {
		t.Errorf("bed-concurrency.json = %q (%v)", data, err)
	}
	// Enough headroom: no warning.
	errb.Reset()
	mustWrite(t, g.Cfg.TestBed+"/.fabrik/config.yaml", "max_concurrent: "+strconv.Itoa(10)+"\n")
	g.noteBedConcurrency(nil, Cell{Auth: "pat", Train: "on"}, phases)
	if errb.Len() != 0 {
		t.Errorf("unexpected warning: %q", errb)
	}
}
