package gate

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---- #1973: the bounded in-leg retry of INCONCLUSIVE tests ----

func jsonEv(e Event) string {
	b, _ := json.Marshal(e)
	return string(b)
}

// inc is the stream of a test that ends with the marker, exactly as go test -json
// emits a t.Skipf("E2E-INCONCLUSIVE: ...").
func inc(test, reason string) string {
	return stream(
		jsonEv(Event{Action: "run", Test: test}),
		jsonEv(Event{Action: "output", Test: test, Output: "=== RUN   " + test + "\n"}),
		jsonEv(Event{Action: "output", Test: test, Output: "    x_test.go:9: E2E-INCONCLUSIVE: " + reason + "\n"}),
		jsonEv(Event{Action: "output", Test: test, Output: "--- SKIP: " + test + " (1.00s)\n"}),
		jsonEv(Event{Action: "skip", Test: test, Elapsed: 1}),
	)
}

// streams concatenates stream fragments, each ending in a newline.
func streams(parts ...string) string {
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(strings.TrimSuffix(p, "\n") + "\n")
	}
	return b.String()
}

func retryGate(t *testing.T, script ...legAttempt) (*Gate, *legFake, *strings.Builder) {
	t.Helper()
	lf := &legFake{suiteScript: script}
	g, _, errb := newLegGate(t, lf)
	g.Cfg.InconclusiveRetries, g.Cfg.InconclusiveWarn = 2, 3
	var eb strings.Builder
	g.Err = &eb
	_ = errb
	return g, lf, &eb
}

func runArgOf(c Cmd) string {
	v, _, _ := extractFlag(c.Args, "run")
	return v
}

func TestLegRetryIsBoundedAndLeftoverIsUncoveredNotFailed(t *testing.T) {
	g, lf, _ := retryGate(t,
		legAttempt{out: streams(pass("TestA"), inc("TestB", "straddled"))},
		legAttempt{out: inc("TestB", "straddled again")},
	)
	var observed LegResult
	g.OnLeg = func(r LegResult) { observed = r }
	if err := g.RunLeg(context.Background(), defaultCell); err != nil {
		t.Fatalf("a leg whose tests stay inconclusive is uncovered, not failed: %v", err)
	}
	if n := len(lf.suiteCmds); n != 3 {
		t.Fatalf("suite invocations = %d, want 1 + 2 retries", n)
	}
	for i, c := range lf.suiteCmds[1:] {
		if got := runArgOf(c); got != "^(TestB)$" {
			t.Errorf("retry %d -run = %q, want only the inconclusive test", i+1, got)
		}
		if !strings.Contains(argsLine(c), "-parallel 4") || !hasEnv(c.Env, "E2E_AUTH_MODE=pat") || !hasEnv(c.Env, "E2E_TRAIN_MODE=off") {
			t.Errorf("a retry must run in the same cell configuration: %s", argsLine(c))
		}
	}
	if len(lf.switchCmds) != 1 {
		t.Errorf("a retry must not restart the bed: %d restarts", len(lf.switchCmds))
	}
	if got := g.leftInconclusiveTests(); len(got) != 1 || got[0] != "pat/off: TestB" {
		t.Errorf("left inconclusive = %v", got)
	}
	if c := observed.Classification(); len(c.Inconclusive) != 1 || c.Inconclusive[0] != "TestB" || len(c.Pass) != 1 {
		t.Errorf("OnLeg classification = %+v", c)
	}
}

func TestLegRetryPassOnRetryIsCoveredAndNamedInTheSummary(t *testing.T) {
	g, lf, _ := retryGate(t,
		legAttempt{out: inc("TestB", "straddled")},
		legAttempt{out: pass("TestB")},
	)
	var out strings.Builder
	g.Out = &out
	var observed LegResult
	g.OnLeg = func(r LegResult) { observed = r }
	if err := g.RunLeg(context.Background(), defaultCell); err != nil {
		t.Fatal(err)
	}
	if len(lf.suiteCmds) != 2 {
		t.Fatalf("a retry that passes ends the retries: %d invocations", len(lf.suiteCmds))
	}
	if len(g.leftInconclusiveTests()) != 0 {
		t.Errorf("a test that passed on retry is covered: %v", g.leftInconclusiveTests())
	}
	if c := observed.Classification(); len(c.Pass) != 1 || len(c.Inconclusive) != 0 {
		t.Errorf("OnLeg must see the retry's result: %+v", c)
	}
	// R4: even a pass-on-retry is named.
	for _, want := range []string{"== inconclusive (leg: pat/off): 1 on the first attempt: TestB", "passed on retry: TestB"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("summary lacks %q:\n%s", want, out.String())
		}
	}
}

func TestLegRetryFailureOnRetryIsAFailure(t *testing.T) {
	g, lf, _ := retryGate(t,
		legAttempt{out: inc("TestB", "straddled")},
		legAttempt{out: fail("TestB"), rc: 1},
	)
	err := g.RunLeg(context.Background(), defaultCell)
	if exitCode(err) != 1 {
		t.Fatalf("a test that FAILS on retry is a FAIL (rc 1), got %v", err)
	}
	if len(lf.suiteCmds) != 2 {
		t.Errorf("no further retry after a failure: %d invocations", len(lf.suiteCmds))
	}
	if len(g.leftInconclusiveTests()) != 0 {
		t.Errorf("a failed retry is not 'left inconclusive': %v", g.leftInconclusiveTests())
	}
}

func TestLegRetryNeverRetriesAFailure(t *testing.T) {
	g, lf, _ := retryGate(t, legAttempt{out: streams(fail("TestA"), pass("TestB")), rc: 1})
	if exitCode(g.RunLeg(context.Background(), defaultCell)) != 1 {
		t.Fatal("a plain failure keeps its exit code")
	}
	if len(lf.suiteCmds) != 1 {
		t.Errorf("a failure is never retried: %d invocations", len(lf.suiteCmds))
	}
}

func TestLegRetryDisabledAndSkippedConditions(t *testing.T) {
	cases := []struct {
		name  string
		setup func(g *Gate)
		cell  Cell
		first legAttempt
		why   string
	}{
		{"disabled", func(g *Gate) { g.Cfg.InconclusiveRetries = 0 }, defaultCell, legAttempt{out: inc("TestB", "x")}, "retries disabled"},
		{"subtest filter", nil, Cell{Auth: "pat", Train: "off", Parallel: "4", Args: []string{"-run", "TestB/case"}}, legAttempt{out: inc("TestB", "x")}, "subtests"},
		{"timeout kill", nil, defaultCell, legAttempt{out: streams(inc("TestB", "x"), `{"Action":"run","Test":"TestC"}`, `{"Action":"output","Test":"TestC","Output":"panic: test timed out after 4h0m0s\n"}`), rc: 2}, "did not complete"},
		{"backoff already engaged", func(g *Gate) { mustWrite(t, g.Cfg.EngineLog, "activating rate-limit backoff\n") }, defaultCell, legAttempt{out: inc("TestB", "x")}, "rate-limit backoff"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, lf, _ := retryGate(t, tc.first)
			var out strings.Builder
			g.Out = &out
			if tc.setup != nil {
				tc.setup(g)
			}
			g.RunLeg(context.Background(), tc.cell)
			if len(lf.suiteCmds) != 1 {
				t.Errorf("must not retry: %d invocations", len(lf.suiteCmds))
			}
			if !strings.Contains(out.String(), "not retrying — ") || !strings.Contains(out.String(), tc.why) {
				t.Errorf("the reason for not retrying must be printed (%q):\n%s", tc.why, out.String())
			}
		})
	}
}

func TestLegRunInvalidAfterARetryStillVoidsTheLeg(t *testing.T) {
	g, lf, _ := retryGate(t,
		legAttempt{out: inc("TestB", "x")},
		legAttempt{out: pass("TestB")},
	)
	// Throttling that begins DURING the retry: the scan runs after the retries.
	lf.suiteHook = func(c Cmd) {
		lf.mu.Lock()
		n := len(lf.suiteCmds)
		lf.mu.Unlock()
		if n == 2 {
			mustWrite(t, g.Cfg.EngineLog, "activating rate-limit backoff\n")
		}
	}
	if code := exitCode(g.RunLeg(context.Background(), defaultCell)); code != ExitBudgetExhausted {
		t.Fatalf("exit = %d, want RUN INVALID (3)", code)
	}
	if len(g.leftInconclusiveTests()) != 0 {
		t.Error("a voided cell must not also report leftovers")
	}
}

func TestLegRetryWarnsAboveTheThresholdOnly(t *testing.T) {
	four := streams(inc("TestA", "x"), inc("TestB", "x"), inc("TestC", "x"), inc("TestD", "x"))
	for _, tc := range []struct {
		warn int
		want bool
	}{{3, true}, {4, false}} {
		g, _, _ := retryGate(t, legAttempt{out: four})
		g.Cfg.InconclusiveWarn = tc.warn
		g.Cfg.InconclusiveRetries = 0
		var out strings.Builder
		g.Out = &out
		g.RunLeg(context.Background(), defaultCell)
		if got := strings.Contains(out.String(), "#1974"); got != tc.want {
			t.Errorf("warn=%d: warning present = %v, want %v\n%s", tc.warn, got, tc.want, out.String())
		}
	}
}

func TestLegRetryWritesPerAttemptLogs(t *testing.T) {
	g, lf, _ := retryGate(t, legAttempt{out: inc("TestB", "x")}, legAttempt{out: pass("TestB")})
	if err := g.RunLeg(context.Background(), defaultCell); err != nil {
		t.Fatal(err)
	}
	if len(lf.suiteCmds) != 2 {
		t.Fatal("expected one retry")
	}
	all, _ := filepath.Glob(filepath.Join(g.Cfg.TmpDir, "fabrik-e2e-pat-off-*.json"))
	var first, retry string
	for _, p := range all {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(p, ".retry-1.") {
			retry = string(b)
		} else {
			first = string(b)
		}
	}
	if !strings.Contains(first, "E2E-INCONCLUSIVE") || strings.Contains(retry, "E2E-INCONCLUSIVE") || !strings.Contains(retry, `"pass"`) {
		t.Errorf("each attempt keeps its own log:\nfirst=%s\nretry=%s", first, retry)
	}
}

// ---- through Gate.run: the ledger and the exit status ----

func TestRunLedgerRecordsRetriesAndPassOnRetryIsCovered(t *testing.T) {
	f := covFixture(t)
	f.lf.suiteScript = []legAttempt{
		{out: streams(pass("TestAlpha"), inc("TestBravo", "straddled"))},
		{out: pass("TestBravo")},
	}
	f.g.Cfg.InconclusiveRetries, f.g.Cfg.InconclusiveWarn = 2, 3
	if code := f.g.Run(context.Background(), nil); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	h := f.g.cov.inputs.hashes
	s := f.ledger(t).Load()
	for _, n := range []string{"TestAlpha", "TestBravo"} {
		if !s.Covered("pat/off", n, h[n]) {
			t.Errorf("%s must be covered: %+v", n, s.Latest["pat/off"][n])
		}
	}
}

func TestRunLeftInconclusiveIsUncoveredAndExitsIncompleteNeverSuccess(t *testing.T) {
	for _, resume := range []bool{false, true} {
		f := covFixture(t)
		f.lf.suiteScript = []legAttempt{{out: streams(pass("TestAlpha"), inc("TestBravo", "straddled"))}}
		f.g.Cfg.InconclusiveRetries = 1
		var out, errb strings.Builder
		f.g.Out, f.g.Err = &out, &errb
		var argv []string
		if resume {
			argv = []string{"--resume"}
		}
		code := f.g.Run(context.Background(), argv)
		if code != ExitCoverageIncomplete {
			t.Fatalf("resume=%v: exit = %d, want %d (an uncovered leg must not read as success)", resume, code, ExitCoverageIncomplete)
		}
		if !strings.Contains(errb.String(), "INCONCLUSIVE") || !strings.Contains(errb.String(), "pat/off: TestBravo") {
			t.Errorf("the message must name the leftover: %s", errb.String())
		}
		h := f.g.cov.inputs.hashes
		s := f.ledger(t).Load()
		if r, _ := s.Record("pat/off", "TestBravo"); r.Outcome != OutcomeInconclusive || s.Covered("pat/off", "TestBravo", h["TestBravo"]) {
			t.Errorf("TestBravo must be recorded INCONCLUSIVE and uncovered: %+v", r)
		}
		if !strings.Contains(out.String(), "inconclusive") {
			t.Errorf("the coverage summary must show the inconclusive count:\n%s", out.String())
		}
	}
}

func TestRunLeftInconclusiveWithoutTheLedgerStillExitsIncomplete(t *testing.T) {
	f := covFixture(t)
	f.g.Cfg.CoverageDir = ""
	f.lf.suiteScript = []legAttempt{{out: inc("TestBravo", "x")}}
	f.g.Cfg.InconclusiveRetries = 0
	if code := f.g.Run(context.Background(), nil); code != ExitCoverageIncomplete {
		t.Fatalf("exit = %d, want %d", code, ExitCoverageIncomplete)
	}
}

func TestRunRealFailureKeepsItsOwnExitCodeOverInconclusive(t *testing.T) {
	f := covFixture(t)
	f.lf.suiteScript = []legAttempt{{out: streams(fail("TestAlpha"), inc("TestBravo", "x")), rc: 1}}
	f.g.Cfg.InconclusiveRetries = 0
	if code := f.g.Run(context.Background(), nil); code != 1 {
		t.Fatalf("exit = %d, want the suite's own 1", code)
	}
}
