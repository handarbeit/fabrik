//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	ghapi "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/channelevents"
	"github.com/handarbeit/fabrik/internal/localapi"
)

// Helpers shared by the live webhook coverage tests (#2427, ADR 2427-a/b).
//
// Every assertion reads the daemon, not GitHub: through fabrik_status and
// fabrik_health over the bed's local API socket, and — for attribution, which only
// channel events carry — through a channel subscriber attached to the same socket.
// The intake counters in fabrik_health (webhook.intake) are what make "a delivery
// was applied / was a duplicate / was matched as an echo" observable; tests compare
// deltas between two reads, because the counters are since process start.

// webhookLegSetup is the common prologue of a webhook test (the test body calls
// LoadEnv itself, which the registry scan keys on): require a running bed, skip structurally on a leg without a webhook transport and
// repair a hook URL a killed earlier run may have left pointing elsewhere.
func webhookLegSetup(t *testing.T, env *Env) {
	t.Helper()
	AssertFabrikRunning(t, env)
	if !bedWebhooksEnabled(env) {
		t.Skip("this leg has no webhook transport (poll-only event source, or a PAT leg) — the behaviour under test is the daemon's handling of webhook deliveries, so there is nothing to observe; a webhook cell of the gate plan (app/on/webhook) runs it and on a poll cell the skip is structural (*/*/poll), #2426")
	}
	restoreStaleHookURL(t, env)
}

// ---- local API reads ----

func bedSocket(env *Env) string { return localapi.SocketPath(env.FabrikTestDir) }

// bedCall performs one local API call with a bounded timeout.
func bedCall(env *Env, method string, params, out any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return localapi.Call(ctx, bedSocket(env), method, params, out)
}

// bedHealth reads fabrik_health.
func bedHealth(env *Env) (*localapi.HealthResult, error) {
	var res localapi.HealthResult
	if err := bedCall(env, localapi.MethodHealth, localapi.HealthParams{}, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// bedStatus reads fabrik_status for one issue.
func bedStatus(env *Env, repo string, issue int) (*localapi.StatusResult, error) {
	var res localapi.StatusResult
	err := bedCall(env, localapi.MethodStatus, localapi.StatusParams{Issue: fmt.Sprintf("%s#%d", repo, issue)}, &res)
	if err != nil {
		return nil, err
	}
	return &res, nil
}

// hasLabel reports whether labels contains want.
func hasLabel(labels []string, want string) bool {
	for _, l := range labels {
		if l == want {
			return true
		}
	}
	return false
}

// daemonLabels returns every label (stage:, fabrik: and other) the daemon holds
// for an issue.
func daemonLabels(env *Env, repo string, issue int) ([]string, error) {
	st, err := bedStatus(env, repo, issue)
	if err != nil {
		return nil, err
	}
	all := append([]string{}, st.Place.StageLabels...)
	all = append(all, st.Place.FabrikLabels...)
	all = append(all, st.Place.OtherLabels...)
	return all, nil
}

// awaitDaemonLabel polls the daemon until it holds (present=true) or no longer
// holds (present=false) label, or timeout expires. A read error is retried.
func awaitDaemonLabel(env *Env, repo string, issue int, label string, present bool, timeout time.Duration) bool {
	return awaitCond(timeout, func() bool {
		labels, err := daemonLabels(env, repo, issue)
		return err == nil && hasLabel(labels, label) == present
	})
}

// awaitCond polls cond every 2s until it is true or timeout expires.
func awaitCond(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(2 * time.Second)
	}
}

// ---- intake counters ----

// intakeCounters is a copy of fabrik_health's webhook.intake block.
type intakeCounters struct {
	Accepted, Duplicates, Dropped, EchoMatched int64
	Applied                                    map[string]int64
}

// readIntake reads the intake counters; an error when the daemon reports none
// (it is poll-only, so this is not a webhook leg).
func readIntake(env *Env) (intakeCounters, error) {
	h, err := bedHealth(env)
	if err != nil {
		return intakeCounters{}, err
	}
	if h.Webhook.Intake == nil {
		return intakeCounters{}, errors.New("fabrik_health reports no webhook.intake block (the daemon has no delivery intake)")
	}
	in := h.Webhook.Intake
	c := intakeCounters{Accepted: in.Accepted, Duplicates: in.Duplicates, Dropped: in.Dropped, EchoMatched: in.EchoMatched, Applied: map[string]int64{}}
	for k, v := range in.Applied {
		c.Applied[k] = v
	}
	return c, nil
}

// mustReadIntake is readIntake failing the test on error.
func mustReadIntake(t *testing.T, env *Env) intakeCounters {
	t.Helper()
	c, err := readIntake(env)
	if err != nil {
		t.Fatalf("reading the daemon's intake counters: %v", err)
	}
	return c
}

// awaitChannelReady waits until fabrik_health reports the channel push ready
// (#2484): a freshly started daemon holds its downtime deltas until the board is
// populated and the restart catch-up has been queued to every subscriber, which
// takes over a minute after a restart on a real bed, so a fixed wait either gives
// up too early or sits idle. It returns whether the daemon reported ready within
// timeout, and whether it reports a channel block at all (false: a daemon without
// the field, or without a channel hub).
func awaitChannelReady(env *Env, timeout time.Duration) (ready, reported bool) {
	ready = awaitCond(timeout, func() bool {
		h, err := bedHealth(env)
		if err != nil {
			return false // the daemon is mid-restart; keep asking
		}
		if h.Channel == nil {
			return false
		}
		reported = true
		return h.Channel.Ready
	})
	return ready, reported
}

// awaitApplied waits until the daemon has applied more than base[eventType]
// deliveries of that type since base was read, and returns the new count.
func awaitApplied(env *Env, base intakeCounters, eventType string, timeout time.Duration) (int64, bool) {
	var got int64
	ok := awaitCond(timeout, func() bool {
		c, err := readIntake(env)
		if err != nil {
			return false
		}
		got = c.Applied[eventType]
		return got > base.Applied[eventType]
	})
	return got, ok
}

// awaitIntakeQuiet waits until the daemon has accepted no new delivery for quiet,
// so stragglers of earlier writes (a label-creation event arrives separately from
// the label-applied one) cannot be mistaken for the delivery a test is about to
// cause. It returns false if the counter never settles within timeout.
func awaitIntakeQuiet(env *Env, quiet, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	var last int64 = -1
	stableSince := time.Now()
	for time.Now().Before(deadline) {
		c, err := readIntake(env)
		if err == nil {
			if c.Accepted != last {
				last, stableSince = c.Accepted, time.Now()
			} else if time.Since(stableSince) >= quiet {
				return true
			}
		}
		time.Sleep(3 * time.Second)
	}
	return false
}

// ---- channel subscriber (attribution) ----

// eventCollector is a channel subscriber attached to the bed's local API socket
// for the life of a test. Attribution (actor=human|fabrik|overseer|unknown) is only
// visible on channel events, not on fabrik_status.
type eventCollector struct {
	mu     sync.Mutex
	events []channelevents.Event
	cancel context.CancelFunc
	done   chan struct{}
	name   string
	env    *Env
}

// startLabelCollector subscribes a uniquely named subscriber to label-applied and
// label-removed events (the default family excludes them) with no label exclusion,
// attaches a stream and returns the collector. It unsubscribes in t.Cleanup, because
// subscriptions persist in the bed's hub.
func startLabelCollector(t *testing.T, env *Env, tag string) *eventCollector {
	t.Helper()
	name := fmt.Sprintf("e2e-webhook-%s-%d", tag, time.Now().UnixNano())
	none := []string{}
	var res localapi.SubscribeResult
	err := bedCall(env, localapi.MethodSubscribe, localapi.SubscribeParams{
		Subscriber:    name,
		Events:        []string{string(channelevents.LabelApplied), string(channelevents.LabelRemoved)},
		ExcludeLabels: &none,
	}, &res)
	if err != nil {
		Inconclusive(t, "could not subscribe to the daemon's channel events (%v), so attribution cannot be observed", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := &eventCollector{cancel: cancel, done: make(chan struct{}), name: name, env: env}
	go func() {
		defer close(c.done)
		_ = localapi.RunStream(ctx, localapi.StreamOptions{
			Path:       bedSocket(env),
			Subscriber: name,
			OnEvent: func(ev channelevents.Event) {
				c.mu.Lock()
				c.events = append(c.events, ev)
				c.mu.Unlock()
			},
		})
	}()
	t.Cleanup(func() {
		cancel()
		<-c.done
		var un localapi.UnsubscribeResult
		if err := bedCall(env, localapi.MethodUnsubscribe, localapi.UnsubscribeParams{Subscriber: name}, &un); err != nil {
			t.Errorf("unsubscribing %s from the bed's hub (a leftover subscription keeps queueing events): %v", name, err)
		}
	})
	return c
}

// labelEvents returns the live label events about repo#issue and label of the given type.
func (c *eventCollector) labelEvents(typ channelevents.EventType, repo string, issue int, label string) []channelevents.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []channelevents.Event
	for _, ev := range c.events {
		if ev.Type != typ || ev.Repo != repo || ev.Issue != issue || ev.Label != label {
			continue
		}
		// Catch-up snapshots and downtime deltas are not the live event under test.
		if o := ev.Meta["origin"]; o != "" && o != "live" {
			continue
		}
		out = append(out, ev)
	}
	return out
}

// awaitLabelEvent waits for at least one live label event and returns all of them.
func (c *eventCollector) awaitLabelEvent(typ channelevents.EventType, repo string, issue int, label string, timeout time.Duration) []channelevents.Event {
	var got []channelevents.Event
	awaitCond(timeout, func() bool {
		got = c.labelEvents(typ, repo, issue, label)
		return len(got) > 0
	})
	return got
}

// ---- App API (delivery log, hook configuration) ----

// appJWT mints a short-lived App JWT from the bed's E2E_APP_* identity. It is a
// secret: it is never logged.
func appJWT(env *Env) (string, error) {
	id, err := readAppIdentity(env)
	if err != nil {
		return "", fmt.Errorf("bed App identity: %w", err)
	}
	pemBytes, err := os.ReadFile(id.KeyPath)
	if err != nil {
		return "", fmt.Errorf("reading App private key %s: %w", id.KeyPath, err)
	}
	key, err := ghapi.ParseAppPrivateKey(pemBytes)
	if err != nil {
		return "", fmt.Errorf("parsing App private key: %w", err)
	}
	jwt, err := ghapi.BuildAppJWT(id.AppID, key)
	if err != nil {
		return "", fmt.Errorf("building App JWT: %w", err)
	}
	return jwt, nil
}

// findAppDelivery looks in the App's delivery log (newest page) for a delivery of
// the given event type and action received after since whose payload match accepts,
// and returns it. It identifies a delivery by content, never by position, because a
// shared App's log carries other beds' and other tests' deliveries.
func findAppDelivery(env *Env, since time.Time, event, action string, match func(payload []byte) bool, timeout time.Duration) (ghapi.AppHookDeliveryDetail, bool, error) {
	var found ghapi.AppHookDeliveryDetail
	var lastErr error
	ok := awaitCond(timeout, func() bool {
		jwt, err := appJWT(env)
		if err != nil {
			lastErr = err
			return false
		}
		items, _, err := ghapi.FetchAppHookDeliveries("", jwt, "", 100)
		if err != nil {
			lastErr = err
			return false
		}
		for _, d := range items {
			if d.Event != event || d.Redelivery || (action != "" && d.Action != action) || d.DeliveredAt.Before(since.Add(-time.Minute)) {
				continue
			}
			detail, err := ghapi.FetchAppHookDelivery("", jwt, d.ID)
			if err != nil {
				lastErr = err
				continue
			}
			if match(detail.Payload) {
				found = detail
				return true
			}
		}
		return false
	})
	return found, ok, lastErr
}

// payloadIssueLabel reports whether an issues/labeled payload is about repo#issue
// and label.
func payloadIssueLabel(repo string, issue int, label string) func([]byte) bool {
	return func(p []byte) bool {
		var v struct {
			Issue struct {
				Number int `json:"number"`
			} `json:"issue"`
			Label struct {
				Name string `json:"name"`
			} `json:"label"`
			Repository struct {
				FullName string `json:"full_name"`
			} `json:"repository"`
		}
		return json.Unmarshal(p, &v) == nil && v.Issue.Number == issue && v.Label.Name == label && v.Repository.FullName == repo
	}
}

// ---- hook URL mutation with a durable restore marker ----

// hookRestoreMarker is where a pending hook-URL restore is recorded, so a run killed
// between the PATCH and its cleanup is repaired by the next webhook test (and the
// gate's hook-url-mismatch preflight names it either way).
func hookRestoreMarker(env *Env) string {
	return filepath.Join(env.FabrikTestDir, ".fabrik", "state", "e2e-hook-url.restore")
}

// setHookURL changes the App's webhook URL and reports the previous one.
func setHookURL(env *Env, url string) (previous string, err error) {
	jwt, err := appJWT(env)
	if err != nil {
		return "", err
	}
	cur, err := ghapi.FetchAppHookConfig("", jwt)
	if err != nil {
		return "", fmt.Errorf("reading the App's webhook config: %w", err)
	}
	if _, err := ghapi.UpdateAppHookConfig("", jwt, url); err != nil {
		return cur.URL, err
	}
	return cur.URL, nil
}

// restoreHookURLFromMarker puts the recorded URL back and removes the marker. A
// missing marker is nothing to do.
func restoreHookURLFromMarker(env *Env) error {
	b, err := os.ReadFile(hookRestoreMarker(env))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	url := strings.TrimSpace(string(b))
	if url == "" {
		return os.Remove(hookRestoreMarker(env))
	}
	var lastErr error
	for i := 0; i < 5; i++ {
		if _, lastErr = setHookURL(env, url); lastErr == nil {
			return os.Remove(hookRestoreMarker(env))
		}
		time.Sleep(3 * time.Second)
	}
	return fmt.Errorf("restoring the App's webhook URL to %s: %w", url, lastErr)
}

// restoreStaleHookURL repairs a hook URL left pointing elsewhere by a killed run. A
// failure is fatal: every later delivery would otherwise be lost.
func restoreStaleHookURL(t *testing.T, env *Env) {
	t.Helper()
	if _, err := os.Stat(hookRestoreMarker(env)); err != nil {
		return
	}
	t.Logf("a previous run left the App's webhook URL changed (%s) — restoring it", hookRestoreMarker(env))
	if err := restoreHookURLFromMarker(env); err != nil {
		t.Fatalf("the App's webhook URL is still wrong and every delivery to this bed is lost until it is restored by hand: %v", err)
	}
}

// breakHookURL points the App's webhook URL at an address that can never reach the
// bed, recording the original in a restore marker first, and registers the restore
// with t.Cleanup. The restore runs on failure and skip too; if it fails the test
// reports it loudly.
func breakHookURL(t *testing.T, env *Env, brokenURL string) (original string) {
	t.Helper()
	jwt, err := appJWT(env)
	if err != nil {
		t.Fatalf("minting an App JWT: %v", err)
	}
	cur, err := ghapi.FetchAppHookConfig("", jwt)
	if err != nil {
		t.Fatalf("reading the App's webhook config: %v", err)
	}
	if cur.URL == "" {
		t.Fatalf("the App has no webhook URL configured — nothing to break and restore")
	}
	if err := os.MkdirAll(filepath.Dir(hookRestoreMarker(env)), 0o755); err != nil {
		t.Fatalf("creating the restore marker directory: %v", err)
	}
	if err := os.WriteFile(hookRestoreMarker(env), []byte(cur.URL), 0o600); err != nil {
		t.Fatalf("writing the hook-URL restore marker: %v", err)
	}
	t.Cleanup(func() {
		if err := restoreHookURLFromMarker(env); err != nil {
			t.Errorf("LOUD: the App's webhook URL was NOT restored (%v); every delivery to this bed is lost until it is set back to %s", err, cur.URL)
		}
	})
	if _, err := ghapi.UpdateAppHookConfig("", jwt, brokenURL); err != nil {
		t.Fatalf("pointing the App's webhook URL elsewhere: %v", err)
	}
	return cur.URL
}

// ---- warnings panel file ----

// bedWarning reports whether the bed's warnings panel file carries an entry whose
// fingerprint is fingerprint. The warning has no local API surface (a documented
// exception, ADR 2427-a), so the file is read; an unreadable file is "no".
func bedWarning(env *Env, fingerprint string) bool {
	b, err := os.ReadFile(filepath.Join(env.FabrikTestDir, ".fabrik", "warnings.json"))
	if err != nil {
		return false
	}
	return strings.Contains(string(b), fingerprint)
}

// ---- daemon process control ----

// signalBed sends sig to the bed daemon's process.
func signalBed(t *testing.T, env *Env, sig syscall.Signal) {
	t.Helper()
	pid := lockedPID(env)
	if pid == 0 {
		t.Fatalf("no live bed daemon to signal (lock %s)", fabrikLockPath(env))
	}
	if err := syscall.Kill(pid, sig); err != nil {
		t.Fatalf("signalling bed daemon pid %d with %v: %v", pid, sig, err)
	}
}

// awaitRestartSince waits until the daemon answers with an uptime that began after
// since — i.e. a new process image is serving. A SIGHUP restart keeps the PID, so
// uptime is the only signal.
func awaitRestartSince(env *Env, since time.Time, timeout time.Duration) bool {
	return awaitCond(timeout, func() bool {
		h, err := bedHealth(env)
		if err != nil {
			return false
		}
		started := time.Now().Add(-time.Duration(h.DaemonUptimeSeconds) * time.Second)
		return started.After(since.Add(-2 * time.Second))
	})
}

// healthTransport returns the first transport row of fabrik_health, or nil.
func healthTransport(env *Env) (*localapi.TransportHealth, error) {
	h, err := bedHealth(env)
	if err != nil {
		return nil, err
	}
	if len(h.Webhook.Transports) == 0 {
		return nil, errors.New("fabrik_health lists no event transport")
	}
	row := h.Webhook.Transports[0]
	return &row, nil
}

// fileInertItem files a paused issue, places it at Specify on the board and waits
// until both GitHub and the daemon show it there, so no Claude stage runs on it and
// the daemon already holds it before a test holds polls. The issue is closed in
// t.Cleanup. It is Inconclusive when the daemon never shows the starting state: with
// no starting state there is nothing to attribute to a delivery.
func fileInertItem(t *testing.T, env *Env, repo, title, body string, labels ...string) (issue int, itemID string) {
	t.Helper()
	stamp := time.Now().UTC().Format("20060102-150405")
	all := append([]string{"fabrik:paused", "fabrik:awaiting-input"}, labels...)
	issue = FileIssue(t, env, repo, title+" "+stamp, body, all...)
	t.Cleanup(func() { CloseIssue(env, repo, issue) })
	itemID = AddIssueToProject(t, env, repo, issue)
	SetIssueStatus(t, env, itemID, "Specify")
	AwaitStatusVisible(t, env, repo, issue, "Specify", awaitSeedTimeout)
	column := func() (string, error) { return daemonColumn(env, repo, issue) }
	if !awaitColumn(column, "Specify", 3*time.Minute) {
		Inconclusive(t, "the daemon never showed %s#%d at Specify within 3m, so there is no starting state", repo, issue)
	}
	return issue, itemID
}

// allLabelEvents returns every label event of typ about repo#issue and label from
// any origin (live, catch-up or downtime delta). The restart tests count these:
// however a change reaches the daemon after a restart it must be announced once.
func (c *eventCollector) allLabelEvents(typ channelevents.EventType, repo string, issue int, label string) []channelevents.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []channelevents.Event
	for _, ev := range c.events {
		if ev.Type == typ && ev.Repo == repo && ev.Issue == issue && ev.Label == label {
			out = append(out, ev)
		}
	}
	return out
}

// ensureLabel creates a repo label if it does not exist (idempotent).
func ensureLabel(t *testing.T, env *Env, repo, label string) {
	t.Helper()
	if out, err := ghOutput(env, "label", "create", label, "-R", repo, "--force", "--color", "ededed",
		"--description", "e2e webhook test label (#2427)"); err != nil {
		t.Fatalf("creating label %q on %s: %v\n%s", label, repo, err, out)
	}
}

// ensureBedRunning restarts the bed in t.Cleanup if the test left it stopped, so a
// failure between a stop and a start cannot wedge later tests.
func ensureBedRunning(t *testing.T, env *Env) {
	t.Helper()
	t.Cleanup(func() {
		if lockedPID(env) == 0 {
			t.Logf("the bed daemon is not running at the end of the test — starting it")
			StartFabrikTestBed(t, env)
		}
	})
}
