# ADR 1786: Detect toolchain declaration drift, warn once per episode, never re-resolve PATH

**Status:** Accepted
**Date:** 2026-09-18
**Issue:** [#1786](https://github.com/handarbeit/fabrik/issues/1786)

## Context

Workers inherit `PATH` from the shell hosting the daemon. A long-lived daemon session
therefore silently pins every worker to whatever toolchain was current when that shell
started — indefinitely, with no signal. Reported in #1658 (`verveguy/liminis`): a
`-zsh` process started 2026-08-11 hosted a `fabrik --auto-upgrade` daemon started
2026-08-21; the repo added `.nvmrc` pinning Node 24 on 2026-08-23 and dropped Node 20
from `engines`, yet workers kept running Node 20.20.2 — the shell's Aug-11 `PATH` —
producing 36 `Unsupported engine` warnings in a single issue's worker logs while CI ran
Node 22+. `--auto-upgrade` kept the daemon *binary* current, which is what made the
staleness hard to suspect: the daemon looked fresh while its environment silently aged
behind it. Restarting `fabrik` alone does not help, since it re-inherits the same
`PATH` from the same parent shell — only restarting the hosting shell does.

`.nvmrc`/`.tool-versions`/`engines`/`go.mod`'s `toolchain` directive are declarations,
not mechanisms: a version manager applies them only when something runs e.g. `nvm use`,
and Fabrik's non-interactive worker subprocesses never source a profile or
auto-switch. The file can be present and correct in the worktree the entire time and
still have no effect.

The codebase already has an established pattern for "detect something at
worker-invocation time, warn loudly without failing the stage, don't repeat the
warning every invocation": `fabrik:claude-limit` (ADR-1119, ADR-1183) and
`fabrik:api-key-helper-detected` (ADR-1346). Both surface via a GitHub label + comment,
gated on the label's own absence, with `StageAttempted` recorded but
`StageRetryIncremented` never called. Both of those precedents, however, **skip the
invocation entirely** on detection — the condition they detect means the stage
genuinely cannot run. A toolchain mismatch is different: there is nothing wrong with
running the stage, only a signal worth surfacing, so the precedent's skip-invocation
half does not transfer.

## Decision

**Warn-and-continue, never skip-invocation.** `checkToolchainDrift` (`engine/toolchain.go`)
never returns an error and is never routed through `finalizeStageOutcome`. It runs as a
direct, best-effort side-effecting call at the top of both invocation paths — after the
existing `apiKeyHelper` check in `runInvocationWithExtension` (stage dispatch), and
after context files are written in `processComments` (comment review) — and the
invocation proceeds exactly as it would have regardless of what it finds. This follows
directly from the issue's R2 ("signal, don't silently proceed") read together with R3
(re-resolving `PATH` is explicitly out of scope): there is nothing for Fabrik to do
about a detected mismatch except tell someone.

**Both invocation paths are covered, closing rather than replicating the pre-existing
`apiKeyHelper` gap.** `findAPIKeyHelper`'s worktree check only runs at stage dispatch,
not comment review — a real, narrow, pre-existing coverage gap. The issue's R5 wording
("across stages and comment-review cycles") is specific enough to read as intentional
for this new check, so `checkToolchainDrift` is called from both `runInvocationWithExtension`
and `processComments` against the same shared implementation, rather than duplicating
logic at each site (which is how the `apiKeyHelper` gap happened in the first place).

**Episode = per-issue, gated by `fabrik:toolchain-stale`'s own absence — not a
daemon-lifetime, cross-issue dedup.** Matches `fabrik:claude-limit`/
`fabrik:api-key-helper-detected` exactly: simpler, reuses the existing settle/sweep
convention (`transientLifecycleLabels`), and keeps the signal visible per-thread, which
is where an operator investigating *this issue's* worker logs is already looking. The
repeated-subprocess-cost concern this could otherwise raise is solved independently
(see below) — episode-scoping and cost-caching are orthogonal and do not need to be
conflated into one mechanism.

**Resolved-version caching is process-lifetime, independent of the per-issue label —
but only for a successful resolution.** `resolveToolVersion` memoizes `node
--version`/`go version` per tool for the daemon's own lifetime, in a package-level
mutex-protected map. This is sound specifically *because* the bug this issue exists to
catch is that the inherited `PATH` cannot change without a daemon restart — a
successful resolution is genuinely invariant for the process's life, so a second check
for the same tool, on any issue, is a cache hit, not a second shell-out. This turns
steady-state cost into "a few small file reads" after the first successful check per
tool, satisfying R4's "no added latency worth noticing" without needing to cache or
rate-limit the label/comment side of the check at all. A resolution *failure*
(subprocess timeout, transient exec error, a momentarily unavailable binary) is
deliberately **not** cached — nothing about `PATH` being invariant implies a failure is
also invariant, and caching one would let a single bad moment permanently and silently
disable detection for that tool until the next restart, with no way to observe or
recover from it (Pruefer, PR #1794 review). Every invocation with a matching
declaration retries an uncached failure, bounded the same way the happy path's cost is
— only paid when a worktree actually declares that tool.

**The comparator is deliberately narrow and fails closed on ambiguity, always.**
`.nvmrc`, `package.json`'s `engines.node`, and `.tool-versions` are compared at
**major-version granularity only** — an `.nvmrc` alias (`lts/*`, `lts/iron`, `system`),
a complex `engines` range (`||`, `x`, multiple whitespace-separated clauses), or any
other unparseable content is treated as "not comparable" and produces silence, never a
warning. `go.mod`'s `toolchain`/`go` directive is compared at **major.minor
granularity** using minimum-version (`>=`) semantics, since a `go.mod` directive states
a floor the module requires, not a pin — and because Go's own toolchain
(`GOTOOLCHAIN=auto`, the Go 1.21+ default) already downloads and switches to the
declared version automatically in the common case, this format is expected to
contribute comparatively little real-world signal; it still catches the
`GOTOOLCHAIN=local` / auto-download-disabled edge case where the resolved `go` binary
is genuinely older than the module's stated minimum. This asymmetric strictness is a
deliberate response to Research's false-positive risk: R2's credibility as a "loud,
trustworthy signal" depends on it never crying wolf, so every ambiguous case anywhere
in the parsing/comparison pipeline resolves to silence, never a warning.

**All four formats named in R1 are implemented**, each independently — a worktree with
both `.nvmrc` and `package.json` `engines.node` declared is checked against both, and
a genuine mismatch in either (or both) is reported, rather than narrowing scope to a
subset of formats. The narrowing lever used to control false-positive risk is
comparator strictness (per-format granularity, fail-closed on anything not cleanly
parseable), not dropping formats.

**No dedicated settle scan.** `fabrik:toolchain-stale` is a fire-once informational
label, structurally identical to `fabrik:api-key-helper-detected` and
`fabrik:nondefault-base-pr-noted` — not a retried "awaiting X" gate with its own
escalation ladder. It self-clears the next time `checkToolchainDrift` runs for that
issue and finds no comparable mismatch (the declaration file was fixed, or, in
practice, the daemon was restarted with a corrected `PATH`). It is added to
`transientLifecycleLabels` (`engine/poll.go`) purely for the existing closed-issue
label sweep, not for any retry/escalation machinery of its own.

## Consequences

- A worktree whose declared toolchain the daemon's inherited `PATH` cannot satisfy now
  produces a loud, GitHub-visible warning (comment + `fabrik:toolchain-stale` label)
  the first time any stage-dispatch or comment-review invocation observes it, instead
  of silently running the stale toolchain indefinitely — directly closing the gap #1658
  reported.
- A daemon/worktree whose resolved toolchain already satisfies every declaration sees
  no new behavior and no new output at all — no comment, no label, and (after the first
  check per tool, process-wide) no added subprocess cost.
- Fabrik still does nothing to fix the mismatch: it does not re-resolve `PATH`, run a
  version manager, or restart itself. The only remediation named in the warning is
  restarting the daemon's hosting shell (or otherwise correcting its environment) —
  consistent with R3's explicit scoping decision. A wider fix (per-worker `PATH`
  re-resolution, driving `nvm`/`asdf`, bundling toolchains) is deliberately left to a
  future, separately-scoped issue.
- The comparator's major-version-only (or major.minor-only, for `go.mod`) granularity
  means a genuine *minor* or *patch* mismatch within an otherwise-matching major
  version is never reported — an accepted false-negative trade-off, made deliberately
  in favor of avoiding false positives (R2's credibility risk), not an oversight.
- `package.json` `engines.node` support is limited to exact versions and simple
  `^`/`~`/`>=` prefixes; any more complex semver-range syntax (`||`, `x`, compound
  clauses) is silently unsupported — a worktree relying on such a range to declare its
  toolchain gets no coverage from this check, only from `.nvmrc`/`.tool-versions` if
  also present.
- `.tool-versions` support is limited to the `nodejs`/`golang` tool names, mapped to
  the `node`/`go` binaries the codebase already knows how to invoke; every other asdf
  tool name is silently ignored — general asdf-tool-to-binary mapping is out of scope.
- The resolved-version cache is genuinely process-lifetime **for a successful
  resolution**: if an operator's inherited `PATH` is corrected by some means other than
  restarting the daemon (unusual, but not impossible — e.g. a symlink swap under an
  already-resolved binary path), the cache would not observe it until the next daemon
  restart. This mirrors the same invariant the bug report itself establishes ("only
  restarting the hosting shell" fixes the underlying condition), so it is treated as
  consistent with the bug's own model, not a new gap. A resolution *failure* is the
  opposite case and is handled oppositely: it is never cached, precisely because a
  failure carries no such invariance guarantee (see the caching decision above) — a
  persistently-failing tool retries on every invocation that declares it, rather than
  going permanently dark after one bad attempt.
