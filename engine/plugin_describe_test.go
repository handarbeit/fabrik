package engine

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func writePluginFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDescribePlugin_MatchesDiscovery(t *testing.T) {
	root := t.TempDir()

	named := filepath.Join(root, "named")
	writePluginFile(t, filepath.Join(named, pluginManifestRel), `{"name":"team"}`)
	writePluginFile(t, filepath.Join(named, "skills", "b", "SKILL.md"), "b")
	writePluginFile(t, filepath.Join(named, "skills", "a", "SKILL.md"), "a")

	keyed := filepath.Join(root, "keyed")
	writePluginFile(t, filepath.Join(keyed, pluginManifestRel), `{"name":"keyed","skills":["./extra"]}`)
	writePluginFile(t, filepath.Join(keyed, "extra", "x", "SKILL.md"), "x")

	single := filepath.Join(root, "single")
	writePluginFile(t, filepath.Join(single, pluginManifestRel), `{"name":"solo"}`)
	writePluginFile(t, filepath.Join(single, "SKILL.md"), "solo")

	for _, tc := range []struct {
		dir    string
		name   string
		skills []string
	}{
		{named, "team", []string{"a", "b"}},
		{keyed, "keyed", []string{"x"}},
		{single, "solo", []string{"solo"}},
		{filepath.Join(root, "missing"), "missing", []string{}},
	} {
		got := DescribePlugin(tc.dir)
		want := discoverPlugin(tc.dir)
		if got.Name != tc.name || got.Name != want.Name || !reflect.DeepEqual(got.Skills, tc.skills) || !reflect.DeepEqual(got.Skills, want.skillNames()) {
			t.Errorf("%s: got %+v, want name %q skills %v", tc.dir, got, tc.name, tc.skills)
		}
	}
}

func TestResolveSkillForTest_ReturnsProviderFile(t *testing.T) {
	root := t.TempDir()
	team := filepath.Join(root, "team")
	writePluginFile(t, filepath.Join(team, pluginManifestRel), `{"name":"team"}`)
	writePluginFile(t, filepath.Join(team, "skills", "fabrik-review", "SKILL.md"), "mine")

	q, file, ok := ResolveSkillForTest([]string{team}, "fabrik-review")
	if !ok || q != "team:fabrik-review" || file != filepath.Join(team, "skills", "fabrik-review", "SKILL.md") {
		t.Fatalf("got %q %q %v", q, file, ok)
	}
	if _, _, ok := ResolveSkillForTest([]string{team}, "nope"); ok {
		t.Fatal("unknown ref must not resolve")
	}
}
