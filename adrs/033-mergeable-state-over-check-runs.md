# ADR 033: Trust `mergeable_state` Over Raw Check-Run Classification

**Status**: Accepted  
**Date**: 2026-04-27

## Context

Fabrik's CI gate (ADR 027) and conjunctive completion label design (ADR 032) both rely on classifying raw check_runs from the GitHub Checks API to determine whether a PR is safe to merge or advance. Each check_run is categorized as NEW REGRESSION, pre-existing failure, pending, or green; the gate blocks until all NEW REGRESSION checks either pass or are resolved by a CI-fix re-invocation.

This approach has a fundamental flaw: Fabrik's per-check classification does not know which checks are *required* by branch protection. A non-required check_run (e.g., a `Cleanup artifacts` workflow job, a notification step, or an informational check) can fail without affecting branch-protection-level mergeability — GitHub itself will allow the merge — but Fabrik's gate treats it as a blocking NEW REGRESSION and prevents auto-merge.

This was observed in production: liminis issues #716 and #717 were blocked by a failing `Cleanup artifacts` check_run for hours even though all required checks were green and GitHub's own merge button was enabled. `mergeable_state` was `CLEAN` throughout. The only way to resolve the block without Fabrik support was to manually remove `fabrik:awaiting-ci` and trigger the merge.

### The `mergeable_state` Field

GitHub's `mergeable_state` field on a PR aggregates all branch protection signals into a single value:

| Value | Meaning |
|-------|---------|
| `clean` | All branch-protection requirements satisfied; PR is ready to merge |
| `unstable` | Non-required checks have failed but all branch-protection-required checks are satisfied; GitHub allows merge |
| `blocked` | A branch protection rule is blocking the merge (required check failed, approval missing, etc.) |
| `behind` | Branch is behind the base; a rebase or merge is required |
| `dirty` | Merge conflict |
| `unknown` | GitHub hasn't computed the value yet |
| `has_hooks` | Repository has pre-receive hooks that affect mergeability |
| `draft` | PR is in draft state |

When `mergeable_state` is `clean` or `unstable`, GitHub's own branch protection has determined the PR is mergeable. Fabrik's raw check_run analysis cannot be more accurate than this signal — branch protection is the authoritative source, and any check_run failures it ignores are by definition non-blocking.

## Decision

Consult `mergeable_state` before per-check classification in both prongs of the CI gate.

### Prong 1 — Merge Guard (`attemptMergeOnValidate`)

Before fetching and classifying check_runs, `attemptMergeOnValidate` now calls `FetchPRMergeableState`. If the result is `clean` or `unstable`:

- The gate clears immediately.
- `fabrik:awaiting-ci` is removed if present (idempotent).
- Execution proceeds directly to `MergePR`.
- Per-check classification (`FetchCheckRuns` + NEW REGRESSION analysis) is **skipped entirely**.

For all other `mergeable_state` values (`blocked`, `behind`, `dirty`, `unknown`, `has_hooks`, `draft`), the gate falls through to the original per-check classification path.

### Prong 2 — Catch-up Loop (`checkCIGate`)

Before per-check classification, `checkCIGate` now calls `FetchPRMergeableState`. If the result is `clean` or `unstable`:

- `addCompleteLabelAndRemoveCI` runs immediately (clears `fabrik:awaiting-ci`, adds `stage:<X>:complete`).
- Stale `fabrik:awaiting-ci` labels are explicitly cleared by this function.
- Per-check classification is **skipped entirely**.
- The gate returns `(false, false, false)` — gate clear.

For all other values, the gate falls through to the original per-check classification path. The existing rules (pending → skip, failure → CI-fix re-invoke, timeout → pause) apply unchanged.

## Rationale

**GitHub's branch protection is the authoritative source of truth for PR mergeability.** `mergeable_state` already aggregates:

- Required check status (only *required* checks block `clean`)
- Reviewer approval requirements
- Branch protection rules (up-to-date branch, signed commits, etc.)
- Status check configuration

Replicating this logic in Fabrik via raw check_run analysis is redundant at best and error-prone in practice. Non-required check_runs appear identical to required ones in the Checks API response — Fabrik has no reliable way to distinguish them without querying the branch protection rules themselves (an additional API call that would require repository admin scope). Trusting `mergeable_state` avoids this complexity entirely.

The `unstable` state is intentionally included in the shortcut: GitHub defines `unstable` as "non-required checks have failed, but all branch-protection-required checks are satisfied and GitHub allows the merge." Blocking on `unstable` would replicate the same over-aggressive gate that caused liminis#716/717.

## Alternatives Considered

### Configurable check-name ignore list

Users could specify a list of check names (e.g., `ignore_checks: ["Cleanup artifacts"]`) in stage YAML. Fabrik would skip these checks during classification.

**Rejected**: This is a whack-a-mole solution. Non-required check names vary by repository and change as CI pipelines evolve. Users should not need to maintain an ignore list to match GitHub's own branch protection configuration. The ignore list would always lag behind the actual required-vs-non-required division.

### Query only `required` checks via the Checks API

GitHub's Checks API can filter by `app_id` or other parameters, but does not expose a `required` flag directly. The Branch Protection API (`GET /repos/{owner}/{repo}/branches/{branch}/protection`) returns the list of required status check contexts — Fabrik could fetch this list and intersect it with the check_run results.

**Rejected**: This requires an additional API call per CI gate evaluation, demands `repo` or `admin:repo` scope, and the required-context list is keyed by context name (a string), not by check_run ID — making matching brittle when check names are templated or dynamic. `mergeable_state` already does this intersection correctly inside GitHub's own infrastructure, with no additional scope or round-trips.

### No change — document the workaround

Accept the over-aggressive gate and document that users should manually remove `fabrik:awaiting-ci` when non-required checks fail.

**Rejected**: This places an operational burden on users for a class of failure that GitHub itself does not treat as blocking. The fix is clean, the correctness argument is strong, and the operator experience is materially better.

## Predecessor Context

- **ADR 027** (`027-ci-gate-and-fix-reinvoke.md`): Defines the two-prong CI gate structure that this ADR extends. ADR 033's shortcut applies *before* the per-check classification described in ADR 027 — it is additive, not a replacement.
- **ADR 032** (`032-ci-gate-conjunctive-completion-label.md`): Defines `fabrik:awaiting-ci` semantics and `addCompleteLabelAndRemoveCI` as the gate-clearing function. The Prong 2 shortcut path calls the same function — fully compatible with ADR 032's conjunctive label design.
