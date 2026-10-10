# ADR 2050: Merge-Train Phase Hook, Live Row and Episode Outcomes

**Date**: 2026-10-10
**Status**: Accepted
**Issue**: #2050 — live merge-train TUI row (membership, phase, PR, time in phase) and one History entry per train outcome. #2036 is the community report behind it (mentioned for context only).

## Context

#1661 gave the train a TUI job row, with a title built once from the dispatched batch and
"fixed at dispatch" (ADR 1661 §Consequences), plus a completion event that was always
`Skipped: true`. In a real incident two members were ejected at assembly while the row still
said "5 member(s)"; waiting for a slot, waiting on CI, bisecting and a wedged trial all looked
the same; and landed, red, bisected, ejected and dissolved trains left the same trace in
History, or none. #2048 added the board status line, written inline at about 25 call sites, so
the board and the TUI were two uncoordinated descriptions of one train.

## Decision

1. **One hook.** `noteTrainPhase` (`engine/train_phase.go`) is the single "train phase changed"
   call. Each phase site calls it once; it writes the board line through the existing
   `status_line_text.go` builders (wording untouched, so the board's lines, dedupe and count are
   byte-identical — FR-014), records the phase on the episode, and emits one `tui.TrainRowEvent`.
   The hook wraps the existing builders rather than replacing them with a new shared renderer:
   a renderer producing both strings would risk changing board wording, which `TestStatusLine_Train_*`
   pins. A static scan test forbids the phase builders elsewhere in `engine/merge_train*.go`.
   Like ADR 2048, call sites rather than an observer.
2. **Episode state in an explicit pointer.** `trainEpisode` lives on `mergeTrainWorkerState` and
   `trialParams` (a pointer inside the by-value struct), not in an Engine registry, so sibling
   partitions cannot interfere and `ejectMember` (also called from the poll goroutine) is not
   touched. Ejections are recorded at worker-side call sites.
3. **A new `TrainRowEvent`**, not a re-emitted `JobStartedEvent` (which would reset `StartedAt`,
   the row's overall elapsed). It is update-only in the TUI, and sent with `emitStructural`: a
   dropped event would leave a stale phase, defeating the purpose. Transitions are low-frequency
   and sub-trials stay quiet.
4. **One completion, from the existing defer.** Sites only record facts; the single
   `JobCompletedEvent` (`Skipped: false`, `Outcome`, `Detail`, `Success`) is emitted from
   `runMergeTrainWorker`'s deferred completion, which already runs on every exit path. Exactly one
   History entry per episode holds by construction.
5. **Outcome vocabulary and precedence**: `abandoned` > `one-at-a-time` > `red → bisected` >
   `landed` > `ejected at assembly` / `ejected` > `dissolved` > catch-all `ended — nothing landed`.
   `Success` is true except for `abandoned` and the catch-all (adjustable). The engine's main-moved
   `dissolveBatch` is reported as `abandoned` so `dissolved` keeps "nothing to land". "Landed" comes
   from members the integration PR claims, not `survivors`.
6. **Time source** is `time.Now()`, matching the TUI's events and ticks (the engine `Clock` would
   disagree in sim tests that move it).
7. **TUI**: `HistoryEntry` gains additive `Outcome`/`Detail` JSON fields (an empty `Outcome`
   renders exactly as before); the row keeps the phase and its elapsed when narrow, dropping the
   log line then shortening the title; History `l`/`r` ignore `#0` train entries.

## Consequences

- Supersedes ADR 1661's "title fixed at dispatch": the title is live. Its emit-before-prepare
  rule and the single deferred completion stand.
- Sibling partitions still share the repo-keyed row (display-only caveat unchanged); each emits
  its own History entry.
- A restart mid-train writes no entry for the interrupted run.
- Neutralisation seam `SetTrainOutcomeNeutralisedForTest` restores the blanket `Skipped`
  completion; the outcome tests are shown to fail under it.
- No new label, flag, config key or GitHub write. The channel-push `emitTrainEvent` consumer is
  untouched.

## References

- ADR 1661, ADR 2048, ADR 1648, ADR 2046, `docs/state-machine.md` §7.17.2, `docs/USER_GUIDE.md` §8
