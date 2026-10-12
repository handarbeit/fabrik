package plugin

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"testing"
)

// migrationFixtureTags are the releases whose plugin tree and stage defaults
// are captured under testdata/migration/<tag>/ (SC-001).
var migrationFixtureTags = []string{"v0.0.64", "v0.0.70", "v0.0.77"}

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0755)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func fixtureTree(t *testing.T, tag string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "plugin")
	copyTree(t, filepath.Join("testdata", "migration", tag, "plugin"), dir)
	return dir
}

func TestShippedFilesTable_WellFormed(t *testing.T) {
	hexRE := regexp.MustCompile(`^[0-9a-f]{64}$`)
	tbl := loadShippedTable()
	if len(tbl) == 0 {
		t.Fatal("shipped_files.json is empty or unparseable")
	}
	for rel, sums := range tbl {
		if len(sums) == 0 {
			t.Errorf("%s: no sums", rel)
		}
		for s := range sums {
			if !hexRE.MatchString(s) {
				t.Errorf("%s: %q is not a sha256 hex sum", rel, s)
			}
		}
	}
}

func TestClassifyDir_FixturesFromEarlierReleasesArePristine(t *testing.T) {
	for _, tag := range migrationFixtureTags {
		t.Run(tag, func(t *testing.T) {
			c, err := ClassifyDir(fixtureTree(t, tag))
			if err != nil {
				t.Fatal(err)
			}
			if c.Customised() {
				t.Errorf("%s: shipped tree classified as customised: %v", tag, c.Unknown)
			}
			if len(c.Skills) == 0 || len(c.EditedSkills) != 0 {
				t.Errorf("%s: skills=%v edited=%v", tag, c.Skills, c.EditedSkills)
			}
		})
	}
}

func TestClassifyDir_EditedSkill(t *testing.T) {
	dir := fixtureTree(t, "v0.0.70")
	p := filepath.Join(dir, "skills", "fabrik-plan", "SKILL.md")
	data, _ := os.ReadFile(p)
	if err := os.WriteFile(p, append(data, []byte("\nlocal rule\n")...), 0644); err != nil {
		t.Fatal(err)
	}
	c, err := ClassifyDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.EditedSkills, []string{"fabrik-plan"}) {
		t.Errorf("EditedSkills = %v", c.EditedSkills)
	}
	if !reflect.DeepEqual(c.Unknown, []string{"skills/fabrik-plan/SKILL.md"}) {
		t.Errorf("Unknown = %v", c.Unknown)
	}
	if !c.Customised() || !c.IsEdited("fabrik-plan") || c.IsEdited("fabrik-research") {
		t.Error("Customised/IsEdited wrong")
	}
}

func TestClassifyDir_MixedVersionsAreNotCustomised(t *testing.T) {
	dir := fixtureTree(t, "v0.0.70")
	newer := filepath.Join("testdata", "migration", "v0.0.77", "plugin", "skills", "fabrik-review", "SKILL.md")
	data, err := os.ReadFile(newer)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "skills", "fabrik-review", "SKILL.md"), data, 0644); err != nil {
		t.Fatal(err)
	}
	c, _ := ClassifyDir(dir)
	if c.Customised() {
		t.Errorf("mixture of shipped versions classified as customised: %v", c.Unknown)
	}
}

func TestClassifyDir_SharedFileOnlyEditRedirectsNoSkill(t *testing.T) {
	dir := fixtureTree(t, "v0.0.77")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("my notes\n"), 0644); err != nil {
		t.Fatal(err)
	}
	c, _ := ClassifyDir(dir)
	if !c.Customised() || len(c.EditedSkills) != 0 {
		t.Errorf("Customised=%v EditedSkills=%v", c.Customised(), c.EditedSkills)
	}
}

func TestClassifyDir_ExtraFilesAndSkills(t *testing.T) {
	dir := fixtureTree(t, "v0.0.77")
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(dir, "skills", "fabrik-plan", "references"), 0755))
	must(os.WriteFile(filepath.Join(dir, "skills", "fabrik-plan", "references", "x.md"), []byte("x"), 0644))
	must(os.MkdirAll(filepath.Join(dir, "skills", "my-skill"), 0755))
	must(os.WriteFile(filepath.Join(dir, "skills", "my-skill", "SKILL.md"), []byte("s"), 0644))
	c, _ := ClassifyDir(dir)
	if !reflect.DeepEqual(c.EditedSkills, []string{"fabrik-plan", "my-skill"}) {
		t.Errorf("EditedSkills = %v", c.EditedSkills)
	}
	if !c.HasSkill("my-skill") {
		t.Error("HasSkill(my-skill) = false")
	}
}

func TestClassifyDir_IgnoresInstalledVersionAndMissingDir(t *testing.T) {
	dir := fixtureTree(t, "v0.0.77")
	if err := WriteVersionHash(dir, "deadbeef"); err != nil {
		t.Fatal(err)
	}
	c, _ := ClassifyDir(dir)
	if c.Customised() {
		t.Errorf(".installed-version counted: %v", c.Unknown)
	}
	c, err := ClassifyDir(filepath.Join(t.TempDir(), "nope"))
	if err != nil || len(c.Files) != 0 {
		t.Errorf("missing dir: %v %v", c, err)
	}
}

func TestShippedSkillNames_MatchEmbed(t *testing.T) {
	names := ShippedSkillNames()
	if len(names) < 12 {
		t.Errorf("ShippedSkillNames = %v", names)
	}
}
