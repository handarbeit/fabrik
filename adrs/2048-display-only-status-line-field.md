# ADR 2048: Display-Only Status-Line Project Field, with the Merge Train as its First Writer

**Date**: 2026-10-10
**Status**: Accepted
**Issue**: #2048 — board: a display-only "Fabrik" status-line project field, with the merge train as its first writer
**Builds on**: [ADR 042](042-mutation-echo-check.md), [ADR 035](035-four-layer-status-reconciliation.md), [ADR 2047](2047-merge-train-overlap-aware-composition.md), [ADR 2044](2044-singleton-catch-up.md)

Mentioned for context: #2042 (the report on the public triage board).

## Context

A card in the `Queued` column looks the same in every phase of the merge train — waiting its turn,
resolving a conflict, waiting on trial CI, bisecting, landing — and a paused, blocked or CI-waiting
item looks like any other. Telling working from stuck meant reading the daemon log or the TUI.

## Decision

Fabrik writes one short line, saying what an item is doing or waiting on right now, into an optional
ProjectV2 **text** field (`project_fields.status_line`, default name `Fabrik`, `off` disables). It
is built as one general writer called from engine transitions, with the merge train as its first
user, not as train-specific code.

### Display only, never read back

Nothing reads the field — not from GitHub, not from the cache, not from the writer's own record of
what it wrote (which exists only to skip a write that would change nothing). Labels and the item
store stay the only state. This is the same rule as "labels are state", applied to a projection.

### Writer (`engine/status_line.go`)

- Write only on a transition and only when the line differs from the last one written for the item;
  the record is in memory (a restart may rewrite an identical line once) and is updated only after a
  successful write, so a failed write is retried at the next transition.
- Lines are capped at 60 characters (runes), ending in `…`. Wording lives in `status_line_text.go`.
- The field is resolved once, TEXT-only; absent, non-text or `off` means every call is a silent
  no-op and exactly one `[startup]` line is logged.
- A failed write is logged and swallowed: it never alters the transition that triggered it.
- Writes are inline, not queued. A batch of N is N mutations per phase on the train worker. Project
  field mutations are outside the GraphQL rate-limit budget, so this is a latency question, not a
  budget one; a bounded asynchronous writer that coalesces to the latest value per item is the
  follow-up if it ever matters.
- A successful write applies `SelfWriteObserved` (#1090), like every other project-field write.

### Hooks are call sites, not an observer

An observer over `LabelDeltas` (the channel-events pattern) would also react to human and webhook
label edits, and needs a queue and a consumer goroutine. The train's phase facts (assembling,
bisect step, draft CI PR number) are not label-derived at all. Explicit call sites next to the
engine's own already-logged writes are simpler to reason about and to test; the cost is that every
new state needs a hook added deliberately.

### Echo and drift suppression

- *Webhook.* A `projects_v2_item` edit that is not a `single_select` (Status) edit is ignored after
  the echo match: no Store change, so no wake, no `localDeltaAt` stamp (no invalidation), no drift.
  `changes.field_value.to` is decoded raw because its JSON type depends on the field type (the real
  text-field shape is unverified; a string, null or object must all be harmless).
- *No echo registration for status-line writes.* Registrations are keyed
  `projects_v2_item:edited:<ItemID>` with no field discriminator: one would collide with Status-move
  registrations, and a missing delivery would inflate the ADR-042 miss counter that flips webhook
  health.
- *`updatedAt` is fixed at the parse layer.* A field write is assumed to bump the project item's
  `updatedAt` (unverified for text fields), which would read as drift in `LightReconcile`, a reason
  to deep-fetch in the probe, and a stale baseline in the store. Rather than teach each consumer, the
  board and probe queries also select the display field's own `updatedAt`
  (`fieldValueByName(...) @include(if: $withStatusLine)`) and the parsers drop the project-item
  contribution when it is not after that value (2 s tolerance). Every consumer reads that one parsed
  value. Alternatives rejected: an engine-side per-item last-write time tolerated in each consumer
  (more code, restart-unsafe) and skipping the compare after a self-write (masks real drift).
  A Status move made just before a display write can be masked in `updatedAt`; Status is compared
  directly and covered by the layer-2 status batch and the webhook.
- The setter, `github.Client.SetStatusLineField`, is deliberately not on the engine's `GitHubClient`
  interface, so no interface signature changed for it.

### Wording decisions

- `trial #N` is the **draft CI PR number** (as in #2042's example). Assembly and conflict resolution
  happen before that PR exists, so those lines are `trial · resolving conflicts` with no number.
- `bisecting · step i of n` is the validation about to run out of the **cost cap**; how many halves
  remain is not known up front, so `n` is a ceiling. Sub-trials write no trial line of their own so
  they do not overwrite it.
- A singleton catch-up waits on the member's **own** PR's CI, so its line is `catch-up · CI on PR #N`
  with that PR.
- Members left Queued without a live batch show `queued`; an overlap-deferred member shows
  `deferred: overlaps #M` until the next formation admits it.
- `claude-limit until HH:MM` is written only for the item whose invocation hit the limit (the
  suspension is account-wide; one write per detection keeps volume down).
- Done is cleared in `advanceToNextStage` (merge-train landings, ordinary merge) and in the two Done moves
  that bypass it (no-work-needed settle, closed-item advance); it also clears on
  any stage-to-stage advance and on reroute off the train, because the next stage writes its own line.

### No `fabrik:merge-train` label

#2042 also proposed a label. It is rejected: labels are engine state in Fabrik, and adding and
removing one on every member of every train adds issue events and cache churn for information the
field already shows. A board filter on the text field covers "in the train now".

## Consequences

- Operators get a legible board for the cost of creating one Text field. Fabrik never creates it
  (board-structure administration is out of scope; ADR 1714 is where that would live).
- A line can go stale where no transition fires (a daemon crash mid-phase). It is overwritten by the
  item's next transition or Done. There is no startup sweep, because that would be a read-back.
- The `updatedAt` discount covers text *writes* only. A *clear* leaves no value node, so
  `fieldValueByName` returns null and there is no field `updatedAt` to discount against; if GitHub
  bumps the item on a clear, that bump reads as ordinary item activity. This adds no new exposure:
  every engine clear is adjacent to a Status move or a label removal that bumps `updatedAt` anyway
  and is covered by the same `SelfWriteObserved` baseline advance (#1090). The sim models the clear
  this way (no value write time), and the re-record script prints the post-clear `updatedAt`.
- A failing field lookup is retried at most every five minutes and warned about once per outage,
  rather than on every transition.
- Each new state needs an explicit hook; states without a cheap, already-logged transition are left
  for a follow-up.
- The sim cannot prove GitHub's real `updatedAt` behaviour for a text write or the real webhook
  payload; the recordings under `github/testdata/recordings/` for the three new operations were
  hand-authored to the vendored schema and should be re-recorded with
  `scripts/wire-contract/record-fixtures.sh` against a sandbox board carrying a `Fabrik` text field
  (the script also prints the item and project `updatedAt` before and after a text write, which
  settles the bump question).
- Two neutralisation seams pin the behaviour: `CacheImpl.SetTextEditSuppressionDisabledForTest` and
  an unset `SetStatusLineField`.
