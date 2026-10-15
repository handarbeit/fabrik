#!/usr/bin/env bash
# scripts/ci/classify-diff.sh — decide whether a PR's diff can affect any Go
# build or test input (#2254, ADR-2254, amending ADR-1857).
#
# First step of both Go test jobs in .github/workflows/ci.yml. The path rules
# live in scripts/ci/go-skip-paths.txt. If every changed path is `skip` there,
# the job skips its Go steps (a step-level early exit, never a workflow
# `paths:` filter, so the required checks still report success on the real head
# SHA). Everything else — a `never` path, a path on no list, an unparseable
# path, an empty or unreadable diff, a HEAD that is not a merge commit — runs
# the full suite. Fail safe: this script always exits 0 in CI mode and every
# failure path reports skip=false.
#
# Usage:
#   classify-diff.sh                     classify `git diff HEAD^1 HEAD` of the
#                                        synthetic PR merge commit (CI mode)
#   classify-diff.sh --paths-from-stdin0 classify a NUL-delimited path list from
#                                        stdin (used by tests/ciskip)
#
# Outputs (appended to $GITHUB_OUTPUT when set): skip, cut_release, killgroup,
# adr_refs — each true|false. When skip=false the three shell-check outputs are
# all true, so a full run executes exactly what it always did. The summary line
# goes to $GITHUB_STEP_SUMMARY when set.
#
# Test seams (environment):
#   CLASSIFY_RULES_FILE            alternate rule table
#   CLASSIFY_DISABLE_NEVER_SKIP=1  ignore every `never` rule (non-vacuity test)
#
# Meant to be runnable and sourceable: the dispatch guard at the bottom keeps
# `main` from running when the file is sourced.

_CLASSIFY_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RULES_FILE="${CLASSIFY_RULES_FILE:-$_CLASSIFY_DIR/go-skip-paths.txt}"

NEVER_PATTERNS=()
SKIP_PATTERNS=()
SKIPTOP_PATTERNS=()

# load_rules fills the three pattern arrays from $RULES_FILE. Returns non-zero
# when the file is unreadable or holds an unknown rule kind.
load_rules() {
  NEVER_PATTERNS=()
  SKIP_PATTERNS=()
  SKIPTOP_PATTERNS=()
  [ -r "$RULES_FILE" ] || return 1
  local line kind pattern
  while IFS= read -r line || [ -n "$line" ]; do
    case "$line" in ''|'#'*) continue ;; esac
    kind="${line%% *}"
    pattern="${line#* }"
    [ -n "$pattern" ] && [ "$pattern" != "$line" ] || return 1
    case "$kind" in
      never) NEVER_PATTERNS+=("$pattern") ;;
      skip) SKIP_PATTERNS+=("$pattern") ;;
      skip-top) SKIPTOP_PATTERNS+=("$pattern") ;;
      *) return 1 ;;
    esac
  done <"$RULES_FILE"
  [ "${#SKIP_PATTERNS[@]}" -gt 0 ] || return 1
}

# classify_path <path> sets CLASS (skip|run) and REASON.
classify_path() {
  local p="$1" pat seg rest
  CLASS=run
  # Unparseable: empty, absolute, backslash, or an empty/./.. segment.
  if [ -z "$p" ]; then REASON="unparseable path (empty)"; return; fi
  case "$p" in
    /*) REASON="unparseable path (absolute)"; return ;;
    *\\*) REASON="unparseable path (backslash)"; return ;;
  esac
  rest="$p"
  while :; do
    seg="${rest%%/*}"
    case "$seg" in
      ''|.|..) REASON="unparseable path (empty, '.' or '..' segment)"; return ;;
    esac
    [ "$seg" = "$rest" ] && break
    rest="${rest#*/}"
  done
  if [ "${CLASSIFY_DISABLE_NEVER_SKIP:-}" != 1 ]; then
    for pat in "${NEVER_PATTERNS[@]}"; do
      # shellcheck disable=SC2053
      if [[ "$p" == $pat ]]; then REASON="never-skip rule '$pat'"; return; fi
    done
  fi
  for pat in "${SKIP_PATTERNS[@]}"; do
    # shellcheck disable=SC2053
    if [[ "$p" == $pat ]]; then CLASS=skip; REASON="skippable rule '$pat'"; return; fi
  done
  if [[ "$p" != */* ]]; then
    for pat in "${SKIPTOP_PATTERNS[@]}"; do
      # shellcheck disable=SC2053
      if [[ "$p" == $pat ]]; then CLASS=skip; REASON="skippable top-level rule '$pat'"; return; fi
    done
  fi
  REASON="matches no skippable rule"
}

# log_safe <string> prints the string with every control character (newline
# included) replaced by '?', so a hostile file name cannot start a line in the
# job log with `::` and be read as a runner workflow command (::add-mask::,
# ::stop-commands::, ...). Display only: classification uses the raw path.
log_safe() {
  printf '%s' "${1//[[:cntrl:]]/?}"
}

# classify_paths reads NUL-delimited paths from stdin and sets the globals
# RESULT_SKIP, OUT_CUT_RELEASE, OUT_KILLGROUP, OUT_ADR_REFS, RESULT_REASON.
# Logs one line per path.
classify_paths() {
  local p n=0 first_run=""
  local cut=false kg=false adr=false
  while IFS= read -r -d '' p; do
    n=$((n + 1))
    classify_path "$p"
    printf '  %-5s %s  (%s)\n' "$CLASS" "$(log_safe "$p")" "$REASON"
    if [ "$CLASS" = run ] && [ -z "$first_run" ]; then first_run="$(log_safe "$p"): $REASON"; fi
    case "$p" in
      scripts/cut-release.sh|scripts/cut_release_gate_test.sh|scripts/lib/*) cut=true ;;
    esac
    case "$p" in scripts/lib/*) kg=true ;; esac
    case "$p" in
      adrs/*|docs/*|CLAUDE.md|scripts/check-adr-references.sh) adr=true ;;
    esac
  done
  if [ "$n" -eq 0 ]; then
    RESULT_SKIP=false
    RESULT_REASON="empty diff (fail safe: run everything)"
  elif [ -n "$first_run" ]; then
    RESULT_SKIP=false
    RESULT_REASON="running everything: $first_run"
  else
    RESULT_SKIP=true
    RESULT_REASON="all $n changed path(s) are skippable"
  fi
  if [ "$RESULT_SKIP" = true ]; then
    OUT_CUT_RELEASE=$cut OUT_KILLGROUP=$kg OUT_ADR_REFS=$adr
  else
    OUT_CUT_RELEASE=true OUT_KILLGROUP=true OUT_ADR_REFS=true
  fi
}

# emit_outputs prints the decision and writes $GITHUB_OUTPUT / the step summary.
emit_outputs() {
  echo "classify-diff: skip=$RESULT_SKIP cut_release=$OUT_CUT_RELEASE killgroup=$OUT_KILLGROUP adr_refs=$OUT_ADR_REFS"
  echo "classify-diff: $RESULT_REASON"
  if [ -n "${GITHUB_OUTPUT:-}" ]; then
    {
      echo "skip=$RESULT_SKIP"
      echo "cut_release=$OUT_CUT_RELEASE"
      echo "killgroup=$OUT_KILLGROUP"
      echo "adr_refs=$OUT_ADR_REFS"
    } >>"$GITHUB_OUTPUT"
  fi
  if [ "$RESULT_SKIP" = true ] && [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
    {
      echo "### Go tests skipped: no Go build inputs changed"
      echo
      echo "$RESULT_REASON. Rules: \`scripts/ci/go-skip-paths.txt\`."
    } >>"$GITHUB_STEP_SUMMARY"
  fi
}

# fail_safe reports run-everything for an internal problem.
fail_safe() {
  RESULT_SKIP=false
  RESULT_REASON="running everything (fail safe): $1"
  OUT_CUT_RELEASE=true OUT_KILLGROUP=true OUT_ADR_REFS=true
  emit_outputs
}

run_stdin0() {
  load_rules || { fail_safe "cannot load $RULES_FILE"; return 0; }
  classify_paths
  emit_outputs
}

run_git() {
  load_rules || { fail_safe "cannot load $RULES_FILE"; return 0; }
  # A pull_request checkout is the synthetic merge commit: parent 1 is the live
  # base tip, parent 2 the PR head. Anything else is unknown territory.
  if ! git rev-parse --verify -q 'HEAD^2' >/dev/null 2>&1; then
    fail_safe "HEAD is not a merge commit"; return 0
  fi
  if ! git rev-parse --verify -q 'HEAD^1' >/dev/null 2>&1; then
    fail_safe "base commit HEAD^1 is unavailable"; return 0
  fi
  local tmp
  tmp="$(mktemp)" || { fail_safe "cannot create a temp file"; return 0; }
  # --no-renames reports both sides of a rename; -z is NUL-safe.
  if ! git diff --name-only --no-renames -z HEAD^1 HEAD >"$tmp" 2>/dev/null; then
    rm -f "$tmp"
    fail_safe "git diff failed"; return 0
  fi
  echo "classify-diff: changed paths (HEAD^1..HEAD):"
  classify_paths <"$tmp"
  rm -f "$tmp"
  emit_outputs
}

main() {
  case "${1:-}" in
    --paths-from-stdin0) run_stdin0 ;;
    '') run_git ;;
    *) echo "usage: classify-diff.sh [--paths-from-stdin0]" >&2; fail_safe "bad usage" ;;
  esac
  return 0
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  main "$@"
fi
