# ADR 1712: Parameterizing `internal/githubauth`'s Manifest Identity and Permissions

**Date**: 2026-09-13
**Status**: Accepted
**Issue**: #1712 — parameterize App identity/permissions and accept Client ID as JWT issuer

## Context

`internal/githubauth`'s own `doc.go` named a caller-agnosticity gap: `manifest.go`'s
`defaultAppName` ("pruefer"), `defaultAppHomepageURL`
(`github.com/handarbeit/fabrik`), and the specific permission set `buildManifest`
requests were all Pruefer-shaped constants — not `Options`/`ManifestFlowOptions` fields
a second caller could override. The engine (#770) is that second caller: it needs its
own App name, its own homepage, and its own permission set (notably
`organization_projects: write`, which Pruefer's set never requests at all). Reusing this
package unchanged would have created an App named "pruefer", homepaged at Fabrik's repo,
scoped to Pruefer's permissions — none of which the engine wants.

Separately, GitHub now recommends the App's Client ID over the numeric App ID as the JWT
issuer (both work — verified live 2026-09-13, a JWT with `"iss": "Iv23li…"` was
accepted). `BuildAppJWT`'s claims map was typed `map[string]int64`, so a string issuer
could not fit — and #770's own spec already names `GITHUB_APP_CLIENT_ID` as a
configuration key, ahead of the code.

This issue closes both gaps without changing Pruefer's own observable behavior at all
(R5/AC2/AC5): same App name, same homepage, same permissions, same JWT issuer.

## Decisions

### 1. Reuse `Options.RequiredPermissions` as the manifest's requested-permission set, rather than a new field

`#1709` made `requiredPermissions` (`manifest.go`) the single source of truth for both
what `buildManifest` requests and what `Options.RequiredPermissions`'s
grant-verification check (already-live installations) requires — closing a two-map
drift where a manifest could request one thing while nothing checked that an
installation actually held it. Adding a *second*, independently-set field for
"what a fresh App's manifest should request" (e.g. `Options.ManifestPermissions`) would
reopen exactly that drift for a second caller: nothing would stop a future edit to one
field without the other. Instead, `buildManifest` grows a `permissions map[string]string`
parameter fed by the same `Options.RequiredPermissions` value already threaded through
`ManifestFlowOptions` — one field per caller, serving both "what to request" and "what to
verify." Pruefer's own `pruefer/execute.go` call site already sets
`RequiredPermissions: githubauth.PrueferRequiredPermissions()` explicitly, so its
behavior is unaffected.

An empty/nil `permissions` argument (the case for every call that doesn't set
`RequiredPermissions`) falls back to `PrueferRequiredPermissions()` inside
`buildManifest` itself — one place that knows what "Pruefer's defaults" are, keeping
`Options`/`ManifestFlowOptions` themselves as plain pass-through data.

### 2. Two purely-new fields for App name and homepage: `Options.AppName`/`AppHomepageURL`, threaded through `ManifestFlowOptions`

No existing field was a candidate for reuse here — these are genuinely new identity
inputs. Both default (empty string) to `defaultAppName`/`defaultAppHomepageURL` inside
`buildManifest`, mirroring the permission-set fallback above.

### 3. Both `ManifestFlowOptions{...}` construction sites in `reconciler.go` forward the same three fields

`reconciler.go` builds `ManifestFlowOptions` at two independent sites: the first-run
bootstrap path (`loadOrBootstrapCredentials`) and the self-heal re-manifest path
(inside `Reconcile`, when a non-pinned `AppID` fails identity validation). Nothing in
the type system enforces the two stay in sync — a copy-paste risk both compile fine with
only one site updated. `TestReconcile_ManifestFlowOptionsForwardedIdentically` drives
both paths with identical non-default `Options` and asserts the `ManifestFlowOptions`
`runManifestFlow` actually receives are identical on `AppName`/`AppHomepageURL`/
`RequiredPermissions`, guarding this directly rather than relying on code review alone.

### 4. `BuildAppJWT`'s issuer parameter widens to `any`, with an internal type switch, rather than a typed `oneof` value or a sibling function

Three options were considered:

1. **Widen to `any`, type-switch internally** (chosen): `BuildAppJWT(issuer any, privateKey *rsa.PrivateKey)` accepts exactly `int64` (App ID) or a non-empty `string` (Client ID), erroring on anything else (including an empty string, and including other integer types like `int`/`int32` — only `int64` and `string` are accepted, so an untyped integer literal used at a call site must be cast explicitly). All six existing call sites (`reconciler.go` ×4, `derive.go`, `tokenauth.go`) pass an already-`int64`-typed variable (`appID`, `a.AppID`) — Go boxes a typed value into an `any` parameter with no cast and no behavior change, so R3's "keep all six working" holds by construction. The claims map widens from `map[string]int64` to `map[string]any`; `encoding/json` marshals an `int64` as a JSON number and a `string` as a JSON string identically either way.
2. A sibling function (e.g. `BuildAppJWTFromClientID`) taking a `string`, with the original `int64` signature untouched. Zero source changes anywhere, but two near-duplicate JWT-building code paths to keep in sync, and doesn't match the issue's framing of "`BuildAppJWT` accepts a string issuer."
3. A typed issuer value (a struct or interface distinguishing `AppID`/`ClientID`). Preserves compile-time safety but requires updating all six call sites for no behavioral gain.

Option 1 was chosen for zero call-site churn at the six real (typed-variable) call
sites, matching R3's explicit framing. The trade-off — an untyped literal like `12345`
defaults to `int`, not `int64`, when boxed into an `any` parameter, so any test using a
bare integer literal must cast it explicitly (`int64(12345)`) — surfaced immediately in
`TestBuildAppJWT_WellFormed` and was fixed there; it does not affect any production call
site, which all pass typed `int64` variables.

### 5. No `organization_projects: admin` anywhere (R4/AC4)

This falls out of Decision 1 automatically: nothing in the manifest-parameterization
work hardcodes `admin` for any permission, and `TestBuildManifest_
NeverRequestsOrganizationProjectsAdmin` asserts the default output never does either.
This is a consequence of the design, not a separate mechanism.

## Consequences

- A second caller (e.g. the engine, #770) can now supply its own `Options.AppName`,
  `Options.AppHomepageURL`, and `Options.RequiredPermissions` and get a manifest scoped
  to its own identity and permissions — `internal/githubauth` no longer hardcodes
  Pruefer's shape. Wiring the engine's actual identity/permissions in is separate,
  out-of-scope follow-up work (part of #770's decomposition); this issue only makes the
  mechanism pluggable.
- `BuildAppJWT` can mint a JWT from either a numeric App ID or a string Client ID. Which
  one a caller should actually use (e.g. reading `Credentials.ClientID`, already
  captured and persisted for every App today but never read back for this purpose) is
  also out of scope — this issue only makes `BuildAppJWT` itself accept both forms.
- `Options.RequiredPermissions` now does double duty (manifest request scoping *and*
  grant verification) for every caller, not just Pruefer. A future caller wanting these
  to diverge (request one set, verify against a different set) would need a new,
  separately-named field — deliberately not provided here, since that is exactly the
  drift #1709 fixed and this issue's Decision 1 was designed to keep closed.

## Prior Art

- ADR-1253 (`1253-github-app-manifest-auth-reconciler.md`) Decision 1 named this exact
  follow-through goal: "this keeps the door open for a future second self-hosted daemon
  to reuse the same reconciler without inheriting Pruefer's own concerns."
- ADR-1709 (`1709-github-app-grant-verification.md`) Decision 4 established
  `requiredPermissions`/`PrueferRequiredPermissions()` as the single source of truth this
  issue's permission-set parameterization must not re-fork.
- `internal/selfupgrade` is `doc.go`'s own cited precedent for "caller supplies identity,
  package logic stays generic," which this issue brings `internal/githubauth` in line
  with.
