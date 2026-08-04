# ADR 1391: Support GitHub Enterprise Server via dual-endpoint client and host-aware plumbing

**Status:** Accepted
**Date:** 2026-08-04
**Issue:** [#1391](https://github.com/handarbeit/fabrik/issues/1391)

## Context

Fabrik was hardwired to github.com in four places, none of which is the GraphQL
schema itself. A capability probe (`scripts/ghes-probe.sh`) run against a live GHES
3.19.8 instance found full API parity for everything Fabrik uses — all 16 mutations
present, every query field present except `Issue.trackedIssues` (zero Fabrik callers).
That result inverted the expected design: no adapter layer, schema introspection, or
runtime capability negotiation was needed. The remaining work was pure endpoint/host
plumbing.

The core defect: `github/client.go`'s `graphqlRequest` derived the GraphQL endpoint by
concatenation, `c.baseURL+"/graphql"`. This holds on github.com, where REST is
`https://api.github.com/...` and GraphQL is `https://api.github.com/graphql` — the
GraphQL path genuinely is a suffix of the REST base. On GHES the two live at unrelated
paths: REST is `https://<host>/api/v3/...`, GraphQL is `https://<host>/api/graphql`.
The probe confirmed this empirically — `POST https://<host>/api/v3/graphql` returns
404; `POST https://<host>/api/graphql` returns 200. A single `baseURL` field cannot
express both.

Three more sites assumed github.com: the engine bypassed `NewClientWithBaseURL`
entirely (`gh.NewClient(cfg.Token)`, hardcoding `defaultBaseURL`); `buildCloneURL`
built literal `github.com` clone URLs; and `buildClaudeEnv` never emitted `GH_HOST`,
so every stage worker's `gh` CLI invocations (e.g. `fabrik-validate`'s Pre-Completion
Gate, which runs `gh pr view` on every invocation) would target github.com even when
the engine talks to a GHES instance — the same silent-wrong-target defect class as
#1346's ambient `ANTHROPIC_API_KEY`, landing in the same function.

Research also surfaced a risk not named in the issue: `engine/upgrade.go`'s
`checkReleaseUpgrade` passes `e.client` — the engine's own board/PR client — into
`selfupgrade.PerformReleaseUpgrade` to fetch Fabrik's own binary release from
`handarbeit/fabrik`, which lives on github.com always, never on a customer's GHES
instance. Once the engine's client is repointed at a configured GHES host, this call
would silently break (404) or, worse, misquery an unrelated repo of the same name on
the GHES instance.

## Decision

**Dual-endpoint client via a second field, not a changed `baseURL` meaning.**
`github.Client` gains a `graphqlURL` field alongside the existing `baseURL`.
`graphqlRequest` uses `c.graphqlURL` instead of deriving it inline. `NewClient` and
`NewClientWithBaseURL` keep deriving `graphqlURL = baseURL + "/graphql"` — unchanged
behavior, byte-identical to today. This preserves the ~150 existing
`github/*_test.go` call sites that construct a client via `NewClientWithBaseURL` against
a single `httptest.NewServer` and implicitly rely on that derivation to serve both
REST and GraphQL paths.

A new constructor, `NewClientForHost(token, host string)`, is the only path that
derives REST and GraphQL independently: `baseURL = https://<host>/api/v3`,
`graphqlURL = https://<host>/api/graphql`. `host` is a bare hostname — no scheme, no
trailing slash — and the constructor defensively strips both if a caller passes a
full URL by mistake. Every REST call site in the `github/` package already builds its
URL as `c.baseURL + "/repos/..."` etc.; once `baseURL` itself is GHES-aware, all ~40 of
those sites are correct with zero per-site changes. `graphqlRequest` is the *only*
GraphQL call site in the codebase, so the dual-endpoint fix is small and contained.

**A single new config value, threaded through the existing precedence ladder.**
`GHESHost` is one new string — `config.ProjectConfig.GHESHost` (yaml: `ghes_host`),
resolved via the same flag > `FABRIK_GHES_HOST` env > `config.yaml` ladder every other
setting uses (precedent: `GitSSH`/`FABRIK_GIT_SSH`). `config.NormalizeGHESHost` strips
a `https://`/`http://` scheme and trailing `/` once, so `cmd/root.go` (the daemon) and
`cmd/watch.go` (`fabrik watch`, a second, previously-unnamed bypass of the configurable
constructor Research found) share one `resolveGHESHost` helper rather than duplicating
the ladder logic. Absent configuration, `GHESHost` is `""` and every downstream
consumer below falls back to literal `"github.com"` — the default path is unchanged.

**Self-upgrade gets a dedicated, always-github.com client — endpoint and
credential both.** `Engine` gains a `releaseClient GitHubClient` field,
always constructed against `defaultBaseURL` regardless of `GHESHost`.
`checkReleaseUpgrade` uses `e.releaseClient` instead of `e.client`.
`NewWithDeps` (the test-construction path) sets `releaseClient = client`, so
every existing mock-based upgrade test is unaffected — `releaseClient` only
diverges from `client` in `New()` when a GHES host is actually configured.
This directly addresses the self-upgrade wrong-host risk Research surfaced:
Fabrik's own release always lives on `github.com/handarbeit/fabrik`, and now
always resolves there no matter what the managed project's host is.

The *credential* needed the same isolation as the endpoint, caught in review
after the initial Implement pass: `releaseClient` was first built with
`gh.NewClient(cfg.Token)`, which pins the URL to github.com but still sends
`cfg.Token` as the bearer credential. When a GHES host is configured,
`cfg.Token` authenticates *that* instance, not github.com — GitHub's REST
API rejects a request bearing an invalid token with 401 "Bad credentials"
rather than falling back to unauthenticated, so this would silently break
self-upgrade end to end on every GHES deployment. `releaseUpgradeToken(cfg)`
(`engine/upgrade.go`) returns `cfg.Token` unchanged on the default path and
`""` whenever `GHESHost` is set; `handarbeit/fabrik`'s releases are public,
so the unauthenticated fallback still works (just more tightly
rate-limited). Both `releaseClient`'s construction in `New()` and
`checkReleaseUpgrade`'s asset-download `Token` field go through this same
helper, so the two can't drift.

**A second concrete client for the version-floor preflight, not an interface
addition.** `Engine` also gains `hostClient *gh.Client` — the same underlying client
as `e.client`, just concretely typed, set only in `New()`. Adding `FetchInstalledVersion`
to the `GitHubClient` interface would have required updating `engine/mocks_test.go`
and every other implementer for a single startup-only, GHES-only call. Keeping it
concrete lets `checkGHESVersionFloor`'s comparison logic
(`checkGHESVersionFloorAgainst`) unit-test directly against a real `*gh.Client` +
`httptest`, with no engine-level mocking at all. `hostClient` is `nil` outside `New()`
(e.g. `NewWithDeps`-constructed test engines); the Engine-level method treats that
defensively as a no-op rather than panicking.

**Version floor: 3.19, major.minor only, fail-loud below it, warn-not-fail on fetch
error.** Startup, when `GHESHost` is configured, calls `hostClient.FetchInstalledVersion()`
(GET `<baseURL>/meta`'s `installed_version`, unauthenticated, no header changes needed
per the probe) and compares major.minor against the floor. A confirmed below-floor
version is a hard startup failure naming both the detected and required versions —
`checkAPIKeyHelper`'s existing fail-loud convention. A `/meta` fetch error (network,
auth) or an unparseable version string is a non-fatal warning instead, mirroring
`checkStageColumnAlignment`'s "log and skip" precedent for fetch failures:
connectivity/auth problems already surface loudly moments later when the project
board fetch runs, and conflating them here would misreport a network blip as "your
GHES is too old." Wired into `Run()` immediately after `checkAPIKeyHelper()`.

**Host threaded through the git-clone layer as a plain parameter, defaulting to the
github.com literal.** `buildCloneURL`, `ensureBareClone`, and `setCommitterIdentity`
each gain a `host string` parameter; an empty host falls back to the literal
`"github.com"` inside the function body, so every existing call site that doesn't yet
pass a host produces byte-identical output. The two `ensureBareClone` call sites in
`engine.go` pass `e.cfg.GHESHost`. `repoNameFromURL`/`ownerRepoDirFromURL` — the
*parsing* direction, confirmed by Research to contain no `"github.com"` literal at
all — needed no production change; they already parse generically by splitting on the
last `/`/`:`, so a GHES-host remote round-trips correctly today. Only a confirming
test was added.

**Commit-author noreply email: `<user>@users.noreply.<host>`, explicitly flagged as
unverified.** GitHub's published GHES admin documentation confirms the
*notification* reply-to MX-record convention (`noreply.[hostname]`) but does not
explicitly document the *private commit email* format's GHES equivalent — the closest
hit was a stale, github.com-shaped example in legacy (GHES 2.3-era) documentation. No
live GHES instance was available during Research to verify directly. Mirroring
github.com's own scheme with the configured host substituted is the only
self-consistent option that doesn't hardcode `github.com` into a GHES commit trail;
`docs/USER_GUIDE.md`'s GHES section and this ADR both flag it as unverified so a user
who hits a different convention on their instance has somewhere to report it.

**`GH_HOST`, not a Fabrik-invented name.** `buildClaudeEnv` emits `GH_HOST` — the `gh`
CLI's own established environment variable for pointing it at a non-github.com host —
alongside `GH_TOKEN`/`GITHUB_TOKEN`, sourced from a new `claudeGHHost` package var
mirroring the existing `claudeGHToken` pattern. Only emitted when non-empty; omitted
entirely (not empty-valued) when no GHES host is configured. It sits outside
`isAnthropicAuthNamespaceKey`'s scope entirely (not `ANTHROPIC_*`-prefixed, not a
`claudeCodeAuthSelectors` entry), so it needs no interaction with the #1346 scrub/
passthrough machinery, and none was added.

**No capability negotiation.** Consistent with the issue's explicit non-goal and the
probe's own finding: no schema introspection, no adapter layer, no runtime
feature-degradation. The version floor is the only gate, and it is a startup-time
version string comparison, not a per-field capability check.

## Consequences

- Absent any `ghes_host` configuration, every constructed URL, the client, clone
  URLs, and the subprocess environment remain byte-identical to pre-GHES Fabrik —
  asserted directly by tests pinning both the github.com default and an explicit
  GHES value in the same test case, not inferred from one or the other alone.
- A GHES-configured engine gets REST and GraphQL endpoints that are genuinely
  independent values (`<host>/api/v3/...` vs `<host>/api/graphql`), not a path-suffix
  relationship — the actual defect this ADR fixes.
- Self-upgrade is now host-isolated: a GHES-configured engine's `fabrik
  upgrade`/`AutoUpgrade` path always checks `handarbeit/fabrik` on github.com,
  regardless of `ghes_host`.
- `cmd/watch.go` was brought into scope alongside the daemon path (`engine/engine.go`)
  — both bypasses of the configurable constructor are fixed together via the shared
  `resolveGHESHost` helper, rather than leaving `fabrik watch` as a tracked gap.
- **Known, deliberate residual gaps, not fixed by this issue:** `checkStageColumnAlignment`'s
  human-facing project-board URL link and `checkURLRewrite`/`checkHTTPSCredentials`'s
  SSH-rewrite detection heuristics still hardcode `github.com`. Both are
  advisory/cosmetic only — no gating behavior — and fixing them would require
  GHES-specific project-URL conventions the issue doesn't specify. A GHES user may see
  a `github.com`-shaped link or a missed SSH-rewrite suppression; documented as a known
  limitation in `docs/USER_GUIDE.md` rather than left silently unaddressed.
- Webhook support and `projects_v2_item` availability on GHES remain unverified and
  out of scope — Fabrik already degrades to polling when webhooks are unavailable, so
  GHES is polling-first with no code path affected.
- FR-9's noreply-email domain convention (`<user>@users.noreply.<host>`) is a
  documented best guess, not verified against a live GHES instance's actual private
  commit-email behavior. If a user reports a different convention, that is new
  evidence for a follow-up, not a defect in this decision.
