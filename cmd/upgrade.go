package cmd

import (
	"bufio"
	"crypto/sha256"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/mattn/go-isatty"
	"github.com/handarbeit/fabrik/config"
	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	fabrikplugin "github.com/handarbeit/fabrik/plugin"
)

// upgradeGitHubClient is the GitHub client used by runUpgrade. It can be
// replaced in tests to avoid real network calls.
var upgradeGitHubClient engine.GitHubClient

// runUpgrade implements the `fabrik upgrade` subcommand.
// For release builds it first checks for a newer binary, downloads and
// atomically replaces it if found, then re-execs so the new binary's embedded
// skills are extracted. Dev builds skip the binary check and only refresh
// plugin skills from the current binary's embedded defaults.
func runUpgrade(args []string) error {
	fset := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	fset.Usage = func() {
		fmt.Fprintf(fset.Output(), "Usage: fabrik upgrade\n\n")
		fmt.Fprintf(fset.Output(), "Upgrade the Fabrik binary and refresh embedded plugin skills.\n\n")
		fmt.Fprintf(fset.Output(), "For release builds: downloads the latest binary from GitHub Releases and\n")
		fmt.Fprintf(fset.Output(), "re-execs with the updated binary. For dev builds (built from source):\n")
		fmt.Fprintf(fset.Output(), "rebuilds from origin/main rather than downloading a release binary.\n")
		fmt.Fprintf(fset.Output(), "In both cases, embedded plugin skills are refreshed on disk.\n")
	}
	if err := fset.Parse(args); err != nil {
		return err
	}

	if !strings.HasPrefix(Version, "dev") {
		token := config.Token()
		client := upgradeGitHubClient
		if client == nil {
			client = gh.NewClient(token)
		}
		engine.PerformReleaseUpgrade(client, Version, token, nil, func(format string, args ...any) {
			fmt.Printf(format, args...)
		})
	}

	wrote, err := fabrikplugin.RefreshPlugin()
	if err != nil {
		return err
	}
	fmt.Printf("fabrik upgrade: refreshed %d plugin file(s)\n", wrote)
	return nil
}

// checkPluginSkills compares embedded plugin files against on-disk files in
// pluginDir. If any differ and stdin is a TTY, the user is prompted to upgrade.
// In non-interactive mode a warning is printed to stderr and execution continues.
// Returns nil if the directory does not exist (no fabrik init done yet).
func checkPluginSkills(pluginDir string) error {
	isTTY := isatty.IsTerminal(os.Stdin.Fd()) || isatty.IsCygwinTerminal(os.Stdin.Fd())
	return checkPluginSkillsWithReader(pluginDir, isTTY, os.Stdin)
}

// checkPluginSkillsWithReader is the testable implementation of checkPluginSkills.
// isTTY and r control interactive-prompt behaviour without requiring a real PTY.
func checkPluginSkillsWithReader(pluginDir string, isTTY bool, r io.Reader) error {
	if _, err := os.Stat(pluginDir); os.IsNotExist(err) {
		return nil
	}

	diffing, err := diffingPluginFiles(pluginDir)
	if err != nil {
		return fmt.Errorf("checking plugin files: %w", err)
	}
	if len(diffing) == 0 {
		return nil
	}

	if !isTTY {
		// Non-interactive (headless daemon, auto-upgrade re-exec, CI): refresh
		// silently so dev builds and auto-upgraded builds always have matching
		// plugin skills without manual intervention.
		for _, rel := range diffing {
			embeddedPath := filepath.Join("fabrik-plugin", rel)
			data, readErr := fabrikplugin.FabrikPlugin.ReadFile(embeddedPath)
			if readErr != nil {
				return fmt.Errorf("reading embedded %s: %w", embeddedPath, readErr)
			}
			destPath := filepath.Join(pluginDir, rel)
			if mkErr := os.MkdirAll(filepath.Dir(destPath), 0755); mkErr != nil {
				return fmt.Errorf("creating directory for %s: %w", destPath, mkErr)
			}
			if writeErr := os.WriteFile(destPath, data, 0644); writeErr != nil {
				return fmt.Errorf("writing %s: %w", destPath, writeErr)
			}
		}
		fmt.Fprintf(os.Stderr, "fabrik: auto-refreshed %d plugin file(s)\n", len(diffing))
		return nil
	}

	fmt.Printf("Plugin skills on disk don't match this version of fabrik. Do you want to upgrade them? [y/N] ")
	reader := bufio.NewReader(r)
	line, _ := reader.ReadString('\n')
	answer := strings.TrimSpace(line)
	if strings.ToLower(answer) != "y" && strings.ToLower(answer) != "yes" {
		return nil
	}

	for _, rel := range diffing {
		embeddedPath := filepath.Join("fabrik-plugin", rel)
		data, readErr := fabrikplugin.FabrikPlugin.ReadFile(embeddedPath)
		if readErr != nil {
			return fmt.Errorf("reading embedded %s: %w", embeddedPath, readErr)
		}
		destPath := filepath.Join(pluginDir, rel)
		if mkErr := os.MkdirAll(filepath.Dir(destPath), 0755); mkErr != nil {
			return fmt.Errorf("creating directory for %s: %w", destPath, mkErr)
		}
		if writeErr := os.WriteFile(destPath, data, 0644); writeErr != nil {
			return fmt.Errorf("writing %s: %w", destPath, writeErr)
		}
	}
	fmt.Printf("fabrik: upgraded %d plugin file(s)\n", len(diffing))
	return nil
}

// diffingPluginFiles walks the embedded FabrikPlugin FS and returns the relative
// paths (from the fabrik-plugin/ root) of files whose SHA256 differs from the
// on-disk counterpart in pluginDir. Missing on-disk files count as differing.
func diffingPluginFiles(pluginDir string) ([]string, error) {
	var diffing []string
	err := fs.WalkDir(fabrikplugin.FabrikPlugin, "fabrik-plugin", func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel("fabrik-plugin", p)
		embeddedData, readErr := fabrikplugin.FabrikPlugin.ReadFile(p)
		if readErr != nil {
			return fmt.Errorf("reading embedded %s: %w", p, readErr)
		}
		embeddedSum := sha256.Sum256(embeddedData)

		diskPath := filepath.Join(pluginDir, rel)
		diskData, diskErr := os.ReadFile(diskPath)
		if os.IsNotExist(diskErr) {
			diffing = append(diffing, rel)
			return nil
		}
		if diskErr != nil {
			return fmt.Errorf("reading %s: %w", diskPath, diskErr)
		}
		diskSum := sha256.Sum256(diskData)
		if embeddedSum != diskSum {
			diffing = append(diffing, rel)
		}
		return nil
	})
	return diffing, err
}
