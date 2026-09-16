# ADR 1756: Worker git/gh Surface Hardening Under GitHub App Auth

**Date**: 2026-09-16
**Status**: Accepted
**Issue**: #1756 — worker `gh` surface under App auth: CI-fix path 403s, worker git over
HTTPS is a latent break

## Context

ADR-1713 gave the engine a second, co-equal authentication path: a GitHub App
installation, alongside the pre-existing personal access token (PAT). Under App auth,
`claudeGHTokenOverrideFn` (`engine/claude.go`) injects the live installation token as
`GH_TOKEN`/`GITHUB_TOKEN` into every stage worker's environment — unconditionally, the
same way for every stage. ADR-1713 explicitly deferred the consequences of that for the
worker's own `gh`/git surface ("Token injection into git remote URLs under App auth is
added surface, not a swap, and remains deliberately deferred"). This issue is that
deferred follow-up, closing two gaps found by an audit of the worker-facing `gh` surface:

- **D1 (measured)**: `RequiredGitHubAppPermissions` (`engine/github_app_auth.go`) grants
  `checks:read` but not `actions:read`. The CI-fix re-invocation instructions in
  `fabrik-review`/`fabrik-validate` called `gh run list`/`gh run view --log-failed`, both of
  which need `actions:read` — measured 403 against a real installation (bed installation
  162085522, 2026-09-16).
- **D2 (traced, unreproduced)**: the bare clone's remote URL defaults to HTTPS
  (`buildCloneURL`, `engine/worktree.go`). A worker's `git fetch`/`push` over that HTTPS
  remote resolves credentials through whatever credential helper is registered (e.g. one
  installed by `gh auth setup-git`) — which prefers `GH_TOKEN`/`GITHUB_TOKEN` from the
  environment, now the installation token. `RequiredGitHubAppPermissions` grants
  `contents:read` (added independently by #1755, for the engine's own compare/merge calls)
  but not `contents:write`, so `git fetch` would likely succeed while `git push`/
  `--force-with-lease` — which every managed stage does, per CLAUDE.md's "commit
  frequently" convention — would still 403. This is masked whenever `git_ssh: true`/`--ssh`
  is set (the clone protocol is SSH, no HTTPS credential helper is ever consulted) or a
  global `url.git@github.com:.insteadOf = https://github.com/` rewrite is active (the HTTPS
  remote is transparently redirected to SSH before any credential helper runs) — neither of
  which is the default, so a fresh install is exposed even though the investigating machine
  (and the e2e bed, which inherits the operator's own git config) was not.

## Decisions

### 1. D1: rewrite the CI-fix instructions to use the Checks API, not `actions:read`

`plugin/fabrik-workflows/skills/fabrik-review/SKILL.md` and `fabrik-validate/SKILL.md`'s
CI-fix steps now call `gh api repos/{owner}/{repo}/commits/<sha>/check-runs` (list failing
check runs) and `gh api repos/{owner}/{repo}/check-runs/<id>/annotations` (file/line detail
a CI job emitted) instead of `gh run list`/`gh run view --log-failed`. Both endpoints run
entirely on the already-granted `checks:read` scope — the engine's own
`RefreshCheckRunsLive` (`engine/ci_settle.go`) already proves this API path works under the
installation token. `RequiredGitHubAppPermissions` is **not** changed: this removes a
permission dependency rather than adding one, which is the preferred fix per the issue's R1.

**Trade-off accepted**: `output.summary`/`output.text`/annotations are only as rich as
whatever the CI job itself wrote (via workflow commands or check-run output), which for a
project whose CI is a single combined `go test`/`go vet` job with no problem-matcher steps
may sometimes be sparse compared to `gh run view --log-failed`'s raw log tail. The rewritten
instructions tell the worker to report explicitly (name the check, point at its
`details_url`) rather than guess when detail is insufficient — this satisfies "no 403" (AC1)
unconditionally; diagnostic *richness* is a separate, empirical follow-up to measure against
the live bed (tracked as a Risk below, not blocking this change).

### 2. D2: refuse App auth + default HTTPS worker git loudly at startup, not force-SSH or inject-token

`RefuseHTTPSWorkerGitUnderAppAuth(gitSSH, hasSSHRewrite bool) error`
(`engine/github_app_auth.go`) fails `Run()` before the first poll when App auth is
configured, HTTPS cloning is in effect, and no SSH rewrite masks it — naming both concrete
fixes (`git_ssh: true`/`--ssh`, or an `insteadOf` rewrite) in the error. This mirrors the
existing `RefuseGHESWithGitHubApp` precedent (loud, config-time refusal for an unsupported
combination) rather than a change that would silently alter behavior.

Two other options were considered and rejected:

- **Force `GitSSH` when App auth is active.** Rejected: this assumes ambient SSH key auth
  to github.com exists on the host, which App auth's own config says nothing about. It
  would trade one silent-until-mid-stage failure mode (HTTPS 403) for a different one (SSH
  auth failure) with no better diagnosis — worse, it changes the *engine's* own git
  behavior too (the bare clone's URL is shared between engine and worker for a given repo;
  there is no per-invocation way to give only the worker a different protocol), for a
  problem this issue frames as worker-specific.
- **Inject the installation token into worker git explicitly, granting `contents:write`.**
  Rejected: `git push --force-with-lease` needs `contents:write`, not the `contents:read`
  #1755 already added for the engine's own compare/merge calls — broadening the
  installation's granted scope beyond what's currently justified, and conflating this
  issue's worker-git-only concern with that separate, deliberately out-of-scope `contents`
  gap (#1750-adjacent, not this issue).

Refuse-loudly is the only option that adds zero new permission surface and never silently
trades one 403 for a different failure mode. Its cost — a one-time manual step
(`git_ssh: true` or an `insteadOf` rewrite) for an App-auth operator on default HTTPS
config — is judged acceptable and is exactly what AC2 asks for ("works ... or is refused at
startup with a clear message").

### 3. R6 audit: the remaining worker `gh` surface is already covered, no code change needed

Enumerated during Research/Specify and re-verified: `gh pr view --json …`, `gh pr checks`,
`gh repo view`, `gh api graphql` (PR-scoped queries and `statusCheckRollup`),
`gh issue view --json comments`, `gh api .../issues/comments/{id}` PATCH,
`gh api .../pulls/{n}/comments`, the `resolveReviewThread` mutation, and the `gh
issue`/`gh label` allowlist in `plan.yaml` are all covered by
`RequiredGitHubAppPermissions`' existing grants (`metadata:read`,
`organization_projects:write`, `issues:write`, `pull_requests:write`, `checks:read`,
`statuses:read`). Nothing in the worker surface touches `/user`, `gh auth`, `gh workflow`,
`gh secret`, `gh release`, or `gh webhook`. No permission or instruction change follows from
this — recorded here so a future contributor auditing this surface again has a dated
baseline rather than re-deriving it from scratch.

## Consequences

- An App-auth operator on a fresh install with default HTTPS git config and no SSH rewrite
  now fails to start, rather than starting successfully and having worker git silently 403
  partway through a stage. This is the intended trade — a config-time failure with a named
  fix beats a mid-stage failure with no context.
- D2 remains genuinely unreproduced end-to-end in the live bed as of this ADR: the same
  host-config masking (an operator's own `insteadOf` rewrite) that hid the bug also hides
  it from the bed, until #1756's R5 (bed git-config isolation, `scripts/e2e/run.sh`) lands
  and is actually run under App auth with default HTTPS. `RefuseHTTPSWorkerGitUnderAppAuth`
  is verified here by unit and `Run()`-level integration tests (code inspection level), not
  a live failing→passing bed run.
- `docs/USER_GUIDE.md`'s "Git operations are unaffected" bullet made the identical
  overstated claim ADR-1713's R7 bullet made, independently discovered during this issue's
  Research; both are corrected in the same change set.

## Prior Art

- ADR-1713 (`1713-engine-github-app-auth.md`) — the App-auth design this issue patches;
  its R7 Consequences bullet is corrected to state its actual preconditions and point here.
- ADR-933 (`933-required-status-context-config.md`) — cited directly in
  `fabrik-validate/SKILL.md` as the precedent for routing around a 403-prone endpoint
  (`branches/{b}/protection`) via PR-scoped GraphQL instead. D1's fix is the same pattern
  applied to a second call (`gh run` → the Checks API).
- ADR-1449/ADR-1454 (sim bed harness, sim-as-pre-gate) — establish that `tests/sim` is a
  separate bed from `tests/e2e`; R5's fix targets the latter
  (`scripts/e2e/run.sh`'s `preflight_bed_start`), not the sim harness's own, already-solved
  `tests/sim/simgh/git.go` isolation.
