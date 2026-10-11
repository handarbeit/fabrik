# ADR 2130: Test-running skills run targeted tests only on CI-gated stages, with a hard 5m cap and diagnose-before-retry

## Status

Accepted. Amends ADR-1997 Decisions 3 and 4.

## Context

ADR-1997 let a worker skip the local full-suite run when `.fabrik-context/ci-status.md` showed `verdict: green` on the current head. In every other case the skills said to "run the step exactly as written", which is the full suite. A comment-review pass that pushed a fix therefore left the next Validate invocation with `verdict: pending`, `ci_gated: true` and HEAD equal to `head_sha`: the skip failed, and the "after you change code" rule did not apply because this invocation had not changed anything.

That is #2097 on 2026-10-11. Validate ran about seven overlapping full or near-full `go test -race` invocations with 8–20 minute timeouts, five of them past the Bash tool's 590 s limit and backgrounded, and spent its turns polling. One run panicked on a timeout; the worker retried more broadly instead of asking why, and a later standalone run of the same package passed in 26 s (host load). The stage hit its 60 minute `max_wall_time` without completing, while CI ran the same suite on the same commit in 7–8 minutes. The skills already said `-timeout 5m` and "run in the foreground", but only as advice.

## Decision

1. **One decision table in every test-running skill** (`fabrik-validate`, `fabrik-review`, `fabrik-implement` and their `-comment` variants), keyed on `ci-status.md`:
   - absent or `ci_gated: false`: the full-suite step, unchanged (`PASSED`);
   - `ci_gated: true`, `green`, HEAD equal to `head_sha`, clean tree: skip (`SKIPPED — CI green on <sha>`);
   - `ci_gated: true`, `red`: read the failing checks (`gh pr checks` or the run log) and reproduce only those tests;
   - `ci_gated: true`, `pending`, `unknown` or `none`, or HEAD differs from `head_sha` (including a head pushed by a *previous* invocation), or a dirty tree: the build plus the tests for the packages the diff touches, once.

   Both targeted cases are reported as `TARGETED — <packages>; full suite left to CI on <sha>`. On a CI-gated stage the full suite is never run locally; CI and the engine's `wait_for_ci` gate decide it. This amends Decision 3 (which only covered `green`).
2. **The `-comment` variants follow the file's `ci_gated`** instead of treating it as `false`. This reverses ADR-1997 Decision 4. That decision reasoned that a comment-pushed head has no `wait_for_ci` gate behind it. The reasoning is true of the stage-completion gate alone, but the head is still covered: Validate is re-dispatched, and the landing path re-checks CI on the head (`attemptMergeOnValidate` / `classifyLandingCI`). Only Validate is `ci_gated: true` in the shipped configuration, so the change is inert in Review and Implement. Keeping the override would leave the full suite open in `fabrik-validate-comment`, which is where #2097 started.
3. **`verdict: none` is treated like `pending`/`unknown`.** CI is still the stage's gate; the issue names only the other three, and this is the conservative reading.
4. **Test command rules are hard rules**, repeated in each skill: a framework timeout (Go `-timeout` of 5m or less) that is never raised to fit a broader run (narrow the run instead), no backgrounding a test command and polling it, and diagnose before retrying. A timeout or timeout panic is answered by checking host load and rerunning only the failing test, once; never a broader set or a longer timeout. A timeout that does not reproduce in isolation is reported as host load and is neither a fix trigger nor a failure of the change. The 5m cap is per `go test` invocation (per test binary); a `ci_gated: false` full-module run that cannot fit the Bash limit is split into foreground package groups, each with `-timeout 5m`.
5. **Host check is `uptime`, with a fallback.** `uptime` is not in the default Bash allowlist, so under `dontAsk` it may be denied. The skills say not to retry a denied `uptime` and to use the failing test's own elapsed time from a single `-v` rerun instead. Adding `Bash(uptime:*)` to the default tool set is an engine change and is left as a follow-up.
6. **The report wording is fixed**: the Test Suite line, the PR description audit and the final summary use exactly one of `TARGETED — <packages>; full suite left to CI on <sha>`, `SKIPPED — CI green on <sha>` or `PASSED` (full run, `ci_gated: false` only).
7. **The block is duplicated across the six skills**, as the ADR-1997 subsection already was; there is no include mechanism. `plugin/ci_gated_skill_rules_test.go` guards the drift: every skill must carry each rule's anchor wording, no `go test … ./...` line may appear without a `ci_gated: false` condition within the preceding eight lines, and no `-timeout` may exceed 5m. The scanners are pure functions, tested against fixtures that show they fail when the wording is neutralised or an unconditional full-suite or over-cap instruction is reintroduced.

## Consequences

- A run like #2097 verifies locally in minutes: a targeted build and test, with CI delivering the full-suite verdict. `max_wall_time` stays the only hard stop; skill text steers a model and is not enforced.
- A comment-pushed Validate fix is no longer verified by the full suite locally. A regression outside the touched packages is caught by CI and the landing-time check, not by the worker.
- The engine is unchanged. `renderCIStatus`'s preamble, which says the file licenses skipping a full-suite run when green, remains accurate and is left alone.
- Alternatives rejected: keeping the ADR-1997 override for comment variants (leaves the full-suite path open where #2097 began); applying the table to Validate comments only (an inconsistency between sibling skills); factoring a shared block (no mechanism); a `max_wall_time` change or engine-side enforcement (out of scope).
- ADR-1857 still holds: CI runs the whole module on every PR, which is what makes "the full suite is CI's job" true for this repo. The skills do not encourage test selection on CI.
