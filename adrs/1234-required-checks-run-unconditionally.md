# ADR 1234: Required-Check Workflows Must Trigger Unconditionally (No Path Filters)

**Date**: 2026-07-28
**Status**: Accepted
**Issue**: #1234 — `docs-drift`'s "Verify llms-full.txt is up to date" check was not a required status check on `main`, and its workflow was path-filtered

## Context

`.github/workflows/docs-drift.yml` regenerates `docs/llms-full.txt` via `scripts/generate-llms-full.sh` and fails the check run `Verify llms-full.txt is up to date` on a mismatch. It existed and worked correctly, but two gaps meant a PR carrying a stale or hand-merged bundle could still land on `main`:

1. The check was not in `required_status_checks.contexts` in `main`'s branch protection (`{"contexts": ["Analyze (go)"], "strict": false}` — verified live, 2026-07-28), so GitHub's merge button never blocked on it.
2. The workflow was scoped with `paths: ["docs/**", "scripts/generate-llms-full.sh"]`, so it never ran at all on a PR that didn't touch those paths.

These two gaps interact in a way that isn't obvious until you try to fix only the first one: naively adding the check to `required_status_checks.contexts` while leaving the `paths:` filter in place would make every PR that doesn't touch `docs/**` sit forever on "Expected — waiting for status" for a check that GitHub has already decided is required but that the workflow's own trigger config guarantees will never run. GitHub has no concept of "required, but only when applicable" — a required context with no matching workflow run just never resolves.

This is a general trap, not specific to `docs-drift`: any workflow using `on.pull_request.paths` becomes unenrollable as a required check without also touching its trigger config. The two other PR-facing workflows in this repo, `ci.yml`'s `Test and vet` and `claude-review.yml`'s `Claude PR Review`, already trigger unconditionally on every `pull_request` with no path filter — this ADR brings `docs-drift.yml` in line with that existing precedent rather than introducing a new pattern.

## Decision

Drop the `paths:` filter from `docs-drift.yml`'s `on.pull_request` block entirely. The `check-llms-full` job now runs on every pull request, regardless of what it touches, and its check run `Verify llms-full.txt is up to date` is added to `main`'s `required_status_checks.contexts` alongside the existing `Analyze (go)` entry.

Enrollment itself is a one-time infrastructure mutation against live branch protection (`PATCH /repos/handarbeit/fabrik/branches/main/protection/required_status_checks`), not a file this repo tracks — it is not part of this ADR's code change, only its consequence.

**Rule for future required checks in this repo**: a workflow whose check run is (or will become) a required status check on `main` must not use `on.pull_request.paths` (or any other conditional trigger that can cause it to be skipped on a valid PR). If the underlying work is expensive enough that running it unconditionally is undesirable, add a cheap skip-job that always reports success when the filter condition doesn't match, rather than omitting the run entirely. `generate-llms-full.sh` doesn't need this — it's a plain `awk`-based concatenation script with no `go build` step and no network calls, cheap enough to run on every PR.

## Rationale

- **Consistency over cleverness.** A path filter is the obvious first instinct for a docs-only check — less noise, less CI time. But the moment such a check is meant to gate merges, "sometimes doesn't run" and "required" are mutually exclusive from GitHub's perspective. The existing filterless precedent (`ci.yml`, `claude-review.yml`) already settled this trade-off the same way for this repo.
- **Cheap to run unconditionally.** No build step, no external calls — the cost of dropping the filter is negligible, so there's no real efficiency argument for keeping it.
- **No new mechanism needed.** This is a one-line trigger-config change plus a branch-protection enrollment, not new engine or workflow logic.

## Alternatives Considered

### Add a skip-job that reports success when the path filter doesn't match

Rejected for this specific check: it would work, but it's strictly more complex than dropping the filter, and no other workflow in this repo uses that pattern. Left as the documented fallback (see Decision) for a future required check whose underlying job actually is too expensive to run unconditionally.

### Keep the path filter, don't make the check required

This is the status quo this issue is fixing — rejected because it leaves stale-bundle PRs (including hand-resolved merge-train conflicts on `docs/llms-full.txt`, which has no generated-file awareness in `engine/merge_train.go`'s conflict resolution) able to merge silently.

## Consequences

**Positive:**
- `Verify llms-full.txt is up to date` now runs on, and can block, every PR — including ones that don't touch docs, closing the vacuous-pass gap.
- Establishes a documented, discoverable rule so a future contributor adding a new required check doesn't reintroduce this exact trap by reflexively scoping it with a `paths:` filter.

**Negative / Trade-offs:**
- `docs-drift` now runs (and consumes a small amount of CI time) on every PR instead of only docs-touching ones. Judged negligible given the script's cost profile.
- `enforce_admins: false` remains on `main` (deliberately untouched — separate, more consequential change, not requested by this issue): an admin-scoped merge can still bypass this required check. Documented as a known, accepted residual gap rather than silently left unaddressed.

## Predecessor Context

- **ADR-933** (`933-required-status-context-config.md`): established why Fabrik's own engine doesn't read branch protection as a load-bearing runtime dependency, and scoped `required_status_contexts` config to classic commit-status visibility gaps. Not applicable here — this check is an ordinary GitHub Actions check run, and the branch-protection change this ADR describes is a one-time human/operator-driven infrastructure mutation, not a new engine dependency.
- **ADR-1153** (`1153-train-ci-completeness-over-mergeable-state.md`): already makes the merge train (`pollTrainCI`) block on any confirmed check-run failure on the trial branch, required or not. Once `docs-drift` runs unconditionally (this ADR), the merge-train side of this issue's motivating hazard was already closed by that existing behavior — no `pollTrainCI` change was needed here.
