package pruefer

import "testing"

const simpleDiff = `diff --git a/foo.go b/foo.go
index 1111111..2222222 100644
--- a/foo.go
+++ b/foo.go
@@ -10,5 +10,6 @@ func Foo() {
 	unchanged1
 	unchanged2
-	removed
+	added1
+	added2
 	unchanged3
`

func TestValidRightAnchors_OffByOneLineCorrectness(t *testing.T) {
	anchors := validRightAnchors(simpleDiff)

	// Hunk header "@@ -10,5 +10,6 @@" means the new file's first printed
	// line (the first context line "unchanged1") is line 10.
	want := map[anchorKey]bool{
		{Path: "foo.go", Line: 10}: true, // unchanged1 (context)
		{Path: "foo.go", Line: 11}: true, // unchanged2 (context)
		{Path: "foo.go", Line: 12}: true, // added1
		{Path: "foo.go", Line: 13}: true, // added2
		{Path: "foo.go", Line: 14}: true, // unchanged3 (context)
	}
	if len(anchors) != len(want) {
		t.Fatalf("anchors = %v, want %v", anchors, want)
	}
	for k := range want {
		if !anchors[k] {
			t.Errorf("missing expected anchor %+v", k)
		}
	}
}

func TestValidRightAnchors_DeletionOnlyLineRejected(t *testing.T) {
	// The hunk has 3 context lines + 1 removed + 2 added = 6 diff lines, but
	// only 5 of them exist on the right side: the removed line consumes no
	// right-side line number at all, so it can never itself be a valid
	// anchor and must not inflate the anchor count.
	anchors := validRightAnchors(simpleDiff)
	if len(anchors) != 5 {
		t.Fatalf("expected exactly 5 anchors (deletion contributes none), got %d: %v", len(anchors), anchors)
	}
}

func TestValidRightAnchors_PathNotInDiff_NoAnchors(t *testing.T) {
	anchors := validRightAnchors(simpleDiff)
	if anchors[anchorKey{Path: "unrelated.go", Line: 10}] {
		t.Error("expected no anchor for a path never touched by the diff")
	}
}

func TestValidRightAnchors_DeletedFile_NoAnchors(t *testing.T) {
	diff := `diff --git a/gone.go b/gone.go
deleted file mode 100644
index 1111111..0000000
--- a/gone.go
+++ /dev/null
@@ -1,3 +0,0 @@
-line1
-line2
-line3
`
	anchors := validRightAnchors(diff)
	if len(anchors) != 0 {
		t.Errorf("expected no anchors for a deleted file, got %v", anchors)
	}
}

func TestValidRightAnchors_RenamedFile_UsesDestinationPath(t *testing.T) {
	diff := `diff --git a/old_name.go b/new_name.go
similarity index 95%
rename from old_name.go
rename to new_name.go
index 1111111..2222222 100644
--- a/old_name.go
+++ b/new_name.go
@@ -1,2 +1,3 @@
 unchanged
+added
 tail
`
	anchors := validRightAnchors(diff)
	if !anchors[anchorKey{Path: "new_name.go", Line: 1}] {
		t.Error("expected anchor at new_name.go:1 (unchanged context line)")
	}
	if !anchors[anchorKey{Path: "new_name.go", Line: 2}] {
		t.Error("expected anchor at new_name.go:2 (added line)")
	}
	if anchors[anchorKey{Path: "old_name.go", Line: 1}] {
		t.Error("expected no anchors under the old (source) path")
	}
}

func TestValidRightAnchors_MultipleFilesAndHunks(t *testing.T) {
	diff := `diff --git a/a.go b/a.go
--- a/a.go
+++ b/a.go
@@ -1,2 +1,2 @@
 ctx
-old
+new
diff --git a/b.go b/b.go
--- a/b.go
+++ b/b.go
@@ -5,1 +5,2 @@
 ctx5
+added6
`
	anchors := validRightAnchors(diff)
	want := []anchorKey{
		{Path: "a.go", Line: 1},
		{Path: "a.go", Line: 2},
		{Path: "b.go", Line: 5},
		{Path: "b.go", Line: 6},
	}
	if len(anchors) != len(want) {
		t.Fatalf("anchors = %v, want %d entries", anchors, len(want))
	}
	for _, k := range want {
		if !anchors[k] {
			t.Errorf("missing expected anchor %+v", k)
		}
	}
}

func TestPartitionFindings_AllAnchorable(t *testing.T) {
	anchors := map[anchorKey]bool{
		{Path: "foo.go", Line: 10}: true,
		{Path: "foo.go", Line: 12}: true,
	}
	findings := []ReviewFinding{
		{Path: "foo.go", Line: 10, Body: "finding A"},
		{Path: "foo.go", Line: 12, Body: "finding B"},
	}
	comments, demoted := partitionFindings(findings, anchors)
	if len(comments) != 2 || len(demoted) != 0 {
		t.Fatalf("comments=%v demoted=%v, want 2 comments, 0 demoted", comments, demoted)
	}
}

func TestPartitionFindings_SomeUnanchorable(t *testing.T) {
	anchors := map[anchorKey]bool{
		{Path: "foo.go", Line: 10}: true,
	}
	findings := []ReviewFinding{
		{Path: "foo.go", Line: 10, Body: "anchorable"},
		{Path: "foo.go", Line: 999, Body: "unanchorable"},
	}
	comments, demoted := partitionFindings(findings, anchors)
	if len(comments) != 1 || comments[0].Body != "anchorable" {
		t.Fatalf("comments = %+v, want 1 entry with body 'anchorable'", comments)
	}
	if len(demoted) != 1 || demoted[0].Body != "unanchorable" {
		t.Fatalf("demoted = %+v, want 1 entry with body 'unanchorable'", demoted)
	}
}

func TestPartitionFindings_NoneAnchorable(t *testing.T) {
	anchors := map[anchorKey]bool{}
	findings := []ReviewFinding{
		{Path: "foo.go", Line: 10, Body: "finding A"},
	}
	comments, demoted := partitionFindings(findings, anchors)
	if len(comments) != 0 {
		t.Fatalf("comments = %v, want none", comments)
	}
	if len(demoted) != 1 {
		t.Fatalf("demoted = %v, want 1", demoted)
	}
}
