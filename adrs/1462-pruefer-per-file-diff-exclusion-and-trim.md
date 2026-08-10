# ADR 1462: Pruefer excludes and trims per file before measuring diff size (amends ADR-1113 §4)

**Status:** Accepted
**Date:** 2026-08-09
**Issue:** [#1462](https://github.com/handarbeit/fabrik/issues/1462)

## Context

ADR-1113 §4 established that `ReviewPR` fetches a PR's diff, compares its raw byte length against `Config.MaxDiffBytes`, and skips (not truncates) an oversized PR — with `excluded_paths` checked separately, whole-diff, as an independent gate. That design had two compounding defects, both filed as #1274 and re-filed narrowly (after #1279's implementation went unmerged) as this issue:

1. **The size gate ran before the exclusion gate.** `pruefer/review.go` compared `len(diff)` against `max_diff_bytes` immediately after fetch, before `changedPaths` was even parsed. An excluded file's bytes counted toward the size verdict regardless — `excluded_paths` could never rescue a PR that was only oversized because of a file it was configured to ignore.
2. **`allPathsExcluded` was all-or-nothing.** It returned true only when *every* changed path matched an exclusion glob, so a diff with even one non-excluded file alongside a giant excluded one was reviewed whole or not at all — there was no way to reduce a diff to its reviewable remainder by configuration alone.

`shadoworg/fantasy#1640` is the motivating instance: a 17 MB diff, 99.5% of which was a single JSONL eval corpus file, skipped 254 times over ~26 hours with nothing ever posted. Excluding that one file would have left the remaining 27 files — the actual code change — comfortably under any reasonable `max_diff_bytes`, but the size gate never gave `excluded_paths` the chance. `handarbeit/fabrik#1507` (a live PR carrying a vendored 74,315-line GraphQL schema fixture) is a second, contemporaneous instance of the identical class.

This ADR amends ADR-1113 §4 to reorder the gates and generalize path exclusion from all-or-nothing to per-file, and additionally generalizes "skip if still oversized" into "trim to fit, disclosed, if still oversized" — a strictly more permissive fallback than #1274's original scope, justified below.

### Relationship to the abandoned `adrs/1274-...` and to ADR-1427

An `adrs/1274-pruefer-per-file-diff-exclusion-and-too-large-notice.md` was drafted on the `fabrik/issue-1274` branch (PR #1279) but never merged — #1279 was abandoned, and Research for this issue confirmed the file does not exist anywhere in `main`'s git history. There is nothing to literally supersede; this ADR is the first version of this decision to actually land, and acknowledges #1274/#1279 as prior art rather than treating them as a document to redirect.

[adrs/1427-pruefer-diff-too-large-degrade-not-block.md](1427-pruefer-diff-too-large-degrade-not-block.md) is a **sibling, not superseded**, decision: it covers the case where GitHub's `.diff` media type refuses to render the diff at all (a 406 `too_large` response, no diff text ever obtained), falling back to the paginated files API for a bare path list. This ADR covers the case where the diff *is* obtained but is too large — a materially different code path (diff text exists and can be split/filtered/trimmed) that ADR-1427 explicitly flagged as out of its own scope and anticipated reconciling later ("reconcile the two `pruefer/notice.go` additions... rather than treating either as authoritative in isolation"). That reconciliation is this issue: both paths now share `pruefer/notice.go`'s `diffTooLargeMarker`/`alreadyNoticedTooLarge`, landing in the same file as originally anticipated rather than via a git merge conflict (since #1279 was abandoned, there was no second add to conflict with). ADR-1427's own decisions — the 406 classification, the files-API fallback, "fallback is path-list-only, not diff reconstruction" — are unaffected and remain as-built.

## Decision

**Amends ADR-1113 §4.** When `FetchPRDiff` succeeds (diff text obtained):

1. **Terminal exclusion runs first (R1, R3).** The existing all-or-nothing `allPathsExcluded` check — "every changed path matches an exclusion glob" — now runs *before* the `max_diff_bytes` comparison, not after. This one-line reorder is the minimal fix for defect 1: an excluded file can no longer contribute to a size verdict it will never actually be measured against, since it can't survive to the size gate in the all-excluded case, and per-file filtering (next) removes it from the measured total in every other case.

2. **Per-file exclusion generalizes the middle case (R2).** A new `pruefer/diffsplit.go` splits the raw diff into per-file blocks (`splitDiffFiles`) and partitions them against `cfg.ExcludedPaths` (`filterExcludedPaths`) — reusing `select.go`'s existing `matchesAny`/`matchGlob` matcher for the per-file test, not a new pattern language. A diff with some (not all) excluded files now measures and reviews only the survivors, instead of being treated as a single indivisible unit.

3. **Trim-to-fit replaces a bare skip when still oversized (R4).** If the diff still exceeds `max_diff_bytes` after exclusion, `trimToFit` drops additional files — largest first, original relative order preserved among survivors — until the remainder fits, and the review proceeds on that remainder. This is a deliberate generalization beyond #1274's original scope (which only ever discussed *exclusion* bringing a diff under budget): trimming is exclusion's natural continuation, not a separate feature, and the issue's own R4 text ("if the diff still exceeds max_diff_bytes *after* exclusions") reads procedurally rather than conditionally on `excluded_paths` being configured at all. A PR with no `excluded_paths` configured and one huge file among many small ones is reviewed on the small ones' remainder — trimming just is exclusion generalized to "any reason a file might need to be left out."

4. **Disclosure is unconditional wherever anything is dropped (R4).** Whatever exclusion or trimming removed is passed to the reviewing Claude invocation as two new `ReviewRequest` fields (`OmittedExcludedPaths`, `OmittedTrimmedPaths`) and rendered as an additive prompt section naming the files and giving a concrete `git diff ... -- . ':!path'` pathspec. This is load-bearing, not cosmetic: Claude never receives diff text as prompt content — it re-derives its own view from the cloned worktree via `git diff` tool calls — so Go-side trimming of the *measured* diff string does not, by itself, stop Claude from independently reading or diffing an omitted file (in the motivating scenario, potentially loading a 17 MB JSONL file into its own context). The pathspec gives it an actionable way to exclude the same files from its own inspection; `validRightAnchors`' anchor-invalidity demotion (below) backstops the case where Claude reports a finding on an omitted file anyway.

5. **Nothing usable surviving is still a terminal skip.** If exclusion and trimming together drop every block (`len(blocks) > 0 && len(kept) == 0`), the review falls through to the existing `SkipDiffTooLarge` disposition rather than reviewing an empty diff that would present as a complete review — exactly the failure mode R4 warns against ("a review of a partial diff that presents as a review of the whole diff is worse than a skip"). This check is on block *count*, not on whether the resulting near-empty diff technically fits the byte budget.

6. **`diff` is rebound to the filtered/trimmed text (R6).** `review.go` reassembles `preamble + kept blocks` (`joinDiff`) and rebinds the local `diff` variable to it before `validRightAnchors(diff)` is called for anchor validation. Since `validRightAnchors` is a pure function of whatever diff string it's handed, this is sufficient on its own — no separate anchor-scrubbing logic is needed. A finding Claude reports against an excluded or trimmed file's line can never validate as an inline-comment anchor; it demotes into the review body through the existing `partitionFindings` path, the same as any other unanchorable finding.

7. **The notice mechanism is reused, not duplicated (R5).** `pruefer/notice.go` gains `buildDiffTooLargeAfterFetchNoticeBody`/`postDiffTooLargeAfterFetchNoticeOnce`, sharing `diffTooLargeMarker`/`alreadyNoticedTooLarge` with ADR-1427's diff-unavailable notice verbatim. A notice for this path is only posted when the *raw* (pre-filter) diff was actually oversized — a routine `vendor/**` exclusion on an otherwise-small PR must not spam a notice on every PR — but the idempotency check means a PR that hits the diff-unavailable path on one poll and this path on a later poll (same head SHA) never accumulates two notices.

8. **Size-related log lines are self-diagnosing (R7).** Every skip or trim decision now logs the post-exclusion measured size, an excluded-count-out-of-total, and an explicit note distinguishing "no `excluded_paths` configured" (the state every operator starts in) from "`excluded_paths=[...]` configured" — replacing a message that was byte-identical whether or not exclusion was configured or did anything, the same failure mode #1496's R3 logging requirement was added to kill for the already-reviewed check.

The 406-fallback branch (`FetchPRFiles`, when `FetchPRDiff` itself 406s) is untouched: it has no diff text to split, filter, or trim, and its own all-or-nothing `excluded_paths` check (via the plain path list) is unaffected. `Config.MaxDiffBytes` never applies to that branch, per ADR-1427.

### Why trim-to-fit generalizes beyond exclusion, rather than staying exclusion-only

The narrower alternative — trim only engages when `excluded_paths` is configured, otherwise the old bare skip stands — was considered and rejected. It would have left the size-only case (no `excluded_paths` at all, one huge file among small ones) exactly as broken as before, when the fix for it is mechanically identical to the exclusion case: drop the large file(s), review the rest, disclose what was cut. Since disclosure (point 4 above) makes a trimmed review distinguishable from a complete one to both the model and any human reading the PR, there is no meaningful safety cost to generalizing — and a real cost to not doing so, since `excluded_paths` being unset is the default, out-of-the-box state every operator starts in.

### Known, carried-forward limitation: quoted and binary paths

`resolveFilePath`'s primary path source is the unambiguous `+++ b/<path>` content line (reusing `diffanchor.go`'s existing `!inHunk`-gated matching, not a second independent fix of the same "`+++` mid-hunk" bug #1274's original branch encountered and fixed iteratively). It falls back to the `diff --git a/X b/Y` header's own greedy, ambiguous `b/`-side capture only when no such line exists — a deleted file, a binary file, or a pure mode change. A path containing a space, quote, backslash, or non-ASCII byte triggers git's C-quoted header form (`diff --git "a/foo\"bar" "b/foo\"bar"`), which none of the regexes in `diffsplit.go` (or, pre-existing, in `select.go`'s `ParseChangedPaths`) recognize — such a file's block would misattribute into `splitDiffFiles`'s preamble rather than becoming its own block. This is not new: it mirrors `ParseChangedPaths`'s existing gap. Not closed here, since the common case (every non-binary changed file has an unambiguous `+++ b/<path>` line) is what the motivating bug actually needed.

## Consequences

**Positive:**
- `excluded_paths` now behaves as documented: a large, deliberately-vendored or generated file can be configured out of size consideration without losing review of everything else in the same PR — the specific fix `fantasy#1640` and `fabrik#1507` both needed.
- A diff oversized for reasons unrelated to configuration (no `excluded_paths` at all) is no longer a hard skip either — the operator gets a reviewed remainder plus a disclosed list of what was cut, rather than nothing.
- Disclosure to the model closes the risk R4 was written against: a partial review that silently presents as complete. The reviewing Claude invocation, the review's own inline-comment anchoring, and the PR notice all agree on the same omitted-files list.
- `pruefer/notice.go`'s two size-related notice paths (diff-unavailable, diff-too-large-after-fetch) share one marker convention by construction, so a human sees identical notice framing regardless of which GitHub failure mode produced it.

**Negative / Trade-offs:**
- Prompt-only disclosure is best-effort, not enforced: Claude retains full `Read`/`Grep`/`Glob`/`git` access to the clone and could still inspect an omitted file despite the instruction. Mitigated by the concrete pathspec example and backstopped by anchor-invalidity demotion (an inline comment can never land on an omitted file even if Claude reports one) — but a demoted finding could still reference a file the prompt named out of scope, appearing in the review body. Acceptable per R4's framing (disclosure, not a hard technical block).
- Quoted-path and binary-file gaps in `resolveFilePath`'s header fallback are carried forward, not closed (see above).
- `trimToFit`'s largest-first policy is a heuristic, not a configurable one: an operator who wants a different trimming order (e.g., alphabetical, or diff-position-based) has no lever for it. Matches the salvaged reference implementation's approach and directly targets the motivating scenario (one oversized file among many small ones); revisit if a different real-world shape emerges.

## Related Work

- `adrs/1113-pruefer-v1-architecture.md` §4 (amended by this ADR).
- `adrs/1427-pruefer-diff-too-large-degrade-not-block.md` — sibling decision for the diff-*unavailable* path (406 `too_large`, no diff text ever obtained); unaffected by this ADR except that `pruefer/notice.go` now hosts both notice bodies side by side, exactly as ADR-1427's own "Related Work" section anticipated.
- `#1274` (original report), `#1279` (abandoned implementation PR; its branch `origin/fabrik/issue-1274` held the salvaged `diffsplit.go`/`diffsplit_test.go` this issue ported and re-reviewed against current `main`, re-deriving `resolveFilePath` to reuse `diffanchor.go`'s anchor-scanning approach rather than the salvaged branch's independent regex-only fix of the same bug). An `adrs/1274-...` ADR was drafted on that branch but never merged and does not exist on `main`'s history — nothing for this ADR to literally supersede.
- `#1496` — origin of the "self-diagnosing log line" convention (`compared=... found=... outcome=...`) this ADR's R7 logging applies to the size gate.
- `#1507` — the contemporaneous, live production instance of this exact defect class (a vendored GraphQL schema fixture) that motivated prioritizing this issue.

**References:** [cmd/pruefer/README.md](../cmd/pruefer/README.md)
