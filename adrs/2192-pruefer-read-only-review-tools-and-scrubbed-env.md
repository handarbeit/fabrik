# ADR 2192: Pruefer read-only review tools and a scrubbed review environment

**Date**: 2026-10-11
**Status**: Accepted
**Issue**: #2192 — path-scoped read-only review tools, Task subagents, scrubbed review environment
**Amends**: ADR 1113 (the "fixed read-only allowlist" statements); builds toward #2190

## Context

Pruefer's review session ran `claude` with `Read`, `Grep`, `Glob` and six `Bash(git <sub>:*)` rules, and with the daemon's whole environment. That was wrong in both directions:

- **Too narrow.** The staged diff lives in the OS temp root, outside the clone, so `cat`/`head`/`sed` on it were denied (the CLI's built-in read-only set only runs inside the working directory). Reviews said "Bash was denied" and concluded "no findings" over unread code (#2190).
- **Too broad.** Bare `Read`/`Grep`/`Glob` allow every path, and the process held `GH_TOKEN`, `.env` values and the rest of the daemon environment. A PR is attacker-controllable input; planted instructions could steer the reviewer to `.pruefer/app-private-key.pem` and quote it into a public review.

## Decisions

1. **Scope is expressed as path-scoped allows, never deny-then-allow.** Claude Code deny rules beat allows, so "deny everything, allow the clone" cannot be written. `buildReviewToolRules` emits `Read`/`Grep`/`Glob(//<clone>/**)` and `Read`/`Grep(//<diff>)`. There are no bare `Read`/`Grep`/`Glob`/`Bash` entries.
2. **`Grep` and `Glob` get their own path rules.** Probing showed that `Read(...)` rules do *not* scope the `Grep`/`Glob` tools (a bare `Grep` with a scoped `Read` returned an outside file); `Grep(//C/**)` and `Glob(//C/**)` do.
3. **Resolved, escaped paths; fail closed.** The CLI applies an allow only when both the requested and the resolved path match, and macOS temp dirs are symlinks (`/var` → `/private/var`). `resolveScopePaths` resolves the clone and diff paths (`EvalSymlinks`) and `Review` uses the resolved paths for the cwd, the prompt and the rules. Gitignore metacharacters and rule delimiters are escaped (`escapeRulePath`). If a path cannot be resolved, `Review` returns an error without invoking `claude`.
4. **Shell: `cat`, `head`, `tail`, `wc`, `ls`** as prefix rules; the CLI path-checks their operands (and redirect targets) against the Read rules. `Bash(find *)` and `Bash(wc *--files0-from*)` are denied via `--disallowedTools` (the built-in set would otherwise allow `find` in the clone).
5. **`sed` and `rg` are left out** (deviation from the issue's literal list). The spec says to omit a command where the rule syntax cannot express a restriction safely. `Bash(sed -n *)` admitted `sed -n -i 1p file` in probing (the CLI denied `1e`, `1w`, redirection and substitution, but not `-i`), and `rg --pre <cmd>` executes programs. Paging is covered by `Read` offset/limit, `head`, `tail`, `cat` and `wc`. Residual: the CLI's *built-in* read-only analysis admits a plain `sed -n 'Np' file` on an in-scope path regardless of our rules (observed live); `sed -i`, `w`, `e` and out-of-scope operands are denied. The prompt does not promise `sed`. If a later CLI makes `sed` provably safe, adding it is one rule and one test.
6. **All `git` rules are dropped.** `git diff`/`grep --no-index` and `blame --contents` read outside the scope, `--output=`/`-O<cmd>` write or execute, and the depth-1 single-commit clone has no history for `log`/`blame`/`diff` to report. The staged diff file and scoped `Grep` replace them.
7. **`Task` is allowed with no further restriction.** Subagents inherit the parent's rules (live: a subagent's outside `Read` and `go test` were denied; `head` on the diff succeeded), and `Review` parses only the last `type:"result"` event, so subagent traffic can never become review text. The output contract and the Go-owned review submission are unchanged.
8. **Environment is an explicit allowlist** (`review_env.go`): `PATH`, `HOME`, `USER`, `LOGNAME`, `SHELL`, `TERM`, `TMPDIR`/`TEMP`/`TMP`, `LANG`, `LANGUAGE`, `LC_*`, `CLAUDE_CONFIG_DIR`, `XDG_{CONFIG,DATA,CACHE}_HOME`, proxy and CA variables, and a few Windows basics, plus the two `CLAUDE_CODE_*` overrides Pruefer sets itself. This is the opposite polarity to the engine's worker scrub (ADR 2027) because the spec requires an allowlist. `ANTHROPIC_*` and `CLAUDE_CODE_OAUTH_TOKEN` are not in the default (subscription-backed profile auth, ADR 1113). `PRUEFER_REVIEW_ENV_PASSTHROUGH` (environment only, comma-separated exact names) is the named opt-in; names under `GH_`, `GITHUB_`, `FABRIK_`, `PRUEFER_`, `HOOKDECK_` are refused, and names (never values) are logged once.
9. **`.git/FETCH_HEAD` is removed after checkout.** It records the fetched URL, which carries the installation token, inside the readable clone.
10. **Prompt (R5).** `renderReviewTools` names the exact tools, shell commands (generated from `reviewShellAllow`) and the path scope, and lists the unavailable families. `renderOmittedPaths` no longer prints a `git diff … ':!path'` example. A test checks every command form the prompt offers is permitted.
11. **Non-vacuity.** `checkReviewArgsScoped` and `checkReviewEnvScrubbed` are the structural assertions; tests also run them against the legacy unscoped allowlist and an unscrubbed environment and require a violation. Behaviour is proven by `pruefer/liveprobe_test.go` (`-tags liveprobe`, `PRUEFER_LIVE_PROBE=1`), which runs the real CLI through the production argument and env builders.

## Live probe evidence

`claude` 2.1.296, `-p --permission-mode dontAsk --setting-sources ""`, model haiku, run 2026-10-11 on macOS against a fixture with a planted `.pruefer/app-private-key.pem`, a secret directory and an in-clone symlink to the key. The review process ran under the allowlisted environment only (login succeeded). Every row passed.

| Probe | Outcome |
|---|---|
| `Read` `.pruefer/app-private-key.pem` | **denied** (`permission_denials`: `Read {"file_path":".../.pruefer/app-private-key.pem"}`) |
| `Read` an in-clone symlink to the key | denied |
| `Grep` / `Glob` rooted at the secret directory | denied |
| `cat` of the key | denied |
| `Read` clone file; `Read` diff | allowed |
| `head`, `cat`, `tail`, `wc`, `ls` on the diff / clone | allowed |
| `go test ./...`; `find <clone>`; `find -exec`; `sed -i` | denied |
| `sed -n 1w`, `sed -n 1e…` | denied |
| `sed -n 1p <key>` (outside scope) | denied |
| `sed -n 1p <diff>` (in scope) | allowed by the CLI's built-in analysis (see decision 5) |
| `cat f > clone/out.txt`; `cat $(…)`; `cat f; curl …` | denied |
| `git diff --no-index <secret> <diff>`; `wc --files0-from=<key>` | denied |
| Task subagent: outside `Read` and `go test` | both denied inside the subagent; no secret content in the result; normal `result` event |
| Task subagent: `head` on the diff | succeeded and reported |

## Residual risks (out of scope here)

- A PR-supplied `CLAUDE.md` in the clone still loads for the reviewer and its subagents; R1/R4 limit what it can reach, but it remains a prompt-injection channel.
- MCP configuration and user-level skills/agents are not suppressed (`--setting-sources ""` stops settings files only).
- The OS-level sandbox (`--settings` with `sandbox.*`) is deferred: it needs bubblewrap on Linux and silently runs unsandboxed when it cannot start.
- All behaviour claims are for CLI 2.1.296; rules degrade closed (unrecognised forms are denied under `dontAsk`). Re-run the live probe on a CLI upgrade.
- Subagents add turns and cost; the 15-minute inactivity watchdog is reset by any stdout write.
