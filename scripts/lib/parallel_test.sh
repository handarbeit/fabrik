#!/usr/bin/env bash
# scripts/lib/parallel_test.sh — tests default_race_parallel (#1677, #2228).
# A fake `getconf`/`nproc`/`uname` on PATH stands in for the host.
set -uo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
fail=0

check() { # name expected os getconf-output nproc-output
  local name="$1" want="$2" os="$3" gc="$4" np="$5" bin got
  bin="$(mktemp -d)"
  printf '#!/bin/sh\n%s\n' "[ -n \"$gc\" ] && echo $gc || exit 1" >"$bin/getconf"
  printf '#!/bin/sh\n%s\n' "[ -n \"$np\" ] && echo $np || exit 1" >"$bin/nproc"
  printf '#!/bin/sh\necho %s\n' "$os" >"$bin/uname"
  chmod +x "$bin"/*
  got="$(PATH="$bin:$PATH" bash -c "source '$root/scripts/lib/parallel.sh'; default_race_parallel")"
  rm -rf "$bin"
  if [ "$got" = "$want" ]; then
    echo "ok   $name"
  else
    echo "FAIL $name: want $want got $got"
    fail=1
  fi
}

check "darwin 28 cores"        4 Darwin 28 ""
check "darwin 2 cores"         2 Darwin 2 ""
check "darwin 1 core"          1 Darwin 1 ""
check "getconf fails, nproc"   4 Darwin "" 16
check "no count at all"        4 Darwin "" ""

if [ "$fail" -ne 0 ]; then exit 1; fi
