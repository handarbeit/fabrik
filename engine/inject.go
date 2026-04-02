package engine

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/verveguy/fabrik/stages"
)

// injectClaudeArtifacts copies .claude/ artifacts from mainRepoDir into the
// worktree so Claude Code can access skills, agents, and rules during the session.
// Copies .claude/skills/, .claude/agents/, and .claude/rules/ subdirectories.
// Overwrites on each invocation to ensure freshness.
// Gracefully skips subdirectories that don't exist in the source.
// Also generates a stage-specific settings.json with tool permissions.
func injectClaudeArtifacts(worktreeDir, mainRepoDir string, stage *stages.Stage) error {
	claudeSrc := filepath.Join(mainRepoDir, ".claude")
	claudeDst := filepath.Join(worktreeDir, ".claude")

	// Copy standard subdirectories
	for _, subdir := range []string{"skills", "agents", "rules"} {
		src := filepath.Join(claudeSrc, subdir)
		if _, err := os.Stat(src); os.IsNotExist(err) {
			continue // graceful skip
		}
		dst := filepath.Join(claudeDst, subdir)
		if err := copyDir(src, dst); err != nil {
			return fmt.Errorf("copying .claude/%s: %w", subdir, err)
		}
	}

	// Generate stage-specific settings.json
	if err := generateSettingsJSON(stage, claudeDst); err != nil {
		return fmt.Errorf("generating settings.json: %w", err)
	}

	return nil
}

// generateSettingsJSON writes a settings.json in the worktree's .claude/ dir
// with tool permissions derived from the stage config.
func generateSettingsJSON(stage *stages.Stage, claudeDir string) error {
	if err := os.MkdirAll(claudeDir, 0755); err != nil {
		return fmt.Errorf("creating .claude dir: %w", err)
	}

	// Base allow list for git/gh operations always present
	allow := []string{
		"Bash(git *)",
		"Bash(gh *)",
		"Bash(go build*)",
		"Bash(go test*)",
		"Bash(go vet*)",
		"Bash(go fmt*)",
	}
	// Append stage-specific allowed tools (deduplication not required — Claude handles it)
	allow = append(allow, stage.AllowedTools...)

	settings := map[string]any{
		"permissions": map[string]any{
			"allow": allow,
		},
	}

	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling settings: %w", err)
	}

	path := filepath.Join(claudeDir, "settings.json")
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("writing settings.json: %w", err)
	}
	return nil
}

// copyDir recursively copies src directory to dst, overwriting existing files.
func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)

		if d.IsDir() {
			return os.MkdirAll(target, 0755)
		}

		return copyFile(path, target)
	})
}

// copyFile copies a single file from src to dst, creating parent directories as needed.
func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return fmt.Errorf("creating parent dir: %w", err)
	}

	srcFile, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("opening source: %w", err)
	}
	defer srcFile.Close()

	info, err := srcFile.Stat()
	if err != nil {
		return fmt.Errorf("stat source: %w", err)
	}

	dstFile, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode())
	if err != nil {
		return fmt.Errorf("opening dest: %w", err)
	}
	defer dstFile.Close()

	if _, err := io.Copy(dstFile, srcFile); err != nil {
		return fmt.Errorf("copying: %w", err)
	}
	return nil
}
