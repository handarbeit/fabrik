# ADR 1773: Merge-Train Base Sanity Check at Landing Time

**Date**: 2026-09-18
**Status**: Accepted
**Issue**: #1773 — merge-train: refuse to open an integration PR whose base contradicts its
members' declared base

## Context

In #1688, the merge train opened an integration PR against a protected `main` while every member
of the batch carried `base:develop`, and it self-merged nine minutes later with no human in the
loop — a production incident on v0.0.81. #1772 fixes the specific cause: an unhydrated member
silently partitioning to the default base at grouping time.

This is the second time this class of defect has reached that installation. #1637/#1646 was the
first, fixed in v0.0.81 by ADR-1647 (interim exclusion) and ADR-1648 (per-base partitioning) — both
addressed *causes* of a member ending up in the wrong partition. #1688 is a different cause reaching
the same outcome, and a cause-specific fix has already been shipped once and proven incomplete. This
issue deliberately does not add a third cause-specific fix; it adds a check on the *outcome* the
train is about to act on, immediately before it acts — so any future cause of the same disagreement,
thought of or not, is caught here too.

## Decision

### 1. Check the outcome, at both landing-time `CreatePR` sites, not the one the issue names

Research found a second `CreatePR` call site beyond the one the issue's Prior Art section
identifies (`landMergeTrainBatch`): `landSingleton`, the FR-5 one-at-a-time fallback taken whenever
bisection can't land a batch together — not a rare corner case. Both call sites pin their target
base from the same `trialParams.baseBranch`, resolved once at batch/worker formation and threaded
unchanged through assembly, CI polling, and possibly inline conflict resolution — all of which can
take a long time, and none of which re-validates that pinned value against what the batch's members
currently declare.

A single shared helper, `refuseIfBaseContradictsMembers(owner, repo, baseBranch, trainKey string,
members []trainMember) bool`, is called from both sites:

- In `landMergeTrainBatch`, immediately after the closed-unmerged-trial escalation and before the
  `if integrationPR != nil { reuse } else { create }` split — covering both the PR-open and the
  PR-reuse branch uniformly, per the issue's own "opens (or reuses)" wording. A reused PR is just as
  capable of carrying a stale/wrong base as a freshly-created one.
- In `landSingleton`, immediately before its own `CreatePR` call.

The singleton *fast path* (`trySingletonFastPath`/`singletonFastPathEligible`) is confirmed out of
scope and left untouched: it merges the member's own existing PR via `MergePRAtHeadSHA`, which
targets whatever base that PR was already opened against — `p.baseBranch` is never used as a PR
target on that path, so it cannot reproduce this contradiction by construction.

### 2. Trigger condition derived from `trainKey`, not a fresh `wm.DefaultBaseBranch()` call

R1 only requires the check to fire when the pinned base *is* the repository default. Research's
draft suggested calling `wm.DefaultBaseBranch()` a second time inside the check; this is rejected in
favor of `isDefaultPartitionKey(owner, repo, trainKey string) bool`, which is simply `trainKey ==
owner+"/"+repo`.

This is not an approximation of R1's condition — it is exactly it. `mergeTrainKey(repoKey,
partitionBase)` (ADR-1648) returns bare `repoKey`, with no delimiter, if and only if `partitionBase
== defaultPartitionBase` (`""`), which is exactly the case where `prepareTrainWorker` resolved
`baseBranch` *through* `wm.DefaultBaseBranch()` rather than through a `base:` label. Reusing that
already-established fact:

- needs no new git round-trip on every landing attempt (the check is otherwise pure/near-pure);
- introduces no new failure mode of its own requiring a fallback policy;
- is trivially unit-testable — most of this file's existing tests build `wm` over a bare
  `t.TempDir()` with no real `.git`, so a live `DefaultBaseBranch()` call would error in nearly
  every existing landing test.

### 3. Live label read is exclusively `e.client.FetchLabels`

The single most important implementation constraint, and the one most easily gotten wrong by
following the issue text literally: the issue's own Prior Art section cites
`GitHubClient.FetchLabels` generically, but in production `e.readClient` is a
`boardcache.CacheImpl`, whose `FetchLabels` serves the **cached** snapshot whenever the cache isn't
paused, falling back to GitHub only on a cache miss. Calling `e.readClient.FetchLabels` here would
compile, would pass a shallow reading of the issue text, and would silently defeat R2 — reproducing
the exact #1688 failure shape this check exists to catch. The check calls `e.client.FetchLabels`
(always live, no caching layer) and never reads `trainMember.item.Labels` (the cached snapshot
already in hand from batch formation). `merge_train.go` already uses `e.client.*` exclusively
throughout the file, so this is also the path of least resistance, not just the correct one.

### 4. Parse-only comparison, not `baseBranchForItem`'s resolution semantics

A new pure helper, `nonDefaultBaseLabelValue(labels []string, pinnedBase string) string`, scans raw
`[]string` labels for a `base:<branch>` value that is non-empty and differs from `pinnedBase`. It
deliberately does **not** reuse `baseBranchForItem` (`engine/item.go`), which additionally validates
remote branch existence and falls back to the default with a warning comment on failure — exactly
the "repair" behavior R3 forbids here. The two helpers operate on different data shapes for a
reason: `baseBranchForItem` resolves a `gh.ProjectItem`'s *effective* base for worktree/PR-targeting
purposes; this check only ever needs "does this member's live label disagree with what's already
pinned," and must never fall back to anything on its own.

### 5. Fail-closed on a label-read error, no repair, no escalation machinery

Any contradiction found, or any per-member `FetchLabels` error, causes `refuseIfBaseContradictsMembers`
to return `true`: no PR is opened or reused, the caller returns immediately, and the batch's members
stay in Queued. This matches R3's "consistent with #1772's fail-closed posture" and the issue's own
accepted risk framing: a spurious refusal is safe (Queued members retry next poll), the alternative
failure mode is not.

R4 phrases a `fabrik:paused`-style escalation for repeated refusals as "Consider," and Acceptance
criteria #1–#8 are fully satisfiable with a loud per-poll log line (via `e.logfRepo`) naming the
pinned base and every contradicting member alone. Building a durable repeat-counter and a new label
now would be speculative state for a scope the acceptance criteria doesn't require. Left as a
natural follow-up if operational experience shows repeated refusals need surfacing beyond logs.

## Consequences

- Both merge-train landing paths that can mint a new integration/landing PR are now guarded by the
  same check; the singleton fast path is confirmed structurally exempt.
- The check is a no-op — no network call at all — for any partition other than the default one,
  preserving R5 for the overwhelming common case (a repo with only default-base Queued members) and
  for every existing `base:<branch>`-partitioned test in this file.
- This is deliberately independent of #1772: no shared code, no ordering dependency either way. If
  #1772 also lands, a member it would have excluded at partition time never reaches this check at
  all; this check still fires for any *other* cause of the same pinned-base-vs-declared-base
  disagreement, including causes not yet discovered.
- No new label is introduced. An operator distinguishes a refusal from ordinary merge-train activity
  by the `merge-train` log tag alone (`e.logfRepo`), not by board state.

## Rejected Alternatives

- **Re-deriving "is this the default base" via a fresh `wm.DefaultBaseBranch()` call inside the
  check** — see Decision §2. Rejected: adds an avoidable git round-trip on every landing attempt,
  and breaks nearly every existing test in this file that constructs `wm` without a real `.git`.
- **Reusing `baseBranchForItem` directly** for the live-label read — see Decision §4. Rejected: its
  remote-existence resolution and fallback-to-default behavior would silently repair the exact
  condition R3 says must only be refused, never repaired.
- **Guarding only `landMergeTrainBatch`**, matching the issue's Prior Art section literally.
  Rejected: `landSingleton` shares the identical `CreatePR(..., p.baseBranch, ...)` exposure and is
  a commonly-taken fallback path, not a rare edge case — leaving it unguarded would make the
  "second line of defence" the issue argues for absent on that path.
- **Durable repeat-counter + `fabrik:paused` escalation for repeated refusals**, per R4's "Consider."
  Rejected for this issue's scope — see Decision §5 — as a deliberate, documented scope call rather
  than an oversight.
