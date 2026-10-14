#!/usr/bin/env bash
# scripts/sim/measure-race-parallel.sh — R1 of #2228 (ADR 2228): measure how
# `go test -race` on tests/sim behaves at several -parallel levels.
#
# Run it on the host you want to characterise (a Linux box or VM sized like CI's
# 4-vCPU runner). For each level it runs `go test -race -count=1 -parallel N
# ./tests/sim/...` REPEATS times and records wall time, exit status and the
# count of known crash signatures, one TSV row per run:
#
#   level  run  seconds  exit  crashes  signature
#
#   bash scripts/sim/measure-race-parallel.sh                # 4 8 16, 5 runs each
#   LEVELS="4 8" REPEATS=2 bash scripts/sim/measure-race-parallel.sh
#   OUT=/tmp/r1 bash scripts/sim/measure-race-parallel.sh    # keep logs + TSV there
#
# Crash signatures (the macOS failures of #1624/#1677): a wedged/failed
# fork/exec, "git: signal: segmentation fault", and a ThreadSanitizer
# "CHECK failed: tsan_rtl.cpp". A run that fails with none of these is an
# ordinary test failure and is reported as exit!=0, crashes=0.
set -uo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$root"

LEVELS="${LEVELS:-4 8 16}"
REPEATS="${REPEATS:-5}"
TIMEOUT="${TIMEOUT:-20m}"
OUT="${OUT:-$(mktemp -d)}"
mkdir -p "$OUT"
tsv="$OUT/results.tsv"

printf 'level\trun\tseconds\texit\tcrashes\tsignature\n' | tee "$tsv"
echo "# host: $(uname -srm) cpus=$(getconf _NPROCESSORS_ONLN 2>/dev/null || nproc) go=$(go version)" >&2
echo "# logs: $OUT" >&2

for n in $LEVELS; do
  for r in $(seq 1 "$REPEATS"); do
    log="$OUT/p${n}-run${r}.log"
    start=$(date +%s)
    go test -race -count=1 -timeout "$TIMEOUT" -parallel "$n" ./tests/sim/... >"$log" 2>&1
    rc=$?
    secs=$(( $(date +%s) - start ))
    sig=""
    grep -q 'signal: segmentation fault' "$log" && sig="${sig}git-segv,"
    grep -q 'tsan_rtl' "$log" && sig="${sig}tsan-check,"
    grep -Eq 'fork/exec|resource temporarily unavailable' "$log" && sig="${sig}fork-exec,"
    crashes=0
    [ -n "$sig" ] && crashes=1
    printf '%s\t%s\t%s\t%s\t%s\t%s\n' "$n" "$r" "$secs" "$rc" "$crashes" "${sig%,}" | tee -a "$tsv"
  done
done
