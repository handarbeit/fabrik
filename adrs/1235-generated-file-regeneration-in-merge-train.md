# ADR 1235: Regenerate Declared Generated Files Instead of Textually Merging Them

**Date**: 2026-07-28
**Status**: Accepted
**Issue**: #1235 — the merge train resolved conflicts in `docs/llms-full.txt` (a generated concatenation) by dispatching Claude to merge conflict hunks textually, producing a bundle that matches neither input and no longer corresponds to its declared sources.

## Context

`assembleTrialBranch` (`engine/merge_train.go`) merges each batch member's head SHA into a trial worktree in sequence. Before this issue, any merge conflict — regardless of which paths it touched — was handed to `resolveConflictWithClaude`, which instructs Claude to resolve every conflict marker and commit. This is correct for hand-authored source, but wrong for a generated artefact: `docs/llms-full.txt` is a fixed-order concatenation of four doc pages, produced by `scripts/generate-llms-full.sh`. When two members both regenerate it, the right resolution is to discard both conflicting versions and re-run the generator against the merged sources — not to merge the hunks. Textual resolution of a generated file is the wrong operation independent of how well an LLM performs it.

Nothing in `engine/*.go` had a concept of a "generated file" prior to this issue.

## Decision

Insert a classification step between "merge fails" and "dispatch Claude": `resolveTrainConflict` lists the conflicted paths (`unmergedPaths`, extracted from `resolveConflictWithClaude`'s pre-existing `git status --porcelain` parsing) and intersects them against a single declared mapping, `generatedFiles` in `engine/generated_files.go` (`generatedFileSpec{Path, Command}`; today's only entry: `docs/llms-full.txt` → `bash scripts/generate-llms-full.sh`). Three outcomes:

1. **No generated paths in the conflict** — unchanged: `resolveConflictWithClaude` dispatches Claude exactly as before this issue.
2. **Conflict confined entirely to declared generated path(s)** — Claude is never invoked. `regenerateAndCommit` runs each distinct regeneration command (deduplicated by command, so two paths sharing one command run it once), stages the regenerated output, verifies the tree is fully resolved, and commits.
3. **Mixed** (a declared generated path conflicts alongside a normal path in the same member) — Claude resolves the non-generated part; regeneration always runs *after*, never before.

**The mixed-case ordering is the load-bearing decision this ADR exists to record:** Claude must run first, regeneration must run last, and this order must never be reversed. A co-conflicted non-generated file can itself be one of the generator's own source inputs (e.g. `docs/state-machine.md`, one of `generate-llms-full.sh`'s four `ORDERED` pages). If regeneration ran before that file's conflict was resolved, the generator would read stale or conflict-marker-laden content and produce a bundle that is wrong in a new, less obvious way — silently reproducing this issue's exact bug while looking fixed. There is no cheap way to know in advance whether a given non-generated conflicted path feeds a particular generator without hardcoding that generator's own input list into Go (which FR-3 deliberately does not ask for), so "always regenerate last" is adopted as the one general-safe rule rather than something computed per generated-file spec.

To make "Claude resolves first, doesn't commit" workable, `buildTrainConflictComment` gains a `generatedPaths []string` parameter: when non-empty, the synthetic comment names the generated path(s) as explicitly out of scope, tells Claude to stage only the files it resolved (not `git add -A`), and tells it not to commit at all. `resolveConflictWithClaude` correspondingly filters its post-check (`unmergedPaths`) against `generatedPaths` — a generated path legitimately remains unmerged at that point — and defers the unscoped `git diff --check` and the commit to `regenerateAndCommit`, which owns finalizing a single commit across both parts once regeneration has staged its own output.

Regeneration failure (non-zero exit, inability to stage, or conflict markers still present afterward) ejects the member via the existing `ejectMember` path (FR-4) with a reason string identifying the failed step. It never falls back to Claude — a failed regeneration says nothing about whether textual resolution would succeed, and falling back would silently reintroduce the exact hazard this issue removes.

## Rationale

- **Correctness over cleverness.** A generated artefact's correct merge resolution is "regenerate," full stop — no amount of prompt engineering makes textual merging of a concatenated bundle correct, because the bundle's correctness is defined relative to its sources, not its own prior text.
- **Single declaration point (FR-3).** `generatedFiles` is the only place a future generated path is declared; `resolveTrainConflict`'s dispatch logic reads the mapping generically and needs no change to support a second entry.
- **Reuses existing ejection semantics (FR-4).** No new failure-reporting mechanism — `ejectMember`'s comment-then-pause-after-N-ejections pattern is unchanged; only the reason string is more specific.
- **Cost.** Each generated-only conflict previously burned a full Claude invocation to produce a worse result than a one-line shell command; this removes that invocation entirely for the confined case.
- **`docs-drift.yml` running unconditionally (ADR-1234) is what makes FR-4 ejection meaningful end-to-end** — it's the trial branch's own combined-Validate check that would have caught a bad regeneration before landing, closing the loop this issue's dependency resolved first.

## Alternatives Considered

### Regenerate before dispatching Claude in the mixed case

Rejected: if a co-conflicted non-generated path is one of the generator's own source inputs, regenerating before that file is resolved reads stale/conflicted content — silently wrong in a way that's hard to notice because the resulting bundle still merges cleanly, just from the wrong sources. Always-regenerate-last has no such failure mode and costs nothing extra in the common case, since git already leaves every non-conflicting file correctly auto-merged in the working tree regardless of ordering.

### Have Claude commit, then have the engine amend the commit with the regenerated content

Rejected: git refuses to commit while any path in the index remains unmerged, so Claude's own `git commit` would fail while the generated path sits unresolved — either the commit errors, or Claude is forced to touch the generated file itself to make the commit succeed, which is the exact outcome this issue exists to prevent. Instructing Claude not to commit at all, and letting the engine own the single final commit after regeneration, avoids this entirely.

### Glob/prefix matching for the generated-path mapping

Rejected for v1: FR-3's only required entry today is one literal path. An exact-match `Path string` keeps `generatedFileSpec` simple; glob support is unneeded speculation until a second entry actually demands it.

### Dedup regeneration commands by declared path instead of by command

Rejected: if two declared paths ever shared one regeneration command, running it once per path would be redundant work for no benefit — the command produces both paths' output in one invocation by construction. Deduping by the joined command avoids this.

## Consequences

**Positive:**
- A generated-only conflict is resolved deterministically and correctly by definition (it's literally the same command CI/`docs-drift.yml` would use to verify it), and costs one shell invocation instead of one Claude invocation.
- Extending generated-file coverage to a second artefact requires touching exactly one file (`engine/generated_files.go`), never the dispatch logic in `engine/merge_train.go`.
- The mixed-case ordering constraint is now written down somewhere a future contributor will find it before getting it backwards.

**Negative / Trade-offs:**
- `resolveConflictWithClaude`'s synthetic-comment prompt is new surface Claude must follow correctly (not touching/staging/committing the generated path) — a compliance failure here is a real edge case (see Risks in the Plan stage output). `regenerateAndCommit` detects a premature Claude commit structurally: it requires `MERGE_HEAD` to still be present at entry — before running any regeneration command or content comparison — and fails closed immediately if it's already gone, ejecting the member rather than silently reporting success. See `TestMergeTrainWorker_ClaudePrematureCommitInMixedModeEjectsMember`.
- The mixed case's "always regenerate last" rule is conservative — it costs nothing when the generator doesn't actually read the co-conflicted file, but there's no way to distinguish that case from the one that matters without hardcoding generator input lists, so it always pays the same (negligible) ordering cost.

**Follow-up fix (review):** `resolveConflictWithClaude`'s "no remaining conflict markers ⇒ resolved" check could not distinguish a genuine resolution from Claude running `git merge --abort` per `buildTrainConflictComment`'s own documented fallback — an abort clears every conflict marker (and `MERGE_HEAD`) exactly as a real resolution would, so the member would be reported as a survivor with its entire contribution silently discarded. `assembleTrialBranch` now captures the trial worktree's pre-merge HEAD SHA and threads it through; `resolveConflictWithClaude` treats a `MERGE_HEAD`-less worktree still sitting on that pre-merge SHA as unresolved rather than resolved. This applies to the plain (pre-#1235) conflict path too, not just the generated-file paths this issue introduces — see `TestMergeTrainWorker_ClaudeAbortMasqueradesAsResolved`.

**Follow-up fix (review, second pass):** the previous fix's own ejection cleanup had the same class of gap: `assembleTrialBranch`'s eject-on-unresolvable path called only `git merge --abort` before continuing the loop to the next member. That is a silent no-op once a commit already exists on `wtDir` — exactly the situation the premature-commit guard above detects and ejects for — so the ejected member's bad commit remained as `wtDir`'s HEAD, contaminating every later member's merge and the pushed trial branch (the same class of bug this issue exists to prevent, reintroduced via the eject path itself). The eject path now unconditionally follows the best-effort abort with `git reset --hard <preMergeHEAD>` (reusing the same SHA captured for the abort-detection fix above), guaranteeing a clean worktree regardless of how far resolution got before failing. See `TestMergeTrainWorker_EjectionAfterPrematureCommitDoesNotContaminateLaterMembers`, a 3-member batch with the poisoned member in the middle so a subsequent merge is actually attempted against the (correctly cleaned) worktree.

**Follow-up fix (review, third pass):** two related gaps in `regenerateAndCommit`, both from relying on post-hoc content comparison instead of git's own structural state:
- The `git diff --check` run after staging the regenerated file compared working tree to index — always identical at that point, since the loop immediately above had just `git add`ed every relevant path. It was a structural no-op that could never see the content being committed. Changed to `git diff --cached --check`, which scans the actual staged diff.
- The premature-commit guard originally ran *after* regeneration, comparing `git diff --cached` to detect leftover staged-but-uncommitted content. That has a blind spot: if Claude's non-compliant commit happens to write byte-identical content to what regeneration would produce, the diff is empty and the guard doesn't trip — the member lands with Claude's premature commit, not the declared regeneration, and nothing catches it. Moved the check to the top of `regenerateAndCommit`, before any regeneration command runs: `MERGE_HEAD` must be present at entry (the function is only ever called with the merge still in conflict or with Claude having resolved-but-not-committed), so its absence is detected structurally and immediately, independent of content. See `TestMergeTrainWorker_ByteIdenticalPrematureCommitStillEjectsMember`, which constructs exactly that worst case and confirms it was previously missed (verified against the prior diff-based implementation) and is now caught.

## Predecessor Context

- **ADR-059** (`059-internal-merge-train.md`): the founding design for the merge train, including D3 ("merge conflict → dispatch Claude to resolve"). This issue narrows D3's scope for a declared generated-file set without contradicting it — Claude resolution remains the default for everything not declared generated.
- **ADR-067** (`067-merge-train-centralized-inflight-cleanup.md`): establishes `finishTrain` as the sole point clearing `mergeTrainInFlight`. The new regeneration-failure path calls `ejectMember` and continues the existing per-member loop rather than returning early, so it relies on the same deferred cleanups as every other eject site and needed no new wiring.
- **ADR-1120** (`1120-claude-usage-limit-backoff-and-suspension.md`): documents `resolveConflictWithClaude`'s `(bool, error)` contract, where a non-nil error is exclusively the ADR-1120 usage-limit sentinel and must never be conflated with "conflict is unresolvable." This issue's `resolveTrainConflict` preserves that contract unchanged — the all-generated case never calls `resolveConflictWithClaude` at all (no Claude dispatch, no suspension interaction), and the mixed case's Claude dispatch still goes through the same gate.
- **ADR-1234** (`1234-required-checks-run-unconditionally.md`): the resolved dependency that makes this issue's FR-4 ejection-on-failure meaningful end-to-end — `docs-drift.yml` now runs unconditionally on every PR, so a bad regeneration in the trial branch is caught by the trial's own combined Validate before it could land.
