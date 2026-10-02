package gate

import (
	"context"
	"reflect"
	"testing"
)

func TestParseRunArgs(t *testing.T) {
	cases := []struct {
		in    []string
		clean bool
		rest  []string
	}{
		{nil, false, nil},
		{[]string{"--clean"}, true, nil},
		{[]string{"--clean", "-run", "X"}, true, []string{"-run", "X"}},
		{[]string{"-run", "X", "--clean"}, false, []string{"-run", "X", "--clean"}}, // only as the FIRST argument
		{[]string{"-v"}, false, []string{"-v"}},
	}
	for _, c := range cases {
		got := ParseRunArgs(c.in)
		if got.Clean != c.clean || !reflect.DeepEqual(append([]string(nil), got.Rest...), append([]string(nil), c.rest...)) {
			t.Errorf("ParseRunArgs(%v) = %+v, want clean=%v rest=%v", c.in, got, c.clean, c.rest)
		}
	}
}

func TestHasRunFlag(t *testing.T) {
	for _, a := range []string{"-run", "--run", "-run=X", "--run=X"} {
		if !HasRunFlag([]string{"-v", a}) {
			t.Errorf("%q should count as -run", a)
		}
	}
	for _, a := range []string{"-v", "-runx", "-skip", "run", "-count=1"} {
		if HasRunFlag([]string{a}) {
			t.Errorf("%q must not count as -run", a)
		}
	}
}

// cellSummary renders a cell compactly: auth/train@parallel args.
func cellSummary(c Cell) string {
	s := c.Label() + "@" + c.Parallel
	for _, a := range c.Args {
		s += " " + a
	}
	return s
}

func summaries(cells []Cell) []string {
	var out []string
	for _, c := range cells {
		out = append(out, cellSummary(c))
	}
	return out
}

// The leg-shape table: every env / -run combination run.sh's dispatch loop had.
func TestPlanCells(t *testing.T) {
	both := []string{"pat", "app"}
	cases := []struct {
		name string
		in   PlanInput
		want []string
	}{
		{"default gate: off, then on; on uses the tighter cap",
			PlanInput{AuthModes: []string{"pat"}, Parallel: "8", ParallelOn: "4"},
			[]string{"pat/off@8", "pat/on@4"}},
		{"default gate runs pat first, then app",
			PlanInput{AuthModes: both, Parallel: "8", ParallelOn: "4"},
			[]string{"pat/off@8", "pat/on@4", "app/off@8", "app/on@4"}},
		{"caller passthrough args reach every cell",
			PlanInput{AuthModes: []string{"pat"}, Parallel: "8", ParallelOn: "4", Args: []string{"-v"}},
			[]string{"pat/off@8 -v", "pat/on@4 -v"}},
		{"a caller -run: off, then a single on leg",
			PlanInput{AuthModes: []string{"pat"}, Parallel: "8", ParallelOn: "4", CallerHasRun: true, Args: []string{"-run", "Smoke"}},
			[]string{"pat/off@8 -run Smoke", "pat/on@4 -run Smoke"}},
		{"E2E_TRAIN_MODE=off: one leg at E2E_PARALLEL",
			PlanInput{AuthModes: []string{"pat"}, TrainMode: "off", Parallel: "8", ParallelOn: "4"},
			[]string{"pat/off@8"}},
		{"E2E_TRAIN_MODE=on: one leg at E2E_PARALLEL (not E2E_PARALLEL_ON)",
			PlanInput{AuthModes: []string{"pat"}, TrainMode: "on", Parallel: "8", ParallelOn: "4"},
			[]string{"pat/on@8"}},
		{"E2E_TRAIN_MODE=on with a caller -run: a single leg",
			PlanInput{AuthModes: []string{"pat"}, TrainMode: "on", Parallel: "8", ParallelOn: "4", CallerHasRun: true, Args: []string{"-run=X"}},
			[]string{"pat/on@8 -run=X"}},
		{"an unrecognised forced mode is passed through verbatim (the restart step then rejects it)",
			PlanInput{AuthModes: []string{"app"}, TrainMode: "maybe", Parallel: "8", ParallelOn: "4"},
			[]string{"app/maybe@8"}},
		{"E2E_PARALLEL / E2E_PARALLEL_ON are passed through as given",
			PlanInput{AuthModes: []string{"app"}, Parallel: "1", ParallelOn: "1"},
			[]string{"app/off@1", "app/on@1"}},
	}
	for _, c := range cases {
		if got := summaries(PlanCells(c.in)); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s\n got: %q\nwant: %q", c.name, got, c.want)
		}
	}
}

func TestSerialSchedulerStopsAtTheFirstFailingLeg(t *testing.T) {
	g, _, out, _ := testGate(t)
	var ran []string
	sched := SerialScheduler{}
	cells := PlanCells(PlanInput{AuthModes: []string{"pat", "app"}, Parallel: "4", ParallelOn: "2"})
	g.runLegFn = func(_ context.Context, c Cell) error {
		ran = append(ran, c.Label())
		if len(ran) == 2 {
			return &ExitError{Code: 1}
		}
		return nil
	}
	err := sched.Run(context.Background(), g, cells)
	if exitCode(err) != 1 {
		t.Fatalf("want exit 1, got %v", err)
	}
	if !reflect.DeepEqual(ran, []string{"pat/off", "pat/on"}) {
		t.Errorf("the run must end at the first failing leg (set -e), ran %v", ran)
	}
	if got := out.String(); got != "== auth leg: pat ==\n" {
		t.Errorf("out=%q", got)
	}
}
