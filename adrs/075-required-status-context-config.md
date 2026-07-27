# ADR 075: Required-Status-Context Awareness via Explicit Per-Repo Config

**Date**: 2026-07-26
**Status**: Accepted
**Issue**: #933 — ci-gate: require the real required-status on current head (run it if missing); never treat skipped/disabled GHA checks as a pass

## Context

`settlePRMergeState` (`engine/pr_settle.go`) and `pollTrainCI` (`engine/merge_train.go`) both fall back to a permissive check-run classifier — `gh.ClassifyCheckRuns`, or (before this change) an inline duplicate in `pollTrainCI` — whenever `mergeable_state` isn't `clean`/`unstable` (the ADR-033 shortcut). That classifier has no concept of "required": only `failure`/`timed_out`/`action_required` count as failed, `queued`/`in_progress` as pending, and everything else — `skipped`, `neutral`, entirely absent — reads as `CheckRunsReady`, i.e. "CI passed."

This is silently wrong for repos that produce their required signal out-of-band. The motivating case (shadoworg/fantasy, 2026-06-20): GitHub Actions is disabled and `pnpm test` posts a classic commit status, `fantasy/local-test`, directly via the Statuses API — there is no check run for it at all. When a PR's head advances past the last SHA that was actually tested (Fabrik's own strict-mode rebase, or a plain developer push), the new head carries only stale `skipped` GHA check runs and no `fantasy/local-test` status. `ClassifyCheckRuns` reads the all-skipped set as ready, `settlePRMergeState` returns `PRMergeReady, Reason: "all CI checks passed"`, and the engine records `validate-sha` for a head that was never actually validated — wedging the PR `BLOCKED` on GitHub's side while Fabrik believes it's done and goes idle.

Two prior fixes, #855 and #860, already closed the *zero-check-runs* variant of this same root cause (a `blocked` `mergeable_state` being silently overridden by an empty check-run read). This issue is the same class of bug recurring for the *non-empty-but-all-permissive* variant, plus a second gap: Fabrik has never had any visibility into the classic Statuses API at all (`github/prs.go`'s `FetchCheckRuns` is the only per-SHA CI signal fetched), so a required classic commit status is invisible to Fabrik except indirectly through GitHub's own `mergeable_state` aggregate.

Fixing this requires knowing which context name(s) are actually *required* for merge — something Fabrik has never tracked. [ADR-033](033-mergeable-state-over-check-runs.md) already considered and rejected reading GitHub's branch-protection API for a related purpose (distinguishing required from non-required check runs generally), on the grounds that it needs an additional API call, demands `repo`/`admin:repo` scope, and produces a context-name list that's brittle to match against templated/dynamic check names. This ADR revisits that question specifically for required-context determination and confirms, rather than overturns, ADR-033's objection.

### Why a branch-protection read still doesn't work here

`GET /repos/{owner}/{repo}/branches/{branch}/protection` is documented to return only 200/404, but real-world reports confirm it returns **403 "Resource not accessible by personal access token"** for classic PATs without admin-level repo access — reading branch protection is gated on admin permission to the repo, not the `repo` OAuth scope Fabrik documents as sufficient (`docs/USER_GUIDE.md`). Making the CI gate depend on a read that realistically 403s for Fabrik's documented minimal-scope token model would be a regression in reliability, not a fix — the exact risk ADR-033 already flagged.

### Why the classic Statuses API gap is comparatively cheap to close

`GET /repos/{owner}/{repo}/commits/{ref}/status` (the combined-status endpoint) needs only pull access — the same `repo` scope Fabrik already requires. There is no permission gap analogous to branch protection here; the only reason Fabrik hasn't used it is that nothing previously needed to.

## Decision

Add an explicit, per-repo Fabrik config field — `required_status_contexts`, a `map[string][]string` keyed by `"owner/repo"` — to `.fabrik/config.yaml` (`config.ProjectConfig.RequiredStatusContexts`, plumbed through `engine.Config.RequiredStatusContexts`). Each entry lists the status/check-run context names that must report a confirmed `success` on a PR's *exact* head SHA before the CI gate will clear it. An unconfigured repo (the default — no entry for its `"owner/repo"` key) gets **zero behavior change**: every new code path introduced by this issue is a no-op unless required contexts are configured.

This is layered as a **pre-filter in front of the existing, untouched logic**, applied identically at every call site:

1. The ADR-033 shortcut (`mergeable_state ∈ {clean, unstable}` → immediate ready) is not touched. GitHub itself has already confirmed required checks passed in that case — reopening that decision is explicitly out of scope for #933.
2. Past that shortcut, for a repo with `required_status_contexts` configured, each required name is checked against the union of (check-run names, classic commit-status contexts) observed on the exact head/trial SHA — matching GitHub's own model, where branch protection matches a required context name against either producer interchangeably. Only a confirmed `success` counts; missing, pending, `skipped`, or `neutral` all resolve to `RequiredContextsPending` (not a regression — nothing has failed, it just hasn't reported yet); a check-run `failure`/`timed_out`/`action_required` conclusion or a commit-status `failure`/`error` state resolves to `RequiredContextsFailed`.
3. Classic commit statuses are fetched (`github.Client.FetchCombinedStatus`, added to `GitHubClient`/`boardcache.ReadClient`/`GitHubAdapter`/`CacheImpl` — the last always-delegates, since statuses change without webhooks, mirroring `FetchPRMergeableFields`) only when at least one required context name isn't already resolvable from check runs alone (`engine.classifyRequiredContexts`) — avoiding an unconditional extra API call on every settle pass for repos whose required contexts are all ordinary check runs.
4. If every required context resolves to `RequiredContextsSatisfied` (or none are configured), execution falls through unchanged to the pre-existing `gh.ClassifyCheckRuns`-based logic.

### Call sites

- **`settlePRMergeState`** (`engine/pr_settle.go`): the required-context check runs both in the zero-check-runs branch and after check-run classification would otherwise read `CheckRunsReady`. A confirmed failure returns `PRMergeBlocked` (mirroring a failed check run); a pending/missing result returns `PRMergeUnsettled`. Both `checkMergeabilityGate` and `checkCIGate` consume `settle.Status`, so this one change covers both gates. The zero-check-runs required-context-failure check is placed **ahead of** the pre-existing hadChecks/dwell/R3-`mergeable_state` guards, not after — a confirmed failure is a definitive, timing-independent signal that must not be masked by those transient-window guards (a `blocked` `mergeable_state`, exactly the local-CI-takeover case, would otherwise return generic `PRMergeUnsettled` before the required-context check was ever reached).
- **`checkCIGate`** (`engine/ci.go`): a new `classifyCIFromRequiredContexts` runs ahead of `classifyCIFromCheckRuns`/`classifyCIFromMergeableState`, because a required-context failure sourced solely from a classic commit status has no check-run footprint for those functions' checkRuns-only view to react to. It applies `fabrik:awaiting-ci` and the same `CIWaitTimeout` guard as the check-run failure path.
- **`pollTrainCI`** (`engine/merge_train.go`): its previously-separate inline permissive loop is replaced with a call to the shared `gh.ClassifyCheckRuns` (fixing a pre-existing dedup-by-check-run-ID drift as a side effect), followed by the same required-context check when check-run classification alone would return `CheckRunsReady`. A confirmed failure returns `TrainCIRed`; pending/missing falls through to keep polling until `CIWaitTimeout`.

## Rationale

**Explicit config over a branch-protection read**: as established above, a real permission gap makes a branch-protection read unsuitable as a load-bearing dependency for the gate — the same objection ADR-033 raised. A hybrid (best-effort branch-protection read, falling back to config on error) was considered and rejected as unnecessary complexity: it would still need the explicit-config path to exist for the common 403 case, so it only adds a second, less-reliable data source without removing the need for the first.

**Multi-repo-keyed, unlike every other `ProjectConfig` field**: Fabrik supports multi-repo mode, where a single instance manages many repos discovered from the board, each with potentially different branch-protection setups. A flat field (Fabrik's existing convention for every other config option) would be wrong here by construction.

**Missing/pending is `Pending`, not `Failed`**: nothing has regressed when a required context simply hasn't reported yet — it should not trigger the CI-fix reinvocation path (`fabrik:awaiting-ci` failure escalation), only the ordinary "still waiting" block. Only a confirmed failure conclusion/state is a real signal.

**Gated commit-status fetch, not unconditional**: `settlePRMergeState` runs every poll cycle for every gated item. An unconditional extra API call per poll would multiply rate-limit cost across a fleet of gated issues for no benefit in the common case where required contexts are check runs. Fetching only when a required name isn't already resolvable from check runs keeps the added cost proportional to actual local-CI-takeover usage.

**`pollTrainCI` refactored to share `gh.ClassifyCheckRuns` rather than mirroring a second inline copy**: the existing inline duplicate had already drifted from the shared classifier (no latest-run-by-name dedup), which is itself evidence that a second copy of this logic accumulates bugs independently. Sharing removes that drift risk going forward, at the cost of a slightly larger diff than a minimal mirror would have been.

## Alternatives Considered

### Branch-protection API read (`GET .../branches/{branch}/protection`)

Rejected — as above, real-world 403s for classic PATs without admin-level repo access make this unsuitable as a hard dependency, reopening exactly the objection ADR-033 already raised for a related purpose. Fabrik's documented required scopes (`repo`, `project`, `workflow`) do not reliably cover it.

### Hybrid: best-effort branch-protection read, falling back to explicit config

Rejected as unnecessary complexity — since the config path must exist regardless (to cover the near-certain 403 case for non-admin tokens), a best-effort read on top adds a second data source and a merge-precedence question without removing the need for the first. Deferred; nothing about this ADR's shape prevents adding it later if reliable elevated-scope tokens become common enough to justify it.

### Unconditionally tightening `gh.ClassifyCheckRuns` to reject all skipped/neutral runs

Rejected — `ClassifyCheckRuns` is shared by the correctness-critical `settlePRMergeState` path for *every* repo, including ones with no required-context complexity at all. Path-filtered matrix legs and informational notify steps legitimately conclude `skipped`/`neutral` constantly in the common vanilla-GHA case, and today correctly don't block. Blindly tightening the shared classifier would regress that common case; required-context awareness has to be layered on top as an explicit, opt-in pre-filter instead.

## Consequences

**Positive:**
- Closes the correctness hole described in #933: a PR is never declared CI-green based solely on skipped/neutral/absent check runs when a real required context hasn't confirmed success on the exact head SHA.
- Closes the classic-Statuses-API blind spot — Fabrik can now see a required status posted that way at all, not just infer it indirectly through `mergeable_state`.
- Zero behavior change for the common case: any repo without `required_status_contexts` configured takes exactly the pre-existing code path, byte-for-byte.
- `pollTrainCI`'s pre-existing dedup-by-ID drift bug is fixed as a side effect of sharing `gh.ClassifyCheckRuns`.

**Negative / Trade-offs:**
- Requires manual config (`required_status_contexts`) rather than automatic discovery — an operator must know and declare the required context name(s) for their repo. This is a deliberate trade against the branch-protection-read alternative's permission risk, not an oversight.
- Adds a conditional extra API call (`FetchCombinedStatus`) per settle pass for configured repos whose required contexts aren't fully resolvable from check runs alone — bounded and opt-in, but non-zero.
- Does not address the *wedge* itself — a PR correctly held as not-yet-validated by this fix still needs something to make it advance. Automatically running the missing validation (local-CI-as-first-class) is explicitly out of scope for #933 and deferred to a follow-up ("Part B"); `ci:bless`-style manual reruns remain the stopgap until that ships.

## Predecessor Context

- **ADR-033** (`033-mergeable-state-over-check-runs.md`): established trusting `mergeable_state` (`clean`/`unstable`) as authoritative and rejected a branch-protection read for a related purpose. This ADR reopens that exact question for required-context determination and reaches the same conclusion — the ADR-033 shortcut itself is untouched.
- **ADR-027** (`027-ci-gate-and-fix-reinvoke.md`) and **ADR-032** (`032-ci-gate-conjunctive-completion-label.md`): define the two-prong CI gate structure and `fabrik:awaiting-ci` semantics this ADR's `classifyCIFromRequiredContexts` addition participates in without otherwise disturbing.
- **ADR-058/059** (merge-queue / internal merge-train): `pollTrainCI` is part of the merge-train worker these introduced; this ADR's change to it stays compatible with its synchronous trial-branch CI-poll loop and timeout/cancellation contract.
- Closed issues **#855/#860**: fixed the zero-check-runs variant of this same root cause (a `blocked` `mergeable_state` silently overridden by an empty check-run read) — this ADR's fix is the same reusable pattern (consult a more authoritative signal even when the naive per-check read looks fine) applied to the non-empty-but-all-permissive variant.
