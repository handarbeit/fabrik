package skillref

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ManifestRel is the manifest path inside a Claude Code plugin directory.
const ManifestRel = ".claude-plugin/plugin.json"

// Plugin is one plugin directory handed to workers via --plugin-dir, with the
// skills Claude Code would register for it (ADR 2137).
type Plugin struct {
	Name   string            // manifest name, else the directory basename
	Dir    string            // absolute plugin directory
	Skills map[string]string // skill name -> SKILL.md path
}

// Catalogue is an immutable snapshot of the loaded plugins, in load order
// (base first, then extras). Never mutated after construction, so it is safe to
// share between goroutines.
type Catalogue struct {
	plugins []Plugin
}

// Build reads each plugin directory from disk. A directory that cannot be read
// yields a plugin with no skills rather than an error: the resolver then
// reports the reference as unresolved, naming the path.
func Build(dirs []string) *Catalogue {
	c := &Catalogue{}
	for _, dir := range dirs {
		c.plugins = append(c.plugins, Discover(dir))
	}
	return c
}

// Empty reports whether the catalogue holds no plugin directories. Nil-safe.
func (c *Catalogue) Empty() bool { return c == nil || len(c.plugins) == 0 }

// Plugin returns the loaded plugin with the given name.
func (c *Catalogue) Plugin(name string) (Plugin, bool) {
	if c == nil {
		return Plugin{}, false
	}
	for _, p := range c.plugins {
		if p.Name == name {
			return p, true
		}
	}
	return Plugin{}, false
}

// Describe lists the searched plugins and their paths. Nil-safe.
func (c *Catalogue) Describe() string {
	if c.Empty() {
		return "(none)"
	}
	parts := make([]string, 0, len(c.plugins))
	for _, p := range c.plugins {
		parts = append(parts, fmt.Sprintf("%s (%s)", p.Name, p.Dir))
	}
	return strings.Join(parts, ", ")
}

// Discover mirrors Claude Code's skill discovery:
//   - skills/<name>/SKILL.md is always scanned;
//   - the manifest "skills" key (string or array) adds directories, each either
//     a directory of <name>/SKILL.md folders or a single folder holding
//     SKILL.md directly ("." is the plugin root);
//   - a root SKILL.md with no skills/ and no "skills" key is a single skill.
//
// The skill identity is the directory name; the commands key and commands/ are
// not skills. A plugin with no manifest is named after its directory.
func Discover(dir string) Plugin {
	p := Plugin{Name: filepath.Base(dir), Dir: dir, Skills: map[string]string{}}
	var manifest struct {
		Name   string          `json:"name"`
		Skills json.RawMessage `json:"skills"`
	}
	hasSkillsKey := false
	if data, err := os.ReadFile(filepath.Join(dir, ManifestRel)); err == nil {
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
			if rel, err := filepath.Rel(dir, target); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
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

func scanSkillsDir(p Plugin, dir string) {
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

// SkillNames returns the plugin's skill names, sorted.
func (p Plugin) SkillNames() []string {
	names := make([]string, 0, len(p.Skills))
	for n := range p.Skills {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
