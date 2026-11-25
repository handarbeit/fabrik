# Feature Specification: Keep the installation token off git's command line in Pruefer's review clone

**Feature Branch**: `fabrik/issue-2622`
**Created**: 2026-11-25
**Status**: Specified
**Input**: User description: 

> ## Problem
>
> Pruefer's per-review clone puts the GitHub App installation token on `git`'s command line. `pruefer/worktree.go` builds the remote as
>
> ```
> https://x-access-token:<token>@github.com/<owner>/<repo>.git
> ```
>
> (`cloneURL`), and `cloneRefAt` passes that URL as an argument to `git fetch --depth 1 -q <url> <ref>` (and to its fallback fetch by head SHA). While each fetch runs, any local process that can list processes (`ps`) can read the token. It is also visible to every git subprocess spawned from that command.
>
> Mitigations already in place, which this issue must keep:
> - `runGit` redacts the token from git's error output.
> - `.git/FETCH_HEAD`, which records the fetched URL, is removed after checkout (#2192).
> - The clone is a per-review temp directory created with `git init` + `fetch <url>`, so no remote containing the URL is written to `.git/config`.
>
> The token is short-lived (~1 h) and is not a PAT, and API requests already authenticate with the `Authorization: Bearer` header. No token is ever sent as a URL query parameter. The risk is the argv exposure on a shared host.
>
> ## Requirements
>
> - R1. No Pruefer git invocation carries a credential in its argv. The remote URL passed to `git` is `https://github.com/<owner>/<repo>.git` with no userinfo.
> - R2. Pass the credential to git through the environment only. Either set `GIT_CONFIG_COUNT`/`GIT_CONFIG_KEY_<n>`/`GIT_CONFIG_VALUE_<n>` on the `runGit` command to supply an `http.https://github.com/.extraHeader` (`Authorization: Basic base64(x-access-token:<token>)`), or use an equivalent credential helper. Choose the approach in Plan, and prefer one that writes no secret to disk. Use the engine's App git credential (ADR 1846) as a reference only. Pruefer must not import `engine` (ADR 1113).
> - R3. The credential reaches only the clone's own git processes. It is not written to `.git/config`, to any file under the clone, or to the daemon's own process environment.
> - R4. Keep the existing mitigations: error-output redaction (extend it to the encoded header value), FETCH_HEAD removal, and the single-use temp clone. The reviewer's sandbox and environment must not gain the credential. Leave the scrubbed review environment unchanged.
> - R5. Behaviour is otherwise unchanged. This covers the ref fetch, the head-SHA fallback, the checkout, and the failure reporting.
>
> ## Acceptance
>
> - A test asserts that no argv of any git command run by the clone path contains the token or a userinfo URL. Capture the commands through the existing git seam, or an equivalent one.
> - A test asserts that the token is absent from the clone's `.git/config` and from every file under the clone after `CloneForReview` returns.
> - A test asserts that the token does not appear in the redacted error output when a fetch fails, in either raw or base64-encoded form.
> - Non-vacuity: restoring the userinfo URL makes the argv test fail.
> - An existing real-git clone test, against a local served repo or a fixture, still passes.
> - `docs`/ADR: record the decision in an ADR numbered after this issue, and update the `CLAUDE.md` Pruefer bullet if it describes the clone credential.
>
> ## Scope
>
> `pruefer/worktree.go` and its tests, plus any small shared helper this needs. The out-of-scope items:
> - Engine git credentials.
> - API authentication, which already uses a header.
> - Token lifetime.

## Background

Pruefer reviews a pull request in a per-review temporary clone. To fetch the PR ref it currently embeds the GitHub App installation token in the remote URL (`https://x-access-token:<token>@github.com/<owner>/<repo>.git`) and passes that URL as an argument to `git fetch`, including the fallback fetch by head SHA. For the duration of each fetch, any local process that can list processes can read the token from the command line, and every git subprocess spawned from that command inherits the same visibility.

The exposure matters on a shared host. The practical risk is bounded: the token is a short-lived (~1 h) installation token, not a PAT. API requests already send it in an `Authorization: Bearer` header, and it is never sent as a URL query parameter. The remaining gap is only the process-argument exposure.

Mitigations that already exist and must be preserved: git error output is redacted of the token; `.git/FETCH_HEAD` (which records the fetched URL) is removed after checkout (#2192); and the clone is a throwaway directory built with `git init` plus a fetch by URL, so no remote URL containing the credential is ever written to the clone's git configuration.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - No credential in any git process's arguments (Priority: P1)

An operator runs Pruefer on a host shared with other users or processes. While a review clones a PR, nobody who can list processes can read the installation token from any git command line.

**Why this priority**: This is the whole point of the issue; it closes the only remaining place the token is exposed in process arguments.

**Independent Test**: Run the clone path with the git invocations captured through the existing git seam, and assert that no captured argument contains the token or a URL with userinfo, for the ref fetch and the head-SHA fallback fetch alike.

**Acceptance Scenarios**:

1. **Given** a review clone of `owner/repo` with an installation token, **When** the clone path runs its ref fetch, **Then** the remote URL given to git is `https://github.com/owner/repo.git` and no argument contains the token or any userinfo.
2. **Given** the ref fetch fails and the clone falls back to fetching by head SHA, **When** the fallback runs, **Then** its arguments likewise carry no credential.
3. **Given** the credential is delivered to git by another route, **When** git performs the authenticated fetch, **Then** the fetch still succeeds and the checkout is identical to before.

---

### User Story 2 - The credential is confined to the clone's own git processes and leaves nothing behind (Priority: P1)

The credential reaches only the git processes that clone the PR, and after the clone completes it exists nowhere on disk under the clone and nowhere in the daemon's own environment.

**Why this priority**: Moving the token out of the command line is worthless if it lands in a file, the clone's git configuration, or the daemon's long-lived environment, where it is more durable and more widely readable.

**Independent Test**: After the clone returns, search `.git/config` and every file under the clone for the token (raw and encoded), and confirm the daemon process's environment and the reviewer's environment do not contain it.

**Acceptance Scenarios**:

1. **Given** a completed clone, **When** `.git/config` and every file under the clone are inspected, **Then** neither the raw token nor its encoded form appears in any of them.
2. **Given** a clone in progress, **When** the daemon's own process environment is inspected, **Then** it holds no credential for git; only the clone's own git subprocesses receive it.
3. **Given** the review then runs, **When** the reviewer's sandbox and environment are inspected, **Then** neither gained the credential, and the scrubbed review environment is unchanged.

---

### User Story 3 - Failures remain diagnosable without leaking the credential (Priority: P2)

When a fetch fails, the operator still sees git's error output, with the credential redacted in every form it could appear.

**Why this priority**: Error output is the one place the credential could still surface; it must stay redacted, and the existing failure reporting must keep working.

**Independent Test**: Force a fetch failure whose git output echoes the credential in raw and in encoded form, and assert the reported error contains neither.

**Acceptance Scenarios**:

1. **Given** a fetch that fails with output containing the raw token, **When** the error is reported, **Then** the token is redacted.
2. **Given** a fetch that fails with output containing the encoded credential (the value carried in the authentication header), **When** the error is reported, **Then** the encoded value is redacted too.
3. **Given** a fetch failure, **When** the failure is reported, **Then** the error otherwise reads as it did before (same context, same classification).

---

### Edge Cases

- A token containing characters that need escaping in a URL no longer matters for the URL, but the delivered credential must still be encoded correctly so authentication succeeds.
- The head-SHA fallback fetch must carry the credential the same way as the primary fetch.
- A failure mid-clone must not leave the credential behind in the temp directory; the temp clone is still removed as today.
- Git subprocesses spawned by git itself during the fetch (helpers, transports) inherit the environment; the credential must still not appear in any argument and must not reach the reviewer.
- Redaction must cover the credential in the form git may echo it, not only the raw token.
- `.git/FETCH_HEAD` removal and the single-use temp clone behave exactly as before; with no userinfo in the URL it can no longer record a credential.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: No git invocation made by Pruefer's review clone path carries a credential in its arguments; the remote URL passed to git is `https://github.com/<owner>/<repo>.git` with no userinfo.
- **FR-002**: The credential is supplied to git through the process environment only (a per-invocation configuration entry supplying an HTTP authorization header for `https://github.com/`, or an equivalent credential helper). The mechanism is chosen in Plan, preferring one that writes no secret to disk.
- **FR-003**: The credential is visible only to the git processes of the clone itself; it is not written to `.git/config`, to any file under the clone, or to the daemon's own process environment.
- **FR-004**: Error-output redaction continues to remove the token and is extended to the encoded form of the credential delivered to git.
- **FR-005**: `.git/FETCH_HEAD` removal after checkout and the single-use per-review temp clone are retained unchanged.
- **FR-006**: The reviewer's sandbox and environment do not gain the credential, and the scrubbed review environment is unchanged.
- **FR-007**: All other clone behaviour is unchanged: the ref fetch, the head-SHA fallback fetch, the checkout, and failure reporting.
- **FR-008**: Pruefer does not import the `engine` package to achieve this (ADR 1113); the engine's App git credential (ADR 1846) is a reference only.
- **FR-009**: The decision is recorded in an ADR numbered after this issue (`adrs/2622-…`), and the `CLAUDE.md` Pruefer bullet is updated if it describes the clone credential.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: Across every git command the clone path runs (ref fetch and head-SHA fallback), zero arguments contain the token or a userinfo URL, verified by an automated test that captures the commands.
- **SC-002**: After a clone completes, zero occurrences of the token (raw or encoded) exist in `.git/config` or in any file under the clone, verified by an automated test.
- **SC-003**: When a fetch fails, zero occurrences of the token in raw or encoded form appear in the reported error, verified by an automated test.
- **SC-004**: Re-introducing the userinfo URL makes the argument-capture test fail (the test is not vacuous).
- **SC-005**: The existing real-git clone test against a local served repo or fixture continues to pass without changes to its expectations.

## Assumptions

- The installation token continues to be a short-lived App token obtained as today; token lifetime and issuance are unchanged.
- Choosing between a configuration-entry header and a credential helper is a design decision for Plan, subject to FR-002/FR-003; either satisfies this spec if it writes no secret to disk and confines the credential to the clone's git processes.
- The existing git-command seam used by the clone tests is sufficient to capture arguments; an equivalent seam may be used if not.
- A small shared helper may be introduced if it keeps the change minimal.

## Out of Scope *(optional)*

- Engine git credentials (the engine's App git credential, ADR 1846).
- API authentication, which already uses an `Authorization` header.
- Token lifetime or rotation.
- Any change to the scrubbed review environment or the reviewer's sandbox beyond ensuring it does not gain the credential.

## Source References *(optional)*

- #2192 — `.git/FETCH_HEAD` removal after checkout (existing mitigation).
- ADR 1846 — the engine's App git credential (reference only).
- ADR 1113 — Pruefer must not import `engine`.
