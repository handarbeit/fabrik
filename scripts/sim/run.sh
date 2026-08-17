#!/usr/bin/env bash
# scripts/sim/run.sh — on-demand entry point for the sim e2e layer (R8, #1454).
#
# Usage:
#   scripts/sim/run.sh                # fast scenario layer only (tests/sim)
#   scripts/sim/run.sh --all          # full tree (tests/sim/... incl. simgh/simclaude/ghfault)
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
# Runtime (R6, #1624): deliberately not documented here as a number. A prior
# version of this comment carried specific per-package figures ("tests/sim
# 42.5s ... ~107s total"); they went stale twice, once to the point of being
# self-contradictory within days of being written (tests/sim/README.md's own
# "~92s total / comfortably under the ~90s line" claim), because the suite
# grows scenarios underneath a number nobody re-measures. `go test`'s own
# per-package "ok"/"FAIL" lines already report real elapsed time for every
# run, and this script also prints its own total wall-clock time below — ask
# the runner, not a comment, if you want to know how long it currently takes.
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
# pins an explicit cap rather than inheriting GOMAXPROCS uncapped — but the
# default is `min(8, host cores)`, not a flat 8 (found during Review,
# #1624): a flat 8 would *increase* concurrent git-spawning, versus the
# previous GOMAXPROCS-derived default, on any host with fewer than 8 cores
# (a modest laptop or CI runner) — the exact mechanism this cap exists to
# guard against, just at smaller absolute scale. Capping at the host's own
# core count when that's lower than 8 preserves the "never worse than
# before" property while still bounding every host at 8 regardless of how
# many cores it has above that. Override via the environment for
# experimentation, but the pre-gate (scripts/e2e/run.sh's run_pregate, and
# scripts/cut-release.sh transitively) always goes through this one script,
# so there is exactly one place this number lives.
#
# Process-group reap (R3, #1624): `go test` is launched as a backgrounded job
# (not via `exec`, which would replace this shell and leave nothing able to
# react to a kill) so a trap can reap it. `set -m` gives that background job
# its own process group, whose leader is the `go test` process itself — its
# own child, the compiled `sim.test` binary that actually runs the suite,
# inherits that same group. Killing this runner (Ctrl-C, a CI job
# cancellation, a plain `kill`) now kills that whole group via the trap
# below, instead of leaving `go test`'s children behind: #1624's evidence was
# eight orphaned `sim.test` processes found alive at ~97% CPU each, some over
# 20 hours old, because nothing had ever reaped them. `setsid`, the common
# Linux idiom for this, is not shipped on macOS by default, so this uses
# portable bash job control instead.

set -euo pipefail
set -m

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"

# Default is min(8, host cores) — see the "Parallelism cap" comment above
# for why a flat 8 is wrong on a sub-8-core host. getconf _NPROCESSORS_ONLN
# is POSIX and portable across macOS and Linux; nproc is a GNU-coreutils
# fallback for the rare shell where getconf lacks that variable, and 8
# itself is the last-resort fallback if neither reports a usable count.
sim_default_parallel() {
  local cores
  cores="$(getconf _NPROCESSORS_ONLN 2>/dev/null || true)"
  if ! [ "${cores:-}" -gt 0 ] 2>/dev/null; then
    cores="$(nproc 2>/dev/null || true)"
  fi
  if [ "${cores:-}" -gt 0 ] 2>/dev/null && [ "$cores" -lt 8 ]; then
    echo "$cores"
  else
    echo 8
  fi
}
SIM_PARALLEL="${SIM_PARALLEL:-$(sim_default_parallel)}"

TARGET="./tests/sim"
if [ "${1:-}" = "--all" ]; then
  TARGET="./tests/sim/..."
  shift
fi

echo "== sim suite: go test -race -count=1 -parallel $SIM_PARALLEL $TARGET $* =="

START_TS=$(date +%s)
# GO_TEST_PID is declared and the trap installed BEFORE backgrounding, not
# after: a signal landing between starting the job and capturing `$!` would
# otherwise have no handler yet, leaving the just-started `go test` (and its
# `sim.test` child) unreaped -- the exact orphan class R3 exists to close
# (found during Review, #1624). A trap's command string is re-expanded at
# signal-delivery time, not install time, so referencing $GO_TEST_PID before
# it's assigned is safe -- `kill -TERM -""` is a harmless no-op under
# `|| true`.
GO_TEST_PID=""
trap 'kill -TERM -"$GO_TEST_PID" 2>/dev/null || true' EXIT INT TERM
go test -race -count=1 -parallel "$SIM_PARALLEL" "$TARGET" "$@" &
GO_TEST_PID=$!

set +e
wait "$GO_TEST_PID"
RC=$?
set -e
trap - EXIT INT TERM

# R6, #1624: print the real elapsed time for this run instead of trusting a
# comment — see the header comment above. go test's own "ok"/"FAIL" lines
# above already gave the per-package breakdown; this is the total.
ELAPSED=$(( $(date +%s) - START_TS ))
echo "== sim suite finished in ${ELAPSED}s (exit $RC) =="
exit "$RC"
