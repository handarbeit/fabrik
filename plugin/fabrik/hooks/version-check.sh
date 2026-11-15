#!/bin/sh
# SessionStart hook: tell the user when this plugin and the `fabrik` binary are
# different versions (#2141). Never blocks: it always exits 0, prints nothing
# when there is nothing to say, and otherwise prints exactly one JSON object.
#
# Silent when: FABRIK_PLUGIN_VERSION_CHECK=off, or either version cannot be read
# (a `fabrik` that is not on PATH yields an empty binary version, so it is the
# same silent path), or the versions are equal. POSIX sh, no jq.
#
# When both versions are release versions (X.Y.Z, optional leading "v") they are
# compared numerically and only the remedy for the side that is behind is named
# (#2584). Anything else keeps the two-remedy wording; a dev build keeps its own.

[ "${FABRIK_PLUGIN_VERSION_CHECK:-}" = "off" ] && exit 0

root="${CLAUDE_PLUGIN_ROOT:-$(cd "$(dirname "$0")/.." 2>/dev/null && pwd)}"
manifest="$root/.claude-plugin/plugin.json"
[ -r "$manifest" ] || exit 0

plugin_version="$(sed -n 's/.*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$manifest" 2>/dev/null | sed -n '1p')"
binary_version="$(fabrik --version 2>/dev/null | sed -n '1p')"
[ -n "$plugin_version" ] && [ -n "$binary_version" ] || exit 0

# goreleaser strips the tag's leading "v"; `go install …@vX.Y.Z` builds keep it.
# Both sides are normalised, so a "v" on either one still compares equal.
plugin_cmp="${plugin_version#v}"
binary_cmp="${binary_version#v}"
[ "$plugin_cmp" = "$binary_cmp" ] && exit 0

# Values are quoted into JSON, so reject anything that could break out of the
# string.
case "$plugin_version$binary_version" in
  *[!A-Za-z0-9._+\(\)-]*) exit 0 ;;
esac

# parse_release V: split a release version (exactly X.Y.Z, each a non-empty run
# of digits) into REL_A REL_B REL_C, with leading zeros removed. Returns 1 for
# anything else (pre-release, build suffix, other component counts).
parse_release() {
  _v="$1"
  case "$_v" in *.*.*) ;; *) return 1 ;; esac
  REL_A="${_v%%.*}"
  _rest="${_v#*.}"
  REL_B="${_rest%%.*}"
  REL_C="${_rest#*.}"
  for _c in "$REL_A" "$REL_B" "$REL_C"; do
    case "$_c" in
      "" | *[!0-9]*) return 1 ;;
    esac
  done
  REL_A="$(strip_zeros "$REL_A")"
  REL_B="$(strip_zeros "$REL_B")"
  REL_C="$(strip_zeros "$REL_C")"
}

# strip_zeros N: remove leading zeros from a digit string, keeping one digit.
strip_zeros() {
  _z="$1"
  while :; do
    case "$_z" in
      0?*) _z="${_z#0}" ;;
      *) break ;;
    esac
  done
  printf '%s' "$_z"
}

# cmp_digits A B: set CMP to lt, eq or gt. Textual, so it is exact for any
# length and never reads a leading zero as octal (no shell arithmetic).
cmp_digits() {
  _a="$1"
  _b="$2"
  if [ "${#_a}" -ne "${#_b}" ]; then
    if [ "${#_a}" -lt "${#_b}" ]; then CMP=lt; else CMP=gt; fi
    return 0
  fi
  CMP=eq
  while [ -n "$_a" ]; do
    _da="${_a%"${_a#?}"}"
    _db="${_b%"${_b#?}"}"
    if [ "$_da" != "$_db" ]; then
      case "0123456789" in
        *"$_da"*"$_db"*) CMP=lt ;;
        *) CMP=gt ;;
      esac
      return 0
    fi
    _a="${_a#?}"
    _b="${_b#?}"
  done
}

# direction: set DIR to plugin-behind, binary-behind, equal or unknown.
direction() {
  DIR=unknown
  parse_release "$plugin_cmp" || return 0
  _pa="$REL_A"; _pb="$REL_B"; _pc="$REL_C"
  parse_release "$binary_cmp" || return 0
  DIR=equal
  for _pair in "$_pa $REL_A" "$_pb $REL_B" "$_pc $REL_C"; do
    # shellcheck disable=SC2086
    set -- $_pair
    cmp_digits "$1" "$2"
    case "$CMP" in
      lt) DIR=plugin-behind; return 0 ;;
      gt) DIR=binary-behind; return 0 ;;
    esac
  done
}

case "$binary_version" in
  dev*)
    msg="Fabrik plugin $plugin_version: the fabrik binary is a development build ($binary_version), so the versions cannot be compared. Set FABRIK_PLUGIN_VERSION_CHECK=off to silence this notice."
    ;;
  *)
    direction
    case "$DIR" in
      equal) exit 0 ;;
      plugin-behind)
        msg="Fabrik plugin $plugin_version is older than the fabrik binary $binary_version. Update the plugin: run /plugin marketplace update fabrik, then /plugin update fabrik@fabrik."
        ;;
      binary-behind)
        msg="Fabrik binary $binary_version is older than the Fabrik plugin $plugin_version. Upgrade the binary: run fabrik upgrade."
        ;;
      *)
        msg="Fabrik plugin $plugin_version and fabrik binary $binary_version differ. Update the plugin (/plugin update fabrik@fabrik) or upgrade the binary (fabrik upgrade) so they match."
        ;;
    esac
    ;;
esac

printf '{"systemMessage":"%s","hookSpecificOutput":{"hookEventName":"SessionStart","additionalContext":"%s"}}\n' "$msg" "$msg"
exit 0
