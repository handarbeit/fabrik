# Fabrik v0.0.52

This release fixes an over-aggressive CI gate that blocked auto-merge whenever any GitHub check run completed with `conclusion=failure` — even when the failed check was a non-required workflow job (e.g. `Cleanup artifacts`) and GitHub itself reported the PR as `mergeStateStatus=CLEAN`. The gate contradicted GitHub's own merge decision, kept reapplying `fabrik:awaiting-ci`, and held issues stuck in Validate even with `fabrik:yolo` set.

Real-world impact (acme/widgets#716): PR #717 was MERGEABLE/CLEAN with all required checks green, but a `Cleanup artifacts` check on the head SHA had failed early in the workflow run. Every catch-up loop iteration saw the failure, re-applied `fabrik:awaiting-ci`, and refused to merge — for hours, despite human-visible green CI.

## Fixes

- **CI gate now consults GitHub's branch-protection-aware `mergeable_state`.** Both `checkCIGate` and `attemptMergeOnValidate` now fetch `mergeable_state` from the linked PR (REST single-PR endpoint — the list endpoint returns null) before classifying raw check_runs. When `mergeable_state` is `clean` (ready to merge per branch protection) or `unstable` (non-required checks failing, still mergeable), the gate skips the raw check_runs classification and proceeds directly to the gate-cleared path. `checkCIGate` clears `stage:X:complete` + removes `fabrik:awaiting-ci`; `attemptMergeOnValidate` falls through directly to `MergePR`. Other states (`blocked`, `behind`, `dirty`, `unknown`, `has_hooks`, `draft`, empty) still fall through to the per-check classification so genuinely-blocked PRs get the right failure-vs-pending dispatch decision.

- **`fabrik:awaiting-ci` no longer survives a successful mergeable_state shortcut.** Stale labels left behind by the prior over-aggressive gate are explicitly cleared when the new shortcut path fires.

## Notes

If your existing `.fabrik/stages/validate.yaml` predates v0.0.49, it may be missing `wait_for_ci: true` and `wait_for_reviews: true`. Without those flags, the catch-up loop falls through to `attemptMergeOnValidate` (which is also fixed in this release) instead of the conjunctive CI gate. Either path now correctly handles non-required check_run failures, but the conjunctive gate provides better per-check observability and timeouts. Compare your local stage YAMLs against the embedded defaults in `plugin/fabrik-plugin` (or in the source repo) and update as needed; Fabrik does not auto-overwrite local stage configs.

## Upgrading

```bash
# Auto-upgrade from a running Fabrik instance
# Fabrik checks for new releases each poll cycle and upgrades automatically with --auto-upgrade

# Or download directly (auto-detects OS and architecture)
gh release download --repo handarbeit/fabrik \
  --pattern "fabrik_*_$(uname -s | tr A-Z a-z)_$(uname -m | sed 's/x86_64/amd64/' | sed 's/aarch64/arm64/').tar.gz" \
  -O - | tar xz
```
