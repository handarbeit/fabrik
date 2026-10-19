# ADR 1995: Handle the issue_dependencies webhook event

## Status

Accepted

## Context

GitHub delivers an `issue_dependencies` webhook (actions `blocked_by_added`, `blocked_by_removed`, `blocking_added`, `blocking_removed`) when a "blocked by" / "blocking" link changes. Fabrik dropped it silently, so a link added in GitHub's UI reached the engine only at a later poll's deep fetch. On 2026-10-15 that let an item dispatch Plan and Implement while it should have been blocked.

Captured real deliveries (`boardcache/testdata/webhooks/issue_dependencies_*.json`) establish the facts the design rests on:

- One change yields **two** deliveries, one per side. `blocked_by_*` carries `blocking_issue_repo`, `blocking_*` carries `blocked_issue_repo`; the issue objects have no `repository_url`, only `html_url`.
- Embedded objects (`issue_dependencies_summary`, `sub_issues_summary`, `state`) reflect the issue when GitHub rendered the delivery, which can be after the action. They are unordered with respect to the action.
- **Closing a blocker sends no `issue_dependencies` delivery**; only `issues` `closed` arrives.

Getting an item re-evaluated promptly needs three separate gates to be passed, and fixing one or two does nothing:

1. **Freshness**: the deep cache must be invalid or the next evaluation reads the old `BlockedBy`.
2. **Wake and cycle set**: `DeepFetchChanged` is in neither `wakeChFlags` nor `cycleSetFlags`, so invalidation alone wakes nothing and admits nothing.
3. **Blocked-item cooldown**: `itemMayNeedWork` suppresses a `fabrik:blocked` item until its `dep-blocked` cooldown expires, so a wake alone does nothing for the very case that matters (an unblock).

## Decisions

1. **Invalidate, never patch.** The handler invalidates the two named issues' dependency data and never applies counts or issue objects from the payload. The live `BlockedBy` read in `checkDependencies` (ADR-1419) stays the only authority for adding or removing `fabrik:blocked`; the delta path writes no label. A stale, partial or reordered delivery costs at most one extra evaluation.
2. **One new Store mutation, `DependenciesInvalidated`, passes all three gates.** It zeroes the deep-fetch baselines (as `DeepFetchInvalidated`), deletes the `dep-blocked` cooldown of an item carrying `fabrik:blocked`, and returns a new flag, `DependenciesInvalidatedChanged`, which is added to `wakeChFlags` and `cycleSetFlags`. The cooldown is *deleted* rather than set to an already-due time: that is independent of the engine clock (the sim and production clocks differ from a webhook's receipt time), and an absent cooldown admits the item exactly as an expired one does. The `"dep-blocked"` key becomes `itemstate.DepBlockedCooldown`.
3. **A dedicated flag instead of putting `DeepFetchChanged` in the wake sets.** `DeepFetchInvalidated` has seven other call sites (new review comment, PR auto-heal, linkage drift); widening its flag would change all of them.
4. **Idempotence is the de-duplication.** If the deep cache is already invalid and there is no cooldown to drop, the mutation returns no flags, so the second delivery of one change causes no extra wake or invalidation. No time-based coalescing.
5. **Resolve each side from its own data.** A side's repository comes from its issue's `html_url`, cross-checked against that side's `*_issue_repo.full_name` when present; a disagreement or an unreadable side is skipped and logged, and the other side is still handled. The top-level `repository` is never read: no capture shows what it means for a cross-repository edge (all four captures are same-repo; cross-repo behaviour is covered by synthetic payloads in tests only and is unverified against GitHub). Every lookup is keyed on (repo, number).
6. **Cached items only, no network.** The handler `Peek`s the Store and ignores an uncached issue; it does not use `ensureIssueInStore`. No Store call is made while the cache lock is held.
7. **Blocker close is a separate observer.** `DependentRefreshObserver` applies the same mutation to every open item whose cached `BlockedBy` names a closing issue, using the Store only. `PushUnblockObserver` is left unchanged: it acts only on items already labelled `fabrik:blocked` and removes the label directly, while the new observer covers unlabelled dependents and makes the item re-evaluate through the live read.
8. **`deliveryKey` keys the event on the blocked issue** (`repo#N`), so it serialises with that issue's other deliveries; the blocking side is in another queue. Ordering is safe either way because the mutation is idempotent and the live read decides.
9. **PAT mode: not in the default `gh webhook forward` list.** Whether `gh webhook forward` accepts `issue_dependencies` was not confirmed against a live forwarding session (that would create a real webhook on a real repository, and only `projects_v2_item` has a graceful rejection fallback). The default list is unchanged and dependency changes stay poll-driven in PAT mode; an operator can opt in with `--webhook-events`. The documentation says so. Adding it to the defaults, with the `projects_v2_item`-style rejection fallback generalised, is a follow-up once a live session confirms support.

## Consequences

- A link added or removed in GitHub's UI is acted on at the next wake under App/Hookdeck delivery, instead of after a poll interval or the 10-poll `dep-blocked` cooldown.
- Closing a blocker now re-evaluates every dependent that lists it, including ones not yet labelled blocked, at the cost of one deep fetch each.
- A new `ChangeFlags` bit exists and is in the wake sets; `observers_test.go`'s pinning test and `docs/state-machine.md` §9.9 list it.
- The cooldown deletion means a delivery for a `fabrik:blocked` item whose blockers are all still open costs one evaluation and re-stamps the cooldown; this is bounded by the delivery rate.

## References

#1995; ADR-1419 (live re-read), ADR-041 (push unblock), ADR-2351 (intake), ADR-2348 (observed-at ordering: `DependenciesInvalidated`, like `DeepFetchInvalidated`, carries no time and always applies). `docs/state-machine.md` §2.5 and the webhook table.
