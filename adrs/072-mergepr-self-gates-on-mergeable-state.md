# ADR 072: `MergePR` Self-Gates on `mergeable_state`, Independent of Branch Protection

**Date**: 2026-07-26
**Status**: Accepted
**Issue**: #1094 — `MergePR` must gate on `mergeable_state` so Fabrik never merges red CI

## Context

`github.Client.MergePR` gated only on GitHub's `mergeable` field:

```go
if prData.Mergeable == nil || !*prData.Mergeable {
    return ErrNotMergeable
}
```

`mergeable` reports **merge-conflict status only**. It stays `true` while required status checks are still pending or outright failing — the CI-aware signal is `mergeable_state` (`clean` / `unstable` / `blocked` / `dirty` / `behind` / `draft` / `has_hooks` / `unknown`), which `MergePR` never read.

Two of `MergePR`'s four call sites — `pollForMergeable` (the merge-train landing path, ADR 059 D3) and `checkCIGate` (the direct/Validate path, ADR 033) — already read `mergeable_state` and refuse to proceed unless it is in `gh.MergeableStateAccepted` (`{clean, unstable}`) before ever calling `MergePR`. But this protection lived entirely in the *callers*. `MergePR` itself enforced nothing, so any call site that skipped (or never had) an upstream gate inherited no protection at all.

**This was not merely a hardening exercise — it was a live, unmitigated gap.** Branch protection (`enforce_admins: true`, or a non-admin token) normally masks the hole by having GitHub itself reject a server-side merge attempt on red CI. But `handarbeit/fabrik` — the repo Fabrik develops itself in — already runs with `enforce_admins: false` (a pre-existing setting, not introduced by this issue or by the 2026-07-26 fleet settings work), and Fabrik authenticates as `arbeithand`, an org admin. On that repo, `attemptMergeOnValidate`'s direct-merge fallback (`engine/stages.go`, reached whenever `wait_for_ci: false`) is the **only** merge path with zero independent `mergeable_state` awareness upstream of `MergePR` — it calls `EnablePullRequestAutoMerge` first, and on failure (interpreted as "PR already CLEAN/UNSTABLE, GitHub refuses to queue") falls straight through to `MergePR` with no settle/gate step in between. On this repo, that path could already merge a PR whose required checks were failing or still pending. A live candidate was observed the same day this issue was filed: `handarbeit/fabrik` PR #1078 sat at `mergeable_state=unstable` while `mergeable=true`, with required checks `["Analyze (go)"]` on `main`.

The operator also wants to relax `enforce_admins: false` on two other Fabrik-coordinated repos (`verveguy/liminis`, `verveguy/liminis-context-graph`) so a *human* can force-merge through GitHub's UI without granting the *engine* the same power. That change is blocked on this issue: today, on those repos, `enforce_admins: true` is the only thing standing between the engine and the same gap that is already live on `handarbeit/fabrik`.

### Prior Art

ADR 033 established that `mergeable_state ∈ {clean, unstable}` is the correct authoritative signal for "GitHub's branch protection considers this PR mergeable," in preference to Fabrik re-deriving the same answer from raw check-run classification — and specifically that `unstable` must be included, not excluded, because GitHub reports `unstable` only when a *non-required* check is red or still pending; a red or pending *required* check yields `blocked` instead. This ADR does not revisit that conclusion — it extends the same principle one layer deeper, into `MergePR` itself, so every caller inherits the guarantee regardless of what gate (if any) ran beforehand.

## Decision

### 1. `MergePR` self-gates on `mergeable_state`, in addition to the existing `mergeable` conflict check

After the existing `mergeable`/`merged` check passes, `MergePR` (`github/prs.go`) makes a second call to `FetchPRMergeableFields` (the same single-endpoint helper `pollForMergeable` and `checkCIGate` already use — no new bespoke fetch path) and refuses to merge — without ever calling the merge endpoint — unless the returned `mergeable_state` satisfies `gh.MergeableStateAccepted`.

A refusal here returns a new, distinct sentinel:

```go
var ErrNotMergeableCI = errors.New("PR mergeable_state is not CI-clean")
```

kept deliberately separate from the pre-existing `ErrNotMergeable` (the conflict/`dirty` case), so callers can distinguish "CI is not green" from "there is a merge conflict" via `errors.Is`. This separation matters operationally: a conflict drives Fabrik's rebase-reinvoke path (`fabrik:rebase-needed`, `checkMergeabilityGate`, `MaxRebaseCycles`) — a CI refusal must never enter that path or consume a rebase cycle. Conflating the two sentinels would have made `MergePR`'s new check silently start triggering rebase cycles for a class of failure (red/pending CI) that a rebase can't fix.

The refusal is logged with the observed `mergeable_state` so operators can see why a merge did not happen.

### 2. Accept `mergeable_state ∈ {clean, unstable}` — not strictly `clean`

`MergePR` reuses the existing `gh.MergeableStateAccepted` allowlist (`{clean, unstable}`) verbatim, rather than requiring `clean` alone.

This was the one open design question on this issue, explicitly raised to the operator before Research/Plan proceeded (a strictly-stronger-than-callers allowlist looked plausible at first glance, since "never merge red CI" sounds like it should mean "only merge when everything is green"). The operator's confirmed resolution (2026-07-26):

> `unstable` means the required checks are green — GitHub reports `unstable` only when a non-required check is red/pending; a red/pending required check yields `blocked` instead. The hole this issue closes ("never merge red CI") is really "never merge when a **required** check is red/pending" = `blocked`, and `blocked` is refused under either option — so strict-`clean` adds no safety on the states that actually matter.

Concretely: `blocked` is refused by both `{clean}` and `{clean, unstable}` alike, since it is neither. The only states where the two allowlists diverge are ones where all *required* checks are already green — refusing there would only ever block on an *optional* check, which by definition does not gate anything. Meanwhile, requiring strict `clean` would have broken two call sites that are explicitly out of scope for this issue and were deliberately left unchanged:

- `pollForMergeable` (merge-train landing) accepts `unstable` and calls `MergePR` immediately afterward — a stricter `MergePR` allowlist would strand already-accepted merge-train members.
- `checkCIGate` (direct/Validate path) clears `fabrik:awaiting-ci` and adds `stage:Validate:complete` on `{clean, unstable}` (the ADR 033 shortcut) *before* `MergePR` ever runs — a stricter `MergePR` allowlist would leave no gate label behind to drive a retry, effectively stranding the item silently.

Keeping `MergePR`'s allowlist byte-for-byte identical to the two callers that already gate on it (`MergeableStateAccepted`) is precisely what avoids both stranding traps, without pulling either out-of-scope function back into this issue.

**Operator note (governs required-check configuration, not `MergePR`'s allowlist):** whether a given check actually refuses an engine merge is controlled by the repository's *required status checks* list in branch protection, not by this allowlist. `handarbeit/fabrik` requires only `["Analyze (go)"]` today, so post-fix the engine refuses when that check is red/pending (`blocked`) and merges when only optional checks are pending (`unstable`) — the intended behavior. If an operator wants a different check to gate engine merges, it belongs in branch protection's required-checks list, not in `MergePR`.

### 3. The engine self-gates rather than relying on branch protection

This is the general principle this ADR establishes, not just the specific `{clean, unstable}` decision above: **Fabrik must not depend on GitHub-side branch protection (`enforce_admins`, required reviewers, etc.) to prevent the engine from merging on red CI.** Branch protection is a defense a *human operator* configures and controls; it is not a substitute for the engine enforcing its own safety invariant, because:

- The engine token may be (and in `handarbeit/fabrik`'s case, already is) an org admin, which bypasses `enforce_admins` entirely regardless of how it's set.
- An operator may reasonably want `enforce_admins: false` for the specific purpose of letting *humans* force-merge through the UI — without that relaxation silently also granting the *engine* the same power. Prior to this issue, those two things were inseparable: whatever branch protection allowed, `MergePR` would also do.

By moving the guarantee into `MergePR` itself, the engine's CI-safety property no longer depends on repo-level configuration the engine doesn't control and a human might reasonably want to relax for unrelated (human-workflow) reasons. This is what makes it safe to relax `enforce_admins: false` on `verveguy/liminis` and `verveguy/liminis-context-graph` for human force-merge, without reopening the same gap that was already live on `handarbeit/fabrik`.

### 4. No call-site behavior changes — only regression tests

None of `MergePR`'s four call sites (`attemptMergeOnValidate`'s direct-merge fallback, the rebase-reinvoke already-clean fallback, `landSingleton`, `landMergeTrainBatch`) read `MergePR`'s returned error to apply `fabrik:rebase-needed` or increment a rebase cycle today — that label is applied earlier and independently, from `settle.Status == PRMergeConflicting` (a separately-fetched signal), before `MergePR` is ever invoked. Each call site already logs any `MergePR` failure and lets the item retry on its own existing cadence (a full Validate re-dispatch, the next convergence pass, or the next merge-train cycle with members left in Queued). The acceptance criterion "a CI refusal must not apply `fabrik:rebase-needed` or consume a rebase cycle" was therefore already true by construction before this change — the work at the four call sites is regression tests locking that invariant in, not new escalation-avoidance logic.

## Consequences

- `handarbeit/fabrik`'s live gap (engine can merge red required-CI PRs due to `enforce_admins: false` + admin token) is closed immediately, independent of any repo-configuration change.
- Relaxing `enforce_admins: false` on the liminis repos (a separate, operator-driven change, out of scope for this issue) is now safe, because the engine no longer inherits merge power from branch protection relaxation.
- `MergePR` issues one additional REST GET per merge attempt (a second call to `FetchPRMergeableFields`, alongside the pre-existing `merged`/`mergeable` fetch). `MergePR` is only called when actually attempting a merge — a rare, non-polling event — so the added REST-quota cost is immaterial.
- A narrow TOCTOU window exists between an upstream gate (`pollForMergeable`, the `checkCIGate` shortcut) judging a PR acceptable and `MergePR`'s own re-check moments later. If `mergeable_state` flips in that window, `MergePR` now correctly refuses (`ErrNotMergeableCI`) rather than silently merging on a state the upstream gate no longer holds true — an intentional strengthening, not a regression, and covered by dedicated regression tests at the merge-train and rebase-reinvoke call sites.
- Humans force-merging through the GitHub UI are unaffected — this is an engine-side (`MergePR`) precondition only.

## Alternatives Considered

### Require strictly `clean`, not `{clean, unstable}`

This was the issue's original recommendation before operator review. Rejected per the reasoning in Decision §2 above: it adds no safety on the states that actually matter (a red/pending *required* check already yields `blocked`, refused either way), while breaking two already-correct, explicitly out-of-scope call sites (`pollForMergeable`, `checkCIGate`) that accept `unstable`.

### Fix only `attemptMergeOnValidate` (the one call site with the live gap), leave `MergePR` unchanged

Rejected: this would patch the one exposure known today but leave `MergePR` itself unprotected for any future or currently-unaudited caller. Moving the guarantee into `MergePR` means every caller — present and future — inherits it automatically, which is also the precondition for the `enforce_admins` relaxation the operator wants on the liminis repos.

### Query branch protection's required-checks list directly, instead of trusting `mergeable_state`

Not reconsidered in this ADR — already rejected in ADR 033 for the same reasons (extra API call, `admin:repo` scope requirement, brittle context-name matching) and those reasons apply identically one layer deeper here.

## Predecessor Context

- **ADR 033** (`033-mergeable-state-over-check-runs.md`): establishes `mergeable_state ∈ {clean, unstable}` (`gh.MergeableStateAccepted`) as the authoritative mergeability signal, and specifically why `unstable` must be included. This ADR extends that same allowlist one layer deeper, into `MergePR` itself, rather than only its callers.
- **ADR 058** (`058-merge-queue-integration.md`) / **ADR 059** (`059-internal-merge-train.md`): establish `pollForMergeable` and the merge-queue/merge-train landing paths that call `MergePR` after their own `MergeableStateAccepted` check. This ADR's regression-test requirement (`MergePR`'s new precondition must not strand a PR the calling gate already judged acceptable) is exactly about preserving those ADRs' invariants under the TOCTOU window described above.
