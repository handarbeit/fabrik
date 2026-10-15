package engine

import (
	"fmt"
	"io"
	"strings"

	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/warnings"
)

const (
	// skillDeprecatedType is the warnings-panel type of the once-per-startup
	// deprecated-reference notice (ADR 2137, FR-004; #2140 widened it from the
	// bare form to the pre-rename "fabrik:fabrik-*" form too).
	skillDeprecatedType = "skill_reference_deprecated"
	// skillUnresolvedType is the panel type of a reference no loaded plugin
	// provides (FR-005); the subject is "<stage>.<key>".
	skillUnresolvedType = "skill_reference"
)

// reportStageSkills checks every skill-valued key of every configured stage
// against the loaded plugins and reports the result to the log and the
// warnings panel: one deprecation notice listing all bare and pre-rename references, and one
// warning per unresolvable reference. Both are Reconciled, so a fixed
// configuration clears them. The check runs to completion before either
// Reconcile, as warnings.Reconcile requires.
func (e *Engine) reportStageSkills(stageList []*stages.Stage, w io.Writer) {
	if e.skillResolver == nil {
		return
	}
	var deprecatedRefs []string
	var unresolved []warnings.Condition
	for _, s := range stageList {
		for _, kv := range stageSkillRefs(s, nil) {
			res, why, cat, ok := e.skillResolver.Resolve(kv[1])
			if !ok {
				ue := &skillUnresolvedError{Stage: s.Name, Key: kv[0], Ref: kv[1], Why: why, Cat: cat}
				unresolved = append(unresolved, unresolvedCondition(ue))
				continue
			}
			if !res.Deprecated {
				continue
			}
			entry := fmt.Sprintf("%s.%s → %s", s.Name, kv[0], res.Name)
			if len(res.Others) > 0 {
				entry += fmt.Sprintf(" (also provided by %s; the base plugin or first loaded wins)", strings.Join(res.Others, ", "))
			}
			deprecatedRefs = append(deprecatedRefs, entry)
		}
	}

	var deprecated []warnings.Condition
	if len(deprecatedRefs) > 0 {
		list := strings.Join(deprecatedRefs, "; ")
		deprecated = append(deprecated, warnings.Condition{
			Subject: "deprecated",
			Level:   warnings.LevelNotice,
			Title:   fmt.Sprintf("%d stage skill reference(s) use a deprecated form", len(deprecatedRefs)),
			Detail: fmt.Sprintf("These references name a skill without its plugin (bare), or use the pre-rename form fabrik:fabrik-<stage> of the base plugin now called fabrik-stages, and were resolved against the loaded plugins: %s.\n\n"+
				"Fix: run `fabrik upgrade`, which rewrites them to <plugin>:<skill> (see docs/USER_GUIDE.md).", list),
			LogLine: fmt.Sprintf("stage skill references use a deprecated form — bare, or the pre-rename fabrik:fabrik-<stage> (resolved: %s) — run `fabrik upgrade` to rewrite them. See docs/USER_GUIDE.md.", list),
		})
	}
	if err := warnings.Reconcile(w, skillDeprecatedType, deprecated); err != nil {
		fmt.Fprintf(w, "[startup] recording %s failed: %v\n", skillDeprecatedType, err)
	}
	if err := warnings.Reconcile(w, skillUnresolvedType, unresolved); err != nil {
		fmt.Fprintf(w, "[startup] recording %s failed: %v\n", skillUnresolvedType, err)
	}
}

func unresolvedCondition(ue *skillUnresolvedError) warnings.Condition {
	return warnings.Condition{
		Type:    skillUnresolvedType,
		Subject: ue.Stage + "." + ue.Key,
		Level:   warnings.LevelWarning,
		Title:   fmt.Sprintf("%s: %s %q does not resolve", ue.Stage, ue.Key, ue.Ref),
		Detail: fmt.Sprintf("Stage %q cannot run: %s %q is unresolved (%s).\n\nPlugins searched: %s.\n\n"+
			"Fix: name a skill a loaded plugin provides as <plugin>:<skill>, or add the plugin to plugin_dirs (see docs/USER_GUIDE.md).",
			ue.Stage, ue.Key, ue.Ref, ue.Why, ue.Cat.describe()),
		Fingerprint: ue.Ref + "|" + ue.Why,
	}
}

// reportSkillUnresolved records a dispatch-time unresolved reference in the
// log and the warnings panel (same entry type and subject as the startup
// check, so the two never duplicate).
func (e *Engine) reportSkillUnresolved(ue *skillUnresolvedError) {
	if err := warnings.Report(e.warnWriter("skill"), unresolvedCondition(ue)); err != nil {
		e.logf(0, "skill", "recording %s: %v\n", skillUnresolvedType, err)
	}
}

// clearSkillUnresolved clears the panel entry of a reference that now resolves.
func (e *Engine) clearSkillUnresolved(stageName, key string) {
	if err := warnings.Resolve(skillUnresolvedType + ":" + stageName + "." + key); err != nil {
		e.logf(0, "skill", "clearing %s: %v\n", skillUnresolvedType, err)
	}
}
