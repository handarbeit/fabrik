# Feature Specification: Pruefer loads repo review plugins from the repo's standard Claude Code plugin declarations, read at the base ref

**Feature Branch**: `fabrik/issue-2366`
**Created**: 2026-10-17
**Status**: Specified
**Input**: User description: "## Problem

#2307 lets a Pruefer operator load review plugins and lets a repo choose its review skill from them. It does not let a repo supply its own review plugin code. An earlier revision of #2307 read a bespoke `.pruefer/plugins/<name>/` tree from the base ref. That approach was dropped for three reasons:

- It invents a Pruefer-only plugin format.
- The plugin has to be copied into every repo that wants it.
- The plugin is separate from the one the repo's developers already use with Claude Code.

A repo should be able to point Pruefer at a normal Claude Code plugin it already declares.

## Requirements

- **R1 (declaration source):** a repo declares plugins the standard Claude Code way. That is either `.claude/settings.json` (`extraKnownMarketplaces` and `enabledPlugins`) or an in-repo `.claude-plugin/marketplace.json` with relative plugin sources. Pruefer reads these files **at the review's resolved base SHA** (#2307's one-base-SHA rule), never from the clone. The clone is the PR head, and Pruefer keeps running the reviewer with `--setting-sources \"\"`, so Claude Code never loads the project settings itself.
- **R2 (explicit selection):** only plugins named in the base-ref `.pruefer/config.yaml` are loaded, for example `plugins: [acme-review@acme-tools]`. Every other plugin the repo enables is ignored. `review_skill:` may then name a skill from them (`plugin:skill`), and resolution uses #2307's resolver and visible fallback.
- **R3 (in-repo sources):** a plugin whose marketplace source is a path inside the repo is fetched from the base SHA through the Git Trees/Blobs API. Reuse the caps, path validation and refusal reporting preserved on branch `pruefer/repo-plugins-fetch`: per-file, per-plugin and total byte and file-count caps, decided from tree metadata before any blob read; symlinks, gitlinks, `..`, absolute paths and case collisions refused; a truncated listing loads nothing.
- **R4 (external sources):** a plugin whose source is another git or GitHub repo loads only if its marketplace is on an operator allowlist (`repo_plugin_marketplaces:` in the operator config, default empty) **and** the source resolves to a fixed commit. A pinned SHA is used as is. A tag is resolved to its SHA and the SHA is recorded. A branch, or a ref that cannot be resolved, is refused and reported. The fetch goes through the API with the same caps as R3. An external source the installation token cannot read is refused and reported, never retried with other credentials.
- **R5 (operator switch):** `repo_plugins:` in the operator config, **default off**. When off, nothing in R1–R4 runs: no reads, no fetches, no `--plugin-dir`. When on, repo plugins load only when the PR's base is the repository's default branch.
- **R6 (loading):** fetched plugins are written to the per-review temp root and passed with `--plugin-dir` after the embedded and operator plugins. The embedded-name collision rule, the scoped `Skill(<ref>)` grant, the scrubbed environment and cleanup on every exit are unchanged from #2307.
- **R7 (cost):** cache fetched plugin trees by tree SHA (in-repo) or commit SHA (external) in a bounded LRU, so an unchanged plugin costs no blob reads on later reviews.
- **R8 (record):** each review logs which repo plugins loaded, their source, the SHA each one came from, and why any were skipped. A fallback caused by a skipped plugin names that reason in the posted review.

## Out of scope

- Installing plugins through `claude plugin install`, or any use of Claude Code's own plugin cache.
- Loading repo plugins for a PR whose base is not the default branch.
- The coded review tools (#2308).

## Acceptance

- With `repo_plugins` off, a repo that declares and selects a plugin causes no tree or blob reads and no repo plugin directory in argv, and the review uses the operator's or the default skill.
- With it on, a plugin from an in-repo marketplace at the base SHA loads and its selected skill runs. A change to that plugin in the PR head has no effect on the review.
- An external plugin from an allowlisted marketplace, pinned by SHA, loads. The same plugin referenced by branch, or from a marketplace not on the allowlist, is refused and named in the log and in the fallback notice.
- A plugin enabled in `.claude/settings.json` but not named in `.pruefer/config.yaml` is not loaded.
- A second review of an unchanged plugin makes no blob reads.
- Every cap and path refusal from R3 has a test that fails with the check neutralised.
- ADR (numbered after this issue), `cmd/pruefer/README.md` and `docs/USER_GUIDE.md` (with `docs/llms-full.txt` regenerated) describe the trust model: who can supply plugin code, where it runs, and how the operator turns it off."

## Background

#2307 lets a Pruefer operator load review plugins and lets a repo choose which of those plugins' skills reviews its PRs (`review_skill`). It deliberately does not let the reviewed repo supply plugin code: the maintainers decided that #2307 ships with no code from the reviewed repo running on the Pruefer host. An earlier revision of #2307 read a bespoke `.pruefer/plugins/<name>/` tree from the base ref; that was dropped because it invents a Pruefer-only plugin format, forces every repo to copy the plugin in, and keeps the review plugin separate from the one the repo's developers already use with Claude Code. That first implementation is preserved on branch `pruefer/repo-plugins-fetch` as a reference for the tree reader, path validation, caps and refusal reporting.

This issue lets a repo point Pruefer at a normal Claude Code plugin it already declares. Pruefer reads the repo's declarations (`.claude/settings.json` and/or an in-repo `.claude-plugin/marketplace.json`) at the review's resolved base SHA, never from the PR-head clone (the clone is the untrusted head, and the reviewer runs with `--setting-sources ""`, so Claude Code never loads project settings itself). Plugins are opt-in twice: the operator must turn the capability on (default off), and the repo must name each plugin it wants Pruefer to load in its base-ref `.pruefer/config.yaml`.

Prior art (Claude Code plugin marketplace documentation): a marketplace is a repository or directory with `.claude-plugin/marketplace.json` holding a `name`, an `owner` and a `plugins` array; a plugin is identified as `<entry-name>@<marketplace-name>`; each entry's `source` is either a relative path from the marketplace root (`./plugins/x`, no `..`) or a source object (`github`, `git-subdir`, `url`, `archive`, `npm`, `command`), and git-based sources can be pinned with `ref` or `sha`. `extraKnownMarketplaces` declares marketplaces in settings and `enabledPlugins` records which `plugin@marketplace` ids are on. This spec reuses those declaration formats and does not invent a new one.

Trust framing: a repo plugin may carry hooks, MCP servers and scripts that run on the Pruefer host as the daemon's operating-system user with a scrubbed environment (ADR 2307 residual risks). Content on the base ref has been merged by the repo's maintainers, but it is still content from the reviewed repo, which is why the capability is operator-gated, default-branch-only, size-capped and, for anything outside the repo, allowlisted and pinned.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Operator keeps the feature off by default (Priority: P1)

An operator who has not opted in must see no behaviour change, even for repos that declare and select plugins.

**Why this priority**: The safety property of the whole feature; it ships default-off and everything else is additive to it.

**Independent Test**: Run a review of a PR whose base ref declares a marketplace, an enabled plugin, a `plugins:` entry and a `review_skill` naming it, with `repo_plugins` off. Assert zero tree/blob/declaration reads for repo plugins and no repo plugin directory in the reviewer's arguments.

**Acceptance Scenarios**:

1. **Given** `repo_plugins` is off (or unset), **When** a PR on a repo that declares and selects a plugin is reviewed, **Then** Pruefer makes no repo-plugin reads or fetches, passes no repo plugin directory, and the review uses the operator's skill or the default skill.
2. **Given** `repo_plugins` is off, **When** the repo's `review_skill` names a repo plugin's skill, **Then** the existing #2307 visible fallback to the default skill applies.

---

### User Story 2 - A repo's in-repo plugin reviews its PRs (Priority: P1)

A repo that keeps a review plugin in its own in-repo marketplace, names it in its base-ref `.pruefer/config.yaml` and selects one of its skills gets that skill used to review PRs.

**Why this priority**: The headline value: the repo uses the plugin its developers already have, without copying it into a Pruefer-specific layout.

**Independent Test**: With `repo_plugins` on and the PR's base the default branch, review a PR on a repo whose base ref has `.claude-plugin/marketplace.json` listing a relative-path plugin, `plugins: [acme-review@acme-tools]` and `review_skill: acme-review:review`; the skill runs. Then change the plugin in the PR head only and confirm the review is unchanged.

**Acceptance Scenarios**:

1. **Given** the capability is on and the base is the default branch, **When** the base-ref config names a plugin declared by an in-repo marketplace, **Then** the plugin is fetched from the base SHA, loaded after the embedded and operator plugins, and the selected skill runs.
2. **Given** the same setup, **When** the PR head modifies the plugin's files, **Then** the review uses the base-SHA content only.
3. **Given** a plugin is enabled in `.claude/settings.json` but not named in the base-ref `.pruefer/config.yaml`, **When** the PR is reviewed, **Then** that plugin is not loaded.
4. **Given** the PR's base is not the repository's default branch, **When** the capability is on, **Then** no repo plugin is read, fetched or loaded.

---

### User Story 3 - An allowlisted external plugin loads only at a fixed commit (Priority: P2)

A repo that references a plugin hosted in another repository can use it only if the operator has allowlisted that marketplace and the source resolves to a fixed commit.

**Why this priority**: Useful for shared organisation-wide review plugins, but it widens who can supply code, so it needs the strictest gating.

**Independent Test**: Declare an external plugin pinned by SHA from an allowlisted marketplace (loads), the same plugin by branch (refused), a tag (loads and the resolved SHA is recorded), and from a non-allowlisted marketplace (refused).

**Acceptance Scenarios**:

1. **Given** the marketplace is on the operator allowlist and the source is pinned by SHA, **When** the PR is reviewed, **Then** the plugin loads at that SHA.
2. **Given** the source is a tag, **When** the PR is reviewed, **Then** the tag is resolved to its commit SHA, the plugin loads at that SHA and the SHA is recorded in the log.
3. **Given** the source is a branch, or a ref that cannot be resolved, **When** the PR is reviewed, **Then** the plugin is refused, the reason is logged, and the review falls back visibly.
4. **Given** the marketplace is not on the allowlist (including the default empty allowlist), **When** the PR is reviewed, **Then** the plugin is refused and named in the log and the fallback notice.
5. **Given** the installation token cannot read the external repository, **When** the PR is reviewed, **Then** the plugin is refused and reported, and no other credential is tried.

---

### User Story 4 - Unchanged plugins are cheap (Priority: P2)

Repeat reviews of an unchanged plugin should not re-download it.

**Why this priority**: Without a cache every review would pay blob reads against the API budget.

**Independent Test**: Review two PRs back to back with an unchanged plugin and assert the second makes no blob reads.

**Acceptance Scenarios**:

1. **Given** a plugin's tree SHA (in-repo) or commit SHA (external) was fetched by an earlier review, **When** a later review needs it, **Then** it is served from the bounded cache with no blob reads.
2. **Given** the plugin changed on the base ref, **When** the next review runs, **Then** the new tree is fetched.

---

### User Story 5 - Operators and repo owners can see and understand what happened (Priority: P2)

Every review records what repo plugins loaded and why any did not, and the docs state the trust model.

**Why this priority**: The feature runs repo-supplied code on the host; the operator needs an audit trail and a clear statement of the trust boundary.

**Independent Test**: Review a PR with one loaded, one refused (cap exceeded) and one non-allowlisted plugin; check the log lines, then the posted fallback notice when the selected skill was among the skipped.

**Acceptance Scenarios**:

1. **Given** any review with the capability on, **When** it finishes, **Then** the log names each loaded repo plugin with its source and the SHA it came from, and each skipped plugin with the reason.
2. **Given** the selected skill belonged to a skipped plugin, **When** the review falls back, **Then** the posted fallback notice names that reason (sanitised, without local paths).

---

### Edge Cases

- A tree listing that GitHub reports as truncated loads nothing from that plugin.
- Per-file, per-plugin and total byte and file-count caps are decided from tree metadata before any blob is read; exceeding any one refuses the plugin whole.
- Symlinks, gitlinks (submodules), `..`, absolute paths and case-colliding paths in a plugin tree refuse the plugin, never "skip the offending file".
- A marketplace plugin whose declared name collides with the reserved embedded name `pruefer-review` is skipped and reported; a repo plugin whose name matches an earlier-loaded operator plugin loses to the earlier one (#2307's first-wins rule) and is reported.
- Two selected plugins of the same name from different marketplaces: the first listed wins, the other is reported.
- A name in `plugins:` that no declared marketplace provides, or whose marketplace is not declared at the base SHA, is reported as unresolved and skipped.
- A plugin source of an unsupported kind (`npm`, `archive`, `command`, a non-GitHub `url`) is refused and reported.
- A marketplace declared via `extraKnownMarketplaces` with a non-repository kind of source (for example a local directory) is ignored and reported; it is never read from the host.
- A malformed `.claude/settings.json`, `.claude-plugin/marketplace.json` or `.pruefer/config.yaml` at the base SHA yields no repo plugins, never a failed review.
- A branch and a tag with the same name make a ref ambiguous; it is refused.
- A plugin that fails to load at runtime, or whose selected skill is never invoked, falls back through #2307's existing one-re-run mechanism.
- A repo-supplied value (plugin name, marketplace name, reason text) that reaches the posted review is sanitised as #2307 already does for `review_skill`.
- The memoisation of an already-reviewed head (ADR 1952) is unchanged: a changed plugin declaration does not re-review a head that was already reviewed.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The operator config has a `repo_plugins` setting, default **off**. When off, Pruefer performs no repo-plugin declaration reads, tree reads, blob reads or external fetches, and passes no repo plugin directory to the reviewer.
- **FR-002**: When `repo_plugins` is on, repo plugins are considered only if the PR's base branch is the repository's default branch; otherwise nothing is read, fetched or loaded, and the log says so.
- **FR-003**: Pruefer reads the repo's plugin declarations — `.claude/settings.json` (`extraKnownMarketplaces`, `enabledPlugins`) and `.claude-plugin/marketplace.json` — only at the review's resolved base SHA (the one SHA used for all base-ref reads under #2307), never from the review clone.
- **FR-004**: A repo plugin is loaded only if it is named, as `<plugin>@<marketplace>`, in a `plugins` list in the base-ref `.pruefer/config.yaml`. A plugin that is enabled in `.claude/settings.json` but not named there is not loaded. The repo `plugins` key is parsed by the same strict repo-config struct as other repo keys, so no operator-only key becomes readable from a repo file.
- **FR-005**: A named plugin is resolved through a marketplace the repo declares at the base SHA: the in-repo `.claude-plugin/marketplace.json` (relative sources resolve from the repository root) or a marketplace in `extraKnownMarketplaces`. A name that does not resolve is skipped and reported.
- **FR-006**: The repo's `review_skill` (a `plugin:skill` reference) may name a skill of a loaded repo plugin; reference resolution, the well-formedness check, and the visible fallback to the default skill follow #2307 unchanged.
- **FR-007**: A plugin whose marketplace source is a path inside the reviewed repo is fetched from the base SHA through the Git Trees and Blobs API, not from the clone.
- **FR-008**: Before any blob is read, the tree metadata is checked against per-file, per-plugin and total byte caps and file-count caps; exceeding any cap refuses the plugin. Symlinks, gitlinks, `..` segments, absolute paths and case-colliding paths refuse the plugin. A truncated tree listing refuses the plugin. Each refusal is logged with its reason.
- **FR-009**: A plugin sourced from another GitHub repository (`github` or `git-subdir` sources) loads only if its marketplace is on the operator allowlist `repo_plugin_marketplaces` (default empty) **and** the source resolves to a fixed commit. A 40-hex SHA is used as is; a tag is resolved to its commit SHA, which is recorded; a branch, an ambiguous ref, or an unresolvable ref is refused and reported.
- **FR-010**: External plugins are fetched through the API with the same caps and path validation as FR-008. An external source the installation token cannot read is refused and reported; no other credential is tried and nothing is retried with other credentials.
- **FR-011**: Allowlist matching is keyed on the marketplace's resolved source repository, not on a name the reviewed repo chooses for it, so a repo cannot gain an allowlisted identity by naming its own marketplace like an allowlisted one.
- **FR-012**: Source kinds other than in-repo relative paths, `github` and `git-subdir` on the same GitHub host as the review (for example `npm`, `archive`, `command`, a non-GitHub `url`) are refused and reported.
- **FR-013**: Fetched plugins are written to the per-review temporary root and passed to the reviewer with `--plugin-dir`, after the embedded and operator plugins. The reserved embedded name rule, the scoped `Skill(<ref>)` grant (the run's own reference only), the scrubbed reviewer environment and removal of the temporary root on every exit path are unchanged from #2307.
- **FR-014**: Fetched plugin trees are cached in a bounded in-memory LRU keyed by tree SHA (in-repo) or commit SHA (external). A later review of an unchanged plugin performs no blob reads.
- **FR-015**: Each review with the capability on logs every loaded repo plugin with its source and the SHA it came from, and every skipped plugin with the reason.
- **FR-016**: When a fallback is caused by a skipped repo plugin, the posted review's fallback notice names the reason. Repo-controlled text in the notice is sanitised as in #2307 and the notice contains no local path.
- **FR-017**: Every cap and path-validation refusal in FR-008, and every gating rule in FR-001, FR-002, FR-004, FR-009 and FR-011, is covered by a test that fails when that specific check is neutralised.
- **FR-018**: The settings `repo_plugins` and `repo_plugin_marketplaces` are configurable through the same channels as the existing operator review-plugin settings (flag, environment and YAML), and the existing static guard that forbids any repo plugin reader in production sources is replaced by one that proves the reader is reachable only behind `repo_plugins`.
- **FR-019**: An ADR numbered after this issue (`adrs/2366-…`), `cmd/pruefer/README.md` and `docs/USER_GUIDE.md` document the trust model — who can supply plugin code (repo maintainers via the base ref; allowlisted external repositories at a fixed commit), where it runs (on the Pruefer host as the daemon's OS user, with the scrubbed environment, not bound by the reviewer's path-scoped tool rules), and how the operator turns it off. `docs/llms-full.txt` is regenerated, and the architecture guide gains an entry.

### Key Entities

- **Repo plugin selection**: the `plugins` list in the base-ref `.pruefer/config.yaml`, each entry `<plugin>@<marketplace>`.
- **Declared marketplace**: a marketplace the repo declares at the base SHA, either its own in-repo `.claude-plugin/marketplace.json` or an `extraKnownMarketplaces` entry.
- **Marketplace allowlist**: the operator's `repo_plugin_marketplaces`, the set of external marketplace sources permitted to supply plugin code.
- **Fixed source reference**: the commit SHA (given, or resolved from a tag) that identifies exactly which bytes of a plugin were loaded.
- **Plugin tree cache**: a bounded LRU of validated plugin trees keyed by tree SHA or commit SHA.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: With `repo_plugins` off, a review of a repo that declares and selects a plugin makes zero repo-plugin tree or blob reads and no repo plugin directory appears in the reviewer's arguments.
- **SC-002**: With `repo_plugins` on, a plugin from an in-repo marketplace at the base SHA loads and its selected skill runs; the same PR with plugin files changed only in the head produces the same loaded plugin content.
- **SC-003**: An external plugin from an allowlisted marketplace pinned by SHA loads; the same plugin by branch, and a plugin from a non-allowlisted marketplace, are each refused and named in both the log and the fallback notice.
- **SC-004**: A plugin enabled in `.claude/settings.json` but absent from the base-ref `.pruefer/config.yaml` is never loaded.
- **SC-005**: A second review of an unchanged plugin makes zero blob reads.
- **SC-006**: For every cap and path refusal rule there is a test that passes with the rule in place and fails with it neutralised.
- **SC-007**: For every review with the capability on, the log shows each loaded plugin's source and SHA and each skipped plugin's reason.
- **SC-008**: The ADR, `cmd/pruefer/README.md` and `docs/USER_GUIDE.md` state the trust model and the off switch, and `docs/llms-full.txt` matches its regeneration.

## Assumptions

- The repo's `plugins` key in `.pruefer/config.yaml` takes `<plugin>@<marketplace>` entries, matching Claude Code's install-id format. It is distinct from the operator's `plugins:` setting (a list of local directories) and appears only in the repo config.
- Naming a plugin in `.pruefer/config.yaml` is the opt-in. Its marketplace must be declared (in-repo, or in `extraKnownMarketplaces`), but the plugin need not also be set to `true` in `enabledPlugins`; `enabledPlugins` is a declaration source, not a second gate.
- A marketplace's identity for allowlisting is its resolved GitHub source (`owner/repo`, compared case-insensitively), not the `name` field in its manifest.
- An allowlisted external marketplace's own manifest and any relative-path plugins it lists are read from that marketplace repository at a fixed commit, under the same pin-or-resolve rule as external plugin sources and the same caps.
- Only repositories on the same GitHub host Pruefer reviews against are fetchable; other hosts are treated as unsupported source kinds.
- Marketplace entry fields beyond `source` that substitute for a plugin's own manifest are not applied: a plugin must be loadable as a directory by `--plugin-dir`. One that is not is skipped and reported.
- Cache bounds and the byte/file caps are fixed, documented constants (starting from the values on branch `pruefer/repo-plugins-fetch`), not operator settings. The cache is in memory and does not survive a restart.
- A repo plugin that loads but whose skill the effective `review_skill` does not select is simply unused. `repo_review_skill: false` (the operator's veto on the repo choosing a skill) does not itself stop repo plugins from loading; `repo_plugins: off` is the operator's switch for that.
- The default-branch check uses the repository's default branch as reported by GitHub for the PR's base repository.
- Existing #2307 behaviour — one base SHA per review, the plugin order, the scrubbed environment, the single fallback re-run and memoisation of reviewed heads — is unchanged except for the additions above.

## Out of Scope *(optional)*

- Installing plugins through `claude plugin install`, or any use of Claude Code's own plugin cache or marketplace state.
- Loading repo plugins for a PR whose base is not the default branch.
- Sandboxing plugin hooks, MCP servers or scripts beyond the existing scrubbed environment (the residual risk recorded in ADR 2307 stands).
- Source kinds other than in-repo relative paths and GitHub-hosted `github`/`git-subdir` sources (`npm`, `archive`, `command`, non-GitHub git hosts).
- Per-repo or per-plugin operator allowlists for in-repo plugins, and persistent (on-disk) caching.
- The coded review tools (#2308).

## Source References *(optional)*

- Issue #2307 and `adrs/2307-pruefer-operator-and-base-ref-review-plugins.md` (one base SHA, plugin set, fallback, deferred repo-plugin design, residual risks)
- Branch `pruefer/repo-plugins-fetch` (reference tree reader, caps, path validation, refusal reporting)
- ADR 2306, ADR 1446, ADR 1642, ADR 1952
- Claude Code documentation: "Create a marketplace" and the marketplace reference (marketplace.json schema, plugin sources, `ref`/`sha` pinning)
