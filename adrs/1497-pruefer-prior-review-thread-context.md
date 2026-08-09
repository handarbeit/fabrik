# ADR 1497: Pruefer's review prompt carries prior-thread context

**Status:** Accepted
**Date:** 2026-08-09
**Issue:** [#1497](https://github.com/handarbeit/fabrik/issues/1497)

## Context

`buildReviewPrompt` (`pruefer/claude.go`) assembled the review prompt from the diff, the PR description, and a fixed set of instructions — nothing about the PR's existing review threads. Every review was therefore a cold read: Pruefer could not tell a first pass from a seventeenth, could not see that a finding it was about to raise was already an open thread, and could not see that a thread it opened two rounds ago had been addressed with a reasoned decline.

On `handarbeit/fabrik#1477`, this produced 21 open threads that deduplicate to roughly a dozen distinct findings — each duplicate a fresh thread restating an observation already sitting open on the same lines. Fabrik's review-reinvoke loop is bounded by `MaxReviewCycles` (default 5); a reviewer that re-raises resolved or deliberately-declined findings on each pass consumes those cycles without converging, and the stage terminates at `pauseForReviewCycleLimit` instead of at agreement (`#1448`).

This issue is deliberately scoped to the *mechanism* (missing context), independent of `#1496` (the re-review-at-unchanged-head loop, which fixes the *rate* of duplication and was merged before this issue landed — see `d608e5ea`). A legitimate re-review after a real push still needs to re-derive only what actually changed, not every finding from scratch.

## Decision

### R1 — Resolved threads are included, tagged with resolution state

`FetchPRReviewThreads` (`github/prs.go`) fetches **both** resolved and unresolved review threads via a new PR-number-keyed GraphQL query, modeled on `FetchPRReviewDecision`'s shape rather than `applyLinkedPRs`' issue-linked traversal (Pruefer reviews PRs directly with no linked issue in scope). Each thread is returned as `PRReviewThread{ID, Path, Line, IsResolved, IsOutdated, Comments []PRReviewThreadComment}` — a thread-grouped shape, not `github.Comment`'s flat per-comment shape, so `buildReviewPrompt` can render "original finding, then author's reply" as a coherent unit.

This is a deliberate divergence from `github/project.go`'s `applyLinkedPRs`, which discards resolved-thread bodies and only counts them (`LinkedPRResolvedThreadCount`). That precedent serves the engine's reinvoke use case, where only unresolved feedback is actionable. Pruefer's problem is different: R2's policy ("don't restate a resolved thread unless the fix introduced a new defect") is meaningless unless the reviewer can see what was resolved and how. The trade-off the issue itself named — suppressing repetition vs. blinding the reviewer to a regression on already-reviewed ground — is resolved in favor of visibility: both groups are included, each tagged `[OPEN]` or `[RESOLVED]` (plus an outdated note), so the reviewer decides what to do with a resolved thread rather than never seeing it.

### Thread-fetch failure degrades, doesn't fail the review

`ReviewPR` (`pruefer/review.go`) calls `FetchPRReviewThreads` after the diff/changed-paths handling and before cloning. On error, it logs a warning and proceeds with `nil` threads — a cold-read fallback to pre-#1497 behavior for that one pass — rather than returning `ReviewOutcome{Err: ...}`. This mirrors `PendingForceReview`'s existing degrade pattern in the same function, not `FetchPRReviews`'s fatal one: `FetchPRReviews`'s result gates eligibility (an already-reviewed-at-this-SHA check), so a fetch failure there must block; thread context is purely advisory prompt content, so a transient GraphQL error must not block a review that would otherwise succeed. The cost is that a *persistent* fetch failure is invisible beyond a log line — no PR-facing notice like `postDiffUnavailableNoticeOnce`'s. Acceptable for V1 since it degrades to the prior baseline rather than blocking review.

### R3 — Deduplication is code-level, not prompt-only

Two of `#1477`'s duplicates (`ArchiveProjectItem`, `UpdateProjectItemStatus`) were raised twice *within a single review pass* — prior-thread context cannot help there, since there is no prior pass to reference. `dedupeFindings` (`pruefer/findings.go`) collapses findings sharing the same `(Path, Line)` — the same anchor identity `partitionFindings` (`pruefer/diffanchor.go`) already uses for inline-comment anchoring — keeping the higher-`severityRank` entry on collision (so collapsing duplicates never accidentally weakens a `REQUEST_CHANGES` threshold decision) with ties keeping the first-seen entry. It runs in `ReviewPR` immediately after `parseReviewFindings`, before `decideEvent`/`partitionFindings` see the findings.

A prompt instruction ("report each distinct underlying finding once") is also added, but only as defense in depth. It is not trusted alone: Claude already violated an unstated norm on `#1477` without being told not to duplicate, so a stated norm is better but not a guarantee. The code-level dedup is what AC3 actually verifies.

### R4 — A fixed, non-configurable cap bounds the prompt

`maxPromptThreads = 20` (`pruefer/claude.go`) bounds how many threads `buildReviewPrompt` renders, unlike `MaxDiffBytes`/`ADR-1251`'s `RequestChangesThreshold`, which are threaded through the full flag/env/YAML/default chain. No evidence yet that operators need to tune this, and keeping it a constant keeps `buildReviewPrompt` a pure function of `ReviewRequest` with no `Config` dependency — simpler to unit test at the boundary.

`selectPromptThreads` orders threads by signal before truncating: unresolved before resolved, then non-outdated before outdated within each group, then most-recently-commented first. Truncation therefore drops resolved threads before unresolved ones, and stale threads before active ones within each group — the reviewer keeps the highest-signal context under the cap. When threads are omitted, the prompt states the count and that unresolved/current threads were prioritized (never a silent drop).

This is separate from, and upstream of, `github/prs.go`'s own `reviewThreads(first: 50)` GraphQL ceiling (unpaginated, matching the existing `fetchItemDetailsQuery` fragment's ceiling). A PR with more than 50 threads would lose the excess at the fetch layer before `selectPromptThreads` ever sees it — out of scope for this issue; R4's cap (20) sits well under the fetch ceiling (50).

### R5 — Sharper bar for a low-severity finding, prompt wording only

The instruction "Skip nitpicks and style preferences unless they matter" is revised to explicitly name the failure mode observed on `#1477` (a dozen `low`-severity fidelity observations against a test fixture): a `low` finding must be something a reviewer would actually act on, not merely true. This stays a prompt-wording change, not a `severity.go` structural change — `severityRank`/`decideEvent` drive `Config.RequestChangesThreshold` (`ADR-1251`), and touching that ladder has a bigger, less reversible blast radius than the issue's "judgement call" framing warrants.

### Forward-compatibility with `#1446`

`#1446` will split `buildReviewPrompt` into an overridable guidance half and a Go-owned contract half. The thread-context section and R2 policy paragraph added by this issue are rendered by `renderReviewThreads`, with a doc comment marking them as belonging to the future Go-owned half — a repo using `mode: replace` on the guidance half must not be able to silently drop this context and reintroduce the duplicate findings this issue exists to eliminate. `#1446` inherits this constraint from the doc comment rather than rediscovering it.

## Consequences

**Positive:**
- A reviewer that already raised a finding, or already saw it declined with reasoning, has the context to not repeat itself — directly addressing the `#1477`/`#1448` failure mode of `MaxReviewCycles` exhausting without convergence.
- `dedupeFindings` is a deterministic, testable guarantee against intra-pass duplication, not just an advisory instruction.
- The prompt is bounded regardless of how many threads a PR accumulates, with an explicit, visible statement of what was dropped rather than a silent truncation.

**Negative / Trade-offs:**
- A persistent thread-fetch failure degrades silently (log only, no PR-facing notice) — acceptable since it degrades to the pre-#1497 baseline, but worth revisiting if it proves to recur in practice.
- Including resolved-thread bodies grows the prompt on a long-lived PR; R4's cap bounds this, but a PR that accumulates more than 20 threads across many rounds will always have some threads (resolved or outdated first) omitted from a given pass.
- The GraphQL `reviewThreads(first: 50)` ceiling has no pagination; a PR with more than 50 threads silently loses the excess at the fetch layer, upstream of and independent from R4's prompt-level cap.

## Related Work

- `adrs/1113-pruefer-v1-architecture.md` — Decision 5 ("Claude never calls `gh` or submits the review itself") is the load-bearing constraint this issue works within: all prior-thread context is Go-fetched and Go-rendered into the prompt text, never tool-fetched by Claude.
- `adrs/1189-pruefer-inline-review-comments.md` — established `partitionFindings`'s `(Path, Line)` anchor identity, reused by `dedupeFindings`.
- `adrs/1251-pruefer-severity-gated-request-changes.md` — the `severityRank`/`RequestChangesThreshold` machinery this issue deliberately does not touch (R5 stays prompt-only).
- `adrs/1427-pruefer-diff-too-large-degrade-not-block.md` — precedent for "degrade rather than block, tell the reviewer/log rather than silently drop," followed here for both the thread-fetch degrade and R4's cap.
- `#1446` (repo-resident review skill, blocked by this issue) — inherits the Go-owned/overridable boundary noted above.
- `#1456` (explicit summary delimiters) — edits `buildReviewPrompt`'s output-contract tail concurrently; this issue's changes are confined to the context preamble, so the two are textually non-conflicting.
- `#1496` (re-review-at-unchanged-head loop) — the blocking issue; already merged (`d608e5ea`) before this issue's implementation began.

**References:** [cmd/pruefer/README.md](../cmd/pruefer/README.md)
