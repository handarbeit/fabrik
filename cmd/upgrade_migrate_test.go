package cmd

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	fabrikplugin "github.com/handarbeit/fabrik/plugin"
)

var migrationTags = []string{"v0.0.64", "v0.0.70", "v0.0.77"}

// migrationFixtureRoot is absolute: the tests chdir into a temp deployment.
var migrationFixtureRoot, _ = filepath.Abs(filepath.Join("..", "plugin", "testdata", "migration"))

func copyDirTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
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

// snapshot returns relative path -> content for every file under dir.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		data, _ := os.ReadFile(p)
		out[filepath.ToSlash(rel)] = string(data)
		return nil
	})
	return out
}

// deployFixture builds a deployment from the release captured under
// plugin/testdata/migration/<tag>/ in a fresh temp dir, chdirs into it and
// returns it. The installed version is seeded from the pristine disk state, as
// the release binary of that era would have written it.
func deployFixture(t *testing.T, tag string) string {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(migrationFixtureRoot, tag)
	copyDirTree(t, filepath.Join(root, "plugin"), filepath.Join(dir, ".fabrik", "plugin"))
	copyDirTree(t, filepath.Join(root, "stages"), filepath.Join(dir, ".fabrik", "stages"))
	pluginDir := filepath.Join(dir, ".fabrik", "plugin")
	h, err := fabrikplugin.ComputeDiskVersion(pluginDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := fabrikplugin.WriteVersionHash(pluginDir, h); err != nil {
		t.Fatal(err)
	}
	chdirTest(t, dir)
	return dir
}

func editSkill(t *testing.T, dir, skill string) {
	t.Helper()
	p := filepath.Join(dir, ".fabrik", "plugin", "skills", skill, "SKILL.md")
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, append(data, []byte("\nLocal rule for "+skill+".\n")...), 0644); err != nil {
		t.Fatal(err)
	}
}

func stageRefs(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	files, _ := filepath.Glob(filepath.Join(dir, ".fabrik", "stages", "*.yaml"))
	for _, f := range files {
		data, _ := os.ReadFile(f)
		for _, line := range strings.Split(string(data), "\n") {
			for _, k := range []string{"skill", "comment_skill", "ci_fix_skill", "rebase_skill"} {
				if v, ok := strings.CutPrefix(line, k+": "); ok {
					out[filepath.Base(f)+":"+k] = strings.TrimSpace(v)
				}
			}
		}
	}
	return out
}

func assertBaseIsEmbedded(t *testing.T, dir string) {
	t.Helper()
	pluginDir := filepath.Join(dir, ".fabrik", "plugin")
	diff, err := diffingPluginFiles(pluginDir)
	if err != nil || len(diff) != 0 {
		t.Fatalf("base plugin differs from the embed: %v %v", diff, err)
	}
	custom, upgrade, _, err := fabrikplugin.CheckPluginState(pluginDir, true)
	if err != nil || custom || upgrade {
		t.Fatalf("base plugin state custom=%v upgrade=%v err=%v", custom, upgrade, err)
	}
}

// SC-001, User Story 1/3: an uncustomised deployment from any earlier release
// reaches the current layout in one run and a second run changes nothing.
func TestMigrate_UncustomisedFromEarlierReleases(t *testing.T) {
	for _, tag := range migrationTags {
		t.Run(tag, func(t *testing.T) {
			dir := deployFixture(t, tag)
			if err := runUpgrade(nil); err != nil {
				t.Fatalf("runUpgrade: %v", err)
			}
			for k, v := range stageRefs(t, dir) {
				if !strings.HasPrefix(v, fabrikplugin.BasePluginName+":") {
					t.Errorf("%s = %q, want a %s-qualified reference", k, v, fabrikplugin.BasePluginName)
				}
			}
			if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(fabrikplugin.LocalWorkflowsRelDir))); !os.IsNotExist(err) {
				t.Error("local-workflows must not be created for an uncustomised deployment")
			}
			assertBaseIsEmbedded(t, dir)

			first := snapshot(t, filepath.Join(dir, ".fabrik"))
			if err := runUpgrade(nil); err != nil {
				t.Fatalf("second runUpgrade: %v", err)
			}
			if !reflect.DeepEqual(first, snapshot(t, filepath.Join(dir, ".fabrik"))) {
				t.Error("second run changed files")
			}
		})
	}
}

// SC-001/SC-002, User Story 2: edits move into local-workflows byte-for-byte,
// edited stages point at them, the rest point at the base, the base is clean.
func TestMigrate_CustomisedDeploymentKeepsEditsInLocalWorkflows(t *testing.T) {
	for _, tag := range migrationTags {
		t.Run(tag, func(t *testing.T) {
			dir := deployFixture(t, tag)
			editSkill(t, dir, "fabrik-plan")
			editSkill(t, dir, "fabrik-review")
			before := snapshot(t, filepath.Join(dir, ".fabrik", "plugin"))

			if err := runUpgrade(nil); err != nil {
				t.Fatalf("runUpgrade: %v", err)
			}
			lw := filepath.Join(dir, filepath.FromSlash(fabrikplugin.LocalWorkflowsRelDir))
			after := snapshot(t, lw)

			for rel, content := range before {
				switch rel {
				case ".installed-version":
					if _, ok := after[rel]; ok {
						t.Error(".installed-version must not travel to local-workflows")
					}
				case pluginManifestRel:
					var a, b map[string]any
					if err := json.Unmarshal([]byte(content), &a); err != nil {
						t.Fatal(err)
					}
					if err := json.Unmarshal([]byte(after[rel]), &b); err != nil {
						t.Fatal(err)
					}
					if b["name"] != fabrikplugin.LocalWorkflowsName {
						t.Errorf("manifest name = %v", b["name"])
					}
					b["name"] = a["name"]
					if !reflect.DeepEqual(a, b) {
						t.Errorf("manifest changed beyond name: %v vs %v", a, b)
					}
				default:
					if after[rel] != content {
						t.Errorf("%s not byte-identical after the move", rel)
					}
				}
			}
			assertBaseIsEmbedded(t, dir)

			for k, v := range stageRefs(t, dir) {
				_, skill, _ := fabrikplugin.Split(v)
				want := fabrikplugin.BasePluginName
				if skill == "fabrik-plan" || skill == "fabrik-review" {
					want = fabrikplugin.LocalWorkflowsName
				}
				if v != fabrikplugin.Qualify(want, skill) {
					t.Errorf("%s = %q, want plugin %q", k, v, want)
				}
			}
			refs := stageRefs(t, dir)
			if refs["plan.yaml:skill"] != "local-workflows:fabrik-plan" || refs["research.yaml:skill"] != "fabrik:fabrik-research" {
				t.Errorf("refs = %v", refs)
			}

			first := snapshot(t, filepath.Join(dir, ".fabrik"))
			if err := runUpgrade(nil); err != nil {
				t.Fatalf("second runUpgrade: %v", err)
			}
			if !reflect.DeepEqual(first, snapshot(t, filepath.Join(dir, ".fabrik"))) {
				t.Error("second run changed files")
			}
		})
	}
}

// SC-003: after the migration the deployment no longer gets refused.
func TestMigrate_LaterUpgradeIsNotRefused(t *testing.T) {
	dir := deployFixture(t, "v0.0.77")
	editSkill(t, dir, "fabrik-plan")
	if err := runUpgrade(nil); err != nil {
		t.Fatal(err)
	}
	// Simulate the next release: the base plugin on disk is an older shipped
	// version, recorded as installed.
	pluginDir := filepath.Join(dir, ".fabrik", "plugin")
	if err := os.RemoveAll(pluginDir); err != nil {
		t.Fatal(err)
	}
	copyDirTree(t, filepath.Join(migrationFixtureRoot, "v0.0.70", "plugin"), pluginDir)
	h, _ := fabrikplugin.ComputeDiskVersion(pluginDir)
	if err := fabrikplugin.WriteVersionHash(pluginDir, h); err != nil {
		t.Fatal(err)
	}
	if err := runUpgrade(nil); err != nil {
		t.Fatalf("upgrade after migration refused: %v", err)
	}
	assertBaseIsEmbedded(t, dir)
}

// Interrupted runs: every intermediate state is picked up and completed, and
// the end state equals an uninterrupted run's.
func TestMigrate_ResumesFromEveryIntermediateState(t *testing.T) {
	reference := func() map[string]string {
		dir := deployFixture(t, "v0.0.77")
		editSkill(t, dir, "fabrik-plan")
		editSkill(t, dir, "fabrik-validate")
		if err := runUpgrade(nil); err != nil {
			t.Fatal(err)
		}
		return snapshot(t, filepath.Join(dir, ".fabrik"))
	}()

	setup := func(t *testing.T) (string, string, string) {
		dir := deployFixture(t, "v0.0.77")
		editSkill(t, dir, "fabrik-plan")
		editSkill(t, dir, "fabrik-validate")
		pluginDir := filepath.Join(dir, ".fabrik", "plugin")
		lw := filepath.Join(dir, filepath.FromSlash(fabrikplugin.LocalWorkflowsRelDir))
		if err := os.MkdirAll(filepath.Dir(lw), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(pluginDir, lw); err != nil {
			t.Fatal(err)
		}
		return dir, pluginDir, lw
	}

	t.Run("after the rename", func(t *testing.T) {
		dir, _, _ := setup(t)
		if err := runUpgrade(nil); err != nil {
			t.Fatal(err)
		}
		if got := snapshot(t, filepath.Join(dir, ".fabrik")); !reflect.DeepEqual(got, reference) {
			t.Error("end state differs from an uninterrupted run")
		}
	})
	t.Run("after the manifest rename", func(t *testing.T) {
		dir, _, lw := setup(t)
		_ = os.Remove(filepath.Join(lw, ".installed-version"))
		if _, err := renameManifest(lw, fabrikplugin.LocalWorkflowsName); err != nil {
			t.Fatal(err)
		}
		if err := runUpgrade(nil); err != nil {
			t.Fatal(err)
		}
		if got := snapshot(t, filepath.Join(dir, ".fabrik")); !reflect.DeepEqual(got, reference) {
			t.Error("end state differs from an uninterrupted run")
		}
	})
	t.Run("after the base restore", func(t *testing.T) {
		dir, pluginDir, lw := setup(t)
		_ = os.Remove(filepath.Join(lw, ".installed-version"))
		if _, err := renameManifest(lw, fabrikplugin.LocalWorkflowsName); err != nil {
			t.Fatal(err)
		}
		if _, err := fabrikplugin.RefreshPluginTo(pluginDir); err != nil {
			t.Fatal(err)
		}
		if err := fabrikplugin.WriteInstalledVersion(pluginDir); err != nil {
			t.Fatal(err)
		}
		if err := runUpgrade(nil); err != nil {
			t.Fatal(err)
		}
		if got := snapshot(t, filepath.Join(dir, ".fabrik")); !reflect.DeepEqual(got, reference) {
			t.Error("end state differs from an uninterrupted run")
		}
	})
}

func TestMigrate_NeverOverwritesExistingLocalWorkflows(t *testing.T) {
	dir := deployFixture(t, "v0.0.77")
	editSkill(t, dir, "fabrik-plan")
	lw := filepath.Join(dir, filepath.FromSlash(fabrikplugin.LocalWorkflowsRelDir))
	if err := os.MkdirAll(lw, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lw, "mine.txt"), []byte("mine"), 0644); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, filepath.Join(dir, ".fabrik"))
	err := runUpgrade(nil)
	if err == nil || !strings.Contains(err.Error(), "local customizations") {
		t.Fatalf("want the customisation refusal, got %v", err)
	}
	if !reflect.DeepEqual(before, snapshot(t, filepath.Join(dir, ".fabrik"))) {
		t.Error("blocked migration changed files")
	}
	plan, _ := planMigration(".", true, "")
	if plan.Blocked == "" || plan.Pending() {
		t.Errorf("plan = %+v", plan)
	}
}

func TestMigrate_DevOverrideAndSymlinkAreReportedNotMoved(t *testing.T) {
	dir := deployFixture(t, "v0.0.77")
	editSkill(t, dir, "fabrik-plan")
	plan, err := planMigration(".", true, "/some/vendored/plugin")
	if err != nil || plan.Skip == "" || plan.Pending() {
		t.Fatalf("override plan = %+v err=%v", plan, err)
	}
	var buf bytes.Buffer
	printMigrationNotice(plan, &buf)
	if !strings.Contains(buf.String(), "skipped") {
		t.Errorf("notice = %q", buf.String())
	}

	// A symlinked .fabrik/plugin is a vendored set too.
	target := filepath.Join(t.TempDir(), "vendored")
	if err := os.Rename(filepath.Join(dir, ".fabrik", "plugin"), target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, ".fabrik", "plugin")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	plan, err = planMigration(".", true, "")
	if err != nil || plan.Skip == "" || plan.Pending() {
		t.Fatalf("symlink plan = %+v err=%v", plan, err)
	}
}

func TestMigrate_NoMigrateKeepsRefusalAndStageFiles(t *testing.T) {
	dir := deployFixture(t, "v0.0.77")
	editSkill(t, dir, "fabrik-plan")
	before := snapshot(t, filepath.Join(dir, ".fabrik"))
	if err := runUpgrade([]string{"--no-migrate"}); err == nil {
		t.Fatal("expected the customisation refusal")
	}
	if !reflect.DeepEqual(before, snapshot(t, filepath.Join(dir, ".fabrik"))) {
		t.Error("--no-migrate changed files")
	}
}

func TestMigrate_SharedFileOnlyEditMovesTreeButRedirectsNoStage(t *testing.T) {
	dir := deployFixture(t, "v0.0.77")
	if err := os.WriteFile(filepath.Join(dir, ".fabrik", "plugin", "README.md"), []byte("notes\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := runUpgrade(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(fabrikplugin.LocalWorkflowsRelDir), "README.md")); err != nil {
		t.Errorf("tree not moved: %v", err)
	}
	for k, v := range stageRefs(t, dir) {
		if !strings.HasPrefix(v, "fabrik:") {
			t.Errorf("%s = %q", k, v)
		}
	}
	assertBaseIsEmbedded(t, dir)
}

func TestMigrate_ExtraFilesAndCustomSkillsTravelWithTheTree(t *testing.T) {
	dir := deployFixture(t, "v0.0.77")
	ref := filepath.Join(dir, ".fabrik", "plugin", "skills", "fabrik-plan", "references")
	if err := os.MkdirAll(ref, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ref, "style.md"), []byte("style"), 0644); err != nil {
		t.Fatal(err)
	}
	custom := filepath.Join(dir, ".fabrik", "plugin", "skills", "my-skill")
	if err := os.MkdirAll(custom, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(custom, "SKILL.md"), []byte("mine"), 0644); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(dir, ".fabrik", "stages", "plan.yaml")
	data, _ := os.ReadFile(stage)
	if err := os.WriteFile(stage, append(data, []byte("ci_fix_skill: my-skill\nrebase_skill: not-anywhere\n")...), 0644); err != nil {
		t.Fatal(err)
	}
	if err := runUpgrade(nil); err != nil {
		t.Fatal(err)
	}
	lw := filepath.Join(dir, filepath.FromSlash(fabrikplugin.LocalWorkflowsRelDir))
	for _, rel := range []string{"skills/fabrik-plan/references/style.md", "skills/my-skill/SKILL.md"} {
		if _, err := os.Stat(filepath.Join(lw, filepath.FromSlash(rel))); err != nil {
			t.Errorf("%s did not travel: %v", rel, err)
		}
	}
	// Nothing of theirs is left in the base.
	if _, err := os.Stat(filepath.Join(dir, ".fabrik", "plugin", "skills", "my-skill")); !os.IsNotExist(err) {
		t.Error("custom skill left in the base plugin")
	}
	refs := stageRefs(t, dir)
	if refs["plan.yaml:skill"] != "local-workflows:fabrik-plan" || refs["plan.yaml:ci_fix_skill"] != "local-workflows:my-skill" || refs["plan.yaml:rebase_skill"] != "not-anywhere" {
		t.Errorf("refs = %v", refs)
	}
}

func TestMigrate_AlreadyQualifiedReferencesAreLeftAloneAndReported(t *testing.T) {
	dir := deployFixture(t, "v0.0.77")
	editSkill(t, dir, "fabrik-plan")
	stage := filepath.Join(dir, ".fabrik", "stages", "plan.yaml")
	data, _ := os.ReadFile(stage)
	data = bytes.Replace(data, []byte("skill: fabrik-plan\n"), []byte("skill: fabrik:fabrik-plan\n"), 1)
	if err := os.WriteFile(stage, data, 0644); err != nil {
		t.Fatal(err)
	}
	plan, err := planMigration(".", true, "")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := applyMigration(plan, &buf); err != nil {
		t.Fatal(err)
	}
	if refs := stageRefs(t, dir); refs["plan.yaml:skill"] != "fabrik:fabrik-plan" {
		t.Errorf("qualified reference rewritten: %v", refs)
	}
	if !strings.Contains(buf.String(), "local-workflows:fabrik-plan") {
		t.Errorf("report does not point at the edited copy:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "git add") {
		t.Errorf("report does not tell the operator to review and commit:\n%s", buf.String())
	}
}

func TestMigrate_PendingIsFalseForMigratedDeployment(t *testing.T) {
	dir := deployFixture(t, "v0.0.77")
	editSkill(t, dir, "fabrik-plan")
	plan, _ := planMigration(".", true, "")
	if !plan.Pending() {
		t.Fatal("customised deployment should be pending")
	}
	if err := runUpgrade(nil); err != nil {
		t.Fatal(err)
	}
	plan, _ = planMigration(".", true, "")
	if plan.Pending() {
		t.Errorf("migrated deployment still pending: %+v", plan)
	}
	_ = dir
}

func TestMigrate_UsesBasePluginNameConstant(t *testing.T) {
	src, err := os.ReadFile("upgrade_migrate.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ReplaceAll(string(src), "BasePluginName", ""), `"fabrik:`) {
		t.Error("upgrade_migrate.go hard-codes a qualified fabrik name; use plugin.BasePluginName")
	}
}

func TestMigrate_PendingDetailNamesWhatWillChange(t *testing.T) {
	dir := deployFixture(t, "v0.0.77")
	editSkill(t, dir, "fabrik-plan")
	plan, err := planMigration(".", true, "")
	if err != nil {
		t.Fatal(err)
	}
	d := migrationPendingDetail(plan)
	for _, want := range []string{"local edits", "local-workflows", "stage file(s)"} {
		if !strings.Contains(d, want) {
			t.Errorf("detail %q lacks %q", d, want)
		}
	}
}

// Bare names nothing resolves cannot be migrated, so they must not keep the
// "pending" entry raised.
func TestMigrate_UnresolvedBareNamesAreNotPending(t *testing.T) {
	dir := deployFixture(t, "v0.0.77")
	files, _ := filepath.Glob(filepath.Join(dir, ".fabrik", "stages", "*.yaml"))
	for _, f := range files {
		data, _ := os.ReadFile(f)
		out := string(data)
		for _, s := range fabrikplugin.ShippedSkillNames() {
			out = strings.ReplaceAll(out, ": "+s+"\n", ": fabrik:"+s+"\n")
		}
		if err := os.WriteFile(f, []byte(out), 0644); err != nil {
			t.Fatal(err)
		}
	}
	data, _ := os.ReadFile(files[0])
	if err := os.WriteFile(files[0], append(data, []byte("rebase_skill: someone-elses-skill\n")...), 0644); err != nil {
		t.Fatal(err)
	}
	plan, err := planMigration(".", true, "")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Pending() || len(plan.Notes) == 0 {
		t.Errorf("pending=%v notes=%v", plan.Pending(), plan.Notes)
	}
}

// An operator's own plugin that already lives at .fabrik/plugins/local-workflows
// under another name is not a moved copy of the base: the migration must not
// rename its manifest (that would break every reference to its name) nor point
// any stage at it as "local-workflows:".
func TestMigrate_OperatorOwnPluginInLocalWorkflowsIsLeftAlone(t *testing.T) {
	dir := deployFixture(t, "v0.0.77")
	lw := filepath.Join(dir, filepath.FromSlash(fabrikplugin.LocalWorkflowsRelDir))
	skillDir := filepath.Join(lw, "skills", "acme-skill")
	if err := os.MkdirAll(filepath.Join(lw, ".claude-plugin"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		t.Fatal(err)
	}
	manifest := []byte("{\n  \"name\": \"acme\",\n  \"version\": \"1.0.0\"\n}\n")
	if err := os.WriteFile(filepath.Join(lw, ".claude-plugin", "plugin.json"), manifest, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("acme\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// A stage using the operator's bare skill name must stay bare.
	plan := filepath.Join(dir, ".fabrik", "stages", "plan.yaml")
	data, err := os.ReadFile(plan)
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, []byte("comment_skill: acme-skill\n")...)
	if err := os.WriteFile(plan, data, 0644); err != nil {
		t.Fatal(err)
	}
	beforeLW := snapshot(t, lw)

	if err := runUpgrade(nil); err != nil {
		t.Fatal(err)
	}
	if got := snapshot(t, lw); !reflect.DeepEqual(beforeLW, got) {
		t.Errorf("the operator's own plugin was modified: before %v after %v", beforeLW, got)
	}
	refs := stageRefs(t, dir)
	if got := refs["plan.yaml:comment_skill"]; got != "acme-skill" {
		t.Errorf("comment_skill = %q, want the bare operator name left alone", got)
	}
	for k, v := range refs {
		if strings.HasPrefix(v, fabrikplugin.LocalWorkflowsName+":") {
			t.Errorf("%s points at %s although local-workflows is not that plugin", k, v)
		}
	}
	assertBaseIsEmbedded(t, dir)
}

// A manifest whose nested author.name comes before the top-level name (any tool
// that sorts keys does this) must be renamed at the top level, not at the first
// "name" token.
func TestRenameManifest_TargetsTopLevelNameNotNestedOne(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude-plugin", "plugin.json")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	src := "{\n  \"author\": {\n    \"name\": \"Someone\"\n  },\n  \"name\": \"fabrik\",\n  \"version\": \"1.0.0\"\n}\n"
	if err := os.WriteFile(path, []byte(src), 0644); err != nil {
		t.Fatal(err)
	}
	changed, err := renameManifest(dir, fabrikplugin.LocalWorkflowsName)
	if err != nil || !changed {
		t.Fatalf("renameManifest = %v, %v; want changed without error", changed, err)
	}
	got, _ := os.ReadFile(path)
	want := strings.Replace(src, `"name": "fabrik"`, `"name": "local-workflows"`, 1)
	if string(got) != want {
		t.Errorf("manifest = %q, want %q", got, want)
	}
}

// A manifest that cannot be renamed refuses the migration at plan time, before
// anything is moved, so the deployment is never left without a base plugin.
func TestMigrate_UnrenamableManifestRefusesBeforeMovingAnything(t *testing.T) {
	dir := deployFixture(t, "v0.0.77")
	editSkill(t, dir, "fabrik-plan")
	manifest := filepath.Join(dir, ".fabrik", "plugin", ".claude-plugin", "plugin.json")
	if err := os.WriteFile(manifest, []byte(`{"name": 7}`), 0644); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, filepath.Join(dir, ".fabrik"))

	p, err := planMigration(dir, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if p.Blocked == "" || p.Pending() {
		t.Fatalf("plan = %+v, want Blocked and not pending", p)
	}
	if err := runUpgrade(nil); err == nil || !strings.Contains(err.Error(), "local customizations") {
		t.Fatalf("want the customisation refusal, got %v", err)
	}
	if got := snapshot(t, filepath.Join(dir, ".fabrik")); !reflect.DeepEqual(before, got) {
		t.Errorf("a refused migration changed the deployment")
	}
}
