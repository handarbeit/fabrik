# ADR 1846: HTTPS Git Authenticates as the App Installation

**Date**: 2026-09-25
**Status**: Accepted
**Issue**: #1846 — App auth: HTTPS git authenticates as the installation (credential helper from the live token)
**Supersedes**: the startup refusal in [ADR-1756](1756-worker-git-gh-surface-under-github-app-auth.md) (`RefuseHTTPSWorkerGitUnderAppAuth`). ADR-1756's `gh` CLI surface decisions (Checks API instead of `actions:read`) are unchanged.

## Context

ADR-1756 refused App auth + HTTPS git at startup. The installation token had `contents:read` only, and a host helper that resolves from `GH_TOKEN` (`gh auth git-credential`) would hand it to worker `git push`, which would then 403. The two ways out were `git_ssh: true` or a global HTTPS→SSH `insteadOf` rewrite. Both send every git write through the **operator's SSH key**. So an "App auth" deployment still attributes its pushes to a human account, and still depends on that machine's SSH setup.

More generally, the engine never supplied git credentials. Engine and worker git both went through whatever helper chain the host had configured: GCM, osxkeychain, `gh auth git-credential` (the operator's login for the engine, the installation token for workers), or a repo-local helper. The identity behind a push depended on the host, not on Fabrik's configuration.

This was measured on the e2e bed (installation 162085522). With `git_ssh: true`, every API action was `fabrik-bed[bot]`, but pushes went over SSH as the operator.

## Decision

1. **HTTPS git under App auth requires `contents:write`.** `setUpGitHubAppAuth` adds `contents: write` to the required set when git runs over HTTPS (`!git_ssh` and no global HTTPS→SSH rewrite). It is checked live by the existing fail-hard `VerifyGrants` call. A shortfall error names `git_ssh: true` as the alternative. `RequiredGitHubAppPermissions` itself is unchanged, so SSH deployments keep `contents:read`.

2. **The engine injects a credential helper into its own environment.** At `Run()` startup, `setUpAppGitCredential` appends two entries to git's `GIT_CONFIG_COUNT`/`GIT_CONFIG_KEY_<n>`/`GIT_CONFIG_VALUE_<n>` list in the process environment:
   - `credential.https://github.com.helper` = *empty*. This resets every github.com helper the host's own config supplied.
   - `credential.https://github.com.helper` = a shell helper that answers `get` with `username=x-access-token` and the password from a token file.

   Every engine `exec.Command("git", …)` inherits `os.Environ()`, and `buildClaudeEnv` builds workers from `os.Environ()`. So one injection covers engine and worker git with no per-call-site change. That matters because there are ~300 git call sites.

3. **The token lives in a file, not the environment.** `.fabrik/state/github-app-git-token` (0600, atomically replaced) is rewritten by `runAppGitTokenWriter` whenever the refresh loop rotates the installation token (checked every 30s; rotation happens well before expiry). The helper reads the file at each credential lookup. A worker running past the ~1h token lifetime therefore still pushes. This closes ADR-1713's env-snapshot gap *for git*; `gh` still reads `GH_TOKEN` from its environment and keeps that gap. A missing or empty file answers nothing, so git fails the operation. It never falls through to an ambient credential.

4. **Re-exec is idempotent.** SIGHUP and self-upgrade `syscall.Exec` with the current environment. The helper value carries a marker (`fabrik-app-git-credential`). `setUpAppGitCredential` always strips previously injected pairs (the marked helper plus its immediately preceding empty reset) before deciding whether to inject. Foreign `GIT_CONFIG_*` entries are preserved in order. So a re-exec into PAT mode or `git_ssh` removes the App helper instead of leaving it to serve a bot token.

## Consequences

- App-auth deployments can push over HTTPS as `<app>[bot]` with no SSH key and no host credential setup. Commits pushed by workers appear as pushed by the App.
- Existing App installations with `contents:read` that run HTTPS now fail startup naming `contents` and `git_ssh`. Before, they were refused outright, so no working configuration regresses. `git_ssh: true` configurations are untouched.
- `fabrik init --github-app` shares the rule through `engine.RequiredGitHubAppPermissionsForGit`/`AppGitUsesHTTPS`: a newly created App always requests `contents:write` (its permission set outlives any one machine's git transport, and HTTPS is the default), and verification applies exactly the engine's startup rule, so init never accepts an installation the engine would refuse. An App created before this change must have Contents raised in its own settings before the installation has anything to approve; init's shortfall error names that page.
- The token file sits on disk for the daemon's lifetime (0600, gitignored `.fabrik/state/`) and is **not** removed at shutdown. `ctx` is cancelled at the *start* of the SIGHUP/shutdown drain, and in-flight workers keep using the helper throughout it. Removing the file then would fail their pushes with a still-valid token, because the reset entry suppresses any ambient helper (a #1847 review finding). Every start overwrites the file before git runs, and a start that injects no helper (`git_ssh`, a global HTTPS→SSH rewrite, or PAT auth) deletes a leftover one, since no worker can be using it at that point. Between a final shutdown and the next start, a token valid for at most an hour stays on disk under the same user that already holds the App private key that mints it. Workers can already read the same token from `GH_TOKEN`, so the file adds no new exposure to them.
- `checkHTTPSCredentials`' "no credential helper configured" advisory is skipped under App auth, where the engine supplies the helper itself.
