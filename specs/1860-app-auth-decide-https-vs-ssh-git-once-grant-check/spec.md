# Feature Specification: App auth — decide HTTPS-vs-SSH git once

**Feature Branch**: `fabrik/issue-1860`
**Created**: 2026-11-07
**Status**: Specified
**Input**: User description:

> ## Problem
>
> Under GitHub App auth, whether git runs over HTTPS as the installation is decided **twice**, by two independent `git config --get-regexp` subprocesses:
>
> 1. In `New()`: `setUpGitHubAppAuth` → `AppGitUsesHTTPS(cfg.GitSSH)` sets the grant requirement. `contents:write` is required only if HTTPS.
> 2. In `Run()`: `checkURLRewrite()` decides whether `setUpAppGitCredential` injects the credential helper (`engine/poll.go`, the `setUpAppGitCredential(ctx, !e.cfg.GitSSH && !httpsToSSH)` call).
>
> If the effective `url.*.insteadOf` state changes between the two reads, they disagree. The harmful direction is when the grant check sees SSH and doesn't require `contents:write`, but `Run()` sees HTTPS and injects the helper: every push then 403s with the installation token.
>
> Raised by Pruefer on PR #1847 (thread on `engine/poll.go`, left unresolved there by design). Deferred from that PR because it was release-gating: the window is milliseconds at startup, in one process, and only opens if `~/.gitconfig` is rewritten inside it.
>
> ## Fix
>
> Compute `httpsGit` once in `New()` and store it on the Engine. Pass it into the grant check (`setUpGitHubAppAuth`, via `resolveGitHubAppAuth`), and have `Run()` read the stored value for `setUpAppGitCredential`.
>
> Watch out for the App-auth test engines (`newAppAuthTestEngine`), which bypass `New()`. They must set the stored value explicitly, or `Run()` needs a defined fallback for engines built outside `New()`. Don't let a zero value silently mean "SSH" and skip injection.
>
> `checkHTTPSCredentials`' advisory use of `httpsToSSH` is independent and can keep its own read.
>
> ## Acceptance
>
> 1. One computation of the HTTPS-vs-SSH decision feeds both the grant requirement and the injection decision.
> 2. A test proves they can't diverge: e.g. the decision is fixed at construction, and a git config change afterwards doesn't alter `Run()`'s injection.
> 3. Existing App-auth git tests still pass unchanged in behaviour.

## Background

Under GitHub App authentication, the engine must know whether git operations run over HTTPS (and therefore push as the App installation, using the installation token) or over SSH. That single fact drives two separate startup decisions:

1. **The permission requirement.** When git runs over HTTPS, the installation must have write access to repository contents; startup refuses an installation without it. When git runs over SSH, that permission is not required.
2. **The credential injection.** When git runs over HTTPS, the engine injects a credential helper so engine and worker git pushes authenticate as the installation. When git runs over SSH, nothing is injected.

Today these two decisions are made by two independent reads of the machine's git configuration (URL rewrite rules that redirect HTTPS URLs to SSH), one during engine construction and one later when the engine starts running. If the git configuration changes between the two reads, the decisions can disagree. The harmful disagreement is: the permission check concludes "SSH" and does not require content-write access, then the later read concludes "HTTPS" and injects the helper, so every push is attempted with an installation token that lacks write access and is rejected (403).

The window is very small (milliseconds, within one process, only if the git configuration is rewritten inside it), which is why it was deferred from the App-auth release work (PR #1847, raised by the Pruefer reviewer). It is still a correctness gap: two answers to one question should not be possible. Engines constructed outside the normal path (notably the App-auth test engines) must not silently get a default that means "SSH" and skip injection.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - One decision feeds both the grant check and the injection (Priority: P1)

As an operator running the engine under GitHub App auth, I want the HTTPS-vs-SSH determination made exactly once at startup so that the permission the engine demands and the credential setup it performs always agree.

**Why this priority**: This is the whole fix; it removes the possibility of the 403-on-every-push disagreement.

**Independent Test**: Construct an App-auth engine, then change the machine's git URL-rewrite configuration, then start the engine. Verify the credential-injection decision matches what the permission check used at construction, regardless of the change.

**Acceptance Scenarios**:

1. **Given** git configuration that resolves to HTTPS at construction, **When** the git configuration is changed to rewrite HTTPS to SSH before the engine starts running, **Then** the engine still treats git as HTTPS for credential injection (matching the permission requirement it enforced).
2. **Given** git configuration that resolves to SSH at construction, **When** the git configuration is changed to remove the rewrite before the engine starts running, **Then** the engine still treats git as SSH and injects no credential helper (matching the permission check, which did not require content-write access).
3. **Given** the engine's SSH setting explicitly enabled, **When** the engine starts, **Then** git is treated as SSH for both decisions, as today.

---

### User Story 2 - Engines built outside the normal constructor behave safely (Priority: P2)

As a maintainer writing tests (or other code) that build an App-auth engine without the normal constructor, I want a defined, non-silent behavior for the HTTPS-vs-SSH decision so an unset value can never quietly mean "SSH" and skip credential injection.

**Why this priority**: Without it, the fix could be defeated or existing App-auth git tests could silently stop exercising injection.

**Independent Test**: Build an engine without the normal constructor, start it, and verify the credential-injection decision is deterministic and documented (either explicitly set by the builder or resolved by a defined fallback), never an accidental "SSH".

**Acceptance Scenarios**:

1. **Given** an App-auth engine built outside the normal constructor with the decision set explicitly to HTTPS, **When** it starts, **Then** credential injection occurs.
2. **Given** an App-auth engine built outside the normal constructor with no explicit decision, **When** it starts, **Then** the outcome follows the defined fallback rule and is not an unintended "SSH, skip injection".

---

### User Story 3 - Advisory credential check is unaffected (Priority: P3)

As an operator, I want the existing advisory startup check about HTTPS credentials to keep working as it does now.

**Why this priority**: Guards against scope creep and regressions; that check is independent and advisory.

**Independent Test**: Run the existing tests for the advisory HTTPS-credentials check; they pass without modification.

**Acceptance Scenarios**:

1. **Given** the advisory HTTPS-credentials check, **When** it runs, **Then** it behaves exactly as before, using its own read of the git rewrite state.

---

### Edge Cases

- The git configuration changes after construction but before start: the stored decision wins; no re-read feeds injection.
- Non-App-auth (PAT) engines: behavior unchanged; they do not use this decision for injection.
- The git-configuration read fails or is unavailable at construction: the decision follows the same rule as today for that case (no new failure mode introduced).
- The engine is restarted in-process (re-exec on upgrade or restart signal): the new process computes the decision afresh at its own construction, as it does today.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: Under GitHub App auth, the HTTPS-vs-SSH git decision MUST be computed exactly once, during engine construction.
- **FR-002**: The single computed decision MUST feed the installation permission requirement (content-write required only for HTTPS).
- **FR-003**: The same stored decision MUST be what the engine uses when it starts running to decide whether to inject the installation credential helper; the engine MUST NOT re-read git configuration to make that decision.
- **FR-004**: Engines constructed outside the normal constructor MUST have a defined behavior for this decision: either the builder sets it explicitly, or a documented fallback applies. An unset value MUST NOT silently mean "SSH" and skip injection.
- **FR-005**: The advisory HTTPS-credentials check MAY continue to perform its own independent read of git rewrite state; its behavior MUST be unchanged.
- **FR-006**: A test MUST demonstrate that the permission requirement and the injection decision cannot diverge: after construction, changing the git configuration does not change what the engine injects.
- **FR-007**: Existing App-auth git tests MUST continue to pass with unchanged behavior.

### Key Entities *(if applicable)*

- **HTTPS-vs-SSH git decision**: A single boolean fact, "git runs over HTTPS as the installation", derived from the SSH setting and the machine's git URL-rewrite configuration, fixed at engine construction.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: There is exactly one place where the HTTPS-vs-SSH decision is computed for App-auth startup, and both the permission requirement and the injection decision consume its result.
- **SC-002**: A regression test fails if the injection decision is re-derived from git configuration after construction, and passes with the single stored decision.
- **SC-003**: Zero scenarios exist in which the engine requires no content-write permission yet injects the credential helper (or the reverse), however the git configuration changes during startup.
- **SC-004**: The pre-existing App-auth git test suite passes without behavioral changes to its expectations.

## Assumptions

- The decision is fixed for the lifetime of a process; a change to git configuration takes effect on the next process start (restart or re-exec), which is acceptable and matches the issue's intent.
- For engines built outside the normal constructor, either explicit setting by the builder or a defined fallback is acceptable; choosing which is a Plan-stage decision, constrained by FR-004.
- Only the App-auth path is in scope; PAT-mode behavior is unchanged.

## Out of Scope *(optional)*

- Changing the advisory HTTPS-credentials check beyond leaving it independent.
- Re-detecting git configuration at runtime or reacting to later configuration changes.
- Changing which permissions are required, or how the credential helper works.
- Pruefer's own App-auth startup (this issue concerns the engine only).
