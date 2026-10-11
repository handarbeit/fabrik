package engine

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/handarbeit/fabrik/plugin"
)

// pluginManifestRel is the manifest path inside a Claude Code plugin directory.
const pluginManifestRel = ".claude-plugin/plugin.json"

// resolvePluginDirs builds the ordered list of plugin directories handed to
// every worker (ADR 2136). Index 0 is the base plugin when one exists: the
// override (--plugin-dir) if set, otherwise <fabrikDir>/.fabrik/plugin if it
// exists. <fabrikDir>/.fabrik/plugins/local-workflows follows it automatically
// when it exists (#2139, FR-007 — where `fabrik upgrade` moves a customised
// plugin), then the configured extras in order.
//
// Extras are resolved against fabrikDir when relative, cleaned, and
// de-duplicated by path (a repeat, or an extra equal to the base, is dropped
// silently). Each extra must be a directory with a readable manifest carrying
// a non-empty name. Two loaded plugins declaring the same manifest name are
// refused, naming both paths. A base whose manifest is missing or unreadable
// has an unknown name and is skipped in the name check, so a bare-directory
// development override keeps working.
func resolvePluginDirs(fabrikDir, override string, extras []string) ([]string, error) {
	var dirs []string

	base := override
	if base == "" {
		defaultDir := filepath.Join(fabrikDir, ".fabrik", "plugin")
		if fi, err := os.Stat(defaultDir); err == nil && fi.IsDir() {
			base = defaultDir
		}
	}
	seen := map[string]bool{}
	names := map[string]string{} // manifest name -> directory
	if base != "" {
		if abs, err := filepath.Abs(base); err == nil {
			base = abs
		}
		dirs = append(dirs, base)
		seen[filepath.Clean(base)] = true
		if name, err := readPluginManifestName(base); err == nil {
			names[name] = base
		}
	}

	// local-workflows is loaded like a configured extra, ahead of them, so the
	// same validation (readable manifest, unique name, de-duplication by path)
	// applies. An unreadable manifest is an error, not a silent skip.
	all := extras
	if lw := filepath.Join(fabrikDir, filepath.FromSlash(plugin.LocalWorkflowsRelDir)); isDir(lw) {
		all = append([]string{lw}, extras...)
	}
	for _, extra := range all {
		if extra == "" {
			continue
		}
		dir := extra
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(fabrikDir, dir)
		}
		dir = filepath.Clean(dir)
		if seen[dir] {
			continue
		}
		seen[dir] = true

		fi, err := os.Stat(dir)
		if err != nil {
			return nil, fmt.Errorf("additional plugin directory %s: %w", dir, err)
		}
		if !fi.IsDir() {
			return nil, fmt.Errorf("additional plugin directory %s: not a directory", dir)
		}
		name, err := readPluginManifestName(dir)
		if err != nil {
			return nil, fmt.Errorf("additional plugin directory %s: %w", dir, err)
		}
		if other, dup := names[name]; dup {
			return nil, fmt.Errorf("plugins %s and %s both declare the manifest name %q; plugin names must be unique", other, dir, name)
		}
		names[name] = dir
		dirs = append(dirs, dir)
	}
	return dirs, nil
}

// readPluginManifestName returns the "name" declared by dir's plugin manifest.
func readPluginManifestName(dir string) (string, error) {
	path := filepath.Join(dir, pluginManifestRel)
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading plugin manifest %s: %w", path, err)
	}
	var m struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return "", fmt.Errorf("parsing plugin manifest %s: %w", path, err)
	}
	if m.Name == "" {
		return "", fmt.Errorf("plugin manifest %s has no name", path)
	}
	return m.Name, nil
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}
