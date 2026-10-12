package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/handarbeit/fabrik/engine"
	fabrikplugin "github.com/handarbeit/fabrik/plugin"
)

// The project-plugin set the workflow migration consults (#2203, ADR 2140
// decision 4 amendment). A bare skill reference is resolved by the daemon at
// runtime against every loaded plugin; `fabrik upgrade` used to know only the
// base tree and local-workflows, so a bare fabrik-<stage> that a configured
// extra plugin provides was silently redirected to the base plugin. The set here
// is the plugins the daemon would load beyond the base plugin and the copy of it
// the migration itself moves: the operator's own local-workflows and the
// plugin_dirs extras, in load order, so the first provider is the one the
// runtime resolver picks (ADR 2137).

// projectPluginLookupDisabledForTest restores the always-base rewrite so a test
// can show the FR-002 assertion is not vacuous.
var projectPluginLookupDisabledForTest bool

type projectPlugin struct {
	Name   string
	Dir    string
	Skills map[string]bool
}

// projectPlugins is an ordered set of project plugins.
type projectPlugins struct {
	plugins []projectPlugin
}

// providers returns the plugins that provide skill, in load order.
func (s *projectPlugins) providers(skill string) []projectPlugin {
	if s == nil || projectPluginLookupDisabledForTest {
		return nil
	}
	var out []projectPlugin
	for _, p := range s.plugins {
		if p.Skills[skill] {
			out = append(out, p)
		}
	}
	return out
}

// qualify returns the reference qualified to the first project plugin that
// provides skill, and a report note naming it and any other providers.
func (s *projectPlugins) qualify(rel, key, skill string) (newRef, note string, ok bool) {
	ps := s.providers(skill)
	if len(ps) == 0 {
		return "", "", false
	}
	newRef = fabrikplugin.Qualify(ps[0].Name, skill)
	note = fmt.Sprintf("%s: %s %q was qualified to %q, the project plugin %q (%s) that provides it, as the runtime resolver would pick it", rel, key, skill, newRef, ps[0].Name, ps[0].Dir)
	if len(ps) > 1 {
		var others []string
		for _, p := range ps[1:] {
			others = append(others, fabrikplugin.Qualify(p.Name, skill))
		}
		note += "; also provided by " + strings.Join(others, ", ")
	}
	return newRef, note, true
}

// projectPluginDirs lists the configured extra plugin directories the way the
// daemon does minus the --plugin-dirs flag, which `fabrik upgrade` does not have:
// FABRIK_PLUGIN_DIRS (comma-separated) wins over plugin_dirs in
// <dir>/.fabrik/config.yaml. A .env-only value is invisible here.
func projectPluginDirs(dir string) (dirs []string, notes []string) {
	if v := os.Getenv("FABRIK_PLUGIN_DIRS"); v != "" {
		return splitGlobList(v), nil
	}
	data, err := os.ReadFile(filepath.Join(dir, ".fabrik", "config.yaml"))
	if err != nil {
		if !os.IsNotExist(err) {
			notes = append(notes, fmt.Sprintf("could not read .fabrik/config.yaml (%v); configured plugin_dirs were not consulted, so references they provide fall back to the base plugin", err))
		}
		return nil, notes
	}
	var pc struct {
		PluginDirs []string `yaml:"plugin_dirs"`
	}
	if err := yaml.Unmarshal(data, &pc); err != nil {
		return nil, append(notes, fmt.Sprintf("could not parse .fabrik/config.yaml (%v); configured plugin_dirs were not consulted, so references they provide fall back to the base plugin", err))
	}
	for _, d := range pc.PluginDirs {
		if d = strings.TrimSpace(d); d != "" {
			dirs = append(dirs, d)
		}
	}
	return dirs, nil
}

// loadProjectPlugins builds the set for the deployment under dir. ownLocal is
// whether .fabrik/plugins/local-workflows is the operator's own plugin (a
// manifest naming something other than the base plugin or local-workflows); a
// moved copy of the base plugin is not a project plugin (its edited skills are
// handled by the existing local-workflows rule, its unedited ones belong to the
// base). Nothing here fails the upgrade: an extra that cannot be read is skipped
// with a note and references it might provide fall back to the base rewrite.
func loadProjectPlugins(dir string, ownLocal bool) (*projectPlugins, []string) {
	set := &projectPlugins{}
	extras, notes := projectPluginDirs(dir)

	seenDir := map[string]bool{
		filepath.Clean(filepath.Join(dir, ".fabrik", "plugin")): true,
	}
	lwDir := filepath.Clean(filepath.Join(dir, filepath.FromSlash(fabrikplugin.LocalWorkflowsRelDir)))
	seenDir[lwDir] = true
	seenName := map[string]string{}

	add := func(path string, isExtra bool) {
		info := engine.DescribePlugin(path)
		if _, err := readManifestName(path); err != nil {
			notes = append(notes, fmt.Sprintf("additional plugin %s has no readable manifest (%v); it was skipped, so references it might provide fall back to the base plugin", path, err))
			return
		}
		if isBasePluginName(info.Name) || (isExtra && info.Name == fabrikplugin.LocalWorkflowsName) {
			return // a base-plugin copy, never a project plugin
		}
		if prev, dup := seenName[info.Name]; dup {
			notes = append(notes, fmt.Sprintf("additional plugin %s repeats the plugin name %q already used by %s; it was skipped", path, info.Name, prev))
			return
		}
		seenName[info.Name] = path
		skills := map[string]bool{}
		for _, s := range info.Skills {
			skills[s] = true
		}
		set.plugins = append(set.plugins, projectPlugin{Name: info.Name, Dir: path, Skills: skills})
	}

	if ownLocal {
		add(lwDir, false)
	}
	for _, e := range extras {
		path := e
		if !filepath.IsAbs(path) {
			path = filepath.Join(dir, path)
		}
		path = filepath.Clean(path)
		if seenDir[path] {
			continue
		}
		seenDir[path] = true
		if !isDirPath(path) {
			notes = append(notes, fmt.Sprintf("additional plugin directory %s is not a readable directory; it was skipped, so references it might provide fall back to the base plugin", path))
			continue
		}
		add(path, true)
	}
	return set, notes
}
