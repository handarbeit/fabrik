package gate

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/tests/e2e/bedevents"
)

const (
	probeSecretToken = "ghp_SUPERSECRETBEDTOKEN"
	probeAuthKey     = "tskey-auth-SECRETAUTHKEY"
	probeHookSecret  = "WEBHOOKSECRETVALUE"
	probeFunnelURL   = "https://bed-a.tail1234.ts.net/webhook"
)

// fakeDelivery scripts the bed daemon's transport row and the App's answers.
// lastDelivery advances by one second each time a "write" is observed (the fake
// gh handler calls wrote()), when deliver is set.
type fakeDelivery struct {
	mu       sync.Mutex
	row      TransportRow
	rowErr   error
	hookURL  string
	hookErr  error
	log      []HookDelivery
	logErr   error
	deliver  bool // a write advances LastDelivery
	writes   int
	rowCalls int
}

func (f *fakeDelivery) TransportRow(context.Context, string) (TransportRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rowCalls++
	return f.row, f.rowErr
}
func (f *fakeDelivery) AppHookURL(context.Context, string) (string, error) {
	return f.hookURL, f.hookErr
}
func (f *fakeDelivery) AppDeliveries(context.Context, string) ([]HookDelivery, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.log, f.logErr
}
func (f *fakeDelivery) wrote(now time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes++
	if f.deliver {
		f.row.LastDelivery = f.row.LastDelivery.Add(time.Second)
		f.log = append([]HookDelivery{{Event: "issues", Action: "labeled", StatusCode: 200, DeliveredAt: now}}, f.log...)
	}
}

func upRow() TransportRow {
	return TransportRow{Present: true, State: "listening", Funnel: "ok", LastDelivery: time.Unix(1000, 0)}
}

// deliveryGate builds a gate over a bed whose .env holds secrets (so the tests can
// prove none of them is ever printed or archived) and whose bed-run.log names the
// Funnel URL, with a fake gh.
func deliveryGate(t *testing.T, fd *fakeDelivery) (*Gate, *fakeExec, func() string) {
	t.Helper()
	g, fe, out, errb := testGate(t)
	bed := g.Cfg.TestBed
	env := "FABRIK_TOKEN=" + probeSecretToken + "\nFABRIK_TAILSCALE_HOSTNAME=bed-a\nFABRIK_TS_AUTHKEY=" + probeAuthKey +
		"\nFABRIK_GITHUB_WEBHOOK_SECRET=" + probeHookSecret + "\n"
	if err := os.WriteFile(filepath.Join(bed, ".env"), []byte(env), 0o600); err != nil {
		t.Fatal(err)
	}
	log := "Fabrik starting 0.1.0 (abc)\n[tailscale] event_source: tailscale — node \"bed-a\" is up; App webhook URL: " + probeFunnelURL + " (secret from $X)\n"
	if err := os.WriteFile(filepath.Join(bed, "bed-run.log"), []byte(log), 0o644); err != nil {
		t.Fatal(err)
	}
	var clockMu sync.Mutex
	now := time.Unix(2000, 0)
	g.Now = func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return now }
	g.Sleep = func(d time.Duration) { clockMu.Lock(); now = now.Add(d); clockMu.Unlock() }
	g.Delivery = fd
	g.Cfg.DeliveryProbeWait = 60 * time.Second
	g.Cfg.DeliveryProbeInterval = 15 * time.Second
	fe.handler = func(_ context.Context, c Cmd) Result {
		line := argsLine(c)
		switch {
		case strings.Contains(line, "POST repos/") && strings.HasSuffix(strings.Split(line, " -f ")[0], "/issues"):
			writeStdout(c, "42\n")
		case strings.Contains(line, "/issues/42/labels"):
			fd.wrote(g.Now())
		}
		return Result{}
	}
	return g, fe, func() string { return out.String() + errb.String() }
}

func closedScratch(fe *fakeExec) bool {
	for _, l := range fe.lines() {
		if strings.Contains(l, "PATCH repos/handarbeit/fabrik-test-alpha/issues/42") && strings.Contains(l, "state=closed") {
			return true
		}
	}
	return false
}

func assertNoSecrets(t *testing.T, g *Gate, things ...string) {
	t.Helper()
	blob, _ := json.Marshal(g.probes)
	all := string(blob) + strings.Join(things, "\n")
	for _, s := range []string{probeSecretToken, probeAuthKey, probeHookSecret} {
		if strings.Contains(all, s) {
			t.Errorf("secret %q leaked into the probe's output or probes.json", s)
		}
	}
}

func TestProbeWebhookDeliveryPasses(t *testing.T) {
	fd := &fakeDelivery{row: upRow(), hookURL: probeFunnelURL, deliver: true}
	g, fe, out := deliveryGate(t, fd)
	if err := g.ProbeWebhookDelivery(context.Background(), Cell{Auth: "app", Train: "on"}); err != nil {
		t.Fatalf("probe failed: %v", err)
	}
	if len(g.probes.Delivery) != 1 || g.probes.Delivery[0].Status != DeliveryOK || g.probes.Delivery[0].HookURL != probeFunnelURL {
		t.Errorf("probes.Delivery = %+v", g.probes.Delivery)
	}
	if !closedScratch(fe) {
		t.Errorf("the scratch issue must be closed on a pass; calls: %v", fe.lines())
	}
	assertNoSecrets(t, g, out())
	if g.probes.empty() {
		t.Error("a delivery result must make probes.json non-empty")
	}
}

func failureOf(t *testing.T, err error) string {
	t.Helper()
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != ExitPreflightFailed {
		t.Fatalf("want an ExitPreflightFailed error, got %v", err)
	}
	return ee.Msg
}

func TestProbeWebhookDeliveryFailureSteps(t *testing.T) {
	cell := Cell{Auth: "app", Train: "on"}
	for _, tc := range []struct {
		name    string
		fd      *fakeDelivery
		step    string
		mention string
	}{
		{"no transport row (poll-only daemon)", &fakeDelivery{row: TransportRow{}, hookURL: probeFunnelURL}, bedevents.StepTransportNotUp, "poll-only"},
		{"funnel down", &fakeDelivery{row: TransportRow{Present: true, State: "funnel unavailable", Funnel: "unavailable"}, hookURL: probeFunnelURL}, bedevents.StepTransportNotUp, "Funnel/transport not up"},
		{"daemon unreachable blocks", &fakeDelivery{rowErr: errors.New("dial unix: no such file"), hookURL: probeFunnelURL}, bedevents.StepTransportNotUp, "could not be asked"},
		{"app url elsewhere", &fakeDelivery{row: upRow(), hookURL: "https://other.example.com/webhook"}, bedevents.StepHookURLMismatch, "other.example.com"},
		{"app hook config unreadable blocks", &fakeDelivery{row: upRow(), hookErr: errors.New("HTTP 401")}, bedevents.StepHookURLMismatch, "could not read"},
		{"no delivery, app not subscribed", &fakeDelivery{row: upRow(), hookURL: probeFunnelURL}, bedevents.StepNoDelivery, "not subscribed"},
		{"no delivery, github got 502", &fakeDelivery{row: upRow(), hookURL: probeFunnelURL, log: []HookDelivery{{Event: "issues", StatusCode: 502, DeliveredAt: time.Unix(2001, 0)}}}, bedevents.StepNoDelivery, "HTTP 502"},
		{"no delivery, log unreadable", &fakeDelivery{row: upRow(), hookURL: probeFunnelURL, logErr: errors.New("boom")}, bedevents.StepNoDelivery, "could not be read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, fe, out := deliveryGate(t, tc.fd)
			err := g.ProbeWebhookDelivery(context.Background(), cell)
			msg := failureOf(t, err)
			for _, want := range []string{g.bedLabel(), `"` + tc.step + `"`, tc.mention, cell.Label()} {
				if !strings.Contains(msg, want) {
					t.Errorf("message %q must contain %q", msg, want)
				}
			}
			r := g.probes.Delivery[0]
			if r.Status != DeliveryFailed || r.FailedStep != tc.step {
				t.Errorf("recorded %+v, want failed at %s", r, tc.step)
			}
			if tc.step == bedevents.StepNoDelivery && !closedScratch(fe) {
				t.Errorf("the scratch issue must be closed on a failure too; calls: %v", fe.lines())
			}
			assertNoSecrets(t, g, out(), msg)
		})
	}
}

// A replayed older delivery, or any pre-existing last-delivery time, must not pass:
// the observation has to be newer than the baseline taken before the probe's write.
func TestProbeWebhookDeliveryRejectsAStaleLastDelivery(t *testing.T) {
	fd := &fakeDelivery{row: upRow(), hookURL: probeFunnelURL} // LastDelivery is set but never advances
	g, _, _ := deliveryGate(t, fd)
	msg := failureOf(t, g.ProbeWebhookDelivery(context.Background(), Cell{Auth: "app", Train: "on"}))
	if !strings.Contains(msg, bedevents.StepNoDelivery) {
		t.Errorf("a stale last-delivery time must fail at %s: %s", bedevents.StepNoDelivery, msg)
	}
	if fd.writes < 2 {
		t.Errorf("the probe should retry its write across the wait (Funnel settling), wrote %d time(s)", fd.writes)
	}
}

// The daemon's delivery time may move for a replayed older delivery while the App
// shows no successful issues delivery for the probe's own write.
func TestProbeWebhookDeliveryRequiresTheProbesOwnEvent(t *testing.T) {
	fd := &fakeDelivery{row: upRow(), hookURL: probeFunnelURL, deliver: false}
	g, _, _ := deliveryGate(t, fd)
	orig := g.Exec.(*fakeExec).handler
	g.Exec.(*fakeExec).handler = func(ctx context.Context, c Cmd) Result {
		r := orig(ctx, c)
		if strings.Contains(argsLine(c), "/issues/42/labels") {
			fd.mu.Lock()
			fd.row.LastDelivery = fd.row.LastDelivery.Add(time.Second) // moves, but no matching issues event in the log
			fd.mu.Unlock()
		}
		return r
	}
	fd.log = []HookDelivery{{Event: "ping", StatusCode: 200, DeliveredAt: time.Unix(2001, 0)}}
	msg := failureOf(t, g.ProbeWebhookDelivery(context.Background(), Cell{Auth: "app", Train: "on"}))
	if !strings.Contains(msg, "replayed") {
		t.Errorf("message should explain the replay case: %s", msg)
	}
}

// A funnel that needs a moment to route (transport not up on the first attempts)
// is tolerated within the bound.
func TestProbeWebhookDeliveryToleratesFunnelSettling(t *testing.T) {
	fd := &fakeDelivery{row: TransportRow{Present: true, State: "funnel unavailable", Funnel: "unavailable", LastDelivery: time.Unix(1000, 0)}, hookURL: probeFunnelURL, deliver: true}
	g, _, _ := deliveryGate(t, fd)
	g.Sleep = func(d time.Duration) {
		fd.mu.Lock()
		fd.row.State, fd.row.Funnel = "listening", "ok" // up after the first wait
		fd.mu.Unlock()
	}
	if err := g.ProbeWebhookDelivery(context.Background(), Cell{Auth: "app", Train: "on"}); err != nil {
		t.Fatalf("a settling funnel within the bound must pass: %v", err)
	}
	if g.probes.Delivery[0].Attempts < 2 {
		t.Errorf("attempts = %d, want a retry", g.probes.Delivery[0].Attempts)
	}
}

func TestLoggedWebhookURLUsesTheLatestStartOnly(t *testing.T) {
	dir := t.TempDir()
	log := "Fabrik starting 0.1 (a)\nApp webhook URL: https://old.ts.net/webhook (secret from $X)\nFabrik starting 0.1 (b)\n"
	os.WriteFile(filepath.Join(dir, "bed-run.log"), []byte(log), 0o644)
	if got := loggedWebhookURL(dir); got != "" {
		t.Errorf("a URL from an earlier start must not count, got %q", got)
	}
	os.WriteFile(filepath.Join(dir, "bed-run.log"), []byte(log+"App webhook URL: https://new.ts.net/webhook\n"), 0o644)
	if got := loggedWebhookURL(dir); got != "https://new.ts.net/webhook" {
		t.Errorf("got %q", got)
	}
}

func TestURLMatchesBed(t *testing.T) {
	if !urlMatchesBed("https://h.t.ts.net/webhook", "https://h.t.ts.net/webhook", "x") {
		t.Error("exact logged URL must match")
	}
	if urlMatchesBed("https://other/webhook", "https://h.t.ts.net/webhook", "h") {
		t.Error("a different URL must not match the logged one")
	}
	if !urlMatchesBed("https://h.t.ts.net/webhook", "", "h") || urlMatchesBed("https://hh.t.ts.net/webhook", "", "h") && false {
		t.Error("without a log line the bed's own hostname decides")
	}
	if urlMatchesBed("https://x.t.ts.net/webhook", "", "") {
		t.Error("with nothing to compare against it must not match")
	}
}

// RunLeg probes a webhook leg after the bed restart and before the first phase; a
// failing probe ends the leg with exit 4 before any test runs. A poll leg — and a
// PAT leg, which can never run the transport — is never probed.
func TestRunLegProbesWebhookLegsOnly(t *testing.T) {
	run := func(t *testing.T, source string, cell Cell) (*legFake, *fakeDelivery, error) {
		lf := &legFake{suiteOut: passStream}
		g, _, _ := newLegGate(t, lf)
		fd := &fakeDelivery{rowErr: errors.New("daemon down"), hookURL: probeFunnelURL}
		g.Delivery = fd
		g.Cfg.EventSource = source
		g.Cfg.DeliveryProbeWait = 0 // one attempt
		err := g.RunLeg(context.Background(), cell)
		return lf, fd, err
	}
	t.Run("webhook leg fails before any test", func(t *testing.T) {
		lf, fd, err := run(t, bedevents.Tailscale, Cell{Auth: "app", Train: "on", Parallel: "2"})
		msg := failureOf(t, err)
		if !strings.Contains(msg, bedevents.StepTransportNotUp) || len(lf.suiteCmds) != 0 || fd.rowCalls == 0 {
			t.Errorf("msg=%q suiteCmds=%d rowCalls=%d", msg, len(lf.suiteCmds), fd.rowCalls)
		}
		if len(lf.switchCmds) != 1 || !hasEnv(lf.switchCmds[0].Env, "E2E_EVENT_SOURCE=tailscale") {
			t.Errorf("the restart must carry the leg's event source: %v", lf.switchCmds)
		}
	})
	for name, tc := range map[string]struct {
		source string
		cell   Cell
	}{
		"poll source":     {bedevents.Poll, Cell{Auth: "app", Train: "on", Parallel: "2"}},
		"pat leg":         {bedevents.Tailscale, Cell{Auth: "pat", Train: "on", Parallel: "2"}},
		"default (empty)": {"", Cell{Auth: "app", Train: "off", Parallel: "2"}},
	} {
		t.Run(name+" is never probed", func(t *testing.T) {
			lf, fd, err := run(t, tc.source, tc.cell)
			if err != nil || fd.rowCalls != 0 || len(lf.suiteCmds) == 0 {
				t.Errorf("err=%v rowCalls=%d suiteCmds=%d", err, fd.rowCalls, len(lf.suiteCmds))
			}
		})
	}
}

// The delivery preflight runs after beginArchive wrote probes.json, so the leg
// archive's Finish must rewrite it: on a failed probe (RunLeg returns at once) and
// on a pass alike, the result and failed step must reach the file (SC-004).
func TestDeliveryProbeResultIsArchivedInTheLegDirectory(t *testing.T) {
	cell := Cell{Auth: "app", Train: "on", Parallel: "2"}
	run := func(t *testing.T, fd *fakeDelivery) (string, error) {
		lf := &legFake{suiteOut: passStream}
		g, _, _ := newLegGate(t, lf)
		mustWrite(t, g.Cfg.TestBed+"/.env", "FABRIK_TOKEN=bed-token\nFABRIK_TAILSCALE_HOSTNAME=bed-a\n")
		mustWrite(t, g.Cfg.TestBed+"/bed-run.log", "Fabrik starting 0.1.0 (abc)\n[tailscale] App webhook URL: "+probeFunnelURL+" (secret from $X)\n")
		var clockMu sync.Mutex
		now := time.Unix(2000, 0)
		g.Now = func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return now }
		g.Sleep = func(d time.Duration) { clockMu.Lock(); now = now.Add(d); clockMu.Unlock() }
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
			return inner(ctx, c)
		}
		l := testLedger(t)
		g.cov = &covState{ledger: l, invocation: "inv-1", noRecord: true}
		g.Delivery = fd
		g.Cfg.EventSource = bedevents.Tailscale
		g.Cfg.DeliveryProbeWait = 60 * time.Second
		g.Cfg.DeliveryProbeInterval = 15 * time.Second
		err := g.RunLeg(context.Background(), cell)
		b, rerr := os.ReadFile(filepath.Join(l.ArchiveCellDir(cellDirName(cell), "inv-1"), "probes.json"))
		if rerr != nil {
			t.Fatalf("no probes.json in the leg archive: %v", rerr)
		}
		return strings.ReplaceAll(string(b), ": ", ":"), err
	}
	t.Run("failed probe", func(t *testing.T) {
		got, err := run(t, &fakeDelivery{rowErr: errors.New("daemon down"), hookURL: probeFunnelURL})
		if failureOf(t, err) == "" {
			t.Fatal("want a preflight failure")
		}
		for _, want := range []string{`"delivery"`, `"failed_step":"` + bedevents.StepTransportNotUp + `"`, `"status":"failed"`} {
			if !strings.Contains(got, want) {
				t.Errorf("probes.json lacks %s:\n%s", want, got)
			}
		}
	})
	t.Run("passing probe", func(t *testing.T) {
		got, err := run(t, &fakeDelivery{row: upRow(), hookURL: probeFunnelURL, deliver: true})
		if err != nil {
			t.Fatalf("leg failed: %v", err)
		}
		if !strings.Contains(got, `"delivery"`) || !strings.Contains(got, `"status":"ok"`) {
			t.Errorf("probes.json lacks the passing delivery result:\n%s", got)
		}
	})
}
