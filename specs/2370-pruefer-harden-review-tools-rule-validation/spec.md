# Feature Specification: Harden Pruefer `review_tools` rule validation

**Feature Branch**: `fabrik/issue-2370`
**Created**: 2026-10-17
**Status**: Specified
**Input**: User description (verbatim original issue body):

> ## Problem
>
> #2308 (PR #2368) lets an operator grant coded review tools via `review_tools:`, with the repo's base-ref config only narrowing that set. Review found three hardening gaps. None of them is exploitable by a PR author today, because only the operator writes the rules. They are defence in depth for operator mistakes.
>
> 1. **`validateReviewToolRule` is a denylist with holes.** These rules pass validation but can run code, write files or reach the network: `Bash(tar *)`, `Bash(less *)`, `Bash(vim *)`, `Bash(stdbuf *)`, `Bash(flock *)`, `Bash(watch *)`, `Bash(busybox sh)`, `Bash(command sh)`, `Bash(rsync *)`, `Bash(java -jar x)`. `Bash(git diff *)` is also accepted, and `--output=` writes files.
> 2. **The interpreter check is case-sensitive.** `Bash(Python3 x)` passes, and on a case-insensitive filesystem (macOS) it resolves to the refused binary.
> 3. **No runtime re-validation.** `checkReviewArgsScopedTools` runs only in tests. `buildReviewArgs` (`pruefer/claude.go`) appends `req.Grant.Extras` without re-checking them. The extras come only from config validated at load today, but nothing at the point of use enforces that.
>
> ## Requirements
>
> - R1: Add `stdbuf`, `flock`, `watch`, `busybox`, `command`, `tar`, `rsync`, `less`, `vim`/`vi`, `java` (and review similar wrappers or exec-capable tools) to the refused set in `reviewToolsInterpreters` or equivalent.
> - R2: Refuse `git` rules that permit file-writing options (at least `--output`), or document why a git rule is acceptable.
> - R3: Compare executable names case-insensitively (and on basename, so a path prefix does not bypass).
> - R4: `buildReviewArgs` returns an error, and the review posts nothing (the same outcome as any other setup failure), if any extra fails `toolRuleValid`. Test this with an extra injected past config validation.
> - R5: Tests for each newly refused shape, proven non-vacuous.
> - R6: Update ADR 2308, `cmd/pruefer/README.md` and the User Guide (regenerate `docs/llms-full.txt`) for the expanded denylist.
>
> ## Scope
>
> Pruefer only. No change to the intersection rule, the author-trust gate or the base-ref read.

## Background

Issue #2308 (PR #2368) lets a Pruefer operator grant the reviewer a small set of coded tools through `review_tools:` rules such as `Bash(shellcheck *)`. A repository's base-ref config can only narrow that operator-granted set, never widen it. Each rule is checked when config loads, and rules that start an interpreter or shell are refused.

Review of that change found three weaknesses in the rule check. A PR author cannot exploit any of them today, because only the operator writes `review_tools:` rules. They matter as defence in depth, because an operator mistake or a future change to where rules come from could turn them into a way to run code, write files or reach the network from inside a review.

1. **Denylist gaps.** The refused-executable list is a denylist, and these exec-capable, file-writing or network-capable executables get through: `tar`, `less`, `vim`, `stdbuf`, `flock`, `watch`, `busybox`, `command`, `rsync` and `java`. `git diff *` is also accepted, and `git diff --output=<file>` writes a file.
2. **Case-sensitive matching.** `Bash(Python3 x)` passes, although on a case-insensitive filesystem such as macOS it resolves to the refused binary.
3. **No check at the point of use.** The scoped-tool argument check runs only in tests, and the review command line is built by appending the granted rules without re-checking them. Today they come only from config validated at load, but nothing at the point of use enforces that.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Refuse exec-capable and file-writing tools at config load (Priority: P1)

An operator who lists a rule for a tool that can run arbitrary code, write files or reach the network gets a clear refusal when the config loads. This covers the tools listed in the issue and similar wrappers and tools, so a mistaken grant never reaches a review.

**Why this priority**: This is the core gap. Every other change is only useful if the validator itself is correct.

**Independent Test**: Load operator config containing each newly refused rule shape and confirm each is refused with an error naming the rule. Confirm ordinary rules such as `Bash(shellcheck *)` still load.

**Acceptance Scenarios**:

1. **Given** an operator config with `Bash(tar *)`, **When** it loads, **Then** the rule is refused and the error names the rule and the reason.
2. **Given** a rule whose executable is any of `less`, `vim`, `vi`, `stdbuf`, `flock`, `watch`, `busybox`, `command`, `rsync` or `java`, **When** it loads, **Then** it is refused.
3. **Given** `Bash(git diff --output=out.txt *)` or any git rule that permits `--output` (in either `--output=<file>` or `--output <file>` form), **When** it loads, **Then** it is refused.
4. **Given** a previously accepted, non-exec rule such as `Bash(shellcheck *)`, **When** it loads, **Then** it is still accepted.

---

### User Story 2 - Executable names compared case-insensitively and by basename (Priority: P1)

A rule cannot dodge the refusal list by changing the case of the executable name or by prefixing a path.

**Why this priority**: Without this, any list extension from User Story 1 is bypassed by `Python3`, `TAR` or `/usr/bin/tar`.

**Independent Test**: For each refused executable, load rules written in mixed case, upper case and with path prefixes, and confirm each is refused.

**Acceptance Scenarios**:

1. **Given** `Bash(Python3 x)`, **When** it loads, **Then** it is refused as the interpreter it is.
2. **Given** `Bash(/usr/bin/tar *)` or `Bash(./vim x)`, **When** it loads, **Then** it is refused.
3. **Given** `Bash(TAR *)`, **When** it loads, **Then** it is refused.

---

### User Story 3 - Review setup re-checks every granted rule (Priority: P2)

When the review command line is built, every granted extra rule is validated again. If any fails, the review is not run and nothing is posted, the same outcome as any other setup failure. This holds even if a bad rule reached the grant by a path other than config validation.

**Why this priority**: It adds a second line of defence at the point of use. It matters only when load-time validation has already failed or been bypassed, so it ranks below the validator fixes.

**Independent Test**: Build a review request whose grant carries an invalid extra rule injected past config validation. Confirm argument building returns an error and that the review run posts nothing.

**Acceptance Scenarios**:

1. **Given** a review grant containing a rule that fails validation, **When** the review arguments are built, **Then** an error is returned that names the offending rule.
2. **Given** that error, **When** the review is run, **Then** no review comment is posted and no reviewer process is started.
3. **Given** a grant whose rules are all valid, **When** the arguments are built, **Then** behaviour is unchanged from today.

---

### User Story 4 - Operators can learn what is refused (Priority: P3)

The documentation lists the expanded refused set and the git `--output` rule, so operators can see why a rule was refused and what to use instead.

**Why this priority**: The documentation has to match the behaviour, but it adds nothing until the behaviour exists.

**Independent Test**: Read ADR 2308, the Pruefer README and the User Guide, and confirm each states the expanded refused set, the case-insensitive and basename comparison, and the runtime re-check. Confirm the generated documentation bundle is up to date.

**Acceptance Scenarios**:

1. **Given** the updated documentation, **When** an operator looks up refused tools, **Then** the list matches what the validator refuses.
2. **Given** the documentation change, **When** CI's documentation-drift check runs, **Then** it passes.

---

### Edge Cases

- A refused executable inside a compound or wrapped rule, such as `Bash(env tar *)` or `Bash(command sh)`: the wrapper itself is refused, and the validator does not need to parse what the wrapper would run.
- A path-prefixed or mixed-case executable that resolves to a refused name, such as `/usr/bin/Vim`, `./TAR` or `Python3`: refused after lower-casing and reducing to the basename.
- Executable names with a version suffix or extension, such as `python3.12` or `java.exe`: existing handling for interpreters is kept, and the new names get the same treatment.
- A rule for a legitimate tool whose name merely contains a refused name as a substring, such as `lesspipe`-like or `tarpit` names: not refused, because matching is on the whole executable name.
- A git rule that does not permit `--output`, such as `Bash(git log *)`: its status is decided by the requirement on git rules below, not by this change.
- An empty extras list: argument building succeeds exactly as today.
- A rule that is valid at load but fails the re-check at build time: the review stops with no output, with the same behaviour as any other setup failure.
- Rules from the repo's base-ref config are only ever a subset of the operator set. This change does not alter that.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The refused-executable set for `review_tools` rules MUST include `stdbuf`, `flock`, `watch`, `busybox`, `command`, `tar`, `rsync`, `less`, `vim`, `vi` and `java`.
- **FR-002**: The change MUST review other similar wrappers and exec-capable, file-writing or network-capable tools beyond those named, and add those that are clearly in the same class to the refused set. The tools added and the rationale MUST be recorded in the documentation.
- **FR-003**: A `git` rule that permits file-writing options, at least `--output` in both `--output=<file>` and `--output <file>` forms, MUST be refused. Other git rules are not made refused by this requirement.
- **FR-004**: Executable names in rules MUST be compared case-insensitively and on the basename, so that neither letter case nor a path prefix bypasses the refused set. This applies to the existing interpreter set as well as the new names.
- **FR-005**: Building the review command line MUST re-validate every granted extra rule with the same predicate used when checking built arguments. If any rule fails, argument building MUST return an error that names the rule.
- **FR-006**: When argument building fails under FR-005, the review MUST NOT start a reviewer process and MUST post nothing, the same outcome as any other review setup failure.
- **FR-007**: Each newly refused rule shape MUST have a test that fails if the corresponding check is removed. This covers every named executable, case variants, path-prefixed variants, the git `--output` forms and the runtime re-check using an extra injected past config validation.
- **FR-008**: ADR 2308, the Pruefer README and the User Guide MUST describe the expanded refused set, the case-insensitive and basename comparison, the git `--output` rule and the runtime re-check. `docs/llms-full.txt` MUST be regenerated in the same change.
- **FR-009**: Rules that are valid today and do not name a refused executable or a git file-writing option MUST continue to be accepted.
- **FR-010**: The change MUST NOT alter the narrowing-only intersection rule, the author-trust gate or the base-ref read. It is limited to Pruefer.

### Key Entities *(if applicable)*

- **Review tool rule**: An operator-supplied permission string of the form `Bash(<executable> <args>)` that grants the reviewer one coded tool.
- **Refused set**: The executables that may never be granted because they can run arbitrary code, write files or reach the network.
- **Grant extras**: The validated list of rules attached to a review request and appended to the reviewer's allowed tools.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: Every executable named in the issue (`tar`, `less`, `vim`, `vi`, `stdbuf`, `flock`, `watch`, `busybox`, `command`, `rsync`, `java`) is refused at config load, in lower case, mixed case and with a path prefix.
- **SC-002**: Every git rule that permits `--output` is refused at config load, and every previously valid non-exec rule used in existing tests is still accepted.
- **SC-003**: A review request with an invalid rule injected past config validation produces an error before any reviewer process starts, and nothing is posted.
- **SC-004**: Each new test fails when its corresponding check is temporarily removed.
- **SC-005**: The documented refused set matches the validator's refused set exactly, and the documentation-drift check passes.

## Assumptions

- #2308 (PR #2368) is the baseline for this work. At the time of writing that PR is still open, so this issue is implemented on top of its merged state. If it has not merged, the work waits for it, or rebases onto it, rather than reintroducing the feature.
- The git rule is handled by refusing file-writing options (`--output` at minimum). Documenting git rules as acceptable would leave `--output=` open, so refusal is chosen. If more file-writing git options turn up during review of the git subcommands, they are included.
- The refused set stays a denylist. Moving to an allowlist of permitted tools is a larger design change and is not part of this issue.
- Refusal happens at config load, as today, and the runtime re-check is a second line of defence. Operators see the error at startup, not at review time, unless a rule bypasses config validation.
- A re-check failure is treated as a setup failure and not as a review verdict, so it posts no comment, consistent with other setup failures.

## Out of Scope *(optional)*

- Changing the intersection rule, which restricts the repo's base-ref config to narrowing the operator set.
- Changing the author-trust gate or how the base-ref is read.
- Replacing the denylist with an allowlist, or parsing the commands that wrappers would run.
- Any change to the Fabrik engine outside Pruefer.
- Loading repo-supplied plugins, which is tracked separately in #2366.

## Source References *(optional)*

- #2308 and PR #2368: the feature this change hardens.
- ADR 2308: the decision record to update.
