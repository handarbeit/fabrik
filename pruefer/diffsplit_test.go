package pruefer

import "testing"

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
