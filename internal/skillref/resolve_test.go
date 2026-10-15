package skillref

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/handarbeit/fabrik/plugin"
)

// fabrikOpts is the engine's wiring of the resolver. The moved tests run with it
// so the legacy and base-preference cases are not vacuous; the zero-Options
// cases below cover a consumer that supplies neither.
var fabrikOpts = Options{
	PreferredBases: []string{plugin.BasePluginName, plugin.LegacyBasePluginName},
	LegacyRef:      plugin.LegacyRef,
}

func newTestResolver(dirs []string) *Resolver { return NewResolver(dirs, fabrikOpts) }

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
	cat := Build([]string{a, b, base})

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
			res, why, ok := cat.Resolve(tt.ref, fabrikOpts)
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
	res, _, ok := Build([]string{b, a}).Resolve("s", fabrikOpts)
	if !ok || res.Name != "beta:s" || len(res.Others) != 1 || res.Others[0] != "alpha:s" {
		t.Fatalf("got %+v ok=%v", res, ok)
	}
}

func TestSkillCatalogueResolve_EmptyPassesThrough(t *testing.T) {
	for _, ref := range []string{"fabrik-review", "x:y"} {
		res, _, ok := Build(nil).Resolve(ref, fabrikOpts)
		if !ok || res.Name != ref || res.Deprecated {
			t.Errorf("%q: got %+v ok=%v", ref, res, ok)
		}
	}
}

func TestSkillResolver_RebuildsOnMiss(t *testing.T) {
	root := t.TempDir()
	dir := skillPlugin(t, root, "p", "proj")
	r := newTestResolver([]string{dir})
	if _, _, _, ok := r.Resolve("proj:late"); ok {
		t.Fatal("resolved before the skill existed")
	}
	writeSkill(t, filepath.Join(dir, "skills", "late", "SKILL.md"))
	res, _, _, ok := r.Resolve("proj:late")
	if !ok || res.Name != "proj:late" {
		t.Fatalf("got %+v ok=%v after the skill appeared", res, ok)
	}
}

// A miss against a snapshot another goroutine has already replaced shares that
// newer snapshot instead of rescanning the plugin directories again.
func TestSkillResolver_RebuildIfStaleCoalesces(t *testing.T) {
	dir := t.TempDir()
	r := newTestResolver([]string{dir})
	first := r.cat.Load()

	second := r.rebuildIfStale(first)
	if second == first {
		t.Fatal("rebuild with the current snapshot did not rescan")
	}
	if got := r.rebuildIfStale(first); got != second {
		t.Error("rebuild with a stale snapshot rescanned instead of reusing the newer one")
	}
}

// SC-003 (#2140): a deployment that upgraded the binary — so .fabrik/plugin was
// refreshed to fabrik-stages with the short skill names — but has not yet run
// `fabrik upgrade` still has stage files saying fabrik:fabrik-<stage>, or the
// bare fabrik-<stage>. Every one of them must resolve, flagged deprecated.
func TestSkillCatalogueResolve_PreRenameReferencesOnRenamedPlugin(t *testing.T) {
	root := t.TempDir()
	var shipped []string
	for _, old := range plugin.LegacySkillNames() {
		n, _ := plugin.LegacySkillTarget(old)
		shipped = append(shipped, n)
	}
	base := skillPlugin(t, root, "base", plugin.BasePluginName, shipped...)
	cat := Build([]string{base})

	for _, old := range plugin.LegacySkillNames() {
		want, _ := plugin.LegacyRef(old)
		for _, ref := range []string{plugin.Qualify(plugin.LegacyBasePluginName, old), old} {
			res, why, ok := cat.Resolve(ref, fabrikOpts)
			if !ok {
				t.Errorf("%s did not resolve: %s", ref, why)
				continue
			}
			if res.Name != want || !res.Deprecated {
				t.Errorf("%s = %+v, want %q deprecated", ref, res, want)
			}
		}
	}
	// The current form is not deprecated.
	if res, _, ok := cat.Resolve("fabrik-stages:review", fabrikOpts); !ok || res.Deprecated {
		t.Errorf("fabrik-stages:review = %+v ok=%v", res, ok)
	}
	// Unknown names stay unresolved; nothing is remapped blindly.
	for _, ref := range []string{"fabrik:fabrik-nope", "fabrik-nope", "fabrik:review", "local-workflows:fabrik-review"} {
		if _, _, ok := cat.Resolve(ref, fabrikOpts); ok {
			t.Errorf("%s resolved, want unresolved", ref)
		}
	}
}

// An operator-customised, unmigrated copy of the old base plugin (still named
// "fabrik", still carrying fabrik-review) keeps winning: the as-is lookup runs
// first and the legacy mapping is only a fallback.
func TestSkillCatalogueResolve_CustomisedLegacyBaseWinsOverMapping(t *testing.T) {
	root := t.TempDir()
	old := skillPlugin(t, root, "old", plugin.LegacyBasePluginName, "fabrik-review")
	cat := Build([]string{old})

	res, why, ok := cat.Resolve("fabrik:fabrik-review", fabrikOpts)
	if !ok || res.Name != "fabrik:fabrik-review" || res.Deprecated {
		t.Fatalf("got %+v %q ok=%v, want the customised plugin's own skill, not deprecated", res, why, ok)
	}
	if res, _, ok := cat.Resolve("fabrik-review", fabrikOpts); !ok || res.Name != "fabrik:fabrik-review" {
		t.Errorf("bare = %+v ok=%v, want fabrik:fabrik-review", res, ok)
	}

	// With both loaded, the as-is hit still wins over the mapped one.
	cur := skillPlugin(t, root, "cur", plugin.BasePluginName, "review")
	cat = Build([]string{cur, old})
	if res, _, ok := cat.Resolve("fabrik:fabrik-review", fabrikOpts); !ok || res.Name != "fabrik:fabrik-review" {
		t.Errorf("both loaded: %+v ok=%v", res, ok)
	}
}

// Migrated layout: local-workflows keeps the pre-rename skill names and its
// references are never remapped.
func TestSkillCatalogueResolve_LocalWorkflowsNeverRemapped(t *testing.T) {
	root := t.TempDir()
	base := skillPlugin(t, root, "base", plugin.BasePluginName, "review")
	lw := skillPlugin(t, root, "lw", plugin.LocalWorkflowsName, "fabrik-review")
	cat := Build([]string{base, lw})
	if res, _, ok := cat.Resolve("local-workflows:fabrik-review", fabrikOpts); !ok || res.Name != "local-workflows:fabrik-review" || res.Deprecated {
		t.Errorf("got %+v ok=%v", res, ok)
	}
	cat = Build([]string{base})
	if _, _, ok := cat.Resolve("local-workflows:fabrik-review", fabrikOpts); ok {
		t.Error("local-workflows reference resolved against the base plugin")
	}
}

func TestResolve_ZeroOptions(t *testing.T) {
	root := t.TempDir()
	base := skillPlugin(t, root, "base", plugin.BasePluginName, "shared", "fabrik-review")
	other := skillPlugin(t, root, "o", "other", "shared")
	cat := Build([]string{other, base})

	// No preferred base: load order decides, even though fabrik-stages provides it.
	res, _, ok := cat.Resolve("shared", Options{})
	if !ok || res.Name != "other:shared" || len(res.Others) != 1 || res.Others[0] != plugin.Qualify(plugin.BasePluginName, "shared") {
		t.Fatalf("got %+v ok=%v, want load order without a preferred base", res, ok)
	}
	// A different base plugin is honoured.
	res, _, ok = cat.Resolve("shared", Options{PreferredBases: []string{"other"}})
	if !ok || res.Name != "other:shared" {
		t.Fatalf("got %+v ok=%v, want the supplied base", res, ok)
	}
	// No legacy table: a pre-rename reference stays unresolved.
	if _, _, ok := Build([]string{base}).Resolve("fabrik:fabrik-research", Options{}); ok {
		t.Error("legacy reference resolved without a LegacyRef")
	}
	// With the table it maps (when the target exists) and is flagged.
	shipped := skillPlugin(t, root, "s", plugin.BasePluginName, "research")
	res, _, ok = Build([]string{shipped}).Resolve("fabrik:fabrik-research", fabrikOpts)
	if !ok || res.Name != plugin.Qualify(plugin.BasePluginName, "research") || !res.Deprecated {
		t.Errorf("legacy mapping: %+v ok=%v", res, ok)
	}
}

func TestResolver_ZeroOptionsAndDescribe(t *testing.T) {
	dir := skillPlugin(t, t.TempDir(), "p", "proj", "s")
	r := NewResolver([]string{dir}, Options{})
	if res, _, _, ok := r.Resolve("s"); !ok || res.Name != "proj:s" {
		t.Fatalf("got %+v ok=%v", res, ok)
	}
	if got := r.Describe(); !strings.Contains(got, "proj") || !strings.Contains(got, dir) {
		t.Errorf("Describe = %q", got)
	}
}

func TestNilSafety(t *testing.T) {
	var r *Resolver
	if got := r.Describe(); got != "(none)" {
		t.Errorf("nil resolver Describe = %q", got)
	}
	var c *Catalogue
	if !c.Empty() || c.Describe() != "(none)" {
		t.Error("nil catalogue must read as empty")
	}
	if _, ok := c.Plugin("x"); ok {
		t.Error("nil catalogue found a plugin")
	}
	ue := &UnresolvedError{Stage: "S", Key: "skill", Ref: "r", Why: "w"}
	if !strings.Contains(ue.Error(), "plugins searched: (none)") {
		t.Errorf("Error = %q", ue.Error())
	}
}
