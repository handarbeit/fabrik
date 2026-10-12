package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/handarbeit/fabrik/engine"
	fabrikplugin "github.com/handarbeit/fabrik/plugin"
)

// projectFixture lays out an unmodified, already-renamed base plugin in
// .fabrik/plugin, one stage file with the given YAML body, and chdirs into it.
func projectFixture(t *testing.T, stageYAML string) string {
	t.Helper()
	dir := t.TempDir()
	pluginDir := filepath.Join(dir, ".fabrik", "plugin")
	if _, err := fabrikplugin.RefreshPluginTo(pluginDir); err != nil {
		t.Fatal(err)
	}
	if err := fabrikplugin.WriteInstalledVersion(pluginDir); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(dir, ".fabrik", "stages", "review.yaml"), stageYAML)
	chdirTest(t, dir)
	t.Setenv("FABRIK_PLUGIN_DIRS", "")
	return dir
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

// addProjectPlugin writes a plugin named name under rel (relative to dir) that
// provides the given skills.
func addProjectPlugin(t *testing.T, dir, rel, name string, skills ...string) string {
	t.Helper()
	root := filepath.Join(dir, filepath.FromSlash(rel))
	writeTestFile(t, filepath.Join(root, ".claude-plugin", "plugin.json"), `{"name":"`+name+`"}`)
	for _, s := range skills {
		writeTestFile(t, filepath.Join(root, "skills", s, "SKILL.md"), "---\nname: "+s+"\n---\n"+name+"\n")
	}
	return root
}

func writePluginDirsConfig(t *testing.T, dir string, rels ...string) {
	t.Helper()
	body := "plugin_dirs:\n"
	for _, r := range rels {
		body += "  - " + r + "\n"
	}
	writeTestFile(t, filepath.Join(dir, ".fabrik", "config.yaml"), body)
}

// resolveStage resolves the stage's skill with the production resolver against
// the plugin set the daemon would load: the base, then the extras.
func resolveStage(t *testing.T, dir string, extras []string, ref string) (qualified, file string) {
	t.Helper()
	dirs := []string{filepath.Join(dir, ".fabrik", "plugin")}
	dirs = append(dirs, extras...)
	q, f, ok := engine.ResolveSkillForTest(dirs, ref)
	if !ok {
		t.Fatalf("%q does not resolve against %v", ref, dirs)
	}
	return q, f
}

func runMigration(t *testing.T, apply bool) (*migrationPlan, string) {
	t.Helper()
	plan, err := planMigration(".", true, "")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if apply {
		if err := applyMigration(plan, &buf); err != nil {
			t.Fatal(err)
		}
	}
	return plan, buf.String()
}

// FR-002 / SC-001: a stage that names fabrik-review runs the same skill file
// before and after `fabrik upgrade` when a project plugin provides it.
func TestProjectPlugin_UpgradeKeepsTheProjectSkill(t *testing.T) {
	dir := projectFixture(t, "name: Review\nskill: fabrik-review\n")
	team := addProjectPlugin(t, dir, ".fabrik/plugins/team", "team", "fabrik-review")
	writePluginDirsConfig(t, dir, ".fabrik/plugins/team")

	_, beforeFile := resolveStage(t, dir, []string{team}, "fabrik-review")
	if !strings.HasPrefix(beforeFile, team) {
		t.Fatalf("precondition: the runtime should pick the project plugin, got %s", beforeFile)
	}

	_, report := runMigration(t, true)
	got := stageRefs(t, dir)["review.yaml:skill"]
	if got != "team:fabrik-review" {
		t.Fatalf("skill = %q, want team:fabrik-review", got)
	}
	_, afterFile := resolveStage(t, dir, []string{team}, got)
	if afterFile != beforeFile {
		t.Errorf("resolved skill changed: before %s, after %s", beforeFile, afterFile)
	}
	for _, want := range []string{"review.yaml", "skill", "team"} {
		if !strings.Contains(report, want) {
			t.Errorf("report does not name %q:\n%s", want, report)
		}
	}
	if !strings.Contains(report, `qualified to "team:fabrik-review"`) {
		t.Errorf("report lacks the qualification note:\n%s", report)
	}

	// Idempotent.
	plan, _ := runMigration(t, false)
	if plan.Pending() || len(plan.StageEdits) != 0 {
		t.Errorf("second run still pending: %+v", plan)
	}
}

// SC-003: restoring the always-base rewrite makes the same assertion fail.
func TestProjectPlugin_NonVacuous(t *testing.T) {
	dir := projectFixture(t, "name: Review\nskill: fabrik-review\n")
	team := addProjectPlugin(t, dir, ".fabrik/plugins/team", "team", "fabrik-review")
	writePluginDirsConfig(t, dir, ".fabrik/plugins/team")
	_, beforeFile := resolveStage(t, dir, []string{team}, "fabrik-review")

	projectPluginLookupDisabledForTest = true
	t.Cleanup(func() { projectPluginLookupDisabledForTest = false })
	runMigration(t, true)
	got := stageRefs(t, dir)["review.yaml:skill"]
	if got != "fabrik-stages:review" {
		t.Fatalf("with the lookup disabled the old rewrite should apply, got %q", got)
	}
	if _, afterFile := resolveStage(t, dir, []string{team}, got); afterFile == beforeFile {
		t.Error("the before/after assertion would not have caught the always-base rewrite")
	}
}

func TestProjectPlugin_NoProviderStillGoesToBase(t *testing.T) {
	dir := projectFixture(t, "name: Review\nskill: fabrik-review\ncomment_skill: fabrik-review-comment\n")
	addProjectPlugin(t, dir, ".fabrik/plugins/team", "team", "something-else")
	writePluginDirsConfig(t, dir, ".fabrik/plugins/team")
	runMigration(t, true)
	refs := stageRefs(t, dir)
	if refs["review.yaml:skill"] != "fabrik-stages:review" || refs["review.yaml:comment_skill"] != "fabrik-stages:review-comment" {
		t.Errorf("refs = %v", refs)
	}
}

// FR-006: the first provider in load order wins and the report names the rest.
func TestProjectPlugin_FirstInLoadOrderWins(t *testing.T) {
	dir := projectFixture(t, "name: Review\nskill: fabrik-review\n")
	b := addProjectPlugin(t, dir, ".fabrik/plugins/b", "b", "fabrik-review")
	a := addProjectPlugin(t, dir, ".fabrik/plugins/a", "a", "fabrik-review")
	writePluginDirsConfig(t, dir, ".fabrik/plugins/b", ".fabrik/plugins/a")
	_, wantFile := resolveStage(t, dir, []string{b, a}, "fabrik-review")

	plan, report := runMigration(t, true)
	got := stageRefs(t, dir)["review.yaml:skill"]
	if got != "b:fabrik-review" {
		t.Fatalf("skill = %q, want b:fabrik-review (config order, not alphabetical)", got)
	}
	if _, f := resolveStage(t, dir, []string{b, a}, got); f != wantFile {
		t.Errorf("provider changed: %s vs %s", f, wantFile)
	}
	if !strings.Contains(report, "also provided by a:fabrik-review") {
		t.Errorf("report does not list the other provider:\n%s", report)
	}
	_ = plan
}

// FABRIK_PLUGIN_DIRS wins over plugin_dirs in config.yaml, as in the daemon.
func TestProjectPlugin_EnvOverridesConfig(t *testing.T) {
	dir := projectFixture(t, "name: Review\nskill: fabrik-review\n")
	addProjectPlugin(t, dir, ".fabrik/plugins/cfg", "cfg", "fabrik-review")
	addProjectPlugin(t, dir, ".fabrik/plugins/envp", "envp", "fabrik-review")
	writePluginDirsConfig(t, dir, ".fabrik/plugins/cfg")
	t.Setenv("FABRIK_PLUGIN_DIRS", ".fabrik/plugins/envp")
	runMigration(t, true)
	if got := stageRefs(t, dir)["review.yaml:skill"]; got != "envp:fabrik-review" {
		t.Errorf("skill = %q", got)
	}
}

// FR-005: a moved local-workflows copy is not a project plugin. Its unedited
// skills go to an extra that provides them (else the base), its edited ones stay
// in local-workflows.
func TestProjectPlugin_MovedCopyIsNotAProjectPlugin(t *testing.T) {
	dir := deployFixture(t, "v0.0.77")
	editSkill(t, dir, "fabrik-plan")
	addProjectPlugin(t, dir, ".fabrik/plugins/team", "team", "fabrik-review")
	writePluginDirsConfig(t, dir, ".fabrik/plugins/team")
	t.Setenv("FABRIK_PLUGIN_DIRS", "")
	writeTestFile(t, filepath.Join(dir, ".fabrik", "stages", "mix.yaml"),
		"name: Mix\nskill: fabrik-plan\ncomment_skill: fabrik-review\nci_fix_skill: fabrik-implement\n")

	runMigration(t, true)
	refs := stageRefs(t, dir)
	if refs["mix.yaml:skill"] != "local-workflows:plan" && refs["mix.yaml:skill"] != "local-workflows:fabrik-plan" {
		t.Errorf("edited skill not kept in local-workflows: %v", refs)
	}
	if refs["mix.yaml:comment_skill"] != "team:fabrik-review" {
		t.Errorf("unedited skill an extra provides should go to the extra: %v", refs)
	}
	if refs["mix.yaml:ci_fix_skill"] != "fabrik-stages:implement" {
		t.Errorf("unedited skill nobody else provides should go to the base: %v", refs)
	}
}

// An operator's edit of a base skill outranks a project plugin that provides the
// same bare name: the edit lives in the moved copy, which the runtime loaded
// ahead of the extras, so the edited skill is what the stage ran (FR-005).
func TestProjectPlugin_EditedBaseSkillOutranksExtra(t *testing.T) {
	dir := deployFixture(t, "v0.0.77")
	editSkill(t, dir, "fabrik-plan")
	addProjectPlugin(t, dir, ".fabrik/plugins/team", "team", "fabrik-plan")
	writePluginDirsConfig(t, dir, ".fabrik/plugins/team")
	t.Setenv("FABRIK_PLUGIN_DIRS", "")
	writeTestFile(t, filepath.Join(dir, ".fabrik", "stages", "mix.yaml"),
		"name: Mix\nskill: fabrik-plan\n")

	runMigration(t, true)
	got := stageRefs(t, dir)["mix.yaml:skill"]
	if got != "local-workflows:fabrik-plan" && got != "local-workflows:plan" {
		t.Errorf("edited base skill should stay in local-workflows, not the extra: %q", got)
	}
}

// An operator's own local-workflows (a differently named plugin) is a project plugin.
func TestProjectPlugin_OperatorOwnLocalWorkflowsCounts(t *testing.T) {
	dir := projectFixture(t, "name: Review\nskill: fabrik-review\n")
	addProjectPlugin(t, dir, ".fabrik/plugins/local-workflows", "mine", "fabrik-review")
	runMigration(t, true)
	if got := stageRefs(t, dir)["review.yaml:skill"]; got != "mine:fabrik-review" {
		t.Errorf("skill = %q", got)
	}
}

// A bare name that is not a shipped skill but an extra provides is qualified to it.
func TestProjectPlugin_NonShippedBareNameProvidedByExtra(t *testing.T) {
	dir := projectFixture(t, "name: Review\nskill: house-review\n")
	addProjectPlugin(t, dir, ".fabrik/plugins/team", "team", "house-review")
	writePluginDirsConfig(t, dir, ".fabrik/plugins/team")
	runMigration(t, true)
	if got := stageRefs(t, dir)["review.yaml:skill"]; got != "team:house-review" {
		t.Errorf("skill = %q", got)
	}
}

// The explicit pre-rename qualified form is not second-guessed.
func TestProjectPlugin_QualifiedLegacyRefUntouchedByLookup(t *testing.T) {
	dir := projectFixture(t, "name: Review\nskill: fabrik:fabrik-review\n")
	addProjectPlugin(t, dir, ".fabrik/plugins/team", "team", "fabrik-review")
	writePluginDirsConfig(t, dir, ".fabrik/plugins/team")
	runMigration(t, true)
	if got := stageRefs(t, dir)["review.yaml:skill"]; got != "fabrik-stages:review" {
		t.Errorf("skill = %q", got)
	}
}

// An unreadable extra never fails the upgrade: it is reported and the base rewrite applies.
func TestProjectPlugin_UnreadableExtraFallsBackWithNote(t *testing.T) {
	dir := projectFixture(t, "name: Review\nskill: fabrik-review\n")
	writeTestFile(t, filepath.Join(dir, ".fabrik", "plugins", "nomanifest", "skills", "x", "SKILL.md"), "x")
	writePluginDirsConfig(t, dir, ".fabrik/plugins/missing", ".fabrik/plugins/nomanifest")
	plan, report := runMigration(t, true)
	if got := stageRefs(t, dir)["review.yaml:skill"]; got != "fabrik-stages:review" {
		t.Errorf("skill = %q", got)
	}
	if !strings.Contains(report, "fall back to the base plugin") {
		t.Errorf("fallback not reported:\n%s", report)
	}
	_ = plan
}

// An extra that repeats a name already used, or names the base, is skipped.
func TestProjectPlugin_DuplicateAndBaseNamedExtrasSkipped(t *testing.T) {
	dir := projectFixture(t, "name: Review\nskill: fabrik-review\n")
	addProjectPlugin(t, dir, ".fabrik/plugins/one", "dup", "fabrik-review")
	addProjectPlugin(t, dir, ".fabrik/plugins/two", "dup", "fabrik-review")
	addProjectPlugin(t, dir, ".fabrik/plugins/base", "fabrik-stages", "fabrik-review")
	writePluginDirsConfig(t, dir, ".fabrik/plugins/base", ".fabrik/plugins/one", ".fabrik/plugins/two")
	_, report := runMigration(t, true)
	if got := stageRefs(t, dir)["review.yaml:skill"]; got != "dup:fabrik-review" {
		t.Errorf("skill = %q", got)
	}
	if !strings.Contains(report, `repeats the plugin name "dup"`) {
		t.Errorf("duplicate not reported:\n%s", report)
	}
}

// Startup and `fabrik upgrade` plan from the same inputs, so a deployment whose
// only pending change is a project-plugin qualification is reported pending.
func TestProjectPlugin_PendingUntilUpgraded(t *testing.T) {
	dir := projectFixture(t, "name: Review\nskill: fabrik-review\n")
	addProjectPlugin(t, dir, ".fabrik/plugins/team", "team", "fabrik-review")
	writePluginDirsConfig(t, dir, ".fabrik/plugins/team")
	if plan, _ := runMigration(t, false); !plan.Pending() {
		t.Fatal("should be pending before the upgrade")
	}
	runMigration(t, true)
	if plan, _ := runMigration(t, false); plan.Pending() {
		t.Error("still pending after the upgrade")
	}
}

// runUpgrade loads .env like the daemon, so a FABRIK_PLUGIN_DIRS that lives only
// there is part of the plugin set the migration sees (otherwise a bare skill an
// extra provides would be rewritten to the base plugin).
func TestRunUpgrade_DotenvPluginDirsKeepTheProjectSkill(t *testing.T) {
	dir := projectFixture(t, "name: Review\nskill: fabrik-review\n")
	addProjectPlugin(t, dir, ".fabrik/plugins/team", "team", "fabrik-review")
	writeTestFile(t, filepath.Join(dir, ".env"), "FABRIK_PLUGIN_DIRS=.fabrik/plugins/team\n")
	// godotenv leaves a variable that is already in the environment alone, so the
	// fixture's empty value must be unset; t.Setenv registers the restore.
	t.Setenv("FABRIK_PLUGIN_DIRS", "x")
	if err := os.Unsetenv("FABRIK_PLUGIN_DIRS"); err != nil {
		t.Fatal(err)
	}
	orig := Version
	Version = "dev(test)"
	t.Cleanup(func() { Version = orig })

	if err := runUpgrade(nil); err != nil {
		t.Fatalf("runUpgrade: %v", err)
	}
	if got := stageRefs(t, dir)["review.yaml:skill"]; got != "team:fabrik-review" {
		t.Errorf("skill = %q, want team:fabrik-review (the .env-only plugin dir was not consulted)", got)
	}
}

// An already-set environment variable wins over .env, as in the daemon.
func TestRunUpgrade_EnvironmentWinsOverDotenv(t *testing.T) {
	dir := projectFixture(t, "name: Review\nskill: fabrik-review\n")
	addProjectPlugin(t, dir, ".fabrik/plugins/envp", "envp", "fabrik-review")
	addProjectPlugin(t, dir, ".fabrik/plugins/dotp", "dotp", "fabrik-review")
	writeTestFile(t, filepath.Join(dir, ".env"), "FABRIK_PLUGIN_DIRS=.fabrik/plugins/dotp\n")
	t.Setenv("FABRIK_PLUGIN_DIRS", ".fabrik/plugins/envp")
	orig := Version
	Version = "dev(test)"
	t.Cleanup(func() { Version = orig })

	if err := runUpgrade(nil); err != nil {
		t.Fatalf("runUpgrade: %v", err)
	}
	if got := stageRefs(t, dir)["review.yaml:skill"]; got != "envp:fabrik-review" {
		t.Errorf("skill = %q, want envp:fabrik-review", got)
	}
}
