# ADR 1346: Scrub the Anthropic auth env namespace and hard-fail on apiKeyHelper

**Status:** Accepted
**Date:** 2026-08-03
**Issue:** [#1346](https://github.com/handarbeit/fabrik/issues/1346)

## Context

Claude Code resolves its API key **before** consulting any subscription path. Fabrik
invokes Claude with `mergeEnv(os.Environ(), extraEnv)` — the engine process's full
ambient environment as the base, with a small set of Fabrik-emitted overrides layered
on top. `mergeEnv` could only add and shadow keys; it had no way to remove one. An
`ANTHROPIC_API_KEY` present anywhere in the engine's own environment — added to a
managed repo's `.env` by a human or an agent doing something locally reasonable —
therefore reached every Claude worker invocation unmodified, silently redirecting the
entire pipeline from subscription billing to metered API billing. No log line, no
label, no board signal: the run looks completely normal, and the cost is discovered on
an invoice rather than in the engine.

Stripping `ANTHROPIC_API_KEY` alone is insufficient. Research (against the installed
Claude Code binary) found a materially larger and structurally open-ended selector
surface: `ANTHROPIC_AUTH_TOKEN`, `CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR`,
`CLAUDE_CODE_OAUTH_TOKEN` (and its file-descriptor variant), and at least seven
`CLAUDE_CODE_USE_*` provider selectors (Bedrock, Vertex, Foundry, and others), among
others. That list is version-specific and grows upstream; Fabrik auto-upgrades its own
binary but does not control Claude Code releases, so an enumerate-to-remove deny-list
is permanently one release behind any newly added billing path.

Separately, `apiKeyHelper` is a `settings.json` key, not an environment variable — a
command Claude Code shells out to for credentials. No amount of environment scrubbing
can prevent it from supplying an API key, and it is reachable from a project's own
checked-in `.claude/settings.json` — repo-resident content a managed repo controls,
not the operator.

## Decision

**Default-deny over a namespace, not a deny-list.** `buildClaudeEnv` scrubs every
inherited variable whose name starts with `ANTHROPIC_` unconditionally, matching on
the exact parsed key (never a substring, so `FANTASY_ANTHROPIC_API_KEY` survives). A
newly introduced upstream `ANTHROPIC_*` billing variable is denied automatically, with
no Fabrik code change. This also scrubs non-auth `ANTHROPIC_*` variables
(`ANTHROPIC_MODEL`, `ANTHROPIC_CONFIG_DIR`, etc.) as an accepted, deliberate
consequence of "namespace, not deny-list" — the alternative (an exclusion list of
"known auth" names within the namespace) is exactly the staleness problem this ADR
exists to avoid, just narrowed to one prefix.

**`CLAUDE_CODE_*` is enumerated, not wildcarded.** Unlike `ANTHROPIC_*`, `CLAUDE_CODE_*`
is a broad general-configuration namespace that already carries Fabrik's own non-auth
`CLAUDE_CODE_EFFORT_LEVEL`/`CLAUDE_CODE_DISABLE_ADAPTIVE_THINKING` — wildcard-scrubbing
it would block legitimate future configuration that has nothing to do with billing.
`claudeCodeAuthSelectors` (`engine/claude.go`) instead names the specific
auth/credential-selector variables verified against the installed binary:
`CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR`, `CLAUDE_CODE_OAUTH_TOKEN` and its
file-descriptor variant, and the `CLAUDE_CODE_USE_BEDROCK`/`_VERTEX`/`_FOUNDRY`/
`_ANTHROPIC_AWS`/`_ANTHROPIC_GOOGLE_CLOUD`/`_MANTLE`/`_GATEWAY` provider selectors.
**Accepted residual risk:** a new, not-yet-enumerated `CLAUDE_CODE_*` billing selector
introduced upstream would leak through until a Fabrik code change adds it — the
"no code change needed" property this ADR claims for `ANTHROPIC_*` does not fully
extend to this half of the scrub.

**`mergeEnv` gains a removal sentinel, not a signature change.** A bare `"KEY"` entry
(no `=`) in `overrides` means "strip this key from `base`, emit nothing" — added
alongside the pre-existing `"KEY=VALUE"` add/shadow/last-wins semantics, which are
unchanged. This needed zero call-site changes beyond `buildClaudeEnv` itself, versus a
`remove []string` third parameter or an options struct, either of which would have
touched all three `InvokeOptions`-constructing call sites for no behavioral benefit —
`buildClaudeEnv` is the only place removal is ever needed. `buildClaudeEnv` also gained
a `baseEnv []string` parameter (both real call sites pass `os.Environ()`) so the entire
scrub/translate/passthrough matrix is a pure function testable with plain slices, with
only a handful of true end-to-end subprocess tests needed for the literal
constructed-environment acceptance criteria.

**Two independent, coexisting exceptions, not one.** `FABRIK_ANTHROPIC_API_KEY` is the
ergonomic path for the common case (translated into an explicit `ANTHROPIC_API_KEY`
override) — corporate deployments with no Claude subscription need API billing, and
the goal is to make that impossible *by accident* and explicit *by name*, not to
forbid it. `FABRIK_ANTHROPIC_ENV_PASSTHROUGH` is a separate, narrow allow-list for the
long tail (Bedrock/Vertex selectors, or whatever upstream adds next) that a bespoke
`FABRIK_`-prefixed translation would put Fabrik permanently one release behind on the
*permit* side — the exact mirror of the deny-list staleness this ADR already rejects
in the other direction. Neither variable is itself forwarded to the worker (both are
present in the engine's own `os.Environ()`, the same ambient-leak class `FABRIK_REPO`
already guards against, so each is emitted as its own removal token). When both name
`ANTHROPIC_API_KEY`, the translation wins: `buildClaudeEnv` emits the passthrough
re-adds first and the translation last, relying on Go's `os/exec` last-occurrence-wins
duplicate-key resolution rather than special-casing the collision — an edge case R7
permits but nobody is expected to actually hit, given the dedicated translation exists.

**`FABRIK_ANTHROPIC_API_KEY`/`FABRIK_ANTHROPIC_ENV_PASSTHROUGH` are resolved once at
engine construction**, mirroring the existing `claudeGHToken` package-var pattern,
rather than threaded through `InvokeOptions` — both are read once from `.env`/shell at
process start and never change mid-run, unlike `FabrikRoot`/`PRNumber`, which are
genuinely re-resolved per invocation.

**`apiKeyHelper` is refused outright, not scrubbed** — it cannot be reached by
environment scrubbing at all, so Fabrik hard-fails instead of silently tolerating it.
`checkAPIKeyHelper` (`engine/startup.go`) is a new fatal startup check, run before
`checkStageColumnAlignment` since it is an unconditional security/billing gate
independent of board configuration. It resolves the managed-policy layer
(`/Library/Application Support/ClaudeCode/managed-settings.json` on darwin,
`/etc/claude-code/managed-settings.json` on linux — R11's literal single-file-per-layer
definition only; no `CLAUDE_CODE_MANAGED_SETTINGS_PATH` override honoring, no
`managed-settings.d/` drop-in directory scan, both real gaps Research surfaced but
outside the issue's enumerated Acceptance list), the user layer (`$CLAUDE_CONFIG_DIR`
or `~/.claude`), and the `fabrikDir` project layer. A missing or unreadable file is not
an error; only a present, parseable `apiKeyHelper` key is fatal.

**A repo-resident `.claude/settings.json` needs its own per-invocation check** — it
doesn't exist at engine startup, only once a worktree is materialized.
`runInvocationWithExtension` checks it immediately alongside the existing account-wide
usage-limit suspension gate, sharing `findAPIKeyHelper` with the startup preflight. A
hit returns `apiKeyHelperDetectedError`, structurally identical to `claudeUsageLimitError`
(`StageAttempted` recorded, `StageRetryIncremented` never called — the stage never
ran, so this does not count against `max_retries`; no `stage:<name>:failed`, no
`fabrik:paused`), gated behind a new label, `fabrik:api-key-helper-detected`, so it is
visible on the board but self-clears once a human fixes the file and the next
invocation reaches Claude. Unlike `fabrik:claude-limit`, there is no account-wide
settle sweep for this label — the condition is inherently per-worktree, not
account-wide.

## Consequences

- An ambient `ANTHROPIC_API_KEY` (or any other inherited `ANTHROPIC_*`/enumerated
  `CLAUDE_CODE_*` selector) in the engine's environment no longer reaches any Claude
  worker invocation, closing the silent-rebilling defect this issue exists to fix.
- Legitimate non-auth `ANTHROPIC_*` configuration (`ANTHROPIC_MODEL`, etc.) is scrubbed
  too unless explicitly named via `FABRIK_ANTHROPIC_ENV_PASSTHROUGH` — a deliberate,
  documented tradeoff of "namespace, not deny-list," not a bug.
- A new, not-yet-enumerated `CLAUDE_CODE_*` auth selector introduced upstream would
  require a Fabrik code change to be scrubbed — the "no code change" property only
  fully holds for the `ANTHROPIC_*` wildcard half of the scrub.
- Two dynamic-indirection mechanisms found in the installed binary —
  `CLAUDE_CODE_HOST_AUTH_ENV_VAR` (names another env var to read credentials from) and
  `CLAUDE_CODE_HOST_CREDS_FILE` (names a file to read credentials from) — are
  structurally identical in shape to the `apiKeyHelper` gap this ADR already accepts,
  except both are environment variables and so are not automatically excluded from the
  scrub's stated goal the way `apiKeyHelper` is. Left unscrubbed and undetected;
  accepted as a residual gap alongside `apiKeyHelper`, not scoped in.
- `CCR_OAUTH_TOKEN_FILE`, a third credential-file prefix found in the binary, is
  deliberately excluded from both the scrub and the `apiKeyHelper` preflight — it is
  tied to a distinct "CCR host" bridge/router feature, not documented anywhere as a
  Fabrik-relevant mechanism.
- `CLAUDE_CODE_MANAGED_SETTINGS_PATH`/`CLAUDE_CODE_REMOTE_SETTINGS_PATH` (which can
  redirect where Claude Code reads its managed-settings file from) and a
  `managed-settings.d/` drop-in directory (multiple JSON files merged) are not honored
  by `checkAPIKeyHelper` — it only reads the single hardcoded path per layer R11
  specifies. A managed-policy `apiKeyHelper` set via either mechanism would not be
  detected at startup. Accepted residual risk, mirroring how `apiKeyHelper` support
  itself is deliberately out of scope.
- `mergeEnv`'s doc comment claiming first-occurrence-wins on POSIX systems was stale
  and is corrected: Go's `os/exec` resolves a duplicate `Cmd.Env` key by last
  occurrence, which is what makes the translation-over-passthrough ordering decision
  above deterministic without special-casing.
