# Feature Specification: Pruefer loads operator review plugins, selected by `plugin:skill`, with a visible fallback

**Feature Branch**: `fabrik/issue-2307`
**Created**: 2026-10-15
**Status**: Specified
**Input**: User description (the original request; it is superseded where "Scope change" below says so): "Third in the chain replacing #1859.

## Problem

An operator or a repo cannot bring real review tooling to Pruefer. Good review plugins exist, for example `pr-review-toolkit` in `claude-plugins-official`, with code-reviewer, silent-failure-hunter, pr-test-analyzer and type-design-analyzer agents. A repo may also want its own review procedure with supporting files, sub-skills, agents, hooks or an MCP server. Today Pruefer accepts only one text file, at `.pruefer/skills/review/SKILL.md`.

## Trust model

Content on the PR's **base ref** has been merged by the repo's maintainers. It is exactly as trusted as the repo's own CI and the Fabrik workers that already run on this machine, so a base-ref plugin may carry every plugin component. The **PR head** stays untrusted, as #1446 C1 decided: nothing the PR changes may alter how that PR is reviewed. The fourth issue in this chain handles running coded tools over the head tree.

## Requirements

- **R1 (operator plugins):** `.pruefer/config.yaml` gains `plugins:`, a list of local plugin directories, for example a checkout of `claude-plugins-official/plugins/pr-review-toolkit` at a pinned commit. They are loaded for every review after `pruefer-review`. Each review logs each plugin's manifest name and version. A directory that is missing or unreadable is reported once and skipped; the review does not fail.
- **R2 (repo plugins) — DEFERRED to #2366, not implemented here:** the original request had Pruefer copy a repo's `.pruefer/plugins/<name>/` tree at the PR's base SHA and load it with `--plugin-dir`. That is removed (see "Scope change" below): no code from the reviewed repo is read or loaded, and see FR-004.
- **R3 (selection):** the review skill is chosen with a `plugin:skill` reference, as in Fabrik. The operator sets `review_skill:` (default `pruefer-review:review`). A repo can override it with `review_skill:` in its base-ref `.pruefer/config.yaml`, unless the operator sets `repo_review_skill: false`. Resolution uses `internal/skillref` against the plugins actually passed.
- **R4 (visible fallback):** if the chosen skill does not resolve, or resolves but fails to load (the detector from the first issue), Pruefer runs `pruefer-review:review` instead. It says so in the posted review body, naming the reference, the base SHA and the reason, and in the log. The review is never failed and never silently substituted.
- **R5 (output contract):** whatever skill runs, the Go-owned prompt still requires the summary, findings and coverage blocks, and a review that does not produce them is handled exactly as today. A plugin that posts to GitHub itself (e.g. the `code-review` command) cannot, because the review environment has no GitHub token (#2192 R4).
- **R6 (environment):** the #2192 environment scrub applies to everything a plugin starts, including hooks and MCP servers: no GitHub token, App key or `.env` secrets. Add a test.

## Out of scope

- Changing which Bash commands the reviewer may run (fourth issue). In this issue, plugin-supplied hooks and MCP servers run, but the reviewer's own tool grant is unchanged.
- Installing plugins from a marketplace by name. The operator supplies directories; a marketplace installer is a possible follow-up.

## Acceptance

- An operator-configured `pr-review-toolkit` checkout is loaded, and a review can use its agents through `Task`. The log names the plugin and version.
- Replaced by the scope change (repo plugins are deferred to #2366): no plugin tree of the reviewed repo is read or loaded, on the base or the head, and a test fails if production code gains a reader for one. A repo's `review_skill` can select only a skill of the embedded or an operator plugin; any other reference falls back visibly.
- An unresolved or unloadable `review_skill` produces a default review whose body names the fallback and its reason.
- With `repo_review_skill: false`, a repo's `review_skill` is ignored and the log says so.
- A plugin hook's environment contains no GitHub token or App key.
- New tests are shown to be non-vacuous; `go test -race ./...` passes."

## Scope change (2026-10-16)

**Repo-supplied plugin code is removed from this issue.** Repo review plugins will come from the repo's standard Claude Code plugin declarations instead of a bespoke `.pruefer/plugins/` tree; that is a separate issue, **#2366**. This change ships with **no code from the reviewed repo running on the Pruefer host**. The first implementation (a base-SHA Trees/Blobs reader with validation and caps, the repo half of the plugin set, the default-branch-only rule and the veto-as-kill-switch wording) is preserved on the branch `pruefer/repo-plugins-fetch` (head `3e6f75eb`), so nothing removed is lost.

What this changes in the original request above: **R2 is deferred to #2366**, and the "repo plugin at the base ref" acceptance bullet is replaced by the acceptance in this section's resulting requirements below. **R1, R3, R4, R5 and R6 stand**, with R3 narrowed: a repo's `review_skill` resolves against only the embedded `pruefer-review` plugin and the operator's plugins, so a reference naming any other plugin falls back visibly. **Kept:** one resolved base SHA per review for every base-ref read (config, guidance; #2308 builds on it), operator `plugins:`, `review_skill: plugin:skill` in the base-ref `.pruefer/config.yaml`, `repo_review_skill: false` as the operator's veto on a repo choosing a skill, the scoped `Skill(<ref>)` grant and `checkReviewArgsScoped`, the visible fallback, the embedded-name collision rule and the scrubbed-environment test (operator plugins can carry hooks and MCP servers).

## Background

Pruefer, Fabrik's review bot, can today be customised in only one way beyond operator guidance text: a repo may commit a single text file, `.pruefer/skills/review/SKILL.md`, read at the PR's base ref and composed onto the default review. There is no way to bring real review tooling: multi-agent review plugins such as `pr-review-toolkit` (code-reviewer, silent-failure-hunter, pr-test-analyzer, type-design-analyzer).

This is the third issue in the chain that replaces #1859. The first (#2305) extracted the skill catalogue, `plugin:skill` resolver and skill-load-failure detector into the shared leaf package `internal/skillref`. The second (#2306) made Pruefer's default review an embedded plugin skill, `pruefer-review:review`, written to a per-review directory, passed with `--plugin-dir` and invoked by name, with the reviewer's `Skill` grant scoped to that one skill. This issue builds on both: it lets an operator add plugin directories, and lets the operator or a repo choose which loaded skill performs the review.

**Trust model.** The operator's plugins are the operator's own, as trusted as the software the operator installs on the machine, so an operator plugin may carry every plugin component, including hooks, MCP servers and scripts. **No code from the reviewed repo is loaded:** content on the PR's base ref was merged by the repo's maintainers, but running it as code on the Pruefer host is a separate decision, deferred to #2366. The **PR head** stays untrusted (ADR 1446 C1): nothing a PR changes may alter how that same PR is reviewed. Pruefer's other protections stay in force: `--setting-sources ""` (nothing from the reviewed tree's `.claude/` or the daemon's profile loads), the path-scoped read-only tool grant, and the allowlisted review environment with no GitHub token or `.env` secrets (ADR 2192). Running coded tools over the head tree is the fourth issue in the chain.

Two facts about today's code shape the scope. `PRDetails` carries only the base branch *name*, not a commit, so reading the config and guidance at "the base" needs one resolved SHA per review. And a repo-side `.pruefer/config.yaml` at the base ref already exists (ADR 1642) with narrowing-only keys; this issue adds `review_skill` as the first key that does not narrow, deliberately limited to the review-skill choice.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Operator adds a review plugin (Priority: P1)

An operator lists a local checkout of a review plugin (for example `pr-review-toolkit` at a pinned commit) under `plugins:` in `.pruefer/config.yaml` and sets `review_skill:` to a skill from it, or leaves the default skill and lets the reviewer use the plugin's agents through `Task`.

**Why this priority**: it delivers the headline value (real review tooling) with the least new trust surface, since the operator controls the machine.

**Independent Test**: configure a fixture plugin directory under `plugins:`; run a review; assert the plugin is passed with `--plugin-dir` after `pruefer-review`, its manifest name and version are logged, and the skill or agent it provides is invocable.

**Acceptance Scenarios**:

1. **Given** `plugins:` lists a readable plugin directory, **When** a review runs, **Then** the plugin is loaded after `pruefer-review` and the log names its manifest name and version.
2. **Given** `plugins:` lists a directory that is missing or unreadable, **When** a review runs, **Then** the problem is reported once, the directory is skipped and the review proceeds.
3. **Given** `review_skill` names a skill in an operator plugin, **When** a review runs, **Then** the reviewer is told to invoke that skill and the posted review is in the usual format.

---

### User Story 2 - A repo chooses among the loaded skills, and none of its code loads (Priority: P1)

A repo names a review skill in its base-ref `.pruefer/config.yaml` with `review_skill:`. The reference can only select a skill of the embedded `pruefer-review` plugin or of an operator plugin. Nothing the repo commits is loaded as code.

**Why this priority**: it is the repo-owned counterpart to Story 1 and carries the security-sensitive rule: no code from the reviewed repo runs on the Pruefer host until #2366 designs it.

**Independent Test**: a repo whose base-ref config names a skill of an operator plugin gets that skill; one naming a plugin that is not loaded gets the visible fallback. A repo that commits `.pruefer/plugins/` has none of it read, and the test fails if any production code gains a reader for a plugin tree of the reviewed repo.

**Acceptance Scenarios**:

1. **Given** the repo's base-ref config names a skill that exists in an operator plugin, **When** a PR is reviewed, **Then** that skill performs the review.
2. **Given** the repo's base-ref config names a plugin that is not the embedded one or an operator plugin, **When** a PR is reviewed, **Then** the reference does not resolve and the default review runs with a notice naming the reference, the base SHA and "did not resolve".
3. **Given** a repo commits plugins under `.pruefer/plugins/` (on the base or only on the PR head), **When** a PR is reviewed, **Then** nothing from that tree is read or loaded.
4. **Given** a repo config that exists only on the PR head, **When** it is reviewed, **Then** it has no effect on the selection.

---

### User Story 3 - Selection is controlled and the operator can veto the repo (Priority: P2)

The operator's `review_skill` (default `pruefer-review:review`) applies unless the repo's base-ref config overrides it; the operator can forbid that override with `repo_review_skill: false`.

**Why this priority**: it makes the choice predictable and gives the operator the last word over the repo's choice.

**Independent Test**: run reviews with and without a repo `review_skill`, with `repo_review_skill` true and false, and assert which reference is resolved and what the log says.

**Acceptance Scenarios**:

1. **Given** no repo override, **When** a review runs, **Then** the operator's `review_skill` (default `pruefer-review:review`) is used.
2. **Given** a repo base-ref `review_skill` and `repo_review_skill` not false, **When** a review runs, **Then** the repo's reference is used.
3. **Given** a repo base-ref `review_skill` and `repo_review_skill: false`, **When** a review runs, **Then** the repo's value is ignored, the operator's is used and the log says so.

---

### User Story 4 - A bad choice degrades to a visible default review (Priority: P1)

If the chosen skill does not resolve, or resolves but fails to load, the review is not lost: the default skill runs and the posted review says exactly what happened.

**Why this priority**: operators and repos will misconfigure references; a failed or silently substituted review is the worst outcome.

**Independent Test**: choose a non-existent reference, then a reference whose skill errors on load; assert the default review is posted, the body names the reference, the base SHA and the reason, and the log records it.

**Acceptance Scenarios**:

1. **Given** `review_skill` names a plugin or skill that is not among the plugins passed, **When** a review runs, **Then** `pruefer-review:review` runs and the review body names the reference, the base SHA and "did not resolve".
2. **Given** the chosen skill resolves but its load fails, **When** the run is checked, **Then** `pruefer-review:review` runs instead and the body names the reference, the base SHA and the load failure.
3. **Given** a fallback happened, **When** the review is posted, **Then** it is a complete review in the usual format, never a failure and never a silent substitution.

---

### User Story 5 - Plugin code cannot reach credentials (Priority: P1)

Hooks and MCP servers that an operator plugin starts run in the same scrubbed environment as the reviewer: no GitHub token, App key or `.env` secrets.

**Why this priority**: operator plugins execute code beside the review; without this, a compromised operator plugin could exfiltrate the review identity.

**Independent Test**: a fixture operator plugin whose hook and MCP server record their environment; assert no `GH_*`/`GITHUB_*`/`FABRIK_*`/`PRUEFER_*`/`HOOKDECK_*` values or `.env` entries appear, and that the test fails against an unscrubbed environment.

**Acceptance Scenarios**:

1. **Given** the daemon holds a GitHub token, App key path and `.env` secrets, **When** a plugin hook runs during a review, **Then** its environment contains none of them.
2. **Given** a plugin tries to post to GitHub itself (for example a `code-review`-style command), **When** it runs, **Then** it cannot, because the review environment holds no GitHub token.

---

### Edge Cases

- A plugin directory name differs from its manifest `name`: references use the manifest name (the resolver's semantics), not the directory name.
- Two plugins declare the same manifest name, or one declares `pruefer-review`: the embedded `pruefer-review` is never replaced; for other duplicates the earlier plugin in load order wins and the later one is skipped and reported.
- The repo commits a `.pruefer/plugins/` tree, on the base or only on the PR head: it is never read and never loaded.
- The repo's base-ref `.pruefer/config.yaml` is absent, malformed or unreadable: it is treated as no override and logged; the review proceeds with the operator's selection.
- The repo's config contains keys other than `review_skill` that are not already recognised: they are ignored and never alter operator-only settings.
- The base branch cannot be resolved to a commit: the base-ref reads fall back to the branch name and the outcome is not memoised.
- The fallback skill (`pruefer-review:review`) itself fails: handled exactly as it is today.
- `review_skill` is not a well-formed `plugin:skill` reference (for example a bare name, or a name containing a character that is a delimiter in the `Skill(<ref>)` grant): it is treated as unresolved and falls back visibly.
- Operator and repo guidance (`review_guidance`, `.pruefer/skills/review/SKILL.md`) are still composed and passed as additional guidance to whichever skill runs, including the fallback.
- A plugin's agents are available to the reviewer through `Task` subagents, subject to the unchanged path-scoped read-only grant. A plugin's sub-skills are not callable: the `Skill` grant names only the selected reference.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: `.pruefer/config.yaml` accepts `plugins:`, a list of local plugin directories; every directory listed and readable is passed to each review with `--plugin-dir`, after `pruefer-review`, in listed order.
- **FR-002**: Every review logs, for each plugin it loads, the plugin's manifest name and version.
- **FR-003**: A `plugins:` directory that is missing or unreadable is reported once and skipped; the review does not fail.
- **FR-004**: No plugin from the reviewed repo is loaded: nothing is read from the repo's `.pruefer/plugins/`, from the PR head or from the clone as plugin code, and no GitHub tree or blob is fetched for that purpose. Repo-supplied plugins are deferred to #2366. A test fails if production code gains such a reader.
- **FR-005**: All plugin components load for operator plugins: skills, agents, hooks, MCP servers and scripts.
- **FR-006**: A repo config that exists only on the PR head has no effect on the review; every base-ref read of one review (config, guidance) uses one commit SHA resolved once for that review, and the SHA a fallback notice names is the one actually read.
- **FR-007**: The review skill is selected by a `plugin:skill` reference. The operator sets `review_skill:` in config, defaulting to `pruefer-review:review`.
- **FR-008**: A repo may override the selection with `review_skill:` in its **base-ref** `.pruefer/config.yaml`, unless the operator sets `repo_review_skill: false`; with that setting the repo's value is ignored and the log says so. The repo's reference resolves only against the embedded `pruefer-review` plugin and the operator's plugins.
- **FR-009**: The selected reference is resolved with `internal/skillref` against exactly the plugin directories passed to that review, and the prompt carries the resolved reference.
- **FR-010**: The reviewer's `Skill` grant is scoped to the resolved review skill (and the default skill when it is the fallback); a bare `Skill` grant is still never issued, and no other part of the reviewer's tool grant changes.
- **FR-011**: If the selected skill does not resolve, or resolves but fails to load (detected as in #2306), the review runs `pruefer-review:review` instead.
- **FR-012**: When a fallback occurs, the posted review body names the selected reference, the base SHA and the reason, and the log records the same; the review is never failed and never silently substituted.
- **FR-013**: Whatever skill runs, the Go-owned prompt still requires the summary, findings and coverage blocks, and a review that does not produce them is handled exactly as today.
- **FR-014**: Anything a plugin starts, including hooks and MCP servers, runs in the #2192 review environment: no GitHub token, App key or `.env` secrets; a test asserts this and is shown to fail against an unscrubbed environment.
- **FR-015**: Operator `review_guidance` and a repo's `.pruefer/skills/review/SKILL.md` keep working unchanged, passed as additional guidance to whichever skill runs.
- **FR-016**: Isolation is unchanged: `--setting-sources ""` stays, and the only plugins visible to a reviewer are those Pruefer passes; nothing from the reviewed tree's `.claude/` or the daemon's profile loads.
- **FR-017**: Only `review_skill` is added to what is read from the repo's base-ref `.pruefer/config.yaml`; every operator-only key stays unparseable from a repo. An absent or malformed repo config is treated as no override and logged.
- **FR-018**: Removed with the repo-plugin loader (deferred to #2366). There is no repo-plugin tree to bound or to scan for symlinks.
- **FR-019**: Plugin manifest-name collisions are resolved deterministically (embedded `pruefer-review` first, then operator plugins in listed order); the first wins and each skipped duplicate is reported. No note names a local path.
- **FR-020**: The per-review plugin directory and its contents are removed on every exit path, as for the embedded plugin today.

### Key Entities

- **Operator plugin**: a local plugin directory named in the operator's `plugins:` list; trusted as the operator's own.
- **Review skill reference**: a `plugin:skill` string, selected by operator config, optionally overridden by the repo's base-ref config, resolved only against the embedded and operator plugins.
- **Fallback notice**: the statement in the posted review body, and the log, that the selected skill was replaced by the default, with reference, base SHA and reason.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: With an operator-configured `pr-review-toolkit` checkout, a review loads it, logs its name and version, and its agents are usable through `Task`.
- **SC-002**: A `review_skill` naming a skill of an operator plugin is invoked for a PR; one naming a plugin that is not loaded falls back visibly; no plugin tree of the reviewed repo is read in any tested configuration.
- **SC-003**: For every unresolved or unloadable `review_skill`, a default review is posted whose body contains the reference, the base SHA and the reason; zero reviews fail or are silently substituted.
- **SC-004**: With `repo_review_skill: false`, a repo's `review_skill` has no effect in every tested configuration, and the log states it was ignored.
- **SC-005**: A plugin hook's and MCP server's environment contains no GitHub token, App key or `.env` secret in any tested configuration.
- **SC-006**: Reviews with no operator plugins and no repo config behave as they do after #2306.
- **SC-007**: New tests are shown to be non-vacuous (each fails when its guarded behaviour is removed) and `go test -race ./...` passes.

## Assumptions

- The repo-side config file already exists (ADR 1642): `review_skill` is added as a recognised key; every operator-only setting stays settable only from the operator's local `.pruefer/config.yaml`.
- `plugins:`, `review_skill:` and `repo_review_skill:` follow the existing Pruefer configuration conventions for how a setting is supplied (config file, flag, environment) and for live reload, as `review_guidance` does.
- Operator plugin directories are trusted local paths and are passed as they are.
- The fallback is a single re-run with `pruefer-review:review`; the guidance layers apply to it as to any skill.
- Reserved name: no plugin may replace `pruefer-review`.
- The reviewer's CLI loads hooks and MCP servers from `--plugin-dir` plugins even with `--setting-sources ""`; this is verified by the same kind of live probe ADR 2306 recorded.

## Out of Scope *(optional)*

- Repo-supplied review plugins, in any form: **deferred to #2366**, which will build them on the repo's standard Claude Code plugin declarations. No code from the reviewed repo is loaded by this issue.
- Changing which Bash commands the reviewer may run, and running coded tools over the head tree (fourth issue in the chain). Operator plugin hooks and MCP servers run here, but the reviewer's own tool grant is unchanged apart from naming the selected skill.
- Installing plugins from a marketplace by name; the operator supplies directories. A marketplace installer is a possible follow-up.
- Reading any setting other than `review_skill` (beyond the keys ADR 1642 already recognises) from a repo's config.
- Loading anything from the PR head.

## Source References *(optional)*

- #1859 (the chain this replaces), #2305 (`internal/skillref`), #2306 (embedded `pruefer-review` plugin), #2192 (review tool scope and scrubbed environment), #2190, #1446 C1 (head untrusted), #2308 (builds on the pinned base SHA), #2366 (repo-supplied plugins, deferred)
- ADR 1446, ADR 2192, ADR 2305, ADR 2306, ADR 2307
