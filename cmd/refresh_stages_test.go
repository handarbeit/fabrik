package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

// syntheticDefaultsFS builds an in-memory fs.FS with one embedded default stage
// YAML under examples/<lowername>.yaml. keys is a map of key → YAML value string
// (e.g. "wait_for_ci" → "true", "allowed_tools" → "- Read\n- Grep").
func syntheticDefaultsFS(name string, kv map[string]string) fstest.MapFS {
	var sb strings.Builder
	sb.WriteString("name: " + name + "\n")
	for k, v := range kv {
		// Multi-line values (sequences/mappings) are already properly indented.
		if strings.Contains(v, "\n") {
			sb.WriteString(k + ":\n")
			for _, line := range strings.Split(v, "\n") {
				if line != "" {
					sb.WriteString("  " + line + "\n")
				}
			}
		} else {
			sb.WriteString(k + ": " + v + "\n")
		}
	}
	return fstest.MapFS{
		"examples/" + strings.ToLower(name) + ".yaml": {Data: []byte(sb.String())},
	}
}

func writeUserStage(t *testing.T, dir, filename, content string) string {
	t.Helper()
	path := filepath.Join(dir, filename)
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("writing user stage: %v", err)
	}
	return path
}

func TestRefreshStages_DryRunNoDrift(t *testing.T) {
	dir := t.TempDir()
	// User stage has all keys the default has → no drift.
	writeUserStage(t, dir, "validate.yaml", "name: Validate\nwait_for_ci: true\nwait_for_reviews: true\n")

	defaults := syntheticDefaultsFS("Validate", map[string]string{
		"wait_for_ci":      "true",
		"wait_for_reviews": "true",
	})

	var out strings.Builder
	err := refreshStagesWithReader(dir, false, false, false, strings.NewReader(""), &out, defaults)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := out.String(); got != "" {
		t.Errorf("expected no output when no drift, got: %q", got)
	}

	// File must be unmodified.
	data, _ := os.ReadFile(filepath.Join(dir, "validate.yaml"))
	if !strings.Contains(string(data), "wait_for_ci: true") {
		t.Errorf("file should be unmodified but content changed")
	}
}

func TestRefreshStages_DryRunWithDrift(t *testing.T) {
	dir := t.TempDir()
	origContent := "name: Validate\nwait_for_ci: true\n"
	path := writeUserStage(t, dir, "validate.yaml", origContent)

	defaults := syntheticDefaultsFS("Validate", map[string]string{
		"wait_for_ci":      "true",
		"wait_for_reviews": "true",
	})

	var out strings.Builder
	err := refreshStagesWithReader(dir, false, false, false, strings.NewReader(""), &out, defaults)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "+ ") {
		t.Errorf("expected + prefix in output, got: %q", got)
	}
	if !strings.Contains(got, "wait_for_reviews") {
		t.Errorf("expected output to mention wait_for_reviews, got: %q", got)
	}
	if !strings.Contains(got, "1 missing field") {
		t.Errorf("expected '1 missing field' in output, got: %q", got)
	}

	// File must be unmodified in dry-run.
	data, _ := os.ReadFile(path)
	if string(data) != origContent {
		t.Errorf("dry-run must not modify file; got:\n%s", string(data))
	}
}

func TestRefreshStages_ApplyWritesMissingKeys(t *testing.T) {
	dir := t.TempDir()
	origContent := "name: Validate\nwait_for_ci: true\n"
	path := writeUserStage(t, dir, "validate.yaml", origContent)

	defaults := syntheticDefaultsFS("Validate", map[string]string{
		"wait_for_ci":      "true",
		"wait_for_reviews": "true",
	})

	var out strings.Builder
	err := refreshStagesWithReader(dir, true, false, false, strings.NewReader(""), &out, defaults)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading file after apply: %v", err)
	}
	content := string(data)

	if !strings.Contains(content, "wait_for_reviews") {
		t.Errorf("expected wait_for_reviews in file after apply, got:\n%s", content)
	}
	// Existing key must still be present.
	if !strings.Contains(content, "wait_for_ci") {
		t.Errorf("expected wait_for_ci still present after apply, got:\n%s", content)
	}
	// Output should mention the update.
	if !strings.Contains(out.String(), "wait_for_reviews") {
		t.Errorf("expected output to mention updated key, got: %q", out.String())
	}
}

func TestRefreshStages_InteractiveYes(t *testing.T) {
	dir := t.TempDir()
	writeUserStage(t, dir, "validate.yaml", "name: Validate\nwait_for_ci: true\n")

	defaults := syntheticDefaultsFS("Validate", map[string]string{
		"wait_for_ci":      "true",
		"wait_for_reviews": "true",
	})

	var out strings.Builder
	// Inject "y" as user answer.
	err := refreshStagesWithReader(dir, true, true, false, strings.NewReader("y\n"), &out, defaults)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, _ := os.ReadFile(filepath.Join(dir, "validate.yaml"))
	if !strings.Contains(string(data), "wait_for_reviews") {
		t.Errorf("expected apply when user answered y; file:\n%s", string(data))
	}
}

func TestRefreshStages_InteractiveNo(t *testing.T) {
	dir := t.TempDir()
	origContent := "name: Validate\nwait_for_ci: true\n"
	path := writeUserStage(t, dir, "validate.yaml", origContent)

	defaults := syntheticDefaultsFS("Validate", map[string]string{
		"wait_for_ci":      "true",
		"wait_for_reviews": "true",
	})

	var out strings.Builder
	// Inject "n" as user answer.
	err := refreshStagesWithReader(dir, true, true, false, strings.NewReader("n\n"), &out, defaults)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// File must be unmodified when user answered n.
	data, _ := os.ReadFile(path)
	if string(data) != origContent {
		t.Errorf("file must be unchanged after n answer; got:\n%s", string(data))
	}
}

func TestRefreshStages_CustomStageSkipped(t *testing.T) {
	dir := t.TempDir()
	// User has a custom stage not present in the defaults.
	origContent := "name: Custom\nprompt: do custom stuff\n"
	path := writeUserStage(t, dir, "custom.yaml", origContent)

	// Defaults only contain "Validate" — no "Custom" entry.
	defaults := syntheticDefaultsFS("Validate", map[string]string{
		"wait_for_ci": "true",
	})

	var out strings.Builder
	err := refreshStagesWithReader(dir, false, false, false, strings.NewReader(""), &out, defaults)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// No output for custom stage.
	if got := out.String(); got != "" {
		t.Errorf("expected no output for custom stage, got: %q", got)
	}
	// File must be unmodified.
	data, _ := os.ReadFile(path)
	if string(data) != origContent {
		t.Errorf("file must be unchanged for custom stage")
	}
}

func TestRefreshStages_InteractiveRequiresApply(t *testing.T) {
	err := refreshStagesWithReader(t.TempDir(), false, true, false, strings.NewReader(""), &strings.Builder{}, fstest.MapFS{})
	if err == nil || !strings.Contains(err.Error(), "--interactive requires --apply") {
		t.Errorf("expected --interactive requires --apply error, got: %v", err)
	}
}
