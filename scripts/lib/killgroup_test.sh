#!/usr/bin/env bash
# scripts/lib/killgroup_test.sh — coverage for scripts/lib/killgroup.sh (#1957):
# kill_own_group / kill_own_pid must signal a group this shell started, and must
# skip (never widen) a stored PID that is no longer ours.
#
# Usage: scripts/lib/killgroup_test.sh
# Exit 0 if all assertions pass, 1 otherwise.

set -uo pipefail
set -m # one process group per background job, as in run.sh / sim/run.sh

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=/dev/null
source "$REPO_ROOT/scripts/lib/killgroup.sh"

FAILED=0
pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1" >&2; FAILED=1; }

CLEANUP_PIDS=()
cleanup() {
  local p
  for p in ${CLEANUP_PIDS[@]+"${CLEANUP_PIDS[@]}"}; do
    kill -KILL -- "-$p" 2>/dev/null || true
    kill -KILL "$p" 2>/dev/null || true
  done
}
trap cleanup EXIT

alive() { kill -0 "$1" 2>/dev/null; }

# is_dead <pid> [secs]: true once the process is gone or a zombie (an
# unreaped child still answers kill -0).
is_dead() {
  local pid="$1" i=0 st
  while [ "$i" -lt "${2:-30}" ]; do
    st="$(ps -o stat= -p "$pid" 2>/dev/null | tr -d ' ')"
    if [ -z "$st" ] || [ "${st#Z}" != "$st" ]; then return 0; fi
    sleep 0.1
    i=$((i + 1))
  done
  return 1
}

# A process that is the leader of its own group but is NOT a child of this
# shell: the shape of a recycled PID held by an unrelated group leader.
start_foreign_leader() {
  local out
  out="$(bash -c 'set -m; sleep 60 >/dev/null 2>&1 & echo $!' 2>/dev/null)"
  CLEANUP_PIDS+=("$out")
  echo "$out"
}

# --- Case 1: empty / unset PID is a silent no-op -----------------------------
out="$(kill_own_group "" TERM 2>&1)"; rc=$?
[ "$rc" -eq 0 ] && [ -z "$out" ] && pass "empty PID is a silent no-op" || fail "empty PID: rc=$rc out='$out'"
out="$(kill_own_group 2>&1)"; rc=$?
[ "$rc" -eq 0 ] && [ -z "$out" ] && pass "unset PID is a silent no-op" || fail "unset PID: rc=$rc out='$out'"

# --- Case 2 (R4): catastrophic targets are refused ---------------------------
own_pgid="$(ps -o pgid= -p $$ | tr -d ' ')"
for tgt in -1 0 1 "$own_pgid" abc; do
  out="$(kill_own_group "$tgt" TERM 2>&1)"; rc=$?
  if [ "$rc" -ne 0 ] && [[ "$out" == *refused* ]]; then
    pass "R4: kill_own_group refuses '$tgt'"
  else
    fail "R4: '$tgt' not refused (rc=$rc out='$out')"
  fi
done
out="$(kill_own_pid 1 TERM 2>&1)"; rc=$?
[ "$rc" -ne 0 ] && [[ "$out" == *refused* ]] && pass "R4: kill_own_pid refuses 1" || fail "R4: kill_own_pid 1 (rc=$rc out='$out')"
alive $$ && pass "R4: this shell survived every refused target" || fail "R4: this shell was signalled"

# --- Case 3: a live group this shell started is signalled --------------------
sleep 60 &
live=$!
CLEANUP_PIDS+=("$live")
kill_own_group "$live" TERM 2>/dev/null
if is_dead "$live"; then pass "live own group is signalled"; else fail "live own group survived kill_own_group"; fi
wait "$live" 2>/dev/null

# --- Case 4: a PID that is not ours (wrong parent) is skipped ----------------
foreign="$(start_foreign_leader)"
if ! alive "$foreign"; then
  fail "fixture: foreign leader did not start"
else
  out="$(kill_own_group "$foreign" KILL 2>&1)"; rc=$?
  sleep 0.3
  if alive "$foreign" && [ "$rc" -ne 0 ] && [[ "$out" == *skipped* ]]; then
    pass "foreign live leader (parent is not us) is skipped and logged"
  else
    fail "foreign leader signalled or not logged (alive=$(alive "$foreign" && echo y || echo n) rc=$rc out='$out')"
  fi
  kill_own_pid "$foreign" KILL 2>/dev/null
  sleep 0.3
  alive "$foreign" && pass "kill_own_pid also skips a PID that is not our child" || fail "kill_own_pid signalled a foreign PID"
fi

# --- Case 5: a live process that does not lead its own group is skipped ------
out="$(bash -c 'sleep 60 >/dev/null 2>&1 & echo $!' 2>/dev/null)" # no set -m: shares the subshell's group
CLEANUP_PIDS+=("$out")
if alive "$out"; then
  kill_own_group "$out" KILL 2>/dev/null
  sleep 0.3
  alive "$out" && pass "process that does not lead its group is skipped" || fail "non-leader was signalled"
fi

# --- Case 6: leader gone, group still has a member -> still cleaned up -------
mf="$(mktemp)"
sh -c 'sleep 60 >/dev/null 2>&1 & echo $! > "$1"; exit 0' _ "$mf" &
leader=$!
wait "$leader" 2>/dev/null # reap the leader: its PID now names only the group
gc="$(cat "$mf")"
rm -f "$mf"
CLEANUP_PIDS+=("$gc")
if alive "$leader" || ! alive "$gc"; then
  fail "fixture: leader alive=$(alive "$leader" && echo y || echo n) grandchild alive=$(alive "$gc" && echo y || echo n)"
else
  kill_own_group "$leader" KILL 2>/dev/null
  if is_dead "$gc"; then pass "post-exit grandchild is killed (leader gone, group populated)"; else fail "post-exit grandchild survived"; fi
fi

# --- Case 7: leader gone, group empty -> harmless no-op ----------------------
sleep 0.1 &
done_pid=$!
wait "$done_pid" 2>/dev/null
out="$(kill_own_group "$done_pid" TERM 2>&1)"; rc=$?
[ "$rc" -eq 0 ] && [ -z "$out" ] && pass "empty group is a silent no-op" || fail "empty group: rc=$rc out='$out'"

# --- Case 8: works from inside a watcher subshell (the with_timeout shape) ---
sleep 60 &
guarded=$!
CLEANUP_PIDS+=("$guarded")
(
  kill_own_group "$guarded" TERM
) >/dev/null 2>&1 &
watcher=$!
wait "$watcher" 2>/dev/null
if is_dead "$guarded"; then pass "watcher subshell signals its sibling job"; else fail "watcher subshell could not signal its sibling job"; fi

if [ "$FAILED" -ne 0 ]; then
  echo "killgroup_test.sh: FAILED" >&2
  exit 1
fi
echo "killgroup_test.sh: all assertions passed"
