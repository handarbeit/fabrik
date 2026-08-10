# ADR 1456: Pruefer review summary delimiter contract

**Status:** Accepted
**Date:** 2026-08-09
**Issue:** [#1456](https://github.com/handarbeit/fabrik/issues/1456)

## Context

`parseReviewFindings` (`pruefer/findings.go`) derived a review's prose summary purely positionally: everything in Claude's output before the fenced ` ```json ` findings block. `buildReviewPrompt` (`pruefer/claude.go`) asked for "no preamble, no meta-commentary" in prose, but that instruction was unenforced — nothing in the output shape gave the parser a way to distinguish "the model's summary" from "the model thinking out loud." Observed live on handarbeit/fabrik#1444: the posted review opened with a dangling mid-investigation sentence ("That file exists, so the reference is valid. No issues found there either.") that had no antecedent for a human reader, with the real summary beginning two lines later under a `## Review Summary` heading the model added on its own.

Today this is cosmetic — Fabrik's review-reinvoke path ignores `COMMENTED` review bodies and reads only `Severity` from parsed findings (`decideEvent`), never `Body` or the prose summary. #1045 changes that: once bot review feedback delivered as a plain comment is gated on and auto-addressed, a garbled summary becomes input a stage reinvocation could act on as if it were reviewer instruction. #1045 is currently blocked and not imminent, but this issue was deliberately sequenced ahead of it.

Three open issues touch `buildReviewPrompt` concurrently: this one, #1446 (splits the function into an overridable guidance half and a Go-owned contract half — `blockedBy` this issue, since the markers added here belong in its eventual contract half), and #1497 (prior-review-thread context, already merged by the time this issue implemented — its own ADR confirms no conflict, since it only touches the context preamble, not the output-contract tail this issue changes).

## Decision

Add an explicit `PRUEFER_SUMMARY_BEGIN`/`PRUEFER_SUMMARY_END` marker pair to the output contract. `buildReviewPrompt` instructs the model to wrap its prose summary in these markers, each alone on its own line, with an inline example, and states explicitly that nothing may precede the opening marker. `parseReviewFindings` extracts the summary from between a well-formed pair when present; when absent or malformed, it falls back byte-for-byte to the original positional behavior (everything before the findings fence).

**Marker naming: `PRUEFER_SUMMARY_BEGIN`/`PRUEFER_SUMMARY_END`, not Fabrik's literal `FABRIK_SUMMARY_BEGIN`/`END` tokens.** There is no actual collision risk — Fabrik's `extractSummary` (`engine/claude.go`) only scans Fabrik stage output, never a PR review body — but a distinct prefix keeps the two unrelated products' marker vocabularies unambiguous to a reader grepping logs or prompts across both.

**Line-anchored regex matching** (`(?m)^PRUEFER_SUMMARY_BEGIN\r?$` / `^PRUEFER_SUMMARY_END\r?$`), mirroring `engine/claude.go`'s `stageCompleteRE` convention for control tokens, rather than a bare substring search. This is more robust against the marker text appearing incidentally inside narration prose — a marker must occupy its own line to count.

**`parseReviewFindings` stays pure; logging happens at the `review.go` call site.** The function gains a third return value, `SummaryParseInfo{MarkersFound bool, DiscardedBytes int}`, rather than taking a `prNumber int` parameter and calling `logf` directly. The rejected alternative would have made `parseReviewFindings` the only side-effecting function in `findings.go`, breaking the file's existing pure/directly-unit-testable convention; `review.go`'s call site already has `pr.Number` in scope to log against once `SummaryParseInfo` is returned, so no additional threading is needed. `logSummaryParseInfo` (`pruefer/review.go`) implements R4: it logs a `warn`-tagged line when markers were entirely absent or malformed (full positional fallback used), and separately when a well-formed pair was found but non-empty preamble was discarded ahead of `PRUEFER_SUMMARY_BEGIN` — both are compliance-drift signals distinct from the happy path (markers found, zero-byte preamble), which logs nothing.

**Discarded-byte accounting is whitespace-trimmed**, not a raw byte count: `DiscardedBytes` is `len(strings.TrimSpace(preamble))`. A preamble consisting only of trailing whitespace before the marker is not real compliance drift and must not trigger the R4 log line or break the happy-path silence.

**A malformed marker pair — BEGIN present with no END, END present with no BEGIN, or END appearing before BEGIN — is treated identically to "markers absent"**, not as a partial-extraction case. `splitSummaryMarkers` searches for END only in the text *after* BEGIN's own offset, so an END that precedes BEGIN is invisible to that search and the pair is correctly reported as not found. This keeps the fallback logic binary (found-and-well-formed vs. not) rather than inventing a third, harder-to-test parse state, and honors R3's "never fail to submit" contract unconditionally.

**Findings-fence extraction is untouched and stays independent of marker extraction.** `splitSummaryMarkers` and `findingsFenceRE`'s match run as two separate regex searches over the same input text; whether the summary came from markers or position has no bearing on whether or how the JSON fence is parsed. This is what makes AC4 (`decideEvent`'s verdict, driven solely by parsed `Severity`, unaffected by this change) structurally guaranteed rather than something the implementation could accidentally break — confirmed by `TestDecideEvent_UnaffectedBySummaryMarkers`.

## Consequences

**Positive:**
- A review's posted summary can no longer include the model's pre-summary narration when the model follows the new contract — the #1444 defect's root cause (no boundary for the parser to find) is closed.
- `parseReviewFindings`'s existing corpus (`findings_test.go`, all no-marker fixtures) passes byte-identically unmodified, proving the fallback path is truly unchanged (AC2).
- The R4 log line surfaces prompt-compliance drift (missing markers, or markers present but still preceded by narration) in `.pruefer/pruefer.log`, the same signal that would have surfaced #1444 without a human noticing the odd wording.
- The change is additive-only to a pure parsing function and a prompt-string builder; `decideEvent`/`partitionFindings` are structurally insulated from anything this issue touches.

**Negative / Trade-offs:**
- The output contract now depends on the model reliably reproducing two exact marker strings; a model that renames or reformats them (e.g. wraps them in backticks) falls through to the fallback path rather than being recognized — acceptable per R3, but means the fix's benefit is conditional on prompt compliance, not a hard guarantee.
- Widening `parseReviewFindings`'s signature from two to three return values touched all 6 existing call sites in `findings_test.go` plus the one in `review.go` — mechanical, but any future caller must remember the third value exists.
- `#1446`'s eventual split of `buildReviewPrompt` into overridable-guidance and Go-owned-contract halves must place the new marker instruction in the contract half — deferred to that issue, which is `blockedBy` this one specifically so it starts after this contract exists.

**Nested-fence safety net (added during PR review, PR #1511):** `buildReviewPrompt` asks the model to place the findings fence after `PRUEFER_SUMMARY_END`, but that's an unenforced prompt request — the same shape of problem this whole feature exists to close. If the model nests the fence between `BEGIN`/`END` anyway, `splitSummaryMarkers` now truncates the delimited text at the fence's start before returning it, mirroring `parseReviewFindings`'s own positional `text[:loc[0]]` behavior. This is not the R5 "heuristic trimming" R5 warns against — R5 protects legitimate summary prose from being guessed away, and a raw findings array was never summary prose. Both a prompt clarification (explicit "fence comes after END, never between the markers" instruction) and this code-level safety net were added, following the same defense-in-depth pattern already used for the "no preamble" instruction — the prompt reduces the odds of the model doing this, the code guarantees it can never leak the JSON into the summary even if the model does it anyway.

## Related Work

- `adrs/1113-pruefer-v1-architecture.md` — establishes Pruefer as an independent sibling of the engine (own `logf`, own marker/parsing conventions, no shared code with `engine/claude.go`); this issue's marker design deliberately does not reuse `engine/claude.go`'s `extractBetweenMarkers` (`engine/claude.go:1920`), whose all-or-nothing semantics (empty string if either marker is missing) is the opposite of R3's fail-open requirement.
- `adrs/1251-pruefer-severity-gated-request-changes.md` — confirms `decideEvent` reads only parsed `Severity`, never prose; this issue's summary-parsing change cannot affect `REQUEST_CHANGES` decisions by construction.
- `adrs/1497-pruefer-prior-review-thread-context.md` — already merged; its own text declares no conflict with this issue (context preamble vs. output-contract tail).
- `#1446` (repo-resident review skill, `blockedBy` this issue) — will split `buildReviewPrompt`; the markers added here belong in its eventual Go-owned contract half.

**References:** [cmd/pruefer/README.md](../cmd/pruefer/README.md)
