# Feature Specification: Live webhook coverage tests — delivery, echo, dedupe, replay, transport health

**Feature Branch**: `fabrik/issue-2427`
**Created**: 2026-10-23
**Status**: Specified
**Input**: User description: "## Problem

The live suite has one webhook test (`TestWebhookChangeSurvivesReconcile`, #2348), and it has never run in a gate. Everything the engine does on receipt of a delivery — the transport-neutral intake (ADR 2351), the persisted delivery dedupe, the engine-level echo registry, per-item serial apply, the webhook facts of ADR 2352, the observed-at ordering rule (ADR 2348), replay of missed deliveries (#2144) and the Tailscale transport's health and recovery (#2143, #2425) — is proven only against simgh and recorded fixtures, which cannot model real delivery timing, redelivery or payload drift. Once the gate has a webhook leg (#2426), it needs tests that fail when webhook ingestion regresses.

## Requirements

Each test below is a live test (`tests/e2e`, `-tags e2e`) with a registry entry (`events: sensitive`, sim twin or `live-only` with a reason). Where the subject is \"the webhook alone did this\", it holds polls (`HoldPolls`, ADR 1978) so only a delivery can move the daemon's state, and so must be `exclusive`. Each assertion reads the daemon through the local API (`fabrik_status`/`board`/`health`), not GitHub.

- **FR-001 Column move by webhook** — `TestWebhookChangeSurvivesReconcile` runs as written (poll-held move, then whole polls must not revert it).
- **FR-002 Human label by webhook** — a label a human adds (e.g. `fabrik:paused`) reaches the Store with polls held, attributed `actor=human` (not `unknown`), and survives the next reconcile.
- **FR-003 Comment wakes an item** — a human comment on a paused item resumes it with polls held for the poll interval, i.e. the wake came from the delivery.
- **FR-004 Engine echo** — an engine label write's webhook echo is matched (no `unknown`-actor label event, no extra wake), proven over a real delivery.
- **FR-005 Review and thread facts (ADR 2352)** — a review-thread resolution and a review comment edit by webhook update the feedback gate's view with polls held.
- **FR-006 check_run completion** — an awaiting-CI item clears its CI gate from the `check_run` delivery without waiting a poll interval.
- **FR-007 Dependency change (ADR 1995)** — removing a `blockedBy` edge on GitHub wakes the blocked item through `issue_dependencies`.
- **FR-008 Redelivery dedupe** — redelivering a delivery through the App's `/app/hook/deliveries/{id}/attempts` is accepted once (the ledger rejects the duplicate; no second apply).
- **FR-009 Replay across downtime (#2144)** — with the bed daemon stopped, a human change is made; on start, the daemon replays the failed delivery and shows the change before its first reconcile would have, and does not double-apply.
- **FR-010 Transport health (#2143, #2425)** — `fabrik_health` reports the tailscale transport healthy only once the Funnel name resolves publicly and a delivery has been received; a deliberately broken route (App hook URL pointed elsewhere for the test, restored in cleanup) shows the `tailscale_webhook_config` warning and no false healthy.
- **FR-011 Restart under load** — a SIGHUP restart (ADR 2259) with deliveries arriving loses none (the intake quiesces; replay or reconcile covers the gap) and applies none twice.

Each test is proven non-vacuous by running it once with the behaviour it guards neutralised (the existing `Set…DisabledForTest`-style seam where one exists, or the engine change reverted), recorded in the PR.

## Acceptance

- Every test above passes on `app/on/webhook` and skips structurally on poll cells (`skip_ok_legs`).
- The PR records a neutralised run per test showing it fails.
- `tests/e2e/README.md` lists the webhook tests and what each one guards.

## Out of scope

- Bed transport and preflight (#2408) and the gate axis (#2426).
- Hookdeck-specific behaviour.
"

## Background

The live e2e suite is the only layer that exercises real GitHub, real delivery timing and real redelivery. It has exactly one webhook test, `TestWebhookChangeSurvivesReconcile` (#2348), which has never run in a gate because no gate bed could receive webhooks. Everything the engine does when a delivery arrives is therefore proven only against the sim bed and recorded fixtures, and the sim cannot model delivery timing, redelivery, the App's delivery log, tailnet/Funnel health or payload drift. The covered behaviour is:

- the transport-neutral intake and its persisted `X-GitHub-Delivery` dedupe (ADR 2351);
- the engine-level echo registry, which matches the webhook echo of an engine's own write (ADR 2351);
- per-item serial apply of deliveries;
- the webhook facts the engine uses — review threads, comment edits, sub-issues, labels and milestones (ADR 2352), and dependencies (ADR 1995);
- the observed-at ordering rule that stops a lagging listing from reverting a webhook-applied fact (ADR 2348);
- replay of deliveries missed while the daemon was down (#2144);
- the embedded Tailscale transport's health and recovery (#2143, #2425).

Two sibling issues carry the prerequisites. #2408 gives beds a real webhook transport and a per-leg delivery preflight (landed in ADR 2408). #2426 adds the `events` axis (`poll` | `webhook`) to the gate matrix: cell label `app/on/webhook`, a required `events: sensitive | neutral` registry field, and "failure to observe a webhook effect on a webhook leg is a failure, not Inconclusive". This issue supplies the tests that make that leg worth running: they must fail when webhook ingestion regresses.

Findings that shape the work:

- The existing test already shows the pattern to reuse: seed a paused item, hold polls (`HoldPolls`, ADR 1978, which requires an `exclusive` registry entry), make the change on GitHub, read the daemon's copy through the local API, and trigger whole polls to prove the change survives. It skips unless `bedWebhooksEnabled`.
- A webhook leg polls every 180 s (ADR 2408 decision 4; #2426 revisits the cadence), so a "quick" effect is evidence of the webhook path. With polls held, only a delivery can move the daemon's state.
- ADR 2408 decision 8 gave the existing test an interim `skip_ok_legs: ["pat/*"]` and a skip message citing #2427 for the matrix axis. The axis is now owned by #2426, so that interim wording and registration are superseded once the axis lands.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A webhook regression fails the gate (Priority: P1)

A release manager runs the gate with a webhook leg. If a change broke how the daemon applies deliveries (an echo mismatch, a dedupe failure, a lost wake-up, a reverted fact), at least one test in the webhook suite fails and names the behaviour it guards.

**Why this priority**: This is the reason the issue exists. A webhook leg with no tests that can fail proves nothing.

**Independent Test**: For each test, neutralise the behaviour it guards (a `Set…DisabledForTest`-style seam or a reverted engine change) and run the test once on the webhook leg; it must fail. Restore the behaviour and it must pass.

**Acceptance Scenarios**:

1. **Given** a webhook-leg bed and polls held, **When** a human adds a label, moves a card or comments on GitHub, **Then** the daemon's local API shows the change without any poll having run.
2. **Given** the guarded behaviour is neutralised, **When** the test runs, **Then** it fails (not skips, not inconclusive) with a message naming the missing effect.
3. **Given** the guarded behaviour is intact, **When** the test runs on `app/on/webhook`, **Then** it passes.

---

### User Story 2 - Deliveries are applied once and attributed correctly (Priority: P1)

An operator relies on the daemon treating each delivery exactly once: a redelivered copy is dropped by the ledger, an engine write's own echo is recognised as the engine's, and a human's change is attributed to a human.

**Why this priority**: Double-apply, echo mismatch and `unknown` attribution cause duplicate work, spurious wake-ups and wrong channel events, and only a real delivery stream exercises them.

**Independent Test**: The redelivery, echo and human-label tests (FR-002, FR-004, FR-008), each run on its own.

**Acceptance Scenarios**:

1. **Given** an accepted delivery, **When** it is redelivered through the App's delivery-attempts endpoint, **Then** the daemon applies it once and the duplicate is rejected by the ledger.
2. **Given** the engine writes a label, **When** the webhook echo of that write arrives, **Then** it is matched as the engine's own — no `unknown`-actor label event and no extra wake.
3. **Given** a human adds a label with polls held, **When** the delivery arrives, **Then** the label is in the daemon's Store attributed `actor=human` and survives the next reconcile.

---

### User Story 3 - Deliveries wake the daemon sooner than a poll (Priority: P1)

A human comment, a completed check run or a removed dependency edge moves an item along within the delivery's latency, not after the next poll interval.

**Why this priority**: Wake-on-event is the main user-visible benefit of webhooks; a regression turns it silently back into polling.

**Independent Test**: The comment, `check_run` and dependency tests (FR-003, FR-006, FR-007), each with polls held for at least the poll interval.

**Acceptance Scenarios**:

1. **Given** a paused item and polls held, **When** a human comments, **Then** the item resumes within the delivery window.
2. **Given** an awaiting-CI item, **When** its check run completes, **Then** the CI gate clears from the `check_run` delivery without waiting a poll interval.
3. **Given** a blocked item, **When** its `blockedBy` edge is removed on GitHub, **Then** the item is woken through the `issue_dependencies` delivery.

---

### User Story 4 - Review facts and the column move arrive by webhook (Priority: P2)

A review-thread resolution, a review-comment edit and a project column move reach the daemon's view of the item by webhook, and a later whole poll does not revert them.

**Why this priority**: These are the ADR 2352 and ADR 2348 facts. They are important, but each has a sim twin and unit coverage; the live test adds real delivery and read lag.

**Independent Test**: FR-001 and FR-005 on their own.

**Acceptance Scenarios**:

1. **Given** polls held, **When** a human moves a card, **Then** the daemon shows the new column and two whole polls do not revert it.
2. **Given** an unresolved review thread, **When** a human resolves it, **Then** the feedback gate's view no longer counts it.
3. **Given** a review comment, **When** a human edits it, **Then** the feedback gate's view shows the edited text.

---

### User Story 5 - Downtime, restarts and transport faults lose and duplicate nothing (Priority: P1)

An operator restarts the daemon (stop/start or SIGHUP) or the route to it breaks. No change is lost, none is applied twice, and `fabrik_health` never claims the transport is healthy when it is not.

**Why this priority**: Replay, quiesce and health are the recovery paths; a failure there loses events invisibly.

**Independent Test**: FR-009, FR-010 and FR-011 on their own.

**Acceptance Scenarios**:

1. **Given** the bed daemon is stopped and a human makes a change, **When** the daemon starts, **Then** it replays the failed delivery and shows the change before its first reconcile would have, without applying it twice.
2. **Given** the App hook URL is pointed elsewhere for the test, **When** `fabrik_health` is read, **Then** the `tailscale_webhook_config` warning is present and the transport is not reported healthy; **and** after cleanup restores the URL the warning clears.
3. **Given** deliveries arrive during a SIGHUP restart, **When** the daemon is back, **Then** every change is present exactly once.

---

### Edge Cases

- **Poll cell:** every test in this issue skips structurally with a reason (registered as `skip_ok_legs`) on a poll cell and on every PAT cell — the transport needs App auth. It never fails or goes inconclusive there.
- **Webhook leg, effect not observed:** a missing webhook effect is a failure, not `Inconclusive` (#2426 FR-005). `Inconclusive` stays for genuinely unprovable situations (for example the seed never became visible).
- **Held polls must be released:** a test that fails or is interrupted must still release the hold (existing `t.Cleanup` behaviour), so a failure cannot wedge the bed for later tests.
- **FR-010 modifies shared App configuration** (the App's hook URL). Cleanup must restore it even when the test fails, and the test must not run concurrently with any other webhook test (it is `exclusive`). A restore failure is reported loudly, because every later delivery would otherwise be lost.
- **FR-009 and FR-011 stop and restart the bed daemon.** They are `exclusive`, must leave the bed running with polls released, and must not leave a pending replay that could contaminate a later test.
- **Redelivery (FR-008):** the original and the redelivered copy are told apart by the same delivery ID; the test must not count the redelivery's own HTTP success as proof of dedupe, only the daemon's observable state.
- **Delivery latency:** waits are bounded and generous (the preflight already proved delivery on this leg). Because a delivery is not guaranteed to arrive within a fixed time, the assertion is "arrived while polls were held", not a latency number.
- **Shared board and repos:** each test files its own labelled issue(s) and cleans them up; no test depends on another's leftover state.

## Requirements *(mandatory)*

### Functional Requirements

Common to all tests (FR-001–FR-011):

- Each is a live test (`tests/e2e`, `-tags e2e`) with a registry entry carrying `events: sensitive` (with a reason), a sim twin or `live-only` (with a reason from the fixed vocabulary), and `skip_ok_legs` covering every cell with no webhook transport.
- Where the subject is "the webhook alone did this", the test holds polls (`HoldPolls`, ADR 1978) so only a delivery can move the daemon's state, and so is registered `exclusive`.
- Each assertion reads the daemon through the local API (`fabrik_status`, `fabrik_board`, `fabrik_health`), not through GitHub.

The tests:

- **FR-001**: Column move by webhook — `TestWebhookChangeSurvivesReconcile` runs as written: a poll-held column move reaches the daemon, and two whole polls afterwards do not revert it.
- **FR-002**: Human label by webhook — a label a human adds (for example `fabrik:paused`) reaches the Store with polls held, is attributed `actor=human` (not `unknown`), and survives the next reconcile.
- **FR-003**: Comment wakes an item — a human comment on a paused item resumes it with polls held for the poll interval, so the wake came from the delivery.
- **FR-004**: Engine echo — an engine label write's webhook echo is matched (no `unknown`-actor label event, no extra wake), proven over a real delivery.
- **FR-005**: Review and thread facts (ADR 2352) — a review-thread resolution and a review-comment edit, each by webhook, update the feedback gate's view with polls held.
- **FR-006**: `check_run` completion — an awaiting-CI item clears its CI gate from the `check_run` delivery without waiting a poll interval.
- **FR-007**: Dependency change (ADR 1995) — removing a `blockedBy` edge on GitHub wakes the blocked item through `issue_dependencies`.
- **FR-008**: Redelivery dedupe — redelivering a delivery through the App's `/app/hook/deliveries/{id}/attempts` is accepted once: the ledger rejects the duplicate and nothing is applied a second time.
- **FR-009**: Replay across downtime (#2144) — with the bed daemon stopped, a human change is made; on start the daemon replays the failed delivery, shows the change before its first reconcile would have, and does not double-apply.
- **FR-010**: Transport health (#2143, #2425) — `fabrik_health` reports the tailscale transport healthy only once the Funnel name resolves publicly and a delivery has been received. With the App hook URL deliberately pointed elsewhere (restored in cleanup), it shows the `tailscale_webhook_config` warning and never a false healthy.
- **FR-011**: Restart under load — a SIGHUP restart (ADR 2259) with deliveries arriving loses none (the intake quiesces; replay or reconcile covers the gap) and applies none twice.

Cross-cutting:

- **FR-012**: Every test above has a `tests/e2e/registry/registry.json` entry that passes the registry completeness test in plain `go test ./...`: `events: sensitive` with `events_reason` (once #2426 defines the field), a sim twin or `live-only` with a reason, `exclusive` (with reason) wherever polls are held or the bed is stopped/restarted, and `skip_ok_legs` for the cells without a webhook transport.
- **FR-013**: On a cell without a webhook transport (every poll cell and every PAT cell), each test skips with a message that states why and cites the owning issue (#2426 for the axis), and the skip is registered as structurally expected so the ledger reports it as a known skip, not a missing result. The existing `TestWebhookChangeSurvivesReconcile` skip text and `skip_ok_legs: ["pat/*"]` are updated to the final wording and the full set of non-webhook cells.
- **FR-014**: On a webhook cell, a test that fails to observe the webhook effect it guards fails (`t.Fatalf`), not `Inconclusive` (#2426 FR-005). `Inconclusive` is kept only for preconditions the test cannot establish (for example, the seed never became visible to the daemon).
- **FR-015**: Each test is proven non-vacuous: it is run once with the behaviour it guards neutralised — an existing `Set…DisabledForTest`-style seam where one exists, otherwise the engine change reverted — and the run is shown to fail. Where a test cannot be neutralised by a seam, the PR states how else the failure was demonstrated. The per-test result is recorded in the PR description.
- **FR-016**: Every test leaves the bed as it found it: polls released, the daemon running, App configuration (hook URL) restored, scratch issues/PRs/labels cleaned up. Cleanup runs on failure and on skip-after-setup paths.
- **FR-017**: `tests/e2e/README.md` lists the webhook tests and, for each, the behaviour it guards, the isolation class (`exclusive` or not) and the neutralisation used to prove it non-vacuous.

### Key Entities *(if applicable)*

- **Live webhook test**: a `-tags e2e` Go test with a registry entry that exercises one delivery-driven behaviour of the daemon against real GitHub.
- **Poll hold**: the test-only bed seam (ADR 1978) that stops the bed engine's automatic polls so that only a delivery can change its state; requires an `exclusive` registry class.
- **Delivery ledger**: the daemon's persisted record of accepted `X-GitHub-Delivery` IDs, which makes a redelivered copy a no-op.
- **Echo**: the webhook delivery GitHub sends for a write the engine itself made; the engine's echo registry must recognise it as its own.
- **Neutralised run**: a run of a test with the guarded behaviour disabled, which must fail, used to show the test is not vacuous.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: All eleven behaviours (FR-001–FR-011) have a live test that passes on `app/on/webhook` against a healthy bed.
- **SC-002**: On every poll and PAT cell, every one of these tests ends as a registered structural skip — zero failures and zero inconclusive results.
- **SC-003**: For each of the eleven tests the PR records one neutralised run that fails, with the behaviour that was disabled.
- **SC-004**: Plain `go test ./...` passes with the new registry entries and fails if one of them is removed or malformed.
- **SC-005**: After the full webhook suite runs, the bed is left running with polls released and the App hook URL unchanged, and no scratch artefacts remain.
- **SC-006**: `tests/e2e/README.md` names every test and its guarded behaviour, so a failing webhook test can be traced to the behaviour that regressed.

## Assumptions

- #2408 (bed transport and delivery preflight, ADR 2408) and #2426 (the `events` axis, `app/on/webhook` cell label, registry `events` field, failure-not-inconclusive rule) are the prerequisites. This issue depends on #2426 (already recorded as a blocker). The wording here matches #2426's spec; if #2426's cell label or registry field names change, `skip_ok_legs` patterns and `events` entries follow it.
- "Webhook cell" means an `app` cell with the webhook event source; the Tailscale Funnel transport is the bed transport (ADR 2408). PAT cells can never have it.
- The webhook leg's bed poll cadence is whatever #2426 settles on (180 s today). Tests do not depend on a specific cadence for correctness; they use held polls to make "only a delivery could have done this" structural, and use the cadence only to size their waits.
- Tests assert on the daemon's observable state through the local API. Where a needed fact (for example attribution `actor=human`/`unknown`, or the ledger's duplicate count) is not exposed there, the available local-API or health surface is used. Adding a new read-only local-API field for that purpose is acceptable but is a design decision for Plan, not a requirement here.
- FR-008 and FR-010 use the GitHub App's own APIs (delivery list/redelivery, hook configuration) with App credentials the bed already has for the delivery preflight. No new secrets are introduced.
- FR-007 can be driven by the repository's dependency (`blockedBy`) API with the bed's existing credentials.
- FR-006 relies on a PR whose CI check run completes while polls are held; the test supplies its own controllable check (the existing awaiting-CI tests' approach) and does not depend on production CI timing.
- Tests may be split across files and may share helpers with `webhook_reconcile_test.go`; grouping is a Plan decision.
- The existing test (FR-001) is "run as written" for its behaviour; only its skip wording, `skip_ok_legs` and (per #2426 FR-005) its webhook-leg inconclusive-to-failure handling change.
- A live-run budget exists for the extra tests; they are `events: sensitive` and run only in the webhook cell (sparse plan), so the poll cells' cost is unchanged.

## Out of Scope *(optional)*

- Bed transport and the delivery preflight (#2408) and the gate axis, ledger keys and matrix planning (#2426).
- Hookdeck-specific behaviour.
- New engine behaviour: the tests guard existing behaviour. Engine changes are only those needed to make a test observable or to neutralise a behaviour for the non-vacuity run, and are decided in Plan.
- Sim twins for scenarios the sim cannot model (delivery timing, redelivery, Funnel health); those are registered `live-only` with a reason.
- Direct (non-Tailscale) App webhook transport coverage; FR-010 is specific to the tailscale transport.

## Source References *(optional)*

- ADR 2348 (observed-at ordering), ADR 2351 (transport-neutral intake, dedupe, echo registry), ADR 2352 (webhook facts), ADR 1995 (`issue_dependencies`), ADR 1978 (poll hold/trigger seam), ADR 2259 (two-phase SIGHUP restart), ADR 2143 / #2425 (Tailscale transport and public-DNS probe), ADR 2144 (delivery replay), ADR 2408 (bed event source and delivery preflight)
- #2408 (bed transport and preflight), #2426 (events axis), #2348 (observed-at ordering)
- `tests/e2e/webhook_reconcile_test.go`, `tests/e2e/poll_control.go`, `tests/e2e/registry/registry.json`, `tests/e2e/README.md`
