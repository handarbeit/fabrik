# ADR 1421: Holding-Stage Board Columns Are Required Unconditionally, Not Only When merge_train: on

**Date**: 2026-08-08
**Status**: Accepted
**Issue**: #1421 — Startup exempts the holding-stage column from validation while the terminal advance
still targets it

## Context

`checkStageColumnAlignment` (`engine/startup.go`) builds the set of stages whose board column is
required to exist, and hard-fails startup if any is missing — the same treatment given to every
non-cleanup, non-unmanaged stage. Before this change, a `HoldingStage: true` stage (the shipped default
is `Queued`, ADR-059) was exempted from that required set whenever `e.cfg.MergeTrain != "on"`:

```go
if s.HoldingStage && e.cfg.MergeTrain != "on" {
    continue
}
```

The comment justifying it read: "they only require a board column when merge_train is on." That premise
conflates two different things — whether the column is *reachable* by any code path right now, and
whether it will *stay* unreachable for the lifetime of the process. `merge_train` is an
operator-togglable runtime config value (flag, env var, or `.fabrik/config.yaml`), not something that
regenerates board columns when flipped. A board validated cleanly with `merge_train: off` can fail the
instant an operator sets `merge_train: on` and restarts — and the failure mode is not a clean startup
error, it is a silent stranding: `advanceToQueued` (`engine/stages.go`) writes a Status value the board's
single-select field doesn't recognize, the write is accepted or silently dropped depending on the
GraphQL mutation's behavior for an unrecognized option, and the item's dependents — linked via native
`blockedBy` edges that clear on **close**, not on **merge** — stay blocked indefinitely even though the
work merged and shipped.

This is exactly what happened in #1082: a community-reported incident on fabrik 0.0.75 where three
merged, deployed, verified issues sat open while five downstream slices waited on them. Startup emitted
only the *extra*-columns warning ("board has columns with no matching stage: In Progress, Todo") — the
board appeared validated. The asymmetry is the root cause: startup validates board→stage (extra columns,
a warning) but only partially validates stage→board (missing columns, a hard error for most stages, a
silent skip for the holding stage) — and the half that was skipped is the half that strands work.

### Is the exemption's premise at least true today?

Yes, narrowly. ADR-1072 (merged 2026-07-27, "Holding Stages Are Reachable Only Via Dedicated Code, Not
Positional Advance") made `stages.NextStage` unconditionally skip `HoldingStage` in its positional walk,
so every generic terminal-advance path lands on `Done` directly, never on the holding column, regardless
of `merge_train`'s value. As of `main`, the *only* code path that writes an item's board Status to a
`HoldingStage` column is `advanceToQueued`, itself gated on `merge_train == "on"` at its sole call site.
So: on the current codebase, with `merge_train` off, the holding column genuinely is unreachable through
any code path.

That finding does not make the exemption safe, for the same reason a locked door with no key issued yet
is not the same guarantee as a door that has been welded shut. `merge_train` can be toggled by an
operator across a restart without ever touching the board. The reachability analysis is a snapshot of
*this build's* call graph, not a durable property of the configuration. Treating "provably unreachable in
today's code" as license to skip validating a *configured* stage's column is the same category of mistake
that produced #1082 in the first place — inferring runtime safety from a flag's value observed once, at
one particular startup.

## Decision

Remove the `HoldingStage && MergeTrain != "on"` exemption entirely. A `HoldingStage: true` stage's board
column is now required whenever that stage is present in `.fabrik/stages/`, with exactly the same
missing-column hard-fail treatment as every other non-cleanup, non-unmanaged stage — independent of
`merge_train`'s value at any given startup.

Two accompanying changes to the failure report, both scoped to `checkStageColumnAlignment`:

1. **The report is now a unified per-required-stage present/missing list** (`<name> (order N):
   present|MISSING`), rather than a "missing" list cross-referenced against a separate "all board
   columns found" list. This directly answers the operator's actual question — "which options must my
   Status field have, and which do I already have?" — in one place.
2. **The report detail moves from stderr-only into the returned `error`.** Previously
   `checkStageColumnAlignment` printed full detail to `os.Stderr` but returned a generic
   `fmt.Errorf("startup check failed: stage/board column mismatch")` with none of it — untestable without
   a stderr-capture harness this test file has no precedent for. The full report (present/missing list,
   board columns found, the holding-stage note) is now baked into `err.Error()` directly, mirroring how
   `checkAPIKeyHelper` and `checkGHESVersionFloorAgainst` in the same file already return fully-detailed
   errors rather than printing separately. The stderr print is kept alongside it for operators tailing
   logs live.

The per-missing-holding-stage note is rewritten from "is a holding stage required by merge_train: on" —
now false — to state the column's terminal-only role plainly: it is never a valid intake/manual-move
target, and a Status of `Todo`/`Backlog`/null means Fabrik ignores the item, so guessing wrong is itself
a stall (the exact ambiguity #1082 flagged the operator as having no way to resolve).

## Rationale

### Why require the column even though ADR-1072 proves it's unreachable today when merge_train is off?

Because "unreachable in this build's call graph" and "safe to leave unvalidated" are different claims.
The guard here is not re-deriving or second-guessing ADR-1072's reachability analysis — it accepts it as
correct — but declines to use it as the basis for a startup-validation exemption, because the thing that
determines reachability (`merge_train`'s value) is itself mutable at runtime without any board change.
Every other non-cleanup, non-unmanaged stage in `.fabrik/stages/` is required unconditionally, without
asking "is this stage's board column provably reachable under the current flag values" first. Holding
stages should follow the identical posture: required because they are *configured*, not because a
runtime flag's current value makes them currently reachable.

### Why not gate the requirement on some other signal instead of removing it outright?

No other signal is more durable than `merge_train`'s own value, and `merge_train`'s value is exactly the
thing that's unreliable here (it's what changes across the restart that breaks the exempted case). There
is no configuration-time way to know in advance whether an operator will flip it on later. The only fully
safe answer is to require the column whenever the stage exists in config, matching the treatment already
given to every other required stage.

### Why fold the report into the returned error instead of adding stderr-capture test scaffolding?

Two existing functions in the same file (`checkAPIKeyHelper`, `checkGHESVersionFloorAgainst`) already
establish the precedent of building full detail into the returned `fmt.Errorf` rather than printing
separately and returning a sentinel. Matching that precedent is a smaller, more consistent change than
introducing a new stderr-capture pattern this test file doesn't otherwise use, and it makes R3
("the error answers in full — every required option and which are present") directly assertable via
`err.Error()` in a unit test.

## Consequences

**Positive:**
- #1082's reported configuration — `merge_train` unset/off, a holding stage configured, its column
  missing from the board — now fails startup loudly instead of booting clean and stranding work later,
  at the single least recoverable point in the pipeline (merge time, where dependency edges have already
  cleared on close).
- The startup failure report is strictly more informative for every stage, not just holding stages: an
  operator now sees every required Status option's present/missing state in one place, rather than having
  to cross-reference a "missing" list against a separately printed "all columns found" list.
- No behavior change for `merge_train: on` installs — removing the exemption is a strict superset of
  required columns (never fewer), so `merge_train: on` startup behavior is unaffected by construction.

**Negative / Trade-offs:**
- An install that has added a `holding_stage: true` entry to `.fabrik/stages/` (e.g. by copying
  `queued.yaml`) but has not yet added the matching board column will now fail startup even with
  `merge_train` off — previously this booted clean. This is the intended behavior change (R1/R2 of the
  issue), not a regression: such an install was always one config flip away from silent stranding, and
  boot time is the only moment this is cheap to fix.
- The returned `error`'s message is now considerably longer (a multi-line report rather than a one-line
  sentinel). This is deliberate per R3, but any caller that logs `err.Error()` inline (rather than
  treating it as an opaque failure signal) will see a larger message than before.

## Related Work

- [ADR-1072: Holding Stages Are Reachable Only Via Dedicated Code, Not Positional Advance](1072-holding-stage-terminal-advance.md)
  — establishes the runtime reachability analysis this ADR explicitly declines to treat as a startup-time
  validation exemption.
- [ADR-059: Internal Merge Train](059-internal-merge-train.md) — defines the `Queued` holding stage and
  `merge_train` config flag this ADR's fix concerns.

**References:** [docs/USER_GUIDE.md § Merge Train / Queued](../docs/USER_GUIDE.md), #1082
