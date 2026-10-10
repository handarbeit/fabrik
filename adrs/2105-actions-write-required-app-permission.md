# ADR 2105: Make `actions: write` a required GitHub App permission

## Status

Accepted (#2105). Supersedes ADR 2052 decision 7 (`actions` is optional, with a soft degrade) and the consequence "Without the `actions` permission nothing changes except one log line". Supersedes the optional-permission mechanism of ADR 2071 (the mechanism is deleted).

## Context

Stage workers running under GitHub App auth cannot read CI job logs or re-run jobs: `gh run view --log-failed` and `gh run rerun` fail. A worker's `GH_TOKEN` is the engine's installation token, and the token mint sends no permission narrowing, so the token carries exactly what the installation grants. Measured 2026-10-10 via `GET /app` and `GET /app/installations/{id}`, neither live App (`fabrik-bed` 4960842, `shadoworg-fabrik-models` 5191013) requests `actions`, so neither installation grants it.

ADR 2052 made `actions: write` optional so existing installations kept starting; ADR 2071 added advisory notices at startup and at `fabrik init` to close the gap. The advisory path produced zero grants: the capability is absent on every live App, and the optional tier has therefore delivered nothing that depends on it — worker CI-log reads, worker re-runs, the merge-train trial re-run and the `startup_failure` retrigger.

## Decision

1. **`actions: write` is part of `RequiredGitHubAppPermissions`** (webhooks on and off, and through `RequiredGitHubAppPermissionsForGit` both git variants). One entry puts it in the startup fail-hard check, in `fabrik init --github-app`'s verify set and in a new App's manifest. `write` covers log reads and re-runs; GitHub has no read-only re-run. Higher levels pass and `read` is a shortfall under the existing ordinal comparison.
2. **One shared remedy text for every required-permission shortfall.** GitHub offers no API for an App to gain a permission, so a refusal that only says "missing actions:write" is not enough. `engine.PermissionShortfallRemedy(shortfalls, owner, slug, installationID)` names the two manual steps with both URLs substituted — (1) an App owner sets the permission at `https://github.com/organizations/<owner>/settings/apps/<slug>/permissions`; (2) an org admin accepts the request at `https://github.com/organizations/<owner>/settings/installations/<id>` — and, when `actions` is among the shortfalls, why it is needed. Engine startup and `fabrik init` both append it, so their wording cannot drift (the concern #2071 designed against). Applying it to *every* shortfall, not only `actions`, replaces the engine's vague "App settings → Install App → Configure" and init's personal-account `https://github.com/settings/installations/<id>`, which is wrong for org Apps; App auth refuses user-owned boards before the grant check, so the org URLs are always right. The `contents`/`git_ssh` hint stays.
3. **The optional-permission mechanism is deleted**, not left empty: `OptionalGitHubAppPermissions`, the `optionalPermissions` descriptors, `OptionalPermissionNotices`, both advisory call sites, init's manifest-merge loop and their tests. `actions` was its only entry, and a live mechanism that does nothing also costs a second `GET /app/installations/{id}` per startup. ADR 2071's argument — keep an optional check separate from, and softer than, the fail-hard one, so a failed read of an optional grant can never be fatal — remains the right shape if a future permission ever returns to an optional tier.
4. **The runtime 403/404 degrade stays.** The grant check runs only at startup. If an admin revokes `actions` while the daemon runs, `isPermissionRefusal`, `fetchWorkflowRunsSoft`, `ciFlakeRerun`'s `ErrForbidden` handling and the train's re-run fallback still log once and degrade. Only the log wording changed: it no longer calls the permission optional and says it was likely revoked since startup.
5. **Worker-token construction is unchanged.** The installation token already carries every granted permission.

## Operator prerequisite — before this merges

Both live Apps must request `actions: write` and have the installation accept it **before** this lands. Otherwise the e2e gate's App legs (including the `app/on` baseline cell) fail preflight on `fabrik-bed`, and the concept-mapping instance (`shadoworg-fabrik-models`) refuses to start on its next launch or self-upgrade re-exec. The failure is loud and recoverable (grant, then restart), but avoidable. This is an intentional breaking change for installations lacking the permission.

## Known follow-up

The embedded `fabrik-review` and `fabrik-validate` skills tell workers that `gh run …` "needs `actions: read`, which is not granted and 403s". That rationale is stale once the permission is universal. The Checks API instructions remain valid, so the skills are deliberately left untouched here: rewriting them is out of scope, and editing the embedded source would trip plugin-customization detection on deployed copies. ADR 1756's measurement is now conditional on the grant, not its decision.

## Alternatives rejected

- **Keep it optional with louder notices.** The notices were already the louder option and produced no grant.
- **Require only `actions: read`.** Re-runs need `write`; requiring `read` would leave workers unable to re-run.
- **Add the remedy text for `actions` only.** Leaves init pointing org admins at the personal-account URL for every other permission.

## Consequences

- An installation lacking `actions: write` cannot start the engine or pass `fabrik init --github-app`; the error carries both manual steps.
- The startup path makes one `GET /app/installations/{id}` instead of two.
- Workers on a granting installation can read CI logs and re-run jobs (verified manually post-merge, recorded on #2105).
- PAT mode and Pruefer's own permission set are unaffected.
