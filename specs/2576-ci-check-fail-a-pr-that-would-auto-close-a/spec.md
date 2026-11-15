# Feature Specification: CI check that fails a PR which would auto-close a community report

**Feature Branch**: `fabrik/issue-2576`
**Created**: 2026-11-14
**Status**: Specified
**Input**: User description: "## Problem

GitHub closes an issue when a PR that names it after a closing keyword merges into the default branch. It matches word adjacency, not grammar, so a sentence that says it does *not* close an issue still closes it. Community reports on the public triage board (#3) must be closed by hand after a release ships (CLAUDE.md, \"Handling Community Bug Reports\"), but nothing enforces this.

It has happened again. PR #2556's body read \"This PR does not close #2497; that report is closed by hand after a release ships.\" When it merged on 2026-11-06, GitHub closed community report 2497 as completed, with no comment and before any release. The report was also only partly addressed. The Implement stage wrote that sentence even though the spec forbade a closing keyword. Spec wording alone is not enough.

## Requirements

1. **R1 — CI check.** Add a required check that runs on every PR (`opened`, `edited`, `synchronize`, `reopened`). It finds every issue the PR would close on merge and fails if any of them is on project board #3.
2. **R2 — Same grammar as GitHub.** Match `close`, `closes`, `closed`, `fix`, `fixes`, `fixed`, `resolve`, `resolves` and `resolved`, case-insensitive, optionally followed by a colon. The target may be `#N`, `owner/repo#N` or an issue URL in this repository. Negations do not exempt a match, and neither does Markdown formatting such as bold, italics or links. Code spans and fenced code blocks are exempt only if GitHub really ignores them there; verify this before relying on it, and record the result in the test fixtures. The check scans:
   - the PR body;
   - the message of every commit on the PR, since commit messages also close issues when they land on the default branch.
3. **R3 — Board-membership signal.** Whether an issue is a community report is decided by its membership of board #3, read live, not by its author or `author_association`. The maintainer's own reports are on board #3 too (for example the one fixed by #2550/#2551). `GITHUB_TOKEN` cannot read an org Projects v2 board, so the check needs a token that can. Reuse an existing secret where possible, or document a new one with read-only scope.
4. **R4 — Fail closed, clearly.** If board #3 cannot be read, the check fails, and it says it could not verify. The failure message names each offending issue, the matched phrase and where it was found (PR body, or a commit SHA), and suggests safe wording: \"mentioned for context: #N\" or \"#N is out of scope here\".
5. **R5 — Merge-train PRs.** Integration and catch-up PRs carry member commits, so the commit scan covers them and a member's closing keyword cannot slip through a trial. The `Closes #<work-issue>` lines Fabrik relies on are unaffected, because work issues are on project #1, not board #3. Add a test showing the check passes on a normal Fabrik PR.
6. **R6 — Tests.** Use fixture-driven tests of the matcher, including a negated sentence (the #2556 shape), a bold keyword, `owner/repo#N`, an issue URL, a commit-message match and a clean body. Keep the matcher pure, so the tests need no network access.

## Scope

- In scope: the workflow, the matcher with its tests, and a CLAUDE.md note that the check exists.
- Out of scope: rewriting existing PR bodies; changing how Fabrik writes `Closes #N` for work issues.
- Mentioned for context: report 2497, PR #2556.

## Acceptance

- A PR whose body or commits put a closing keyword next to a board #3 issue number fails the check, even inside a negation, and the failure message names the issue.
- A normal Fabrik work PR passes.
- An unreadable board fails the check with a \"could not verify\" message."

## Background

GitHub closes an issue when a PR naming it after a closing keyword merges into the default branch. It matches word adjacency, not grammar, so a sentence that says the PR does *not* close an issue still closes it. Community reports on the public triage board (project board #3) must be closed by hand after a release containing the fix has shipped (CLAUDE.md, "Handling Community Bug Reports"), but nothing enforces this.

It has already happened twice. PR #2556's body read "This PR does not close #2497; that report is closed by hand after a release ships." When it merged on 2026-11-06, GitHub closed community report 2497 as completed, with no comment and before any release, and the report was only partly addressed. The Implement stage wrote that sentence even though the spec forbade a closing keyword, so spec wording alone is not enough; the guard has to be mechanical.

Existing infrastructure relevant to this: the repository already has a workflow that routes community issues to board #3 using the `ADD_TO_PROJECT_PAT` secret (`route-community-issues.yml`), which shows a token able to reach that org board already exists. The default `GITHUB_TOKEN` cannot read an org Projects v2 board.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Block a PR that would close a community report (Priority: P1)

A maintainer, a contributor, or the Fabrik Implement stage opens or edits a PR whose body or commit messages put a closing keyword next to the number of an issue that sits on board #3. The PR check fails before merge and tells the author exactly what to change.

**Why this priority**: This is the whole point of the issue; it prevents a report from being closed before a release ships.

**Independent Test**: Run the check against a PR body of the #2556 shape ("This PR does not close #N; …") where #N is on board #3. The check fails and names #N, the matched phrase and the PR body.

**Acceptance Scenarios**:

1. **Given** a PR body containing "does not close #N" and #N is on board #3, **When** the check runs, **Then** it fails and names issue N, the phrase "close #N" and "PR body".
2. **Given** a PR with a commit whose message says "Fixes #N" and #N is on board #3, **When** the check runs, **Then** it fails and names issue N, the phrase and the commit SHA.
3. **Given** a PR body with `**Closes** #N`, `Resolves: owner/repo#N` or `fixed https://github.com/<owner>/<repo>/issues/N` for a board #3 issue, **When** the check runs, **Then** each form is detected and the PR fails.
4. **Given** a PR whose body says "mentioned for context: #N" for a board #3 issue, **When** the check runs, **Then** it passes.

---

### User Story 2 - Fabrik's own work PRs keep passing (Priority: P1)

Fabrik-generated PRs carry `Closes #<work-issue>` and must not be disturbed, including merge-train integration and catch-up PRs that carry member commits.

**Why this priority**: A check that blocks normal pipeline PRs would stop the engine; it must not produce false positives on the intended `Closes` lines.

**Independent Test**: Run the check on a normal Fabrik PR whose body has `Closes #<work-issue>` where that issue is on the engine board (project #1) and not on board #3. It passes.

**Acceptance Scenarios**:

1. **Given** a PR body containing `Closes #W` where W is not on board #3, **When** the check runs, **Then** it passes.
2. **Given** a merge-train integration PR whose member commits contain `Closes #W` for work issues only, **When** the check runs, **Then** it passes.
3. **Given** a merge-train integration PR where a member commit contains a closing keyword for a board #3 issue, **When** the check runs, **Then** it fails and names that commit SHA.

---

### User Story 3 - Fail closed when the board cannot be read (Priority: P1)

If the check cannot read board #3 (token missing, expired, lacking scope, or a GitHub error), it must not silently pass.

**Why this priority**: A silently passing check would recreate the exact gap this issue closes.

**Independent Test**: Run the check with the board lookup failing; the check fails with a message that says it could not verify.

**Acceptance Scenarios**:

1. **Given** a PR containing at least one closing-keyword reference and the board read fails, **When** the check runs, **Then** it fails with a "could not verify" message that says why the board could not be read.
2. **Given** a PR containing no closing-keyword reference at all, **When** the check runs and the board cannot be read, **Then** [see FR-009] the outcome is defined and does not depend on the board.

---

### Edge Cases

- Negated sentences ("does not close #N", "never fixes #N"): still a match.
- Markdown formatting around the keyword (bold, italics, links) does not exempt a match.
- Code spans and fenced code blocks: exempt only if GitHub really ignores keywords there. This must be verified against real GitHub behaviour and the result recorded in the test fixtures; until verified, treat them as matches.
- Keyword followed by a colon (`Closes: #N`) matches.
- Case-insensitive keywords (`CLOSES #N`, `fixed #N`).
- Targets of the form `owner/repo#N` and issue URLs resolve to a repository plus number; board membership is checked against that identity, not the bare number, so `#N` in this repository and `#N` in another repository are not confused.
- The same issue referenced several times, or in both the body and a commit: reported once per issue with all locations, not once per occurrence in a way that hides other issues.
- A PR with many commits: every commit message is scanned, not only the head commit.
- PR body edited after opening (`edited`) or new commits pushed (`synchronize`): the check re-runs and re-evaluates both.
- A PR that closes several issues of which only some are on board #3: fails, naming only the board #3 ones.
- An issue number that does not exist or is a PR: not on board #3, so not an offender.
- A PR opened from a fork, where repository secrets are normally unavailable: the check must still reach a defined outcome (see Assumptions).

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: A check runs on every PR on `opened`, `edited`, `synchronize` and `reopened`, and is configurable as a required status check.
- **FR-002**: The check finds every issue the PR would close on merge by scanning (a) the PR body and (b) the message of every commit on the PR.
- **FR-003**: The matcher recognises the keywords `close`, `closes`, `closed`, `fix`, `fixes`, `fixed`, `resolve`, `resolves`, `resolved`, case-insensitively, optionally followed by a colon, followed by a target of the form `#N`, `owner/repo#N`, or an issue URL in this repository.
- **FR-004**: Negation in the surrounding sentence and Markdown formatting (bold, italics, links) never exempt a match.
- **FR-005**: Code spans and fenced code blocks are exempt only if verified to be ignored by GitHub when determining closing references; the verification result is recorded in the test fixtures. If it cannot be verified, they are treated as matches.
- **FR-006**: An issue is classified as a community report solely by its membership of project board #3, read live at check time. Author and `author_association` are not used.
- **FR-007**: The check fails if any issue the PR would close is on board #3. The failure message names each offending issue, each matched phrase, where it was found (PR body, or a commit SHA), and suggests the safe wordings "mentioned for context: #N" and "#N is out of scope here".
- **FR-008**: If board #3 cannot be read, the check fails with a message that states it could not verify and why, instead of passing.
- **FR-009**: When the PR contains no closing-keyword reference to any issue, the check passes without needing to read board #3.
- **FR-010**: The check uses a credential able to read the org Projects v2 board; it reuses an existing repository secret where possible, and any new secret is documented with read-only scope. The credential is never printed in logs or failure messages.
- **FR-011**: Merge-train integration and catch-up PRs are covered by the commit scan, so a member commit's closing keyword for a board #3 issue fails the check on that PR.
- **FR-012**: A normal Fabrik work PR with `Closes #<work-issue>` for an issue not on board #3 passes; a test demonstrates this.
- **FR-013**: The matcher is a pure function over text (PR body, commit messages) and returns structured matches (issue identity, phrase, location); it performs no network access.
- **FR-014**: The matcher is covered by fixture-driven tests, including: the #2556 negated shape, a bold keyword, `owner/repo#N`, an issue URL, a commit-message match, a code span and a fenced block (with the verified GitHub behaviour recorded), and a clean body.
- **FR-015**: CLAUDE.md gains a note stating that the check exists, what it enforces and how to word PR bodies safely.

### Key Entities *(if applicable)*

- **Closing reference**: a keyword plus target found in the PR body or a commit message; carries the target issue identity (repository and number), the matched phrase and the location.
- **Community report**: an issue that is a member of project board #3 at check time.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A PR whose body or commits put a closing keyword next to a board #3 issue number fails the check 100% of the time, including inside a negation, and the failure output names the issue.
- **SC-002**: A normal Fabrik work PR (including merge-train integration and catch-up PRs with only work-issue closing references) passes the check.
- **SC-003**: With board #3 unreadable and a closing reference present, the check fails and the output contains a "could not verify" message; it never reports success in that state.
- **SC-004**: The matcher's test suite runs offline and covers every fixture class listed in FR-014.
- **SC-005**: After the check is made required, no community report on board #3 is closed by a PR merge without a human action.

## Assumptions

- Board #3 is `https://github.com/orgs/handarbeit/projects/3`, the same board used by the community-issue routing workflow; the check reads item membership live rather than caching it.
- The existing `ADD_TO_PROJECT_PAT` secret is reused if its scope allows reading board items; if a read-only token is preferable or the existing scope is insufficient, a new read-only secret is documented. Which one is used is a planning decision, not a spec question.
- For fork PRs, where secrets are not exposed, the check must still run with a credential and must never execute code supplied by the PR; if that cannot be arranged, it fails closed with the "could not verify" message rather than passing. The mechanism is a planning decision.
- GitHub applies closing keywords from the PR description and commit messages when the PR merges into the default branch; closing behaviour for PR titles and non-default base branches is not part of this check unless verification shows otherwise.
- Work issues sit on project #1 (the engine board) and never on board #3, so existing `Closes #<work-issue>` lines are unaffected.
- Making the check "required" in branch protection is a repository setting applied by a maintainer after the workflow exists; the workflow itself only needs to provide a stable check name.
- Whether code spans and fenced code blocks are ignored by GitHub is unknown until verified; the safe default is to treat them as matches.

## Out of Scope *(optional)*

- Rewriting or editing existing PR bodies.
- Changing how Fabrik writes `Closes #N` for work issues.
- Automatically reopening a report that was already closed (for example report 2497).
- Checks on issue comments, PR titles or review text, unless verification shows GitHub treats them as closing references.

## Source References *(optional)*

- CLAUDE.md, "Handling Community Bug Reports"
- `.github/workflows/route-community-issues.yml` (board #3 routing and its token)
- Mentioned for context: report 2497, PR #2556
