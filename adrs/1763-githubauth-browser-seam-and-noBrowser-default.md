# ADR 1763: `internal/githubauth` Browser Test Seam and the Engine's Inverted `NoBrowser` Default

**Date**: 2026-09-16
**Status**: Accepted
**Issue**: #1763 — App auth opens a 404 browser window per reconcile — no NoBrowser control, unstubbable seam, name used as slug

## Context

`internal/githubauth`'s `guideMissingInstallations` (`derive.go`), reached from `Reconcile()`'s non-pinned discovery branch, logs a guided-install URL for every watched-but-uninstalled owner and, at most once per call, attempts to open it in a browser via the package-level `openBrowser` var. Two independent defects surfaced while three concurrent stage workers wrote engine-side App-auth tests (#1750/#1754/#1756): any test that reaches this branch — a `WatchedRepos` entry naming an uninstalled owner, no `AppInstallationID` pin — pops a real browser window during `go test`, repeatedly.

**D1**: the engine never set `Options.NoBrowser` at all — there was no config key, flag, or env var for it, unlike Pruefer's full `no_browser` surface (`pruefer/config.go`).

**D2**: `openBrowser` is unexported, so only tests inside `internal/githubauth` itself could stub it, even though the package's own `doc.go` documents it as designed for external callers.

**D3** (the original report's slug/name-conflation claim) did not survive Specify-stage code review and was confirmed void by Research's full-history `git log -S` trace: `guideMissingInstallations`'s `installURL` is built exclusively from the live-fetched `gh.FetchAppSlug` result, never from `Options.AppName` (which only ever feeds `manifest.go`'s `buildManifest`, a structurally separate code path). No code change was warranted for D3 — only a regression test locking in the existing-correct behavior.

Research additionally established that the engine's own production runtime never reaches `guideMissingInstallations` at all today: `setUpGitHubAppAuth`'s all-or-nothing App-auth config (`validateGitHubAppConfig`) means `AppInstallationID` is always non-zero when App auth is configured, so `Reconcile`'s pinned branch always returns before the non-pinned branch that calls it. The concretely reproducible trigger is exclusively tests — in `engine` or any other external package — that call `githubauth.Reconcile` directly without a pin.

## Decisions

### 1. `Options.OpenBrowser func(url string) error` — an additive override field, not an exported var

The two candidate seam designs were (a) export the existing `openBrowser` package var so external tests can reassign it directly, or (b) add a new `Options` field carrying an opener override, consulted in preference to the package var when non-nil.

(b) was chosen. An exported package var is global mutable state one layer up from where it is today — a test must remember to save/restore it (`defer func(){ openBrowser = old }()`), and a caller-side test author still has to know the var exists. An `Options`-scoped field is naturally scoped to a single `Reconcile` call, requires no restore, and satisfies AC3's literal wording ("without needing to know about `NoBrowser`") more directly: a caller sets one field on the `Options` it already constructs and gets a side-effect-free reconcile, full stop.

`guideMissingInstallations` resolves `opener := openBrowser; if opts.OpenBrowser != nil { opener = opts.OpenBrowser }` and calls `opener(installURL)` — the package var remains the production default; the field is purely an override.

### 2. `Options.NoBrowser`'s zero value is left unsafe, deliberately

`Options.NoBrowser`'s zero value (`false`) means "attempt to open a browser" — correct for Pruefer's own first-run-setup default (unset = browser opens, matching its existing behavior), but unsafe for any caller that zero-values `Options` without deliberately setting `NoBrowser: true`. R1's literal instruction to "plumb it into `githubauth.Options.NoBrowser` (that field already exists)" forecloses changing this field's name or polarity — so R3's seam (`OpenBrowser`) is the fix, not a NoBrowser default flip. R6's own regression test (`engine/github_app_auth_test.go`'s `TestReconcile_NonPinnedDiscovery_NoBrowserOpensViaOptionsSeam`) is written to prove this explicitly: it leaves `NoBrowser` unset and relies solely on `OpenBrowser` to stay side-effect-free.

### 3. `ManifestFlowOptions`/`RunManifestFlow`'s own `openBrowser` call site is untouched

The manifest-creation browser flow (`bootstrap.go`) is explicitly out of scope — it is legitimately interactive and user-initiated (`fabrik init --github-app`'s own setup-time `--no-browser` flag already covers it), and the engine's all-or-nothing App-auth config means the engine never reaches it in production. Threading `OpenBrowser` into `ManifestFlowOptions` too would be speculative generality with no AC requiring it.

### 4. Engine `NoBrowser` default is `true` (suppressed) — the inverse of Pruefer's

Pruefer's `NoBrowser` defaults to `false` (browser opens) because its first-run setup flow's entire point is walking an operator through App creation interactively. The engine is a long-running daemon, frequently headless, containerized, or on a remote box — an unconditional browser-open default is actively hostile there. `cmd/root.go`'s `--no-browser` flag defaults to `true`; `FABRIK_NO_BROWSER`/`.fabrik/config.yaml`'s `no_browser` (a `*bool`, mirroring `ProjectConfig.TUI`'s existing tri-state pattern) can re-enable it, gated by the same `explicitFlags`-checked flag > env > YAML precedence used throughout `cmd/root.go`. Guidance is still logged either way (R2) — only the automatic `open`/`xdg-open`/`rundll32` invocation is gated.

Because the daemon-level `--no-browser` and the setup-time `fabrik init --github-app --no-browser` flag are parsed by two different `flag.FlagSet`s dispatched at different points (`cmd/init.go` vs. `cmd/root.go`'s `flag.CommandLine`), the identical name is not a collision — the same precedent already exists for `--github-app-id` being reused across both entry points. `docs/USER_GUIDE.md` states plainly that these are two separate controls with inverted defaults.

### 5. D3 and R5: regression tests, not behavior changes

`internal/githubauth/reconciler_test.go`'s `TestReconcile_GuidedInstallURL_UsesSlugNotAppName` locks in that the guided-install URL is always built from the live-fetched slug, using an `Options.AppName` value deliberately distinguishable from the fake server's slug so the assertion can't pass vacuously. No production code changed for D3/R4 — Research's trace already proved the conflation doesn't exist.

R5 (whether/how often the reconcile path should attempt guided installation) is satisfied by documentation, not a code change: `openedInstallBrowser`'s existing per-`Reconcile`-call bound is already a per-process-lifetime bound in steady state, since both the engine (`New()`) and Pruefer (`execute.go`) call `Reconcile()` exactly once per process lifetime, and periodic re-derivation (`Reconciler.Derive`/Pruefer's `rederiveRepos`) never calls `guideMissingInstallations` at all.

## Consequences

- The engine's production runtime is unaffected by this issue's core defect today (the non-pinned branch is unreachable under its all-or-nothing App-auth config) — R1–R3's value is test hygiene for the concretely-observed pollution (D2) and future-proofing for Pruefer's own non-pinned/discovery mode (its primary intended use, per ADR-1641) and any future non-pinned engine mode.
- `engine/github_app_auth_test.go`'s `newFakeGitHubAppServer` gained `GET /app/installations` (list) and `GET /installation/repositories` handlers, needed only by the non-pinned discovery path R6 exercises — a fixture extension, not a behavior change to any existing pinned-path test.
- A future second `internal/githubauth` caller that wants a side-effect-free test gets the same `Options.OpenBrowser` seam for free, with no per-caller plumbing.

## Prior Art

- ADR-1253 (`1253-github-app-manifest-auth-reconciler.md`) originally documented `openBrowser` as "exposed as a package var so tests can replace it" — confirms the seam was designed for internal-package tests only, with no prior consideration of external-caller testability; this ADR is the first to address that gap structurally.
- ADR-1641 (`1641-pruefer-installation-derived-repo-discovery.md`) is why `guideMissingInstallations` exists in its current shape at all — a "preserved even though discovery itself is no longer driven by `watched_repos`" carry-over.
- ADR-1713 (`1713-engine-github-app-auth.md`) already states the manifest/browser bootstrap flow never runs under the engine's compat-mode-only usage — direct precedent for this ADR's Decision 4 default choice and the R5 reachability finding.
- `config/config.go`'s `ProjectConfig.TUI *bool` is the direct precedent for `NoBrowser *bool`'s tri-state YAML pattern (Decision 4).
