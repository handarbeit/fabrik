# Fabrik v0.0.50

This release tightens the stage-skill instructions for emitting `FABRIK_STAGE_COMPLETE`. v0.0.49 and earlier left the marker description ambiguous enough that Claude could mimic the surrounding code formatting and emit it wrapped in backticks (`` `FABRIK_STAGE_COMPLETE` ``) — which the engine regex correctly rejects, but only after Claude had declared the stage "ready to merge." The result was an infinite re-dispatch loop where `fabrik:yolo` never triggered auto-merge because completion was never recognised.

## Fixes

- **Hardened `FABRIK_STAGE_COMPLETE` emission across all six stage skills.** Each skill (`fabrik-specify`, `fabrik-research`, `fabrik-plan`, `fabrik-implement`, `fabrik-review`, `fabrik-validate`) now states the exact engine regex (`^FABRIK_STAGE_COMPLETE$`), requires the marker as the sole content of its own line, and explicitly lists silently-rejected variants: backticks, code fence, bold, blockquote, embedded-in-sentence, trailing punctuation. The `fabrik-validate` skill additionally calls out the specific failure mode — "ready to merge but no marker" producing the dispatch loop — and includes a worked example showing the marker as plain text on a line of its own. Real-world impact: acme/widgets#716 ran Validate 12× over 54 minutes against a clean, mergeable PR before being unstuck manually.

## Documentation

- README and USER_GUIDE updates for the v0.0.49 conjunctive CI gate semantics (`fabrik:awaiting-ci` set on `FABRIK_STAGE_COMPLETE`, `stage:X:complete` deferred until CI green).

## Internal

- `cut-release` skill now requires explicit verification of the current branch (`git branch --show-current`) before any tag operations; closes a near-miss from v0.0.49 where pre-flight ran on a detached HEAD.

## Upgrading

```bash
# Auto-upgrade from a running Fabrik instance
# Fabrik checks for new releases each poll cycle and upgrades automatically with --auto-upgrade

# Or download directly (auto-detects OS and architecture)
gh release download --repo handarbeit/fabrik \
  --pattern "fabrik_*_$(uname -s | tr A-Z a-z)_$(uname -m | sed 's/x86_64/amd64/' | sed 's/aarch64/arm64/').tar.gz" \
  -O - | tar xz
```
