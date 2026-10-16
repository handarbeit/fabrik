# ADR 2306: Pruefer's default review is an embedded plugin skill, `pruefer-review:review`

## Status

Accepted. Partially supersedes ADR 1446 decision C2 (the default guidance is embedded *text*); the rest of ADR 1446 stands.

## Context

Pruefer's default review guidance was a Go string constant (`defaultReviewGuidance`), with operator and repo guidance composed onto it as text. There was nothing to invoke, hence no foundation for review plugins. Fabrik already solves the same shape: an embedded base plugin, written to disk, loaded with `--plugin-dir`, and skills named by reference. `internal/skillref` (ADR 2305) now provides the catalogue and resolver to Pruefer without importing `engine` (ADR 1113).

## Decision

1. **An embedded base plugin, `pruefer-review`, with one skill, `review`.** It lives in `pruefer/reviewplugin/` (its own `embed.FS`), not `plugin/`: it is private to Pruefer, so the shipped-files table, the PM plugin and `fabrik upgrade` do not apply. The skill body is, byte for byte, the old constant (pinned by a digest test).
2. **A per-review directory.** Each `RealClaudeInvoker.Review` writes the plugin to a fresh `os.MkdirTemp` directory (symlink-resolved, outside the clone and the staged-diff directory), passes it as the only `--plugin-dir`, and removes it on every return path. Concurrent reviews never share a directory. A plugin that cannot be written or resolved returns an error before `claude` starts, so nothing is posted.
3. **Resolution through `internal/skillref`** with the zero `Options`, against exactly the directory passed. The prompt carries the resolved name.
4. **Grant `Skill(pruefer-review:review)`, not bare `Skill`.** `checkReviewArgsScoped` accepts only a `Skill(pruefer-review:<name>)` rule without wildcards and still rejects a bare `Skill`.
5. **Isolation (R3) is unchanged and now tested.** `--setting-sources ""` stays; a test asserts it directly under two different `CLAUDE_CONFIG_DIR` values together with "the only `--plugin-dir` is Pruefer's", and is shown to fail when either is removed.
6. **The output contract stays in Go (R4).** `renderContract` is untouched; a golden pins its bytes, and a test asserts every guidance combination ends with it.
7. **Existing guidance keeps working (R5).** Operator then repo layers are still folded by `composeGuidanceLayer`, now onto an empty base, and passed in the prompt under "Additional review guidance", in the slot the composed text occupied. Replace mode: the reviewer still invokes the skill (one code path; R6 holds for every review), under the heading "Review guidance that replaces the skill's criteria", which tells it to use the skill only for the shared method and to follow the guidance wherever they differ. A repo that uses `mode: replace` therefore sees a differently framed prompt than before, though its own text is the criteria. This is the one semantic risk; the replace branch is isolated in `renderSkillSection`, so "do not invoke the skill under replace" is a contained change if it proves wrong.
8. **A skill that did not run fails the review.** After the run, `skillref.DetectLoadFailure` plus a main-thread `Skill` call check over the stream-json: an errored call, or an event stream with no call, returns an error and posts nothing (Pruefer's existing rule for invocation failure; it has no pause analogue). A bare result object with no events cannot be judged and passes.
9. **R6 log line.** Every review logs `review skill: <ref> (plugin <name> v<version>, embedded, dir <path>)`, with a suffix when guidance replaces the skill's criteria.

## Live probe (claude 2.1.301)

- With `--setting-sources ""`, `--plugin-dir <dir>` still loads the plugin and registers `pruefer-review:review`.
- A skill planted in `<cwd>/.claude/skills/` is not offered.
- The CLI's own bundled skills remain listed and invocable, and a scoped `Skill(...)` allow rule does not stop them under `dontAsk`. They are not plugins from a profile or the tree, so R3 holds; a skill only loads prompt text, and every Read/Grep/Glob/Bash/Task call stays bound by the path-scoped rules. The scoped grant is the narrowest form, not a skill allowlist.

A different CLI version could change plugin loading or skill listing; the isolation and lifecycle tests guard the Pruefer side of that contract.

## Consequences

- The reviewer makes one more tool call per review, and the criteria arrive as a tool result rather than in the prompt.
- Coverage is unaffected: `deriveOpened` ignores `Skill` calls, and a test proves subagent crediting and the non-authoritative downgrade still behave.
- Operator or repo plugins, and selecting a skill other than the default, are the next issue in the chain.
- No flag, config key, label or marker changed, so no canonical doc page changes.
