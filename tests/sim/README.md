# `tests/sim` — the simulation test layer

This directory holds a simulated GitHub that Fabrik's engine can be driven
against in an ordinary `go test` run.

It exists because Fabrik had two test layers and nothing between them.

## The three tiers

| | `engine/mocks_test.go` | **`tests/sim`** | `tests/e2e` |
|---|---|---|---|
| What it is | Per-call function hooks | Stateful model, real git backing | Real daemon, real GitHub |
| State across calls | None | Full | Real |
| Multi-poll sequences | No | Yes | Yes |
| Adversarial paths | Hard | On demand | Barely reachable |
| Cost per run | Milliseconds | Seconds | Claude tokens, GraphQL budget, wall clock, a maintained live test bed |
| Catches wire bugs | No | **No** | Yes |

`engine/mocks_test.go` hands each test a set of hooks — `fetchProjectBoardFn`,
`fetchLinkedPRFn`, `fetchCheckRunsFn` — and the test hand-builds the exact
snapshot it wants the engine to see. Nothing carries across poll cycles, so no
test in `engine/` can assert a *sequence* of transitions. The engine is a state
machine; that layer can only photograph one frame at a time.

`tests/e2e` drives a real fabrik daemon against real GitHub, real Claude
workers, and real review bots. It is the only place a multi-poll sequence runs
end to end, and it costs real money and wall-clock time to find out.

The consequence was that every state-machine behaviour — label lifecycles,
conjunctive gates, settle-scan escalation and its `MaxRetries` cutover, cycle
limits, restart recovery — was either asserted one frozen frame at a time, or
cost a live e2e run. Adversarial paths (confirmed CI failure, non-mergeable PR,
API error, engine killed mid-stage) were barely covered at all, because the live
bed cannot be made to produce them on demand.

`tests/sim` is the missing middle: faithful enough that a transition *sequence*
means something, cheap enough to run on every commit.

## What is here

- **[`simgh/`](simgh/)** — a stateful model of a GitHub project (issues, project
  items, PRs, check runs, commit statuses, reviews, labels, comments, blockedBy
  links) implementing the full `engine.GitHubClient` interface, backed by a real
  local bare git repository for everything git can genuinely answer.
- **[`simgh/FIDELITY.md`](simgh/FIDELITY.md)** — every place the model knowingly
  departs from real GitHub, and what each divergence costs. **Read it before
  trusting a sim-backed test to cover a subtle behaviour.**
- **[`simgh/nonvacuity.sh`](simgh/nonvacuity.sh)** — a mutation sweep that
  neutralises each modelled behaviour and asserts the suite goes red.

The seam this layer targets is `engine.NewWithDeps(cfg, GitHubClient,
ClaudeInvoker, *WorktreeManager)`, which accepts substituted dependencies.
Wiring `simgh` into it, along with a `ClaudeInvoker` and a scenario harness, is
follow-on work — this package currently ships standalone with its own tests.

## Two things make it worth having

**It is stateful.** A mutation is observable by every subsequent read, the way
GitHub behaves. Read projections are computed fresh from the model on every
call and never cached, so that is a structural property rather than something
each test re-asserts.

**It is backed by real git.** Mergeability comes from an actual trial merge of
head into base. `MergePR` writes an actual two-parent merge commit onto the base
ref, and only then flips `merged`. Commits-behind comes from `rev-list`. A fake
that lets a test *declare* mergeability cannot catch a mergeability bug.

## What this layer is permanently blind to

**GraphQL and REST wire correctness.** This is not a gap to be closed later in
this package — it is structural, and worth being blunt about.

`simgh` implements a Go interface. Query documents, mutation names, JSON field
mappings, pagination, and status-code handling are all below it and entirely
invisible.

The concrete example is the `addBlockedBy` regression that shipped broken in
v0.0.66 (see [`../e2e/README.md`](../e2e/README.md)). The bug was a wrong
mutation *name* — a string literal inside a query document. Every
interface-level implementation of `AddBlockedByIssue`, including this one, is
perfectly green against it. A sim-backed suite would have shipped that bug with
full confidence.

Two things close that gap, and neither is replaced by this layer:

- **Wire-contract tests**, covering the query documents themselves.
- **`tests/e2e`**, which is not being retired. It remains the only place the
  whole system runs against the real thing.

Do not read "the sim scenarios pass" as "this works". Read it as "the state
machine behaves, assuming the wire layer does what we think".

## Running it

```bash
go test -race ./tests/sim/...          # the suite
bash tests/sim/simgh/nonvacuity.sh     # the mutation sweep (needs a clean tree)
```

The tests use the real `git` binary and skip when it is unavailable, following
the repo-wide `skipIfNoGit` convention.
