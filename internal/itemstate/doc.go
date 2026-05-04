// Package itemstate provides the canonical single-owner state store for
// per-issue tracking in the Fabrik engine.
//
// This package is the foundation of the reactive cache architecture described
// in docs/cache-refactor/02-design.md. It replaces the 25+ fragmented in-memory
// state structures spread across engine/ and boardcache/ with a single Store that
// holds one ItemState per (repo, issueNumber) pair.
//
// The core contract:
//   - All state mutations flow through Store.Apply — no bypassing writes.
//   - All reads flow through Store.Get, which returns an immutable Snapshot.
//   - Downstream components react to changes via Observer subscriptions.
//
// Design rationale is in adrs/036-reactive-cache-single-owner.md.
// Phase-by-phase migration strategy is in docs/cache-refactor/02-design.md §5.
//
// This package (Phase 3-A) is a pure addition. It is not yet wired into the
// engine or boardcache; that happens in Phase 3-B onward.
package itemstate
