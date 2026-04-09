# Fabrik v0.0.25

## Features

- **`fabrik:unrestricted` label** (#264) — Per-issue `--dangerously-skip-permissions` for Claude Code. Use when issues need to write to paths outside normal project settings (e.g. `.claude/skills/`).
- **Any-author comment processing** (#253) — Comments from all users (colleagues, bots, the configured user) now trigger processing. Previously only comments from `--user` were seen. Multi-instance safety via lock-then-verify protocol with deterministic tie-breaking.
- **Lock-then-verify protocol** (#253) — When two Fabrik instances race to lock the same issue, a lock-then-verify pattern with 2-second delay and lexicographic tie-breaking ensures exactly one winner. No deadlocks, no livelocks.
- **`/audit-documentation` skill** (#258) — Scan closed issues, compare against docs, file gap issues for undocumented features. Invoked as `/audit-documentation` or `/audit-documentation --since v0.0.20`.
- **Issue decomposition documentation** (#267) — Plan stage can now split oversized issues via `FABRIK_DECOMPOSED` marker. Documented in USER_GUIDE and README.

## Fixes

- **Archive grace period skipped when UpdatedAt is zero** — Items with zero `UpdatedAt` were archived immediately instead of respecting the 24-hour grace period. Fixed to skip archiving when timestamp is unknown.

## Improvements

- **Specify skill preserves problem statements** — The "Problem" section is now first in the spec template, with explicit instructions to never compress the user's motivation.
- **Implement skill includes doc updates** — User-facing features now include documentation tasks in the same PR, not deferred to follow-ups.
- **Research skill surfaces documentation impact** — Findings now note when features will need user doc updates.
- **Plan skill includes doc tasks** — Task checklists for user-facing features include explicit doc update tasks.

## Internal

- Project `.claude/settings.json` updated with Read/Edit/Write permissions for pipeline stages.
- Documentation updates across USER_GUIDE.md, README.md, and marketing site for formations, auto-archive, pending reviewer gate, skills, and labels.
- ADR updates for any-author processing and lock-then-verify.

## Upgrading

```bash
# Auto-upgrade from a running Fabrik instance
# Fabrik checks for new releases each poll cycle and upgrades automatically with --auto-upgrade

# Or download directly
gh release download --repo tenaciousvc/fabrik --pattern '*darwin_arm64*' -O - | tar xz
```
