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

**This describes the initial cut's design only.** See the Addendum below: before merge, PR review found
the unconditional hard-fail breaks the default `fabrik init` onboarding path, and the shipped behavior
splits *severity* from *membership* — the column stays in the required set unconditionally as described
above, but a missing one is fatal only when `merge_train: on`; off/unset produces a startup warning
instead, and Fabrik still boots.

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
  missing from the board — no longer boots silently clean: Fabrik now surfaces a startup warning naming
  the column, closing the exact gap #1082 reported (startup previously emitted only the unrelated
  extra-columns warning, so the board appeared validated). See the Addendum below: the severity for this
  specific configuration is a warn-and-boot, not a hard fail — only `merge_train: on` is fatal. The
  guarantee this change actually adds is that the gap can no longer go *unnoticed*, not that it can no
  longer occur while off.
- The startup failure report is strictly more informative for every stage, not just holding stages: an
  operator now sees every required Status option's present/missing state in one place, rather than having
  to cross-reference a "missing" list against a separately printed "all columns found" list.
- No behavior change for `merge_train: on` installs — removing the exemption is a strict superset of
  required columns (never fewer), so `merge_train: on` startup behavior is unaffected by construction.

**Negative / Trade-offs:**
- The returned `error`'s message is now considerably longer (a multi-line report rather than a one-line
  sentinel). This is deliberate per R3, but any caller that logs `err.Error()` inline (rather than
  treating it as an opaque failure signal) will see a larger message than before.
- See the Addendum below: the original text of this bullet claimed a missing holding-stage column would
  fail startup even with `merge_train` off. That was corrected before merge — see Addendum for why, and
  for the actual (severity-split) behavior.

## Addendum (2026-08-08): Severity Is Keyed to Reachability, Not to Configuration

The initial cut of this fix (described above) removed the `merge_train`-conditional exemption entirely,
making a missing holding-stage column **fatal unconditionally**, whenever the stage is configured — matching
every other non-cleanup, non-unmanaged stage. PR review caught that this breaks a large class of installs
that were working correctly before this change, and this addendum documents the correction, made in the same
PR before merge.

### The breaking case

Three facts compose:

1. `fabrik init` (`cmd/init.go`) extracts **every** embedded default stage unconditionally — including
   `queued.yaml` — with no prompt and no `merge_train` conditional. This has been true independent of this
   issue; it did not change here.
2. `merge_train` defaults to **off** (`mergeTrainMode` in `cmd/root.go` normalizes an unset value to `"off"`).
3. The initial cut of this fix made a configured holding stage's column fatal-if-missing unconditionally.

Composing these: any operator who ran `fabrik init`, left `merge_train` at its default, and never created a
`Queued` column — the default onboarding path, not an edge case — would hard-fail at startup after upgrading
to this fix. The column is one they had no reason to create: nothing in their configuration could reach it
(per ADR-1072, `advanceToQueued` is the only write path and it is itself gated on `merge_train: on`). Under
`--auto-upgrade`, this converts a working, unattended daemon into a non-booting one with no operator present
to notice.

The mitigation that would have made this safe — `fabrik init` surfacing which board columns it requires
before the operator finishes setup (#1359) — is milestoned separately and does not ship in the same release
as this fix.

### The correction: split severity from membership

The holding-stage column remains part of the **required set** whenever the stage is configured — R1's
"membership is derived from reachability, not from `merge_train` mode" still holds as a description of what's
*required*. What changes is that **severity**, not membership, is what's actually keyed to reachability:

- **`merge_train: on`** → a missing holding-stage column is **fatal**, exactly as the initial cut of this fix
  implemented. The column is live (the only write path is armed) and the stranding risk described in the main
  body of this ADR is real and immediate.
- **`merge_train` off/unset** → a missing holding-stage column is **not fatal**. Fabrik boots and emits a
  startup warning instead, naming the column, stating its terminal-only role (R4, unchanged), and stating
  that it must exist before `merge_train` can be enabled.

This preserves the entire safety argument the main body of this ADR makes: the risk being guarded against is
"an operator flips `merge_train` on against a board that was never validated for it," and a warning emitted
on every startup while off reaches that operator before they can do that — it does not require them to have
already read the warning to be protected, since flipping the flag on is exactly the transition that converts
the warning into the fatal check. Nothing about the stranding-prevention goal is weakened; what's removed is
treating "the stage is configured" as sufficient justification for failing startup on its own, when the
thing that actually determines whether the column can strand anything is whether the write path is armed.

This is also the same severity model `checkStageColumnAlignment` already uses elsewhere in the function:
a required-but-missing non-cleanup, non-unmanaged column is fatal; an extra column with no matching stage is
a warning. A column that is required (by configuration) but not currently reachable (by mode) is a third,
now-explicit case, and warn is its natural severity — it was previously conflated with the fatal case by
the initial cut of this fix.

### Does `fabrik upgrade` or `refresh-stages` also introduce `queued.yaml` into installs that never had it?

No, confirmed by reading both:

- `cmd/refresh_stages.go` (`refreshStagesWithReader`) only reads `entries, err := os.ReadDir(stagesDir)` —
  it iterates **existing** files in `.fabrik/stages/` and matches each by its own `name:` field against the
  embedded defaults (`idx, ok := defaultsByName[name]; if !ok { continue }` — an unmatched name is silently
  skipped as a custom stage). It only ever adds missing top-level *keys* to a file that already exists by
  that name; it never creates a new stage file. An install that never had `queued.yaml` cannot acquire one
  via `refresh-stages`.
- `cmd/upgrade.go` does not touch `.fabrik/stages/` at all — its only mention of stages is a printed
  suggestion to run `fabrik refresh-stages --apply` manually.

So the only path that introduces a holding stage into an install's configuration is `fabrik init` writing
`queued.yaml` on first bootstrap (or `--force` re-extraction) — which is universal, since every install must
run `init` at least once. This is why the breaking case above is the default onboarding path, not a narrow
edge case, and why the severity split (rather than some upgrade-path-specific guard) is the correct fix: it
has to hold for every install, because every install is affected by the same `fabrik init` behavior.

## Related Work

- [ADR-1072: Holding Stages Are Reachable Only Via Dedicated Code, Not Positional Advance](1072-holding-stage-terminal-advance.md)
  — establishes the runtime reachability analysis this ADR explicitly declines to treat as a startup-time
  validation exemption.
- [ADR-059: Internal Merge Train](059-internal-merge-train.md) — defines the `Queued` holding stage and
  `merge_train` config flag this ADR's fix concerns.

**References:** [docs/USER_GUIDE.md § Merge Train / Queued](../docs/USER_GUIDE.md), #1082
