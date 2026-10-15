package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/handarbeit/fabrik/internal/skillref"
	"github.com/handarbeit/fabrik/plugin"
	"github.com/handarbeit/fabrik/stages"
)

// writePlugin creates dir/.claude-plugin/plugin.json with the given manifest
// body and returns dir.
func writePlugin(t *testing.T, dir, manifest string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".claude-plugin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, pluginManifestRel), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func namedPlugin(t *testing.T, dir, name string) string {
	t.Helper()
	return writePlugin(t, dir, `{"name":"`+name+`"}`)
}

func TestResolvePluginDirs_NoBaseNoExtras(t *testing.T) {
	got, err := resolvePluginDirs(t.TempDir(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("got %v, want empty", got)
	}
}

func TestResolvePluginDirs_DefaultBase(t *testing.T) {
	root := t.TempDir()
	base := namedPlugin(t, filepath.Join(root, ".fabrik", "plugin"), "fabrik")
	got, err := resolvePluginDirs(root, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{base}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestResolvePluginDirs_OverrideReplacesBase(t *testing.T) {
	root := t.TempDir()
	namedPlugin(t, filepath.Join(root, ".fabrik", "plugin"), "fabrik")
	override := t.TempDir() // bare directory: no manifest, must be tolerated
	extra := namedPlugin(t, filepath.Join(root, "proj"), "proj")
	got, err := resolvePluginDirs(root, override, []string{extra})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{override, extra}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestResolvePluginDirs_ExtrasInOrderRelativeToFabrikDir(t *testing.T) {
	root := t.TempDir()
	base := namedPlugin(t, filepath.Join(root, ".fabrik", "plugin"), "fabrik")
	a := namedPlugin(t, filepath.Join(root, ".fabrik", "plugins", "a"), "a")
	b := namedPlugin(t, t.TempDir(), "b")
	got, err := resolvePluginDirs(root, "", []string{".fabrik/plugins/a", b})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{base, a, b}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestResolvePluginDirs_ExtrasAloneWithoutBase(t *testing.T) {
	root := t.TempDir()
	a := namedPlugin(t, filepath.Join(root, "a"), "a")
	got, err := resolvePluginDirs(root, "", []string{a})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{a}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestResolvePluginDirs_DuplicatePathsDropped(t *testing.T) {
	root := t.TempDir()
	base := namedPlugin(t, filepath.Join(root, ".fabrik", "plugin"), "fabrik")
	a := namedPlugin(t, filepath.Join(root, "a"), "a")
	got, err := resolvePluginDirs(root, "", []string{a, base, "a", a + "/"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{base, a}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestResolvePluginDirs_BadExtraNamesPath(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(root, "missing")
	noManifest := filepath.Join(root, "bare")
	if err := os.MkdirAll(noManifest, 0o755); err != nil {
		t.Fatal(err)
	}
	malformed := writePlugin(t, filepath.Join(root, "bad"), `{not json`)
	emptyName := writePlugin(t, filepath.Join(root, "noname"), `{"name":""}`)
	file := filepath.Join(root, "afile")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	for _, dir := range []string{missing, noManifest, malformed, emptyName, file} {
		_, err := resolvePluginDirs(root, "", []string{dir})
		if err == nil {
			t.Errorf("%s: expected error", dir)
			continue
		}
		if !strings.Contains(err.Error(), dir) {
			t.Errorf("%s: error %q does not name the path", dir, err)
		}
	}
}

func TestResolvePluginDirs_DuplicateNameNamesBothPaths(t *testing.T) {
	root := t.TempDir()
	a := namedPlugin(t, filepath.Join(root, "a"), "same")
	b := namedPlugin(t, filepath.Join(root, "b"), "same")
	_, err := resolvePluginDirs(root, "", []string{a, b})
	if err == nil {
		t.Fatal("expected duplicate-name error")
	}
	if !strings.Contains(err.Error(), a) || !strings.Contains(err.Error(), b) {
		t.Fatalf("error %q should name both %s and %s", err, a, b)
	}
}

func TestResolvePluginDirs_ExtraNamedFabrikCollidesWithDefaultBase(t *testing.T) {
	root := t.TempDir()
	base := namedPlugin(t, filepath.Join(root, ".fabrik", "plugin"), "fabrik")
	extra := namedPlugin(t, filepath.Join(root, "mine"), "fabrik")
	_, err := resolvePluginDirs(root, "", []string{extra})
	if err == nil {
		t.Fatal("expected collision error")
	}
	if !strings.Contains(err.Error(), base) || !strings.Contains(err.Error(), extra) {
		t.Fatalf("error %q should name both paths", err)
	}
}

func TestResolvePluginDirs_BaseWithoutManifestSkippedInNameCheck(t *testing.T) {
	root := t.TempDir()
	base := t.TempDir()
	extra := namedPlugin(t, filepath.Join(root, "x"), "fabrik")
	got, err := resolvePluginDirs(root, base, []string{extra})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{base, extra}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// withPluginDirs sets the package global for one test and restores it.
func withPluginDirs(t *testing.T, dirs []string) {
	t.Helper()
	prev := claudePluginDirs
	claudePluginDirs = dirs
	t.Cleanup(func() { claudePluginDirs = prev })
}

func pluginDirArgs(args []string) []string {
	var out []string
	for i, a := range args {
		if a == "--plugin-dir" && i+1 < len(args) {
			out = append(out, args[i+1])
		}
	}
	return out
}

func TestBuildClaudeArgs_PluginDirs(t *testing.T) {
	stage := &stages.Stage{Name: "Plan", Prompt: "plan"}

	t.Run("none emits no --plugin-dir", func(t *testing.T) {
		withPluginDirs(t, nil)
		args := buildClaudeArgs(stage, "", "", 0, false, "", "")
		if got := pluginDirArgs(args); len(got) != 0 {
			t.Fatalf("got %v, want none", got)
		}
	})

	t.Run("single dir is exactly today's arguments", func(t *testing.T) {
		withPluginDirs(t, nil)
		without := buildClaudeArgs(stage, "sess", "", 0, false, "", "")
		withPluginDirs(t, []string{"/base"})
		with := buildClaudeArgs(stage, "sess", "", 0, false, "", "")
		if got := pluginDirArgs(with); !reflect.DeepEqual(got, []string{"/base"}) {
			t.Fatalf("got %v", got)
		}
		// Removing the one flag pair must give the no-plugin arguments.
		var stripped []string
		for i := 0; i < len(with); i++ {
			if with[i] == "--plugin-dir" {
				i++
				continue
			}
			stripped = append(stripped, with[i])
		}
		if !reflect.DeepEqual(stripped, without) {
			t.Fatalf("stripped %v != %v", stripped, without)
		}
	})

	t.Run("base plus extras emit one flag each in order", func(t *testing.T) {
		withPluginDirs(t, []string{"/base", "/x", "/y"})
		args := buildClaudeArgs(stage, "", "", 0, false, "", "")
		if got := pluginDirArgs(args); !reflect.DeepEqual(got, []string{"/base", "/x", "/y"}) {
			t.Fatalf("got %v", got)
		}
	})

	t.Run("plugin dirs precede --resume", func(t *testing.T) {
		withPluginDirs(t, []string{"/base", "/x"})
		args := buildClaudeArgs(stage, "sess_1", "", 0, false, "", "")
		last, resume := -1, -1
		for i, a := range args {
			if a == "--plugin-dir" {
				last = i
			}
			if a == "--resume" {
				resume = i
			}
		}
		if last < 0 || resume < 0 || last > resume {
			t.Fatalf("plugin-dir at %d, resume at %d in %v", last, resume, args)
		}
	})
}

func TestNew_PluginDirsRefusedOnDuplicateName(t *testing.T) {
	root := t.TempDir()
	namedPlugin(t, filepath.Join(root, ".fabrik", "plugin"), "fabrik")
	extra := namedPlugin(t, filepath.Join(root, "mine"), "fabrik")
	t.Chdir(root)
	withPluginDirs(t, []string{"sentinel"})
	_, err := New(Config{PluginDirs: []string{extra}})
	if err == nil || !strings.Contains(err.Error(), extra) {
		t.Fatalf("expected error naming %s, got %v", extra, err)
	}
	if !reflect.DeepEqual(claudePluginDirs, []string{"sentinel"}) {
		t.Fatalf("global modified by refused New: %v", claudePluginDirs)
	}
}

func localWorkflowsDir(fabrikDir string) string {
	return filepath.Join(fabrikDir, ".fabrik", "plugins", "local-workflows")
}

// FR-007: local-workflows loads automatically, right after the base and
// before the configured extras.
func TestResolvePluginDirs_AutoLoadsLocalWorkflows(t *testing.T) {
	fabrikDir := t.TempDir()
	base := namedPlugin(t, filepath.Join(fabrikDir, ".fabrik", "plugin"), "fabrik")
	lw := namedPlugin(t, localWorkflowsDir(fabrikDir), "local-workflows")
	extra := namedPlugin(t, filepath.Join(fabrikDir, "extra"), "extra")

	got, err := resolvePluginDirs(fabrikDir, "", []string{extra})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{base, lw, extra}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestResolvePluginDirs_LocalWorkflowsAbsentIsNoChange(t *testing.T) {
	fabrikDir := t.TempDir()
	base := namedPlugin(t, filepath.Join(fabrikDir, ".fabrik", "plugin"), "fabrik")
	got, err := resolvePluginDirs(fabrikDir, "", nil)
	if err != nil || !reflect.DeepEqual(got, []string{base}) {
		t.Fatalf("got %v err %v", got, err)
	}
}

// An operator who also lists the directory in plugin_dirs is not refused.
func TestResolvePluginDirs_LocalWorkflowsAlsoListedIsDeduplicated(t *testing.T) {
	fabrikDir := t.TempDir()
	base := namedPlugin(t, filepath.Join(fabrikDir, ".fabrik", "plugin"), "fabrik")
	lw := namedPlugin(t, localWorkflowsDir(fabrikDir), "local-workflows")
	got, err := resolvePluginDirs(fabrikDir, "", []string{".fabrik/plugins/local-workflows"})
	if err != nil || !reflect.DeepEqual(got, []string{base, lw}) {
		t.Fatalf("got %v err %v", got, err)
	}
}

// A copy that still declares the base name (a half-done move) is refused
// loudly rather than loaded beside the base.
func TestResolvePluginDirs_LocalWorkflowsNameClashIsRefused(t *testing.T) {
	fabrikDir := t.TempDir()
	namedPlugin(t, filepath.Join(fabrikDir, ".fabrik", "plugin"), "fabrik")
	namedPlugin(t, localWorkflowsDir(fabrikDir), "fabrik")
	_, err := resolvePluginDirs(fabrikDir, "", nil)
	if err == nil || !strings.Contains(err.Error(), "must be unique") {
		t.Fatalf("err = %v", err)
	}
	// The refusal names the remedy for this state: 'fabrik upgrade' finishes
	// the interrupted migration (cmd's resume tests cover that it does).
	if !strings.Contains(err.Error(), "fabrik upgrade") {
		t.Fatalf("err = %v, want it to point at 'fabrik upgrade'", err)
	}
}

// The hint is specific to the auto-loaded directory: a clash between
// configured extras keeps the plain message.
func TestResolvePluginDirs_ExtraNameClashHasNoMigrationHint(t *testing.T) {
	root := t.TempDir()
	a := namedPlugin(t, filepath.Join(root, "a"), "same")
	b := namedPlugin(t, filepath.Join(root, "b"), "same")
	_, err := resolvePluginDirs(root, "", []string{a, b})
	if err == nil || strings.Contains(err.Error(), "fabrik upgrade") {
		t.Fatalf("err = %v", err)
	}
}

func TestResolvePluginDirs_LocalWorkflowsLoadsBesideDevOverride(t *testing.T) {
	fabrikDir := t.TempDir()
	override := namedPlugin(t, filepath.Join(t.TempDir(), "vendored"), "fabrik")
	lw := namedPlugin(t, localWorkflowsDir(fabrikDir), "local-workflows")
	got, err := resolvePluginDirs(fabrikDir, override, nil)
	if err != nil || !reflect.DeepEqual(got, []string{override, lw}) {
		t.Fatalf("got %v err %v", got, err)
	}
}

// The migrated layout end to end at the loader: the dirs resolvePluginDirs
// returns feed the skill catalogue, so a stage pointing at
// local-workflows:<skill> resolves, and the base's copy stays reachable by its
// own qualified name. (The sim bed builds engines with NewWithDeps, which load
// no plugins, so this is the twin-level coverage; see ADR 2139.)
func TestResolvePluginDirs_LocalWorkflowsSkillsResolveQualified(t *testing.T) {
	fabrikDir := t.TempDir()
	skillPlugin(t, fabrikDir, filepath.Join(".fabrik", "plugin"), plugin.BasePluginName, "plan", "research")
	// A moved copy keeps its pre-rename skill names (ADR 2140).
	skillPlugin(t, fabrikDir, filepath.Join(".fabrik", "plugins", "local-workflows"), plugin.LocalWorkflowsName, "fabrik-plan")

	dirs, err := resolvePluginDirs(fabrikDir, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	cat := skillref.Build(dirs)
	for ref, want := range map[string]string{
		"local-workflows:fabrik-plan": "local-workflows:fabrik-plan",
		"fabrik-stages:plan":          "fabrik-stages:plan",
		"fabrik-stages:research":      "fabrik-stages:research",
		"plan":                        "fabrik-stages:plan", // bare still resolves base-first
		// Pre-rename references land on the renamed skills (FR-004), but
		// local-workflows:* references are never remapped.
		"fabrik:fabrik-research": "fabrik-stages:research",
		"fabrik:fabrik-plan":     "fabrik-stages:plan",
	} {
		res, why, ok := cat.Resolve(ref, fabrikSkillOptions)
		if !ok {
			t.Errorf("%s did not resolve: %s", ref, why)
			continue
		}
		if res.Name != want {
			t.Errorf("%s resolved to %q, want %q", ref, res.Name, want)
		}
	}
}

// FR-005 (#2140): every worker invocation disables the PM plugin
// (fabrik@fabrik) through an inline --settings value that carries nothing else.
func TestBuildClaudeArgs_DisablesPMPlugin(t *testing.T) {
	const want = `{"enabledPlugins":{"fabrik@fabrik":false}}`
	if got := workerSettingsJSON(); got != want {
		t.Fatalf("workerSettingsJSON = %s, want %s", got, want)
	}
	// Only enabledPlugins, and the stage plugins are never named in it.
	var decoded map[string]map[string]bool
	if err := json.Unmarshal([]byte(workerSettingsJSON()), &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded) != 1 || len(decoded["enabledPlugins"]) != 1 {
		t.Fatalf("settings carry more than the PM plugin: %v", decoded)
	}
	for _, n := range []string{plugin.BasePluginName, plugin.LegacyBasePluginName, plugin.LocalWorkflowsName} {
		if _, bad := decoded["enabledPlugins"][n]; bad {
			t.Errorf("settings name %q", n)
		}
	}

	stage := &stages.Stage{Name: "Plan", Prompt: "plan"}
	withPluginDirs(t, []string{"/base", "/lw"})
	for name, args := range map[string][]string{
		"restricted":   buildClaudeArgs(stage, "", "", 0, false, "", ""),
		"unrestricted": buildClaudeArgs(stage, "", "", 0, true, "", ""),
		"resume":       buildClaudeArgs(stage, "sess", "", 0, false, "", ""),
	} {
		var found []string
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "--settings" {
				found = append(found, args[i+1])
			}
		}
		if len(found) != 1 || found[0] != want {
			t.Errorf("%s: --settings values = %v, want exactly [%s]", name, found, want)
		}
		if got := pluginDirArgs(args); !reflect.DeepEqual(got, []string{"/base", "/lw"}) {
			t.Errorf("%s: plugin dirs = %v", name, got)
		}
	}
}
