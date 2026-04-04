package cmd

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	fabrikplugin "github.com/verveguy/fabrik/plugin"
)

// runUpgrade implements the `fabrik upgrade` subcommand.
// It refreshes the plugin skills in .fabrik/plugin/ from the embedded defaults,
// always overwriting existing files. Does not touch stages or config.yaml.
func runUpgrade(args []string) error {
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
