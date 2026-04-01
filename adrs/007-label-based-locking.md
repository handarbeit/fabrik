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
- **Recoverable**: If a Fabrik instance crashes, the label remains but can
  be manually removed. No orphaned locks in an external system.

## Locking Rules

1. **User filter**: Each instance only processes changes made by its `--user`.
   This is the primary guard — two instances rarely compete.
2. **Lock check**: Before processing, check for `fabrik:locked:<other-user>`
   labels. If present, skip.
3. **Editing guard**: The `fabrik:editing` label prevents any processing while
   the issue body is being rewritten.

## Trade-offs

- **Not atomic**: There's a race window between checking for a lock and
  acquiring it. In practice, the user-filter rule makes this unlikely.
- **No TTL**: Labels don't expire. A crashed instance leaves a stale lock.
  This is acceptable for a tool where the user is present and can intervene.
- **Label clutter**: Issues accumulate Fabrik labels. These could be cleaned
  up when an issue reaches Done.
