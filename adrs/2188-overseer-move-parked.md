# ADR 2188: Overseer move-parked action

Status: accepted (issue #2188; extends [ADR-1966-c](1966-c-fabrik-mcp-actions.md) and [ADR 2126](2126-overseer-advance.md), parent initiative #1966)

## Context

A board may configure more than one unmanaged parking column, for example `Triage` ahead of `Backlog`. The overseer is told when items arrive in either (#2123, #2187) but, with `fabrik_promote` alone, could only move a Triage item into the pipeline. The obvious triage decision "accepted, not yet" (Triage → Backlog, or back) needed a human dragging the card. A move between two parking columns starts no work (the engine never dispatches for an unmanaged column), so it is the lowest-risk board write the overseer could make.

## Decisions

### 1. A separate tool, `fabrik_move_parked`, not a widened `fabrik_promote`

The deciding constraint is that a tool's name must not suggest it starts work when it does not. "Promote" says an item enters the pipeline. Widening it so that `to` may name a parking column would also contradict pinned behaviour (promote refuses an unmanaged target, and its documentation says unmanaged columns are engine-managed), force `valid_targets` and the "will auto-advance" note to branch on the destination, and make an older daemon answer with a confusing refusal rather than the clean "does not support overseer actions" that an unknown method gets.

The cost is one `Actor` method and the compiler-checked touchpoints (`localapi` method and params, server dispatch, the MCP tool and its tests). The method set is now six.

### 2. The name is `fabrik_move_parked`, not `fabrik_park`

"Park" reads as pulling work out of the pipeline into a parking column, which is explicitly out of scope. `move_parked` says what the item is (already parked) and what happens (it moves). The tool description repeats that it only moves between parking columns and never pulls work out of the pipeline.

### 3. Both ends are parking columns, judged by `stages.IsParkingColumn`

The source (cached, then live) and the destination must satisfy `stages.IsParkingColumn`: an `Unmanaged` stage, or the legacy literal `Backlog` when no `Backlog` stage is configured. A configured, non-unmanaged `Backlog` is a pipeline column on both sides. The destination must also exist as a board option, since otherwise the write would fail with "no status option". The legacy rule is not redefined: candidates are the configured stage names plus `Backlog`, and `IsParkingColumn` decides.

### 4. A same-column move is a refusal

A request whose destination is the item's current column is refused, on the cached column and again on the live one, and nothing is written. This follows the convention that a refused action writes nothing (`advance`, `revalidate`) rather than `set_autonomy`'s no-op success, and avoids a success that did nothing.

### 5. It reuses the existing write path and writes no label

The move is `moveItemToStatus` (with the requester carried onto `column-changed`) and the comment goes through the single `postAudit` path via `finish`. The column is confirmed with one live ID-keyed `FetchProjectItemStatus` that fails closed. No label is written or removed, so a paused or blocked item can be filed and its pause is untouched (ADR-1813), and a `fabrik:cruise`/`fabrik:yolo` item is moved like any other with no auto-advance note. `overseer_park.go` is held to the same source scan as the other action files, with no `postComment` of its own and no label write.

### 6. Attribution follows the shipped behaviour

The issue text said the resulting event is attributed to `fabrik`. The shipped attribution for an overseer action is `actor=overseer`, `via=<subscriber>` (ADR 2125, state-machine §7.15), and the tests assert that. `tools/list` stays static, as for the other actions.

## Consequences

- The overseer can make the triage decision with one audited call.
- With only one parking column the tool is advertised but always refuses ("no other parking column").
- Out of scope, unchanged: moving an item from the pipeline back into a parking column, any change to which columns produce `item-added`, and any tool that lifts a pause or posts a free-form comment.
