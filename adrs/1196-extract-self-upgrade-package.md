# ADR 1196: Extract self-upgrade into `internal/selfupgrade`

**Date**: 2026-07-27
**Status**: Accepted
**Issue**: #1196 — extract self-upgrade into a shared package so Pruefer can use it without importing `engine`

## Context

Fabrik's self-upgrade logic — version comparison, release-asset download, dev-build rebuild, re-exec — lived entirely in `engine/upgrade.go`. (No step here cryptographically verifies release asset contents — only an HTTP status check on the download and, on darwin, an ad-hoc re-sign so the binary passes AMFI; see `internal/selfupgrade/doc.go`.) Pruefer needs the same capability, but ADR-1113 established a hard boundary: Pruefer must not import `engine` (it must not depend on board/stage concepts). That boundary held cleanly for V1 because Pruefer had no self-upgrade code at all — only one daemon could self-upgrade.

Unlike Fabrik, a stale Pruefer is silent: it has no board, no issues, nothing that visibly looks wrong. On 2026-07-27, #1189 and #1190 both landed while a running Pruefer instance kept using pre-#1189/#1190 logic until someone noticed by hand. The fix is for Pruefer to gain the same self-upgrade capability, without violating the no-`engine`-import boundary.

## Decision

Extract the generic parts of `engine/upgrade.go` into `internal/selfupgrade`, a new module-internal package neither daemon owns (following the `internal/itemstate` precedent for shared, dual-consumer code). `engine` is rewired to call into it; wiring Pruefer to it is an explicit follow-up (out of scope here) — this issue only proves the extraction preserves Fabrik's behavior exactly.

### Package shape

Two entry points, split along how the two upgrade paths actually diverge:

- **`release.go`** — `PerformReleaseUpgrade(cfg ReleaseConfig) error`: GitHub Releases API → download → tarball extract → atomic `os.Rename` → darwin codesign → re-exec.
- **`devbuild.go`** — `CheckAndRebuildDev(cfg DevBuildConfig)`: git rev-parse/fetch/merge-base/pull → `go build` → optional post-build hook → re-exec.
- **`semver.go`** / **`tarball.go`** — the pure/OS-primitive helpers both paths share (`SemverGreater`, `ExtractBinaryFromTarball`).
- **`seams.go`** — `execFn`/`executableFn`, the `syscall.Exec`/`os.Executable` test seams, shared across both paths.

Both entry points take a config struct rather than positional parameters (`ReleaseConfig`, `DevBuildConfig`) — matching the existing `InvokeOptions` precedent in `engine/interfaces.go` — since each needs 5+ caller-supplied identity fields plus optional hooks.

### Narrow interface, not `engine.GitHubClient`

`PerformReleaseUpgrade` previously took the full `engine.GitHubClient` (50+ methods) for one method call. The package now defines its own:

```go
type ReleaseFetcher interface {
    FetchLatestRelease(owner, repo string) (*gh.LatestRelease, error)
}
```

Go's structural typing means `engine.GitHubClient`-typed values (e.g. `e.client`) satisfy `ReleaseFetcher` automatically — no adapter, no import edge back to `engine`. The interface does depend on `github.LatestRelease`/`github.ReleaseAsset`, which is fine: `github` doesn't import `engine` either, and `pruefer/` already imports `github` freely.

### Caller-supplied identity and hooks, nothing hardcoded

Every piece of "fabrik" that was previously hardcoded is now a config field: binary name (`ExtractBinaryFromTarball`'s tarball-entry match, the release asset-name format), owner/repo for the Releases API, and the expected source-checkout remote substring (`IsSourceCheckout`, renamed/parameterized from `isFabrikSourceCheckout`).

Two optional hooks carry the two pieces of genuinely Fabrik-specific behavior that don't belong in a generic package:

- `PostBuildHook func(exe, dir string) error` — the dev-build path's plugin-skill refresh (`exec.Command(exe, "upgrade")`). Its error is logged and non-fatal, matching the original inline behavior exactly; a caller with no such hook (a future Pruefer) simply passes nil.
- `StatusFn`/`StatusClearFn` — the dev-build path's transient "checking origin/main ..." status line. `engine` wires these to its own `pollStatus`/`pollStatusClear` TUI globals, which cannot themselves move to the shared package (they're engine's log-file/TTY plumbing). Both are nil-safe no-ops when unset, preserving the exact existing UX for Fabrik without leaking engine internals into the package.

### `checkVersionSkew` keeps its own seam

`checkVersionSkew` (`engine/upgrade.go`, explicitly out of scope for this issue) previously shared the `upgradeExecutableFn` package var with `PerformReleaseUpgrade`. Since that function moved packages, `checkVersionSkew` now has its own private `versionSkewExecutableFn = os.Executable` — a one-line duplication that avoids coupling an unrelated diagnostic feature to `internal/selfupgrade`'s internals.

## Why extraction, not duplication (diverging from ADR-1113 Decision 6)

ADR-1113 Decision 6 made the opposite call for a structurally similar problem: `pruefer/procattr_*.go` duplicates `engine/procattr_*.go`'s process-group-kill logic (~80 lines) rather than extracting it, reasoning that touching `engine`'s existing call sites for stable, security-relevant OS primitives was riskier than a one-time copy.

Self-upgrade differs on the axis that actually matters for this trade-off:

- **Not small or stable.** It has a real incident history (#1074, the suffix-tolerant semver fix) and three active call sites (`engine/poll.go` ×2, `cmd/upgrade.go`), not one.
- **A duplicated copy would need the same fix applied twice**, and silently diverging is exactly the failure mode (a stale, silently-different upgrade path) this issue exists to prevent.
- **The dev-build path had zero test coverage before this issue** — duplicating untested code doubles the risk surface instead of fixing it once.

Extraction costs more up front (a new package boundary, a narrower interface, config structs instead of positional args) but the alternative — Pruefer eventually growing its own copy of `checkAndUpgrade`'s ~150 lines of git/build/re-exec logic — reintroduces the exact "only one daemon can have it" problem this issue exists to solve.

## Consequences

**Positive:**
- `internal/selfupgrade` has zero dependency on `engine`, verified by `go build`/`go vet` across the module — Pruefer can consume it later with no boundary violation.
- The dev-build path gained its first-ever direct test coverage (`internal/selfupgrade/devbuild_test.go`), including a regression test for the plugin-refresh hook's non-fatal-failure behavior — a gap this issue's own Risks section flagged as easy to silently drop during extraction.
- `cmd/upgrade.go` no longer imports `engine` at all, incidentally.
- Zero behavior change for Fabrik: same asset-name format, same merge-base-ancestor guard (unpushed local commits are never clobbered), same darwin codesign step, same plugin-refresh hook, same re-exec tail — verified by the full existing `PerformReleaseUpgrade`/`SemverGreater`/tarball test suite passing unchanged against the moved code.

**Negative / Trade-offs:**
- Two config structs (`ReleaseConfig`, `DevBuildConfig`) with several fields each are more ceremony at call sites than the previous positional-argument functions.
- This issue lands zero functional benefit for Pruefer by itself — wiring Pruefer to `internal/selfupgrade` is a separate, chained follow-up issue that this one blocks.
- The dev-build path's transient status-line UX now depends on `StatusFn`/`StatusClearFn` being wired correctly at every call site that wants it; a future caller that forgets them silently loses the live-overwrite behavior (falls back to plain `Logf` calls) rather than failing loudly. Acceptable: it's a cosmetic degradation, not a functional one.

## Related Work

- `adrs/1113-pruefer-v1-architecture.md` — the no-`engine`-import boundary this issue exists to satisfy, and Decision 6's duplication precedent this issue deliberately diverges from.
- `internal/itemstate/doc.go` — the existing precedent for a module-internal package shared across `engine` and other consumers.
- `engine/upgrade.go` — the trimmed-down caller: `checkAndUpgrade`/`checkReleaseUpgrade` now delegate to `internal/selfupgrade`; `checkVersionSkew` is unchanged and out of scope.
- `docs/state-machine.md` — the version-skew watchdog paragraph, corrected to reflect that `checkVersionSkew` no longer shares a seam with the now-relocated `PerformReleaseUpgrade`.
- **Follow-up (blocked by this issue):** wiring Pruefer to `internal/selfupgrade` and adding Pruefer to the release matrix.
