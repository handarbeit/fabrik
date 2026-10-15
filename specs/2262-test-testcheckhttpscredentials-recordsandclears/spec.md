# Feature Specification: Make TestCheckHTTPSCredentials_RecordsAndClears hermetic against repository-level git config

**Feature Branch**: `fabrik/issue-2262`
**Created**: 2026-10-14
**Status**: Specified
**Input**: User description: "## Problem

`TestCheckHTTPSCredentials_RecordsAndClears` (`engine/warnings_conditions_test.go`) fails when run from a git worktree of `~/dev/fabrik`. That repository has its own local `credential.helper`, and a worktree reads the main repository's config. The test isolates only the global git config, so the repository-level helper leaks in and changes the result. It passes in CI and in a plain clone without that helper.

## Requirements

- Make the test hermetic against repository-level and worktree-level git config, not only global config. Run it in a temp repository it creates, or point the check at an isolated config with `GIT_CONFIG_NOSYSTEM` and an explicit repository, using `internal/testenv.IsolateGit` if that fits.
- Check the other `engine` tests that read `credential.helper` or other git config for the same gap.

## Acceptance

- The test passes when run from a worktree of a repository whose local config sets `credential.helper`. Add a fixture that sets one to prove it."

## Background

The test's job is to prove that the HTTPS-credentials startup check records a warning when no credential helper is configured and clears it when one is. It already isolates the global git config through the shared test-isolation helper. That is not enough. The check asks git for `credential.helper` from the process's working directory, so git also applies the config of whichever repository the test binary runs in. Git does not resolve the repository config from the test's temporary home, so a worktree of the Fabrik repository, which sets its own local helper, contributes a helper the test never configured. The "no helper configured" half of the test then sees one and fails.

This only shows up for developers (and Fabrik's own workers) running the suite from a worktree or checkout whose local config sets a credential helper. CI and clean clones pass, so the defect is invisible to the normal gate and surfaces as a confusing local-only failure.

The same gap may exist in other engine tests that read `credential.helper` or other git config and isolate only the global layer. Those need to be found and closed in the same change so one fix is not followed by a sibling failure.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Test passes from any working directory (Priority: P1)

A developer or Fabrik worker runs the engine test suite from a git worktree of a repository whose local config sets `credential.helper`. `TestCheckHTTPSCredentials_RecordsAndClears` passes, exactly as it does in CI.

**Why this priority**: This is the reported failure. Without it the suite is red on the project's own worktrees.

**Independent Test**: In a worktree of a repository whose local config sets a credential helper, run only this test. It passes.

**Acceptance Scenarios**:

1. **Given** the working directory belongs to a repository whose local config sets `credential.helper`, **When** the test runs, **Then** the "no helper configured" case still observes no helper and records the warning.
2. **Given** the same working directory, **When** the test's "helper configured" case runs, **Then** it observes only the helper the test itself configured and clears the warning.
3. **Given** a plain clone with no local helper, **When** the test runs, **Then** it still passes (no regression).

---

### User Story 2 - A fixture proves the isolation holds (Priority: P1)

A reviewer can see, from a test that sets a repository-level credential helper on purpose, that the isolation actually works and is not passing by accident of the environment it ran in.

**Why this priority**: The acceptance criterion requires proof. Without a fixture the fix only passes where the bug could not occur, so it would verify nothing.

**Independent Test**: Run the fixture-backed test from any directory. It fails if the isolation is removed and passes with it in place.

**Acceptance Scenarios**:

1. **Given** a fixture repository whose local config sets `credential.helper`, **When** the check runs with the test's isolation in force from inside it, **Then** the helper from that repository is not observed.
2. **Given** the isolation is deliberately neutralised, **When** the fixture test runs, **Then** it fails, showing it detects the leak.

---

### User Story 3 - Sibling tests are free of the same gap (Priority: P2)

Other engine tests that read `credential.helper` or other git config are audited. Any that isolate only global config are fixed to the same standard, or documented as not affected.

**Why this priority**: Prevents the same local-only failure recurring in a neighbouring test, but it is not the reported symptom.

**Independent Test**: Run the affected engine tests from the fixture-style worktree. All pass.

**Acceptance Scenarios**:

1. **Given** the audited tests, **When** run from a worktree whose local config sets `credential.helper`, **Then** none change result because of that helper.

---

### Edge Cases

- A worktree reads the main repository's config, so a helper set on the main repository must be covered as well as one set in the worktree itself.
- System-level git config must not influence the result either.
- Inherited `GIT_CONFIG_*` environment entries must not influence the result.
- The fix must not depend on the test's temporary directory being outside any repository.
- The check's production behaviour is unchanged. Only the test's isolation changes.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: `TestCheckHTTPSCredentials_RecordsAndClears` produces the same result regardless of any `credential.helper` set in the local, worktree-shared, global or system git config of the directory it is run from.
- **FR-002**: The test's "no helper" case observes no helper and the "helper configured" case observes exactly the helper the test configures.
- **FR-003**: A fixture sets a repository-level `credential.helper` and demonstrates that the isolation prevents that helper from being observed by the check.
- **FR-004**: The fixture fails when the isolation is removed, so it cannot pass vacuously.
- **FR-005**: Every other `engine` test that reads `credential.helper` or other git config is reviewed for the same gap. Each one that is exposed is made hermetic to the same standard, and each one found unaffected is recorded as such with its reason (in the change description).
- **FR-006**: No production behaviour of the HTTPS-credentials check changes.
- **FR-007**: The test and fixture pass in CI and in a plain clone with no local helper.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: `TestCheckHTTPSCredentials_RecordsAndClears` passes when run from a worktree of a repository that sets a local `credential.helper` (the failing case today).
- **SC-002**: The same test still passes in CI and in a clean clone.
- **SC-003**: The fixture test fails when the isolation is neutralised and passes when it is in place.
- **SC-004**: Every engine test reading git config is listed as fixed or confirmed unaffected, and none of them changes outcome when run from the fixture-style repository.

## Assumptions

- The cause is the repository-level and worktree-shared config layer leaking in, as described in the issue; the existing global isolation is otherwise correct.
- The shared test-isolation helper may be extended or complemented to cover the repository layer, since the issue allows using it "if that fits"; the choice of mechanism is left to Plan.
- "Other git config" in the audit means config the engine tests read through git (credential helpers, URL rewrites and similar), not unrelated git behaviour.
- A temporary fixture repository created by the test itself is acceptable; no checked-in repository is required.

## Out of Scope *(optional)*

- Changing how the production HTTPS-credentials check resolves git config.
- Reworking git test isolation in packages other than `engine`, unless the shared helper has to change to support this fix.
- Adding CI jobs that run the suite from a worktree.
- Unrelated flaky or environment-dependent engine tests.

## Source References *(optional)*

- Issue #2262
- `engine/warnings_conditions_test.go` (the failing test)
- `internal/testenv` (shared git isolation helper)
