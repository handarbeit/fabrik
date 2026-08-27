# ADR 1233: Pruefer Multi-Installation Auth Derived from `watched_repos`

**Date**: 2026-07-28
**Status**: Accepted (Decision 1's security framing superseded — see Amendment below)
**Issue**: #1233 — derive App installations from `watched_repos` (multi-org, one daemon)

## Amendment (2026-08-27, #1641)

Decision 1's core claim — **"this is also the whole security control": the set of
installations Pruefer ever tokenizes equals exactly the set of owners in
`watched_repos`, nothing more** — is superseded. #1253 replaced the shared,
publicly-installable App (`handarbeit-pruefer`) this ADR was written against with a
per-operator dedicated App, bootstrapped via manifest flow
(`internal/githubauth.Reconciler`); `pruefer/auth.go`'s `BootstrapMulti`/`AuthSet`
(the code this ADR describes) no longer exists. #1641 then inverted discovery
direction entirely: `internal/githubauth.Reconciler.Derive` now discovers and mints
**every** installation of the operator's own App unconditionally, regardless of
whether `watched_repos` names its owner — the opposite of Decision 1's "only ever
mints a token for an owner that appears in `watched_repos`."

This is safe, not a regression, because the *reason* Decision 1's allowlist existed
— a stranger installing the same shared App must never enlist this operator's daemon
— is now satisfied structurally by App identity instead: `GET /app/installations` is
JWT-scoped to the calling App, so it is physically incapable of returning another
App's installations. Once every operator runs their own dedicated App, "installed on
it" and "mine to review" are the same statement, and an owner-derived-from-`watched_repos`
allowlist adds no additional protection — see
[adrs/1641-pruefer-installation-derived-repo-discovery.md](1641-pruefer-installation-derived-repo-discovery.md)
for the full rationale and `internal/githubauth`'s own containment regression test
(`TestDerive_Containment_NeverContactsOtherAppsInstallations`).

`watched_repos` is not removed — it becomes an optional, operator-preference
narrowing filter over the installation-derived set (never a widening, never the
containment boundary) — see ADR-1641's R3.

What this amendment does **not** touch, because #1253/#1641 retained and reused it
rather than replacing it:
- **Decision 2** (one `*github.Client`/`*Auth` per owner, not a token-provider inside
  `github/client.go`) — the owner-scoped client-map shape is unchanged; `Reconciler.clients`
  is exactly this pattern under a new name.
- **Decision 3** (`RunRefreshLoop` per distinct `*Auth`) — `Reconciler.RunRefreshLoops`
  is the direct successor, same dedup-by-pointer-identity mechanism.
- **Decision 4** (`github_app_installation_id` as a legacy pin/escape hatch) — preserved
  byte-for-byte; ADR-1641 confirms this pinned path is fully exempt from the R1
  discovery inversion, exactly as it was exempt from Decision 1's `watched_repos`-driven
  discovery here.
- **Decision 6** (pagination gap in `FetchAppInstallations`/`FetchInstallationRepositories`,
  deferred at the time) — now addressed: both functions paginate for real as of #1641,
  since installation enumeration became the *primary* discovery path rather than a
  verification check against a short list, making silent under-enumeration a
  materially worse failure mode. See ADR-1641.

Decision 5 (`repository_selection: selected` handled via `FetchInstallationRepositories`)
is subsumed by `Derive`, which now calls that same endpoint unconditionally for every
installation regardless of mode — see ADR-1641.

## Context

ADR-1113 Decision 1 authenticates Pruefer to **one** GitHub App installation per daemon: `github_app_installation_id` if set, or auto-discovery when the App has exactly one installation — zero or multiple installations was a hard bootstrap error. That was correct for a single-org deployment, but the App (`handarbeit-pruefer`) is now installed on four accounts (`handarbeit`, `verveguy`, `shadoworg`, `liminisapp`). Two consequences followed directly from the single-installation design:

1. Auto-discovery now hard-errors on every fresh start, since more than one installation exists.
2. Covering all four orgs required four separate daemons, one per installation, because an installation token is strictly scoped to that account's repos — `handarbeit`'s token cannot read a `verveguy` repo. A single `watched_repos` list spanning owners could never work against one token.

The operator wants one daemon, driven by the `watched_repos` list they already maintain, covering repos across every org the App is installed on.

This ADR amends/supersedes ADR-1113 Decision 1's single-installation resolution: installation resolution is now **derived from `watched_repos`** rather than pinned or auto-selected once per daemon.

## Decisions

### 1. Installations are resolved per distinct owner in `watched_repos`, not once per daemon

`BootstrapMulti` (`pruefer/auth.go`, replacing `Bootstrap`) groups `Config.WatchedRepos` by owner (`distinctOwners`) and, for each distinct owner, resolves that owner's installation by matching `AppInstallation.Account == owner` against `FetchAppInstallations`'s result. One token is minted per distinct owner required, not per repo and not for every installation of the App.

**This is also the whole security control.** The set of installations Pruefer ever mints a token for, or contacts, equals exactly the set of owners appearing in `watched_repos` — nothing more. Pruefer never enumerates "every installation of the App" and acts on all of them. An account that installs the (public) App but is never named in any operator's `watched_repos` is never tokenized and never contacted — structurally, not by a separate allowlist check that could drift out of sync with config. This is what makes registering the App as installable-by-anyone safe: "only act on my own accounts" falls directly out of what's listed in `watched_repos`, with no additional mechanism to keep correct. `TestBootstrapMulti_MultiOwnerMintsPerOwnerAndNeverTouchesStranger` asserts this directly — a "stranger" installation present in the mocked `FetchAppInstallations` response but absent from `watched_repos` receives zero requests.

An owner with **no** matching installation is a hard bootstrap error naming the owner and how to fix it (`"no GitHub App installation found for owner %q ... install the app on %q"`) — consistent with ADR-1113's existing bootstrap-error style, just re-scoped from "the daemon's one installation" to "this one owner's installation."

### 2. One `*github.Client` per owner, not a token-provider inside `github/client.go`

Two mechanisms were available: (a) one `*github.Client` (and its own `*Auth`) per owner, reusing `SetToken`/`RunRefreshLoop` unchanged, or (b) threading an owner/request context through `github.Client`'s request core (`do`/`doWithAccept`/`graphqlRequest`) so a single client selects the right token per call.

(a) was chosen. It confines the change to `pruefer/`: `AuthSet.Clients map[string]*gh.Client` holds one client per owner, and `Daemon.Clients map[string]GitHubLister` (replacing the single `Daemon.Client`) looks up the right client by owner in the poll loop (`daemon.go`'s `poll()`). (b) would have reached into `github/client.go`'s and `github/rest.go`'s shared request plumbing — used by Fabrik's own engine (board/PR/label calls) well beyond Pruefer — coupling a Pruefer-specific multi-tenancy concern onto code with a much broader blast radius, for no offsetting benefit: every method Pruefer calls (`ListOpenPRs`, `FetchPRDiff`, `FetchPRReviews`, `SubmitPRReview`, plus the clone token via `client.Token()`) already takes `owner, repo` explicitly, so the owner is known at every call site regardless of which mechanism routes the token.

`ReviewPR`'s signature is unchanged — it already accepted one client per call; routing means only "which client `daemon.go` passes in," not a change to `ReviewPR` itself.

### 3. Per-installation token refresh stays exactly `Auth.RunRefreshLoop`, dispatched once per distinct `*Auth`

`AuthSet.RunRefreshLoops` starts one `RunRefreshLoop` goroutine per distinct `*Auth` in `AuthSet.auths`, deduplicated by pointer identity. `RunRefreshLoop` itself is untouched: each loop's failure handling (log + retry after `tokenRefreshRetryDelay`, current token stays valid until real expiry) was already fully self-contained per `*Auth` instance before this issue — becoming a multi-installation daemon only required running N independent instances of that already-independent loop instead of one, not redesigning its resilience contract. `TestAuthSet_RunRefreshLoops_IndependentPerInstallation` asserts a forced refresh failure on one installation has zero effect on another's continuing to refresh successfully.

Deduping by `*Auth` pointer (not by owner) matters for the pinned path below: several owners sharing one pinned installation must get exactly one refresh goroutine, not one redundant goroutine per owner independently re-minting the same token.

### 4. `github_app_installation_id` becomes a legacy pin/escape hatch, not a requirement

When set (non-zero), `BootstrapMulti` skips owner-derived discovery entirely: it mints one token onto one client and maps every distinct watched owner to that same client, byte-for-byte reproducing pre-issue single-installation behavior (including skipping the `repository_selection` check in Decision 5 below — that check only applies on the discovery path). This preserves existing single-org deployments and the documented escape hatch unchanged; a genuinely wrong owner behind a pin still fails loudly (403) at review time, the same as it always has.

When unset (the default, `0`), resolution is fully repo-derived per Decision 1. Zero watched repos plus no pin mints zero tokens and contacts zero installations — the security property in Decision 1 applied to its own degenerate case.

`TestBootstrapMulti_ExplicitInstallationIDSkipsDiscovery` and `TestBootstrapMulti_SingleOwnerAutoDiscovered` cover both paths; existing single-owner/pinned-config test assertions from before this issue continue to hold under the new construction API.

### 5. `repository_selection: selected` exclusion is a hard bootstrap error, checked eagerly, discovery-path-only

`AppInstallation` gained a `RepositorySelection` field (`"all"` or `"selected"`, decoded from `GET /app/installations`'s existing response — previously discarded). For an owner resolved via discovery whose installation is `selected`-mode, `BootstrapMulti` eagerly calls the new `FetchInstallationRepositories` (`GET /installation/repositories`, authenticated with that installation's own token — not the App JWT, since this endpoint is scoped to installation identity) and hard-errors naming the specific watched repo if it isn't in the accessible set. Eager-at-bootstrap was chosen over lazy/on-first-403 to produce a clear, actionable startup error consistent with the issue's "actionable bootstrap errors" requirement, rather than surfacing the same problem only after a live review-time 403. This check is skipped on the pinned-legacy path (Decision 4) — the pin's contract is byte-for-byte pre-issue behavior, which never performed this check.

### 6. Pagination gap in `FetchAppInstallations`/`FetchInstallationRepositories` is a known, deferred limitation

Neither `GET /app/installations` nor `GET /installation/repositories` is paginated by this change — both default to GitHub's standard page size (~30). At today's install count (4) and typical `selected`-mode repo counts this is not a live bug, but it is a latent gap: an owner resolvable via a page GitHub doesn't return in the first response would produce the same "no installation found" error as a genuinely missing installation. Explicitly out of scope for this issue (per its own scope section); noted here rather than silently punted so a future installation-count increase past a page boundary has a paper trail.

## Consequences

**Positive:**
- One daemon now covers every org/account the App is installed on, driven entirely by the `watched_repos` operators already maintain — no more one-daemon-per-installation, no more `github_app_installation_id` becoming mandatory the moment a second installation exists.
- The public-App safety property is structural and testable (`TestBootstrapMulti_MultiOwnerMintsPerOwnerAndNeverTouchesStranger`), not a convention operators must remember to uphold separately.
- Change is confined to `pruefer/` plus two additive fields/functions in `github/app.go`; `github/client.go`'s shared request core — used by Fabrik's engine — is untouched.
- Single-owner and pinned configurations behave exactly as before; the escape hatch for forcing one installation regardless of owner still exists.

**Negative / Trade-offs:**
- `AuthSet.Clients` holds N `*github.Client` instances instead of one; `Daemon.Clients` is now a map keyed by owner instead of a single field — every call site that used to reach `d.Client` directly (the poll loop, the `RateLimitReporter` type assertion) had to become owner-aware.
- `RateLimitSnapshotEvent`'s single `Stats` field became `{Owner, Stats}`; the TUI footer shows whichever owner's snapshot arrived most recently rather than an aggregate — a minor, cosmetic-only behavior change with no test/behavior contract depending on cross-owner aggregation.
- A `selected`-mode installation now costs one extra API round-trip at bootstrap (only when that mode is actually encountered, and only on the discovery path).
- Pagination remains unaddressed (Decision 6) — a real but currently inert gap.

## Related Work

- ADR-1113 (`adrs/1113-pruefer-v1-architecture.md`) Decision 1 — the single-installation design this ADR amends/supersedes. Decisions 2–9 of that ADR (review-state mechanism, ephemeral clone, diff-size guard, etc.) are unaffected; this ADR only revises how installation tokens are resolved and routed.
- ADR-1189 (`adrs/1189-pruefer-inline-review-comments.md`) — orthogonal; inline comment anchoring has no interaction with which token submitted the review.
- `cmd/pruefer/README.md` — Setup step 7, the config example, and the configuration reference table's `github_app_installation_id` row were updated in the same change to describe owner-derived resolution and the public-App-safety property.

**References:** [cmd/pruefer/README.md](../cmd/pruefer/README.md)
