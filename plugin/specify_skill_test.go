package plugin

import (
	"strings"
	"testing"
)

// TestSpecifySkillsUseSpecKitStructure pins ADR 2034: both Specify skills author
// the spec in Spec Kit's content structure, tell the worker the engine (not the
// worker) persists it, and keep Open Questions as the last section so the engine
// can strip it from the committed spec.
func TestSpecifySkillsUseSpecKitStructure(t *testing.T) {
	for _, skill := range []string{"fabrik-specify", "fabrik-specify-comment"} {
		t.Run(skill, func(t *testing.T) {
			b, err := FabrikPlugin.ReadFile("fabrik-workflows/skills/" + skill + "/SKILL.md")
			if err != nil {
				t.Fatal(err)
			}
			content := string(b)
			for _, want := range []string{
				"## User Scenarios & Testing",
				"(Priority: P1)",
				"*Independent test:*",
				"**FR-001**",
				"## Success Criteria",
				"**SC-001**",
				"## Edge Cases",
				"## Assumptions",
				"specs/<issue number>-<slug>/spec.md",
			} {
				if !strings.Contains(content, want) {
					t.Errorf("%s/SKILL.md missing %q", skill, want)
				}
			}
			// Open Questions is the last heading inside the structure template.
			tmpl := content[strings.Index(content, "## Problem"):]
			if end := strings.Index(tmpl, "\n```"); end >= 0 {
				tmpl = tmpl[:end]
			}
			if idx := strings.LastIndex(tmpl, "\n## "); idx < 0 || !strings.HasPrefix(tmpl[idx:], "\n## Open Questions") {
				t.Errorf("%s/SKILL.md: Open Questions must be the last section of the structure template", skill)
			}
		})
	}
}
