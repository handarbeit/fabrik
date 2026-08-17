#!/usr/bin/env bash
# scripts/sim/run.sh — on-demand entry point for the sim e2e layer (R8, #1454).
#
# Usage:
#   scripts/sim/run.sh                # fast scenario layer only (tests/sim), ~42.5s
#   scripts/sim/run.sh --all          # full tree (tests/sim/... incl. simgh/simclaude/ghfault), ~107s
#   scripts/sim/run.sh -run TestFoo    # forwarded to `go test`, e.g. one scenario
#   scripts/sim/run.sh --all -run Foo  # forwarded, against the full tree
#
# This is BOTH the frictionless manual entry point ("run the sim layer by
# hand at any point before committing to a full live e2e run") AND what
# scripts/e2e/run.sh's pre-gate (run_pregate) calls with --all — there is
# exactly one definition of "the sim suite" in this repo, so the two never
# drift apart.
#
# The sim layer carries no `sim` build tag (see tests/sim/README.md's
# "Runtime and the `sim` tag decision") and already runs unconditionally
# inside `go test -race ./...` on every PR — this script exists to give it a
# distinct, nameable identity for manual/pre-gate use, not to change how CI
# invokes it.
#
# Default target is the scenario layer only (./tests/sim, not ./tests/sim/...)
# — that's the part that matters for a pre-gate (fast, and the layer that
# actually exercises the pipeline), per R8's explicit instruction. --all adds
# tests/sim/simgh (the model's own unit tests), tests/sim/simclaude, and
# tests/sim/simgh/ghfault.
#
# Current measured runtime on a dev machine (tests/sim/README.md, #1454 —
# inherently machine- and load-dependent; see that file for the caveat):
#   tests/sim                42.5s   the scenarios (this script's default)
#   tests/sim/simgh          105.0s  unit tests of the model itself (--all)
#   tests/sim/simclaude        1.1s  (--all)
#   tests/sim/simgh/ghfault    1.1s  (--all)
#                            ~107s total wall clock (--all)
#
# Parallelism cap (R1, #1624): `go test`'s default `-parallel` is
# `GOMAXPROCS`, i.e. every core the host has. On a high-core-count machine
# (28 cores, the one that produced #1624) that means dozens of concurrent
# `t.Parallel()` scenarios each spawning real `git` children at once —
# high-concurrency `fork/exec` from a heavily multi-threaded Go process,
# which #1624 documents causing three distinct failure modes: a wedged
# fork/exec that strands a scenario's `gitMu` until the suite-wide test
# timeout, a child `git` killed outright (SIGSEGV), and even a `-race`
# (ThreadSanitizer) runtime abort. None of that is proportional to how much
# real work the suite is doing — it's purely a function of host core count —
# so leaving `-parallel` at its default makes the gate's reliability depend
# on which machine happens to run it, which is backwards: a bigger machine
# should never make the same suite *less* reliable. SIM_PARALLEL therefore
# pins an explicit, host-core-count-independent cap rather than inheriting
# GOMAXPROCS; override via the environment for experimentation, but the
# pre-gate (scripts/e2e/run.sh's run_pregate, and scripts/cut-release.sh
# transitively) always goes through this one script, so there is exactly one
# place this number lives.

set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"

SIM_PARALLEL="${SIM_PARALLEL:-8}"

TARGET="./tests/sim"
if [ "${1:-}" = "--all" ]; then
  TARGET="./tests/sim/..."
  shift
fi

echo "== sim suite: go test -race -count=1 -parallel $SIM_PARALLEL $TARGET $* =="
exec go test -race -count=1 -parallel "$SIM_PARALLEL" "$TARGET" "$@"
