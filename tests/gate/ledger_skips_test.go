package gate

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type fakeStates map[int]string // "" => error

func (f fakeStates) State(_ context.Context, n int) (string, error) {
	st, ok := f[n]
	if !ok || st == "ERR" {
		return "", errors.New("no gh access")
	}
	return st, nil
}

func TestClassifySkip(t *testing.T) {
	cases := []struct {
		name       string
		rec        Record
		structural bool
		states     fakeStates
		want       SkipKind
		reason     string
	}{
		{"open issue is a known skip", Record{Issues: []int{916}}, false, fakeStates{916: "OPEN"}, SkipKnown, "open #916"},
		{"closed issue is missing", Record{Issues: []int{916}}, false, fakeStates{916: "CLOSED"}, SkipMissing, "stale"},
		{"gh failure is missing, not known", Record{Issues: []int{916}}, false, fakeStates{916: "ERR"}, SkipMissing, "unknown"},
		{"uncited is missing", Record{}, false, fakeStates{}, SkipMissing, "no issue"},
		{"structural skip", Record{}, true, fakeStates{}, SkipStructural, "skip_ok_legs"},
		{"structural wins over a closed citation", Record{Issues: []int{1}}, true, fakeStates{1: "CLOSED"}, SkipStructural, ""},
		{"any open of several is known", Record{Issues: []int{1, 2}}, false, fakeStates{1: "CLOSED", 2: "OPEN"}, SkipKnown, "#2"},
		{"all closed of several is missing", Record{Issues: []int{1, 2}}, false, fakeStates{1: "CLOSED", 2: "CLOSED"}, SkipMissing, "#1, #2"},
		{"closed plus unreadable is missing", Record{Issues: []int{1, 2}}, false, fakeStates{1: "CLOSED", 2: "ERR"}, SkipMissing, "unknown (#2)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ClassifySkip(context.Background(), c.rec, c.structural, c.states)
			if got.Kind != c.want || !strings.Contains(got.Reason, c.reason) {
				t.Fatalf("got %v %q, want %v containing %q", got.Kind, got.Reason, c.want, c.reason)
			}
		})
	}
}

func TestGHIssueStatesUsesCommanderWithBoundAndCaches(t *testing.T) {
	g, fe, _, _ := testGate(t)
	g.Cfg.IssueRepo = "o/r"
	fe.handler = func(_ context.Context, c Cmd) Result {
		if c.Name == "gh" && strings.Contains(strings.Join(c.Args, " "), "issue view 916") {
			writeStdout(c, "CLOSED\n")
		}
		return Result{}
	}
	s := g.newIssueStates()
	for i := 0; i < 3; i++ {
		st, err := s.State(context.Background(), 916)
		if err != nil || st != "CLOSED" {
			t.Fatalf("state = %q, %v", st, err)
		}
	}
	calls := fe.callsNamed("gh")
	if len(calls) != 1 {
		t.Fatalf("answers must be cached: %d gh calls", len(calls))
	}
	c := calls[0]
	if !c.Session || c.Timeout != g.Cfg.GHAPITimeout || !strings.Contains(argsLine(c), "--repo o/r") {
		t.Errorf("gh call must be bounded and repo-scoped: %+v", c)
	}
}

func TestGHIssueStatesFailuresAreErrors(t *testing.T) {
	for name, res := range map[string]Result{
		"exit":    {ExitCode: 1},
		"timeout": {TimedOut: true, ExitCode: 124},
	} {
		g, fe, _, _ := testGate(t)
		g.Cfg.GHAPITimeout = time.Second
		r := res
		fe.handler = func(context.Context, Cmd) Result { return r }
		if st, err := g.newIssueStates().State(context.Background(), 5); err == nil || st != "" {
			t.Errorf("%s: want an error and no state, got %q %v", name, st, err)
		}
	}
	g, fe, _, _ := testGate(t)
	fe.handler = func(_ context.Context, c Cmd) Result { writeStdout(c, "weird"); return Result{} }
	if _, err := g.newIssueStates().State(context.Background(), 5); err == nil {
		t.Error("an unexpected state string must be an error")
	}
}
