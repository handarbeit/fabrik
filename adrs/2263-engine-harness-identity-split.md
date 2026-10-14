# ADR 2263: Multi-bed identity scheduling — engine use is exclusive, harness use is shared above a budget floor

## Status

Accepted (#2263). Amends ADR-1976 §2 (identity sets) and §4 (what a shared identity serializes); everything else in ADR-1976 stands. Builds on ADR-1975 (the sparse plan) and #1678 (the GraphQL budget numbers). **ADR-1454 is unchanged:** no live scenario moves to the sim. The live two-bed certification run, and standing up the second bed, are #1990.

## Context

ADR-1976 made the GitHub identity the scheduling unit and had every cell hold its whole identity set exclusively for the whole leg: its engine identity plus the user of the harness token. Both beds' harness token is `@arbeithand`, and GitHub's terms allow one free machine account per person, so a second machine user is not an option. Bed A's baseline therefore held `user:arbeithand` for its 3h18m, every cell bed B could run needed it too, and a two-bed gate took as long as a one-bed gate.

The exclusivity protects each identity's 5,000/h GraphQL budget. The v0.0.84 gate's numbers (#1678) show it is far stronger than the budget needs for *harness* use: arbeithand's lowest point across all four legs was 3,800/5,000 (76%), and that was after `pat/on`, where arbeithand was also the **engine** identity — an engine polls, reconciles and drives every stage, and is the large consumer. On app legs arbeithand is only the harness (the engine spends the App installation's budget), and its spend there is a fraction of that.

## Decision

### 1. Each identity in a cell's set has a role

- **`engine`** — the identity the bed engine authenticates as on this leg: `app:<installation id>` on an app leg, the bed token's `user:<login>` on a pat leg.
- **`harness`** — the bed token's user, when it is not also the engine identity.

On a pat leg the two are the same key; the set holds one entry, `engine` winning the collision (its budget line says `engine+harness`). An engine holder is exclusive: nobody else may hold the key in any role, so two engines never share an identity. A harness holder is shared: any number of cells may hold the key as harness at once, but a harness holder excludes an engine holder. A pat cell therefore waits while another bed uses its user in any role, and an app cell waits while another bed's pat cell uses the harness user as engine.

The registry (`multiSched.held`) records, per key, at most one exclusive holder or a list of shared holders. Acquisition stays atomic and all-or-nothing under one mutex, so hold-and-wait deadlock stays impossible. `finish` releases exactly the finishing cell's own holds.

### 2. Reservations carry the role

A waiting strict-queue head (bed A's baseline) still reserves its set so a shared-queue bed cannot starve it. The reservation records the head's role per identity: an engine reservation blocks every other bed; a harness reservation blocks only another bed's *engine* use, so harness sharing keeps working while bed A waits.

### 3. A budget floor for shared identities

Before a cell joins an identity that other cells already hold as harness, the scheduler reads that identity's remaining GraphQL budget (`IdentityOps.Budget`, which now also returns the real limit — an App installation's exceeds 5,000 — falling back to 5,000). It joins only if `remaining ≥ limit × E2E_SHARED_IDENTITY_FLOOR / 100` (default 40; `0` disables the floor and its probe). Otherwise the cell waits, logging the identity, what remains of the limit and the reset time.

- The first holder of an identity is never probed, so a one-bed run — and the first cell of a two-bed run — are unchanged.
- An unreadable budget counts as below the floor for a second holder. It is never a failure.
- `next()` stays pure and holds the scheduler lock with no I/O. When a join needs a reading, it returns the identities to probe; the bed's loop reads them outside the lock and calls `next()` again. A reading is valid for one **epoch**; the epoch moves on every acquire and every release, so a decision always rests on a reading taken after the last state change and one reading admits at most one join (the floor is not merely advisory).
- A bed waiting on the floor wakes on the next leg finishing (the release moves the epoch) or after `SharedIdentityReprobe` (5 minutes), whichever is first. Its engine is stopped while it waits, like any other wait.

### 4. Logging

Wait lines say why: `needs user:x exclusively, held by bed A (app/on) as harness`, `needs user:x (harness), held exclusively by bed A (pat/on)`, `…, reserved for bed A's next cell`, or `shares user:x (harness) with bed A (app/on), but its budget is below the floor: 1500 of 5000 remaining (floor 40%), resets <time>` (or `could not be read (<err>)`). A repeated identical wait is logged once; a changed reason or reading logs again. The `schedWaitHook` test seam takes the reason (`exclusive`, `reserved`, `floor`). Each leg's start and end budget lines mark the role: `== identity budget (leg: app/on, start): user:arbeithand (harness) — 4210 remaining, resets … ==`, with `(engine)`, `(harness)` or `(engine+harness)`. That marker is the only change to a single-bed run's output.

### 5. RUN INVALID

Unchanged. The `exhausted` set is keyed by identity, so an exhausted shared identity stops every cell that charges it in either role. As before, only the failing cell's *engine* identity is marked: an app leg's RUN INVALID exhausts the App, not the harness user.

## Alternatives rejected

- **A second machine user for bed B.** Against GitHub's terms (one free machine account per person).
- **Sharing with no floor.** The #1678 numbers leave headroom, but they are one run's; a floor makes the sharing self-limiting when a leg is unusually expensive, at the cost of one `rateLimit` read per second holder.
- **Probing inside the scheduler lock.** It would hold every bed's scheduling still across a network call. The epoch-versioned reading gets the same guarantee without it.
- **A fixed 5,000 limit for the floor.** Wrong for an App identity; only user keys are shared today, but the real limit costs one extra field.

## Consequences

- With the documented setup (bed B on its own App, same harness user), bed B's `app/off` runs beside bed A's baseline; its `pat/on` and `pat/off` still wait for the baseline to release the user, because a pat cell spends the user as its engine. The wall-clock gain is therefore smaller than with a distinct harness user per bed, and is what #1990 measures.
- Two beds spending one user's budget can still hit RUN INVALID; the floor makes that unlikely, and `--resume` recovers any lost coverage.
- The neutralisation seam `Gate.roleSplitDisabledForTest` treats every identity as `engine` (the pre-#2263 rule); the scheduler tests use it to show they fail without the split.
