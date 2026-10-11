# ADR 2116: Targeted reproduction on a red CI verdict; tell the worker what failed

## Status

Accepted. Amends ADR 1997.

## Context

ADR 1997 licenses skipping a local full-suite run only on `verdict: green`; every other verdict sends the worker back to the full suite. For `red` that is the wrong response: CI has usually run the whole suite already and often one unrelated check failed. On 2026-10-11 a Validate CI-fix worker (#2096) ran the full sim suite locally while three other workers ran broad suites; the host saturated and the run produced load-induced failures CI did not show. The `-comment` skills also treat `ci_gated` as `false` unconditionally ("a push from comment processing has no CI gate behind it"), which is untrue of the CI-fix reinvoke: it is dispatched from the gate, and the gate re-evaluates the new head. Finally, the shipped skills named `go test` and `-race`, although they serve projects in every language.

## Decision

1. **`red` means targeted.** The six test-running skills reproduce only the failing checks (the failing tests or the smallest unit containing them; a non-test check's own command), re-run those plus the tests covering the changed code after a fix, and leave the full-suite verdict to CI where a gate follows. `pending`, `none` and `unknown` are unchanged (run as written, plus the load check): there is no finished suite to defer to.
2. **`ci-status.md` says what failed.** For a `red` verdict the engine adds a `## Failing checks` section (latest run per check name that is `failure`/`timed_out`/`action_required`), rendered with the CI-fix prompt's helper (`checkRunFailureDetail`, ADR-1991's bounds) including the new `CheckRun.OutputTitle` (`output.title`). Caps: 10 listed, annotations read for the first 5. **Annotations only**: they need `checks:read`, which the engine already holds, and are runner-neutral. Job logs are not fetched (a new client method, archive handling, size limits and the `actions` permission for modest gain over the details URL), and failing test names are not parsed out of text — language-specific regexes would violate language neutrality and rot. Every extra read is live (`e.client`) and soft-failing; on failure the section keeps the check-level fields.
3. **`ci_gate_ahead`.** The engine also writes whether any later managed stage waits for CI, from stage config. Implement and Review report `ci_gated: false` although Validate normally gates the final head; without this line the skills would hedge or keep demanding the full suite. `ci_gated` and `ci_gate_ahead` are the authority for "is this push re-gated?"; the skills carry a per-path explanation (stage dispatch, comment review, CI-fix/review/rebase reinvokes, merge-train auto-repair) but tell the worker to trust the flags, since stage YAML is user-editable. When both are `false` the skill still requires a local run broad enough to stand in for the missing gate.
4. **ADR 1997's blanket `-comment` rule is removed**, replaced by the per-path statement.
5. **Relation to ADR 2130.** ADR 2130 (landed on `main` while this was in review) already turned the "Skipping a redundant full-suite run" subsection into the "Which tests to run locally" decision table and removed the blanket `-comment` rule. This ADR keeps that table and builds on it: "CI-gated" in its rows becomes "a CI gate stands behind the push" (`ci_gated` or `ci_gate_ahead`), the `red` row points at the `## Failing checks` section and the red rule, and the per-path list and load rules are added. Its Go-specific wording (`go test -race -timeout 5m`) is reworded per R5; the 5m cap and the other hard rules are unchanged.
5. **Load awareness.** Before a run broader than the failing tests the worker checks `uptime` against the CPU count; a failure that does not reproduce in isolation and passes in CI on the same head is reported as load-induced and not fixed; no waiting on a suite in a `sleep` loop beyond the test timeout.
6. **Language-neutral skills.** The six skills name no single toolchain's commands; `plugin/ci_status_skill_test.go` pins the wording and the absence of `go test`, `go build`, `go vet`, `-race`, `./...` and `gofmt`. Fabrik's own Go commands live in `CLAUDE.md`.

## Consequences

- A red head costs at most five extra REST reads per invocation (annotations), none for other verdicts.
- `ci-status.md` text changes (new line, new paragraph, new section); nothing parses it but workers.
- `ci_gate_ahead` over-trusts a later gate: a pipeline whose later gated stage is skipped is not detectable at invocation time. Accepted; Validate precedes landing in every stock configuration and the targeted tests still run.
- The file is a start-of-invocation snapshot; a flaky red the engine re-runs (ADR 2072) may be green by the time the worker acts. The skills' "does not reproduce" wording covers it.
