# ADR 2417: Bed wait timeouts are launch-injected from one definition; negative windows are anchored at the arming event

## Status

Accepted.

## Context

The live e2e gate's wall-clock is dominated by waiting (about 8 h per leg). Some of it is
evidence-free: the bed `.env` files did not set the engine's review or CI wait timeouts, so
production defaults (15 / 30 min) applied; tests read the bed `.env` and fell back to a
harness default, so a bed and its tests could disagree; and negative-observation windows were
padded by a flat 10 minutes and measured from a later observation than the event that arms
the bug (`TestReviewAuthorityDeclaredBotDoesNotDeferHumanEscalation` waited `T+10` from after
the review-reinvoke, although its comment said it was timed from the `fabrik:awaiting-review`
application). The bed `.env` files are operator-owned, outside the repository.

## Decision

1. **One definition, launch-injected.** `tests/e2e/bedtimeout` (untagged, stdlib-only, like
   `internal/pollctl`) holds the review and CI wait timeouts. Both launch sites
   (`tests/gate/bed.go` `BedStartCmd`, `tests/e2e/lifecycle.go` `StartFabrikTestBed`) inject
   `bedtimeout.Env()`; tests read `bedtimeout.ReviewWait()`. No test parses the bed `.env` for
   them.
2. **The launch value wins** over an operator's `.env` value, because `.env` loading never
   overrides an already-set process variable. The alternative (fail preflight on mismatch)
   needs the engine to expose its effective value, which is an engine change, and adds a
   failure mode for no gain.
3. **Values.** Review 10 min (the floor is the slowest legitimate review wait on the bed:
   the cycle-limit race, the Yolo block-persist window, Pruefer latency under Phase 2 at 2T).
   CI 30 min, pinned and not shortened: it is a liveness-stall dwell (ADR 1410), no live test
   waits it out, and the 25-min `slow-gate` check is the shape it cannot tell from a stall.
4. **Negative windows** are armed at the event that arms the bug (read from the events log)
   and, where the correct engine takes a visible escalation at the same instant the bug would
   fire, end at that positive observable with a failure ceiling of
   `armed + timeout + worker allowance + 3 × bed poll` (`bedtimeout.Window` / `Watch`). Margins
   are poll-derived, not flat.
5. **The poll seam (ADR-1978) is not adopted** for the review tests: their gate is a
   wall-clock comparison that held polls cannot advance, and `exclusive` costs the shared
   phase's parallelism.
6. **Reporting.** `tests/e2e/registry/wallclock.json` classifies tests by dominant wall-clock
   category and `gate report` joins it as a column. It is separate from `registry.json` so it
   can never change the gate's required set.

## Consequences

* A bed started by hand (not through the gate or the harness) keeps the operator's `.env`
  value while the tests assume the shared one; the engine does not report its effective value
  to check against. The gate is the supported launcher (documented in `tests/e2e/README.md`).
* The review timeout is one constant; a gate run that shows a loss at 10 raises it.
* Real-work time (Claude, CI, trial CI, the test repos' `slow-gate`) is untouched; the
  projected saving is modest (about 24 min per leg) and recorded in
  `tests/e2e/WALLCLOCK_AUDIT.md`.
* No engine behaviour changes; no live scenario moves to the sim (ADR 1454).
