# Feature Specification: Pruefer read-only review tools and a scrubbed review environment

**Feature Branch**: `fabrik/issue-2192`
**Created**: 2026-10-11
**Status**: Specified
**Input**: User description: "pruefer: read-only review tools (path-scoped Read/shell paging, Task subagents) and a scrubbed review environment

## Problem

Pruefer's review tool set is too narrow to review large PRs well, and too broad in one place.

- **Too narrow.** `reviewAllowedTools` (`pruefer/claude.go`) allows only `Read`, `Grep`, `Glob` and six `git` subcommands. Reviews regularly try to page the staged diff with `sed`/`head`/`cat`, are denied, and stop early. Review bodies then say \"Bash was denied\" and conclude \"no findings\" over code they never read (see #2190 for the evidence). The reviewer also has no way to split a large PR across subagents.
- **Too broad.** `Read`, `Grep` and `Glob` are allowed with no path restriction, and the review process inherits the daemon's full environment (`mergeEnv(os.Environ(), …)`). A PR is unreviewed, attacker-controllable input. Instructions planted in it could get the reviewer to read files outside the clone, such as `.pruefer/app-private-key.pem`, `.env` or `~/.claude*`, and quote them into a review that is posted publicly. Not demonstrated; the permission rules allow it.

## Requirements

- **R1, restrict paths:** `Read`, `Grep` and `Glob` reach only the PR clone and the staged diff file(s), expressed as path-scoped permission rules. A read outside them is denied.
- **R2, read-only shell tools:** allow read-only paging and inspection commands, limited to the same paths: `sed -n`, `head`, `tail`, `wc`, `cat`, `ls`, and `rg` if present. Do not allow `find` (its prefix rule would admit `-exec`/`-delete`; `Glob` covers it). Every added rule must be shown, by test, unable to write, execute PR code, or reach the network.
- **R3, subagents:** allow the Task tool so the reviewer can split a large diff by file or area. Confirm subagents inherit the same allowlist and path scope (a test or captured transcript), and that their output still flows through the Go-owned output contract.
- **R4, environment:** pass the review process only what `claude` needs: the Claude profile selection, PATH, HOME and locale. Drop GitHub tokens, `.env` contents and other ambient secrets. Pruefer's Go code submits the review, so the session needs no GitHub credentials.
- **R5, prompt:** the review prompt names the exact tools and shell forms available, so the model doesn't guess at commands that will be denied.

## Out of scope

- **Running the PR's code** (tests, builds, linters, package managers), now or behind a flag. That is the worker's job, done under CI. Pruefer stays a reader.
- Repo-supplied review skills: that is #1859.
- Coverage reporting: #2190 builds on this issue.

## Acceptance

- A review whose session tries to `Read` `.pruefer/app-private-key.pem` (or any path outside the clone and diff) is denied; shown by a test of the generated permission arguments, and by one captured live denial.
- `sed -n`/`head`/`cat` on the staged diff succeed in a live review; a non-read-only command (e.g. `go test`, `find -exec`, `sed -i`) is denied.
- A large-PR review that uses Task subagents completes, with findings parsed through the normal contract.
- The review process environment contains no `GH_TOKEN`, `GITHUB_TOKEN`, `FABRIK_TOKEN` or other non-allowlisted variables (unit test on the built env).
- The path-scope and env tests are checked non-vacuous: they fail with the restriction removed."

## Background

Pruefer is the review bot that reads a PR and posts findings. Its review session runs `claude` with a fixed, read-only tool allowlist. Today that allowlist is wrong in both directions.

**Too narrow.** The allowlist covers only `Read`, `Grep`, `Glob` and a handful of `git` subcommands. Reviewers routinely try to page the staged diff with `sed`, `head` or `cat`, get denied, and stop early. The resulting review bodies say "Bash was denied" and then conclude "no findings" over code that was never read. A "no findings" result is therefore not evidence the code was reviewed (#2190 has the evidence and builds coverage reporting on this issue). There is also no way to split a large PR across subagents.

**Too broad.** `Read`, `Grep` and `Glob` carry no path restriction, and the review process inherits the daemon's whole environment. A PR is unreviewed, attacker-controllable input. Instructions planted in it could steer the reviewer into reading files outside the clone (the GitHub App private key under `.pruefer/`, `.env`, the Claude profile directories) and quoting them into a review that is posted publicly. This has not been demonstrated, but the current permission rules permit it.

The change widens the reviewer's useful reach (read-only paging, subagents) while narrowing its blast radius (path scope, scrubbed environment). Pruefer stays strictly a reader.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Reviewer cannot read outside the PR (Priority: P1)

A reviewer session, whether steered by injected PR content or simply mistaken, tries to read a file that is not part of the PR clone or the staged diff. The attempt is denied, and nothing outside those paths can be quoted into a posted review.

**Why this priority**: This closes a possible secret-disclosure path into public review comments. It is the only part of the issue with a security consequence.

**Independent Test**: Build the review invocation and inspect the permission arguments. A `Read`, `Grep` or `Glob` target outside the clone and diff (for example `.pruefer/app-private-key.pem`) is not permitted. One captured live review shows the denial.

**Acceptance Scenarios**:

1. **Given** a review session, **When** it tries to `Read` `.pruefer/app-private-key.pem`, **Then** the read is denied.
2. **Given** a review session, **When** it runs `Grep` or `Glob` rooted outside the clone and the staged diff, **Then** the call is denied.
3. **Given** a review session, **When** it reads a file inside the PR clone or the staged diff file, **Then** the read succeeds.

---

### User Story 2 - Reviewer pages large diffs with read-only shell commands (Priority: P1)

The reviewer pages through a large staged diff with `sed -n`, `head`, `tail`, `wc`, `cat`, `ls` (and `rg` where installed), limited to the clone and the staged diff. It no longer stops early after a denial and reports "no findings" over unread code.

**Why this priority**: This is the direct cause of the shallow and falsely clean reviews.

**Independent Test**: In a live review of a PR with a large diff, `sed -n`, `head` and `cat` on the staged diff succeed. Commands that write, execute code or reach the network are denied.

**Acceptance Scenarios**:

1. **Given** a staged diff, **When** the reviewer runs `sed -n '100,200p'`, `head`, `tail`, `wc`, `cat` or `ls` on it, **Then** the command succeeds.
2. **Given** a review session, **When** it runs `go test`, `sed -i`, `find … -exec`, or any command that writes, executes PR code or uses the network, **Then** the command is denied.
3. **Given** a read-only command pointed at a path outside the clone and diff, **When** it runs, **Then** it is denied.

---

### User Story 3 - Reviewer splits a large PR across subagents (Priority: P2)

For a large PR, the reviewer delegates parts of the diff (by file or area) to subagents and merges their results into one review.

**Why this priority**: It improves coverage on large PRs but is not needed to fix the false "no findings" problem. Paging alone helps most reviews.

**Independent Test**: Review a large PR in which the session uses Task subagents. The review completes and its findings parse through the normal Go-owned output contract. A test or captured transcript shows a subagent is bound by the same tool allowlist and path scope as its parent.

**Acceptance Scenarios**:

1. **Given** a large PR, **When** the reviewer spawns subagents, **Then** the review completes and findings are parsed through the normal output contract.
2. **Given** a subagent, **When** it tries a read outside the clone and diff or a non-read-only command, **Then** it is denied exactly as its parent would be.

---

### User Story 4 - Review process holds no ambient secrets (Priority: P1)

The process that runs the review contains only what `claude` needs. A session cannot see GitHub tokens or other daemon secrets, even through a shell command that prints its environment.

**Why this priority**: It is defence in depth for Story 1. A path-scope bug must not also expose credentials.

**Independent Test**: Unit-test the built review environment. It contains only the Claude profile selection, `PATH`, `HOME` and locale variables, and none of `GH_TOKEN`, `GITHUB_TOKEN`, `FABRIK_TOKEN` or other non-allowlisted variables.

**Acceptance Scenarios**:

1. **Given** a daemon environment containing `GH_TOKEN`, `GITHUB_TOKEN`, `FABRIK_TOKEN` and loaded `.env` values, **When** the review environment is built, **Then** none of them appear.
2. **Given** the allowlisted variables are set in the daemon, **When** the review environment is built, **Then** they are passed through so `claude` still runs and uses the correct profile.

---

### User Story 5 - Prompt matches the permitted tools (Priority: P2)

The review prompt states exactly which tools and shell forms are available, so the model does not try commands that will be denied.

**Why this priority**: It reduces wasted turns and denials, but the safety guarantees do not depend on it.

**Independent Test**: Inspect the generated review prompt. It names the permitted tools and shell forms and the path limits. It does not suggest any denied command.

**Acceptance Scenarios**:

1. **Given** a review prompt, **When** it is generated, **Then** it lists the allowed tools, the allowed shell forms and the path scope.
2. **Given** the permission set and the prompt text, **When** they are compared, **Then** every command form the prompt offers is permitted.

---

### Edge Cases

- `rg` is not installed on the host: the rule is harmless, and the prompt must not promise `rg` unless it is available.
- Symlinks inside the PR clone that point outside it (a PR can add one). A read through such a link must not escape the path scope.
- Relative paths and `..` segments that resolve outside the clone or diff.
- `sed -n` permitted but `sed -i`, `sed -e '…w file'`, or `sed` with the `e` command used to write or execute. The rule must not admit them.
- Command chaining or substitution in a permitted command (`cat file; curl …`, `$(…)`, redirection to a file) must not bypass the allowlist.
- `cat`, `head` and the like pointed at a path outside the scope through an absolute path or a glob.
- Multiple staged diff files, such as chunked diffs for large PRs: all must be readable, and nothing else outside the clone.
- A subagent given a prompt that asks it to exceed the parent's permissions.
- The environment allowlist omits a variable `claude` actually needs on some host (for example a non-default profile selector), and the review fails to start.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: `Read`, `Grep` and `Glob` are permitted only for paths inside the PR clone and the staged diff file(s), expressed as path-scoped permission rules. Any other path is denied.
- **FR-002**: The review session permits these read-only shell commands, limited to the same paths: `sed -n`, `head`, `tail`, `wc`, `cat`, `ls`, and `rg` where present.
- **FR-003**: `find` is not permitted.
- **FR-004**: No permitted shell rule can write to the filesystem, execute code from the PR, or reach the network. Each added rule is shown by a test to be unable to do so, including `sed` write/execute forms, redirection, command chaining and substitution.
- **FR-005**: The Task tool is permitted so the reviewer can split a diff across subagents.
- **FR-006**: Subagents are subject to the same tool allowlist and path scope as the parent session, shown by a test or a captured transcript.
- **FR-007**: A review that uses subagents still returns its findings through the existing Go-owned output contract, and the Go code remains the only thing that submits the review.
- **FR-008**: The review process environment is built from an explicit allowlist: the Claude profile selection, `PATH`, `HOME` and locale variables. It is not built by inheriting the daemon environment.
- **FR-009**: The review environment never contains `GH_TOKEN`, `GITHUB_TOKEN`, `FABRIK_TOKEN`, values loaded from `.env`, or any other non-allowlisted variable.
- **FR-010**: The review prompt names the exact tools, the permitted shell forms and the path scope, and offers no command form that the permission set denies.
- **FR-011**: A live review denies a `Read` of `.pruefer/app-private-key.pem`. One captured live denial is recorded as evidence.
- **FR-012**: In a live review, `sed -n`, `head` and `cat` on the staged diff succeed, and `go test`, `find -exec` and `sed -i` are denied.
- **FR-013**: The path-scope tests and the environment tests are non-vacuous: each is shown to fail when the restriction it guards is removed.
- **FR-014**: Pruefer does not run PR code (tests, builds, linters, package managers) under any setting added by this change.

### Key Entities

- **Review session**: One `claude` invocation that reviews a single PR, with its generated permission arguments, prompt and environment.
- **PR clone**: The checkout of the PR under review, the root of the permitted path scope.
- **Staged diff file(s)**: The diff Pruefer writes for the reviewer to read; the only permitted path outside the clone.
- **Review environment**: The explicit set of variables handed to the review process.
- **Subagent**: A Task-spawned child session of the reviewer.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: Zero reads outside the PR clone and staged diff succeed in a review session, shown by tests of the generated permission arguments and by one captured live denial of `.pruefer/app-private-key.pem`.
- **SC-002**: In a live review of a large-diff PR, the reviewer reads the full staged diff using only permitted commands, with no "Bash was denied" statements in the review body.
- **SC-003**: 100% of the non-read-only probes tried (`go test`, `find -exec`, `sed -i`, and write, network and chaining variants) are denied.
- **SC-004**: The built review environment contains none of `GH_TOKEN`, `GITHUB_TOKEN`, `FABRIK_TOKEN` or any non-allowlisted variable, verified by a unit test.
- **SC-005**: A large-PR review that uses Task subagents completes, and its findings are parsed through the normal output contract with no change to the contract.
- **SC-006**: The path-scope and environment tests each fail when the corresponding restriction is removed.

## Assumptions

- The review process needs only the Claude profile selection, `PATH`, `HOME` and locale to run; the exact variable set is confirmed against what `claude` actually needs when the allowlist is written.
- Pruefer's Go code, not the session, submits the review, so no GitHub credentials are needed in the session.
- Permission rules are the enforcement mechanism for path scope and command shapes. Where the rule syntax cannot express a restriction safely, the command is left out of the allowlist rather than admitted loosely.
- `Glob` stands in for `find`, so dropping `find` loses no needed capability.
- Subagent inheritance of the parent's permissions is verified rather than assumed (FR-006).
- `rg` is optional: it is permitted when present and advertised in the prompt only when available.

## Out of Scope

- Running the PR's code (tests, builds, linters, package managers), now or behind a flag. That is the worker's job, done under CI. Pruefer stays a reader.
- Repo-supplied review skills (#1859).
- Coverage reporting (#2190, which builds on this issue).

## Source References

- #2190: evidence of reviews stopping early after "Bash was denied".
- #1859: repo-supplied review skills.
- `pruefer/claude.go`: the current `reviewAllowedTools` and the review environment construction.
