package plugin

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ciGatedRuleSkills are the six built-in skills that run the test suite
// (#2130 R6). Each carries the same "Which tests to run locally" decision
// table and "Test command rules (hard)" block, duplicated because skills load
// separately and have no include mechanism — this test is what keeps the six
// copies from drifting.
var ciGatedRuleSkills = []string{
	"fabrik-validate",
	"fabrik-validate-comment",
	"fabrik-review",
	"fabrik-review-comment",
	"fabrik-implement",
	"fabrik-implement-comment",
}

// maxLocalTestTimeout is the R3 hard cap on a local test command.
const maxLocalTestTimeout = 5 * time.Minute

// fullSuiteGuardWindow is how many preceding lines may carry the
// `ci_gated: false` condition that licenses a full-module test command.
const fullSuiteGuardWindow = 8

var (
	// fullModuleGoTestRe matches a Go test command over the whole module.
	fullModuleGoTestRe = regexp.MustCompile(`go test\b.*\./\.\.\.`)
	// goTimeoutRe matches the Go-style `-timeout <value>` flag, but not the
	// double-dash flags other frameworks use (pytest --timeout=60) nor
	// jest's --testTimeout.
	goTimeoutRe = regexp.MustCompile("(?:^|[\\s`(])-timeout[= ]([0-9][0-9a-z.]*)")
)

// findUnconditionalFullSuite returns one description per line that runs a
// full-module `go test` without a `ci_gated: false` condition on the same line
// or within the fullSuiteGuardWindow preceding lines (#2130 R2): on a
// CI-gated stage the full suite is CI's job, so the only place a skill may
// instruct it is under that explicit condition.
func findUnconditionalFullSuite(content string) []string {
	lines := strings.Split(content, "\n")
	var hits []string
	for i, line := range lines {
		if !fullModuleGoTestRe.MatchString(line) {
			continue
		}
		start := i - fullSuiteGuardWindow
		if start < 0 {
			start = 0
		}
		guarded := false
		for _, l := range lines[start : i+1] {
			if strings.Contains(l, "ci_gated: false") {
				guarded = true
				break
			}
		}
		if !guarded {
			hits = append(hits, fmt.Sprintf("line %d: full-module go test without a `ci_gated: false` condition: %s", i+1, strings.TrimSpace(line)))
		}
	}
	return hits
}

// findOverCapTimeouts returns one description per `-timeout` value above
// maxLocalTestTimeout (#2130 R3). A bare number counts as seconds; a value
// that does not parse is reported too, since it cannot be shown to be within
// the cap.
func findOverCapTimeouts(content string) []string {
	var hits []string
	for i, line := range strings.Split(content, "\n") {
		for _, m := range goTimeoutRe.FindAllStringSubmatch(line, -1) {
			raw := m[1]
			d, err := time.ParseDuration(raw)
			if err != nil {
				secs, convErr := strconv.ParseFloat(raw, 64)
				if convErr != nil {
					hits = append(hits, fmt.Sprintf("line %d: unparseable -timeout %q", i+1, raw))
					continue
				}
				d = time.Duration(secs * float64(time.Second))
			}
			if d > maxLocalTestTimeout {
				hits = append(hits, fmt.Sprintf("line %d: -timeout %s exceeds the %s cap", i+1, raw, maxLocalTestTimeout))
			}
		}
	}
	return hits
}

// ciGatedRuleAnchors are the literal fragments that must be present in every
// skill that runs tests. Each pins one #2130 requirement; neutralising the
// wording that carries it makes missingCIGatedRules report it.
var ciGatedRuleAnchors = []struct {
	rule   string
	anchor string
}{
	{"R1 pending/unknown targeted-only rule", "`verdict` is `pending`, `unknown` or `none`"},
	{"R1 CI-gated stage never runs the full suite locally", "you never run the full suite locally"},
	{"R1 names the wait_for_ci gate as the backstop", "`wait_for_ci` gate"},
	{"R2 red verdict reads the failing checks", "`gh pr checks`"},
	{"R2 red verdict reproduces only failing tests", "reproduce only the failing tests"},
	{"R2 full-suite step stays when ci_gated is false", "does the full-suite step stay"},
	{"R3 5m hard cap", "that timeout is 5m or less per test process"},
	{"R3 never raise the timeout", "Never raise the timeout to fit a broader run — narrow the run instead"},
	{"R3 never background a test command", "Never background a test command and then poll it"},
	{"R4 diagnose before retrying", "Diagnose before retrying"},
	{"R4 rerun only the failing test", "rerun only the failing test, once"},
	{"R4 never retry broader or longer", "Never retry with a broader set of tests or a longer timeout"},
	{"R4 host load is reported, not a failure", "report it as host load"},
	{"R5 TARGETED report line", "`TARGETED — <packages>; full suite left to CI on <sha>`"},
	{"R5 SKIPPED report line", "`SKIPPED — CI green on <sha>`"},
	{"R5 PASSED report line", "`PASSED` (a full run, only when no CI gate stands behind the push)"},
}

// missingCIGatedRules returns the description of every required rule whose
// anchor is absent from content.
func missingCIGatedRules(content string) []string {
	var missing []string
	for _, a := range ciGatedRuleAnchors {
		if !strings.Contains(content, a.anchor) {
			missing = append(missing, fmt.Sprintf("%s (expected %q)", a.rule, a.anchor))
		}
	}
	return missing
}

// TestCIGatedSkillRules pins #2130 against the embedded skills: no skill
// instructs an unconditional full-module test or a timeout above 5m, and every
// skill that runs tests carries the CI-gated, hard-cap and
// diagnose-before-retry rules and the three report lines.
func TestCIGatedSkillRules(t *testing.T) {
	for _, name := range ciGatedRuleSkills {
		t.Run(name, func(t *testing.T) {
			content, err := FabrikPlugin.ReadFile("fabrik-workflows/skills/" + name + "/SKILL.md")
			if err != nil {
				t.Fatalf("reading %s/SKILL.md from embed: %v", name, err)
			}
			if hits := findUnconditionalFullSuite(string(content)); len(hits) > 0 {
				t.Errorf("%s/SKILL.md instructs a full-module test run outside a `ci_gated: false` condition:\n%s", name, strings.Join(hits, "\n"))
			}
			if hits := findOverCapTimeouts(string(content)); len(hits) > 0 {
				t.Errorf("%s/SKILL.md carries a test timeout above the 5m cap:\n%s", name, strings.Join(hits, "\n"))
			}
			if missing := missingCIGatedRules(string(content)); len(missing) > 0 {
				t.Errorf("%s/SKILL.md is missing required rule wording:\n%s", name, strings.Join(missing, "\n"))
			}
		})
	}
}

// TestCIGatedScannersFailOnReintroduction demonstrates acceptance 4: each
// scanner fires on the instruction it exists to forbid, run against literal
// fixtures rather than a mutated SKILL.md.
func TestCIGatedScannersFailOnReintroduction(t *testing.T) {
	t.Run("unguarded full-module go test is flagged", func(t *testing.T) {
		content := "Run the full suite:\n\n```bash\ngo test -race -timeout 5m ./...\n```\n"
		if len(findUnconditionalFullSuite(content)) == 0 {
			t.Error("findUnconditionalFullSuite did not flag an unguarded full-module go test")
		}
	})
	t.Run("a guard further back than the window does not count", func(t *testing.T) {
		content := "Only when `ci_gated: false`:\n" + strings.Repeat("filler\n", fullSuiteGuardWindow+1) + "go test -timeout 5m ./...\n"
		if len(findUnconditionalFullSuite(content)) == 0 {
			t.Error("findUnconditionalFullSuite accepted a guard outside the window")
		}
	})
	t.Run("guarded and targeted runs pass", func(t *testing.T) {
		content := "Only when `ci_gated: false`:\n\n```bash\ngo test -race -timeout 5m ./...\n```\n\nTargeted: `go test -race -timeout 5m ./engine ./plugin`\n"
		if hits := findUnconditionalFullSuite(content); len(hits) != 0 {
			t.Errorf("findUnconditionalFullSuite false positive: %v", hits)
		}
	})
	for _, tc := range []struct{ name, line string }{
		{"20m", "go test -timeout 20m ./engine"},
		{"14m", "run `go test -timeout=14m ./engine ./cmd`"},
		{"1h", "go test -timeout 1h ./tests/sim"},
		{"6m", "go test -timeout 6m ./plugin"},
		{"bare seconds over the cap", "go test -timeout 600 ./plugin"},
	} {
		t.Run("over-cap timeout "+tc.name+" is flagged", func(t *testing.T) {
			if len(findOverCapTimeouts(tc.line)) == 0 {
				t.Errorf("findOverCapTimeouts did not flag %q", tc.line)
			}
		})
	}
	t.Run("timeouts within the cap and other frameworks' flags pass", func(t *testing.T) {
		content := "go test -timeout 5m ./engine\ngo test -timeout=90s ./plugin\npytest --timeout=600\njest --testTimeout=3000000\ntimeout --signal=KILL <N> npm run dev\n"
		if hits := findOverCapTimeouts(content); len(hits) != 0 {
			t.Errorf("findOverCapTimeouts false positive: %v", hits)
		}
	})
	t.Run("each required rule is reported when its wording is neutralised", func(t *testing.T) {
		var full strings.Builder
		for _, a := range ciGatedRuleAnchors {
			full.WriteString(a.anchor + "\n")
		}
		if missing := missingCIGatedRules(full.String()); len(missing) != 0 {
			t.Fatalf("missingCIGatedRules reported rules on content that carries every anchor: %v", missing)
		}
		for _, a := range ciGatedRuleAnchors {
			neutralised := strings.ReplaceAll(full.String(), a.anchor, "")
			missing := missingCIGatedRules(neutralised)
			if len(missing) != 1 || !strings.HasPrefix(missing[0], a.rule) {
				t.Errorf("neutralising %q: want exactly that rule reported, got %v", a.rule, missing)
			}
		}
	})
}
