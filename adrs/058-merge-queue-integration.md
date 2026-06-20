# ADR 058: GitHub Merge Queue Integration

**Date**: 2026-06-20
**Status**: Proposed (spike complete; implementation to be chained off this ADR)
**Issues**: #924 (problem statement); implementation chain TBD
**Builds on**: ADR-056 (consolidated convergence/gate/recovery — single settle-owner), ADR-050 (convergence budget + native auto-merge), ADR-033 (mergeable-state over check-runs)
**Spike**: schema introspection + GitHub-docs behavioral research, 2026-06-20

## Context

Fabrik lands PRs **serially**: it merges one ready PR, and `merge-gate` then rebases each remaining
open PR onto the advanced base. On any repo with `required_status_checks.strict: true` (require
branches up-to-date before merge), a batch of N concurrently-ready PRs produces an **O(N²) rebase +
retest cascade** — each merge invalidates every other ready PR's "up-to-date + green" status,
forcing a rebase (new HEAD SHA) and a full required-check re-run before the next merge. Observed on
`shadoworg/fantasy` 2026-06-20: 4 concurrent `fabrik:cruise` PRs at Validate, ~N−1 extra validate
rounds throttled by a low-slot test queue (#924).

This is not project-specific — it bites any Fabrik-driven repo with concurrent PRs and strict
protection. Fabrik already owns merge orchestration (`merge-gate`, `rebase-reinvoke`, `ci-gate`,
`validate-sha`, `checkAutoMergeConvergence`), so batching belongs here.

### Why a merge queue is the right tool (and why not non-strict)

The user's gate philosophy: **Validate is the authority** — reviews addressed, PR approved, CI green
— and the merge happens *after* that gate. The implicit invariant is that Validate's verdict is
valid **against the main the PR actually merges into**. `strict: true` is what enforces that
invariant; its cost is the O(N²) cascade. Dropping to non-strict would *break* the invariant — a PR
could merge on a verdict computed against an older main, admitting semantic-conflict breakage that no
individual Validate caught. So non-strict is rejected.

A merge queue is **strict's guarantee made cheap**: GitHub batches the ready set onto a temporary
`gh-readonly-queue/{base}` branch, runs the required checks on the *combined* result **once**, and
merges atomically. The "tested against the real main it lands on" guarantee is preserved; the cost
drops from O(N²) to ~O(1) per batch. This is aligned with, not a weakening of, the gate philosophy.

### Spike findings (authoritative)

**Schema (GraphQL introspection):**
- Auto-detect signal: `pullRequest.isMergeQueueEnabled` (per-PR boolean; **GraphQL-only** — the REST
  branch-protection endpoint does not expose it). DRY: GitHub is the source of truth.
- Hands-off signal: `pullRequest.isInMergeQueue` (boolean).
- Entry: `pullRequest.mergeQueueEntry { state position enqueuer { login } }`; `state ∈ {QUEUED,
  AWAITING_CHECKS, MERGEABLE, UNMERGEABLE, LOCKED}`. `enqueuer.login` distinguishes Fabrik-enqueued
  (yolo) from human-enqueued (cruise/manual) entries.
- Mutations: `enqueuePullRequest(pullRequestId, expectedHeadOid, jump)` (optimistic-concurrency via
  `expectedHeadOid`) and `dequeuePullRequest`.
- Config (read-only): `MergeQueueConfiguration { mergingStrategy minimumEntriesToMerge
  maximumEntriesToMerge checkResponseTimeout mergeMethod ... }`.

**Behavior (GitHub docs):**
1. **Direct merge is rejected** on a queue-required branch — REST `PUT .../merge` returns HTTP 405
   *"Changes must be made through the merge queue."* So Fabrik *must* adapt; conforming is "keep
   working," not a surprising new behavior. (Whether any admin-bypass param rescues the plain merge
   call is undocumented — live-test item.)
2. **Enabling auto-merge IS the enqueue path** — `enablePullRequestAutoMerge` (which Fabrik already
   calls) enqueues when green, or "merge-when-ready" until green. The existing call largely Just
   Works on a queue repo; the yolo enqueue change is close to zero new code. `enqueuePullRequest` is
   the explicit alternative when the PR is already green.
3. **`merge_group` is a hard CI prerequisite** — repo workflows must trigger on `merge_group` or the
   required check never reports against the queue branch and **the queue stalls** (not treated as
   passed). Required contexts are coupled to branch protection (same checks; must also fire on
   `merge_group`). This is the one per-repo operator step Fabrik cannot perform.
4. **GitHub does the speculative bisection** — a failing group member is auto-ejected and the group
   recomputed for survivors. Fabrik never reimplements "who poisoned the batch"; it only reacts to
   *its own* PR being ejected.
5. **`pull_request.dequeued` fires for EVERY removal — including successful merge.** A `reason`
   string field disambiguates (confirmed to exist in go-github), but **its enum values are
   undocumented** — the single empirical gap (live-test item). Fabrik must never treat "dequeued" as
   "failed"; it reads `reason` and/or cross-checks merged-vs-open.
6. **"Require merge queue" is a single per-branch-protection-rule boolean** — all-or-nothing for the
   targeted branch (cannot use wildcard patterns). Clean auto-detect.

## Decision

Integrate GitHub's **native** merge queue (GitHub is Fabrik's only host) as the batching engine, and
compose Fabrik's existing `rebase-reinvoke` (Claude conflict resolution) onto the ejection path —
which is Fabrik's differentiator over a raw queue (GitHub *ejects* on conflict; Fabrik *resolves*).

### D1 — Auto-detect, opt-out, no surprise
Detect per-PR via `isMergeQueueEnabled` (added to the GraphQL PR fetch). When false → today's serial
behavior, byte-for-byte unchanged (the backward-compat guarantee). Config knob `merge_queue: auto`
(default) | `off` (kill-switch; on a queue-required repo `off` means yolo merges fail, so it is only
a rollout safety valve). No config needed in the common case — GitHub is the source of truth.

### D2 — The merge *action* enqueues; only on the yolo path
In `engine/stages.go:attemptMergeOnValidate` (the yolo merge action), when `isMergeQueueEnabled`,
enqueue instead of direct-merge. **Cruise already returns early here**, so cruise never enqueues —
its contract (stop at Validate, human merges) is preserved. Manual/no-auto-advance likewise never
enqueues; the operator's merge flows through GitHub's queue transparently.

### D3 — Queue-awareness applies to ALL paths (this is how "don't break cruise" is honored)
"yolo-only" governs the merge *action*; **queue-awareness governs every path**. On a queue-enabled
repo:
- **Never rebase or mutate a PR while `isInMergeQueue`** — the queue owns it; pushing to a queued PR
  ejects it (expected; live-test item). Guard every rebase site on `isInMergeQueue`.
- **Stop preemptive cruise/manual rebasing** — the queue enforces up-to-date at merge time, so
  Fabrik's preemptive rebase is redundant *and* harmful (fights the queue). This also eliminates the
  cruise rebase-thrash for free — the "don't-thrash-rebases" win falls out here.
- Not making cruise queue-aware is exactly what *would* break it (Fabrik rebase-fighting the queue),
  so D3 touches cruise strictly in the correct direction: less thrash, same ownership.

### D4 — Settle composition (the ADR-056 care-area)
Extend the **single** settle-owner / convergence path — do **not** add a parallel scanner (ADR-056's
governing rule: "extend the owner, not add loop N+1").

**Hard constraint: classification MUST be poll-native and webhook-independent.** Fabrik's boardcache
is correct via poll + reconcile alone; webhook deltas (`boardcache/delta.go`) are a pure latency
optimization layered on top (cache refactor v0.0.54). The merge-queue classifier obeys the same rule
— it derives **correctness entirely from poll-observable state**, never from a webhook payload. This
is load-bearing because, once a PR is dequeued, `pullRequest.mergeQueueEntry` becomes `null` and the
`dequeued.reason` exists *only* in the webhook event — so a design keyed on `reason` would silently
break in poll-only deployments. We do not key on it.

- `settlePRMergeState` gains a `PRMergeQueued` transient status (entry `QUEUED`/`AWAITING_CHECKS`, or
  `isInMergeQueue == true` → hands off, re-evaluate next poll). The PR query gains
  `isInMergeQueue`/`mergeQueueEntry{state}`; reconcile populates them from polling like every other
  field — no webhook required.
- `checkAutoMergeConvergence` (already the yolo convergence monitor) gains a queue-aware branch.
  On a poll where the item left the queue (`isInMergeQueue` was true, now false), classify from
  **poll-observable state only**:
  - **PR merged** → **Done** (existing path). *Guard: leaving the queue also happens on success — the
    monitor MUST check merged-state first and never treat dequeue as failure. This is the #913-class
    state-misread trap and gets the heaviest test coverage.*
  - **PR open + conflicting** (`mergeable_state == "dirty"` → `PRMergeConflicting`) → **rebase-reinvoke**
    (Claude resolves) → re-enqueue.
  - **PR open + not conflicting** → **re-run Fabrik's own Validate against current `origin/<base>`**
    (rebase + gate). This is the poll-native source of truth: a merge-group ejection means "this PR is
    not good against the main it would land on" (a semantic break its head-SHA checks missed), which
    Fabrik's own Validate re-derives without GitHub's reason. Validate fails → existing
    `ci-fix-reinvoke`/pause; Validate passes → re-enqueue.
  - A re-enqueue cap (reuse the `MaxCiFixCycles`-style bound) prevents loops; exhaustion → pause for
    human. Re-enqueue fresh after off-queue resolution (not re-enqueue-in-place) so conflict-heavy
    PRs don't starve.
- **Webhook `dequeued.reason` is a pure optimization, never a dependency.** When webhooks are present,
  a new `boardcache/delta.go` `pull_request`/`dequeued` function persists `reason` to `e.store`, which
  lets the monitor *skip the redundant re-Validate* (go straight to `ci-fix` on a CI-failure reason).
  Absent webhooks, the re-Validate path above is fully correct on its own — mirroring the existing
  delta-optimizes-poll split exactly.

### D5 — Detect-and-warn on the `merge_group` prerequisite
If `isMergeQueueEnabled` but merge-group checks never report (queue stalls), Fabrik surfaces a clear
operator error ("enable `on: merge_group` in CI") rather than hanging silently. This makes the one
unavoidable per-repo prerequisite a loud, actionable failure.

## The one empirical unknown (now off the correctness path)

The `dequeued.reason` enum string values are undocumented — but per D4 they are needed **only for the
webhook optimization** (skipping a redundant re-Validate), not for correctness. Poll-native
classification (`PRMergeConflicting` for conflict; Fabrik's own re-Validate against current base for
everything else) is fully correct without them. So this unknown no longer blocks or gates the design.

A live test still **confirms** the poll-side assumptions the classifier relies on — chiefly that
`mergeQueueEntry` goes `null` (not a lingering `UNMERGEABLE` state) on dequeue, and that pushing to a
queued PR ejects it — plus captures the `reason` values for the optimization. The first
implementation step (operator-led, **not** a boarded code issue) owns the live env: stand up a
queue-enabled repo, run success / CI-fail / conflict / push-while-queued scenarios, log the raw
payloads + post-ejection poll signals. *Spike note: standing up the live env hit a GitHub
rulesets-API quirk (the `merge_queue` rule returned an opaque 422 despite the org's Team plan
supporting private merge queue); not pursued further in-spike since the architecture is determined.*

## Consequences

**Positive:**
- O(N²) → ~O(1)-per-batch, preserving the strict "tested against real main" guarantee.
- Reuses Fabrik's existing reinvoke machinery; GitHub owns the hard parts (batching, bisection,
  atomic merge).
- Eliminates cruise rebase-thrash as a side effect (D3).
- Backward compatible (D1): zero change absent a queue.

**Negative / risk:**
- Lands in the freshly-consolidated convergence subsystem (ADR-056) — the exact code where #913/#915
  bugs lived. D4 must be specified and tested for the `dequeued`-on-success trap and every
  `{reason} × {merged/open}` combination, or it reopens the whack-a-mole.
- One unavoidable per-repo operator prerequisite (`on: merge_group` in CI) Fabrik cannot perform —
  mitigated by D5 detect-and-warn.
- Fabrik loses some visibility into queue internals (speculative groups are GitHub's).

## Non-goals
- **Host-agnostic merge train** (the issue's "flavor 2"): deferred. GitHub-only is acceptable
  (Fabrik's only host); a self-managed train would have to reimplement speculative bisection.
- **Batching the cruise/manual *merge*** — out of scope; those merges are human-owned by definition.
  D3 only removes Fabrik's rebase-thrash for them.
- No change to cruise/yolo semantics, the ADR-008 pause model, or the convergence budget (ADR-050).

## Proposed implementation plan

**Step 0 — Live-env + ejection telemetry (operator-led, NOT a boarded code issue).** Stand up a
queue-enabled repo, run success / CI-fail / conflict / push-while-queued scenarios, confirm the
poll-side assumptions (`mergeQueueEntry` → null on dequeue; push ejects) and capture the
`dequeued.reason` values for the optimization. Feeds Step 5; does not gate correctness (D4 is
poll-native).

**Boarded spec-kit issues (blockedBy-chained off this ADR):**

1. **PR-fetch + client surface** — add `isMergeQueueEnabled`/`isInMergeQueue`/`mergeQueueEntry` to
   the GraphQL PR query and `PRDetails`; add `EnqueuePullRequest`/`DequeuePullRequest`. *(default model)*
2. **Enqueue on yolo** — `attemptMergeOnValidate` queue branch; `merge_queue: auto|off` config; D1.
   *(default model)*
3. **Queue-aware all paths** — `isInMergeQueue` guards on every rebase/mutation site; stop preemptive
   cruise rebase on queue repos (D3). *(`model:opus` — cross-cutting "find every site")*
4. **Settle composition** — `PRMergeQueued` in `settlePRMergeState`; `checkAutoMergeConvergence`
   poll-native queue branch (D4 classifier); the leaving-the-queue-on-success guard; optional
   `delta.go` `dequeued` enrichment; unit tests across `{poll-state} × {merged/open/conflict}`. (D4 —
   the care-area.) *(`model:opus` + `effort:max`)*
5. **Detect-and-warn** on missing `merge_group` checks (D5); `docs/state-machine.md` update.
   *(default model)*
