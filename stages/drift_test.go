package stages

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/handarbeit/fabrik/warnings"
)

// setWarningsOverride redirects warnings I/O to a temp file for test isolation.
func setWarningsOverride(t *testing.T) {
	t.Helper()
	warnings.WarningsPathOverride = filepath.Join(t.TempDir(), "warnings.json")
	t.Cleanup(func() { warnings.WarningsPathOverride = "" })
}

// syntheticFS builds an in-memory fs.FS with a single default stage YAML under
// examples/<name>.yaml for use in drift tests.
func syntheticFS(t *testing.T, name string, keys ...string) fstest.MapFS {
	t.Helper()
	var sb strings.Builder
	sb.WriteString("name: " + name + "\n")
	for _, k := range keys {
		sb.WriteString(k + ": true\n")
	}
	return fstest.MapFS{
		"examples/" + strings.ToLower(name) + ".yaml": {Data: []byte(sb.String())},
	}
}

// syntheticFSRaw builds an in-memory fs.FS with a single default stage YAML
// under examples/<name>.yaml, using the full body verbatim. Unlike syntheticFS
// (flat scalar keys only), this lets tests express nested blocks like
// kill_grace: / completion:.
func syntheticFSRaw(name, body string) fstest.MapFS {
	return fstest.MapFS{
		"examples/" + strings.ToLower(name) + ".yaml": {Data: []byte(body)},
	}
}

// makeUserStage writes a YAML stage file to dir and returns a *Stage with FilePath set.
func makeUserStage(t *testing.T, dir, filename, content string) *Stage {
	t.Helper()
	path := filepath.Join(dir, filename)
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("writing stage file: %v", err)
	}
	return &Stage{
		Name:     extractName(t, content),
		FilePath: path,
	}
}

// extractName pulls the name: value from a minimal YAML string for test helper use.
func extractName(t *testing.T, yaml string) string {
	t.Helper()
	for line := range strings.SplitSeq(yaml, "\n") {
		if strings.HasPrefix(line, "name:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "name:"))
		}
	}
	t.Fatal("no name: field in yaml")
	return ""
}

func TestWarnStageDrift_MissingKey(t *testing.T) {
	setWarningsOverride(t)
	dir := t.TempDir()
	// User stage is missing "wait_for_ci" which the embedded default has.
	userStage := makeUserStage(t, dir, "validate.yaml", "name: Validate\nprompt: do stuff\n")

	defaults := syntheticFS(t, "Validate", "prompt", "wait_for_ci")

	var out strings.Builder
	warnDriftFrom([]*Stage{userStage}, "v0.0.99", &out, defaults)

	got := out.String()
	if !strings.Contains(got, "[startup] warning:") {
		t.Errorf("expected warning, got: %q", got)
	}
	if !strings.Contains(got, "wait_for_ci") {
		t.Errorf("expected warning to mention wait_for_ci, got: %q", got)
	}
	if !strings.Contains(got, "validate.yaml") {
		t.Errorf("expected warning to mention validate.yaml, got: %q", got)
	}
	if !strings.Contains(got, "v0.0.99") {
		t.Errorf("expected warning to mention version v0.0.99, got: %q", got)
	}
}

func TestWarnStageDrift_AllKeysPresent(t *testing.T) {
	setWarningsOverride(t)
	dir := t.TempDir()
	// User stage has all keys that the embedded default has.
	userStage := makeUserStage(t, dir, "validate.yaml", "name: Validate\nprompt: do stuff\nwait_for_ci: true\n")

	defaults := syntheticFS(t, "Validate", "prompt", "wait_for_ci")

	var out strings.Builder
	warnDriftFrom([]*Stage{userStage}, "v0.0.99", &out, defaults)

	if got := out.String(); got != "" {
		t.Errorf("expected no warning when all keys present, got: %q", got)
	}
}

func TestWarnStageDrift_KeyPresentWithNonDefaultValue(t *testing.T) {
	setWarningsOverride(t)
	dir := t.TempDir()
	// User stage has wait_for_ci explicitly set to false — key is present, no warning.
	userStage := makeUserStage(t, dir, "validate.yaml", "name: Validate\nprompt: do stuff\nwait_for_ci: false\n")

	defaults := syntheticFS(t, "Validate", "prompt", "wait_for_ci")

	var out strings.Builder
	warnDriftFrom([]*Stage{userStage}, "v0.0.99", &out, defaults)

	if got := out.String(); got != "" {
		t.Errorf("expected no warning when key present with non-default value, got: %q", got)
	}
}

func TestWarnStageDrift_CustomStageSkipped(t *testing.T) {
	setWarningsOverride(t)
	dir := t.TempDir()
	// "Custom" does not appear in the synthetic defaults — should be silently skipped.
	userStage := makeUserStage(t, dir, "custom.yaml", "name: Custom\nprompt: do custom stuff\n")

	defaults := syntheticFS(t, "Validate", "prompt", "wait_for_ci")

	var out strings.Builder
	warnDriftFrom([]*Stage{userStage}, "v0.0.99", &out, defaults)

	if got := out.String(); got != "" {
		t.Errorf("expected no warning for custom stage, got: %q", got)
	}
}

func TestWarnStageDrift_EmptyUserStages(t *testing.T) {
	setWarningsOverride(t)
	defaults := syntheticFS(t, "Validate", "prompt", "wait_for_ci")

	var out strings.Builder
	warnDriftFrom(nil, "v0.0.99", &out, defaults)

	if got := out.String(); got != "" {
		t.Errorf("expected no warning for empty userStages, got: %q", got)
	}
}

func TestWarnStageDrift_MultipleMissingKeysSorted(t *testing.T) {
	setWarningsOverride(t)
	dir := t.TempDir()
	// User stage is missing both wait_for_ci and wait_for_reviews.
	userStage := makeUserStage(t, dir, "validate.yaml", "name: Validate\nprompt: do stuff\n")

	defaults := syntheticFS(t, "Validate", "prompt", "wait_for_ci", "wait_for_reviews")

	var out strings.Builder
	warnDriftFrom([]*Stage{userStage}, "v0.0.99", &out, defaults)

	got := out.String()
	// Both keys must appear and be in sorted order.
	idxCI := strings.Index(got, "wait_for_ci")
	idxRev := strings.Index(got, "wait_for_reviews")
	if idxCI == -1 || idxRev == -1 {
		t.Fatalf("expected both missing keys in warning, got: %q", got)
	}
	if idxCI > idxRev {
		t.Errorf("expected wait_for_ci before wait_for_reviews (sorted), got: %q", got)
	}
}

func TestWarnStageDrift_KillGraceNoOpOmitted(t *testing.T) {
	setWarningsOverride(t)
	dir := t.TempDir()
	// User stage omits kill_grace entirely — the embedded default's value
	// (10s/10s) is identical to the engine's inherit-on-omission behavior.
	userStage := makeUserStage(t, dir, "implement.yaml", "name: Implement\nprompt: do stuff\n")

	defaults := syntheticFSRaw("Implement", "name: Implement\nprompt: do stuff\nkill_grace:\n  sigint: 10s\n  sigterm: 10s\n")

	var out strings.Builder
	warnDriftFrom([]*Stage{userStage}, "v0.0.99", &out, defaults)

	if got := out.String(); got != "" {
		t.Errorf("expected no warning for no-op kill_grace (10s/10s), got: %q", got)
	}
}

func TestWarnStageDrift_KillGraceMeaningfulOmittedStillWarns(t *testing.T) {
	setWarningsOverride(t)
	dir := t.TempDir()
	// User stage omits kill_grace; the embedded default sets sigint: 0s,
	// which skips the SIGINT step entirely — behaviorally different from
	// omission, so this must still warn.
	userStage := makeUserStage(t, dir, "implement.yaml", "name: Implement\nprompt: do stuff\n")

	defaults := syntheticFSRaw("Implement", "name: Implement\nprompt: do stuff\nkill_grace:\n  sigint: 0s\n  sigterm: 10s\n")

	var out strings.Builder
	warnDriftFrom([]*Stage{userStage}, "v0.0.99", &out, defaults)

	got := out.String()
	if !strings.Contains(got, "[startup] warning:") {
		t.Errorf("expected warning for meaningful kill_grace (sigint: 0s), got: %q", got)
	}
	if !strings.Contains(got, "kill_grace") {
		t.Errorf("expected warning to mention kill_grace, got: %q", got)
	}
}

func TestWarnStageDrift_CompletionNoOpOmitted(t *testing.T) {
	setWarningsOverride(t)
	dir := t.TempDir()
	// User stage omits completion entirely — the embedded default's value
	// (type: claude) is identical to loadOne's fallback on omission.
	userStage := makeUserStage(t, dir, "implement.yaml", "name: Implement\nprompt: do stuff\n")

	defaults := syntheticFSRaw("Implement", "name: Implement\nprompt: do stuff\ncompletion:\n  type: claude\n")

	var out strings.Builder
	warnDriftFrom([]*Stage{userStage}, "v0.0.99", &out, defaults)

	if got := out.String(); got != "" {
		t.Errorf("expected no warning for no-op completion (type: claude), got: %q", got)
	}
}

func TestMissingTopLevelKeys_ReturnsMissingKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stage.yaml")
	if err := os.WriteFile(path, []byte("name: Test\nprompt: do stuff\n"), 0644); err != nil {
		t.Fatal(err)
	}

	defaultKeys := map[string]bool{
		"name":        true,
		"prompt":      true,
		"wait_for_ci": true,
	}

	missing, err := MissingTopLevelKeys(path, defaultKeys)
	if err != nil {
		t.Fatalf("missingTopLevelKeys: %v", err)
	}
	if len(missing) != 1 || missing[0] != "wait_for_ci" {
		t.Errorf("missing = %v, want [wait_for_ci]", missing)
	}
}

func TestMissingTopLevelKeys_NoneWhenAllPresent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stage.yaml")
	if err := os.WriteFile(path, []byte("name: Test\nprompt: do stuff\nwait_for_ci: true\n"), 0644); err != nil {
		t.Fatal(err)
	}

	defaultKeys := map[string]bool{
		"name":        true,
		"prompt":      true,
		"wait_for_ci": true,
	}

	missing, err := MissingTopLevelKeys(path, defaultKeys)
	if err != nil {
		t.Fatalf("missingTopLevelKeys: %v", err)
	}
	if len(missing) != 0 {
		t.Errorf("expected no missing keys, got: %v", missing)
	}
}

func TestMissingTopLevelKeys_SortedOutput(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stage.yaml")
	if err := os.WriteFile(path, []byte("name: Test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	defaultKeys := map[string]bool{"zebra": true, "apple": true, "mango": true}

	missing, err := MissingTopLevelKeys(path, defaultKeys)
	if err != nil {
		t.Fatalf("missingTopLevelKeys: %v", err)
	}
	if len(missing) != 3 {
		t.Fatalf("expected 3 missing keys, got %d: %v", len(missing), missing)
	}
	if missing[0] != "apple" || missing[1] != "mango" || missing[2] != "zebra" {
		t.Errorf("expected sorted output [apple mango zebra], got: %v", missing)
	}
}
