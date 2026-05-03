package watch

import (
	"os"
	"path/filepath"
	"testing"
)

// writeLog creates a stub .log file in dir with the given name. Content is irrelevant.
func writeLog(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0o644); err != nil {
		t.Fatalf("writeLog %s: %v", name, err)
	}
}

// TestBuildStageTabs_PipelineOrder verifies that tabs appear in pipeline order
// (not file-chronological order) when stageOrder is provided.
func TestBuildStageTabs_PipelineOrder(t *testing.T) {
	dir := t.TempDir()
	// Create Implement log first (older timestamp), Research log second (newer).
	// Without ordering: Implement would appear last. With ordering: Research(1) first.
	writeLog(t, dir, "Implement-20260101-100000-000000000.log")
	writeLog(t, dir, "Research-20260101-110000-000000000.log")

	stageOrder := map[string]int{
		"Research":  1,
		"Implement": 3,
	}
	tabs := buildStageTabs(dir, stageOrder, "")

	if len(tabs) != 2 {
		t.Fatalf("want 2 tabs, got %d", len(tabs))
	}
	if tabs[0].Label != "Research" {
		t.Errorf("tab[0] want Research, got %s", tabs[0].Label)
	}
	if tabs[1].Label != "Implement" {
		t.Errorf("tab[1] want Implement, got %s", tabs[1].Label)
	}
	// Newest file is Research log — it should be IsLive.
	if !tabs[0].IsLive {
		t.Error("Research tab should be IsLive (it has the newest file)")
	}
	if tabs[1].IsLive {
		t.Error("Implement tab should not be IsLive")
	}
}

// TestBuildStageTabs_CommentReviewGrouped verifies that a comment-review log
// is absorbed into its parent stage tab, and if it is the newest file, the
// parent tab is marked IsLive.
func TestBuildStageTabs_CommentReviewGrouped(t *testing.T) {
	dir := t.TempDir()
	writeLog(t, dir, "Specify-20260101-090000-000000000.log")
	// comment-review log is newer
	writeLog(t, dir, "Specify-comment-review-20260101-100000-000000000.log")

	stageOrder := map[string]int{"Specify": 2}
	tabs := buildStageTabs(dir, stageOrder, "")

	if len(tabs) != 1 {
		t.Fatalf("want 1 tab (grouped), got %d", len(tabs))
	}
	if tabs[0].Label != "Specify" {
		t.Errorf("want label Specify, got %s", tabs[0].Label)
	}
	// LogPath should point to the comment-review log (newer).
	if base := filepath.Base(tabs[0].LogPath); base != "Specify-comment-review-20260101-100000-000000000.log" {
		t.Errorf("LogPath should be the comment-review file, got %s", base)
	}
	if !tabs[0].IsLive {
		t.Error("Specify tab should be IsLive")
	}
}

// TestBuildStageTabs_FallbackChronological verifies that an empty stageOrder
// causes tabs to be sorted lexicographically by filename (fallback mode).
// The IsLive tab is the one with the lexicographically largest filename
// (which combines label + timestamp, so label prefix affects ordering).
func TestBuildStageTabs_FallbackChronological(t *testing.T) {
	dir := t.TempDir()
	// Both labels unknown; sorted by filename lexicographically.
	// "Alpha-..." < "Beta-..." because 'A' < 'B'.
	// Beta also has the newer timestamp, so it is correctly IsLive.
	writeLog(t, dir, "Alpha-20260101-070000-000000000.log")
	writeLog(t, dir, "Beta-20260101-080000-000000000.log")

	tabs := buildStageTabs(dir, map[string]int{}, "")

	if len(tabs) != 2 {
		t.Fatalf("want 2 tabs, got %d", len(tabs))
	}
	// Lexicographic: Alpha before Beta.
	if tabs[0].Label != "Alpha" {
		t.Errorf("tab[0] want Alpha, got %s", tabs[0].Label)
	}
	if tabs[1].Label != "Beta" {
		t.Errorf("tab[1] want Beta, got %s", tabs[1].Label)
	}
	// Beta has both the later timestamp and the lexicographically larger filename.
	if !tabs[1].IsLive {
		t.Error("Beta tab should be IsLive (newest file)")
	}
}

// TestBuildStageTabs_UnknownAppended verifies that unknown stage logs are
// appended after known stages.
func TestBuildStageTabs_UnknownAppended(t *testing.T) {
	dir := t.TempDir()
	// Unknown stage has a newer timestamp than the known stage.
	writeLog(t, dir, "Research-20260101-090000-000000000.log")
	writeLog(t, dir, "Freeform-20260101-100000-000000000.log")

	stageOrder := map[string]int{"Research": 1}
	tabs := buildStageTabs(dir, stageOrder, "")

	if len(tabs) != 2 {
		t.Fatalf("want 2 tabs, got %d", len(tabs))
	}
	if tabs[0].Label != "Research" {
		t.Errorf("tab[0] want Research (known), got %s", tabs[0].Label)
	}
	if tabs[1].Label != "Freeform" {
		t.Errorf("tab[1] want Freeform (unknown), got %s", tabs[1].Label)
	}
}

// TestBuildStageTabs_EmptyDir verifies that an empty log directory returns nil.
func TestBuildStageTabs_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	tabs := buildStageTabs(dir, map[string]int{"Research": 1}, "")
	if len(tabs) != 0 {
		t.Errorf("want 0 tabs for empty dir, got %d", len(tabs))
	}
}

// TestIsUserTurn verifies NDJSON user-type detection for the watch follower.
func TestIsUserTurn(t *testing.T) {
	tests := []struct {
		line string
		want bool
	}{
		{`{"type":"user","message":{}}` + "\n", true},
		{`{"type":"assistant","message":{}}` + "\n", false},
		{`{"type":"result","num_turns":10}` + "\n", false},
		{`{"type":"tool_use"}` + "\n", false},
		{"not json\n", false},
		{"", false},
		{"{}\n", false},
	}
	for _, tt := range tests {
		got := isUserTurn([]byte(tt.line))
		if got != tt.want {
			t.Errorf("isUserTurn(%q) = %v, want %v", tt.line, got, tt.want)
		}
	}
}

// TestBuildStageTabs_IsLive_ByTimestampNotLabel is a regression test for the
// bug where an alphabetically-later stage label (e.g. "Specify" > "Research")
// would be wrongly selected as IsLive even when its log has an older timestamp.
// After the fix, the Research tab (newer timestamp) must be IsLive.
func TestBuildStageTabs_IsLive_ByTimestampNotLabel(t *testing.T) {
	dir := t.TempDir()
	// Specify log is older (09:41); Research log is newer (17:04).
	// "Specify-..." > "Research-..." lexicographically ('S' > 'R'),
	// so without the fix Specify would wrongly win.
	writeLog(t, dir, "Specify-20260407-094159-000000000.log")
	writeLog(t, dir, "Research-20260407-170451-000000000.log")

	stageOrder := map[string]int{
		"Research": 1,
		"Specify":  2,
	}
	tabs := buildStageTabs(dir, stageOrder, "")

	if len(tabs) != 2 {
		t.Fatalf("want 2 tabs, got %d", len(tabs))
	}
	// Pipeline order: Research(1) first, Specify(2) second.
	if tabs[0].Label != "Research" {
		t.Errorf("tab[0] want Research, got %s", tabs[0].Label)
	}
	if tabs[1].Label != "Specify" {
		t.Errorf("tab[1] want Specify, got %s", tabs[1].Label)
	}
	// Research has the newer timestamp — it must be IsLive.
	if !tabs[0].IsLive {
		t.Error("Research tab should be IsLive (newer timestamp), not Specify")
	}
	if tabs[1].IsLive {
		t.Error("Specify tab should not be IsLive (older timestamp)")
	}
}

// TestNewestLogFile_ByTimestampNotLabel is a regression test for the bug where
// newestLogFile would return the alphabetically-last filename regardless of
// timestamp, so "Specify-..." would beat "Research-..." even when Research is newer.
// After the fix, newestLogFile must return the Research log (newer timestamp).
func TestNewestLogFile_ByTimestampNotLabel(t *testing.T) {
	dir := t.TempDir()
	// Specify log is older (09:00); Research log is newer (10:00).
	writeLog(t, dir, "Specify-20260101-090000-000000000.log")
	writeLog(t, dir, "Research-20260101-100000-000000000.log")

	got := newestLogFile(dir)
	want := filepath.Join(dir, "Research-20260101-100000-000000000.log")
	if got != want {
		t.Errorf("newestLogFile: want %s, got %s", filepath.Base(want), filepath.Base(got))
	}
}

// TestBestLogFileForStage_FiltersByStage verifies that bestLogFileForStage returns
// the newest log matching the requested stage and ignores other stages.
func TestBestLogFileForStage_FiltersByStage(t *testing.T) {
	dir := t.TempDir()
	writeLog(t, dir, "Review-20260101-090000-000000000.log")
	writeLog(t, dir, "Implement-20260101-100000-000000000.log") // newer, but wrong stage

	got := bestLogFileForStage(dir, "Review")
	want := filepath.Join(dir, "Review-20260101-090000-000000000.log")
	if got != want {
		t.Errorf("bestLogFileForStage(Review): want %s, got %s", filepath.Base(want), filepath.Base(got))
	}
}

// TestBestLogFileForStage_IncludesCommentReview verifies that a comment-review log
// for the requested stage is included and returned when it is newer.
func TestBestLogFileForStage_IncludesCommentReview(t *testing.T) {
	dir := t.TempDir()
	writeLog(t, dir, "Review-20260101-090000-000000000.log")
	writeLog(t, dir, "Review-comment-review-20260101-100000-000000000.log") // newer

	got := bestLogFileForStage(dir, "Review")
	want := filepath.Join(dir, "Review-comment-review-20260101-100000-000000000.log")
	if got != want {
		t.Errorf("bestLogFileForStage(Review) should return comment-review file; got %s", filepath.Base(got))
	}
}

// TestBestLogFileForStage_FallbackWhenNoMatch verifies that bestLogFileForStage
// falls back to newestLogFile when no file matches the requested stage.
func TestBestLogFileForStage_FallbackWhenNoMatch(t *testing.T) {
	dir := t.TempDir()
	writeLog(t, dir, "Implement-20260101-090000-000000000.log")
	writeLog(t, dir, "Research-20260101-100000-000000000.log")

	// Ask for a stage that has no log file; should fall back to the globally newest.
	got := bestLogFileForStage(dir, "Validate")
	want := filepath.Join(dir, "Research-20260101-100000-000000000.log")
	if got != want {
		t.Errorf("bestLogFileForStage(Validate) fallback: want %s, got %s", filepath.Base(want), filepath.Base(got))
	}
}

// TestBestLogFileForStage_EmptyStageName verifies that an empty stageName falls
// back to newestLogFile behaviour (globally newest file).
func TestBestLogFileForStage_EmptyStageName(t *testing.T) {
	dir := t.TempDir()
	writeLog(t, dir, "Research-20260101-090000-000000000.log")
	writeLog(t, dir, "Implement-20260101-100000-000000000.log")

	got := bestLogFileForStage(dir, "")
	want := filepath.Join(dir, "Implement-20260101-100000-000000000.log")
	if got != want {
		t.Errorf("bestLogFileForStage(\"\") should behave like newestLogFile; got %s", filepath.Base(got))
	}
}

// TestBuildStageTabs_LabelOverride_BeatTimestamp is the primary regression test:
// an Implement-comment-review log with a newer timestamp must NOT steal IsLive
// from the Review tab when "Review" is the active stage from labels.
func TestBuildStageTabs_LabelOverride_BeatTimestamp(t *testing.T) {
	dir := t.TempDir()
	// Review log is older; Implement-comment-review log is newer.
	writeLog(t, dir, "Review-20260101-180000-000000000.log")
	writeLog(t, dir, "Implement-comment-review-20260101-190000-000000000.log")
	writeLog(t, dir, "Implement-20260101-120000-000000000.log")

	stageOrder := map[string]int{
		"Research":  1,
		"Implement": 3,
		"Review":    4,
	}
	tabs := buildStageTabs(dir, stageOrder, "Review")

	// Find the Review tab and assert it is live.
	var reviewLive, implementLive bool
	for _, t2 := range tabs {
		if t2.Label == "Review" {
			reviewLive = t2.IsLive
		}
		if t2.Label == "Implement" {
			implementLive = t2.IsLive
		}
	}
	if !reviewLive {
		t.Error("Review tab must be IsLive when activeStage=Review, even though Implement-comment-review log is newer")
	}
	if implementLive {
		t.Error("Implement tab must NOT be IsLive when activeStage=Review")
	}
}

// TestBuildStageTabs_LabelOverride_FallbackWhenNoMatch verifies that when the
// activeStage label names a stage with no tab, the timestamp heuristic is used.
func TestBuildStageTabs_LabelOverride_FallbackWhenNoMatch(t *testing.T) {
	dir := t.TempDir()
	// Only Research log exists; activeStage says "Validate" (no tab for it).
	writeLog(t, dir, "Research-20260101-100000-000000000.log")

	stageOrder := map[string]int{"Research": 1, "Validate": 5}
	tabs := buildStageTabs(dir, stageOrder, "Validate")

	if len(tabs) != 1 {
		t.Fatalf("want 1 tab, got %d", len(tabs))
	}
	// Falls back to timestamp heuristic: Research is the only tab and must be IsLive.
	if !tabs[0].IsLive {
		t.Error("Research tab must be IsLive via timestamp fallback when activeStage tab doesn't exist")
	}
}
