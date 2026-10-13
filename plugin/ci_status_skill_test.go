package plugin

import (
	"strings"
	"testing"
)

// ciStatusSkills are the six test-running skills that carry the "Skipping a
// redundant full-suite run" rules, now the "Which tests to run locally" table (#1997, amended by #2116, ADR-2116).
var ciStatusSkills = []string{
	"implement", "review", "validate",
	"implement-comment", "review-comment", "validate-comment",
}

// ciStatusSkillPins are phrases every one of the six skills must carry. One
// loop over all six is what keeps the copies from drifting apart.
var ciStatusSkillPins = []struct{ name, phrase string }{
	// R1: a red verdict means targeted reproduction, never the full suite first.
	{"red rule", "When `verdict` is `red`, reproduce only what CI says failed — never the full suite as a first step."},
	{"failing checks section", "`## Failing checks`"},
	{"smallest unit", "the smallest unit that contains them"},
	{"non-test check", "run that check's own command"},
	{"after a fix", "re-run those failing tests plus the tests that cover the code you changed"},
	// The green skip rule from #1997 is unchanged.
	{"green skip", "`verdict: green`"},
	{"green skip sha", "prints `head_sha`"},
	// R3: per-path statement of which pushes a CI gate stands behind.
	{"gate flags", "`ci_gate_ahead`"},
	{"ci-fix path", "CI-fix reinvoke and rebase reinvoke"},
	{"auto-repair path", "Merge-train auto-repair"},
	{"no gate", "If both are `false`, no CI gate stands behind this push"},
	// R4: load awareness.
	{"uptime", "compare `uptime` with the CPU count"},
	{"load-induced", "is load-induced"},
	{"sleep loop", "in a `sleep` loop for longer than the test timeout"},
}

// Tokens that name a single toolchain's commands. The shipped skills serve every
// project, so they describe actions ("the project's test runner") instead (R5).
var ciStatusSkillBanned = []string{
	"go test", "go build", "go vet", "go fmt", "gofmt", "-race", "./...",
}

func TestCIStatusSkills_PinnedWording(t *testing.T) {
	for _, skill := range ciStatusSkills {
		body := readSkill(t, skill)
		for _, pin := range ciStatusSkillPins {
			if !strings.Contains(body, pin.phrase) {
				t.Errorf("%s: missing %s: %q", skill, pin.name, pin.phrase)
			}
		}
	}
}

func TestCIStatusSkills_BlanketCommentRuleGone(t *testing.T) {
	for _, skill := range ciStatusSkills {
		body := readSkill(t, skill)
		for _, gone := range []string{
			"Treat `ci_gated` as `false`",
			"Do not fall back to targeted tests only",
			"has no CI gate behind it — whatever `ci_gated` says",
		} {
			if strings.Contains(body, gone) {
				t.Errorf("%s still carries the removed blanket rule: %q", skill, gone)
			}
		}
	}
}

// No skill may license a full-suite run as the first response to a red verdict:
// the red paragraph forbids it, and the only "run the full suite" licence left
// is the no-gate-anywhere case.
func TestCIStatusSkills_RedNeverLicensesFullSuite(t *testing.T) {
	for _, skill := range ciStatusSkills {
		body := readSkill(t, skill)
		start := strings.Index(body, "**When `verdict` is `red`")
		end := strings.Index(body, "**Which pushes a CI gate stands behind.**")
		if start < 0 || end < start {
			t.Fatalf("%s: red paragraph not found", skill)
		}
		red := strings.ToLower(body[start:end])
		for _, bad := range []string{"run the full suite", "run the full test suite", "run the whole suite"} {
			if strings.Contains(red, bad) {
				t.Errorf("%s: red section licenses %q", skill, bad)
			}
		}
	}
}

func TestCIStatusSkills_LanguageNeutral(t *testing.T) {
	for _, skill := range ciStatusSkills {
		body := readSkill(t, skill)
		for _, banned := range ciStatusSkillBanned {
			if strings.Contains(body, banned) {
				t.Errorf("%s names a single toolchain's command: %q", skill, banned)
			}
		}
	}
}
