# ADR 007: Label-Based Locking for Multi-User Safety

## Status

Accepted

## Context

Multiple users can run their own Fabrik instances against the same project board.
We need to prevent two instances from processing the same issue simultaneously.

## Decision

Use GitHub issue labels as lightweight locks:
- `fabrik:locked:<username>` — issue is being processed by this user's instance
- `fabrik:editing` — issue body is being updated (prevents concurrent edits)

## Rationale

- **Visible**: Labels are visible on the issue and in the board UI. Anyone
  can see who has locked what.
- **No infrastructure**: No external lock service, no database, no Redis.
  Labels are stored in GitHub alongside everything else.
- **Self-documenting**: The label name tells you who locked it and why.
- **Self-healing**: If a Fabrik instance crashes, stale `fabrik:locked:<user>` and `fabrik:editing` labels are automatically removed by `runStartupCleanup()` on the next restart. No orphaned locks in an external system. If labels cannot be cleared automatically (e.g., network errors exhaust retry budget), they can be manually removed.

## Locking Rules

1. **Lock-then-verify**: Before processing, add `fabrik:locked:<user>`. Then
   sleep 2 seconds and re-fetch labels. If another `fabrik:locked:*` label is
   present, apply lexicographic tie-breaking: the instance whose username sorts
   **lower** keeps its lock and proceeds; the **higher** username releases its
   lock and skips this poll cycle. This is deterministic — exactly one instance
   wins any conflict, with no deadlock or livelock.
2. **Lock check**: Before acquiring a lock, check for `fabrik:locked:<other-user>`
   labels already present (from a previous poll cycle's winner). If present, skip.
3. **Editing guard**: The `fabrik:editing` label prevents any processing while
   the issue body is being rewritten.

## Trade-offs

- **2-second sleep per lock acquisition**: The sleep window allows competing
  instances to place their lock before either checks. This adds ~2s latency to
  every stage start, which is acceptable given typical poll intervals.
- **Not perfectly atomic**: There remains a tiny race window between the re-fetch
  and acting on results; in practice this window is negligible and the
  tie-breaking rule handles the worst case correctly.
- **No TTL**: Labels don't expire. A crashed instance leaves a stale lock, but `runStartupCleanup()` removes stale `fabrik:locked:<user>` and `fabrik:editing` labels on the next restart, so the stuck state is self-healing without manual intervention in the common case.
- **Label clutter**: Issues accumulate Fabrik labels. These could be cleaned
  up when an issue reaches Done.
- **Same-username tie-breaking degrades to "both win"**: If two instances share
  the same `--user`, tie-breaking is a no-op and both proceed. This is an
  unsupported configuration.
