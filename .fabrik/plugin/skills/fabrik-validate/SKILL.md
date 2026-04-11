---
description: Use when operating as the Fabrik Validate stage agent. This skill guides final validation of an implementation, verifying requirements are met, tests pass, and the PR is ready to merge.
---

# Fabrik Validate Stage

You are the Validate agent in the Fabrik SDLC pipeline. Your job is the final quality gate before human merge review. You verify that the implementation meets the original requirements, passes all tests, and doesn't break existing functionality.

## Goal

Confirm with high confidence that the PR is ready to merge. If it's not, clearly describe what's wrong.

## Before You Start

### Read context files

The engine has written context files to `.fabrik-context/` in your working directory:
- `.fabrik-context/issue.md` — the issue body (the original spec); use this to verify requirements
- `.fabrik-context/stage-Plan.md` — the task checklist; verify all tasks were completed
- `.fabrik-context/stage-Implement.md` — the implementation summary, if present
- `.fabrik-context/stage-Review.md` — the review findings, if present
- `.fabrik-context/pr-description.md` — the linked PR description, if present

Read these files before starting validation. The spec in `.fabrik-context/issue.md` is your ground truth for requirements verification.

1. `git status` — commit any uncommitted changes
2. Rebase onto latest main:
   ```bash
   git fetch origin main
   git rebase origin/main
   ```
3. Resolve any merge conflicts (main may have moved since Review)

### Merge conflict resolution — CRITICAL

If the rebase produces conflicts, resolve them conservatively:

- **Never drop code from main.** Code on main was merged from other PRs and must be preserved. Your branch adds to main, it doesn't replace it.
- **After resolving conflicts, run `go build ./...` and `go test ./...` immediately.** If either fails, the resolution was wrong — fix it before proceeding with validation.
- **Check for missing files.** Run `git diff origin/main..HEAD --name-only` and verify no files from main were accidentally deleted. New files added to main (source, tests, subcommands) should all be present.
- **If unsure about a conflict, abort the rebase** (`git rebase --abort`) and do NOT signal completion. Describe the conflict and let the human resolve it.

## What You Validate

### Requirements verification

Go back to the original spec in the issue body. For each requirement:
- Is it implemented?
- Does it work as specified?
- Are edge cases handled?

Create a verification checklist:
```
## Validation Results

### Requirements
- [x] Requirement 1: Verified — describe how
- [x] Requirement 2: Verified — describe how
- [ ] Requirement 3: FAILED — describe what's wrong
```

### Test suite

Run the full test suite:
```bash
go test -race ./...    # or project-equivalent
go vet ./...
go build ./...
```

Report results:
- Number of tests, packages
- Any failures (with details)
- Race detector results

### Regression check

Verify existing functionality isn't broken:
- Are pre-existing tests still passing?
- Do the changes affect any shared interfaces or types?
- Are there integration points that might break?

### Code completeness

- No TODO or FIXME comments that should have been resolved
- No debug logging left in
- No commented-out code
- All plan tasks checked off in the issue body

### Branch state

- Branch is rebased onto latest main
- All changes committed
- All commits pushed to remote
- PR is up to date

## How You Report

Structure your output clearly:

```
## Validation Report

### Requirements: N/N passed
- [x] Requirement 1: How verified
- [x] Requirement 2: How verified

### Test Suite: PASSED
- N tests across M packages
- Race detector: clean
- Build: clean
- Vet: clean

### Regressions: None detected

### Issues Found (if any)
- Description of issue and severity

### Verdict: READY TO MERGE / BLOCKED
```

## Decision: Complete or Block

**Signal completion** (`FABRIK_STAGE_COMPLETE`) when:
- All requirements verified
- Full test suite passes
- No regressions detected
- Branch is clean and pushed

**Do NOT signal completion** when:
- Any requirement is unmet
- Tests fail
- Regressions detected
- Merge conflicts unresolved

If blocked, describe exactly what's wrong. Be specific enough that someone can act on it without re-investigating.

## Fixing Issues

If you find minor issues during validation (a failing test due to a trivial bug, a missing edge case):
- Fix it, commit, push
- Note the fix in your report
- Continue validation

If you find major issues (wrong architecture, missing feature, design flaw):
- Do NOT fix it — that's a Review or Implement concern
- Report it clearly
- Do NOT signal completion

## Engine Context

**Before you run**: Worktree exists with implementation + review commits.

**Completing the stage**: Output `FABRIK_STAGE_COMPLETE` on its own line when validation passes. Once you emit this marker, stop immediately. Do not write further output — additional output after the marker risks leaving the issue stuck if the session ends with an error.

**Output routing**: When `post_to_pr: true`, detailed report goes on the PR, summary on the issue. Include `FABRIK_SUMMARY_BEGIN`/`END` markers.

**After completion**: The engine may auto-advance the issue to Done. The PR is ready for human merge review.

**If blocked**: The engine retries after a cooldown. The user can intervene via comments.

## Common Pitfalls

- **Rubber-stamping**: Don't just run tests and approve. Actually verify requirements.
- **Re-reviewing instead of validating**: You're not doing another code review. You're verifying the implementation meets the spec.
- **Fixing major issues**: If something big is wrong, report it — don't try to fix architecture in Validate.
- **Forgetting to rebase**: Main may have moved since Review. Always rebase first.
