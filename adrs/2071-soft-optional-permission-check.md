# ADR 2071: Soft, separate optional-permission check

## Status

Accepted

## Context

#2052 added `actions: write` as an optional GitHub App permission. Apps created before it lack the permission, and nothing told the operator: `fabrik init` and engine startup verify only the required set, and each optional feature silently falls back on `ErrForbidden`. GitHub offers no API for an App to gain a permission, so Fabrik can only detect the gap and guide the operator.

## Decision

- Engine startup (`setUpGitHubAppAuth`) and `fabrik init` (`runGitHubAppSetup`) make a **second** `Reconciler.VerifyGrants(OptionalGitHubAppPermissions())` call, after the fail-hard required check has passed, and print one advisory notice per shortfall. A failed read prints one warning line. Neither path ever fails.
- The permission names, levels and "what it enables" text live together in the `optionalPermissions` descriptors beside `OptionalGitHubAppPermissions()`. The new-App manifest, the startup notice and the `init` notice all derive from them, and `OptionalPermissionNotices` builds the notice text once.
- The notice states that Fabrik cannot change the permission itself and names the two manual steps: the App's permissions page (App owner), then the installation page (org admin).

## Alternative rejected

Merging the optional map into the required `VerifyGrants` call and partitioning the shortfalls. A grant-read failure would then be fatal again (the required check must fail hard), contradicting the requirement that the optional notice never block startup. The separate call costs one extra authenticated GET per daemon start.

## Consequences

Optional-permission checks must stay soft and separate from the required check. A new optional permission is added as one descriptor; a test requires each descriptor to carry non-empty "enables" text.
