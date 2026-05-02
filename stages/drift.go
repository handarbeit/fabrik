package stages

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// WarnStageDrift compares each user stage against the embedded default with the
// same name. If the embedded default contains top-level YAML keys that the user's
// file does not, a warning is written to w naming the missing keys. Custom stages
// (no matching embedded default) are silently skipped. Empty userStages is a no-op.
func WarnStageDrift(userStages []*Stage, version string, w io.Writer) {
	warnDriftFrom(userStages, version, w, DefaultStages)
}

// warnDriftFrom is the testable core of WarnStageDrift. It accepts any fs.FS
// containing YAML stage files under "examples/", so tests can inject a synthetic FS.
func warnDriftFrom(userStages []*Stage, version string, w io.Writer, defaults fs.FS) {
	if len(userStages) == 0 {
		return
	}

	// Index user stages by name for fast lookup.
	byName := make(map[string]*Stage, len(userStages))
	for _, s := range userStages {
		byName[s.Name] = s
	}

	// Walk embedded defaults (best-effort; individual entry errors skip that entry).
	fs.WalkDir(defaults, "examples", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries rather than aborting the walk
		}
		if d.IsDir() {
			return nil
		}
		ext := filepath.Ext(d.Name())
		if ext != ".yaml" && ext != ".yml" {
			return nil
		}

		data, err := defaults.Open(path)
		if err != nil {
			return nil // best-effort; skip unreadable embedded file
		}
		defer data.Close()

		var buf []byte
		buf, err = io.ReadAll(data)
		if err != nil {
			return nil
		}

		var defaultMap map[string]any
		if err := yaml.Unmarshal(buf, &defaultMap); err != nil {
			return nil
		}

		name, _ := defaultMap["name"].(string)
		if name == "" {
			return nil
		}

		userStage, ok := byName[name]
		if !ok {
			return nil // custom stage — skip
		}
		if userStage.FilePath == "" {
			return nil // in-memory stage (e.g. tests) — skip
		}

		defaultKeys := make(map[string]bool, len(defaultMap))
		for k := range defaultMap {
			defaultKeys[k] = true
		}

		missing, err := MissingTopLevelKeys(userStage.FilePath, defaultKeys)
		if err != nil || len(missing) == 0 {
			return nil
		}

		fmt.Fprintf(w, "[startup] warning: %s is missing fields present in %s defaults: %s. Run `fabrik refresh-stages --apply` to add the missing keys.\n",
			userStage.FilePath, version, strings.Join(missing, ", "))
		return nil
	})
}

// MissingTopLevelKeys reads the YAML file at userPath and returns a sorted slice
// of keys from defaultKeys that are absent from the user file's top-level map.
func MissingTopLevelKeys(userPath string, defaultKeys map[string]bool) ([]string, error) {
	data, err := os.ReadFile(userPath)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", userPath, err)
	}

	var userMap map[string]any
	if err := yaml.Unmarshal(data, &userMap); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", userPath, err)
	}

	var missing []string
	for k := range defaultKeys {
		if _, ok := userMap[k]; !ok {
			missing = append(missing, k)
		}
	}
	sort.Strings(missing)
	return missing, nil
}
