# Feature Specification: Price invocations from the served model, not from Claude Code's Anthropic-priced figure

**Feature Branch**: `fabrik/issue-1412`
**Created**: 2026-10-28
**Status**: Draft
**Input**: User description: "Cost reporting is Anthropic-priced regardless of the model actually served. Fabrik's reported cost is Claude Code's Anthropic-priced figure, passed through verbatim regardless of which provider or model actually served the request. On a gateway-routed setup the number is wrong by a large factor in the discouraging direction."

## Background

Fabrik records an invocation's cost by taking the Claude CLI's `total_cost_usd` unmodified. That figure is priced at Anthropic's rates for whichever model the CLI believes it called. It flows unchanged into `history.json`, the TUI and the local API. There is no rate card, no per-token computation and no model-aware pricing anywhere in Fabrik.

When requests are routed through a gateway to a cheaper model, the figure is wrong by a large factor. @bdueck measured a 24-hour, six-worker run on DeepSeek V4 Flash via OpenRouter (community report #1385, discussion #1386), covering 355 runs and 7,143 turns. Fabrik reported $1,481.63 and they were billed $57.00, a 26× overstatement. The error runs in the direction that discourages the cheap-model setups that work. Cost is the number people use to decide whether Fabrik is affordable on such a setup. Their advice today is to "derive your own divisor once, then ignore the dollar column forever."

The prerequisite is done. #1411 (merged to `main` as #2501, ADR 1411) now persists the four token counts per invocation. It also records the requested model and the served model (the concrete model ids the CLI reports) as separate values. Cost can now be derived from persisted data instead of copied from the CLI.

The report's discussion also argues for three principles, which stand on their own merits:
- Reconcile on the served model (and a request identifier where one exists), not the requested model.
- Treat a missing provider cost as unknown, not zero.
- Keep the upstream figure rather than discarding it. (The reasoning came from an affiliated vendor and is not treated as neutral guidance or as a reason to prefer any provider.)

Two existing facts shape the design:
- Fabrik already warns about unrecognised **top-level** keys in `.fabrik/config.yaml`, using the fields of the project config struct as the known set. That check does not look inside nested structures. A rate card is nested, so a mistyped key inside it needs its own check.
- `archive_after`, `reconcile_interval` and `max_review_cycles` are CLI/env-only, and the top-level check warns about them in `config.yaml`. That silent no-op trap is the failure mode to avoid for the new setting.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Accurate cost on a gateway-routed cheap model (Priority: P1)

An operator runs Fabrik against a cheap model through a gateway such as OpenRouter. They add a rate card for the served model to `config.yaml`. The cost shown for each invocation is computed from the persisted token counts at their rates, so the dollar column is usable and roughly matches their provider bill.

**Why this priority**: This is the reported defect. A 26× overstatement makes the cost column useless on exactly the setups where cost matters most.

**Independent Test**: Configure a rate card for a non-Anthropic served model. Run (or replay) an invocation whose recorded tokens and served model are known. Check that the recorded cost equals tokens × rates, not the CLI's `total_cost_usd`, and that the basis is shown as rate-card.

**Acceptance Scenarios**:

1. **Given** a rate card entry for the served model, **When** an invocation completes, **Then** its cost is computed from the persisted input, output, cache-creation and cache-read token counts at the configured rates.
2. **Given** the same invocation, **When** the upstream CLI figure differs, **Then** the CLI figure is still recorded separately and the displayed cost is the rate-card value.
3. **Given** a session that was served by two models (for example a fallback), **When** cost is computed, **Then** each model's tokens are priced at that model's own rate and the results are summed.

---

### User Story 2 - Unknown cost is never shown as a number (Priority: P1)

An operator on a gateway has configured no rate card. Fabrik cannot know what the invocation cost. It says so, instead of showing the Anthropic figure or $0.00.

**Why this priority**: Showing a wrong or zero number is the original defect in a different form. A missing cost must be distinguishable from a genuine $0.00.

**Independent Test**: Run an invocation served by a non-Anthropic model with no rate card and no provider-reported cost. Check that history, the TUI and the local API all show "unknown", not a number and not $0.00.

**Acceptance Scenarios**:

1. **Given** no provider cost, no applicable rate card entry and a served model that is not an Anthropic model, **When** the invocation completes, **Then** the cost is recorded as unknown.
2. **Given** an unknown-cost entry and an entry that genuinely cost $0.00, **When** both are rendered, **Then** the two are visibly different.
3. **Given** unknown-cost entries in the history, **When** a cost total is shown, **Then** it excludes them and indicates that it is partial.

---

### User Story 3 - Provider-reported cost wins when available (Priority: P2)

The provider or gateway reports its own cost for a request. Fabrik records that value as the cost, because it is the closest thing to the actual bill.

**Why this priority**: It is the most accurate source, but it depends on what the CLI/gateway already hands Fabrik. It is not required for the rate-card fix to deliver value.

**Independent Test**: Feed Fabrik a result that carries a provider-reported cost, with a rate card also configured. Check that the recorded cost is the provider value and the basis says so.

**Acceptance Scenarios**:

1. **Given** a provider-reported cost and a configured rate card, **When** the invocation completes, **Then** the provider value is used, not the rate card or `total_cost_usd`.
2. **Given** the provider also reports a request identifier, **When** the entry is recorded, **Then** the identifier is stored with it.

---

### User Story 4 - Direct-Anthropic users see no change (Priority: P1)

An operator on direct Anthropic access, with no new configuration, upgrades. Their cost figures stay exactly as before.

**Why this priority**: Silently repricing existing setups would break trust in a long-standing number.

**Independent Test**: Run an invocation served by an Anthropic model with no rate card. Check that the displayed cost equals the CLI's `total_cost_usd` and that the basis shows it is the CLI-reported figure.

**Acceptance Scenarios**:

1. **Given** a served model that is an Anthropic model and no new configuration, **When** an invocation completes, **Then** the displayed cost equals `total_cost_usd`.
2. **Given** history entries written before this change, **When** they are loaded, **Then** they display their stored cost as before and carry no recorded basis.

---

### User Story 5 - Misconfiguration is loud (Priority: P2)

An operator mistypes a rate-card field or leaves out a rate. Fabrik tells them at startup instead of quietly pricing nothing.

**Why this priority**: A rate card that silently no-ops would reproduce a known, costly failure mode.

**Independent Test**: Load a config with a misspelled field inside the rate card and another with a missing or negative rate. Check that each produces a clear, specific message naming the offending model and field.

**Acceptance Scenarios**:

1. **Given** a rate card entry with an unrecognised field, **When** Fabrik starts, **Then** it reports the unrecognised field by name through the existing operator-warning path.
2. **Given** an entry that omits one of the four rates, or has a negative rate, **When** Fabrik starts, **Then** the configuration is rejected with a message naming the model and the field.

---

### Edge Cases

- The served model is unknown or empty (an older entry, or a response without model usage): the rate card cannot apply, so cost falls through to the next source or unknown. The requested model is never substituted.
- The rate card lacks an entry for one of several served models: the rate-card source does not apply to that invocation, and it falls through to the next source. It is never priced partially.
- An invocation that never reached a provider (no turns, no tokens): its cost is a genuine $0.00 with a basis saying no request was made, distinct from unknown.
- Rates change or are removed after a run: history entries keep the value and basis recorded at the time and are never repriced retroactively.
- The CLI reports `total_cost_usd` of 0 or omits it for a non-Anthropic model: the upstream figure is still recorded as reported (zero or absent) and is not used for display.
- The alias resolves to a different served model after a release: the rate card is matched on the served id, so an unlisted new id yields unknown instead of using the old model's rates.
- Cost totals or the `Last run` style summaries that sum cost across entries: unknown entries are excluded from the sum and the total is marked partial.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: For each invocation Fabrik MUST determine the displayed cost from the first applicable source, in this order: (1) a provider-reported cost; (2) a configured rate card applied to the persisted token counts; (3) the upstream CLI `total_cost_usd`, but only when the served model is an Anthropic model; (4) otherwise unknown.
- **FR-002**: Each entry MUST record the basis of its displayed cost, one of: provider-reported, rate card, CLI-reported, no provider request, or unknown. The basis MUST be visible wherever cost is shown, so a reader is never handed a differently-derived number unannounced.
- **FR-003**: The upstream CLI `total_cost_usd` MUST always be recorded separately on the entry (when the CLI reports one), whichever source supplies the displayed cost, and MUST NOT be overwritten.
- **FR-004**: Unknown cost MUST be represented distinctly from $0.00 in persisted history, the TUI history and detail views, and the local API. It MUST NOT be serialised as zero.
- **FR-005**: Fabrik MUST accept a rate card in `.fabrik/config.yaml` mapping a model id to four rates: input, output, cache-creation and cache-read, each in dollars per million tokens. All four rates MUST be given explicitly for each entry, and an explicit 0 is allowed.
- **FR-006**: The rate card MUST be matched on the served model id, not the requested model. For an invocation served by several models, each model's own token counts MUST be priced at that model's own rates and summed. If any served model has no entry, the rate-card source does not apply to that invocation (FR-001 then continues to the next source).
- **FR-007**: Where the CLI result or gateway response already carries a provider-reported cost, Fabrik MUST record it and prefer it over the rate card and the upstream figure, and MUST record a request identifier when one is present. Fabrik MUST NOT add per-provider billing-API clients for this.
- **FR-008**: With no new configuration, an invocation served by an Anthropic model MUST display the same cost as before this change.
- **FR-009**: The rate-card key MUST be a recognised `config.yaml` setting that is read from `config.yaml`. An unrecognised key nested inside the rate card MUST produce a named warning in the operator-warnings panel and MUST NOT be silently ignored. A rate card with a missing, negative or non-numeric rate, or an empty model id, MUST be rejected at startup with a message naming the model and field.
- **FR-010**: Any displayed total or aggregate of cost MUST exclude unknown entries and MUST indicate that it is partial when any were excluded.
- **FR-011**: A `history.json` written before this change MUST load without error and without dropping entries. Its entries keep their stored cost and carry no recorded basis, and are not repriced.
- **FR-012**: Cost already recorded in history MUST NOT be recomputed when the rate card changes. Each entry's value and basis are fixed when the invocation completes.
- **FR-013**: The user guide MUST document: the precedence in FR-001; the rate-card format and units; that rates are user-maintained and Fabrik ships no pricing table; that rates go stale as vendor pricing changes; the meaning of each basis label; and the tolerance within which a rate-card figure is expected to reconcile with provider billing (SC-002).

### Key Entities *(if applicable)*

- **Cost basis**: The label recording how an entry's displayed cost was determined (provider-reported, rate card, CLI-reported, no provider request, unknown).
- **Rate card**: User-maintained configuration mapping a served model id to four per-million-token rates (input, output, cache-creation, cache-read).
- **Upstream cost**: The CLI's own `total_cost_usd`, kept on each entry for comparison and never used as ground truth for non-Anthropic models.
- **Provider-reported cost**: A cost value (and optional request identifier) supplied by the provider or gateway through data the CLI already returns.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: With a rate card configured, 100% of invocations whose served models are all listed show a cost equal to their persisted token counts multiplied by the configured rates, independent of `total_cost_usd`.
- **SC-002**: On a gateway-routed run with accurate rates, the summed rate-card cost reconciles with the provider's billed total within a documented tolerance. The target is 5%, to be confirmed against a recorded real run. This replaces the 26× overstatement measured in the report.
- **SC-003**: An operator can tell unknown cost from $0.00 at a glance in the TUI, `history.json` and the local API, with no entry of unknown cost rendered as a number.
- **SC-004**: With no configuration change, 100% of invocations served by Anthropic models show a cost identical to the pre-change value.
- **SC-005**: Every cost shown carries its basis, and the CLI-reported figure is retrievable for every new entry.
- **SC-006**: A mistyped or incomplete rate card is reported at startup in every case tested, never silently ignored.
- **SC-007**: A history file written before this change loads in full with all entries intact.

## Assumptions

- "Anthropic model" for the purpose of FR-001 step (3) means a served model id beginning with `claude-`. A gateway serving a `claude-*` id is therefore treated like direct Anthropic access; the CLI figure is the best available estimate and is labelled CLI-reported. (See Open Question 1.)
- The rate card is YAML-only under `.fabrik/config.yaml`. A structured table does not fit a flag or environment variable, and `config.yaml` is the place the existing top-level unknown-key warning already covers. No CLI flag or environment variable is added.
- Rates are dollars per million tokens, matching how vendors publish prices.
- Every rate-card entry gives all four rates, because defaulting a missing cache rate would imply an accuracy Fabrik cannot guarantee.
- A rate card applies only when all served models are listed. This prevents a silently partial, understated figure.
- The only cost sources Fabrik reads are fields the CLI result or stream already carries. If no provider-reported cost is present in that data, that source never applies. (See Open Question 2.)
- #1411's served-model value and per-model token counts are the inputs. Whether per-model token breakdowns must be persisted additionally, to price multi-model sessions, is a Research question, not decided here.
- Rate cards are user data. Fabrik ships no default rates and makes no claim that they are current.
- Pricing is computed once, when the invocation completes, and stored. It is not recomputed at display time.

## Out of Scope *(optional)*

- Persisting token counts and the served model (done in #1411).
- Per-provider billing-API integrations or any new provider client.
- A shipped, maintained pricing table or automatic rate updates.
- Retroactively repricing existing history entries.
- Warning about unrecognised keys at the top level of `config.yaml`, which already exists. Making CLI/env-only keys such as `archive_after` work from `config.yaml` is a separate matter.
- Budgeting, spend limits or alerts based on cost.
- Closing #1385 or #1386. The PR must mention them only by bare number and must not use a closing keyword (`Closes`/`Fixes`/`Resolves`), because the community report is closed by hand once a release ships.

## Source References *(optional)*

- Community report #1385 and discussion #1386, with the full measurement (24 h, six workers, 355 runs / 7,143 turns, $1,481.63 reported against $57.00 billed, about $0.0080 per turn).
- #1411 / PR #2501 / ADR 1411: token counts and requested-vs-served model recorded per invocation.
- `config/unknown_keys.go`: the existing top-level unrecognised-key warning.
