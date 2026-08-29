# ADR 1676: `scripts/e2e/run.sh` Hang Hardening — Timeouts, Post-Suite Watchdog, Stall Detector

**Date**: 2026-08-29
**Status**: Accepted
**Issue**: #1676 — run.sh can hang forever after go test exits — unguarded network probe, no
watchdog, no stall detection

## Context

During the v0.0.81 cut, `scripts/e2e/run.sh` hung for **17 hours 19 minutes**, with its log
untouched for the last ~19 of them, while the `e2e.test` binary was long gone (`go test` had
already exited). Nothing in the log indicated a failure — the suite had completed 71 passes;
the script simply never proceeded past the test invocation. Cost containment was luck (the
bed happened to go idle around 04:30, ~$28) rather than design.

The root cause: `switch_and_run` calls `gh api rate_limit` twice per leg — once before the
suite invocation (`budget_before`) and once after (`budget_after`) — to report GraphQL budget
consumption (A3 of #1527). Both calls had **no timeout**:

```sh
budget_after=$(GH_TOKEN="$BED_TOKEN" gh api rate_limit --jq '...' 2>/dev/null || echo "")
```

`|| echo ""` guards a *failing* call, not a *hanging* one — exactly the distinction that
mattered here. These were confirmed the only two `gh api`/`curl`/`wget` calls in the script
outside the `go test -tags=e2e` invocations themselves.

This script gates every release cut (`scripts/cut-release.sh` invokes it) — any fix must be
conservative about false positives, since a spurious abort blocks a release exactly as
effectively as a real hang would, just faster.

## Decision

Three independent layers, matching the issue's own R1–R3:

### R1 — `with_timeout <seconds> <cmd...>`, wrapping both `gh api rate_limit` calls

A new helper near `run_reaped` (the file's existing "background job, reap on kill"
precedent), implemented as portable bash job control — background the guarded command under
`set -m` (which gives it its own process group), background a `sleep <seconds>` watcher that
group-kills it on expiry, `wait`, then report a marker-file-detected timeout as exit 124
(matching GNU `timeout(1)`'s convention). Chosen over `perl`/`python3` (the issue's own
Prior Art lead): both are confirmed present, but the file has zero existing interpreter
dependency, and this is exactly the idiom `run_reaped`/`stop_bed_instance` already establish.
`E2E_GH_API_TIMEOUT` (default 30s) controls the deadline — generous for what are lightweight
REST metadata calls.

**Critical correctness constraint, found empirically during Implement: `with_timeout` must
never be invoked via `$(with_timeout ...)`.** Command substitution always runs its command
list in a subshell, and `set -m` job control's one-process-group-per-background-job behavior
does not apply inside that subshell — a job backgrounded there keeps the *substitution
subshell's own* process group instead of getting one of its own. The deadline watcher's
group-kill (`kill -TERM -"$pid"`, a negative PGID) then silently targets a process group that
was never created (`kill` exits 1, "No such process"), the guarded command is never actually
killed, and it just runs to completion — or hangs forever — entirely unbounded, with no
visible error. This would have silently defeated R1 at both of its only two real call sites
(`budget_before=$(with_timeout ...)`, `budget_after=$(with_timeout ...)`), reproducing the
exact class of hang this issue exists to fix, in code that looked correct and passed a
timeout test invoked outside a subshell. The fix: both call sites capture output via a temp
file instead —

```sh
budget_before_tmp="$(mktemp)"
if with_timeout "$GH_API_TIMEOUT" env "GH_TOKEN=$BED_TOKEN" gh api rate_limit --jq '...' \
    > "$budget_before_tmp" 2>/dev/null; then
  budget_before="$(cat "$budget_before_tmp" 2>/dev/null || echo "")"
fi
rm -f "$budget_before_tmp"
```

— invoked as a plain foreground statement, `with_timeout` runs in the caller's own shell
(never a substitution subshell), where `set -m` correctly assigns each backgrounded job its
own process group and the group-kill works as designed. `scripts/e2e/hang_hardening_test.sh`
carries a permanent regression guard (Case 4b) asserting `$(with_timeout ...)` genuinely does
NOT enforce its deadline, so a future refactor back to that pattern at a real call site fails
this test instead of silently reintroducing the hang.

### R2 — post-suite watchdog

A background watchdog is armed the instant `go test` exits (right after `wait "$suite_pid"`)
and disarmed at every one of `switch_and_run`'s own exit points (both normal returns and the
`BUDGET_EXHAUSTED_EXIT` path). Mechanism: a checkpoint file, updated as the post-suite tail
progresses (consumer drain → `budget_after` probe → `report_test_timings` → failure
classification → `detect_rate_limit_backoff`), and a deadline watcher armed inline (not
through a shared "arm" helper returning a PID via `$(...)` — the same subshell/job-control
hazard as R1 applies) that signals the *script's own PID* (`$$`) via `SIGTERM` on expiry. A
`TERM` trap installed for this window checks a `fired` marker at signal-delivery time: if
present, this was the watchdog, and it prints a diagnostic (elapsed time since `go test`
exited, the last checkpoint reached, the last engine-log line) and exits
`POST_SUITE_WATCHDOG_EXIT` (6, extending the file's existing distinct-exit-code convention:
`BUDGET_EXHAUSTED_EXIT`=3, `PREFLIGHT_FAILED_EXIT`=4, `PREGATE_FAILED_EXIT`=5); if absent,
this was some other `TERM` (e.g. an external kill) landing in the same window, and it falls
through to the pre-existing suite/consumer group-kill-then-exit-143 behavior unchanged.
`E2E_POST_SUITE_WATCHDOG` (default 300s) controls the window — generous relative to what the
watched tail actually does (a couple of REST calls plus `jq` parsing, normally seconds).

**Chosen over a foreground per-step elapsed check.** A foreground check can only observe
elapsed time *between* statements — it cannot detect or interrupt a single statement that is
itself hung, which is precisely the incident's failure mode. Only an independently-scheduled
background process can enforce a deadline against a stuck foreground statement.

**Known, accepted gap:** by the time the watchdog could fire, R1 has already removed the only
two calls this issue identifies as hang-capable in this tail — so in the common case R2
should never actually fire. It exists as a backstop for a future regression (a call added to
this tail without routing through `with_timeout`), and cannot group-kill a hypothetical
stuck *foreground* statement it doesn't itself own a process group for — it can only
terminate the whole script via `SIGTERM $$`. This is an intentional trade-off: building full
group-kill machinery for a step that, by construction once R1 lands, should never need it,
was judged not worth the added complexity. See the "KNOWN GAP" precedent elsewhere in this
file for the same documented-not-solved convention.

### R3 — stall detector on suite output

Independently of R2, a background loop (started alongside the existing `tee`/`jq` consumer,
torn down immediately once `go test` exits rather than waiting out its own ~60s poll
interval) polls the JSON log's mtime — not the terminal/tee output a human might be watching,
since the incident's own `ps` evidence shows the run was backgrounded with no one watching a
terminal. On `E2E_STALL_WARN_MINUTES` (default 15) of no mtime change while `go test` is
still alive, it logs a warning naming the last completed scenario (`last_completed_test_name`,
reusing `report_test_timings`'s own terminal-action `jq` filter) and the silence duration,
then resets its own counter so a long-quiet run warns once per window rather than once ever.
Purely advisory — never touches `$rc`, never aborts (R4).

The isolated `TestMergeTrainRunawayGuardPausesBatch` leg (runs alone, deliberately idle for
long stretches while a 1-hour-windowed runaway guard is armed) is expected to trigger this
warning on every healthy run — documented in `tests/e2e/README.md` so it doesn't read as a
regression.

### R4 — non-gating, for free

The budget probe is a report, never a gate: a timeout under R1 degrades exactly like the
pre-existing "call failed" case (the `budget_before=""`/`budget_after=""` fallback plus the
existing `[ -n "$budget_before" ] && [ -n "$budget_after" ]` gate), never affecting the
suite's own exit code. R3's warning is likewise purely advisory.

### Defaults kept at the issue's proposed starting points

30s / 5m / 15m, not further tuned: nothing in the post-suite tail plausibly approaches
minutes, and the one measured real-world inter-event gap on record (5½ minutes,
`TestReviewAuthorityClearsOnApproval`, multi-scenario "on" leg) sits comfortably under the
15-minute R3 default. See `tests/e2e/README.md`'s "Hang hardening" section for the full
derivation and rationale for keeping rather than changing these values.

## Consequences

- A hung `gh api rate_limit` call can no longer block the script indefinitely — it is killed,
  whole process group, within `E2E_GH_API_TIMEOUT` (default 30s), and degrades to a skipped
  budget report exactly as a failed call already did.
- A future regression in `switch_and_run`'s post-suite tail (a new call that forgets to route
  through `with_timeout`) fails loudly with a diagnostic naming the stuck step within
  `E2E_POST_SUITE_WATCHDOG` (default 5m), instead of hanging silently for hours.
- Prolonged silence in the suite's own output, while `go test` is still running, is now
  surfaced (not gated) — an operator watching a long run can distinguish "still working" from
  "gone quiet with no signal either way."
- `with_timeout` must never be called via `$(...)` — this is now a load-bearing, tested
  constraint (`hang_hardening_test.sh` Case 4b), not just a comment. Both real call sites use
  the temp-file-capture pattern instead.
- `scripts/e2e/hang_hardening_test.sh` provides permanent regression coverage for `with_timeout`
  (including the group-kill discipline and the `$(...)` hazard), the deadline-watcher/marker
  pattern R2 uses, and `last_completed_test_name` — mirroring `backoff_detection_test.sh`'s and
  `pregate_test.sh`'s existing precedent for shell-level verification of this file.
