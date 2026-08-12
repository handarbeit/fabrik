package plugin

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestBeforeYouStartRebaseIsConditionalAndPushesNotResets guards against the
// rebase-reset loop reported in #1254/#1253 and fixed by #1554: the earlier
// "Before You Start" rebase step in fabrik-review and fabrik-validate used to
// be unconditional and had no push step, so a stage agent whose rebase
// produced SHAs that didn't match the remote would reach for
// `git reset --hard origin/<branch>` — discarding the rebase (and its
// commits) it had just performed.
//
// This is scoped to the "## Before You Start" section specifically, not the
// whole file: fabrik-validate/SKILL.md's later Pre-Completion Gate (Checks
// A/B/C, added by ADR-1364/#1364) already contains "behind_count" and
// "rev-list --count" for a *different*, untouched rebase point. A whole-file
// substring test would false-pass against pre-fix text there. See ADR-1554.
func TestBeforeYouStartRebaseIsConditionalAndPushesNotResets(t *testing.T) {
	for _, skill := range []string{"fabrik-review", "fabrik-validate"} {
		t.Run(skill, func(t *testing.T) {
			content, err := FabrikPlugin.ReadFile("fabrik-workflows/skills/" + skill + "/SKILL.md")
			if err != nil {
				t.Fatalf("reading %s/SKILL.md from embed: %v", skill, err)
			}
			section := extractTopLevelSection(string(content), "## Before You Start")
			if section == "" {
				t.Fatalf("%s/SKILL.md has no '## Before You Start' section — did the heading change?", skill)
			}
			assertRebaseSectionIsFixed(t, skill+"/SKILL.md \"Before You Start\" section", section)
		})
	}

	t.Run("CLAUDE.md", func(t *testing.T) {
		content, err := os.ReadFile("../CLAUDE.md")
		if err != nil {
			t.Fatalf("reading ../CLAUDE.md: %v", err)
		}
		bullet := extractRebaseBullet(string(content))
		if bullet == "" {
			t.Fatalf("CLAUDE.md has no 'Rebase onto the latest base branch' bullet — did it get renamed or removed?")
		}
		assertRebaseSectionIsFixed(t, "CLAUDE.md's rebase bullet", bullet)
	})
}

// TestBeforeYouStartResolvesBaseBranchDynamically guards against the
// regression identified during #1554's review: the initial fix added a
// force-with-lease push after the "Before You Start" rebase, but both
// skills still hardcoded `origin/main` for that rebase point. For an issue
// carrying a `base:<branch>` label, that combination turns a previously
// self-cancelling bug (a wrong-base rebase got reset away, discarding
// itself) into a persisted one (the wrong-base rebase now gets
// force-pushed onto the PR's actual, different base — silent corruption).
// The fix resolves the base branch dynamically via `gh pr view --json
// baseRefName` (the same mechanism fabrik-validate's own, untouched
// Pre-Completion Gate already uses ~230 lines below) and uses it
// throughout, instead of a literal `origin/main`.
func TestBeforeYouStartResolvesBaseBranchDynamically(t *testing.T) {
	for _, skill := range []string{"fabrik-review", "fabrik-validate"} {
		t.Run(skill, func(t *testing.T) {
			content, err := FabrikPlugin.ReadFile("fabrik-workflows/skills/" + skill + "/SKILL.md")
			if err != nil {
				t.Fatalf("reading %s/SKILL.md from embed: %v", skill, err)
			}
			section := extractTopLevelSection(string(content), "## Before You Start")
			if section == "" {
				t.Fatalf("%s/SKILL.md has no '## Before You Start' section — did the heading change?", skill)
			}
			assertBaseBranchIsResolvedDynamically(t, skill+"/SKILL.md \"Before You Start\" section", section)
		})
	}

	// fabrik-review's "Read the diff, not just the code" step (under "## How
	// You Review", not "## Before You Start") had the same hardcoded-origin/main
	// defect flagged by the same review — verify it was swept too.
	t.Run("fabrik-review/read-the-diff", func(t *testing.T) {
		content, err := FabrikPlugin.ReadFile("fabrik-workflows/skills/fabrik-review/SKILL.md")
		if err != nil {
			t.Fatalf("reading fabrik-review/SKILL.md from embed: %v", err)
		}
		if hardcodedDiffRe.MatchString(string(content)) {
			t.Errorf("fabrik-review/SKILL.md still has a hardcoded `git diff origin/main..HEAD` — should diff against the resolved $base_branch")
		}
	})
}

var (
	baseRefNameRe     = regexp.MustCompile(`baseRefName`)
	hardcodedRebaseRe = regexp.MustCompile(`git rebase origin/main\b`)
	hardcodedBehindRe = regexp.MustCompile(`HEAD\.\.origin/main\b`)
	hardcodedResetRe  = regexp.MustCompile(`reset --hard origin/main\b`)
	hardcodedDiffRe   = regexp.MustCompile(`git diff origin/main\.\.HEAD\b`)
)

// assertBaseBranchIsResolvedDynamically checks a "Before You Start" section
// resolves the base branch from the PR (`baseRefName`) rather than hardcoding
// `origin/main` in its rebase, behind-check, or reset-prohibition commands.
func assertBaseBranchIsResolvedDynamically(t *testing.T, label, section string) {
	t.Helper()

	if !baseRefNameRe.MatchString(section) {
		t.Errorf("%s does not resolve the base branch dynamically (expected \"baseRefName\" via `gh pr view --json baseRefName`)", label)
	}
	if hardcodedRebaseRe.MatchString(section) {
		t.Errorf("%s still hardcodes `git rebase origin/main` — must rebase onto the resolved $base_branch instead", label)
	}
	if hardcodedBehindRe.MatchString(section) {
		t.Errorf("%s still hardcodes `HEAD..origin/main` in its behind-check — must compare against the resolved $base_branch instead", label)
	}
	if hardcodedResetRe.MatchString(section) {
		t.Errorf("%s still hardcodes `reset --hard origin/main` in its reset prohibition — must name the resolved $base_branch instead", label)
	}
}

var (
	behindCheckRe    = regexp.MustCompile(`rev-list --count|behind_count`)
	forceWithLeaseRe = regexp.MustCompile(`force-with-lease`)
	// A reset-to-remote prohibition must both name the forbidden command and
	// qualify it with a negative-instruction word. Presence of "reset --hard"
	// alone isn't enough — the fix text necessarily mentions the forbidden
	// command in order to prohibit it, so absence-of-substring can't be the
	// test (see plan Task 5).
	resetHardRe        = regexp.MustCompile(`reset --hard`)
	resetProhibitionRe = regexp.MustCompile(`(?i)(never|do not|don't)[^.\n]{0,80}reset --hard|reset --hard[^.\n]{0,80}(never|discard|data loss)`)
	// A rejected --force-with-lease push (the remote moved since fetch — e.g.
	// the engine's own worktree push or commitWIP wrote the branch in the
	// meantime) is the same unguided-divergence state the fix exists to
	// close, one level down: without explicit guidance here, an agent facing
	// a rejected push has nothing to do but improvise again.
	rejectedPushRe = regexp.MustCompile(`(?i)rejected`)
)

// assertRebaseSectionIsFixed checks a section of text (a SKILL.md's "Before
// You Start" section, or CLAUDE.md's rebase bullet) for the three elements
// #1554 requires: a behind-check precondition (R2), a force-with-lease push
// instruction and an explicit reset-to-remote prohibition (R1), and mentions
// "reset --hard" only inside that prohibition, never as a bare instruction.
func assertRebaseSectionIsFixed(t *testing.T, label, section string) {
	t.Helper()

	if !behindCheckRe.MatchString(section) {
		t.Errorf("%s is missing a behind-check precondition (expected \"rev-list --count\" or \"behind_count\")", label)
	}
	if !forceWithLeaseRe.MatchString(section) {
		t.Errorf("%s is missing a \"force-with-lease\" push instruction", label)
	}
	if resetHardRe.MatchString(section) && !resetProhibitionRe.MatchString(section) {
		t.Errorf("%s mentions \"reset --hard\" without an adjacent prohibition (expected \"never\"/\"do not\" nearby, or \"discard\"/\"data loss\" framing)", label)
	}
	if !resetHardRe.MatchString(section) {
		t.Errorf("%s never mentions \"reset --hard\" at all — expected an explicit prohibition against resetting to the remote tip", label)
	}
	if !rejectedPushRe.MatchString(section) {
		t.Errorf("%s is missing guidance for a rejected --force-with-lease push (expected \"rejected\")", label)
	}
}

// extractTopLevelSection returns the text between a "## <heading>" line
// (matched exactly, trailing whitespace ignored) and the next line starting
// with "## " (or EOF), excluding the heading line itself. Returns "" if the
// heading isn't found. Nested "### " subsections are included — only another
// top-level "## " heading ends the section.
func extractTopLevelSection(content, heading string) string {
	lines := strings.Split(content, "\n")
	var out []string
	inSection := false
	for _, line := range lines {
		trimmed := strings.TrimRight(line, " \t\r")
		if trimmed == heading {
			inSection = true
			continue
		}
		if inSection && strings.HasPrefix(trimmed, "## ") {
			break
		}
		if inSection {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// extractRebaseBullet returns the single Markdown list-item line in
// CLAUDE.md's "Important Conventions" section that starts the rebase
// convention (identified by its stable "Rebase onto the latest base branch"
// lead-in, bold or not), or "" if not found.
func extractRebaseBullet(content string) string {
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "- ") && strings.Contains(trimmed, "Rebase onto the latest base branch") {
			return trimmed
		}
	}
	return ""
}
