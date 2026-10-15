# Feature Specification: Pause on the first denied file edit that no allowed_tools rule can grant, and name the file

**Feature Branch**: `fabrik/issue-2301`
**Created**: 2026-10-14
**Status**: Specified
**Input**: User description:

> ## Problem
>
> Report #1981 (v0.0.82): a worker needed to edit `.claude/skills/<skill>/SKILL.md` in the target repo. Claude Code refused the `Edit` in `--permission-mode dontAsk`, even though Fabrik passes `--allowedTools Edit(<worktree>/**)`. Fabrik then re-ran the comment review again and again until the no-op comment breaker paused the issue, with a message that pointed the operator the wrong way.
>
> The unbounded loop was fixed in v0.0.83 by #1704. Both invocation paths now share one `ToolsDeniedRetries` counter and pause at `MaxToolsDeniedRetries` (default 3), ahead of the comment breakers (`engine/comments.go:581-589`, `engine/item.go:1930` and `engine/item.go:2007-2008`, bound at `engine/item.go:2282-2283`). Three problems remain in v0.0.85.
>
> **(a) Fabrik retries a denial that cannot succeed.** The Claude Code permission docs (code.claude.com/docs/en/permission-modes, "Protected paths") say writes to protected paths are **denied** in `dontAsk`. The same docs say `permissions.allow` rules "do not pre-approve protected-path writes", because the safety check runs before allow rules are evaluated. The protected directories include `.git`, `.claude` (with a few exceptions, such as `.claude/worktrees/`), `.vscode`, `.idea`, `.husky`, `.cargo`, `.devcontainer`, `.yarn`, `.mvn`, `.config/git` and any `--plugin-dir` directory. There is also a list of protected files (`.gitconfig`, `.gitmodules`, shell rc files, `.npmrc`, `.envrc` and others). The list is CLI-version-specific.
>
> Fabrik's own allow rules for the file tools are `Edit(<worktree>/**)` and `Write(<worktree>/**)` (`applyWorktreeBoundary`, `engine/boundary.go:16-34`, applied in `buildClaudeArgs`, `engine/claude.go:1390-1399`). So a denied `Edit` or `Write` on a path inside the worktree that Fabrik's own rule already covers cannot be fixed by an `allowed_tools` change, by simpler commands or by a retry. Today the engine still re-runs it up to the bound: three runs with the default.
>
> The tool result text the CLI gives Claude is the generic "Permission to use Edit has been denied because Claude Code is running in don't ask mode…". It does not say "protected path". The structured `permission_denials` entries Fabrik reads carry `tool_name` and `tool_input` (`permissionDenial`, `engine/claude.go:184-187`), not a reason. The protected-path cause therefore cannot be read from a reason field. It has to be inferred structurally from the denied call and from the rules Fabrik itself passed.
>
> **(b) The denied file is never named.** `classifyToolsDenied` (`engine/claude.go:238`) decodes `tool_input` only when `ToolName == "Bash"` (`engine/claude.go:248-250`, via `decodeToolCommand`). `claudeerr.ToolDenial` (`internal/claudeerr/claudeerr.go:215-218`) has only `ToolName` and `Command`. For an `Edit`, `Write`, `MultiEdit` or `NotebookEdit` denial, the comment says only "**Edit**". The path appears only in the session JSONL.
>
> **(c) The advice cannot help in this case.** The explanatory comment (`recordToolsDeniedDetection`, `engine/item.go:2273-2277`) and the pause comment (`pauseForToolsDeniedLimit`, `engine/item.go:2312-2320`) suggest re-running the step as simpler commands or adding an `allowed_tools` rule. For a protected path, neither works. Per the same docs, `bypassPermissions`, which Fabrik uses for `fabrik:unrestricted` (`engine/claude.go:1355-1356`), does allow protected-path writes. That is a real option, with the existing caveat that it lifts every restriction. Making the change by hand on the issue branch is the other option.
>
> ## Requirements
>
> R1. Decode the target path of a denied file-tool call:
>    - `file_path` for `Edit`, `Write` and `MultiEdit`;
>    - `notebook_path` for `NotebookEdit`.
>
>    Decoding is best-effort and never fabricates a value, the same contract as `decodeToolCommand`. Carry the path on `claudeerr.ToolDenial`.
>
>    The exact `tool_input` shape for these tools must be confirmed against a real CLI capture before relying on it. The existing code comment notes that there is no captured evidence for non-Bash shapes. If the shape differs, adapt to the evidence.
>
> R2. A denial is **ungrantable** when all three of the following hold:
>    - it is a file-tool call (`Edit`, `Write`, `MultiEdit` or `NotebookEdit`);
>    - its decoded path resolves inside the invocation's worktree;
>    - the allow list Fabrik passed for this invocation already contains a rule that covers that tool and path: the bare tool name, or `Tool(<worktree>/**)`.
>
>    The decision uses only the CLI's structured denial and Fabrik's own arguments for the invocation. It never uses the tool-result prose. It is not a hard-coded protected-path list, which would drift with CLI versions. Plan may add a documented-path match (R4) for wording only. An invocation run with `fabrik:unrestricted` is never classified this way.
>
> R3. On the **first** invocation with an ungrantable denial, on either path (stage dispatch or the comment-review/reinvoke family), the engine pauses the issue (`fabrik:paused` + `fabrik:awaiting-input`, with `EnginePaused` applied as `pauseForToolsDeniedLimit` does). It does not wait for `MaxToolsDeniedRetries`. Everything else about the denial outcome is unchanged:
>    - no `stage:<name>:failed`;
>    - no `max_retries` charge;
>    - `commitWIP` and push still run;
>    - the comment breakers are still bypassed.
>
>    Removing `fabrik:paused` resumes the item, as it does after `pauseForToolsDeniedLimit` today. Other denials (Bash, a path outside the worktree, a tool not covered by Fabrik's allow list) keep today's bounded-retry behaviour.
>
> R4. The pause comment for an ungrantable denial:
>    - names the tool and each denied path, relative to the worktree, sanitised for inline code like the Bash command is;
>    - says that Claude Code refuses writes to this path in Fabrik's non-interactive permission mode, and that no `allowed_tools` rule can change that;
>    - gives the options:
>      1. make the change by hand on the `fabrik/issue-N` branch, push it, then remove `fabrik:paused`;
>      2. restructure the work so that it does not need to write there;
>      3. add `fabrik:unrestricted` for this issue, with the existing caveat that it removes all tool restrictions.
>
>    When the path matches a directory or file in Claude Code's documented protected-path list (for example `.claude/` or `.git/`), the comment may say so and link the doc. This is wording only (see R2).
>
> R5. Named paths also reach the existing non-pausing comment and log lines. `toolsDeniedLogSummary` and `recordToolsDeniedDetection`'s comment show the first denied path for a file-tool denial, the same way they show the first command for Bash.
>
> R6. Log lines distinguish the two outcomes, e.g. `[#N tools-denied] ungrantable denial: Edit .claude/skills/x/SKILL.md (covered by Edit(<worktree>/**)) — pausing on first occurrence`.
>
> ## Scope
>
> In scope: decoding the file-tool path, the ungrantable classification, first-occurrence pause on both paths, and the comment and log wording.
>
> Out of scope, with reasons:
>
> - **TUI display of the denial.** The pause already produces a comment, labels and an attention state (`needs-human`) that the TUI and MCP surfaces show. A dedicated TUI row is a presentation feature with its own design and does not belong in a bug-fix release.
> - **Plan-time flagging of planned changes under `.claude/`.** That would need the Plan skill to predict file paths. It is a skill and prompt change with a false-positive risk, separate from engine handling of a denial that has actually happened.
> - **Bash writes to protected paths** (redirects, `tee`). Bash rule matching is not modelled by Fabrik, so these stay on the bounded #1704 path.
> - **Any change to Fabrik's default allow list or permission mode.**
>
> Mention #1981 in the PR body. Do NOT use a closing keyword (`Closes`/`Fixes`/`Resolves`) near #1981. This issue's own number gets `Closes`.
>
> ## Acceptance
>
> - A captured CLI fixture (`testdata`) of a real `dontAsk` run whose `Edit` on `<worktree>/.claude/...` was denied, recorded with Fabrik's own flags, used by the decode tests.
> - Unit tests:
>   - path decoding for each file tool, plus malformed or absent input (empty, never fabricated);
>   - the ungrantable predicate for each condition:
>     - covered by the bare name;
>     - covered by `Tool(<worktree>/**)`;
>     - a path outside the worktree is not ungrantable;
>     - a tool absent from a custom `allowed_tools` is not ungrantable;
>     - an unrestricted invocation is not ungrantable;
>     - a Bash denial is not ungrantable;
>   - first-occurrence pause on the stage path and on the comment path, with no `stage:<name>:failed`, no `max_retries` charge and `EnginePaused` set;
>   - the comment names the relative path and lists the three options;
>   - a non-ungrantable denial still follows the #1704 bound unchanged.
> - Sim twin in `tests/sim`: simclaude returns a `claudeerr.ToolsDeniedError` with an `Edit` denial on `.claude/skills/x/SKILL.md` during a comment review. Assert that the item is paused after exactly one invocation, the comment names the path, and no second invocation is dispatched. A second scenario with a Bash denial still gets the bounded retries.
> - Non-vacuity: disable the ungrantable classification and show that the sim test sees further invocations before the pause and the first-occurrence unit tests fail.
> - As-built docs:
>   - update `docs/state-machine.md` §7.3b/§7.3c and the `fabrik:tools-denied` label row;
>   - update the `docs/USER_GUIDE.md` "Tool permission denied" troubleshooting note (ungrantable denials pause at once; the options);
>   - regenerate `docs/llms-full.txt` with `bash scripts/generate-llms-full.sh` in the same commit.

## Background

Report #1981 (v0.0.82): a worker needed to edit a skill file under `.claude/` in the target repo. Claude Code refused the edit in its non-interactive permission mode, even though Fabrik grants file edits anywhere in the worktree. Fabrik then re-ran the comment review repeatedly until a no-op breaker paused the issue, with a message that pointed the operator the wrong way. Fix #1704 (v0.0.83) bounded the loop: both invocation paths now share one tool-denial counter and pause after a default of 3 denied runs. Three problems remain:

1. **Fabrik retries a denial that cannot succeed.** Claude Code refuses writes to its "protected paths" (`.git`, `.claude`, `.vscode`, `.idea`, `.husky` and others, plus some protected files) in non-interactive mode. Allow rules do not pre-approve such writes, because the safety check runs before allow rules are evaluated. The list is CLI-version-specific. So when a file edit or write is denied on a path Fabrik's own allow rule already covers, no `allowed_tools` change, simpler command or retry can help. Today Fabrik still spends up to three full runs on it. The CLI's denial record carries only the tool name and input, and its tool-result text is generic, so "this cannot be granted" has to be inferred from the denied call and the rules Fabrik itself passed. It must not be read from prose or from a hard-coded protected-path list.
2. **The denied file is never named.** The denial is decoded only for shell commands. For a file edit or write, the operator sees just the tool name, and the path appears only in the session transcript.
3. **The advice cannot help.** The existing comments suggest simpler commands or an `allowed_tools` rule. Neither works for a protected path. The real options are making the change by hand on the issue branch, restructuring the work, or the existing `fabrik:unrestricted` label, which allows protected-path writes but removes all tool restrictions.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Stop at once on a denial no rule can grant (Priority: P1)

As an operator, when a worker's file edit or write is denied on a path that Fabrik's own permissions already cover, I want the issue paused on the first occurrence, so that no tokens or time go into re-runs that cannot succeed.

**Why this priority**: This is the behavioural bug. The other stories refine what the operator sees.

**Independent Test**: Run a stage invocation and a comment-review invocation, each ending in a denied `Edit` on `.claude/skills/x/SKILL.md` inside the worktree under Fabrik's default allow list. Each issue is paused after exactly one invocation.

**Acceptance Scenarios**:

1. **Given** a comment-review invocation whose only denial is an `Edit` on a worktree path covered by the allow list, **When** the invocation ends, **Then** the issue carries `fabrik:paused` + `fabrik:awaiting-input` after that one invocation and no second invocation is dispatched.
2. **Given** the same denial in a stage-dispatch invocation, **When** it ends, **Then** the issue is paused after that one invocation with no `stage:<name>:failed` label and no `max_retries` charge.
3. **Given** the pause, **When** an operator removes `fabrik:paused`, **Then** the item resumes as it does after the existing denial-limit pause.
4. **Given** the invocation made commits before the denial, **When** the pause happens, **Then** the WIP commit and push still happen and the comment breakers are still bypassed.

---

### User Story 2 - A pause comment that names the file and gives usable options (Priority: P1)

As an operator, I want the pause comment to name the denied tool and file and tell me what I can actually do, so that I can unblock the issue without reading session transcripts.

**Why this priority**: Without it, the immediate pause is just as opaque as today's behaviour.

**Independent Test**: Trigger the Story 1 pause and read the comment.

**Acceptance Scenarios**:

1. **Given** an ungrantable denial of `Edit` on `<worktree>/.claude/skills/x/SKILL.md`, **When** the pause comment is posted, **Then** it names `Edit` and `.claude/skills/x/SKILL.md` (relative to the worktree, safe for inline code).
2. **Given** that comment, **Then** it says Claude Code refuses writes to this path in Fabrik's non-interactive mode and that no `allowed_tools` rule can change that.
3. **Given** that comment, **Then** it lists three options: make the change by hand on the `fabrik/issue-N` branch, push, then remove `fabrik:paused`; restructure the work so it does not need to write there; or add `fabrik:unrestricted` for the issue, with the caveat that it removes all tool restrictions.
4. **Given** a path in Claude Code's documented protected-path list (such as `.claude/` or `.git/`), **Then** the comment may say so and link the documentation. This is wording only and never changes the classification.

---

### User Story 3 - Denied files are named in every denial message and log (Priority: P2)

As an operator watching the log or the non-pausing explanatory comment, I want the first denied file named for any file-tool denial, so that I can see what was blocked even when the issue is not paused yet.

**Why this priority**: It is useful on its own and covers denials that stay on the bounded-retry path.

**Independent Test**: Produce a denied `Write` on a path outside the worktree. The explanatory comment and log line name the path, and the denial is not classified as ungrantable.

**Acceptance Scenarios**:

1. **Given** a denial of `Edit`, `Write`, `MultiEdit` or `NotebookEdit` with a readable target path, **When** the explanatory comment and the log summary are produced, **Then** the first denied path is shown, the same way the first command is shown for Bash.
2. **Given** an ungrantable denial, **When** the engine logs the outcome, **Then** the line says it is an ungrantable denial, names the tool, path and the covering rule, and says it is pausing on first occurrence. This line is clearly distinguishable from the bounded-retry log line.
3. **Given** a denial whose `tool_input` is missing, malformed or lacks a path, **Then** no path is shown and none is invented.

---

### User Story 4 - Every other denial behaves as today (Priority: P1)

As an operator, I want denials that a rule change or a simpler command might fix to keep their current bounded-retry behaviour, so that this fix causes no new pauses.

**Why this priority**: A wrong "ungrantable" verdict would pause issues that could have recovered, so the verdict must be conservative.

**Independent Test**: Denials that fail any one condition of the ungrantable test (Bash call, path outside the worktree, tool not covered by a custom `allowed_tools`, `fabrik:unrestricted` invocation, undecodable path) still get the retries up to `MaxToolsDeniedRetries`, then the existing pause.

**Acceptance Scenarios**:

1. **Given** a denied Bash command, **When** the invocation ends, **Then** the bounded-retry path is followed unchanged.
2. **Given** a denied file edit on a path outside the worktree, **Then** the denial is not ungrantable and the bounded-retry path is followed.
3. **Given** a custom `allowed_tools` list that omits the denied file tool, **Then** the denial is not ungrantable.
4. **Given** an invocation run under `fabrik:unrestricted`, **Then** its denials are never ungrantable.
5. **Given** an invocation with both an ungrantable file denial and another denial, **Then** it is treated as ungrantable and pauses at once, naming every ungrantable path.

---

### Edge Cases

- A denial with an empty, relative or otherwise unresolvable path: not ungrantable, and no path shown. The existing bounded behaviour applies.
- A path that only appears to be inside the worktree (`..` segments or a symlink escape) is judged on its resolved location. A path that resolves outside is not ungrantable.
- A path inside the worktree that matches no protected-path rule in the CLI (an ordinary source file) but was denied for some other reason is still classified by the structural test. The pause comment must not assert a specific cause beyond "Claude Code refused this write in non-interactive mode".
- Several denials in one invocation: the comment names every ungrantable path, de-duplicated and capped like the Bash command list.
- A pause already in effect when a further invocation reports an ungrantable denial: no duplicate comment (existing comment de-duplication applies).
- A CLI version that changes its protected-path list changes only which denials occur. It never changes how Fabrik classifies them.
- `NotebookEdit` carries its path in a different field from the other file tools.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: For a denied `Edit`, `Write` or `MultiEdit` call, the engine MUST decode the target file path from the denial's tool input. For `NotebookEdit` it MUST decode the notebook path. Decoding is best-effort: missing, malformed or non-string input yields an empty path and never a fabricated one. The decoded path is carried on the denial record alongside the tool name and, for Bash, the command.
- **FR-002**: The exact tool-input shape for these four tools MUST be confirmed against a real CLI capture before the decoder relies on it. If the capture differs from the assumed field names, the decoder follows the capture.
- **FR-003**: A denial MUST be classified **ungrantable** if and only if: (a) the tool is `Edit`, `Write`, `MultiEdit` or `NotebookEdit`; (b) the decoded path resolves inside the invocation's worktree; and (c) the allow list passed to the CLI for that invocation contains a rule covering that tool and path — the bare tool name or `Tool(<worktree>/**)`. The invocation must not have been run under `fabrik:unrestricted`.
- **FR-004**: The classification MUST use only the CLI's structured denial record and Fabrik's own arguments for the invocation. It MUST NOT use tool-result prose or a hard-coded list of protected paths.
- **FR-005**: On the first invocation that reports an ungrantable denial, on both the stage-dispatch path and the comment-review/reinvoke path, the engine MUST pause the issue (`fabrik:paused` + `fabrik:awaiting-input`, engine-pause recorded as for the existing denial-limit pause) without waiting for `MaxToolsDeniedRetries`.
- **FR-006**: An ungrantable-denial pause MUST NOT apply `stage:<name>:failed`, MUST NOT charge `max_retries`, MUST still run the WIP commit and push, and MUST still bypass the comment-processing breakers. Removing `fabrik:paused` MUST resume the item as it does after the existing denial-limit pause.
- **FR-007**: A denial that is not ungrantable (Bash, a path outside the worktree, a tool not covered by the invocation's allow list, an unrestricted invocation, an undecodable path) MUST keep the existing bounded-retry behaviour unchanged, including the shared counter, bound and wording for the Bash case.
- **FR-008**: The pause comment for an ungrantable denial MUST name the denied tool and each denied path relative to the worktree, sanitised for inline code in the same way as the Bash command.
- **FR-009**: That comment MUST state that Claude Code refuses writes to this path in Fabrik's non-interactive permission mode and that no `allowed_tools` rule can change that. It MUST list three options: (1) make the change by hand on the `fabrik/issue-N` branch, push, then remove `fabrik:paused`; (2) restructure the work so it does not need to write there; (3) add `fabrik:unrestricted` for the issue, noting that it removes all tool restrictions.
- **FR-010**: When the denied path matches a directory or file in Claude Code's documented protected-path list, the comment MAY say so and link the documentation. This affects wording only and never the classification.
- **FR-011**: The existing non-pausing explanatory comment and the denial log summary MUST show the first denied path for a file-tool denial with a readable path, in the same way they show the first command for Bash.
- **FR-012**: The log MUST distinguish the two outcomes: an ungrantable denial logs the tool, path, covering allow rule and that the engine is pausing on first occurrence. A bounded-retry denial keeps its current log line.
- **FR-013**: Test evidence MUST include a captured CLI fixture of a real non-interactive run, using Fabrik's own flags, in which an `Edit` on a worktree `.claude/...` path was denied. The decode tests use it.
- **FR-014**: Tests MUST cover: path decoding for each file tool plus malformed and absent input; each condition of the ungrantable predicate (covered by bare name, covered by `Tool(<worktree>/**)`, outside the worktree, tool absent from a custom list, unrestricted, Bash); first-occurrence pause on both paths with no `failed` label, no `max_retries` charge and the engine-pause recorded; the comment content (relative path and three options); and an unchanged bounded path for non-ungrantable denials.
- **FR-015**: A deterministic sim scenario MUST show that a comment-review invocation returning an `Edit` denial on `.claude/skills/x/SKILL.md` leaves the item paused after exactly one invocation, with the path named and no second invocation. A second scenario with a Bash denial MUST still get the bounded retries.
- **FR-016**: The change MUST be non-vacuous: with the ungrantable classification disabled, the sim scenario MUST see further invocations before the pause and the first-occurrence unit tests MUST fail.
- **FR-017**: The as-built documentation MUST be updated in the same change: the tool-permission-denial sections and the `fabrik:tools-denied` label row in `docs/state-machine.md`, the "Tool permission denied" troubleshooting note in `docs/USER_GUIDE.md` (ungrantable denials pause at once; the options), and the regenerated `docs/llms-full.txt`.
- **FR-018**: The PR body MUST mention #1981 by bare number only, with no closing keyword near it, and MUST carry `Closes #2301`.

### Key Entities *(if applicable)*

- **Denial record**: One tool call the CLI refused, with its tool name and one decoded target (a shell command for Bash, a file or notebook path for the file tools). The path is new.
- **Ungrantable denial**: A denial on a file-tool call whose path is inside the worktree and already covered by an allow rule Fabrik passed, so no further allow rule could change the outcome.
- **Invocation allow list**: The allow rules Fabrik passed to the CLI for one invocation, including the worktree-boundary rules and any stage-level `allowed_tools`.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: An ungrantable file denial is followed by exactly one Claude invocation before the issue is paused, on both stage and comment paths, where today it takes three with the default bound.
- **SC-002**: For every file-tool denial with a readable path, the operator-facing comment and the log line contain the denied path. The ungrantable pause comment contains all three remediation options.
- **SC-003**: For all denials not meeting the ungrantable test, the number of invocations before the pause is identical to the current behaviour.
- **SC-004**: No denial is classified ungrantable from tool-result text or from a fixed list of protected paths. Disabling the classification restores the old retry count and fails the new tests.
- **SC-005**: The documentation describes the first-occurrence pause and the options, and the documentation bundle is in sync with its sources.

## Assumptions

- The denial record's tool input for the four file tools uses `file_path` (`Edit`, `Write`, `MultiEdit`) and `notebook_path` (`NotebookEdit`). This is to be confirmed against the captured fixture (FR-002). The fixture wins if it differs.
- "Inside the worktree" is judged on the lexically cleaned, resolved path against the invocation's worktree directory. Relative decoded paths are not treated as inside.
- When a stage sets a custom `allowed_tools`, the worktree-boundary rules are still applied for the file tools it lists. A file tool the list omits is not covered, so its denials stay on the bounded path.
- Several ungrantable denials in one invocation are treated as one pause with all paths named, up to the same display cap used for Bash commands.
- A path that is denied for a reason unrelated to the CLI's protected paths still pauses at once, since Fabrik cannot tell the difference. The comment wording avoids over-claiming a cause.
- The documented-protected-path wording (FR-010) is optional polish. If omitted, the other requirements are unaffected.

## Out of Scope *(optional)*

- A dedicated TUI row for the denial. The pause already produces a comment, labels and a `needs-human` attention state that the TUI and MCP surfaces show.
- Plan-time flagging of planned changes under `.claude/`. That is a skill and prompt change with false-positive risk.
- Bash writes to protected paths (redirects, `tee`). Bash rule matching is not modelled, so these stay on the bounded path.
- Any change to Fabrik's default allow list or permission mode.
- Closing report #1981. It is closed by hand after a release containing the fix ships.

## Source References *(optional)*

- Claude Code permission modes, "Protected paths": code.claude.com/docs/en/permission-modes
- Report #1981 (v0.0.82), fix #1704 (v0.0.83), `docs/state-machine.md` §7.3b/§7.3c
