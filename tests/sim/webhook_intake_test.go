package sim

import (
	"fmt"
	"testing"
)

// The transport-neutral delivery intake (#2351, ADR 2351) through the real
// engine: dedupe by X-GitHub-Delivery across transports and restarts, per-item
// arrival order. The sim has no transport layer; Engine.EnableDeliveryIntakeForTest
// builds the intake the way a started transport's setup does and the scenarios
// submit deliveries as a transport would. The matching unit tests live in
// engine/intake_test.go and engine/webhook_acceptance_test.go; the headline
// "echo arrives before its write returns" ordering is unit-tested there too
// (the simulated GitHub has no mid-write hook).

func intakeEnv(t *testing.T) (*Env, int) {
	t.Helper()
	env := NewEnv(t, EnvOptions{Stages: smokeStages(), BoardCache: true})
	num := FileIssue(t, env, "intake", "body", "Specify", "fabrik:paused", "fabrik:awaiting-input")
	RunPolls(t, env, 3)
	return env, num
}

func labelDelivery(env *Env, num int, action, label string) []byte {
	return []byte(fmt.Sprintf(
		`{"action":%q,"sender":{"login":"alice"},"label":{"name":%q},"issue":{"number":%d},"repository":{"full_name":%q}}`,
		action, label, num, env.OwnerRepo))
}

// A duplicate delivery applies once, whichever transport carries it.
func TestWebhookIntake_DuplicateAppliesOnce(t *testing.T) {
	t.Parallel()
	env, num := intakeEnv(t)
	env.Engine.EnableDeliveryIntakeForTest("")
	key := fmt.Sprintf("%s#%d", env.OwnerRepo, num)
	const label = "intake-label"

	if !env.Engine.SubmitDeliveryForTest("webhook", "D1", "issues", labelDelivery(env, num, "labeled", label)) {
		t.Fatal("the first delivery was not queued")
	}
	env.Engine.DrainDeliveriesForTest()
	if !hasLabel(labelsOf(t, env, num), label) {
		t.Fatal("the first delivery was not applied")
	}

	// The engine (or a human) removes the label; the same delivery then arrives
	// again, from the same transport and from another.
	env.BoardCache.ApplyLabelRemoved(key, label)
	for _, transport := range []string{"webhook", "hookdeck"} {
		if env.Engine.SubmitDeliveryForTest(transport, "D1", "issues", labelDelivery(env, num, "labeled", label)) {
			t.Fatalf("a redelivery of D1 via %s was queued", transport)
		}
	}
	env.Engine.DrainDeliveriesForTest()
	if hasLabel(labelsOf(t, env, num), label) {
		t.Fatal("a duplicate delivery was applied a second time")
	}

	// Non-vacuity: a fresh delivery ID does apply.
	env.Engine.SubmitDeliveryForTest("webhook", "D2", "issues", labelDelivery(env, num, "labeled", label))
	env.Engine.DrainDeliveriesForTest()
	if !hasLabel(labelsOf(t, env, num), label) {
		t.Fatal("non-vacuity: a new delivery ID must be applied")
	}
}

// A delivery applied before a restart is not applied again after it.
func TestWebhookIntake_DuplicateAppliesOnceAcrossRestart(t *testing.T) {
	t.Parallel()
	env, num := intakeEnv(t)
	dir := t.TempDir()
	env.Engine.EnableDeliveryIntakeForTest(dir)
	const label = "intake-label"

	env.Engine.SubmitDeliveryForTest("hookdeck", "D1", "issues", labelDelivery(env, num, "labeled", label))
	env.Engine.DrainDeliveriesForTest()
	env.Engine.CloseDeliveryIntakeForTest() // shutdown flushes the ledger

	restarted := RestartEnv(t, env)
	cache, _ := restarted.Engine.UseBoardCacheForTest()
	restarted.BoardCache = cache
	restarted.Engine.EnableDeliveryIntakeForTest(dir)
	RunPolls(t, restarted, 3)
	// The restarted store knows the item without the label (a rebuild from GitHub,
	// where the label was never written).
	if hasLabel(labelsOf(t, restarted, num), label) {
		t.Fatal("precondition: the rebuilt store should not carry the webhook-only label")
	}

	if restarted.Engine.SubmitDeliveryForTest("webhook", "D1", "issues", labelDelivery(restarted, num, "labeled", label)) {
		t.Fatal("a delivery applied before the restart was queued again after it")
	}
	restarted.Engine.DrainDeliveriesForTest()
	if hasLabel(labelsOf(t, restarted, num), label) {
		t.Fatal("a delivery applied before the restart was applied again after it")
	}
	restarted.Engine.SubmitDeliveryForTest("webhook", "D3", "issues", labelDelivery(restarted, num, "labeled", label))
	restarted.Engine.DrainDeliveriesForTest()
	if !hasLabel(labelsOf(t, restarted, num), label) {
		t.Fatal("non-vacuity: a new delivery after the restart must be applied")
	}
}

// Deliveries about one item apply in arrival order.
func TestWebhookIntake_SameItemAppliesInArrivalOrder(t *testing.T) {
	t.Parallel()
	env, num := intakeEnv(t)
	env.Engine.EnableDeliveryIntakeForTest("")
	const label = "order-label"

	// labeled, unlabeled, labeled, unlabeled: the final state is only "absent" if
	// they apply in the order they arrived.
	for i, action := range []string{"labeled", "unlabeled", "labeled", "unlabeled"} {
		env.Engine.SubmitDeliveryForTest("webhook", fmt.Sprintf("O%d", i), "issues", labelDelivery(env, num, action, label))
	}
	env.Engine.DrainDeliveriesForTest()
	if hasLabel(labelsOf(t, env, num), label) {
		t.Fatal("deliveries were applied out of arrival order: the label survived its final removal")
	}
}
