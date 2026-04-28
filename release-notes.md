# Fabrik v0.0.53

This release adds stage-config drift detection at startup and bundles documentation updates that bring the as-built specs and the user-facing docs back in sync with v0.0.51 (project board indexer retry) and v0.0.52 (`mergeable_state` CI gate shortcut). The drift detection in particular would have caught the missing `wait_for_ci: true` flag on liminis's validate.yaml that contributed to verveguy/liminis#716 sitting stuck for hours during yesterday's GitHub outage.

## Features

- **Stage YAML drift detection at startup (#464).** When Fabrik starts, it now compares each loaded stage in `.fabrik/stages/` against the corresponding embedded default and logs a `[startup] warning: ...` line for every field present in the embedded version but missing locally (e.g. `wait_for_ci: true`, `wait_for_reviews: true` on Validate). Local stage YAMLs are NOT auto-overwritten — users may have intentional customizations — but the drift surfaces loudly so silent feature degradation after `fabrik upgrade` is no longer possible. Useful in particular for users whose stage configs predate later v0.0.x releases.

## Documentation

- **ADR 033 — `mergeable_state` over raw `check_runs`.** Documents the v0.0.52 decision to consult GitHub's branch-protection-aware `mergeable_state` before classifying raw check_runs in both `checkCIGate` and `attemptMergeOnValidate`, including why "clean" and "unstable" are accepted but "has_hooks" is not.
- **State machine spec updated for `mergeable_state` shortcut.** §1.4 (label semantics), §2.10 (CI Check Completed), §3.2 (Awaiting CI transitions), §5.4 (Auto-Merge on Validate), and §6.2 (catch-up loop Phase 1) all now reflect the new shortcut path and clarify that it sits *after* `stage:Validate:complete` is set, so it cannot skip Validate-Claude work.
- **USER_GUIDE and README updated** for both v0.0.51 board-fetch retry and the v0.0.52 CI gate behavior. CLAUDE.md gained a short note about the new stage-drift warning so contributors know to update embedded defaults when adding stage fields.

## Internal

- Several Copilot-review-driven fixups across ADR 033, the USER_GUIDE board-fetch note, and the drift-detection PR.

## Upgrading

```bash
# Auto-upgrade from a running Fabrik instance
# Fabrik checks for new releases each poll cycle and upgrades automatically with --auto-upgrade

# Or download directly (auto-detects OS and architecture)
gh release download --repo shadoworg/fabrik \
  --pattern "fabrik_*_$(uname -s | tr A-Z a-z)_$(uname -m | sed 's/x86_64/amd64/' | sed 's/aarch64/arm64/').tar.gz" \
  -O - | tar xz
```
