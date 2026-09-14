# ADR 1674: Pruefer TUI Height-Aware Layout, Panel Reorder, and Skip Filtering

**Date**: 2026-09-13
**Status**: Accepted
**Issue**: #1674 — pruefer TUI: fill the terminal height, hide skips from Completed Reviews,
align its columns, move Watched Repos last

## Context

Four defects, all visible in one screenshot on a ~62-row terminal:

1. **The TUI didn't fill the terminal.** `Model.height` was captured from `WindowSizeMsg` and
   never read again — every component's `View(width int)` sized itself off constants
   (`maxDisplayedHistoryRows = 10`), so a tall terminal left the bottom half empty.
2. **Skips dominated Completed Reviews.** At a typical watched-repo count, `already reviewed at
   this head SHA` is the overwhelming steady-state outcome; rendered unconditionally, it evicted
   the reviews that actually carry information (9 of 10 visible rows in the reported screenshot).
3. **Completed Reviews columns were ragged.** The repo/PR label, timestamp, duration, and reason
   were concatenated into one string and truncated as a whole at a fixed rune offset, landing
   mid-token with no column alignment.
4. **Watched Repos — the largest, static panel — rendered first**, pushing the panels that
   actually change (In-Flight, Completed) down the screen.

`pruefer/tui` deliberately mirrors Fabrik's own `tui/` package structurally (ADR-1114) but is a
separate copy, not an import — Fabrik's copy had already solved height-awareness
(`updateLayout`/`SetLayout` called from `Update()`, backing a persistent `bubbles/viewport`);
Pruefer's had not.

## Decision

### 1. Layout budget computed inline in `Model.View()`, not via `SetLayout` calls from `Update()`

Fabrik's `tui/model.go` calls `updateLayout()` from a dozen `Update()` branches because its
history pane wraps a persistent `bubbles/viewport` whose scroll offset must stay synchronized
with content between key events and the next render. Pruefer's history and repos panes use
manual windowing recomputed fresh on every render — only the selection index (`h.idx`) persists,
and it is untouched by this change — so there is no persistent scroll state requiring an
out-of-band sync point.

`Model.View()` therefore computes the height budget itself, every render, from `m.height` (set
once by `WindowSizeMsg`, already captured pre-#1674) and the other panes' own `Height()`:

```go
fixed := lineCount(header) + lineCount(active) + lineCount(footer) [+ lineCount(detail)]
avail := m.height - fixed - 2*paneChrome
hRows, rRows := allocateRows(avail, wantHistory, wantRepos)
m.history.SetMaxRows(hRows)
m.repos.SetMaxRows(rRows)
```

`SetMaxRows` mutates the local copy of `m.history`/`m.repos` inside a value-receiver `View()` —
side-effect-free, no interface change to `Component.View(width int) string`. This is simpler than
Fabrik's scattered-call-site pattern and has fewer places to get wrong; it's a deliberate
departure from that precedent, not an oversight.

`active.go`, `footer.go`, and `detail.go` are unchanged. Header, In-Flight Reviews, Footer, and
Detail (when open) always render at their natural, content-derived height and are never
truncated by the budget — only their pre-existing `Height()` is read by the calc. No
`SetLayout`-shaped hook was added to any of the three.

### 2. Space-allocation priority: the changing pane is served first

`allocateRows(avail, wantHistory, wantRepos)` states the rule explicitly rather than leaving it
incidental:

- **Completed Reviews gets what its content needs**, up to `avail - minPaneRows` (reserving a
  floor for Watched Repos).
- **Watched Repos takes the remainder**, capped at what it actually needs; any surplus it
  doesn't use flows back to Completed Reviews so the total still fills `avail` exactly.
- Both panes keep a `minPaneRows` (3) floor whenever there's enough space to grant it; below
  `2*minPaneRows` total, the available rows are still split (never zero, never negative), with
  Completed Reviews getting the larger half.
- Whichever pane's content doesn't fit its grant windows around the current selection with a
  "… N more" line, exactly as `active.go` already does for In-Flight Reviews — this issue extends
  that existing pattern to a variable budget instead of a fixed constant, rather than introducing
  a new truncation mechanism.

Watched Repos is therefore the panel that shrinks first, and can be squeezed to its floor (or, at
the very smallest terminals, further) before Completed Reviews yields anything — the direct
space-allocation analog of moving it last in render order (Decision 4): the changing panel is more
important than the static one, both spatially and in view order.

### 3. Skips are filtered by default and disclosed as a count, not a toggle or new keybinding

`HistoryPaneComponent.visibleEntries()` filters `Skipped && Err == ""` — an entry is hidden only
when it is skip-only; an entry that errored is never filtered, even if `Skipped` also happens to
be set. All three consumers of "what's visible" — navigation bounds, `Selected()`, and `View()`'s
render loop — route through this one method, so the highlighted row, the detail panel, and the
rendered list can never disagree about which entries exist. (This exact class of bug — resolving
against the raw `h.entries` ring buffer instead of the filtered/visible list — was the review
finding fixed in this issue's own implementation history; see the regression test
`TestHistoryPane_SelectionTracksVisibleRows`.)

Discoverability is a dim count appended to the pane's title (`Completed Reviews (7) · 22
skipped`), not a toggle key or a separate summary panel — the lowest-risk option that needs no new
interaction surface. When every retained entry is a skip, the pane's empty-state line also
discloses the count (`no completed reviews yet (7 skipped)`) rather than rendering the same bare
placeholder as a genuinely idle daemon — distinguishing "nothing has happened" from "everything so
far was filtered."

### 4. Column alignment computed from the rendered set, not fixed-width formatting

Completed Reviews' `repo#PR` column width is measured from the current visible set each render
(capped at `maxKeyColumn` so one very long repo name can't push the remaining columns
off-screen), then every row is built as separate columns (status, key, timestamp, duration,
extra) rather than one pre-concatenated, then-truncated string. An over-long key is shortened with
`elideMiddle`, which removes from the middle and keeps both ends — preserving the `#N` suffix
(the identifying part) instead of truncating it away, per the issue's explicit preference. Watched
Repos keeps its existing fixed-width `%-30s` formatting; column alignment is scoped to Completed
Reviews only, per the issue's own file/requirement scope (R3).

### 5. Panel render order: header → In-Flight → Completed Reviews → Watched Repos → footer

`Model.View()`'s section order changes from header → repos → active → [detail] → history → footer
to header → active → [detail] → history → repos → footer. The `tab` focus-cycle order is
unchanged (`repos → active → history → repos`) — the acceptance criteria for this issue constrain
render order only, and changing the cycle order was judged separate scope not to bundle into this
fix.

## Consequences

- **The `Height()`/`View()` consistency invariant is load-bearing and enforced by tests.** Both
  `HistoryPaneComponent` and `RepoPaneComponent` pad their content to exactly their granted
  budget when one has been set (`SetMaxRows`), and `Height()` reports that padded height rather
  than a content-derived count — `TestPaneHeightMatchesRender` pins this for both panes. Getting
  this wrong previously caused a real regression in this exact file (`Height()` under-reporting
  relative to what `View()` actually padded to) that was found and fixed once already during this
  issue's implementation history; the invariant is now covered by an explicit test rather than
  relying on inspection.
- **A zero-value component (no `SetMaxRows` call) keeps its pre-#1674 default height** — 10 rows
  for history, 40 for repos — so existing unit tests that construct a bare
  `HistoryPaneComponent{}`/`RepoPaneComponent{}` without going through `Model` continue to render
  sensibly.
- **Watched Repos can render `""`/`Height()==0`-adjacent output at the smallest terminals** it
  gets squeezed to its floor before Completed Reviews gives up anything, by design (Decision 2).
- No new external dependency — `bubbles/viewport` was considered (Fabrik's own `tui/history.go`
  precedent) and deliberately not adopted; the existing windowed-selection pattern already used by
  `active.go`/`history.go` extends cleanly to a variable budget without introducing new
  component-internal scroll state.
