package plugin

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// unallowlistableShapeRes are the three regexes from #1777's R4: Claude
// Code's Bash allow rules (Bash(git:*), Bash(gh:*), ...) are whole-command
// prefix matches, so a shell shape whose own leading token isn't one of
// those prefixes is denied under --permission-mode dontAsk no matter what
// command it wraps. `x=$(...)` leads with the variable name, `if`/`for`/
// `while` lead with the keyword, and a herestring (`<<<`) is inseparable
// from the `read ... <<< "$var"` construct that carries it. None of these
// can be expressed as an allowlist prefix, so the fix is to never emit
// them in a shipped skill's own instructions to the model.
//
// These three patterns are a starting set (see the issue's own Risks
// section) — a future unallowlistable shape they don't recognize (process
// substitution `<(...)`, arithmetic `$(( ))`, backticks) would pass this
// guard undetected. Not a reason to widen scope now, but worth remembering
// if a new shape is ever reported.
var unallowlistableShapeRes = []struct {
	name string
	re   *regexp.Regexp
}{
	{"variable assignment via command substitution (x=$(...))", regexp.MustCompile(`^\s*\w+=\$\(`)},
	{"shell conditional (if/for/while)", regexp.MustCompile(`^\s*(if|for|while)\s`)},
	{"herestring (<<<)", regexp.MustCompile(`<<<`)},
}

// findUnallowlistableShapes scans content line by line for any of the three
// #1777 R4 shapes and returns one description per hit ("line N: <name>").
// Factored out of the enumeration test so AC2 (the guard demonstrably fails
// when a shape is reintroduced) can be exercised directly against literal
// fixture strings, without dirtying a real SKILL.md file.
func findUnallowlistableShapes(content string) []string {
	var hits []string
	for i, line := range strings.Split(content, "\n") {
		for _, shape := range unallowlistableShapeRes {
			if shape.re.MatchString(line) {
				hits = append(hits, fmt.Sprintf("line %d: %s", i+1, shape.name))
			}
		}
	}
	return hits
}

// TestNoUnallowlistableBashShapesInShippedSkills is the R4 regression guard:
// it scans every shipped fabrik-workflows/skills/*/SKILL.md (stage skills
// and comment-path skills alike — Research confirmed zero false positives
// against the entire current corpus, so scanning file-wide rather than
// scoping to the 3 in-scope skills keeps the issue's own "should stay
// conditional" note about the comment-path carve-out honest without extra
// logic) and fails if any of the three R4 shapes appears. AC1.
func TestNoUnallowlistableBashShapesInShippedSkills(t *testing.T) {
	skillDirs, err := FabrikPlugin.ReadDir("fabrik-workflows/skills")
	if err != nil {
		t.Fatalf("reading skills dir from embed: %v", err)
	}
	if len(skillDirs) == 0 {
		t.Fatal("no skill directories found in embed — did the embed path change?")
	}

	for _, d := range skillDirs {
		if !d.IsDir() {
			continue
		}
		name := d.Name()
		t.Run(name, func(t *testing.T) {
			content, err := FabrikPlugin.ReadFile("fabrik-workflows/skills/" + name + "/SKILL.md")
			if err != nil {
				t.Fatalf("reading %s/SKILL.md from embed: %v", name, err)
			}
			hits := findUnallowlistableShapes(string(content))
			if len(hits) > 0 {
				t.Errorf("%s/SKILL.md contains %d unallowlistable Bash shape(s) that --permission-mode dontAsk will always deny:\n%s",
					name, len(hits), strings.Join(hits, "\n"))
			}
		})
	}
}

// TestUnallowlistableShapeDetectorCatchesEachShape demonstrates the guard
// actually fails on a reintroduced shape (AC2), by running it directly
// against small literal fixtures — one per R4 shape — rather than
// temporarily dirtying a real SKILL.md file and leaving a footgun behind.
func TestUnallowlistableShapeDetectorCatchesEachShape(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"variable assignment via command substitution", "some prose\nbase_branch=$(gh pr view --json baseRefName --jq .baseRefName)\nmore prose"},
		{"if conditional", "some prose\nif [ -z \"$base_branch\" ]; then\n  base_branch=main\nfi"},
		{"for loop", "some prose\nfor f in $(git diff --name-only); do\n  echo \"$f\"\ndone"},
		{"while loop", "some prose\nwhile read -r line; do\n  echo \"$line\"\ndone"},
		{"herestring", "some prose\nread -r owner repo <<< \"$owner_repo\"\nmore prose"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hits := findUnallowlistableShapes(c.content)
			if len(hits) == 0 {
				t.Errorf("findUnallowlistableShapes did not catch a %s shape — the guard would silently pass a reintroduced occurrence", c.name)
			}
		})
	}

	t.Run("clean content produces no false positives", func(t *testing.T) {
		hits := findUnallowlistableShapes("gh pr view --json baseRefName --jq .baseRefName\ngit rev-list --count HEAD..origin/<base-branch>\nIf that prints nothing, fall back to main.")
		if len(hits) != 0 {
			t.Errorf("findUnallowlistableShapes reported false positive(s) on clean content: %v", hits)
		}
	})
}

// preCompletionGateInvariantRes are the substrings that must survive the
// #1777 rewrite of fabrik-validate's Pre-Completion Gate unchanged: the
// four distinct skip-reason tags (ADR-1364) and the Check A/B
// fail-toward-skip vs. Check C fail-toward-rebase asymmetry language. A
// rewrite that blurred any of these while converting shell `if` to prose
// would be a silent behavioral regression, not just a cosmetic one (R3,
// AC4) — so this is asserted, not just reviewed by eye.
var preCompletionGateInvariantRes = []struct {
	name string
	re   *regexp.Regexp
}{
	{"skipped-in-queue tag", regexp.MustCompile(`skipped-in-queue`)},
	{"skipped-detection-failed tag", regexp.MustCompile(`skipped-detection-failed`)},
	{"skipped-up-to-date tag", regexp.MustCompile(`skipped-up-to-date`)},
	{"skipped-ci-fresh tag", regexp.MustCompile(`skipped-ci-fresh`)},
	{"rebased outcome tag", regexp.MustCompile("`rebased`")},
	{"fail-toward-rebase language (Check C)", regexp.MustCompile(`fail-toward-rebase`)},
	{"fail-toward-skip language (Check A)", regexp.MustCompile(`fail-toward-skip`)},
}

// TestPreCompletionGatePreservesSkipReasonsAndDefaults guards AC4:
// fabrik-validate's Pre-Completion Gate section must still contain all
// four skip-reason tags and both the fail-toward-skip (Checks A/B) and
// fail-toward-rebase (Check C) default language after the R1/R2 rewrite.
func TestPreCompletionGatePreservesSkipReasonsAndDefaults(t *testing.T) {
	content, err := FabrikPlugin.ReadFile("fabrik-workflows/skills/fabrik-validate/SKILL.md")
	if err != nil {
		t.Fatalf("reading fabrik-validate/SKILL.md from embed: %v", err)
	}
	section := extractTopLevelSection(string(content), "## Pre-Completion Gate — MANDATORY before emitting FABRIK_STAGE_COMPLETE")
	if section == "" {
		t.Fatal("fabrik-validate/SKILL.md has no '## Pre-Completion Gate — MANDATORY before emitting FABRIK_STAGE_COMPLETE' section — did the heading change?")
	}

	for _, inv := range preCompletionGateInvariantRes {
		if !inv.re.MatchString(section) {
			t.Errorf("Pre-Completion Gate section is missing %q — the rewrite may have collapsed a distinct skip reason or blurred the fail-toward-skip/fail-toward-rebase asymmetry", inv.name)
		}
	}
}
