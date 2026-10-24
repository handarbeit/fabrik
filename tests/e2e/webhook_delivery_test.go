//go:build e2e

package e2e

import (
	"time"

	"testing"

	ghapi "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/channelevents"
)

// TestWebhookRedeliveryAppliedOnce (FR-008): redelivering a delivery through the
// App's /app/hook/deliveries/{id}/attempts is accepted once. The daemon's ledger
// rejects the redelivered copy as a duplicate (counted in webhook.intake), and the
// change it carries is announced once, not twice. The HTTP success of the
// redelivery request is never taken as proof of anything; only the daemon's own
// state is.
func TestWebhookRedeliveryAppliedOnce(t *testing.T) {
	env := webhookLegSetup(t)
	repo := env.RepoAlpha
	issue, _ := fileInertItem(t, env, repo, "e2e webhook redelivery dedupe",
		"e2e scenario for #2427 FR-008: a redelivered webhook must be applied once. Paused so no stage runs.")
	const label = "fabrik:cruise"
	coll := startLabelCollector(t, env, "redelivery")

	HoldPolls(t)
	base := mustReadIntake(t, env)
	started := time.Now()
	AddLabel(t, env, repo, issue, label)
	if _, ok := awaitApplied(env, base, "issues", webhookApplyWait); !ok {
		webhookEffectMissing(t, env, "the original issues/labeled delivery was not applied within %v while polls were held", webhookApplyWait)
	}
	if got := coll.awaitLabelEvent(channelevents.LabelApplied, repo, issue, label, webhookApplyWait); len(got) != 1 {
		t.Fatalf("got %d label-applied events after the original delivery, want 1", len(got))
	}

	// Identify OUR delivery by its payload, then ask GitHub to redeliver it.
	d, ok, lastErr := findAppDelivery(env, started, "issues", "labeled", payloadIssueLabel(repo, issue, label), 3*time.Minute)
	if !ok {
		Inconclusive(t, "could not find the label delivery for %s#%d in the App's delivery log (last error: %v), so there is nothing to redeliver", repo, issue, lastErr)
	}
	afterFirst := mustReadIntake(t, env)
	jwt, err := appJWT(env)
	if err != nil {
		t.Fatalf("minting an App JWT: %v", err)
	}
	if err := ghapi.RedeliverAppHookDelivery("", jwt, d.ID); err != nil {
		Inconclusive(t, "GitHub refused the redelivery request: %v", err)
	}

	var after intakeCounters
	if !awaitCond(3*time.Minute, func() bool {
		c, err := readIntake(env)
		after = c
		return err == nil && c.Duplicates > afterFirst.Duplicates
	}) {
		webhookEffectMissing(t, env, "the redelivered copy of delivery %s was never rejected as a duplicate within 3m (intake counters before %+v, now %+v)", d.GUID, afterFirst, after)
	}
	time.Sleep(10 * time.Second)
	if n := len(coll.allLabelEvents(channelevents.LabelApplied, repo, issue, label)); n != 1 {
		t.Errorf("got %d label-applied events for one change delivered twice, want 1: the redelivery was applied again", n)
	}
}
