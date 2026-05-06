package plugin

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// RefreshPlugin overwrites .fabrik/plugin/ with the embedded plugin files.
// Returns the number of files written.
func RefreshPlugin() (int, error) {
	pluginDir := ".fabrik/plugin"
	wrote := 0
	err := fs.WalkDir(FabrikPlugin, "fabrik-workflows", func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, _ := filepath.Rel("fabrik-workflows", p)
		destPath := filepath.Join(pluginDir, rel)

		if d.IsDir() {
			return os.MkdirAll(destPath, 0755)
		}

		data, readErr := FabrikPlugin.ReadFile(p)
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
