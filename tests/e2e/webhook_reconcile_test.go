//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/internal/localapi"
)

// TestWebhookChangeSurvivesReconcile is the live half of #2348 (ADR 2348, the
// observed-at ordering rule): a column move a human makes in GitHub reaches the
// daemon by webhook, and the next poll — the probe and the Layer 2 status sweep,
// whose listings are fetched from a read that lags GitHub — must not revert it.
//
// The sim twins (tests/sim/webhook_ordering_test.go) prove the rule with a held,
// stale listing; the sim cannot model GitHub's real read lag or its real webhook
// delivery, which is what this test adds. It holds the bed's polls first, so the
// only thing that can move the daemon's copy to the new column is the webhook; if
// the daemon never shows the move that is a failure on a webhook leg, where the
// delivery preflight has proved delivery (#2426, FR-005), and Inconclusive
// anywhere else. Then it triggers whole polls and asserts that
// the daemon still shows the new column.
//
// The item is filed paused at Specify, so no Claude stage runs; the move is
// Specify to Research. It needs a leg whose bed engine receives webhooks: the gate
// starts it with the embedded Tailscale Funnel receiver for E2E_EVENT_SOURCE=
// tailscale on an App leg (#2408, tests/e2e/bedevents), and a webhook delivery
// preflight has by then proved delivery. A poll leg, and every PAT leg (the
// transport is App-auth only), skip.
func TestWebhookChangeSurvivesReconcile(t *testing.T) {
	env := LoadEnv(t)
	AssertFabrikRunning(t, env)
	if !bedWebhooksEnabled(env) {
		t.Skip("this leg has no webhook transport (poll-only event source, or a PAT leg) — a column move would reach the daemon only through a listing, so there is no webhook-applied change to protect; run it with E2E_EVENT_SOURCE=tailscale on an App leg (a webhook cell of the gate plan runs it; on a poll cell the skip is structural, */*/poll)")
	}
	repo := env.RepoAlpha

	stamp := time.Now().UTC().Format("20060102-150405")
	issue := FileIssue(t, env, repo, "e2e webhook change survives reconcile "+stamp,
		"e2e scenario for #2348: a webhook-applied column move must survive the next poll's lagging listings. Paused so no stage runs.",
		"fabrik:paused", "fabrik:awaiting-input")
	itemID := AddIssueToProject(t, env, repo, issue)
	SetIssueStatus(t, env, itemID, "Specify")
	AwaitStatusVisible(t, env, repo, issue, "Specify", awaitSeedTimeout)

	column := func() (string, error) { return daemonColumn(env, repo, issue) }
	if !awaitColumn(column, "Specify", 3*time.Minute) {
		Inconclusive(t, "the daemon never showed #%d at Specify within 3m, so there is no starting state to move from", issue)
	}

	// From here only a webhook can change the daemon's copy.
	HoldPolls(t)
	SetIssueStatus(t, env, itemID, "Research")
	AwaitStatusVisible(t, env, repo, issue, "Research", awaitSeedTimeout)
	if !awaitColumn(column, "Research", 90*time.Second) {
		webhookEffectMissing(t, env, "the daemon did not apply the webhook column move within 90s while polls were held (webhook feed down or delayed)")
	}

	// Whole polls, twice: the probe, the status sweep and every settle scan run
	// against GitHub's current — and possibly lagging — reads.
	for i := 1; i <= 2; i++ {
		TriggerPoll(t)
		got, err := column()
		if err != nil {
			t.Fatalf("reading the daemon's column after poll %d: %v", i, err)
		}
		if got != "Research" {
			t.Fatalf("after poll %d the daemon shows %q for #%d, want Research: a listing reverted the webhook-applied move", i, got, issue)
		}
	}
}

// daemonColumn reads the board column the bed's daemon holds for an issue from its
// local API (fabrik_status), without any GitHub call.
func daemonColumn(env *Env, repo string, issue int) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var res localapi.StatusResult
	err := localapi.Call(ctx, localapi.SocketPath(env.FabrikTestDir), localapi.MethodStatus,
		localapi.StatusParams{Issue: fmt.Sprintf("%s#%d", repo, issue)}, &res)
	if err != nil {
		return "", err
	}
	if !res.Place.Status.Valid {
		return "", fmt.Errorf("the daemon holds no column for %s#%d yet", repo, issue)
	}
	return strings.TrimSpace(res.Place.Status.V), nil
}

// awaitColumn polls read until it reports want or timeout expires. A read error is
// retried, never treated as "not there".
func awaitColumn(read func() (string, error), want string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if got, err := read(); err == nil && got == want {
			return true
		}
		time.Sleep(3 * time.Second)
	}
	return false
}

// TestWebhookEffectMissingDecision pins FR-005 (#2426): a missing webhook effect
// fails on a webhook leg and is Inconclusive anywhere else. It is a harness unit
// test (no LoadEnv), so it is not a live scenario.
func TestWebhookEffectMissingDecision(t *testing.T) {
	if !webhookEffectMissingFails(true) {
		t.Error("a webhook leg must fail on a missing webhook effect")
	}
	if webhookEffectMissingFails(false) {
		t.Error("a poll leg must keep the Inconclusive outcome")
	}
}
