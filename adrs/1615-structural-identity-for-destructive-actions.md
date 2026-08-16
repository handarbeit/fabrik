# ADR 1615: Structural Identity for Destructive Merge-Train Actions

**Date**: 2026-08-16
**Status**: Accepted
**Issue**: #1615 — `findIntegrationPR`/`isTrainPR` both identified a merge-train PR by a
body-marker match that any PR body may legitimately contain for unrelated reasons; the
second exploitation of the same defect class closed this fix's own PR mid-review.
**Builds on**: ADR-059 (Fabrik-Internal Merge Train)

## Context

This issue's R1–R6 fix closed the first exploitation of a body-marker-only identity
check: `findIntegrationPR` matched an unrelated, already-merged PR from a different
batch and `landMergeTrainBatch` misattributed a landing to it — reported live in #1614.
The fix required `HeadRefName == trialBranch` as the mandatory, primary signal, with
the marker reduced to non-fatal corroboration.

`isTrainPR` (`engine/merge_train.go`), the sole gate for `reconstructTrainState`'s
restart-recovery sweep, had the identical marker-OR-branch defect and was left
untouched by R1–R6 — the original report was about misattributed *landings*, and this
call site does something different: it decides which stale PRs to *close* during
restart recovery. It was exploited independently, live, during this very PR's own
review: `reconstructTrainState`, running for an unrelated batch (#1592), found this
PR's own description discussing `mergeTrainBatchMarker` in prose (while explaining the
R1 fix), matched `isTrainPR`'s marker arm, found none of *this* PR's referenced issues
in *that* batch's still-Queued snapshot, and closed it — via the exact stale-open-PR
cleanup path that exists to remove abandoned trial PRs. The branch survived only
because the separate branch-cleanup arm (`trialNameFromBranch`) correctly rejected the
non-train head ref — a second check that happened to be more careful than the first.

Both incidents share one root cause: **identity was asserted from prose an artifact's
body may legitimately contain for unrelated reasons** (in both cases, discussing the
marker literal while explaining a bug fix) **rather than from a fact only Fabrik
itself controls.**

## Decision

**Artifact identity is established from structural facts Fabrik itself controls —
in this codebase, the head ref a PR was opened on — never from prose content a body
may independently contain.** An annotation (the batch marker) may corroborate an
already-structurally-confirmed identity, and its absence may be logged as a soft
signal, but it is never itself sufficient to establish identity, and never sufficient
on its own to authorize an action.

`isTrainPR` now gates on `strings.HasPrefix(pr.HeadRefName, trainBranchPrefix)` alone
(R7). Every genuine trial PR — draft CI PR or promoted landing PR — is Fabrik-created
on `fabrik/merge-train/<trialName>`; no other actor produces a PR on that branch
pattern. The marker check is dropped from the identity test entirely, matching R1's
own resolution of the same question at `findIntegrationPR`'s call site.

**A destructive action re-confirms that identity check at its own call site, not only
inherited from an upstream filter.** `reconstructTrainState`'s stale-open-PR cleanup
path re-checks `strings.HasPrefix(pr.HeadRefName, trainBranchPrefix)` immediately
before its `CloseIssue` call (R8) — deliberately redundant with the `isTrainPR` filter
already gating loop entry above it. This is not defensive over-engineering for its own
sake: this issue is itself proof that "fixed at one read site" (R1's
`findIntegrationPR`) does not imply "fixed at every read site" — `isTrainPR` was a
distinct, independently-exploitable instance of the same defect, sitting one call
away. Re-confirming identity at the point of the destructive action means a future
change anywhere upstream (a broadened `isTrainPR`, a new caller reusing it for a less
careful purpose) cannot, by itself, cause an incorrect close — the regression test
(`TestReconstructTrainState_MarkerOnlyNonTrainBranchPR_NeverClosed`) only fails when
*both* checks are reverted together, confirming the two are independently sufficient.

**Ambiguity fails closed.** A PR that does not pass the structural identity check at
the close site is skipped and logged — never closed on the strength of a negative
inference ("no members of this PR are in today's batch") alone. An observable, logged
skip is a recoverable state (the next reconstruct pass tries again, or a human
notices the log line); an incorrect close of a live, unrelated PR is not — this
incident's own branch survived only by luck (a stricter, independent check elsewhere
happened to reject it), not by design. That is precisely the property this decision
stops relying on.

## Rationale

**Why branch, not marker, as primary**: a body marker is just text. Any PR — including
one whose entire purpose is to *discuss* the merge train, as this fix's own PR
description does — can contain that text without being a trial PR. A head ref, by
contrast, is set once at PR creation by whichever actor opened it; Fabrik is the only
actor that ever opens a PR on `fabrik/merge-train/*`, making it a fact about
provenance rather than content.

**Why redundant, not fixed once**: the cost of re-checking a `strings.HasPrefix` call
a second time, immediately before a destructive API call, is negligible. The benefit —
removing an entire class of future regression, where an upstream change to a shared
predicate silently widens what a *destructive* call site accepts — is not. Identity
checks that gate read-only decisions (which PR to search for, which comment to post)
can reasonably live in one shared predicate; identity checks that gate an irreversible
action are worth re-asserting locally, at the cost of a few duplicated lines.

## Consequences

- `isTrainPR` no longer recognizes a PR by body content alone. A caller wanting the
  marker as corroboration (not identity) reads `pr.Body` itself — `reconstructTrainState`
  now does this, logging (non-fatal) when the marker is absent on a
  structurally-identified train PR, which is the expected, routine case for an
  unpromoted draft CI PR.
- `reconstructTrainState`'s stale-open-PR cleanup path (AC7: unchanged in outcome for a
  genuine stale trial PR) gained one redundant `strings.HasPrefix` check immediately
  before its `CloseIssue` call.
- `TestIsTrainPR` was updated: a marker-only, non-train-branch PR now asserts `false`
  (previously asserted `true` — the bug's own behavior, encoded as a passing test
  before this fix).
- `findIntegrationPR` (already fixed by R1) and the branch-cleanup arm
  (`trialNameFromBranch`, already correctly branch-gated — the reason this PR's own
  branch survived the incident) are unchanged.
- Out of scope, per the issue: the underlying reasons a reconstruct pass runs
  concurrently with an unrelated, actively-discussed PR in the first place (an
  ordinary, expected occurrence — `ListPRs` returns every PR in the repo, not just
  merge-train ones) — this decision is about never mistaking that concurrency for
  identity, which holds regardless of how often it occurs.
