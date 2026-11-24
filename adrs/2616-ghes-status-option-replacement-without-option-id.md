# ADR 2616: Status option replacement on servers whose option input has no `id`

**Date**: 2026-11-24
**Status**: Accepted
**Issue**: #2616 — repair-board / init --create-board: handle GHES ProjectV2 option input without id, never clearing item Statuses
**Amends**: ADR 1714 (the id-echo contract now applies where the server accepts an `id`)
**Makes a scoped exception to**: ADR 1391 ("no schema introspection, no runtime capability negotiation")

## Context

`fabrik repair-board --apply` echoes every existing Status option's `id` through
`updateProjectV2Field` because, on github.com, an option sent without its `id` is
regenerated and every item's Status is cleared (ADR 1714, R4). GHES 3.19.10's
`ProjectV2SingleSelectFieldOptionInput` has only `name`, `color` and `description` (the last
two non-null). The echo is rejected during validation (`Field is not defined on
ProjectV2SingleSelectFieldOptionInput`), so nothing executes and the operator cannot add a
column through Fabrik. Simply dropping the `id` would be silently destructive on a board that
has items, since on such a server no option can keep its identity through a replacement.

`fabrik init --create-board` already sent no ids (`StatusOptionInput.ID` nil for every entry), so
it was not broken; the new board has no items.

## Decisions

1. **Capability by introspection, once per command.** `Client.StatusOptionIDSupport()` runs
   `__type(name:"ProjectV2SingleSelectFieldOptionInput"){inputFields{name}}` and returns
   accepted / not accepted / unknown. A version rule would be a guess (no known GHES version adds
   `id`), and an error-driven retry would key on message text. Detection is version-agnostic: a
   GHES that later adds `id` takes the github.com path.
2. **Unknown is treated as "no id".** A transport error, denied introspection or a null type never
   sends an `id`; it replaces the option list only on a board with no Status value, and otherwise
   refuses. Cost: a github.com instance that denies introspection is refused on a populated board
   (safe, documented, expected to be rare).
3. **Population check is a dedicated, fail-closed query.** `Client.BoardHasStatusValue` pages
   `items(archivedStates:[ARCHIVED, NOT_ARCHIVED])`, selecting only whether `Status` has a value,
   and stops at the first hit. It does not reuse `FetchProjectBoard`: that excludes archived items
   (Fabrik archives Done items, which still hold a Status), skips items with no content (draft or
   redacted items can hold a Status) and retries an empty result. Any error returns
   `(true, err)`, so a caller that ignores the error still treats the board as populated.
4. **Three modes** (`planStatusReplace`, `cmd/board_admin.go`): echo ids (accepted); name-only
   (no id/unknown, and no item has a Status); refuse (otherwise). Refusal sends no option mutation,
   names the missing columns in stage order, states the manual remedy, and `repair-board --apply`
   returns it as an error (non-zero exit). Display-field creation is independent and still runs.
   The dry run reports the same outcome and never suggests `--apply` for columns it cannot add.
5. **`Client.SetStatusFieldOptionsWithoutIDs`** returns an error, before any request, if any option
   carries an id, so "never send `id` where it is not accepted" holds by construction on that
   path. `SetStatusFieldOptions` is unchanged; ADR 1714's payload tests pass untouched. The
   github.com request is byte-identical; the one difference is one read-only introspection query,
   made only when columns are missing.
6. **`init --create-board`** uses the without-ids call and makes no capability or population query:
   a freshly created board is empty by construction.
7. **Wire contract.** Both new documents are registered. The GHES shape is derived in-test from
   the vendored github.com SDL by removing `id` from the input type; the derivation asserts it found
   and removed the field, so a schema refresh cannot silently make it vacuous. Variable validation
   proves the echo payload is valid on github.com and rejected on the GHES shape, and the
   name-only payload is valid on both.

## Consequences

- ADR 1391's "no introspection / capability negotiation" rested on a parity premise this one
  mutation falsifies; the exception is scoped to this call. The User Guide's GHES section no longer
  claims full parity and names the limitation and the remedy.
- Re-creating options on an empty board regenerates option ids; a running daemon caches them at
  startup and must be restarted (documented).
- **Not verified on a live GHES.** Whether 3.19.10 answers `__type`, supports `archivedStates` on
  `items` and accepts `description: ""` cannot be checked from CI; the empty-board success path is
  proven by fixtures only. If the server rejects `__type` or `archivedStates` the code fails closed
  (unknown capability; population read error → refusal), so an empty GHES board would be refused
  rather than corrupted. A live check is a manual follow-up.
- Out of scope: other GHES schema differences, preserving Statuses across a replacement on a
  server with no option identity, automating the UI remedy.
