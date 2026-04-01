# ADR 010: File Granularity and Module Structure

## Status

Accepted

## Context

As Fabrik's codebase evolved, several files accumulated multiple distinct
concerns. The primary hotspot was `engine/engine.go`, which grew to over 1,000
lines and handled poll loop orchestration, item lifecycle, comment processing,
PR workflow, stage advancement, label management, and worker concurrency — all
in a single file.

Because many features touch overlapping concerns, PRs frequently conflicted on
the same files, creating unnecessary merge churn.

## Decision

Organize files by **single concern**. Each file in a package should own one
logical domain. Files that grow to handle multiple unrelated concerns should be
split along domain boundaries.

### Principles

1. **One concern per file.** A file should handle one logical domain — not
   "everything in the engine package." If you cannot summarize a file's purpose
   in five words, it is doing too much.

2. **Name files after what they do.** `pr.go` handles PR workflows.
   `comments.go` handles comment processing. `poll.go` handles the poll loop.
   Avoid generic names like `utils.go`, `helpers.go`, or `misc.go`.

3. **Group by domain within packages.** It is acceptable for multiple files to
   share a package (e.g., `engine`). Splitting into sub-packages is not required
   and risks circular imports. File-level separation within a package is
   sufficient.

4. **Struct definitions and constructors stay in the package's root file.**
   The central struct (e.g., `Engine`) and its `New`/`NewWithDeps` constructors
   live in the package's eponymous file (e.g., `engine/engine.go`). Other files
   hold methods on that struct, organized by concern.

5. **Rough size guideline: under 300 lines.** This is a heuristic, not a hard
   limit. A file can exceed 300 lines if its concern is genuinely cohesive (e.g.,
   a single large GraphQL query with its helpers). Use judgment; the goal is
   clarity, not line-count compliance.

6. **Test files mirror implementation files.** When an implementation file is
   split, prefer splitting the corresponding test file to match. This is not
   mandatory for existing test files, but new test code should follow this rule.

## Applied Refactor (Issue #57)

This ADR was written alongside a concrete refactor that applied these principles:

**`engine/engine.go`** reduced to struct definition, constructors, and shared
helpers. Concerns extracted to:
- `engine/poll.go` — poll loop, worker management, git utilities
- `engine/item.go` — item lifecycle, work detection, label removal
- `engine/comments.go` — comment detection and processing
- `engine/pr.go` — PR creation, readiness, output posting
- `engine/stages.go` — stage advancement and completion

**`github/mutations.go`** (mixed domain mutations) replaced by:
- `github/labels.go` — label mutations
- `github/comments.go` — comment and issue body mutations
- `github/prs.go` — PR mutations
- `github/status.go` — project item status mutations

## Rationale

- **Reduced PR contention.** PRs that touch comment processing no longer need
  to modify the same file as PRs that touch PR workflow. Conflicts become rare
  rather than routine.
- **Faster orientation.** A contributor debugging PR readiness logic goes
  directly to `engine/pr.go` rather than scanning 1,000 lines.
- **Smaller, reviewable diffs.** PRs are easier to review when the changed file
  has a clear, narrow purpose.
- **No import cost.** File splits within a package carry zero runtime or
  compilation cost. This refactor changes no exported names, interfaces, or
  behaviors.

## Trade-offs

- **More files to navigate.** A developer unfamiliar with the structure may
  need to look across multiple files to trace a workflow. Mitigated by
  consistent, descriptive naming.
- **Discipline required.** The principles only help if followed. New code should
  be placed in the appropriate existing file or a new focused file — not
  appended to the nearest large file.
