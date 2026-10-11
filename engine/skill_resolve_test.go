package engine

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/handarbeit/fabrik/plugin"
	"github.com/handarbeit/fabrik/stages"
)

// skillPlugin writes a plugin named name under root/<dir> with the given skills.
func skillPlugin(t *testing.T, root, dir, name string, skills ...string) string {
	t.Helper()
	d := namedPlugin(t, filepath.Join(root, dir), name)
	for _, s := range skills {
		writeSkill(t, filepath.Join(d, "skills", s, "SKILL.md"))
	}
	return d
}

func TestSkillCatalogueResolve(t *testing.T) {
	root := t.TempDir()
	base := skillPlugin(t, root, "base", plugin.BasePluginName, "fabrik-review", "shared")
	a := skillPlugin(t, root, "a", "myorg", "review", "shared")
	b := skillPlugin(t, root, "b", "other", "only-b", "shared")
	cat := buildSkillCatalogue([]string{a, b, base})

	tests := []struct {
		name       string
		ref        string
		want       string
		deprecated bool
		others     int
		fail       string
	}{
		{name: "qualified", ref: "myorg:review", want: "myorg:review"},
		{name: "qualified base", ref: plugin.Qualify(plugin.BasePluginName, "fabrik-review"), want: plugin.Qualify(plugin.BasePluginName, "fabrik-review")},
		{name: "bare single provider", ref: "fabrik-review", want: plugin.Qualify(plugin.BasePluginName, "fabrik-review"), deprecated: true},
		{name: "bare multi prefers base", ref: "shared", want: plugin.Qualify(plugin.BasePluginName, "shared"), deprecated: true, others: 2},
		{name: "bare multi without base uses load order", ref: "only-b", want: "other:only-b", deprecated: true},
		{name: "plugin not loaded", ref: "nope:review", fail: `plugin "nope" is not loaded`},
		{name: "skill missing", ref: "myorg:missing", fail: `has no skill "missing"`},
		{name: "bare unknown", ref: "ghost", fail: "no loaded plugin provides"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, why, ok := cat.resolve(tt.ref)
			if tt.fail != "" {
				if ok || !strings.Contains(why, tt.fail) {
					t.Fatalf("ok=%v why=%q, want failure containing %q", ok, why, tt.fail)
				}
				return
			}
			if !ok || res.Name != tt.want || res.Deprecated != tt.deprecated || len(res.Others) != tt.others {
				t.Fatalf("got %+v ok=%v, want name=%q deprecated=%v others=%d", res, ok, tt.want, tt.deprecated, tt.others)
			}
		})
	}
}

func TestSkillCatalogueResolve_BareMultiWithoutBasePicksFirstLoaded(t *testing.T) {
	root := t.TempDir()
	a := skillPlugin(t, root, "a", "alpha", "s")
	b := skillPlugin(t, root, "b", "beta", "s")
	res, _, ok := buildSkillCatalogue([]string{b, a}).resolve("s")
	if !ok || res.Name != "beta:s" || len(res.Others) != 1 || res.Others[0] != "alpha:s" {
		t.Fatalf("got %+v ok=%v", res, ok)
	}
}

func TestSkillCatalogueResolve_EmptyPassesThrough(t *testing.T) {
	for _, ref := range []string{"fabrik-review", "x:y"} {
		res, _, ok := buildSkillCatalogue(nil).resolve(ref)
		if !ok || res.Name != ref || res.Deprecated {
			t.Errorf("%q: got %+v ok=%v", ref, res, ok)
		}
	}
}

func TestSkillResolver_RebuildsOnMiss(t *testing.T) {
	root := t.TempDir()
	dir := skillPlugin(t, root, "p", "proj")
	r := newSkillResolver([]string{dir})
	if _, _, _, ok := r.Resolve("proj:late"); ok {
		t.Fatal("resolved before the skill existed")
	}
	writeSkill(t, filepath.Join(dir, "skills", "late", "SKILL.md"))
	res, _, _, ok := r.Resolve("proj:late")
	if !ok || res.Name != "proj:late" {
		t.Fatalf("got %+v ok=%v after the skill appeared", res, ok)
	}
}

func TestResolveStageSkills(t *testing.T) {
	root := t.TempDir()
	base := skillPlugin(t, root, "base", plugin.BasePluginName, "fabrik-x", "fabrik-x-comment", "fix", "rebase")
	other := skillPlugin(t, root, "o", "myorg", "review")
	e := &Engine{skillResolver: newSkillResolver([]string{base, other})}

	orig := &stages.Stage{Name: "X", Skill: "myorg:review", CommentSkill: "fabrik-x-comment", CIFixSkill: "fix", RebaseSkill: plugin.Qualify(plugin.BasePluginName, "rebase")}
	got, resolved, err := e.resolveStageSkills(orig)
	if err != nil {
		t.Fatal(err)
	}
	q := func(s string) string { return plugin.Qualify(plugin.BasePluginName, s) }
	if got.Skill != "myorg:review" || got.CommentSkill != q("fabrik-x-comment") || got.CIFixSkill != q("fix") || got.RebaseSkill != q("rebase") {
		t.Errorf("resolved stage = %+v", got)
	}
	if orig.CommentSkill != "fabrik-x-comment" {
		t.Errorf("original stage mutated: %+v", orig)
	}
	dep := 0
	for _, r := range resolved {
		if r.Deprecated {
			dep++
		}
	}
	if dep != 2 {
		t.Errorf("deprecated refs = %d, want 2 (%+v)", dep, resolved)
	}
}

func TestResolveStageSkills_Unresolved(t *testing.T) {
	base := skillPlugin(t, t.TempDir(), "base", plugin.BasePluginName, "a")
	e := &Engine{skillResolver: newSkillResolver([]string{base})}
	_, _, err := e.resolveStageSkills(&stages.Stage{Name: "Review", Skill: "a", CommentSkill: "myorg:nope"})
	ue, ok := err.(*skillUnresolvedError)
	if !ok {
		t.Fatalf("err = %v", err)
	}
	msg := ue.Error()
	for _, want := range []string{`"Review"`, "comment_skill", "myorg:nope", plugin.BasePluginName, base} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q lacks %q", msg, want)
		}
	}
}

func TestResolveStageSkills_NoResolverOrNoSkills(t *testing.T) {
	s := &stages.Stage{Name: "X", Skill: "bare"}
	if got, _, err := (&Engine{}).resolveStageSkills(s); err != nil || got != s {
		t.Errorf("nil resolver: got %p err %v", got, err)
	}
	e := &Engine{skillResolver: newSkillResolver([]string{skillPlugin(t, t.TempDir(), "b", "b")})}
	holding := &stages.Stage{Name: "Queued"}
	if got, _, err := e.resolveStageSkills(holding); err != nil || got != holding {
		t.Errorf("no skills: got %p err %v", got, err)
	}
}
