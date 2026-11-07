package stages

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/handarbeit/fabrik/warnings"
)

// extractDefaults writes the embedded default stages to a fresh directory,
// exactly what `fabrik init` does, and loads them.
func extractDefaults(t *testing.T) (string, []*Stage) {
	t.Helper()
	dir := t.TempDir()
	entries, err := fs.ReadDir(DefaultStages, "examples")
	if err != nil {
		t.Fatalf("read embedded defaults: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		data, err := DefaultStages.ReadFile("examples/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, e.Name()), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	loaded, err := LoadAll(dir)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	return dir, loaded
}

// FR-008 (#1894): an unmodified `fabrik init` install raises no
// undeclared_reviewers notice, in the log or the warnings panel.
func TestShippedDefaults_RaiseNoUndeclaredReviewersNotice(t *testing.T) {
	setWarningsOverride(t)
	_, loaded := extractDefaults(t)

	waiting := 0
	for _, s := range loaded {
		if s.WaitForReviews != nil && *s.WaitForReviews {
			waiting++
			if s.ExpectedReviewers == nil {
				t.Errorf("shipped stage %s has wait_for_reviews without an expected_reviewers declaration", s.Name)
			}
		}
	}
	if waiting == 0 {
		t.Fatal("no shipped stage waits for reviews; the test no longer covers FR-008")
	}

	var out strings.Builder
	WarnUndeclaredReviewers(loaded, &out)
	if out.String() != "" {
		t.Errorf("unmodified defaults produced a notice: %q", out.String())
	}
	entries, err := warnings.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Key, "undeclared_reviewers:") {
			t.Errorf("unmodified defaults recorded panel entry %s", e.Key)
		}
	}
}

// FR-009: an operator-authored stage that waits for reviews and declares
// nothing still gets the notice.
func TestOperatorAuthoredUndeclaredStageStillWarns(t *testing.T) {
	setWarningsOverride(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "review.yaml")
	if err := os.WriteFile(path, []byte("name: Review\norder: 4\nprompt: x\nwait_for_reviews: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadAll(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	WarnUndeclaredReviewers(loaded, &out)
	if !strings.Contains(out.String(), "expected_reviewers") {
		t.Errorf("expected the undeclared notice, got %q", out.String())
	}
}

// An existing install whose stage files predate the shipped declaration gets no
// stage_drift entry for expected_reviewers (ADR 1894 amends ADR 1283).
func TestExpectedReviewersIsNeverReportedAsDrift(t *testing.T) {
	setWarningsOverride(t)
	dir := t.TempDir()
	// Old-style user file: no expected_reviewers key at all.
	path := filepath.Join(dir, "review.yaml")
	old := "name: Review\norder: 4\nprompt: x\nwait_for_reviews: true\n"
	if err := os.WriteFile(path, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadAll(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Defaults = the real embedded Review stage, narrowed to the keys the old
	// file has plus expected_reviewers, so only that key can differ.
	defaults := syntheticFSRaw("Review", "name: Review\norder: 4\nprompt: x\nwait_for_reviews: true\nexpected_reviewers: []\nwait_for_ci: true\n")
	var out strings.Builder
	WarnStageDriftFromFS(loaded, "v-test", &out, defaults)
	// Non-vacuity: the unrelated missing key must still be reported.
	if !strings.Contains(out.String(), "wait_for_ci") {
		t.Fatalf("drift check did not run (wait_for_ci not reported): %q", out.String())
	}
	if strings.Contains(out.String(), "expected_reviewers") {
		t.Errorf("drift reported expected_reviewers: %q", out.String())
	}

	// And the rule itself: always filtered, whatever the default stage says.
	got := FilterNoOpKeys([]string{"expected_reviewers", "wait_for_ci"}, &Stage{})
	if len(got) != 1 || got[0] != "wait_for_ci" {
		t.Errorf("FilterNoOpKeys = %v, want only wait_for_ci", got)
	}
}
