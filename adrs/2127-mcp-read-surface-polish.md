# ADR 2127: MCP read-surface polish — what the new figures mean

## Status

Accepted (#2127). Amends ADR 1966-a/b (read API, channel events), ADR 2122 (head semantics) and reads ADR 2072 (flake re-run).

## Context

An in-situ session against the live daemon found small defects in the MCP read surface, each of which sent the overseer back to `gh`: the flow view dropped Done silently, `fabrik_status` hid non-Fabrik labels, an empty filtered attention view dropped its key, `last_invocation` mixed per-call and session figures, and `validate-settled` could not tell a clean `COMMENTED` review from one with findings, hid a flake re-run behind `ci=green` and carried no title. All of it is rendering over state the daemon already holds.

## Decisions

**Reconciliation, not a Done column (R1/R3).** The flow view still lists open items only. `excluded.closed` counts the matched closed items it leaves out and `matched` counts everything that passed the filter, so `sum(flow.count) + excluded.closed == matched`. A Done column would change the "columns" contract, and a Done-column item with an open issue is already counted in Done today. An attention view always serialises `attention`; because the existing consumers read `[]AttentionEntry`, the key is guaranteed by a `MarshalJSON` on `BoardResult` rather than a pointer type.

**`clean_pass` is a statement about the head, and differs from the feedback gate (R5).** Every per-reviewer field is evaluated at `AgreedHead()` (#2122). `clean_pass` is true only when the reviewer's latest review is at the head and nothing is outstanding; a pass on an older head is false, so is a `CHANGES_REQUESTED` review (not a pass at all), and any input the cache lacks (a review with no `commit_id`, no agreed head) yields `unknown`, never `true`. `outstanding` counts unresolved, non-outdated threads and an actionable, not-durably-addressed review body *whether or not the engine has processed them*. `unaddressed` is the feedback gate's own predicate. They differ on purpose: a non-relaxed `validate-settled` is only announced when nothing is unaddressed, so `outstanding` is what separates a clean pass from findings that were worked through but never resolved. A thread comment has no review id, so "inline comments at the head" links a review to its threads by author plus commit. The `pull_request_review` webhook now carries `commit_id`, so a webhook-ingested review is not unknown until the next deep fetch.

**`figure_basis` labels, it does not change accounting (R4).** Duration is the invocation's wall-clock time; tokens and cost are the totals the Claude CLI reports for the session the invocation ran in; turns are the CLI's count for the call. The labels say what each field is, "as reported by the CLI". On a first, non-resumed invocation the two coincide, so the label describes the field, not that the numbers differ. `InvocationRecorded` and `tokenUsageFromResponse` are untouched.

**`ci_rerun` is in memory only (R7).** The names of the checks re-run by the stage gate (R1 and R2 of ADR 2072) are recorded on `prFlakeState` when a re-run is actually made, and read for the head the event reports. The state is not persisted, so the attribute is absent after a daemon restart or after `startupStateTTL` (24 h) without a gate touch, and for the merge train's own re-run. A hint that can be missing is acceptable; a persisted record would add a new store for it. It is emitted only when the verdict is `green`.

**Title in `baseEvent` (R8).** One choke point reaches every item-scoped event and needs no wire or shim change (`meta.title` passes the shim's key filter). Events built for an item that has left the Store carry none.

## Consequences

The scans that forbid GitHub reads in `localapi*.go` and `channel_*.go` stay valid: the verdict builder takes only cached state and lives in `review_verdict.go`, pinned GitHub-free by its own test. Each requirement has a test and a neutralised twin via `SetReadPolishDisabledForTest`.
