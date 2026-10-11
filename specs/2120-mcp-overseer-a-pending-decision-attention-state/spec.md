## Problem

The overseer's core question is "what needs me?" `fabrik_board view=attention` answers it wrongly for the most common human decision point: **a stage has completed, the item has no autonomy label, and nothing will move until a human advances it.**

Observed in an in-situ test (a Claude Code session using `fabrik mcp` against the live dev daemon, 2026-10-11). The daemon had been up 20 min:

- **Only one item showed.** `attention` listed only #1342 (`awaiting-input`).
- **Ten Specify items were in fact waiting on a human:**
  - #1209, #1359, #1378, #1380 and #1411 carry `stage:Specify:complete` with no cruise or yolo. The label was applied on **2026-08-04**, 68 days earlier.
  - #1340 and #1343 are blocked by issues **parked in Backlog** (#1372, #1405), which will never move without a human.
  - #1341 and #1412 are chained behind those.
- **The first five surfaced late and wrong.** At daemon uptime ≈ 30 min (`--stall-threshold`) they arrived as `stalled` pushes, code `stage-complete-not-advanced`, "has not advanced for 31m0s". This is wrong three ways:
  1. **Invisible after a restart.** For the first 30 min after every restart these items do not appear, because the age is anchored on daemon-first-seen (`status_entered_basis: daemon-observed`, ADR-1833).
  2. **Age understated.** "31m" stands for 68 days.
  3. **Wrong class and no instruction.** `stalled` reads as an engine fault, but the engine is correctly waiting for a decision. `next_action` says "nothing scheduled; the next poll re-evaluates the item" and does not name the decision.
- **They were re-announced with nothing new.** At about 13:29Z the same five items were pushed again as `stalled`, "1h20m30s" (`progress_age_seconds 4830`, still measured from daemon start). Nothing had changed since the first burst. The re-fire interval (about 50 min) matches no configured threshold, and the second burst omitted the `comment=` link the first carried.
- **The blocked items never surface at all.** #1340 and #1343 stay `waiting` ("resumes when they close") even though their blockers are parked.
- **`fabrik_status` contradicts itself.** For #1209 it says `waiting`, `cooldown:periodic-re-eval`, "the dispatch cooldown expires and the item is re-dispatched at 12:39:32Z". Nothing will be re-dispatched: the periodic re-evaluation cooldown is presented as a next action although it cannot change anything.
- **A decision made seconds earlier read as a long stall.** #2096 settled at Validate under cruise at 13:03:43Z and was correctly *not* reported as stalled while it waited for a human. The operator switched it to yolo at about 13:41Z. Seconds after the next reconcile (13:42:02Z), a push arrived: `stalled stage-complete-not-advanced` "has not advanced for 38m22s". The age counted from Validate completion, not from when the item became eligible to advance automatically.
- **A settled decision can be withdrawn, but nothing announces the withdrawal.** #2116 (about 15:55Z) and #2114 (about 15:35Z) each published `validate-settled` while waiting for a human under cruise. Then main moved and both went `fabrik:rebase-needed`; PR #2149 was DIRTY. No event said either was no longer ready, so an overseer acting on the earlier push would have hit a conflict. During the rebase rework, `stage:Validate:complete` was re-applied and pushed while the PR was still DIRTY or CI was pending, then removed a minute later.

## Requirements

- **R1 — A derived "pending decision" state.** Add it to `internal/attention`. It is derived and is not a label. An item is in this state when either holds:
  - **(a) Completed stage, no auto-advance:** `stage:<X>:complete` is present for its current column, the stage does not auto-advance for this item (no `fabrik:cruise`/`fabrik:yolo`, stage `auto_advance` false), and it is not Validate (Validate-settled under cruise keeps its existing ranking);
  - **(b) Parked blocker:** it is blocked only by open items that sit in an unmanaged parking column (Backlog), directly or transitively.

  Rank it with `needs-human`. Its summary and `next_action` name the decision:
  - for (a): advance to `<next stage>`, or set cruise/yolo;
  - for (b): promote blocker #N, or remove the dependency.

  It is never `stalled`.
- **R2 — Name: "Suspended".** The state is called **Suspended**, both in attention's `state` value and in user-visible text. Today the word is taken by the account-wide Claude usage limit; #2129 renames that to "usage limited" and is a blocker of this issue. After #2129 lands, "suspend" appears nowhere else on the user-visible surfaces, so this state is the word's only meaning.
- **R3 — An honest age.**
  - **Anchor:** the age of a pending decision is anchored on the GitHub time the deciding fact became true (the `stage:<X>:complete` labeled time, or when the item became blocked by the parked item), not on daemon-first-seen.
  - **No new per-request GitHub calls:** the local API's zero-GitHub-call property (`TestLocalAPISourceDoesNotReachGitHub`) holds. Obtain the time from data the engine already fetches, or record it once per item in the store/persisted state.
  - **Unknown:** when it is unknown, say so ("waiting at least since <first-seen>"). Never present a daemon-observed age as the wait.
  - **Anchor resets when eligibility to proceed changes.** The progress anchor resets whenever the item's eligibility to proceed changes. That covers an autonomy label being added or removed (`fabrik:cruise`/`fabrik:yolo`), a pending decision being resolved (advanced, or moved by a human), and a blocker clearing. `stalled` measures time since the item could last have moved on its own, and never includes time it spent legitimately waiting for a human. After a reset, the reported age starts at the change.
- **R4 — Visible from the first poll after a restart.** A pending decision appears in `attention` on the first served board after startup. It does not wait for a threshold.
- **R5 — A cooldown is not a next action when nothing will happen.** When the item's state is pending-decision, paused/awaiting-input, or blocked:
  - `next_action`/`next_at` must not be a dispatch cooldown's expiry;
  - `next_at` is unknown/absent;
  - the action names the human or external event that moves it.

  This also fixes `#1342`'s `next_at` contradiction.
- **R6 — Push.** Entering the pending-decision state publishes one event per episode, deduped like `validate-settled`. The ADR decides whether to generalise `validate-settled` into a `stage-settled` family or add a sibling event. The event's attributes are:
  - issue, column, completed stage;
  - the decision options;
  - blockers (for case b);
  - the GitHub-anchored since-time.

  Catch-up on attach includes items currently in this state.
- **R6a — Reminders are explicit.** Re-announcing an unchanged pending decision is a **reminder**, not a new stall:
  - it is off by default, or on a documented configurable interval (a per-subscription option such as `remind_after`);
  - it is marked as a reminder and carries the same attributes as the original event, including the comment link;
  - it never fires on a schedule driven by daemon uptime.

  A stalled pending-decision item is never re-published as `stalled`.
- **R-unsettle-1 — Withdrawn settlement is announced.** When an item that published `validate-settled` (or a pending-decision event under this issue) stops meeting the settled predicate before a human acts, publish one event saying so, with the reason. The reason is one of: rebase needed, new feedback, CI red or pending on a new head, or worker dispatched. The ADR chooses between a `validate-unsettled` event and a `superseded=true` re-publication. The event is deduplicated per settle episode, like `validate-settled`.
- **R-unsettle-2 — Label events are not a readiness signal.** The `fabrik_subscribe` description, and the event-catalogue docs, state that label events are not a readiness signal; only the settled and unsettled events are.
- **R-unsettle-3 — Resettling.** After an unsettled event, a fresh `validate-settled` is published once the item resettles (a new settle episode).
- **R-options-1 — Decision-bearing events say how to act.**
  - **Scope:** every decision-bearing event (`validate-settled` under cruise, the pending-decision event, and the paused/`needs-human` family).
  - **Field:** it carries an `options` list. Each option names the action and the MCP tool that performs it, when one exists. For example:
    - merge PR #N (no tool: the human's merge);
    - `fabrik_set_autonomy yolo`;
    - `fabrik_advance` (shipped in #2126);
    - `fabrik_promote`.
- **R-options-2 — A paused event gives the engine's own resume method.**
  - A paused event states the resume method that the engine's own pause comment gives. For example, a daemon-clean-stop pause says "remove `fabrik:paused`", while an awaiting-input pause says "a human comment".
  - It never gives a generic method that contradicts the engine (A16 in the in-situ report).
- **R7 — Docs.**
  - `docs/state-machine.md` §7.13 (attention) and §7.14 (events).
  - `docs/USER_GUIDE.md`'s MCP tool and event tables.
  - Regenerate `docs/llms-full.txt`.
  - Write `adrs/<this issue>-<slug>.md`.

## Scope

In scope:
- the classifier state;
- its age anchoring, including the reset when eligibility to proceed changes;
- next-action honesty for non-progressing states;
- its push event;
- the unsettled event (or superseded re-publication) for withdrawn settlements, the `fabrik_subscribe` description wording;
- the `options` list on decision-bearing events and the engine-accurate resume method on paused events.

Out of scope:
- an action that advances an item (separate issue);
- worker/queue classification (separate issue);
- Hookdeck.

## Acceptance

Each case below is a classifier unit test, shown non-vacuous by neutralisation:
- **Specify complete, no autonomy, label applied 68 days ago, daemon just started:** the item is pending-decision, ranked with needs-human, its age is ≈68 days anchored on GitHub, and `next_action` names "advance to Research, or set cruise".
- **Same item with `fabrik:cruise`:** not pending-decision.
- **Blocked by a Backlog item:** pending-decision, naming the blocker. **Blocked by an item in an active pipeline column:** stays `waiting`.
- **Anchor reset on autonomy change:** an item settled at Validate under cruise for 40 min that then gets `fabrik:yolo` is not reported as `stalled` until the stall threshold has elapsed *after* the yolo change, and its reported age starts at the change.
- **Reminders:** a sim test with no reminder configured publishes no re-announcement over several stall thresholds. With a reminder interval it publishes reminders at that interval, marked as reminders, with the same attributes as the original event.
- **Paused item:** `next_at` is absent, not a cooldown expiry.
- **Simulation (`tests/sim`):** after a simulated restart, the first board served lists the item, and exactly one pending-decision event is published per episode, not re-published on every poll.
- **Unsettle (`tests/sim`):** settle an item under cruise, then make the base move so the item needs a rebase. Exactly one unsettled event is published, and a fresh `validate-settled` follows once the item resettles.
- **Options (classifier or channel test per decision kind):** a test for each decision kind (`validate-settled` under cruise, pending decision, paused/`needs-human`) asserts the `options` list. A test for the clean-stop pause asserts its resume method matches the engine comment.
- **Zero GitHub calls:** `TestLocalAPISourceDoesNotReachGitHub` and the channel scan stay green and are not weakened.
- `go test -race ./...` is clean.

Mentioned for context: #1966. Mention it by bare number only in the PR body. Do NOT use a closing keyword (`Closes`/`Fixes`/`Resolves`) near #1966.
