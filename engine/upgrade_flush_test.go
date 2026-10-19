package engine

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// The self-upgrade exec runs no deferred cleanup, so its PreExecHook must flush
// the delivery ledger's debounce window (#2351): an ID accepted in the last
// second is still deduplicated after the restart.
func TestFlushBeforeExec_PersistsTheDeliveryLedger(t *testing.T) {
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	dir := t.TempDir()
	eng.EnableDeliveryIntakeForTest(dir)

	if !eng.SubmitDeliveryForTest("webhook", "ACCEPTED-LAST-SECOND", "issues", payloadFor("o/r", 1)) {
		t.Fatal("first delivery must be queued")
	}
	eng.DrainDeliveriesForTest()

	eng.flushBeforeExec() // what both self-upgrade configs run before syscall.Exec

	reloaded := newDeliveryLedger(filepath.Join(dir, deliveryLedgerFileName), time.Now(), nil)
	if reloaded.Accept("ACCEPTED-LAST-SECOND", time.Now()) {
		t.Fatal("an ID accepted before the exec was not persisted: the restarted process would apply it again")
	}
}

func TestSelfUpgradeConfigsFlushBeforeExec(t *testing.T) {
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	if eng.devBuildConfig().PreExecHook == nil || eng.releaseUpgradeConfig().PreExecHook == nil {
		t.Fatal("both self-upgrade configs must carry a PreExecHook")
	}
}

func TestFlushBeforeExec_NoIntakeIsInert(t *testing.T) {
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	eng.flushBeforeExec() // nil intake, no hub: must not panic
}

func TestSelfUpgradeConfigsResumeAfterAFailedExec(t *testing.T) {
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	if eng.devBuildConfig().ExecFailedHook == nil || eng.releaseUpgradeConfig().ExecFailedHook == nil {
		t.Fatal("both self-upgrade configs must resume the intake when the exec returns")
	}
}

// The exec must not lose a delivery the ledger already records as seen: a blocked
// apply makes flushBeforeExec wait up to the grace, new work is refused (and not
// recorded) meanwhile, and a failed exec resumes the intake.
func TestFlushBeforeExec_WaitsForInFlightDeliveriesUpToTheGrace(t *testing.T) {
	old := intakeQuiesceGrace
	intakeQuiesceGrace = 200 * time.Millisecond
	t.Cleanup(func() { intakeQuiesceGrace = old })

	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	release := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	in := newTestIntake(func(string, []byte) { <-release })
	eng.intake = in
	if !in.Submit(delivery{ID: "in-flight", EventType: "issues", Payload: payloadFor("o/r", 1)}) {
		t.Fatal("first delivery must be queued")
	}

	start := time.Now()
	eng.flushBeforeExec()
	if waited := time.Since(start); waited < 150*time.Millisecond || waited > 3*time.Second {
		t.Fatalf("flushBeforeExec waited %v with a blocked apply, want about the %v grace", waited, intakeQuiesceGrace)
	}

	// Quiesced: new work is refused and never recorded as seen.
	if in.Submit(delivery{ID: "during-quiesce", EventType: "issues", Payload: payloadFor("o/r", 2)}) {
		t.Fatal("a delivery was accepted while the intake was quiesced for the exec")
	}
	// The exec failed: the intake accepts work again, including the refused ID.
	eng.resumeAfterFailedExec()
	if !in.Submit(delivery{ID: "during-quiesce", EventType: "issues", Payload: payloadFor("o/r", 2)}) {
		t.Fatal("the intake did not resume after a failed exec (or the refused ID was recorded as seen)")
	}
	once.Do(func() { close(release) })
	in.Drain()
}

// When nothing is blocked the quiesce returns as soon as the intake is idle.
func TestFlushBeforeExec_DoesNotWaitWhenIdle(t *testing.T) {
	old := intakeQuiesceGrace
	intakeQuiesceGrace = 5 * time.Second
	t.Cleanup(func() { intakeQuiesceGrace = old })

	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	rec := &recorder{}
	in := newTestIntake(rec.apply)
	eng.intake = in
	in.Submit(delivery{ID: "a", EventType: "issues", Payload: payloadFor("o/r", 1)})

	start := time.Now()
	eng.flushBeforeExec()
	if waited := time.Since(start); waited > 2*time.Second {
		t.Fatalf("flushBeforeExec took %v for an idle intake", waited)
	}
	if len(rec.list()) != 1 {
		t.Fatal("the queued delivery was not applied before the exec")
	}
}
