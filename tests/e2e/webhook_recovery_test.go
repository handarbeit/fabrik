//go:build e2e

package e2e

import (
	"fmt"
	"syscall"
	"time"

	"testing"

	"github.com/handarbeit/fabrik/internal/channelevents"
)

// Webhook recovery (#2427, FR-009 and FR-011): downtime and restarts lose nothing
// and apply nothing twice. A change is "announced once" when exactly one
// label-applied channel event reaches the test's subscriber, however the change got
// to the daemon (live delivery, replay, reconcile or the downtime delta); the
// subscription is persisted by the daemon, so it survives the restart.

// replayWait bounds the wait for the replay pass after a start: the tailscale
// transport holds the startup pass back 90 s for Funnel ingress, and a
// not-yet-resolvable Funnel name holds it back further (#2143, #2425).
const replayWait = 12 * time.Minute

// TestWebhookReplayAcrossDowntime (FR-009): with the bed daemon stopped, a human
// change is made; GitHub records the delivery as failed. On start the daemon finds
// the failed delivery in the App's log and requests its redelivery (#2144), the
// change is present, and it is announced once. "Before the first reconcile would
// have" is not provable (the startup poll precedes the 90 s replay grace), so the
// replay is proven by its own health row: found_failed and requested are at least 1.
func TestWebhookReplayAcrossDowntime(t *testing.T) {
	env := LoadEnv(t)
	webhookLegSetup(t, env)
	repo := env.RepoAlpha
	ensureBedRunning(t, env)
	issue, _ := fileInertItem(t, env, repo, "e2e webhook replay across downtime",
		"e2e scenario for #2427 FR-009: a change made while the daemon is down must be replayed. Paused so no stage runs.")
	const label = "fabrik:cruise"
	coll := startLabelCollector(t, env, "replay")

	StopFabrikTestBed(t, env)
	downAt := time.Now()
	AddLabel(t, env, repo, issue, label)

	// GitHub must have recorded the delivery as failed, or there is nothing to replay.
	d, ok, lastErr := findAppDelivery(env, downAt, "issues", "labeled", payloadIssueLabel(repo, issue, label), 3*time.Minute)
	if !ok {
		Inconclusive(t, "the App's delivery log shows no delivery for the label added while the bed was down (last error: %v), so there is nothing to replay", lastErr)
	}
	if d.Succeeded() {
		Inconclusive(t, "GitHub recorded the delivery made while the bed was down as succeeded (status %d), so there is nothing to replay", d.StatusCode)
	}

	StartFabrikTestBed(t, env)
	if !awaitCond(replayWait, func() bool {
		tr, err := healthTransport(env)
		if err != nil || tr.Replay == nil {
			return false
		}
		return tr.Replay.FoundFailed.Valid && tr.Replay.FoundFailed.V >= 1 && tr.Replay.Requested.Valid && tr.Replay.Requested.V >= 1
	}) {
		tr, _ := healthTransport(env)
		webhookEffectMissing(t, env, "the daemon did not replay the delivery that failed while it was down within %v (transport row: %+v)", replayWait, tr)
	}
	if !awaitDaemonLabel(env, repo, issue, label, true, replayWait) {
		t.Fatalf("after the replay the daemon does not hold %s on %s#%d", label, repo, issue)
	}
	if got := coll.awaitLabelEvent(channelevents.LabelApplied, repo, issue, label, 2*time.Minute); len(got) == 0 {
		// The change may have arrived as a downtime delta or reconcile event rather than a live one.
		if len(coll.allLabelEvents(channelevents.LabelApplied, repo, issue, label)) == 0 {
			t.Fatalf("the change made while the daemon was down was never announced")
		}
	}
	time.Sleep(15 * time.Second)
	if n := len(coll.allLabelEvents(channelevents.LabelApplied, repo, issue, label)); n != 1 {
		t.Errorf("got %d label-applied events for one change made during downtime, want exactly 1 (applied twice?)", n)
	}
}

// TestWebhookRestartUnderLoad (FR-011): a SIGHUP restart (ADR 2259) while label
// changes are being delivered loses none and applies none twice. The intake drains
// before the exec and replay or reconcile covers the gap; every label ends up in the
// daemon and is announced exactly once, and the labels already queued at the signal
// are announced as the human's own (applied by the drain, not healed by reconcile).
func TestWebhookRestartUnderLoad(t *testing.T) {
	env := LoadEnv(t)
	webhookLegSetup(t, env)
	repo := env.RepoAlpha
	ensureBedRunning(t, env)
	issue, _ := fileInertItem(t, env, repo, "e2e webhook restart under load",
		"e2e scenario for #2427 FR-011: label changes delivered across a SIGHUP restart must be applied once each. Paused so no stage runs.")
	const n = 6
	labels := make([]string, n)
	for i := range labels {
		labels[i] = fmt.Sprintf("e2e-webhook-load-%d", i+1)
		ensureLabel(t, env, repo, labels[i])
	}
	coll := startLabelCollector(t, env, "restart-load")

	// The first two labels are delivered and queued at the intake BEFORE the signal,
	// so they are applied by the pre-exec drain (ADR 2259, ADR 2351) — or lost to the
	// exec when the drain is neutralised. The rest are added around the restart and
	// reach the daemon by replay or reconcile if the transport was down for them.
	const before = 2
	if !awaitIntakeQuiet(env, 12*time.Second, 2*time.Minute) {
		Inconclusive(t, "the daemon's intake never went quiet after the scratch labels were created, so the deliveries this test counts cannot be told from stragglers")
	}
	// Polls are held from before these two land until the restart, so each label
	// reaches the Store from its own delivery (attributed to its sender) or, when
	// the drain is neutralised, from reconcile after the exec. With polls running,
	// the first label's delivery wakes a poll whose listing can carry the second
	// label before its own delivery is applied, and a listing-applied label has no
	// sender (actor unknown, ADR 2125) — the 0.0.90 gate's failure, not a drain fault.
	HoldPolls(t)
	base := mustReadIntake(t, env)
	for _, l := range labels[:before] {
		AddLabel(t, env, repo, issue, l)
	}
	if !awaitCond(webhookApplyWait, func() bool {
		c, err := readIntake(env)
		return err == nil && c.Accepted >= base.Accepted+before
	}) {
		webhookEffectMissing(t, env, "the daemon did not accept the %d label deliveries made before the restart within %v (intake counters before %+v)", before, webhookApplyWait, base)
	}
	// The hold stays on through the SIGHUP: the restarted process starts released
	// and clears the request (ADR 1978), so the ReleasePolls below is a no-op unless
	// the restart failed.
	signalled := time.Now()
	signalBed(t, env, syscall.SIGHUP)
	for _, l := range labels[before:] {
		AddLabel(t, env, repo, issue, l)
		time.Sleep(2 * time.Second)
	}
	if !awaitRestartSince(env, signalled, 3*time.Minute) {
		// The first SIGHUP only drains (ADR 2259); a second one completes the restart.
		signalBed(t, env, syscall.SIGHUP)
		if !awaitRestartSince(env, signalled, 5*time.Minute) {
			t.Fatalf("the bed daemon did not restart after SIGHUP within 8m")
		}
	}
	ReleasePolls(t)

	for _, l := range labels {
		if !awaitDaemonLabel(env, repo, issue, l, true, replayWait) {
			webhookEffectMissing(t, env, "after the restart the daemon does not hold %s on %s#%d: a delivery made around the restart was lost", l, repo, issue)
		}
	}
	time.Sleep(20 * time.Second)
	for i, l := range labels {
		evs := coll.allLabelEvents(channelevents.LabelApplied, repo, issue, l)
		if len(evs) != 1 {
			t.Errorf("label %s was announced %d times across the restart, want exactly 1", l, len(evs))
			continue
		}
		// A delivery already queued when the signal came must have been applied by the
		// drain, so its change is attributed to the human who made it. A delivery lost to
		// the exec is healed by reconcile and announced with no sender (actor unknown),
		// which is what the no-intake-drain fault produces.
		if a := evs[0].Meta["actor"]; i < before && a != "human" {
			t.Errorf("label %s was queued at the intake before the SIGHUP but is attributed actor=%q, want human: the pre-exec drain did not apply it", l, a)
		}
	}
}
