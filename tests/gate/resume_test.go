package gate

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/handarbeit/fabrik/tests/e2e/registry"
)

var fixtureLiveList = []string{"TestAlpha", "TestBravo", "TestCharlie", "TestDelta", "TestEcho"}

func evaluatorFor(l *Ledger, states IssueStates) *Evaluator {
	return &Evaluator{Snap: l.Load(), Hashes: fixtureHashes(), Entries: map[string]registry.Entry{}, States: states}
}

func runArg(c Cell) string {
	v, _, _ := extractFlag(c.Args, "run")
	return v
}

func TestParseRunArgsResume(t *testing.T) {
	cases := []struct {
		in            []string
		clean, resume bool
		rest          []string
	}{
		{[]string{"--resume"}, false, true, nil},
		{[]string{"--clean", "--resume"}, true, true, nil},
		{[]string{"--resume", "--clean", "-run", "X"}, true, true, []string{"-run", "X"}},
		{[]string{"--resume", "-run", "X"}, false, true, []string{"-run", "X"}},
		{[]string{"-run", "X", "--resume"}, false, false, []string{"-run", "X", "--resume"}}, // only while leading
	}
	for _, c := range cases {
		got := ParseRunArgs(c.in)
		if got.Clean != c.clean || got.Resume != c.resume || !reflect.DeepEqual(append([]string(nil), got.Rest...), append([]string(nil), c.rest...)) {
			t.Errorf("ParseRunArgs(%v) = %+v", c.in, got)
		}
	}
}

func TestSelectedTests(t *testing.T) {
	live := []string{"TestMergeTrainRunawayGuardPausesBatch", "TestMergeTrainHappy", "TestSmoke", "TestSwitchTrainMode"}
	cases := []struct {
		name string
		args []string
		want []string
	}{
		{"no flags", nil, live},
		{"skip isolated", []string{"-skip", TrainIsolatedRE}, []string{"TestMergeTrainHappy", "TestSmoke", "TestSwitchTrainMode"}},
		{"isolated run", []string{"-run", isolatedRunArg()}, []string{"TestMergeTrainRunawayGuardPausesBatch"}},
		{"unanchored run", []string{"-run", "Smoke"}, []string{"TestSmoke"}},
		{"run=form", []string{"-run=Smoke|Happy"}, []string{"TestMergeTrainHappy", "TestSmoke"}},
		{"run and skip", []string{"-run", "TestMerge", "-skip", "Runaway"}, []string{"TestMergeTrainHappy"}},
		{"passthrough ignored", []string{"-v", "-run", "Smoke"}, []string{"TestSmoke"}},
		{"subtest skip does not skip the parent", []string{"-skip", "TestSmoke/case"}, live},
		{"subtest run selects the parent", []string{"-run", "TestSmoke/case"}, []string{"TestSmoke"}},
	}
	for _, c := range cases {
		got, err := SelectedTests(live, c.args)
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v (%v), want %v", c.name, got, err, c.want)
		}
	}
	if _, err := SelectedTests(live, []string{"-run", "("}); err == nil {
		t.Error("an invalid regexp must be an error")
	}
}

func TestHasSubtestFilter(t *testing.T) {
	for args, want := range map[string]bool{"-run X": false, "-run X/y": true, "-skip X/y": true, "-run=X/y": true, "-v": false, "-run (a/b)": false} {
		if got := hasSubtestFilter(strings.Fields(args)); got != want {
			t.Errorf("hasSubtestFilter(%q) = %v, want %v", args, got, want)
		}
	}
}

func TestRequiredTestsUnionsCellsPerLabel(t *testing.T) {
	live := []string{"TestMergeTrainRunawayGuardPausesBatch", "TestSmoke"}
	cells := PlanCells(PlanInput{AuthModes: []string{"pat"}, Parallel: "4", ParallelOn: "2"})
	legs, req, err := RequiredTests(live, cells)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(legs, []string{"pat/off", "pat/on"}) {
		t.Fatalf("legs = %v", legs)
	}
	// "on" is two cells (main + isolated) under one label: the union covers every live test.
	if !reflect.DeepEqual(req["pat/on"], []string{"TestMergeTrainRunawayGuardPausesBatch", "TestSmoke"}) {
		t.Errorf("pat/on required = %v", req["pat/on"])
	}
	if len(req["pat/off"]) != 2 {
		t.Errorf("pat/off required = %v", req["pat/off"])
	}
}

// The acceptance scenario: two recorded partial runs. --resume selects exactly
// the uncovered tests, and the gate is complete only after the second.
func TestResumeAcrossTwoPartialRuns(t *testing.T) {
	ctx := context.Background()
	l := testLedger(t)
	states := fakeStates{916: "OPEN"}
	cells := []Cell{appOff}

	// Nothing recorded yet: everything is selected.
	out, resolved, err := ResumeCells(ctx, cells, fixtureLiveList, evaluatorFor(l, states))
	if err != nil || resolved != 0 || len(out) != 1 || runArg(out[0]) != "^(TestAlpha|TestBravo|TestCharlie|TestDelta|TestEcho)$" {
		t.Fatalf("fresh resume: %v resolved=%d err=%v", out, resolved, err)
	}

	// Partial run 1: Alpha+Bravo PASS, Charlie FAIL, Delta SKIP (#916, open), Echo killed in flight.
	recordFixture(t, l, appOff, "i1", "partial-run-1.json", -1)
	ev := evaluatorFor(l, states)
	out, resolved, err = ResumeCells(ctx, cells, fixtureLiveList, ev)
	if err != nil || len(out) != 1 {
		t.Fatalf("resume after run 1: %v %v", out, err)
	}
	if got := runArg(out[0]); got != "^(TestCharlie|TestEcho)$" {
		t.Errorf("resume must select exactly the uncovered tests, got %s", got)
	}
	if resolved != 3 {
		t.Errorf("resolved = %d, want 3 (Alpha, Bravo, known-skip Delta)", resolved)
	}
	legs, req, _ := RequiredTests(fixtureLiveList, cells)
	if BuildReport(ctx, ev, "sha", legs, req).Complete() {
		t.Fatal("the gate must NOT pass after only the first partial run")
	}

	// Partial run 2 covers the rest.
	recordFixture(t, l, appOff, "i2", "partial-run-2.json", -1)
	ev = evaluatorFor(l, states)
	if out, _, _ = ResumeCells(ctx, cells, fixtureLiveList, ev); len(out) != 0 {
		t.Errorf("a fully resolved leg must be dropped, got %v", out)
	}
	rep := BuildReport(ctx, ev, "sha", legs, req)
	if !rep.Complete() {
		t.Fatalf("the gate must pass once coverage is complete:\n%s", rep.Format())
	}
	if cov, miss, known, _, _ := rep.Totals(); cov != 4 || miss != 0 || known != 1 {
		t.Errorf("totals = %d covered %d missing %d known", cov, miss, known)
	}

	// The cited issue closing turns the known skip into a missing pair again (R6).
	ev = evaluatorFor(l, fakeStates{916: "CLOSED"})
	out, _, _ = ResumeCells(ctx, cells, fixtureLiveList, ev)
	if len(out) != 1 || runArg(out[0]) != "^(TestDelta)$" {
		t.Errorf("a stale skip must be reopened: %v", out)
	}
	if BuildReport(ctx, ev, "sha", legs, req).Complete() {
		t.Error("a skip citing a closed issue must block the gate")
	}
}

func TestResumeChangedTestSourceReopensOnlyThatTest(t *testing.T) {
	ctx := context.Background()
	l := testLedger(t)
	recordFixture(t, l, appOff, "i1", "partial-run-1.json", -1)
	recordFixture(t, l, appOff, "i2", "partial-run-2.json", -1)
	ev := evaluatorFor(l, fakeStates{916: "OPEN"})
	ev.Hashes["TestBravo"] = "edited"
	out, _, _ := ResumeCells(ctx, []Cell{appOff}, fixtureLiveList, ev)
	if len(out) != 1 || runArg(out[0]) != "^(TestBravo)$" {
		t.Fatalf("only the edited test reopens: %v", out)
	}
}

func TestResumeCallerRunIntersectsAndKeepsPassthrough(t *testing.T) {
	ctx := context.Background()
	l := testLedger(t)
	recordFixture(t, l, appOff, "i1", "partial-run-1.json", -1)
	ev := evaluatorFor(l, fakeStates{916: "OPEN"})
	cell := appOff
	cell.Args = []string{"-v", "-run", "Charlie|Alpha"} // Alpha is covered, Charlie is not
	out, _, err := ResumeCells(ctx, []Cell{cell}, fixtureLiveList, ev)
	if err != nil || len(out) != 1 {
		t.Fatal(out, err)
	}
	if got := runArg(out[0]); got != "^(TestCharlie)$" {
		t.Errorf("caller -run ∩ uncovered = %s", got)
	}
	if !reflect.DeepEqual(out[0].Args, []string{"-run", "^(TestCharlie)$", "-v"}) {
		t.Errorf("passthrough args must survive: %v", out[0].Args)
	}
	// A caller selection that is entirely covered runs nothing.
	cell.Args = []string{"-run", "Alpha"}
	if out, _, _ = ResumeCells(ctx, []Cell{cell}, fixtureLiveList, ev); len(out) != 0 {
		t.Errorf("nothing to run: %v", out)
	}
}

func TestResumeKeepsIsolatedCellSeparate(t *testing.T) {
	ctx := context.Background()
	live := []string{"TestMergeTrainRunawayGuardPausesBatch", "TestSmoke", "TestOther"}
	hashes := map[string]string{}
	for _, n := range live {
		hashes[n] = "h"
	}
	l := testLedger(t)
	l.Append(Record{Test: "TestSmoke", Leg: "pat/on", Cell: "pat-on", Invocation: "i1", Outcome: OutcomePass, Hash: "h"})
	ev := &Evaluator{Snap: l.Load(), Hashes: hashes, Entries: map[string]registry.Entry{}, States: fakeStates{}}
	cells := PlanCells(PlanInput{AuthModes: []string{"pat"}, TrainMode: "on", Parallel: "4", ParallelOn: "2"})
	if len(cells) != 2 {
		t.Fatalf("setup: %v", cells)
	}
	out, _, err := ResumeCells(ctx, cells, live, ev)
	if err != nil || len(out) != 2 {
		t.Fatal(out, err)
	}
	if runArg(out[0]) != "^(TestOther)$" || out[0].Isolated {
		t.Errorf("main cell = %+v", out[0])
	}
	if runArg(out[1]) != "^(TestMergeTrainRunawayGuardPausesBatch)$" || !out[1].Isolated {
		t.Errorf("isolated cell = %+v", out[1])
	}
}

func TestAnchoredRunRegexLongListAndGuard(t *testing.T) {
	var names []string
	for i := 0; i < 500; i++ {
		names = append(names, fmt.Sprintf("TestScenarioNumber%04dWithALongDescriptiveName", i))
	}
	re, err := anchoredRunRegex(names)
	if err != nil || len(re) < 20_000 || len(re) > maxRunRegexLen {
		t.Fatalf("a 500-name regex must be accepted: len=%d err=%v", len(re), err)
	}
	if got, _ := SelectedTests(names, []string{"-run", re}); len(got) != 500 {
		t.Errorf("the built regex must select exactly its names, got %d", len(got))
	}
	huge := make([]string, 5000)
	for i := range huge {
		huge[i] = strings.Repeat("T", 30)
	}
	if _, err := anchoredRunRegex(huge); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Errorf("an oversized regex must be a loud error, got %v", err)
	}
}

// #1973: an INCONCLUSIVE record is uncovered and is re-run by --resume; it never
// satisfies the gate, and the report names it as inconclusive rather than failed.
func TestResumeReRunsInconclusiveAndNeverCountsItCovered(t *testing.T) {
	ctx := context.Background()
	l := testLedger(t)
	for _, n := range []string{"TestAlpha", "TestBravo"} {
		l.Append(Record{Test: n, Leg: "app/off", Cell: "app-off", Invocation: "i1", Hash: "hash-" + n, Head: "head", Outcome: OutcomePass})
	}
	l.Append(Record{Test: "TestCharlie", Leg: "app/off", Cell: "app-off", Invocation: "i1", Hash: "hash-TestCharlie", Head: "head", Outcome: OutcomeInconclusive, SkipMsg: "E2E-INCONCLUSIVE: straddled"})
	live := []string{"TestAlpha", "TestBravo", "TestCharlie"}
	ev := evaluatorFor(l, fakeStates{})
	out, resolved, err := ResumeCells(ctx, []Cell{appOff}, live, ev)
	if err != nil || resolved != 2 || len(out) != 1 || runArg(out[0]) != "^(TestCharlie)$" {
		t.Fatalf("resume must re-run exactly the inconclusive test: %v resolved=%d err=%v", out, resolved, err)
	}
	legs, req, _ := RequiredTests(live, []Cell{appOff})
	rep := BuildReport(ctx, ev, "sha", legs, req)
	if rep.Complete() {
		t.Fatalf("an INCONCLUSIVE test must leave the gate incomplete:\n%s", rep.Format())
	}
	if !strings.Contains(rep.Format(), "inconclusive") {
		t.Errorf("the summary must name the inconclusive count:\n%s", rep.Format())
	}
}
