# Where tests run: workers vs CI

**Status:** idea, not scheduled. Measured 2026-10-12/13 across every Fabrik instance with transcripts. Nothing here has been filed as an issue yet.

**Motivating question:** we run tests constantly: workers while they iterate, CI on every push, again at Review, again for every Validate steer, again in the merge train. How do we run fewer of them without missing regressions?

---

## TL;DR

1. **Workers spend most of their time running tests, not thinking.** In the code stages, test, lint and build commands take 55–90% of a worker's held time. A worker slot is the scarce resource. CI is parallel and holds no worker.
2. **Worker test runs do not stop regressions reaching main. The landing gate does.** A worker run only decides *when* a failure is found. Running fewer worker tests is safe if the gate is complete. Then the only cost is the occasional extra CI-fix pass.
3. **The gate has two gaps today:** a merge made outside the train can land a tree CI never ran (`strict: false`), and nothing runs CI on main after a merge. Close these first.
4. **Then narrow what workers run.** Run tests for the changed scope, without `-race` by default. Leave the full suite and the race detector to CI, and add a nightly stress run to make up for fewer race runs.
5. **The policy is generic and belongs in the shipped skills. The commands are per project:** each repo states its test tiers in its CLAUDE.md. Flake recording and a gate-gap warning are generic engine work.

---

## Finding 1: where worker time goes

These are wall-clock shares of a worker's held time spent running test, lint or build commands. Overlapping calls are counted once. The data comes from `.fabrik/logs/<repo>/issue-*/*.log` (stream-json, matching each `Bash` tool_use with its tool_result timestamp).

| Instance | Implement | Review | Validate | Rework after Validate |
|---|---|---|---|---|
| handarbeit/fabrik | 66%+ ¹ | 92% | 86% | 82% |
| shadoworg/liminis-models | 53% | 76% | 78% | 68% |
| verveguy/concept-maps | 37% | 63% | 81% | 54% |
| shadoworg/liminis-notes | 57% | 69% | 78% | 50% |
| verveguy/liminis | 37% | 71% | 73% | 47% |
| verveguy/liminis-context-graph | 26% | 25% | 7% | 13% |

¹ This is a lower bound: 57 background test runs returned at once and were not timed.

Fantasy has no transcripts (`fantasy/.fabrik/logs` is empty), so it is not measured.

### Engine repo: 7,900 worker-minutes of `go test`, by scope

| Scope | Calls | Minutes | Share | Per call |
|---|---|---|---|---|
| `tests/sim -race` | 737 | 2,473 | **31%** | 3.4 min |
| `./engine -run X -race` | 719 | 1,283 | 16% | 1.8 min |
| `./...` (whole module) `-race` | 230 | 1,214 | 15% | 5.3 min |
| `./engine` whole package `-race` | 169 | 987 | 13% | 5.8 min |
| other packages `-race` | 555 | 632 | 8% | 1.1 min |
| `tests/sim` (no race) | 402 | 381 | 5% | 0.9 min |
| everything else, no race | 1,198 | 926 | 12% | 0.8 min |

- **`-race` costs about 4× per sim call** (3.4 against 0.9 minutes). Race runs are 83% of the minutes.
- **Whole-module and whole-engine runs** take 2,600 minutes, mostly in Implement, where the change is still moving.
- In concept-mapping, the full `pnpm test:run` takes 1,715 of Validate's 2,967 worker-minutes.

### Repeated full-suite runs on a head CI already passed

The `ci-status.md` skip (#1997) mostly works. In the engine repo, 31 Review/Validate runs that saw `verdict: green` still ran `./...` before making any change: 21 in Review (152 min) and 10 in Validate (78 min). That is real waste, but small next to the rows above.

### CI volume (engine repo, since 2026-09-28)

- **4.4 CI runs per issue branch** on average: median 4, p90 8. Median run length is 7 minutes, p90 11.
- **Of 509 runs, 21% were cancelled** because a newer push arrived, and 9% failed.

---

## Finding 2: the landing gate is what protects main

A worker's local test run is advisory. The only thing that stops a regression reaching main is CI on the tree that lands. Narrowing worker tests is therefore safe exactly when three conditions hold.

### 2a. The tree that lands was tested in full

- **Merge-train landings are covered:**
  - a trial runs CI on the merged tree;
  - the singleton fast path requires base to be an ancestor of the PR head, so the tree that lands is the tree that was tested;
  - a catch-up creates a new head and runs CI on it again.

  `pull_request` CI also tests GitHub's merge ref, which is the head merged with base.
- **Gap 1: a merge outside the train.** `main`'s required checks are `Analyze (go)`, `Verify llms-full.txt is up to date` and `Test and vet`, with **`strict: false`**. A hand-merged `fix/*`, `chore/*` or dependabot PR can land a combination with current main that CI never ran. Since 09-28, about 50 of the 509 runs were on such branches.
- **Gap 2: nothing runs CI on main.** `ci.yml` triggers only on `pull_request` to `main`. An escape is not flagged on main. It appears as red CI on the next, unrelated PR, and that issue is charged a CI-fix pass for someone else's regression.

### 2b. The suite covers the change

How often tests run does not change what they cover. A regression gets through when no test exercises the code it breaks. Two levers:

- **Review checks that each new test is real:** the test fails when the fix is temporarily removed. A green test is a claim, not evidence.
- **Optionally, CI reports which changed lines the tests cover,** so Review can see code no test reaches.

### 2c. Intermittent failures get enough runs

This is what moving `-race` to CI really costs. A race shows up only on some runs. Workers run the sim with `-race` about 740 times in three weeks; CI alone would run it about 4.4 times per issue. Two existing mechanisms can also turn a real race green: the stage gate's flake re-run (#2072) and the train's failed-job re-run (#2052).

Mitigations:

- **A nightly stress run on main**, e.g. `go test -race -count=5 ./tests/sim/... ./engine/...`. One scheduled job gives more race coverage than the scattered worker runs did.
- **Record every re-run that turned green as a flake,** with the check and test name. A test that recurs becomes an issue instead of being absorbed by re-runs.

---

## Proposal

### What workers run

| Situation | Run |
|---|---|
| Before pushing a change | the `changed` tier: tests for the packages or files touched, without race or stress |
| The change touches concurrency, process lifecycle or the train | also the `stress` tier for those packages |
| A docs- or comments-only pass | nothing (except doc generators such as `generate-llms-full.sh`) |
| A head CI already passed, nothing changed | nothing (already the #1997 rule) |
| CI is red | exactly the failing checks (already the #2116 rule) |
| Never, by default | the `full` tier: CI's job |

### What CI and the repo run

- **CI on every PR:** the `full` tier, unchanged (ADR-1857 stands: CI is the cheap, parallel resource).
- **CI on push to main:** the `full` tier. This closes gap 2 and names the escaping commit.
- **Branch protection `strict: true`, or every merge through the train.** This closes gap 1.
- **Nightly:** the `stress` tier, for repos that have one.

---

## Generic vs per project

### Generic: the shipped skills (`plugin/fabrik-workflows/skills/`)

The policy table above, worded toolchain-neutrally. That is required anyway (`plugin/ci_status_skill_test.go` pins it). The skills refer to tiers by name and never name a command:

- run the `changed` tier before pushing;
- run `stress` only when the change touches that risk;
- never run `full` by default;
- in Review, prove each new test fails without the fix.

### Per project: a test-tier convention in each repo's CLAUDE.md

Today "Build & Test" is free-form prose, so workers fall back to running everything. The proposal is a fixed section with four named tiers and one command each. Skills read it, and `fabrik init` could create a skeleton.

Draft for this repo:

```markdown
## Test tiers

| Tier | Command | When |
|---|---|---|
| fast | `go build ./... && go vet ./...` | after every edit |
| changed | `go test ./<pkg>/... -run '<Pattern>'` for each touched package; `go test ./tests/sim/ -run '<Scenario>'` when engine behaviour changed | before every push |
| full | `go test -race ./...` | CI only |
| stress | `go test -race -count=3 ./<pkg>/... -run '<Pattern>'` | when touching concurrency, process reaping or the merge train; nightly on main for `./tests/sim/... ./engine/...` |
```

For comparison, concept-mapping would be:

- **fast:** `pnpm typecheck && pnpm lint`
- **changed:** `pnpm exec vitest run <files>`
- **full:** `pnpm test:run`
- **stress:** probably none.

A project with no meaningful `stress` tier says so, and has no nightly job.

Also per project:

- the `push: main` trigger in its CI workflow;
- the branch-protection `strict` setting;
- the nightly workflow.

### Generic: engine work

- **Flake recording.** The engine already re-runs failed jobs (#2072, #2052). Record the check and test name each time a re-run turns green. When the same one recurs, raise an operator warning (#2135 panel). Every project gets this with no configuration.
- **Gate-gap preflight.** At startup or in `repair-board`, warn on the panel when:
  - the default branch's CI has no `push` trigger; or
  - branch protection is `strict: false` while merges also happen outside the train.

  The fix is made in the repo; the engine only reports it.

---

## Order

The order matters: make escapes visible before reducing the tests that might be catching them.

1. **Close the gate gaps:** `push: main` in `ci.yml`, and `strict` or train-only merges. This repo first, then the other instances.
2. **Add the nightly stress job** (engine repo).
3. **Add the test-tier sections:** this repo's CLAUDE.md first, as the reference; then liminis-models, concept-maps and the Liminis repos.
4. **Change the skill wording** to the tier policy, in a Fabrik issue.
5. **Engine:** flake recording, then the gate-gap preflight, one issue each.

## How to tell it worked

| Signal | Today | Watch for |
|---|---|---|
| Worker test share of held time | 55–90% in code stages | falls substantially |
| First-push CI failure rate after a worker pass | about 9% of runs fail | a rise means the `changed` tier is too narrow |
| CI red on main after a merge | not observable (no trigger) | these are the real escapes; target zero |
| CI-fix passes per issue | measure before step 4 | should not rise much |
| Recurring flakes | not recorded | each one an issue |

## Open questions

- Should Implement keep running `-race` on packages it changed, given that Implement is where races are introduced? The cost is moderate: `engine -run X -race` is 1.8 min per call.
- Should a CI result be keyed to the tree instead of the commit, so that a pure rebase or catch-up producing an already-tested tree skips its CI run? It would remove whole runs from the train path, but needs care to stay exact.
- Should only Validate gate on CI (dropping `wait_for_ci` at Implement and Review)? The number of CI runs is unchanged, but the CI wait leaves the critical path of two stages.
