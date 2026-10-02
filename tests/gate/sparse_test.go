package gate

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/handarbeit/fabrik/tests/e2e/registry"
)

func TestLoadConfigMatrix(t *testing.T) {
	get := func(v string) func(string) string {
		return func(k string) string {
			if k == "E2E_MATRIX" {
				return v
			}
			return ""
		}
	}
	for in, want := range map[string]string{"": MatrixSparse, "sparse": MatrixSparse, " Sparse ": MatrixSparse, "full": MatrixFull, "FULL": MatrixFull} {
		c, err := LoadConfig(get(in), "/repo")
		if err != nil || c.Matrix != want {
			t.Errorf("E2E_MATRIX=%q -> %q, %v; want %q", in, c.Matrix, err, want)
		}
	}
	for _, bad := range []string{"2x2", "dense", "sparse,full", "off"} {
		_, err := LoadConfig(get(bad), "/repo")
		if err == nil || !strings.Contains(err.Error(), "E2E_MATRIX") {
			t.Errorf("E2E_MATRIX=%q must be rejected with a clear error, got %v", bad, err)
		}
	}
}

// sparseFixture is a live set that covers every cell-membership case.
func sparseFixture() *SparseInput {
	S, N := registry.Sensitive, registry.Neutral
	ent := func(name string, a, tr registry.Sensitivity, skip ...string) registry.Entry {
		return registry.Entry{Name: name, Auth: a, Train: tr, SkipOKLegs: skip}
	}
	es := []registry.Entry{
		ent("TestAuthOnly", S, N),
		ent("TestBoth", S, S),
		ent("TestMergeTrainRunawayGuardPausesBatch", N, S, "*/off"),
		ent("TestNeither", N, N),
		ent("TestPATOnly", S, N, "app/*"),
		ent("TestAppOnly", S, N, "pat/*"),
		ent("TestTrainOnly", N, S),
	}
	in := &SparseInput{Entries: map[string]registry.Entry{}}
	for _, e := range es {
		in.Live = append(in.Live, e.Name)
		in.Entries[e.Name] = e
	}
	return in
}

func sparseIn(modes ...string) PlanInput {
	return PlanInput{AuthModes: modes, Parallel: "4", ParallelOn: "2", Sparse: sparseFixture()}
}

func TestPlanCellsSparseDefault(t *testing.T) {
	got := summaries(PlanCells(sparseIn("pat", "app")))
	want := []string{
		// baseline first: every test (the exclusive runaway-guard scenario is the
		// last phase of this cell, not a cell of its own — #1977)
		"app/on@2",
		// other train mode, same auth: train-sensitive, minus structural */off skips
		"app/off@4 -run ^(TestBoth|TestTrainOnly)$",
		// other auth mode, same train: auth-sensitive, minus the App-only test
		"pat/on@2 -run ^(TestAuthOnly|TestBoth|TestPATOnly)$",
		// diagonal: sensitive on both axes
		"pat/off@4 -run ^(TestBoth)$",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sparse plan\n got: %q\nwant: %q", got, want)
	}
}

func TestPlanCellsSparseRequiredSet(t *testing.T) {
	in := sparseIn("pat", "app")
	legs, req, err := RequiredTests(in.Sparse.Live, PlanCells(in))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(legs, []string{"app/on", "app/off", "pat/on", "pat/off"}) {
		t.Fatalf("legs = %v", legs)
	}
	if len(req["app/on"]) != 7 {
		t.Errorf("the baseline must require every live test: %v", req["app/on"])
	}
	if !reflect.DeepEqual(req["app/off"], []string{"TestBoth", "TestTrainOnly"}) ||
		!reflect.DeepEqual(req["pat/on"], []string{"TestAuthOnly", "TestBoth", "TestPATOnly"}) ||
		!reflect.DeepEqual(req["pat/off"], []string{"TestBoth"}) {
		t.Errorf("required = %v", req)
	}
	// Every test is selected by exactly one cell per leg label.
	runs := map[string]int{}
	for _, c := range PlanCells(in) {
		sel, _ := SelectedTests(in.Sparse.Live, c.Args)
		for _, n := range sel {
			runs[c.Label()+" "+n]++
		}
	}
	for k, n := range runs {
		if n != 1 {
			t.Errorf("%s selected by %d cells, want 1", k, n)
		}
	}
}

func TestPlanCellsSparseNarrowingFilters(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*PlanInput)
		want []string
	}{
		{"E2E_AUTH_MODE=pat is a partial run: no baseline", func(p *PlanInput) { p.AuthModes = []string{"pat"} },
			[]string{"pat/on@2 -run ^(TestAuthOnly|TestBoth|TestPATOnly)$", "pat/off@4 -run ^(TestBoth)$"}},
		{"E2E_AUTH_MODE=app", func(p *PlanInput) { p.AuthModes = []string{"app"} },
			[]string{"app/on@2", "app/off@4 -run ^(TestBoth|TestTrainOnly)$"}},
		{"E2E_TRAIN_MODE=on runs at E2E_PARALLEL", func(p *PlanInput) { p.TrainMode = "on" },
			[]string{"app/on@4", "pat/on@4 -run ^(TestAuthOnly|TestBoth|TestPATOnly)$"}},
		{"E2E_TRAIN_MODE=off", func(p *PlanInput) { p.TrainMode = "off" },
			[]string{"app/off@4 -run ^(TestBoth|TestTrainOnly)$", "pat/off@4 -run ^(TestBoth)$"}},
		{"a caller -run is intersected with every cell and keeps passthrough args", func(p *PlanInput) {
			p.CallerHasRun, p.Args = true, []string{"-run", "TestBoth|TestNeither", "-v"}
		}, []string{
			"app/on@2 -run ^(TestBoth|TestNeither)$ -v", "app/off@4 -run ^(TestBoth)$ -v",
			"pat/on@2 -run ^(TestBoth)$ -v", "pat/off@4 -run ^(TestBoth)$ -v"}},
		{"a caller -run that matches no cell's selection drops those cells", func(p *PlanInput) {
			p.CallerHasRun, p.Args = true, []string{"-run", "TestNeither"}
		}, []string{"app/on@2 -run ^(TestNeither)$"}},
		{"a caller's subtest filter is preserved on the narrowed -run", func(p *PlanInput) {
			p.CallerHasRun, p.Args = true, []string{"-run", "TestBoth/case"}
		}, []string{
			"app/on@2 -run ^(TestBoth)$/case", "app/off@4 -run ^(TestBoth)$/case",
			"pat/on@2 -run ^(TestBoth)$/case", "pat/off@4 -run ^(TestBoth)$/case"}},
		{"passthrough args follow the narrowed -run", func(p *PlanInput) {
			p.Args = []string{"-v"}
		}, []string{
			"app/on@2 -v",
			"app/off@4 -run ^(TestBoth|TestTrainOnly)$ -v", "pat/on@2 -run ^(TestAuthOnly|TestBoth|TestPATOnly)$ -v",
			"pat/off@4 -run ^(TestBoth)$ -v"}},
		{"an unrecognised forced train mode falls through to the full plan (the restart step rejects it)", func(p *PlanInput) {
			p.TrainMode = "maybe"
		}, []string{"app/maybe@4", "pat/maybe@4"}},
	}
	for _, c := range cases {
		in := sparseIn("app", "pat")
		c.mut(&in)
		if c.name == "an unrecognised forced train mode falls through to the full plan (the restart step rejects it)" {
			in.AuthModes = []string{"app", "pat"}
		}
		if got := summaries(PlanCells(in)); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s\n got: %q\nwant: %q", c.name, got, c.want)
		}
	}
}

func TestPlanCellsNilSparseIsTheFullMatrix(t *testing.T) {
	in := PlanInput{AuthModes: []string{"pat", "app"}, Parallel: "4", ParallelOn: "2"}
	withNil := in
	withNil.Sparse = nil
	if !reflect.DeepEqual(PlanCells(in), PlanCells(withNil)) || len(PlanCells(in)) != 4 {
		t.Fatalf("a nil Sparse must plan the full 2×2: %q", summaries(PlanCells(in)))
	}
}

func TestSparseSelectionUnknownTestRunsEverywhere(t *testing.T) {
	in := &SparseInput{Live: []string{"TestNoEntry"}, Entries: map[string]registry.Entry{}}
	for _, sc := range sparseOrder {
		if got := sparseSelection(in, sc.auth, sc.train); len(got) != 1 {
			t.Errorf("%s/%s: a test with no registry entry must run (fail safe), got %v", sc.auth, sc.train, got)
		}
	}
}

// ---- gate-level: the plan the gate runs and the required set agree ----

func sparseCovFixture(t *testing.T) *covFix {
	t.Helper()
	f := covFixture(t)
	mustWrite(t, f.g.Cfg.RepoRoot+"/tests/e2e/registry/registry.json", `{"version":1,"tests":[
{"name":"TestAlpha","parity":"gap","auth":"neutral","train":"neutral"},
{"name":"TestBravo","parity":"gap","auth":"sensitive","auth_reason":"x","train":"sensitive","train_reason":"y"}]}`)
	f.g.Cfg.Matrix = MatrixSparse
	return f
}

func TestGateRunsOnlyTheSparseCellsAndRequiresExactlyThem(t *testing.T) {
	f := sparseCovFixture(t)
	f.lf.suiteOut = stream(pass("TestBravo"))
	var so strings.Builder
	f.g.Out = &so
	// covFixture pins pat/off: under the sparse plan that cell holds only the
	// tests sensitive on both axes.
	if code := f.g.Run(context.Background(), nil); code != 0 {
		t.Fatalf("exit = %d\n%s", code, so.String())
	}
	if got := strings.Join(lastSuite(f).Args, " "); !strings.HasSuffix(got, "-run ^(TestBravo)$") {
		t.Errorf("the pat/off cell must select exactly the both-sensitive test: %s", got)
	}
	if !strings.Contains(so.String(), "E2E_MATRIX=sparse") {
		t.Errorf("the plan summary should name the matrix mode:\n%s", so.String())
	}
	// The ledger requires only that pair: TestAlpha (neutral) is never required in pat/off.
	var co strings.Builder
	f.g.Out = &co
	if code := f.g.Coverage(context.Background(), []string{"--sha", covSHA}); code != 0 {
		t.Fatalf("coverage exit = %d\n%s", code, co.String())
	}
	if !strings.Contains(co.String(), "required set (E2E_MATRIX=sparse): 1 (test, leg) pairs of 8 in the full 2×2") {
		t.Errorf("coverage summary should state the required-set size against the full 2×2:\n%s", co.String())
	}
}

func TestSparsePlanDoesNotDependOnTheLedger(t *testing.T) {
	f := sparseCovFixture(t)
	f.g.Cfg.CoverageDir = "" // ledger disabled
	f.lf.suiteOut = stream(pass("TestBravo"))
	if code := f.g.Run(context.Background(), nil); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if got := strings.Join(lastSuite(f).Args, " "); !strings.HasSuffix(got, "-run ^(TestBravo)$") {
		t.Errorf("with the ledger off the plan must still be sparse: %s", got)
	}
}

func TestFullMatrixKeepsTheWholeSuitePerLeg(t *testing.T) {
	f := sparseCovFixture(t)
	f.g.Cfg.Matrix = MatrixFull
	f.lf.suiteOut = stream(pass("TestAlpha"), pass("TestBravo"))
	if code := f.g.Run(context.Background(), nil); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	// The phase split names every selected test (#1977) but must not drop any.
	if got := strings.Join(lastSuite(f).Args, " "); !strings.Contains(got, "TestAlpha") || !strings.Contains(got, "TestBravo") {
		t.Errorf("E2E_MATRIX=full must not narrow the leg: %s", got)
	}
}

func TestSparseResumeOverASparsePlan(t *testing.T) {
	f := sparseCovFixture(t)
	f.lf.suiteOut = stream(fail("TestBravo"))
	f.lf.suiteRC = 1
	if code := f.g.Run(context.Background(), nil); code != 1 {
		t.Fatalf("run 1 exit = %d", code)
	}
	f.g.cov = nil
	f.lf.suiteOut, f.lf.suiteRC = stream(pass("TestBravo")), 0
	if code := f.g.Run(context.Background(), []string{"--resume"}); code != 0 {
		t.Fatalf("run 2 exit = %d", code)
	}
	if got := strings.Join(lastSuite(f).Args, " "); !strings.HasSuffix(got, "-run ^(TestBravo)$") {
		t.Errorf("--resume over a sparse plan must run only the uncovered pair: %s", got)
	}
}

func TestReportNotesLineNamesTheMatrix(t *testing.T) {
	r := &Report{SHA: covSHA, Invocations: 2, Matrix: MatrixSparse, FullMatrixPairs: 208,
		Legs: []LegReport{{Leg: "app/on", Pairs: []Pair{{Test: "TestA", Status: PairCovered}}}}}
	if got := r.NotesLine(); !strings.Contains(got, "E2E_MATRIX=sparse (1 required pairs of 208 in the full 2×2)") {
		t.Errorf("notes line = %q", got)
	}
	if got := (&Report{SHA: covSHA}).NotesLine(); strings.Contains(got, "E2E_MATRIX") {
		t.Errorf("a report with no matrix info must not mention one: %q", got)
	}
}
