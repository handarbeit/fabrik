# Fabrik v0.0.48

This release is the v0.0.47 follow-up: both the live turn counter and the progress-based turn extension shipped in v0.0.47 had subtle bugs that surfaced in production use. Both are now fixed.

## Fixes

- **Live turn counter now matches Claude's own turn accounting (#447).** The TUI's live "N/M turns" badge was counting `{"type":"assistant"}` NDJSON events, but a single logical Claude turn produces one user event followed by *multiple* assistant events (one per `tool_use` block). On tool-heavy runs the badge inflated to e.g. `76/50` while Claude internally was at turn 50. Now counts `{"type":"user"}` events, which aligns exactly with Claude's `num_turns`. Same fix applied to `fabrik watch`'s independent log-follow path (`watch/logfollow.go`).

- **Progress-based extension now detects uncommitted work for Implement (#448).** The Implement progress signal was HEAD-SHA-only — if Claude spent 50 turns editing files without reaching a commit milestone, `detectProgress` returned false and the engine retried the entire stage from scratch. Real-world impact: develop issue #705 retried three times (~$21 total) on work a single 100–150 turn run could have completed. Implement now extends when SHA changed *or* when the working tree was clean at baseline and is now dirty. The baseline-clean guard prevents pre-existing dirty state from counting as progress.

- **`detectProgress` always logs its verdict.** Previously a `false` return was silent, making non-extensions impossible to diagnose without reflog forensics on the worktree. Every call now emits a structured log line on both pass and fail, listing the evaluated signals and the `has_progress` verdict. No debug flag required.

- **Startup board validation message clarified** as best-effort when the board fetch itself fails.

## Documentation

- **State-machine doc gains an executive summary + lifecycle overview SVG** (#444 follow-up). The §10 Mermaid diagrams were also unrenderable due to literal `\n` in note bodies; fixed and direction switched to top-to-bottom to avoid viewport overflow.
- **User guide stubs added** for Multi-Repo Support, Startup Board Validation, and Yolo Mode (filling broken anchors from the marketing site).
- **`fabrik:extend-turns` label and live turn counter documented** in USER_GUIDE, README, and the Help Panel (#442).
- Marketing site (`docs/index.md`): feature cards converted to brief tagline links; tagline tightening; CSS polish for block-link interactivity and focus rings.

## Internal

- `isWorkingTreeDirty` extracted as a shared helper for `commitWIP`, `updateWorktreeFromMain`, and the new progress check, with consistent filtering of engine-managed paths.
- New unit tests in `engine/extend_test.go` covering the dirty-tree progress path, baseline-dirty guard, log output verification, and `isWorkingTreeDirty` itself.
- Test helper rename (`import_` → `line` in `captureLogf`).

## Upgrading

```bash
# Auto-upgrade from a running Fabrik instance
# Fabrik checks for new releases each poll cycle and upgrades automatically with --auto-upgrade

# Or download directly (auto-detects OS and architecture)
gh release download --repo handarbeit/fabrik \
  --pattern "fabrik_*_$(uname -s | tr A-Z a-z)_$(uname -m | sed 's/x86_64/amd64/' | sed 's/aarch64/arm64/').tar.gz" \
  -O - | tar xz
```
