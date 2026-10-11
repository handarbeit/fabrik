# ADR 2120: A "Suspended" attention state for pending decisions, with an honest age, one push per episode and withdrawn settlements

Status: accepted (issue #2120; amends [ADR-1966-a](1966-a-fabrik-mcp-read-api.md) and [ADR-1966-b](1966-b-fabrik-mcp-channel-events.md), parent initiative #1966; builds on [ADR 2125](2125-channel-label-event-actor-attribution.md) and [ADR 2126](2126-overseer-advance.md))

## Context

The overseer's question is "what needs me?". For the commonest human decision point — a stage has completed, the item has no autonomy label and nothing will move until a human advances it — `fabrik_board view=attention` answered wrongly. An in-situ session on 2026-10-11 found ten Specify items waiting on a human, one listed:

- A completed-stage item fell through to the classifier's last branch and became `stalled` (`stage-complete-not-advanced`) only once `--stall-threshold` had elapsed since the **daemon started**: the progress anchor was `StatusEnteredAt`, which is reset to "now" on every restart (ADR-1833). So the five items were invisible for 30 minutes after each restart, then reported as "31m" for a wait of 68 days, in a class (`stalled`) that reads as an engine fault.
- The `periodic-re-eval` cooldown flipped them `stalled → waiting → stalled`; each return was a "new transition" and re-published, at an interval matching no configured threshold.
- Items blocked by issues parked in Backlog stayed `waiting` ("resumes when they close") although nothing would ever close them.
- `next_action` presented the periodic cooldown's expiry as something that would happen.
- A human setting `fabrik:yolo` on an item settled at Validate under cruise read as a 38-minute stall seconds later, because the age counted from Validate completion.
- A `validate-settled` push was never withdrawn when main moved and the PR needed a rebase, and label events (`stage:Validate:complete` re-applied while the PR was DIRTY) look like readiness but are not.
- Pause events gave a generic "a human comment resumes it" that contradicts many engine pause comments ("Remove `fabrik:paused`").

## Decisions

### 1. `suspended` is a new, derived state, ranked with `needs-human`

`internal/attention` gains `State` `suspended` with two codes: `decision-pending` (a completed stage that does not auto-advance) and `parked-blocker` (blocked only by open work parked in an unmanaged column, directly or transitively). It is derived, never a label. It shares rank 0 with `needs-human`; within that rank the attention view lists a pause that needs an answer before a decision that waits. It sits after the cruise-at-Validate and `api-key-helper-detected` rows and **before** the wait rows, so a cooldown can no longer flip it between `waiting` and `stalled`; it is never `stalled`.

The word "Suspended" is used only for this state: #2129 renamed the Claude usage-limit hold, and the generic uses in `plugin/fabrik-workflows/LABELS.md` were reworded ("stop", "block").

**Masks.** The state requires that nothing the engine owns is going on: no worker, no `fabrik:paused`, no in-flight marker (`fabrik:editing`, `fabrik:locked:*`, `stage:*:in_progress`), and no state-bearing wait label (for case (b), `fabrik:blocked` is the parking label and is exempt). For (a) it also requires no unprocessed feedback. **Cooldowns never mask**: a cooldown can only mean "re-check later". Unknown data fails closed to the old behaviour.

**Case (a) mirrors the engine, not a copy of it.** "Does not auto-advance" is `stageAutoAdvances`, the autonomy gate of `runCatchUpPhase2` extracted unchanged (a parity table test pins every combination of global yolo, autonomy label and stage `auto_advance`). Validate is excluded: its next stage is the cleanup stage, `fabrik_advance` refuses it and the existing `awaiting-merge-decision` ranking covers the cruise case. A Validate-complete item with no autonomy keeps its previous classification; this is deliberate scope, not an oversight. The next stage is the one `fabrik_advance` would move to (`decisionNextStage`), empty when that is a holding or cleanup stage.

**Case (b) resolves cache-only.** `resolveParkedBlockers` walks `BlockedBy` through the store (`Peek`, or a one-`Scan` index when called inside a `Scan` visitor — a nested read lock could deadlock behind a waiting writer), at most 8 deep, cycle-safe. A blocker is stuck when it is in a parking column (`stages.IsParkingColumn`, the predicate `fabrik_promote` uses) or itself blocked only by stuck blockers. A blocker the store does not hold (cross-repo), one not yet deep-fetched, or an active-column blocker with no open blockers of its own fails closed to `waiting`. Because `BlockedBy` is a deep-fetch field, the first board after a restart may list case (a) items before all case (b) items; the docs say so (R4 is fully satisfiable for (a)).

### 2. The age is anchored on GitHub time, from data the engine already has

Three sources were considered:

- **A. A poll-side recorder** reads the GitHub time once per item per restart through the existing `labelAppliedAt` helper and stores it in the store's `LabelAppliedAt` map. Chosen.
- **B. A `timelineItems` extension of the deep query** would also answer "when did the blocker enter Backlog" and human autonomy changes made while the daemon was down, but adds nodes to every deep fetch, needs the wire-contract recording refreshed, and only helps deep-fetched items (not Backlog blockers).
- **C. The cached Fabrik stage comment's `CreatedAt`** costs nothing and is within seconds of the label for stages that comment. Kept as the fallback.

`engine/decision_anchor.go` (`settleDecisionAnchors`, a per-poll settle step on the poll goroutine) looks up `stage:<Status>:complete` or `fabrik:blocked` only for items the classifier would call suspended, at most 8 per poll, remembering a miss or an error for 10 minutes. It is deliberately **not** named `localapi*` or `channel_*`: those files are source-scanned and forbid the GitHub-capable identifiers, and the scans are not weakened. The classifier resolves the since-time as label time, else stage-comment time, else the daemon's first sight (`first-seen`, a *lower bound*): then `age_seconds` is unknown, `at_least_age_seconds` carries the bound and the text says "waiting at least since …". A daemon-observed age is never presented as the wait. For case (b) the since-time is the `fabrik:blocked` label time, matching "when the item became blocked"; if the blocker was parked after the edge was added the true time is later and the cache has no GitHub time for that move (a known limit).

### 3. The anchor resets when eligibility to proceed changes

`ItemState.EligibilityChangedAt` is stamped by the store (`stampEligibility`) from the before/after label diff of an item it **already held** when a `fabrik:cruise`, `fabrik:yolo`, `fabrik:blocked`, `fabrik:paused` or `stage:*:complete` label is added or removed. A population or baseline never stamps, so the field is zero after a restart and for changes made while the daemon was down: an unknown stamp can only make an age longer, never shorter. `progressAnchor` includes it, so an item settled at Validate under cruise for 40 minutes that gets `fabrik:yolo` is not `stalled` until one threshold after the change, and a pending decision's `since` is `max(GitHub time, EligibilityChangedAt)` (`since_basis: eligibility-change`). It is daemon time, accurate for live changes — the case in the incident, where a human set yolo seconds earlier.

### 4. Next-action honesty (R5)

For states only a human or an external event moves (`suspended`, `needs-human`, `escalated`, awaiting-merge-decision) the result has no deadline and `next_at` is absent; a `fabrik:blocked` wait drops its cooldown deadlines. `cooldown:periodic-re-eval` is never a deadline anywhere: it is the safety-net re-check, it cannot change what happens to the item, and presenting it as "re-dispatched at …" was false. It stays a *reason*. This also fixes `#1342`'s `next_at` contradiction. Other waits (`awaiting-ci`, …) keep their real deadlines.

### 5. Events: siblings, not a generalised `stage-settled`

`validate-settled` keeps its name and meaning. Two new events:

- **`decision-pending`** (digest-able) — one event per episode when an item becomes suspended.
- **`settlement-withdrawn`** (immediate) — one event when a `validate-settled` or `decision-pending` episode ends because the engine took the item back.

Rejected: generalising `validate-settled` into a `stage-settled` family. It would rename a documented event consumers already filter on, and a withdrawal published *as* a `validate-settled` re-publication (`superseded=true`) could be read as readiness by anything that matches on the type alone, and would be dropped by the hub's per-key dedup. A distinct type cannot be mistaken for "ready".

**Episodes** use the persisted hub counters `validate-settled` uses (`dp:<ref>`/`dpclosed:<ref>`, dedup key `dp:<ref>:<n>`): a restart neither replays an announced episode nor loses an unannounced one. **This amends ADR-1966-b's "a restart announces nothing about the existing board" for this one event**: R4/R6 require a pending decision to be visible after a restart, so a suspended item with no open persisted episode is announced once at first sight, and one whose episode is open is adopted without a re-announcement. The pause family and everything else keep the silent baseline. An item whose GitHub time is not yet known waits `decisionAnchorGrace` (3 minutes) for the recorder, and the recorder's success nudges the deriver (`channelReevaluate`) so the announcement carries the exact time; after the grace it goes out with `since_basis=first-seen`. The first-start flood is bounded by the event being digest-able and by catch-up carrying the full list.

**Withdrawal reasons** are a closed list: `rebase-needed`, `new-feedback`, `ci-on-new-head`, `worker-dispatched`. They are attributed from the cache (labels, the PR head against `ValidateCompletedSHA`, the worker handle, the feedback gate's pure cores). `rebase-needed` also covers a `fabrik:rebase-needed` that was applied and removed between two evaluations, because the engine applies it and dispatches the rebase within one pass. A human acting — autonomy set, the item advanced, merged or closed, `fabrik:revalidate`, a blocker cleared, a pause — ends the episode **silently**. For `validate-settled`, a worker dispatch or new feedback now also end the episode (they previously did not), except for an episode opened at the yolo landing decision, which still has its worker. A parked-blocker episode never emits a withdrawal.

**Catch-up** gains a third branch: a currently suspended item produces a `decision-pending` snapshot (`catch_up=true`, no dedup key, no counter change).

### 6. Reminders are explicit and persisted (R6a)

`remind_after` is a per-subscription option (600 s – 30 days, default off) on `fabrik_subscribe`. The hub persists, per (subscriber, episode), the time and count of the last reminder (`dedup.json`'s `reminders`), and measures the interval from the later of that and its own persisted announcement time (the dedup entry). `Hub.PublishReminders` builds the reminder from the event as announced, so it carries identical attributes and comment link, adds `reminder=true` and `reminder_count`, and has no dedup key. Only attached subscribers are reminded (a detached one is caught up on attach), a reminder is never held, and closing the episode clears the entries. With no announcement on record a first look only plants the anchor, so **no reminder is ever driven by daemon uptime**, and a stalled pending decision is never re-published as `stalled`. A per-subscription option cannot reuse `Publish`/`DedupKey`, which dedup hub-wide per key; hence the separate path.

### 7. Decision-bearing events say how to act (R-options)

`internal/attention` builds one ordered `[]Option{action, tool}` per decision kind and both the classifier's `next_action` text and the event's `options` meta (a JSON string; meta values are strings) read from it, so they cannot drift: decision pending → `fabrik_advance`, `fabrik_set_autonomy cruise`, `fabrik_set_autonomy yolo`; parked blocker → one `fabrik_promote` per blocker plus "remove the blocking dependency"; `validate-settled` under cruise → `merge PR #N` (no tool: the human's merge) and `fabrik_set_autonomy yolo`; paused/`needs-human`/`escalated` → the resume method below. The types and tool names are the ones that exist today (`fabrik_advance` shipped in #2126).

**Resume method.** The classifier's generic "a human comment resumes it" was wrong for pauses whose engine comment says "remove `fabrik:paused`" (the daemon clean stop, CI and merge-gate pauses, the spawn pauses, …). `resumeMethodOf` derives it from the item's **latest cached Fabrik comment** only: one that tells the operator to remove `fabrik:paused` gives `remove-label`; otherwise it is unknown. A plain awaiting-input question with no such instruction gives `human-comment`; anything else points at the pause comment and claims neither method. Only the newest Fabrik comment is consulted, so an awaiting-input question posted after an older pause comment supersedes it. A test captures the clean-stop comment by running the real shutdown path and asserts the reported method matches it.

### 8. Label events are not a readiness signal (R-unsettle-2)

The `fabrik_subscribe` description, the channel instructions, USER_GUIDE §9 and state-machine §7.14 say so: only `validate-settled` / `decision-pending` (ready for you) and `settlement-withdrawn` (no longer ready) are readiness signals.

## Consequences

- The first board after a restart lists case (a) pending decisions with a real age; case (b) follows once `BlockedBy` is cached. A cross-repo or un-deep-fetched blocker keeps an item `waiting`.
- A daemon-observed age is never presented as a wait; `since_basis` and `at_least_age_seconds` say what is known.
- The classifier's inputs grew (autonomy resolution, next stage, feedback, stage-comment time, parked blockers, resume method, eligibility stamp), all plain values built in `attentionInput`; the package stays pure and the zero-GitHub scans stay as strict as before.
- One extra REST read per suspended item per restart (bounded, 8 per poll), made by the poll goroutine.
- ADR-1966-a's precedence table and restart caveat and ADR-1966-b's restart statement are amended as described; `docs/state-machine.md` §7.13/§7.14 and USER_GUIDE §9 are the as-built references.
- The sim has no write-through board cache, so a `fabrik:rebase-needed` that the engine applies and clears within one pass is not seen by the store there; the sim withdrawal scenario therefore accepts `worker-dispatched` as well as `rebase-needed`. In production the cache sees the label and the reason is `rebase-needed`.
