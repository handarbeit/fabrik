# Feature Specification: Make `actions: write` a required GitHub App permission

**Feature Branch**: `fabrik/issue-2105`
**Created**: 2026-10-10
**Status**: Specified
**Input**: User description: "feat(engine): make actions:write a required GitHub App permission so workers can read CI logs and re-run jobs — Fabrik stage workers running under GitHub App auth cannot read CI job logs or re-run jobs, because the installation token they receive carries no `actions` permission. Neither live App's definition requests it. #2052 introduced `actions: write` as an optional permission; the advisory notices from #2071 did not lead to it being granted. Make it required: fail-hard at startup and at `fabrik init --github-app`, name the manual remedy (both URLs) in the refusal, remove it from the optional mechanism (and the mechanism itself if it ends up empty), keep the engine's degrade-on-403/404 handling for runtime revocation, leave the worker-token construction unchanged, update the docs/ADR/e2e README, and note the operator prerequisite that both live Apps must be granted the permission before this merges."

## Background

Fabrik stage workers running under GitHub App auth cannot read CI job logs or re-run jobs. Their `GH_TOKEN` is the engine's installation token, and that token carries no `actions` permission, so `gh run view --log`, `gh run view --log-failed` and `gh run rerun` fail.

The token is not being narrowed by Fabrik: the installation token is minted with no permission restriction, so it carries everything the installation grants. The installations simply grant no `actions`, because **neither live App's definition requests it**. Measured 2026-10-10 via `GET /app` and `GET /app/installations/{id}`:

| App | owner | installation | `actions` in App definition | `actions` granted |
|---|---|---|---|---|
| `fabrik-bed` (4960842) | handarbeit | 162085522 | absent | absent |
| `shadoworg-fabrik-models` (5191013) | shadoworg | 167973217 | absent | absent |

Both carry exactly `checks:read, contents:write, issues:write, metadata:read, organization_projects:write, pull_requests:write, statuses:read`.

#2052 (ADR 2052) introduced `actions: write` as an **optional** permission, deliberately kept out of the fail-hard startup grant check so existing installations kept starting. It reaches a new App only through the `fabrik init --github-app` manifest, and #2071 added advisory notices at init and startup. Neither live App was created after that, and the advisory notice did not lead to the permission being granted. Optional has produced the outcome it was designed to tolerate: the capability is absent everywhere it matters.

Because of this gap the user guide currently tells operators that the installation token is "not granted `actions:read`", and the built-in CI-fix skill instructions route around it via the Checks API. Making the permission required changes those statements.

Capabilities that need the permission: worker CI-log reads and job re-runs, the merge-train trial re-run, and the `startup_failure` retrigger (#2052).

Evidence: `GET /app` and `GET /app/installations/{id}` for both Apps (2026-10-10, table above); the installation-token mint sends no permission narrowing; the permission sets and optional-permission mechanism live in `engine/github_app_auth.go`, the manifest merge and init-time optional check in `cmd/init_github_app.go`; `adrs/2052-ci-infrastructure-failures-in-merge-train.md`.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Engine refuses to start without `actions: write` and says how to fix it (Priority: P1)

An operator starts the engine against a GitHub App installation that does not grant `actions: write`. The engine refuses to start and the error tells them exactly how to get the permission granted, because GitHub offers no API for an App to gain a permission itself.

**Why this priority**: This is the core behaviour change; it is what guarantees workers have the capability on every running instance.

**Independent Test**: Start the engine against a fake App server whose installation lacks `actions`; it must fail at startup with an error naming the missing permission and both remedy URLs.

**Acceptance Scenarios**:

1. **Given** an installation granting every other required permission but not `actions`, **When** the engine starts, **Then** startup fails and the error names `actions` (required `write`, granted none).
2. **Given** that failure, **When** the operator reads the error, **Then** it contains (1) `https://github.com/organizations/<owner>/settings/apps/<slug>/permissions` for an App owner to set the permission, and (2) `https://github.com/organizations/<owner>/settings/installations/<id>` for an org admin to accept the request, with owner, slug and installation id substituted.
3. **Given** an installation granting `actions: write` (or higher), **When** the engine starts, **Then** startup proceeds and no permission notice about `actions` is printed.
4. **Given** the required set with `actions` removed (neutralised change), **When** the startup test runs, **Then** it fails — proving the test guards the behaviour.

---

### User Story 2 - `fabrik init --github-app` enforces the permission (Priority: P1)

An operator running `fabrik init --github-app` either creates a new App or adopts an existing one. A new App's manifest requests `actions: write` as a required permission; an existing App/installation lacking it is refused with the same remedy text.

**Why this priority**: Without it, fresh Apps would still be created or adopted without the capability and fail at first startup instead.

**Independent Test**: Build the manifest and assert it includes `actions: write`; run init against an existing installation lacking `actions` and assert refusal with the remedy text.

**Acceptance Scenarios**:

1. **Given** a new-App flow, **When** the manifest is built, **Then** it includes `actions: write`.
2. **Given** an existing installation lacking `actions`, **When** `fabrik init --github-app` verifies it, **Then** it refuses with the same remedy text as the engine startup refusal (both URLs, substituted).
3. **Given** an installation that grants `actions: write`, **When** init verifies it, **Then** it succeeds and emits no optional-permission notice about `actions`.

---

### User Story 3 - Workers can read CI logs and re-run jobs (Priority: P2)

On an installation that grants `actions: write`, a stage worker's `gh` can read failed-job logs and re-run failed jobs for its repo, with no change to how the worker's token is built.

**Why this priority**: This is the motivation, but it follows automatically from Stories 1–2 plus the operator granting the permission; it is verified manually after merge.

**Independent Test**: On a granting installation, a worker runs `gh run view <id> --log-failed` and `gh run rerun <id> --failed` against its repo and both succeed.

**Acceptance Scenarios**:

1. **Given** an installation granting `actions: write`, **When** a worker runs `gh run view <id> --log-failed`, **Then** the logs are returned.
2. **Given** the same installation, **When** a worker runs `gh run rerun <id> --failed`, **Then** the re-run is triggered.

---

### User Story 4 - A permission revoked at runtime still degrades gracefully (Priority: P2)

The grant check runs only at startup. If an admin revokes `actions` while the daemon runs, the engine's own workflow-run reads and re-runs (merge-train trial re-run, `startup_failure` retrigger) must continue to degrade rather than crash.

**Why this priority**: Protects a running daemon from a runtime revocation; existing behaviour that must not be removed as "dead code".

**Independent Test**: The existing tests for the 403/404 degrade path still pass.

**Acceptance Scenarios**:

1. **Given** a running engine whose installation loses `actions`, **When** a workflow-run read or re-run returns 403/404, **Then** the engine logs and skips it exactly as before, without pausing members or crashing.

---

### User Story 5 - Operators and maintainers can understand and provision correctly (Priority: P3)

Documentation lists `actions: write` as required with its purpose, records the decision in an ADR superseding ADR 2052's optional-permission decision, and the e2e bed setup notes the bed App must grant it.

**Why this priority**: Prevents repeating the failure (an App created without the permission) and keeps docs truthful.

**Independent Test**: Docs/README/ADR contain the described content; the docs bundle regenerates cleanly.

**Acceptance Scenarios**:

1. **Given** the user guide's GitHub App section, **When** read, **Then** `actions: write` is among the required permissions with its purposes, is not described as optional-but-recommended, and no statement still claims the installation token lacks `actions`.
2. **Given** the e2e README bed setup, **When** read, **Then** it states the bed App must grant `actions: write`.
3. **Given** ADR 2052, **When** read, **Then** it points to the superseding ADR.

---

### Edge Cases

- **Higher level already granted**: an installation granting `actions` at a level above `write` is accepted, as for every other required permission under the existing comparison rules.
- **`actions: read` only**: counts as a shortfall (required `write`, granted `read`) and is refused with the same remedy.
- **Webhooks mode / HTTPS git**: `actions` is required in both webhook modes and therefore also in the git-aware required set (the `contents` variant logic is unchanged).
- **Operator not yet granted**: an existing instance restarting or self-upgrading onto this change refuses to start until the permission is granted — loud and recoverable (grant, then restart).
- **Permission requested but not yet accepted**: the App definition requests it but the org admin has not accepted; the installation still reports it ungranted, so startup is refused with the remedy (step 2 URL).
- **PAT mode**: unaffected; no App-permission check runs.
- **Runtime revocation**: covered by Story 4.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: `actions: write` MUST be part of the engine's required GitHub App permission set in both webhooks-enabled and webhooks-disabled modes, and therefore in the git-aware required set used for startup and init verification.
- **FR-002**: Engine startup's fail-hard grant check MUST refuse to start against an installation that lacks `actions: write`.
- **FR-003**: `fabrik init --github-app` MUST refuse an existing App/installation that lacks `actions: write`.
- **FR-004**: The new-App manifest MUST request `actions: write` as a required permission.
- **FR-005**: The refusal for a missing `actions` permission MUST name the remedy with both manual steps and both URLs — (1) an App owner sets the permission at `https://github.com/organizations/<owner>/settings/apps/<slug>/permissions`; (2) an org admin accepts the request at `https://github.com/organizations/<owner>/settings/installations/<id>` — with owner, slug and installation id substituted, identical in wording between engine startup and `fabrik init`, matching what the #2071 advisory notice provided. A refusal that only says "missing actions:write" is not acceptable.
- **FR-006**: `actions` MUST be removed from the optional-permission set. No optional-permission advisory notice MUST be emitted for `actions` at startup or at init.
- **FR-007**: If removing `actions` leaves the optional-permission mechanism with no entries, the mechanism and its two advisory call sites (engine startup, `fabrik init`) MUST be removed rather than left as live code that does nothing — unless another permission genuinely belongs there, in which case the change MUST state which.
- **FR-008**: The engine's existing degrade-on-403/404 handling for its own actions calls (merge-train trial re-run, `startup_failure` retrigger) MUST be retained, with its tests still passing.
- **FR-009**: How the worker's `GH_TOKEN` is built MUST NOT change; the installation token already carries every granted permission.
- **FR-010**: `docs/USER_GUIDE.md` "GitHub App Authentication" MUST list `actions: write` among the required permissions with its purpose (worker CI-log reads and re-runs, the merge-train trial re-run, the `startup_failure` retrigger), MUST drop it from optional-but-recommended, MUST keep the manual "upgrading an existing App" steps in a form that matches the refusal, and MUST correct statements that the installation token lacks `actions` / `actions` is "notably absent".
- **FR-011**: `docs/llms-full.txt` MUST be regenerated after any change to the canonical doc pages.
- **FR-012**: A new ADR for this issue MUST supersede ADR 2052's optional-permission decision, recording that the advisory path left the permission absent on every live App and that workers need it; ADR 2052 MUST be amended with a pointer to the new ADR.
- **FR-013**: `tests/e2e/README.md` bed setup MUST state that the bed App must grant `actions: write`, so a fresh bed is provisioned correctly.
- **FR-014**: Unit tests MUST pin that the required set contains `"actions": "write"` in both webhook modes, that engine startup against an installation lacking `actions` fails with both remedy URLs substituted, that `fabrik init` refuses with the same text, and that the manifest includes `actions: write`.

### Key Entities *(if applicable)*

- **Required permission set**: the map of GitHub App permissions the engine needs; used for the new-App manifest, `fabrik init` verification and the startup fail-hard check.
- **Optional permission set**: the advisory-only permission mechanism from #2052/#2071; expected to be empty after this change.
- **Installation token**: the credential the engine and its workers use under App auth; carries whatever the installation grants.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: Both required-permission sets (webhooks on/off) contain `"actions": "write"`, pinned by a unit test.
- **SC-002**: Engine startup against an installation lacking `actions` fails with an error containing both remedy URLs with correct substitutions; with `actions` dropped from the required set, that test fails.
- **SC-003**: `fabrik init --github-app` against an installation lacking `actions` refuses with the same remedy text; its manifest includes `actions: write`.
- **SC-004**: No optional-permission notice for `actions` is emitted at startup or init, and no dead optional-permission code remains (unless a specific other permission is named as justifying it).
- **SC-005**: The existing 403/404 degrade-path tests for the engine's actions calls pass unchanged.
- **SC-006**: Documentation, ADR, ADR 2052 pointer, e2e README note and regenerated `docs/llms-full.txt` are in place and `go test -race ./...` passes.
- **SC-007** (manual, post-merge, recorded on this issue): on an installation granting `actions: write`, a worker successfully runs `gh run view <id> --log-failed` and `gh run rerun <id> --failed` against its repo.

## Assumptions

- `actions: write` (rather than `actions: read`) is required, as it covers both log reads and re-runs; installations granting a higher level are accepted per existing comparison rules.
- The refusal remedy reuses the wording and URL shape of the #2071 advisory notice so operators see the same instructions they already know.
- Making the permission required is intentionally a breaking change for installations lacking it; the loud-and-recoverable refusal is accepted as the cost.
- Built-in stage skills that currently route CI-fix work through the Checks API (because the token lacked `actions`) keep working unchanged; whether to switch them to `gh run` is a separate decision.
- Existing worker `gh` command allowance (default `Bash(gh:*)`) already permits `gh run` commands, so no tool-permission change is needed.

## Out of Scope *(optional)*

- Pruefer's own permission set (`PrueferRequiredPermissions`) — separate product, separate App.
- PAT mode, which is unaffected.
- Any change to which `gh` commands workers are allowed to run.
- Any change to how the worker token is built or narrowed.
- Rewriting built-in skills to prefer `gh run` over the Checks API (possible follow-up once the permission is universal).
- Granting the permission on the live Apps themselves — see the operator prerequisite below.

## Operator prerequisite — do before this merges

Both Apps above must request and accept `actions: write` **before** this lands, or the change stops them:

- the e2e gate's App legs (including the `app/on` baseline cell) fail preflight on `fabrik-bed`;
- the concept-mapping instance (`shadoworg-fabrik-models`) refuses to start on its next launch or self-upgrade re-exec.

Failure is loud and recoverable (grant, then restart), but avoidable.

## Source References *(optional)*

- `GET /app` and `GET /app/installations/{id}` for both Apps, 2026-10-10 (table in Background).
- `engine/github_app_auth.go` — required/optional permission sets, `OptionalPermissionNotices`, startup optional check.
- `cmd/init_github_app.go` — manifest merge and init-time optional check.
- `docs/USER_GUIDE.md` "GitHub App Authentication" — "The engine currently requires", "Optional but recommended: `actions: write`", "Upgrading an existing App", "Worker `gh` CLI authentication".
- `adrs/2052-ci-infrastructure-failures-in-merge-train.md`; #2071 (advisory notices); ADR 1756 (worker `gh` surface under App auth).
