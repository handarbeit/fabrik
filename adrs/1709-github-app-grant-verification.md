# ADR 1709: GitHub App Grant Verification (Granted vs. Requested Permissions)

**Date**: 2026-09-13
**Status**: Accepted
**Issue**: #1709 — `/pruefer review` acknowledgment reactions will 403; App has `issues: read`, needs `issues: write`

## Context

Pruefer's `/pruefer review` acknowledgment reactions (👀/🚀, `AcknowledgeForceReview`/`MarkForceReviewsProcessed` in `pruefer/review.go`) POST to GitHub's Issue Comments reactions endpoint, which requires `issues: write`. The live `handarbeit-pruefer` App holds only `issues: read` — a hand-created App that predates `internal/githubauth/manifest.go`'s `buildManifest`, which already requests the correct scoped set. The feature has never fired in production (`forceReview` is gated on a `/pruefer review` comment nobody has sent yet), so nothing is currently broken — but the first real use would 403, non-fatally logged and silently unnoticed by the user who sent the comment.

The deeper problem this issue surfaces: `GET /app` (what `buildManifest`/`FetchAppSlug` and this codebase's only prior App-auth REST surface ever call) reports what an App **requests**, not what an installation has actually been **granted**. GitHub App permission changes never take effect on an existing installation automatically — raising a permission on the App's settings page only takes effect once the resulting permission-change request is separately approved on each installation. Only `GET /app/installations` (list) or `GET /app/installations/{id}` (single) report the actually-granted set. Nothing in `internal/githubauth` or Pruefer had ever called either endpoint's `permissions` field, or compared it against anything — so a scope mismatch like this one is invisible until a 403 at first use. The same trap applies to any future App-auth consumer, including #770's engine identity.

Raising the App's `issues` permission to `write` and approving it on all 5 existing installations (handarbeit, verveguy, liminisapp, shadoworg, kolfadser1) is an external, human, GitHub-UI administration action — outside this repository's code and outside what a pipeline-driven PR can perform. It is **not** part of this issue's code deliverable; it is tracked as a manual follow-up. This ADR covers only the code deliverable: a grant-verification check that would have caught this drift at startup instead of at first use, and that verifies the manual fix once applied.

## Decisions

### 1. Read granted permissions from the installation resource, never `GET /app`

`AppInstallation` (`github/app.go`) gains a `Permissions map[string]string` field, decoded from both `GET /app/installations` (list) and a new `GET /app/installations/{id}` (single) call — GitHub returns the same resource shape from either endpoint, so both decode through one shared `rawInstallation` type. `GET /app`'s own `permissions` field (what `FetchAppSlug` already calls, unchanged) is never read for this purpose — AC3's explicit requirement, and the whole reason this drift was invisible before.

### 2. A new `FetchAppInstallation` call, not just a field on the list response

Pruefer's pinned-installation compat mode (`github_app_installation_id` set — very plausibly `handarbeit-pruefer`'s own actual deployment shape, per this issue's own evidence that it was hand-created rather than manifest-flow-created) never calls the list endpoint at all: `Reconcile`'s pinned branch and `derive`'s pinned early-return (`derivedSetForPinned`) both skip discovery entirely and go straight to minting a token for the one known installation ID. A field added only to the list response's decoding would leave this exact mode — the one this issue exists because of — permanently unchecked. `github.FetchAppInstallation(baseURL, jwt, installationID)` (`GET /app/installations/{installation_id}`, JWT-authenticated) closes that gap, following the same `appRequest`/pagination/error-wrapping conventions (`ErrAppUnauthorized`/`ErrNotFound`) as `FetchAppSlug`/`FetchAppInstallations`/`MintInstallationToken` in the same file.

### 3. Ordinal comparison, not string equality

GitHub's permission levels are cumulative: `admin` implies `write` implies `read`. `internal/githubauth/permissions.go`'s `permissionOrdinal` ranks `none=0 < read=1 < write=2 < admin=3` (an unrecognized/empty level ordinals to 0), and `checkGrantedPermissions` compares granted-ordinal against required-ordinal per permission. A plain string-equality comparison would false-positive whenever a scope is granted higher than required (e.g. flagging `issues: admin` as insufficient against a required `issues: write`) — a correctness requirement, not a style choice, confirmed by a dedicated non-regression test at both the unit (`checkGrantedPermissions`) and integration (`Derive`) level.

### 4. Required-permission set is caller-supplied, never hardcoded in `internal/githubauth`

`internal/githubauth` must never import `pruefer` (see `doc.go`). `checkGrantedPermissions` takes `required map[string]string` as a parameter; nothing in the comparison logic hardcodes Pruefer's four scopes. `Options.RequiredPermissions` threads a caller's required set into `Reconciler.requiredPermissions`. For Pruefer specifically, `internal/githubauth/manifest.go`'s `buildManifest` (what a freshly manifest-created App **requests**) and the new `PrueferRequiredPermissions()` accessor (what the grant check **requires**) are collapsed onto one package-level `requiredPermissions` var — closing, for this one caller, the exact drift this issue is about: the manifest and the check can no longer independently diverge. `pruefer/execute.go` passes `githubauth.PrueferRequiredPermissions()` into `Reconcile`. A future second caller (#770's engine identity) supplies its own set; the comparison logic itself has no Pruefer-specific knowledge.

### 5. Soft, non-fatal — never a hard startup failure

Every existing verification step in `internal/githubauth`'s discovery path (`verifyRepoAccess`'s per-repo authorization gaps, `logDerivedSet`'s truncation/cap warnings, `guideMissingInstallations`) is soft: logged loudly, never blocking `Reconcile`'s return. The grant-verification check follows the same convention — `logPermissionShortfalls` logs one `!`-prefixed line per shortfall (naming the installation, the permission, the level required, and the level actually granted), and never fails `Reconcile` or `Derive`. A hard failure here would mean an operator's already-broken `handarbeit-pruefer` App (missing `issues: write` today) could no longer even *start* Pruefer until the manual App-permission fix lands — inconsistent with this issue's own framing that the underlying condition has never fired in production and is non-fatal by design.

### 6. Dual wiring for pinned mode: `Reconcile` (startup) and `derive`'s pinned branch (every re-derivation)

`derive`'s non-pinned discovery loop already iterates every installation and — once `FetchAppInstallations`' response carries `Permissions` — computes `DerivedInstallation.PermissionShortfalls` for free, no extra API call. Pinned mode has no equivalent loop to attach to (Decision 2), so a new `verifyPinnedGrants` helper (builds its own JWT, calls `FetchAppInstallation`, logs shortfalls) is called from **two** sites: once from `Reconcile`'s pinned branch (covers AC2's literal "starting Pruefer" wording) and again from `derive`'s pinned early-return (covers R3's "re-run without restart" requirement — a pinned Reconciler never reaches the discovery loop, so without this second call site, only Reconcile's very first invocation would ever check, and confirming a manual fix in production would require a full process restart). Both call sites share the same helper so the pinned and re-derivation paths cannot drift on what "checking" means.

### 7. R3 is the same code as R2, not separate

Re-running the grant check against the live installations, after the manual permission raise (R1) is applied in production, is how that raise gets verified — this ADR does not introduce a separate verification code path for that; it is Decision 6's re-derivation-triggered re-check, exercised again once the App administrator's manual fix lands.

## Consequences

- **AC1** (the acknowledgment feature working end-to-end) is not made true by this PR's code — it depends entirely on the manual, out-of-scope App-permission raise and its approval on all 5 installations. This ADR's code deliverable is AC2/AC3: the check that makes a scope mismatch loud instead of silent.
- The required-permission list remains a hand-maintained correspondence with Pruefer's actual API usage: a future new Pruefer API call needing a permission not yet added to `requiredPermissions` will pass this check while a genuinely new gap goes undetected. Collapsing the manifest's requested set and the check's required set onto one var (Decision 4) closes the drift between *those two*, but does not make the required set self-deriving from actual code paths — that remains a maintenance dependency, not a defect in the check itself.
- A future second `internal/githubauth` caller (#770) reuses `checkGrantedPermissions`/`logPermissionShortfalls`/`Options.RequiredPermissions` unmodified, supplying its own required set — no Pruefer-specific change needed in this package to support it.

## Prior Art

- ADR-1253 (`1253-github-app-manifest-auth-reconciler.md`) established the 9-step `Reconcile` state machine this issue's check extends as an additive step, consistent with that ADR's own soft-verification precedent (step 8, repo access, is explicitly non-fatal).
- ADR-1233 (`1233-pruefer-multi-installation-auth.md`) established the pinned-installation compat mode (`github_app_installation_id`) Decision 2/6 above are built around.
- ADR-1641 (`1641-pruefer-installation-derived-repo-discovery.md`) established `Derive`/`DerivedInstallation`/`logDerivedSet`, which Decision 6's non-pinned path extends with `PermissionShortfalls` as a natural sibling to `RepoCount`/`MintError`/`RepoListError`.
- `internal/githubauth/installations.go`'s `verifyRepoAccess` is the direct precedent for "soft, discovery-path-only" verification this issue's check follows one level up (permissions instead of repos).
