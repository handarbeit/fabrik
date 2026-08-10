# ADR 1197: Ship Pruefer as a released artifact with self-upgrade

**Date**: 2026-08-02
**Status**: Accepted
**Issue**: #1197 — ship Pruefer as a released artifact and wire auto-upgrade

## Context

Pruefer had no auto-upgrade: it silently ran whatever binary was last built by hand. That is worse than it sounds for a daemon with no board and no issues — a stale Fabrik is *visible* (its behavior on issues looks wrong and someone investigates); a stale Pruefer just keeps reviewing with old logic with no signal. On 2026-07-27, both #1189 (inline review comments) and #1190 (`--setting-sources`) landed while a running Pruefer instance kept posting body-only reviews with trust warnings until someone noticed and rebuilt manually.

`internal/selfupgrade` (ADR-1196) already extracted Fabrik's self-upgrade logic into an engine-free package specifically so Pruefer could consume it without violating ADR-1113's no-`engine`-import boundary. This issue is that follow-up: publish a `pruefer` binary from `.goreleaser.yaml` and wire `cmd/pruefer`/`pruefer/` to `internal/selfupgrade`.

**This issue captures the work; it does not authorize shipping it in the imminent release.** Pruefer needs more bake time in the dev setup before it ships as a distributed artifact — see the issue's own top-of-body note. The `fabrik:cruise` label (not `fabrik:yolo`) was on this issue for that reason: cruise advances stages but does not auto-merge, leaving the actual merge timing to a human decision made separately from this ADR.

## Decisions

### 1. Version stamping is duplicated into `pruefer/version.go`, not shared

`cmd/version.go`'s `Version` var and its `versionWithSHA`/`appendDirtyIfModified` helpers live in package `cmd`, which imports `engine` (`cmd/root.go`). Pruefer cannot import `cmd` without crossing the ADR-1113 boundary. The ~70 lines of version-stamping logic are duplicated into `pruefer/version.go` verbatim instead, following the precedent ADR-1113 Decision 6 already set for `pruefer/procattr_*.go` (small, stable OS-primitive code duplicated across a hard architectural boundary rather than adding a third shared package). Version-stamping logic changes rarely and has no daemon-specific behavior to diverge on, so the duplication-drift risk ADR-1196 weighed against for the self-upgrade logic itself doesn't apply here.

### 2. `DevBuildConfig.Dir` points at `cmd/pruefer`, not the checkout root

Fabrik's `main.go` lives at the repo root, so `engine/upgrade.go` passes `Dir: e.fabrikDir` (the checkout root) straight through, and `go build -o exe .` builds the right package. Pruefer's `main.go` lives at `cmd/pruefer/`. Pointing `Dir` at the checkout root would make `CheckAndRebuildDev`'s hardcoded `go build -o exe .` silently build a **fabrik** binary and write it to Pruefer's own executable path — a `go vet`/`go test` pass would not catch this.

Instead, `pruefer/upgrade.go`'s `devBuildDir` helper resolves `Dir` to `<FabrikDir>/cmd/pruefer`. Every git subcommand `CheckAndRebuildDev` runs (`rev-parse`, `fetch`, `pull --ff-only`, `remote get-url`) resolves correctly from any subdirectory of the checkout — git walks up to find `.git` — so this requires zero changes to `internal/selfupgrade`, keeping that package's "no change beyond what's needed to call it from Pruefer" scope intact.

### 3. The release-check client is a dedicated, unauthenticated `github.Client`

Pruefer's only GitHub credentials are per-`watched_repos`-owner App installation tokens (`Daemon.Clients`, built by `BootstrapMulti`). There is no guarantee `handarbeit` is even a watched owner, and App installations can be scoped to "selected repositories," which may not include `fabrik` itself. Repurposing a review-owner's token for an unrelated cross-repo release check would also be a scope violation of that token's purpose.

`checkReleaseUpgrade` constructs its own `github.NewClientWithBaseURL("", releaseAPIBaseURL)` (an unauthenticated client in production; `releaseAPIBaseURL` is a test-only seam pointing at an httptest server). `fabrik` is a public repo, so an unauthenticated client is sufficient for both the `FetchLatestRelease` call and the asset download. Accepted tradeoff: GitHub's lower unauthenticated rate limit. If `handarbeit/fabrik` ever goes private, this path breaks with no fallback credential — out of scope to solve here, but worth flagging for whoever hits it.

### 4. `--auto-upgrade` defaults to off

Mirrors `cmd/root.go`'s `-auto-upgrade` flag default, despite this issue's own framing that Pruefer's staleness is uniquely invisible (no board, no issues) compared to Fabrik's. This is a new, lightly-battle-tested code path in a daemon with no human-in-the-loop visibility of its own; an operator should opt in deliberately via `--auto-upgrade` / `PRUEFER_AUTO_UPGRADE` / `auto_upgrade: true`, rather than the daemon silently starting to replace its own binary the moment this ships. Consistency with Fabrik's existing convention also matters more than closing the visibility gap by default — the README documents the recommendation to enable it explicitly.

### 5. The upgrade check runs at the poll boundary, throttled to 30 minutes

`Daemon.poll(ctx)` already calls `wg.Wait()` before returning, so **every return from `poll()` guarantees zero in-flight reviews** — unlike Fabrik's engine, which spans async workers across multiple poll cycles and needs `HasInFlightWorker()`/idle-count machinery to establish the same guarantee. Placing the upgrade check in `Daemon.Run`'s loop, immediately after `d.poll(ctx)` returns, satisfies the "never swap mid-review" requirement by construction — re-exec'ing mid-review would orphan an ephemeral clone and a running `claude` subprocess with no review posted.

The 30-minute throttle (`upgradeCheckInterval`, tracked via a local `time.Time` in `Run` — not a `Daemon` field, since `Run` is the sole sequential caller) is a rate/cost control, not a safety requirement: Pruefer's default poll interval is 120s, which would otherwise mean a `git fetch`/GitHub-Releases-API hit roughly every 2 minutes.

### 6. Darwin codesign requires no new call site

`resignDarwinBinary` (`internal/selfupgrade/release.go`) already runs unconditionally inside `PerformReleaseUpgrade` itself, generic since ADR-1196. Wiring Pruefer to `PerformReleaseUpgrade` automatically exercises it on darwin/arm64 — confirmed by reading the call graph; no separate call site or Pruefer-specific wiring was needed. The dev-rebuild path (`CheckAndRebuildDev`) never calls `resignDarwinBinary` — by design: it builds the binary locally with `go build`, which never hits the AMFI trust-cache issue that ad-hoc resigning a *downloaded* binary is there to work around.

### 7. Release cadence: one shared tag ships both `fabrik` and `pruefer`

`.goreleaser.yaml` gains a second `builds`/`archives` pair (`id: pruefer`, `main: ./cmd/pruefer`, same darwin/linux × arm64/amd64 matrix and ldflags-version-stamp shape as `fabrik`, its own `hooks.post` block — goreleaser hooks are per-build-id, not global, so the `verify-release-artifact` VCS-cleanliness check had to be duplicated onto the new build entry). Both binaries publish from the same tag/release.

This is the simplest option and matches the monorepo — no second release pipeline, no cross-repo tag coordination. Accepted tradeoff: every Fabrik-only release republishes an unchanged Pruefer binary (and vice versa). An independent tag/release flow per binary was considered and rejected: it would require a second `release.yml`-equivalent workflow and a second versioning scheme, for a cost (occasional redundant Pruefer republish) that's cheap compared to the coordination overhead of two release trains in one repo.

## Consequences

**Positive:**
- A release now publishes a `pruefer` binary for the same platform matrix as `fabrik`, with `pruefer --version` reporting a stamped release tag or `dev(<sha>)` for a source build — closing the artifact gap that made auto-upgrade impossible before this issue.
- The poll-boundary placement gives Pruefer a stronger "never mid-review" guarantee than Fabrik's own engine needs comparable machinery for — a structural property of Pruefer's simpler, fully-synchronous-per-cycle poll loop, not something this issue had to build.
- `pruefer/` and `cmd/pruefer/` still import no `engine` symbols (`go list -deps ./pruefer/... ./cmd/pruefer/... | grep engine` is empty), preserving ADR-1113's boundary.

**Negative / Trade-offs:**
- Version-stamping logic now exists in two places (`cmd/version.go`, `pruefer/version.go`) that must be kept in sync by hand if the stamping scheme ever changes — an accepted, ADR-1113-precedented cost for a hard architectural boundary.
- The unauthenticated release-check client has no fallback if `handarbeit/fabrik` ever goes private or unauthenticated rate limits prove too tight in practice.
- Every Fabrik-only release republishes an unchanged Pruefer artifact (and vice versa) under the shared-tag decision.
- `--auto-upgrade` defaulting off means an operator who doesn't read the README continues to run a Pruefer that never self-upgrades — the exact problem this issue exists to fix, until they opt in. Documented prominently in `cmd/pruefer/README.md` as a recommendation, not a default.

## Related Work

- `adrs/1113-pruefer-v1-architecture.md` — the no-`engine`-import boundary this issue respects, and Decision 6's duplication precedent for version stamping.
- `adrs/1196-extract-self-upgrade-package.md` — the extraction this issue builds on; explicitly named "wiring Pruefer to `internal/selfupgrade` and adding Pruefer to the release matrix" as this issue's follow-up.
- `engine/upgrade.go` — the wiring model `pruefer/upgrade.go` mirrors, adapted for Pruefer's simpler synchronous poll loop.
- `cmd/pruefer/README.md` — documents both deployment modes (dev rebuild vs. release download) and the `--auto-upgrade` recommendation.
- #1189, #1190 — the changes a stale, unupgraded Pruefer daemon silently missed in production, motivating this issue.
