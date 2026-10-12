# ADR 2137: Stage skills are addressed by fully-qualified name

## Status

Accepted

## Context

Stage YAML named skills bare (`skill: fabrik-review`) and the prompt told the model to
"follow the fabrik-review skill", while Claude Code registers plugin skills as
`<plugin>:<skill>`. Claude models map the bare name to the namespaced entry on their own;
a non-Claude model behind LiteLLM did not and every stage skill failed with "Unknown
skill" (#1373). A bare name also cannot say which plugin, so a stage could not be pointed
at a project's own plugin (ADR 2136 made several plugins loadable). Deployments differ: a
customised `.fabrik/plugin/` is never auto-refreshed (ADR 1787) and auto-upgrade replaces
the binary without touching stage YAML, so resolution must work from what is loaded on
disk, not from what the binary expects.

## Decision

- **Every skill-valued stage key** (`skill`, `comment_skill`, `ci_fix_skill`,
  `rebase_skill`) accepts `<plugin>:<skill>`, split on the **first** colon
  (`plugin.Split`). The separator and Fabrik's base plugin name each have one Go
  definition (`plugin.NamespaceSep`, `plugin.BasePluginName`); #2140's rename changes only
  the latter. `stages.loadOne` rejects a malformed reference (empty half, whitespace) but
  never checks plugin existence at load.
- **Resolution runs against the loaded plugins**, i.e. the `--plugin-dir` set handed to
  workers (`claudePluginDirs`), read from disk: manifest `name` (else the directory
  basename, as Claude Code does) and skills discovered the way Claude Code does
  (`skills/<dir>/SKILL.md`, directories added by the manifest `skills` key including
  `"."`, a root `SKILL.md` as a single skill when there is no `skills/` and no key). The
  skill identity is the directory name; a frontmatter `name:` is not honoured (the shipped
  skills have none and the plugin reference documents only the folder form). `commands/`
  are not skills.
- **Resolver outcomes.** Qualified: use as is when the plugin is loaded and has the skill.
  Bare: the base plugin wins when it provides the skill, otherwise the first loaded
  provider; other providers are named in the deprecation notice. Anything else is
  unresolved. The catalogue is an immutable snapshot on `Engine.skillResolver` (no new
  process global), rebuilt once under a mutex on an unresolved lookup so an on-disk edit
  or upgrade is picked up without a scan per dispatch.
- **Empty loaded set passes the name through unchanged.** With no plugin directory (the
  plugin comes from Claude Code's own install) or a `NewWithDeps` engine there is nothing
  to resolve against; refusing would break SC-003. The engine never consults Claude Code's
  installed-plugin list, only `--plugin-dir` entries, and the docs say so.
- **Qualification happens at dispatch, not in the prompt builders.** `resolveStageSkills`
  returns a copy of the stage (the configured stage is shared by concurrent workers and
  never mutated), so `buildPrompt`/`buildCommentReviewPrompt` keep their signatures and
  emit the resolved name. Two gates, each beside the existing did-not-run gates and before
  a worker or reinvoke cycle is spent: `runInvocationWithExtension` (the `skill` key) and
  `processCommentsClassified` (the `comment_skill` key, which carries `ci_fix_skill` and
  `rebase_skill` through `stageVariant`). Each path resolves only the key it uses.
- **Failure shape (FR-005): a self-healing did-not-run, not a pause and not a failure.**
  The `apiKeyHelper` model: `StageAttempted` is recorded (the dispatch cooldown applies)
  but never `StageRetryIncremented`; neither `stage:<name>:failed` nor `fabrik:paused` is
  applied; one comment naming the reference, the plugins searched and their paths is
  posted and the new `fabrik:skill-unresolved` label applied, both gated on the label's
  absence; the label clears on the next invocation that resolves. On the comment path the
  result is `didNotRunSkillUnresolved`, so a reinvoke's cycle charge is refunded (#1812)
  and no comment breaker is fed. A pause was rejected: only a human comment lifts it, so
  fixing the configuration would not by itself resume the stage, whereas the next
  cooldown-bounded dispatch re-resolves.
- **Operator surfaces.** Once per startup (`Run()`, beside `WarnUndeclaredReviewers`) one
  `skill_reference_deprecated` notice lists every bare reference as `stage.key → qualified
  name` and points at `fabrik upgrade`; each unresolvable reference is a
  `skill_reference:<stage>.<key>` warning, both through `warnings.Reconcile` after the
  full check completes. A dispatch-time failure reports the same entry; a later
  successful resolution clears it. `fabrik:skill-unresolved` is also an attention
  `needs-human` state.
- **Drift is unchanged.** `stages/drift.go` compares key presence only, so bare vs
  qualified is never drift; a test pins that.
- **Shipped defaults are written qualified** (`fabrik:fabrik-<stage>`), so a fresh `fabrik
  init` starts without a deprecation notice. Existing configuration is untouched;
  rewriting it and moving customisations is `fabrik upgrade`'s job (#2139).
  *(Amended by ADR 2140: the base plugin is now `fabrik-stages` and defaults are
  written `fabrik-stages:<stage>`.)*

## Consequences

- A bare reference keeps working but is silently rewritten to its qualified name in the
  prompt; the original appears in the log, the panel and the failure comment.
- A plugin installed through Claude Code's own marketplace config rather than
  `--plugin-dir`/`plugin_dirs` is invisible to resolution; referencing it by name while
  other plugin directories are loaded is reported unresolved.
- Any gap between the engine's skill discovery and Claude Code's turns a working stage
  into a refusal. The refusal is loud and self-healing, which is why discovery mirrors
  Claude Code's rules rather than the minimal `skills/<dir>/SKILL.md` form.
- The sim replaces the invoker; prompt text and resolution are covered by unit tests, and
  the gates sit before the invoker so engine-level tests exercise them.

## Non-goals

- Resolving skills from the project's `.claude/skills/` (#969).
- Detecting a skill present on disk that fails to register at runtime — see ADR 2138, which pauses the item instead of this ADR's self-healing shape.
