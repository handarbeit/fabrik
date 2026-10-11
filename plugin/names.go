package plugin

import "strings"

const (
	// NamespaceSep separates a plugin name from a component name in the form
	// Claude Code registers plugin skills under: "<plugin>:<skill>" (ADR 2137).
	NamespaceSep = ":"

	// BasePluginName is the manifest name of Fabrik's own workflow plugin
	// (plugin/fabrik-workflows/.claude-plugin/plugin.json). It is the one place
	// the name is defined in Go; the shipped stage defaults qualify their skills
	// with it and the skill resolver prefers it for bare references.
	BasePluginName = "fabrik"

	// LocalWorkflowsName is the manifest name of the plugin `fabrik upgrade`
	// moves an operator-customised workflow plugin into (#2139, ADR 2139).
	LocalWorkflowsName = "local-workflows"

	// LocalWorkflowsRelDir is where that plugin lives, relative to the Fabrik
	// directory; the engine loads it automatically when it exists (FR-007).
	LocalWorkflowsRelDir = ".fabrik/plugins/local-workflows"
)

// Qualify returns the fully-qualified "<plugin>:<skill>" name.
func Qualify(pluginName, skill string) string {
	return pluginName + NamespaceSep + skill
}

// Split splits a skill reference on the first NamespaceSep. A bare reference
// (no separator) returns qualified=false and the whole input as skill.
func Split(ref string) (pluginName, skill string, qualified bool) {
	i := strings.Index(ref, NamespaceSep)
	if i < 0 {
		return "", ref, false
	}
	return ref[:i], ref[i+len(NamespaceSep):], true
}
