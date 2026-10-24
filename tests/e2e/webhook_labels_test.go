//go:build e2e

package e2e

import (
	"fmt"
	"time"

	"testing"

	"github.com/handarbeit/fabrik/internal/channelevents"
	"github.com/handarbeit/fabrik/internal/localapi"
)

// Live webhook coverage: labels and comments (#2427, FR-002–FR-004). Every test
// holds the bed's polls (HoldPolls, ADR 1978) so only a webhook delivery can move
// the daemon's copy, and reads the daemon — never GitHub — through its local API.
// "Applied" is proven by the intake counters in fabrik_health (webhook.intake),
// which only a delivery's apply increments; attribution by a channel subscriber.

const webhookApplyWait = 90 * time.Second

// TestWebhookHumanLabelByWebhook (FR-002): a label a human adds reaches the
// daemon's Store with polls held, attributed actor=human (not unknown), and survives
// whole polls afterwards.
func TestWebhookHumanLabelByWebhook(t *testing.T) {
	env := LoadEnv(t)
	webhookLegSetup(t, env)
	repo := env.RepoAlpha
	if login := TokenLogin(t, env.GHToken); login == "" {
		t.Fatal("cannot resolve the harness account's login")
	}
	assertHarnessAccountIsHuman(t, env)
	issue, _ := fileInertItem(t, env, repo, "e2e webhook human label",
		"e2e scenario for #2427 FR-002: a human-added label must reach the daemon by webhook, attributed to a human. Paused so no stage runs.")
	const label = "fabrik:cruise"
	coll := startLabelCollector(t, env, "human-label")

	HoldPolls(t)
	base := mustReadIntake(t, env)
	AddLabel(t, env, repo, issue, label)

	if !awaitDaemonLabel(env, repo, issue, label, true, webhookApplyWait) {
		webhookEffectMissing(t, env, "the daemon did not show %s on %s#%d within %v while polls were held", label, repo, issue, webhookApplyWait)
	}
	if _, ok := awaitApplied(env, base, "issues", webhookApplyWait); !ok {
		t.Errorf("the issues delivery was never counted as applied while polls were held (intake counters: %+v)", mustReadIntake(t, env))
	}
	evs := coll.awaitLabelEvent(channelevents.LabelApplied, repo, issue, label, webhookApplyWait)
	if len(evs) == 0 {
		webhookEffectMissing(t, env, "no live label-applied event for %s on %s#%d reached the subscriber", label, repo, issue)
	}
	for _, ev := range evs {
		if got := ev.Meta["actor"]; got != "human" {
			t.Errorf("label-applied %s on %s#%d has actor=%q, want human (an unattributed webhook label is the regression this test guards)", label, repo, issue, got)
		}
	}
	if len(evs) != 1 {
		t.Errorf("got %d label-applied events for one human label, want exactly 1", len(evs))
	}

	for i := 1; i <= 2; i++ {
		TriggerPoll(t)
		labels, err := daemonLabels(env, repo, issue)
		if err != nil {
			t.Fatalf("reading the daemon's labels after poll %d: %v", i, err)
		}
		if !hasLabel(labels, label) {
			t.Fatalf("after poll %d the daemon no longer holds %s on %s#%d: a reconcile reverted the webhook-applied label", i, label, repo, issue)
		}
	}
}

// TestWebhookCommentWakesItem (FR-003): a human comment on a paused item is
// delivered while polls are held, and the next poll — which only the comment makes
// admit the item — lifts the pause. Holding polls drops the wake-up itself (a held
// bed runs no cycle), so the structural proof the comment came by delivery is the
// issue_comment apply counter, which nothing but a delivery increments.
func TestWebhookCommentWakesItem(t *testing.T) {
	env := LoadEnv(t)
	webhookLegSetup(t, env)
	repo := env.RepoAlpha
	assertHarnessAccountIsHuman(t, env)
	issue, _ := fileInertItem(t, env, repo, "e2e webhook comment wakes item",
		"e2e scenario for #2427 FR-003: a human comment on a paused item must reach the daemon by webhook. Paused so no stage runs until the comment.")

	HoldPolls(t)
	base := mustReadIntake(t, env)
	CommentOnIssue(t, env, repo, issue, "e2e webhook wake: please acknowledge this comment; no change to the spec is needed.")

	if _, ok := awaitApplied(env, base, "issue_comment", webhookApplyWait); !ok {
		webhookEffectMissing(t, env, "the issue_comment delivery was not applied within %v while polls were held (intake counters: %+v)", webhookApplyWait, mustReadIntake(t, env))
	}
	// The comment is in the daemon's copy; one whole poll must now admit the item and
	// lift the pause (resumeAuthorised needs the comment to be human and after the pause).
	TriggerPoll(t)
	if !awaitDaemonLabel(env, repo, issue, "fabrik:paused", false, 4*time.Minute) {
		t.Fatalf("the daemon still holds fabrik:paused on %s#%d after a poll that followed the delivered comment: the comment never reached the daemon's view of the item", repo, issue)
	}
}

// TestWebhookEngineEchoMatched (FR-004): an engine label write's webhook echo is
// recognised as the engine's own — counted as an echo match, and the label's channel
// event is attributed to the engine's actor (overseer, via the requesting
// subscriber), never unknown or human, with no second event for the echo.
func TestWebhookEngineEchoMatched(t *testing.T) {
	env := LoadEnv(t)
	webhookLegSetup(t, env)
	repo := env.RepoAlpha
	issue, _ := fileInertItem(t, env, repo, "e2e webhook engine echo",
		"e2e scenario for #2427 FR-004: the webhook echo of an engine label write must be matched as the engine's. Paused so no stage runs.")
	const label = "fabrik:yolo"
	coll := startLabelCollector(t, env, "engine-echo")

	HoldPolls(t)
	base := mustReadIntake(t, env)
	var res localapi.ActionResult
	err := bedCall(env, localapi.MethodSetAutonomy, localapi.SetAutonomyParams{
		Subscriber: "e2e-webhook-echo", Issue: fmt.Sprintf("%s#%d", repo, issue), Mode: "yolo",
	}, &res)
	if err != nil {
		Inconclusive(t, "the overseer set_autonomy action (the engine label write that drives this test) was refused or failed: %v", err)
	}

	// The echo is the issues/labeled delivery of the engine's own write.
	var after intakeCounters
	if !awaitCond(webhookApplyWait, func() bool {
		c, err := readIntake(env)
		after = c
		return err == nil && c.EchoMatched > base.EchoMatched
	}) {
		webhookEffectMissing(t, env, "the webhook echo of the engine's %s write was not matched within %v (intake counters before %+v, now %+v)", label, webhookApplyWait, base, after)
	}

	evs := coll.awaitLabelEvent(channelevents.LabelApplied, repo, issue, label, webhookApplyWait)
	if len(evs) == 0 {
		t.Fatalf("no live label-applied event for the engine's %s write on %s#%d", label, repo, issue)
	}
	for _, ev := range evs {
		if a := ev.Meta["actor"]; a != "overseer" && a != "fabrik" {
			t.Errorf("the engine's own label write is attributed actor=%q, want overseer or fabrik: its echo was not recognised", a)
		}
	}
	// Give any stray second event (an unmatched echo) time to arrive, then require one.
	time.Sleep(10 * time.Second)
	if n := len(coll.labelEvents(channelevents.LabelApplied, repo, issue, label)); n != 1 {
		t.Errorf("got %d label-applied events for one engine write, want exactly 1 (the echo must not produce a second)", n)
	}
}
