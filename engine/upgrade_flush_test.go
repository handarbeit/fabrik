package engine

import (
	"path/filepath"
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
