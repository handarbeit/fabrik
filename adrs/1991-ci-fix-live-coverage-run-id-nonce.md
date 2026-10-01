# ADR 1991: Live CI-Fix Reinvoke Coverage — Run-ID Acknowledgement Nonce, Annotations in the Prompt, Logged Prompt

**Date**: 2026-10-01
**Status**: Accepted
**Issue**: #1991 — e2e: restore live coverage for the CI-fix reinvoke path

## Context

`TestCIFixReinvoke` is the only live coverage of the CI-fix reinvoke positive path (CI
fails, the engine reinvokes the stage with the failure, the fix lands). It was skipped
"pending #916", but #916 was closed not-planned, so the path had no live coverage for
~2.5 months. The difficulty: a capable Implement agent reads `ci.yml`, sees the
`ci-fix-sentinel` condition, and satisfies it in its first commit; the first CI run is
green and the reinvoke never fires. Telling the agent not to fix CI does not hold.

The issue also requires that the test prove the reinvoke **prompt carried the CI failure**,
not merely that the engine retried. That was unobservable: `buildCIFixComment` embedded
only check names and conclusions (no output at all), and the synthetic comment is never
posted to GitHub — the engine logged comment IDs only.

## Decision

1. **A run-ID acknowledgement nonce, held in the bed workflow.** For a PR whose body
   carries `ci-fix-sentinel-ack`, the bed's sentinel resolves `T` = the ID of the earliest
   `ci.yml` `pull_request` run on the branch and passes only if root file `CI_FIX_ACK`
   contains `ack:<T>` and the current run is not run `T`. Run `T` is red by construction;
   `T` is a future GitHub-assigned ID, not derivable from the worktree, git history or
   `ci.yml`. The sibling `-required` / `-unfixable` branches are untouched.
2. **Annotations in the CI-fix prompt.** `buildCIFixComment` appends, per NEW-REGRESSION
   check, up to 5 annotations (`github.FetchCheckRunAnnotations`, first page, live via
   `e.client`, optional `checkRunAnnotationFetcher` interface) plus any
   `output.summary`/`output.text`, capped, fail-soft. Annotations are the only confirmed
   channel for an Actions `::error::` message; the nonce reaches the agent only this way.
3. **The prompt is logged.** `dispatchCIFixReinvoke`'s `build` hook logs
   `[#N ci-fix-reinvoke] prompt (<n> bytes): "<%q body>"`, bounded to 4 KB. The live test
   independently derives `T` from the Actions API and asserts the logged prompt contains
   the check name and `ack:<T>`, then that the fix commit's `CI_FIX_ACK` carries it. If the
   CI-fail handler stops reinvoking, the line never appears and the test fails.
4. **Fail loud, never skip.** Every timeout/assertion is `Fatalf`; a bed without the ack
   branch fails in preflight with a pointer to the README. Only the existing environment
   gates (bed not running, sentinel not enrolled as a required check) skip, and none cites
   an issue. The first-run-red precondition is checked as soon as `fabrik:awaiting-ci`
   appears and names a green first run as the agent pre-empting the sentinel.

## Alternatives rejected

- **Repo variable held by the harness (issue option b).** The worker's `GH_TOKEN` may be
  able to read or write Actions variables, so "the agent cannot see or change it" needs a
  bed-token-scope proof; clearing a variable does not turn a red check green (it needs a
  rerun trigger); and the prompt-content proof (annotations + log) is needed either way.
- **First-commit-SHA nonce (PR #103 in the bed).** Bypassable: an agent that reads `ci.yml`
  can make two local commits and push both; the first push is then green.
- **Raw `$GITHUB_RUN_ID` as the nonce.** The fix commit triggers a new run with a new ID, so
  it would never match.
- **Delivering failure context via `$GITHUB_STEP_SUMMARY` / `output.summary`.** #916
  confirmed it never reaches the check run's `output.summary` for Actions jobs.
- **Widening `GitHubClient`/`ReadClient` with the annotations method.** Would touch every
  mock and sim implementer; a type assertion keeps the engine change narrow.

## Consequences

- The bed change lives in the external `handarbeit/fabrik-test-alpha` repo and is a manual
  prerequisite (exact script in `tests/e2e/README.md`); a cross-repo spawn would run the
  Fabrik pipeline against the sentinel workflow agents must not touch. The live run cannot
  happen until it is applied.
- One remaining bypass: an Implement agent that pushes, waits for run 1 to go red and fixes
  it before Validate. The body forbids waiting on or inspecting CI; if it happens the
  harness fails fast with a named diagnostic.
- Prompt content may include check output; annotations are already public on the PR and the
  4 KB bound limits exposure.
- The sim twin (`tests/sim/ci_fix_reinvoke_test.go`) asserts the `ci-fix-synthetic` body
  names the failing check and embeds its output; real Actions annotations are `live-only:
  real-ci`. The #1933 registry entry and #1975 leg conformance are deferred until those
  formats exist; the sim README row carries the entry meanwhile.
- A `ci-fix-reinvoke` log line is not a charged cycle (ADR-1812 refunds a did-not-run
  reinvoke) — tests assert presence, not counts.
