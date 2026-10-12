package pruefer

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// syntheticLargeDiff builds a diff well past one Read call's limit: many small
// files, one huge multi-hunk file, and one file whose single added line is far
// larger than a whole chunk (the long-line case that truncates first).
func syntheticLargeDiff() (diff string, paths []string) {
	var b strings.Builder
	file := func(path string, hunks ...string) {
		fmt.Fprintf(&b, "diff --git a/%s b/%s\n--- a/%s\n+++ b/%s\n", path, path, path, path)
		for _, h := range hunks {
			b.WriteString(h)
		}
		paths = append(paths, path)
	}
	for i := 0; i < 40; i++ {
		var h strings.Builder
		fmt.Fprintf(&h, "@@ -1,2 +1,22 @@\n context\n")
		for j := 0; j < 20; j++ {
			fmt.Fprintf(&h, "+small line %d\n", j)
		}
		h.WriteString(" context\n")
		file(fmt.Sprintf("pkg/f%02d.go", i), h.String())
	}
	var hunks []string
	for hi := 0; hi < 5; hi++ {
		var h strings.Builder
		start := 1 + hi*1000
		fmt.Fprintf(&h, "@@ -%d,0 +%d,400 @@ func part%d()\n", start, start, hi)
		for j := 0; j < 400; j++ {
			fmt.Fprintf(&h, "+%s\n", strings.Repeat("x", 79))
		}
		hunks = append(hunks, h.String())
	}
	file("big/huge.go", hunks...)
	file("data/long.json", "@@ -0,0 +1 @@\n+"+strings.Repeat("y", 100*1024)+"\n")
	return b.String(), paths
}

// checkStagedCovers is the acceptance check for the staged layout: every
// changed path is listed in the index and on disk, no entry exceeds the read
// bound unless it is a single line that cannot be split, and IDs are unique.
func checkStagedCovers(s *StagedDiff, paths []string) error {
	index, err := os.ReadFile(s.IndexPath)
	if err != nil {
		return fmt.Errorf("reading index: %w", err)
	}
	seenID := map[string]bool{}
	seenPath := map[string]bool{}
	for _, e := range s.Entries {
		if seenID[e.ID] {
			return fmt.Errorf("duplicate entry ID %s", e.ID)
		}
		seenID[e.ID] = true
		seenPath[e.Path] = true
		data, err := os.ReadFile(s.EntryPath(e))
		if err != nil {
			return fmt.Errorf("entry %s not on disk: %w", e.ID, err)
		}
		if len(data) != e.Bytes {
			return fmt.Errorf("entry %s is %d bytes on disk, index says %d", e.ID, len(data), e.Bytes)
		}
		if e.Bytes > stagedChunkMaxBytes && !e.Long {
			return fmt.Errorf("entry %s is %d bytes, over the %d read bound, and is not flagged long-line", e.ID, e.Bytes, stagedChunkMaxBytes)
		}
		if !strings.Contains(string(index), e.ID+"\t") {
			return fmt.Errorf("entry %s is not listed in the index", e.ID)
		}
	}
	for _, p := range paths {
		if !seenPath[p] {
			return fmt.Errorf("changed path %q has no staged entry", p)
		}
		if !strings.Contains(string(index), "\t"+p+"\t") {
			return fmt.Errorf("changed path %q is not in the index", p)
		}
	}
	return nil
}

// legacyStaged reproduces the pre-#2190 layout — the whole diff in one file —
// as a StagedDiff, so the same check that passes the new layout can be shown to
// fail the old one.
func legacyStaged(t *testing.T, diff string) *StagedDiff {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "0001.diff"), []byte(diff), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, stagedIndexName), []byte("0001\t0001.diff\t(whole diff)\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &StagedDiff{Dir: dir, IndexPath: filepath.Join(dir, stagedIndexName),
		Entries: []StagedEntry{{ID: "0001", File: "0001.diff", Path: "(whole diff)", Chunk: 1, Chunks: 1, Bytes: len(diff)}}}
}

func TestStageDiff_LargeDiffCoversEveryPath(t *testing.T) {
	diff, paths := syntheticLargeDiff()
	if len(diff) < 4*stagedChunkMaxBytes {
		t.Fatalf("fixture too small to exceed one read: %d bytes", len(diff))
	}
	blocks, preamble := splitDiffFiles(diff)
	s, cleanup, err := stageDiff(blocks, preamble, 7)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	if err := checkStagedCovers(s, paths); err != nil {
		t.Fatalf("staged layout does not cover the diff: %v", err)
	}
	var chunksOfHuge, longEntries int
	for _, e := range s.Entries {
		if e.Path == "big/huge.go" {
			chunksOfHuge++
		}
		if e.Long {
			longEntries++
			if e.Path != "data/long.json" {
				t.Errorf("unexpected long-line entry %+v", e)
			}
		}
		if e.Lines > stagedChunkMaxLines && !e.Long {
			t.Errorf("entry %s has %d lines, over the %d bound", e.ID, e.Lines, stagedChunkMaxLines)
		}
	}
	if chunksOfHuge < 3 {
		t.Errorf("huge file staged as %d chunk(s), want it split", chunksOfHuge)
	}
	if longEntries != 1 {
		t.Errorf("long-line entries = %d, want exactly 1", longEntries)
	}
}

// TestStageDiff_LegacySingleFileLayoutFailsTheCheck is the non-vacuity check:
// with the fix neutralised (the whole diff in one file) the same assertion
// fails, so the test above can only pass because of the staged layout.
func TestStageDiff_LegacySingleFileLayoutFailsTheCheck(t *testing.T) {
	diff, paths := syntheticLargeDiff()
	if err := checkStagedCovers(legacyStaged(t, diff), paths); err == nil {
		t.Fatal("checkStagedCovers accepted the legacy single-file layout")
	}
}

// TestChunkBlock_PreservesPostChangeLineNumbers: a file cut into chunks must
// keep every line anchorable at its original post-change line number, with the
// chunks together reproducing exactly the original body lines.
func TestChunkBlock_PreservesPostChangeLineNumbers(t *testing.T) {
	diff, _ := syntheticLargeDiff()
	blocks, _ := splitDiffFiles(diff)
	for _, b := range blocks {
		if b.Path != "big/huge.go" && b.Path != "data/long.json" && b.Path != "pkg/f00.go" {
			continue
		}
		chunks := chunkBlock(b.Raw, stagedChunkMaxBytes, stagedChunkMaxLines)
		want := validRightAnchors(b.Raw)
		got := map[anchorKey]bool{}
		var bodyWant, bodyGot []string
		for _, l := range strings.Split(strings.TrimSuffix(b.Raw, "\n"), "\n") {
			if strings.HasPrefix(l, "+") || strings.HasPrefix(l, " ") || strings.HasPrefix(l, "-") {
				if !strings.HasPrefix(l, "+++ ") && !strings.HasPrefix(l, "--- ") {
					bodyWant = append(bodyWant, l)
				}
			}
		}
		for _, c := range chunks {
			if !strings.HasPrefix(c, "diff --git a/"+b.Path) {
				t.Errorf("%s: chunk lacks the repeated file header", b.Path)
			}
			for k := range validRightAnchors(c) {
				got[k] = true
			}
			inHunk := false
			for _, l := range strings.Split(strings.TrimSuffix(c, "\n"), "\n") {
				if strings.HasPrefix(l, "@@") {
					inHunk = true
					continue
				}
				if inHunk {
					bodyGot = append(bodyGot, l)
				}
			}
		}
		if len(got) != len(want) {
			t.Errorf("%s: chunks anchor %d lines, original %d", b.Path, len(got), len(want))
		}
		for k := range want {
			if !got[k] {
				t.Errorf("%s: anchor %+v lost by chunking", b.Path, k)
			}
		}
		if strings.Join(bodyGot, "\n") != strings.Join(bodyWant, "\n") {
			t.Errorf("%s: chunk bodies do not reassemble to the original hunk lines", b.Path)
		}
	}
}

func TestChunkBlock_SmallBlockIsUnchanged(t *testing.T) {
	raw := "diff --git a/a b/a\n--- a/a\n+++ b/a\n@@ -1 +1 @@\n-a\n+b\n"
	chunks := chunkBlock(raw, stagedChunkMaxBytes, stagedChunkMaxLines)
	if len(chunks) != 1 || chunks[0] != raw {
		t.Errorf("chunks = %q, want the block unchanged", chunks)
	}
}

var stagedNameRE = regexp.MustCompile(`^(\d{4}\.diff|INDEX\.txt)$`)

// TestStageDiff_HostilePathsStayInsideTheDirectory: names derive from IDs, never
// from PR paths, so traversal, empty, newline, unicode and duplicate paths all
// land as ordinary files inside the staging directory.
func TestStageDiff_HostilePathsStayInsideTheDirectory(t *testing.T) {
	var blocks []diffFileBlock
	for _, p := range []string{"../../etc/passwd", "", "a\nb\nfake\t1\t1", "日本語/ファイル.go", "dup.go", "dup.go", strings.Repeat("d/", 400) + "x.go"} {
		blocks = append(blocks, diffFileBlock{Path: p, Raw: "diff --git a/x b/x\n+++ b/x\n@@ -0,0 +1 @@\n+hi\n"})
	}
	s, cleanup, err := stageDiff(blocks, "", 3)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	ents, err := os.ReadDir(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
		if !stagedNameRE.MatchString(e.Name()) {
			t.Errorf("unexpected file %q in staging dir", e.Name())
		}
	}
	sort.Strings(names)
	if len(names) != len(blocks)+1 {
		t.Errorf("staging dir has %d files, want %d entries plus the index: %v", len(names), len(blocks), names)
	}
	for _, e := range s.Entries {
		if filepath.Dir(s.EntryPath(e)) != s.Dir {
			t.Errorf("entry %s resolves outside the staging dir: %s", e.ID, s.EntryPath(e))
		}
	}
	index, _ := os.ReadFile(s.IndexPath)
	if got := strings.Count(string(index), "\n"); got != 2+len(blocks) {
		t.Errorf("index has %d lines, want %d (a newline in a path must not forge an extra row):\n%s", got, 2+len(blocks), index)
	}
}

func TestStageDiff_PreambleIsItsOwnEntry(t *testing.T) {
	s, cleanup, err := stageDiff([]diffFileBlock{{Path: "a", Raw: "diff --git a/a b/a\n"}}, "stray preamble\n", 1)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if len(s.Entries) != 2 || s.Entries[0].Path != stagedPreamblePath {
		t.Errorf("entries = %+v, want the preamble first", s.Entries)
	}
}
