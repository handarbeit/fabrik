# ADR 051: Engine-Mechanized PR Creation via `FABRIK_PR_CREATE` Marker

**Date**: 2026-05-28  
**Status**: Accepted  
**Extends**: [ADR 048](048-spawn-child-engine-side.md)

## Context

The Implement skill was responsible for creating the draft PR via `gh pr create` and for including `Closes #N` in the PR body so GitHub's `closingIssuesReferences` / `closedByPullRequestsReferences` fields would link the PR to its parent issue.

In production, a Fabrik-driven Implement stage produced a PR body that referenced an internal FR identifier (`Fixes FR-007`) but omitted the canonical `Closes #N` closing reference. The consequences cascaded:

- **Review gate** (`checkReviewGate`): reads `item.LinkedPRReviews`, populated only when the GraphQL deep-fetch finds a linked PR via `closedByPullRequestsReferences`. With no link, `hasReviews` stayed false and `fabrik:awaiting-review` was reapplied on every poll — indefinitely — despite six reviews having been submitted. The user saw no diagnostic signal.
- **CI gate and auto-merge**: similarly depend on `LinkedPR` state populated through the same GraphQL linkage.

The root cause: the engine seeded the draft PR body with `Closes #N` as the first line, but the Implement skill later replaced the body via `gh pr edit`, overwriting the closing keyword. The skill prompt asked Claude to maintain the closing keyword — a constraint Claude can silently violate when producing long output.

## Decision

Move PR creation authority from the Implement skill to the engine, using the same `FABRIK_*_BEGIN/END` structured-marker convention established by ADR 048 for sub-issue spawning.

**Implement skill** emits a `FABRIK_PR_CREATE_BEGIN/END` block in its output:

```
FABRIK_PR_CREATE_BEGIN
TITLE: <single-line PR title>

<PR body content — no closing keyword>
FABRIK_PR_CREATE_END
```

The skill is explicitly prohibited from calling `gh pr create` and from writing any closing keyword (`Closes`, `Fixes`, `Resolves` + `#N`).

**Engine** (`engine/prcreate.go`) processes the marker block during `processItem()`:

1. Parses title and body from the block.
2. Composes the final PR body as `"Closes #N\n\n" + block.Body` — the closing keyword is the engine's responsibility and is always the first line.
3. Calls `CreateDraftPR()` with the composed body.
4. Records the PR number in the Store and posts an acknowledgement comment.
5. Falls back to `ensureDraftPR` (legacy path) when no marker is found.

**Post-Implement linkage backstop** (`verifyAndHealLinkage`): after any Implement completion, the engine verifies `closedByPullRequestsReferences` via `FetchItemDetails`. If linkage is missing, it attempts one auto-heal: prepend `Closes #N\n\n` to the existing PR body and re-verify. An in-memory `LinkageHealAttempted` flag (keyed by PR head SHA) prevents repeated heal attempts. If heal fails or cannot be attempted, the issue is paused with `fabrik:paused` and a copy-paste `gh pr edit` recovery command.

**Gate-side broken-linkage detection** (`checkReviewGate`): if `item.LinkedPRNumber == 0` despite a PR existing on the `fabrik/issue-N` branch (discovered via `FetchLinkedPR` REST fallback), the gate pauses the issue rather than silently reapplying `fabrik:awaiting-review`. `fabrik:awaiting-review` is NOT applied in the broken-linkage case.

## Rationale

### Engine-owns-mutations (ADR 048 extended)

The core principle from ADR 048 applies: GitHub mutations that Fabrik's engine depends on for correctness MUST go through `GitHubClient` in engine code, not through Claude subprocess `gh` calls. PR creation with the closing keyword is a mutation the engine's every downstream gate depends on — it belongs in engine code.

### First-line placement

`Closes #N` is the first line of the PR body (not a footer) for two reasons:
1. **Visibility**: humans reading the PR see the parent issue link immediately.
2. **Reliability**: GitHub's closing-keyword detection is not positionally sensitive, but placing the keyword first makes it harder for subsequent `gh pr edit` calls (which could truncate or replace later sections) to inadvertently remove it.

### Belt-and-braces design

The marker path (primary), the post-Implement linkage backstop (secondary), and the gate-side detection (tertiary) are shipped together. In the transition period, some Implement sessions may still use the old skill version. The backstop catches those cases. Once all skill versions are updated, the backstop becomes a cold safety net — but it stays because future body edits can always re-introduce the problem.

## Alternatives Considered

**Post-process body injection (engine rewrites the body after `gh pr create`):** The skill still calls `gh pr create`; the engine then edits the body to prepend `Closes #N`. Rejected: requires the engine to know that a `gh pr create` just happened (no durable signal), and still leaves a race window where the uncorrected PR is visible.

**Skill-side guard (stronger prompt instructions):** The skill prompt is made more explicit. Rejected: the observed failure was a prompt-following failure; stronger prompts make the failure less likely but not impossible. The whole point of mechanization is to eliminate the "skill can silently drop it" failure class, not to reduce its probability.

**Append to footer (keep existing `ensurePRLinksIssue` behavior):** The existing `ensurePRLinksIssue` appends `Closes #N` to the body footer. Rejected as the sole mechanism: footer placement is less visible, and the function uses a `strings.Contains` check (not GraphQL verification) — it cannot detect when a subsequent body overwrite removes the keyword. The new post-Implement check uses the authoritative GraphQL `closingIssuesReferences` field.

## Consequences

- **Skill prompt changed**: The Implement skill no longer calls `gh pr create`. It emits `FABRIK_PR_CREATE_BEGIN/END`. This is a breaking change for skill versions that still use `gh pr create` — the backstop catches those cases during the transition period.
- **One extra GraphQL call per Implement completion**: `verifyAndHealLinkage` calls `FetchItemDetails` (and possibly a second time after healing). This is ~1–2 API calls per Implement stage — acceptable given infrequency.
- **`postHealComment` removed**: The heal confirmation comment is inlined into `verifyAndHealLinkage` to satisfy the `TestAddCommentCompliance` invariant (all `AddComment` body arguments must start with `"🏭 **Fabrik"`).
- **`PRCreationFailed` retry path unchanged**: The marker-based creation failure path reuses the same `PRCreationFailed` in-memory flag and R5 retry logic as the legacy `ensureDraftPR` path.
