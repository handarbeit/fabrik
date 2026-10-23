# Feature Specification: e2e gate events axis (poll | webhook)

**Feature Branch**: `fabrik/issue-2426`
**Created**: 2026-10-23
**Status**: Specified
**Input**: User description: "## Problem

The gate's matrix has two axes, auth × train (`tests/gate` `Cell{Auth, Train}`, sparse plan ADR 1975). Event ingestion — poll-only vs webhook — is a third dimension of engine behaviour with its own failure modes (ordering, dedupe, echo, wake-ups, replay), and the gate has no way to schedule, require or record it. Once beds can receive webhooks (#2408), the gate needs a webhook leg whose coverage the ledger requires.

## Requirements

- **FR-001**: `Cell` gains an `Events` dimension, `poll` | `webhook`. The cell label gains it (`app/on/webhook`); a poll cell's label stays `app/on` etc., so existing ledger records keep their keys. Webhook cells are App-auth only (the webhook transports require App auth); `pat/*/webhook` is never planned.
- **FR-002**: The registry (`tests/e2e/registry/registry.json`) gains a required `events` field per live test, `sensitive` | `neutral`, with `events_reason` required for `sensitive` and no default — the same shape as `auth`/`train` (#1975). The completeness test (plain `go test ./...`) fails on a missing or malformed value, and requires `events: sensitive` for every test that calls `bedWebhooksEnabled` or holds polls to isolate a webhook effect.
- **FR-003**: Sparse plan (default): the existing baseline and narrowed cells unchanged, plus `app/on/webhook` running every `events: sensitive` test. `E2E_MATRIX=full`: the four poll cells plus `app/on/webhook` and `app/off/webhook` running the **whole** suite, since a release that changes ingestion must show every scenario still converges with webhooks as the primary wake source. `buildPlanInput` stays the one source for `gate run` and `gate coverage`.
- **FR-004**: A webhook cell restarts its bed with the webhook event source (#2408 FR-002) and runs #2408's delivery preflight first; a poll cell restarts it poll-only. Multi-bed scheduling (ADR 1976) assigns a webhook cell only to a bed configured for webhooks, and the identity set is unchanged (the transport adds no new GitHub identity).
- **FR-005**: On a webhook leg a test's failure to observe a webhook effect is a **failure**, not `Inconclusive` (the preflight has proved delivery). Tests that today call `Inconclusive` when a webhook effect is missing switch on the leg.
- **FR-006**: `gate report` and the per-leg archive show the events dimension; the leg summary records the bed's webhook delivery count and the intake's dedupe/drop counts for the leg.
- **FR-007**: `CLAUDE.md` (the four-layer table and the matrix description), `tests/gate/README.md` and ADR 1975's successor ADR (`adrs/<this issue>-…`) record the axis and the full-matrix rule.

## Acceptance

- `gate coverage` on a fresh SHA lists `app/on/webhook` pairs as required for every `events: sensitive` test (sparse) and every test (full).
- A run where the webhook preflight fails leaves those pairs uncovered and exits non-zero; poll cells are unaffected.
- Existing poll-cell ledger records from before this change still count (label unchanged).
- Plain `go test ./...` fails for a registry entry without `events`.

## Out of scope

- Bed transport setup and the delivery preflight (#2408).
- New webhook-specific live tests (#2427)."

## Background

The release gate plans its live e2e legs over two axes, auth (`pat`/`app`) and train (`off`/`on`), with a sparse default (ADR 1975) and a per-SHA coverage ledger that decides when the gate is accepted. How the engine ingests GitHub events — poll-only, or with webhooks as the primary wake source — is a third dimension with its own failure modes (ordering, dedupe, echo, wake-ups, replay). The gate cannot currently schedule, require or record it, so a release that changes ingestion can pass the gate without any scenario having run with webhooks. Once beds can receive webhooks (#2408), the gate needs a webhook leg whose coverage the ledger requires.

A steering comment (2026-10-23) raised a cadence problem. #2408 (PR #2431) runs a webhook leg's bed at a longer poll interval (`-poll 180`, `bedevents.WebhookPollSeconds`) than the poll legs' 60 s, so that a quick effect can only have come from a delivery. Many existing scenarios' fixed waits are tuned to a 60 s poll, so running the **whole** suite on webhook cells at 180 s would time out or fail tests for reasons unrelated to webhooks, making the full plan an unreliable release gate. This spec therefore makes the poll cadence an explicit, planned and recorded part of the cell, rather than something a webhook cell inherits from the bed setup.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Webhook coverage is planned and required by the ledger (Priority: P1)

A release manager runs the gate (or `gate coverage`) on a fresh SHA. The plan includes an `app/on/webhook` cell that runs every `events: sensitive` test, and the ledger counts those (test, cell) pairs as required, so the gate cannot be accepted without them.

**Why this priority**: This is the whole point of the issue: ingestion behaviour becomes a gated dimension.

**Independent Test**: With a stubbed `Commander` and registry fixture, call the shared plan builder and `gate coverage` and check the required set and the printed cell labels.

**Acceptance Scenarios**:

1. **Given** the sparse default and a registry with some `events: sensitive` tests, **When** the cells are planned, **Then** the existing poll cells are unchanged and one extra `app/on/webhook` cell runs exactly the `events: sensitive` tests.
2. **Given** `E2E_MATRIX=full`, **When** planned, **Then** the four poll cells plus `app/on/webhook` and `app/off/webhook` are planned and the two webhook cells run the whole suite.
3. **Given** a fresh SHA, **When** `gate coverage` runs, **Then** it lists the `app/on/webhook` pairs as required and exits 8 (incomplete) until each has a PASS.

---

### User Story 2 - Existing poll coverage keeps working (Priority: P1)

An operator upgrades to the new gate with a ledger already holding poll-cell records for the current SHA. Those records still satisfy the poll cells' requirements.

**Why this priority**: Breaking ledger keys would throw away expensive (~10 h) live runs.

**Independent Test**: Load a ledger fixture written before the change and compute coverage over a plan containing only poll cells.

**Acceptance Scenarios**:

1. **Given** records labelled `app/on`, `pat/off` etc., **When** coverage is computed after the change, **Then** they count for the identical poll cells.
2. **Given** a webhook preflight failure, **When** the gate finishes, **Then** the webhook pairs are uncovered, the exit is non-zero, and poll-cell results and coverage are unaffected.

---

### User Story 3 - Registry declares events sensitivity (Priority: P1)

A contributor adding a live test must declare whether it is sensitive to the ingestion mode, with a reason, exactly as for `auth` and `train`.

**Why this priority**: The sparse webhook cell is only as good as the sensitivity classification.

**Independent Test**: Plain `go test ./...` on the registry package with a missing, malformed, or reason-less entry.

**Acceptance Scenarios**:

1. **Given** a registry entry with no `events`, **When** `go test ./...` runs, **Then** the completeness test fails naming the test.
2. **Given** `events: sensitive` without `events_reason`, or any value other than `sensitive`/`neutral`, **Then** the completeness test fails.
3. **Given** a test that calls `bedWebhooksEnabled` or holds polls to isolate a webhook effect but is marked `neutral`, **Then** the completeness test fails.

---

### User Story 4 - Webhook cells run the existing suite reliably at a stated cadence (Priority: P1)

The webhook cells' poll cadence is specified per cell, planned, printed and archived. By default it is the poll legs' 60 s, so the existing suite's fixed waits hold, and `events: neutral` tests behave the same on a webhook cell as on the matching poll cell. Evidence that an effect arrived by webhook comes from structure (observed deliveries, polls held, the App's delivery log), not from a longer poll interval.

**Why this priority**: Without it the full plan is unreliable as a release gate (see Background).

**Independent Test**: Unit-test that the planner assigns the cadence to every cell and passes it to the bed restart; verify the printed plan and `bed.json`/leg archive contain it.

**Acceptance Scenarios**:

1. **Given** the default plan, **When** a webhook cell starts, **Then** its bed is restarted with a 60 s poll cadence and the cadence appears in the plan output and the leg archive.
2. **Given** `E2E_BED_POLL_SECONDS` set by the operator, **When** the plan is built, **Then** the override is printed loudly and recorded as an override on the leg.
3. **Given** a full-plan run, **When** the webhook cells finish, **Then** no `events: neutral` test that passes on the matching poll cell is Inconclusive or failed on the webhook cell, except entries on the documented exception list.

---

### User Story 5 - A missing webhook effect fails the webhook leg (Priority: P2)

On a webhook leg, a test that expects a webhook effect and does not see it fails; it does not self-report `Inconclusive`, because the preflight has already proved delivery. On a poll leg the old behaviour is unchanged.

**Why this priority**: Otherwise a broken ingestion path would quietly present as "uncovered/inconclusive" instead of red.

**Independent Test**: Run the harness helper under a simulated webhook leg and a poll leg and compare outcomes.

**Acceptance Scenarios**:

1. **Given** a webhook leg and a test whose webhook effect never appears, **Then** the test fails.
2. **Given** the same test on a poll leg, **Then** it keeps its current behaviour.

---

### User Story 6 - Reports and archives show the events dimension (Priority: P2)

An operator reading `gate report` or a leg archive can see which cells were webhook cells and, for each webhook leg, how many deliveries the bed received and how many the intake deduped or dropped.

**Why this priority**: Needed to interpret a webhook leg's result and spot ingestion anomalies.

**Independent Test**: Feed fixture leg summaries into the report/archive code and check the output.

**Acceptance Scenarios**:

1. **Given** a webhook leg, **When** its summary is written, **Then** it records delivery, dedupe and drop counts for that leg.
2. **Given** `gate report`, **Then** cells are shown with the events dimension and the poll cadence.

---

### User Story 7 - Webhook cells only run on webhook-capable beds (Priority: P2)

In a multi-bed run, a webhook cell is scheduled only on a bed configured for webhooks; the identity set is unchanged.

**Why this priority**: Prevents a webhook cell being run on a bed that cannot receive deliveries.

**Independent Test**: Multi-bed scheduler tests with one webhook-capable and one poll-only bed.

**Acceptance Scenarios**:

1. **Given** beds A (poll-only) and B (webhook-capable), **When** a webhook cell is queued, **Then** only B takes it.
2. **Given** no webhook-capable bed, **Then** the gate refuses the webhook cell up front with a named preflight failure rather than running it poll-only.

---

### Edge Cases

- `E2E_AUTH_MODES`/`PlanInput.AuthModes` excluding `app`: no webhook cell is planned, since webhook cells are App-only.
- The caller supplies `-run` (a selected run): the caller's selection is honoured on webhook cells as on poll cells, and a selected run is not treated as full coverage.
- Webhook preflight fails: the webhook cell's tests are not run, pairs stay uncovered, exit is non-zero (the existing preflight exit code), poll cells proceed.
- `--resume` on a SHA with webhook pairs already covered runs only what is missing.
- A test legitimately cannot pass on a webhook cell for a reason unrelated to webhooks: it is listed with a reason (see FR-011), not silently skipped.
- A test that self-skips structurally on a leg uses the existing `skip_ok_legs` mechanism, whose patterns now match the three-part label (for example `*/off/*`, `*/*/webhook`).

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: `Cell` gains an `Events` dimension, `poll` | `webhook`. The cell label gains it (`app/on/webhook`); a poll cell's label stays `app/on` etc., so existing ledger records keep their keys. Webhook cells are App-auth only (the webhook transports require App auth); `pat/*/webhook` is never planned.
- **FR-002**: The registry (`tests/e2e/registry/registry.json`) gains a required `events` field per live test, `sensitive` | `neutral`, with `events_reason` required for `sensitive` and no default — the same shape as `auth`/`train` (#1975). The completeness test (plain `go test ./...`) fails on a missing or malformed value, and requires `events: sensitive` for every test that calls `bedWebhooksEnabled` or holds polls to isolate a webhook effect.
- **FR-003**: Sparse plan (default): the existing baseline and narrowed cells unchanged, plus `app/on/webhook` running every `events: sensitive` test. `E2E_MATRIX=full`: the four poll cells plus `app/on/webhook` and `app/off/webhook` running the **whole** suite, since a release that changes ingestion must show every scenario still converges with webhooks as the primary wake source. `buildPlanInput` stays the one source for `gate run` and `gate coverage`.
- **FR-004**: A webhook cell restarts its bed with the webhook event source (#2408 FR-002) and runs #2408's delivery preflight first; a poll cell restarts it poll-only. Multi-bed scheduling (ADR 1976) assigns a webhook cell only to a bed configured for webhooks, and the identity set is unchanged (the transport adds no new GitHub identity).
- **FR-005**: On a webhook leg a test's failure to observe a webhook effect is a **failure**, not `Inconclusive` (the preflight has proved delivery). Tests that today call `Inconclusive` when a webhook effect is missing switch on the leg.
- **FR-006**: `gate report` and the per-leg archive show the events dimension; the leg summary records the bed's webhook delivery count and the intake's dedupe/drop counts for the leg.
- **FR-007**: `CLAUDE.md` (the four-layer table and the matrix description), `tests/gate/README.md` and ADR 1975's successor ADR (`adrs/2426-…`) record the axis, the full-matrix rule and the poll-cadence rule.
- **FR-008**: The poll cadence is a field of the cell definition. It is set when the plan is built, is printed with the plan, is passed to the bed restart for the leg, and is archived with the leg (`bed.json`/leg summary). It is not inherited from the bed setup's default.
- **FR-009**: The default cadence of every cell, webhook cells included, is the poll legs' 60 s, so the existing suite's fixed waits hold. No planned cell runs at 180 s. #2408's `bedevents.WebhookPollSeconds` default applies only to ad hoc, non-gate bed runs; the gate always passes its cell's cadence explicitly.
- **FR-010**: `E2E_BED_POLL_SECONDS` is an operator override of the planned cadence, not part of how the plan works. When set it is printed loudly, recorded as an override on the leg, and legs run under an override do not count toward ledger coverage for the cell label they would otherwise satisfy.
- **FR-011**: Webhook-path evidence in `events: sensitive` tests comes from structure, not timing: a delivery observed in the bed's health or the gate's delivery counts, an effect that lands while polls are held (`HoldPolls`, ADR 1978), or the App's delivery log. Existing `events: sensitive` tests are checked for reliance on a longer poll interval and adjusted to this rule where needed.
- **FR-012**: On a full-plan run, an `events: neutral` test that passes on the matching poll cell passes on the webhook cell too. A test that cannot, for a reason unrelated to webhooks, is listed through the registry's existing `skip_ok_legs` mechanism with a one-line reason for each entry; no unexplained exception is allowed, and the completeness test requires the reason.
- **FR-013**: Registry `skip_ok_legs` patterns, ledger keys, `--resume` selection, `gate coverage` output and exit codes handle the three-part webhook label; a poll cell's label remains the two-part form everywhere it is keyed, matched or printed.

### Key Entities *(if applicable)*

- **Cell**: one planned leg: auth, train, events, poll cadence, parallelism and test selection. Its label identifies it in the ledger.
- **Registry entry**: per live test, now including `events` and `events_reason` beside `auth`/`train`.
- **Leg summary**: per-leg archive record, now including events, cadence (and override flag), webhook delivery count and intake dedupe/drop counts.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: `gate coverage` on a fresh SHA lists `app/on/webhook` pairs as required for every `events: sensitive` test (sparse) and for every test (full).
- **SC-002**: A run where the webhook preflight fails leaves those pairs uncovered and exits non-zero, while poll cells run and are recorded as before.
- **SC-003**: Ledger records written before this change still satisfy the poll cells' requirements (label unchanged).
- **SC-004**: Plain `go test ./...` fails for a registry entry with a missing or malformed `events`, a missing `events_reason` on `sensitive`, or a `neutral` test that calls `bedWebhooksEnabled` or holds polls to isolate a webhook effect.
- **SC-005**: In a full-plan run, zero `events: neutral` tests are Inconclusive or failed on a webhook cell when they pass on the matching poll cell, other than documented, reasoned exceptions.
- **SC-006**: Every planned cell's poll cadence is visible in the plan output and the leg archive, and no gate-planned cell runs at 180 s.
- **SC-007**: A webhook leg's archive records delivery, dedupe and drop counts, and `gate report` shows the events dimension.

## Assumptions

- #2408 (bed webhook transport, delivery preflight, `bedWebhooksEnabled`, `bedevents`) lands first; this issue builds on its interfaces and does not implement them.
- The webhook-capable bed is identified by #2408's bed configuration; this issue only uses it for scheduling.
- A leg run under a poll-cadence override is treated as non-conforming and does not count toward coverage (FR-010); this is the safe default and can be relaxed later.
- Sparse webhook coverage is a single cell, `app/on/webhook`; `app/off/webhook` appears only under `E2E_MATRIX=full`.
- Reusing `skip_ok_legs` for the full-plan exception list (FR-012) avoids adding another registry field.
- The webhook transports are App-auth only, so webhook cells never need a PAT identity.

## Out of Scope *(optional)*

- Bed transport setup and the delivery preflight (#2408).
- New webhook-specific live tests (#2427).
- Tuning or replacing the bed's default 180 s cadence for non-gate runs.

## Source References *(optional)*

- ADR 1975 (sparse auth × train matrix), ADR 1976 (multi-bed), ADR 1978 (poll hold/trigger seam), `tests/gate/schedule.go` (`Cell`), `tests/e2e/registry/`.
- #2408 / PR #2431 (bed webhook transport and delivery preflight); steering comment of 2026-10-23 on poll cadence for webhook cells.
