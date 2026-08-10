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
	blocks, preamble := splitDiffFiles(diff)
	if preamble != "" {
		t.Errorf("preamble = %q, want empty for a well-formed diff", preamble)
	}
	if len(blocks) != 2 {
		t.Fatalf("blocks = %+v, want 2 entries", blocks)
	}
	if blocks[0].Path != "a.go" || blocks[1].Path != "b.go" {
		t.Errorf("paths = [%q, %q], want [a.go, b.go]", blocks[0].Path, blocks[1].Path)
	}
	if joinDiff(preamble, blocks) != diff {
		t.Errorf("joinDiff round-trip did not reconstruct the original diff")
	}
	if blocksLen(preamble, blocks) != int64(len(diff)) {
		t.Errorf("blocksLen = %d, want %d", blocksLen(preamble, blocks), len(diff))
	}
}

// TestSplitDiffFiles_NoTrailingNewline_SingleBlock pins the bot-review fix:
// splitDiffFiles/joinDiff must round-trip a diff byte-for-byte even when the
// source text has no trailing "\n" — the prior implementation unconditionally
// appended "\n" when flushing the last block, silently growing the diff by
// one byte and desyncing blocksLen/the anchored diff from what was actually
// fetched.
func TestSplitDiffFiles_NoTrailingNewline_SingleBlock(t *testing.T) {
	diff := "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1,1 +1,1 @@\n-old\n+new"
	if strings.HasSuffix(diff, "\n") {
		t.Fatal("test fixture must not end in a newline")
	}
	blocks, preamble := splitDiffFiles(diff)
	if got := joinDiff(preamble, blocks); got != diff {
		t.Errorf("joinDiff round-trip = %q, want %q (exact byte match, no phantom trailing newline)", got, diff)
	}
	if got := blocksLen(preamble, blocks); got != int64(len(diff)) {
		t.Errorf("blocksLen = %d, want %d", got, len(diff))
	}
}

// TestSplitDiffFiles_NoTrailingNewline_MultiBlock proves the fix holds
// across a block boundary too: the newline between the first block and the
// second header must still be restored (it was a real "\n" in the source),
// while the final block — the one actually missing a trailing newline in
// the source — must not gain one.
func TestSplitDiffFiles_NoTrailingNewline_MultiBlock(t *testing.T) {
	diff := "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1,1 +1,1 @@\n-old\n+new\n" +
		"diff --git a/b.go b/b.go\n--- a/b.go\n+++ b/b.go\n@@ -1,1 +1,1 @@\n-old2\n+new2"
	blocks, preamble := splitDiffFiles(diff)
	if len(blocks) != 2 {
		t.Fatalf("blocks = %+v, want 2 entries", blocks)
	}
	if !strings.HasSuffix(blocks[0].Raw, "\n") {
		t.Errorf("blocks[0].Raw = %q, want a trailing newline — a real line boundary preceded the next header", blocks[0].Raw)
	}
	if strings.HasSuffix(blocks[1].Raw, "\n") {
		t.Errorf("blocks[1].Raw = %q, want no trailing newline — the source diff did not end in one", blocks[1].Raw)
	}
	if got := joinDiff(preamble, blocks); got != diff {
		t.Errorf("joinDiff round-trip = %q, want %q", got, diff)
	}
}

// TestSplitDiffFiles_NoTrailingNewline_PreambleOnly covers the no-blocks-at-
// all case (a headerless/garbage diff) with no trailing newline — the same
// phantom-byte bug applied here too, since preamble followed the identical
// unconditional-append pattern.
func TestSplitDiffFiles_NoTrailingNewline_PreambleOnly(t *testing.T) {
	diff := "not a diff at all\njust some text"
	blocks, preamble := splitDiffFiles(diff)
	if len(blocks) != 0 {
		t.Fatalf("blocks = %+v, want none", blocks)
	}
	if preamble != diff {
		t.Errorf("preamble = %q, want %q (exact byte match, no phantom trailing newline)", preamble, diff)
	}
}

func TestSplitDiffFiles_RenameKeyedByDestinationPath(t *testing.T) {
	diff := `diff --git a/old_name.go b/new_name.go
similarity index 100%
rename from old_name.go
rename to new_name.go
`
	blocks, _ := splitDiffFiles(diff)
	if len(blocks) != 1 || blocks[0].Path != "new_name.go" {
		t.Errorf("splitDiffFiles(rename) = %+v, want single block keyed as new_name.go", blocks)
	}
}

func TestSplitDiffFiles_PreambleBeforeFirstHeader(t *testing.T) {
	diff := "some preamble noise\nmore noise\n" + `diff --git a/a.go b/a.go
index 111..222 100644
--- a/a.go
+++ b/a.go
@@ -1,1 +1,1 @@
-old
+new
`
	blocks, preamble := splitDiffFiles(diff)
	if preamble != "some preamble noise\nmore noise\n" {
		t.Errorf("preamble = %q, want the leading noise", preamble)
	}
	if len(blocks) != 1 || blocks[0].Path != "a.go" {
		t.Errorf("blocks = %+v, want single a.go block", blocks)
	}
	if joinDiff(preamble, blocks) != diff {
		t.Error("joinDiff round-trip did not reconstruct the original diff with preamble")
	}
}

func TestSplitDiffFiles_GarbageInput_AllPreamble(t *testing.T) {
	diff := "not a diff at all\njust some text\n"
	blocks, preamble := splitDiffFiles(diff)
	if len(blocks) != 0 {
		t.Errorf("blocks = %+v, want none", blocks)
	}
	if preamble != diff {
		t.Errorf("preamble = %q, want the entire input", preamble)
	}
}

func TestSplitDiffFiles_Empty(t *testing.T) {
	blocks, preamble := splitDiffFiles("")
	if blocks != nil || preamble != "" {
		t.Errorf("splitDiffFiles(\"\") = (%+v, %q), want (nil, \"\")", blocks, preamble)
	}
}

// TestResolveFilePath_PlusPlusPlusMidHunk is the ported regression for old
// commit 97cdce20: an added content line that itself begins with "+++ b/"
// (e.g. a diff fixture embedded as test data inside the actual changed
// file) must not be misread as a new file's header line once a hunk has
// already started — the same inHunk-gating discipline diffanchor.go's
// validRightAnchors already relies on.
func TestResolveFilePath_PlusPlusPlusMidHunk(t *testing.T) {
	diff := `diff --git a/pruefer/review_test.go b/pruefer/review_test.go
index 1111111..2222222 100644
--- a/pruefer/review_test.go
+++ b/pruefer/review_test.go
@@ -10,2 +10,4 @@
 const diff = ` + "`" + `diff --git a/fake b/fake
+++ b/fake
` + "`" + `
 var x int
`
	blocks, _ := splitDiffFiles(diff)
	if len(blocks) != 1 {
		t.Fatalf("blocks = %+v, want exactly 1 (the mid-hunk lookalike must not start a second block)", blocks)
	}
	if blocks[0].Path != "pruefer/review_test.go" {
		t.Errorf("Path = %q, want %q (must resolve from the real header, not the embedded lookalike)", blocks[0].Path, "pruefer/review_test.go")
	}
}

func TestResolveFilePath_DeletedFile_FallsBackToHeader(t *testing.T) {
	diff := `diff --git a/old.go b/old.go
deleted file mode 100644
index 1111111..0000000
--- a/old.go
+++ /dev/null
@@ -1,1 +0,0 @@
-content
`
	blocks, _ := splitDiffFiles(diff)
	if len(blocks) != 1 || blocks[0].Path != "old.go" {
		t.Errorf("splitDiffFiles(deleted file) = %+v, want single block keyed as old.go (header fallback, no +++ b/ line)", blocks)
	}
}

func TestResolveFilePath_NoContentDelta_FallsBackToHeader(t *testing.T) {
	// A pure file-mode change has no "+++"/"--- " content lines at all.
	diff := `diff --git a/script.sh b/script.sh
old mode 100644
new mode 100755
`
	blocks, _ := splitDiffFiles(diff)
	if len(blocks) != 1 || blocks[0].Path != "script.sh" {
		t.Errorf("splitDiffFiles(mode-only change) = %+v, want single block keyed as script.sh", blocks)
	}
}

func TestFilterExcludedPaths_NoPatterns_KeepsAll(t *testing.T) {
	blocks := []diffFileBlock{{Path: "a.go", Raw: "a\n"}, {Path: "b.go", Raw: "b\n"}}
	kept, dropped := filterExcludedPaths(blocks, nil)
	if len(kept) != 2 || len(dropped) != 0 {
		t.Errorf("filterExcludedPaths(nil patterns) = kept=%+v dropped=%+v, want all kept", kept, dropped)
	}
}

func TestFilterExcludedPaths_SomeMatch_PartitionsPerFile(t *testing.T) {
	blocks := []diffFileBlock{
		{Path: "vendor/lib.go", Raw: "1\n"},
		{Path: "pkg/a.go", Raw: "2\n"},
		{Path: "pkg/b.go", Raw: "3\n"},
	}
	kept, dropped := filterExcludedPaths(blocks, []string{"vendor/**"})
	if len(kept) != 2 || len(dropped) != 1 {
		t.Fatalf("filterExcludedPaths = kept=%+v dropped=%+v, want 2 kept, 1 dropped", kept, dropped)
	}
	if dropped[0].Path != "vendor/lib.go" {
		t.Errorf("dropped[0].Path = %q, want vendor/lib.go", dropped[0].Path)
	}
	if kept[0].Path != "pkg/a.go" || kept[1].Path != "pkg/b.go" {
		t.Errorf("kept paths = [%q, %q], want original relative order preserved", kept[0].Path, kept[1].Path)
	}
}

func TestFilterExcludedPaths_AllMatch_KeepsNone(t *testing.T) {
	blocks := []diffFileBlock{{Path: "docs/a.md", Raw: "1\n"}, {Path: "docs/b.md", Raw: "2\n"}}
	kept, dropped := filterExcludedPaths(blocks, []string{"docs/*"})
	if len(kept) != 0 || len(dropped) != 2 {
		t.Errorf("filterExcludedPaths(all match) = kept=%+v dropped=%+v, want none kept", kept, dropped)
	}
}

func TestTrimToFit_UnderCap_NoDrops(t *testing.T) {
	blocks := []diffFileBlock{{Path: "a.go", Raw: strings.Repeat("x", 10)}}
	kept, dropped, fits := trimToFit(blocks, 0, 100)
	if len(kept) != 1 || len(dropped) != 0 || !fits {
		t.Errorf("trimToFit(under cap) = kept=%+v dropped=%+v fits=%v, want all kept, fits=true", kept, dropped, fits)
	}
}

func TestTrimToFit_DropsLargestFirst_OrderPreservedAmongSurvivors(t *testing.T) {
	blocks := []diffFileBlock{
		{Path: "small1.go", Raw: strings.Repeat("a", 10)},
		{Path: "huge.jsonl", Raw: strings.Repeat("b", 1000)},
		{Path: "small2.go", Raw: strings.Repeat("c", 10)},
	}
	kept, dropped, fits := trimToFit(blocks, 0, 50)
	if !fits {
		t.Fatalf("fits = false, want true (dropping huge.jsonl alone satisfies the 50-byte cap)")
	}
	if len(dropped) != 1 || dropped[0].Path != "huge.jsonl" {
		t.Fatalf("dropped = %+v, want exactly huge.jsonl (the largest block)", dropped)
	}
	if len(kept) != 2 || kept[0].Path != "small1.go" || kept[1].Path != "small2.go" {
		t.Errorf("kept = %+v, want [small1.go, small2.go] in original relative order", kept)
	}
}

func TestTrimToFit_ExactBudgetEdge_Fits(t *testing.T) {
	blocks := []diffFileBlock{{Path: "a.go", Raw: strings.Repeat("x", 50)}}
	kept, dropped, fits := trimToFit(blocks, 0, 50)
	if !fits || len(kept) != 1 || len(dropped) != 0 {
		t.Errorf("trimToFit(exact budget) = kept=%+v dropped=%+v fits=%v, want kept=all fits=true", kept, dropped, fits)
	}
}

func TestTrimToFit_PreambleAloneExceedsCap_NeverFits(t *testing.T) {
	kept, dropped, fits := trimToFit(nil, 1000, 50)
	if fits {
		t.Error("fits = true, want false — an oversized preamble can never be trimmed away")
	}
	if kept != nil || dropped != nil {
		t.Errorf("kept/dropped = %+v/%+v, want both nil (no blocks to move)", kept, dropped)
	}
}

func TestTrimToFit_SingleOversizedBlockAlone_DoesNotFit(t *testing.T) {
	blocks := []diffFileBlock{{Path: "huge.jsonl", Raw: strings.Repeat("x", 1000)}}
	kept, dropped, fits := trimToFit(blocks, 0, 50)
	if fits {
		t.Error("fits = true, want false — dropping the PR's only file is nothing usable, not a successful trim")
	}
	if len(kept) != 0 || len(dropped) != 1 {
		t.Errorf("kept/dropped = %+v/%+v, want kept=none dropped=[huge.jsonl]", kept, dropped)
	}
}

func TestPathsOf(t *testing.T) {
	if got := pathsOf(nil); got != nil {
		t.Errorf("pathsOf(nil) = %+v, want nil", got)
	}
	blocks := []diffFileBlock{{Path: "a.go"}, {Path: "b.go"}}
	got := pathsOf(blocks)
	if len(got) != 2 || got[0] != "a.go" || got[1] != "b.go" {
		t.Errorf("pathsOf = %+v, want [a.go, b.go]", got)
	}
}
