package plugin

import (
	"strings"
	"testing"
)

// TestSpawnSkills_ChildSpecsStartFromBaseBranch pins the #1784 guidance in
// every skill that can declare FABRIK_SPAWN_CHILD_* blocks (Plan, Review and
// Validate — ADR-1419). A spawned child's spec is read by an agent starting
// from the base branch, so the spawning stage must not present its own
// unmerged work as present, and must account for the child blocking its
// parent. CI can prove the text is there; Claude's behaviour under it is only
// observable in a live run.
func TestSpawnSkills_ChildSpecsStartFromBaseBranch(t *testing.T) {
	markers := []struct{ name, phrase string }{
		{"heading", "### Child specs start from the base branch"},
		{"base branch reader", "starts from the **base branch**, not from your worktree"},
		{"base label inheritance", "`base:<branch>` label, which same-repo children inherit"},
		{"unmerged not present", "**Unmerged work is not present.**"},
		{"acceptance criterion", "do not make them part of any acceptance criterion"},
		{"spec-persist artifact", "`specs/<N>-<slug>/spec.md`"},
		{"allowed background", "clearly marked in-flight background that no acceptance criterion requires"},
		{"child blocks parent", "**The child blocks its parent.**"},
		{"circular wait", "circular wait"},
		{"reframe", "**Reframe it to stand on the base branch.**"},
		{"do not spawn", "do not spawn a child"},
		{"depends_on siblings", "**`DEPENDS_ON:` covers siblings only.**"},
	}
	for _, skill := range []string{"plan", "review", "validate"} {
		body := readSkill(t, skill)
		for _, m := range markers {
			t.Run(skill+"/"+m.name, func(t *testing.T) {
				if !strings.Contains(body, m.phrase) {
					t.Errorf("%s skill missing %q", skill, m.phrase)
				}
			})
		}
	}
}

// TestSpawnSkills_ReviewAndValidateDocumentDependsOn: DEPENDS_ON already works
// for Review/Validate blocks (shared parser and spawnChildren); the skills must
// say so, as Plan does.
func TestSpawnSkills_ReviewAndValidateDocumentDependsOn(t *testing.T) {
	for _, skill := range []string{"review", "validate"} {
		t.Run(skill, func(t *testing.T) {
			body := readSkill(t, skill)
			for _, p := range []string{
				"#### Declaring sibling ordering with `DEPENDS_ON`",
				"**immediately after** `TITLE:`",
				"forward-only",
			} {
				if !strings.Contains(body, p) {
					t.Errorf("%s skill missing %q", skill, p)
				}
			}
		})
	}
}
