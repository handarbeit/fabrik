# ADR 2362: Pruefer tells the reviewer to read staged entries in amounts the CLI returns whole

## Status

Accepted. Amends the reading instruction of ADR 2190 (diff coverage contract); builds on ADR 2303 (kept transcripts). ADR 2190 decision 5's rule for crediting a `cat` is unchanged apart from the guard in decision 4 below.

## Context

The reviewer opens the staged diff by batching entries into one shell command (`cat 0001.diff 0002.diff …`, often after `cd <dir>;`). The Claude CLI returns a Bash result whole only below about 30 KB. Above that it saves the output to a file in the session directory and gives the model a `<persisted-output>` notice with the first 2 KB. The reviewer then sees a small slice of the batch.

Transcripts kept under ADR 2303 (dev Pruefer, 2026-10-16) show 22 of 50 partial-coverage transcripts with such results. On #2360 four of 13 tool calls were previews and 22 of 30 entries were unread. A partial review blocks the patch-identity skip of #2230 (its FR-008): 44 of 53 patch-identity checks re-reviewed only for that reason. The coverage check was right; the instruction was wrong.

## Decision

1. **One limit, defined once.** `stagedBatchMaxBytes` (`pruefer/diffstage.go`) is 20 KiB: the most the reviewer is told to put in one `cat`, counted as the sum of the `bytes` column of `INDEX.txt`. It leaves about 10 KB below the CLI threshold, which is unmeasured and may count characters rather than bytes. `renderDynamicContext` formats it through `stagedBatchLimitText()`. The review skill is a static embedded file, so it states the same number as text and a test asserts both contain `stagedBatchLimitText()`.
2. **Prompt and skill.** The Go-owned staging paragraph (always present, whatever the guidance mode) and the review skill both say: one entry per `Read`, or one plain `cat` by absolute path within the limit, no `cd` prefix; a `<persisted-output>` result means nothing in that command was seen and the entries must be reopened in smaller pieces. The skill's version moves to 0.1.1 because its body changed. Absolute paths also make the command one `catEntryIDs` can credit.
3. **No new read permission.** The persisted file stays outside the allowed paths. The instruction is to reopen the entries, not to read it.
4. **Credit guard.** A `tool_result` that is a preview never credits an entry, even a small entry whose whole text fits inside the 2 KB preview. This makes "a preview never credits" literally true. It is the one narrowing of `deriveOpened`; every other crediting rule is unchanged.
5. **Diagnostic.** `analyzeTranscript` (which `deriveOpened` now wraps) also returns the staged entries named by a Bash command whose result is a preview. Detection is structural: the result text, after trimming, must start with `<persisted-output>`; a result that only mentions it is not one, and an error result is not one. Naming uses `stagedEntryIDsNamed`, a looser extractor than `catEntryIDs` (absolute paths and bare `NNNN.diff` names, any shell shape, so `cd <dir>; cat …` is attributed), which attributes only and never credits. `computeCoverage` counts the unread entries in that set (`Coverage.TruncatedUnread`), so an entry later reopened properly is not counted. The `coverage: partial` log line adds `; N of the unread entries were in shell results cut to a preview` when N > 0. The review body and the `PRUEFER_COVERAGE:` line are unchanged. Subagent traffic is counted, as it is for reads.

## Consequences

- Large PRs need more, smaller tool calls; a 20 KiB limit rather than something near 10 KiB keeps that modest. A single entry over the limit (a `long-line` entry) cannot be batched and is read alone, as before.
- The instruction cannot force compliance. The preview-means-unseen rule and the log count are the backstops. Whether previews disappear can only be seen on the dev daemon after merge (a live review of a PR with 30+ entries should have no `<persisted-output>` in its transcript).
- Only `Bash` is handled; `Read` truncates in place and does not persist.
- The preview notice format is CLI behaviour not pinned in this repo. `testdata/persisted_output_cat.jsonl` is built from the notice shape seen in saved transcripts, not captured from a live run; detection depends only on the prefix.

## Rejected

- Extending `catEntryIDs` to recognise `cd …; cat …`: it would change what counts as opened.
- Templating the number into the skill: the skill is a static embedded file; a test binds the two instead.
- Granting `Read` on the persisted file: widens the reviewer's read scope (ADR 2192).
