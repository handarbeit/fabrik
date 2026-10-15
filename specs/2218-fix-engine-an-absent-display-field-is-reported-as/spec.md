# Feature Specification: An absent display field is reported as absent, not as a lookup failure

**Feature Branch**: `fabrik/issue-<N>`
**Created**: 2026-10-13
**Status**: Specified
**Input**: Seen on a live daemon whose board has none of the display fields (#2048, #2049).

## Background

`fetchProjectFieldID` (`github/textfield.go`) looks a display field up with `ProjectV2.field(name:)` and treats a `null` field as "absent", which `resolveDisplayField` (`engine/status_line.go`) turns into one quiet "no … field named … (create one to enable it)" line.

GitHub does not return `null` for a missing name. It returns a GraphQL error, `Could not resolve to a Unions::ProjectV2FieldConfiguration with the name '<name>'`. So on a board without the fields, every startup logs three lines:

```
[startup] warning: could not look up the "Fabrik" status-line field: fetching text field "Fabrik" for project PVT_…: GraphQL error: Could not resolve to a Unions::ProjectV2FieldConfiguration …
```

The same happens for `Last activity` and `Last run`. Each is then retried every `statusLineLookupRetry` for the life of the daemon. The lines are written through `logf` with the tag `startup` and the text `warning: …`, so `TestOperatorWarningsReachThePanel` never sees a `[startup] warning` literal. They are neither in the warnings panel nor the quiet absent notice.

## Requirements

- **FR-001**: `fetchProjectFieldID` classifies GitHub's "could not resolve … with the name" error for the field lookup as *absent* and returns `("", "", nil)`, so the existing absent path runs: one notice, no retry loop. The match must be narrow: the error must be on the `field` path or carry that exact message shape. Any other GraphQL or transport error stays an error.
- **FR-002**: A genuine lookup failure (anything FR-001 does not classify) is reported through `warnings.Report` with a stable key per field, and resolved when a later lookup succeeds. No bare `warning:` log line.
- **FR-003**: *(Superseded by FR-005.)* The absent notice names the fix: `fabrik repair-board --apply` creates the missing display fields (#2110). The warnings-panel entry in FR-005 now carries the fix; the log notice can stay as it is.
- **FR-004**: `TestOperatorWarningsReachThePanel` (or a sibling) also catches `logf(…, "startup", "warning: …")`-shaped calls, not only the bracketed literal. Each existing hit is either moved to the panel or added to the audited exception list with a reason.
- **FR-005**: When one or more enabled display fields (`project_fields.status_line`, `last_activity`, `last_run` not set to `off`) are absent from the board, record one warnings-panel entry through `warnings.Report`. It has a stable key, lists the missing field names in the detail, and sets its `FixAction` to `fabrik repair-board --apply`. It is resolved (`warnings.Resolve`) when a later lookup finds every enabled field, or when the missing ones are set to `off`. A field of the wrong type, which `repair-board` reports as a type mismatch and never writes, gets an entry saying so, with no `FixAction`.

## Success Criteria

- **SC-001**: A wire-contract fixture with GitHub's real error response for an absent field resolves to *absent* with no lookup-failure warning. Shown non-vacuous by reverting FR-001.
- **SC-002**: A board without the three fields starts with three absent notices, no lookup-failure warnings, no retries, and exactly one FR-005 panel entry.
- **SC-003**: A sim or engine test starts with a board missing the three fields and asserts one panel entry with `FixAction` `fabrik repair-board --apply`. A second run with the fields created asserts the entry is resolved. Shown non-vacuous by removing the `Report` call.

## Notes

- The quoted lines are verbatim from a live daemon's log (shadoworg board #5).
- On a live board missing all three fields, `fabrik repair-board` (dry run) reports them as missing, but nothing else tells the operator; FR-005 closes that gap.
