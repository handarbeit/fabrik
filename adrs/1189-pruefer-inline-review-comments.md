# ADR 1189: Pruefer Inline Review Comments

**Date**: 2026-07-27
**Status**: Accepted
**Issue**: #1189 — post findings as inline review thread comments so Fabrik's review-reinvoke can consume them

## Context

Pruefer V1 (ADR-1113) submitted its entire review as a single top-level `body` on the `pull_request_review`, deliberately scoping inline comments as "desirable but optional" to ship. That left a gap: Fabrik's review-reinvoke path (`buildReviewThreadComments`, `engine/reviews.go`) reads only line-anchored review comments, never the top-level body. A Pruefer review satisfied `wait_for_reviews: true` but produced zero auto-fix work — findings existed only as prose a human had to relay by hand (observed live on PR #1185).

This issue closes that gap: `SubmitPRReview` now accepts an optional `comments[]` array alongside `body` in the same request, the review prompt emits structured findings instead of free prose, and findings are mapped onto diff-validated anchors before submission.

## Decisions

### 1. Two-part output contract: prose summary, then a fenced ```json findings array

`buildReviewPrompt` (`pruefer/claude.go`) instructs Claude to produce free-form prose first (the only text GitHub surfaces outside inline comments — it must stand on its own as an overall assessment), followed by exactly one fenced ` ```json ` code block containing a JSON array of `{"path", "line", "body"}` objects, one per finding. `path` must match the diff exactly; `line` must be a line visible in the new (post-change) file version.

A fenced JSON block was chosen over markdown headings or another prose convention because it parses reliably with a single non-greedy regex plus `encoding/json` — no heuristic section-splitting, no ambiguity about where one finding ends and the next begins. This is new territory for the codebase (no prior structured-output contract exists for a Claude invocation anywhere in Fabrik or Pruefer), so the format was picked for parse reliability over prompt elegance.

### 2. No fenced block, or malformed JSON inside it, degrades to whole-text-as-summary — never an error

`parseReviewFindings` (`pruefer/findings.go`) is intentionally forgiving: if the fence is absent or its content doesn't unmarshal as `[]ReviewFinding`, the entire input becomes the summary and zero findings are extracted. This is not a parse-failure error path — it is the same body-only review Pruefer already submitted before this issue, so it costs nothing beyond "no inline comments this round."

This also settles a question ADR-1113's Decision 9 ("on invocation failure, post nothing") left open: a *successful* Claude invocation that didn't follow the new contract is not treated the same as an invocation failure. Decision 9 is reserved for genuine invocation failures (process crash, empty/error output) — a review that came back but wasn't structured the way the prompt asked still gets posted, just without inline comments. Never let an imperfect (or absent) findings block sink the review.

### 3. Anchor validation runs client-side, before submission — never after a 422

GitHub's `POST /pulls/{n}/reviews` rejects the **entire review** if any single entry in `comments[]` has a `path`/`line` that isn't a valid position in the diff. There is no partial-success response and no structured error type in `github/client.go` that would let a caller distinguish a 422 from any other 4xx after the fact (`restPostWithResponse` surfaces every non-2xx as the same formatted error string). Retrying-after-422 was never a viable design — the only way to guarantee "the review submits" is to never send an invalid anchor in the first place.

`validRightAnchors` (`pruefer/diffanchor.go`) parses the PR's unified diff (already fetched by `ReviewPR` for the size guard — no second `FetchPRDiff` call) into the set of valid RIGHT-side `(path, line)` positions: every context or added line inside a hunk, keyed off each hunk's `+++ b/<path>` header so renamed files resolve to their destination path. Deletion-only lines, lines outside any hunk, and deleted files (`+++ /dev/null`) produce no anchors.

### 4. Unanchorable findings demote into the body, under an explicit heading — never dropped, never silent

`partitionFindings` splits findings into `comments` (valid anchor → `gh.ReviewComment`) and `demoted` (everything else). `buildReviewBody` appends demoted findings to the prose summary under a `## Additional findings (could not anchor to diff)` heading, each rendered as `**path:line**: body`.

The heading matters: a demoted finding that read identically to ambient summary prose would be just as opaque to `buildReviewThreadComments` as today's all-prose reviews, defeating the point of distinguishing it at all. It's still unconsumable by the auto-fix loop either way — that's inherent to demotion, not a bug — but a human reading the review body should be able to tell "this was a specific finding Pruefer couldn't place" from "this is the overall verdict."

### 5. `side` is hardcoded to `"RIGHT"` in `SubmitPRReview`, mirroring the existing hardcoded `event`

`ReviewComment` (`github/types.go`) has no `Side` field. `SubmitPRReview` (`github/prs.go`) sets `"side": "RIGHT"` on every wire-level comment entry unconditionally — single-line, post-change-side anchors are the only shape V1 supports (`start_line` ranges and `LEFT`-side/base-diff anchors are out of scope). This is the same structural-invariant pattern ADR-1113 Decision 8 used for `event: "COMMENT"`: not caller-controllable, and locked down by a dedicated test (`TestSubmitPRReview_HardcodesRightSide`) the same way `TestSubmitPRReview_HardcodesCommentEvent` locks down `event`.

### 6. `comments` is omitted from the request body when empty, not sent as `[]`

When there are no anchorable findings — including today's plain-prose case — `SubmitPRReview`'s request body has no `comments` key at all, preserving the exact wire shape callers relied on before this parameter existed. `TestSubmitPRReview_HardcodesCommentEvent`'s assertions needed no changes beyond its call site's new (nil-comments) argument.

### 7. Anchor validation lives in `pruefer`, not `github`

`validRightAnchors` and `partitionFindings` are Pruefer-specific diff-domain logic, not generic REST client behavior — `github` stays a thin client with no knowledge of diff formats. This mirrors `ParseChangedPaths` (`pruefer/select.go`, file-level diff parsing already living in `pruefer`) and keeps consistent with ADR-1113 Decision 6's "no shared imports between `pruefer` and `engine`" boundary, extended here to keep `github` free of diff-domain logic too.

## Consequences

**Positive:**
- Pruefer's findings are now consumable by Fabrik's existing review-reinvoke path with zero changes to the consumer side (`buildReviewThreadComments` already reads `gh.Comment.Path`/`.Line`) — the circuit Pruefer → Fabrik auto-fix → Pruefer re-review closes.
- The 422-all-or-nothing risk is structurally eliminated, not mitigated: an unanchorable finding can never reach the wire, so it can never take down the rest of the review.
- Backward compatible by construction: a plain-prose Claude response (today's behavior, or the fallback when Claude doesn't follow the new contract) produces zero findings and an unmodified body-only review — no special-case branch needed.

**Negative / Trade-offs:**
- The `path`/`line` findings contract is a new prompt-engineering surface with no runtime guarantee Claude follows it — degrades gracefully to zero inline comments, but under-delivers silently rather than erroring. Not fully verifiable by unit tests; the issue's own "Verification worth doing by hand" (a live Pruefer review on a `fabrik:yolo` PR) is the real check.
- Off-by-one errors in hunk-line-counting are the sharpest correctness risk: a bug here produces a *wrong* anchor (attaches to the wrong line) rather than a missing one, which "does it 422" tests alone wouldn't catch — mitigated by dedicated line-number-correctness tests against hand-authored synthetic diffs.
- Multi-line (`start_line`) ranges remain unsupported; a finding that genuinely spans several lines is still reported as a single-line anchor or demoted.

## Related Work

- ADR-1113 (`adrs/1113-pruefer-v1-architecture.md`) — Decision 8 scoped inline comments as "desirable but optional" for V1; this issue is the explicitly anticipated follow-up. Decision 9 ("on invocation failure, post nothing") is extended here to distinguish invocation failure from successful-but-unstructured output (Decision 2 above).
- `engine/reviews.go` (`buildReviewThreadComments`) — the consumer this issue feeds; explicitly out of scope to change, since it already reads the `gh.Comment.Path`/`.Line` shape this issue's `comments[]` produces.
- `github/pruefer_extensions_test.go` (`TestSubmitPRReview_HardcodesCommentEvent`) — the pre-existing invariant this issue's signature change had to preserve unmodified.
- PR #1185 — the live review that demonstrated the gap this issue closes.

**References:** [cmd/pruefer/README.md](../cmd/pruefer/README.md)
