# Fabrik v0.0.49

This release fixes the wasteful "Validate fires every 3 minutes during CI await" pattern observed on production issues. Under v0.0.48 and earlier, a Validate stage with `wait_for_ci: true` would re-invoke Claude on every poll cycle while CI was running — the `stage:Validate:complete` label was set as soon as Claude emitted `FABRIK_STAGE_COMPLETE`, but the dispatcher couldn't distinguish "Claude is done, engine is waiting on CI" from "stage needs work." Now it can.

## Fixes

- **Conjunctive CI gate completion (#456, ADR 032).** `stage:X:complete` is no longer applied immediately when Claude emits `FABRIK_STAGE_COMPLETE` on a `wait_for_ci: true` stage. Instead, `fabrik:awaiting-ci` is applied as the in-progress marker, and `stage:X:complete` is added only when `checkCIGate` confirms CI is green. The dispatcher (`itemNeedsWork`) treats `fabrik:awaiting-ci` as "engine owns the next decision" and skips re-dispatch. Catch-up loop entry broadened to admit items with either label so the gate continues to evaluate. Net effect: zero Claude invocations during CI await; CI is polled via cheap REST every cycle as designed. Real-world impact (verveguy/liminis #710): three full Validate Claude runs over six minutes during CI wait → zero with this fix.

- **R5 post-push registration race (#457).** `checkCIGate` rule R5 (`len(checkRuns) == 0` → "no CI configured, gate clears") was firing during the brief window between Claude's CI-fix push and GitHub registering the new check runs against the new HEAD SHA. Under the conjunctive gate design that's a load-bearing edge: a premature R5 firing would strip `fabrik:awaiting-ci` and let the dispatcher fire a fresh Validate run before CI even started. Fixed via a per-issue `prHasHadChecks` sticky flag — once we've ever seen check runs for a PR, `len=0` means "not yet registered, keep waiting" instead of "no CI configured." First-ever zero polls still clear the gate (preserves existing no-CI repo behavior).

- **`checkCIGate` no-PR path now applies the conjunctive label.** When `FetchLinkedPR` returns nil (no linked PR found), the function previously returned gate-cleared without adding `stage:X:complete` or removing `fabrik:awaiting-ci`. Under the new label semantics this would leave the item in CI-await forever. Now the no-PR path calls `addCompleteLabelAndRemoveCI` to match the no-check-runs and all-green paths.

- **CI gate timeout now covers the full CI-await window.** Previously, CI checks stuck in `queued`/`in_progress` indefinitely would never trigger `pauseForCITimeout` because the timeout was tied to `fabrik:awaiting-ci` being applied — and that label was only set on confirmed failure. Under ADR 032's expanded semantics (label present from `FABRIK_STAGE_COMPLETE`), the timeout now applies to stuck-pending checks too.

- **`addCompleteLabelAndRemoveCI` returns early on label-add failure.** A transient API error during `AddLabelToIssue` would previously drop `fabrik:awaiting-ci` while CI was still pending, allowing the dispatcher to re-invoke the stage on the next poll. Now the function returns early so the in-progress marker is preserved.

- **`itemNeedsWork` awaiting-ci guard scoped to `wait_for_ci: true` stages.** A stale `fabrik:awaiting-ci` label on a non-CI-gated stage would previously suppress dispatch permanently. The guard now only fires for stages that actually use the CI gate.

## Documentation

- New ADR 032 documenting the evolution from Approach A (immediate `stage:X:complete`) to Approach A' (deferred until CI green). Explains why ADR 027's rejection of Approach B doesn't apply, the semantic expansion of `fabrik:awaiting-ci`, and the dispatcher consequences.
- USER_GUIDE updated for v0.0.48 features: `fabrik:extend-turns` Implement progress signal expanded to mention the uncommitted-edits case; `[#N extend-turns]` verdict log lines added to the Poll Log reference so users can diagnose turn-extension decisions without `--debug-output`.

## Upgrading

```bash
# Auto-upgrade from a running Fabrik instance
# Fabrik checks for new releases each poll cycle and upgrades automatically with --auto-upgrade

# Or download directly (auto-detects OS and architecture)
gh release download --repo shadoworg/fabrik \
  --pattern "fabrik_*_$(uname -s | tr A-Z a-z)_$(uname -m | sed 's/x86_64/amd64/' | sed 's/aarch64/arm64/').tar.gz" \
  -O - | tar xz
```
