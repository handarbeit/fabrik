# Fabrik v0.0.61

Targeted release adding the `FABRIK_NO_WORK_NEEDED` marker. Plan (or any earlier stage) can now signal that the issue requires no implementation and the engine cleanly short-circuits the pipeline to Done — instead of advancing to Implement, finding no commits, and failing PR creation with `HTTP 422: No commits between main and <branch>`.

## Features

- **`FABRIK_NO_WORK_NEEDED` marker** (#733). Any stage that determines the issue is complete-with-no-action (e.g., a docs audit on a pure-internal release, or an issue filed against an already-resolved problem) can emit `FABRIK_NO_WORK_NEEDED` on a line of its own alongside `FABRIK_STAGE_COMPLETE`. The engine marks the current stage complete, marks all subsequent non-cleanup stages as `:complete` with a "skipped: no work needed" comment, and advances directly to Done. PR creation is skipped entirely, so empty-commit branches no longer fail with HTTP 422. The fabrik-plan skill is updated with explicit guidance on when to emit the marker. Documented in `docs/state-machine.md` §5.6 and ADR-045.

## Fixes

- The skipped-stage comment posted when `FABRIK_NO_WORK_NEEDED` short-circuits a stage is now prefixed with the canonical Fabrik header line (matching other engine-posted comments).
- Internal documentation correction for `buildPrompt()`'s mutual-exclusivity description of the new marker.

## Internal

- New tests for `CheckNoWorkNeeded` (marker detection) and `handleNoWorkNeeded` (pipeline short-circuit dispatch).
- `docs/llms-full.txt` regenerated after `state-machine.md` update.

## Upgrading

```bash
# Auto-upgrade from a running Fabrik instance
# Fabrik checks for new releases each poll cycle and upgrades automatically with --auto-upgrade

# Or download directly (auto-detects OS and architecture)
gh release download --repo handarbeit/fabrik \
  --pattern "fabrik_*_$(uname -s | tr A-Z a-z)_$(uname -m | sed 's/x86_64/amd64/' | sed 's/aarch64/arm64/').tar.gz" \
  -O - | tar xz
```
