package gate

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// LegResult is what a leg that reached a normal post-suite result hands to
// Gate.OnLeg (not RUN INVALID, watchdog or restart-failure legs; see OnLeg): the typed outcome
// stream #1972's per-SHA ledger consumes, and where #1973 classifies
// INCONCLUSIVE.
type LegResult struct {
	Cell     Cell
	ExitCode int
	LogPath  string  // the per-leg `go test -json` log
	Events   []Event // the decoded stream (empty if the log could not be read)
	// BudgetBefore/After are the bed token's remaining GraphQL budget around the
	// suite invocation, or -1 when a probe failed.
	BudgetBefore, BudgetAfter int
}

// Classification buckets the leg's tests by last action.
func (r LegResult) Classification() Classification { return Classify(r.Events) }

// suiteWriter is the consumer of the suite's combined stdout+stderr. It is the
// Go replacement for run.sh's named-pipe `tee | jq` pipeline: every raw byte is
// appended to the per-leg log, and every JSON "output" event is echoed to the
// terminal (non-JSON lines — build errors, `go: downloading` — stay in the log
// only, exactly as `jq 'fromjson? // empty'` dropped them).
type suiteWriter struct {
	log  io.Writer
	term io.Writer

	mu            sync.Mutex
	buf           []byte
	lastCompleted string

	lastWrite atomic.Int64 // unix nanos of the most recent Write; the stall signal
	now       func() time.Time

	// sink, if set, observes every decoded event as it streams in — the coverage
	// ledger's recorder (#1972). It runs on the stream, not at leg end, so a leg
	// killed mid-way keeps what had already completed.
	sink func(Event)
}

func newSuiteWriter(log, term io.Writer, now func() time.Time) *suiteWriter {
	w := &suiteWriter{log: log, term: term, now: now, lastCompleted: "(none yet)"}
	w.lastWrite.Store(now().UnixNano())
	return w
}

func (w *suiteWriter) Write(p []byte) (int, error) {
	w.lastWrite.Store(w.now().UnixNano())
	if _, err := w.log.Write(p); err != nil {
		return 0, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		w.line(string(w.buf[:i]))
		w.buf = w.buf[i+1:]
	}
	return len(p), nil
}

// Flush handles a final line that had no newline.
func (w *suiteWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.buf) > 0 {
		w.line(string(w.buf))
		w.buf = nil
	}
}

func (w *suiteWriter) line(s string) {
	e, ok := DecodeEvent(s)
	if !ok {
		return
	}
	if terminal(e) {
		w.lastCompleted = e.Test
	}
	if w.sink != nil {
		w.sink(e)
	}
	if e.Action == "output" {
		// jq -r prints the string and then a newline of its own, so the terminal
		// has always been double-spaced; kept as-is (a quirk, not fixed here).
		fmt.Fprintf(w.term, "%s\n", e.Output)
	}
}

func (w *suiteWriter) last() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lastCompleted
}

// RunLeg is run.sh's switch_and_run, the behavioural core of the gate: switch
// the bed to the leg's train and auth mode (via the dedicated TestSwitchTrainMode
// invocation — a separate `go test` process, so the restart completes, bed fully
// back up, before the suite starts), run the suite, then do the post-suite
// bookkeeping. In order:
//
//  1. The restart step. A failure here ends the run. KNOWN GAP, preserved: it
//     has none of the classification/teardown machinery below — it is a fast
//     (~seconds), narrow check with its own fixed 3m timeout, and auto-tearing-
//     down a bed that may just be mid-restart rather than stuck would be worse.
//  2. The bed token's GraphQL budget before the suite (a report, never a gate).
//  3. The suite, `go test -json`, teed to a per-leg log under $TMPDIR, with a
//     stall warning (advisory) if its output goes quiet while it still runs.
//     (#1973) When tests in it ended INCONCLUSIVE, only those are re-run, in the
//     same cell and bed, up to E2E_INCONCLUSIVE_RETRIES times — before the
//     watchdog below starts and before the RUN INVALID scan, so a long retry is
//     not killed and throttling during one still voids the cell. What stays
//     inconclusive is uncovered, not failed (see Gate.withLeftInconclusive).
//  4. A post-suite watchdog over everything after `go test` exits (budget probe,
//     reports, backoff scan): if the tail has not finished within
//     E2E_POST_SUITE_WATCHDOG the leg is aborted with ExitPostSuiteWatchdog.
//  5. On a non-zero exit: the completed/still-running/never-started
//     classification and, if the log shows Go's own timeout panic, best-effort
//     teardown via Reset (a killed run's t.Cleanup never executes).
//  6. The rate-limit-backoff scan of the bed's fabrik.log, REGARDLESS of the
//     suite's result (a throttled run can present as a pass): a match prints the
//     RUN INVALID banner and ends the whole run with ExitBudgetExhausted,
//     skipping any remaining leg against a compromised budget.
//  7. Otherwise the suite's own exit code is returned.
func (g *Gate) RunLeg(ctx context.Context, cell Cell) error {
	mode := cell.Train
	label := cell.Label()
	legEnv := withEnv(g.Env, "E2E_AUTH_MODE="+cell.Auth)

	// #1972: when the coverage ledger is live, this leg's logs are archived and
	// its outcomes recorded as they stream in. The archive's sampler starts before
	// the restart step so the engine log is captured across the mode switch, and
	// Finish runs on EVERY exit path (a cancelled or killed leg included).
	cs := g.cov
	var arch *legArchive
	if cs != nil {
		cs.once.Do(func() {
			if err := cs.ledger.NoteInvocation(cs.invocation, cs.head, cs.resume); err != nil {
				g.errf("warning: cannot record this invocation in the coverage ledger: %v\n", err)
			}
		})
		arch = g.beginArchive(cell, cs)
		defer arch.Finish()
	}

	g.outf("== switching test bed to FABRIK_MERGE_TRAIN=%s, auth=%s (leg %s) ==\n", mode, cell.Auth, label)
	res := g.Exec.Run(ctx, Cmd{
		Name: "go", Args: []string{"test", "-tags=e2e", "-v", "-count=1", "-timeout", "3m", "-run", "^TestSwitchTrainMode$", "./tests/e2e/..."},
		Dir: g.Cfg.RepoRoot, Env: withEnv(legEnv, "E2E_TRAIN_SWITCH=1", "E2E_TRAIN_MODE="+mode),
		Stdout: g.Out, Stderr: g.Err, Session: true, Grace: g.Cfg.KillGrace,
	})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if res.ExitCode != 0 {
		return &ExitError{Code: res.ExitCode}
	}

	g.outf("== running suite with E2E_TRAIN_MODE=%s, E2E_AUTH_MODE=%s, -parallel=%s (leg %s) ==\n", mode, cell.Auth, cell.Parallel, label)
	// Without the ledger the log name carries no per-leg uniqueness beyond auth
	// and mode (the two "on" cells overwrite each other — the pre-#1972 quirk).
	// With it, the log lives in the leg's own archive directory.
	jsonlog := filepath.Join(g.Cfg.TmpDir, fmt.Sprintf("fabrik-e2e-%s-%s-%d.json", cell.Auth, mode, g.Self))
	if arch != nil && arch.dir != "" {
		jsonlog = filepath.Join(arch.dir, "go-test.json")
	}

	// Snapshot the bed's GraphQL budget right before the suite runs — purely for
	// the cost report, which should reflect the suite's own consumption, not the
	// restart step's (unlike the backoff scan, which deliberately covers the
	// restart too).
	budgetBefore := g.probeBudget(ctx, "budget_before", label)

	var recorder *legRecorder
	if cs != nil && !cs.noRecord {
		recorder = newLegRecorder(cs.ledger, cell, cs.invocation, cs.head, cs.inputs.hashes, cs.inputs.liveSet, g.errf)
	}

	suiteArgs := cat([]string{"test", "-tags=e2e", "-json", "-count=1", "-timeout", g.Cfg.Timeout, "-parallel", cell.Parallel, "./tests/e2e/..."}, cell.Args)
	sres, err := g.runSuiteAttempt(ctx, label, legEnv, mode, suiteArgs, jsonlog, recorder)
	if err != nil {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	rc := sres.ExitCode
	logs := []string{jsonlog}
	events, rerr := readEventsFile(jsonlog)
	var inc retryOutcome

	// #1973: bounded in-leg retry of the tests that declared themselves
	// INCONCLUSIVE. It runs BEFORE the post-suite watchdog starts (a legitimately
	// long retry must not be killed by a bound meant for seconds of bookkeeping)
	// and BEFORE the RUN INVALID scan in postSuiteTail (throttling during a retry
	// must still void the cell). Retries stop early on anything that makes the bed
	// state untrustworthy; see retryInconclusive.
	if rerr == nil {
		var rretry int
		events, logs, rretry, inc, err = g.retryInconclusive(ctx, cell, label, legEnv, mode, jsonlog, events, recorder)
		if err != nil {
			return err
		}
		if rretry != 0 && rc == 0 {
			rc = rretry // a retried test FAILED (or the retry run died): the leg is red
		}
	}
	if recorder != nil {
		n, failed := recorder.Counts()
		g.outf("== coverage ledger (leg: %s): recorded %d test outcome(s)", label, n)
		if failed > 0 {
			g.outf(", %d FAILED to record", failed)
		}
		g.outln(" ==")
	}
	suiteExit := g.Now()

	// R2 (#1676): post-suite watchdog. From here through this function's return
	// is bookkeeping that normally takes seconds. The v0.0.81 cut hung ~17 hours
	// inside exactly this tail (unbounded `gh api` probes) with go test long gone
	// and nothing to say so. It is a backstop against a future regression — a
	// step added to this tail without a bound — not the expected path.
	var checkpoint atomic.Value
	checkpoint.Store("waiting for the suite's output to drain")
	tailCtx, cancelTail := context.WithCancel(ctx)
	defer cancelTail()
	result := make(chan error, 1)
	go func() {
		result <- g.postSuiteTail(tailCtx, cell, label, logs, events, rerr, inc, rc, budgetBefore, &checkpoint)
	}()
	timer := time.NewTimer(g.Cfg.PostSuiteWatchdog)
	defer timer.Stop()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		cancelTail()
		<-result
		return ctx.Err()
	case <-timer.C:
		var b strings.Builder
		b.WriteString("\n############################################################\n")
		fmt.Fprintf(&b, "## POST-SUITE WATCHDOG (leg: %s): go test exited %ds ago,\n", label, int(g.Now().Sub(suiteExit).Seconds()))
		b.WriteString("## but the script has not progressed past its post-suite steps\n")
		fmt.Fprintf(&b, "## within %ds (E2E_POST_SUITE_WATCHDOG).\n", int(g.Cfg.PostSuiteWatchdog.Seconds()))
		fmt.Fprintf(&b, "## Stuck in: %s\n", checkpoint.Load())
		fmt.Fprintf(&b, "## Last engine log line: %s\n", lastLogLine(g.Cfg.EngineLog))
		b.WriteString("############################################################\n")
		g.errf("%s", b.String())
		// Reap whatever the stuck step is waiting on, then give it a bounded
		// moment to unwind so no goroutine outlives the leg.
		cancelTail()
		select {
		case <-result:
		case <-time.After(5 * time.Second):
		}
		return &ExitError{Code: ExitPostSuiteWatchdog}
	}
}

// runSuiteAttempt is one `go test -json` invocation of the leg's suite: it
// tees the raw stream to logPath, echoes output events to the terminal, feeds the
// ledger recorder (if any) as events stream in, and runs the advisory stall
// detector for the attempt's lifetime. The first attempt and each #1973 retry go
// through it, so a retry is exactly as observable — and as recorded — as the
// suite itself. A non-nil error means the attempt could not even be set up.
func (g *Gate) runSuiteAttempt(ctx context.Context, label string, legEnv []string, mode string, args []string, logPath string, recorder *legRecorder) (Result, error) {
	logf, err := os.Create(logPath)
	if err != nil {
		return Result{}, exitErr(1, "gate: cannot create the leg log %s: %v", logPath, err)
	}
	defer logf.Close()
	w := newSuiteWriter(logf, g.Out, g.Now)
	if recorder != nil {
		w.sink = recorder.Observe
	}

	// R3 (#1676): stall detector — advisory only, never touches the exit code.
	stallCtx, stopStall := context.WithCancel(ctx)
	var stallDone sync.WaitGroup
	stallDone.Add(1)
	go func() {
		defer stallDone.Done()
		g.watchStall(stallCtx, label, w)
	}()

	drain := g.Cfg.PostSuiteDrainTimeout
	if drain <= 0 {
		drain = time.Millisecond
	}
	sres := g.Exec.Run(ctx, Cmd{
		Name: "go", Args: args, Dir: g.Cfg.RepoRoot, Env: withEnv(legEnv, "E2E_TRAIN_MODE="+mode),
		Stdout: w, Stderr: w, Session: true, Grace: g.Cfg.KillGrace, WaitDelay: drain,
	})
	w.Flush()
	// go test has exited — stop the stall detector immediately.
	stopStall()
	stallDone.Wait()
	if ctx.Err() != nil {
		return sres, nil
	}
	if sres.PipeWedged {
		// Something that outlived go test (the detached bed was the real-world
		// case, #1694) is holding the output pipe open. go test has exited, so its
		// JSON log is complete and this leg's results are unaffected.
		g.errf("warning: output consumer did not drain within %ds after go test exited (leg: %s).\n", int(drain.Seconds()), label)
		g.errln("         Something that outlived go test is holding the output pipe open — find the leaked")
		g.errln("         descriptor with lsof. go test has already exited, so its JSON log is complete and")
		g.errln("         this leg's results are unaffected; continuing to the next leg.")
	}
	return sres, nil
}

// retryInconclusive is R3 (#1973): re-run the leg's INCONCLUSIVE tests, and only
// those, up to Config.InconclusiveRetries times in the same cell — same env,
// -parallel and bed, no restart, each attempt in its own log. It returns the
// leg's merged event stream (last attempt wins per test), every log written, the
// exit code of a retry that went RED (0 if none did), and what the retries
// amounted to.
//
// Only a test that declared itself inconclusive is ever retried; a failure is
// never retried. No retry is attempted when:
//   - retries are disabled (E2E_INCONCLUSIVE_RETRIES=0) or nothing is inconclusive;
//   - the suite did not finish (a test still running or never started — a timeout
//     kill leaves the bed in an unknown state);
//   - the cell's -run/-skip narrows to subtests (rewriting the selection to
//     top-level names would widen it);
//   - the engine's rate-limit backoff has already engaged (the run is RUN INVALID;
//     postSuiteTail will say so and void the cell — more spend would only add to it).
func (g *Gate) retryInconclusive(ctx context.Context, cell Cell, label string, legEnv []string, mode, firstLog string, events []Event, recorder *legRecorder) (merged []Event, logs []string, retryRC int, out retryOutcome, err error) {
	merged, logs = events, []string{firstLog}
	cls := Classify(merged)
	first := cls.Inconclusive
	if len(first) == 0 {
		return merged, logs, 0, out, nil
	}
	out = summarizeRetries(first, cls, 0)
	defer func() {
		out = summarizeRetries(first, Classify(merged), out.Attempts)
		g.outf("%s", out.Summary(label, g.Cfg.InconclusiveWarn))
	}()

	skip := func(why string) {
		g.outf("== inconclusive (leg: %s): not retrying — %s ==\n", label, why)
	}
	switch {
	case g.Cfg.InconclusiveRetries <= 0:
		skip("retries disabled (E2E_INCONCLUSIVE_RETRIES=0)")
		return
	case suiteIncomplete(cls) || fileContains(firstLog, "panic: test timed out after"):
		skip("the suite did not complete, so the bed state is unknown")
		return
	case hasSubtestFilter(cell.Args):
		skip("the cell's -run/-skip narrows to subtests")
		return
	case DetectRateLimitBackoff(g.Cfg.EngineLog):
		skip("the engine's rate-limit backoff engaged (RUN INVALID)")
		return
	}

	for n := 1; n <= g.Cfg.InconclusiveRetries; n++ {
		cls = Classify(merged)
		if len(cls.Inconclusive) == 0 {
			return
		}
		sel, serr := rewriteSelection(cell.Args, cls.Inconclusive)
		if serr != nil {
			g.errf("warning: cannot retry the inconclusive tests (leg: %s): %v\n", label, serr)
			return
		}
		logPath := retryLogPath(firstLog, n)
		g.outf("== retrying %d inconclusive test(s) (leg: %s, attempt %d of %d): %s ==\n", len(cls.Inconclusive), label, n, g.Cfg.InconclusiveRetries, strings.Join(cls.Inconclusive, ", "))
		args := cat([]string{"test", "-tags=e2e", "-json", "-count=1", "-timeout", g.Cfg.Timeout, "-parallel", cell.Parallel, "./tests/e2e/..."}, sel)
		sres, rerr := g.runSuiteAttempt(ctx, label, legEnv, mode, args, logPath, recorder)
		if rerr != nil {
			err = rerr
			return
		}
		if ctx.Err() != nil {
			err = ctx.Err()
			return
		}
		out.Attempts = n
		logs = append(logs, logPath)
		revents, rerr := readEventsFile(logPath)
		if rerr != nil {
			g.errf("warning: cannot read the retry log %s: %v — stopping the retries\n", logPath, rerr)
			if sres.ExitCode != 0 {
				retryRC = sres.ExitCode
			}
			return
		}
		merged = mergeAttempts(merged, revents)
		if sres.ExitCode != 0 {
			retryRC = sres.ExitCode // a retried test failed, or the retry run died
			return
		}
		if suiteIncomplete(Classify(merged)) {
			return
		}
		if DetectRateLimitBackoff(g.Cfg.EngineLog) {
			return // postSuiteTail voids the cell
		}
	}
	return
}

// lastLogLine is `tail -n1 file`, or "(unavailable)".
func lastLogLine(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "(unavailable)"
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	return lines[len(lines)-1]
}

// probeBudget reads the bed token's remaining GraphQL budget, reporting a
// failure as a warning and returning -1 (R4, #1676: a report, never a gate).
func (g *Gate) probeBudget(ctx context.Context, which, label string) int {
	if g.Cfg.BedToken == "" {
		return -1
	}
	n, stderr, err := g.GraphQLBudgetRemaining(ctx, g.Cfg.BedToken)
	if err != nil {
		g.reportProbeFailure(stderr, fmt.Sprintf("%s (leg: %s)", which, label))
		return -1
	}
	return n
}

// watchStall is R3 (#1676): independently of go test exiting, warn — never abort
// — if the suite's own combined output has gone quiet for E2E_STALL_WARN_MINUTES
// WHILE go test is still running, naming the last completed scenario. A real
// scenario can legitimately wait on Claude for extended periods, so silence
// alone is never treated as a hang, only surfaced so it is never mistaken for
// progress either. It warns once per window rather than once ever or on every
// check, so a deliberately idle leg (the isolated runaway-guard scenario) warns
// each window. The signal is the time of the last output write; bash polled the
// log file's mtime, which is the same thing observed from outside.
func (g *Gate) watchStall(ctx context.Context, label string, w *suiteWriter) {
	interval := g.Cfg.StallCheckInterval
	if interval <= 0 {
		return
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	var prev int64
	var stall time.Duration
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		cur := w.lastWrite.Load()
		if cur == prev {
			stall += interval
		} else {
			stall = 0
		}
		prev = cur
		if stall >= g.Cfg.StallWarn {
			g.errf("== STALL WARNING (leg: %s): no new suite output for %ds (E2E_STALL_WARN_MINUTES=%d) — go test is still running. Last completed scenario: %s ==\n",
				label, int(stall.Seconds()), int(g.Cfg.StallWarn.Minutes()), w.last())
			stall = 0
		}
	}
}

// postSuiteTail is everything after `go test` exits. Each step updates the
// watchdog's checkpoint first so a firing watchdog can name the stuck step.
//
// logs are every `go test -json` log of the leg (the first attempt, then each
// #1973 retry); events is the merged stream the reports and OnLeg are built from
// (rerr non-nil: the first log could not be read). inc is what the retries of the
// leg's inconclusive tests amounted to.
func (g *Gate) postSuiteTail(ctx context.Context, cell Cell, label string, logs []string, events []Event, rerr error, inc retryOutcome, rc, budgetBefore int, checkpoint *atomic.Value) error {
	jsonlog := logs[0]
	checkpoint.Store("gh api rate_limit budget_after probe")
	budgetAfter := g.probeBudget(ctx, "budget_after", label)
	if ctx.Err() != nil {
		return ctx.Err() // cancelled (a signal or the watchdog): print nothing more
	}
	if budgetBefore >= 0 && budgetAfter >= 0 {
		if budgetAfter <= budgetBefore {
			g.outf("== GraphQL budget (leg: %s): %d -> %d remaining (consumed %d pts) ==\n", label, budgetBefore, budgetAfter, budgetBefore-budgetAfter)
			if cell.Auth == "app" {
				g.outln("   (app leg: this is the harness's FABRIK_TOKEN budget only — the bed engine spends the App installation's own budget)")
			}
		} else {
			g.outf("== GraphQL budget (leg: %s): %d -> %d remaining (budget reset mid-leg; consumption not computable) ==\n", label, budgetBefore, budgetAfter)
		}
	} else {
		g.errf("warning: could not read GraphQL rate_limit before/after leg %s (gh api call failed) — skipping budget report\n", label)
	}

	checkpoint.Store("report_test_timings")
	g.outf("== per-test wall-clock (leg: %s), slowest first ==\n", label)
	if rerr != nil {
		g.errf("warning: failed to compute test timings (read error) — inspect the raw JSON log directly: %s\n", jsonlog)
	} else {
		g.outf("%s", FormatTimings(Timings(events)))
	}

	if rc != 0 {
		checkpoint.Store(fmt.Sprintf("failure classification / teardown (leg %s failed, rc=%d)", label, rc))
		g.errf("== suite FAILED (leg: %s, exit %d) — classifying test outcomes ==\n", label, rc)
		g.errf("JSON log: %s\n", jsonlog)
		if rerr != nil {
			g.errf("warning: failed to classify test outcomes (read error) — inspect the raw JSON log directly: %s\n", jsonlog)
		} else {
			g.errf("%s\n", Classify(events).Report())
		}
		// KNOWN LIMITATION, preserved: a literal text match against the whole log,
		// not scoped to output from the outer `go test` process. No current e2e
		// scenario shells out to a nested `go test` (checked via grep across
		// tests/e2e/*.go), so nothing today could print this string other than the
		// outer suite's own -timeout kill. If one ever does, this misfires and
		// triggers teardown on a run that was not actually a timeout kill.
		if fileContains(jsonlog, "panic: test timed out after") {
			g.errf("== E2E_TIMEOUT kill detected (leg: %s) — running best-effort teardown ==\n", label)
			if err := g.Reset(ctx, ResetOptions{}); err != nil {
				g.errln("warning: automatic teardown failed; run scripts/e2e/reset.sh manually")
			}
			g.errln("NOTE: worktrees were NOT cleaned automatically (that requires stopping the bed first).")
			g.errln("      Run scripts/e2e/reset.sh --worktrees for full parity before the next release-gate run.")
		}
	}

	// #1527: did the engine's own rate-limit backoff engage at any point during
	// this leg, regardless of rc? Once it does, the engine's polling slows enough
	// that items sit past scenarios' wait deadlines, producing a wall of timeouts
	// indistinguishable from real regressions.
	checkpoint.Store("detect_rate_limit_backoff scan")
	if DetectRateLimitBackoff(g.Cfg.EngineLog) {
		var b strings.Builder
		b.WriteString("\n############################################################\n")
		fmt.Fprintf(&b, "## RUN INVALID (leg: %s): GraphQL rate-limit backoff engaged mid-run.\n", label)
		b.WriteString("## Any test failures/timeouts above may be throttling artifacts, not real\n")
		b.WriteString("## regressions — this run's verdict cannot be trusted.\n")
		b.WriteString("##\n")
		fmt.Fprintf(&b, "## Engine log: %s\n", g.Cfg.EngineLog)
		b.WriteString("## (look for 'activating rate-limit backoff' for the exact event(s))\n")
		b.WriteString("##\n")
		b.WriteString("## See tests/e2e/README.md's GraphQL budget section for mitigation\n")
		b.WriteString("## (E2E_PARALLEL_ON, splitting the leg across budget windows).\n")
		b.WriteString("############################################################\n")
		g.errf("%s", b.String())
		// #1972: this verdict cannot be trusted, so nothing this cell recorded
		// counts — not even its PASSes. Fail closed.
		if g.cov != nil {
			if err := g.cov.ledger.Void(label, cellDirName(cell), g.cov.invocation); err != nil {
				g.errf("warning: could not void this cell's ledger records (%v) — they may count as coverage\n", err)
			} else {
				g.errf("== the coverage ledger discarded every outcome this cell recorded (RUN INVALID) ==\n")
			}
		}
		return &ExitError{Code: ExitBudgetExhausted}
	}

	// #1973: what is STILL inconclusive after the retries is uncovered, not failed
	// — remembered so the invocation cannot end reading as success. Recorded only
	// after the RUN INVALID check above, which discards the cell's verdict whole.
	g.noteLeftInconclusive(label, inc.Still)

	if g.OnLeg != nil {
		g.OnLeg(LegResult{Cell: cell, ExitCode: rc, LogPath: jsonlog, Events: events, BudgetBefore: budgetBefore, BudgetAfter: budgetAfter})
	}
	if rc != 0 {
		return &ExitError{Code: rc}
	}
	return nil
}

func readEventsFile(path string) ([]Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ReadEvents(f)
}

func anyFileContains(paths []string, needle string) bool {
	for _, p := range paths {
		if fileContains(p, needle) {
			return true
		}
	}
	return false
}
