# Feature Specification: Pruefer skips re-reviewing a head whose change it has already reviewed

**Feature Branch**: `fabrik/issue-2230`
**Created**: 2026-10-14
**Status**: Specified
**Input**: User description: "## Problem

Pruefer spends most of its reviews repeating work. Since 2026-10-10 the fabrik installation has run 331 reviews. Many PRs were reviewed again and again: #2209 13 times, #2177 12, #2183 11, #2201 9. A large share of those heads carry no new change:

- **Rebases onto main.** The Validate skill rebases when the branch is behind, and so does an engine comment pass. Several of #2209's reviewed heads are the same commit replayed: `ef917b4c` and `0e197fba` share a message, and so do `c059923e` and `28bb1176`. Each got a full review.
- **Merge-train work while the item is Queued.** The singleton catch-up merges the base into the member branch, and trial or integration PRs assemble already-reviewed members. The existing #2066 skip covers only the engine's *pure catch-up merge commit* with a verified marker, and it has never fired in the dev Pruefer's log (`pure merge-train catch-up` appears 0 times). Trial and integration PRs are reviewed in full.

Each repeat costs a Claude review of the whole diff and quota, and it posts a review that can perturb the item. A `COMMENTED` body can trigger a review reinvoke, and a finding on a Queued member ejects it (ADR-1208).

## Requirements

- **R1: Skip a patch-identical head.** Before reviewing a new head, compare the PR's change at that head with the change at the last head Pruefer reviewed on that PR. The change is the diff against the merge base with the PR's base branch. Compare by patch identity, e.g. `git patch-id --stable` over the whole diff, or an empty `git range-diff`. If they're identical, skip with a new reason (e.g. `SkipPatchUnchanged`), logging both heads. This needs no marker and trusts nothing the PR author controls, because it's computed from git objects.
  - A conflict-resolution rebase or merge that changes the patch is not identical, so it is reviewed. That is correct.
  - Bound the chain the way #2066's `maxCatchUpChain` does, so a periodic real review still happens.
- **R2: Skip merge-train trial and integration PRs that add nothing beyond their members.** Recognise them positively:
  - the PR is authored by an operator-configured engine login (as #2066's trust model does);
  - its head branch has the engine's trial prefix (`merge-train-<base>-<ts>[-t<n>]`).

  Skip when the trial's combined patch equals the merge of its members' already-reviewed patches. Otherwise review only the difference: the conflict-resolution edits, which no one has reviewed. If that can't be established, review as today (fail safe).
- **R3: Keep the #2066 catch-up skip.** R1 should subsume it; record whether it can be retired.
- **R4: Visibility.** Count each skip reason in the log and the TUI, so the saving can be seen.

## Acceptance

- Unit tests for patch identity: a rebase with no change is skipped; a rebase that resolves a conflict differently is reviewed; a new commit is reviewed. A trial-PR test covers both the clean case and the conflict-edit case. Each skip is shown to fail with the skip removed.
- On the dev installation, quote the number of reviews per merged PR before and after, over a comparable window.

## Notes

- Pruefer is production-critical. Ship behind config, default on, with a kill switch, and roll out in a quiet window.
- Related: #2066 (the pure catch-up skip), #2190 (coverage reporting), ADR-1208, ADR-2044.

**Added in discussion (@arbeithand, 2026-10-13):** *R0: make the #2066 skip effective by default.* `catch_up_marker_authors` is empty by default, which disables the skip. It was unset in all four installations, so the skip never fired, even though a pure catch-up marker was posted for PR #2229 head `76ea1a27` and Pruefer ran a full review anyway. The operator shouldn't have to discover a trust-anchor setting before the skip does anything. Options: derive a default from a known engine identity; refuse to start quietly-disabled, with one startup notice naming the setting; or a warnings entry when a merge-train catch-up marker is seen from an untrusted login. **Status (2026-10-15):** all four installations now set `catch_up_marker_authors`, so the configuration side is done. The default-on ask is still open, and so is R1, the patch-identical skip, which is the main saving."

## Background

Pruefer reviews on every `synchronize` event. A new head SHA therefore triggers a full Claude review of the whole diff, even when the head is the same change replayed. Common causes are a rebase onto an updated base, a merge-train singleton catch-up merge, or a disposable merge-train trial or integration PR that only recombines PRs Pruefer has already reviewed.

Each repeat has three costs:
- model quota and wall-clock time;
- a review body a human must read;
- a posted review that can perturb the item. A `COMMENTED` review body can trigger a review reinvoke, and a finding on a Queued merge-train member ejects it from the queue (ADR-1208).

The measured scale: 331 reviews on the fabrik installation since 2026-10-10, with individual PRs reviewed 9 to 13 times. Several of the reviewed heads of #2209 were the same commit replayed (matching messages, different SHAs).

The existing safeguard, ADR 2066, skips only an engine-authored pure catch-up merge commit. Four conditions must hold for it to fire:
- a marker comment from a login on the operator-only `catch_up_marker_authors` list;
- a head with exactly two parents;
- a second parent on the base branch;
- a first parent that was already reviewed.

It is off by default because that list is empty. Every installation left it unset, so it has never fired. The operator has since set it on all four installations, but a default that only works after an operator discovers a trust-anchor setting is still a gap. The 2066 skip also does not cover rebases or trial and integration PRs at all.

This spec generalises the skip. A head is skipped when the PR's change at that head is provably the same as at a head Pruefer already reviewed. The proof is computed from git objects and does not depend on any marker or on anything the PR author controls.

Merge-train trial and integration PRs are opened by the engine from a disposable branch named `fabrik/merge-train/merge-train-<base>-<unix-ts>[-t<n>]`. Their body lists the member PRs they combine. Related: ADR 2066 (pure catch-up skip), ADR 2044 (singleton catch-up), ADR 1208 (Queued review-finding ejection), #2190 (coverage reporting).

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A no-change rebase is not reviewed again (Priority: P1)

An author, the Validate stage or an engine comment pass rebases a reviewed PR onto a newer base. The commit SHAs change, but the PR's own change relative to its merge base is the same patch.

**Why this priority**: This is the largest source of repeat reviews and the main saving. It also removes the risk of a repeated review perturbing the item.

**Independent Test**: Review a PR at head A. Rebase it so the head becomes B with an identical diff against the merge base. Confirm that Pruefer skips B, logs both heads and the skip reason, and posts no review.

**Acceptance Scenarios**:

1. **Given** a PR reviewed at head A, **When** the branch is rebased onto a newer base and the head becomes B with a patch-identical change, **Then** Pruefer skips the review of B with a distinct skip reason, logs A and B, and posts nothing.
2. **Given** a PR reviewed at head A, **When** a rebase resolves a conflict differently and the patch at B differs from A, **Then** Pruefer reviews B.
3. **Given** a PR reviewed at head A, **When** a new commit adds a change and the patch at B differs, **Then** Pruefer reviews B.
4. **Given** a chain of consecutive patch-identical heads that reaches the chain bound, **When** the next patch-identical head arrives, **Then** Pruefer performs a real review.

---

### User Story 2 - Merge-train trial and integration PRs that add nothing are not reviewed (Priority: P2)

The merge train assembles Queued members into a trial or integration PR. When that PR's combined change equals the combination of its members' already-reviewed changes, there is nothing new to review. When conflict resolution added edits, those edits are the only part no one has seen.

**Why this priority**: These PRs are reviewed in full today. Findings on a Queued member eject it (ADR-1208), so a redundant review can disrupt the landing path. It is lower priority than User Story 1 because it needs positive identification of engine-owned PRs and per-member lookups.

**Independent Test**: Build a trial PR from two reviewed members with no conflicts. Confirm that it is skipped. Build another that includes a conflict-resolution edit. Confirm that only that difference is reviewed.

**Acceptance Scenarios**:

1. **Given** a PR authored by a configured engine login, whose head branch has the engine's trial naming, and whose combined patch equals the merge of its members' reviewed patches, **When** Pruefer evaluates it, **Then** it skips with a distinct skip reason and logs the members.
2. **Given** the same PR but with conflict-resolution edits beyond the members' patches, **When** Pruefer evaluates it, **Then** it reviews only the difference, meaning the unreviewed edits.
3. **Given** a trial PR where equivalence cannot be established (a member was never reviewed, a member cannot be resolved, or a git or API read fails), **When** Pruefer evaluates it, **Then** it reviews as it does today.
4. **Given** a PR with the trial branch naming but authored by a login that is not a configured engine login, **When** Pruefer evaluates it, **Then** it is treated as an ordinary PR.

---

### User Story 3 - The operator can see the saving and the skip stays safe to run (Priority: P2)

The operator rolls out a change to a production-critical reviewer and needs to see what it saves, switch it off quickly if it misbehaves, and not depend on discovering a hidden trust-anchor setting.

**Why this priority**: Pruefer serves in-flight projects. Without visibility and a kill switch the change cannot be accepted. Without a safe default, the existing #2066 skip stays dormant.

**Independent Test**: Run a mix of skipped and reviewed heads. Check that the log and the TUI show a count per skip reason. Disable the feature through configuration and confirm that every head is reviewed as before.

**Acceptance Scenarios**:

1. **Given** Pruefer has skipped heads for several reasons, **When** the operator looks at the log and the TUI, **Then** each skip reason has its own count.
2. **Given** the feature is disabled by its kill switch, **When** the same events arrive, **Then** Pruefer behaves exactly as it did before the feature existed.
3. **Given** the #2066 catch-up skip has no trusted marker author configured, **When** Pruefer starts or sees an engine catch-up marker from an unrecognised login, **Then** the operator is told once, naming the setting, instead of the skip being silently inert. Alternatively the skip works without the setting.

---

### Edge Cases

- A rebase that is a no-op for the patch but changes the merge base: the diff against the merge base is compared, so an unchanged patch is still identical.
- A base update that touches the same lines the PR touches: the patch changes, so the head is reviewed. This is intended.
- A force-push that restores an earlier reviewed head, or a revert and re-apply that recreates a reviewed patch: the patch matches a reviewed head, so the head is skipped.
- Whitespace-only or context-only differences in the patch: the identity comparison follows one stable definition, and any difference it reports means a review.
- A PR Pruefer has never reviewed: there is no previous reviewed head, so the first review always happens.
- A reviewed head that has since been garbage-collected or is unreachable: the comparison cannot run, so the head is reviewed.
- A very large or partially reviewed diff (#2190): the skip must not treat a partial review as a full review of the patch. Whether the last reviewed head counts as reviewed follows the coverage contract.
- `/pruefer review` (an explicit user request) bypasses the skip, as it does for #2066.
- A trial PR rebuilt after bisection (`-t<n>` suffix): each is evaluated on its own contents.
- Two PRs with the same patch: a head is compared only with heads reviewed on the same PR.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: Before reviewing a new head of a PR, Pruefer MUST compare the PR's change at that head with the change at the last head it reviewed on that same PR. The change is the diff against the merge base with the PR's base branch.
- **FR-002**: The comparison MUST be by patch identity, computed from git objects only (for example a stable patch-id over the whole diff, or an empty range-diff). It MUST NOT depend on a marker, a commit message, a trailer or any other value the PR author controls.
- **FR-003**: When the two changes are identical, Pruefer MUST skip the review. It MUST report a new skip reason, distinct from every existing reason, and log both the new and the previously reviewed head SHAs. It MUST NOT post a PR comment for the skip.
- **FR-004**: When the two changes differ, including a rebase or merge that resolved a conflict differently, Pruefer MUST review the head as it does today.
- **FR-005**: Consecutive patch-identical skips MUST be bounded by a chain limit, in the manner of ADR 2066's `maxCatchUpChain`, so that a real review happens periodically.
- **FR-006**: Any missing, ambiguous or failed input to the comparison MUST fall back to reviewing. A tolerated read failure MUST NOT cause a review result to be memoised as conclusive, as in ADR 2066's degraded handling.
- **FR-007**: The skip MUST apply on every path that can start a review (event dispatch, poll and reconciliation sweep), not only on the event path. An explicit `/pruefer review` request MUST bypass it.
- **FR-008**: A last-reviewed head counts as reviewed for FR-001 only if Pruefer's review of it was a completed review in the sense of the diff coverage contract (#2190, ADR 2190). A head whose review was partial for the compared change MUST NOT suppress a review of an identical patch.
- **FR-009**: Pruefer MUST recognise merge-train trial and integration PRs positively, and only when both hold: the PR is authored by a login on an operator-configured engine-login list, and its head branch has the engine's trial naming (`fabrik/merge-train/merge-train-<base>-<unix-ts>[-t<n>]`). A PR meeting only one condition MUST be treated as an ordinary PR.
- **FR-010**: For a recognised trial or integration PR, when its combined patch equals the merge of its members' already-reviewed patches, Pruefer MUST skip it with a distinct skip reason and log the members considered.
- **FR-011**: For a recognised trial or integration PR whose combined patch includes edits beyond its members' reviewed patches (conflict resolution), Pruefer MUST review only that difference. If equivalence or the difference cannot be established, it MUST review the PR as it does today.
- **FR-012**: The members of a trial or integration PR MUST be identified from information the engine authored on that PR. A member MUST count as reviewed only if Pruefer has a completed review of that member's PR at its current head patch.
- **FR-013**: Pruefer MUST count each skip reason separately and expose the counts in the log and in the TUI, so that the saving is visible. This covers the new patch-unchanged and trial-equivalent reasons and the existing catch-up reason.
- **FR-014**: The feature MUST be controlled by configuration, enabled by default, with a kill switch that restores previous behaviour exactly. The setting MUST reload live where Pruefer's other settings do.
- **FR-015**: The ADR 2066 catch-up skip MUST keep working. The change MUST record whether the patch-identical skip makes it redundant and whether it can be retired. The pure-catch-up path retires only if every case it covers is covered by FR-001 to FR-004, which the record must show.
- **FR-016**: The ADR 2066 catch-up skip MUST NOT stay silently inert because a trust-anchor setting is unset. Either it works by default, or Pruefer tells the operator once at startup, naming the setting, or when it sees an engine catch-up marker from a login it does not trust. Whichever is chosen MUST NOT widen trust to a login the operator did not nominate.
- **FR-017**: Unit tests MUST cover: a no-change rebase is skipped; a rebase that resolves a conflict differently is reviewed; a new commit is reviewed; a trial PR is skipped in the clean case; a trial PR with a conflict-resolution edit is reviewed on the difference only. Each skip test MUST be shown to fail when the skip is removed.
- **FR-018**: The user guide, the Pruefer README and the relevant ADR record MUST describe the new skip reasons, the setting, the kill switch and the counters.

### Key Entities

- **Patch identity**: A stable fingerprint of a PR's whole change against its merge base with the base branch. Two heads with the same fingerprint carry the same change, whatever their commit SHAs.
- **Last reviewed head**: The most recent head on a PR for which Pruefer posted a completed review.
- **Skip reason**: A named category for a head not reviewed (draft, excluded path, already reviewed, catch-up, and the new patch-unchanged and trial-equivalent reasons). Each is counted separately.
- **Trial or integration PR**: A disposable PR authored by the merge-train engine that combines Queued member PRs for CI. It is identified by the engine's author login together with its branch naming.
- **Engine login list**: The operator-configured set of logins trusted to author engine-owned PRs and markers.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: On the dev installation, over a comparable window before and after rollout, the number of Pruefer reviews per merged PR falls measurably. The before and after figures are quoted in the PR or the release notes.
- **SC-002**: No head whose patch differs from the last reviewed head's patch is ever skipped by this feature. Tests cover rebases, conflict-resolved rebases and new commits.
- **SC-003**: For every merge-train trial or integration PR that is skipped, the combined patch provably equals its members' reviewed patches. For every one that is not, the edits beyond the members' patches are reviewed.
- **SC-004**: An operator can see, from the log and the TUI alone, how many heads were skipped under each reason.
- **SC-005**: With the kill switch off, behaviour matches the previous release in every tested scenario.
- **SC-006**: The #2066 catch-up skip is either effective by default or tells the operator, within one startup or one marker sighting, which setting needs to be supplied.

## Assumptions

- "The last head Pruefer reviewed" is determined from Pruefer's own posted reviews on the PR, as ADR 2066 already does for its reviewed-set check.
- A single stable definition of patch identity is used for all comparisons. Any difference it reports counts as changed and leads to a review, which errs toward reviewing.
- The trial branch pattern is taken from the engine's real naming (`fabrik/merge-train/merge-train-<base>-<unix-ts>[-t<n>]`). The issue text quotes only the trailing `merge-train-...` segment.
- The members of a trial or integration PR are read from the list in the engine-authored PR body. The author check in FR-009 is what makes that body trustworthy.
- Under personal-access-token auth the "engine login" is a human account, so the author check is only as strong as that account's access. This is the same trust model as ADR 2066.
- For R0, the requirement is that the skip not be silently dormant. The choice between a safe default and a visible notice is a design decision left for planning, as long as it widens no trust.
- The configuration side of R0 (setting `catch_up_marker_authors` on the four installations) is already done by the operator and is not part of this work.
- Pruefer is production-critical. The change ships enabled by default behind a kill switch and is rolled out in a quiet window, as ADR 2066 did.
- Before-and-after review counts in SC-001 are gathered by the implementer from the dev Pruefer log or GitHub review history.

## Out of Scope *(optional)*

- Changing how the engine builds, marks or names merge-train catch-up commits and trial PRs.
- Reviewing the actual content of a partially reviewed large diff differently (#2190 covers coverage reporting).
- Skipping reviews based on semantic or whitespace-insensitive equivalence of different patches.
- Changing the merge-train's reaction to review findings (ADR-1208).
- Adding a per-PR opt-out label beyond the existing `/pruefer review` bypass and the global kill switch.
- Setting `catch_up_marker_authors` on installations. The operator has done this.

## Source References *(optional)*

- ADR 2066 (`adrs/2066-pruefer-skip-pure-catch-up.md`): the existing catch-up skip, its trust model and chain bound
- ADR 2044 (`adrs/2044-singleton-catch-up.md`): the engine's singleton catch-up and its marker
- ADR 1208 (`adrs/1208-queued-review-finding-ejection.md`): findings on a Queued member eject it
- ADR 2190 (`adrs/2190-pruefer-diff-coverage-contract.md`): the diff coverage contract
- `pruefer/catchup.go`, `pruefer/select.go`, `pruefer/review.go`: current skip reasons and the decision point
- `engine/merge_train.go`: trial naming and integration PR body
