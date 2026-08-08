# ADR 1427: Pruefer degrades, not blocks, on a 406 too_large diff (amends ADR-1113 §4)

**Status:** Accepted
**Date:** 2026-08-08
**Issue:** [#1427](https://github.com/handarbeit/fabrik/issues/1427)

## Context

ADR-1113 §4 established that `ReviewPR` fetches a PR's diff via `FetchPRDiff` before cloning, purely as a guard: measuring size against `max_diff_bytes` and parsing changed paths for `excluded_paths` matching. That guard assumed the fetch itself always succeeds or fails transiently. It does not: GitHub's `.diff` media type has a hard, undocumented-in-code 20,000-line ceiling, independent of `max_diff_bytes`. A PR whose diff exceeds it gets `406 Not Acceptable` with `{"errors":[{"code":"too_large"}]}` — deterministic, and identical on every retry until the PR's head changes.

`doWithAccept` (`github/rest.go`) had no case for 406; it fell through to the generic `>= 400` error path. `ReviewPR` treated any `FetchPRDiff` error identically — `ReviewOutcome{Err: ...}` — which the daemon logs and naturally retries next poll, per ADR-1113 §9's "state is GitHub-derived, retry is free" design. For a *transient* failure that design is correct. For this *deterministic* one it means an unbounded, backoff-free hot loop: one API round-trip every `poll_interval_seconds`, forever, with no comment ever posted and no `Skipped` disposition ever recorded — confirmed live on `verveguy/zusammen#103` (245 files, +2182/-26253, ~84 unsuccessful cycles over ~2h50m before diagnosis).

The deeper issue ADR-1113 §4 didn't anticipate: the fetched diff is a **guard input**, not the review's actual content — Claude re-derives its own diff from the local clone `clone()` produces a few lines later (§4's own text already notes this). A guard whose own acquisition fails should degrade the guard, not block the review it's guarding.

This issue is a sibling to #1274 ("one oversized file silently suppresses review of the whole PR"), which covers the case where the diff **is** obtained and then trips `max_diff_bytes`. This issue covers the case where the diff is **never obtained at all** — control never reaches the `max_diff_bytes` comparison. At the time this issue landed, #1274's implementation (`fabrik/issue-1274`, PR #1279) was complete but unmerged.

## Decision

**Amends ADR-1113 §4.** A `FetchPRDiff` failure is no longer treated uniformly. `doWithAccept` now classifies GitHub's specific 406 `too_large` response into a dedicated sentinel, `github.ErrDiffTooLarge`, using the same `errors.Is`-compatible pattern already established for `ErrNotFound`/`ErrMethodNotAllowed`/`ErrUnprocessableEntity` — narrowly gated on both `resp.StatusCode == 406` *and* a successful decode of `errors[0].code == "too_large"`, so an unparseable or differently-shaped 406 body (or any other status) keeps the prior generic-error behavior unchanged.

`ReviewPR` (`pruefer/review.go`) branches on this sentinel:

- **Not `ErrDiffTooLarge`** (network error, timeout, 5xx, etc.): unchanged — `ReviewOutcome{Err: ...}`, naturally retried next poll. This is still the right behavior for a genuinely transient failure.
- **Is `ErrDiffTooLarge`**: attempt a fallback via `github.FetchPRFiles`, a new paginated wrapper around `GET /pulls/{n}/files` (100/page, following `FetchLabelAppliedAt`'s existing pagination idiom) — a different GitHub endpoint with no line-count ceiling.
  - **Fallback succeeds**: the review proceeds normally against the local clone, exactly as if the diff had been fetched — `diff` is treated as `""` for the remainder of the call, and the fallback's path list feeds `excluded_paths` matching in place of `ParseChangedPaths(diff)`. No `max_diff_bytes` comparison runs on this path (see below).
  - **Fallback also fails**: `ReviewOutcome{Skipped: true, Reason: SkipDiffTooLarge}` — the same terminal disposition the existing `max_diff_bytes` gate already produces, reused rather than widened per the issue's explicit instruction. A single idempotent PR comment is posted first (see below) so a human sees why, rather than the failure only ever appearing in a log line.

### Fallback is path-list-only, not diff reconstruction

`/pulls/{n}/files`'s per-file `patch` field lacks the `diff --git`/`+++ b/` header lines `validRightAnchors` (`pruefer/diffanchor.go`) requires to compute inline-comment anchors, and is itself truncated for very large individual files. Synthesizing a fake diff body just to preserve anchoring was considered and rejected as needless fragility for marginal benefit: `validRightAnchors("")` already returns an empty anchor set, so every finding degrades cleanly into the review body via the existing `partitionFindings` demotion path — no inline comments for these PRs, but a complete review body. This satisfies R3's "the review proceeds normally" without inventing a diff-shaped artifact GitHub never actually produced.

A corollary: no `max_diff_bytes` comparison runs on the fallback path. GitHub's 406 already establishes the diff exceeds 20,000 lines, which exceeds any reasonable `max_diff_bytes`, and there is no diff text to measure in the first place. Only `excluded_paths`, checked against the fallback's path list, can still skip a fallback-path PR (`SkipExcludedPath`, not `SkipDiffTooLarge`).

### The notice mechanism deliberately duplicates #1274's marker format, not its code

R4 requires a single idempotent PR comment, keyed to the head SHA, when Pruefer ultimately declines to review a PR for a size reason — and requires coordinating with #1274 so the two size paths (diff obtained-but-oversized vs. diff never obtained) don't post divergent or duplicate notices. At the time of this issue, #1274's branch already had a complete, tested implementation of exactly this mechanism (`pruefer/notice.go`: `diffTooLargeMarker(headSHA) string` → `<!-- pruefer:diff-too-large:%s -->`, and `alreadyNoticedTooLarge(comments []gh.Comment, headSHA string) bool`, which scans `FetchIssueComments` for the marker — the same "derive idempotency from GitHub, never persist locally" pattern ADR-1113 established for review state itself) — but that branch was unmerged, so this issue's branch had none of it.

Rather than either (a) inventing an independent marker format that would silently diverge from #1274's once it merges, or (b) blocking this issue on #1274 merging first, this issue's `pruefer/notice.go` copies `diffTooLargeMarker` and `alreadyNoticedTooLarge` **verbatim** — same name, same signature, same format string — into a new file of the same path. When #1274 merges, git will see two independent adds of `pruefer/notice.go` and conflict, forcing an explicit human reconciliation rather than two marker conventions silently coexisting on the same PR type. This is a deliberate trade: a guaranteed, visible merge conflict is preferable to a subtle behavioral divergence that only shows up as duplicate comments on a real PR.

The notice **body** does not attempt this reuse. #1274's `buildTooLargeNoticeBody` always has a successfully-measured diff to report (`DiffSizeDetail`: bytes, dominant paths, per-file breakdown). This issue's terminal case has neither a diff nor — by definition of reaching this branch — a working file list; GitHub refused both. `buildDiffUnavailableNoticeBody` is therefore a distinct function reporting only the head SHA and a plain statement that GitHub could not enumerate the PR's changes by either mechanism Pruefer has. Forcing the two bodies through one function would have produced a signature mismatch at merge time, not a clean conflict — worse than the file-level duplication above.

### Idempotency check runs immediately before posting, not moved earlier

#1274's branch restructures `ReviewPR` to check `alreadyNoticedTooLarge` *before* `FetchPRDiff`, saving a network round-trip on an already-noticed PR. This issue does not adopt that restructuring: `ReviewPR`'s existing check order is left intact, and the comment fetch for idempotency happens only immediately before `AddComment`, on the already-rare fallback-failure path. This costs a few extra, cheap, deterministic API calls per poll on a permanently-406 PR (diff fetch, files-API fallback, comment fetch) — bounded and non-hot-looping in the sense that matters (no retry storm, no backoff-free error accumulation), and a smaller, lower-risk diff than reordering `ReviewPR`'s control flow. Worth revisiting if/when #1274's restructuring lands and the two paths are reconciled.

## Consequences

**Positive:**
- A PR whose diff GitHub refuses to render is either reviewed (fallback succeeds — the common case, since the fallback endpoint's own undocumented ceiling is far higher than 20,000 lines for realistic file counts) or visibly, terminally skipped exactly once — never hot-retried every poll with nothing to show for it.
- The classification is narrow: `errors.Is(err, github.ErrDiffTooLarge)` requires both status 406 and a specific error code, so no other 4xx/5xx behavior changes.
- The notice marker format is shared with #1274 by construction (verbatim duplication), so the two sibling size paths present identically to a human reading PR comments, and the eventual merge conflict is a known, deliberate reconciliation point rather than a surprise.

**Negative / Trade-offs:**
- Per-poll API cost on a permanently-406 PR rises from 1 call (diff fetch, erroring) to up to 3 (diff fetch, files-API fallback, comment fetch for the notice's idempotency check) before the terminal skip. Still bounded and non-hot-looping; not optimized further here.
- `pruefer/notice.go` is expected to produce an add/add git conflict when #1274 merges, requiring manual reconciliation. This is treated as acceptable and anticipated, not a defect — see the marker-reuse rationale above.
- The files-API fallback has its own undocumented ceiling for very large file counts (GitHub's REST pagination degrades for repositories with thousands of changed files); out of scope for this issue, same as it was for the original `.diff` ceiling. Realistic PRs (hundreds of files) are unaffected — the reproduction case (245 files) is three pages.
- A PR reviewed via the fallback path never gets inline comments, only a body-only review — a strictly worse review UX than a normally-sized PR, but strictly better than "never reviewed at all," which was the prior behavior.

## Related Work

- `adrs/1113-pruefer-v1-architecture.md` §4 (amended by this ADR) and §9 ("on invocation failure, post nothing" — unaffected; this issue's terminal skip is a *disposition* change, not a return to posting a stub).
- `adrs/1274-pruefer-per-file-diff-exclusion-and-too-large-notice.md` — the sibling ADR from `fabrik/issue-1274` (PR #1279), covering the "diff obtained but exceeds `max_diff_bytes`" path. **Not present on `main` as of this ADR** — #1279 was unmerged when this issue landed. This ADR's notice mechanism deliberately shares its marker format; reconcile the two `pruefer/notice.go` additions at merge time rather than treating either as authoritative in isolation.
- `#1274` (issue) — the still-open sibling this issue's R4 was required to coordinate with.

**References:** [cmd/pruefer/README.md](../cmd/pruefer/README.md)
