#!/bin/sh
# SessionStart hook: tell the user when this plugin and the `fabrik` binary are
# different versions (#2141). Never blocks: it always exits 0, prints nothing
# when there is nothing to say, and otherwise prints exactly one JSON object.
#
# Silent when: FABRIK_PLUGIN_VERSION_CHECK=off, `fabrik` is not on PATH, or
# either version cannot be read. POSIX sh, no jq.

[ "${FABRIK_PLUGIN_VERSION_CHECK:-}" = "off" ] && exit 0
command -v fabrik >/dev/null 2>&1 || exit 0

root="${CLAUDE_PLUGIN_ROOT:-$(cd "$(dirname "$0")/.." 2>/dev/null && pwd)}"
manifest="$root/.claude-plugin/plugin.json"
[ -r "$manifest" ] || exit 0

plugin_version="$(sed -n 's/.*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$manifest" 2>/dev/null | sed -n '1p')"
binary_version="$(fabrik --version 2>/dev/null | sed -n '1p')"
[ -n "$plugin_version" ] && [ -n "$binary_version" ] || exit 0

# goreleaser strips the tag's leading "v"; `go install …@vX.Y.Z` builds keep it.
binary_cmp="${binary_version#v}"
[ "$plugin_version" = "$binary_cmp" ] && exit 0

# Versions are compared as opaque strings (no semver direction), like the
# daemon/MCP-shim skew note. Values are quoted into JSON, so reject anything
# that could break out of the string.
case "$plugin_version$binary_version" in
  *[!A-Za-z0-9._+\(\)-]*) exit 0 ;;
esac

case "$binary_version" in
  dev*)
    msg="Fabrik plugin $plugin_version: the fabrik binary is a development build ($binary_version), so the versions cannot be compared. Set FABRIK_PLUGIN_VERSION_CHECK=off to silence this notice."
    ;;
  *)
    msg="Fabrik plugin $plugin_version and fabrik binary $binary_version differ. Update the plugin (/plugin update fabrik@fabrik) or upgrade the binary (fabrik upgrade) so they match."
    ;;
esac

printf '{"systemMessage":"%s","hookSpecificOutput":{"hookEventName":"SessionStart","additionalContext":"%s"}}\n' "$msg" "$msg"
exit 0
