package skillref

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// writePlugin creates dir/.claude-plugin/plugin.json with the given manifest
// body and returns dir.
func writePlugin(t *testing.T, dir, manifest string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".claude-plugin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ManifestRel), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func namedPlugin(t *testing.T, dir, name string) string {
	t.Helper()
	return writePlugin(t, dir, `{"name":"`+name+`"}`)
}

func writeSkill(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("---\ndescription: x\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDiscover_DefaultSkillsDir(t *testing.T) {
	dir := namedPlugin(t, t.TempDir(), "myorg")
	writeSkill(t, filepath.Join(dir, "skills", "review", "SKILL.md"))
	writeSkill(t, filepath.Join(dir, "skills", "plan", "SKILL.md"))
	if err := os.MkdirAll(filepath.Join(dir, "skills", "nofile"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := Discover(dir)
	if p.Name != "myorg" {
		t.Errorf("name = %q", p.Name)
	}
	if got, want := p.SkillNames(), []string{"plan", "review"}; !reflect.DeepEqual(got, want) {
		t.Errorf("skills = %v, want %v", got, want)
	}
}

func TestDiscover_ManifestSkillsKey(t *testing.T) {
	dir := writePlugin(t, t.TempDir(), `{"name":"m","skills":["./extra","./solo"]}`)
	writeSkill(t, filepath.Join(dir, "skills", "a", "SKILL.md"))
	writeSkill(t, filepath.Join(dir, "extra", "b", "SKILL.md"))
	writeSkill(t, filepath.Join(dir, "solo", "SKILL.md"))
	p := Discover(dir)
	if got, want := p.SkillNames(), []string{"a", "b", "solo"}; !reflect.DeepEqual(got, want) {
		t.Errorf("skills = %v, want %v", got, want)
	}
}

func TestDiscover_ManifestSkillsStringAndDot(t *testing.T) {
	dir := writePlugin(t, t.TempDir(), `{"name":"m","skills":"."}`)
	writeSkill(t, filepath.Join(dir, "SKILL.md"))
	p := Discover(dir)
	if _, ok := p.Skills["m"]; !ok {
		t.Errorf("skills = %v, want root skill named after plugin", p.SkillNames())
	}
}

func TestDiscover_ManifestSkillsEscapingRootIgnored(t *testing.T) {
	outer := t.TempDir()
	writeSkill(t, filepath.Join(outer, "evil", "SKILL.md"))
	dir := writePlugin(t, filepath.Join(outer, "plug"), `{"name":"m","skills":"../evil"}`)
	if p := Discover(dir); len(p.Skills) != 0 {
		t.Errorf("skills = %v, want none", p.SkillNames())
	}
}

func TestDiscover_RootSingleSkill(t *testing.T) {
	dir := namedPlugin(t, t.TempDir(), "solo")
	writeSkill(t, filepath.Join(dir, "SKILL.md"))
	p := Discover(dir)
	if got, want := p.SkillNames(), []string{"solo"}; !reflect.DeepEqual(got, want) {
		t.Errorf("skills = %v, want %v", got, want)
	}
}

func TestDiscover_RootSkillIgnoredWhenSkillsDirExists(t *testing.T) {
	dir := namedPlugin(t, t.TempDir(), "p")
	writeSkill(t, filepath.Join(dir, "SKILL.md"))
	writeSkill(t, filepath.Join(dir, "skills", "x", "SKILL.md"))
	if got, want := Discover(dir).SkillNames(), []string{"x"}; !reflect.DeepEqual(got, want) {
		t.Errorf("skills = %v, want %v", got, want)
	}
}

func TestDiscover_ManifestlessNamedAfterDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "barefoot")
	writeSkill(t, filepath.Join(dir, "skills", "s", "SKILL.md"))
	p := Discover(dir)
	if p.Name != "barefoot" {
		t.Errorf("name = %q, want barefoot", p.Name)
	}
	if _, ok := p.Skills["s"]; !ok {
		t.Errorf("skills = %v", p.SkillNames())
	}
}

func TestDiscover_CommandsAreNotSkills(t *testing.T) {
	dir := namedPlugin(t, t.TempDir(), "p")
	writeSkill(t, filepath.Join(dir, "commands", "do", "SKILL.md"))
	if p := Discover(dir); len(p.Skills) != 0 {
		t.Errorf("skills = %v, want none", p.SkillNames())
	}
}

// A sibling directory sharing the plugin directory's name as a string prefix
// (plug vs plug-evil) is outside the plugin root and must be ignored.
func TestDiscover_ManifestSkillsSiblingPrefixIgnored(t *testing.T) {
	outer := t.TempDir()
	writeSkill(t, filepath.Join(outer, "plug-evil", "skills", "x", "SKILL.md"))
	dir := writePlugin(t, filepath.Join(outer, "plug"), `{"name":"m","skills":"../plug-evil/skills"}`)
	if p := Discover(dir); len(p.Skills) != 0 {
		t.Errorf("skills = %v, want none", p.SkillNames())
	}
}

func TestDiscover_ManifestVersion(t *testing.T) {
	with := writePlugin(t, t.TempDir(), `{"name":"v","version":"1.2.3"}`)
	if got := Discover(with).Version; got != "1.2.3" {
		t.Errorf("version = %q, want 1.2.3", got)
	}
	without := namedPlugin(t, t.TempDir(), "nov")
	if got := Discover(without).Version; got != "" {
		t.Errorf("version = %q, want empty", got)
	}
}
