# Feature Specification: Carry non-template issue-body sections forward verbatim in Specify skills

**Feature Branch**: `fabrik/issue-2088`
**Created**: 2026-10-10
**Status**: Specified
**Input**: User description: "skills: carry non-template issue-body sections forward verbatim, so a human-added section survives a Specify round. The Specify skills tell the model to keep the body to the Spec Kit template. `fabrik-specify/SKILL.md` says \"**Every other heading stays exactly as in the template**\". `fabrik-specify-comment/SKILL.md` says \"keep every heading, header field and the order exactly as they are\". Report #2056 shows the effect: a human adds a section to the issue body between runs (e.g. `## Human Decisions`). The next `FABRIK_ISSUE_UPDATE` drops that section, because the model normalises the body back to the template. The spec persisted by #2034 (`specs/<N>-*/spec.md`) then loses it too."

## Background

The Specify skills instruct the model to keep the issue body strictly in the Spec Kit template shape. That instruction was written to stop the model inventing or reordering the template's own sections. It is currently worded so broadly that it also covers content the template never defined.

Report #2056 shows the effect. A human adds a section to the issue body between runs, for example `## Human Decisions`. The next `FABRIK_ISSUE_UPDATE` drops it, because the model normalises the body back to the template. The spec persisted under `specs/<N>-*/spec.md` (#2034) is a one-way projection of the issue body, so it loses the section too.

On v0.0.83 the model sometimes carried such a section forward. The template rule added since then (02dca7a7, 3fb02273, not yet released) makes dropping more likely, so the next release would regress this unless the skill text is fixed.

This issue covers the **skill-text half only**. The engine half, where an edit made *during* a run is overwritten by a blind PATCH, is a separate and larger design under discussion on #2056. It is out of scope here.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A human-added section survives a Specify comment round (Priority: P1)

A human adds a section that is not part of the template, such as `## Human Decisions`, to the issue body and then comments on the issue. The Specify comment round runs and emits an updated body. The extra section is still there, with the same heading, the same content and the same position relative to the template sections.

**Why this priority**: This is the reported regression (#2056). Silent loss of human-authored content from the spec is the worst outcome, and it would ship with the next release.

**Independent Test**: Take a body containing the template sections plus an extra `## Human Decisions` section. Run the Specify comment round, or assert on the skill text if the round cannot be exercised deterministically. Confirm the section survives verbatim in the emitted body.

**Acceptance Scenarios**:

1. **Given** an issue body with all template sections plus a `## Human Decisions` section placed after the template sections, **When** a Specify comment round updates the body, **Then** the emitted body still contains `## Human Decisions` with identical heading and content, in the same relative position.
2. **Given** the same body also has a `## Source References` section, **When** the body is updated, **Then** the extra section is still placed after the template sections and before `## Source References`.
3. **Given** the extra section records a decision that bears on a requirement, **When** the comment round runs, **Then** the decision is folded into the relevant requirement where appropriate, and the extra section itself is still not deleted.

---

### User Story 2 - A human-added section survives the first Specify run (Priority: P2)

A human adds a non-template section to a rough issue body before Specify first runs. The initial Specify run restructures the body into the template. The extra section is carried forward verbatim rather than being normalised away.

**Why this priority**: The same loss can happen on the first pass, but the initial run rewrites the body most heavily, so the preserve-versus-restructure boundary is less obvious there. The rule is the same, but it matters slightly less than the comment-round case that #2056 reports.

**Independent Test**: Give the Specify skill a rough body containing an extra `## Human Decisions` section. Confirm the emitted body keeps it verbatim after the template sections.

**Acceptance Scenarios**:

1. **Given** a rough issue body that includes a non-template section, **When** the Specify stage first runs, **Then** the emitted body is in template shape and also contains the non-template section verbatim.

---

### User Story 3 - Template rules still govern the template's own sections (Priority: P2)

The carry-forward rule must not weaken the existing template discipline. The template's own sections keep their exact headings, order, header fields and numbering rules.

**Why this priority**: The template rule exists for good reasons (consistent specs, a stable persisted file). The fix must narrow its scope, not remove it.

**Independent Test**: Read the revised skill text and confirm the template rules are still stated for template sections, with the carry-forward rule stated as covering only sections outside the template.

**Acceptance Scenarios**:

1. **Given** a body with template sections and an extra section, **When** the body is updated, **Then** the template sections still follow the template (headings, order, header fields, `FR-NNN`/`SC-NNN` numbering) and only the extra section is exempt from template normalisation.

---

### Edge Cases

- The extra section sits in the middle of the body, between template sections. The rule places it after the template sections, before `## Source References` if present. The issue defines no other position; see Assumptions.
- There are several extra sections. Each is carried forward verbatim and their relative order is preserved.
- The extra section duplicates information already in a requirement. It is still carried forward; the model may fold the decision into the requirement but never deletes the section.
- The extra section's heading resembles a template heading, for example a differently worded `## Open Question`. It is treated as a non-template section unless it is exactly a template heading.
- `## Open Questions` is a template section with its own removal rule. It is not covered by the carry-forward rule and keeps its existing behaviour.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: `fabrik-specify/SKILL.md` MUST state that any section in the current body that is not part of the template is carried forward verbatim: same heading, same content, same relative position (after the template sections, before `## Source References` if present). It MUST state that the template rules govern the template's own sections only.
- **FR-002**: The `fabrik-specify` Quality Checklist MUST include a matching item verifying that non-template sections were carried forward verbatim.
- **FR-003**: `fabrik-specify-comment/SKILL.md` MUST state the same rule: non-template sections are preserved verbatim, content a human added to the body is treated as input, any decisions it records are folded into the relevant requirements where appropriate, and the section itself is never deleted.
- **FR-004**: Every other skill that emits `FABRIK_ISSUE_UPDATE_BEGIN/END` MUST receive the same one-line rule if it contains a body-shape instruction that could cause sections to be dropped. The Plan stage enumerates the affected skills.
- **FR-005**: The rule MUST be worded consistently across all affected skills so the same sentence or an obvious restatement appears in each.
- **FR-006**: All edits MUST be made to the embedded source under `plugin/fabrik-workflows/skills/`, not to the deployed copy under `.fabrik/plugin/`.
- **FR-007**: The change MUST be verified by a sim scenario showing a body with an extra `## Human Decisions` section surviving a Specify comment round. If skill behaviour cannot be exercised deterministically in the sim, Plan MUST say so, and a skill-text assertion test MUST pin the rule in every affected skill instead. An existing skill-text test that already pins skill content may serve if it fits.
- **FR-008**: The PR body MUST mention #2056 by bare number and MUST NOT place a closing keyword (`Closes`/`Fixes`/`Resolves`) near it. The PR carries `Closes #2088` for this issue only.

### Key Entities

- **Template section**: A section defined by the Spec Kit spec template used by Specify, including the header fields and `## Open Questions`.
- **Non-template section**: Any heading-delimited section in the current issue body that the template does not define, for example `## Human Decisions`.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: In a Specify comment round over a body containing an extra `## Human Decisions` section, the emitted body contains that section with identical heading and content.
- **SC-002**: The carry-forward rule appears in `fabrik-specify`, `fabrik-specify-comment` and every other skill Plan identifies as emitting an issue-body update with a shape instruction. A repository check confirms its presence in each.
- **SC-003**: The rule's wording is consistent across all affected skills; a reviewer can match it by a single shared phrase.
- **SC-004**: The template rules for the template's own sections (headings, order, header fields, numbering) remain stated unchanged in the skills.
- **SC-005**: No file under `.fabrik/plugin/` is changed by the PR.

## Assumptions

- "Same relative position" means the extra section stays after all template sections and before `## Source References` when that section is present, as the issue specifies. For several extra sections, their order relative to each other is preserved.
- The only skills currently known to emit `FABRIK_ISSUE_UPDATE_BEGIN/END` are `fabrik-specify` and `fabrik-specify-comment`. `plugin/fabrik-workflows/README.md` also mentions the marker. Plan confirms the full list.
- Folding a human-recorded decision into a requirement is permitted but not required. The only hard rule is that the section is never deleted.
- Whether the sim can exercise the skill text deterministically is decided in Plan. Because the sim uses a scripted Claude invoker, a skill-text assertion test is the expected fallback.

## Out of Scope *(optional)*

- Any engine change to the body write path, such as detecting or merging concurrent edits or the blind PATCH overwrite of an edit made *during* a run. This is tracked on #2056 pending the reporter's confirmation.
- Changes to the template itself.
- Changes to how the engine projects the body to `specs/<N>-*/spec.md`.

## Source References *(optional)*

- #2056: report of the dropped `## Human Decisions` section (mention by bare number only)
- #2034: spec persistence to `specs/<N>-*/spec.md`
- Commits 02dca7a7 and 3fb02273: the template rule added since v0.0.83
