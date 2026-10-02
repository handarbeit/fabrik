# scripts/lib/killgroup.sh — ownership-checked process-group signalling (#1957).
#
# Meant to be `source`d, not executed. Defines kill_own_group and kill_own_pid
# and nothing else — no side effects on source, no traps.
#
# Used only by scripts/sim/run.sh. scripts/e2e/run.sh is a shim over the Go
# gate runner (tests/gate, #1994), which sends no negative-PID group signal.
#
# Why: `kill -TERM -"$pid"` signals whatever process group currently has ID
# $pid. When $pid was stored earlier (a job already `wait`ed, a watcher that
# has exited) the number may have been recycled by an unrelated group leader,
# and the kill lands on someone else's processes. These helpers confirm the
# group is still the one this shell started and otherwise skip, logging why —
# they never fall back to a broader signal.
#
#   kill_own_group <pid> <sig>   signal the process group led by <pid>
#   kill_own_pid   <pid> <sig>   signal the single process <pid> (same checks)
#
# Rules (same as internal/sessionreap's SignalGroup, ADR-1957):
#   - empty/unset <pid>: silent no-op (the trap-before-assign idiom relies on it);
#   - R4: a target of 0, 1 (so -1/-0 can never be formed), a non-numeric
#     value, or the caller's own process group is refused, before ownership;
#   - a live process holding <pid> must lead its own group (pgid == pid, for
#     kill_own_group) and be a child of this shell — a recycled PID held by an
#     unrelated process fails the parent test and is skipped;
#   - nothing holds <pid> (the leader was reaped): the group is still ours
#     only while it has a live member (POSIX never reuses a PID that still
#     names a populated group), so it is signalled then, and an empty group is
#     a harmless no-op; if membership cannot be read, skip.
#
# "Child of this shell" accepts $$ (the main shell), $BASHPID (this subshell)
# and, inside a subshell, that subshell's own parent: a deadline watcher
# `( sleep n; kill_own_group "$pid" TERM ) &` signals its sibling job, whose
# parent is the shell that forked both. The check is best-effort: a group that
# empties and is reused between the check and the kill is not closed here.

_kg_log() { echo "kill_own_group: $*" >&2; }

# _kg_is_ours_parent <ppid>: succeeds if <ppid> is this shell, this subshell,
# or this subshell's parent.
_kg_is_ours_parent() {
  local pp="$1" self="${BASHPID:-$$}"
  [ "$pp" = "$$" ] && return 0
  [ "$pp" = "$self" ] && return 0
  if [ "$self" != "$$" ]; then
    local sp
    sp="$(ps -o ppid= -p "$self" 2>/dev/null | tr -d ' ')"
    [ -n "$sp" ] && [ "$pp" = "$sp" ] && return 0
  fi
  return 1
}

# _kg_signal <pid> <sig> <group|pid>: shared body.
_kg_signal() {
  local pid="${1:-}" sig="${2:-TERM}" scope="${3:-group}"
  [ -n "$pid" ] || return 0

  case "$pid" in
    *[!0-9]*)
      _kg_log "refused: '$pid' is not a PID"
      return 1
      ;;
  esac
  if [ "$pid" -le 1 ]; then
    _kg_log "refused: target $pid is never signalled"
    return 1
  fi
  if [ "$scope" = group ]; then
    local own_pgid
    own_pgid="$(ps -o pgid= -p "$$" 2>/dev/null | tr -d ' ')"
    if [ -n "$own_pgid" ] && [ "$pid" = "$own_pgid" ]; then
      _kg_log "refused: $pid is the caller's own process group"
      return 1
    fi
  fi

  local info pgid ppid
  info="$(ps -o pgid= -o ppid= -p "$pid" 2>/dev/null)"
  if [ -n "$info" ]; then
    # A process holds the PID: it must be ours.
    set -- $info
    pgid="${1:-}" ppid="${2:-}"
    if [ "$scope" = group ] && [ "$pgid" != "$pid" ]; then
      _kg_log "skipped: PID $pid is live but does not lead its own process group (pgid=$pgid)"
      return 1
    fi
    if ! _kg_is_ours_parent "$ppid"; then
      _kg_log "skipped: PID $pid is live but its parent ($ppid) is not this shell (recycled PID?)"
      return 1
    fi
    if [ "$scope" = group ]; then
      kill "-$sig" -- "-$pid" 2>/dev/null || true
    else
      kill "-$sig" "$pid" 2>/dev/null || true
    fi
    return 0
  fi

  # Nothing holds the PID.
  [ "$scope" = group ] || return 0
  local table members
  if ! table="$(ps -axo pid=,pgid=,stat= 2>/dev/null)" || [ -z "$table" ]; then
    _kg_log "skipped: PID $pid is gone and the process table is unreadable"
    return 1
  fi
  members="$(printf '%s\n' "$table" | awk -v g="$pid" '$2 == g && $3 !~ /^Z/ { n++ } END { print n + 0 }')"
  if [ "${members:-0}" -gt 0 ]; then
    kill "-$sig" -- "-$pid" 2>/dev/null || true
  fi
  return 0
}

kill_own_group() { _kg_signal "${1:-}" "${2:-TERM}" group; }
kill_own_pid() { _kg_signal "${1:-}" "${2:-TERM}" pid; }
