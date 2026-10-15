package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/handarbeit/fabrik/plugin"
	"github.com/handarbeit/fabrik/stages"
)

func writeSkill(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("---\ndescription: x\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// skillPlugin writes a plugin named name under root/<dir> with the given skills.
func skillPlugin(t *testing.T, root, dir, name string, skills ...string) string {
	t.Helper()
	d := namedPlugin(t, filepath.Join(root, dir), name)
	for _, s := range skills {
		writeSkill(t, filepath.Join(d, "skills", s, "SKILL.md"))
	}
	return d
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

// newSkillResolver must carry Fabrik's whole wiring into the shared resolver
// (ADR 2305): dropping the legacy table or either preferred base fails here,
// not only in internal/skillref.
func TestNewSkillResolver_CarriesFabrikWiring(t *testing.T) {
	root := t.TempDir()
	cur := skillPlugin(t, root, "cur", plugin.BasePluginName, "review", "shared")
	res, _, _, ok := newSkillResolver([]string{cur}).Resolve("fabrik-review")
	if !ok || res.Name != plugin.Qualify(plugin.BasePluginName, "review") || !res.Deprecated {
		t.Errorf("legacy mapping: %+v ok=%v, want fabrik-stages:review deprecated", res, ok)
	}

	// A customised pre-#2140 base still wins a bare name over a later plugin.
	old := skillPlugin(t, root, "old", plugin.LegacyBasePluginName, "shared")
	other := skillPlugin(t, root, "oth", "zzz", "shared")
	res, _, _, ok = newSkillResolver([]string{other, old}).Resolve("shared")
	if !ok || res.Name != plugin.Qualify(plugin.LegacyBasePluginName, "shared") {
		t.Errorf("legacy base preference: %+v ok=%v", res, ok)
	}
	// The renamed base outranks the legacy one.
	res, _, _, ok = newSkillResolver([]string{other, old, cur}).Resolve("shared")
	if !ok || res.Name != plugin.Qualify(plugin.BasePluginName, "shared") {
		t.Errorf("base preference: %+v ok=%v", res, ok)
	}
}
