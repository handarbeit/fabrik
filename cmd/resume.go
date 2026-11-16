package cmd

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/handarbeit/fabrik/engine"
	"github.com/handarbeit/fabrik/internal/workspace"
	"github.com/handarbeit/fabrik/stages"
)

// execFn is the exec implementation used by runResume. Replaced in tests.
var execFn = func(argv0 string, argv []string, envv []string) error {
	return syscall.Exec(argv0, argv, envv)
}

// runResume implements the `fabrik resume <issue-number> [--stage <name>]` subcommand.
// It locates the issue's worktree, reads the session file for the given stage,
// and execs into an interactive Claude session. No GitHub credentials required.
func runResume(args []string) error {
	fset := flag.NewFlagSet("resume", flag.ContinueOnError)
	stageName := fset.String("stage", "", "Stage name to resume (required until #109 lands)")
	stagesDir := fset.String("stages", ".fabrik/stages", "Directory containing stage YAML configs")
	pluginDir := fset.String("plugin-dir", "", "Path to Fabrik plugin directory")
	repoFlag := fset.String("repo", "", "owner/repo of the worktree, required when issue-N exists under more than one repo")
	workspaceRoot := fset.String("workspace-root", "", "Workspace root holding worktrees and sessions (also FABRIK_WORKSPACE_ROOT, config.yaml workspace_root; default <dir>/.fabrik)")

	fset.Usage = func() {
		fmt.Fprintf(fset.Output(), "Usage: fabrik resume <issue-number> [--stage <name>] [flags]\n\n")
		fmt.Fprintf(fset.Output(), "Arguments:\n")
		fmt.Fprintf(fset.Output(), "  <issue-number>    GitHub issue number (required)\n\n")
		fmt.Fprintf(fset.Output(), "Flags:\n")
		fset.PrintDefaults()
	}

	// Detect --help/-h as the first arg before treating it as the issue number.
	// This prevents "--help" from being parsed as the issue number and producing
	// a confusing "must be a positive integer" error.
	if len(args) > 0 && (args[0] == "--help" || args[0] == "-h") {
		fset.Usage()
		return flag.ErrHelp
	}

	// Support `fabrik resume <issue-number> [flags]`: extract the issue number
	// from the front of args before flag parsing (flag.FlagSet stops at first
	// non-flag argument, so we must pull the positional out first).
	if len(args) == 0 || args[0] == "" {
		return fmt.Errorf("resume: expected exactly one positional argument: <issue-number>\nUsage: fabrik resume <issue-number> [--stage <name>]")
	}
	issueArg := args[0]
	args = args[1:]

	if err := fset.Parse(args); err != nil {
		return err
	}
	if fset.NArg() != 0 {
		return fmt.Errorf("resume: unexpected positional argument(s): %v\nUsage: fabrik resume <issue-number> [--stage <name>]", fset.Args())
	}

	// Allow env vars to fill in missing flags
	if *stagesDir == ".fabrik/stages" {
		if v := os.Getenv("FABRIK_STAGES"); v != "" {
			*stagesDir = v
		}
	}
	if *pluginDir == "" {
		if v := os.Getenv("FABRIK_PLUGIN_DIR"); v != "" {
			*pluginDir = v
		}
	}

	issueNumber, err := strconv.Atoi(issueArg)
	if err != nil || issueNumber <= 0 {
		return fmt.Errorf("resume: issue number must be a positive integer, got %q", issueArg)
	}

	if *stageName == "" {
		return fmt.Errorf("resume: --stage <name> is required (stage auto-detection requires #109 which is not yet implemented)")
	}

	// Locate the worktree
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resume: getting working directory: %w", err)
	}
	ws, err := workspace.Load(cwd, *workspaceRoot)
	if err != nil {
		return fmt.Errorf("resume: %w", err)
	}
	worktreeRoot := ws.Dir(workspace.Worktrees)
	var worktreeDir string
	if *repoFlag != "" {
		// An explicit repo is never resolved to some other repo's worktree.
		worktreeDir = filepath.Join(worktreeRoot, strings.ReplaceAll(*repoFlag, "/", "-"), fmt.Sprintf("issue-%d", issueNumber))
	} else {
		matches := worktreeMatches(worktreeRoot, issueNumber)
		if len(matches) > 1 {
			return fmt.Errorf("resume: issue #%d has worktrees in more than one repo:\n  %s\nPass --repo owner/repo to choose one.", issueNumber, strings.Join(matches, "\n  "))
		}
		worktreeDir = findWorktreeDir(worktreeRoot, issueNumber)
	}
	if _, err := os.Stat(worktreeDir); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("resume: worktree for issue #%d not found at %s\n"+
				"The issue has not been processed yet, or the worktree was cleaned up.", issueNumber, worktreeDir)
		}
		return fmt.Errorf("resume: stat %s: %w", worktreeDir, err)
	}

	// Load stage configs to get the model
	stageCfgs, err := stages.LoadAll(*stagesDir)
	if err != nil {
		return fmt.Errorf("resume: loading stages from %s: %w", *stagesDir, err)
	}
	stage := stages.FindStage(stageCfgs, *stageName)
	if stage == nil {
		var names []string
		for _, s := range stageCfgs {
			names = append(names, s.Name)
		}
		return fmt.Errorf("resume: stage %q not found in %s (available: %v)", *stageName, *stagesDir, names)
	}

	// Locate the claude binary
	claudeBin, err := exec.LookPath("claude")
	if err != nil {
		return fmt.Errorf("resume: claude binary not found in PATH\n" +
			"Install Claude Code: https://docs.anthropic.com/en/docs/claude-code/quickstart")
	}

	// Determine the repo for namespaced session lookup by scanning the worktree root.
	repo := repoFromWorktree(worktreeDir)

	// Build the claude args
	claudeArgs := []string{"claude"}
	sessionID := engine.ReadSessionID(ws, repo, issueNumber, *stageName)
	if sessionID != "" {
		claudeArgs = append(claudeArgs, "--resume", sessionID)
	}
	if stage.Model != "" {
		claudeArgs = append(claudeArgs, "--model", stage.Model)
	}
	if *pluginDir != "" {
		claudeArgs = append(claudeArgs, "--plugin-dir", *pluginDir)
	}

	// Change to the worktree directory before exec
	if err := os.Chdir(worktreeDir); err != nil {
		return fmt.Errorf("resume: chdir to %s: %w", worktreeDir, err)
	}

	// Replace the current process with claude
	return execFn(claudeBin, claudeArgs, os.Environ())
}

// findWorktreeDir returns the worktree directory of an issue under worktreeRoot:
// the flat <root>/issue-N when it exists, else <root>/<owner>-<repo>/issue-N for
// the first per-repo directory that has one. When none exists it returns the
// flat path, so the "not found" message names a concrete location. Fabrik has
// namespaced worktrees by repo for a long time; the flat-only lookup this
// replaces never found them (#2470).
func findWorktreeDir(worktreeRoot string, issueNumber int) string {
	name := fmt.Sprintf("issue-%d", issueNumber)
	flat := filepath.Join(worktreeRoot, name)
	if _, err := os.Stat(flat); err == nil {
		return flat
	}
	if entries, err := os.ReadDir(worktreeRoot); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			cand := filepath.Join(worktreeRoot, e.Name(), name)
			if fi, err := os.Stat(cand); err == nil && fi.IsDir() {
				return cand
			}
		}
	}
	return flat
}

// worktreeMatches returns every worktree directory of an issue under
// worktreeRoot: the flat <root>/issue-N and each <root>/<owner>-<repo>/issue-N.
// Issue numbers are per repo, so more than one match means the caller must be
// told which repo is meant rather than be handed the first by directory order.
func worktreeMatches(worktreeRoot string, issueNumber int) []string {
	name := fmt.Sprintf("issue-%d", issueNumber)
	var out []string
	if fi, err := os.Stat(filepath.Join(worktreeRoot, name)); err == nil && fi.IsDir() {
		out = append(out, filepath.Join(worktreeRoot, name))
	}
	entries, err := os.ReadDir(worktreeRoot)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		cand := filepath.Join(worktreeRoot, e.Name(), name)
		if fi, err := os.Stat(cand); err == nil && fi.IsDir() {
			out = append(out, cand)
		}
	}
	return out
}

// repoFromWorktree reads the git remote origin URL from dir and returns
// "owner/repo", or "" if dir doesn't exist or the remote can't be read.
func repoFromWorktree(dir string) string {
	if _, err := os.Stat(dir); err != nil {
		return ""
	}
	cmd := exec.Command("git", "remote", "get-url", "origin")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return parseOwnerRepoFromURL(strings.TrimSpace(string(out)))
}

// parseOwnerRepoFromURL parses a git remote URL and returns "owner/repo".
// Handles both HTTPS and SSH formats. Returns "" if parsing fails.
func parseOwnerRepoFromURL(remoteURL string) string {
	u := strings.TrimSuffix(remoteURL, ".git")
	// Normalize SSH format: git@github.com:owner/repo → owner/repo
	if colonIdx := strings.LastIndex(u, ":"); colonIdx >= 0 {
		if slashIdx := strings.Index(u, "/"); slashIdx < 0 || slashIdx > colonIdx {
			u = u[colonIdx+1:]
		}
	}
	parts := strings.Split(u, "/")
	if len(parts) < 2 {
		return ""
	}
	owner := parts[len(parts)-2]
	repo := parts[len(parts)-1]
	if owner == "" || repo == "" {
		return ""
	}
	return owner + "/" + repo
}
