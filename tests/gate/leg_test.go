package gate

import (
	"bytes"
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// legFake scripts every subprocess a leg runs: the bed-restart `go test`, the
// suite `go test -json`, the two budget probes, and reset's gh calls.
type legFake struct {
	mu         sync.Mutex
	switchRC   int
	suiteRC    int
	suiteOut   string // written to the suite's combined stdout+stderr
	suiteWedge bool
	suiteHook  func(c Cmd) // runs while the "suite" is live (stall tests)
	budgets    []string    // successive `gh api graphql` budget probe answers; "ERR" fails, "HANG" blocks until ctx done
	ghCalls    []string
	suiteCmds  []Cmd
	switchCmds []Cmd
}

func isSwitch(c Cmd) bool {
	return c.Name == "go" && strings.Contains(strings.Join(c.Args, " "), "^TestSwitchTrainMode$")
}
func isSuite(c Cmd) bool {
	return c.Name == "go" && strings.Contains(strings.Join(c.Args, " "), " -json ")
}

func (f *legFake) handle(ctx context.Context, c Cmd) Result {
	switch {
	case isSwitch(c):
		f.mu.Lock()
		f.switchCmds = append(f.switchCmds, c)
		rc := f.switchRC
		f.mu.Unlock()
		return Result{ExitCode: rc}
	case isSuite(c):
		f.mu.Lock()
		f.suiteCmds = append(f.suiteCmds, c)
		f.mu.Unlock()
		if c.Stdout != nil {
			writeStdout(c, f.suiteOut)
		}
		if f.suiteHook != nil {
			f.suiteHook(c)
		}
		return Result{ExitCode: f.suiteRC, PipeWedged: f.suiteWedge}
	case c.Name == "gh" && len(c.Args) >= 2 && c.Args[0] == "api" && c.Args[1] == "graphql" && strings.Contains(strings.Join(c.Args, " "), "rateLimit"):
		f.mu.Lock()
		ans := "1000"
		if len(f.budgets) > 0 {
			ans, f.budgets = f.budgets[0], f.budgets[1:]
		}
		f.mu.Unlock()
		switch ans {
		case "ERR":
			writeStderr(c, "gh: HTTP 502\n")
			return Result{ExitCode: 1}
		case "TIMEOUT":
			return Result{ExitCode: 143, TimedOut: true}
		case "HANG":
			<-ctx.Done()
			return Result{ExitCode: 143}
		}
		writeStdout(c, ans+"\n")
		return Result{}
	case c.Name == "gh":
		f.mu.Lock()
		f.ghCalls = append(f.ghCalls, argsLine(c))
		f.mu.Unlock()
		return Result{}
	}
	return Result{}
}

func newLegGate(t *testing.T, lf *legFake, env ...string) (*Gate, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	g, fe, out, errb := testGate(t, env...)
	fe.handler = lf.handle
	g.Cfg.BedToken = "bed-token"
	g.Cfg.GHAPITimeout = 2 * time.Second
	g.Cfg.PostSuiteWatchdog = 10 * time.Second
	mustWrite(t, g.Cfg.TestBed+"/.env", "FABRIK_TOKEN=bed-token\n")
	return g, out, errb
}

var defaultCell = Cell{Auth: "pat", Train: "off", Parallel: "4"}

const passStream = `{"Action":"run","Test":"TestSmoke"}
not json — a build line
{"Action":"output","Test":"TestSmoke","Output":"=== RUN   TestSmoke\n"}
{"Action":"pass","Test":"TestSmoke","Elapsed":12.5}
{"Action":"run","Test":"TestSlow"}
{"Action":"pass","Test":"TestSlow","Elapsed":99}
`

func TestLegHappyPath(t *testing.T) {
	lf := &legFake{suiteOut: passStream, budgets: []string{"4000", "1200"}}
	g, out, errb := newLegGate(t, lf)
	var observed LegResult
	g.OnLeg = func(r LegResult) { observed = r }
	cell := Cell{Auth: "app", Train: "on", Parallel: "2", Args: []string{"-skip", "X", "-v"}}

	if err := g.RunLeg(context.Background(), cell); err != nil {
		t.Fatalf("RunLeg: %v\nstderr: %s", err, errb)
	}

	// The restart step: a separate -v go test with the fixed 3m timeout.
	if len(lf.switchCmds) != 1 {
		t.Fatalf("switch calls: %d", len(lf.switchCmds))
	}
	sw := lf.switchCmds[0]
	if got := strings.Join(sw.Args, " "); got != "test -tags=e2e -v -count=1 -timeout 3m -run ^TestSwitchTrainMode$ ./tests/e2e/..." {
		t.Errorf("switch argv = %q", got)
	}
	for _, kv := range []string{"E2E_TRAIN_SWITCH=1", "E2E_TRAIN_MODE=on", "E2E_AUTH_MODE=app"} {
		if !hasEnv(sw.Env, kv) {
			t.Errorf("switch env missing %s", kv)
		}
	}

	// The suite: exact argv, mode and auth in its environment.
	su := lf.suiteCmds[0]
	if got := strings.Join(su.Args, " "); got != "test -tags=e2e -json -count=1 -timeout 4h -parallel 2 ./tests/e2e/... -skip X -v" {
		t.Errorf("suite argv = %q", got)
	}
	for _, kv := range []string{"E2E_TRAIN_MODE=on", "E2E_AUTH_MODE=app"} {
		if !hasEnv(su.Env, kv) {
			t.Errorf("suite env missing %s", kv)
		}
	}
	if hasEnv(su.Env, "E2E_TRAIN_SWITCH=1") {
		t.Error("E2E_TRAIN_SWITCH must only reach the restart step")
	}
	if su.Dir != g.Cfg.RepoRoot || !su.Session {
		t.Errorf("suite must run from the repo root in its own session: dir=%q session=%v", su.Dir, su.Session)
	}

	// Per-leg log: the raw stream including the non-JSON line, named by auth/mode.
	wantLog := g.Cfg.TmpDir + "/fabrik-e2e-app-on-" + strconv.Itoa(g.Self) + ".json"
	if observed.LogPath != wantLog {
		t.Errorf("log path = %q, want %q", observed.LogPath, wantLog)
	}
	if data, _ := os.ReadFile(wantLog); string(data) != passStream {
		t.Errorf("the log must hold every raw byte, got %q", data)
	}

	// Terminal: only JSON output events, each followed by jq -r's own newline.
	if !strings.Contains(out.String(), "=== RUN   TestSmoke\n\n") || strings.Contains(out.String(), "a build line") {
		t.Errorf("terminal echo wrong: %q", out)
	}
	for _, want := range []string{
		"== switching test bed to FABRIK_MERGE_TRAIN=on, auth=app (leg app/on) ==",
		"== running suite with E2E_TRAIN_MODE=on, E2E_AUTH_MODE=app, -parallel=2 (leg app/on) ==",
		"== GraphQL budget (leg: app/on): 4000 -> 1200 remaining (consumed 2800 pts) ==",
		"   (app leg: this is the harness's FABRIK_TOKEN budget only — the bed engine spends the App installation's own budget)",
		"== per-test wall-clock (leg: app/on), slowest first ==\n99s    pass  TestSlow\n12.5s  pass  TestSmoke\n",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("stdout missing %q:\n%s", want, out)
		}
	}

	// The outcome stream a ledger consumes.
	if observed.ExitCode != 0 || observed.BudgetBefore != 4000 || observed.BudgetAfter != 1200 || len(observed.Events) != 5 {
		t.Errorf("observed = %+v", observed)
	}
	if c := observed.Classification(); strings.Join(c.Pass, ",") != "TestSlow,TestSmoke" {
		t.Errorf("classification = %+v", c)
	}
}

func hasEnv(env []string, kv string) bool {
	for _, e := range env {
		if e == kv {
			return true
		}
	}
	return false
}

func TestLegSwitchFailureEndsTheRunWithItsExitCode(t *testing.T) {
	lf := &legFake{switchRC: 1, suiteOut: passStream}
	g, _, _ := newLegGate(t, lf)
	err := g.RunLeg(context.Background(), defaultCell)
	if exitCode(err) != 1 {
		t.Fatalf("want exit 1, got %v", err)
	}
	if len(lf.suiteCmds) != 0 {
		t.Error("the suite must not run after a failed restart step")
	}
}

func TestLegSuiteFailureClassifiesAndReturnsItsCode(t *testing.T) {
	lf := &legFake{suiteRC: 1, suiteOut: `{"Action":"run","Test":"TestA"}
{"Action":"fail","Test":"TestA","Elapsed":3}
{"Action":"run","Test":"TestB"}
{"Action":"pause","Test":"TestC"}
`}
	g, _, errb := newLegGate(t, lf)
	err := g.RunLeg(context.Background(), defaultCell)
	if exitCode(err) != 1 {
		t.Fatalf("want exit 1 (the suite's own), got %v", err)
	}
	for _, want := range []string{
		"== suite FAILED (leg: pat/off, exit 1) — classifying test outcomes ==",
		"JSON log: ",
		"completed - fail (1): TestA",
		"still running at kill time (1): TestB",
		"never started - queued behind -parallel cap (1): TestC",
	} {
		if !strings.Contains(errb.String(), want) {
			t.Errorf("stderr missing %q:\n%s", want, errb)
		}
	}
	if len(lf.ghCalls) != 0 {
		t.Errorf("an ordinary failure must not trigger teardown, got %v", lf.ghCalls)
	}
}

func TestLegTimeoutPanicTriggersBestEffortTeardown(t *testing.T) {
	lf := &legFake{suiteRC: 2, suiteOut: `{"Action":"run","Test":"TestA"}
{"Action":"output","Test":"TestA","Output":"panic: test timed out after 4h0m0s\n"}
`}
	g, _, errb := newLegGate(t, lf)
	err := g.RunLeg(context.Background(), defaultCell)
	if exitCode(err) != 2 {
		t.Fatalf("want exit 2, got %v", err)
	}
	if !strings.Contains(errb.String(), "== E2E_TIMEOUT kill detected (leg: pat/off) — running best-effort teardown ==") {
		t.Errorf("stderr: %s", errb)
	}
	if len(lf.ghCalls) == 0 || !strings.Contains(strings.Join(lf.ghCalls, "\n"), "pr list") {
		t.Errorf("teardown must run reset's gh calls, got %v", lf.ghCalls)
	}
	if !strings.Contains(errb.String(), "worktrees were NOT cleaned automatically") {
		t.Errorf("missing the worktree NOTE: %s", errb)
	}
}

func TestLegTeardownFailureIsOnlyAWarning(t *testing.T) {
	lf := &legFake{suiteRC: 2, suiteOut: `{"Action":"output","Test":"TestA","Output":"panic: test timed out after 1m\n"}` + "\n"}
	g, _, errb := newLegGate(t, lf)
	os.Remove(g.Cfg.TestBed + "/.env") // reset refuses: no bed
	err := g.RunLeg(context.Background(), defaultCell)
	if exitCode(err) != 2 {
		t.Fatalf("a failed teardown must not change the leg's exit code, got %v", err)
	}
	if !strings.Contains(errb.String(), "warning: automatic teardown failed; run scripts/e2e/reset.sh manually") {
		t.Errorf("stderr: %s", errb)
	}
}

// #1527: a throttled run can present as a pass as easily as a fail, so the
// backoff scan runs regardless of the suite's result, and ends the whole run.
func TestLegBackoffInvalidatesTheRunWhateverTheSuiteSaid(t *testing.T) {
	for _, suiteRC := range []int{0, 1} {
		lf := &legFake{suiteRC: suiteRC, suiteOut: passStream}
		g, _, errb := newLegGate(t, lf)
		mustWrite(t, g.Cfg.EngineLog, "x\n2026-08-11T09:00:00Z [warn] low (19%) — activating rate-limit backoff\n")
		var observed bool
		g.OnLeg = func(LegResult) { observed = true }
		err := g.RunLeg(context.Background(), defaultCell)
		if exitCode(err) != ExitBudgetExhausted {
			t.Fatalf("suite rc %d: want exit 3, got %v", suiteRC, err)
		}
		for _, want := range []string{"## RUN INVALID (leg: pat/off): GraphQL rate-limit backoff engaged mid-run.", "## Engine log: " + g.Cfg.EngineLog} {
			if !strings.Contains(errb.String(), want) {
				t.Errorf("stderr missing %q:\n%s", want, errb)
			}
		}
		if observed {
			t.Error("an invalidated leg must not be recorded as a result")
		}
	}
	// The per-poll "is low" line alone must not invalidate.
	lf := &legFake{suiteOut: passStream}
	g, _, _ := newLegGate(t, lf)
	mustWrite(t, g.Cfg.EngineLog, "GraphQL rate limit is low (25% remaining) — consider reducing poll frequency\n")
	if err := g.RunLeg(context.Background(), defaultCell); err != nil {
		t.Errorf("got %v", err)
	}
}

// R2 (#1676): the post-suite watchdog. A tail step that never returns is
// aborted with exit 6 and a diagnostic naming the stuck step. The fake probe
// blocks until its context is cancelled; with the probe's own deadline out of
// the way (the fake ignores Cmd.Timeout) only the watchdog can end it.
func TestLegPostSuiteWatchdogFires(t *testing.T) {
	lf := &legFake{suiteOut: passStream, budgets: []string{"4000", "HANG"}}
	g, _, errb := newLegGate(t, lf)
	g.Cfg.PostSuiteWatchdog = 300 * time.Millisecond
	mustWrite(t, g.Cfg.EngineLog, "first line\nlast engine line\n")

	start := time.Now()
	err := g.RunLeg(context.Background(), defaultCell)
	if exitCode(err) != ExitPostSuiteWatchdog {
		t.Fatalf("want exit 6, got %v", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Errorf("took %v", time.Since(start))
	}
	if !strings.Contains(errb.String(), "## POST-SUITE WATCHDOG (leg: pat/off): go test exited ") ||
		!strings.Contains(errb.String(), "## Stuck in: gh api rate_limit budget_after probe") ||
		!strings.Contains(errb.String(), "## Last engine log line: last engine line") {
		t.Errorf("diagnostic wrong:\n%s", errb)
	}
}

// The watchdog's neutralisation twin: the same hang with the watchdog out of the
// way (a huge window) is NOT cut short, so the case above is the watchdog and
// not the probe failing on its own.
func TestLegWatchdogTwinHangIsNotCutShortWithoutIt(t *testing.T) {
	lf := &legFake{suiteOut: passStream, budgets: []string{"4000", "HANG"}}
	g, _, _ := newLegGate(t, lf)
	g.Cfg.PostSuiteWatchdog = time.Hour
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- g.RunLeg(ctx, defaultCell) }()
	select {
	case err := <-done:
		if exitCode(err) == ExitPostSuiteWatchdog {
			t.Fatal("the watchdog fired with a one-hour window")
		}
		if err == nil || ctx.Err() == nil {
			t.Fatalf("the hung probe returned on its own: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancelling the run did not unwind the hung tail")
	}
}

// Healthy runs never see the watchdog: it costs nothing and never fires.
func TestLegWatchdogDoesNotFireOnAHealthyRun(t *testing.T) {
	lf := &legFake{suiteOut: passStream, budgets: []string{"4000", "3000"}}
	g, _, errb := newLegGate(t, lf)
	g.Cfg.PostSuiteWatchdog = 5 * time.Second
	if err := g.RunLeg(context.Background(), defaultCell); err != nil {
		t.Fatalf("got %v\n%s", err, errb)
	}
	if strings.Contains(errb.String(), "WATCHDOG") {
		t.Errorf("stderr: %s", errb)
	}
}

// R4 (#1676): the budget probe is a report, never a gate; a failure or an
// enforced timeout degrades to a warning that relays the diagnostic.
func TestLegBudgetProbeFailuresAreWarningsNeverGates(t *testing.T) {
	lf := &legFake{suiteOut: passStream, budgets: []string{"ERR", "TIMEOUT"}}
	g, _, errb := newLegGate(t, lf)
	if err := g.RunLeg(context.Background(), defaultCell); err != nil {
		t.Fatalf("a failed probe must not fail the leg: %v", err)
	}
	for _, want := range []string{
		"warning: budget_before (leg: pat/off) probe: gh: HTTP 502",
		"warning: budget_after (leg: pat/off) probe: with_timeout: command exceeded 2s, killed: gh api graphql",
		"warning: could not read GraphQL rate_limit before/after leg pat/off (gh api call failed) — skipping budget report",
	} {
		if !strings.Contains(errb.String(), want) {
			t.Errorf("stderr missing %q:\n%s", want, errb)
		}
	}
}

func TestLegBudgetProbeCommandIsScopedToTheBedToken(t *testing.T) {
	lf := &legFake{suiteOut: passStream, budgets: []string{"5", "4"}}
	g, fe, _, _ := testGate(t)
	fe.handler = lf.handle
	g.Cfg.BedToken = "bed-token"
	g.Env = append(g.Env, "GH_TOKEN=ambient-wrong-identity")
	if err := g.RunLeg(context.Background(), defaultCell); err != nil {
		t.Fatal(err)
	}
	var probes int
	for _, c := range fe.callsNamed("gh") {
		probes++
		if !hasEnv(c.Env, "GH_TOKEN=bed-token") || hasEnv(c.Env, "GH_TOKEN=ambient-wrong-identity") {
			t.Errorf("the probe must measure the bed's token, not the ambient gh identity: %v", c.Env)
		}
		if got := strings.Join(c.Args, " "); !strings.Contains(got, "api graphql") || !strings.Contains(got, "rateLimit { remaining }") || strings.Contains(got, "rate_limit") {
			t.Errorf("the probe must use GraphQL's inline rateLimit, never the dead REST gauge: %q", got)
		}
		if !c.Session || c.Timeout != g.Cfg.GHAPITimeout {
			t.Errorf("every network call must be bounded and reapable: %+v", c)
		}
	}
	if probes != 2 {
		t.Errorf("want a before and an after probe, got %d", probes)
	}
}

func TestLegNoBedTokenSkipsTheBudgetReport(t *testing.T) {
	lf := &legFake{suiteOut: passStream}
	g, fe, _, errb := testGate(t)
	fe.handler = lf.handle
	if err := g.RunLeg(context.Background(), defaultCell); err != nil {
		t.Fatal(err)
	}
	if len(fe.callsNamed("gh")) != 0 {
		t.Error("no token, no probe")
	}
	if !strings.Contains(errb.String(), "skipping budget report") {
		t.Errorf("stderr: %s", errb)
	}
}

func TestLegBudgetResetMidLegIsReported(t *testing.T) {
	lf := &legFake{suiteOut: passStream, budgets: []string{"100", "4900"}}
	g, out, _ := newLegGate(t, lf)
	if err := g.RunLeg(context.Background(), defaultCell); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "100 -> 4900 remaining (budget reset mid-leg; consumption not computable)") {
		t.Errorf("stdout: %s", out)
	}
	if strings.Contains(out.String(), "app leg") {
		t.Error("the app note belongs to app legs only")
	}
}

// #1694: a wedged output pipe is reported and the leg carries on with the
// suite's own result.
func TestLegWedgedPipeWarnsAndContinues(t *testing.T) {
	lf := &legFake{suiteOut: passStream, suiteWedge: true}
	g, _, errb := newLegGate(t, lf)
	if err := g.RunLeg(context.Background(), defaultCell); err != nil {
		t.Fatalf("a wedged drain must not fail the leg: %v", err)
	}
	if !strings.Contains(errb.String(), "warning: output consumer did not drain within 30s after go test exited (leg: pat/off).") ||
		!strings.Contains(errb.String(), "continuing to the next leg.") {
		t.Errorf("stderr: %s", errb)
	}
}

// R3 (#1676): the stall warning is advisory, names the last completed scenario,
// repeats once per window, and never touches the exit code.
func TestLegStallWarningIsAdvisory(t *testing.T) {
	lf := &legFake{
		suiteOut:  `{"Action":"pass","Test":"TestLastDone","Elapsed":1}` + "\n",
		suiteHook: func(Cmd) { time.Sleep(400 * time.Millisecond) }, // the suite then goes quiet
		budgets:   []string{"10", "9"},
	}
	g, _, errb := newLegGate(t, lf)
	g.Cfg.StallCheckInterval = 40 * time.Millisecond
	g.Cfg.StallWarn = 120 * time.Millisecond
	if err := g.RunLeg(context.Background(), defaultCell); err != nil {
		t.Fatalf("a stall warning must never fail the leg: %v", err)
	}
	if n := strings.Count(errb.String(), "== STALL WARNING (leg: pat/off): no new suite output for "); n < 2 {
		t.Errorf("want the warning once per window (>=2 over ~400ms), got %d:\n%s", n, errb)
	}
	if !strings.Contains(errb.String(), "Last completed scenario: TestLastDone ==") || !strings.Contains(errb.String(), "E2E_STALL_WARN_MINUTES=0") {
		t.Errorf("stderr: %s", errb)
	}
}

func TestLegNoStallWarningWhileOutputKeepsArriving(t *testing.T) {
	lf := &legFake{suiteOut: passStream}
	lf.suiteHook = func(c Cmd) {
		for i := 0; i < 8; i++ {
			writeStdout(c, `{"Action":"output","Test":"TestSmoke","Output":"tick\n"}`+"\n")
			time.Sleep(25 * time.Millisecond)
		}
	}
	g, _, errb := newLegGate(t, lf)
	g.Cfg.StallCheckInterval = 40 * time.Millisecond
	g.Cfg.StallWarn = 150 * time.Millisecond
	if err := g.RunLeg(context.Background(), defaultCell); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(errb.String(), "STALL WARNING") {
		t.Errorf("output kept arriving, no warning expected: %s", errb)
	}
}

func TestLegCancelledMidSuiteReturnsPromptly(t *testing.T) {
	lf := &legFake{suiteOut: passStream}
	ctx, cancel := context.WithCancel(context.Background())
	lf.suiteHook = func(Cmd) { cancel() }
	g, _, _ := newLegGate(t, lf)
	err := g.RunLeg(ctx, defaultCell)
	if err == nil || ctx.Err() == nil {
		t.Fatalf("want the cancellation, got %v", err)
	}
}

func TestSuiteWriterHandlesLinesSplitAcrossWritesAndAFinalPartialLine(t *testing.T) {
	var logb, term bytes.Buffer
	w := newSuiteWriter(&logb, &term, time.Now)
	for _, chunk := range []string{`{"Action":"out`, `put","Output":"he`, `llo\n"}` + "\n" + `{"Action":"pass","Test":"TestZ"}`} {
		if _, err := w.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if term.String() != "hello\n\n" {
		t.Errorf("term = %q", term.String())
	}
	if w.last() != "(none yet)" {
		t.Errorf("the partial final line is not yet consumed, last=%q", w.last())
	}
	w.Flush()
	if w.last() != "TestZ" {
		t.Errorf("Flush must process the final unterminated line, last=%q", w.last())
	}
	if logb.Len() == 0 {
		t.Error("the log must hold the raw bytes")
	}
}
