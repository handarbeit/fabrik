package engine

import "github.com/handarbeit/fabrik/plugin"

// PluginInfo describes one plugin directory the way the skill resolver sees it.
type PluginInfo struct {
	Name   string   // manifest name, else the directory basename
	Dir    string   // the plugin directory as given
	Skills []string // skill names Claude Code would register, sorted
}

// DescribePlugin reports the manifest name and skill set of the plugin in dir
// using the resolver's own discovery (ADR 2137), so `fabrik upgrade` qualifies a
// reference only to a plugin the workers would actually load it from (#2203).
// A directory that cannot be read yields a plugin with no skills.
func DescribePlugin(dir string) PluginInfo {
	p := discoverPlugin(dir)
	return PluginInfo{Name: p.Name, Dir: p.Dir, Skills: p.skillNames()}
}

// ResolveSkillForTest resolves ref against the plugins in dirs (load order) with
// the production resolver and returns the qualified name and the SKILL.md path
// of the chosen provider ("" when unresolved).
func ResolveSkillForTest(dirs []string, ref string) (qualified, skillFile string, ok bool) {
	res, _, cat, ok := newSkillResolver(dirs).Resolve(ref)
	if !ok {
		return "", "", false
	}
	if pn, skill, q := plugin.Split(res.Name); q {
		if p, found := cat.plugin(pn); found {
			return res.Name, p.Skills[skill], true
		}
	}
	return res.Name, "", true
}
