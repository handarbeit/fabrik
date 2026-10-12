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
	return deployFixtureOpts(t, tag, true)
}

// deployFixtureNoVersion is deployFixture without .installed-version. With the
// pristine old disk differing from the current embed, CheckPluginState then
// reports local changes, so the per-file shipped table is what decides whether
// the tree is really customised.
func deployFixtureNoVersion(t *testing.T, tag string) string {
	t.Helper()
	return deployFixtureOpts(t, tag, false)
}

func deployFixtureOpts(t *testing.T, tag string, seedVersion bool) string {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(migrationFixtureRoot, tag)
	copyDirTree(t, filepath.Join(root, "plugin"), filepath.Join(dir, ".fabrik", "plugin"))
	copyDirTree(t, filepath.Join(root, "stages"), filepath.Join(dir, ".fabrik", "stages"))
	pluginDir := filepath.Join(dir, ".fabrik", "plugin")
	if seedVersion {
		h, err := fabrikplugin.ComputeDiskVersion(pluginDir)
		if err != nil {
			t.Fatal(err)
		}
		if err := fabrikplugin.WriteVersionHash(pluginDir, h); err != nil {
			t.Fatal(err)
		}
	}
	chdirTest(t, dir)
	return dir
}

// requireCustom fails unless CheckPluginState already reports local changes, so
// the test cannot silently stop consulting the per-file table.
func requireCustom(t *testing.T, dir string) {
	t.Helper()
	pluginDir := filepath.Join(dir, ".fabrik", "plugin")
	if _, err := os.Stat(filepath.Join(pluginDir, ".installed-version")); !os.IsNotExist(err) {
		t.Fatalf(".installed-version must be absent: %v", err)
	}
	custom, _, _, err := fabrikplugin.CheckPluginState(pluginDir, true)
	if err != nil {
		t.Fatal(err)
	}
	if !custom {
		t.Fatal("CheckPluginState must report local changes, else the per-file table is never consulted")
	}
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

// An uncustomised old deployment whose .installed-version is missing is judged
// by the per-file shipped table alone: nothing moves, the base is restored and
// every reference is qualified to the base plugin.
func TestMigrate_UncustomisedWithoutInstalledVersion(t *testing.T) {
	for _, tag := range migrationTags {
		t.Run(tag, func(t *testing.T) {
			dir := deployFixtureNoVersion(t, tag)
			requireCustom(t, dir)
			if err := runUpgrade(nil); err != nil {
				t.Fatalf("runUpgrade: %v", err)
			}
			if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(fabrikplugin.LocalWorkflowsRelDir))); !os.IsNotExist(err) {
				t.Error("nothing may be moved to local-workflows for an uncustomised deployment")
			}
			for k, v := range stageRefs(t, dir) {
				if !strings.HasPrefix(v, fabrikplugin.BasePluginName+":") {
					t.Errorf("%s = %q, want a %s-qualified reference", k, v, fabrikplugin.BasePluginName)
				}
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

// With .installed-version missing and exactly one skill hand-edited, the tree
// moves and only references to that skill are redirected to local-workflows.
func TestMigrate_EditedWithoutInstalledVersionScopesToEditedSkill(t *testing.T) {
	for _, tag := range migrationTags {
		t.Run(tag, func(t *testing.T) {
			dir := deployFixtureNoVersion(t, tag)
			editSkill(t, dir, "fabrik-plan")
			requireCustom(t, dir)
			edited, err := os.ReadFile(filepath.Join(dir, ".fabrik", "plugin", "skills", "fabrik-plan", "SKILL.md"))
			if err != nil {
				t.Fatal(err)
			}

			if err := runUpgrade(nil); err != nil {
				t.Fatalf("runUpgrade: %v", err)
			}
			lw := filepath.Join(dir, filepath.FromSlash(fabrikplugin.LocalWorkflowsRelDir))
			moved, err := os.ReadFile(filepath.Join(lw, "skills", "fabrik-plan", "SKILL.md"))
			if err != nil {
				t.Fatalf("edited skill not moved to local-workflows: %v", err)
			}
			if !bytes.Equal(moved, edited) {
				t.Error("edited skill not byte-identical after the move")
			}
			assertBaseIsEmbedded(t, dir)

			refs := stageRefs(t, dir)
			if refs["plan.yaml:skill"] != "local-workflows:fabrik-plan" {
				t.Errorf("plan.yaml:skill = %q, want local-workflows:fabrik-plan", refs["plan.yaml:skill"])
			}
			for k, v := range refs {
				if k == "plan.yaml:skill" {
					continue
				}
				if !strings.HasPrefix(v, fabrikplugin.BasePluginName+":") {
					t.Errorf("%s = %q, want a %s-qualified reference (only the edited skill is redirected)", k, v, fabrikplugin.BasePluginName)
				}
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
			if refs["plan.yaml:skill"] != "local-workflows:fabrik-plan" || refs["research.yaml:skill"] != "fabrik-stages:research" {
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
	// The state the loader refuses with a pointer to 'fabrik upgrade': the base
	// plugin recreated (fabrik init, a hand copy) while the moved tree still
	// declares the base name.
	t.Run("base recreated before the manifest rename", func(t *testing.T) {
		dir, pluginDir, _ := setup(t)
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
		if !strings.HasPrefix(v, fabrikplugin.BasePluginName+":") {
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

func TestMigrate_PreRenameQualifiedReferenceToEditedSkillIsRedirected(t *testing.T) {
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
	// A pre-rename qualified reference to an edited skill is redirected to the
	// moved copy, so the edit survives the rename (ADR 2140).
	if refs := stageRefs(t, dir); refs["plan.yaml:skill"] != "local-workflows:fabrik-plan" {
		t.Errorf("edited skill not redirected to local-workflows: %v", refs)
	}
	if !strings.Contains(buf.String(), "fabrik:fabrik-plan -> local-workflows:fabrik-plan") {
		t.Errorf("report does not show the redirect:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "git add") {
		t.Errorf("report does not tell the operator to review and commit:\n%s", buf.String())
	}
}

// A stage file already on the new name, over a plugin tree whose fabrik-plan was
// edited before the rename: the reference is left alone, and the note names the
// operator's edited copy under its on-disk (pre-rename) name.
func TestMigrate_NewNameReferenceOverPreRenameEditedSkillIsReported(t *testing.T) {
	dir := deployFixture(t, "v0.0.77")
	editSkill(t, dir, "fabrik-plan")
	stage := filepath.Join(dir, ".fabrik", "stages", "plan.yaml")
	data, _ := os.ReadFile(stage)
	data = bytes.Replace(data, []byte("skill: fabrik-plan\n"), []byte("skill: fabrik-stages:plan\n"), 1)
	if err := os.WriteFile(stage, data, 0644); err != nil {
		t.Fatal(err)
	}
	plan, err := planMigration(".", true, "")
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, n := range plan.Notes {
		if strings.Contains(n, "fabrik-stages:plan") && strings.Contains(n, "local-workflows:fabrik-plan") {
			found = true
		}
	}
	if !found {
		t.Errorf("no note pointing at the edited copy; notes = %v", plan.Notes)
	}
	if refs := stageRefs(t, dir); refs["plan.yaml:skill"] != "fabrik-stages:plan" {
		t.Errorf("reference was rewritten: %v", refs)
	}
}

// An old-form reference over a plugin tree the binary already refreshed to the
// renamed plugin, whose skills/plan the operator then edited: at runtime the
// legacy mapping sends the reference to that edited copy, so the migration must
// point it at the moved copy under its new name rather than at the restored
// stock skill.
func TestMigrate_OldFormReferenceOverNewNameEditIsRedirected(t *testing.T) {
	dir := t.TempDir()
	pluginDir := filepath.Join(dir, ".fabrik", "plugin")
	if _, err := fabrikplugin.RefreshPluginTo(pluginDir); err != nil {
		t.Fatal(err)
	}
	if err := fabrikplugin.WriteInstalledVersion(pluginDir); err != nil {
		t.Fatal(err)
	}
	skill := filepath.Join(pluginDir, "skills", "plan", "SKILL.md")
	data, _ := os.ReadFile(skill)
	if err := os.WriteFile(skill, append(data, []byte("\nLocal rule.\n")...), 0644); err != nil {
		t.Fatal(err)
	}
	stagesDir := filepath.Join(dir, ".fabrik", "stages")
	if err := os.MkdirAll(stagesDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stagesDir, "plan.yaml"), []byte("name: Plan\nskill: fabrik:fabrik-plan\ncomment_skill: fabrik-plan-comment\n"), 0644); err != nil {
		t.Fatal(err)
	}
	chdirTest(t, dir)
	plan, err := planMigration(".", true, "")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := applyMigration(plan, &buf); err != nil {
		t.Fatal(err)
	}
	refs := stageRefs(t, dir)
	if refs["plan.yaml:skill"] != "local-workflows:plan" {
		t.Errorf("edited skill not redirected to the moved copy: %v", refs)
	}
	if refs["plan.yaml:comment_skill"] != "fabrik-stages:plan-comment" {
		t.Errorf("unedited skill not moved to the base plugin: %v", refs)
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
		for _, old := range fabrikplugin.LegacySkillNames() {
			newRef, _ := fabrikplugin.LegacyRef(old)
			out = strings.ReplaceAll(out, ": "+old+"\n", ": "+newRef+"\n")
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

// A reference qualified with the current base name to a skill the operator
// edited (a customised copy of the renamed plugin) is left alone and reported:
// the edit lives in local-workflows under the same skill name.
func TestMigrate_CurrentQualifiedReferenceToEditedSkillIsLeftAloneAndReported(t *testing.T) {
	dir := t.TempDir()
	pluginDir := filepath.Join(dir, ".fabrik", "plugin")
	if _, err := fabrikplugin.RefreshPluginTo(pluginDir); err != nil {
		t.Fatal(err)
	}
	if err := fabrikplugin.WriteInstalledVersion(pluginDir); err != nil {
		t.Fatal(err)
	}
	skill := filepath.Join(pluginDir, "skills", "plan", "SKILL.md")
	data, _ := os.ReadFile(skill)
	if err := os.WriteFile(skill, append(data, []byte("\nLocal rule.\n")...), 0644); err != nil {
		t.Fatal(err)
	}
	stagesDir := filepath.Join(dir, ".fabrik", "stages")
	if err := os.MkdirAll(stagesDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stagesDir, "plan.yaml"), []byte("name: Plan\nskill: fabrik-stages:plan\ncomment_skill: plan-comment\n"), 0644); err != nil {
		t.Fatal(err)
	}
	chdirTest(t, dir)
	plan, err := planMigration(".", true, "")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := applyMigration(plan, &buf); err != nil {
		t.Fatal(err)
	}
	refs := stageRefs(t, dir)
	if refs["plan.yaml:skill"] != "fabrik-stages:plan" || refs["plan.yaml:comment_skill"] != "fabrik-stages:plan-comment" {
		t.Errorf("refs = %v", refs)
	}
	if !strings.Contains(buf.String(), "local-workflows:plan") {
		t.Errorf("report does not point at the edited copy:\n%s", buf.String())
	}
}

// qualifyLegacyBare rewrites every bare fabrik-<stage> skill value in the
// deployment's stage files to the qualified fabrik:fabrik-<stage> form that
// #2137/#2139 wrote, i.e. the layout a deployment has between that release and
// the #2140 rename. Other values are untouched.
func qualifyLegacyBare(t *testing.T, dir string) {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(dir, ".fabrik", "stages", "*.yaml"))
	for _, f := range files {
		data, _ := os.ReadFile(f)
		out := string(data)
		for _, old := range fabrikplugin.LegacySkillNames() {
			out = strings.ReplaceAll(out, ": "+old+"\n", ": fabrik:"+old+"\n")
		}
		if err := os.WriteFile(f, []byte(out), 0644); err != nil {
			t.Fatal(err)
		}
	}
}

func assertNoLegacySkillDirs(t *testing.T, dir string) {
	t.Helper()
	for _, old := range fabrikplugin.LegacySkillNames() {
		if _, err := os.Stat(filepath.Join(dir, ".fabrik", "plugin", "skills", old)); !os.IsNotExist(err) {
			t.Errorf("leftover pre-rename skills/%s in the base plugin (err=%v)", old, err)
		}
	}
}

func assertStageRefsAreCurrent(t *testing.T, dir string, local map[string]bool) {
	t.Helper()
	for k, v := range stageRefs(t, dir) {
		pn, skill, q := fabrikplugin.Split(v)
		switch {
		case !q:
			t.Errorf("%s = %q is still bare", k, v)
		case local[skill] && pn == fabrikplugin.LocalWorkflowsName:
		case pn == fabrikplugin.BasePluginName && !strings.HasPrefix(skill, "fabrik-"):
		default:
			t.Errorf("%s = %q, want a %s:<skill> reference (or local-workflows for edited %v)", k, v, fabrikplugin.BasePluginName, local)
		}
	}
}

// SC-002 (#2140): deployments from three earlier releases, in the qualified
// form #2137/#2139 produced, reach the renamed plugin in one `fabrik upgrade`,
// with no pre-rename skill left behind and a second run changing nothing.
func TestMigrate_Rename_QualifiedFormFromEarlierReleases(t *testing.T) {
	for _, tag := range migrationTags {
		t.Run(tag, func(t *testing.T) {
			dir := deployFixture(t, tag)
			qualifyLegacyBare(t, dir)
			for k, v := range stageRefs(t, dir) {
				if !strings.HasPrefix(v, "fabrik:fabrik-") {
					t.Fatalf("fixture precondition: %s = %q", k, v)
				}
			}
			if err := runUpgrade(nil); err != nil {
				t.Fatalf("runUpgrade: %v", err)
			}
			assertStageRefsAreCurrent(t, dir, nil)
			assertNoLegacySkillDirs(t, dir)
			assertBaseIsEmbedded(t, dir)
			if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(fabrikplugin.LocalWorkflowsRelDir))); !os.IsNotExist(err) {
				t.Error("local-workflows must not be created for an uncustomised deployment")
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

// The customised variant, qualified form: the edited skills end up referenced
// as local-workflows:fabrik-<x> (the moved copy keeps its names), everything
// else as fabrik-stages:<x>, and the moved plugin is named local-workflows.
func TestMigrate_Rename_CustomisedQualifiedFormKeepsEdits(t *testing.T) {
	for _, tag := range migrationTags {
		t.Run(tag, func(t *testing.T) {
			dir := deployFixture(t, tag)
			editSkill(t, dir, "fabrik-plan")
			editSkill(t, dir, "fabrik-review")
			qualifyLegacyBare(t, dir)
			if err := runUpgrade(nil); err != nil {
				t.Fatalf("runUpgrade: %v", err)
			}
			refs := stageRefs(t, dir)
			if refs["plan.yaml:skill"] != "local-workflows:fabrik-plan" || refs["review.yaml:comment_skill"] != "fabrik-stages:review-comment" || refs["review.yaml:skill"] != "local-workflows:fabrik-review" || refs["research.yaml:skill"] != "fabrik-stages:research" {
				t.Errorf("refs = %v", refs)
			}
			lw := filepath.Join(dir, filepath.FromSlash(fabrikplugin.LocalWorkflowsRelDir))
			if name, err := readManifestName(lw); err != nil || name != fabrikplugin.LocalWorkflowsName {
				t.Errorf("moved plugin name = %q err=%v", name, err)
			}
			if _, err := os.Stat(filepath.Join(lw, "skills", "fabrik-review", "SKILL.md")); err != nil {
				t.Errorf("edited skill lost: %v", err)
			}
			assertNoLegacySkillDirs(t, dir)
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

// A deployment already migrated by #2139 (an existing local-workflows copy that
// keeps the pre-rename names, stage files in the qualified form) reaches the
// same end state, and references to other plugins are byte-unchanged.
func TestMigrate_Rename_AlreadyMigratedDeployment(t *testing.T) {
	dir := deployFixture(t, "v0.0.77")
	editSkill(t, dir, "fabrik-plan")
	// Run the pre-rename migration shape by hand: move the edited tree aside as
	// #2139 did, leaving the manifest named local-workflows and a pristine base
	// from an earlier release.
	pluginDir := filepath.Join(dir, ".fabrik", "plugin")
	lw := filepath.Join(dir, filepath.FromSlash(fabrikplugin.LocalWorkflowsRelDir))
	if err := os.MkdirAll(filepath.Dir(lw), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(pluginDir, lw); err != nil {
		t.Fatal(err)
	}
	if _, err := renameManifest(lw, fabrikplugin.LocalWorkflowsName); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(lw, ".installed-version"))
	copyDirTree(t, filepath.Join(migrationFixtureRoot, "v0.0.77", "plugin"), pluginDir)
	h, _ := fabrikplugin.ComputeDiskVersion(pluginDir)
	if err := fabrikplugin.WriteVersionHash(pluginDir, h); err != nil {
		t.Fatal(err)
	}
	qualifyLegacyBare(t, dir)
	other := filepath.Join(dir, ".fabrik", "stages", "plan.yaml")
	data, _ := os.ReadFile(other)
	data = bytes.Replace(data, []byte("skill: fabrik:fabrik-plan\n"), []byte("skill: local-workflows:fabrik-plan\n"), 1)
	data = append(data, []byte("rebase_skill: myorg:rebase\n")...)
	if err := os.WriteFile(other, data, 0644); err != nil {
		t.Fatal(err)
	}
	lwBefore := snapshot(t, lw)

	if err := runUpgrade(nil); err != nil {
		t.Fatalf("runUpgrade: %v", err)
	}
	refs := stageRefs(t, dir)
	if refs["plan.yaml:skill"] != "local-workflows:fabrik-plan" || refs["plan.yaml:rebase_skill"] != "myorg:rebase" || refs["research.yaml:skill"] != "fabrik-stages:research" {
		t.Errorf("refs = %v", refs)
	}
	if !reflect.DeepEqual(lwBefore, snapshot(t, lw)) {
		t.Error("local-workflows was modified")
	}
	assertNoLegacySkillDirs(t, dir)
	assertBaseIsEmbedded(t, dir)
	first := snapshot(t, filepath.Join(dir, ".fabrik"))
	if err := runUpgrade(nil); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, snapshot(t, filepath.Join(dir, ".fabrik"))) {
		t.Error("second run changed files")
	}
}
