# ADR 1114: Pruefer Terminal UI Architecture

**Date**: 2026-07-27
**Status**: Accepted
**Issue**: #1114 — terminal UI (bubbletea), modelled on Fabrik's `tui/` package

## Context

Pruefer's V1 core (#1113) runs headless: it polls watched repos, invokes `claude`, and submits comment-only PR reviews, emitting structured log lines. That's enough to be useful but opaque while running — no at-a-glance view of which repos are being watched, which PRs are in flight, what a review cost, or why a PR was skipped. The V1 ADR explicitly scoped a TUI out as a follow-up; this ADR records that follow-up's structural decisions.

The requirement is to feel like the same family of tool as Fabrik's own engine TUI (`tui/`) — same stack (`bubbletea`/`bubbles`/`lipgloss`), same model/update/view split — while keeping headless operation fully supported and behaviorally identical to TUI-on, and without introducing a Go import from `pruefer/` into `engine/`/`itemstate`/`boardcache` (the constraint ADR-1113 already established).

## Decisions

### 1. New `pruefer/tui` package, not a shared import of Fabrik's `tui/`

Fabrik's `tui/` styles and helpers (`dimStyle`, `borderStyle`, `fmtDuration`, …) are unexported package-level values — "reuse the pattern" necessarily means duplicating a small set of lipgloss styles and format helpers, not importing them. `pruefer/tui/styles.go` and the per-pane components (`header.go`, `repos.go`, `active.go`, `history.go`, `detail.go`, `footer.go`, `model.go`) mirror Fabrik's `tui/` structure file-for-file where a direct analogue exists. This follows the same precedent as `pruefer/procattr_unix.go` duplicating `engine/procattr_unix.go` (ADR-1113 Decision 6): small, stable code is cheaper to duplicate than to extract into a shared package for one caller. Extracting genuinely shared code, if it emerges, is an explicit follow-up per the issue — not part of this change.

### 2. Event emission wraps `ReviewPR` from `poll()`, not inside it

`Daemon.poll`'s dispatch goroutine already has everything an observer needs — the review's start time and the full `ReviewOutcome` returned by `ReviewPR` — so `ReviewStartedEvent`/`ReviewCompletedEvent` are emitted immediately before and after the existing `ReviewPR` call, entirely from `poll()` (`pruefer/daemon.go`). **`ReviewPR`'s signature, control flow, and return shape are untouched.** This was chosen deliberately over emitting from inside `ReviewPR` (or wrapping/altering its decision branches) because it makes the issue's "no behaviour coupling" requirement close to structurally guaranteed rather than a matter of careful discipline: there is no code path inside `ReviewPR` that can observe, and therefore no code path that can accidentally depend on, whether a TUI is attached.

### 3. `Daemon.Emit func(ptui.Event)`, nil-checked at every call site

Pruefer has no access to Fabrik's `itemstate.Store` pub/sub observer machinery (ADR-1113's "no shared Go imports with `engine`" constraint rules it out), so the daemon-side hook is a single field — `Daemon.Emit func(ptui.Event)` — checked through a private `d.emit(ev)` helper that is a true no-op when `Emit == nil`. This is the direct analogue of `engine.Engine`'s `e.events == nil` sentinel (`engine/engine.go`), reused here without the observer-store machinery, since `poll()` is the sole place events originate and Fabrik's dual-store fan-out has no counterpart to build. `-notui` operation therefore costs one nil check per event site — the same "zero-cost/no-op-safe when disabled" property the issue requires.

### 4. Per-repo poll status: one `RepoPollEvent` per repo per cycle, combining timestamp + count + error

The spec's first open question — what "poll status" means per watched repo — is resolved as: last-poll timestamp, PR count found, and last error, combined into a single event emitted at the existing `ListOpenPRs` call site inside `poll()`'s per-repo loop (`pruefer/daemon.go`). No new API calls: all three fields are already locally available at that call site (the timestamp is taken immediately before the call; the count and error come from its return value).

### 5. Completed-review retention: bounded in-memory ring buffer (last 200), not persisted

The spec's second open question — retention window for "recently completed reviews" — is resolved as an in-memory ring buffer capped at 200 entries (`pruefer/tui/history.go`), evicting oldest-first. This is a **deliberate divergence** from Fabrik's own `tui/history.go`, which is unbounded and persists to `.fabrik/history.json`. Rationale: Pruefer is a long-running daemon (weeks/months between restarts), not an interactively-restarted CLI — unbounded+persisted history would grow without limit and requires new disk I/O this issue doesn't otherwise need. A capped in-memory buffer keeps the "recently completed" view useful (last ~200 reviews is ample for a live dashboard) without either concern. Restarting Pruefer clears history, matching the session-scoped nature of Decision 6 below.

### 6. Session-total cost/turns: in-memory, reset on restart, accumulated in the footer

The spec's third open question — whether the session aggregate persists across restarts — is resolved as in-memory-only, reset on every daemon restart, matching Fabrik's own session-scoped TUI state. Fabrik's TUI has no existing footer/header precedent for this (confirmed in Research: only a per-entry `HistoryEntry.CostUSD`, summed ad hoc in one test), so `pruefer/tui/footer.go`'s running total was designed fresh — accumulated from each `ReviewCompletedEvent` and displayed alongside the rate-limit gauge, consistent with where Fabrik places session-adjacent gauges without copying a component that doesn't exist.

### 7. Rate-limit surfacing via an optional interface, not a required one

Adding `RateLimitStats() (rest, graphql gh.RateLimitStats)` directly to `GitHubLister`/`GitHubReviewer` would force every existing test fake to implement a method it has no use for. Instead, `pruefer/daemon.go` defines a narrow `RateLimitReporter` interface and type-asserts `d.Client.(RateLimitReporter)` once per poll cycle; `*github.Client` satisfies it in production, and test fakes are unaffected unless they choose to implement it (in which case they simply emit no `RateLimitSnapshotEvent` — correct behavior, not a gap). The event carries the `rest` return value only, never `graphql` — Pruefer issues only REST calls (`ListOpenPRs`, `FetchPRDiff`, `FetchPRReviews`, `SubmitPRReview`), so surfacing `graphql` (Fabrik's own footer convention, since the engine is GraphQL-heavy) would always render a zero/empty gauge for Pruefer.

### 8. Turn/cost capture: `ClaudeInvoker.Review` returns `ReviewResult`, not a bare string

`pruefer/claude.go`'s `claudeReviewResponse` previously carried only `Result`/`IsError`. This issue adds `NumTurns`/`CostUSD` (JSON keys `num_turns`/`total_cost_usd`, matching `engine/claude.go`'s equivalent parsing), and both of `parseClaudeReviewJSON`'s parse paths — the single-JSON-object case and the NDJSON-envelope-loop case — copy them through explicitly (the two-path parser is exactly where Fabrik's own equivalent function has had to get this right before; both paths are covered by tests to guard against one path regressing silently). `ClaudeInvoker.Review`'s signature changes from `(string, error)` to `(ReviewResult, error)`, where `ReviewResult{Text, NumTurns, CostUSD}` — an additive, mechanical change: `ReviewPR` swaps `reviewText` for `result.Text` and copies the two new fields into `ReviewOutcome`, which itself gains `NumTurns`/`CostUSD` fields (zero value = "no data", consistent with `ReviewOutcome`'s existing zero-value conventions).

### 9. No TUI-driven review trigger

Confirms the spec's default assumption: the existing `/pruefer review` GitHub-comment path (`pruefer/comment.go`) already covers on-demand review. The TUI is observe-only; it has no keybinding or code path that can initiate, retry, or cancel a review.

## Consequences

**Positive:**
- `ReviewPR`'s decision logic is provably untouched by TUI wiring — event emission is bolted on from the outside in `poll()`, not threaded through the function whose behavior "no behaviour coupling" protects.
- `-notui` is a true zero-cost path: every event site is a single nil-check away from being a no-op, mirroring `engine.Engine`'s own sentinel idiom.
- Rate-limit and turn/cost surfacing required no new instrumentation beyond what the issue explicitly asked for (`github.Client.RateLimitStats()` already existed; `claude.go` parsing was the one genuinely new piece).

**Negative / Trade-offs:**
- `pruefer/tui` duplicates a non-trivial amount of structure from Fabrik's `tui/` (styles, `Component` interface, header/detail/footer shapes). Two copies to keep conceptually in sync if either evolves, accepted per Decision 1's rationale and the issue's explicit "extraction is a follow-up" scoping.
- Completed-review history and the session-total cost/turn count are both lost on every daemon restart (Decisions 5 and 6) — a deliberate trade against unbounded growth and new disk I/O, but it means a long-running daemon's "session total" resets more often than an operator might expect from the name.
- `ClaudeInvoker.Review`'s signature change is a breaking change to the interface, requiring every caller and test mock in `pruefer/` to update in the same change set (compile-time enforced, not a runtime risk, but a wider diff than a purely additive change would have been).

## Related Work

- `adrs/1113-pruefer-v1-architecture.md` — establishes the "no shared Go imports with `engine`" constraint this ADR's Decisions 2, 3, and 7 all work within, and the duplication-over-extraction precedent Decision 1 follows.
- `tui/` (`model.go`, `header.go`, `active.go`, `history.go`, `detail.go`, `footer.go`, `component.go`, `events.go`) — the structural pattern `pruefer/tui` mirrors.
- `engine/engine.go`'s `Engine.SetEvents`/`e.events == nil` — the nil-channel-as-no-op idiom `Daemon.Emit`/`d.emit` reuses.
- `engine/claude.go`'s `claudeResponse`/`tokenUsageFromResponse` — the turn/cost parsing shape `pruefer/claude.go`'s `claudeReviewResponse`/`ReviewResult` mirrors.

**References:** [cmd/pruefer/README.md](../cmd/pruefer/README.md)
