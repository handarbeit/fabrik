# ADR 1990: A pat leg's engine shares the bed token's user with other beds' harness use

## Status

Accepted (#1990). Amends ADR 2263's role rule for one case; everything else in ADR 2263 and ADR-1976 stands.

## Context

ADR 2263 made an identity's `engine` use exclusive and its `harness` use shared above a budget floor. On a pat leg the engine identity *is* the bed token's user, which the other bed also uses as harness. So under ADR 2263 a pat cell on bed B could not start until bed A's baseline (`app/on`, about 3 hours) released that user. Only `app/off` overlapped the baseline.

The v0.0.85 two-bed gate (engine `75ce7aa`, 2026-10-14) measured the effect:

- **Saving:** about 15% over a single bed (4h07m against 4h51m serial), below #1990's 30% bar.
- **If pat cells overlapped the baseline:** about 35% (about 3h08m).
- **Budget:** arbeithand never fell below 4,121 of 5,000 as harness for both beds, nor below 4,206 as pat engine plus harness. The 0.0.84 gate's worst pat leg left 76%. A pat engine plus the other bed's harness traffic projects to roughly 1,500–2,000 points an hour.

ADR 2263 made the engine use exclusive as a cautious default. The budget data doesn't call for it.

## Decision

1. **A pat engine shares.** An identity that is the engine on a pat leg and also the bed token's user (`Identity.SharesHarness`) is held as a *shared engine*. Other beds may hold the same user as harness at the same time, above the ADR 2263 budget floor (`E2E_SHARED_IDENTITY_FLOOR`). The floor check applies in both directions: a harness use joining a shared engine, and a shared engine joining harness uses.
2. **Two engines never share a user.** A second pat cell on the same user waits for the first, so two bed engines never poll as one user.
3. **App installations stay exclusive.** An `app:<installation id>` engine identity is never shared.
4. **Reservations carry the use.** A reservation blocks another bed only where the two uses conflict: an exclusive use conflicts with everything, and a shared engine conflicts with another shared engine.

## Consequences

- With the documented setup (bed B on its own App, same harness user), bed B's `app/off`, `pat/on` and `pat/off` can all run beside bed A's baseline. The expected saving is about 35%, to be confirmed by the next two-bed run.
- Bed B's pat engine and bed A's harness draw on one 5,000/h budget at once. The floor and RUN INVALID handling from ADR 2263 are the protection. A RUN INVALID on the shared user stops every cell that charges it, in any role.
- Self-recognition is unaffected. Each bed's engine watches only its own board and repos, so the other bed's harness comments, also authored by the shared user, are never seen by it.
- Seam: `Gate.patEngineShareDisabledForTest` restores ADR 2263's rule. Scheduler tests that need an exclusive conflict for other mechanics (cancel, reservations, stopping an idle engine, RUN INVALID) set it, and `TestMultiBedPatEngineSharingIsVacuousWithoutIt` shows the new tests fail without the change.
