# ADR 2488: Pruefer ends every review body with a findings-count marker

**Date**: 2026-10-28
**Status**: Accepted
**Issue**: #2488 — a clean review and a review whose findings were all demoted into the body were indistinguishable to the engine
**Builds on**: ADR 2190 (the `PRUEFER_COVERAGE:` line and its "absence means pre-contract" rule), ADR 2307 (skill fallback review), ADR 1456 (graceful-degrade parsing), ADR 1189 (inline versus demoted findings)
**Consumer**: #2489 (engine side, not part of this change). Community report #2453 is mentioned for context only.

## Context

A Pruefer review body is the prose summary, any demoted findings under `## Additional findings (could not anchor to diff)`, and the coverage line, with optional fallback and read-only notices prepended. Inline findings are separate review comments, so the body says nothing about them. Since #1045 the engine treats every non-dismissed Pruefer body as potentially actionable, which costs a worker round on each clean review. The engine needs one machine-readable statement of what the review contained.

## Decision

1. **One hidden marker is the last line of every posted body.**

   ```
   <!-- pruefer:findings inline=<N|unknown> body=<N|unknown> coverage=<complete|partial> -->
   ```

   - Literal prefix `<!-- pruefer:findings `, literal suffix ` -->`, single spaces, keys exactly `inline`, `body`, `coverage` in that order.
   - `N` is a non-negative decimal integer with no sign and no leading zeros (`0` is `0`).
   - The marker is preceded by one blank line and appears exactly once. It is appended as the final step of assembly, after the notices are prepended (`assembleReviewBody`, `pruefer/findings_marker.go`), so "last line" holds by construction.
2. **Counts are what the review submits.** `inline` is the number of inline comments sent to GitHub and `body` the number of demoted findings rendered in the body, both after `dedupeFindings` and `partitionFindings`. They are never taken from the model's prose or its own claimed counts.
3. **Unknown is not zero.** `parseReviewFindings` is a graceful-degrade parser: a missing fence, invalid JSON, a non-array value and a `null` fence all yielded zero findings and a normally posted review. `SummaryParseInfo.FindingsParsed` is true only when the fence unmarshals to a non-nil JSON array (`[]` included). When it is false, **both** counts are the literal `unknown`. Unknown is spelled inside the marker, not by omitting it, because omission already means "a review from before this contract". `coverage` is independent of this and always says `complete` or `partial`.
4. **Coverage comes from the same value as the visible line.** `coverage=` mirrors `Coverage.Complete`, the value that produces `PRUEFER_COVERAGE:`. The free-text reason never appears in the marker.
5. **Look-alikes are neutralised.** Any text matching `(?i)<!--\s*pruefer:findings` in the pre-marker body (summary, demoted `path`/`body`, coverage list, notices) has its opener rewritten to `&lt;!--`. The match is position-independent, not line-anchored, because an HTML comment may start mid-line. Text is kept visible, not deleted. A stray unrelated `<!--` is left alone.
6. **A review that posts nothing posts no marker.** Invocation, clone and submission failures return before a body exists.
7. **Nothing else changes.** No visible text, `PRUEFER_COVERAGE` line, inline comment or review event decision changes. The marker carries no event decision, severity or head SHA.

## Parser contract (for #2489)

A consumer reads the last non-empty line of the body and:

- tolerates extra whitespace around and between tokens;
- ignores additional trailing `key=value` pairs after `coverage` (so the grammar can be extended additively);
- treats any count that is not a non-negative integer as unknown;
- treats absence of the marker as a review from before this contract (unknown; keep today's behaviour);
- treats an unrecognised `coverage` value as partial.

The producer is stricter than the consumer needs to be: it emits only the exact form above. Changing the prefix, key names, key order, the `unknown` spelling or the coverage vocabulary is a contract change and must update this ADR, `pruefer/findings_marker_test.go` (`TestRenderFindingsMarker_Contract`) and the fixtures in `pruefer/testdata/review_body_*.md`.

## Consequences

- A clean review reads `inline=0 body=0 coverage=complete`; the engine can skip a worker round for it once #2489 lands. A review with demoted findings (`body>0`) or unknown counts is still treated as actionable.
- The first ```` ```json ```` fence in the output is the findings fence (unchanged). A quoted json example in the summary can therefore turn a valid review into `unknown`. That is the safe direction and costs only the engine's saving on such a review.
- An unterminated `<!--` in model text could swallow the marker in GitHub's rendering of the body. The stored raw body, which the engine parses, is unaffected, and the marker is hidden either way.

## Assumptions

- An HTML comment renders as nothing on GitHub and is preserved in the stored review body. This is standard GitHub-flavoured markdown behaviour and the engine already relies on hidden `<!-- fabrik:... -->` comments. It was not verified with a live probe.
- Reviews posted before this change carry no marker.
