# ADR 046: Installed-Version Fingerprint for Plugin Customization Detection

**Date**: 2026-05-17  
**Status**: Accepted

## Context

Fabrik embeds its plugin skills in the binary and deploys them to `.fabrik/plugin/`
via `fabrik init` / `fabrik upgrade`. Operators routinely customize these skills by
editing `.fabrik/plugin/skills/<name>/SKILL.md` to adapt Claude's behavior to their
team's process.

Before this change, `fabrik upgrade` compared only embedded vs. on-disk hashes.
Any divergence — whether from an upstream change *or* a local edit — triggered
the "files differ, upgrade?" prompt. This meant:

1. **Silent overwrite in non-interactive mode.** Auto-upgrade (no TTY) called
   `RefreshPlugin()` unconditionally, silently erasing operator customizations.

2. **No distinction between "upstream changed" and "operator changed."** Both
   cases looked identical. The prompt did not help operators know whether the
   change was coming from upstream or going away from their customization.

## Decision

Track what the binary last wrote to `.fabrik/plugin/` using a fingerprint file:
`.fabrik/plugin/.installed-version`. The fingerprint is the SHA256 of the
sorted concatenation of all embedded file hashes.

### Three-way comparison

| installedVer | diskVer | embeddedVer | State | Action |
|---|---|---|---|---|
| absent | == embeddedVer | any | Migration — pristine install | Seed installedVer=diskVer; no refresh this cycle |
| absent | != embeddedVer, != "" | any | Migration — pre-existing customizations | Do NOT seed; return customWorkflow=true |
| absent | "" | any | Migration — empty plugin dir | No-op; no seed |
| present | == installedVer | == installedVer | Up to date | No-op |
| present | == installedVer | != installedVer | installedVer in KnownEmbeddedVersions | Upgrade available — auto-refresh (non-TTY) or y/N prompt (TTY) |
| present | == installedVer | != installedVer | installedVer NOT in KnownEmbeddedVersions | Corrupted migration — treat as custom workflow; warn operator |
| present | != installedVer | any | Custom workflow | Block refresh; warn operator |

### Migration baseline

On first encounter (no `.installed-version`), the engine compares `diskVer` to
`embeddedVer` before deciding what to do:

- **Pristine install** (`diskVer == embeddedVer`): seeds `installedVer = diskVer`
  and returns no-op. Subsequent embedded changes are detected correctly.
- **Pre-existing customizations** (`diskVer != embeddedVer`): does NOT seed
  `installedVer`. Returns `customWorkflow=true` so the operator is warned and
  `RefreshPlugin` is not called. Seeding would record the customised hash as the
  baseline, causing the next embedded-version change to auto-overwrite it.
- **Empty plugin dir** (`diskVer == ""`): returns no-op without seeding. An
  empty directory is not a customization.

### Known-embedded-versions list

`plugin.KnownEmbeddedVersions` is a Go slice baked into the binary containing
every plugin fingerprint ever legitimately written to `.installed-version` by a
release binary. It serves as a secondary guard against a corrupted `installedVer`:

When `disk == installed != embedded`, before treating this as a safe auto-refresh,
`checkPluginState` checks whether `installedVer` appears in `KnownEmbeddedVersions`.
If it does not, the value was written by the buggy pre-fix migration (which
recorded the customised disk hash, not an embedded hash) and the state is treated
as a custom workflow instead of an upgrade.

The list grows by one entry per release, appended automatically by
`scripts/cut-release.sh` after the build step. The initial list bootstraps hashes
for all releases from v0.0.64 (when `.installed-version` was first introduced)
through the current release.

### Upgrade paths for custom workflow

When customizations are detected, the engine refuses to overwrite silently and
instead offers three paths:

- **`fabrik upgrade --force`**: unconditional overwrite, updates `.installed-version`
- **`fabrik upgrade --reconcile`**: prints a Claude Code prompt for guided diff and
  merge of customizations against the new embedded version; exits zero
- **TUI `[u]` key → `[1] Reconcile` / `[2] Overwrite`**: interactive equivalents
  of the above flags, with a typed `OVERWRITE` confirmation gate for the destructive path

### Implementation

New functions in `plugin/refresh.go`:

- `ComputeEmbeddedVersion() string` — SHA256 of sorted embedded file hashes
- `ComputeDiskVersion(pluginDir string) (string, error)` — same algorithm over on-disk files; skips `.installed-version`
- `WriteVersionHash(pluginDir, hash string) error` — writes hash to `.installed-version`
- `WriteInstalledVersion(pluginDir string) error` — writes the current embedded hash (post-upgrade)
- `ReadInstalledVersion(pluginDir string) (string, error)` — reads the file; returns `("", nil)` if absent
- `CheckPluginState(pluginDir string) (customWorkflow, upgradeNeeded bool, err error)` — implements the four-state table above; runs migration as a side-effect when installed-version is absent

All auto-refresh paths in `cmd/root.go` and `cmd/upgrade.go` are updated to call
`CheckPluginState` before touching `.fabrik/plugin/`.

## Rationale

### Why SHA256-of-hashes instead of a version number?

A version number requires discipline to bump on every skill edit. A content hash
is computed automatically — no coordination, no manual steps, no version drift.
The embedded fingerprint changes whenever any skill file changes, regardless of
whether the release version was bumped.

### Why compare diskVer to embeddedVer during migration?

The original migration seeded `installedVer = diskVer` unconditionally. That
assumption — "whatever is on disk was intentionally put there" — is wrong for
pristine installs: it records the customised disk hash as the baseline, after
which any embedded-version change silently auto-overwrites those files.

The corrected approach distinguishes pristine from customised:
- **Pristine** (`diskVer == embeddedVer`): seeding `installedVer = diskVer` is
  equivalent to seeding from embedded. Future embedded changes are detected
  correctly, and no false-positive "custom workflow" is raised.
- **Customised** (`diskVer != embeddedVer`): do not seed. Return
  `customWorkflow=true` immediately so the operator is warned. Recording the
  customised hash would corrupt the baseline; the next embedded change would then
  see `disk == installed != embedded` and conclude "safe to auto-refresh" —
  silently destroying the customisation.

### Why require typed `OVERWRITE` for the destructive TUI path?

High-friction confirmation follows the same pattern as `terraform destroy` and
GitHub's repository deletion UI. A simple `y/N` for a destructive reset of
operator-edited files is too easy to confirm accidentally. The typed word makes
the operator's intent explicit and prevents accidental data loss.

### Why print the reconcile prompt to stderr after `p.Run()` returns?

The TUI runs in alt-screen mode. Writing to stderr during alt-screen garbles the
output because the terminal is in a different mode. Storing the prompt in
`model.pendingReconcilePrompt` and printing it after `p.Run()` returns ensures
the output appears cleanly in the normal terminal scrollback after the TUI exits.
