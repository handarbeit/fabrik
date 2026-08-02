# ADR 1297: Dev-Build Gate for the Plugin Corrupted-State Guard

**Date**: 2026-08-02
**Status**: Accepted

## Context

ADR 046 introduced `plugin.KnownEmbeddedVersions`, a hardcoded list of every
plugin fingerprint ever legitimately written to `.installed-version` by a
**release** binary. `checkPluginState`'s corrupted-state guard uses list
membership to decide, when `disk == installedVer` but `embeddedVer` differs,
whether `installedVer` traces back to a legitimate release or to the buggy
pre-v0.0.64 migration that recorded a customised disk hash as `installedVer`
(the incident chain: #746 → #747 → #820 "EMERGENCY: customisation-protection
migration silently 'forgets' pre-existing customisations" → #822, which
introduced `KnownEmbeddedVersions` as the fix).

That list is release-only by construction: it grows one entry per release
whose embedded plugin content changed, appended automatically by
`scripts/cut-release.sh`. A **dev build** — `fabrik` built from source, as
every developer and every test bed runs — writes a fingerprint of its own
embedded plugin content, which can never appear in a release-only list. The
very first plugin refresh performed by a dev build therefore poisons
`.installed-version` with a fingerprint the corrupted-state guard will never
recognize, and every subsequent startup takes the "custom workflow" branch —
indistinguishable from a real operator edit. This is worse than cosmetic: the
project's own e2e release-gate test bed and primary dev instance were both
found silently running stale agent skills as a result, meaning the release
gate had been validating against instructions that differed from what ships.

## Decision

Gate the corrupted-state guard's "unlisted `installedVer`" branch on whether
**the checking binary itself** is a dev build. `cmd.Version` already
distinguishes dev builds from release builds cleanly (`strings.HasPrefix(Version,
"dev")`, the same check `runUpgrade` already uses to skip the binary-download
path). `checkPluginState` and `CheckPluginState` gain an `isDevBuild bool`
parameter; when an unlisted `installedVer` is encountered:

- **`isDevBuild == true`**: trust it — treat as a legitimate prior dev-build
  write, return `(false, true)` (safe to auto-refresh), same as a
  known-embedded hash.
- **`isDevBuild == false`** (release binary): unchanged from pre-#1297
  behavior — treat as the buggy migration's corrupted state, return
  `(true, false)` (custom workflow, skip auto-refresh).

`plugin` cannot import `cmd` (the dependency direction is `cmd → plugin`), so
the dev/release signal is computed by `cmd` — which already has `Version` in
scope at all four `CheckPluginState` call sites — and passed in as a bool.

For the accompanying "name the differing files" requirement, `cmd/root.go`'s
two "local customizations" warnings now call a new `pluginCustomizationWarning`
helper, which reuses `diffingPluginFiles` (`cmd/upgrade.go`) — the same
embedded-vs-disk comparison basis already used for stale-skill counting — to
list which files differ, appended to the existing warning text.

## Alternatives Considered

**Provenance tagging.** Record `dev` vs `release` alongside the fingerprint
(e.g. a second line in `.installed-version`, or a parallel metadata file).
Rejected as the primary mechanism because it does not self-heal
already-poisoned installs: an existing `.installed-version` written before
this fix has no tag, so the classification logic still needs a fallback rule
for "no tag present" — at which point the fallback rule *is* the actual fix,
and the tag is redundant machinery on top of it. It would also require a
`ReadInstalledVersion` format change, which the "no format/schema change"
constraint (self-heal without migration, see Risks below) rules out for a
targeted correction.

**Dropping list-membership entirely for this branch.** Re-derive the
"customized" signal purely from `disk == installedVer` (already computed
earlier in the same function) rather than checking `installedVer` against
`KnownEmbeddedVersions` at all. Self-heals instantly and needs no format
change, but reopens exactly the #820 data-loss class for **release** builds:
`KnownEmbeddedVersions` exists specifically because a real operator's
customised disk hash, once corrupted into `installedVer` by the old buggy
migration, is indistinguishable from a legitimate release fingerprint using
`disk == installedVer` alone. Dropping the check would silently overwrite any
release install still carrying a pre-v0.0.68 corrupted `installedVer` —
precisely the case `TestCheckPluginState_CorruptedMigration` exists to
prevent. Rejected: too broad given the explicit "must not weaken detection"
requirement.

**Local allowlist file, appended on write.** Have a build append its own
embedded fingerprint to a local (gitignored) allowlist when it writes
`.installed-version`, so a later startup recognizes it. Same self-heal gap as
provenance tagging — an already-poisoned install's *classification* logic
still needs to change for the current bad state to recover, since a
`customWorkflow=true` verdict short-circuits every code path that would
otherwise append to the allowlist. Also adds a new on-disk artifact and file
I/O for a "targeted correction" scoped issue.

## Rationale for the Chosen Approach

- **Self-heals immediately, no migration step.** The gate depends only on the
  checking binary's own runtime identity (`cmd.Version`), not on any
  persisted tag or list. An already-poisoned `.installed-version` from a
  prior dev-build refresh — including the project's own e2e bed and primary
  dev instance, both found stuck in this state — is reclassified correctly
  on the very next startup, satisfying the issue's explicit no-migration
  constraint.
- **Doesn't touch the population `KnownEmbeddedVersions` was built to
  protect.** The #820 incident was a release-binary migration bug hitting
  real operator installs. Gating on `isDevBuild` leaves a release build's
  behavior for an unlisted `installedVer` completely unchanged — it still
  treats it as a corrupted migration and still protects that population.
  Only the population this fix is actually about (dev builds, whose
  fingerprints structurally can never be listed) gets new behavior.
- **No format change.** `.installed-version` remains a bare SHA256 string.
  No `ReadInstalledVersion`/`WriteVersionHash` changes, no new on-disk
  artifact.
- **Minimal surface area for a targeted correction.** One new parameter
  threaded through two functions, four call sites updated, one new warning
  helper. The untouched migration branch (`installedVer` absent) and the
  `diskVer != installedVer` branch (genuine operator edit — detected
  regardless of build type) are unaffected, per the issue's explicit
  "must not weaken/must not rewrite" constraints.

### Acknowledged trade-off

A developer who hand-edits `.fabrik/plugin/` on a dev-build machine, at the
exact moment disk still equals a stale `installedVer` and embedded content has
changed, is no longer flagged as customized — `isDevBuild` trusts the
unlisted fingerprint outright in that branch. This narrows the corrupted-state
guard's protection window, but does not eliminate it: the far more common real
case — actively editing `.fabrik/plugin/` mid-session, where `diskVer !=
installedVer` — hits the untouched branch at `checkPluginState`'s line 174 and
is still protected identically to a release build. The #820 incident this
guard exists to prevent happened on release-binary installs, which remain
fully protected.

## Risks / Dependencies

- Whichever mechanism is chosen must not require a one-time migration step for
  installations already stuck misclassified — satisfied, since the gate is
  evaluated fresh on every startup from live binary identity, not from a
  value that needs to have been written under the new logic to take effect.
- `cmd/upgrade.go`'s own "local customizations" messages
  (`checkPluginSkillsWithReader`'s TTY and non-TTY paths, and the
  `upgrade --force`-required error) consume the same `CheckPluginState`
  return values and so stop misfiring on dev builds as a side effect of this
  fix, without their wording being touched (explicitly out of scope per the
  originating issue).

## See Also

- ADR 046 — the original three-way comparison design and `KnownEmbeddedVersions`
- #746, #747, #820, #822 — the incident lineage that produced `KnownEmbeddedVersions`
- #1297 — this fix
