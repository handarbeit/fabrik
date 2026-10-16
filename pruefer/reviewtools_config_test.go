package pruefer

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestLoadConfig_ReviewToolsLayers(t *testing.T) {
	dir := t.TempDir()
	cfg, err := LoadConfig([]string{"-config", filepath.Join(dir, "missing.yaml")})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.ReviewTools) != 0 {
		t.Errorf("default = %v, want empty (today's grant)", cfg.ReviewTools)
	}

	path := writeYAMLConfig(t, dir, "review_tools:\n  - \"Bash(golangci-lint:*)\"\n  - \"  Bash(semgrep   --config p/ci *) \"\n  - \"Bash(golangci-lint:*)\"\n")
	cfg, err = LoadConfig([]string{"-config", path})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"Bash(golangci-lint:*)", "Bash(semgrep --config p/ci *)"}; !slices.Equal(cfg.ReviewTools, want) {
		t.Errorf("yaml = %v, want normalised %v", cfg.ReviewTools, want)
	}

	t.Setenv("PRUEFER_REVIEW_TOOLS", "Bash(shellcheck *), Bash(actionlint)")
	cfg, err = LoadConfig([]string{"-config", path})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"Bash(shellcheck *)", "Bash(actionlint)"}; !slices.Equal(cfg.ReviewTools, want) {
		t.Errorf("env = %v, want env to override yaml: %v", cfg.ReviewTools, want)
	}

	cfg, err = LoadConfig([]string{"-config", path, "-review-tools", "Bash(gitleaks detect *)"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"Bash(gitleaks detect *)"}; !slices.Equal(cfg.ReviewTools, want) {
		t.Errorf("flag = %v, want flag to override env: %v", cfg.ReviewTools, want)
	}
}

// Acceptance: review_tools: ["Bash(go test:*)"] is refused at config load, from
// every layer, naming the rule.
func TestLoadConfig_RefusesATestRunnerInReviewTools(t *testing.T) {
	check := func(t *testing.T, err error) {
		t.Helper()
		if err == nil {
			t.Fatal("expected a refusal at config load")
		}
		for _, want := range []string{"Bash(go test:*)", "test runners are not permitted"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not contain %q", err, want)
			}
		}
	}
	path := writeYAMLConfig(t, t.TempDir(), "review_tools: [\"Bash(go test:*)\"]\n")
	_, err := LoadConfig([]string{"-config", path})
	check(t, err)

	missing := filepath.Join(t.TempDir(), "missing.yaml")
	_, err = LoadConfig([]string{"-config", missing, "-review-tools", "Bash(go test:*)"})
	check(t, err)

	t.Setenv("PRUEFER_REVIEW_TOOLS", "Bash(shellcheck *),Bash(go test:*)")
	_, err = LoadConfig([]string{"-config", missing})
	check(t, err)
}

func TestLoadConfig_RefusesBlanketAndMalformedReviewTools(t *testing.T) {
	for _, rule := range []string{"Bash", "Bash(*)", "Edit", "Bash(a, b)", "Bash(unclosed"} {
		path := writeYAMLConfig(t, t.TempDir(), "review_tools: [\""+rule+"\"]\n")
		if _, err := LoadConfig([]string{"-config", path}); err == nil || !strings.Contains(err.Error(), "review_tools") {
			t.Errorf("%q: err = %v, want a review_tools refusal", rule, err)
		}
	}
}

func TestReload_ReviewToolsIsLiveAndRefusedOnReload(t *testing.T) {
	merged, diff := applyConfigReload(Config{}, Config{ReviewTools: []string{"Bash(shellcheck *)"}})
	if !slices.Equal(merged.ReviewTools, []string{"Bash(shellcheck *)"}) {
		t.Errorf("merged = %+v", merged)
	}
	if len(diff.RestartOnlyChanged) != 0 {
		t.Errorf("restart-only changes: %+v", diff.RestartOnlyChanged)
	}
	// The reload path re-runs LoadConfig, so a refused rule never reaches it.
	path := writeYAMLConfig(t, t.TempDir(), "review_tools: [\"Bash(pytest:*)\"]\n")
	if _, err := LoadConfig([]string{"-config", path}); err == nil {
		t.Error("a reloaded config naming a test runner must be refused")
	}
}

// ADR 1642 AC4: review_tools is repo-settable, but only as a selection — it
// never reaches Config, and absent differs from an explicit empty list.
func TestFetchRepoConfig_ReviewToolsRecognised(t *testing.T) {
	rc, prov := fetchRepoConfig(newFakeContentsClient([]byte("review_tools:\n  - \"Bash(a *)\"\n"), nil), "o", "r", "sha")
	if rc.ReviewTools == nil || !slices.Equal(*rc.ReviewTools, []string{"Bash(a *)"}) {
		t.Errorf("review_tools not parsed: %+v", rc.ReviewTools)
	}
	if slices.Contains(prov.UnknownKeys, "review_tools") {
		t.Errorf("review_tools reported as not repo-settable: %v", prov.UnknownKeys)
	}
	eff, _ := applyRepoNarrowing(Config{ReviewTools: []string{"Bash(b *)"}}, rc)
	if !slices.Equal(eff.ReviewTools, []string{"Bash(b *)"}) {
		t.Errorf("the repo changed Config.ReviewTools: %v", eff.ReviewTools)
	}

	rc, _ = fetchRepoConfig(newFakeContentsClient([]byte("review_tools: []\n"), nil), "o", "r", "sha")
	if rc.ReviewTools == nil || len(*rc.ReviewTools) != 0 {
		t.Errorf("explicit empty list must be non-nil and empty: %+v", rc.ReviewTools)
	}
	rc, _ = fetchRepoConfig(newFakeContentsClient([]byte("excluded_labels: [x]\n"), nil), "o", "r", "sha")
	if rc.ReviewTools != nil {
		t.Errorf("absent key must be nil: %+v", rc.ReviewTools)
	}
}
