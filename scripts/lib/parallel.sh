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
# (The ceiling is now per OS — see the #2228 paragraph below; the 4 here is the
# macOS/other value.)
#
# #1624 originally capped this at min(8, host cores), scoped only to
# scripts/sim/run.sh. #1677's own measurement on the 28-core host that
# produced it found 8 still SIGSEGVs roughly 1 run in 5 — not reliable
# enough for a release gate — while 4 showed zero SIGSEGVs across multiple
# consecutive runs. So the ceiling here is 4, not 8, and this helper is now
# shared by every invocation that exercises the same git-forking scenarios
# under -race: scripts/sim/run.sh (SIM_PARALLEL's default, when SIM_RACE=1)
# and the CI workflow's repo-wide step. (cut-release.sh step 4 ran a third
# copy until v0.0.84, when the local race run was removed.) One number, one
# algorithm, all coherent — see #1677's own
# Research findings on why capping only some of the three left the others
# free to reproduce the exact crash the cap exists to prevent.
#
# #2228 (ADR 2228) measured the same suite on Linux: GitHub Actions
# ubuntu-latest runners (x86-64, 4 vCPU), `go test -race -count=1 -parallel N
# ./tests/sim/...`, five runs per level, 15 runs total. Every run was clean —
# no segfault, no tsan_rtl CHECK, no fork/exec failure — at N = 4, 8 and 16, and
# wall time fell from ~405 s (4) to ~249 s (8) to ~182 s (16). The fork/exec
# crash that motivated the cap is therefore macOS-specific as far as measured
# (macOS's TSan runtime / fork handling), so the ceiling is per OS: 4 on
# anything that is not Linux, 16 on Linux. Five clean runs only bound the crash
# rate (about <=45% at 95% confidence); the standing validation is CI itself,
# which runs this at every PR. If a Linux crash ever appears, lower the Linux
# ceiling here — it is the single place CI, the worker-shaped job and
# SIM_RACE=1 all read.
#
# Unmeasured: the 16 was measured only on 4-vCPU runners, but it applies to
# every Linux host, including a many-core workstation running SIM_RACE=1 or a
# hand-run `go test -race`. ADR 1677's macOS crash appeared on a 28-core host,
# so a Linux box with many cores is the case most like it and has not been
# tried. Set SIM_PARALLEL (scripts/sim/run.sh) or pass -parallel explicitly to
# stay lower there; the ceiling is not scoped to CI.
#
# getconf _NPROCESSORS_ONLN is POSIX and portable across macOS and Linux;
# nproc is a GNU-coreutils fallback for the rare shell where getconf lacks
# that variable, and the OS ceiling itself is the last-resort fallback if
# neither reports a usable count. Capping at the host's own core count when
# that is lower than 4 preserves the "never worse than before" property (a
# flat ceiling would *increase* concurrent git-spawning on a host with fewer
# than 4 cores) — mirrors #1624's own min(8,cores)-not-flat-8 reasoning.
default_race_parallel() {
  local cores ceiling
  case "$(uname -s 2>/dev/null || true)" in
    Linux) ceiling=16 ;;
    *) ceiling=4 ;;
  esac
  cores="$(getconf _NPROCESSORS_ONLN 2>/dev/null || true)"
  if ! [ "${cores:-}" -gt 0 ] 2>/dev/null; then
    cores="$(nproc 2>/dev/null || true)"
  fi
  if [ "${cores:-}" -gt 0 ] 2>/dev/null && [ "$cores" -lt 4 ]; then
    echo "$cores"
  else
    echo "$ceiling"
  fi
}
