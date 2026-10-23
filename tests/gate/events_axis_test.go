package gate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/tests/e2e/bedevents"
)

// A leg's event source and poll cadence are the CELL's (#2426): a run-wide
// E2E_EVENT_SOURCE no longer leaks into a poll cell, and the planned cadence rides
// in its own variable.
func TestRunLegEnvironmentComesFromTheCell(t *testing.T) {
	run := func(t *testing.T, cell Cell) *legFake {
		lf := &legFake{suiteOut: passStream}
		g, _, _ := newLegGate(t, lf)
		g.Cfg.EventSource = bedevents.Tailscale // the deprecated run-wide input
		g.Delivery = &fakeDelivery{row: upRow(), hookURL: probeFunnelURL, deliver: true}
		g.Cfg.DeliveryProbeWait = 0
		_ = g.RunLeg(context.Background(), cell)
		return lf
	}
	poll := run(t, Cell{Auth: "app", Train: "on", Parallel: "2", PollSeconds: "60"})
	if len(poll.switchCmds) != 1 {
		t.Fatalf("switch calls: %d", len(poll.switchCmds))
	}
	for _, want := range []string{"E2E_EVENT_SOURCE=poll", "E2E_CELL_POLL_SECONDS=60"} {
		if !hasEnv(poll.switchCmds[0].Env, want) {
			t.Errorf("a poll cell's restart must carry %s", want)
		}
	}
	if hasEnv(poll.switchCmds[0].Env, "E2E_EVENT_SOURCE=tailscale") {
		t.Error("the run-wide event source must not reach a poll cell")
	}
	// A hand-built cell with no cadence gets the plan's 60 s, never the 180 s default.
	hand := run(t, Cell{Auth: "app", Train: "on", Parallel: "2"})
	if !hasEnv(hand.switchCmds[0].Env, "E2E_CELL_POLL_SECONDS=60") {
		t.Error("a cell with no cadence must carry the planned 60")
	}
	web := run(t, Cell{Auth: "app", Train: "on", Events: EventsWebhook, Parallel: "2", PollSeconds: "60"})
	if !hasEnv(web.switchCmds[0].Env, "E2E_EVENT_SOURCE=tailscale") || !hasEnv(web.switchCmds[0].Env, "E2E_CELL_POLL_SECONDS=60") {
		t.Errorf("a webhook cell's restart must carry the tailscale source at 60 s: %v", web.switchCmds[0].Env)
	}
	if len(poll.suiteCmds) == 0 || !hasEnv(poll.suiteCmds[0].Env, "E2E_CELL_POLL_SECONDS=60") {
		t.Error("the suite must see the cell's cadence too")
	}
}

// An operator override equal to the plan is not an override; any other value
// makes the leg run and archive but record nothing (FR-010).
func TestPollOverrideLegRecordsNothing(t *testing.T) {
	for _, tc := range []struct {
		name, override string
		recorded       bool
	}{
		{"no override", "", true},
		{"override equal to the plan", "60", true},
		{"override differing from the plan", "180", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := covFixture(t)
			f.g.Cfg.BedPollOverride = tc.override
			f.lf.suiteOut = stream(pass("TestAlpha"), pass("TestBravo"))
			var so strings.Builder
			f.g.Out = &so
			if code := f.g.Run(context.Background(), nil); code != 0 {
				t.Fatalf("exit = %d\n%s", code, so.String())
			}
			h := f.g.cov.inputs.hashes
			got := f.ledger(t).Load().Covered("pat/off", "TestAlpha", h["TestAlpha"])
			if got != tc.recorded {
				t.Errorf("recorded = %v, want %v", got, tc.recorded)
			}
			if banner := strings.Contains(so.String(), "POLL CADENCE OVERRIDE"); banner == tc.recorded {
				t.Errorf("override banner printed = %v, want %v\n%s", banner, !tc.recorded, so.String())
			}
			if len(f.lf.suiteCmds) == 0 {
				t.Error("an overridden leg still runs the suite")
			}
			b, err := os.ReadFile(filepath.Join(f.ledger(t).ArchiveCellDir("pat-off", f.g.cov.invocation), "phases.json"))
			if err != nil {
				t.Fatal(err)
			}
			wantOverride := tc.override != "" && tc.override != "60"
			if has := strings.Contains(string(b), `"poll_override"`); has != wantOverride {
				t.Errorf("phases.json poll_override present = %v, want %v\n%s", has, wantOverride, b)
			}
		})
	}
}

func TestCountIntakeLines(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "fabrik.log")
	before := "[intake] duplicate delivery old (issues via tailscale) — already applied\n"
	during := strings.Join([]string{
		"[intake] duplicate delivery a (issues via tailscale) — already applied",
		"[intake] duplicate delivery b (issue_comment via tailscale) — already applied",
		"[intake] intake queue full for repo#1 — dropping issues delivery c; reconcile will heal it",
		"[poll] nothing to do",
		"",
	}, "\n")
	mustWrite(t, p, before+during)
	d, dr := countIntakeLines(p, int64(len(before)))
	if d != 2 || dr != 1 {
		t.Errorf("deduped=%d dropped=%d, want 2/1 (lines before the offset are not the leg's)", d, dr)
	}
	// A replaced (shorter) log is read whole.
	mustWrite(t, p, "[intake] duplicate delivery z (x via y) — already applied\n")
	if d, dr := countIntakeLines(p, 1<<20); d != 1 || dr != 0 {
		t.Errorf("replaced log: deduped=%d dropped=%d", d, dr)
	}
	if d, dr := countIntakeLines(filepath.Join(dir, "missing"), 0); d != 0 || dr != 0 {
		t.Error("a missing log counts nothing")
	}
}

func TestWebhookLegSummaryRecordsDeliveryDedupeAndDropCounts(t *testing.T) {
	lf := &legFake{suiteOut: passStream}
	g, _, _ := newLegGate(t, lf)
	l := testLedger(t)
	g.cov = &covState{ledger: l, invocation: "inv-1", noRecord: true}
	now := time.Unix(5000, 0)
	g.Now = func() time.Time { return now }
	fd := &fakeDelivery{row: upRow(), hookURL: probeFunnelURL, deliver: true}
	g.Delivery = fd
	g.Cfg.DeliveryProbeWait = 0
	mustWrite(t, g.Cfg.TestBed+"/bed-run.log", "Fabrik starting 0.1.0 (abc)\n[tailscale] App webhook URL: "+probeFunnelURL+" (secret from $X)\n")
	mustWrite(t, g.Cfg.EngineLog, "[intake] duplicate delivery pre — already applied\n")
	// The engine log grows during the leg: the restart step appends the leg's lines.
	fe := g.Exec.(*fakeExec)
	inner := fe.handler
	fe.handler = func(ctx context.Context, c Cmd) Result {
		line := argsLine(c)
		switch {
		case c.Name == "gh" && strings.Contains(line, "POST repos/") && strings.HasSuffix(strings.Split(line, " -f ")[0], "/issues"):
			writeStdout(c, "42\n")
			return Result{}
		case c.Name == "gh" && strings.Contains(line, "/issues/42/labels"):
			fd.wrote(g.Now())
			return Result{}
		case c.Name == "gh" && strings.Contains(line, "/issues/42"):
			return Result{}
		}
		if isSwitch(c) {
			f, _ := os.OpenFile(g.Cfg.EngineLog, os.O_APPEND|os.O_WRONLY, 0o644)
			f.WriteString("[intake] duplicate delivery a (x via y) — already applied\n[intake] intake queue full for k — dropping x delivery b; reconcile will heal it\n")
			f.Close()
		}
		return inner(ctx, c)
	}
	cell := Cell{Auth: "app", Train: "on", Events: EventsWebhook, Parallel: "2", PollSeconds: "60"}
	if err := g.RunLeg(context.Background(), cell); err != nil {
		t.Fatalf("RunLeg: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(l.ArchiveCellDir("app-on-webhook", "inv-1"), "phases.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := strings.ReplaceAll(string(b), " ", "")
	for _, want := range []string{`"events":{`, `"events":"webhook"`, `"poll_seconds":"60"`, `"deduped":1`, `"dropped":1`, `"deliveries":`} {
		if !strings.Contains(strings.ReplaceAll(s, "\n", ""), want) {
			t.Errorf("phases.json lacks %s:\n%s", want, b)
		}
	}
	bed, _ := os.ReadFile(filepath.Join(l.ArchiveCellDir("app-on-webhook", "inv-1"), "bed.json"))
	if !strings.Contains(string(bed), `"events": "webhook"`) || !strings.Contains(string(bed), `"poll_seconds": "60"`) {
		t.Errorf("bed.json must record the events dimension and cadence: %s", bed)
	}
}

// webhookCovFixture is an app/on gate over a registry with one events-neutral and
// one events-sensitive test, so the sparse plan is the baseline plus app/on/webhook.
func webhookCovFixture(t *testing.T) *covFix {
	t.Helper()
	f := covFixture(t)
	f.g.Env = withEnv(f.g.Env, "E2E_AUTH_MODE=app", "E2E_TRAIN_MODE=on")
	mustWrite(t, f.g.Cfg.RepoRoot+"/tests/e2e/registry/registry.json", `{"version":1,"tests":[
{"name":"TestAlpha","parity":"gap","auth":"neutral","train":"neutral","events":"neutral"},
{"name":"TestBravo","parity":"gap","auth":"neutral","train":"neutral","events":"sensitive","events_reason":"wake-ups"}]}`)
	f.g.Cfg.Matrix = MatrixSparse
	f.g.Cfg.DeliveryProbeWait = 0
	return f
}

// A failing webhook preflight leaves the webhook pairs uncovered and the run
// non-zero, while the poll cell has run and been recorded (SC-002); `gate coverage`
// then lists the app/on/webhook pair as required and exits 8.
func TestWebhookPreflightFailureLeavesOnlyTheWebhookPairsUncovered(t *testing.T) {
	f := webhookCovFixture(t)
	f.g.Delivery = &fakeDelivery{rowErr: errors.New("daemon down"), hookURL: probeFunnelURL}
	f.lf.suiteOut = stream(pass("TestAlpha"), pass("TestBravo"))
	var so strings.Builder
	f.g.Out = &so
	code := f.g.Run(context.Background(), nil)
	if code != ExitPreflightFailed {
		t.Fatalf("exit = %d, want the preflight code %d\n%s", code, ExitPreflightFailed, so.String())
	}
	if len(f.lf.suiteCmds) != 1 {
		t.Fatalf("exactly the poll cell's suite must have run, got %d", len(f.lf.suiteCmds))
	}
	h := f.g.cov.inputs.hashes
	s := f.ledger(t).Load()
	if !s.Covered("app/on", "TestAlpha", h["TestAlpha"]) || !s.Covered("app/on", "TestBravo", h["TestBravo"]) {
		t.Errorf("the poll cell's results must be recorded: %+v", s.Latest)
	}
	if s.Covered("app/on/webhook", "TestBravo", h["TestBravo"]) {
		t.Error("a webhook pair must not be covered by a leg that failed its preflight")
	}

	var co strings.Builder
	f.g.Out = &co
	if code := f.g.Coverage(context.Background(), []string{"--sha", covSHA}); code != ExitCoverageIncomplete {
		t.Fatalf("coverage exit = %d, want %d\n%s", code, ExitCoverageIncomplete, co.String())
	}
	for _, want := range []string{"app/on/webhook", "TestBravo", "covered 2/2"} {
		if !strings.Contains(co.String(), want) {
			t.Errorf("coverage output lacks %q:\n%s", want, co.String())
		}
	}
}

// --resume after the webhook leg could not run: once the preflight passes, only
// the missing webhook pair runs, on the webhook cell.
func TestResumeRunsOnlyTheMissingWebhookPair(t *testing.T) {
	f := webhookCovFixture(t)
	f.g.Delivery = &fakeDelivery{rowErr: errors.New("daemon down"), hookURL: probeFunnelURL}
	f.lf.suiteOut = stream(pass("TestAlpha"), pass("TestBravo"))
	if code := f.g.Run(context.Background(), nil); code != ExitPreflightFailed {
		t.Fatalf("run 1 exit = %d", code)
	}

	// Run 2: the delivery path is healthy now.
	f.g.cov = nil
	fd := &fakeDelivery{row: upRow(), hookURL: probeFunnelURL, deliver: true}
	f.g.Delivery = fd
	mustWrite(t, f.g.Cfg.TestBed+"/bed-run.log", "Fabrik starting 0.1.0 (abc)\n[tailscale] App webhook URL: "+probeFunnelURL+" (secret from $X)\n")
	mustWrite(t, f.g.Cfg.TestBed+"/.env", "FABRIK_TOKEN=bed-token\nFABRIK_TAILSCALE_HOSTNAME=bed-a\n")
	fe := f.g.Exec.(*fakeExec)
	inner := fe.handler
	fe.handler = func(ctx context.Context, c Cmd) Result {
		line := argsLine(c)
		switch {
		case c.Name == "gh" && strings.Contains(line, "POST repos/") && strings.HasSuffix(strings.Split(line, " -f ")[0], "/issues"):
			writeStdout(c, "42\n")
			return Result{}
		case c.Name == "gh" && strings.Contains(line, "/issues/42/labels"):
			fd.wrote(f.g.Now())
			return Result{}
		case c.Name == "gh" && strings.Contains(line, "/issues/42"):
			return Result{}
		}
		return inner(ctx, c)
	}
	f.lf.suiteOut = stream(pass("TestBravo"))
	before := len(f.lf.suiteCmds)
	if code := f.g.Run(context.Background(), []string{"--resume"}); code != 0 {
		t.Fatalf("run 2 exit = %d", code)
	}
	if len(f.lf.suiteCmds) != before+1 {
		t.Fatalf("--resume must run exactly the webhook cell: %d -> %d suite runs", before, len(f.lf.suiteCmds))
	}
	got := strings.Join(lastSuite(f).Args, " ")
	if !strings.HasSuffix(got, "-run ^(TestBravo)$") {
		t.Errorf("--resume must select exactly the uncovered webhook pair: %s", got)
	}
	if !hasEnv(lastSuite(f).Env, "E2E_EVENT_SOURCE=tailscale") {
		t.Error("the resumed leg must be the webhook cell")
	}
	h := f.g.cov.inputs.hashes
	if !f.ledger(t).Load().Covered("app/on/webhook", "TestBravo", h["TestBravo"]) {
		t.Error("the webhook pair must now be covered")
	}
}

// Ledger records written before the events axis keep counting for the poll cells:
// a pre-change app-on.jsonl satisfies the same plan's poll cell.
func TestPreChangeLedgerRecordsStillCountForPollCells(t *testing.T) {
	f := webhookCovFixture(t)
	in, err := f.g.loadCoverageInputs()
	if err != nil {
		t.Fatal(err)
	}
	l, err := OpenLedger(f.g.Cfg.CoverageDir, covSHA, f.g.Now)
	if err != nil {
		t.Fatal(err)
	}
	// The file name and record shape a pre-#2426 gate wrote.
	for _, name := range []string{"TestAlpha", "TestBravo"} {
		if err := l.Append(Record{Test: name, Leg: "app/on", Cell: "app-on", Invocation: "old", Outcome: OutcomePass, Hash: in.hashes[name]}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(l.outcomesDir(), "app-on.jsonl")); err != nil {
		t.Fatalf("a poll leg's outcome file must stay app-on.jsonl: %v", err)
	}
	s := l.Load()
	for _, name := range []string{"TestAlpha", "TestBravo"} {
		if !s.Covered("app/on", name, in.hashes[name]) {
			t.Errorf("%s: a pre-change record must still cover app/on", name)
		}
		if s.Covered("app/on/webhook", name, in.hashes[name]) {
			t.Errorf("%s: a poll record must never cover the webhook cell", name)
		}
	}
}
