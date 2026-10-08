package plugin

import (
	"strings"
	"testing"
)

func readSkill(t *testing.T, skill string) string {
	t.Helper()
	b, err := FabrikPlugin.ReadFile("fabrik-workflows/skills/" + skill + "/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// specTemplateLines are the template's structural lines, in order. The Specify
// skill must carry them verbatim: the shipped default promotes the Spec Kit
// template downstream projects already produce, so a change here is a change of
// format, not of wording.
var specTemplateLines = []string{
	"# Feature Specification: [Feature Title]",
	"**Feature Branch**: `fabrik/issue-<N>`",
	"**Created**: [YYYY-MM-DD]",
	"**Status**: Draft",
	"**Input**: User description: \"[original request]\"",
	"## Background",
	"## User Scenarios & Testing *(mandatory)*",
	"### User Story 1 - [Brief Title] (Priority: P1)",
	"**Why this priority**: [Rationale]",
	"**Independent Test**: [How to test independently]",
	"**Acceptance Scenarios**:",
	"1. **Given** [state], **When** [action], **Then** [outcome]",
	"### Edge Cases",
	"## Requirements *(mandatory)*",
	"### Functional Requirements",
	"- **FR-001**: [Specific, testable requirement]",
	"### Key Entities *(if applicable)*",
	"## Success Criteria *(mandatory)*",
	"### Measurable Outcomes",
	"- **SC-001**: [Measurable, technology-agnostic outcome]",
	"## Assumptions",
	"## Out of Scope *(optional)*",
	"## Open Questions *(only if unresolved questions remain)*",
	"- [ ] [Question]",
	"## Source References *(optional)*",
}

// TestSpecifySkillCarriesSpecTemplate pins ADR 2034: the Specify skill inlines
// the Spec Kit spec template verbatim (no pointer to .specify/ or an example
// spec — managed repos may have neither), in order, and tells the worker the
// engine, not the worker, persists the spec.
func TestSpecifySkillCarriesSpecTemplate(t *testing.T) {
	content := readSkill(t, "fabrik-specify")
	pos := 0
	for _, line := range specTemplateLines {
		i := strings.Index(content[pos:], line)
		if i < 0 {
			t.Fatalf("fabrik-specify/SKILL.md: template line %q missing or out of order", line)
		}
		pos += i + len(line)
	}
	for _, want := range []string{
		"specs/<issue number>-<slug>/spec.md",
		"`Draft` while any open question remains and `Specified` once none do",
		"exactly `## Open Questions`",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("fabrik-specify/SKILL.md missing %q", want)
		}
	}
	for _, banned := range []string{".specify/templates", "spec-template.md"} {
		if strings.Contains(content, banned) {
			t.Errorf("fabrik-specify/SKILL.md must not point at %q: the template is inlined", banned)
		}
	}
}

// TestSpecifyCommentSkillReferencesTemplate pins that the comment skill uses
// the same template by reference and does not carry a second copy that could
// drift from fabrik-specify's.
func TestSpecifyCommentSkillReferencesTemplate(t *testing.T) {
	content := readSkill(t, "fabrik-specify-comment")
	for _, want := range []string{
		"`fabrik-specify` skill",
		"Spec template",
		"specs/<issue number>-<slug>/spec.md",
		"`Draft` while any open question remains, `Specified` once none do",
		"exactly that",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("fabrik-specify-comment/SKILL.md missing %q", want)
		}
	}
	for _, copied := range []string{
		"# Feature Specification: [Feature Title]",
		"**Why this priority**: [Rationale]",
		"## Source References *(optional)*",
	} {
		if strings.Contains(content, copied) {
			t.Errorf("fabrik-specify-comment/SKILL.md restates template text %q; reference fabrik-specify instead", copied)
		}
	}
}
