# ADR 2123: Channel arrivals, column moves, closes and the restart baseline

## Status

Accepted. Amends ADR-1966-b decision 4 ("a board population, `Reset` or an item's first creation is a baseline") and supersedes the statement in ADR 2125 that `promote` produces no channel event.

## Context

An in-situ session against the live dev daemon (2026-10-11) showed that the channel (ADR-1966-b) never reported a new item arriving or an item changing column, that a manual close read as "unblocked" and a landing's `merged` could trail its own close tracking, and — most damaging — that every daemon restart or self-upgrade published about 400 false `label-applied` events: about 150 as open items were first deep-fetched and about 250 at the first reconcile, covering Done, Backlog and closed items too.

The cause of the flood and of the silent arrivals is one rule. The store treated an item's *first creation* as the baseline for label deltas. After a restart the store is bootstrapped from the board probe, which carries no labels, so every later fetch that did carry them diffed against an empty list and emitted each as a change. Conversely, an item arriving after startup was a "first creation" and therefore silent. The line between "present at startup" and "added later" had to be drawn once and used for both.

## Decision

### 1. Baseline = first label-bearing population

`gh.ProjectItem.LabelsAbsent` marks a producer that did not observe labels. `applyProjectItem`/`applyShallowItem` then leave `Labels` untouched and do not set the new `ItemState.LabelsKnown`. A non-label-write mutation produces `LabelDeltas` only when `before.LabelsKnown`; explicit label writes (`LocalLabel*`, `IssueLabeled/Unlabeled`) always do. So a label-less seed followed by a deep fetch, a shallow reconcile or a board reconcile is a silent baseline, for open, Done, Backlog and closed items alike, while a real change afterwards still publishes exactly once.

The flag is **negative** deliberately. A positive "labels known" marker that one producer forgets would silently turn real label changes into swallowed baselines; a forgotten `LabelsAbsent` fails noisy and is detectable. Only the two label-less producers set it: `CacheImpl.BootstrapFromProbe` and the minimal `IssueOpened` the probe loop seeds (and the parking-column seed below). The board query, `FetchItemDetails`, `FetchProjectItem` and the webhook `opened` payload carry labels and set nothing; `ItemDeepFetched` clears the flag unconditionally because a deep fetch always carries labels. `TestBootstrapFromProbe_LabelsNotKnown` guards the bootstrap producer, and the restart sim covers the probe-loop seed. `Store.SetLegacyLabelBaselineForTest` restores the old rule so the tests can show they are not vacuous.

State-derived events (pause family, `cycle-limit-near`) read labels through the attention classifier, so `derive`, `seed` and the ticker skip an item until `LabelsKnown`; its first label-bearing evaluation takes the existing silent first-sight branch. Removing that guard makes the restart scenario re-publish `awaiting-input` (verified), so it is not vacuous either.

### 2. `item-added`

`Change.Created` is set for a record the running store learns about; `Reset` never sets it, so a mid-run reset cannot cause an arrival storm. The observer registers a pending arrival only after `Engine.boardPopulated`, which is set when the first poll returns successfully (both `Run` and `PollOnce`; the production store is bootstrapped before the hub starts, the sim's during the first poll, so one definition serves both). The consumer publishes the event from `evaluateItem` once the snapshot has `LabelsKnown`, a board Status, is open and is not a PR card — i.e. when the labels are first known, never an empty set (FR-005). `Store.Get` fallback records for non-board issues have no Status and never announce. A pending arrival that never becomes announceable is dropped, with a log line, after the one-hour prune interval. The dedup key `item-added:<ref>` keeps it once across restarts.

Matching: `Event.Labels` carries the arrival labels. A subscription's `labels` include patterns match when any of them matches; `exclude_labels` does not apply to `item-added`, so an arrival carrying `fabrik:editing` is not hidden. Repo, issue and milestone scopes apply as usual and an unknown milestone never matches a milestone scope (ADR-1966-a).

Parking columns count as managed — see the arrival rule below (#2187, which amends the earlier "any open on-board issue with a Status qualifies"). The probe loop used to skip new items in `Unmanaged` stages entirely, which made a hand move out of Backlog a first sight rather than a move; it now seeds them (minimal `IssueOpened`, `LabelsAbsent`, inside `FromProbe`, still no deep fetch — #778 stands).

Residuals, stated plainly: items created between hub start and the end of the first successful poll are silent; an arrival while the daemon is down is not captured (the existing "events during downtime" limit); an item that was absent from a probe (the 0-items guard) and later learned could be announced once.

### 3. `column-changed`

`Change.PrevStatus` is set when the previous Status was known and differs; an unknown previous Status, or a `Created` change, is a baseline and invents no event. One move is one event for free: the engine's `LocalStatusUpdated` write-through changes the store first, and the webhook echo, Layer 2 sweep or probe re-observing the same Status is a no-op that never reaches an observer. The actor reuses the ADR 2125 vocabulary (`labelAttribute`): `fabrik` for the write-through or a matched echo, `overseer` with `via` for `fabrik_promote` (the issue's `overseer:<subscriber>` shorthand), `human`/`bot` when a `projects_v2_item` webhook named the sender, `unknown` otherwise. `ProjectV2ItemEdited` gained `Sender`, `EchoOfEngine`, `Requester` and `At` (decoded from the payload's `sender.login` and `projects_v2_item.updated_at`); the sweep, probe, deep fetch and reconcile carry none, so their moves are `unknown` with no `status_at` (zero, never "now"). Status often reaches the daemon only through the sweep, so `unknown` will be common. `moveItemToStatus` now carries the requester (`CacheImpl.UpdateItemStatusBy`, `RegisterEchoIfSubscribedVia`) — this supersedes ADR 2125's note that `promote` produces no channel event and needs no attribution. If the sweep observes an engine move before the write-through lands, the move is attributed `unknown` and the write-through is then a no-op: still exactly one event.

### 4. `closed` and ordering

`Change.Closed` marks open → closed on an item the store held. The `closed` event carries `column`, `actor` and `landing`. `landing=true` when `emitLanded` announced a landing for the item, or the snapshot shows `fabrik:awaiting-landing-verification`, a close-tracking label (`fabrik:awaiting-close`, `fabrik:awaiting-member-close`, `fabrik:credited-pr:*`) or a merged linked PR; a close is attributed `fabrik` only when `emitLanded` announced the landing (GitHub's auto-close sender would otherwise read as a human). The announced-landing mark is cleared when the issue is reopened or `fabrik:landing-verification-failed` is applied, so a later manual close names its own actor and is not tagged as part of the reversed landing. A landing inferred from the snapshot alone cannot tell a Fabrik landing from a human merge or a later manual close, so it is tagged `landing=true` but keeps the actor the change carries. Any other close names the webhook sender (`human`/`bot`) or `unknown`, and the board column. The engine registers no `issues`/`closed` echo, so `IssueClosed` has no echo flag: only the `LocalIssueClosed` write-through is attributable to Fabrik.

`merged` used to be derived only from the `fabrik:awaiting-landing-verification` label delta, which is applied after the Done move and after the close tracking, so it could arrive after `awaiting-close`. `emitLanded` now publishes it from the landing paths at the merge itself (before GitHub's close reaches the store and before any close tracking), with the *same* dedup key as the label-derived event (`merged:<ref>:<pr>`), which stays as the fallback for paths that observe a merge they did not make (native auto-merge, merge queue). Exactly one is published. Close-tracking label events for a landing carry `landing=true`. A hold-and-reorder buffer was rejected: it adds latency and a timing window to fix an ordering the source can simply get right.

### 5. The `progress_via_column` option (R3)

A boolean subscription option. When set and the subscription takes `column-changed` (`events` empty, or naming it), `label-applied`/`label-removed` for `stage:*:complete` and `stage:*:in_progress` are dropped, even when `labels` or `exclude_labels: []` would admit them. Setting it with an `events` list that omits `column-changed` is a `Validate` error, not a silent no-op. It is `omitempty`, so persisted subscriptions load unchanged and re-subscribing stays idempotent. **The default `exclude_labels` are unchanged:** `stage:*:complete` stays default-visible because existing subscribers rely on it and the issue asks for an opt-in remedy; a default change would silently alter every existing subscription.

### 6. Catch-up never replays arrivals (R4)

A `catch_up=true` snapshot is current state, not history. It never contains `item-added`, `column-changed` or `closed` (`TestCatchUpNeverReplaysLifecycleEvents`). A subscriber that missed an arrival or attaches later reads the board with `fabrik_board`.

### 7. Observation only (R5)

The new code lives in `engine/channel_lifecycle.go` and the existing channel files and uses only the `Change`, the snapshot handed to the observer, and `Store.Peek`. `TestChannelEventsSourceDoesNotReachGitHub` is unweakened and `TestChannelEventsAreObservationOnly` still shows identical GitHub mutations with and without a hub. The new store fields are bookkeeping: none enters the poll loop's wake sets, and the probe loop's writes stay inside `FromProbe`.

## Consequences

- A restart or self-upgrade publishes about zero false events instead of about 400.
- Operators can follow a cruise item by column and drop the stage-label stream with one option.
- `human` attribution of a column move needs a webhook; `unknown` is the common case and is documented.
- Amends ADR-1966-b decision 4 ("first creation" → "first label-bearing population") and supersedes ADR 2125's `promote` note.
- Documented in `docs/state-machine.md` §7.14/§7.15 and the `docs/USER_GUIDE.md` event table.

## Amendment (#2187): the arrival rule

`item-added` is published for an open, non-PR issue that arrives after startup in any column that has a configured stage — managed or `unmanaged` (Backlog included). An item in a board column with no configured stage is outside Fabrik's known column set and never produces `item-added`, whether it is first seen by the probe, a deep fetch or a reconcile.

- **Supersedes #2123 FR-004's unmanaged exclusion.** FR-004 said unmanaged items must not produce `item-added`. The decision (2026-10-12) is the opposite: an overseer wants to know when work is filed, including into Backlog. The behaviour of PR #2175 was already this; it is now specified and tested.
- **Keyed on the flag, not the name.** "Configured stage" means any stage loaded from the stage configuration (managed, unmanaged, holding such as `Queued`, or cleanup such as `Done`). A second unmanaged stage (e.g. `Triage`) behaves exactly like Backlog. The one name-based case is the one `stages.IsParkingColumn` already defines: a column literally named `Backlog` with no configured stage (installs that predate `backlog.yaml`) is a parking column, so it is seeded by the probe and announced. Any other column without a stage is not. The probe's parking-seed branch (`engine/terminal.go`) and `announceArrival` both use this one definition (`Engine.knownArrivalColumn`).
- **Decided at evaluation time, in `announceArrival`.** Once the item has a Status, an unknown column drops the pending arrival for good; every entry path (probe, deep fetch, reconcile, webhook) ends there, so no new Store state is needed.
- **First seen unconfigured, later moved:** one `column-changed` and no `item-added`. The move is a transition of an item the daemon already knows about. If the move happens before the debounced evaluation runs, the evaluation sees only the configured column and announces it once (an accepted race).
- **Moving an announced item into a managed column** (Backlog → Specify) is one `column-changed` and never a second `item-added`: the move is not `Created`.
- **Holding and cleanup columns** are configured stages, so an open item arriving directly in `Queued` or `Done` is announced.
- **Announcement timing.** A parking-column card is seeded by the probe without labels, so its `item-added` waits for the first label-bearing population: a webhook `issues` payload immediately, otherwise the next light reconcile (about three minutes by default). Pending arrivals are dropped after the one-hour prune interval.
- **Restarts.** `BootstrapFromProbe` → `Reset` puts every board item, unconfigured columns included, into the Store before `boardPopulated` is set and never sets `Created`, so the restart path suspected in the post-merge review does not produce `item-added` (the guard test `TestChannelRestartIsSilentForUnconfiguredColumn` passes on the pre-change code). The real gap was a card added *after* startup to an unconfigured column and first seen by a reconcile (`IssueOpened`, `Created`), now closed by the column check.
- **Residuals** (unchanged, not extended): a record re-learned after a transient listing gap (`Remove` then `IssueOpened`) and the 0-items probe guard can still announce an existing item once; no seen-keys memory is added.

## Known limit: landings GitHub completes itself

`merged` is published from the landing path after the engine performs or observes the merge. For a landing GitHub completes on its own (native auto-merge, merge queue) the `issues closed` webhook can reach the store first; that `closed` is then published before `merged` with `landing=false`. The ordering guarantee (FR-015) therefore holds for engine-driven landings only.
