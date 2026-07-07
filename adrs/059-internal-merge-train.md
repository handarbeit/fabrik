# ADR 059: Fabrik-Internal Merge Train (the `Queued` board status)

**Date**: 2026-06-23
**Status**: Proposed (landing mechanism de-risked by spike; implementation to be chained off this ADR)
**Issues**: #924 (the O(N²) problem); implementation chain TBD
**Builds on**: ADR-058 (GitHub merge queue integration — the queue-aware surface and poll-native settle composition are reused), ADR-056 (single settle-owner)
**Supersedes (in part)**: ADR-058's "GitHub-only is acceptable" non-goal — see Context

## Context

ADR-058 integrated GitHub's **native** merge queue to kill the O(N²) rebase+retest cascade that
`strict` branch protection produces on concurrent ready PRs (#924). It shipped correct, poll-native,
and backward-compatible — but on activation we hit a wall: **GitHub's merge queue is available only on
Enterprise Cloud (for private repos) or on org-owned public repos.** It is *not* available on the
plans Fabrik's actual repos run under:

- `handarbeit/fantasy` — **private, GitHub Team** → no merge queue.
- `arbeithand/example-repo` (and the Liminis fleet) — owned by a **personal account** → not
  eligible even when public.

So the ADR-058 work, while correct, **cannot be activated on the repos that experience the problem.**
ADR-058's deferral of "flavor 2 — the host-agnostic merge train" as a non-goal (on the assumption
"GitHub-only is acceptable") was the mistake; that assumption does not hold for this fleet.

The N² thrash is **recurring and painful here**, not hypothetical: many issues are worked in parallel,
main moves quickly (wide collision window), the test suite is long, and the test runner has been
**manually serialized to limit compute contention** — which is the worst case for N², because the N−1
redundant retests after each merge queue up behind each other on a single slot. Dropping `strict` is
the wrong trade: a fast-moving main makes semantic conflicts *more* likely, so the "validated against
the main you actually land on" guarantee matters *more*, not less.

A **Fabrik-internal merge train** is **plan- and host-agnostic** — it works on private Team and
personal-account repos where GitHub's queue cannot — and it preserves strict's guarantee cheaply by
testing the *combined batch* against the final main state once, instead of O(N²) times.

### De-risk spike (2026-06-23) — the landing mechanism is proven

The single highest-risk question (the analogue of the merge-queue availability miss) was: *can a batch
land on a `strict`-protected private Team `main` without admin bypass?* A spike on
`handarbeit/fabrik-mergetrain-spike` (private, Team, `strict` + required check) proved it end-to-end:

1. Three PRs — including a **textual conflict** and a **semantic-consistency requirement** (a CI gate
   where a `MARK`-line count must equal a `COUNT.txt` value) — were built onto a trial branch.
2. The conflict was **resolved** (kept both marks; set `COUNT=2`, the semantic fix git does not flag).
3. **One** gate run validated the *combined* result (and would have failed on an incorrect resolution).
4. The batch **landed via a single integration PR through normal branch protection — `--merge`, no
   `--admin`.** `main` received all three changes atomically.

One finding: member-PR closure via `Closes #N` is **inconsistent** (one member came out
`closed`/`merged=false`, two `merged=true`). Fabrik already owns the issue lifecycle (it advances
issues to Done directly), so the train will **close member PRs explicitly** with a batch reference
rather than rely on the keyword. Not a blocker.

## Decision

Build a Fabrik-internal merge train staged on a new **`Queued` board status**. The `Queued` column is
the durable, observable queue; its engine handler drains it in batches via the spike-proven
integration-PR landing.

### D1 — The `Queued` board status is the queue
Add a `Queued` board column between `Validate` and `Done`. On a quality-pass (`Validate` complete —
reviews/CI/approval all satisfied), the issue advances `Validate → Queued`. This reuses Fabrik's core
principle — **board status is the primary, durable state** — so the queue is:

- **Restart-safe**: the queue *is* the column; no in-memory ready-set to lose.
- **Observable**: operators watch the batch form on the board, pull an item out (move it back), or see
  exactly what is about to land.
- **A declarative trigger**: the train's input set is *"every item in `Queued`"* — no separate state.

`Queued` is a **non-Claude holding stage** (no agent invocation), analogous to the engine-managed
cleanup/Done stage; the merge train is its handler. The startup board-validation requires a matching
`Queued` column (one-time operator setup).

### D2 — Snapshot-and-batch (solves continuous arrival)
Each poll, the train **snapshots** the current `Queued` set and processes one batch (up to a tunable
`max_batch_size`, ordered by entry). Items that become ready mid-train simply **wait in `Queued` for
the next train.** The column *is* the arrival buffer — no separate arrival state machine.

### D3 — The landing mechanism (spike-proven)
Per batch:
1. Build a **trial integration branch** off current `origin/<base>`.
2. Merge each member PR's head onto it in order; on conflict, dispatch **Claude** to resolve
   (reusing the ADR-058 rebase-reinvoke machinery — semantic resolution, the thing a raw queue can't do).
3. Run **one** combined Validate on the trial branch (poll the required checks, exactly like the
   existing CI gate).
4. On green: open **one integration PR** (trial → base), land it through **normal branch protection**
   (`--merge`, no `--admin`), then **advance each member `Queued → Done`** and **close each member PR
   explicitly** with a "landed via batch #N" reference (D1 of the spike findings).

### D4 — Bisection on a red batch (tuned to long / serialized tests)
A batch is **usually green** — every member already passed Validate individually, so a batch fails only
on a genuine *cross-PR* interaction. So bisection is the exception, which matters because long +
serialized tests make each bisection round expensive. Policy:
- On red: **halving bisection** to find the poisoner (O(log N) combined runs), **not** per-PR retest
  (which would re-introduce O(N)).
- The ejected poisoner returns to **`Queued`** (or is paused for human attention after repeated
  ejection); the batch re-forms with the survivors and retries.
- `max_batch_size` is the operator's lever: smaller batches → cheaper worst-case bisection, less N²
  savings. Tune to test cost.

### D5 — Main moved during validation → serialize the train
`strict` requires the integration PR to be up-to-date; if `main` advances while the trial branch
validates, the integration PR goes `behind`. Because **the train is the only thing Fabrik lands on
`main`**, the clean answer is to **serialize the train — one batch in flight per repo at a time** — so
`main` does not move under a train. The only exception is an *external* direct push to `main` (a human
pushing outside Fabrik): detect (`behind`), **rebase the trial branch onto new main and re-validate**.

### D6 — Compose with ADR-058 (one column, two landing engines)
The `Queued` column is **universal**; its handler picks the landing engine by auto-detection:
- Repo has a GitHub native merge queue (`isMergeQueueEnabled`, ADR-058) → **enqueue to GitHub's
  queue**, let GitHub batch (gets GitHub's speculative parallel execution for free).
- Otherwise → **run the internal merge train** (this ADR).

Both drain `Queued`; the difference is only *who batches*. This unifies 058 and 059 under one board
model rather than maintaining two parallel merge paths. (For the target fleet — private Team,
personal — the internal-train branch is always taken.)

### D7 — yolo / cruise / manual
- **yolo** items: advance to `Queued` at Validate-complete and ride the next train automatically.
- **cruise / manual** items: advance to `Queued` but **wait for an explicit human "go"** before riding
  (a label or sub-state), preserving cruise's "human decides to merge" contract — now batched.
  (v1 may scope the train to yolo `Queued` items and treat cruise batching as a fast-follow.)

### D8 — Runaway guard (composition-agnostic trial rate cap)

**Problem.** On 2026-07-06, a billing-blocked CI on `handarbeit/fabrik-test-alpha` produced 2,730 GitHub Actions workflow runs in a single day (~1,400 trial PRs × 2 required checks). The root cause: GitHub reports a billing-blocked job as plain `conclusion: failure`, indistinguishable from a content-red batch without parsing the per-step annotation text ("recent account payments have failed"). The train therefore treated the infra fault as a persistent poisoner, bisected and ejected every member (up to `MaxMergeTrainEjections` rounds each), and re-formed a fresh batch on every poll cycle — burning ~1 trial family per 60 seconds until the account was exhausted.

**Why not classify failure causes?** Parsing per-check annotations to distinguish "infra failure" from "code failure" would require fetching the check run's step annotations on every red result — extra API traffic, fragile to GitHub annotation format changes, and still not complete (a broken base branch or a test suite permanently broken by a merged commit looks exactly like a content-red to annotation parsing). This path is deferred as a potential future fast-path but is not taken here.

**Decision: composition-agnostic trial rate cap.** Add a cross-poll-cycle rolling-window counter keyed `owner/repo` (parallel to `mergeTrainEjectionCounts`). If a repo creates ≥ `MaxTrainTrialsPerWindow` trial branches with **zero successful landings** within a rolling `TrainTrialWindowDuration` window, pause all Queued members for that repo and stop dispatching until a human clears the labels.

**Design:**
- `recordTrial(repoKey)` is called at the top of `assembleAndValidate` (the single site where all trial branches are created) before the test-seam branch, so every trial — initial batch, bisection sub-trials, and one-at-a-time singletons — counts.
- `resetTrialCounter(repoKey)` is called from `landMergeTrainBatch` and `landSingleton` after a successful merge. A train where survivors do land (normal poison bisection) never accumulates toward the cap.
- Two hooks ensure all Queued members are paused, not just the active batch: **Hook 1** inside `runMergeTrainWorker` (pauses the active batch immediately when the guard fires); **Hook 2** in `routeQueuedGroup` before dispatch (pauses any beyond-cap Queued members on the next poll). The one-poll-cycle gap for beyond-cap members is acceptable because they cannot form a new batch while the worker goroutine is still active.
- `fireRunawayGuard` logs at the `merge-train` tag, applies `fabrik:paused` + `fabrik:awaiting-input` to each member, and posts an alert comment explaining the trial count, window, and remediation steps.

**Defaults:** `MaxTrainTrialsPerWindow = 20`, `TrainTrialWindowDuration = 60 min`.

Derivation: worst-case legitimate bisection (MaxBatchSize=5) resets the counter at or before trial 8 (a bisection that isolates a poisoner, plus one green re-form trial). The one-at-a-time fallback with a cross-PR interaction accumulates at most 7 trials before the first singleton lands and resets. N=20 is well above both: a billing-blocked repo accumulates 20 trials in seconds (CI fails immediately); a healthy repo lands members and resets before reaching 20. The 60-min window covers slow CI (30 min/check × 2 checks = 60 min for one real trial) while billing-blocked CI hits the cap in under a minute.

**Restart semantics:** the counter is in-memory and resets on engine restart. `fabrik:paused` labels persist on GitHub, so the poison-well exclusion (`groupQueuedByRepo`) continues preventing re-formation after a restart. The only gap is a simultaneous restart + manual unpause while infra is still broken — this is acceptable per the spec (R6).

## Consequences

**Positive:**
- O(N²) → ~O(1) test runs per batch on **any** repo (plan/host-agnostic) — exactly the fleet
  (private Team, personal) where ADR-058 cannot run.
- Durable, observable, restart-safe queue by reusing board-status-as-primary-state.
- Preserves strict's "validated against the main you land on" guarantee.
- Unifies the merge path (yolo + human-merge land via the train); composes cleanly with 058.
- Claude conflict-resolution within the batch is a capability a raw merge queue lacks.

**Negative / risk:**
- A **substantial build** — larger than ADR-058. New: the trial-branch/integration-PR lifecycle, the
  bisection engine, the train state machine, explicit member-PR closure.
- Bisection cost on red batches (mitigated: batches are usually green; halving; tunable size).
- Serialized train bounds throughput to one batch-validate at a time per repo — still vastly better
  than O(N²) serial, but a ceiling to be aware of.
- Lands in the convergence/settle subsystem (the #913/#915 territory) — the new merge path must be
  composed into the single owner (ADR-056), not bolted on as a parallel scanner.

## Non-goals
- **Replacing ADR-058 where a native queue exists** — D6 keeps it as the landing engine for
  queue-enabled repos. This ADR adds the universal fallback, it does not delete 058.
- **GitHub-style speculative parallel execution** in v1 — the internal train serializes (D5); parallel
  speculative batches are a later optimization if batch-validate latency becomes the bottleneck.
- No change to the `Validate` quality gate, the ADR-008 pause model, or cruise/yolo semantics beyond
  the merge *timing* moving into the `Queued` stage.

## Proposed implementation chain (spec-kit issues, blockedBy-chained off this ADR)

**Spike (done):** integration-PR landing on strict private Team — proven (see Context).

1. **`Queued` stage + board model** — add the `Queued` non-Claude holding stage; `Validate → Queued`
   advancement on quality-pass; startup board-validation for the column; the engine handler skeleton
   (snapshot the `Queued` set; no batching yet).
2. **Trial-branch builder + Claude conflict resolution** — build the integration branch, merge members,
   dispatch Claude on conflict (reuse rebase-reinvoke); the combined-Validate poll.
3. **Integration-PR landing + member lifecycle** — open/poll/merge the integration PR (no admin);
   advance members `Queued → Done`; **explicit** member-PR closure with batch reference.
4. **Bisection + ejection** (D4) — halving bisection on a red batch; eject poisoner → `Queued`/pause;
   re-form survivors; `max_batch_size` config.
5. **Train serialization + main-moved handling** (D5) — one batch in flight per repo; detect external
   `main` advance → rebase trial + re-validate.
6. **058 composition** (D6) — `Queued` handler auto-detects `isMergeQueueEnabled` → GitHub queue vs
   internal train; `docs/state-machine.md` update for the `Queued` status and the train lifecycle.

The settle/convergence machinery, queue-aware rebase guards, and Claude conflict-resolution dispatch
from ADR-058 are reused throughout.
