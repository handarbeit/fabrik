# ADR 2218: An absent display field is reported as absent, not as a lookup failure

**Status:** Accepted
**Date:** 2026-10-13
**Issue:** [#2218](https://github.com/handarbeit/fabrik/issues/2218)
**Amends:** ADR 2135 (audit disposition of the display-field lookup), ADR 2048 / 2049 (the "unavailable" startup line)

## Context

`fetchProjectFieldID` (`github/textfield.go`) treated a `null` `ProjectV2.field(name:)` as an absent
field. GitHub does not return `null` for a missing name; it returns a GraphQL error, `Could not resolve
to a Unions::ProjectV2FieldConfiguration with the name '<name>'`. On a board without the three
display fields (`Fabrik`, `Last activity`, `Last run`) every start therefore logged three
`[startup] warning: could not look up …` lines and retried each lookup every `statusLineLookupRetry`
for the life of the daemon. The lines were written through `logf`, so neither the warnings panel nor the
scan test (`TestOperatorWarningsReachThePanel`) saw them — and even once classified as absent, nothing
told the operator that fields they had enabled would never be written.

## Decision

1. **Classify in `github`.** `graphqlRequest` returns a typed `*GraphQLError{Message, Path}` whose text is
   unchanged (`GraphQL error: <message>`), so every existing message-matching classifier keeps working.
   `fetchProjectFieldID` treats an error as *absent* only when the message carries the exact
   `Could not resolve to a Unions::ProjectV2FieldConfiguration with the name` shape **and** the path, when
   present, ends in `field`. A bad project id (`… with the global id of …`), `boom`, a 5xx and transport
   errors stay errors. The absent path then runs unchanged: one `unavailable` line, no retry.
2. **Same-named field of another type is a typed error.** `fetchProjectFieldID` returns
   `*FieldTypeMismatchError` for a field of another `dataType` and for the empty object the
   `ProjectV2Field` fragment gives a single-select or iteration field. Keeping this inside the one query
   avoids adding `FetchProjectFields` to the engine's `GitHubClient` interface (and the mock, `simgh.Sim`,
   `simgh.Instrumented` and cmd test client it would touch) and costs no extra round trip.
3. **Panel entries (`engine/display_field_warnings.go`).**
   - `display_field:missing` — one entry naming every enabled field the board lacks, with
     `FixAction: fabrik_command` / `args: repair-board --apply` (the only values the TUI fix key accepts;
     the `stage_drift` precedent). On a user-owned board, which `repair-board` refuses (ADR 1714 R7), the
     entry has no `FixAction` and says to create the fields by hand.
   - `display_field:mismatch:<label>` — one per field of the wrong type, never with a `FixAction`
     (`repair-board` reports a mismatch and never writes it).
   - `display_field_lookup:<label>` — a genuine lookup failure, reported once per outage through
     `warnings.Report` (replacing the bare `warning:` log line) with a stable fingerprint, and resolved by a
     later successful lookup or by the field being set to `off`.
4. **Reconcile only when conclusive.** The aggregate runs from `resolveDisplayFields` (startup and every
   poll, guarded by an outcome signature so an unchanged poll does no panel I/O). When every enabled field
   has an answer, `warnings.Reconcile` clears stale `display_field` entries, including ones a previous
   process left behind. While any enabled field is unresolved only known conditions are reported and
   nothing is cleared, and the missing entry never names a field whose state is unknown.
5. **An absent field stays final for the process.** `attempted` is not relaxed: the `missing` entry clears
   at the next start after `repair-board --apply` (or at once, by restart, when the keys are set to `off`).
   A periodic re-lookup would add a recurring GraphQL call for a condition an operator fixes once.
6. **FR-004 is pinned, not widened.** `operatorWarnLiteral` already matches `"startup", "warning:`; a test
   now pins the `logf` shapes it must flag. The now-stale `engine/status_line.go` exception is removed.
   Widening the scan to other tags (e.g. `"warn"` lines) would force dispositions on unrelated hits and is
   out of scope.

## Consequences

- A board missing the fields starts with three quiet `unavailable` notices, no lookup warnings, no retries
  and one panel entry with the fix. This reverses ADR 2135's "log only, retried every poll" disposition for
  the display-field lookup (as ADR 1750 reversed ADR 1347).
- The writer's behaviour is untouched: nothing reads a field back and no write path changed (ADR 2048/2049).
- The recording of GitHub's real error response is hand-authored from the message quoted in the issue; the
  provenance says so, and `scripts/wire-contract/record-fixtures.sh` re-records it against the sandbox board.
