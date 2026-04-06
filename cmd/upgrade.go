package cmd

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

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

	wrote, err := refreshPlugin()
	if err != nil {
		return err
	}
	fmt.Printf("fabrik upgrade: refreshed %d plugin file(s)\n", wrote)
	return nil
}

// refreshPlugin overwrites .fabrik/plugin/ with the embedded plugin files.
// Returns the number of files written.
func refreshPlugin() (int, error) {
	pluginDir := ".fabrik/plugin"
	wrote := 0
	err := fs.WalkDir(fabrikplugin.FabrikPlugin, "fabrik-plugin", func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, _ := filepath.Rel("fabrik-plugin", p)
		destPath := filepath.Join(pluginDir, rel)

		if d.IsDir() {
			return os.MkdirAll(destPath, 0755)
		}

		data, readErr := fabrikplugin.FabrikPlugin.ReadFile(p)
		if readErr != nil {
			return fmt.Errorf("reading embedded %s: %w", p, readErr)
		}
		if mkErr := os.MkdirAll(filepath.Dir(destPath), 0755); mkErr != nil {
			return fmt.Errorf("creating directory for %s: %w", destPath, mkErr)
		}
		if writeErr := os.WriteFile(destPath, data, 0644); writeErr != nil {
			return fmt.Errorf("writing %s: %w", destPath, writeErr)
		}
		wrote++
		return nil
	})
	return wrote, err
}
