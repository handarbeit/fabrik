# Fabrik v0.0.39

## Features

- Review-feedback processing now runs for **all** issues, not just `fabrik:yolo` or `fabrik:cruise` (#392). The catch-up loop is split into Phase 1 (review gate + review reinvoke — unconditional) and Phase 2 (advancement — still gated). Copilot/Gemini/human inline review comments are addressed automatically after Review and Validate complete.
- Post a Fabrik-marked summary comment on the PR after addressing review-thread feedback, with a machine-generated per-thread footer (#394). Reviewers can confirm at a glance which threads were addressed.
- `processComments` now widens its input on PR-backed items to include unresolved review-thread comments alongside any user-nudge conversation comments — closes the race where a user nudge would leave threads unresolved (#392).
- Seed PR body from context files on creation and auto-update a "Verification" section as stages complete.
- Unified idle/rate-limit backoff in the poll loop — `w` key wakes the poll early; backoff honors rate-limit reset windows.
- Auto-react with 🚀 on engine-posted comments as a secondary dedup signal, complementing the 🏭 header check (#399).

## Fixes

- Prefix every engine-emitted comment with the 🏭 Fabrik header (#398). Three sites were missing the prefix (base-branch fallback, unmergeable-PR notice, dependency-block notice), which caused Fabrik to re-process its own comments as user input.
- Key `reviewCycleCount` by stage instead of by issue (#393). Review's reinvoke cycles no longer consume Validate's budget.
- Emit `JobStarted`/`JobCompleted` TUI events for review reinvoke — review-feedback processing now appears in the In Progress panel.
- `AddComment` correctly captures the created comment ID from the REST response; empty-path review-thread comments fall back cleanly in the per-thread footer.
- Rate-limit backoff no longer caps at the 5-minute idle ceiling — long resets are honored in full.

## Improvements

- Broaden the `fabrik-plan` skill's doc-impact framing to cover engineering/as-built docs, not only user-facing ones (#395).
- Add a "Canonical Documentation" section to Fabrik's root `CLAUDE.md` naming `docs/state-machine.md` and `docs/stage-lifecycle.md` as authoritative as-built docs that must be updated alongside engine behavior changes (#396).
- Ship a comprehensive issue state machine specification at `docs/state-machine.md` — the authoritative as-built reference for state transitions, label semantics, marker handling, review gating, and PR lifecycle coupling (#383).
- Prohibit direct `gh pr comment` / `gh issue comment` posting in stage skills with `post_to_pr: true` (#397). Claude's output flows to stdout only; the engine is the single posting point.

## Internal

- `.fabrik/plugin/` is no longer tracked in git — it is a generated mirror refreshed from the embedded source on each `fabrik init` / `fabrik upgrade`. The authoritative source is `plugin/fabrik-plugin/skills/`.
- Unit test coverage expanded for review-reinvoke detection, thread-entry building, PR summary formatting, per-stage cycle counting, idle backoff, and TUI header rendering.

## Upgrading

```bash
# Auto-upgrade from a running Fabrik instance
# Fabrik checks for new releases each poll cycle and upgrades automatically with --auto-upgrade

# Or download directly
gh release download --repo handarbeit/fabrik --pattern '*<os>_<arch>*' -O - | tar xz
```
