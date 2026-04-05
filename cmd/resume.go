package cmd

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/handarbeit/fabrik/engine"
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

	if err := fset.Parse(args); err != nil {
		return err
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

	if fset.NArg() != 1 {
		return fmt.Errorf("resume: expected exactly one positional argument: <issue-number>\nUsage: fabrik resume <issue-number> [--stage <name>]")
	}

	issueNumber, err := strconv.Atoi(fset.Arg(0))
	if err != nil || issueNumber <= 0 {
		return fmt.Errorf("resume: issue number must be a positive integer, got %q", fset.Arg(0))
	}

	if *stageName == "" {
		return fmt.Errorf("resume: --stage <name> is required (stage auto-detection requires #109 which is not yet implemented)")
	}

	// Locate the worktree
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resume: getting working directory: %w", err)
	}
	worktreeDir := filepath.Join(cwd, ".fabrik", "worktrees", fmt.Sprintf("issue-%d", issueNumber))
	if _, err := os.Stat(worktreeDir); err != nil {
		return fmt.Errorf("resume: worktree for issue #%d not found at %s\n"+
			"The issue has not been processed yet, or the worktree was cleaned up.", issueNumber, worktreeDir)
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

	// Build the claude args
	claudeArgs := []string{"claude"}
	sessionID := engine.ReadSessionID(issueNumber, *stageName)
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
