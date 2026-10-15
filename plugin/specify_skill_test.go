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
	content := readSkill(t, "specify")
	pos := 0
	for _, line := range specTemplateLines {
		i := strings.Index(content[pos:], line)
		if i < 0 {
			t.Fatalf("specify/SKILL.md: template line %q missing or out of order", line)
		}
		pos += i + len(line)
	}
	for _, want := range []string{
		"specs/<issue number>-<slug>/spec.md",
		"`Draft` while any open question remains and `Specified` once none do",
		"exactly `## Open Questions`",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("specify/SKILL.md missing %q", want)
		}
	}
	for _, banned := range []string{".specify/templates", "spec-template.md"} {
		if strings.Contains(content, banned) {
			t.Errorf("specify/SKILL.md must not point at %q: the template is inlined", banned)
		}
	}
}

// TestSpecifyCommentSkillReferencesTemplate pins that the comment skill uses
// the same template by reference and does not carry a second copy that could
// drift from specify's.
func TestSpecifyCommentSkillReferencesTemplate(t *testing.T) {
	content := readSkill(t, "specify-comment")
	for _, want := range []string{
		"`fabrik-stages:specify` skill",
		"Spec template",
		"specs/<issue number>-<slug>/spec.md",
		"`Draft` while any open question remains, `Specified` once none do",
		"exactly that",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("specify-comment/SKILL.md missing %q", want)
		}
	}
	for _, copied := range []string{
		"# Feature Specification: [Feature Title]",
		"**Why this priority**: [Rationale]",
		"## Source References *(optional)*",
	} {
		if strings.Contains(content, copied) {
			t.Errorf("specify-comment/SKILL.md restates template text %q; reference specify instead", copied)
		}
	}
}

// TestSpecifySkillsCarryNonTemplateSections pins the carry-forward rule (#2088):
// a section in the current issue body that the Spec template does not define
// (for example a human-added "## Human Decisions") must survive a Specify
// round. The sim's ClaudeInvoker is scripted and never reads a skill, so the
// instruction itself is what these assertions guard.
func TestSpecifySkillsCarryNonTemplateSections(t *testing.T) {
	const sharedPhrase = "carry non-template sections forward verbatim"
	specify := readSkill(t, "specify")
	comment := readSkill(t, "specify-comment")

	for name, body := range map[string]string{"specify": specify, "specify-comment": comment} {
		if !strings.Contains(strings.ToLower(body), sharedPhrase) {
			t.Errorf("%s: missing shared phrase %q", name, sharedPhrase)
		}
		if !strings.Contains(body, "## Human Decisions") {
			t.Errorf("%s: carry-forward rule should name a concrete example section", name)
		}
		if !strings.Contains(body, "never deleted") && !strings.Contains(body, "never delete the section") {
			t.Errorf("%s: missing the never-delete wording", name)
		}
	}

	if !strings.Contains(specify, "- [ ] If the current body already started with `# Feature Specification:`, any non-template section was carried forward verbatim") {
		t.Error("specify: Quality Checklist is missing the gated carry-forward item")
	}

	// The rule is gated on template shape: a rough first-run body is input to
	// restructure, never preserved and appended after the template.
	const gate = "when the current body already starts with `# Feature Specification:`"
	const restructure = "input to restructure, not sections to preserve"
	for name, body := range map[string]string{"specify": specify, "specify-comment": comment} {
		if !strings.Contains(body, gate) {
			t.Errorf("%s: carry-forward rule is missing the template-shape gate %q", name, gate)
		}
		if !strings.Contains(body, restructure) {
			t.Errorf("%s: missing the pre-format restructure wording %q", name, restructure)
		}
	}
	for _, p := range []string{
		"the original request verbatim in `**Input**` and the motivation in `## Background`",
		"never append the rough body after the template",
	} {
		if !strings.Contains(specify, p) {
			t.Errorf("specify: first-run restructure wording missing %q", p)
		}
	}

	// The over-broad wordings that covered the whole body must be gone.
	banned := map[string][]string{
		"specify":         {"**Every other heading stays exactly as in the template**", "The body follows the Spec template exactly"},
		"specify-comment": {"keep every heading, header field and the order exactly as they are"},
	}
	for name, phrases := range banned {
		body := map[string]string{"specify": specify, "specify-comment": comment}[name]
		for _, p := range phrases {
			if strings.Contains(body, p) {
				t.Errorf("%s: still contains over-broad wording %q", name, p)
			}
		}
	}

	// Template discipline for the template's own sections is retained.
	for _, p := range []string{
		"**Every other *template* heading stays exactly as in the template**",
		"The template sections follow the Spec template exactly",
		"never renumber an existing requirement",
	} {
		if !strings.Contains(specify, p) {
			t.Errorf("specify: template rule weakened, missing %q", p)
		}
	}
	for _, p := range []string{
		"keep every *template* heading, header field and the order exactly as they are",
		"never renumber existing FR/SC identifiers",
	} {
		if !strings.Contains(comment, p) {
			t.Errorf("specify-comment: template rule weakened, missing %q", p)
		}
	}
}
