package plugin

import "strings"

const (
	// NamespaceSep separates a plugin name from a component name in the form
	// Claude Code registers plugin skills under: "<plugin>:<skill>" (ADR 2137).
	NamespaceSep = ":"

	// BasePluginName is the manifest name of Fabrik's own workflow plugin
	// (plugin/fabrik-workflows/.claude-plugin/plugin.json). It is the one place
	// the name is defined in Go; the shipped stage defaults qualify their skills
	// with it and the skill resolver prefers it for bare references. Renamed
	// from "fabrik" by #2140 (ADR 2140) to free that name for the PM plugin.
	BasePluginName = "fabrik-stages"

	// LegacyBasePluginName is the name the base plugin carried before #2140. It
	// survives in deployments that have not yet run `fabrik upgrade`, in
	// operator-edited copies of the old plugin, and in stage references.
	LegacyBasePluginName = "fabrik"

	// PMPluginID is the marketplace identity of the human-facing PM plugin
	// (plugin/fabrik, published through .claude-plugin/marketplace.json). Every
	// worker invocation disables it (FR-005): it must never run in a worker.
	PMPluginID = "fabrik@fabrik"

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

// legacySkillPrefix is the prefix every pre-#2140 base skill carried; #2140
// dropped it (fabrik-review -> review).
const legacySkillPrefix = "fabrik-"

// legacySkillNames are the twelve skills the base plugin shipped before #2140,
// by their old directory name.
var legacySkillNames = []string{
	"fabrik-specify", "fabrik-specify-comment",
	"fabrik-research", "fabrik-research-comment",
	"fabrik-plan", "fabrik-plan-comment",
	"fabrik-implement", "fabrik-implement-comment",
	"fabrik-review", "fabrik-review-comment",
	"fabrik-validate", "fabrik-validate-comment",
}

// LegacySkillNames returns the pre-#2140 base skill names (a copy).
func LegacySkillNames() []string {
	return append([]string(nil), legacySkillNames...)
}

// LegacySkillTarget maps a pre-#2140 base skill name (fabrik-review) to its
// current name (review). ok is false for any other name.
func LegacySkillTarget(skill string) (newSkill string, ok bool) {
	for _, n := range legacySkillNames {
		if n == skill {
			return strings.TrimPrefix(n, legacySkillPrefix), true
		}
	}
	return "", false
}

// LegacyRef maps a pre-#2140 reference to a base skill, either the qualified
// "fabrik:fabrik-X" or the bare "fabrik-X", to its current fully-qualified name
// "fabrik-stages:X". It maps nothing else: references to local-workflows or to
// any other plugin, and names that are not one of the twelve shipped skills,
// return ok=false. This is the one mapping the runtime resolver (FR-004) and
// `fabrik upgrade` (FR-003) both use, so the two cannot disagree.
func LegacyRef(ref string) (newRef string, ok bool) {
	pluginName, skill, qualified := Split(ref)
	if qualified && pluginName != LegacyBasePluginName {
		return "", false
	}
	target, ok := LegacySkillTarget(skill)
	if !ok {
		return "", false
	}
	return Qualify(BasePluginName, target), true
}
