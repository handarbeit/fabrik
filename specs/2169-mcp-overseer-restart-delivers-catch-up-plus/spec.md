## Problem

For an overseer session that *reacts* to pushes, today's channel stream has the wrong shape across a daemon restart, and its default is too raw. Observed in the in-situ MCP session on 2026-10-11 and discussed with the maintainer.

- **Restart replays are noise, and dangerous.** Every replayed label looks like a trigger to a reacting session. They cost about 400 messages of context per restart, and they bury the real events. #2123's baseline rule removes the false label deltas. It does not define what a restart *should* deliver.
- **Changes made while the daemon is down are lost.** During the 18:40–18:41Z downtime, `fabrik:paused` / `fabrik:awaiting-input` were removed from #2120 and #2123. Neither removal was ever pushed: the restart treats the first observation as a baseline, so a real change made during downtime is indistinguishable from a replay.
- **The catch-up snapshot is the right model, but only fires on first attach.** Resubscribing produced exactly five `catch_up=true` messages: #1342 awaiting-input, plus `validate-settled` for #1805, #2091, #2116 and #2128. That is the whole "what needs you" picture. A reconnect after a restart gets nothing comparable.
- **Raw label events are the default.**
  - An overseer acts on semantic facts, not on label churn. The facts it acts on: settled, unsettled, pending decision, paused with a cause, merged, column advanced, retry starved, poll stalled.
  - Label events are the noisiest category, and the actor is often `unknown` (ADR 2125).
  - Label pushes are not a readiness signal (see #2120's R-unsettle-2).
- **A re-settle does not say why it happened.** When an item already at Validate-complete is switched from `fabrik:cruise` to `fabrik:yolo`, a second `validate-settled` arrives for the **same `head_sha`**. It differs from the first only in `autonomy` and `next`. There is no cause, actor or via, so a subscriber that has narrowed its feed to semantic events cannot tell that a human made the merge decision; it reads as a fresh settle. Seen on #2123, #2178 and #2138 (in-situ MCP test, 2026-10-12 00:18Z onward, daemon `dev(9ede632)`).

## Requirements

- **R1 — Restart sequence.** After a daemon restart, an attached or reattaching subscriber receives exactly the following, in order:
  1. `daemon-reachable`;
  2. one `catch_up` snapshot of the current "needs you" picture, on **every reconnect** after a daemon restart, not only the first attach;
  3. real deltas for changes that happened while the daemon was down.

  It receives no replay of unchanged state.
- **R2 — Downtime deltas.** The daemon persists the last-published per-item state it needs (labels and column at minimum) beside the hub's state. On start, it diffs the first full observation of each item against that persisted state, rather than treating it as a baseline:
  - a difference is a real event, with actor `unknown` unless known;
  - an item with no persisted state is a baseline.

  This is the persisted counterpart of #2123's in-memory `LabelsKnown` rule.
- **R3 — Semantic events by default.**
  - A new subscription without an explicit `events` list receives the semantic event family and not raw `label-applied` / `label-removed`. Raw label events stay available by naming them.
  - The ADR lists the default family and decides how existing persisted subscriptions are treated (grandfathered or migrated).
  - The `fabrik_subscribe` description says label events are opt-in and not a readiness signal.
- **R3a — Provenance on every push.**
  - Every event carries `source=live|catch_up|downtime-delta`. There is no `replay`: R1 forbids replays.
  - A label or column event derived from a reconcile or probe that the daemon cannot attribute to a known write or webhook carries `unverified=true`, so a phantom such as the reconcile-race pushes (A9) can be told apart. `cache-divergence` is in #2170.
  - The actor uses the ADR 2125 vocabulary (`fabrik`, `overseer` with `via`, `human`/`bot` with `actor_login`, `unknown`).
  - **Re-settle cause.** Every `validate-settled` after the first in an episode carries `cause=autonomy-changed | revalidated | new-head`, so a subscriber on a semantic-only feed can tell why the item settled again.
    - The first settle of an episode carries no cause, or `cause=initial`; the ADR picks which.
    - `cause=autonomy-changed` also carries the actor of the autonomy-label write (for example the `fabrik:cruise` to `fabrik:yolo` swap), in the ADR 2125 vocabulary: `fabrik`, `overseer` with `via`, `human`/`bot` with `actor_login`, or `unknown`.
- **R3b — Catch-up is the situation digest.** The catch-up snapshot in R1 is the `situation` digest from #2171 once that lands. Until then, it is today's catch-up builder.
- **R4 — Docs.** Cover the restart sequence, downtime deltas, the default family and the re-settle cause in `docs/state-machine.md` §7.14 and in `docs/USER_GUIDE.md`, regenerate `docs/llms-full.txt`, and add an ADR.

## Scope

In scope:
- restart and reconnect delivery;
- the persisted last-published state;
- the default subscription family;
- provenance on pushes, including the cause and actor of a re-settle.

Out of scope:
- the in-memory baseline rule itself (#2123);
- the settled/unsettled events (#2120);
- shim/daemon skew (separate issue).

## Acceptance

- **Restart sim:**
  - Several labelled items, with a subscriber attached across a restart.
  - While the daemon is down, one item's `fabrik:paused` is removed and another moves column.
  - After the restart the subscriber receives `daemon-reachable`, one catch-up snapshot, then exactly those two deltas, and nothing else.
  - It is shown non-vacuous against the pre-change behaviour.
- **Default family:** a subscription created with no `events` receives `validate-settled` but not `label-applied` for a label change. Naming `label-applied` restores it.
- **Re-settle cause:**
  - A sim test settles an item on `fabrik:cruise`, then swaps it to `fabrik:yolo` by a human label write.
  - It asserts that the second `validate-settled` has the same `head_sha`, `cause=autonomy-changed` and `actor=human`.
  - It is shown non-vacuous against the current behaviour, where the second settle carries no cause or actor.
- `go test -race ./...` is clean.

Mentioned for context: #1966 (the human feature thread). Mention it by bare number only in the PR body. Do NOT use a closing keyword (`Closes`/`Fixes`/`Resolves`) near #1966.
