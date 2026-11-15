# ADR 2584: PM plugin version-check hook names the side that is behind

Amends decision 7 of [ADR 2141](2141-pm-plugin-local-api-and-release-pinning.md).

## Context

The `SessionStart` hook compared the plugin and binary versions as opaque strings and always offered both remedies. After a daemon running with `--auto-upgrade` moves the binary forward the plugin is the side that is behind, and `fabrik upgrade` is the wrong advice (it does nothing for the plugin, and is refused while a daemon holds the directory, ADR 2458).

## Decisions

1. **Grammar.** A release version is exactly `X.Y.Z`, each a non-empty run of digits, with one optional leading `v` (on either side). Anything else (pre-release, build suffix, other component counts, empty components) is unparseable and keeps the two-remedy wording. Full semver precedence is out of scope.
2. **Comparison is textual, not arithmetic.** Leading zeros are stripped, a longer digit string is larger, equal lengths are decided at the first differing digit. This is exact for any length and never reads `09` as octal (which aborts macOS `sh` arithmetic) or overflows 64 bits. Built-ins only: POSIX sh, no jq, always exit 0, one JSON object or nothing.
3. **Silence.** Versions that are numerically equal (`0.0.09` and `0.0.9`) or equal after stripping a leading `v` from either side are silent. A `dev*` binary keeps its own wording, decided before any parsing.
4. **Wording.** Plugin behind: names only `/plugin marketplace update fabrik`, then `/plugin update fabrik@fabrik`. Binary behind: names only `fabrik upgrade`. Tests assert the wrong remedy is absent.

## R3: does `/plugin update fabrik@fabrik` alone pick up the new release?

**Not observed in this pipeline run.** The pipeline is non-interactive and cannot run `/plugin` commands. Evidence available: the marketplace entry's `ref` is pinned to the tag at each cut (decision 4 of ADR 2141), so a cached marketplace names the previous tag until refreshed; `/plugin update` compares the cached `plugin.json` version with the one at the marketplace ref (`tools/bump-plugin-version`); the README, User Guide and v0.0.84 release notes all give the marketplace refresh first. The advice therefore names both steps in order, which is correct whether or not the refresh is strictly needed.

Manual check for the next release (a person in a live Claude Code session, with a plugin from the previous release and a newer binary): start a session and confirm the notice names the plugin; run `/plugin update fabrik@fabrik` alone, then `/reload-plugins` and start a new session. If the notice is gone the refresh is unnecessary and only the plugin-behind string in `version-check.sh` needs shortening; otherwise run `/plugin marketplace update fabrik` first and confirm the notice clears. Record the release and result here.
