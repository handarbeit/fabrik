package gate

import (
	"context"
	"strings"
	"testing"
)

// driftHandler scripts the three git calls DriftCheck makes.
func driftHandler(known bool, diff, untracked string) func(context.Context, Cmd) Result {
	return func(_ context.Context, c Cmd) Result {
		line := strings.Join(c.Args, " ")
		switch {
		case strings.HasPrefix(line, "cat-file -e"):
			if !known {
				return Result{ExitCode: 128}
			}
		case strings.HasPrefix(line, "diff --name-only"):
			writeStdout(c, diff)
		case strings.HasPrefix(line, "ls-files --others"):
			writeStdout(c, untracked)
		}
		return Result{}
	}
}

func TestDriftCheck(t *testing.T) {
	sha := strings.Repeat("b", 40)
	cases := []struct {
		name      string
		known     bool
		diff      string
		untracked string
		valid     bool
		paths     []string
		reason    string
	}{
		{"no diff", true, "", "", true, nil, ""},
		{"test-only diff", true, "tests/e2e/foo_test.go\nscripts/e2e/run.sh\ntests/gate/leg.go\n", "", true, nil, ""},
		{"engine diff invalidates", true, "tests/e2e/foo_test.go\nengine/poll.go\n", "", false, []string{"engine/poll.go"}, ""},
		{"untracked engine file invalidates", true, "tests/e2e/x.go\n", "engine/new.go\n", false, []string{"engine/new.go"}, ""},
		{"untracked test file is fine", true, "", "tests/e2e/new_test.go\n", true, nil, ""},
		{"similar prefix is not allowed", true, "tests/e2e2/x.go\nscripts/e2e-other/y\n", "", false, []string{"scripts/e2e-other/y", "tests/e2e2/x.go"}, ""},
		{"unreachable sha fails closed", false, "", "", false, nil, "git fetch"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g, fe, _, _ := testGate(t)
			fe.handler = driftHandler(c.known, c.diff, c.untracked)
			d := g.DriftCheck(context.Background(), sha)
			if d.Valid != c.valid {
				t.Fatalf("Valid = %v, want %v (%+v)", d.Valid, c.valid, d)
			}
			if strings.Join(d.Paths, ",") != strings.Join(c.paths, ",") {
				t.Errorf("Paths = %v, want %v", d.Paths, c.paths)
			}
			if c.reason != "" && !strings.Contains(d.Reason, c.reason) {
				t.Errorf("Reason = %q, want it to mention %q", d.Reason, c.reason)
			}
			rep := d.Report()
			if !strings.Contains(rep, "bbbbbbb") {
				t.Errorf("report must name the SHA: %s", rep)
			}
			if !c.valid && !strings.Contains(rep, "INVALID") {
				t.Errorf("report must say INVALID: %s", rep)
			}
			for _, p := range c.paths {
				if !strings.Contains(rep, p) {
					t.Errorf("report must name the invalidating path %s: %s", p, rep)
				}
			}
		})
	}
}

func TestDriftReportTruncatesLongPathLists(t *testing.T) {
	d := Drift{SHA: "abcdef0123", Paths: []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l"}}
	if rep := d.Report(); !strings.Contains(rep, "and 2 more") {
		t.Errorf("long lists must be truncated: %s", rep)
	}
}
