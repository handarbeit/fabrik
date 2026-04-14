# Fabrik v0.0.37

## Features

- **Three-phase review gate with re-invocation loop** — The Review stage now automatically addresses PR review feedback (from bots and human reviewers) before advancing the issue. When reviews are submitted, Claude is re-invoked to address each reviewer's comments, push fixes, and re-request review. A cycle cap prevents infinite loops. If the review timeout elapses, the issue is paused with `fabrik:awaiting-input` rather than silently advancing — giving humans visibility and control before the next stage begins.

## Fixes

- **Blocked items no longer get permanently stuck** — Dependency gating has been moved from `itemNeedsWork` to `processItem` (via `checkDependencies`). Previously, items with open blockers were silently skipped in `itemNeedsWork`, which never applied the `fabrik:blocked` label — so the `updatedAt` cache-bypass logic never re-evaluated them after blockers closed (blocker closure doesn't bump the blocked item's `updatedAt`). Now items pass through to `processItem`, which applies the label and ensures re-evaluation when dependencies resolve.
- **Skip re-invocation when all submitted PR reviews have empty bodies** — Previously, empty-body review submissions (e.g., approvals with no comment) could trigger an unnecessary Claude re-invocation. Fabrik now skips the re-invocation cycle when all submitted reviews in a batch have empty bodies.
- **TUI header never clipped** — Enforced a height invariant in the TUI so the header line is never truncated when the terminal is resized or the window is smaller than expected.
- **`history.View()` empty-string guard** — Guarded against an empty-string return from `history.View()` that could cause an off-by-one in height accounting, and synchronized `Height()` to match the actual rendered `View()` output.

## Internal

- Skills config checkpoint for own-dog-fooding configuration.
- USER_GUIDE: added Permissions section; fixed §9→§10 cross-reference display text.
- README: fixed permission wording to match current behavior.
- Added positioning notes for future marketing (internal docs).

## Upgrading

```bash
# Auto-upgrade from a running Fabrik instance
# Fabrik checks for new releases each poll cycle and upgrades automatically with --auto-upgrade

# Or download directly
gh release download --repo handarbeit/fabrik --pattern '*darwin_arm64*' -O - | tar xz
```
