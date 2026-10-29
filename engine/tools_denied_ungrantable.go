package engine

import (
	"fmt"
	"path/filepath"
	"strings"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

// Ungrantable tool denials (#2301, ADR 2301; amends ADR-1523/ADR-1775).
//
// Claude Code refuses writes to its "protected paths" (.git, .claude, …) in
// non-interactive mode, and allow rules do not pre-approve them. Its denial
// record carries only the tool name and tool_input, and its tool-result text is
// generic, so "no rule can grant this" is inferred structurally: a file-tool
// call whose decoded path is inside the invocation's worktree and which the
// allow list Fabrik passed *already covers* (the bare tool name, or
// Tool(<worktree>/**)) cannot be fixed by an allowed_tools change, a simpler
// command or a retry. Such a denial pauses on its first occurrence instead of
// after MaxToolsDeniedRetries.
//
// The verdict uses only the structured denial and Fabrik's own arguments for
// the invocation — never tool-result prose and never a hard-coded protected-path
// list (that list is CLI-version-specific). protectedPathDoc is wording only.

// fileDenialTools are the file tools whose denied calls can be ungrantable.
var fileDenialTools = map[string]bool{"Edit": true, "Write": true, "MultiEdit": true, "NotebookEdit": true}

// allowRulesFromArgs returns the --allowedTools values in args and whether the
// invocation ran with --dangerously-skip-permissions (the fabrik:unrestricted
// path, which sends no --allowedTools at all). Reading them from the args slice
// actually handed to the CLI means the read-only and worktree-boundary rewrites
// of buildClaudeArgs cannot drift from the verdict.
func allowRulesFromArgs(args []string) (rules []string, skipPermissions bool) {
	for i, a := range args {
		switch a {
		case "--dangerously-skip-permissions":
			skipPermissions = true
		case "--allowedTools":
			if i+1 < len(args) {
				rules = append(rules, args[i+1])
			}
		}
	}
	return rules, skipPermissions
}

// ungrantableDenial is the pure predicate: it reports the worktree-relative path
// and the covering rule when d is a file-tool denial no allow rule can change.
func ungrantableDenial(d toolDenial, rules []string, workDir string, skipPermissions bool) (relPath, coveredBy string, ok bool) {
	if skipPermissions || workDir == "" || !fileDenialTools[d.ToolName] || d.Path == "" {
		return "", "", false
	}
	// A relative decoded path is not judged inside the worktree: the check is
	// lexical on an absolute path, matching how the CLI compares against the
	// rule text (no filesystem access, no symlink resolution).
	if !filepath.IsAbs(d.Path) || !filepath.IsAbs(workDir) {
		return "", "", false
	}
	root := filepath.Clean(workDir)
	rel, err := filepath.Rel(root, filepath.Clean(d.Path))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", false
	}
	scoped := fmt.Sprintf("%s(%s/**)", d.ToolName, workDir)
	for _, r := range rules {
		if r == d.ToolName || r == scoped {
			return filepath.ToSlash(rel), r, true
		}
	}
	return "", "", false
}

// markUngrantableDenials stamps the ungrantable verdict on e's denials from the
// args of the invocation that produced them.
func markUngrantableDenials(e *claudeToolsDeniedError, args []string, workDir string) {
	rules, skip := allowRulesFromArgs(args)
	for i := range e.Denials {
		if rel, covered, ok := ungrantableDenial(e.Denials[i], rules, workDir, skip); ok {
			e.Denials[i].Ungrantable = true
			e.Denials[i].RelPath = rel
			e.Denials[i].CoveredBy = covered
		}
	}
}

// SetUngrantableDenialDisabledForTest makes the engine treat every denial as not
// ungrantable (the pre-#2301 bounded-retry behaviour), so a test can show its
// first-occurrence assertions bite (the non-vacuity seam). Per-Engine.
func (e *Engine) SetUngrantableDenialDisabledForTest(disabled bool) {
	e.ungrantableDenialOff.Store(disabled)
}

// ungrantableDenials returns the denials stamped ungrantable, empty while the
// test seam is on.
func (e *Engine) ungrantableDenials(denials []toolDenial) []toolDenial {
	if e.ungrantableDenialOff.Load() {
		return nil
	}
	var out []toolDenial
	for _, d := range denials {
		if d.Ungrantable && d.RelPath != "" {
			out = append(out, d)
		}
	}
	return out
}

const ungrantablePathsShown = 5

// ungrantableDenialLines renders each distinct ungrantable "tool path" as an
// inline-code bullet, capped at ungrantablePathsShown.
func ungrantableDenialLines(ungrantable []toolDenial) (lines []string, protectedNote string) {
	seen := map[string]bool{}
	var distinct []toolDenial
	for _, d := range ungrantable {
		key := d.ToolName + "\x00" + d.RelPath
		if seen[key] {
			continue
		}
		seen[key] = true
		distinct = append(distinct, d)
	}
	for i, d := range distinct {
		if i >= ungrantablePathsShown {
			lines = append(lines, fmt.Sprintf("- …and %d more", len(distinct)-ungrantablePathsShown))
			break
		}
		lines = append(lines, fmt.Sprintf("- **%s** `%s`", d.ToolName, sanitizeToolsDeniedCommand(d.RelPath)))
		if protectedNote == "" && protectedPathDoc(d.RelPath) {
			protectedNote = "\n\nThis path is on Claude Code's documented list of protected paths ([permission modes — protected paths](https://code.claude.com/docs/en/permission-modes))."
		}
	}
	return lines, protectedNote
}

// protectedPathDoc reports whether rel (slash-separated, worktree-relative)
// matches a directory or file in Claude Code's documented protected-path list.
// Wording only (R4): it never feeds the classification, and the list is
// CLI-version-specific.
func protectedPathDoc(rel string) bool {
	rel = strings.TrimPrefix(filepath.ToSlash(rel), "./")
	if strings.HasPrefix(rel, ".claude/worktrees/") {
		return false
	}
	first, _, _ := strings.Cut(rel, "/")
	switch first {
	case ".git", ".claude", ".vscode", ".idea", ".husky", ".cargo", ".devcontainer", ".yarn", ".mvn":
		return true
	}
	if strings.HasPrefix(rel, ".config/git/") {
		return true
	}
	switch rel {
	case ".gitconfig", ".gitmodules", ".bashrc", ".bash_profile", ".zshrc", ".zprofile", ".profile", ".npmrc", ".envrc":
		return true
	}
	return false
}

// pauseForToolsDenied pauses the issue after recordToolsDeniedDetection asked
// for it: on the first occurrence for an ungrantable denial, otherwise at the
// MaxToolsDeniedRetries bound. Called identically from the stage-dispatch path
// and the comment-review/reinvoke family.
func (e *Engine) pauseForToolsDenied(item gh.ProjectItem, stage *stages.Stage, count int, toolNames []string, denials []toolDenial) {
	if ungrantable := e.ungrantableDenials(denials); len(ungrantable) > 0 {
		e.pauseForUngrantableDenial(item, stage, ungrantable, denials)
		return
	}
	e.pauseForToolsDeniedLimit(item, stage, count, e.cfg.MaxToolsDeniedRetries, toolNames, denials)
}

// pauseForUngrantableDenial pauses on the first denial no allow rule can change
// (#2301). Like pauseForToolsDeniedLimit: fabrik:paused + fabrik:awaiting-input,
// EnginePaused applied (removing fabrik:paused resets the counters through
// clearFailedStage) and deliberately no stage:<name>:failed.
func (e *Engine) pauseForUngrantableDenial(item gh.ProjectItem, stage *stages.Stage, ungrantable, all []toolDenial) {
	lines, protectedNote := ungrantableDenialLines(ungrantable)
	other := ""
	if n := len(all) - len(ungrantable); n > 0 {
		other = fmt.Sprintf("\n\nThe same run also had %d other denied tool call(s), which may not be the cause of this pause.", n)
	}
	comment := fmt.Sprintf(
		"🏭 **Fabrik — tool permission denied (cannot be granted)**\n\nStage **%s** had a file edit denied by the CLI's permission configuration:\n\n%s%s\n\n"+
			"Fabrik already allows this tool on this path, yet Claude Code refused the write in Fabrik's non-interactive permission mode. No `allowed_tools` rule can change that, and re-running or splitting the step into simpler commands will not help, so Fabrik has paused this issue on the first occurrence instead of retrying. "+
			"This is not a stage failure — it does not count against `max_retries`.%s\n\n"+
			"Options:\n"+
			"1. Make the change by hand on the `fabrik/issue-%d` branch, push it, then remove the `fabrik:paused` and `fabrik:awaiting-input` labels to resume.\n"+
			"2. Restructure the work so it does not need to write there, then remove the `fabrik:paused` and `fabrik:awaiting-input` labels.\n"+
			"3. Add the `fabrik:unrestricted` label for this issue, then remove `fabrik:paused` and `fabrik:awaiting-input` (caveat: it removes all tool restrictions, not just this one, so use it deliberately).",
		stage.Name, strings.Join(lines, "\n"), protectedNote, other, item.Number,
	)
	e.pauseIssue(item, comment, pauseOpts{
		awaitingInput: true,
		reactRocket:   true,
		cause:         pauseCauseQuestion,
	})

	repoStr := itemOwnerRepoString(item, e.defaultRepo())
	e.store.Apply(itemstate.EnginePaused{Repo: repoStr, Number: item.Number, StageName: stage.Name})
}

// logUngrantableDenials emits the R6 log line distinguishing a first-occurrence
// pause from a bounded retry.
func (e *Engine) logUngrantableDenials(item gh.ProjectItem, ungrantable []toolDenial) {
	for _, d := range ungrantable {
		e.logf(item.Number, "tools-denied", "ungrantable denial: %s %s (covered by %s) — pausing on first occurrence\n", d.ToolName, sanitizeToolsDeniedCommand(d.RelPath), d.CoveredBy)
	}
}
