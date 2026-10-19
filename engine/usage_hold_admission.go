package engine

import (
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// usageHoldReinvokeRefused is the account-wide Claude usage-limit hold's gate for
// the re-invocation family (#2407, ADR 2407). It runs in dispatchReinvoke before a
// worker is registered, beside the forge hold: under an active hold no worker is
// entered, so no WorkerExited wakes the poll (the hot loop an awaiting-ci item's
// settle scan otherwise drove, one refused dispatch every few seconds), the cycle
// the caller charged is refunded, and the item gets an admission cooldown that
// expires when the hold does — the #2232 rule that every engine-scheduled retry
// owns one. Without that cooldown an item admitted by a consumed periodic
// re-evaluation and dispatched from catch-up Phase 1 (which marks it advanced, so
// the poll defer re-stamps nothing) was left with no admission path at all.
func (e *Engine) usageHoldReinvokeRefused(item gh.ProjectItem, itemRepo string, stage *stages.Stage, opts reinvokeOpts) bool {
	if e.usageHoldReinvokeGateDisabled.Load() {
		return false
	}
	if !e.recordUsageHoldCooldown(item) {
		return false
	}
	e.logfThrottled("usage-hold-reinvoke:"+issueKey(item, e.defaultRepo()), item.Number, "claude-limit",
		"Claude usage limited; not starting %s re-invocation for stage %q until the account-wide hold lifts\n", opts.tag, stage.Name)
	e.refundDidNotRunCycle(item, itemRepo, stage, opts.tag, didNotRunAccountUsageLimited, opts.cycle)
	return true
}

// recordUsageHoldCooldown records a retry-backoff admission cooldown expiring when
// the account-wide Claude usage-limit hold lifts, and reports whether a hold is
// active. The comment-review path calls it when it refuses for the hold, so the
// item is re-admitted on the first poll after the lift instead of at its next
// periodic re-evaluation (#2407).
func (e *Engine) recordUsageHoldCooldown(item gh.ProjectItem) bool {
	hold, held := e.claudeUsageLimitedUntilTime(e.now())
	if !held {
		return false
	}
	if !e.usageHoldReinvokeGateDisabled.Load() {
		e.recordStageRetryCooldownUntil(item, hold)
	}
	return true
}

// SetUsageHoldReinvokeGateDisabledForTest neutralises the #2407 fix for this
// engine: a re-invocation under the usage-limit hold registers a worker and is
// refused inside it, and no hold-end cooldown or did-not-run re-arm is recorded.
func (e *Engine) SetUsageHoldReinvokeGateDisabledForTest(disabled bool) {
	e.usageHoldReinvokeGateDisabled.Store(disabled)
}
