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

// --- WarnUndeclaredReviewers (FR-7) ---

func TestWarnUndeclaredReviewers_Fires(t *testing.T) {
	setWarningsOverride(t)
	waitTrue := true
	s := &Stage{Name: "Review", FilePath: filepath.Join(t.TempDir(), "review.yaml"), WaitForReviews: &waitTrue}

	var out strings.Builder
	WarnUndeclaredReviewers([]*Stage{s}, &out)

	got := out.String()
	if !strings.Contains(got, "[startup] notice:") {
		t.Errorf("expected a startup notice, got: %q", got)
	}
	if !strings.Contains(got, "Review") {
		t.Errorf("expected the notice to name the stage, got: %q", got)
	}
	if !strings.Contains(got, "expected_reviewers") {
		t.Errorf("expected the notice to mention expected_reviewers, got: %q", got)
	}

	entries, err := warnings.Load()
	if err != nil {
		t.Fatalf("warnings.Load: %v", err)
	}
	var found bool
	for _, e := range entries {
		if e.Key == "undeclared_reviewers:Review" {
			found = true
		}
	}
	if !found {
		t.Error("expected an undeclared_reviewers:Review entry recorded")
	}
}

func TestWarnUndeclaredReviewers_AbsentWhenDeclaredNonEmpty(t *testing.T) {
	setWarningsOverride(t)
	waitTrue := true
	declared := []string{"handarbeit-pruefer"}
	s := &Stage{Name: "Review", FilePath: filepath.Join(t.TempDir(), "review.yaml"), WaitForReviews: &waitTrue, ExpectedReviewers: &declared}

	var out strings.Builder
	WarnUndeclaredReviewers([]*Stage{s}, &out)

	if got := out.String(); got != "" {
		t.Errorf("expected no notice once expected_reviewers is declared, got: %q", got)
	}
}

func TestWarnUndeclaredReviewers_AbsentWhenDeclaredExplicitlyEmpty(t *testing.T) {
	setWarningsOverride(t)
	waitTrue := true
	none := []string{}
	s := &Stage{Name: "Review", FilePath: filepath.Join(t.TempDir(), "review.yaml"), WaitForReviews: &waitTrue, ExpectedReviewers: &none}

	var out strings.Builder
	WarnUndeclaredReviewers([]*Stage{s}, &out)

	if got := out.String(); got != "" {
		t.Errorf("expected no notice for explicit expected_reviewers: [], got: %q", got)
	}
}

func TestWarnUndeclaredReviewers_AbsentWhenWaitForReviewsFalse(t *testing.T) {
	setWarningsOverride(t)
	waitFalse := false
	s := &Stage{Name: "Implement", FilePath: filepath.Join(t.TempDir(), "implement.yaml"), WaitForReviews: &waitFalse}

	var out strings.Builder
	WarnUndeclaredReviewers([]*Stage{s}, &out)

	if got := out.String(); got != "" {
		t.Errorf("expected no notice when wait_for_reviews is false, got: %q", got)
	}
}

func TestWarnUndeclaredReviewers_AbsentWhenWaitForReviewsNil(t *testing.T) {
	setWarningsOverride(t)
	s := &Stage{Name: "Plan", FilePath: filepath.Join(t.TempDir(), "plan.yaml")}

	var out strings.Builder
	WarnUndeclaredReviewers([]*Stage{s}, &out)

	if got := out.String(); got != "" {
		t.Errorf("expected no notice when wait_for_reviews is unset, got: %q", got)
	}
}

// FR-7's core self-limiting requirement: once a declaration (including
// explicit "none") is added, the previously-recorded entry is cleared, not
// just left unrenewed.
func TestWarnUndeclaredReviewers_ClearsOnceDeclared(t *testing.T) {
	setWarningsOverride(t)
	waitTrue := true
	s := &Stage{Name: "Review", FilePath: filepath.Join(t.TempDir(), "review.yaml"), WaitForReviews: &waitTrue}

	var out1 strings.Builder
	WarnUndeclaredReviewers([]*Stage{s}, &out1)
	if !strings.Contains(out1.String(), "[startup] notice:") {
		t.Fatalf("expected initial notice, got: %q", out1.String())
	}

	entries, _ := warnings.Load()
	var recordedBefore bool
	for _, e := range entries {
		if e.Key == "undeclared_reviewers:Review" {
			recordedBefore = true
		}
	}
	if !recordedBefore {
		t.Fatal("expected the entry to be recorded before the declaration is added")
	}

	// Operator adds a declaration.
	declared := []string{"handarbeit-pruefer"}
	s.ExpectedReviewers = &declared

	var out2 strings.Builder
	WarnUndeclaredReviewers([]*Stage{s}, &out2)
	if got := out2.String(); got != "" {
		t.Errorf("expected no notice after declaration is added, got: %q", got)
	}

	entries, _ = warnings.Load()
	for _, e := range entries {
		if e.Key == "undeclared_reviewers:Review" {
			t.Error("expected undeclared_reviewers:Review entry to be cleared once declared, but it is still present")
		}
	}
}

func TestWarnUndeclaredReviewers_EmptyUserStages(t *testing.T) {
	setWarningsOverride(t)

	var out strings.Builder
	WarnUndeclaredReviewers(nil, &out)

	if got := out.String(); got != "" {
		t.Errorf("expected no notice for empty userStages, got: %q", got)
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

func TestSweepStaleWarnings_ClearsRenamedOrRemovedStage(t *testing.T) {
	setWarningsOverride(t)
	if err := warnings.Record(warnings.Entry{Key: "stage_drift:OldName", Type: "stage_drift"}); err != nil {
		t.Fatal(err)
	}
	if err := warnings.Record(warnings.Entry{Key: "undeclared_reviewers:OldName", Type: "undeclared_reviewers"}); err != nil {
		t.Fatal(err)
	}

	var buf strings.Builder
	SweepStaleWarnings([]*Stage{{Name: "NewName"}}, &buf)

	entries, _ := warnings.Load()
	if len(entries) != 0 {
		t.Fatalf("expected both stale entries cleared, got %v", entries)
	}
	if !strings.Contains(buf.String(), "stage_drift:OldName") || !strings.Contains(buf.String(), "undeclared_reviewers:OldName") {
		t.Errorf("expected log to name both cleared keys, got: %q", buf.String())
	}
}

func TestSweepStaleWarnings_PreservesConfiguredStage(t *testing.T) {
	setWarningsOverride(t)
	if err := warnings.Record(warnings.Entry{Key: "stage_drift:Implement", Type: "stage_drift"}); err != nil {
		t.Fatal(err)
	}

	var buf strings.Builder
	SweepStaleWarnings([]*Stage{{Name: "Implement"}}, &buf)

	entries, _ := warnings.Load()
	if len(entries) != 1 || entries[0].Key != "stage_drift:Implement" {
		t.Fatalf("expected still-configured stage's warning preserved, got %v", entries)
	}
	if buf.String() != "" {
		t.Errorf("expected no log output when nothing stale, got: %q", buf.String())
	}
}

func TestSweepStaleWarnings_IgnoresOtherTypes(t *testing.T) {
	setWarningsOverride(t)
	if err := warnings.Record(warnings.Entry{Key: "allow_auto_merge:owner/repo", Type: "allow_auto_merge"}); err != nil {
		t.Fatal(err)
	}

	var buf strings.Builder
	SweepStaleWarnings(nil, &buf)

	entries, _ := warnings.Load()
	if len(entries) != 1 || entries[0].Key != "allow_auto_merge:owner/repo" {
		t.Fatalf("expected unrelated warning type untouched, got %v", entries)
	}
}

// TestSweepStaleWarnings_EmptyUserStagesPreservesEntries guards against the
// destructive-wipe shape flagged in review: without an early return for an
// empty/nil userStages (matching WarnStageDrift's own guard), a present set
// derived from zero stages would make every recorded stage_drift/
// undeclared_reviewers entry look "absent" and clear them all in one pass.
// cmd/root.go already fails startup before Run() is reached if stage loading
// produces zero configs, so this is normally unreachable — the guard is
// defense in depth for any future caller that bypasses that validation.
func TestSweepStaleWarnings_EmptyUserStagesPreservesEntries(t *testing.T) {
	setWarningsOverride(t)
	if err := warnings.Record(warnings.Entry{Key: "stage_drift:Implement", Type: "stage_drift"}); err != nil {
		t.Fatal(err)
	}
	if err := warnings.Record(warnings.Entry{Key: "undeclared_reviewers:Implement", Type: "undeclared_reviewers"}); err != nil {
		t.Fatal(err)
	}

	var buf strings.Builder
	SweepStaleWarnings(nil, &buf)

	entries, _ := warnings.Load()
	if len(entries) != 2 {
		t.Fatalf("expected both entries preserved when userStages is empty, got %v", entries)
	}
	if buf.String() != "" {
		t.Errorf("expected no log output when the sweep is skipped, got: %q", buf.String())
	}
}

func TestSweepStaleWarnings_IncludesUnmanagedStages(t *testing.T) {
	// The sweep must be called with the unfiltered stage set (including
	// Unmanaged stages) — otherwise a currently-valid Unmanaged stage's
	// warning would be wrongly swept.
	setWarningsOverride(t)
	if err := warnings.Record(warnings.Entry{Key: "stage_drift:Backlog", Type: "stage_drift"}); err != nil {
		t.Fatal(err)
	}

	var buf strings.Builder
	SweepStaleWarnings([]*Stage{{Name: "Backlog", Unmanaged: true}}, &buf)

	entries, _ := warnings.Load()
	if len(entries) != 1 {
		t.Fatalf("expected Unmanaged stage's warning preserved, got %v", entries)
	}
}

func TestDriftTitle_NamesMissingFields(t *testing.T) {
	// The summary title must name the missing key(s), not just their count, so
	// the warnings panel is actionable without opening the detail view.
	got := driftTitle(".fabrik/stages/implement.yaml", []string{"kill_grace"})
	want := ".fabrik/stages/implement.yaml missing 1 field(s): kill_grace"
	if got != want {
		t.Errorf("driftTitle = %q, want %q", got, want)
	}

	// Multiple missing fields are all named.
	multi := driftTitle("review.yaml", []string{"kill_grace", "wait_for_ci"})
	if !strings.Contains(multi, "kill_grace, wait_for_ci") {
		t.Errorf("expected both fields named, got %q", multi)
	}
}
