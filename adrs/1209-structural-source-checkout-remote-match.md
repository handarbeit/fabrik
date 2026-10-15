# ADR 1209: Structural remote match for `IsSourceCheckout`

**Date**: 2026-10-15
**Status**: Accepted
**Issue**: #1209 — `IsSourceCheckout` authorises a build+re-exec on a substring match against the git remote
**Supersedes**: the "remote substring" wording of ADR 1196

## Context

`selfupgrade.IsSourceCheckout` is the only gate in front of the dev-build path (`git pull`, `go build -o <live binary>`, post-build hook, re-exec). It authorised that with `strings.Contains(originURL, "handarbeit/fabrik")`, which accepts `github.com/attacker/handarbeit-fabrik-fork`, `evil.com/github.com/handarbeit/fabrik` and similar. The behaviour was inherited from the pre-extraction `engine/upgrade.go` (ADR 1196), but the helper is now shared by the engine's upgrade and staleness paths and by Pruefer, a daemon built to run against untrusted PRs.

## Decision

1. **Structural match.** `IsSourceCheckout(dir, ownerRepo)` reads the origin URL and passes it to the pure `matchGitHubRemote`, which parses it and compares host and `owner/repo` exactly. Anything that does not parse as a recognised form is *not* a source checkout (fail closed).
2. **Strict allow-list of forms.** Only `https://github.com/o/r`, `ssh://git@github.com/o/r` and `git@github.com:o/r` are accepted, each with or without a trailing `.git` and/or `/`. Rejected: `http`, `git`, `git+ssh`, `file` and local paths; userinfo on https; any port; `ssh.github.com`; percent-encoding, queries and fragments; extra, empty, `.`/`..` path segments; names outside GitHub's `[A-Za-z0-9._-]` alphabet.
3. **Fixed host.** The host is fixed to `github.com` inside the package. The `owner/repo` stays caller-supplied (`DevBuildConfig.ExpectedRemote` keeps its type and shape; nothing repo-specific is hardcoded). A consumer on another forge needs a follow-up that threads a host through the config.
4. **Case-insensitive compare** of host and `owner/repo`. GitHub treats both case-insensitively and nobody else can register a differently-cased twin, so this is the same identity, and a legitimately cased clone (`Handarbeit/Fabrik`) keeps working.
5. **Test seam.** `originURLFn` (with `SetOriginURLFnForTest`) wraps `git remote get-url origin` so tests that need a real local clone — whose origin is a filesystem path and can never be `github.com` — can present a `github.com` URL while every other git operation stays real. `insteadOf` cannot do this: `get-url` applies it in the opposite direction.

## Consequences

- A dev checkout whose remote uses an `~/.ssh/config` host alias (`git@github-personal:handarbeit/fabrik.git`) or `https://token@github.com/...` no longer self-upgrades and reports "not a source checkout". This is the intended fail-closed behaviour.
- No caller changes: `engine/upgrade.go`, `engine/staleness.go` and `pruefer/upgrade.go` pass the same `owner/repo` string.
