# ADR 002: GitHub Issues + Projects as Sole State Store

## Status

Accepted

## Context

Fabrik needs to track work items, their status in the workflow, comments/feedback,
and stage completion. We needed to decide where authoritative state lives.

## Decision

Use GitHub Issues and GitHub Projects (v2) as the sole state store. No custom
database, no local state files beyond ephemeral caches.

## Rationale

- **Already exists**: Teams already use GitHub Issues for tracking work. Fabrik
  plugs into existing workflows rather than requiring migration.
- **Cloud-hosted**: Shared state accessible to all team members without
  infrastructure setup.
- **Multi-user**: Built-in access control, notifications, and collaboration
  features.
- **Rich metadata**: Issues have titles, bodies, labels, assignees, comments,
  and project board status — all the primitives we need.
- **API support**: GitHub's GraphQL API lets us fetch an entire project board
  in a single query.
- **Auditability**: Full history of changes, comments, and state transitions
  is preserved by GitHub.

## Consequences

- **Rate limits**: GitHub API rate limits apply. Polling frequency must be
  balanced against the 5,000 requests/hour limit.
- **Latency**: Changes are detected on the next poll cycle, not instantly.
- **GitHub dependency**: Fabrik is tightly coupled to GitHub. Supporting
  other platforms (GitLab, Linear) would require a provider abstraction.
- **No offline mode**: Requires internet connectivity to function.

## Local State

Fabrik maintains local ephemeral state only:
- `processedSet` (in-memory): Tracks which items/comments have been processed
  in the current session to avoid re-processing.
- Session files (`~/.fabrik/sessions/`): Claude Code session IDs for resumption.
- Worktrees (`.fabrik/worktrees/`): Git worktrees for issue isolation.

None of this is authoritative — it's a cache that can be safely deleted.
