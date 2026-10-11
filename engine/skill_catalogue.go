package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// loadedPlugin is one plugin directory handed to workers via --plugin-dir,
// with the skills Claude Code would register for it (ADR 2137).
type loadedPlugin struct {
	Name   string            // manifest name, else the directory basename
	Dir    string            // absolute plugin directory
	Skills map[string]string // skill name -> SKILL.md path
}

// skillCatalogue is an immutable snapshot of the loaded plugins, in load order
// (base first, then plugin_dirs extras). Never mutated after construction, so
// it is safe to share between worker goroutines.
type skillCatalogue struct {
	plugins []loadedPlugin
}

// buildSkillCatalogue reads each plugin directory from disk. A directory that
// cannot be read yields a plugin with no skills rather than an error: the
// resolver then reports the reference as unresolved, naming the path.
func buildSkillCatalogue(dirs []string) *skillCatalogue {
	c := &skillCatalogue{}
	for _, dir := range dirs {
		c.plugins = append(c.plugins, discoverPlugin(dir))
	}
	return c
}

func (c *skillCatalogue) empty() bool { return c == nil || len(c.plugins) == 0 }

func (c *skillCatalogue) plugin(name string) (loadedPlugin, bool) {
	for _, p := range c.plugins {
		if p.Name == name {
			return p, true
		}
	}
	return loadedPlugin{}, false
}

// discoverPlugin mirrors Claude Code's skill discovery:
//   - skills/<name>/SKILL.md is always scanned;
//   - the manifest "skills" key (string or array) adds directories, each either
//     a directory of <name>/SKILL.md folders or a single folder holding
//     SKILL.md directly ("." is the plugin root);
//   - a root SKILL.md with no skills/ and no "skills" key is a single skill.
//
// The skill identity is the directory name; the commands key and commands/ are
// not skills. A plugin with no manifest is named after its directory.
func discoverPlugin(dir string) loadedPlugin {
	p := loadedPlugin{Name: filepath.Base(dir), Dir: dir, Skills: map[string]string{}}
	var manifest struct {
		Name   string          `json:"name"`
		Skills json.RawMessage `json:"skills"`
	}
	hasSkillsKey := false
	if data, err := os.ReadFile(filepath.Join(dir, pluginManifestRel)); err == nil {
		if json.Unmarshal(data, &manifest) == nil {
			if manifest.Name != "" {
				p.Name = manifest.Name
			}
			hasSkillsKey = len(manifest.Skills) > 0 && string(manifest.Skills) != "null"
		}
	}

	scanSkillsDir(p, filepath.Join(dir, "skills"))

	if hasSkillsKey {
		var paths []string
		var one string
		if json.Unmarshal(manifest.Skills, &one) == nil {
			paths = []string{one}
		} else {
			_ = json.Unmarshal(manifest.Skills, &paths) // malformed: no extra paths
		}
		for _, rel := range paths {
			target := filepath.Join(dir, filepath.FromSlash(strings.TrimPrefix(rel, "./")))
			if !strings.HasPrefix(filepath.Clean(target), filepath.Clean(dir)) {
				continue // outside the plugin root
			}
			if fi, err := os.Stat(filepath.Join(target, "SKILL.md")); err == nil && !fi.IsDir() {
				name := filepath.Base(target)
				if filepath.Clean(target) == filepath.Clean(dir) {
					name = p.Name
				}
				p.Skills[name] = filepath.Join(target, "SKILL.md")
				continue
			}
			scanSkillsDir(p, target)
		}
	} else if len(p.Skills) == 0 {
		if _, err := os.Stat(filepath.Join(dir, "skills")); err != nil {
			if fi, err := os.Stat(filepath.Join(dir, "SKILL.md")); err == nil && !fi.IsDir() {
				p.Skills[p.Name] = filepath.Join(dir, "SKILL.md")
			}
		}
	}
	return p
}

func scanSkillsDir(p loadedPlugin, dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		skill := filepath.Join(dir, e.Name(), "SKILL.md")
		if fi, err := os.Stat(skill); err == nil && !fi.IsDir() {
			p.Skills[e.Name()] = skill
		}
	}
}

// skillNames returns the plugin's skill names, sorted.
func (p loadedPlugin) skillNames() []string {
	names := make([]string, 0, len(p.Skills))
	for n := range p.Skills {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
