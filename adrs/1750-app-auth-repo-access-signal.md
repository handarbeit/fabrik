# ADR 1750: App-auth repo access from installation coverage, not user permissions

**Status:** Accepted
**Date:** 2026-09-16
**Issue:** [#1750](https://github.com/handarbeit/fabrik/issues/1750)

## Context

ADR-1347 introduced `resolveRepoAccess`/`RepoAccess.CanPush` to gate dispatch, label
seeding, and the `allow_auto_merge` check on write access, sourced from `GET
/repos/{owner}/{repo}`'s `permissions` object. That object describes the
**authenticated user's** access. #1713 (GitHub App installation authentication)
postdates ADR-1347 and never revisited this: under App auth, the same GET request
returns `permissions` all-`false` for every field — including `pull`, on a token that
reads the board, PRs, reviews, and check runs without error all night — because an
installation is not a user and GitHub simply doesn't populate that field meaningfully
for one.

The consequence: `CanPush` caches as `false` for every repo on the very first probe,
`itemMayNeedWork`'s ADR-1347 dispatch gate then rejects every item from that repo
unconditionally, and the engine dispatches nothing. This shipped without being caught
in CI — `tests/sim` cannot express App-auth mode at all — and was only found by the
first live e2e gate run ever executed against App auth (a four-hour run: zero workers
dispatched, zero worktrees created). ADR-1347's own fail-open/fail-closed design intent
("fail open on probe error, fail closed on genuine `push: false`") is exactly what
breaks here: a well-formed `200` response is not a probe error, so the existing
fail-open branch never engages, and a confidently wrong signal is cached as a
definitive "no."

#1713 R7 already established that git operations — and therefore `push` access — are
irrelevant to what the installation token is used for; the engine's own
`RequiredGitHubAppPermissions` doesn't even request `administration` or `contents`.
`permissions.push` was only ever a *proxy* for "can Fabrik operate on this repo," and
the proxy happens to coincide with git-push access under a PAT (same credential, both
questions) but not under App auth (different credential, different questions: API
write access vs. git push, which App auth never touches).

## Decision

**Branch `resolveRepoAccess` on auth mode, not on the response shape.** PAT mode
(`e.ghAppAuth == nil`) is byte-for-byte unchanged: `FetchRepoAccess`'s
`permissions.push`. App mode (`e.ghAppAuth != nil`) consults a different signal
entirely — the pinned installation's own accessible-repository enumeration (`GET
/installation/repositories`, wrapped as `(*githubauth.Reconciler) AccessibleRepos()`)
— because `permissions.push` is not merely often-wrong under App auth, it is
structurally inapplicable: there is no "authenticated user" whose access it could
describe.

**Installation-level permission grants are not re-checked per repo.** #1713's
`setUpGitHubAppAuth` already calls `Reconciler.VerifyGrants` at startup and fails hard
if the installation lacks `issues:write`/`pull_requests:write` — the permission-grant
half of what R1 considered is already fail-hard-verified before `resolveRepoAccess` is
ever reached under App auth. What's missing is purely the *per-repo* question: for a
`repository_selection: selected` installation, is this specific repo actually included?
`AccessibleRepos()` answers exactly that, and nothing else needs re-checking.

**Fetched once, eagerly, at startup — not lazily per repo.** `Engine.New()` calls
`AccessibleRepos()` a single time when App auth is configured, populating three
read-only fields (`appAccessibleRepos map[string]bool`, `appAccessibleReposTrunc bool`,
`appAccessibleReposReady bool`) before any worker goroutine exists — so they need no
mutex, matching ADR-1347's own "cache the failure for the rest of the process run,
self-heals on restart" precedent for the PAT-mode error path. `resolveRepoAccess`'s
per-repo call (`resolveAppRepoAccess`) is then a pure in-memory lookup, identical in
shape to the PAT branch's own cache read.

**Ambiguity always fails open, including truncation — never a silent `CanPush:
false`.** Three cases:

1. The repo is present in the fetched list → `CanPush: true`.
2. The startup fetch never succeeded at all (`appAccessibleReposReady == false`) →
   an error, routed through `resolveRepoAccess`'s existing `if err != nil { ...assuming
   writable }` branch — the same fail-open path PAT-mode probe errors already use. No
   new error-handling shape was invented for this.
3. The list was fetched successfully but hit `FetchInstallationRepositories`'s
   100-repos-per-page pagination ceiling, and the repo isn't in it → also an error,
   same fail-open branch. A repo beyond the ceiling is indistinguishable from a
   genuinely excluded one, so treating it as excluded would risk exactly the kind of
   silent false-negative this issue exists to fix.

Only a repo confirmed absent from a **complete**, successfully-fetched list produces a
definitive `CanPush: false`.

**Two-tier escalation for a genuine no-access determination (R4).** A single
`logf` warn line, once per repo per process — ADR-1347's original mechanism — was
demonstrably insufficient: it was the *only* evidence of a four-hour total work
stoppage. Two distinct situations now get two distinct treatments:

- **Zero accessible repos, installation-wide** → a hard startup refusal from `New()`,
  naming the installation ID and its settings URL. This is an unambiguous "nothing to
  do, ever" misconfiguration — mirrors `RefuseUserOwnedBoardForAppAuth`'s existing
  precedent for a different structural App-auth misconfiguration (#1713).
- **A specific board repo confirmed excluded from an otherwise-populated list** → a
  persistent `warnings.Record` entry (`Type: "repo_access"`), visible in the TUI
  Warnings panel until the repo is added to the installation's selection or removed
  from the board — not merely a single line among forty at startup. This is a
  deliberate, narrow reversal of ADR-1347's original "no `warnings.Record` for repo
  access" call (it judged no `FixAction` fit and didn't want to invent one); applied to
  **both** PAT and App mode for consistency, with auth-mode-specific wording in the
  `Detail` field, since PAT mode can hit the same confirmed-exclusion branch and
  deserves the same visibility.

**`RepoAccess.CanPush`'s name is unchanged.** It is a slight misnomer under App
auth — the installation token never uses this field's namesake capability (git push)
at all — but a rename would touch three existing consumers (label seeding, dispatch
gate, `allow_auto_merge` check) for a naming nicety with no behavioral benefit.

**`checkAllowAutoMerge` skips entirely under App auth (R5).** GitHub only returns a
repo's real `allow_auto_merge` value to a caller with `administration:read` access,
which `RequiredGitHubAppPermissions` deliberately does not request — an installation
token gets back `null` unconditionally, structurally unreadable rather than merely
often-wrong. The only consumer of `RepoAccess.AllowAutoMerge` anywhere in the engine is
this one advisory warning (confirmed by inspection — no functional gate depends on it,
unlike `CanPush`); skipping and documenting as "unknowable" beats both misreporting an
unreadable value as `false` and widening the App's permission footprint for one
advisory check, which would contradict the narrow-scope design `RequiredGitHubAppPermissions`
already establishes.

## Consequences

- App-auth dispatch works: a repo covered by the pinned installation's accessible-repo
  list is admitted for label seeding and dispatch exactly as a PAT-mode repo with
  `permissions.push: true` would be. PAT-mode behavior, tests, and call sites are
  completely unaffected — the branch point is `e.ghAppAuth != nil`, already the
  established idiom for this distinction elsewhere in the engine (e.g.
  `claudeGHTokenOverrideFn`).
- A `repository_selection: selected` installation that excludes a board-tracked repo
  now produces a persistent, actionable warning instead of silent non-dispatch for that
  one repo — every other covered repo keeps working.
- A confirmed zero-repo installation refuses to start rather than running indefinitely
  and doing nothing, closing the exact failure mode this issue reports.
- `allow_auto_merge` visibility is a known, documented gap under App auth (see
  `docs/USER_GUIDE.md`'s "Per-repo access coverage") — an operator relying on
  `fabrik:yolo` auto-merge on an App-auth-managed repo must verify `allow_auto_merge`
  manually; Fabrik will not warn about it under this auth mode. This is an accepted
  trade-off, not a partial fix: requesting `administration:read` for one advisory
  check was judged worse than the gap itself.
- **A narrower, adjacent trust-model gap is a side effect of adding `AccessibleRepos()`,
  not something this issue set out to fix**: before this change, nothing in the
  engine's pinned-installation path verified that the pin actually covers the repos
  Fabrik tries to operate on — the operator's `github_app_installation_id` was trusted
  outright (`internal/githubauth`'s own doc comment: pinned mode "skips discovery
  entirely... there is no per-owner 'is this actually covered' question here"). This
  fix closes that gap for the repo-level case as a byproduct of fixing the `CanPush`
  bug, which is a net positive but a slightly wider footprint than "fix the false
  negative" alone.
- Pagination truncation (a `selected`-mode installation covering more than 100 repos)
  is handled by failing open, but is untested against a real installation that large —
  accepted, since the live e2e bed's installation is well under the ceiling.
- The `allow_auto_merge` field on an App-mode `RepoAccess` is always left at its zero
  value (`false`) — safe only because `checkAllowAutoMerge` is the sole reader and it
  never reaches that field under App auth; a future new consumer of
  `RepoAccess.AllowAutoMerge` would need to account for this rather than trusting the
  field at face value regardless of auth mode.
