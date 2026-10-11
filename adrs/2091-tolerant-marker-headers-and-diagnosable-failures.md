# ADR 2091: Tolerant marker headers and diagnosable marker failures

## Status

Accepted

## Context

Report #2055: `FABRIK_PR_CREATE` required the literal `TITLE:` header, so a model writing `title:` failed the parse. The error ("missing TITLE line") did not show what was found, and `finalizeStageOutcome` paused and returned *before* pushing the stage's work, so un-pausing re-ran a finished Implement. `FABRIK_SPAWN_CHILD` had a duplicated strict parser and dropped a malformed block silently — the child was simply never created.

## Decision

1. **One shared header helper** (`engine/marker_header.go`): `matchHeader(line, key)` splits at the first colon, trims, and compares the key with `strings.EqualFold`. The caller names the key it expects, so prose such as `Summary: …` is still rejected. The grammar is otherwise unchanged (delimiters, keys, the title/body split, `DEPENDS_ON` on the line directly after `TITLE`).
2. **Errors quote what was found.** `quoteFoundLine` wraps `sanitizeToolsDeniedCommand` (newline collapse, backtick replacement, truncation) and additionally neutralises control characters and `@` (zero-width space) so quoted model output cannot ping accounts. The existing function is untouched.
3. **Malformed spawn blocks are surfaced and the whole declaration is held.** `ParseSpawnBlocksDetailed` returns `[]SpawnBlockError` beside the blocks; `ParseSpawnBlocks` stays as a wrapper. Any error → `pauseForMalformedSpawn` (log + `🏭` comment naming the 1-based block index and quoted line + `fabrik:paused`), and no child is created. Dropping the bad block and spawning siblings was rejected: it would shift `DEPENDS_ON` indexes and ADR-1583's `fabrik:spawned-child:<index>:<n>` markers, and leave `blockedBy` half-populated. This also means the error index (all BEGIN/END pairs) and the `spawnChildren` index (well-formed list) never coexist.
4. **Plan-origin pause site is `preImplement`/recovery.** Pausing in Plan's own `finalizeStageOutcome` would leave the stage incomplete with no comment posted. Plan instead gets a non-pausing warning line from `formatSpawnReceiptNote` so the problem is visible when the Plan posts. Because the stored Plan comment is immutable, a bare un-pause re-pauses until the Plan is edited; the pause comment says so. Review/Validate regenerate fresh output on retry, so they do not loop.
5. **Commit and push before the pause (R4).** `preserveWorkOnMarkerFailure` (`commitWIP` then `pushBranchOnCancel`) runs in the `FABRIK_PR_CREATE` parse-failure branch and the Review/Validate malformed-spawn branch, following the R8/ADR-1393 cancellation precedent. `pushBranchOnCancel` is used because it never pauses, so the zero-ahead guard (ADR-2089) cannot add a second pause. Failures are logged only. A parse failure means the marker path creates no PR, so the push-before-PR-creation sequencing is unaffected; the successful-parse path is unchanged. The inline structure keeps the existing early return, so `StageAttempted`/`InvocationRecorded` remain unrecorded on that path (no dispatch cooldown delays the re-run after un-pause); making that bookkeeping uniform is out of scope.

## Consequences

- `title:`, `Title:` and ` TITLE :` work for both markers.
- A `FABRIK_PR_CREATE` parse failure leaves the stage's work on the remote branch.
- Known remaining silent case: a `FABRIK_SPAWN_CHILD_BEGIN` with no matching END still produces only the existing log line (out of scope).
- Review/Validate no longer meet a malformed block in `stripSpawnBlocks`; its leave-visible branch is defensive only.
