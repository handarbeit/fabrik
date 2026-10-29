# ADR 2467: The review-wait timeout measures reviewer silence on the current head

Status: accepted (#2467). Extends [ADR-017](017-review-gate-two-phase-design.md) (the two-phase bot re-prompt ladder), [ADR-1283](1283-declared-unrequested-reviewers.md) (declared reviewers) and [ADR-1518](1518-review-gate-non-convergence-terminal-check.md) (the non-convergence terminal check). Complements [ADR 2465](2465-review-gate-head-aware-verdict.md), which decides which review counts at which head and explicitly left the review-wait clock to this ADR.

## Context

`checkAwaitingReviewTimeout` anchored the review-wait timeout on the first application of `fabrik:awaiting-review`. That label stays on through every fix round, so a push that asks the reviewer for a fresh look at a new head never restarted the clock: the timeout measured how long the fix loop had run, not how long the reviewer had been silent on the commit in front of them. Reported instance (community thread #2453, mentioned by number only): chronicle#529 / PR #534, label at 4:28 PM, two fix rounds, last fix pushed 4:45:47 PM, `review wait timeout elapsed` at 4:46:18 PM — 31 seconds after the push, before the bot could have reviewed the new head. The bot re-prompt ladder had the same flaw: Phase 1 ran from the same label, and Phase 2 from `fabrik:bot-reprompted`, which a later push left untouched.

## Decision

1. **The anchor is `max(label applied time, current head's arrival)`.** `checkAwaitingReviewTimeout` keeps its label-only test and, only when that would fire, asks `headRestartsWait` whether a head that arrived after the label still has an unelapsed window. If so the gate keeps waiting and writes nothing. With no head change after the label the timeout fires at the same instant as before.
2. **Head arrival is the head commit's committer date** (`GET /repos/{o}/{r}/commits/{sha}`, new `gh.Client.FetchCommitTime`). Alternatives considered and rejected:
   - `LinkedPRState.LastHeadSHAUpdate`: the host clock, memory-only; a restart would reset it (violates R2).
   - The `synchronize` webhook's `updated_at`: absent on poll-only daemons, not persisted, and bumped by any PR activity.
   - Check-suite `created_at`: only exists when CI is configured.
   - An engine-written marker label re-applied on each head change: durable and GitHub-sourced, but it adds label churn, echo/attention/channel side effects and extra events on every push.
   - GraphQL `pushedDate`: removed by GitHub in 2023.
   The committer date is a GitHub-reported fact, available on any daemon and any repo, and re-readable after a restart, so a restart neither resets nor extends the wait (R2).
3. **It is author-controlled and a rebase rewrites it, so it is bounded.** A date more than 2 minutes ahead of the host clock is ignored (label anchor); a smaller lead is clamped to now, so the anchor cannot slide forward; the `max` means it can only extend a wait; every read failure or unparseable value falls back to the label time and never produces an earlier timeout or a pause (FR-003, FR-007). A failed read is remembered for one minute so a commit GitHub cannot answer costs one request a minute rather than one per poll.
4. **Read lazily, memoise per SHA.** The head SHA is resolved live (`FetchPRDetails`, falling back to the snapshot's `LinkedPRHeadSHA`) because the poll-start snapshot can predate a push that just happened; the read happens only when the label-only timeout would otherwise fire, or while the ladder is engaged. A commit's date is immutable, so successes are cached for the process lifetime in `Engine.headTimes`.
5. **Any head change counts**, including a force-push or a rebase. Distinguishing a content push from a rebase is not attempted; `MaxReviewCycles` bounds the loop. Merge-train catch-up pushes happen only in Queued, where the review gate is not evaluated.
6. **The ladder follows the same anchor (R4).** Before Phase 2, while `fabrik:bot-reprompted` is present, a head that arrived after the re-prompt label removes only that label (`resetLadderOnNewHead`): Phase 2 does not fire on the clock of a commit the bot was already asked about, `fabrik:awaiting-review` stays on its own (head-based) anchor, and Phase 1 re-fires a full window after the new head. Removing only the re-prompt label avoids a gap in which the item is not marked as waiting, and is preferred to moving Phase 2 onto `max(repromptedAt, headAt)`, which would leave a stale re-prompt state.
7. **The bound (R3).** A loop in which every round pushes can no longer be ended by the timeout. It already ends at `MaxReviewCycles`: `handleReviewGate` dispatches actionable feedback through `dispatchWithCycleLimit` against `max(ReviewCycles, ReviewBlockedCycles)`, and `ReviewBlockedCycles` is charged while the gate is blocked (`chargeBlocked`) and never refunded for a no-op reinvoke. No code change was needed there; it is pinned by tests.
8. **Comparisons stay on the host clock.** The first plan moved the timeout comparisons to `e.now()`. That silently changes what every existing sim scenario measures: they drive this gate by backdating GitHub-sourced label times against the host clock (`tests/sim/timeout_test.go`), while the sim's engine clock starts at the same backdated instant. The timeout and head comparisons therefore use `time.Since`/`time.Now`, exactly like the label comparison they extend; production behaviour is unchanged.
9. **No new configuration, no new label, no new admission cooldown.** A blocked gate already records `review-blocked` at `githubRecheckInterval()` each poll (ADR 2232), so a longer wait is re-evaluated.

## Consequences

- The reported scenario no longer pauses: the reviewer gets the full window counted from the last push.
- A wait on a head that is never reviewed still ends: a full silent window on the current head fires the timeout (or Phase 1 / Phase 2) as before.
- One extra `FetchPRDetails` and one `FetchCommitTime` per evaluation at the would-fire point, and one `FetchPRDetails` per poll while the ladder is engaged; the commit read is cached per SHA. A failure degrades to today's behaviour.
- A commit committed long before it is pushed anchors the wait earlier than the push; it still restarts the clock relative to the label.
- Not addressed (siblings): which review counts at which head (#2465), the declined-finding exit (#2466), `FABRIK_REVIEW_WAIT_TIMEOUT`'s default, and the CI-wait timeout.

## Verification

- `engine/review_wait_anchor_test.go`: the reported shape (red against the label-only anchor first), no push, a full window after a push, read failure, future-dated and skewed heads, live head resolution and per-SHA caching, restart, the ladder reset and its non-vacuity controls, and the `MaxReviewCycles` bound with the anchor active.
- `tests/sim/review_wait_head_anchor_test.go`: the same scenarios end to end, with `SetReviewWaitHeadAnchorDisabledForTest` controls that flip the outcome; `simgh.FetchCommitTime` is scenario-seeded (FIDELITY.md).
