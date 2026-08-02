# ADR 1348: Stale Warning Sweep

**Date**: 2026-08-02
**Status**: Accepted
**Issue**: #1348 — `allow_auto_merge` warnings are never cleared when the repo they refer to leaves the
board (durable-state leak)

## Context

`.fabrik/warnings.json` (`warnings/warnings.go`, ADR-052) backs the TUI's Warnings panel. Several
engine-side detectors follow the same idiom: record an entry (`warnings.Record`) when a condition is
true, clear it (`warnings.Clear`) when the same subject is re-checked and the condition has resolved.
That idiom has a gap — a subject that stops being checked *at all* never revisits the `Clear` branch, so
any warning recorded for it is immortal.

`checkAllowAutoMerge` (`engine/startup.go`) is called once per repo per process run, from the label-
seeding path (`poll.go`) for repos discovered on the *current* board. A repo that leaves the board is
never re-checked, so a warning recorded while it was still on the board can never be cleared. This was
observed in production on the `verveguy` project: four `allow_auto_merge` warnings for repos absent from
the current board persisted indefinitely, pushing the non-dismissed count past the TUI panel's row cap
and hiding live warnings behind dead ones. This is the same shape as the orphaned `stage:*:in_progress`
labels (#1135) and the `fabrik:claude-limit` account-wide label before its own sweep (ADR-1183).

Research (R7) confirmed two further instances of the identical shape — `stage_drift` and
`undeclared_reviewers` (`stages/drift.go`), both keyed by stage name and only re-visited for stage names
present in the *current* `e.cfg.Stages` pass — and one warning type that is safe by construction:
`version_skew` (`engine/upgrade.go`), whose key subject (the resolved on-disk executable path) is
re-derived fresh on every check rather than looked up in a shrinking discovered set, so its `Clear`
branch is always reachable.

## Decision

**Add `warnings.ClearMissing(warningType string, present map[string]bool) ([]string, error)`** as a
shared bulk-predicate primitive, rather than having each sweep loop over per-key `warnings.Clear` calls.
`Clear` unconditionally calls `save()` even when the key was never present in the file — a naive loop
over N possibly-stale keys, run every poll, would write the file on every poll unless every caller
independently got the pre-filtering exactly right. Putting "only write if something is actually stale"
inside the package removes an entire class of regression (AC5: no writes when nothing needs clearing)
rather than relying on caller discipline, and collapses what would otherwise be an O(N) sequence of
independent load-filter-save round trips into a single pass — relevant once multiple entries are stale
in the same poll, as the observed field case (four) was.

**Two separate, type-specific call sites — not one generic sweep runner.** The repo-keyed and
stage-name-keyed leaks have different known-good sets (`seenRepos` vs. `e.cfg.Stages` names), different
owning packages (`engine` vs. `stages`), and different correct cadences (see below). Forcing both through
one cross-package abstraction would buy nothing for two call sites total. `stage_drift` and
`undeclared_reviewers`, which *do* share everything (same known-good set, same call site), are unified
into one `stages.SweepStaleWarnings` call that invokes `ClearMissing` twice.

**`allow_auto_merge` sweeps every poll; `stage_drift`/`undeclared_reviewers` sweep once at startup.** The
repo sweep's known-good set (`seenRepos`) is itself rebuilt every poll from the live board, so running
the sweep every poll makes a repo's departure (or return) visible immediately, at zero extra cost (no new
fetch — R2/R4). The stage-name sweep's known-good set (`e.cfg.Stages`) is fixed for the life of the
process and only changes across a restart (including the in-place SIGHUP restart, which re-execs and
re-runs `Run()` from scratch) — running it every poll would be correct but pointless work, so it runs
once, alongside the existing `WarnStageDrift`/`WarnUndeclaredReviewers` calls at startup.

**The engine's own configured repo is exempted from the repo sweep (single-repo mode only).**
`checkAllowAutoMerge(e.cfg.Owner, e.cfg.Repo)` fires unconditionally at `Run()` startup regardless of
whether the board currently has any open items for that repo, but `seenRepos` is built purely from
`board.Items`. Without an exemption, a transient zero-open-items poll for the operator's own configured
repo (e.g. everything currently sitting in Done) would durably clear a legitimate warning — and because
`checkedAutoMergeRepos` never re-fires for that repo after the first startup call, the warning would not
reappear until a process restart, unlike every other cosmetic, self-healing settle scan in this codebase.
`sweepStaleAllowAutoMergeWarnings` therefore unions `e.defaultRepo()` into the comparison set (copying
`seenRepos` first — never mutating the caller's map) whenever it is non-empty. Multi-repo mode
(`e.cfg.Repo == ""`) has no equivalent always-present repo, so `defaultRepo()` returning `""` is a
correct no-op there.

**`version_skew` gets a doc comment, not a sweep.** Its subject is re-derived fresh on every
idle-upgrade check rather than looked up in a shrinking set, so its existing `Clear` branch is already
reachable every evaluation — adding a sweep would be dead code. A comment on `checkVersionSkew`
(`engine/upgrade.go`) records this reasoning explicitly per R7, rather than leaving it to silence.

## Consequences

- A warning whose subject has genuinely left the board (or the stage config) is cleared automatically,
  without operator intervention or a restart, closing the gap that required a hand-edit of
  `warnings.json` in the field incident.
- `ClearMissing` is now the preferred idiom for any future "durable per-subject warning whose known-good
  set is already computed elsewhere" case — the two sweeps here, plus `version_skew`'s existing
  self-refreshing case, cover the full current warning surface (R7).
- No change to which conditions produce a warning, no TUI rendering change, and this is not a general
  expiry/TTL mechanism: only subjects provably absent from an already-known-good set are ever cleared.
- Over-clearing risk (a sweep keying off the wrong field, or comparing against a filtered rather than the
  full known-good set) is mitigated by scoping `ClearMissing` to one `Type` per call and by explicit test
  coverage: a still-on-the-board repo's warning (regardless of its auto-merge state) and a still-
  configured `Unmanaged` stage's warning must both survive their respective sweeps.

See `docs/state-machine.md` §7.11 for the as-built behavior.
