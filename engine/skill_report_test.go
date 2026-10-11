package engine

import (
	"strings"
	"testing"

	"github.com/handarbeit/fabrik/plugin"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/warnings"
)

func panelKeys(t *testing.T) map[string]warnings.Entry {
	t.Helper()
	entries, err := warnings.Load()
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]warnings.Entry{}
	for _, e := range entries {
		m[e.Key] = e
	}
	return m
}

func TestReportStageSkills_DeprecationAndUnresolved(t *testing.T) {
	isolateWarningsFile(t)
	root := t.TempDir()
	base := skillPlugin(t, root, "base", plugin.BasePluginName, "fabrik-review", "fabrik-review-comment")
	org := skillPlugin(t, root, "org", "myorg", "fabrik-review")
	e := &Engine{skillResolver: newSkillResolver([]string{base, org})}

	list := []*stages.Stage{
		{Name: "Review", Skill: "fabrik-review", CommentSkill: "fabrik-review-comment"},
		{Name: "Plan", Skill: "myorg:fabrik-review"},
		{Name: "Broken", Skill: "nope:x", CommentSkill: "ghost"},
		{Name: "Prompted", Prompt: "no skill here"},
	}
	var out strings.Builder
	e.reportStageSkills(list, &out)

	log := out.String()
	if strings.Count(log, "deprecated bare form") != 1 {
		t.Errorf("want exactly one deprecation line, got:\n%s", log)
	}
	q := plugin.Qualify(plugin.BasePluginName, "fabrik-review")
	for _, want := range []string{"Review.skill → " + q, "Review.comment_skill → " + plugin.Qualify(plugin.BasePluginName, "fabrik-review-comment"), "also provided by myorg:fabrik-review", "fabrik upgrade"} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, "Plan.skill") {
		t.Errorf("qualified reference listed as deprecated:\n%s", log)
	}

	panel := panelKeys(t)
	if _, ok := panel[skillDeprecatedType+":bare"]; !ok {
		t.Errorf("deprecation entry missing: %v", panel)
	}
	for _, key := range []string{skillUnresolvedType + ":Broken.skill", skillUnresolvedType + ":Broken.comment_skill"} {
		ent, ok := panel[key]
		if !ok {
			t.Errorf("missing %s in %v", key, panel)
			continue
		}
		if !strings.Contains(ent.Detail, base) {
			t.Errorf("%s detail lacks plugin path: %s", key, ent.Detail)
		}
	}
	if len(panel) != 3 {
		t.Errorf("panel = %v, want 3 entries", panel)
	}

	// A fixed configuration clears both entry types.
	var out2 strings.Builder
	e.reportStageSkills([]*stages.Stage{{Name: "Review", Skill: q}}, &out2)
	if p := panelKeys(t); len(p) != 0 {
		t.Errorf("panel after fix = %v, want empty", p)
	}
	if strings.Contains(out2.String(), "deprecated") {
		t.Errorf("fixed config still logged a deprecation: %s", out2.String())
	}
}

func TestReportStageSkills_NoResolverOrEmptyCatalogueIsSilent(t *testing.T) {
	isolateWarningsFile(t)
	list := []*stages.Stage{{Name: "Review", Skill: "fabrik-review"}}
	var out strings.Builder
	(&Engine{}).reportStageSkills(list, &out)
	(&Engine{skillResolver: newSkillResolver(nil)}).reportStageSkills(list, &out)
	if out.Len() != 0 || len(panelKeys(t)) != 0 {
		t.Errorf("want silence, got log %q panel %v", out.String(), panelKeys(t))
	}
}
