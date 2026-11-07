# ADR 1860: App auth decides HTTPS-vs-SSH git once

## Status

Accepted. Refines [ADR 1846](1846-app-auth-https-git-credential-helper.md) and builds on [ADR 1713](1713-engine-github-app-auth.md).

## Context

ADR 1846 makes git run over HTTPS as the App installation unless `git_ssh` is set or a global `url.*.insteadOf` rewrite sends HTTPS to SSH. That one fact drives two startup decisions: whether `contents:write` is required (grant check, in `New()`), and whether the credential helper is injected (`Run()`). Both used to read git config independently (`AppGitUsesHTTPS` in `New()`, `checkURLRewrite` in `Run()`). If the config changed between the reads, the grant check could see SSH (no `contents:write` required) while `Run()` saw HTTPS and injected the helper, so every push would 403 with a read-only installation token. The window is small but includes `checkStageColumnAlignment`'s network call. Raised by Pruefer on PR #1847.

## Decisions

1. **One computation.** `decideAppGitHTTPS(cfg)` is called once, by `New()`. It returns nil when App auth is not configured (PAT mode makes no `git` call) and otherwise `AppGitUsesHTTPS(cfg.GitSSH)`.
2. **The grant check takes the value as a parameter.** `resolveGitHubAppAuth` / `setUpGitHubAppAuth` gain `httpsGit bool` and no longer read git config.
3. **Stored on the Engine as `appGitHTTPS *bool`.** `Run()` calls `e.appGitUsesHTTPS()` for `setUpAppGitCredential` and does not re-read git config for that decision.
4. **nil means "not decided", never "SSH".** An engine built outside `New()` (`testEngine`, `NewWithDeps`, `SetGitHubAppModeForTest`) ran no grant check, so `appGitUsesHTTPS` falls back to one live `AppGitUsesHTTPS` read; that cannot diverge from anything and preserves prior behaviour. A plain `bool` was rejected because its zero value would silently skip injection.
5. **The advisory check keeps its own read.** `checkHTTPSCredentials(httpsToSSH)` still uses `checkURLRewrite`; it is independent (and returns early under App auth).
6. **Static guard.** `TestAppGitDecisionCallSitesAreSingle` fails if the live readers (`AppGitUsesHTTPS`, `gitHTTPSRewrittenToSSH`, `checkURLRewrite`) gain a caller outside the decision site, the fallback and the advisory check.
7. `AppGitUsesHTTPS` stays exported; `fabrik init --github-app` decides once and uses it once.

## Consequences

The decision is fixed per process; a git config change takes effect on the next start (restart or re-exec, which re-runs `New()`). No new flag, key or variable.
