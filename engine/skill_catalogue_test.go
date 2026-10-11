package engine

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
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

func TestDiscoverPlugin_DefaultSkillsDir(t *testing.T) {
	dir := namedPlugin(t, t.TempDir(), "myorg")
	writeSkill(t, filepath.Join(dir, "skills", "review", "SKILL.md"))
	writeSkill(t, filepath.Join(dir, "skills", "plan", "SKILL.md"))
	if err := os.MkdirAll(filepath.Join(dir, "skills", "nofile"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := discoverPlugin(dir)
	if p.Name != "myorg" {
		t.Errorf("name = %q", p.Name)
	}
	if got, want := p.skillNames(), []string{"plan", "review"}; !reflect.DeepEqual(got, want) {
		t.Errorf("skills = %v, want %v", got, want)
	}
}

func TestDiscoverPlugin_ManifestSkillsKey(t *testing.T) {
	dir := writePlugin(t, t.TempDir(), `{"name":"m","skills":["./extra","./solo"]}`)
	writeSkill(t, filepath.Join(dir, "skills", "a", "SKILL.md"))
	writeSkill(t, filepath.Join(dir, "extra", "b", "SKILL.md"))
	writeSkill(t, filepath.Join(dir, "solo", "SKILL.md"))
	p := discoverPlugin(dir)
	if got, want := p.skillNames(), []string{"a", "b", "solo"}; !reflect.DeepEqual(got, want) {
		t.Errorf("skills = %v, want %v", got, want)
	}
}

func TestDiscoverPlugin_ManifestSkillsStringAndDot(t *testing.T) {
	dir := writePlugin(t, t.TempDir(), `{"name":"m","skills":"."}`)
	writeSkill(t, filepath.Join(dir, "SKILL.md"))
	p := discoverPlugin(dir)
	if _, ok := p.Skills["m"]; !ok {
		t.Errorf("skills = %v, want root skill named after plugin", p.skillNames())
	}
}

func TestDiscoverPlugin_ManifestSkillsEscapingRootIgnored(t *testing.T) {
	outer := t.TempDir()
	writeSkill(t, filepath.Join(outer, "evil", "SKILL.md"))
	dir := writePlugin(t, filepath.Join(outer, "plug"), `{"name":"m","skills":"../evil"}`)
	if p := discoverPlugin(dir); len(p.Skills) != 0 {
		t.Errorf("skills = %v, want none", p.skillNames())
	}
}

func TestDiscoverPlugin_RootSingleSkill(t *testing.T) {
	dir := namedPlugin(t, t.TempDir(), "solo")
	writeSkill(t, filepath.Join(dir, "SKILL.md"))
	p := discoverPlugin(dir)
	if got, want := p.skillNames(), []string{"solo"}; !reflect.DeepEqual(got, want) {
		t.Errorf("skills = %v, want %v", got, want)
	}
}

func TestDiscoverPlugin_RootSkillIgnoredWhenSkillsDirExists(t *testing.T) {
	dir := namedPlugin(t, t.TempDir(), "p")
	writeSkill(t, filepath.Join(dir, "SKILL.md"))
	writeSkill(t, filepath.Join(dir, "skills", "x", "SKILL.md"))
	if got, want := discoverPlugin(dir).skillNames(), []string{"x"}; !reflect.DeepEqual(got, want) {
		t.Errorf("skills = %v, want %v", got, want)
	}
}

func TestDiscoverPlugin_ManifestlessNamedAfterDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "barefoot")
	writeSkill(t, filepath.Join(dir, "skills", "s", "SKILL.md"))
	p := discoverPlugin(dir)
	if p.Name != "barefoot" {
		t.Errorf("name = %q, want barefoot", p.Name)
	}
	if _, ok := p.Skills["s"]; !ok {
		t.Errorf("skills = %v", p.skillNames())
	}
}

func TestDiscoverPlugin_CommandsAreNotSkills(t *testing.T) {
	dir := namedPlugin(t, t.TempDir(), "p")
	writeSkill(t, filepath.Join(dir, "commands", "do", "SKILL.md"))
	if p := discoverPlugin(dir); len(p.Skills) != 0 {
		t.Errorf("skills = %v, want none", p.skillNames())
	}
}
