# ADR 1787: Plugin Staleness Is Reported Independently of Customization Trust

**Date**: 2026-09-18
**Status**: Accepted
**Issue**: #1787 — vendored plugin drift is invisible: customisation short-circuits the staleness
signal, and the version never moves

## Context

`checkPluginState` (`plugin/refresh.go`) performs a three-way comparison between `installedVer`
(from `.fabrik/plugin/.installed-version`), `diskVer` (`ComputeDiskVersion`, the current on-disk
content fingerprint), and `embeddedVer` (`ComputeEmbeddedVersion`, the fingerprint of the plugin
compiled into the running binary). Before this issue, the function's `diskVer != installedVer`
branch (operator customization) returned immediately — `(customWorkflow=true, upgradeNeeded=false)`
— without ever comparing `installedVer` to `embeddedVer`. `embeddedVer` was already computed in
scope at that point; the comparison was simply never made.

The practical effect: the moment an operator customized `.fabrik/plugin/` (the supported path —
`fabrik upgrade` correctly refuses to clobber it), Fabrik stopped telling them anything about how
far the *installed baseline* had drifted from what the current binary embeds. Reported in #1699: a
downstream project sat 40 commits and ~67KB behind for two months with zero signal, discovered only
while investigating unrelated stage failures.

A second, related cause: `plugin/fabrik-workflows/.claude-plugin/plugin.json`'s `version` field has
never moved since it was introduced, so it never looked like a signal an operator could check —
because it isn't one for this plugin (see Decision, R3 below).

## Decision

### `stale` is a third, independent fact — never folded into the trust logic

`checkPluginState`/`CheckPluginState` gained a third return value, `stale bool`, computed once,
before any branching:

```go
stale = installedVer != "" && installedVer != embeddedVer
```

and returned unchanged from every existing return statement, including the customization branch.
`customWorkflow` and `upgradeNeeded` retain their exact pre-existing meaning and decide exactly what
they decided before — "is it safe to auto-refresh." `stale` answers a different question — "is the
installed baseline behind what's embedded in this binary?" — and is **never** consulted by any
auto-refresh decision. This is what makes it safe to compute unconditionally: it cannot change
whether Fabrik overwrites anything, only whether it *says* something. The migration path
(`installedVer == ""`) always reports `stale=false`, since there is no installed fingerprint to
compare against `embeddedVer` — unaffected by this issue, as the Risks section of the originating
issue anticipated.

`stale` is deliberately independent of the known-embedded-versions trust logic
(`isKnownEmbedded`/`isDevBuild`, ADR-1297) that decides whether an unrecognized `installedVer` is
trusted for auto-refresh. `stale` fires whenever `installedVer != embeddedVer`, full stop — including
inside ADR-1297's corrupted-migration/dev-build branches, which are already inside that
`embeddedVer != installedVer` condition and so are correctly stale by construction. Do not add a
"but only if trusted" qualifier to `stale` in a future change — that would silently resurrect a
narrower version of the bug this issue fixes, for exactly the unrecognized-fingerprint case
ADR-1297 exists to handle gracefully.

### Presentation: one combined signal per surface, not two parallel ones

Every caller — the non-TUI startup warning, `fabrik upgrade`'s refusal message,
`checkPluginSkillsWithReader`'s TTY/non-TTY messaging, and the TUI header badge/`u`-key dialog — was
updated to report both facts together when both are true, rather than adding a second, separate
signal alongside the first:

- The TUI header badge becomes a 3-way switch: `[u] custom workflow` (customized only),
  `[u] skills out of date` (stale only), or `[u] custom workflow (N stale)` (both) — one badge slot,
  conditionally longer text, rather than two simultaneous badges. This deliberately avoids reworking
  `header.go`'s width-truncation-budget arithmetic (`badgeWidth`/`available`/the truncation loop),
  which is written for exactly one badge.
- The customization warning/refusal messages (`pluginCustomizationWarning`, `runUpgrade`'s refusal,
  `checkPluginSkillsWithReader`'s TTY/non-TTY branches) append an additive "also stale" clause,
  naming the drifted-file count and `fabrik upgrade --reconcile`, when `stale` is true — never in
  place of the customization warning, per R1.

File-naming for the staleness signal reuses `diffingPluginFiles` (embedded-vs-disk, SHA256 per
file) exactly as-is — the same comparison basis the customization warning already used. This is a
deliberate simplification, not an oversight: `.installed-version` stores a single aggregate
fingerprint, not a per-file baseline, so there is no stored record of what each file looked like at
install time. A clean "you edited this file" vs. "upstream changed this file" per-file attribution
would require a manifest format change, which is out of scope for this issue. The two signals'
file lists legitimately overlap when both fire — this is expected, not a bug.

The optional "N known releases behind" ordinal (`plugin.VersionsBehind`, over the existing
chronological `KnownEmbeddedVersions` list) is shown in the two textual warnings (non-TUI startup,
`fabrik upgrade` CLI messaging) but deliberately omitted from the TUI's compact header badge and
status line, to avoid over-engineering a space-constrained surface. It is omitted entirely (not
fabricated as `0`) whenever `installedVer` is not a recognized entry — a dev build's own fingerprint,
or a baseline predating `KnownEmbeddedVersions` tracking.

### R3: the `plugin.json` version field is documentation, not a fix

`plugin/fabrik-workflows/.claude-plugin/plugin.json`'s `version` field is not consulted by any
staleness or upgrade-detection logic anywhere in Fabrik, and this issue does not change that. This
is a deliberate, pre-existing design, not an oversight this issue needed to correct: ADR-816's
version-auto-bump mechanism exists for the *sibling* `plugin/fabrik` marketplace plugin, whose
staleness detection (Claude Code's own `/plugin update`) is driven entirely by comparing that
cached `version` field — a mechanism ADR-816 explicitly declares out of scope for
`plugin/fabrik-workflows`, because this plugin's staleness detection is Fabrik's own
`ComputeEmbeddedVersion`/`ComputeDiskVersion` content-fingerprint comparison instead. The two
plugins have two unrelated staleness mechanisms; conflating them by also auto-bumping
`fabrik-workflows`'s `plugin.json` would duplicate a mechanism ADR-816 deliberately excluded this
plugin from, for a value nothing reads. `docs/USER_GUIDE.md`'s "Plugin version auto-bump" section
now states this explicitly, so a future reader isn't left inferring it from ADR-816's scope note
alone.

## Consequences

- A customized-and-stale plugin now surfaces both facts, with equal prominence, on every operator
  surface — closing the #1699 gap without weakening `fabrik upgrade`'s refusal to auto-overwrite
  customizations (R5): every call site's existing `customWorkflow`/`upgradeNeeded` branch structure
  is byte-for-byte unchanged: `stale` only ever adds text to branches that already existed.
- `CheckPluginState`'s signature change (`(bool, bool, error)` → `(bool, bool, bool, error)`) touched
  4 production call sites and 8 pre-existing test call sites; a caller silently discarding the new
  `stale` value via `_` would reproduce a narrower version of the original bug in that one path — the
  non-vacuous test added at every modified call site (rather than a compile-only check) is the actual
  guard against this, since `go vet`/`-race` cannot catch a discarded return value.
- `cmd/root.go`'s two structurally-identical `CheckPluginState` call sites (daemon startup,
  auto-upgrade/SIGHUP re-exec) were consolidated behind one shared helper,
  `evaluatePluginStartupState`, so they cannot independently drift on what they report — and so the
  previously-untestable inline startup block became directly unit-testable.
- `header.go`'s combined badge exposed a pre-existing, previously-uncovered width-truncation gap: the
  badge itself was never truncated or dropped when it didn't fit a narrow terminal — only the status
  text was. Fixed alongside this issue (the combined badge's longer text is what surfaced it): when
  even an empty status still leaves the badge overflowing the available width, the badge is now
  dropped entirely rather than left to overflow.
- No version-bump automation was added for `plugin/fabrik-workflows`'s `plugin.json` — see R3 above.
  Do not add it later under the assumption that its absence was an oversight.

## See Also

- ADR-046 (`046-installed-version-tracking.md`) — the original two-fact design this ADR's `stale`
  addition builds on top of, left unedited as an accurate historical record of that decision.
- ADR-1297 (`1297-dev-build-fingerprint-corrupted-state-guard.md`) — the known-embedded-versions
  trust logic `stale` is deliberately independent of; see Decision above.
- ADR-816 (`816-plugin-version-auto-bump.md`) — the sibling `plugin/fabrik` version-bump mechanism
  this issue's R3 explicitly does not duplicate for `plugin/fabrik-workflows`.
- `docs/USER_GUIDE.md`'s "Upgrade protection for customized skills" and "Plugin version auto-bump"
  sections.
