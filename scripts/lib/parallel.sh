# scripts/lib/parallel.sh — shared `-parallel` cap for every `-race`
# invocation that exercises tests/sim's real-git-spawning scenarios (#1677).
#
# Meant to be `source`d, not executed. Defines one function,
# default_race_parallel, and nothing else — no side effects on source.
#
# Background (#1624, extended by #1677): go test's default `-parallel` is
# GOMAXPROCS — every core the host has. On a high-core-count machine (28
# cores, the one that produced both #1624 and #1677) that means dozens of
# concurrent t.Parallel() scenarios each spawning real `git` children at
# once — high-concurrency fork/exec from a heavily multi-threaded Go
# process, which reliably reproduces a ThreadSanitizer (-race) fork/exec
# SIGSEGV. None of that is proportional to how much real work the suite is
# doing — it's purely a function of host core count — so leaving `-parallel`
# at its default makes the gate's reliability depend on which machine
# happens to run it, which is backwards: a bigger machine should never make
# the same suite *less* reliable.
#
# #1624 originally capped this at min(8, host cores), scoped only to
# scripts/sim/run.sh. #1677's own measurement on the 28-core host that
# produced it found 8 still SIGSEGVs roughly 1 run in 5 — not reliable
# enough for a release gate — while 4 showed zero SIGSEGVs across multiple
# consecutive runs. So the ceiling here is 4, not 8, and this helper is now
# shared by every invocation that exercises the same git-forking scenarios
# under -race: scripts/sim/run.sh (SIM_PARALLEL's default), cut-release.sh
# step 4's repo-wide `go test -race ./...` (which runs the identical
# tests/sim package as part of `./...`), and the CI workflow's equivalent
# step. One number, one algorithm, all three coherent — see #1677's own
# Research findings on why capping only some of the three left the others
# free to reproduce the exact crash the cap exists to prevent.
#
# getconf _NPROCESSORS_ONLN is POSIX and portable across macOS and Linux;
# nproc is a GNU-coreutils fallback for the rare shell where getconf lacks
# that variable, and 4 itself is the last-resort fallback if neither reports
# a usable count. Capping at the host's own core count when that's lower
# than 4 preserves the "never worse than before" property (a flat 4 would
# *increase* concurrent git-spawning on a host with fewer than 4 cores)
# while still bounding every host at 4 regardless of how many cores it has
# above that — mirrors #1624's own min(8,cores)-not-flat-8 reasoning,
# just with a lower ceiling.
default_race_parallel() {
  local cores
  cores="$(getconf _NPROCESSORS_ONLN 2>/dev/null || true)"
  if ! [ "${cores:-}" -gt 0 ] 2>/dev/null; then
    cores="$(nproc 2>/dev/null || true)"
  fi
  if [ "${cores:-}" -gt 0 ] 2>/dev/null && [ "$cores" -lt 4 ]; then
    echo "$cores"
  else
    echo 4
  fi
}
