package pruefer

import (
	"strings"
	"testing"
)

func TestSplitDiffFiles_MultiFile(t *testing.T) {
	diff := `diff --git a/a.go b/a.go
index 111..222 100644
--- a/a.go
+++ b/a.go
@@ -1,1 +1,1 @@
-old
+new
diff --git a/b.go b/b.go
index 333..444 100644
--- a/b.go
+++ b/b.go
@@ -1,1 +1,1 @@
-old2
+new2
`
	files, preamble := splitDiffFiles(diff)
	if preamble != "" {
		t.Errorf("preamble = %q, want empty for a well-formed diff", preamble)
	}
	if len(files) != 2 {
		t.Fatalf("files = %+v, want 2 entries", files)
	}
	if files[0].Path != "a.go" || files[1].Path != "b.go" {
		t.Errorf("paths = [%q, %q], want [a.go, b.go]", files[0].Path, files[1].Path)
	}
	for _, f := range files {
		if f.Bytes != int64(len(f.Body)) {
			t.Errorf("file %q: Bytes = %d, want len(Body) = %d", f.Path, f.Bytes, len(f.Body))
		}
	}
	total := int64(0)
	for _, f := range files {
		total += f.Bytes
	}
	if total != int64(len(diff)) {
		t.Errorf("sum of file bytes = %d, want len(diff) = %d (well-formed diff should reconstruct exactly)", total, len(diff))
	}
}

func TestSplitDiffFiles_RenameKeyedByDestinationPath(t *testing.T) {
	diff := `diff --git a/old_name.go b/new_name.go
similarity index 100%
rename from old_name.go
rename to new_name.go
`
	files, _ := splitDiffFiles(diff)
	if len(files) != 1 || files[0].Path != "new_name.go" {
		t.Errorf("splitDiffFiles(rename) = %+v, want single file keyed as new_name.go", files)
	}
}

func TestSplitDiffFiles_PathContainsSpaceBSlash_ResolvedViaUnambiguousLine(t *testing.T) {
	// "foo b/bar.txt" contains the literal substring " b/", making the
	// header line's own "diff --git a/X b/Y" split ambiguous — greedily
	// matching the LAST " b/" occurrence in the line yields the wrong,
	// truncated path ("bar.txt") if the header's capture groups are trusted
	// directly. The unambiguous "+++ b/<path>" line must win instead.
	diff := `diff --git a/foo b/bar.txt b/foo b/bar.txt
index 111..222 100644
--- a/foo b/bar.txt
+++ b/foo b/bar.txt
@@ -1,1 +1,1 @@
-old
+new
`
	files, _ := splitDiffFiles(diff)
	if len(files) != 1 || files[0].Path != "foo b/bar.txt" {
		t.Errorf("splitDiffFiles(ambiguous path) = %+v, want single file keyed as \"foo b/bar.txt\"", files)
	}
}

func TestSplitDiffFiles_DeletedFileWithAmbiguousPath_ResolvedViaMinusLine(t *testing.T) {
	// Deletions have no "+++ b/<path>" line ("+++ /dev/null" instead), so
	// the unambiguous "--- a/<path>" line must be preferred over the
	// header's own ambiguous split.
	diff := `diff --git a/foo b/bar.txt b/foo b/bar.txt
deleted file mode 100644
index 111..000
--- a/foo b/bar.txt
+++ /dev/null
@@ -1,1 +0,0 @@
-old
`
	files, _ := splitDiffFiles(diff)
	if len(files) != 1 || files[0].Path != "foo b/bar.txt" {
		t.Errorf("splitDiffFiles(deleted, ambiguous path) = %+v, want single file keyed as \"foo b/bar.txt\"", files)
	}
}

func TestSplitDiffFiles_BinaryFileModified_AmbiguousPath_ResolvedViaBinaryFilesLine(t *testing.T) {
	// Binary files never get "+++"/"--- " content lines, only "Binary files
	// ... differ". Same ambiguous-path scenario as the text-diff cases
	// above, but relying on the new binary-marker-line resolution instead.
	diff := `diff --git a/foo b/bar.png b/foo b/bar.png
index 111..222 100644
Binary files a/foo b/bar.png and b/foo b/bar.png differ
`
	files, _ := splitDiffFiles(diff)
	if len(files) != 1 || files[0].Path != "foo b/bar.png" {
		t.Errorf("splitDiffFiles(binary modify, ambiguous path) = %+v, want single file keyed as \"foo b/bar.png\"", files)
	}
}

func TestSplitDiffFiles_BinaryFileAdded_AmbiguousPath_ResolvedViaDevNullLine(t *testing.T) {
	diff := `diff --git a/foo b/bar.png b/foo b/bar.png
new file mode 100644
index 000..111
Binary files /dev/null and b/foo b/bar.png differ
`
	files, _ := splitDiffFiles(diff)
	if len(files) != 1 || files[0].Path != "foo b/bar.png" {
		t.Errorf("splitDiffFiles(binary add, ambiguous path) = %+v, want single file keyed as \"foo b/bar.png\"", files)
	}
}

func TestSplitDiffFiles_BinaryFileDeleted_AmbiguousPath_ResolvedViaDevNullLine(t *testing.T) {
	diff := `diff --git a/foo b/bar.png b/foo b/bar.png
deleted file mode 100644
index 111..000
Binary files a/foo b/bar.png and /dev/null differ
`
	files, _ := splitDiffFiles(diff)
	if len(files) != 1 || files[0].Path != "foo b/bar.png" {
		t.Errorf("splitDiffFiles(binary delete, ambiguous path) = %+v, want single file keyed as \"foo b/bar.png\"", files)
	}
}

func TestSplitDiffFiles_GarbageInput_AllPreamble(t *testing.T) {
	diff := "x diff content"
	files, preamble := splitDiffFiles(diff)
	if len(files) != 0 {
		t.Errorf("files = %+v, want none for non-diff content", files)
	}
	if preamble == "" {
		t.Error("preamble must not be empty for non-diff content — it must still count toward the size measurement")
	}
}

func TestSplitDiffFiles_Empty(t *testing.T) {
	files, preamble := splitDiffFiles("")
	if files != nil || preamble != "" {
		t.Errorf("splitDiffFiles(\"\") = (%+v, %q), want (nil, \"\")", files, preamble)
	}
}

// TestSplitDiffFiles_HeaderLookalikeAsRealDiffContent_NotTreatedAsNewFile
// guards against the failure mode flagged in PR #1279 review: a PR touching
// a .patch/.diff fixture, or documentation showing an example git-diff
// header, could contain a line shaped exactly like "diff --git a/... b/...".
// As it actually appears in a real diff, such a content line is never
// unprefixed — git's unified-diff format prefixes every hunk content line
// with "+"/"-"/" ", so the embedded line here renders as
// "+diff --git a/fake b/fake" (added) — which does not match
// diffFileHeaderLineRE's anchored "^diff --git a/(.+) b/(.+)$" (the leading
// "+" breaks the match at position 0). This is why splitDiffFiles treats
// every literal, unprefixed "diff --git " match as an unconditional file
// boundary, mirroring validRightAnchors' identical unconditional treatment
// of the same line shape in diffanchor.go: the prefix invariant, not extra
// hunk-tracking state, is what keeps this correct for well-formed diffs.
func TestSplitDiffFiles_HeaderLookalikeAsRealDiffContent_NotTreatedAsNewFile(t *testing.T) {
	diff := `diff --git a/fixtures/example.patch b/fixtures/example.patch
index 111..222 100644
--- a/fixtures/example.patch
+++ b/fixtures/example.patch
@@ -1,2 +1,2 @@
-old fixture line
+diff --git a/fake b/fake
 unchanged context
diff --git a/b.go b/b.go
index 333..444 100644
--- a/b.go
+++ b/b.go
@@ -1,1 +1,1 @@
-old2
+new2
`
	files, preamble := splitDiffFiles(diff)
	if preamble != "" {
		t.Errorf("preamble = %q, want empty", preamble)
	}
	if len(files) != 2 {
		t.Fatalf("files = %+v, want exactly 2 (the prefixed lookalike line must not split fixtures/example.patch in two)", files)
	}
	if files[0].Path != "fixtures/example.patch" || files[1].Path != "b.go" {
		t.Errorf("paths = [%q, %q], want [fixtures/example.patch, b.go]", files[0].Path, files[1].Path)
	}
	if !strings.Contains(files[0].Body, "+diff --git a/fake b/fake") {
		t.Errorf("files[0].Body = %q, want the lookalike line retained as content of fixtures/example.patch", files[0].Body)
	}
}

func TestSplitDiffFiles_PreambleBeforeFirstHeader(t *testing.T) {
	diff := "some leading noise\n" + `diff --git a/a.go b/a.go
index 111..222 100644
--- a/a.go
+++ b/a.go
@@ -1,1 +1,1 @@
-old
+new
`
	files, preamble := splitDiffFiles(diff)
	if preamble != "some leading noise\n" {
		t.Errorf("preamble = %q, want %q", preamble, "some leading noise\n")
	}
	if len(files) != 1 || files[0].Path != "a.go" {
		t.Errorf("files = %+v, want single a.go entry", files)
	}
}

// The following five cases port the glob-matching scenarios formerly
// exercised directly against select.go's now-removed EligibilityInput
// ChangedPaths/ExcludedPaths fields (never populated on ReviewPR's real call
// path — see the issue's Research findings). filterExcludedPaths is the live
// replacement; these prove its glob semantics unchanged.
func TestFilterExcludedPaths(t *testing.T) {
	mk := func(path string) diffFile { return diffFile{Path: path, Bytes: 10} }

	tests := []struct {
		name     string
		files    []diffFile
		patterns []string
		wantKept []string
		wantDrop []string
	}{
		{
			name:     "full match: all files dropped",
			files:    []diffFile{mk("docs/a.md"), mk("docs/b.md")},
			patterns: []string{"docs/*"},
			wantKept: nil,
			wantDrop: []string{"docs/a.md", "docs/b.md"},
		},
		{
			name:     "partial match: only matching files dropped",
			files:    []diffFile{mk("docs/a.md"), mk("engine/claude.go")},
			patterns: []string{"docs/*"},
			wantKept: []string{"engine/claude.go"},
			wantDrop: []string{"docs/a.md"},
		},
		{
			name:     "no files: nothing to filter",
			files:    nil,
			patterns: []string{"docs/*"},
			wantKept: nil,
			wantDrop: nil,
		},
		{
			name:     "** matches nested paths",
			files:    []diffFile{mk("vendor/pkg/foo/bar.go")},
			patterns: []string{"vendor/**"},
			wantKept: nil,
			wantDrop: []string{"vendor/pkg/foo/bar.go"},
		},
		{
			name:     "** does not match outside the prefixed dir",
			files:    []diffFile{mk("engine/claude.go")},
			patterns: []string{"vendor/**"},
			wantKept: []string{"engine/claude.go"},
			wantDrop: nil,
		},
		{
			name:     "no patterns configured: everything kept",
			files:    []diffFile{mk("docs/a.md")},
			patterns: nil,
			wantKept: []string{"docs/a.md"},
			wantDrop: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kept, dropped := filterExcludedPaths(tt.files, tt.patterns)
			if got := pathsOf(kept); !equalStrings(got, tt.wantKept) {
				t.Errorf("kept = %v, want %v", got, tt.wantKept)
			}
			if got := pathsOf(dropped); !equalStrings(got, tt.wantDrop) {
				t.Errorf("dropped = %v, want %v", got, tt.wantDrop)
			}
		})
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestTrimToFit_UnderCap_NoDrops(t *testing.T) {
	files := []diffFile{{Path: "a.go", Bytes: 10}, {Path: "b.go", Bytes: 10}}
	kept, dropped, fits := trimToFit(files, 0, 100)
	if len(dropped) != 0 || !fits {
		t.Errorf("trimToFit under cap: dropped=%v fits=%v, want none dropped and fits=true", dropped, fits)
	}
	if len(kept) != 2 {
		t.Errorf("kept = %+v, want both files", kept)
	}
}

func TestTrimToFit_DropsLargestFirst(t *testing.T) {
	files := []diffFile{
		{Path: "small.go", Bytes: 10},
		{Path: "huge.jsonl", Bytes: 1000},
		{Path: "medium.go", Bytes: 50},
	}
	kept, dropped, fits := trimToFit(files, 0, 100)
	if !fits {
		t.Fatalf("trimToFit: fits = false, want true (dropping huge.jsonl alone fits under 100)")
	}
	if len(dropped) != 1 || dropped[0].Path != "huge.jsonl" {
		t.Errorf("dropped = %+v, want exactly [huge.jsonl] (largest first)", dropped)
	}
	if len(kept) != 2 || kept[0].Path != "small.go" || kept[1].Path != "medium.go" {
		t.Errorf("kept = %+v, want [small.go, medium.go] in original order", kept)
	}
}

func TestTrimToFit_PreambleAloneExceedsCap_NeverFits(t *testing.T) {
	files := []diffFile{{Path: "a.go", Bytes: 10}}
	kept, dropped, fits := trimToFit(files, 1000, 100)
	if fits {
		t.Error("fits = true, want false — preamble bytes can never be dropped, so a preamble alone over cap must never fit")
	}
	if len(dropped) != 1 || len(kept) != 0 {
		t.Errorf("kept=%+v dropped=%+v, want every file dropped once preamble alone exceeds cap", kept, dropped)
	}
}

func TestTrimToFit_DroppingEverythingStillOverCap_DoesNotFit(t *testing.T) {
	files := []diffFile{{Path: "a.go", Bytes: 1000}}
	kept, dropped, fits := trimToFit(files, 0, 100)
	if fits {
		t.Error("fits = true, want false — dropping every file isn't a successful trim")
	}
	if len(kept) != 0 || len(dropped) != 1 {
		t.Errorf("kept=%+v dropped=%+v, want the single oversized file dropped and none kept", kept, dropped)
	}
}
