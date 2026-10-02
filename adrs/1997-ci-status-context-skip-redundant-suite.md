# ADR 1997: CI status context file lets workers skip redundant full-suite runs

## Status

Accepted

## Context

The Implement, Review and Validate skills (and their `-comment` variants) tell the worker to run the project's full test suite at several points, often on a PR head CI has already run the whole suite on. For Fabrik, CI runs the whole module under `-race` on every PR (ADR-1857), so each such local rerun repeats finished work and loads the host (a named cause of sim timeouts and `-race` crashes in the 0.0.83 e2e gate). Issue #884 proposed a heavy design (branch-protection and commit-status fetchers, a cache, config fields, an agent-written log, daemon-side staleness re-dispatch). Most of that is unnecessary: the engine already judges "green and complete" for a PR head (`classifyLandingCI`, `ciSuiteHold` — ADR-1822), and `wait_for_ci` stages already defer `stage:<name>:complete` until CI passes on the pushed head.

## Decision

1. **The engine writes `.fabrik-context/ci-status.md`** (`engine/ci_status.go`, called from `writeContextFiles` ahead of the `pr-description.md` block, so it covers stage and comment-processing invocations alike). It records the PR number, the live PR head SHA, a verdict, `ci_gated` (the stage's `wait_for_ci`) and a timestamp, plus a table of every check run. No linked open PR means no file, and any stale file is removed.
2. **Verdict rules, all conservative.** Reads are live on `e.client`. Any read error the writer sees is `unknown`. Zero check runs is `none`, decided before the classifier so the ADR-933 `mergeable_state` green path can never license a skip. Otherwise `classifyLandingCI` is used unmodified (`TrainCIGreen`/`Red`/`Pending` → `green`/`red`/`pending`; a suite hold or a suite-read error is `pending`). Because `gh.ClassifyCheckRuns` fails only `failure`/`timed_out`/`action_required`, a writer-local guard demotes `green` to `pending` unless every latest run per name is completed with `success` or `neutral` (`skipped` is not passing — a path-filtered test job reports it without running the suite). The guard lives in the writer so landing gates are untouched, and it only ever removes a skip.
3. **Skills skip the suite only on a conjunction**: `verdict: green`, `git rev-parse HEAD` equal to `head_sha`, and an empty `git status --porcelain`. Any other case runs as before. The worker-side checks are essential because `updateWorktreeFromMain` may rebase the worktree onto a moved base before the invocation, so the engine cannot know the local HEAD when it writes the file.
4. **After a change** (including a rebase) in a `ci_gated: true` stage, the worker runs build plus tests for what it touched and pushes; the `wait_for_ci` conjunctive gate on the new head is the backstop. With `ci_gated: false` no gate backstops it, so the full suite runs as before.
5. **Only the test invocation is skipped.** Validate's requirements verification, regression check, PR description audit, rebase and Pre-Completion Gate, and Review's code review, still run. Reports say `SKIPPED — CI green on <sha>` rather than `PASSED`.

## Consequences

- No new configuration, no branch-protection or commit-status reads, no cache (R2). Per invocation the cost is a handful of REST reads.
- Legacy commit statuses (non-check-run CI) show up as `none` and never skip. This is safe; supporting them is a follow-up outside this ADR.
- The skip fires less often on busy repos, where a base that moved makes local HEAD differ from the pushed head. This is the safe direction.
- In practice only Validate is `wait_for_ci: true` by default, so the targeted-tests rule mainly applies there; Implement and Review benefit from the skip only on an unchanged green head.
- Supersedes the design of #884.
