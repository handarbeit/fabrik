# ADR 1454: The Sim Bed Is a Pre-Gate, Not a Replacement for Live E2E

**Date**: 2026-08-15
**Status**: Accepted
**Issue**: #1454 — Wire the sim bed in as a fast pre-gate — not a replacement for live e2e

## Context

#1449–#1453 built a deterministic sim bed (`tests/sim`) that ports the live e2e
state-machine scenarios (#1450), adds recovery/settle-scan coverage the live bed
structurally cannot provoke on demand (#1451), adds merge-train coverage with real git
assembly and per-SHA bisection (#1452), and adds `github/` wire-contract tests validating
every GraphQL query/mutation against GitHub's real schema (#1453). None of that changed
the release path: `scripts/e2e/run.sh` still ran the live suite as the sole integration
gate with no cheaper check in front of it, and `scripts/cut-release.sh` had no notion of
the live suite at all — `tests/e2e/README.md` still described a two-layer flow with a
`cut-release.sh --integration-check` note marked "suggested, not yet wired."

The issue that produced this chain originally floated a stronger idea: that the sim bed
might eventually earn the right to retire some live scenarios. That premise is withdrawn.
It is not "not yet" — it is a **no**. Real Claude, real review bots, real GitHub wire
behaviour, and real merge-queue semantics have no substitute, and nothing the sim
demonstrates changes that. What the sim actually earns is narrower and still valuable: a
free, fast sanity check at the point where committing to the cost of a full live run
would be premature.

## Decision

### Four layers, not two, with an explicit ordering

```
go test ./...            unit
        v
sim e2e                   fast sanity check: full pipelines, failure/timeout/restart/
                          merge-train paths — seconds, $0 tokens, -race (tests/sim)
        v
github contract tests     wire-format truth: schema + recorded fixtures (github/)
        v
live e2e (full)           integration truth: real Claude, real bots, real GitHub —
                          runs in full, before every release, permanently (tests/e2e)
```

Each layer answers a different question, and each has a named blind spot:

| Layer | Answers | Cannot see |
|---|---|---|
| `go test ./...` | Does the unit under test behave correctly in isolation? | Cross-stage pipeline behavior, GitHub wire correctness, real Claude |
| sim e2e (`tests/sim`) | Is the state machine obviously broken — full pipelines, failure/timeout/restart/merge-train paths? | GraphQL/REST wire correctness (`simgh` is a hand-modeled fake — see `tests/sim/simgh/FIDELITY.md`); real Claude behavior; real review-bot timing |
| github wire-contract tests (`github/`) | Is every query/mutation this codebase sends schema-valid against GitHub's real API? | Whether the *logic* built on top of a schema-valid query is correct — a query can be shape-valid and semantically wrong; runtime behavior at all |
| live e2e (`tests/e2e`) | Does this actually work against real GitHub, real Claude, and real review bots? | Nothing structural — this is the layer with no substitute, which is exactly why it is never reduced |

The sim's blind spot is the most important one to keep saying out loud: **wire-format
correctness is structural and permanent**, closed only by the wire-contract tests and by
live e2e itself. `tests/sim/README.md` already states this plainly ("read 'the sim
scenarios pass' as 'the state machine behaves, assuming the wire layer does what we
think'") — this ADR is the layering decision that framing sits inside.

### The sim is a pre-gate, never a releasability verdict

The sim answers "is the state machine obviously broken?" in seconds, for nothing, so that
live budget — Claude tokens, GraphQL quota, review-bot turnaround — is spent only on a
candidate that has already cleared the cheap checks. It never answers "is this
releasable." Only the live bed does that, and it always runs, in full, before a release
(mechanism below).

### R1 — `scripts/e2e/run.sh` refuses to spend live budget on a caught bug

`scripts/e2e/run.sh` gained `run_pregate`, called as the very first statement in its
dispatch guard — strictly before bed preflight, before any build, before any live
GitHub/Claude call. It runs `scripts/sim/run.sh --all` (the sim scenarios plus `simgh`'s
own model tests) and `go test -race ./github/...`, and aborts with a new, distinct exit
code (`PREGATE_FAILED_EXIT=5`, continuing the script's existing `3`/`4` convention) on
either failure. The ordering — not a runtime check inside the suite — is what proves "no
live call is made on a pre-gate failure"; `scripts/e2e/pregate_test.sh` pins that ordering
structurally (a grep-based check that `run_pregate` precedes `prepare_bed_and_reset` in
the dispatch guard) in addition to exercising the exit-code contract against a
PATH-shadowed fake `go` binary.

The sim and wire-contract layers are re-run here even though both already run
unconditionally inside `go test -race ./...` on every PR (R7, below) — deliberately, not
redundantly: `scripts/e2e/run.sh` is regularly invoked standalone, and must never assume
unit tests were "just run" by whoever invoked it.

### R2 — the live suite is mandatory by default; the one skip flag is loud and recorded

`scripts/cut-release.sh` gained two new steps: an unconditional, never-skippable "sim +
wire-contract pre-gate" (mirroring `run_pregate`'s reasoning — cheap, so no reason to ever
skip it) and a "live e2e integration gate" that invokes `scripts/e2e/run.sh` by default.

The requirement text left two options open and asked for an explicit choice:

- **(a)** no skip flag exists at all — the live suite always runs before a release, full
  stop; or
- **(b)** a skip flag exists only as a loudly-labelled operator escape hatch, and using it
  records in the release notes that integration was skipped.

**Decision: (b).** A pure "no flag at all" forces every iteration on `cut-release.sh`
itself — or a genuine emergency release — through a multi-hour live run with no sanctioned
way out, which in practice invites someone hacking around the script instead: commenting
out the step, running an old binary, or some other unsanctioned bypass that leaves no
trace at all. That is a worse outcome than one sanctioned, loud, self-documenting escape
hatch. `--skip-integration=<reason>` keeps the *default* mandatory — matching
`--skip-tests`'s existing "last resort" precedent in this exact script — while a bare
`--skip-integration` (no reason) is a hard usage error, never a silent skip. When used, it
prints a loud warning banner to stderr and inserts a recorded line into the release notes'
`## Internal` section (via the new `insert_notes_line` helper, shared with the pre-existing
plugin-bump changelog line) — so anyone reading the shipped release notes sees explicitly
that integration was skipped and why. This satisfies the requirement's own hard
constraint: "a `--skip-integration` flag that silently produces an unvalidated release is
not an acceptable outcome either way" — the flag exists, but silence does not.

The live-suite step is positioned after build+test and before the release-notes commit —
not bolted on at the end — so a live-suite failure aborts before anything is committed or
pushed, and the skip-reason line (when present) folds into the same release-notes commit
rather than requiring a second push. Since `scripts/e2e/run.sh` fetches `origin/main` by
default and this step runs immediately before that commit is pushed and tagged, it tests
exactly the commit about to ship.

`scripts/e2e/run.sh`'s exit codes are branched on explicitly rather than treated as a
single pass/fail: `3` (GraphQL budget exhausted — the verdict cannot be trusted, rerun
later) and `5` (the live script's *own* pre-gate failed, which is unexpected since
`cut-release.sh`'s own pre-gate step already passed against the same tree — investigate
the discrepancy) both produce a distinct, actionable message; anything else is treated as
a real regression, with an explicit instruction not to retry with `--skip-integration` to
work around it.

### R4 — the fidelity-drift policy

The sim's value as a pre-gate is entirely contingent on this rule, and it gets more
important as the sim's role grows, not less: a sim that passes what live would fail is
actively misleading — worse than no gate at all, because it manufactures false
confidence exactly where confidence is being relied on to save a live run.

**Rule:** any live-e2e failure that the sim passed is a fidelity bug in the sim, and is
fixed there as well as in the engine.

**Procedure**, recorded in `tests/sim/README.md` (canonical) and referenced from
`tests/e2e/README.md`, and made a checklist step in
`.claude/skills/cut-release/SKILL.md` so it does not depend on someone remembering:

1. File a fidelity issue describing the divergence — what live e2e caught that the sim
   didn't, and why the sim's model let it through.
2. Add the scenario to `tests/sim` so the same class of bug cannot silently pass here
   again.
3. Update `tests/sim/simgh/FIDELITY.md` — the existing, permanent ledger of every place
   `simgh` knowingly departs from real GitHub — with the divergence found and what it
   cost. This document already exists and already states its own thesis precisely: "the
   failure mode a fake introduces is not a red test — it is a **green** one."

This policy does not retroactively cover every possible gap (the sim is a fake by
construction, and `simgh/FIDELITY.md`'s "Absent"-labeled entries are known, accepted
gaps) — it covers the specific, high-signal case where live e2e actually caught something
in production use that the pre-gate should have caught first.

### R7 — CI placement: confirmed, not built

Per the issue's own framing, this was mostly already true:

- **`go test ./...`** — every PR, unconditionally. Unchanged by this issue.
- **sim e2e (`tests/sim`)** — carries no `sim` build tag (confirmed:
  `grep -rn "//go:build" tests/sim/*.go` returns nothing) and therefore already runs
  inside `go test -race -timeout 5m ./...` in `.github/workflows/ci.yml`, on every PR.
  Indistinguishable in CI output from ordinary unit tests — this issue's
  `scripts/sim/run.sh` gives it a distinct, nameable identity for manual/pre-gate use, but
  does not change how CI itself invokes it.
- **github wire-contract tests (`github/wire_contract_test.go` + `github/testdata/`)** —
  same situation: no build tag (`package github`, not `github_test`), already inside
  `go test -race ./...`, already on every PR.
- **live e2e (`tests/e2e`)** — deliberately excluded from `go test ./...` by its `e2e`
  build tag. CI only compiles it (`go test -tags e2e -run '^$' ./tests/e2e/...`), never
  runs it — unchanged, and correct: it drives a real Fabrik bed against real repos and
  cannot run unattended in a PR job. `scripts/e2e/run.sh` (standalone, or via
  `scripts/cut-release.sh`) is the only thing that actually executes it, and only against
  the real `~/dev/fabrik-test` bed, before a release.

The genuine gap this issue closes was entirely in the **release path**, not CI: layers
1–3 already shared one `go test -race ./...` invocation and already ran on every PR;
nothing tied that fact to "therefore live e2e is safe to run" or to whether a release
should proceed. `scripts/e2e/run.sh`'s new pre-gate and `scripts/cut-release.sh`'s new
steps are what make that ordering real in the one place it wasn't already: right before a
release ships.

### R8 — an on-demand entry point, with honest runtime numbers

`scripts/sim/run.sh` is the frictionless "run the sim layer by hand at any point before
committing to a full live run" entry point the issue asked for — a script, not a
remembered `go test` incantation (no `Makefile` exists in this repo to hang a target off
instead). It defaults to the scenario layer only (`./tests/sim`) — the part that matters
for a pre-gate — and takes `--all` to run the full tree
(`./tests/sim/...`, including `simgh`'s own model tests, `simclaude`, and
`simgh/ghfault`). `run_pregate` in `scripts/e2e/run.sh` calls it with `--all`; a human
iterating locally is expected to use the faster default.

Runtime figures are inherently machine- and load-dependent — `tests/sim/README.md`'s own
revision history shows several "Updated for #N" figures under different conditions — so
this issue records one honest, dated measurement rather than chasing an exact number:

```
tests/sim               42.5s     the scenarios (this script's default)
tests/sim/simgh        105.0s     unit tests of the model itself
tests/sim/simclaude      1.1s
tests/sim/simgh/ghfault  1.1s
                        ~107s total wall clock (--all)
```

replacing the stale "~92s total" / "comfortably under the ~90s line" conclusion that
predated the merge-train suite's addition (#1452) — see `tests/sim/README.md`'s own
"Runtime and the `sim` tag decision" section for the discrepancy and the still-current "no
build tag" decision it protects.

## Consequences

- The live suite is never reduced, retired, or made conditionally-optional by default.
  This ADR does not revisit that; it is the governing constraint every decision above
  operates inside.
- A real `cut-release.sh` invocation now takes materially longer by default — build+test,
  plus the sim/wire-contract pre-gate (~107s), plus a full live e2e run (hours) — unless
  `--skip-integration=<reason>` is explicitly used. This is R2's intended effect, not an
  accident: the script's whole job is to make skipping integration validation a visible,
  justified choice rather than a fast default.
- `--skip-integration`'s reason lands in shipped release notes verbatim. Authors of that
  reason should write it for the audience that will read it there, not as an internal
  note.
- The fidelity-drift policy (R4) creates ongoing maintenance obligation on `tests/sim`
  whenever live e2e catches something the sim missed — this is the cost of the sim's
  value as a pre-gate being real rather than illusory, and is deliberately made a release
  checklist item so it isn't optional in practice.
- `scripts/e2e/run.sh`'s pre-gate and `scripts/cut-release.sh`'s pre-gate step both run
  the sim + wire-contract layers, and both can fire in the same release cut (once
  standalone via a manual `scripts/e2e/run.sh` invocation, once inside
  `cut-release.sh`'s own step 3). This redundancy is accepted, not engineered away — the
  two entry points serve different callers (a human iterating vs. the release script) and
  the cost (~107s twice, at most) is cheap by this issue's own framing.

## Related

- #1449, #1450, #1451, #1452, #1453 — the chain that built the sim bed and the
  wire-contract tests this ADR gates ahead of live e2e.
- `adrs/1449-sim-harness-engine-seams.md`, `adrs/1451-sim-bed-restart-harness.md`,
  `adrs/1452-mergetrain-sim-harness-seams.md`,
  `adrs/1453-wire-contract-graphql-schema-validation.md` — the ADRs for each link in the
  chain.
- `tests/sim/README.md` — "What this layer is permanently blind to" (the sim's own
  blind-spot statement this ADR's layer table restates), "Runtime and the `sim` tag
  decision" (R8's numbers), and the fidelity-drift policy section (R4) added by this
  issue.
- `tests/sim/simgh/FIDELITY.md` — the permanent ledger of `simgh`'s known divergences
  from real GitHub; the target of R4's step 3.
- `tests/e2e/README.md` — "Where this lives in the release flow" (the four-layer diagram
  added by this issue) and "Known limitations" (CI placement, clarified by this issue).
- `scripts/e2e/run.sh`'s `run_pregate` and `scripts/cut-release.sh`'s pre-gate/live-gate
  steps — the implementation.
- `.claude/skills/cut-release/SKILL.md` — the release checklist R4's procedure is wired
  into.
