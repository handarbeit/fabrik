# Fabrik end-to-end tests

Scenario-driven integration tests for Fabrik. Each test drives a real Fabrik
instance (`~/dev/fabrik-test/`) against the real test repositories
(`handarbeit/fabrik-test-alpha`, `handarbeit/fabrik-test-beta`), files an
issue, and asserts on the resulting pipeline behaviour.

## What this is for

These tests catch regressions that escape `go test ./...` — bugs that only
manifest in real integration with GitHub, real Claude invocations, real
worktrees, and the real end-to-end stage pipeline. The category of bug:

- The `addBlockedBy` GraphQL mutation name (shipped broken in v0.0.66; fixed by #800)
- The pre-Implement spawn step failing on never-touched repos (#797, #803)
- The `Closes #N` getting absorbed into a nested code fence (#738)
- The CI gate `HeadSHA` resolution in poll-only mode (#779)

Every such regression that escapes a release earns a new scenario here.

## Where this lives in the release flow

Four layers, not two (#1454 — see `adrs/1454-sim-pre-gate-not-replacement.md` for the
full layering decision). **The sim bed is a fast, free pre-gate. It is never a
replacement for this suite** — real Claude, real review bots, real GitHub wire
behaviour, and real merge-queue semantics have no substitute, and this suite runs in
full, before every release, permanently.

```
[ go test ./... ]               unit tests; fast; run on every PR
        |
        v
[ sim e2e (tests/sim) ]         fast sanity check: full pipelines, failure/timeout/
        |                       restart/merge-train paths — seconds, $0 tokens, -race;
        |                       already part of `go test -race ./...`, so also every PR
        v
[ github wire-contract tests ]  wire-format truth: schema + recorded fixtures (github/);
        |                       also already part of `go test -race ./...`, every PR
        v
[ tests/e2e/... ]                integration truth: real Claude, real bots, real
        |                       GitHub — slow, costs real tokens, runs before release
        v
[ scripts/cut-release.sh ]      cuts a release
```

Each layer's blind spot, most important first:

- **sim e2e is permanently blind to GraphQL/REST wire correctness** — `tests/sim`'s
  GitHub client (`simgh`) is a hand-modeled fake; see `tests/sim/README.md`'s "What this
  layer is permanently blind to" and `tests/sim/simgh/FIDELITY.md` for the maintained
  ledger of every known divergence from real GitHub. Closed only by the wire-contract
  tests below and by this suite.
- **The wire-contract tests validate shape, not runtime behavior** — a query can be
  schema-valid and still be logically wrong. They prove "this is a query GitHub's schema
  accepts," not "this is the query that produces the right result." That's what sim e2e
  and this suite are for.
- **This suite (`tests/e2e`) has no structural blind spot** — it's the only layer that
  exercises real Claude, real review bots, and real GitHub wire behaviour together. That
  is exactly why it is never reduced or retired, no matter how much sim coverage grows.

Wired into `scripts/cut-release.sh` (R2, #1454): the sim suite and wire-contract tests
run as an unconditional, never-skippable pre-gate step; the live suite (this directory)
runs as a mandatory-by-default step immediately after build+test. The one sanctioned
escape hatch is loud and recorded, never silent:

```bash
scripts/cut-release.sh v0.0.67                              # default — runs the full live suite
scripts/cut-release.sh v0.0.67 --skip-integration=<reason>  # loud, recorded escape hatch —
                                                              # the reason ships in the release notes
```

A bare `--skip-integration` (no reason) is a hard usage error, by design — see
`adrs/1454-sim-pre-gate-not-replacement.md`'s R2 section for why "no flag at all" was
rejected in favor of this loudly-labelled one.

**The live gate is coverage-based, not invocation-based (#1972, ADR-1972).** A ~10-hour
run rarely survives unbroken — GitHub has outages, a scenario hits a harness race, the
operator suspends the run, the quota runs out — and none of the passes already recorded
for the same engine SHA should be thrown away. The gate runner records every live
scenario's outcome, per leg, in a durable per-SHA ledger as it streams in, and
`cut-release.sh` accepts the live gate when **every required (test, leg) pair has a valid
PASS for the SHA being released**, across however many invocations that took. Step 5 asks
`scripts/e2e/run.sh coverage` first, runs `scripts/e2e/run.sh --clean --resume` (only the
missing pairs) if coverage is incomplete, asks again, and records the engine SHA and the
number of gate invocations in the release notes. Re-running `cut-release.sh` after an
interrupted gate therefore resumes it. See "Coverage ledger, `--resume`, and suspending a
run" below.

`scripts/e2e/run.sh` itself also runs the sim + wire-contract pre-gate first (R1,
#1454) — before any bed preflight, build, or live call — whether invoked standalone or
via `cut-release.sh`, so a live-gate run never spends live budget on a bug the free
layers would have caught for $0.

**`run.sh` is a shim over a Go program (#1994, ADR-1994).** The orchestration — preconditions,
pre-gate, bed preflight and start, the auth × train legs, hang protection, the reports, and
the exit-code contract `cut-release.sh` keys on — lives in [`tests/gate`](../gate/README.md)
(`tests/gate/cmd/gate` is the `main`). It compiles **without** the `e2e` build tag, so its
tests run in plain `go test -race ./...` on every PR, and it sits beside rather than under
this directory so the live legs' `./tests/e2e/...` package set (and their outcome report)
is untouched. `scripts/e2e/run.sh` builds it and `exec`s it, so the runner — not a
`go run` wrapper — receives INT/TERM and reaps everything it started. Every flag and
environment variable below keeps its meaning. `scripts/e2e/reset.sh` is likewise a shim
(`gate reset`). The shim builds into `${E2E_GATE_BIN_DIR:-$TMPDIR/fabrik-e2e-gate-<uid>}`.

## Test bed prerequisites

These tests assume:

1. **`~/dev/fabrik-test/` exists** with `.fabrik/config.yaml`, `.env`
   (containing `FABRIK_TOKEN` for `@arbeithand`), and a built `fabrik` binary.
2. **`handarbeit/fabrik-test-alpha`** and **`handarbeit/fabrik-test-beta`**
   are reachable with the token.
3. **`handarbeit/projects/2`** ("Fabrik Test") exists with stage columns
   (Backlog, Specify, Research, Plan, Implement, Review, Validate, Done).
4. **No other Fabrik instance is using the `@arbeithand` token's GraphQL
   budget concurrently** (or use `--max-concurrent 1` if you have to share).
   As of #1684, `scripts/e2e/run.sh` checks this automatically before any
   live call and refuses (rather than silently discovering it via backoff 78
   minutes in) if it finds a competing local instance — see "Operational
   up/down contract" below for the full mechanism and escape hatch
   (`E2E_SKIP_TOKEN_CHECK=1`).

5. **For the App-mode legs (#1861):** the bed's `.env` also carries the
   GitHub App identity as `E2E_APP_ID`, `E2E_APP_PRIVATE_KEY_PATH` (relative
   to the bed dir, e.g. `.fabrik/github-app-key.pem`) and
   `E2E_APP_INSTALLATION_ID`, for an org-owned App installed on both test repos
   with `contents: write` (git runs over HTTPS as the installation — ADR-1846).
   `.fabrik/config.yaml` must **not** set `github_app_*`: auth mode is applied
   per leg through `.env`, and a config key would silently turn every PAT leg
   into App auth. `run.sh` refuses up front if either is wrong.

See `~/fabrik-oss-launch-notes.md` (under "Files and where they live") for
the canonical setup.

## GraphQL budget

The bed token has 5,000 GraphQL points/hour, shared by the suite and the
test-bed engine. Measured 2026-07-28:

| Operation | Cost |
|---|---|
| `gh project item-list --limit 200` | ~101 pts |
| `gh project field-list` | ~106 pts |
| `fetchBoardItems` / `fetchStatusField` (harness.go) | ~2 pts |
| Engine board poll, warm cache | ~4 pts |
| Engine board poll, cold bootstrap | ~349 pts |

**The harness must not read the board through `gh project` subcommands.** They
resolve field values with a follow-up query per item, so cost scales with the
requested limit rather than with the data needed. When the wait-helpers used
them, a single scenario polling on a 10-15s interval cost ~24,000 pts/hour —
nearly 5x the whole budget — and a full `E2E_PARALLEL=4` run exhausted the
token in ~14 minutes. Everything after that fails with `gh` exit 1, which
looks like a pile of engine regressions but is not.

Use `fetchBoardItems` / `fetchStatusField` instead; they return the same data
for ~2 points by resolving Status inline via `fieldValueByName`. At that cost a
parallel-4 run is ~1,900 pts/hour, plus ~480 for the engine — roughly 2x
headroom. `gh project item-add` / `item-edit` are one-shot mutations and are
fine as-is.

`E2E_PARALLEL`'s default of 4 is the same value #971 originally picked, but is
now re-justified against this budget math rather than carried over
unrevisited — see "How the timeout/parallelism defaults are derived" below.
Raising `E2E_PARALLEL` (or raising `E2E_TIMEOUT` while holding parallelism
fixed, which widens the window of concurrent activity) should be re-checked
against this ~2x headroom before changing either default.

Once board reads were cheap, the residual cost became the issue/PR wait-helpers
(`gh issue view --json`, `gh pr list --json` — also GraphQL, ~1 pt each) polling
on behalf of a dozen parallel scenarios. Those run on `pollBase()`, default 30s,
overridable:

```bash
E2E_POLL_INTERVAL=15s scripts/e2e/run.sh -run TestCruiseFullPipeline
```

Shortening it is safe for a single scenario in isolation, where budget is not a
constraint; leave it at the default for a full parallel run.

`WaitForIssueClosedWithReviewCheck` (#1396) adds one `gh pr view --json
isDraft,createdAt,reviews` call (also ~1 pt) per poll cycle, on the 7 call
sites that adopt it, only until a review lands or the fail-fast check fires
— a bounded addition well inside the ~2x headroom above.

If you hit the limit anyway, check the reset time and wait it out:

```bash
gh api rate_limit --jq '.resources.graphql'
```

### The two-mode gate's "on" leg: a separate, larger cost driver (#1527)

The `~2x headroom` math above covers the suite's own harness reads and the
engine's ordinary board polling — it does **not** account for the extra cost
merge-train mode (`FABRIK_MERGE_TRAIN=on`) adds on top. During the 0.0.78
pre-release gate (2026-08-09/10), the `on` leg alone drove the bed's GraphQL
budget from a near-full 4,871 points down to 19% remaining **twice** in one
run — burning more than a full hour's allowance and then more again after the
reset — while the `off` leg (69 pass, 0 fail, 0 backoff events) was unaffected.
The failures that resulted were **all timeouts**, not assertion violations:
once backoff engages, polling slows enough that board items sit in `Queued` /
`fabrik:awaiting-ci` past scenarios' wait deadlines.

**Root cause, confirmed by static analysis (not the merge-train worker's own
CI polling — that's REST, a separate budget):** the ADR-1270/ADR-1208 settle-
scan pattern —
[`settleAwaitingCIScan`](../../engine/ci_settle.go) (both legs, any
`wait_for_ci` stage) and, merge-train-exclusively,
[`settleQueuedReviewFindings`](../../engine/queued_review_settle.go) — each
performs an unconditional, no-cooldown `FetchItemDetails` **GraphQL deep-fetch**
(the single most expensive query in the codebase: `comments(first:100)` +
`closedByPullRequestsReferences` with nested `comments`/`reviewThreads`/
`latestReviews`/`reviewRequests`) for every matching item, every poll cycle.
This is a *documented, correctness-motivated trade-off*, not a bug —
`settleQueuedReviewFindings` exists specifically to close a review-finding
blackout window on `Queued` merge-train members (ADR-1208), and its "every
member, every poll" semantics are load-bearing for that guarantee. Cost is
therefore proportional to **how many items are concurrently Queued /
awaiting-CI**, not to how fast the train itself polls — which is why the `on`
leg (17 scenarios vs. 13 for `off`, since train-only scenarios skip near-
instantly under `off`) is the one that exhausts the budget.

**Mitigation shipped in #1527 (does not touch the settle scans' per-item
correctness guarantees):**

- **`E2E_PARALLEL_ON`** (default 2, half of `E2E_PARALLEL`'s default 4) caps
  concurrency specifically on the two-mode gate's `on` leg, shrinking the
  population those scans iterate. `off` and any forced single-mode run
  (`E2E_TRAIN_MODE` set explicitly) are unaffected — they keep using
  `E2E_PARALLEL`.
- **Fail-loud detection (R2), independent of whether the cap above is enough:**
  the gate runner (`scripts/e2e/run.sh`) now scans the bed's `fabrik.log` after each leg (scoped
  to just that leg's own output) for the engine's one-shot
  `"...activating rate-limit backoff"` line. On a match — regardless of the
  leg's own pass/fail exit code — the script prints a `RUN INVALID` banner and
  exits with a dedicated code (`3`, distinct from `go test`'s propagated `1`),
  short-circuiting any remaining leg. A throttled run must never be reported
  in a way that reads like a normal pass/fail; see `tests/gate/backoff.go` and
  `tests/gate/leg.go` for the full mechanism.
- **Per-leg cost visibility (A3):** each leg is now bracketed with
  inline GraphQL `rateLimit { remaining }` queries (1 point each — the REST
  `rate_limit` endpoint reports a dead, permanently full bucket for the bed's
  token), so every run reports its own actual GraphQL consumption — the manual check documented
  above is now automatic, per leg, on every invocation.

**Measured per-leg cost:** not yet captured — obtaining it requires a live
two-mode (or per-leg) gate run against the `~/dev/fabrik-test` bed, which
is a multi-hour, real-GitHub-mutating operation not run as part of landing
this change. To fill in this table, run:

```bash
E2E_TRAIN_MODE=off scripts/e2e/run.sh   # then, separately:
E2E_TRAIN_MODE=on  scripts/e2e/run.sh
```

and record each leg's `== GraphQL budget (leg: ...): N -> M remaining
(consumed D pts) ==` line below:

| Leg | GraphQL pts consumed | Backoff events | Notes |
|---|---|---|---|
| `off` | _pending measurement_ | _pending_ | Historically 0 backoff events (0.0.78 gate) |
| `on`, after `off` | _pending measurement_ | _pending_ | `E2E_PARALLEL_ON` applied |
| `on`, alone (full budget) | _pending measurement_ | _pending_ | `E2E_PARALLEL_ON` applied |

### Additional prerequisites for `TestCIFixReinvoke` and `TestCIFixReinvokeCycleLimit`

5. **`ci-fix-sentinel` enrolled as a required status check** on
   `handarbeit/fabrik-test-alpha/main`. Both tests skip gracefully (via
   `t.Skip`) if this check is not enrolled — an environment gate, citing no
   issue.
   `TestCIFixReinvoke` additionally needs the bed's **run-ID acknowledgement
   branch** of the sentinel job (next section) and **fails** — it does not
   skip — if the bed's `ci.yml` lacks it, since a skip there would silently
   reintroduce a test that can never run.
6. **`FABRIK_MAX_CI_FIX_CYCLES=2` in the test bed `.env`** (for
   `TestCIFixReinvokeCycleLimit` only). The test skips with an instructional
   message if the value is `> 3`. After editing `.env`, restart the test-bed
   Fabrik instance so the new value takes effect.
7. The default `E2E_TIMEOUT=4h` already covers `TestCIFixReinvoke` run in
   isolation (inner waits alone total ~75–90 min). Use a *smaller* override
   to fail faster while iterating on just this scenario, e.g.:
   ```bash
   E2E_TIMEOUT=1h scripts/e2e/run.sh -run TestCIFixReinvoke
   ```
8. **`FABRIK_CI_WAIT_TIMEOUT` — no override needed; the engine default (30
   min, see `ciWaitTimeout()`) applies.** #1320 had introduced a
   `FABRIK_CI_WAIT_TIMEOUT=120` requirement here, reasoning that
   `TestCIFixReinvokeCycleLimit` needed extra headroom for the cycle-limit
   path (`pauseForCIFixCycleLimit`) to win its race against the fixed
   CI-wait-timeout timer (`pauseForCITimeout`). **handarbeit/fabrik#1323
   found that premise false**: the scenario's fixture never guaranteed a new
   commit on each CI-fix reinvoke, so the `#958 leg 2` no-op guard
   (`engine/catch_up_handlers.go`) latched permanently after the very first
   reinvoke — `CIFixCycleIncremented` never advanced past 1, and
   `pauseForCIFixCycleLimit` was structurally unreachable *at any*
   `FABRIK_CI_WAIT_TIMEOUT`. Raising the timeout to 120 min therefore didn't
   fix a race; it just quadrupled time-to-escalation for every `wait_for_ci`
   item in the bed, with no compensating benefit.

   The fixture (`ciFixCycleLimitBody`) now forces an unconditional
   scratch-file commit+push on every single CI-fix reinvoke, independent of
   the agent's belief about fixability — the no-op guard only checks head-SHA
   equality, so this is sufficient to make `CIFixCycleIncremented` genuinely
   advance to `MaxCiFixCycles` through real repeated dispatch. With the path
   actually reachable, whether the CI-wait-timeout timer also fires is
   ordinary e2e timing risk like any other scenario in the bed — not a
   known-guaranteed structural loss requiring bespoke pre-flight timer math.
   The test now also asserts the PR gained at least `maxCycles` commits
   beyond baseline (see `TestCIFixReinvokeCycleLimit`'s doc comment), making
   "the cycle-limit path was genuinely exercised" independently verifiable
   rather than inferred from timer coordination.

   The 87-minute data point that backed the old `120` recommendation (a
   captured release-gate run "still executing" with `FABRIK_MAX_CI_FIX_CYCLES=2`)
   is now understood to have likely been an artifact of the same no-op-latch
   bug — a cycle count stuck at 1 while wall-clock kept advancing is
   indistinguishable from this exact defect. It should not be treated as a
   trustworthy per-cycle cost measurement; a future live run against the
   fixed fixture is what should supply real numbers if the wait-timeout timer
   does turn out to race in practice.

### Bed workflow for `TestCIFixReinvoke` (#1991)

`TestCIFixReinvoke` was skipped for ~2.5 months because a capable Implement
agent reads `ci.yml`, satisfies the sentinel in its first commit, and the first
CI run is green — so the reinvoke never fires (#916). The test now forces the
first failure with a **run-ID acknowledgement nonce** (ADR 1991):

- The nonce `T` is the ID of the **earliest `ci.yml` `pull_request` run on the
  PR's head branch** (`fabrik/issue-N`). That run is red by construction — its
  own ID cannot be inside a file committed before it existed — and `T` is a
  future GitHub-assigned number, not derivable from the worktree, git history
  or `ci.yml`.
- The sentinel passes only if the repo root holds `CI_FIX_ACK` containing the
  line `ack:<T>` **and** the current run is not run `T`.
- On failure the job emits `::error title=ci-fix-sentinel::…ack:<T>`. The engine
  embeds that annotation (`github.FetchCheckRunAnnotations`) in the CI-fix prompt
  (`engine/ci.go` `ciFailureDetail`) and logs the prompt, so `T` reaches the
  agent only through the reinvoke.

The two sibling branches (`ci-fix-sentinel-required`, `ci-fix-sentinel-unfixable`
— used by `TestCIFixReinvokeCycleLimit` and the conjunctive-gate test) are left
untouched. **This workflow change lives in the external
`handarbeit/fabrik-test-alpha` repo and must be applied by an operator with bed
access before the live run** (the engine/harness change is independent of it).
Add this branch to the sentinel job's script, alongside the existing ones, and
give the job `permissions: { contents: read, actions: read }` and
`GH_TOKEN: ${{ github.token }}` in its env, with `PR_BODY` and `BRANCH` taken
from `github.event.pull_request.body` and `github.head_ref`:

```bash
if printf '%s' "$PR_BODY" | grep -q 'ci-fix-sentinel-ack'; then
  WF=$(gh api "repos/$GITHUB_REPOSITORY/actions/runs/$GITHUB_RUN_ID" --jq .workflow_id)
  T=$(gh api "repos/$GITHUB_REPOSITORY/actions/workflows/$WF/runs?branch=$BRANCH&event=pull_request&per_page=100" \
        --jq '[.workflow_runs[].id] | min')
  if [ "$GITHUB_RUN_ID" != "$T" ] && grep -qx "ack:$T" CI_FIX_ACK 2>/dev/null; then
    echo "ci-fix-sentinel: acknowledgement ack:$T found"
    exit 0
  fi
  echo "::error title=ci-fix-sentinel::CI-fix required: create CI_FIX_ACK at the repo root containing the line ack:$T"
  exit 1
fi
```

The harness derives `T` independently through the same API (`ci.yml` runs on
the branch, `event=pull_request`, minimum ID), so the bed's `ci.yml` file name
must stay `ci.yml` (`bedCIWorkflowFile` in `ci_fix_ack.go`).

What the test asserts (all `Fatalf`, never `Skip`): the first CI run is red and
its annotation carries `ack:<T>` (fail-fast; a green first run is named as the
agent pre-empting the sentinel); the engine logs
`[#N ci-fix-reinvoke] re-invoking stage`; the logged `prompt (` line carries the
failing check's name and `ack:<T>`; CI converges; the PR head tree contains
`CI_FIX_ACK` with `ack:<T>`; exactly one commit lands after `fabrik:awaiting-ci`.

### Marker-substring assertion audit (#1320)

`TestCIFixReinvokeCycleLimit` used to assert on a `🏭 **Fabrik —` marker via
`strings.Contains` across every issue comment — including stage-output
comments authored by the Claude agent. On `handarbeit/fabrik-test-alpha#4049`
this produced a **false pass**: the only comment containing the cycle-limit
marker text was the Research stage's own output, which quoted the marker in
prose while researching the feature. The engine never posted a cycle-limit
comment on that issue at all — it paused via the CI-wait-timeout path
instead. This is the same class of defect as #1319
(`TestExpectedReviewersPrecedenceGuard` passing regardless of which
resolution actually occurred) and #1263 (a Plan stage quoting
`FABRIK_SPAWN_CHILD_BEGIN` in prose defeated a different assertion).

A repo-wide search (`grep -rn "🏭" tests/e2e/*.go`) found every call site in
`tests/e2e/` that matches a `🏭 **Fabrik —` marker. Audit result:

1. **`ci_fix_reinvoke_test.go` (`TestCIFixReinvokeCycleLimit`) — fixed here.**
   Replaced the `strings.Contains` scan with `hasEngineCycleLimitComment`,
   which matches via `strings.HasPrefix` — the same body-prefix convention
   `findNewComments` (`engine/comments.go`) uses to distinguish Fabrik's own
   output from prose. See `TestHasEngineCycleLimitComment` in
   `ci_fix_reinvoke_marker_test.go` for a fast, non-e2e proof that this
   rejects a comment shaped like the `#4049` false-pass evidence while still
   accepting a genuine engine comment.
2. **`ci_fix_reinvoke_test.go` (`TestCIFixReinvoke`) — fixed in #1991.**
   It called `WaitForPRCommentContaining(…, "🏭 **Fabrik — stage: Validate**", …)`,
   a `strings.Contains` scan across all PR comments (the same unscoped-match
   shape as the original defect), left unfixed while the test was skipped. The
   assertion is gone: dispatch is now proven by the engine's own
   `[#N ci-fix-reinvoke] re-invoking stage` log line, and prompt content by its
   `prompt (` line (`WaitForLogLine`, scoped to this issue number and to log
   lines written after the issue was filed) — see "Bed workflow for
   `TestCIFixReinvoke`" above.
3. **`expected_reviewers_test.go:175` and `review_authority_test.go:174`
   (`WaitForPRCommentContainingAny`) — already sound, not `🏭` markers.**
   Both call sites match on non-marker substrings specific to their scenario
   (`"@" + syntheticName`, `"reviewDecision=CHANGES_REQUESTED"`,
   `"requested changes"`), not a `🏭 **Fabrik —` prefix, so an agent quoting a
   *Fabrik* marker in prose can't accidentally satisfy them. They share the
   underlying `Contains`-based helper family
   (`WaitForPRCommentContaining`/`WaitForPRCommentContainingAny` in
   `harness.go`) with the flawed assertions above, so a future scenario that
   reuses these helpers to match a `🏭 **Fabrik —` marker would reintroduce
   the same risk — a broader audit of "any engine-message substring
   assertion" (not just `🏭` markers) is a known boundary of this audit, not
   a finding against these two call sites as they stand today.

No other `tests/e2e/*.go` file references the `🏭` emoji or a
`"🏭 **Fabrik —"` literal.

### Marker-path convention (#1394)

Multiple scenarios run in parallel against the same repo. Early on, several of
them instructed the agent to append its marker to the *same* file —
`README.md` on `fabrik-test-alpha`. When two such PRs landed close together,
the second could pick up a merge conflict on the shared trailing lines and
fail in its own harness code (e.g. `MergePR`), before any engine assertion
ever ran — a flake, not a regression.

Every scenario that writes a marker file (other than the deliberate exception
below) must instead target its own dedicated, fixed path under
`e2e/markers/`, so parallel scenarios can never collide on the same file:

- **`markerPaths`** (`harness.go`) is the single source of truth: a map from
  test-function name to its unique `e2e/markers/<scenario>.md` path.
  `markerPath(name string) string` is the accessor — it panics on an unknown
  name so a typo'd lookup fails loudly at test setup rather than silently
  producing an empty path in an issue body.
- **`TestMarkerPathsAreUnique`** (`marker_paths_test.go`) statically enumerates
  `markerPaths` and fails on any duplicate or empty value. It requires no live
  bed:

  ```
  go test -tags e2e -run TestMarkerPathsAreUnique -v ./tests/e2e/
  ```

- Paths are **fixed per scenario**, not derived from a per-run timestamp. This
  differs from the merge-train members' `uniqueMemberPath` (`e2e/train/<name>-<issue>.txt`),
  which needs a fresh path every run because a landed batch merges member
  files into `main` and a fixed path would collide with the existing blob's
  SHA on the next run's Contents-API PUT. These marker files are instead
  edited through a normal agent git commit in a worktree — a real diff, not a
  raw Contents-API PUT — so a fixed path across runs is fine, and it's what
  lets `TestMarkerPathsAreUnique` enumerate the set statically instead of
  needing to execute the tests to discover the paths.
- `TestPausedMergedPRRecovery`'s 3 sequential sub-variants deliberately share
  one path: they run strictly sequentially (`t.Run`, no `t.Parallel` between
  them) and each variant's PR merges before the next is filed, so there is no
  concurrent write to race.

**Deliberate exception — `TestConvergenceRace`:** this scenario is absent from
`markerPaths` on purpose. Its entire premise (see its own top-of-file comment)
is that two yolo issues insert at the *same* anchor line of the *same*
`README.md`, so that whichever PR merges first forces the other into a
genuine `mergeable=CONFLICTING` state that Fabrik must resolve via a single
rebase reinvoke. Giving it a unique path would eliminate the exact condition
it exists to provoke. Any new scenario whose behavior genuinely depends on a
shared file should be documented as such here, rather than left to race.

When adding a new marker-writing scenario, add an entry to `markerPaths`
before wiring up the issue body template — see "Adding a scenario" below.

### `TestNoWorkNeeded` discriminator verification (#1355)

`TestNoWorkNeeded` used to assert only `WaitForIssueClosed` (plus a
best-effort "no open PR" spot-check) — insufficient by itself, since closing
proves the pipeline finished, not that it took the no-work-needed path
specifically (the same vacuity class as #1320/#1319). It now additionally
asserts the engine's own no-work-needed skip comment
(`noWorkNeededSkipComment`, `engine/no_work_needed_settle.go`) is present,
matched by body prefix via `hasNoWorkNeededSkipComment`
(`no_work_needed_marker_test.go`) — never by substring-anywhere, for the same
reason `hasEngineCycleLimitComment` matches by prefix.

**Verified so far:** `TestHasNoWorkNeededSkipComment` is a fast, no-live-bed
unit test that proves the helper accepts both genuine comment variants
`noWorkNeededSkipComment` can produce and rejects a fixture where an agent
quotes the marker text in prose (the #4049/#1320 false-pass shape) — run it
with `go test -tags e2e -run TestHasNoWorkNeededSkipComment ./tests/e2e/`.
This establishes the matching logic itself discriminates correctly.

**Not yet verified: a genuine red/green cycle against the live e2e bed**
(the issue's own Verification Note calls this the load-bearing step). Doing
so requires temporarily neutralizing `noWorkNeededSkipComment`'s posting in
a locally-built engine binary, deploying that binary to the shared
`~/dev/fabrik-test` bed, and restarting the bed's Fabrik instance — all
outside the `tests/e2e/` worktree, and outside the sandbox boundary an
automated Fabrik Implement/Review/Validate stage operates under (see
CLAUDE.md's "Worktree Boundary"). No pipeline stage in this repo can safely
perform that step; it requires a human operator with direct access to the
bed. Steps to complete AC3 before merge:

```bash
cd ~/dev/fabrik-test
# Stop the running instance (Ctrl-C in its session, or: kill $(cat .fabrik/fabrik.lock))

# RED: neutralize the skip-comment posting, uncommitted
#   in engine/no_work_needed_settle.go, make noWorkNeededSkipComment return ""
go build -o fabrik .
./fabrik -notui &   # do NOT pass --auto-upgrade, it would overwrite this build
cd -   # back to this branch's checkout
go test -tags e2e -run TestNoWorkNeeded ./tests/e2e/ -timeout 25m -v
# Expect FAIL: "no engine-authored no-work-needed skip comment ... found"

cd ~/dev/fabrik-test
git diff --stat engine/   # confirm the neutralization is still the only engine change
git checkout -- engine/no_work_needed_settle.go   # revert
go build -o fabrik .
# restart the instance (Ctrl-C the red one first): ./fabrik -notui --auto-upgrade &
cd -
go test -tags e2e -run TestNoWorkNeeded ./tests/e2e/ -timeout 25m -v
# Expect PASS
```

Record both outcomes (pass/fail, timestamps, relevant log lines) in the PR
or as a follow-up comment on handarbeit/fabrik#1355 once run.

### Additional prerequisites for `TestPausedMergedPRRecovery`

9. **Gate labels seeded** in `handarbeit/fabrik-test-alpha`: `fabrik:awaiting-ci`,
   `fabrik:awaiting-review`, `fabrik:paused`, and `fabrik:awaiting-input` are
   production labels that must exist. `AddLabel` fatals immediately if a label is
   absent — create them manually in the repo if needed.
10. The default `E2E_TIMEOUT=4h` already covers `TestPausedMergedPRRecovery`
   run in isolation (three sequential cruise pipelines, Specify → Implement
   each, total ~60–90 min). Use a *smaller* override to fail faster while
   iterating on just this scenario, e.g.:
   ```bash
   E2E_TIMEOUT=1h30m scripts/e2e/run.sh -run TestPausedMergedPRRecovery
   ```

### Additional prerequisites for `TestConjunctiveCIReviewGate`

11. **`slow-gate` enrolled as a required status check** on
    `handarbeit/fabrik-test-alpha/main`. The test skips gracefully (via
    `t.Skip`) if not enrolled — safe to merge before enrollment. Its
    `ci.yml` must also honour the `slow-ci-required-long` PR-body marker
    (`SLOW_CI_LONG_SECONDS: "1500"`, checked before the plain
    `slow-ci-required` one, which is a prefix of it). The window runs from
    the PR push, and Review's latency eats into it before `fabrik:awaiting-ci`
    appears, so the plain 10-minute marker is too short for this test. The
    test reads `slow-gate`'s real state and fails as a fixture problem, not an
    engine regression, when too little window is left.
12. **The engine process must actually authenticate as `FABRIK_TOKEN`'s
    identity** — no shell export shadowing it. `config.Token()`'s precedence
    is `FABRIK_TOKEN > GITHUB_TOKEN`, and `godotenv.Load(".env")` does not
    override a variable already set in the process environment, so this only
    breaks if `FABRIK_TOKEN` itself (not merely `GITHUB_TOKEN`) is exported in
    the shell that launches the test-bed Fabrik instance, shadowing the value
    in `.env` with a different identity (see handarbeit/fabrik#925 Confound 1
    for the incident this generalizes from). If this drifts, PRs come out
    authored by the wrong identity and `RequestPRReviewer` silently no-ops
    (GitHub forbids requesting a review from the PR author). The test's
    `AssertPRAuthorIsExpectedIdentity` preflight check catches this in
    seconds rather than after a full 60–100 min run — if it fails, check the
    launching shell's environment for a stray `FABRIK_TOKEN` export.
13. **One of the following for R5 (joint-clear verification)**:
    - **`FABRIK_REVIEWER_TOKEN` in the test bed `.env`** — a GitHub PAT for a
      non-`@arbeithand` account with write access to `fabrik-test-alpha`. The
      test uses this token to submit an approving PR review from a second
      identity (GitHub forbids self-approval). This exercises the full
      approval-path joint-clear (R5). Combined with a generous
      `FABRIK_REVIEW_WAIT_TIMEOUT` (see next bullet), the test defers
      requesting this reviewer until `fabrik:awaiting-ci` is observed (R1) —
      not until `stage:Review:complete`, which is applied immediately when
      Review's Claude invocation finishes, simultaneously with
      `fabrik:awaiting-review` and before Review's own gate has actually
      cleared. `fabrik:awaiting-ci` only appears once Validate has been
      dispatched, which only happens after Review's own `wait_for_reviews`
      gate has genuinely cleared first via the incidental
      `gemini-code-assist` review — so only Validate's gate blocks on the
      real reviewer.
    - **`FABRIK_REVIEW_WAIT_TIMEOUT` left at a generous value (e.g. the
      15-minute default)** when running the approval path — a short timeout
      (e.g. `2`) risks Review's own gate timing out before
      `gemini-code-assist` submits its incidental review (documented
      30s–10m behind PR-ready), which would break the Review-then-Validate
      sequencing the approval path depends on. The test skips with an
      instructional message if `FABRIK_REVIEWER_TOKEN` is set and this value
      is `< 10`. Use `FABRIK_REVIEW_WAIT_TIMEOUT=2` only for the
      timeout-fallback path below (no `FABRIK_REVIEWER_TOKEN`), so the
      review-timeout fallback path (R5 reduced scope) completes in a
      reasonable wall-clock budget. If this value exceeds 5 and no reviewer
      token is present, the test skips with an instructional message. After
      editing `.env`, restart Fabrik. Note: the timeout-fallback path has no
      second identity to request, so it remains exposed to the
      `gemini-code-assist` bot clearing the gate before the timeout fires
      (residual flakiness, not fixed by this redesign — see #925).
14. The default `E2E_TIMEOUT=4h` already covers this test run in isolation
    (worst case ~60–90 min for the approval path). Use a *smaller* override
    to fail faster while iterating on just this scenario, e.g.:
    ```bash
    E2E_TIMEOUT=1h30m scripts/e2e/run.sh -run TestConjunctiveCIReviewGate
    ```

### Additional prerequisites for the merge-train scenarios (ADR-059)

`TestMergeTrainHappyPathLanding`, `TestMergeTrainBisectionEjectsPoisoner`,
`TestMergeTrainRestartSafety`, `TestMergeTrainRunawayGuardPausesBatch`,
`TestMergeTrainRedSingletonReroutesOffQueued`,
`TestMergeTrainSingletonFastPathLandsExactlyOnce`, and
`TestMergeTrainConflictBisectPrefixRerere` need one-time bed setup. They
**skip cleanly** (`requireTrainBed`) if the `Queued` column is absent, so they
are safe to merge before the bed is set up. They
also skip cleanly under train mode `"off"` — these scenarios place issues
directly in `Queued` via the GitHub API, which succeeds regardless of mode,
but nothing drains `Queued` when `merge_train: off` (no per-item dispatch,
no batch handler), so without this check they'd hang to their full 10–50 min
timeout instead of skipping. Only run in the `on` leg of the two-mode gate.

15. **`Queued` board column** on `handarbeit/projects/2`, positioned between
    `Validate` and `Done` (ADR-059 D1 — the durable train queue). Add it in the
    Project's Status field options.
16. **`queued.yaml` holding stage** in the bed's `.fabrik/stages/`, e.g.:
    ```yaml
    name: Queued
    order: 8            # after Validate, before Done
    holding_stage: true # engine-managed; no Claude invocation
    ```
    Copy from `stages/examples/queued.yaml` (`fabrik init` / `fabrik refresh-stages`).
17. **Train-capable binary** in the bed, built from `main` (the release does not
    yet carry ADR-059). Run it **without `--auto-upgrade`** so it is not reverted
    to a release mid-suite:
    ```bash
    (cd ~/dev/fabrik && go build -o ~/dev/fabrik-test/fabrik .)
    # on macOS/Apple Silicon a copied binary may be SIGKILL'd; build in place or:
    #   xattr -cr ~/dev/fabrik-test/fabrik && codesign --force --sign - ~/dev/fabrik-test/fabrik
    ```
18. **`train-poison-guard` required check** on `fabrik-test-alpha` — for both
    `TestMergeTrainBisectionEjectsPoisoner` and
    `TestMergeTrainRedSingletonReroutesOffQueued` (#1545). Commit
    `tests/e2e/testdata/train-poison-guard.yml` to the repo as
    `.github/workflows/train-poison-guard.yml` and mark the `train-poison-guard`
    check REQUIRED on branch protection, so the combined-Validate poll gates on it.
    The bisection test skips this check indirectly — if the guard is absent the
    combined batch is green and no bisection occurs, failing the `bisecting`
    log-line wait; run it only after the guard is enrolled.
    **Keep the bed's copy in sync with `tests/e2e/testdata/`.** The guard fails a
    merge-train trial/integration branch immediately, but a poison member's own PR
    only after `POISON_MEMBER_DELAY` (600s). That delay lets #1821's admission gate
    admit the member, which it would defer if its own CI were already red, while
    making sure its own CI is never green, so the singleton fast path can never land
    POISON on `main`. An older copy that fails member PRs immediately breaks every
    poison scenario (bisection, conflict, red-singleton, runaway) at admission.
    `TestMergeTrainRedSingletonReroutesOffQueued` queues exactly one poison
    member, so its own combined Validate goes red with nobody to bisect against —
    the top-level `len(survivors) == 1` arity guard short-circuits straight to
    `ejectRedSingleton` instead. It deliberately does **not** call `t.Parallel()`:
    the train forms from every item currently in `Queued` on a repo, so running
    concurrently with `TestMergeTrainHappyPathLanding`/
    `TestMergeTrainBisectionEjectsPoisoner` (both parallel, same repo) risks this
    test's lone member being batched together with a sibling test's members,
    which would route through bisection instead of the arity guard this test
    exists to exercise. Dropping `t.Parallel()` guarantees it completes before any
    parallel Alpha merge-train scenario begins queueing, mirroring
    `TestMergeTrainRunawayGuardPausesBatch`'s identical non-parallel rationale
    below. Wall-clock: ~10–20 min (one combined validation, no bisection, no
    landing CI).
19. The default `E2E_TIMEOUT=4h` already covers these run in isolation
    (happy/bisect: 20–40 min; restart — two sequential landings: 25–50 min).
    Use a *smaller* override to fail faster while iterating, e.g.
    `E2E_TIMEOUT=1h`.
20. **`train-poison-guard` required check on `fabrik-test-beta`** — only for
    `TestMergeTrainRunawayGuardPausesBatch`. Commit
    `tests/e2e/testdata/train-poison-guard.yml` to `handarbeit/fabrik-test-beta`
    as `.github/workflows/train-poison-guard.yml` and mark the
    `train-poison-guard` check REQUIRED on branch protection (same steps as for
    Alpha in prerequisite #18, targeting Beta instead). The runaway test skips
    cleanly until this is enrolled.
    **`FABRIK_MAX_TRAIN_TRIALS_PER_WINDOW=6`** must also be set in the bed's
    `.env` before launching the Fabrik instance for this test. At the default
    (20), the guard would require ~20 red trials — the 4-member all-poison batch
    generates only ~7–10, so the test would time out. A cap of 6 sits above
    Alpha's bisect-scenario max (~4 trials) with comfortable margin. Wall-clock:
    ~10–20 min; ~6 trials × 2 required checks ≈ 12 Actions runs.
    `TestMergeTrainRunawayGuardPausesBatch` intentionally does not call
    `t.Parallel()` — it deliberately induces a repo-wide fault on
    `fabrik-test-beta`, and `TestCrossRepoSpawn` is the only other scenario
    that drives real work on that same repo. Running them concurrently let the
    induced fault pause `TestCrossRepoSpawn`'s in-flight child issue as
    collateral damage in the 0.0.77 validation gate (issue #1395); dropping
    `t.Parallel()` here guarantees the runaway scenario completes before
    `TestCrossRepoSpawn` starts real work, mirroring the same idiom
    `TestMergeTrainRestartSafety` already uses below.
21. **`TestMergeTrainQueuedDeeperThanBatchCap`** (#1850, ADR-1833) — queues
    **seven** clean members against the bed's `max_batch_size` and asserts
    (A1) the first trial holds exactly the first five, (A2) batch membership
    stays stable from the first `batch snapshot` line to the batch-1 landing
    (exactly one snapshot line, no trial PR closed unmerged), and (A3) all seven
    land in two batches (5, then 2). It is the only scenario that queues more
    members than the cap, so it is the only release-gate coverage of ADR-1833's
    deterministic Queued ordering (the sim bed cannot host that defect — it uses
    the pass-through adapter, never the map-backed `itemstate.Store`).
    - **Bed prerequisites:** the default `max_batch_size` of **5** (no
      `FABRIK_MAX_BATCH_SIZE` in the bed `.env`, no `max_batch_size` in
      `.fabrik/config.yaml`), and **no other open, non-paused item in `Queued`**
      on Alpha. A pre-flight skips on a configured non-5 value and fails loudly
      on stale Queued items; the engine's own `batch capped … max_batch_size=N`
      line is the authoritative check and fails the scenario if `N != 5`.
    - **No cache-mode prerequisite.** `board_cache_mode` was never a real config
      key (#1544) and a live bed always runs the in-memory `CacheImpl`, which is
      the mode the defect needs. ADR-1833's wording is historical.
    - **Not parallel**, on `RepoAlpha`/`main` (the default partition production
      uses). Any other Queued member on the same (repo, base) would join the
      partition and change the batch composition; Go runs non-parallel tests to
      completion before resuming parallel ones, so no `run.sh` change is needed.
    - **Batch gating.** The engine has no batching dwell, so members are filed
      carrying `fabrik:paused`, all seven are placed in `Queued`, then the label
      is removed from all seven concurrently (REST). Paused Queued members are
      excluded from the partition and cause no engine side effect. The unpause
      window is about one API round-trip, not zero: if a poll lands inside it the
      scenario fails with a **"poll boundary straddled the unpause … re-run"**
      message (first cap line not `7 Queued`, or first snapshot under five
      members). That is a harness race, not an engine regression — re-run. A
      failed run re-pauses any still-open member so it cannot leak into a later
      scenario's batch. The `fabrik:paused` label itself is never deleted.
    - **Placement order.** Members are filed and placed sequentially, so
      placement order equals issue-number order under either observation timing
      of the engine's `StatusEnteredAt`. "First five by entry order" therefore
      means the five lowest issue numbers; the scenario cannot distinguish
      `(StatusEnteredAt, Number)` from `Number` alone (that would need permuted
      placement with a poll between each, 5+ min extra), but it does catch the
      pre-#1833 map-order selection.
    - **Log anchors** (copied from the engine; pinned by
      `mergetrain_batchcap_parse_test.go`): `batch capped for <key>: 7 Queued
      item(s) exceed max_batch_size=5 — landing first 5 by entry order`,
      `batch snapshot for <key>: N item(s) — …`, `opened draft CI PR #N for
      owner/repo (K survivor(s))`, `merged integration PR #N for owner/repo`,
      `landing complete for <key> (integration PR #N, K members)`. The A2 window
      ends at `merged integration PR`, not `landing complete`: once members
      advance to Done the Queued set legitimately shrinks and a new snapshot is
      correct. Log reads are scoped by a `LogOffset` taken just before the
      unpause, the repo's trainKey, and this scenario's own members.
    - **Cost / wall-clock:** two green trial CI cycles (a 5-member trial, then a
      2-member trial) and **no** Claude conflict invocations (every member writes
      a distinct path) — low cost, ~30–60 min. Covered by the default
      `E2E_TIMEOUT=4h`.

22. **`TestMergeTrainConflictBisectPrefixRerere`** (#1848) — the live counterpart of
    the sim bed's scripted-Claude conflict/bisect/prefix/rerere coverage, and the
    only release-gate scenario that creates a textual conflict, so the only one that
    runs **real Claude** conflict resolution under the real merge-train prompt
    (#1841) alongside trial-prefix reuse (#1835) and rerere replay (#1834). It needs
    no bed setup beyond #15–#18 (Queued column, `queued.yaml`, train-capable binary,
    `train-poison-guard` required on Alpha) plus:
    - **Real Claude usable from the `Queued` holding stage.** The default tool set
      (`Bash(git:*)`, Edit, Write) is enough; nothing else is configured.
    - **Non-parallel** (no `t.Parallel()`), for the same reason as the red-singleton
      scenario: the train batches every item in `Queued` on a repo, so a concurrent
      sibling would join and break the batch shape. It runs to completion before any
      parallel Alpha train scenario resumes.
    - **Shape.** Four members queued A, B, C, P: A and B write the same path
      (`e2e/train/conflict/shared-<stamp>.txt`, outside `e2e/train/entries/`, so
      `train-poison-guard` cannot trip on it) with divergent content; C is clean; P
      is the poisoner. The trial `[A,B,C,P]` has Claude resolve B onto A, goes red,
      and bisects: `[A,B]` reuses the recorded prefix (2/2, no Claude), `[C,P]`
      red, `[C]` green, `[P]` red → P ejected; `[A,B,C]` re-forms on the prefix
      (3/3) and lands. P ends off `Queued` (re-batched as a singleton, then
      rerouted). All four members are prepared first and moved to `Queued` back to
      back; the scenario then fails fast unless the first `batch snapshot` line
      lists exactly A, B, C, P in that order.
    - **What it asserts** (each from GitHub state or a log line copied from `main`;
      the analysis is pure and unit-tested in `mergetrain_conflict_log_test.go`,
      run with `go test -tags e2e ./tests/e2e/ -run 'ConflictTrain|BatchSnapshot|SnapshotForRepo'`):
      (A1) `conflict for #B resolved`, no `cannot resolve conflict for #B`, no
      ejection comment on B, no conflict markers on `main`; (A2) B's single Claude
      invocation uses at most `maxConflictResolutionTurns` (20) turns — parsed from
      either `used N turns` (error/turn-limit exit) or `completed in N turns`
      (clean exit); (A3) `bisection isolated #P as the batch poisoner` and an ejection
      comment on P; (A4) after `bisecting to isolate the poisoner`, `reusing a
      recorded prefix (2/2 member(s) already merged)` appears and no Claude is
      invoked for any member before the `(3/3 …)` re-form; (A5) B has exactly one
      Claude invocation for the whole run and no `did not fully replay via rerere`
      warning; (A6) A, B, C land and P ends off `Queued`, not `Done`.
    - **The rerere replay line is asserted only conditionally.** A logged
      B-onto-A re-merge (`conflict for #B fully resolved by git rerere replay — no
      Claude invocation`) is *not* guaranteed by this shape: prefix reuse covers
      every re-form, and the one unconditional re-merge (inside
      `forgetPoisonerResolutions`) logs nothing on success. The line is required
      only if main moved mid-run and the train rebuilt on the new base
      (`(main moved) — rebasing off the new base`), which changes the base SHA and
      misses the prefix cache.
    - **Admission-gate caveat (shared with the bisect scenario).** If P's own PR CI
      finishes red before the batch forms, the admission gate (#1821, active when
      the stage before `Queued` has `wait_for_ci: true`) defers P and no bisection
      happens. The scenario fails fast naming `deferring #<P> (own PR CI confirmed
      red` as the cause; the gate must be pending or skipped, so re-run.
    - **Turn bound is unmeasured.** 20 is under half the holding stage's 50-turn
      comment cap (the cap the pre-#1841 prompt burned through) but has no live
      baseline; the failure message prints the actual N. Tune
      `maxConflictResolutionTurns` after the first release-gate run.
    - **Cost (unmeasured estimates).** Claude: **one** conflict-resolution invocation
      (roughly $0.05–0.30) plus the bed reviewer's reviews of four member PRs. CI: 4
      member-PR runs, ~6 trial cycles (initial, `[A,B]`, `[C,P]`, `[C]`, `[P]`,
      `[A,B,C]`), and ~1 follow-up cycle for P's singleton disposition. Wall-clock
      ~45–80 min, inside the default `E2E_TIMEOUT=4h`. The batch adds 3 non-green
      trials (initial, `[C,P]`, `[P]`) to the runaway guard's counter, well under
      the default cap of 20.
22a. **`TestMergeTrainSingletonFastPathLandsExactlyOnce`** (#1874, ADR-1871) — the
    only scenario that lands one member alone through the **singleton fast path**
    (#1644) deterministically, and the fast-path half of the exactly-once landing
    check (see "Exactly-once landing assertion" below). It needs no bed setup beyond
    #15–#18 (Queued column, `queued.yaml`, train-capable binary, `train-poison-guard`
    required on Alpha, which supplies the member's own check run — the scenario skips
    cleanly where it is not enrolled).
    - **Non-parallel** (no `t.Parallel()`): `main` must not move (the fast path needs
      the pinned base to be an ancestor of the member head) and no sibling may join the
      batch. It runs to completion before any parallel Alpha train scenario resumes.
    - The member is *prepared*, not queued, until its own CI is complete and green and
      its `mergeable_state` is `clean`/`unstable`; it then fails loudly (never skips)
      if the log shows `singleton fast path not taken for #N`, so a bed that cannot
      support the fast path (e.g. branch protection needing an approval) is a visible
      bed problem, not silently dropped coverage.


### Additional prerequisites for `TestReviewAuthority*` scenarios

`TestReviewAuthorityReinvokesOnChangesRequested`, `TestReviewAuthorityCycleLimitPauses`,
`TestReviewAuthorityClearsOnApproval`, and `TestReviewAuthorityYoloDoesNotBypassBlock`
cover ADR-1250's `review_authority: authoritative` mode (as amended by ADR-1375's
reinvoke-governs-working / authoritative-governs-merging model). All five
`TestReviewAuthority*` scenarios, including `TestReviewAuthorityAdvisoryRegressionGuard`,
run against the bed's existing `Review` column/stage (default, untouched config) — **no
bed column or stage-YAML setup is required**, beyond `FABRIK_REVIEWER_TOKEN` and the
`review-authority:authoritative` label (both below).

**Mechanism: a per-issue label, not a bed column.** Authoritative mode is applied per item
via the `review-authority:authoritative` label, passed as an extra label at seed time
(`seedReviewGateItem`'s `extraLabels`). Engine support for that label is tracked separately
in #1261 — the three authoritative scenarios above cannot pass until both #1261 and this
issue's PR are merged; `TestReviewAuthorityAdvisoryRegressionGuard` has no such dependency
and ships green regardless.

An earlier design applied authority via a bed-local `Review-Authoritative` board column +
matching stage YAML (mirroring the `Queued`/`queued.yaml` precedent). That was rejected:
`review_authority` is a property of a stage's config, not a distinct kind of stage, so it
doesn't belong on the board as a column name — and requiring a bed prerequisite the operator
hadn't set up yet meant three of the four scenarios silently skipped, letting the suite go
green having validated zero authoritative behavior. Tests gating a release should fail loudly
when they can't run their intended assertion, not pass vacuously. See
`adrs/1258-e2e-review-authority-coverage.md` for the full rationale.

**Why these scenarios can't cover the landing/auto-merge gate:** `reviewGateBlocksLanding` is
only reachable through a stage literally named `Validate` — `engine/stages.go`,
`engine/poll.go`, and `engine/pr_terminal_advance.go` all hard-gate on `stage.Name ==
"Validate"`. Applying the authority label to an item on `Review` cannot reach that
stage-name-gated path, and authoritative-izing the bed's real `Validate` stage would violate
"no change to the bed's default stage config" and risk corrupting concurrently-running
advisory scenarios on the shared bed. This is a documented, accepted e2e gap — the three
scenarios below therefore assert the gate *clears* (`fabrik:awaiting-review` disappears,
`fabrik:paused` never applied), not that the item merges.

21. **`FABRIK_REVIEWER_TOKEN` in the test bed `.env`** — same non-author PAT documented
    in prerequisite #13 above. All five `TestReviewAuthority*` scenarios skip with an
    instructional message if it is unset; there is no timeout-fallback path here (unlike
    `TestConjunctiveCIReviewGate`) because these scenarios exist specifically to assert
    on a deterministic verdict, not on gate-timeout behavior alone.
22. **`review-authority:authoritative` label seeded** in `handarbeit/fabrik-test-alpha`
    (the only repo these scenarios use). `FileIssue` passes it straight through to
    `gh issue create --label`, which — like `AddLabel` (prerequisite #9) — fatals
    immediately if the label doesn't already exist as a label object in the repo; `gh`
    does not auto-create labels on issue creation. Create it manually
    (`gh label create review-authority:authoritative -R handarbeit/fabrik-test-alpha`)
    if needed. This is independent of #1261: #1261 adds the engine code that
    *interprets* the label on an issue it already carries, not the GitHub label object
    itself — the object must exist before any of these scenarios can even file their
    seed issue.
23. **Why the bed's real reviewer (Pruefer, as of #1396 — see "Reviewer topology"
    below) is not used for verdict assertions here**: this is narrower than it
    once was. Until 2026-08-13 the bed's incidental reviewer was
    `claude-review.yml`, which submits `gh pr review --comment` in both its agent
    path and its fallback path — it could never produce `APPROVE` or
    `CHANGES_REQUESTED`, so it could not exercise authoritative mode's blocking or
    clearing paths at all. #1396 intended to retire it in favour of Pruefer, but
    the handover was only completed on 2026-08-13 (see "Reviewer topology"); the
    workflow ran until then. Pruefer *can* submit
    APPROVE/REQUEST_CHANGES verdicts — but using it as the deterministic verdict
    source for `TestReviewAuthority*`'s assertions was explicitly rejected for issue
    #1258, and #1396 does not reopen that rejection: non-determinism (verdict
    depends on Claude's severity classification of a synthetic diff), latency
    (Pruefer polls, default 120s, vs. an Action firing on PR-open), cost (a real
    Claude invocation per test PR), and coupling (Fabrik's release gate depending
    on Pruefer's health) all still apply. All verdict assertions in
    `TestReviewAuthority*` instead use `SubmitPRReview` + `FABRIK_REVIEWER_TOKEN` —
    deterministic, harness-posted formal reviews from a non-author identity.
24. **`TestReviewAuthorityReinvokesOnChangesRequested`'s wall-clock is dominated by one
    real Claude invocation** (the review-reinvoke itself addressing the harness's
    synthetic feedback), typically several minutes, plus a bounded 90s settle window for
    its AC7 "not re-processed" check — well within the default `E2E_TIMEOUT=4h`. **Do not
    use a very short value like `FABRIK_REVIEW_WAIT_TIMEOUT=2` here**:
    `TestReviewAuthorityYoloDoesNotBypassBlock` runs concurrently against the same bed
    setting and needs the timeout comfortably above ~2 minutes, since its 90s "block
    persists under yolo" window starts shortly after `fabrik:awaiting-review` first
    appears — a too-short timeout risks a legitimate review-wait-timeout pause landing
    inside that window, which the test detects and fails on explicitly (distinct message,
    not misreported as a yolo bypass) rather than passing. **Leave
    `FABRIK_REVIEW_WAIT_TIMEOUT` at its 15-minute default**: it satisfies this, and
    `TestConjunctiveCIReviewGate` (prerequisite 13) *skips* below 10 when
    `FABRIK_REVIEWER_TOKEN` is set, so a value like 5 silently drops that scenario
    from the gate.
24a. **`TestReviewAuthorityCycleLimitPauses` needs a small `FABRIK_MAX_REVIEW_CYCLES`**
    (set `FABRIK_MAX_REVIEW_CYCLES=3` in the bed `.env`) for a bounded wall-clock — each
    cycle requires a full reinvoke (a real Claude invocation) before the next distinct
    `REQUEST_CHANGES` review can be submitted. It also has to reach the cycle-limit pause
    before the review-wait timeout (15 minutes, above) pauses the item first. With the
    engine default of 5 and dispatches several minutes apart under load, the timeout
    won that race in the 0.0.83 gate and the scenario failed on the wrong terminal
    comment. Not 2: the bed's own reviewer (Pruefer) reviews test PRs, and each of its
    reviews spends a cycle. At 2, that paused `TestReviewAuthorityYoloDoesNotBypassBlock`
    (a harness `REQUEST_CHANGES` plus Pruefer's review) before its `APPROVE` could clear
    the gate. 3 is the smallest value that serves both scenarios. The test skips itself with an instructional message if the bed's
    configured value is above 5 (too large for a reasonable e2e run). Defaults to the
    engine's own default (5) if `FABRIK_MAX_REVIEW_CYCLES` is unset in the bed `.env`,
    which will cause the skip.
25. **Note on scope**: neither test bed repo has a branch-protection review requirement
    configured (only required *status checks* are documented as enrolled), so
    `FetchPRReviewDecision` returns `""` for every scenario here and `reviewGateAuthorityVerdict`
    exercises its Fabrik-computed fallback branch, not GitHub's native `reviewDecision`
    branch. A verdict-fetch-failure / unrecognized-`reviewDecision` scenario (issue #1258's
    optional scenario 6) was excluded for the same reason — producing `REVIEW_REQUIRED` or
    an unrecognized value would require new branch-protection bed setup, which is not
    "cheaply expressible" per the issue's own bar for that scenario.

### Additional prerequisites for `TestExpectedReviewers*` scenarios

`TestExpectedReviewersFastAdvance`, `TestExpectedReviewersDeclaredWaitsAndReprompts`,
and `TestExpectedReviewersFastAdvanceComposesWithAuthoritative` cover ADR-1283's
`expected_reviewers` (declared unrequested reviewers for the review gate).
`TestExpectedReviewersUndeclaredRegressionGuard` is the fourth scenario and has no
dependency described below. All four run against the bed's existing `Review`
column/stage (default, untouched config) — **no bed column or stage-YAML setup is
required**, beyond `FABRIK_REVIEW_WAIT_TIMEOUT` (already documented above) and the
two labels below.

A fifth scenario in this file, `TestReviewAuthorityDeclaredBotDoesNotDeferHumanEscalation`,
covers AC2 of #1375 (Finding 2's fix): a declared `expected_reviewers` bot must not defer
an outstanding human reviewer's `review_authority: authoritative` escalation. It combines
both mechanisms — `expected-reviewers:declared` (this section) and
`review-authority:authoritative` (previous section) — on the same issue, with a real
requested human reviewer (via `RequestPRReviewer` + `FABRIK_REVIEWER_TOKEN`, not a draft
PR), so it has both sections' prerequisites and needs `FABRIK_REVIEWER_TOKEN` in addition
to the two labels below.

**Mechanism: two per-issue labels, not a bed column.** A declared `expected_reviewers`
value is applied per item via one of two labels, passed as an extra label at seed
time (`seedReviewGateItem`'s `extraLabels`):

- `expected-reviewers:none` → `expected_reviewers: []` (fast-advance path)
- `expected-reviewers:declared` → `expected_reviewers: [e2e-synthetic-declared-reviewer]`
  (waiting/re-prompt-ladder path)

This is the same mechanism `TestReviewAuthority*` uses for
`review-authority:authoritative` (see above), applied to a second, list-shaped
stage-config field. Engine support for reading these two labels is tracked as a
separate, decoupled follow-up issue (not yet filed as of #1298's PR — see that
PR's description for the exact spec: the four call sites to update, the
`declared` > `none` precedence rule, and the `github/labels.go` pre-seeding step).
`TestExpectedReviewersFastAdvance`, `TestExpectedReviewersDeclaredWaitsAndReprompts`,
and `TestExpectedReviewersFastAdvanceComposesWithAuthoritative` cannot pass until that
follow-up merges and both labels exist on the bed repo — they either run for real
or fail loudly (not skip) if the follow-up hasn't landed yet, mirroring #1258's
rejection of silent skips. `TestExpectedReviewersUndeclaredRegressionGuard` sets
neither label, exercises the bed's untouched default (`nil`) config, and ships
green regardless.

A bed-local `wait_for_reviews`-bearing stage/board-column variant (mirroring the
`Queued`/`queued.yaml` precedent) was considered and rejected for this feature too
— see `adrs/1298-e2e-expected-reviewers-coverage.md`. Its blast radius is worse
than the `Review-Authoritative` design #1258 already rejected: a normal (non-
`HoldingStage`, non-`Unmanaged`) stage gets no board-column-alignment exemption at
startup (`engine/startup.go` `checkStageColumnAlignment`), so a missing column
would stop the shared bed from starting entirely — not just skip a handful of
scenarios — taking every other in-flight parallel scenario down with it.

**Why these scenarios can't cover the landing/auto-merge gate:** same reason as
`TestReviewAuthority*` above — `reviewGateBlocksLanding` is only reachable through
a stage literally named `Validate`, and seeding on `Review` cannot reach it. This
is a documented, accepted e2e gap.

26. **`expected-reviewers:none` and `expected-reviewers:declared` labels seeded**
    in `handarbeit/fabrik-test-alpha` (the only repo these scenarios use).
    `FileIssue` passes extra labels straight through to `gh issue create --label`,
    which — like `AddLabel` (prerequisite #9) and prerequisite #22's
    `review-authority:authoritative` label — fatals immediately if a label doesn't
    already exist as a label object in the repo; `gh` does not auto-create labels
    on issue creation. Create both manually if needed:
    ```
    gh label create expected-reviewers:none -R handarbeit/fabrik-test-alpha
    gh label create expected-reviewers:declared -R handarbeit/fabrik-test-alpha
    ```
    This is independent of the follow-up engine issue: that issue adds the code
    that *interprets* a label an issue already carries, not the GitHub label
    object itself — the objects must exist before any of these scenarios can even
    file their seed issue.
27. **`TestExpectedReviewersDeclaredWaitsAndReprompts`'s wall-clock is long**
    (~2×`FABRIK_REVIEW_WAIT_TIMEOUT` + buffer) — Phase 1 and Phase 2
    of the bot re-prompt ladder are folded into one continuation. The same
    moderate `FABRIK_REVIEW_WAIT_TIMEOUT` value recommended in prerequisite #24
    (e.g. `5`) applies here too; a very short value risks a legitimate Phase 1/2
    transition racing this test's own bounded-window assertions in
    `TestExpectedReviewersFastAdvance`/`...ComposesWithAuthoritative`.
28. **The synthetic declared-reviewer name (`e2e-synthetic-declared-reviewer`)
    must never resolve to a real, active GitHub account** on the bed's org —
    same rationale as prerequisite #23's warning against reusing a real installed
    bot: an unrelated real actor submitting a review would race the deterministic
    re-prompt-ladder assertions in `TestExpectedReviewersDeclaredWaitsAndReprompts`.
29. **Draft-PR determinism technique (#1312)**: `TestExpectedReviewersFastAdvance`,
    `TestExpectedReviewersDeclaredWaitsAndReprompts`,
    `TestExpectedReviewersUndeclaredRegressionGuard`, and
    `TestExpectedReviewersFastAdvanceComposesWithAuthoritative` seed via
    `seedReviewGateItemDraft` (which calls `CreateMemberPRDraft`) instead of
    `seedReviewGateItem`. Their properties under test — "nothing was requested",
    "declared but unrequested" — would be falsified by requesting a reviewer via
    `RequestPRReviewer`, so unlike `TestReviewAuthority*` these scenarios can't
    win the race deterministically that way. Opening the member
    PR as a draft instead removes the race altogether: Pruefer (the bed's real
    reviewer — see "Reviewer topology" below; `claude-review.yml` was
    deleted 2026-08-13) only lists "open, non-draft PRs" each poll
    (`cmd/pruefer/README.md`), so a draft PR that is never marked ready is
    permanently invisible to it — there is no incidental bot review to land
    before the engine's first gate evaluation, or before these scenarios' own
    bounded-window assertions. This requires no additional bed setup; it is
    purely a change in how the harness constructs the member PR.

### Additional prerequisites for `TestLateCheckRunSuiteGate` (#1849)

`TestLateCheckRunSuiteGate` is the live proof of the suite-aware CI gate (#1822,
#1829): with `wait_for_ci` on, the gate must not clear while a check run that does
not exist yet — a job queued behind a `needs:` dependency — is still to come.
The sim bed cannot see this GitHub wire timing (ADR-1454), so it lives here.

- **Install the workflow on `fabrik-test-alpha`.** Commit
  `tests/e2e/testdata/late-check-suite-gate.yml` to
  `handarbeit/fabrik-test-alpha` as `.github/workflows/late-check-suite-gate.yml`
  (a manual commit, exactly as for `train-poison-guard.yml` in the merge-train
  prerequisites). It defines two jobs: `late-check-fast` (succeeds once the
  engine has applied `fabrik:awaiting-ci`, so the CI gate is active; fails open
  after ~35 min) and `late-check-slow` (`needs: late-check-fast`, sleeps ~4 min,
  succeeds). The scenario skips cleanly if the workflow is absent or not
  `active`.
- **Do NOT mark either check required on branch protection.** The workflow is
  path-scoped (`e2e/late-check/**`), so on any other PR it never reports; a
  required check that never reports sits at "Expected" forever and would block
  every unrelated PR on the repo. The engine's gate reads every check run and
  suite on the SHA regardless of required status, so nothing is lost — and the
  scenario needs no branch-protection API access.
- **Scoping.** Only PRs that touch `e2e/late-check/**` *and* whose head branch is
  `fabrik/issue-N` run the jobs (a job-level `if:` guard; merge-train
  trial/integration PRs that happen to match the path filter get skipped check
  runs, which the engine treats as neither pending nor failed). No other scenario
  pays the ~4 min sleep or is otherwise affected.
- **Optional: widen the window.** The gap between the fast job finishing and the
  late job's check run existing is normally only seconds. `late-check-slow`
  declares `environment: late-check-gate`, which GitHub auto-creates on first
  use; adding a **wait timer** (e.g. 3 min) to that environment in the repo's
  Settings → Environments holds the job before it is scheduled, widening the
  "fast green, late run absent" window from seconds to minutes and making the
  scenario a much stronger detector of a #1822 regression. Without it the
  scenario still proves the deterministic property (gate holds until the late job
  completes) but a regressed engine polling every 60s would only sometimes clear
  inside the gap.
- **Cost / wall-clock.** One real Validate Claude invocation and one CI cycle
  (~5 runner-minutes) per mode; ~20–35 min, of which ~4 min is the sleep.

### Notes on `TestPauseLiftedOnlyByPostPauseHumanComment` (#1876)

The live proof of ADR-1813 / #1813 (the #1752 incident class): a pause is lifted only
by a **human** comment created **at or after** the latest `fabrik:paused` `labeled`
event. The rule depends on real GitHub event timestamps, which the sim bed cannot
see (ADR-1454; `tests/sim/pause_resume_anchor_test.go` covers the logic against
simulated timestamps only). No bed setup beyond the standard one.

- **How the comment predates the pause without being consumed.** The issue is filed
  in Alpha but **not added to the project board**. The engine discovers work only
  through `FetchProjectBoard`, so an off-board issue can be neither deep-fetched nor
  dispatched; the comment cannot be processed before the pause anchor exists. The
  scenario then posts the old comment, waits 6s, applies `fabrik:paused` +
  `fabrik:awaiting-input`, waits 6s, and only then adds the issue to the board and
  moves it to Specify. (Parking in a Backlog column is untested on the bed — no
  `backlog.yaml` is installed there — and is a weaker guarantee than off-board.)
  The 6s gaps exceed GitHub's one-second timestamp resolution; equality resolves
  toward resuming, so they must not be shrunk.
- **Assertions.** A1: the engine's own refusal line for this issue
  (`[#N skip] awaiting-input: 1 human comment(s) predate the pause — still waiting`,
  `engine/item.go`'s `itemNeedsWork`; *not* the unreachable `resume refused
  (ADR-1813)` lines in `processItem`), both pause labels still present after a
  ≥4 min window (≥3 polls at the 60s bed cadence), and no 👀/🚀 on the old
  comment. A quiescent paused item is **not** re-evaluated every poll, so the line
  is not counted per poll: two `🏭 **Fabrik — e2e nudge**` comments are posted to
  re-evaluate it (they are skipped by `findNewComments`, so never resume-eligible).
  A2: the `issues/N/events` log shows exactly one `labeled` and zero `unlabeled`
  `fabrik:paused` events (no lift/re-add cycles, no re-stamp on the board move). A3:
  a new human comment posted after the pause gets 👀 then 🚀 and the events log gains
  an `unlabeled` `fabrik:paused` event (the events log, not current labels, because
  the resumed worker may block again). Nothing is asserted about the *old* comment
  after the resume: an authorised resume hands the full raw `findNewComments` set to
  `processComments` (ADR-1813 R5), so it is legitimately processed with the new one.
- **Both auth legs.** Only the harness account (arbeithand, `FABRIK_TOKEN`) posts. It
  matches none of `gh.IsBotLogin`'s patterns, so `filterHuman` reads it as human in
  both legs; in the PAT leg it is also Fabrik's own identity, but every engine
  comment carries the `🏭 **Fabrik` prefix and is skipped by `findNewComments`. In
  the App leg the engine posts as `<slug>[bot]`. No reviewer-token fallback is
  needed and there is no per-leg skip. A4 (the item's own bot comments never
  resume) is observed only for the `🏭`-prefix path through the nudges; a genuine
  `[bot]`-login classification is left to the App-auth self-recognition scenario.
- **Cost / wall-clock.** One Specify comment-review invocation after the resume;
  ~10–15 min, ~$0.15–0.40 per leg. Parallel-safe: it touches only its own issue and
  reads the shared bed log scoped by issue number and `LogOffset`. The pure helpers
  (`pauseRefusalLogNeedle`, `parseLabelEvents`, `countLabelEvents`,
  `parseCommentReactions`) are unit-tested by
  `go test -tags e2e -run 'PauseRefusal|LabelEvent|CommentReactions' ./tests/e2e/`.

### Additional prerequisites for the unprocessed-comment landing scenarios (#1873)

`TestCommentLandingGateHolds`, `TestQueuedMemberCommentEjection` and
`TestPostMergeCommentNotApplied` cover the #1862/#1863 behaviour at the landing
boundary (see `adrs/1873-e2e-comment-landing-coverage.md`).

- **`FABRIK_REVIEWER_TOKEN`** in the bed's `.env` (a non-author PAT — see
  "Reviewer topology"). `TestCommentLandingGateHolds` and
  `TestQueuedMemberCommentEjection` skip cleanly without it: they need a
  deterministic `APPROVE` so the review gate never decides when the item lands, and
  a review-wait timeout would apply `fabrik:paused` and corrupt their assertions.
  `TestPostMergeCommentNotApplied` does not need it.
- **`slow-gate` required on `fabrik-test-alpha/main`** for the first two
  (`assertSlowGateRequired` skips otherwise). The gate scenario waits for it to go
  green so no Phase 1 gate claims the item; the eject scenario relies on it to hold
  the occupant member's trial open.
- **Queued column + `merge_train: on`** for `TestQueuedMemberCommentEjection` only
  (`requireTrainBed`). `TestCommentLandingGateHolds` runs in both legs;
  `TestPostMergeCommentNotApplied` is mode-invariant.
- **Labels** `fabrik:yolo`, `stage:Validate:complete` and `stage:Implement:complete`
  must exist in the repo (they are seeded with `gh issue create --label` /
  `AddLabel`); they already do on a bed that runs the other scenarios.
- **`TestQueuedMemberCommentEjection` is not parallel** and pre-flights for stale
  Queued items on RepoAlpha, like the batch-cap scenario (prerequisite #22).
- **Auth legs.** The harness posts every "human" comment as `FABRIK_TOKEN`'s
  account (arbeithand). That reads as human to Fabrik in both the PAT leg (Fabrik
  posts as the same account, distinguished only by the `🏭 **Fabrik` body prefix,
  which the helper refuses to post) and the App leg (Fabrik posts as `<slug>[bot]`).
  Reaction assertions check existence and timestamps only, never the reacting login.
- **Cost / wall-clock** (per run): gate ~20–35 min and ~$0.10–0.50 per train mode
  and auth leg; eject ~35–60 min and ~$0.10–0.50, on-leg only; post-merge ~3–8 min
  and no Claude. Across both auth legs and both train modes that adds roughly
  2×(2×25 + 45 + 5) ≈ 200 min of scenario time (much of it overlapping the parallel
  pool) and ~$1–3 to a full gate run.

### Additional prerequisites for `TestAppSelfRecognition*` (#1877)

Three App-leg-only scenarios (files `self_recognition_test.go`, `app_identity.go`,
`self_recognition_helpers.go`; decision record `adrs/1877-e2e-app-self-recognition.md`).

1. **Bed-local `E2E_APP_ID` / `E2E_APP_PRIVATE_KEY_PATH` / `E2E_APP_INSTALLATION_ID`**
   in the bed's `.env` (already required by the App auth leg). The harness mints
   an installation token from them with the `github` package's own
   `ParseAppPrivateKey`/`BuildAppJWT`/`MintInstallationToken` (no second JWT
   implementation) so it can post as `<slug>[bot]`. A relative key path resolves
   against the bed directory. The token is passed to `gh` via the environment only,
   never in argv, and never logged; every `gh` output that reaches a failure
   message is redacted. With none of the keys set the scenarios skip; a partial set
   fails.
2. **`fabrik:paused`, `fabrik:awaiting-input`** labels exist on Alpha (created
   on demand, never deleted), and **`review-authority:authoritative`** (as for
   `TestReviewAuthority*`).
3. **`FABRIK_REVIEWER_TOKEN`** set (distinct from the PR-author account) for the
   review-suppression scenario only; it skips without it.
4. The harness account (`FABRIK_TOKEN`) must not classify as a bot
   (`github.IsBotLogin`); in the App legs it is the "human" of every positive control.

| Scenario | Asserts | Claude cost | Wall-clock |
|---|---|---|---|
| `TestAppSelfRecognitionBotCommentNeverResumes` (A1) | A plain, marker-free bot comment on a paused item never resumes it: after the engine's `none human-authored` evaluation line, 3+ polls with the pause labels intact, no `unpause`/`unblock` line, no 👀/🚀 on the comment; then a human comment resumes it (control) | 1 comment-processing invocation | ~6–10 min |
| `TestAppSelfRecognitionBlockedCommentUpdatedInPlace` (A2) | After the dependency set changes ({B1} → {B1,B2}) the one blocked comment (same id, bot-authored) is edited in place: its parsed dependency set equals the new set | none | ~12–25 min (waits out the `dep-blocked` cooldown, `PollSeconds*10`) |
| `TestAppSelfRecognitionDurableReviewSuppression` (A3) | A `review-ids-addressed` marker comment authored by the bot suppresses redelivery of that review (`durablyAddressedReviewIDs`); the same marker authored by the harness account does not (control) | 1 review-reinvoke (control arm) | ~8–15 min |

Run in parallel, ~15–25 min per App leg, ≈ $0.10–0.60; the cost is paid in both the
`app/off` and `app/on` legs. There is no PAT counterpart: the other scenarios
already exercise the PAT identity.

**Non-vacuity, stated honestly.**

- **A3 discriminates #1754.** Pre-fix `durablyAddressedReviewIDs` compared
  `c.Author != e.cfg.User` to the REST author `fabrik-bed[bot]`, so the bot's
  marker was ignored and `review-body:R` was dispatched. The control arm proves the
  author scoping still rejects a non-self marker.
- **A2's dependency-set assertion is the one that catches the #1754 regression.**
  Pre-fix `findBlockedComment(…, e.cfg.User)` matched nothing and skipped the
  update, leaving a *stale* body, not a duplicate; a count check alone would not
  notice.
- **A1 does not discriminate the #1754 delta.** Pre-fix `filterHuman` had no
  `cfg.User` comparison; the defect was the cache write-through stamping
  `Author: cfg.User` on Fabrik's own posts, a transient human classification that is
  not live-observable. A1 guards `filterHuman`/`gh.IsBotLogin` on the real wire
  shape and the ADR-1813 resume path. Its negative arm is positive-first: it waits
  for the engine's own evaluation line before starting the hold, since only the
  first evaluation of a paused item is guaranteed to log.

**Known expected-red: A1 and A2 on the App leg until the engine normalises
comment authors at ingestion (tracked in #1898).** Research found (from the code, not confirmed live)
that REST reports a bot comment's author as `fabrik-bed[bot]` but the GraphQL
comment fragment (`author { login }`, no `__typename`) yields the bare
`fabrik-bed`, and under App auth (webhooks are refused, ADR-1752) the engine reads
issue comments through GraphQL. If so, `IsBotLogin("fabrik-bed")` is false and
`findBlockedComment` never matches, so A1 and A2 fail on a live App leg for a reason
outside this change (mirror `applyLinkedPRs`'s review-author normalisation
for comments). The scenarios assert the correct behaviour and are not weakened; each
logs both wire shapes (`REST=… GraphQL=…`) so a failure is attributable. A3 reads
REST and is unaffected. A release gate that goes red on A1/A2 for this reason can be
released with `cut-release.sh --skip-integration=<reason>` until #1898 lands. A name ending in `-bot` would make A1 pass through `IsBotLogin`'s suffix rule
regardless; the bed's `fabrik-bed` does not.

**A3 topology.** GraphQL `latestReviews` keeps one review per reviewer, so two
reviews cannot share a PR: each arm has its own item/PR and one review from the
reviewer token. Each review is created PENDING (invisible to the engine), the marker
comment is posted against its id, and only then is the review submitted, so there is
no race with the poll. The suppressed arm logs nothing of its own; the control arm's
dispatch is the positive proof that the engine evaluated review feedback after both
reviews existed, before the 3-poll negative hold.

### Reviewer topology (#1396)

**Fixture PRs opt out of Pruefer.** Every PR the harness creates itself
(`createMemberPRBody`: merge-train members, review-authority and expected-reviewers
seeds, and the other fixture scenarios) carries the `pruefer:ignore` label, Pruefer's
built-in opt-out. Those scenarios drive every review themselves through
`FABRIK_REVIEWER_TOKEN` and assert on exact review counts, cycles or batch membership.
In the 0.0.83 gate an incidental Pruefer review perturbed two of them: a reasonable
duplicate-fixture finding ejected batch-cap members, and a `COMMENTED` review spent a
review cycle. Scenarios that need a real review use engine-created pipeline PRs, which
stay unlabelled. The label only takes effect once the bed's Pruefer runs a build that
honours it, or has `excluded_labels: [pruefer:ignore]` in its config.

Every scenario that drives a PR through the organic Review gate depends on
some external actor actually submitting a review. As of #1396, the bed's
reviewer topology is:

- **Pruefer (`handarbeit-pruefer`) is the present, real reviewer.** Once
  `.pruefer/config.yaml`'s `watched_repos` includes `fabrik-test-alpha` and
  `fabrik-test-beta` (an operator action against Pruefer's own deployment,
  not a file in this repo — `.gitignore` excludes `.pruefer/` wholesale),
  Pruefer polls both test repos like every other repo it watches (default
  `poll_interval_seconds: 120`, `concurrency_cap: 3`) and reviews open,
  non-draft PRs. The bed's `expected_reviewers: [handarbeit-pruefer]`
  declaration in `review.yaml`/`validate.yaml` names this identity — no bed
  stage-YAML change was needed, since the identity was already correct, only
  previously unreachable.
- **`e2e-synthetic-declared-reviewer` is the deliberately absent reviewer.**
  Reachable per-issue via the `expected-reviewers:declared` label (ADR-1283,
  see prerequisite #26), it never posts a review by construction
  (`engine/reviews.go:523`) — used by scenarios that need a declared-but-
  never-responding reviewer (`TestExpectedReviewersDeclaredWaitsAndReprompts`
  and friends), independent of Pruefer's own presence or health.
- **`claude-review.yml` is deleted from both test repos** (2026-08-13), along
  with the copy on `handarbeit/fabrik`. Pruefer is the only reviewer bot in
  this topology.

  This section previously claimed the workflow was "disabled, not deleted"
  via `gh workflow disable`, and that #1396 had disabled it when Pruefer took
  over. **Neither was true.** The workflow was enabled and reviewed every bed
  PR right up to its deletion, and Pruefer's `watched_repos` never included
  the test repos, so the reviewer the bed's stage configs declared
  (`expected_reviewers: [handarbeit-pruefer]`) could not reach it — #1396
  Defect 1, on a closed issue. The prose described the intended end state as
  if it had been reached; nothing re-checked it, so it read as settled fact
  for weeks.

  What actually made the handover real: adding both repos to Pruefer's
  `watched_repos`, then confirming empirically that it posts reviews
  (`fabrik-test-alpha#4731`, 2026-08-13T12:56:11Z) before deleting anything.
  If you are reading this because reviews stopped arriving, check Pruefer's
  TUI row for the repo — and note that its "polled N ago" counter climbing
  means the PR count beside it is *stale*, not that the repo is empty.
- **Gemini does not participate in this topology at all.** It was considered
  as the deliberately-absent reviewer and rejected: Gemini is *usually*
  silent on the bed but not reliably so, which would make a scenario's
  verdict a function of a third party's uptime rather than a deterministic
  property (see the issue's discussion for the full reasoning).
- **A dropped Pruefer review-dispatch still presents as a review-timeout
  pause** — same failure signature the Problem section of #1396 describes
  for the (now-retired) `claude-review.yml` case. `WaitForIssueClosedWithReviewCheck`
  (see prerequisite/harness discussion above) exists to catch this faster and
  more legibly than running out the full close-timeout, but it does not
  change the underlying risk: Pruefer going silent (dispatch dropped,
  daemon down, etc.) is still a single point of failure for every scenario
  that depends on an organic review landing.

### Operational up/down contract (#1684)

The two sections above describe two operational requirements that, before
this issue, existed only in operators' heads — getting either wrong costs an
hour or more per mistake. Both were hit during the v0.0.81 cut, and one
directly caused the other: Pruefer was stopped to free budget, which broke
every review-gated scenario.

**Must be UP: Pruefer.** Every scenario that drives a PR through the organic
Review gate depends on Pruefer (see "Reviewer topology" above) actually
submitting a review. If it's down, PRs sit with zero reviews and review-gated
scenarios fail ~10 minutes in with a timeout that looks like a Fabrik defect,
not an ops gap.

**Must be DOWN (non-competing): every other Fabrik instance authenticated as
`@arbeithand`.** The bed and this suite share one `@arbeithand` PAT with a
5,000/hour GraphQL bucket (see "GraphQL budget" above), and a single live leg
consumes ~4,000 of it. Any other poller on that token — most commonly an
operator's own always-on production Fabrik instance — pushes the run into
backoff, which invalidates it *after* it has already spent an hour and
appeared to pass.

**The trap: stopping Pruefer to save budget breaks the suite, and saves
nothing.** Pruefer authenticates as a GitHub App with its own
per-installation rate-limit bucket (ADR-1113, ADR-1253) — it never draws on
the shared `@arbeithand` user PAT at all. Stopping it does not free a single
point of the 5,000/hour budget the gate actually needs; it only guarantees
every review-gated scenario times out. The oscillation between "stop it for
budget" and "start it for reviews" was the symptom this issue exists to
eliminate — the two constraints are independent, not in tension.

**Automated preflight (`scripts/e2e/run.sh`):** both facts are now checked
before any live GitHub/Claude call, ahead of the sim+wire-contract pre-gate
(#1454) and the bed preflight:

- `CheckCompetingTokenConsumers` (R1) enumerates locally-running `fabrik`
  processes, resolves each one's working directory, and compares its `.env`
  `FABRIK_TOKEN` against the bed's own — excluding the bed's own directory
  (already budgeted into the ~4,000/5,000 estimate). **Refuses by default**
  on a match, naming the offending PID(s) and directory/directories: a silent
  proceed-into-backoff is strictly worse than one operator round-trip.
  Escape hatch: `E2E_SKIP_TOKEN_CHECK=1`.
- `CheckReviewerReachable` (R2) checks Pruefer's own liveness via
  `$PRUEFER_DIR/.pruefer/pruefer.lock` (default `PRUEFER_DIR`:
  `$HOME/dev/fabrik`, matching the observed co-located deployment
  convention) — `kill -0` against the PID Pruefer writes into that lock file
  on acquisition. **Refuses** when `PRUEFER_DIR` exists but the lock is
  missing or names a dead process (confidently down); **warns only** (never
  blocks) when `PRUEFER_DIR` itself doesn't exist, since a remote Pruefer
  deployment (see `cmd/pruefer/README.md`'s SSH-tunnel setup section) is
  undeterminable from here, not confirmed-down. Automatically
  skipped when the invocation includes `-run`/`--run`, since a narrowed
  scenario subset may not include any review-gated scenario — left to the
  operator's judgment rather than inferred. Escape hatch:
  `E2E_SKIP_REVIEWER_CHECK=1`.

Both checks are local-only and make no live GitHub/Claude call themselves
(process/file inspection only), so they add negligible wall-clock ahead of
the checks they front-run. See `tests/gate/consumers.go` for the mechanism, and
`tests/gate/preconditions_test.go` for their regression coverage (ported from the
former `token_consumer_check_test.sh` / `reviewer_reachable_check_test.sh`).

## Running

The recommended entrypoint is the runner script, which sets sensible defaults:

```bash
# Full two-mode validation gate — off, then on (slow: two full runs)
scripts/e2e/run.sh

# Single scenario, both modes
scripts/e2e/run.sh -run TestSmokeSingleRepoDispatch

# Subset by name pattern, both modes
scripts/e2e/run.sh -run 'Smoke|NoWork'

# Resume an interrupted gate: run only the (test, leg) pairs the per-SHA ledger
# still lacks a valid PASS for (details: "Coverage ledger, --resume, ..." below)
scripts/e2e/run.sh --resume
scripts/e2e/run.sh --clean --resume

# Read-only: is live coverage complete for this SHA? (exit 0 yes, 8 no)
scripts/e2e/run.sh coverage
```

Anything after the script name is passed through to `go test`. Override the
overall test timeout with `E2E_TIMEOUT` (default `4h`) — see "How the
timeout/parallelism defaults are derived" and "Timeout & failure reporting"
below for what backs that number and what happens if it's still hit.

The `e2e` build tag keeps all of this out of the default `go test ./...` run.

Set **`E2E_JITTER_SEED`** (a `uint64`) to make the harness's poll-interval
jitter (see below) reproducible — useful for locally reproducing a
flaky-looking failure. Leave it unset for normal runs; the jitter self-seeds
randomly by default.

#### Two-mode validation gate — `merge_train: off` and `merge_train: on`

`FABRIK_MERGE_TRAIN` is read once, at Fabrik startup, so exercising both
landing paths requires restarting the bed between them — it cannot be flipped
mid-run while `t.Parallel()` scenarios are in flight. By default `run.sh`
drives this itself: for `off` then `on`, it runs a narrow `go test` invocation
of `TestSwitchTrainMode` (stops the bed, edits `FABRIK_MERGE_TRAIN` in its
`.env`, restarts it — a wholly separate process from the suite invocation
that follows, so the restart is always complete before any scenario starts),
then the full suite with `E2E_TRAIN_MODE` exported. `off` runs first because
it's the path nearly all real usage takes; a regression there surfaces before
spending time on the less-common train-on run.

Force a single mode instead of the two-mode default with `E2E_TRAIN_MODE`:

```bash
E2E_TRAIN_MODE=off scripts/e2e/run.sh -run TestSmokeSingleRepoDispatch
E2E_TRAIN_MODE=on  scripts/e2e/run.sh
```

A two-mode run is roughly double the single-mode GitHub API cost — see #1219
for the budget headroom this assumes, and merge it before attempting a full
two-mode run.

Both `go test` invocations inside the leg executor (`RunLeg`; the `TestSwitchTrainMode`
step and the suite invocation that follows it) pass `-count=1`, Go's standard
mechanism for defeating the test cache. Neither is safe to serve from cache:
the switch step is deliberately side-effecting (it stops/restarts the shared
bed), and the suite invocation reads live external state (the bed process,
GitHub). A cached `PASS` on either would be replayed as though it ran while
none of that actually happened — see #1327. `TestSwitchTrainMode` also
asserts its own postcondition (bed running, `.env` mode matches) after
`StartFabrikTestBed` returns, so a cached or partially-failed switch fails
loudly instead of reporting success.

#### Auth-mode legs — PAT and GitHub App (#1861)

The train-mode legs above run once per auth mode: first with the bed engine on
its PAT (`FABRIK_TOKEN`), then as the GitHub App installation. Auth mode changes
Fabrik's own identity: every "is this mine or a human's?" decision, every push
and merge, and repo-access resolution. So the App legs rerun the full suite.

The same `TestSwitchTrainMode` restart applies it. When `E2E_AUTH_MODE` is set,
it writes `FABRIK_GITHUB_APP_*` into the bed's `.env` from the bed's own
`E2E_APP_*` values (or blanks them for `pat`), restarts the bed, and reads the
bed's stdout (`bed-run.log`) to verify the identity it started as. A bed that
comes up as the wrong identity fails the switch step before any scenario runs.

```bash
scripts/e2e/run.sh                          # pat/off, pat/on, then app/off, app/on
E2E_AUTH_MODE=app scripts/e2e/run.sh        # App legs only
E2E_AUTH_MODE=pat E2E_TRAIN_MODE=off scripts/e2e/run.sh -run TestSmokeSingleRepoDispatch
```

Leg labels in the reports carry both modes (e.g. `app/on`). The full default
gate is therefore four suite runs (six `go test` legs, counting the isolated
runaway-guard leg under each auth mode), roughly double the Claude quota of the
PAT-only gate. When only App legs are planned, the competing-token check warns
instead of refusing: the bed engine spends the installation's own GraphQL
budget, and only the harness's own calls share `FABRIK_TOKEN`'s. For the same
reason, an App leg's "GraphQL budget" report line measures only the harness.

The harness still files issues and posts its scripted replies as
`FABRIK_TOKEN`'s account. In PAT mode that is Fabrik's own identity; in App
mode it is a human to Fabrik. Triage an App-only failure with that in mind:
it may be a scenario that leaned on the harness sharing Fabrik's identity.

**App-only scenarios (#1877).** `TestAppSelfRecognition*` assert that Fabrik
recognises its own `<slug>[bot]` comments as its own (guarding #1754's
`selfLogin()` routing). They run only in the App legs and **skip in the PAT legs**
with a stated reason: `requireAppLeg` decides from `E2E_AUTH_MODE`
(`normalizeAuthMode`) or, when unset, from the bed's startup identity
(`bedAuthIdentity`) via the pure `decideAppLegRun`. An `app` leg whose bed shows no
App identity fails loudly rather than skipping. See "Additional prerequisites for
`TestAppSelfRecognition*`" below.

Scenarios resolve mode via `resolveTrainMode` (`harness.go`): `E2E_TRAIN_MODE`
takes precedence when set (an invalid value is a hard test failure), falling
back to a lenient read of the bed's own `.env` for ad-hoc/manual runs where
the switch step never ran. The "Mode" column in the Scenarios table below
records which scenarios assert a mode-specific contract.

#### Parallelism cap — the shared bed oversubscribes easily

16 of the 18 scenarios are `t.Parallel()`, but they **all drive one shared
Fabrik bed** (5 workers by default) against **one shared board and one shared
GitHub API budget**. Go's default `-parallel` is `GOMAXPROCS` (~8–12 cores), so
an unbounded full run fires ~16 scenarios at once, floods the 5-worker bed, and
saturates the API — producing cascading `transient gh error … (will retry)`
timeouts **even though every scenario passes standalone** (see issue #971).

`run.sh` therefore caps concurrency with `-parallel`, defaulting to **4**
(`E2E_PARALLEL`):

```bash
E2E_PARALLEL=2 scripts/e2e/run.sh   # tighter cap for a heavy/merge-train-heavy run
E2E_PARALLEL=6 scripts/e2e/run.sh   # looser, only if the bed's --max-concurrent is raised too
```

Lower values reduce oversubscription at the cost of wall-clock. The long
merge-train and CI-fix scenarios (see their notes above) are still best run in
isolation. **Do not** run the full suite unbounded expecting a clean pass — the
failure will be timeouts, not real regressions.

On top of the `-parallel` cap, every GitHub-polling wait helper's retry
interval is jittered ±20% (see `pollSleep` in `harness.go`) so concurrent
scenarios' polls desynchronize instead of converging into lockstep bursts
against the shared API budget (see #1104).

#### How the timeout/parallelism defaults are derived

Both `E2E_TIMEOUT=4h` and `E2E_PARALLEL=4` are backed by data already
committed in this file and by a real two-mode gate run, not chosen
arbitrarily — they're documented here so future drift (new scenarios, bed
resizing, GitHub API changes) is visible and the numbers can be revisited
deliberately rather than silently going stale.

**`E2E_TIMEOUT`: 90m → 4h.** A real two-mode gate run was killed by the
original 90m timeout while `TestCIFixReinvokeCycleLimit` was still executing
at 1h26m26s — already past its own documented 30–60min ceiling (see the
per-scenario table below). Two scenarios with paired off/on timings from that
run showed a ~1.55–1.61x contention multiplier under load:
`TestConjunctiveCIReviewGate` 1335s → 2152s, `TestPausedMergedPRRecovery`
1382s → 2146s. Applying that multiplier to the heaviest documented
per-scenario ceilings (`TestCIFixReinvoke` 75–90min, `TestPausedMergedPRRecovery`
60–90min) puts the contended worst case in the ~93–145min range (60min ×
1.55x low end, 90min × 1.61x high end). `4h` leaves ~95–147min of margin
above that for bed-restart and pipeline-setup overhead.
This is a reasoned extrapolation from two paired data points plus one
partial-kill observation, not a fresh full-suite measurement under the new
default — treat it as provisional, and re-derive it (repeating this
arithmetic with fresh paired timings) after any run that gets meaningfully
closer to 4h than the numbers above predict.

**`E2E_PARALLEL`: kept at 4, not lowered.** The available contention data
doesn't clearly indict 4 as an oversubscribed cap: the bed has 5 workers, so
4 already reserves headroom, and the "on" leg's slowdown is at least partly
explained by it having strictly more real work to do (17 scenarios vs. 13 —
the four Train-only scenarios skip near-instantly under "off"), not
necessarily by 4 being too high a concurrency cap. The one scenario failure
plausibly linked to bed starvation in the observed run
(`TestReviewAuthorityClearsOnApproval` timing out waiting for
`fabrik:awaiting-review` alongside a 5½-minute processing gap in the bed log)
is explicitly unconfirmed — its sibling (at the time of this run,
`TestReviewAuthorityBlocksAndPausesOnChangesRequested`; renamed to
`TestReviewAuthorityReinvokesOnChangesRequested` by #1375), same helper, same
assertion, passed in the same leg. Lowering `E2E_PARALLEL`
without stronger evidence would itself be an unmeasured, guessed change, and
risks masking real `t.Parallel()` interleaving defects for no demonstrated
benefit. Instead, the risk this requirement is aimed at — a scenario failing
purely because it was bed-starved — is addressed by the timeout increase
above: a starved scenario now has enough wall-clock room to actually finish
rather than racing a too-tight deadline. See `E2E_PARALLEL=2` /
`E2E_PARALLEL=6` above for the documented escape hatches if you observe a
repeated starvation pattern in practice.

**Update (#1527): `E2E_PARALLEL` itself is still kept at 4** — the analysis
above still holds — but the "on" leg specifically now gets a tighter,
independent cap (`E2E_PARALLEL_ON`, default 2) for a different reason than
oversubscription: it's the GraphQL cost of the ADR-1270/ADR-1208 settle-scan
pattern, not bed contention. See "The two-mode gate's 'on' leg: a separate,
larger cost driver (#1527)" above.

#### Hang hardening (#1676): `E2E_GH_API_TIMEOUT`, `E2E_POST_SUITE_WATCHDOG`, `E2E_STALL_WARN_MINUTES`

The v0.0.81 cut hung for **17h19m** — with its log untouched for the last
~19 of them, and the `e2e.test` binary long gone — because the two `gh api
rate_limit` budget-probe calls in the (then bash) leg executor (`budget_before`/
`budget_after`) had no timeout at all: `|| echo ""` only guards a *failing*
call, not a *hanging* one. The gate runner now guards against a repeat at three
independent layers (see `tests/gate/leg.go` and `tests/gate/exec.go`; since #1994
these are contexts and `internal/sessionreap` rather than bash job control —
ADR-1994 maps each mechanism):

- **`E2E_GH_API_TIMEOUT`** (default `30`, seconds) — every ancillary network
  call (currently: the two GraphQL budget probes) runs through the runner's
  `Commander` with a `Timeout` and is killed — its whole session — if it exceeds
  this. A caught hang prints `with_timeout: command exceeded Ns, killed: …` as a
  warning, so it is distinguishable from an ordinary `gh` error. These are lightweight REST metadata calls (see "GraphQL budget
  exhaustion detection" above), so 30s is generous, not tight.
- **`E2E_POST_SUITE_WATCHDOG`** (default `300`, seconds) — a background
  watchdog, armed the instant `go test` exits, aborts the run loudly
  (a distinct exit code, `ExitPostSuiteWatchdog = 6`) with a diagnostic
  naming the stuck step if the leg executor's own post-suite bookkeeping
  (the budget probe, timing/outcome reports, the backoff scan — normally
  seconds, not minutes) hasn't finished within this window. This is the
  exact failure mode from the v0.0.81 incident: `go test` long gone, no
  watchdog to say so.
- **`E2E_STALL_WARN_MINUTES`** (default `15`, minutes) — independently of
  the watchdog above, warns (never aborts) if the suite's own combined
  output has gone quiet for this long *while `go test` is still running*,
  naming the last completed scenario. Purely advisory: a real scenario can
  legitimately wait on Claude for extended periods (see the `E2E_TIMEOUT`
  derivation above), so silence alone is never treated as a hang — it's
  only surfaced, so it's never mistaken for progress either.

**Defaults kept as proposed, not further tuned.** Nothing in the
post-`go test` tail this watchdog covers (a couple of GraphQL calls plus
parsing the JSON log) plausibly approaches minutes, so `300s` is a
generous backstop relative to normal completion time. For the stall
detector, the one measured real-world inter-event gap already on record in
this file — **5½ minutes**, `TestReviewAuthorityClearsOnApproval`, in a
multi-scenario "on" leg (see "How the timeout/parallelism defaults are
derived" above) — sits comfortably under the 15-minute default, so nothing
found during Research/Plan indicted the issue's own starting-point values.

**Expected warning on the isolated `TestMergeTrainRunawayGuardPausesBatch`
leg.** This scenario runs alone (see `TrainIsolatedRE` in `tests/gate/config.go`),
deliberately queuing poison members until a 1-hour-windowed runaway guard
fires, with no other parallel scenario keeping the combined output stream
busy in the meantime. The stall detector is expected to warn during this
leg on every healthy run — that is not a regression, and the warning text
is worded to read as informational rather than alarming.

Composition with `E2E_GH_API_TIMEOUT`: a single hung `gh api` call is
bounded well within the post-suite watchdog's own window (30s default vs.
300s default), so in the common case the watchdog should never actually
fire — `E2E_GH_API_TIMEOUT` already removes the only two calls known to
cause the original hang. The watchdog exists as a backstop against a
*future* regression (a call added to that tail without a bound), not the
expected path. Any new network call goes through `Commander` with a `Timeout`.

**Wedged output pipe (#1694).** If something that outlived `go test` (the
detached bed was the real case) inherited the suite's output pipe, the runner
stops waiting for EOF after `E2E_POST_SUITE_DRAIN_TIMEOUT` (default `30`
seconds), prints a warning, and carries on to the remaining legs — the JSON log
is already complete. (The bash runner's named-pipe `tee | jq` consumer is gone;
the runner copies the pipe itself.)

#### Timeout & failure reporting

On a non-zero exit, `run.sh` classifies every top-level test by the last
action it emitted in the `go test -json` stream, and prints a labeled report
before failing:

```
== suite FAILED (leg: off, exit 1) — classifying test outcomes ==
JSON log: /tmp/fabrik-e2e-off-12345.json
completed - pass (11): TestBaseBranchPipeline, TestBlockedOnInput, ...
completed - fail (0):
completed - skip (2): TestMergeTrainHappyPathLanding, TestMergeTrainRestartSafety
still running at kill time (2): TestCIFixReinvokeCycleLimit, TestConjunctiveCIReviewGate
never started - queued behind -parallel cap (2): TestConvergenceRace, TestCruiseFullPipeline
```

This distinguishes three states a bare `FAIL` can't: **completed** (actually
ran to pass/fail/skip), **still running at kill time** (executing when the
process died — the only state Go's own built-in `-timeout` panic dump
reports), and **never started** (parked waiting for a free `-parallel` slot,
which the built-in panic dump omits entirely). The full JSON log is kept for
follow-up debugging at the path printed above.

#### Per-test wall-clock summary (#1355)

Unlike the failure classification above, the timing report runs
unconditionally — pass or fail — right after each leg's suite invocation
finishes, so "which scenarios cost the most" is a measured number from every
gate run rather than a guess:

```
== per-test wall-clock (leg: off), slowest first ==
2312.4s  pass  TestConvergenceRace
1382.1s  pass  TestPausedMergedPRRecovery
612.0s   pass  TestNoWorkNeeded
58.3s    pass  TestSmokeSingleRepoDispatch
0s       skip  TestMergeTrainHappyPathLanding
```

Elapsed is Go's own per-test `Elapsed` field from the `go test -json` stream
(only meaningful on a test's terminal pass/fail/skip event), sorted
descending; subtests are folded into their parent, same as the failure
classification above. Printed once per leg — a combined cross-leg table
isn't possible without changing the per-leg `jsonlog` scoping (the log is named
by auth and train mode, so the two "on" sub-legs overwrite each other; the
per-cell archive is #1972's).

**Verification status:** the report code is proven against a recorded
`go test -json` stream whose golden outputs were generated by the original
`jq | column -t` pipeline (multiple tests, a subtest, all three terminal actions,
non-JSON lines, sorted correctly) — see `tests/gate/events_test.go`. A full smoke test
of the integrated `scripts/e2e/run.sh` output (AC4) against a live gate run
was not performed for the same reason as `TestNoWorkNeeded`'s live-bed
verification above: doing so touches `~/dev/fabrik-test`, outside any
automated Fabrik stage's worktree sandbox. Run `scripts/e2e/run.sh -run
TestSmokeSingleRepoDispatch` manually to confirm the table prints in
context.

#### Teardown on kill

A run killed by `E2E_TIMEOUT` (or an external signal) skips every in-flight
scenario's `t.Cleanup` — this is a hard Go-runtime constraint (the timeout
panic fires from a separate timer goroutine that crashes the whole process
before any test goroutine's deferred cleanup runs; an external signal doesn't
invoke Go-level defers at all), not something fixable in the test code.

When `run.sh` detects Go's own timeout-panic text in the JSON log (i.e. this
was specifically an `E2E_TIMEOUT` kill, not a normal scenario failure), it
automatically runs `scripts/e2e/reset.sh` (the plain form) as best-effort
teardown — closing stray PRs/issues, deleting leftover `fabrik/*` branches,
and draining the board, so the next run starts no dirtier than after a
completed one. A normal scenario `FAIL` does **not** trigger this — an
operator debugging a real regression needs the board/issue state left
intact.

This runs immediately and unattended, without giving an operator a chance to
inspect the stranded PRs/issues first — worth knowing if you're debugging why
a scenario hung rather than just re-running the gate. In practice, the two
most useful artifacts for that survive teardown anyway: the classification
report above (printed *before* teardown runs) already names the exact
still-running/never-started tests, and `close_open_issues_in`/
`close_open_prs_in` **close** rather than delete — their full comment
history, labels, and timeline stay inspectable afterward via
`gh issue view --repo <alpha|beta> <n>` / `gh pr view --repo <alpha|beta> <n>`
even after this teardown runs. What does *not* survive: `close_open_prs_in`
deletes each PR's head branch, and `drain_board` deletes (not just moves)
every project-board item, so board-column position at kill time is lost
unless you happened to capture it live.

**Worktrees are the one exception — they are not auto-cleaned.** The only
worktree-cleanup path (`scripts/e2e/reset.sh --worktrees`) nukes *all*
worktrees and bare clones bed-wide and requires stopping the bed first; it
cannot be scoped to just the interrupted run's artifacts, and running a
destructive full-bed operation automatically from a kill-detection path would
risk firing against a bed that isn't actually safe to stop at that moment.
If a run was killed by `E2E_TIMEOUT`, run this manually before the next
release-gate run if you need full parity with a completed run:

```bash
# stop the test-bed Fabrik instance first, then:
scripts/e2e/reset.sh --worktrees
```

### Reset between runs

**Run this as part of test prep** — before a clean suite, so the bed starts from a
known-empty state. Stale closed issues linger as **project-board items** and leftover
`fabrik/*` branches otherwise pollute the next run's merge-train snapshots and make
results hard to read. `run.sh` also runs the plain form of this automatically on a
detected `E2E_TIMEOUT` kill — see "Teardown on kill" above; a manual run is still
needed after such a kill if you want worktrees cleaned up too.

```bash
scripts/e2e/reset.sh             # full clean: PRs + issues + branches + board items (alpha + beta)
scripts/e2e/reset.sh --worktrees # ALSO wipes Fabrik's worktrees + bare clones (destructive)
```

The plain form resets to a clean slate: closes open PRs (deleting their branches),
closes open issues, deletes leftover `fabrik/*` branches, and **removes every item
from the "Fabrik Test" project board** (board items survive an issue close, so this
is what an earlier issues-only reset missed). Overridable via `FABRIK_TEST_PROJECT_OWNER`
/ `FABRIK_TEST_PROJECT_NUMBER` (default `handarbeit` / `2`).

The `--worktrees` form is for when the test bed itself is wedged — stop Fabrik first,
it will refuse otherwise.

> Do **not** run reset while a suite is in flight — it will drain the board out from
> under the running tests.

## Coverage ledger, `--resume`, and suspending a run (#1972)

The gate records what it has proven, so partial runs add up to a complete gate
(ADR-1972). The ledger is **git-ignored** and lives at `.e2e-coverage/` in the repo root
(override with `E2E_COVERAGE_DIR`; point a clean checkout at an operator's directory to
share it). It is local to one machine and never committed.

```
.e2e-coverage/<full engine sha>/
  outcomes/<auth>-<train>.jsonl   append-only; one line per recorded test outcome
  invocations.jsonl               one line per gate invocation that started a leg
  archive/<cell>/<invocation>/    the bulky per-leg logs (below)
.e2e-coverage/pregate/<head>.json pre-gate pass for a clean checkout HEAD
.e2e-coverage/pregate/<head>.retries.jsonl  pre-gate TSan-crash retries (#1973)
```

**What counts as covered.** A (test, leg) pair — the leg is `<auth>/<train>`, e.g.
`app/off` — is covered when its latest recorded outcome is a **PASS against the test's
current source hash**. The set of required pairs comes from the plan the gate itself
builds (every live test × every leg), never from a hard-coded matrix, and ignores any
`-run` you pass: a `-run` narrows what one invocation runs, not what the gate needs.
Only live scenario tests are entries (a top-level `Test*` calling `LoadEnv`, per the
registry); subtests roll up into their parent.

**Outcomes are recorded as they stream in**, not at leg end, each fsynced. A test with no
terminal `pass`/`fail`/`skip` event is never recorded as a PASS.

**When coverage is reopened.**

| Change since the PASS was recorded | Effect |
|---|---|
| Only `tests/e2e/`, `scripts/e2e/`, `tests/gate/` differ from the engine SHA | The ledger stays valid. |
| Anything else differs (engine code, docs, …) | A different engine SHA, a new and empty ledger. The check prints which paths. |
| A test's source changed | Only that test loses its PASS. The hash covers the test function, its whole file, and every package-level declaration in `tests/e2e` it transitively references (so a `harness.go` helper edit reopens its dependents — extra reruns, never a false certification). Not hashed: non-Go inputs. |
| The leg ended in the RUN INVALID backoff banner (exit 3) | Everything that cell recorded in that invocation is discarded. |
| The test's last outcome was FAIL, INCONCLUSIVE (#1973, below), or a skip the ledger does not accept | Uncovered. |

**Skips.** A test that skips citing an issue (`#N`) is a **known skip** — listed
separately, not counted as covered, not blocking — **only while that issue (any one of
them, in `E2E_ISSUE_REPO`, default `handarbeit/fabrik`) is open.** Cite no issue, cite only
closed ones, or run where `gh` cannot read the issue, and the skip counts as **missing**.
Skips that are structural — merge-train scenarios under train `off`, `TestSwitchTrainMode`
outside its restart step — are declared in the registry (`skip_ok_legs`, e.g. `["*/off"]`)
and reported as **structural skips**: listed, not blocking, not counted as covered. If a
first real run shows a skip the registry does not explain, the summary prints its message;
the fix is a one-line registry edit.

**`--resume`.** `scripts/e2e/run.sh --resume` (with `--clean` in either order; both must
lead the arguments) runs, per cell, only the tests with no valid PASS for that SHA and leg,
as one anchored `-run '^(A|B|…)$'`; skips legs already fully covered (including their bed
restart); runs the pre-gate once per clean HEAD; and exits **8** if every leg it ran passed
but required coverage is still incomplete. A caller `-run` intersects with the resume set
and never credits a test it did not run; a subtest filter (`-run 'TestX/case'`) is refused,
since it exercises only part of a test. Without `--resume` the gate runs everything as before
(and still records, and prints the summary — exit codes unchanged).

**Reading the summary.** Printed at the end of every run, passing, failing or killed:

```
== live coverage for engine SHA 1a2b3c4 ==
ledger drift check: engine SHA 1a2b3c4 — VALID (3 test/gate-only path(s) differ; none touch the engine)
  app/off  covered 40/44  missing 3  known-skip 1  structural-skip 0  inconclusive 0
    missing          TestX: no record
    known-skip       TestY: blocked on open #123 — "blocked on #123"
== coverage INCOMPLETE: … ; 2 invocation(s) ==
```

`scripts/e2e/run.sh coverage [--sha S] [--format notes]` prints the same summary read-only
(exit 0 complete, 8 not) — what `cut-release.sh` calls. It never touches the bed.

**Per-leg log archive.** Nothing a leg produces is overwritten by the next. Under
`archive/<cell>/<invocation>/`: `go-test.json` (the `go test -json` stream), `fabrik.log.<n>`
(the bed **engine** log, one file per engine run), `bed-run.log`, `preflight.txt`, `load.json`
(the host's 1-minute load average at leg start and end) and `bed-config.sha256` (a hash of the
bed's `.fabrik/stages/` and `config.yaml`; the runner warns when it differs between invocations
of one ledger). The log that used to vanish is the engine's `.fabrik/fabrik.log`, which the
engine truncates on every start — so it is sampled while the leg runs and a new segment starts
at each restart. Archives are pruned by SHA: the newest `E2E_COVERAGE_KEEP_SHAS` (default 5)
keep their `archive/`; the outcome records are never pruned.

### Suspending and resuming a run

Stopping a gate mid-leg is safe, and is meant to be done — for instance while GitHub is
degraded.

- **Stop it** with Ctrl-C, or `kill -TERM <pid of the gate runner>` (`pgrep -f 'gate run'`).
  The runner reaps its whole process tree — `go test`, the scenarios' `gh`/`git` children, and
  any command sessions they started — so nothing is left running under the suite. Scope the
  signal to **that** process: do not `pkill go` or `pkill fabrik`.
- **The bed** (`~/dev/fabrik-test`'s engine) is deliberately a detached process that outlives
  the gate. Leave it running if you will resume soon, or stop it with the bed's own lock
  (`.fabrik/fabrik.lock`) → `kill -TERM`. A `--resume` re-runs preflight and restarts it either way.
- **What is lost:** only the tests in flight at that moment (they had no terminal event, so
  nothing was recorded for them). Everything that had already passed stays recorded.
- **Resume** with `scripts/e2e/run.sh --resume` (add `--clean` to reset the bed first). It runs
  only what is still missing. Check where you are any time with `scripts/e2e/run.sh coverage`.
- **A scenario's worktrees, branches and PRs** from the interrupted leg are cleaned by `--clean`
  (and by the reset in "Reset between runs").

## The INCONCLUSIVE outcome and bounded retry (#1973)

A live test has three terminal outcomes the gate cares about, plus an ordinary skip:

| Outcome | How a test gets there | Coverage |
|---|---|---|
| PASS | the test passes | covered |
| FAIL | any assertion about what the engine did (`t.Fatalf`/`t.Errorf`) | uncovered; **never retried** |
| SKIP | an env-gated or known-issue `t.Skip` | known / structural / missing, per "Skips" above |
| **INCONCLUSIVE** | `Inconclusive(t, "reason")` — *the precondition this scenario needs never arose* | **uncovered, not failed**; retried, then re-run by `--resume` |

**The marker contract.** `Inconclusive(t, format, args...)` (`tests/e2e/harness.go`) ends the
test with `t.Skip("E2E-INCONCLUSIVE: <reason>")`. The string lives in the untagged package
`tests/e2e/inconclusive` (the gate runner cannot import the `e2e`-tagged package), whose
`IsMarked` matches only a skip message that **starts** with the marker — a log line that merely
mentions it, or an ordinary skip quoting it, is not a declaration. It must be called from a
**top-level** test's own goroutine (a subtest's output is invisible to the runner, so a call
from `t.Run` is a `t.Fatalf`).

**Use it only for a guard that fires before any assertion about engine behaviour**, when the
harness failed to produce the state the scenario needs (a poll boundary straddled a release, a
cold-cache line never appeared, a bed reviewer ejected a member). An assertion about what the
engine did stays a failure: retrying a failure would mask a regression. Where the awaited line
is itself the engine behaviour under test, keep `waitForLogMatch` (timeout = `t.Fatalf`); a
guard whose timeout means "the setup never happened" uses `waitForLogMatchInconclusive`.
Cleanups that key on failure (`repauseOnFailure`) also run for an inconclusive skip, so a retry
never inherits un-paused Queued members.

**What the gate does.** At the end of each leg it re-runs **only that leg's inconclusive tests**
(one `-run '^(A|B)$'` invocation per attempt, same cell: same auth/train mode, `-parallel`, bed,
no restart; each attempt in its own log, `go-test.retry-N.json`), at most
`E2E_INCONCLUSIVE_RETRIES` times (default **2**, `0` disables). Retries run before the post-suite
watchdog starts and before the RUN INVALID scan, so a long retry is not killed and throttling
during one still voids the cell. No retry is attempted after a timeout kill, for a
subtest-filtered cell, or once the engine's rate-limit backoff has engaged. A test that passes
on retry is covered; one that fails on retry is a FAIL; one still inconclusive after the last
retry is recorded `INCONCLUSIVE` in the ledger (uncovered — the coverage summary's
`inconclusive` column) and re-run by `--resume`.

**Visible rate.** Each leg prints, whenever any test ended inconclusive:

```
== inconclusive (leg: app/on): 2 on the first attempt: TestA, TestB ==
   after 2 retry attempt(s): passed on retry: TestA; failed on retry: none; still inconclusive (UNCOVERED, not failed): TestB
```

— so a leg that is quietly flaky is never invisible, even when every retry passed. When the
first-attempt count exceeds `E2E_INCONCLUSIVE_WARN` (default **3**) a warning points at #1974
(the underlying harness races).

**An uncovered leg never reads as success.** If an invocation otherwise ends cleanly but tests
stayed inconclusive, it exits **8** (`ExitCoverageIncomplete`), with or without `--resume` and
whether or not the ledger is on, and names them. A real failure keeps its own exit code.
`scripts/cut-release.sh` already maps 8 and re-checks coverage after its resume run, so a
release can never be cut on INCONCLUSIVE; the 3/4/5/6/7 contract is unchanged.

**Pre-gate crash retry (R5).** Separately, when a pre-gate step (the sim suite or the github
wire-contract tests) fails **only** with the known TSan fork/exec crash signature (#1624/#1677:
a TSan `CHECK failed … tsan_*.cpp` abort, or a child `git` killed by `signal: segmentation
fault`, with no Go panic, `fatal error:`, `WARNING: DATA RACE` or goroutine dump anywhere in the
output) that step is re-run **once**. The retry is recorded with the host load average in
`.e2e-coverage/pregate/<head>.retries.jsonl` (also when it fails again) and, on a pass, in
`pregate/<head>.json` and the coverage summary's pre-gate line. A second crash or any other
failure is the same hard stop (exit 5) as before.

### Audit of the "harness race / vacuous / re-run" guards (#1973 R2)

| Guard | Disposition |
|---|---|
| `mergetrain_batchcap_test.go` — "poll boundary straddled the unpause" | **Converted.** Fires before A1's selection assertion. Its two sibling `switch` cases (cap mismatch; no cap line with ≥ members) are bed-configuration errors and stay `Fatalf`. |
| `mergetrain_batchcap_test.go` — A2 "ejected for reviewer feedback" | **Converted.** Bed-reviewer artifact; fires before any A2 assertion. |
| `mergetrain_coldbase_test.go` — cold-cache "not yet hydrated" never appeared ("vacuous") | **Converted** (timeout only, via `waitForLogMatchInconclusive`). #1974 fixed the *cause*: the seed now awaits the board listing showing every member (and the primer) before the bed starts — see "The awaitVisible family". |
| `mergetrain_coldbase_test.go` — landed via the singleton fast path | **Converted.** Members did not batch together; fires before the A1/A2 assertions it protects. |
| `comment_landing_gate_test.go` — landing decision never reached with the comment pending | **Converted** (timeout only). `assertHeld` and every later check stay `Fatalf`. #1974: the `mergeable` wait before placing the item is `AwaitPRMergeableSettled`. |
| `awaitVisible` timeouts in every seed path (#1974) | **Converted** — a harness write that never became visible is "the precondition never arose". `dirty`/`behind` mergeability stays `Fatalf` (a scenario assertion, not lag). See "The awaitVisible family". |
| `comment_queued_eject_test.go` — "occupant window closed" | **Converted.** Precondition (M2 not owned by a live batch) of the direct-route assertion. |
| `mergetrain_bisect_test.go` — release straddled a poll boundary | **Converted.** Same class as batchcap. |
| `mid_stage_label_test.go` — Research judged "no work needed" | **Converted.** Claude non-determinism; fires before any rework-marker assertion. |
| `mid_stage_label_test.go` / `label_events.go` / `checkrun_timing.go` — existing `INCONCLUSIVE:` fatals | **Migrated** at the call sites (`failOrInconclusive`/`Inconclusive`); the pure checkers keep their `INCONCLUSIVE` error prefix. Their contract changes from "fails" to "uncovered, retried". |
| `conjunctive_ci_review_gate_test.go` — slow-gate window already consumed / too short | **Converted.** |
| `conjunctive_ci_review_gate_test.go` — `stage:Validate:complete` inside the R1 window though slow-gate had completed | **Left `Fatalf`.** Fires after an engine behaviour was observed. The sibling "no slow-gate check run on PR head" is a bed-config error: left. |
| `mergetrain_batchcap_test.go`, `comment_queued_eject_test.go` — stale Queued items, "clear the Queued column and re-run" | **Left `Fatalf`.** Bed state an operator must clear; a retry hits the same state. |
| `mergetrain_conflict_test.go` — partial batch, or stale Queued items | **Left `Fatalf`.** The two causes are indistinguishable and a retry clears only one (#1974). |
| `comment_queued_eject_test.go` — occupant took the singleton fast path | **Left `Fatalf`.** Not in the race class; a fast-path change is an engine-side signal. |
| `convergence_race_test.go` — "setup failed … not an engine regression, re-run" | **Left `Fatalf`.** A setup *error*, not a precondition that never arose; could be a permanent harness bug a retry would turn into "uncovered". |
| `mergetrain_helpers.go` — landed-comment post failed transiently (#1275) | **Left `Fatalf`.** A known engine defect; retrying would mask it. |

`tests/e2e/inconclusive/guards_test.go` pins, statically (it parses the tagged sources), that the
three named guards use an inconclusive helper, that each still has assertion `Fatalf`s after it,
and that `Inconclusive` is never called from a goroutine or a subtest.

## The awaitVisible family (#1974, ADR-1974)

GitHub is eventually consistent, and the engine reads through paths that lag independently of the
harness's writes. A harness that writes (adds a board item, opens a PR with `Closes #N`, moves a
Status) and carries on as if every reader can already see it races the engine: the scenario either
never reaches the state it tests (a vacuous pass — 0.0.83's `TestMergeTrainColdCacheBaseMember`,
whose bootstrap board fetch missed two members the listing had not yet shown) or fails for a reason
that is not the engine's (`TestCommentLandingGateHolds`, `mergeable` still `null`).

`tests/e2e/await_visible.go` is the one family of helpers that block until a write is visible
**through the surface the engine itself consults**, within an explicit per-call timeout. They all
share one polling loop, `awaitvisible.Poll` (`tests/e2e/awaitvisible`, untagged so its unit tests run
on every PR; CI only *compiles* this tagged package).

| Helper | Waits for | Read path |
|---|---|---|
| `AwaitBoardItemVisible` / `AwaitStatusVisible` | the issue in the ProjectV2 item listing (with a given Status) | `projectV2.items(first:100)` GraphQL — what the engine's bootstrap fetch lists |
| `AwaitClosingLinkage` | `Closes #N` visible on **both** sides | `issue.closedByPullRequestsReferences` (the engine's read) and `pullRequest.closingIssuesReferences`, one GraphQL call. Default-base PRs only: GitHub makes no link for a non-default base |
| `AwaitPRMergeableComputed` | `mergeable` computed (`mergeable_state` ≠ `unknown`) | REST `/pulls/N` |
| `AwaitPRMergeableSettled` | computed **and** `clean`/`unstable` (#1982's verdict, layered on the primitive: `blocked`/`unknown` keep waiting, `dirty`/`behind` fail fast) | REST `/pulls/N` |
| `AwaitLabelVisible` | a label the **harness** applied | REST issue read |
| `AwaitPRForBranchVisible` | the harness-opened PR on `fabrik/issue-N` | REST `/pulls?head=owner:branch`, as the engine's `FetchLinkedPR` |

**Classification rule.** Waiting for a *harness write* to become visible is lag: a timeout ends the
test **Inconclusive** (#1973 — uncovered, retried, never a PASS, never a FAIL), and the reason names
what was waited for, for how long, how many reads, and the last observation or read error. Waiting for
the *engine* to do something (`WaitForProjectStatus` after the engine moves an item,
`WaitForIssueLabel` for an engine-applied label, `LinkedPRNumber` for an engine-created PR,
`WaitForCheckConclusion`, the `waitForLogMatch*` family) is an assertion about engine behaviour and
stays `t.Fatalf` — retried into green it would mask a regression. A state that will not resolve by
waiting (`mergeable_state` `dirty`/`behind`) is a scenario assertion and also stays `Fatalf`.

A *read error* (a transient `gh` failure) is retried and logged, never counted as "not visible".
Timeouts are explicit per call; the seed helpers use `awaitSeedTimeout` (10 min — the 2026-09-30
outage stretched listing lag past five minutes). GraphQL waits poll every 10 s (the budget is shared
with the bed engine, #1695); REST waits every 5 s.

**Seed paths.** `createMemberPR` (so `QueueMember*`, `PrepareMemberExactPath`, `seedReviewGateItemImpl`
and `seedLandingCandidate` reach it) awaits the closing linkage; the seeders await the harness-opened PR,
the labels they apply and the board item/Status they place. `waitForClosingLinkage` and
`WaitForPRMergeableSettled` are gone — `inconclusive/guards_test.go` pins that they stay gone and that
no seed path grows a loop or sleep of its own.

**Seed in the top-level test.** `Inconclusive` must end a *top-level* test from its own goroutine;
called from a subtest (or a goroutine) it is a loud `t.Fatalf`. A seed helper called inside `t.Run`
(`auto_merge_test.go`, `convergence_race_test.go`) therefore turns an await timeout into a failure with
a clear message rather than an invisible marker. Seed at the top level where you can.

`TestMergeTrainQueuedDeeperThanBatchCap` is a poll-boundary race, not a consistency race: it stays
Inconclusive here and is #1978's.

The wrapper layer is tested against an `httptest` fake GitHub through a fake `gh` on `PATH`
(`await_visible_test.go`, `-tags e2e`, local only); the loop and the pure classifiers are tested in the
untagged package on every PR. The neutralised-wait check — with an await stubbed out, ColdCache and
CommentLanding reproduce their old vacuous outcome, now reported Inconclusive — needs a live bed and is
recorded manually in the PR.

### Environment probes (#1974)

Before any live budget is spent the gate runner probes two environmental causes of the 0.0.83 flakes.
Both wait, re-probe and report; neither ever kills anything. See `tests/gate/README.md`.

| Knob | Meaning | Default |
|---|---|---|
| `E2E_LAG_PROBE_THRESHOLD` | seconds a freshly added board item may take to appear in the listing | 30 |
| `E2E_LOAD_PROBE_FACTOR` | 1-minute load threshold, as a multiple of the CPU count | 2 |
| `E2E_LOAD_PROBE_THRESHOLD` | absolute 1-minute load threshold (overrides the factor) | unset |
| `E2E_PROBE_WAIT_MAX` | seconds each probe may spend waiting and re-probing before preflight fails (exit 4) | 600 |
| `E2E_PROBE_INTERVAL` | seconds between re-probes | 30 |
| `E2E_SKIP_PROBES` | skip both probes (recorded as skipped) | off |

## Scenarios

"Mode" records each scenario's classification from the #1217 mode audit (FR-2/FR-3/FR-4):
**Both** — mode-invariant, single assertion set, passes under both `merge_train`
settings unmodified. **Both (mode-aware)** — genuinely differs by mode; the
scenario branches internally (via `resolveTrainMode`) and asserts the
mode-appropriate contract in each. **Train-only (on)** — exercises the merge
train directly; skips cleanly (`requireTrainBed`) under mode `"off"` or when
the `Queued` column is absent, so it only runs in the gate's `on` leg.

| Test | What it verifies | Mode | Approx wall-clock | Cost |
|---|---|---|---|---|
| `TestSmokeSingleRepoDispatch` | Worker dispatches on a trivial issue; Specify completes | Both | 3–5 min | $0.10–0.20 |
| `TestSmokeSingleRepoFullPipeline` | Full single-repo pipeline (Specify → … → Done with merged PR) | Both | 20–40 min | $0.50–1.50 |
| `TestNoWorkNeeded` | `FABRIK_NO_WORK_NEEDED` short-circuit closes issue without PR | Both | 10–15 min | $0.30–0.50 |
| `TestBlockedOnInput` | `FABRIK_BLOCKED_ON_INPUT` pause + comment-driven resume | Both | 10–15 min | $0.30–0.50 |
| `TestCrossRepoSpawn` | Cross-repo decomposition (spawn child in beta, gate parent, resume on close); final assertion reads back a per-run-unique sentinel string from the merged beta-side file, not merely the `fabrik:children-spawned` label | Both | 45–60 min | $1.00–2.00 |
| `TestYoloAutoMergeLabel` | `fabrik:yolo` auto-advance to Done; mode-appropriate landing contract (native auto-merge + `fabrik:auto-merge-enabled` under "off"; train close-not-merge + label never applied under "on") | Both (mode-aware) | 20–40 min | $0.50–1.50 |
| `TestConvergenceRace` | Deterministic post-Validate auto-merge race (#829): two conflicting yolo PRs; mode-appropriate `fabrik:auto-merge-enabled` contract, both land within budget, neither ends `fabrik:paused` | Both (mode-aware) | 80–100 min | $2–4 |
| `TestCruiseFullPipeline` | `fabrik:cruise` auto-advances to Validate-complete without auto-merge; PR merged by human closes issue | Both | 30–50 min | $0.80–2.00 |
| `TestBaseBranchPipeline` | `base:<branch>` non-default base branch: throwaway branch created off main, PR targets it (not main), pipeline does not falsely pause at end of Implement, review gate clears via the base-independent REST feed | Both | 35–55 min | $0.80–2.00 |
| `TestCIFixReinvoke` | CI-fix reinvoke positive path: the first CI run is forced red by a run-ID ack nonce the agent cannot pre-empt, the engine dispatches a reinvoke whose prompt carries the failure (asserted via the engine log), Claude fixes, CI passes, issue closes | Both | 75–90 min | $1.00–3.00 |
| `TestCIFixReinvokeCycleLimit` | CI-fix reinvoke negative path: unfixable sentinel exhausts MaxCiFixCycles, issue pauses | Both | 30–60 min | $0.50–1.50 |
| `TestPausedMergedPRRecovery` | paused + gate-label at Validate with merged PR heals to CLOSED (3 sequential sub-tests: awaiting-ci, awaiting-review, no-gate-label); regression guard for #874 class | Both | 60–90 min (3 sequential sub-tests, ~20–30 min each); covered by the default `E2E_TIMEOUT=4h` | $1.50–4.50 |
| `TestConjunctiveCIReviewGate` | Conjunctive CI∧review gate: fabrik:awaiting-ci holds before CI, PR comment during CI-await not dropped, fabrik:awaiting-review holds before approval, advance suppressed until both gates clear | Both | 80–115 min (approval path) / 50–75 min (timeout path) | $1.00–2.50 |
| `TestReviewAuthorityReinvokesOnChangesRequested` | ADR-1250/ADR-1375 authoritative mode (via `review-authority:authoritative` label): CHANGES_REQUESTED verdict blocks checkReviewGate, but a bounded reinvoke fires immediately (AC1/AC6, engine log assertion, not a label transition) — the body-only review shape SubmitPRReview produces is enough with zero inline comments; the same review is not re-dispatched on a later poll (AC7) | Both | several min (one real Claude invocation) + 90s settle window | $0.10–0.50 (one Claude invocation) |
| `TestReviewAuthorityCycleLimitPauses` | ADR-1375 R5 terminal fallback (via `review-authority:authoritative` label): repeated distinct CHANGES_REQUESTED reviews up to `FABRIK_MAX_REVIEW_CYCLES` (bed-configured small) terminate in `pauseForReviewCycleLimit`, not an unbounded reinvoke loop (AC4) | Both | ~`FABRIK_MAX_REVIEW_CYCLES` × several min (one Claude invocation per cycle) | $0.20–1.00 (`FABRIK_MAX_REVIEW_CYCLES` Claude invocations) |
| `TestReviewAuthorityClearsOnApproval` | ADR-1250 authoritative mode (via `review-authority:authoritative` label, requires #1261): APPROVED verdict clears the gate; fabrik:paused never applied | Both | 2–5 min | ~$0.02 (no Claude) |
| `TestReviewAuthorityYoloDoesNotBypassBlock` | ADR-1250 composition guarantee (via `review-authority:authoritative` label, requires #1261): fabrik:yolo does not bypass an authoritative gate — blocked while CHANGES_REQUESTED stands, clears once approved | Both | 5–10 min | ~$0.03 (no Claude) |
| `TestReviewAuthorityAdvisoryRegressionGuard` | Regression guard: advisory (default) mode still clears on any submitted review regardless of verdict — proves the additive authoritative check didn't narrow the default path | Both | 2–5 min | ~$0.02 (no Claude) |
| `TestExpectedReviewersFastAdvance` | ADR-1283 `expected_reviewers: []` (via `expected-reviewers:none` label, requires follow-up engine issue): nothing requested/reviewed → gate fast-advances instead of waiting out the review timeout (the #1080 stall this feature fixes) | Both | 2–5 min | ~$0.02 (no Claude) |
| `TestExpectedReviewersDeclaredWaitsAndReprompts` | ADR-1283 `expected_reviewers: [<name>]` (via `expected-reviewers:declared` label, requires follow-up engine issue): declared-but-unrequested reviewer holds the gate open, Phase 1 re-prompt ladder fires with an @mention comment, Phase 2 pauses for human when no response arrives | Both | ~2×`FABRIK_REVIEW_WAIT_TIMEOUT` + buffer | ~$0.05 (no Claude) |
| `TestExpectedReviewersUndeclaredRegressionGuard` | Regression guard: undeclared (`nil`) `expected_reviewers` still never fast-advances — pins the `expected != nil` check and proves the shipped default (FR-5) is unchanged | Both | 2–5 min | ~$0.02 (no Claude) |
| `TestExpectedReviewersFastAdvanceComposesWithAuthoritative` | ADR-1283 composition guard (via `expected-reviewers:none` + `review-authority:authoritative` labels, requires follow-up engine issue + #1261): fast-advance still fires ahead of the authority-verdict branch, since it only activates once hasReviews is true | Both | 2–5 min | ~$0.02 (no Claude) |
| `TestReviewAuthorityDeclaredBotDoesNotDeferHumanEscalation` | ADR-1375 Finding 2/AC2 (via `expected-reviewers:declared` + `review-authority:authoritative` labels, human requested via `RequestPRReviewer`): a declared bot's re-prompt ladder must never defer an outstanding human's authoritative CHANGES_REQUESTED escalation — the reinvoke fires and `fabrik:bot-reprompted` never applies | Both | ~`FABRIK_REVIEW_WAIT_TIMEOUT` + ~15 min | $0.10–0.50 (one Claude invocation) |
| `TestLateCheckRunSuiteGate` | ADR-1822/#1829 suite-aware CI gate (yolo item taken to Validate, `wait_for_ci`): the gate must not clear while a `needs:`-gated late check run is outstanding. Asserts on GitHub timestamps — late run starts after the fast run completes (A1), `stage:Validate:complete` is applied only after the late run completes (A2), and the fast run finished after `fabrik:awaiting-ci` (A3, vacuity guard). Needs `late-check-suite-gate.yml` installed on Alpha (not required); skips if absent. Mode-invariant | Both | 20–35 min (incl. ~4 min sleep) | ~$0.10–0.50 (one Validate Claude invocation) + one CI cycle |
| `TestYoloRemovedMidValidateBlocksMerge` | ADR-1769/#1769 live re-read of autonomy labels (yolo item taken to Validate): `fabrik:yolo` is removed over REST the moment `stage:Validate:in_progress` appears; the PR must not merge — no `fabrik:auto-merge-enabled`, PR and issue stay OPEN, board Status stays `Validate` with `stage:Validate:complete` present. The mid-stage window is proven from the events log by event id (`in_progress` < yolo removal < `awaiting-ci`); a missed window ends the test INCONCLUSIVE (#1973: uncovered, retried by the gate, never green). On the bed's `wait_for_ci` Validate a pre-fix engine can also pass when the board cache had already caught up — the scenario guards the operator-trust property and the cache-lag window. Mode-invariant | Both | 15–30 min | ~$0.10–0.50 (one Validate Claude invocation) + one CI cycle |
| `TestCommentReentryShowsReworking` | ADR-1802/#1802 comment re-entry rework marker: after a real Research run parks the item, a human comment triggers re-entry; the events log (after the pre-comment event id) must show `fabrik:reworking:Research` labeled, `stage:Research:complete` unlabeled, `stage:Research:complete` re-labeled, then the marker unlabeled — in that order — and the item ends with `:complete` restored, marker gone, still at Research. Events-log ordering is the proof (the window is sub-second); a live poll is informational only. Mode-invariant | Both | 10–20 min | ~$0.20–0.60 (two Claude invocations) |
| `TestAppSelfRecognitionBotCommentNeverResumes` | #1877 A1 (guards #1754): a plain bot-authored comment never resumes a paused item; a human comment does (control). See "Additional prerequisites for `TestAppSelfRecognition*`" | App legs only (skips in `pat`) | ~6–10 min | ~$0.10–0.30 (one comment-processing invocation) |
| `TestAppSelfRecognitionBlockedCommentUpdatedInPlace` | #1877 A2 (guards #1754): a changed `blockedBy` set edits Fabrik's single blocked comment in place; body reflects the new set | App legs only (skips in `pat`) | ~12–25 min (dep-blocked cooldown) | none |
| `TestAppSelfRecognitionDurableReviewSuppression` | #1877 A3 (guards #1754): a bot-authored `review-ids-addressed` marker suppresses that review's redelivery; a non-self marker does not | App legs only (skips in `pat`) | ~8–15 min | ~$0.10–0.30 (one review-reinvoke) |
| `TestMergeTrainHappyPathLanding` | ADR-059 internal train: 3 clean Queued members → one integration PR → all advance Queued→Done, PRs closed, no O(N²) per-member retests | Train-only (on) | 13–28 min (incl. exactly-once settle wait, #1874) | low (no Claude) |
| `TestMergeTrainSingletonFastPathLandsExactlyOnce` | #1874 / ADR-1871: one clean member with green own CI, queued alone → landed by the **singleton fast path** (log `singleton fast path taken`, comment `Landed via singleton fast path PR #N.` citing its own PR) → exactly one landing comment and one issue close after a 3-poll settle wait. **Not parallel** (prerequisite #22a). Needs the `train-poison-guard` required check | Train-only (on) | 10–15 min (est.) | low (no Claude) |
| `TestMergeTrainBisectionEjectsPoisoner` | ADR-059 D4: red combined batch → halving bisection isolates the poison member → ejected → survivors land. Needs the `train-poison-guard` required check | Train-only (on) | 20–40 min | low–moderate |
| `TestMergeTrainConflictBisectPrefixRerere` | #1848: 4-member batch (A/B same-path conflict, clean C, poison P) → **real Claude** resolves B onto A (small turn count) → red trial → bisect ejects P → first half reuses the recorded prefix with no Claude → A, B, C land, P off Queued; rerere replay asserted only if main moves. Needs the `train-poison-guard` required check. **Not parallel** — the train batches every Queued item (prerequisite #22) | Train-only (on) | 45–80 min (est.) | 1 Claude invocation (~$0.05–0.30) + ~11 CI cycles |
| `TestMergeTrainRestartSafety` | ADR-059 D5 / #960: after a landing, a restart with the historical merged integration PR present does NOT stall the next batch (reconstruct proceeds fresh). **Not parallel** — restarts the bed | Train-only (on) | 28–53 min (incl. exactly-once settle wait, #1874) | low |
| `TestMergeTrainColdCacheBaseMember` | ADR-1772 / ADR-1773: two `base:<branch>` members are queued **while the bed is stopped**, then the bed is started, so the first poll of the fresh process sees them with a cold cache. Asserts they are excluded as "not yet hydrated" (fail-as-vacuous if not), then land on the declared base: every `fabrik/merge-train/*` PR carrying a member targets that branch (state=all, whole window), no batch forms under the bare default key, member files are absent from `main`, and #1773's `REFUSING to open/reuse` line never fires. Files a blocked Specify "primer" issue to register the repo's `WorktreeManager` after the restart. Skips on a webhook-enabled bed. Posts no comments. **Not parallel** — stops the bed | Train-only (on) | 15–30 min | low (no Claude) |
| `TestMergeTrainRunawayGuardPausesBatch` | ADR-059 D8 (#964/#965): persistently-red 4-member batch trips the runaway guard at cap=6, pauses all Queued members, no member reaches Done. Runs on RepoBeta for counter isolation. **Not parallel** — induces a repo-wide fault on RepoBeta that would collide with `TestCrossRepoSpawn`'s use of the same repo (#1395) | Train-only (on) | 10–20 min | low (no Claude) |
| `TestMergeTrainQueuedDeeperThanBatchCap` | ADR-1833 / #1850: 7 clean members Queued against `max_batch_size` 5 → first trial holds exactly the first five, membership stays stable (one snapshot line, no unmerged-closed trial PR), all seven land as 5 then 2. Members are queued paused then released together. **Not parallel** — shares the (RepoAlpha, main) partition | Train-only (on) | 33–63 min (incl. exactly-once settle wait, #1874) | low (no Claude) |
| `TestPauseLiftedOnlyByPostPauseHumanComment` | ADR-1813 / #1876: a human comment that predates a pause does not lift it (item parked off-board, commented, paused, then moved to Specify: refusal log line, labels hold ≥4 min, no 👀/🚀 on the old comment, exactly one `labeled` and no `unlabeled` `fabrik:paused` event); a post-pause human comment resumes it (👀→🚀 + `unlabeled` event) | Both | 10–15 min | $0.15–0.40 |
| `TestCommentLandingGateHolds` | #1862 landing gate (ADR-1862): a human comment posted while the item has no Status, then the item moved into Validate (yolo, green CI, reviewer APPROVE) — the `comment-gate` hold line appears, the item is not merged/Queued/`fabrik:auto-merge-enabled` while the comment has no 🚀, the comment gets 👀 then 🚀, and only then does the item land (`closed_at` >= 🚀). Holds under both train modes; the `advance` "skipping stage" line the issue text names fires only for non-Validate stages, so `comment-gate` is asserted in both. Needs `FABRIK_REVIEWER_TOKEN` and `slow-gate`; skips otherwise | Both | 20–35 min per mode and auth leg | ~$0.10–0.50 (one comment-review Claude invocation) + one CI wait |
| `TestQueuedMemberCommentEjection` | #1863 (ADR-1863): a Queued member receiving an unprocessed human comment is ejected — `🏭 **Fabrik merge-train — ejected (unprocessed comment)**` comment + `ejected for an unprocessed comment … (not paused, no ejection counted)` log line; never `fabrik:paused`, and no counted-ejection artifact (comment, `pausing after N ejections`, `ejected N time(s) — pausing`); the comment is then processed (👀 then 🚀) and the member re-queues and lands. An occupant member M1 holds a slow-gate trial so M2 is deterministically outside the batch. "Not counted" is asserted indirectly (the counter is in memory). **Not parallel** — shares the (RepoAlpha, main) partition. Needs `FABRIK_REVIEWER_TOKEN` and `slow-gate` | Train-only (on) | 35–60 min | ~$0.10–0.50 (one comment-review Claude invocation) + two trial CI cycles |
| `TestPostMergeCommentNotApplied` | #1862 post-merge guard: a human comment on an OPEN item whose PR already merged (member PR without a closing keyword, item parked at Implement, admin merge) gets `🏭 **Fabrik — comment not applied**` exactly once on the issue and on the PR, no 👀/🚀 on the comment, no `comments … processing` line, no `stage:Implement:in_progress`, branch tip unchanged. Fails loudly (never passes vacuously) if the item was already closed/moved before the comment | Both | 3–8 min | none (no Claude) |

**Exactly-once landing assertion (#1874, regression coverage for #1871 / ADR-1871).**
`AssertMembersLandedExactlyOnce` (`landed_once.go`) asserts each member the scenario
landed has **exactly one** landing comment on its own PR (all three engine forms are
counted: `Landed via batch PR #N.`, `Landed via singleton fast path PR #N.` and
`Landed one-at-a-time via singleton PR #N.`) and **exactly one `closed` and no
`reopened`** timeline event on its issue. It is called from
`TestMergeTrainHappyPathLanding`, `TestMergeTrainQueuedDeeperThanBatchCap`,
`TestMergeTrainRestartSafety`, the `train-mode=on` subtest of `TestYoloAutoMergeLabel`
and `TestMergeTrainSingletonFastPathLandsExactlyOnce`; later train scenarios can reuse
it. The comment count is the load-bearing check (closing an already-closed issue creates
no timeline event, so the close count mainly catches a reopen-and-re-close).
- **Settle wait:** it sleeps **3 × the bed poll interval** once per scenario before
  counting (180 s at the default `bedPollSeconds()` of 60; follows
  `E2E_BED_POLL_SECONDS`). The defect re-lands a member in the poll *after* the landing,
  so counting at landing time would pass vacuously; two polls is the minimum and the third
  absorbs jitter/backoff (the bed log has no per-poll line to count instead). That is
  **~3 min added to each wired scenario**, reflected in the wall-clock column above.
- **Retry duplicates:** `addLandedCommentWithRetry` can legitimately post an identical
  comment within ~1 s when a post succeeded but its response failed; matching comments
  within 10 s of each other count as one landing (logged, not failed).
- **Zero comments fails** too (the engine's landed comment is best-effort, #1275); the
  message points at the bed log's `could not post landed comment` warning when present.
- Counting is scoped to each scenario's own member PR/issue numbers (safe under
  `t.Parallel()`), by body never by author, and posts no comments — so it holds
  identically under PAT and GitHub App auth. Under `merge_train: off` the ordinary
  auto-merge path posts no landing comment, so there is no off-mode counterpart.
- The pure parsing/verdict half has bed-free unit tests:
  `go test -tags e2e -run 'LandedExactlyOnce|LandingComment|ParseIssueComments|CountLifecycle|CollapseRetry|LandingSettle' ./tests/e2e/`.

Approximate single-mode suite total: ~730 min wall-clock, $10.60–31 in Claude
tokens. A full two-mode gate run is roughly double this, minus the
near-instant skip of the four Train-only scenarios in the `off` leg — the
default `E2E_TIMEOUT=4h` and the contention data behind it (see "How the
timeout/parallelism defaults are derived" above) assume this full two-mode
shape, not just the single-mode total.

The three unprocessed-comment landing scenarios (#1873) are not yet included in the
totals above; see "Additional prerequisites for the unprocessed-comment landing
scenarios" for their per-scenario cost and wall-clock.

### Regression coverage map

| Scenario | Issues / fixes it protects |
|---|---|
| `TestSmokeSingleRepoDispatch` | General pipeline breakage |
| `TestSmokeSingleRepoFullPipeline` | Full pipeline regression |
| `TestNoWorkNeeded` | #733 (marker), #742 (close-on-no-work) |
| `TestBlockedOnInput` | `FABRIK_BLOCKED_ON_INPUT` marker, ed46b7fc (awaiting-input label clear) |
| `TestCrossRepoSpawn` | #797 / #803 (on-demand spawn-target init), v0.0.66 spawn machinery, #800 (addBlockedBy mutation name), #1308 (per-run-unique fixture token; stronger final assertion) |
| `TestYoloAutoMergeLabel` | #829 (GitHub native auto-merge for yolo), #831/#835/#871 (convergence regression cascade) |
| `TestConvergenceRace` | #829 (post-Validate auto-merge race, Story 2/SC-002); regression guard for the production failure on example-org/example-repo#82 (spurious CI-fix-cycle-limit pause); #1217 (mode-aware `fabrik:auto-merge-enabled` assertion) |
| `TestCruiseFullPipeline` | #898 (cruise/yolo gate at Validate, `engine/poll.go`); ensures cruise never triggers `checkAutoMergeConvergence` |
| `TestBaseBranchPipeline` | #1046 (report: base:<branch> GraphQL data gap), #1047 (`verifyAndHealLinkageByBody` linkage fix), #1050 (base-independent review-gate REST data feed) |
| `TestCIFixReinvoke` | #888 ADR-056 D1 (settling primitive reinterprets CI-gate signals); CI-fix reinvoke loop (engine/ci.go) |
| `TestCIFixReinvokeCycleLimit` | CI-fix cycle limit (`pauseForCIFixCycleLimit`), `MaxCiFixCycles` exhaustion path |
| `TestPausedMergedPRRecovery` | #874 (paused+merged PR recovery class), #887 (settle-owner structural fix, `runValidatePRTerminalAdvance`), ADR-056 D2 (single-owner for PR-terminal → Done) |
| `TestConjunctiveCIReviewGate` | ADR-056 D2 (conjunctive gate joint-clear), #887 (settle-owner), #895 (this scenario), #925 (identity/dual-gate/bot-reviewer redesign) |
| `TestReviewAuthorityReinvokesOnChangesRequested` | ADR-1250/ADR-1375 (`review_authority: authoritative`), #1258 (original scenario), #1375 (reinvoke-not-pause model), `handleReviewGate`'s reinvoke-before-block ordering, `buildReviewBodyComments`'s review-body actionability |
| `TestReviewAuthorityCycleLimitPauses` | ADR-1375 R5 (`pauseForReviewCycleLimit` as the terminal fallback), #1375 |
| `TestReviewAuthorityClearsOnApproval` | ADR-1250, #1258 |
| `TestReviewAuthorityYoloDoesNotBypassBlock` | ADR-1250's yolo/cruise composition guarantee, #1258 |
| `TestReviewAuthorityAdvisoryRegressionGuard` | ADR-1250 additive-check regression guard, #1258 |
| `TestExpectedReviewersFastAdvance` | ADR-1283 (`expected_reviewers`), #1298 (this scenario), the #1080 stall the feature exists to eliminate |
| `TestExpectedReviewersDeclaredWaitsAndReprompts` | ADR-1283, #1298 — first e2e coverage of the bot re-prompt ladder (`fabrik:bot-reprompted`, Phase 1/2 of `checkAwaitingReviewTimeout`) |
| `TestExpectedReviewersUndeclaredRegressionGuard` | ADR-1283 FR-5 regression guard, #1298 — pins `reviewGateFastAdvance`'s `expected != nil` check |
| `TestExpectedReviewersFastAdvanceComposesWithAuthoritative` | ADR-1283, #1298 — fast-advance independence from `review_authority` (ADR-1250) |
| `TestReviewAuthorityDeclaredBotDoesNotDeferHumanEscalation` | ADR-1375 Finding 2 (`reviewGateAllBots` gated on `authorityReason == ""`), AC2, #1375 |
| `TestLateCheckRunSuiteGate` | ADR-1822 / #1822 (suite-aware CI gate: `ciSuiteHold`, `settlePRMergeState`), #1829 (`github-actions` suites never inert), #1849 (this scenario) |
| `TestYoloRemovedMidValidateBlocksMerge` | ADR-1769 / #1769 (`refreshAutonomyLabels`, `engine/stages.go`, called from `handleStageComplete` and `runCatchUpPhase2` ahead of `attemptMergeOnValidate`), #1878 (this scenario) |
| `TestCommentReentryShowsReworking` | ADR-1802 / #1802 (`beginStageRework`/`endStageRework`, `engine/comments.go`; `reworkingLabelPrefix`), #1878 (this scenario) |
| `TestAppSelfRecognitionBotCommentNeverResumes` | #1754 (`selfLogin()`), #1877; guards `filterHuman`/`gh.IsBotLogin` on the real wire shape and ADR-1813's resume rule (does not discriminate #1754's cache write-through delta) |
| `TestAppSelfRecognitionBlockedCommentUpdatedInPlace` | #1754 (`findBlockedComment` under App auth), #1877; expected red on the App leg until comment authors are normalised at ingestion |
| `TestAppSelfRecognitionDurableReviewSuppression` | #1754 (`durablyAddressedReviewIDs`), #1555, #1877 |
| `TestMergeTrainHappyPathLanding` | ADR-059 D1/D3 (#946, #947, #948) — Queued column, trial-branch build, integration-PR landing + member lifecycle |
| `TestMergeTrainBisectionEjectsPoisoner` | ADR-059 D4 (#949) — halving bisection, ejection, one-at-a-time fallback |
| `TestMergeTrainConflictBisectPrefixRerere` | #1841 (conflict prompt without build/test commands; turn-limited-but-resolved exit kept), #1835 (trial-prefix reuse), #1834 (rerere replay, `forgetPoisonerResolutions`), #1833 (deterministic Queued order), #1848 (this scenario) |
| `TestMergeTrainRestartSafety` | ADR-059 D5 (#950) + PR #960 (reconstruct must not stall on a historical merged PR) |
| `TestMergeTrainColdCacheBaseMember` | #1688 (report: default branch pinned for a `base:<branch>` member whose cache was cold after a restart), #1772 / ADR-1772 (per-item `IsItemDeepFetched` guard — fail closed on an unhydrated member), #1773 / ADR-1773 (`refuseIfBaseContradictsMembers`, the independent second line), ADR-1648 (per-(repo, base) partitions; unique per-run branch); the warm-cache two-base scenario and the default-base restart scenario cannot cover it |
| `TestMergeTrainRunawayGuardPausesBatch` | ADR-059 D8 (#964) — runaway guard trial cap, per-repo counter isolation |
| `TestMergeTrainSingletonFastPathLandsExactlyOnce` | ADR-1871 / #1871 (a stale Queued snapshot re-landing a just-landed member: second `Landed via singleton fast path` comment), ADR-1644 (singleton fast path), #1874 (this scenario). The exactly-once assertion also runs in the happy-path, batch-cap, restart-safety and yolo (train-on) scenarios for the integration-PR path |
| `TestMergeTrainQueuedDeeperThanBatchCap` | ADR-1833 / #1833 (deterministic Queued ordering — pre-fix the capped batch was an arbitrary map-order subset that churned poll to poll), #1850 (this scenario); the sim bed cannot cover it (ADR-1833 non-vacuity proof) |
| `TestPauseLiftedOnlyByPostPauseHumanComment` | ADR-1813 / #1813 (pre-fix, any human comment lifted a pause — including one older than it, re-lifting every poll), #1752 (the incident: comment circuit breakers zeroed ten times), #1861 (both auth legs), #1876 (this scenario); the sim bed sees only simulated timestamps |
| `TestCommentLandingGateHolds` | #1862 / ADR-1862 (`commentGateBlocksLanding` — an unprocessed comment holds the landing decision in both train modes), #1873 (this scenario) |
| `TestQueuedMemberCommentEjection` | #1863 / ADR-1863 (`settleQueuedCommentCause`, `ejectQueuedMemberForComments` — eject, no pause, no counted ejection), #1873 (this scenario) |
| `TestPostMergeCommentNotApplied` | #1862 / ADR-1862 (`postMergeCommentGuard` — "comment not applied" reply, no worker, no 🚀), #1873 (this scenario) |

Every escape-from-release regression earns a new scenario in this table.

## Adding a scenario

1. Pick a name like `cross_repo_spawn_test.go`. Use `Test<DescriptiveName>` for the
   function so `-run` filtering is clean.
2. Use the helpers in `harness.go` to file the trigger issue and watch for
   expected events.
3. Always clean up at the end — close opened issues, remove the worktree from
   the test bed (`t.Cleanup` is your friend).
4. Document what regression the scenario protects against. Reference the
   originating issue or PR.
5. **If the scenario writes a marker file, give it its own path — do not
   append to `README.md`.** Add an entry to `markerPaths` (`harness.go`) and
   read it via `markerPath("Test...")` when building the issue body. See the
   "Marker-path convention (#1394)" section above; `TestMarkerPathsAreUnique`
   will fail the build if the new path collides with an existing one.
6. **Add a sim-parity registry entry** to `tests/e2e/registry/registry.json`
   (see "Sim parity registry (#1933)" below). `go test ./...` fails until the new
   live test is listed, and prefer adding the sim twin in the same PR.
7. **Assert on something that can only be produced by the engine, on the
   specific path under test** (handarbeit/fabrik#1355). A scenario that
   passes just as easily against a broken engine as a working one is worse
   than no scenario at all — it looks like coverage but proves nothing, and
   the failure is invisible until someone happens to ask why the test
   passed. Two incidents motivated this rule:
   - **#1320**: `TestCIFixReinvokeCycleLimit` matched the engine's
     `🏭 **Fabrik — CI fix cycle limit reached**` marker via
     `strings.Contains` across *every* comment on the issue, including
     stage-output comments the Claude agent itself wrote. On
     `handarbeit/fabrik-test-alpha#4049` a Research-stage comment quoted the
     marker text in prose while describing the feature — the engine never
     posted the marker at all, but the substring scan matched the prose
     instead and the test passed green.
   - **#1319**: `TestExpectedReviewersPrecedenceGuard` passed identically
     against an engine with zero `expected_reviewers` support and against
     the fixed engine — its assertion (`fabrik:awaiting-review` held while a
     reviewer is outstanding) was true under both, so it never actually
     exercised the feature it was named for. It was deleted rather than
     fixed (see handarbeit/fabrik#1355 R1) once shown the property it wanted
     to prove is unreachable in e2e by construction: `reviewGateFastAdvance`
     (`engine/reviews.go`) short-circuits on the outstanding-reviewer check
     before the `expected_reviewers` branch is ever reached.

   In practice:
   - **Match engine-authored comments by body prefix, never by substring
     anywhere in the body.** Mirror `findNewComments`
     (`engine/comments.go:17`), which uses `strings.HasPrefix(c.Body, "🏭
     **Fabrik")` to distinguish Fabrik's own output from anything an agent
     wrote. See the "Marker-substring assertion audit (#1320)" section above
     for the full audit of every `🏭`-marker call site in this package, and
     `ci_fix_reinvoke_marker_test.go` / `no_work_needed_marker_test.go` for
     the concrete pattern to copy: a literal prefix `const`, a
     `strings.HasPrefix`-based helper, and a companion fast (no live bed)
     unit test with fixtures proving the helper accepts genuine engine
     output and rejects a fixture shaped like the #4049 false-pass (prose
     that quotes the marker text).
   - Before writing the assertion, ask what a *broken* version of the
     feature under test would do differently. If the answer is "nothing
     observable," the scenario doesn't yet discriminate — strengthen it
     (per the `TestNoWorkNeeded`/#1355 R2 fix above) or don't add it.
   - **Prefer a loud, diagnostic `t.Skip` over an assertion that can be
     satisfied by accident.** A skip with a clear reason ("prerequisite X
     not configured") is honest about missing coverage; a green check that
     doesn't actually exercise the path under test is worse, because it
     hides the gap.

## Sim parity registry (#1933)

`tests/e2e/registry/registry.json` is the single checked-in, machine-readable
registry keyed by live e2e test name. Its first field records whether the sim bed
(`tests/sim`) covers the same engine path, so the "green sim ⇒ green live run"
promise of the pre-gate (ADR-1454) is something you can check rather than assume.
Later per-test metadata (e.g. auth/train sensitivity, packs, exclusivity) is added
as **fields on the same entries**, not as separate lists.

- **What is a live test?** By rule, not by hand: a top-level `Test*` function in
  `tests/e2e/*_test.go` whose body (nested closures included) calls `LoadEnv`.
  Harness unit tests such as `TestMarkerPathsAreUnique` are excluded by that rule.
  A `LoadEnv` call anywhere that is *not* inside such a body (e.g. through a
  helper) is an error, so an unusual shape is caught rather than silently excluded.
- **Entry shape:** `{"name", "parity": "sim" | "live-only" | "gap", "sim": [...],
  "live_only_reason": "...", "note": "..."}`; the top level is
  `{"version": 1, "tests": [...]}`, sorted by `name`. The vocabulary for
  `live_only_reason` (`model-judgement`, `wire-format`, `review-bot`, `real-ci`,
  `app-auth`, `other`+note) and the rules for `sim`/`gap` are in
  `tests/sim/README.md`'s "Sim parity registry" section.
- **Enforcement:** `tests/e2e/registry` is an untagged Go package, so its
  completeness test runs in plain `go test ./...` on every PR. It fails on a live
  test with no entry, an entry for a test that no longer exists, a sim reference
  that does not exist, or a malformed entry. It discovers tests by parsing source
  (`go/parser`), never by importing the build-tagged e2e package.
- **Reading it from shell:** it is plain JSON, so `jq` works directly —
  `jq -r '.tests[] | select(.parity=="gap") | .name' tests/e2e/registry/registry.json`.
  `scripts/e2e/run.sh` prints `sim parity: N covered, M live-only, K gap` from it
  before the pre-gate (and even when the pre-gate is skipped); the line is
  informational and never gates.
- **Outcomes (#1973).** The registry describes *which* live tests have a sim twin, not
  how a run ended, and needs no schema change for the INCONCLUSIVE outcome: an
  inconclusive test is an ordinary live test whose latest ledger record is
  `INCONCLUSIVE` (uncovered, never a skip — so `skip_ok_legs` never applies to it). See
  "The INCONCLUSIVE outcome and bounded retry" above.

## Design notes

- Scenarios do **not** start or stop the Fabrik instance — the instance is
  expected to be already running. Three exceptions, all using the
  `StopFabrikTestBed`/`StartFabrikTestBed` helpers in `lifecycle.go`:
  `TestMergeTrainRestartSafety` (restarts mid-scenario to exercise
  restart-safety), `TestMergeTrainColdCacheBaseMember` (stops the bed,
  queues its members while it is down, then starts it — see below) and
  `TestSwitchTrainMode` (restarts to flip `FABRIK_MERGE_TRAIN` for the
  two-mode gate — not itself a scenario, run only via `run.sh`'s mode-switch
  step). All three are deliberately **not** `t.Parallel()`.
- `TestMergeTrainColdCacheBaseMember` is ordered **stop → queue → start**, not
  "queue, then restart". The bed polls every 60s, so a batch can form between
  queueing and a literal restart and the scenario would prove nothing. With the
  engine down nothing can batch, and the fresh process's first poll is the first
  formation opportunity with a provably virgin cache (`BootstrapFromProbe` seeds
  members with no labels and no deep-fetch; nothing deep-fetches a Queued item
  before `handleMergeTrainBatch`). Its non-obvious assumptions:
  - It uses **two** members: a one-member batch takes the singleton fast path,
    which lands the member's own PR and cannot reproduce the contradiction.
  - After a restart the engine's `WorktreeManager` map is empty, and a
    `base:`-labelled Queued member is excluded until something calls
    `ensureRepoReady` for its repo. The scenario therefore files a **primer**: an
    issue at Specify with an open `blockedBy` edge, which `processItem` registers
    (before `checkDependencies` labels it `fabrik:blocked`) with no Claude spend.
    A default-base Queued primer would land on `main` and break the
    "nothing reaches `main`" assertion. The underlying engine gap (Queued
    `base:` members can wait indefinitely after a restart until another item in
    the repo is processed) is not fixed by this test-only scenario.
  - `fabrik.log` is truncated on every engine start, so it reads from offset 0
    and scopes every matcher to its own issue numbers and train key.
  - It fails (never passes) if a member has no "not yet hydrated" line, and skips
    on a webhook-enabled bed, where poll 1 hydrates members before batching.
  - The healthy run is **not** expected to log #1773's `REFUSING to open/reuse
    integration PR` line, since #1772 prevents the contradiction; the scenario
    asserts its absence.
  - It posts no comments, so PAT and GitHub App legs behave identically (a
    harness comment would read as human in both modes and eject the member).
- Assertions are on **observable outcomes**, not internal state. We check
  GitHub for label changes, comments, PR creation, etc. — not the engine's
  internal `worktreeManagers` map.
- Log-line assertions are deliberately last-resort. Prefer GitHub state. Logs
  are only useful when the observable outcome is "Fabrik logged something
  specific" (e.g., the spawn error from #797/#803).
- Scenarios should be **idempotent** — running twice in a row should produce
  the same result. If a scenario depends on starting state that prior runs
  modify, normalize it at the start of the test.

## Known limitations

- **Cost per run is non-trivial.** A full cross-repo scenario costs $1–3 in
  Claude tokens. The suite is not for casual local iteration.
- **This suite deliberately never runs in CI** — it drives a real Fabrik bed
  against real repos with real Claude and real review bots, which cannot run
  unattended in a PR job (cost, time, and blast radius). CI only compiles it
  (`go test -tags e2e -run '^$' ./tests/e2e/...`, catching signature breaks
  without executing a scenario or making a network call). It is operator-run
  before cutting a release, via `scripts/cut-release.sh` (R2, #1454) or
  standalone via `scripts/e2e/run.sh`. This is a permanent, deliberate
  design choice, not a "not wired yet" gap — contrast with the sim e2e and
  github wire-contract layers above (R7, #1454), which already run on every
  PR unconditionally (confirmed, not built — see `tests/sim/README.md`'s
  "Runtime and the `sim` tag decision").
- **GitHub rate-limit pressure.** Shared with `~/dev/fabrik/` (the dev
  instance) under the `@arbeithand` token. Stop the dev instance if running
  the full suite.
