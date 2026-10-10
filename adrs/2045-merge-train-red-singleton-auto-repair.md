# ADR 2045: Merge-Train Red-Singleton Auto-Repair

**Date**: 2026-10-10
**Status**: Accepted
**Issue**: #2045 — merge-train: auto-repair a red singleton through Validate instead of pausing for a human
**Amends**: [ADR 1545](1545-red-singleton-reroute-before-pause.md) (the "reroute + pause" decision, §2/R3)

## Context

ADR-1545 made `ejectRedSingleton` reroute a member whose own combined Validate is red off `Queued` to
the stage before it (normally Validate) and then **pause it for a human**. It chose the pause because a
standalone combined-Validate failure has no persistent re-detection signal: the failure was only ever
observed on the synthetic trial branch, never on the member's own already-green PR, and the engine's own
comment is filtered out of "new comment" detection. Rerouting without pausing would have stranded the
member inertly on Validate.

On a busy board that human step became the main reason finished work did not land. Overnight on one
production board 15 PRs landed and five others sat paused until morning, each with a semantic conflict
against something that had just landed. The human fix was always the same: comment with the train's
diagnostic, then apply `fabrik:revalidate`. One member had already caught up and gone green by itself
and still sat paused. Since #2044 a merely *behind* singleton catches up and lands; this ADR covers the
member that genuinely conflicts.

## Decision

### 1. The engine supplies the missing trigger

Instead of waiting for a human to apply `fabrik:revalidate`, `ejectRedSingleton` performs its effect
itself (`reenterValidate`, extracted from `handleRevalidateLabel`, which still delegates to it): it
clears `stage:Validate:complete`/`:failed`, `fabrik:paused`, `fabrik:awaiting-input`,
`fabrik:awaiting-ci`, `fabrik:auto-merge-enabled` and resets the store's Validate retry/cycle/cooldown
state. Validate then dispatches on the next poll as an ordinary dispatch (R5): the train worker never
starts a Claude worker for the repair and never holds a slot for it. The attempt cap (§3) stands in for
the external signal ADR-1545 found missing.

Ordering is ADR-1208's reroute-before-side-effects: a failed reroute posts nothing, counts nothing and
leaves the member in `Queued` for the next poll. Re-entry is *secured* before the attempt is recorded,
the comment posted or the event emitted. If the direct clear fails part-way (the member is by then in
Validate, unpaused, with `stage:Validate:complete` possibly still set — the quiet stranding ADR-1545
warns about), the engine applies the `fabrik:revalidate` trigger label so `settleRevalidateScan` retries
it every poll; the attempt is counted then, so a persistently failing clear cannot loop uncapped. If
both fail the member is paused exactly as before and nothing is counted.

Auto-repair applies only when the reroute target is literally named `Validate` (`reenterValidate` is
hardcoded to it); any other configuration keeps the pause.

### 2. The repair gets the diagnostic, and the instruction to reconcile

The failing checks, the trial head and pinned base SHAs, the member's head at failure and the job-log
excerpts already gathered for `renderDiagnosticBlock` travel from the train worker to the poll
goroutine as an in-memory per-member record. `writeContextFiles` materialises it as
`.fabrik-context/merge-train-repair.md` for a Validate dispatch. The write does not consume the record,
so a retry of that same Validate (incomplete run, turn-limit slice, tools-denied) is given the file
again; the record is dropped when Validate completes (`handleStageComplete`). With no pending
record (any other stage, a Validate after completion, comment processing, or a daemon restart between
eject and dispatch) a leftover file is removed and Validate still runs without it. A pending record that waited
more than 2 hours or whose member head no longer matches the item's PR head is discarded the same way,
so it cannot leak into a later unrelated Validate run. The file's text is
self-contained — base moved, rebase or merge onto it, reconcile with what landed, **never revert or work
around the landed change** — and is also carried by the embedded `fabrik-validate` skill and one static
`buildPrompt` line, so it reaches the worker without users refreshing their stage YAML.

### 3. Cap, per member and per base SHA

`max_train_auto_repair_attempts` (flat key — `merge_train` is already a scalar in `config.yaml`, so a
nested mapping cannot coexist; flag `--max-train-auto-repair-attempts`, env
`FABRIK_MAX_TRAIN_AUTO_REPAIR_ATTEMPTS`; flag > env > config.yaml > default). Default **1**; **0
disables the feature**, including §4, and the pause path is then byte-for-byte today's. An invalid value
warns and uses the default. The default is applied in `cmd`, not an engine helper: the engine cannot
tell an explicit 0 from an unset field, and a directly built `engine.Config{}` therefore keeps today's
behaviour.

Attempts are counted per member per **pinned base SHA of the failing trial** and recorded when
auto-repair is dispatched. A new base SHA is a new count. At or past the cap the member is paused as
ADR-1545 did, and the pause comment lists the attempts made (base SHA, member head, failing check
names, time). The counter lives in memory, like `mergeTrainEjectionCounts`; a restart resets it (at
worst one extra repair per member per base SHA) and is documented as such. It is cleared when the member
lands.

### 4. A member that is already fixed is left alone (R3)

Before rerouting, the engine re-reads the PR and the live `origin/<base>` and reuses
`singletonFastPathEligible` unmodified on a `trainMember` rebuilt from the *live* head (so its
snapshot-equality check passes by construction — the head having moved since the trial is exactly the
point). Head contains the live base and own CI green and complete → the member stays in `Queued` with
no reroute, comment, label or pause. Any API error or ambiguity is "not confirmed" and falls through to
repair or pause. This requeue is allowed once per member per base SHA, tracked separately from the
repair budget; a second red at the same base goes to repair, then to the pause.

### 5. What keeps the human gate

Bisection-isolated poisoners (`ejectMember`, `MaxMergeTrainEjections`), the runaway guard, the
landing-verification escalations (#1615) and every disposition not attributed to a single member alone
are untouched. **`ejectRedCatchUpSingleton`** (#2044: a member red on its own caught-up head) is also
unchanged and still pauses; it is the obvious follow-up candidate, deliberately out of scope here
because the issue scopes only `ejectRedSingleton`.

### 6. Observers

`emitTrainEvent` still emits `merge-train-failed`, now with cause `red-singleton-auto-repair`, a reason
that says repair has started and no human action is needed, and `auto_repair=true`, `attempt` and `cap`
meta. The cause `red-singleton` is emitted only when the member is actually paused.

## Consequences

- Finished work no longer waits for a human when the cause is a semantic conflict with what just landed.
- Each repair is a real Validate run (tokens). The cap bounds repairs per base SHA, **not in total**: a
  member that keeps losing the race against fast-moving bases can be repaired repeatedly. The runaway
  guard and `MaxMergeTrainEjections` remain the backstops for the train itself.
- A worker that "fixes" the failure by reverting the landed change would defeat the point. The
  instruction forbids it; Review and Validate remain the check.
- A restart can allow one extra repair per member per base SHA and drops a pending diagnostic (Validate
  still runs).
- R3 trusts live reads; a red trial against a head that already contains the base and is green is most
  likely flaky or stale, and the once-per-base bound limits the cost to one extra train formation.
- The live red-singleton e2e test asserts the pause, so both bed launch sites start the bed with
  `FABRIK_MAX_TRAIN_AUTO_REPAIR_ATTEMPTS=0`; the repair path is covered by its sim twin.

## References

[ADR 1545](1545-red-singleton-reroute-before-pause.md), [ADR 1208](1208-queued-review-finding-ejection.md),
[ADR 1644](1644-merge-train-singleton-fast-path.md), [ADR 1420](1420-merge-train-ejection-diagnostics.md),
[ADR 2044](2044-singleton-catch-up.md), `docs/state-machine.md` §6.31.
