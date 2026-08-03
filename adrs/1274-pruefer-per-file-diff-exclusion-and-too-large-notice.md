# ADR 1274: Pruefer per-file diff exclusion and a visible too-large notice (amends ADR-1113 §Decision 4)

**Status:** Accepted
**Date:** 2026-07-31
**Issue:** [#1274](https://github.com/handarbeit/fabrik/issues/1274)

## Context

`shadoworg/fantasy#1640` had a 17 MB diff, 99.5% of which was a single JSONL
eval corpus file; the other 27 files (the actual code change, ~85 KB) never
got reviewed. Pruefer's own logs showed it evaluating and skipping the same
head SHA 254 times over ~26 hours, and posted nothing to the PR — from
GitHub's side the PR simply never got a verdict, indistinguishable from
"still reviewing."

Three compounding defects, all in `pruefer/review.go` and `pruefer/select.go`:

1. **Gate ordering.** The whole-diff size check (`len(diff) > MaxDiffBytes`)
   ran *before* the exclusion check. Adding `packages/evals/corpus/**` to
   `excluded_paths` would not have fixed this PR — the size gate short-
   circuited first, making the operator's escape hatch dead code in exactly
   the scenario it exists for.
2. **All-or-nothing exclusion.** `allPathsExcluded` only fired when *every*
   touched path matched an exclusion glob. There was no way to reduce a diff
   to its reviewable subset by configuration alone — one non-excluded file
   among many excluded ones defeated the whole mechanism. Separately,
   `EligibilityInput`'s own `ExcludedPaths`/`ChangedPaths` fields were never
   populated by `ReviewPR` at all — a second, independent instance of "config
   that can never take effect."
3. **Silent skip.** No comment, no check run, nothing but a local log line.
   This became load-bearing once ADR-1250 landed review authority: a
   silently-skipped PR is indistinguishable from "no verdict yet," so
   `review_authority: authoritative` gates could stall on a PR that will
   never be reviewed, with no visible signal telling anyone why.

ADR-1113 §Decision 4 established the size guard's existence and its
skip-not-truncate policy — both correct and unchanged here. This ADR amends
*how* that guard measures and reports, not whether it exists.

## Decision

### Exclude-then-measure, per file, not whole-diff

`pruefer/diffsplit.go` (new) parses a fetched diff into per-file blocks
(`diffFile{Path, Body, Bytes}`), keyed by the b/-side (post-change) path —
consistent with `ParseChangedPaths`'s existing convention, so renames key by
destination path the same way exclusion globs already expect. Any diff bytes
before the first `diff --git` header (a malformed or headerless diff) are
kept separately as "preamble" and can never be excluded or dropped — an
unparseable diff must never silently vanish from the size accounting.

`filterExcludedPaths` replaces `allPathsExcluded`: it partitions files into
kept/dropped per file, rather than deciding all-or-nothing across the whole
diff. `ReviewPR` now filters `cfg.ExcludedPaths` out **before** measuring
against `cfg.MaxDiffBytes`, closing defect #1. "Every touched file excluded"
now falls out naturally as `len(kept) == 0` — no test scenario from the old
all-or-nothing check is lost, only relocated to `diffsplit_test.go`.

`EligibilityInput`'s dead `ExcludedPaths`/`ChangedPaths` fields and their
branch in `Eligible` are removed rather than wired up. Path exclusion
fundamentally needs per-file diff structure that only exists after
`FetchPRDiff`; `Eligible` is deliberately the cheap, pre-diff-fetch check
(draft/self-authored/excluded-author/label/already-reviewed) and stays that
way. Keeping a second, wired-up exclusion mechanism inside `Eligible` would
just reintroduce defect #2's class of bug in a new place.

### Best-effort partial review (FR-5)

If exclusion alone still leaves the diff over `MaxDiffBytes`, `trimToFit`
(also `diffsplit.go`) greedily drops the largest remaining file(s), by raw
diff bytes, until the rest fits. `fits` is true only when at least one file
survives *and* the total then fits — dropping every file to satisfy the cap
isn't "reviewed the remainder," it's nothing to review, so that case falls
through to the notice-and-skip path below rather than being reported as a
success.

**Claude must be told what was dropped, not just Go.** Claude does not
consume the Go-fetched diff text — `buildReviewPrompt` (`pruefer/claude.go`)
already tells it to run `git diff <base>...HEAD` itself in the cloned
worktree (ADR-1113 §Decision 4's "clone, don't paste the diff into the
prompt" design). `ReviewRequest` gains `ExcludedPaths []string`; when
non-empty, the prompt adds an "Out of scope for this review" section naming
each path and instructing Claude to exclude it via a `git diff` pathspec
(`':(exclude)<path>'`) and to note the omission in its own summary. Without
this, a Go-side-only fix would still let Claude's own `git diff` ingest the
oversized file and exhaust its context — the same failure mode the issue's
"why not just raise `max_diff_bytes`" scope note rules out for a different
reason. `buildReviewBody` (`pruefer/findings.go`) gains an additive-only
`omittedPaths []string` parameter that renders an "## Omitted from this
review" section — empty/nil produces byte-identical output to before, so
every prior exact-string test assertion holds unchanged.

### A visible, idempotent too-large notice (FR-2/FR-3/FR-4)

When nothing fits — exclusion and trimming both fail to bring the diff under
cap — `ReviewPR` posts a PR comment (`pruefer/notice.go`'s
`buildTooLargeNoticeBody`) stating the measured size, the cap, which paths
were already excluded by config (if any), and the largest remaining
contributors (capped to the top 5 — `dominantPathsLimit` — so the notice
stays actionable even when many small files, not one huge one, are the
cause), plus concrete guidance: add the named path(s) to `excluded_paths`, or
split the PR.

**Idempotency follows the same convention as `alreadyReviewedAtHead`, and the
codebase's one existing precedent for it: `engine/merge_train.go`'s
`mergeTrainBatchMarker`.** The notice body embeds an HTML-comment marker,
`<!-- pruefer:diff-too-large:<headSHA> -->`, keyed per head SHA exactly like
the marker Fabrik's engine already uses to recognize a posted integration PR
across restarts. `alreadyNoticedTooLarge` scans `FetchIssueComments` output
for that marker before `FetchPRDiff` is ever called — a new pre-diff-fetch
cheap check, placed alongside (but after) `Eligible`'s existing cheap checks.
This is deliberate, not incidental: per ADR-1113 §Decision 2, Pruefer derives
all "already done X" facts from GitHub itself, never from local process
memory or disk state, and this is the first time that convention has needed
restating for a *notice* rather than a *review*. No new ADR precedent was
needed beyond citing `engine/merge_train.go`'s — but this is the first time
the pattern appears inside `pruefer`, worth recording here for the next
Pruefer feature that needs "acted once per state, durably, without local
storage."

Checking the marker before `FetchPRDiff` is also FR-4's entire mechanism:
once a SHA is known too large, the next poll recognizes the existing notice
and returns `Skipped`/`SkipDiffTooLarge` without re-fetching or re-measuring
anything — closing the 254-evaluations-in-26-hours waste structurally, not
via a retry counter or backoff timer. `ReviewOutcome` gains
`SizeDetail *DiffSizeDetail`, set only on a *fresh* measurement (not the
pre-fetch "already noticed" skip, which returns before any measurement
exists) — mirroring the notice's own content for callers that want it
without re-parsing the posted comment.

`SkipDiffTooLarge` is reused as the `SkipReason` for both the fresh-measurement
skip and the already-noticed skip, rather than adding a new value — this
keeps every existing `==`-based test assertion against `SkipReason` valid,
and both cases satisfy the same acceptance property: "every `Skipped: true`
outcome is either visible on the PR or provably benign." A repeat skip after
the notice already exists is provably benign precisely because the visible
notice is what made it provable.

### One up-front comment fetch, not two

`ReviewPR` now fetches `FetchIssueComments` exactly once per invocation and
derives both `forceReview` (via a new pure `pendingReviewComments` helper,
extracted from `unprocessedReviewCommands`) and `alreadyNoticedTooLarge` from
that single list — rather than issuing a second API round-trip for the new
check. `GitHubCommenter` gains `AddComment(owner, repo string, issueNumber
int, body string) (int, error)`, backed by the already-existing
`github.Client.AddComment` (no new GitHub-side code); this is additive to the
interface every implementer (`*github.Client`, `fakeCommenter` in tests) must
now satisfy.

## Consequences

**Positive:**
- The `#1640` acceptance case now works exactly as the operator would
  expect: adding `packages/evals/corpus/**` to `excluded_paths` gets the 27
  code files reviewed, with the corpus file reported as omitted.
- Even with *no* exclusion configured, FR-5's auto-trim reviews the
  reviewable remainder of an oversized PR in the common case (one dominant
  offender), rather than reviewing nothing.
- No more silent skips: an unreviewable PR always carries a comment
  explaining why, satisfying `review_authority: authoritative`'s need to
  distinguish "no verdict yet" from "cannot be reviewed."
- 254-evaluations-in-26-hours-style waste is now structurally bounded by the
  marker check running before `FetchPRDiff`, not by a new retry/backoff
  mechanism that could itself have edge cases.

**Negative / Trade-offs:**
- A comment-fetch failure at the very top of `ReviewPR` degrades
  `alreadyNoticedTooLarge` to "not noticed" (fail-open, matching
  `PendingForceReview`'s existing behavior) — a transient fetch failure
  immediately after a successful notice post could cause one duplicate
  notice. Self-correcting once the fetch succeeds again, and strictly better
  than the fully-silent status quo.
- Claude's read-only tool allowlist (`Read`, `Grep`, `Glob`,
  `Bash(git diff/log/show/blame/grep/status:*)`) does not structurally block
  it from reading an excluded path via `Read`/`Grep` despite the prompt
  instruction — this is a pre-existing class of prompt-injection-resistance
  limit (severity/event decisions are already kept Go-side for the same
  reason) and isn't closed further here. In practice, `Read`/`Grep` touching
  a single huge JSONL line is far less likely to exhaust context than a
  wholesale `git diff` would.
- `trimToFit`'s greedy largest-first heuristic is simple, not optimal — it
  doesn't attempt to keep a smaller total set of files under cap when a
  different combination might fit more code. Sufficient for the motivating
  case (one dominant offender) and any future need for smarter selection is
  separable follow-up.
- **Known, tracked gap: git's C-quoted diff header form is unhandled.**
  `diffsplit.go`'s file-boundary and path-resolution regexes all match git's
  unquoted `diff --git a/<path> b/<path>` header. Under the default
  `core.quotepath=true`, a touched file whose path contains a space, double
  quote, backslash, or non-ASCII byte makes git emit a C-quoted header
  instead (`diff --git "a/foo bar" "b/foo bar"`), which none of those
  regexes match. That file's entire block — headers, hunks, everything —
  falls into `splitDiffFiles`'s `preamble`, which per its own contract can
  never be excluded via `excluded_paths` or auto-dropped by `trimToFit`: an
  operator who adds such a path to `excluded_paths` gets no protection from
  it, and its bytes still count toward `MaxDiffBytes` under a `DominantPaths`
  list that never names the actual file. This is the same silent-signal
  failure class this ADR exists to close, reached through a different diff
  shape — accepted as a follow-up rather than closed here (see
  `diffFileHeaderLineRE`'s doc comment and the "Known limitations" note in
  `cmd/pruefer/README.md`).

## Related Work

- `adrs/1113-pruefer-v1-architecture.md` §Decision 2 (GitHub-derived state,
  no local persistence — extended, not reversed, by the notice marker) and
  §Decision 4 (size guard exists, skip-not-truncate — extended to measure
  post-exclusion and attempt a best-effort trim before skipping).
- `adrs/1251-pruefer-severity-gated-request-changes.md` — the precedent for
  computing a decision deterministically in Go from parsed structure, never
  from Claude's prose; `buildDiffSizeDetail`'s dominant-paths computation
  follows the same discipline.
- `adrs/1250-review-authority-orthogonal-to-autonomy.md` — the reason a
  silent skip became load-bearing rather than merely unfortunate; this ADR's
  visible-notice requirement exists to keep that gate from stalling
  invisibly.
- `engine/merge_train.go`'s `mergeTrainBatchMarker` — the prior art this
  ADR's notice-idempotency marker follows, now with a second, independent
  implementation in `pruefer`.

**References:** [cmd/pruefer/README.md](../cmd/pruefer/README.md)
