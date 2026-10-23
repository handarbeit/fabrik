package gate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	ghapi "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/localapi"
	"github.com/handarbeit/fabrik/tests/e2e/bedevents"
)

// The webhook delivery preflight (#2408, ADR-2408). A webhook leg whose bed cannot
// actually receive GitHub's App deliveries would not fail visibly: its webhook
// tests would time out as INCONCLUSIVE or skip, burning hours into a gate that
// looks covered and is not. So before the leg spends live budget the gate proves
// delivery end to end — a GitHub-originated App delivery, caused by a write of
// its own, reaches the bed daemon — and otherwise stops with ExitPreflightFailed
// naming the bed and the step that failed.
//
// It is a LIVE probe (a repository write), so it runs after the pre-gate
// (ADR-1454) like the board-lag probe (ADR-1974), and it is archived in
// probes.json. It cannot live in DefaultLivePreflights, though: it needs the bed
// daemon RUNNING with the transport, and those run before any bed is built, so it
// is a per-leg step in RunLeg after the leg's bed restart. Two deliberate
// differences from ADR-1974's probes: a reading it cannot take BLOCKS (a leg that
// would silently produce skipped or inconclusive webhook tests is exactly what it
// exists to stop), and it is per leg and bed, not per gate.
//
// Secrets: the bed's tailnet auth key and webhook secret are never read here
// (presence checks live in CheckBedWebhookProvisioning, values are never read);
// the App JWT and bed PAT are used in-process or through GH_TOKEN and never
// appear in any message or in probes.json.

// Delivery probe outcomes (DeliveryProbeResult.Status).
const (
	DeliveryOK     = "ok"
	DeliveryFailed = "failed"
)

// deliveryClockSkew is the slack allowed between the gate's clock and GitHub's
// delivery timestamps when matching the probe's own `issues` `labeled` delivery.
const deliveryClockSkew = 10 * time.Second

// deliveryProbeLabel is the one label the probe adds to (and removes from) its
// scratch issue. It is fixed, so repeated runs never accumulate labels on the
// alpha repository; add-after-remove still gives each attempt a distinct
// `labeled` delivery.
const deliveryProbeLabel = "e2e-delivery-probe"

// deliveryProbeTitlePrefix marks the probe's scratch issues so a leftover from a
// run killed mid-probe can be recognised and swept by the next one.
const deliveryProbeTitlePrefix = "e2e-delivery-probe-"

// DeliveryProbeResult is one leg's delivery preflight, recorded in probes.json.
type DeliveryProbeResult struct {
	Bed           string  `json:"bed"`
	Leg           string  `json:"leg"`
	Status        string  `json:"status"`
	FailedStep    string  `json:"failed_step,omitempty"` // a bedevents.Step* value
	Attempts      int     `json:"attempts"`
	WaitedSeconds float64 `json:"waited_seconds"`
	HookURL       string  `json:"hook_url,omitempty"` // the App's configured webhook URL (not a secret)
	Note          string  `json:"note,omitempty"`
}

// TransportRow is the part of the bed daemon's tailscale transport health row the
// probe reads.
type TransportRow struct {
	Present      bool
	State        string // "listening", "not listening", "tailnet disconnected", "funnel unavailable", ...
	Funnel       string // "ok", "unavailable", "" (unknown)
	LastDelivery time.Time
}

// up reports whether the transport can receive: listening, and its Funnel not
// reported unavailable.
func (r TransportRow) up() bool {
	return r.Present && r.State == "listening" && (r.Funnel == "" || r.Funnel == "ok")
}

// HookDelivery is one entry of the App's delivery log (GET /app/hook/deliveries).
type HookDelivery struct {
	Event       string
	Action      string
	StatusCode  int
	DeliveredAt time.Time
}

// DeliveryOps is the seam over the probe's reads of the bed daemon and the App.
// The real implementation uses the daemon's local API socket and the App's JWT;
// tests substitute a fake.
type DeliveryOps interface {
	// TransportRow reads the bed daemon's health (fabrik_health) for its tailscale
	// transport row. An error means the daemon could not be asked.
	TransportRow(ctx context.Context, bedDir string) (TransportRow, error)
	// AppHookURL is the webhook URL the bed's App is configured with
	// (GET /app/hook/config). The secret is never decoded.
	AppHookURL(ctx context.Context, bedDir string) (string, error)
	// AppDeliveries lists the App's recent deliveries, newest first.
	AppDeliveries(ctx context.Context, bedDir string) ([]HookDelivery, error)
}

// osDeliveryOps is the real DeliveryOps.
type osDeliveryOps struct{}

func (osDeliveryOps) TransportRow(ctx context.Context, bedDir string) (TransportRow, error) {
	var hr localapi.HealthResult
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := localapi.Call(cctx, localapi.SocketPath(bedDir), localapi.MethodHealth, nil, &hr); err != nil {
		return TransportRow{}, err
	}
	for _, t := range hr.Webhook.Transports {
		if t.Name != bedevents.Tailscale {
			continue
		}
		row := TransportRow{Present: true, State: t.State}
		if t.Funnel.Valid {
			row.Funnel = t.Funnel.V
		}
		if t.LastDeliveryAt.Valid {
			row.LastDelivery = t.LastDeliveryAt.V
		}
		return row, nil
	}
	return TransportRow{}, nil
}

// appCall runs fn (a blocking App-API call with its own bounded HTTP client)
// beside ctx so a cancelled gate does not wait on it.
func appCall[T any](ctx context.Context, fn func() (T, error)) (T, error) {
	type res struct {
		v   T
		err error
	}
	ch := make(chan res, 1)
	go func() {
		v, err := fn()
		ch <- res{v, err}
	}()
	select {
	case r := <-ch:
		return r.v, r.err
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}

func (osDeliveryOps) AppHookURL(ctx context.Context, bedDir string) (string, error) {
	jwt, _, err := appCredentials(bedDir)
	if err != nil {
		return "", err
	}
	cfg, err := appCall(ctx, func() (ghapi.AppHookConfig, error) { return ghapi.FetchAppHookConfig("", jwt) })
	return cfg.URL, err
}

func (osDeliveryOps) AppDeliveries(ctx context.Context, bedDir string) ([]HookDelivery, error) {
	jwt, _, err := appCredentials(bedDir)
	if err != nil {
		return nil, err
	}
	items, err := appCall(ctx, func() ([]ghapi.AppHookDelivery, error) {
		items, _, err := ghapi.FetchAppHookDeliveries("", jwt, "", 30)
		return items, err
	})
	if err != nil {
		return nil, err
	}
	out := make([]HookDelivery, 0, len(items))
	for _, d := range items {
		out = append(out, HookDelivery{Event: d.Event, Action: d.Action, StatusCode: d.StatusCode, DeliveredAt: d.DeliveredAt})
	}
	return out, nil
}

// webhookURLLogRE matches the engine's startup line naming the Funnel URL
// (engine/tailnet.go): "... App webhook URL: https://host.tailnet.ts.net/webhook (secret ...".
var webhookURLLogRE = regexp.MustCompile(`App webhook URL: (https://\S+)`)

// loggedWebhookURL is the Funnel URL the bed daemon's latest start logged ("" when
// none). Only the text after the last startup banner (bedStartMarker) counts, so a webhook leg's URL
// from an earlier start is never mistaken for this one's.
func loggedWebhookURL(bedDir string) string {
	data, err := os.ReadFile(filepath.Join(bedDir, "bed-run.log"))
	if err != nil {
		return ""
	}
	s := string(data)
	if i := strings.LastIndex(s, bedStartMarker); i >= 0 {
		s = s[i:]
	}
	m := webhookURLLogRE.FindAllStringSubmatch(s, -1)
	if len(m) == 0 {
		return ""
	}
	return m[len(m)-1][1]
}

// urlMatchesBed reports whether the App's configured URL is this bed's Funnel URL:
// exactly the URL the daemon logged, or — when no log line is available — a URL on
// the bed's own tailnet hostname.
func urlMatchesBed(appURL, logged, hostname string) bool {
	if logged != "" {
		return appURL == logged
	}
	if hostname == "" {
		return false
	}
	return strings.HasPrefix(appURL, "https://"+hostname+".")
}

// ProbeWebhookDelivery is the webhook delivery preflight for one webhook leg.
func (g *Gate) ProbeWebhookDelivery(ctx context.Context, cell Cell) error {
	if g.Delivery == nil {
		return nil // a Gate built by hand
	}
	bedDir := g.Cfg.TestBed
	bed := g.bedLabel()
	res := &DeliveryProbeResult{Bed: bed, Leg: cell.Label()}
	g.probes.Delivery = append(g.probes.Delivery, res)
	start := g.Now()
	g.outf("== probing webhook delivery to %s (leg %s; scratch issue label on %s) ==\n", bed, res.Leg, g.resetConfig().Alpha)

	fail := func(step, detail string) error {
		res.Status, res.FailedStep, res.Note = DeliveryFailed, step, detail
		res.WaitedSeconds = g.Now().Sub(start).Seconds()
		return exitErr(ExitPreflightFailed, "preflight: webhook delivery probe failed on %s (leg %s) at step %q — %s. "+
			"A webhook leg would run its webhook tests against a bed that cannot receive deliveries. See tests/e2e/README.md (\"Webhook delivery\") to check the bed by hand.",
			bed, res.Leg, step, detail)
	}

	interval := g.Cfg.DeliveryProbeInterval
	if interval <= 0 {
		interval = 15 * time.Second
	}
	attempts := 1
	if g.Cfg.DeliveryProbeWait > 0 {
		attempts = int(g.Cfg.DeliveryProbeWait/interval) + 1
	}

	hostname := EnvFileValue(filepath.Join(bedDir, ".env"), "FABRIK_TAILSCALE_HOSTNAME")
	scratch := &deliveryScratch{g: g, repo: g.resetConfig().Alpha, token: EnvFileValue(filepath.Join(bedDir, ".env"), "FABRIK_TOKEN")}
	if scratch.token == "" {
		return fail(bedevents.StepProbeSetup, "no bed token (FABRIK_TOKEN in the bed's .env) to make the probe's write with — a provisioning problem, not a delivery one")
	}
	defer scratch.cleanup(ctx)
	scratch.sweepLeftovers(ctx)

	var lastStep, lastDetail string
	urlChecked := false
	for attempt := 0; attempt < attempts; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		res.Attempts = attempt + 1
		row, err := g.Delivery.TransportRow(ctx, bedDir)
		switch {
		case err != nil:
			lastStep, lastDetail = bedevents.StepTransportNotUp, fmt.Sprintf("the bed daemon could not be asked for its health (%v) — is it running?", err)
		case !row.Present:
			lastStep, lastDetail = bedevents.StepTransportNotUp, "the bed daemon reports no tailscale transport: it is running poll-only, or the transport failed to start (see bed-run.log)"
		case !row.up():
			lastStep, lastDetail = bedevents.StepTransportNotUp, fmt.Sprintf("Funnel/transport not up (transport state %q, funnel %q)", row.State, orUnknown(row.Funnel))
		default:
			if !urlChecked {
				appURL, uerr := g.Delivery.AppHookURL(ctx, bedDir)
				switch {
				case uerr != nil:
					lastStep, lastDetail = bedevents.StepHookURLMismatch, fmt.Sprintf("could not read the App's webhook configuration (%v)", uerr)
				case !urlMatchesBed(appURL, loggedWebhookURL(bedDir), hostname):
					res.HookURL = appURL
					return fail(bedevents.StepHookURLMismatch, fmt.Sprintf("the App's webhook URL is %q, not this bed's Funnel URL %q — point the App at the bed (README \"Webhook delivery\")", appURL, orUnknown(firstNonEmpty(loggedWebhookURL(bedDir), hostname))))
				default:
					res.HookURL, urlChecked = appURL, true
				}
			}
			if urlChecked {
				ok, step, detail := g.probeOneDelivery(ctx, scratch, row, attempt, interval)
				if step == bedevents.StepProbeSetup {
					// The probe's own write failed (token scope, repo access): not a
					// Funnel-settling condition, so it is not retried.
					return fail(step, detail)
				}
				if ok {
					res.Status = DeliveryOK
					res.WaitedSeconds = g.Now().Sub(start).Seconds()
					g.outf("   delivery reached %s after %d attempt(s) (%.0fs)\n", bed, res.Attempts, res.WaitedSeconds)
					return nil
				}
				lastStep, lastDetail = step, detail
				continue // probeOneDelivery already waited out an interval
			}
		}
		if attempt+1 < attempts {
			g.outf("   %s: %s — re-probing in %s (%d of %d)\n", lastStep, lastDetail, interval, attempt+1, attempts-1)
			g.Sleep(interval)
		}
	}
	return fail(lastStep, lastDetail)
}

// probeOneDelivery makes one write that GitHub turns into an App delivery and
// waits one interval for it. The observation must be a delivery NEWER than the
// baseline read just before the write; when the App's delivery log is readable it
// must also show the probe's own event, so a replayed older delivery cannot satisfy
// it. step and detail explain a miss: StepNoDelivery, or StepProbeSetup when the
// probe's own write failed.
func (g *Gate) probeOneDelivery(ctx context.Context, s *deliveryScratch, before TransportRow, attempt int, interval time.Duration) (bool, string, string) {
	writeAt := g.Now()
	if err := s.toggleLabel(ctx, attempt); err != nil {
		return false, bedevents.StepProbeSetup, fmt.Sprintf("the probe's own write failed: %v (a probe-setup problem — check the bed token's issues:write access on the alpha repo — not a delivery one)", err)
	}
	g.Sleep(interval)
	after, err := g.Delivery.TransportRow(ctx, g.Cfg.TestBed)
	if err != nil || !after.Present {
		return false, bedevents.StepNoDelivery, "the bed daemon stopped answering while waiting for the delivery"
	}
	if !after.LastDelivery.After(before.LastDelivery) {
		return false, bedevents.StepNoDelivery, g.explainMissingDelivery(ctx, writeAt)
	}
	if ds, derr := g.Delivery.AppDeliveries(ctx, g.Cfg.TestBed); derr == nil && len(ds) > 0 {
		for _, d := range ds {
			if d.Event == "issues" && d.Action == "labeled" && !d.DeliveredAt.Before(writeAt.Add(-deliveryClockSkew)) && d.StatusCode >= 200 && d.StatusCode < 300 {
				return true, "", ""
			}
		}
		return false, bedevents.StepNoDelivery, "the daemon's last-delivery time moved, but the App's delivery log shows no successful issues delivery for the probe's write (an older delivery was replayed)"
	}
	return true, "", ""
}

// explainMissingDelivery turns the App's delivery log into the most likely cause of
// a missing delivery. Best-effort: an unreadable log yields the plain statement.
func (g *Gate) explainMissingDelivery(ctx context.Context, since time.Time) string {
	plain := "no delivery observed at the bed daemon within the wait"
	ds, err := g.Delivery.AppDeliveries(ctx, g.Cfg.TestBed)
	if err != nil {
		return plain + " (the App's delivery log could not be read to say why)"
	}
	for _, d := range ds {
		if d.Event != "issues" || d.DeliveredAt.Before(since.Add(-time.Minute)) {
			continue
		}
		if d.StatusCode < 200 || d.StatusCode >= 300 {
			return fmt.Sprintf("%s: GitHub did send the delivery but got HTTP %d — the Funnel or the App's URL is unreachable from GitHub", plain, d.StatusCode)
		}
		return plain + ": GitHub reports the delivery succeeded, so it went to a different endpoint than this bed"
	}
	return plain + ": GitHub created no delivery for the probe's write — the App is probably not subscribed to Issues events"
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// deliveryScratch is the probe's scratch issue: created lazily on the bed's alpha
// repository with the bed PAT, labelled and unlabelled to cause deliveries, and
// ALWAYS closed again. It is not on the board, so it can never trip the bed's
// stale-Queued-member checks.
type deliveryScratch struct {
	g      *Gate
	repo   string
	token  string
	number int
}

func (s *deliveryScratch) gh(ctx context.Context, args ...string) (string, bool) {
	so, _, res := output(ctx, s.g.Exec, Cmd{
		Name: "gh", Args: args, Env: withEnv(s.g.Env, "GH_TOKEN="+s.token),
		Session: true, Timeout: s.g.Cfg.GHAPITimeout, Grace: s.g.Cfg.KillGrace,
	})
	return so, res.ExitCode == 0 && res.Err == nil && !res.TimedOut
}

func (s *deliveryScratch) ensureIssue(ctx context.Context) error {
	if s.number != 0 {
		return nil
	}
	title := fmt.Sprintf("%s%d", deliveryProbeTitlePrefix, s.g.Now().UnixNano())
	so, ok := s.gh(ctx, "api", "-X", "POST", "repos/"+s.repo+"/issues", "-f", "title="+title,
		"-f", "body=Scratch issue of the e2e gate's webhook delivery preflight (#2408). Closed automatically.", "--jq", ".number")
	n, err := strconv.Atoi(strings.TrimSpace(so))
	if !ok || err != nil || n <= 0 {
		return fmt.Errorf("creating the scratch issue on %s failed", s.repo)
	}
	s.number = n
	return nil
}

// toggleLabel adds the fixed probe label (an `issues` `labeled` delivery). On a
// retry it first removes it, so the re-add is a new `labeled` event rather than a
// no-op.
func (s *deliveryScratch) toggleLabel(ctx context.Context, attempt int) error {
	if err := s.ensureIssue(ctx); err != nil {
		return err
	}
	if attempt > 0 {
		s.gh(ctx, "api", "-X", "DELETE", fmt.Sprintf("repos/%s/issues/%d/labels/%s", s.repo, s.number, deliveryProbeLabel)) // best effort: absent is fine
	}
	if _, ok := s.gh(ctx, "api", "-X", "POST", fmt.Sprintf("repos/%s/issues/%d/labels", s.repo, s.number), "-f", "labels[]="+deliveryProbeLabel); !ok {
		return fmt.Errorf("labelling the scratch issue #%d on %s failed", s.number, s.repo)
	}
	return nil
}

// cleanup closes the scratch issue, on a context detached from cancellation so an
// interrupted gate still cleans up.
func (s *deliveryScratch) cleanup(ctx context.Context) {
	if s.number == 0 {
		return
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*s.g.Cfg.GHAPITimeout+10*time.Second)
	defer cancel()
	if _, ok := s.gh(cctx, "api", "-X", "PATCH", fmt.Sprintf("repos/%s/issues/%d", s.repo, s.number), "-f", "state=closed"); !ok {
		s.g.errf("warning: could not close the webhook delivery probe's scratch issue #%d on %s — the next run sweeps it, or run reset\n", s.number, s.repo)
	}
}

// sweepLeftovers closes scratch issues an earlier, killed run left open.
func (s *deliveryScratch) sweepLeftovers(ctx context.Context) {
	so, ok := s.gh(ctx, "issue", "list", "-R", s.repo, "--state", "open", "--limit", "50", "--json", "number,title",
		"--jq", fmt.Sprintf(`.[] | select(.title | startswith(%q)) | .number`, deliveryProbeTitlePrefix))
	if !ok {
		return
	}
	for _, f := range strings.Fields(so) {
		if _, err := strconv.Atoi(f); err == nil {
			s.gh(ctx, "api", "-X", "PATCH", fmt.Sprintf("repos/%s/issues/%s", s.repo, f), "-f", "state=closed")
		}
	}
}
