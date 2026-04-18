# ADR 028: Two-Threshold Hysteresis for Rate-Limit Backoff

**Status**: Accepted  
**Date**: 2026-04-18

## Context

Fabrik's rate-limit backoff activates when GraphQL API remaining quota drops below 20% of the hourly limit, doubling the poll interval to conserve remaining budget. The problem: the backoff was reset to the base poll interval whenever "activity detected" fired — which happens when any item's `updatedAt` changes on the board.

On a busy 92-item board, something changes frequently enough (bot comments, label events, PR updates) that activity detection fires every few minutes. The resulting cycle:

1. GraphQL quota drops below 20% → rate-limit backoff activates
2. Quota window resets (hourly) or quota recovers slightly above 20%
3. Some item's `updatedAt` changes → "activity detected — idle backoff reset, poll interval restored to 15s"
4. 15s polling burns through the fresh quota → back to step 1

This produced three backoff/reset cycles per hour on the widgets-project board (92 items), with the backoff providing only ~20 minutes of relief per hour while 15s polling dominated the remaining 40 minutes.

The root cause was conflating two independent backoff concerns:
- **Idle backoff**: increases poll interval when nothing to do; should reset on activity.
- **Rate-limit backoff**: increases poll interval when quota is low; should reset only when quota is genuinely healthy.

## Decision

### Two-Threshold Hysteresis

Replace the single-threshold rate-limit state flip with a two-threshold hysteresis function (`nextRateLimitLow`):

- **Activate**: when remaining quota ratio < **20%** and not already in backoff
- **Clear**: only when remaining quota ratio > **50%** and currently in backoff
- **Between thresholds (20%–50%)**: state is sticky — no change

The 20%/50% gap is the "hysteresis band". Within this band, the backoff state doesn't change regardless of quota fluctuations. This prevents the thrashing pattern where partial quota recovery (e.g., 10% → 25%) immediately cleared the backoff and resumed aggressive polling.

### Separation of Concerns

Activity detection resets idle backoff (correct existing behavior) but no longer touches rate-limit backoff. The two backoff components are independently maintained in `doPollCycle` and combined via `computeEffectiveInterval` (unchanged): `max(idle interval, rate-limit interval)`.

When rate-limit backoff is active, Fabrik logs the effective poll interval each cycle (R4 observability).

### Alternatives Considered

**Single threshold at a higher value (e.g., 35%)**: Would activate and clear at the same point. Still susceptible to quota fluctuations near the threshold — any polling that burns quota back down to 34% would re-activate the backoff, oscillating. The hysteresis band eliminates this.

**Time-based sticky backoff (hold for N minutes after activation)**: Ignores actual quota state. If quota recovers to 80% after an hourly reset, there's no reason to stay backed off for the remaining hold period. Quota-driven clearing is more accurate than time-driven clearing.

**Exponential backoff with separate timer**: More complex. The existing 2× rate-limit interval is sufficient for the problem — the issue was not the backoff magnitude but the premature clearing. No timer needed.

## Consequences

- Rate-limit backoff is now sticky between 20% and 50%, preventing thrashing on busy boards.
- Three Fabrik instances sharing a PAT now each back off independently based on their own quota observations, but the hysteresis prevents the rapid re-activation that was consuming quota faster than the backoff could help.
- Operators can observe rate-limit backoff state in the log stream by searching for `"rate-limit backoff active"`.
- The healthy threshold (50%) is a constant (`rateLimitHealthyThreshold`). Future tuning is possible without changing the hysteresis logic.
