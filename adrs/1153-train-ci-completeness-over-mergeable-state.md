# ADR 1153: `pollTrainCI` Requires Check-Run Completeness Before Declaring Green, Strict on Non-Required Failures

**Date**: 2026-07-27
**Status**: Accepted
**Issue**: #1153 — merge-train declares CI green while checks are still running (`mergeable_state` "unstable" accepted, check-run loop unreachable)

## Context

`pollTrainCI` (`engine/merge_train.go`) polls a merge-train batch's trial-branch integration PR in a loop, with two independent signals per iteration: GitHub's `mergeable_state` aggregate (`FetchPRMergeableFields`), and the individual check runs on the trial SHA (`FetchCheckRuns` → `gh.ClassifyCheckRuns`, plus the #933 required-context classifier, `classifyRequiredContexts`).

Before this change, an accepted `mergeable_state` (`gh.MergeableStateAccepted`, `clean` or `unstable`) returned `TrainCIGreen` immediately — before the check-run pass ever ran. `mergeable_state` is computed by GitHub from **required** checks only (per branch protection); `unstable` specifically means "a *non-required* check is failing or still pending." On a repo where the actual test suite isn't marked required (`handarbeit/fabrik`'s own configuration — only `Analyze (go)` is required on `main`), the train would therefore merge an integration PR the instant the required subset finished, regardless of what non-required CI was still in flight.

This was observed live on integration PR #1150 (2026-07-27): `Analyze (go)` (the sole required check) went green at 02:42:09, the train merged the batch 20 seconds later, and `Test and vet` (non-required, running the actual test suite) reported success 14 seconds *after* the merge had already happened. The batch happened to be fine — every member had passed CI individually before being queued — but the train did not wait for its own trial branch's tests to finish.

The check-run pass below the `mergeable_state` branch already existed and already handled `queued`/`in_progress` runs correctly, and already applied #933's required-context classification — but it was unreachable on the common path, because the `mergeable_state` branch returned first. It was, in effect, dead code whenever `mergeable_state` resolved before all checks finished.

This is the same defect class as #933 (`adrs/933-required-status-context-config.md`) one layer up: #933 fixed `settlePRMergeState`/`checkCIGate` treating absent/skipped checks as a pass; this issue fixes the merge train treating *pending non-required* checks as a pass. Notably, #933's own integration PR was sitting in the train's Queued column when this was found, about to be landed by the exact mechanism carrying this flaw.

### Why this doesn't reopen ADR-072

[ADR-072](072-mergepr-self-gates-on-mergeable-state.md) (#1094) established that `mergeable_state ∈ {clean, unstable}` is the correct "GitHub considers this mergeable" signal for `MergePR`'s single-PR self-gate, and specifically that `unstable` belongs in the accepted set *for that use case* — a single, human-observable PR merge, where a operator can see and tolerate a still-running non-required check. This ADR does not revisit that conclusion; `gh.MergeableStateAccepted` and `MergePR` are unchanged. The distinction this ADR draws is that an unattended **batch** merge of N issues has no human in the loop to notice a still-running check — it needs a stronger, additional completeness guarantee that the single-PR path does not.

## Decision

### 1. `mergeable_state` becomes a red/permission gate, not a green shortcut, in `pollTrainCI`

- `mergeable_state == "dirty"` continues to return `TrainCIRed` immediately — unambiguous, unchanged.
- An accepted `mergeable_state` (`clean`/`unstable`) no longer returns `TrainCIGreen`. It is recorded (`mergeableAccepted`) and used only as one further input to the zero-check-runs branch (below) — necessary, but not by itself sufficient, for green.
- Every other state falls through to the check-run pass, exactly as before.

### 2. The check-run pass is the actual green determinant on the common path

Reachable on every iteration now (previously dead code whenever `mergeable_state` resolved first), unchanged in its own logic:

- Any check run `queued`/`in_progress` → keep polling. This is the `#1150` case: a non-required check still running while `mergeable_state` already reads accepted.
- A confirmed failing check run → `TrainCIRed` (see the Strict policy below).
- All check runs completed with no failures (`gh.CheckRunsReady`) → run the existing #933 `classifyRequiredContexts` pass, unchanged: `RequiredContextsSatisfied` → `TrainCIGreen`; `RequiredContextsFailed` → `TrainCIRed`; otherwise keep polling.

### 3. Zero-check-runs handling (#933) is preserved exactly, with one new arm

The #933 branch for a trial SHA with **no check-run footprint at all** (e.g. GitHub Actions disabled) is unchanged: a confirmed required-context failure still returns `TrainCIRed` immediately. One new arm is added after it: `mergeableAccepted && classifyRequiredContexts(...) == RequiredContextsSatisfied` → `TrainCIGreen`. This is the one place `mergeable_state` remains genuinely load-bearing for green — with zero check runs, there is no per-check completeness signal to consult at all, so an accepted `mergeable_state` is the only remaining evidence that nothing is outstanding.

### 4. Non-required-failure policy: **Strict** — any confirmed check-run failure blocks the train, required or not

Once the shortcut is removed, `gh.ClassifyCheckRuns`'s existing `CheckRunsFailed` classification — which does not distinguish required from non-required checks — starts blocking on *any* confirmed failure on the trial SHA, including one Fabrik cannot identify as non-required. This is a deliberate choice, not an oversight:

- Fabrik has no general way to determine which checks are required beyond the opt-in `RequiredStatusContexts` config (`adrs/933-required-status-context-config.md` §"Why a branch-protection read still doesn't work here" — reading branch protection realistically 403s for non-admin tokens). A "permissive on non-required failures" policy would need to distinguish "confirmed non-required failure" from "confirmed required-but-unconfigured failure," and for the common **unconfigured** case (`handarbeit/fabrik` itself has no `required_status_contexts` entry) it cannot — it would have to default to strict anyway, so the extra classification machinery buys nothing for the repo that triggered this issue.
- A wrong-direction Strict call does not stall the batch indefinitely: it drives the existing bisection/ejection mechanism, which isolates and ejects the poisoning member and re-forms the survivors. The cost is one bisection cycle.
- A wrong-direction Permissive call would silently reintroduce this issue's own failure class, just narrowed from "pending" to "failed" — a worse outcome for an unattended batch merge than an occasional unnecessary bisection.
- It requires zero new classification code — removing the shortcut alone is sufficient — keeping the change contained to `pollTrainCI`'s control flow.

**Escape hatch for a known-flaky non-required check**: none, beyond not running it on `main`/the trial branch at all. `RequiredStatusContexts` only *adds* required contexts; it has no mechanism to *exempt* a check from `ClassifyCheckRuns`'s generic failure classification. This is a pre-existing limit of `ClassifyCheckRuns`, unchanged by this issue.

### 5. Logging: every terminal decision now names the specific checks it was based on

A `describeCheckRuns` helper renders `"name (status-or-conclusion)"` pairs (conclusion when `status == "completed"`, else the raw status), joined by `", "` — mirroring the existing convention in `classifyCIFromCheckRuns` (`engine/ci.go`). It backs the `TrainCIRed`/`TrainCIGreen` returns from the check-run pass and the `TrainCIPending` timeout returns (via loop-scoped `lastPending`/`lastFailed` tracking), so a batch's green/red/pending outcome can be reconstructed from the logs after the fact. Previously, the `mergeable_state` shortcut path logged nothing about individual checks at all — it returned before ever fetching them.

## Rationale

**Compose, don't race**: the two signals disagreed about what "done" means (required-only vs. every check), and letting the faster one win by construction discarded the slower one's answer. Making `mergeable_state` gate-only and the check-run pass load-bearing for green means both signals actually get consulted before a batch-wide, unattended decision is made.

**No changes to shared classifiers**: `gh.ClassifyCheckRuns` and `classifyRequiredContexts` are shared with `settlePRMergeState`/`classifyCIFromCheckRuns` (the single-PR path); ADR-933 already established they must not be tightened globally, since that would regress the single-PR path's legitimate tolerance of skipped/neutral non-required runs. This issue's fix is entirely a `pollTrainCI` control-flow change — the classifiers themselves are reused unmodified, exactly as `classifyRequiredContexts` itself was layered on top rather than folded in for #933.

## Alternatives Considered

### Split `ClassifyCheckRuns`'s failed set into required-vs-not, tolerate non-required failures

Rejected. As above, this can only be made precise for repos with a fully-populated `RequiredStatusContexts`; for the common unconfigured case it degrades to the same strict behavior anyway, so it adds classification complexity without closing the gap for the repo that motivated this issue. Deferred — nothing about this decision prevents adding it later if a real, common need for tolerating a specific known-flaky non-required check emerges.

### Trust `mergeable_state` for completeness whenever it resolves, but re-poll once more before accepting green

Rejected. This just narrows the race window instead of closing it — a single extra poll tick doesn't guarantee the non-required check has finished, and it reintroduces exactly the kind of timing-dependent heuristic ADR-933 already argued against in a related context.

### Fix `pollForMergeable` (the landing-step function, `engine/merge_train.go`) in the same change

Out of scope for this issue, per its explicit scope section — only `pollTrainCI` is named. `pollForMergeable` has an identical `mergeable_state`-only shortcut with no completeness pass at all, used later when actually landing the batch. In the common case (the draft CI PR is reused as the landing PR), it re-polls the same PR/SHA `pollTrainCI` just validated, so its own shortcut is largely inert post-fix for that case — but it is a separate function on a separate call path and was not touched here. Flagged as a candidate fast-follow.

## Consequences

**Positive:**
- Closes the #1150 failure mode: the train no longer merges a batch while a non-required check is still `queued`/`in_progress` on the trial SHA.
- The check-run pass (including #933's required-context classification) is now reachable on the common path instead of being dead code.
- Zero-check-runs handling (#933) and its existing test coverage are unaffected — verified by `TestPollTrainCI_MergeableStateClean_ReturnsGreen` / `_Unstable_ReturnsGreen` (which exercise exactly this branch, via the mock's default zero-check-runs behavior) continuing to pass unmodified.
- Every green/red/pending decision now logs the specific checks it was based on.

**Negative / Trade-offs:**
- One additional `FetchCheckRuns` call per poll iteration on the common green path (previously avoided by the shortcut). At the existing 30-second poll interval, train-only scope, this is immaterial API cost.
- Strict non-required-failure blocking can eject a batch member over a flaky, non-required check that has nothing to do with the member's actual changes. Mitigated by the existing bisection/ejection mechanism (cost: one bisection cycle, not an indefinite stall) — accepted as the better failure mode versus Permissive's silent reintroduction of this issue's own class of bug.

## Predecessor Context

- **ADR-072** (`072-mergepr-self-gates-on-mergeable-state.md`, #1094): established `mergeable_state ∈ {clean, unstable}` as correct for `MergePR`'s single-PR self-gate, `unstable` included deliberately. Not revisited — this ADR is the batch-path sequel, establishing that an unattended merge of N issues needs a completeness guarantee a single human-reviewed merge doesn't.
- **ADR-933** (`933-required-status-context-config.md`, #933): added `classifyRequiredContexts` and `gh.ClassifyCheckRuns` as the shared classifier, explicitly leaving "the ADR-033 shortcut... untouched... reopening that decision is explicitly out of scope for #933." This ADR is the direct sequel: it reopens exactly the shortcut #933 declined to touch, for `pollTrainCI` specifically. #933's zero-check-runs branch and required-context classification are preserved and reused unmodified.
- **ADR-058/059** (merge-queue / internal merge-train): `pollTrainCI` is part of the merge-train worker these introduced; this ADR's change stays compatible with its synchronous trial-branch CI-poll loop and timeout/cancellation contract (unchanged).
