# Feature Specification: Self-upgrade rebuilds `+dirty` dev builds and always logs why it did not

**Feature Branch**: `fabrik/issue-2234`
**Created**: 2026-10-14
**Status**: Specified
**Input**: User description: "## Problem

`extractBinarySHA` (`internal/selfupgrade/devbuild.go`) accepts only `dev(<sha>)`. It requires the version to end in `)`. A build from a checkout with uncommitted or untracked files is stamped `dev(<sha>)+dirty`, so the function returns `\"\"`. `CompareDevBuild` then skips the binary-vs-HEAD check (`ShaMismatch` is false). If the checkout's HEAD already equals `origin/main`, because something else pulled it, the function returns \"up to date\" and calls `statusClear()`. Nothing is rebuilt and **nothing is logged**: the log ends at `[upgrade] checking origin/main ...`.

Seen on 2026-10-13: the dev daemon restarted at 15:43:03 as `dev(ca4c0a2)+dirty`, with the checkout at `b2e0af68`. One untracked file had made it dirty. It kept running the stale `ca4c0a2` binary. The same happens for Pruefer's dev builds, since the code is shared.

## Requirements

- **R1:** `extractBinarySHA` accepts `dev(<sha>)` with an optional `+dirty` suffix (and any other suffix the build stamps), and returns the SHA. A dirty binary whose SHA differs from HEAD is then rebuilt.
- **R2:** decide, and record in the ADR or code comment, whether a `+dirty` build at the *same* SHA as HEAD should rebuild. A rebuild would pick up committed changes but still carry the dirt.
- **R3:** every way `CompareDevBuild` decides not to rebuild logs one line saying why: up to date, local ahead or diverged, unparseable version, not a source checkout. A silent \"checking ...\" with no outcome must not happen again.
- **R4:** if the checkout is dirty, log that once with the dirty paths. That's the usual reason a dev daemon stops upgrading.

## Acceptance

Unit tests for each version shape (`dev(abc)`, `dev(abc)+dirty`, release `v0.0.84`, garbage). A test that `dev(old)+dirty` with HEAD = origin/main ≠ old needs a rebuild, shown to fail with R1 removed.

## Notes

- Related: ADR-1196 (the shared self-upgrade package).
- Separately, something pulls `~/dev/fabrik` every 30 minutes without rebuilding. That's what produced the HEAD = origin/main, stale-binary state. Still unidentified."

## Background

Fabrik and Pruefer share one dev-build self-upgrade path (the shared self-upgrade package, ADR-1196). A dev daemon decides whether it is stale by comparing the commit SHA embedded in its version string with the source checkout's current HEAD, and by comparing HEAD with `origin/<base>`.

The embedded SHA is only recognised when the version has the exact shape `dev(<sha>)`. A binary built from a checkout with uncommitted or untracked files is stamped `dev(<sha>)+dirty`. That shape is not recognised, so the binary-vs-HEAD check is silently skipped. If something else has already moved the checkout to `origin/main`, the remote comparison also reports "up to date". The result is a daemon that keeps running an old binary indefinitely, and a log that ends at `[upgrade] checking origin/main ...` with no outcome.

On 2026-10-13 the dev daemon restarted as `dev(ca4c0a2)+dirty` while the checkout was at `b2e0af68`. A single untracked file had made the build dirty, and it kept running the stale `ca4c0a2` binary. Pruefer's dev builds are affected identically because the code is shared.

Two failures are therefore being fixed: a version shape the parser wrongly rejects, and a decision path that gives the operator no explanation when it declines to rebuild. A dirty checkout is the usual reason a dev daemon stops upgrading, and the operator currently has no way to see that.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A stale dirty binary is rebuilt (Priority: P1)

An operator runs a dev daemon built from a checkout that had an untracked file, so its version is `dev(<old>)+dirty`. Later the checkout's HEAD moves to a different commit (for example because another process pulled it). On the next upgrade check the daemon recognises that its embedded SHA no longer matches HEAD and rebuilds and re-executes.

**Why this priority**: This is the incident. A daemon silently running code that is days old defeats the purpose of self-upgrade.

**Independent Test**: Compare a version of `dev(old)+dirty` against a checkout whose HEAD equals `origin/main` and differs from `old`. The comparison reports that a rebuild is needed. With the suffix tolerance removed, the same test fails.

**Acceptance Scenarios**:

1. **Given** a binary version `dev(old)+dirty` and a checkout whose HEAD is a different commit that equals `origin/main`, **When** the upgrade check runs, **Then** it reports a SHA mismatch and a rebuild is needed.
2. **Given** a binary version `dev(abc)` and HEAD starting with `abc`, **When** the check runs, **Then** behaviour is unchanged from today.
3. **Given** a binary version with any other `+suffix` after the closing parenthesis, **When** the check runs, **Then** the embedded SHA is still extracted.

---

### User Story 2 - Every "no rebuild" decision says why (Priority: P1)

An operator reads the daemon log after an upgrade check that did not rebuild. The log contains exactly one line stating the outcome and the reason, so a check never ends at "checking origin/main ..." without a result.

**Why this priority**: The incident went undiagnosed because nothing was logged. Silence is the defect, independent of the parser fix.

**Independent Test**: For each non-rebuilding outcome, run the check and assert one explanatory line was emitted.

**Acceptance Scenarios**:

1. **Given** the local checkout equals `origin/<base>` and the binary SHA matches HEAD, **When** the check runs, **Then** one line says the build is up to date.
2. **Given** local HEAD is ahead of or diverged from `origin/<base>`, **When** the check runs, **Then** one line says so and that nothing was pulled.
3. **Given** the version string is not a dev build or carries no parseable SHA, **When** the check runs, **Then** one line says the binary-vs-HEAD check was skipped because the version could not be parsed, naming the version string.
4. **Given** the directory is not a source checkout of the expected remote, **When** the check runs, **Then** one line says self-upgrade does not apply and why.
5. **Given** the fetch or ref resolution fails, **When** the check runs, **Then** the existing error line is still logged.

---

### User Story 3 - A dirty checkout is reported with its paths (Priority: P2)

When the source checkout has uncommitted or untracked changes, the operator is told once, with the dirty paths, since that is the usual reason a dev daemon stops upgrading.

**Why this priority**: It explains the root cause of the incident directly, but the P1 stories already fix the behaviour.

**Independent Test**: Run the check on a checkout with an untracked file and assert one line naming it. Run again in the same process and assert it is not repeated.

**Acceptance Scenarios**:

1. **Given** a checkout with an untracked file, **When** the check runs, **Then** one line reports the checkout is dirty and lists the dirty paths.
2. **Given** the same dirty state on later checks in the same process, **When** the check runs again, **Then** the dirty-paths line is not repeated.
3. **Given** a clean checkout, **When** the check runs, **Then** no dirty line is logged.

---

### User Story 4 - A dirty build at the same SHA as HEAD is left alone, explicitly (Priority: P2)

A binary stamped `dev(<sha>)+dirty` whose SHA already equals HEAD is not rebuilt on that basis alone, and the log says why. The decision is recorded in the code or ADR.

**Why this priority**: The requirement asks for a recorded decision. Getting it wrong would cause an endless rebuild loop.

**Independent Test**: With version `dev(<HEAD>)+dirty` and HEAD equal to `origin/main`, the check reports no rebuild needed and logs the dirty-same-SHA explanation.

**Acceptance Scenarios**:

1. **Given** version `dev(<HEAD sha>)+dirty` and HEAD equal to `origin/<base>`, **When** the check runs, **Then** no rebuild occurs and one line explains that the build is dirty at the current commit and a rebuild would not clear that.
2. **Given** version `dev(<HEAD sha>)+dirty` and `origin/<base>` ahead of HEAD, **When** the check runs, **Then** the existing pull-and-rebuild path runs as for a clean build.

---

### Edge Cases

- A short SHA stamped in the version is a prefix of HEAD (existing prefix comparison); a `+dirty` suffix does not change that.
- An empty SHA in `dev()` or `dev()+dirty` is treated as unparseable, not as a match.
- A release version such as `v0.0.84` is not a dev build; the existing skip applies and now logs that reason.
- A garbage or empty version string is unparseable and logs that reason.
- Listing dirty paths must not itself fail the check: if the dirty-state query errors, the check proceeds and logs at most a short note.
- A very large number of dirty paths is truncated in the log rather than flooding it.
- Callers that only want to report staleness (the busy-board warning, #1464) must not gain new side effects from the added logging.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The embedded-SHA extractor accepts `dev(<sha>)` followed by an optional suffix, including `+dirty` and any other suffix the build stamps after the closing parenthesis, and returns `<sha>`.
- **FR-002**: The extractor returns an empty result for non-dev versions (for example a release `v0.0.84`), for garbage, and for an empty SHA.
- **FR-003**: A dev binary whose embedded SHA does not prefix the checkout's HEAD is reported as needing a rebuild, whether or not its version carries a `+dirty` suffix.
- **FR-004**: A `+dirty` build whose embedded SHA already matches HEAD does not by itself trigger a rebuild. This decision and its reason (the rebuild would still be dirty and would re-trigger on every start, so the check would loop) are recorded in a code comment or the ADR.
- **FR-005**: Every outcome of the upgrade check that does not rebuild logs one line stating the reason: up to date; local ahead or diverged; unparseable version; not a source checkout; dirty at current commit.
- **FR-006**: If the checkout has uncommitted or untracked changes, the check logs that fact once per process, with the dirty paths.
- **FR-007**: The fix applies to both consumers of the shared package (Fabrik and Pruefer) without per-consumer changes beyond the shared code.
- **FR-008**: Existing behaviour for clean `dev(<sha>)` builds, release builds and error paths is unchanged apart from the added log lines.
- **FR-009**: Tests cover each version shape (`dev(abc)`, `dev(abc)+dirty`, release `v0.0.84`, garbage), the dirty-binary-needs-rebuild case, and each "no rebuild" log line; the rebuild case demonstrably fails if FR-001 is reverted.

### Key Entities *(if applicable)*

- **Dev build version string**: The version stamped into a binary built from source, shaped `dev(<sha>)` with an optional suffix such as `+dirty`.
- **Upgrade check outcome**: The decision (rebuild or not) together with the single log line that explains it.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A `dev(<old>)+dirty` daemon whose checkout HEAD differs from `<old>` rebuilds on its next upgrade check; the 2026-10-13 scenario no longer leaves a stale binary running.
- **SC-002**: In 100% of upgrade checks, the log contains an outcome line (rebuild reason or no-rebuild reason) after any "checking origin/..." line.
- **SC-003**: An operator with a dirty checkout can identify the dirty paths from the daemon log without inspecting the checkout.
- **SC-004**: A dirty build at the same SHA as HEAD never causes a rebuild-and-re-exec loop.
- **SC-005**: The new tests pass, and the rebuild-needed test for `dev(old)+dirty` fails when the suffix tolerance is removed.

## Assumptions

- Decision on the open design point: a `+dirty` build at the same SHA as HEAD does **not** rebuild. A rebuild would pick up no committed changes (the SHA already matches) and would be stamped dirty again, so each start would re-trigger it. The dirty state is surfaced through the log line instead. This is recorded as the answer to the issue's second requirement.
- The suffix after the parenthesis is treated as opaque; only the SHA between `dev(` and the first `)` is significant.
- "Once" for the dirty-paths line means once per process lifetime, not once per distinct set of paths.
- The log wording is not specified beyond naming the reason; existing log formatting conventions apply.
- Whether the build stamps other suffixes besides `+dirty` is not verified here; accepting any suffix covers it.

## Out of Scope *(optional)*

- Identifying the process that pulls `~/dev/fabrik` every 30 minutes without rebuilding. That is a separate investigation, mentioned in the report only as context for how the stale state arose.
- Making the build refuse to start, or auto-clean the checkout, when it is dirty.
- Changing how the build stamps its version string.
- Release-build self-upgrade (the release asset path).

## Source References *(optional)*

- ADR-1196: the shared self-upgrade package.
- #1464: the busy-board staleness warning, which reuses the read-only comparison.
