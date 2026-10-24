# ADR 2427-b: A hidden, bed-only `FABRIK_TEST_FAULTS` env makes the webhook live tests provably non-vacuous

## Status

Accepted (#2427). Follows the pattern of ADR 1978 (`FABRIK_TEST_POLL_CONTROL`). Companion: ADR 2427-a.

## Context

Each live webhook test must be shown to fail when the behaviour it guards is disabled. The engine's `Set…DisabledForTest` seams are per-`Engine` Go methods called from in-process tests; a live bed is a separate daemon process, so none of them can be reached. Reverting an engine commit per test and rebuilding the bed leaves no reproducible evidence and cannot be rerun in a later release.

## Decisions

1. **`FABRIK_TEST_FAULTS` is a comma-separated list read only by `cmd/root.go`** (`engine.TestFaultsEnv` → `Config.TestFaults`). Like `FABRIK_TEST_POLL_CONTROL` it has no flag, no YAML key and no entry in the user guide. It is classified in `internal/workerenv` (`ident`, scrubbed from every worker) and is **honoured only when the poll-control seam is also enabled** (`Config.PollControlFile` non-empty), which only a bed launch sets; otherwise the engine logs that it ignored the value. When any fault is active the engine logs a loud `[startup] TEST FAULTS ACTIVE` line. Unset, every code path is byte-identical.

2. **The fault catalogue** (each a one-line gate in the path it names):

   | Fault | Effect | Guards |
   |---|---|---|
   | `drop-event:<type>` | after dedupe, skip the apply of one GitHub event type (not counted applied) | FR-002 (`issues`), FR-003 (`issue_comment`), FR-005 (`pull_request_review_thread`, `pull_request_review_comment`), FR-006 (`check_run`), FR-007 (`issue_dependencies`) |
   | `no-dedupe` | the ledger's rejection is ignored, so a redelivered copy is applied again | FR-008 |
   | `no-echo` | the echo registry never matches | FR-004 |
   | `no-replay` | no delivery replayer is built | FR-009 |
   | `no-intake-drain` | the SIGHUP restart closes the intake with no grace and a self-upgrade exec skips the quiesce, so queued deliveries are lost to the exec | FR-011 |
   | `observed-ordering` | turns the observed-at ordering rule off (the existing Store seam) | FR-001 |
   | `no-hook-config-check` | skips the App webhook-config check (R7) | FR-010 |

3. **The harness passes the operator's `E2E_NEUTRALISE` to the bed as `FABRIK_TEST_FAULTS`** at both launch sites (`tests/e2e/lifecycle.go`, `tests/gate/bed.go`), stripping any ambient value first so a shell value cannot reach an ordinary leg. A leg run with `E2E_NEUTRALISE` **runs and is archived but writes no ledger record** (like the poll-cadence override of ADR 2426 decision 5), prints a banner and is recorded in `bed.json`/`phases.json`, so a non-vacuity run can never satisfy a release gate.

4. **The procedure is documented in `tests/e2e/README.md`**: run the test on a webhook leg, run it again with `E2E_NEUTRALISE=<fault>` and `E2E_EVENTS=webhook`, and the neutralised run must fail; the table pairs every test with its fault.

## Consequences

- A few conditionals in the intake, echo, replay, exec and hook-check paths, nil-safe and inert in production.
- A mistyped fault name is logged and ignored by the engine; the harness README tells the operator to confirm the `TEST FAULTS ACTIVE` line before trusting a neutralised result.
- The neutralised runs are performed by an operator on a webhook bed; CI never runs them.
