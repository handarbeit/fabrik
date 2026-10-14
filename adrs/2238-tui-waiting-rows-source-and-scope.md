# ADR 2238: TUI waiting rows — source, scope and header rule

## Status

Accepted.

## Context

The In Progress panel listed running workers and dependency-blocked items. An item the engine was
holding on anything else — CI, reviews, a retry, a rebase, the merge train, a human reply — did not
appear, and the header read `[poll] nothing to do`. To an operator that looks like a forgotten item.
The engine already classifies every item (`internal/attention`, ADR 1966-a) and writes a one-line
status to a board field (ADR 2048).

## Decision

1. **A sampled snapshot, not hooks and not a pull closure.** Waits arise in many places (labels,
   cooldowns, retries, the train, pauses); a hook per site would miss some and drift. A pull closure
   would change `tui.New`'s signature and put engine state inside the model. `runWaitingPublisher`
   follows ADR 2115's slot publisher: it samples the Store once per second and sends a
   full-replacement `tui.WaitingItemsEvent` through `emitStructural` only when it changes, so rows
   appear and disappear with no bookkeeping.
2. **No new detection and no GitHub calls.** Classification is the shared `attentionInput` →
   `attention.Classify` path. The publisher reads the Store with `Scan` only; a static test forbids
   the GitHub-capable identifiers the local API's scan forbids.
3. **Row text is built from the classifier's code plus cache-only facts, in a pure function.** The
   status-line text is not a source: ADR 2048 forbids reading it back, even from the writer's own
   record, and that record is empty when the field is `off`. `Result.Summary` is a long sentence that
   changes while the poll is overdue, so it is only the fallback for codes without tailored wording.
   `internal/attention` is unchanged. An unknown fact is omitted, never guessed.
4. **Scope.** Shown: `waiting`, `needs-human`, `escalated`, `suspended`. Not shown: `working` (own
   row), `idle`, `stalled` and `ci:red` (surfaced by stall events and health), the between-polls
   `waiting-for-dispatch`/`queued-for-slot` states (normal for any item, no operator value) and
   `fabrik:blocked` items (they keep the `IssueBlockedEvent` row).
5. **Order and cap.** `✋` rows (human) first by rank; `⏳` rows are all unranked, so ranking them
   would place them first — they are ordered by earliest known deadline, then longest-waiting, then
   ref. The engine caps at 5 rows and reports the rest as `Omitted`.
6. **Header.** The wait count is separate header state (the next poll-level log line overwrites
   `statusLine`). The exact status `[poll] nothing to do` renders as `waiting on N item(s)[, M need
   you]` while waits are open; the engine's log line is unchanged, and a test pins the wording
   coupling.

## Consequences

- The stale `🔒` quirk (a blocked row is cleared only when a worker starts for the item) is not
  changed here. Folding blocked items into the snapshot would fix it and is a follow-up.
- A reviewer who was never formally requested (self-submitting bots such as Pruefer) is not named;
  the row reads `review (since …)`.
- Per-second `Classify` over the open board is O(board) but allocation-light and runs only with a TUI.
