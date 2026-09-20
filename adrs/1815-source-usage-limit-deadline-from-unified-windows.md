# ADR 1815: Source the usage-limit suspension deadline from `unifiedWindows.*.resetsAt`

## Status

Accepted. Supersedes the "`ResetTime` is not populated (issue R6 dropped)" paragraph of
[ADR-1811](1811-split-api-error-on-http-status.md) (lines 37-49) without rewriting it, and fulfils
the deferred follow-up in [ADR-1183](1183-structural-claude-usage-limit-detection.md) §3.

## Context

A usage-limit exit suspends Claude dispatch account-wide, but Fabrik did not know when the limit
lifts. The only reset time it had ever seen was prose in the CLI's `result` string ("resets 3:30am
(America/New_York)"), which ADR-1183 rightly forbids reading — a prose match once suspended dispatch
for ~11 hours. So the suspension always used `claudeUsageLimitFallbackBackoff` (one hour, ADR-1120),
which is wrong in both directions: far too long after a late detection, irrelevant when the window
is short (#1566's measured 46-minute and 7-minute stalls).

ADR-1811 dropped its R6 (carry a reset time) because, as written, it meant populating `ResetTime`
from `result` prose, and `ResetTime` feeds the deadline. That reasoning does not apply to the CLI's
*structured* reset, identified on #1566: `rate_limit_info.unifiedWindows.<window>.resetsAt`, a Unix
timestamp in a named field. Research over 346 captured `rate_limit_event` transcripts found:

- It is **not** on the `result` line. It is on a separate `{"type":"rate_limit_event"}` NDJSON line
  that precedes it, so `claudeResponse` cannot carry it.
- Only `five_hour` and `seven_day` windows ever appear. On every one of the 346 `rejected` events
  `five_hour.utilization` is exactly `1` (a JSON integer); `seven_day` is 0.49-0.84 and resets days
  later. Top-level `resetsAt` equals the window's `resetsAt` in every event.
- On 429 exits `overageStatus` is `rejected` and never carries a reset; overage fields do not
  participate.

## Decision

1. **Scan the raw stream, only after detection.** `extractUsageLimitReset`
   (`engine/usage_limit_reset.go`) is a tolerant NDJSON scan run once `classifyUsageLimitExit` has
   already fired. The trigger stays purely structural; the scan supplies only the deadline, and can
   never make classification fail (it is independent of `parseClaudeJSON`).
2. **Exhausted windows only.** The reset is the latest `resetsAt` among windows with
   `utilization >= 1`. A naive "latest of all windows" would suspend the account for days on a
   `seven_day` window at 65% while it can resume within hours. Keys are read generically so a future
   exhausted window (e.g. per-model) is honoured without a code change.
3. **Last event wins; no `status == "rejected"` cross-check.** In every captured 429 stream the last
   event is the rejected one. Requiring `status` would couple the deadline to another undocumented
   field; a disagreement degrades safely to the fallback. A malformed line never displaces an
   earlier valid instant.
4. **`ResetTime string` is removed, not overloaded.** `UsageLimitError` now carries `ResetAt
   time.Time` and `ResetFallbackReason string`, so nothing can feed prose into the deadline — R2 is
   structural. `activateClaudeSuspension` takes the error (nil = absent).
5. **One resolver.** `resolveUsageLimitDeadline` applies R3 and R4: zero → fallback (decode-time
   reason: `absent` / `zero` / `malformed` / `no_exhausted_window`); `<= now` → fallback (`past`);
   more than **8 days** ahead → clamped and logged; else exact. 8 days = the 7-day window plus one
   day of margin (the largest observed `seven_day` reset is ~5.3 days out, and it was never the
   exhausted window). A `resetsAt` above 1e11 seconds (millisecond-scale) is malformed, not clamped.
   Every fallback logs its named reason — the silent fallback was the original complaint.
6. **No near-future tolerance.** Any `resetsAt > now` is valid, however short.
7. **Display (R6).** The comment and log carry `(resets <local time>)` formatted from the resolved
   deadline, and only when the deadline was exact or clamped; on fallback no suffix is shown, so
   the text never claims a time Fabrik did not source.
8. **The prose parser is deleted.** `parseUsageLimitResetTime`, `splitUsageLimitResetFragment` and
   `computeUsageLimitResetDeadline` had no production caller once `ResetTime` was gone; keeping
   dead code that parses model text is the foot-gun ADR-1183 warns about.

## Consequences

- A limit window suspends dispatch until the real reset rather than one hour, in both directions.
- Suspension behaviour is otherwise unchanged (R5): `fabrik:claude-limit`, the once-per-episode
  comment, the settle sweeps, `fabrik:clear-claude-limit`, and extend-never-shorten under
  `claudeSuspendMu`.
- **Accepted edge:** a fallback-derived deadline from one worker (`now + 1h`) can extend an exact
  deadline from another that ends sooner than an hour away. It needs two concurrent 429 workers
  with different evidence, and all captured 429 exits carry the event, so it is not handled with
  extra suspension state.
- A CLI that renames the event or moves the field degrades to the one-hour fallback with a logged
  reason.
- Mid-session 429s (real turns and cost) remain undetected by the usage gate; this change does not
  shorten those stalls.
