package gate

import (
	"context"
	"strings"
	"testing"
)

// D9: each bed resets its own board and repos with its own token; a reset of
// one bed never touches another's.
func resetBedsSetup(t *testing.T) (*Gate, *resetFake) {
	t.Helper()
	g, fe, _, _ := twoBedGate(t)
	rf := &resetFake{projectID: "PVT_x", remaining: "0"}
	fe.handler = rf.handle
	return g, rf
}

// bedOfCall names which bed's repos or board a gh call addresses ("" if none).
func bedOfCall(line string) []string {
	var beds []string
	for bed, marks := range map[string][]string{
		"A": {"o/alpha-a", "o/beta-a", "projectV2(number:2)"},
		"B": {"o/alpha-b", "o/beta-b", "projectV2(number:3)"},
	} {
		for _, m := range marks {
			if strings.Contains(line, m) {
				beds = append(beds, bed)
				break
			}
		}
	}
	return beds
}

func TestResetBedsResetsEachBedWithItsOwnToken(t *testing.T) {
	g, rf := resetBedsSetup(t)
	if err := g.ResetBeds(context.Background(), ResetOptions{}, ""); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for i, line := range rf.calls {
		tok := rf.tokens[i]
		for _, bed := range bedOfCall(line) {
			seen[bed] = true
			if want := "token-" + strings.ToLower(bed); tok != want {
				t.Errorf("%q addresses bed %s with %s", line, bed, tok)
			}
		}
	}
	if !seen["A"] || !seen["B"] {
		t.Errorf("both beds must be reset: %v", seen)
	}
	out := g.Out.(interface{ String() string }).String()
	if !strings.Contains(out, "[bed A] == resetting bed A") || !strings.Contains(out, "[bed B] == resetting bed B") || !strings.Contains(out, "[bed B] done.") {
		t.Errorf("out:\n%s", out)
	}
}

func TestResetOfOneBedNeverTouchesAnother(t *testing.T) {
	g, rf := resetBedsSetup(t)
	if err := g.ResetBeds(context.Background(), ResetOptions{}, g.Cfg.BedDirs[0]); err != nil {
		t.Fatal(err)
	}
	if len(rf.calls) == 0 {
		t.Fatal("nothing was reset")
	}
	for i, line := range rf.calls {
		if rf.tokens[i] != "token-a" {
			t.Errorf("bed A's reset used another token: %q", line)
		}
		for _, bed := range bedOfCall(line) {
			if bed != "A" {
				t.Errorf("bed A's reset addressed bed %s: %q", bed, line)
			}
		}
	}
	if out := g.Out.(interface{ String() string }).String(); strings.Contains(out, "[bed") {
		t.Errorf("--bed is a single-bed reset, unprefixed:\n%s", out)
	}
}

func TestResetBedsContinuesPastAFailure(t *testing.T) {
	g, rf := resetBedsSetup(t)
	fe := g.Exec.(*fakeExec)
	fe.handler = func(ctx context.Context, c Cmd) Result {
		if envValue(c.Env, "GH_TOKEN") == "token-a" && strings.HasPrefix(argsLine(c), "gh pr list") {
			return Result{ExitCode: 4}
		}
		return rf.handle(ctx, c)
	}
	err := g.ResetBeds(context.Background(), ResetOptions{}, "")
	if exitCode(err) != 4 {
		t.Fatalf("the first failure's code: %v", err)
	}
	reachedB := false
	for _, line := range fe.lines() {
		for _, bed := range bedOfCall(line) {
			reachedB = reachedB || bed == "B"
		}
	}
	if !reachedB {
		t.Error("bed B must still be reset after bed A failed")
	}
}

func TestResetBedsSingleBedIsReset(t *testing.T) {
	g, fe, out, _ := testGate(t)
	mustWrite(t, g.Cfg.TestBed+"/.env", "FABRIK_TOKEN=bed-token\n")
	rf := &resetFake{projectID: "PVT_x", remaining: "0"}
	fe.handler = rf.handle
	if err := g.ResetBeds(context.Background(), ResetOptions{}, ""); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "[bed") || !strings.Contains(out.String(), "handarbeit/fabrik-test-alpha: no open PRs") {
		t.Errorf("one bed: today's reset, unprefixed:\n%s", out)
	}
}
