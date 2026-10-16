package pruefer

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Staged-diff layout (#2190, ADR 2190). The review used to get the whole diff
// as one file, read through Read's per-call token cap: a diff with long lines
// was cut off well before its end and the model often stopped paging, so the
// tail (tests, sim, later files) went unread. The diff is now staged as a
// directory of small, independently readable entries plus an index, so no part
// of it can be silently cut off and every entry is something that can be
// reported opened or unread.
const (
	// stagedChunkMaxBytes bounds one staged entry file. Chosen by bytes, not
	// lines: a long-line diff hits Read's token cap long before 2000 lines, so
	// line count is not the binding limit. Conservative against the cap; the
	// liveprobe test measures it against the real CLI.
	stagedChunkMaxBytes = 16 * 1024
	// stagedBatchMaxBytes is the most the reviewer is told to open in one `cat`:
	// the combined `bytes` of the entries named, as INDEX.txt lists them (#2362,
	// ADR 2362). The Claude CLI returns a Bash result whole only below about
	// 30 KB; above that it saves the output to a file and hands the model a
	// 2 KB `<persisted-output>` preview, so the rest of the batch is never seen.
	// 20 KiB leaves a margin of roughly 10 KB for the unmeasured threshold and
	// for any difference between bytes and characters. This is the one place
	// the number lives: renderDynamicContext formats it and a test asserts the
	// review skill's text states the same value.
	stagedBatchMaxBytes = 20 * 1024
	// stagedChunkMaxLines bounds one staged entry file by line count.
	stagedChunkMaxLines = 600
	// stagedIndexName is the index file inside the staging directory.
	stagedIndexName = "INDEX.txt"
	// stagedWideLineBytes is the line width above which Read cuts a line
	// mid-line (it truncates lines of about 2000 characters). An entry holding
	// such a line is flagged wide-line and must be opened with cat.
	stagedWideLineBytes = 2000
	// stagedPreamblePath is the display path of the diff text that precedes the
	// first "diff --git" header, when there is any.
	stagedPreamblePath = "(diff preamble)"
)

// StagedEntry is one file in the staging directory: one changed path, or one
// bounded chunk of a large path.
type StagedEntry struct {
	ID     string // "0001"; what the reviewer reports in its coverage block
	File   string // base name inside the staging directory, "0001.diff"
	Path   string // original changed path, for display and for the unread list
	Chunk  int    // 1-based chunk number within Path
	Chunks int    // total chunks of Path
	Lines  int
	Bytes  int
	Long   bool // a single line (or header) larger than the byte cap: cannot be split further
	Wide   bool // holds a line over stagedWideLineBytes: Read would cut it, so only cat opens it

	content string // the entry file's text, for verifying a cat against it
}

// StagedDiff describes a staging directory written by stageDiff.
type StagedDiff struct {
	Dir       string // symlink-resolved absolute directory
	IndexPath string
	Entries   []StagedEntry
}

// EntryPath returns the absolute path of e's file.
func (s *StagedDiff) EntryPath(e StagedEntry) string {
	return filepath.Join(s.Dir, e.File)
}

var diffHunkFullRE = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@(.*)$`)

// diffHunk is one parsed hunk of a file block.
type diffHunk struct {
	oldStart, newStart int
	section            string // text after the closing "@@", including the leading space
	lines              []string
}

// stagePiece is a run of hunk lines with the header it will be written under.
type stagePiece struct {
	header string
	lines  []string
}

func (p stagePiece) bytes() int {
	n := len(p.header) + 1
	for _, l := range p.lines {
		n += len(l) + 1
	}
	return n
}

func (p stagePiece) lineCount() int { return 1 + len(p.lines) }

// hunkLineCounts returns how many old-side and new-side lines a hunk body line
// accounts for. "\ No newline at end of file" counts for neither.
func hunkLineCounts(l string) (old, new int) {
	switch {
	case strings.HasPrefix(l, "\\"):
		return 0, 0
	case strings.HasPrefix(l, "+"):
		return 0, 1
	case strings.HasPrefix(l, "-"):
		return 1, 0
	default: // context line (an empty line is a context line with its space stripped)
		return 1, 1
	}
}

// parseFileBlock splits a file block's lines into header lines (everything
// before the first hunk) and hunks. Lines the header ends with are kept verbatim.
func parseFileBlock(lines []string) (header []string, hunks []diffHunk) {
	for _, l := range lines {
		if m := diffHunkFullRE.FindStringSubmatch(l); m != nil {
			oldStart, _ := strconv.Atoi(m[1])
			newStart, _ := strconv.Atoi(m[3])
			hunks = append(hunks, diffHunk{oldStart: oldStart, newStart: newStart, section: m[5]})
			continue
		}
		if len(hunks) == 0 {
			header = append(header, l)
			continue
		}
		h := &hunks[len(hunks)-1]
		h.lines = append(h.lines, l)
	}
	return header, hunks
}

// splitHunk cuts a hunk into pieces each within the byte and line budget, with
// hunk headers that carry the true starting line numbers and counts of each
// piece, so post-change line numbers stay correct. A line larger than the
// budget becomes a piece of its own.
func splitHunk(h diffHunk, budgetBytes, budgetLines int) []stagePiece {
	var pieces []stagePiece
	oldLine, newLine := h.oldStart, h.newStart
	var cur []string
	curOld, curNew := 0, 0
	startOld, startNew := oldLine, newLine
	size := 0
	flush := func() {
		if len(cur) == 0 {
			return
		}
		hdr := fmt.Sprintf("@@ -%d,%d +%d,%d @@%s", startOld, curOld, startNew, curNew, h.section)
		pieces = append(pieces, stagePiece{header: hdr, lines: cur})
		cur = nil
		curOld, curNew = 0, 0
		size = 0
		startOld, startNew = oldLine, newLine
	}
	for _, l := range h.lines {
		add := len(l) + 1
		if len(cur) > 0 && (size+add > budgetBytes || len(cur)+2 > budgetLines) {
			flush()
		}
		cur = append(cur, l)
		size += add
		o, n := hunkLineCounts(l)
		curOld += o
		curNew += n
		oldLine += o
		newLine += n
	}
	flush()
	return pieces
}

// hunkPiece renders a whole hunk as one piece, with its original header.
func hunkPiece(h diffHunk, oldCount, newCount int) stagePiece {
	return stagePiece{header: fmt.Sprintf("@@ -%d,%d +%d,%d @@%s", h.oldStart, oldCount, h.newStart, newCount, h.section), lines: h.lines}
}

// chunkBlock splits one file block (Raw, "diff --git" header through the end)
// into chunk texts, each within maxBytes/maxLines or flagged long. Every chunk
// repeats the file's header lines; a hunk split across chunks gets synthesized
// hunk headers with correct line numbers. A block that fits, or has no hunks
// to split, comes back as a single chunk byte-identical to the input.
func chunkBlock(raw string, maxBytes, maxLines int) []string {
	trailingNL := strings.HasSuffix(raw, "\n")
	lines := strings.Split(strings.TrimSuffix(raw, "\n"), "\n")
	if len(raw) <= maxBytes && len(lines) <= maxLines {
		return []string{raw}
	}
	header, hunks := parseFileBlock(lines)
	if len(hunks) == 0 {
		return []string{raw}
	}
	headerBytes := 0
	for _, l := range header {
		headerBytes += len(l) + 1
	}
	// Leave room for the header; never let it starve the budget to nothing.
	budgetBytes := maxBytes - headerBytes
	if budgetBytes < maxBytes/2 {
		budgetBytes = maxBytes / 2
	}
	budgetLines := maxLines - len(header)
	if budgetLines < maxLines/2 {
		budgetLines = maxLines / 2
	}

	var pieces []stagePiece
	for _, h := range hunks {
		var oldN, newN int
		for _, l := range h.lines {
			o, n := hunkLineCounts(l)
			oldN += o
			newN += n
		}
		whole := hunkPiece(h, oldN, newN)
		if whole.bytes() <= budgetBytes && whole.lineCount() <= budgetLines {
			pieces = append(pieces, whole)
			continue
		}
		pieces = append(pieces, splitHunk(h, budgetBytes-len(h.section)-32, budgetLines)...)
	}

	var chunks []string
	var cur strings.Builder
	curLines, curBytes := 0, 0
	open := func() {
		cur.Reset()
		for _, l := range header {
			cur.WriteString(l)
			cur.WriteByte('\n')
		}
		curLines, curBytes = len(header), headerBytes
	}
	closeChunk := func() {
		s := cur.String()
		if !trailingNL {
			s = strings.TrimSuffix(s, "\n")
		}
		chunks = append(chunks, s)
	}
	open()
	hasPiece := false
	for _, p := range pieces {
		if hasPiece && (curBytes+p.bytes() > maxBytes || curLines+p.lineCount() > maxLines) {
			closeChunk()
			open()
			hasPiece = false
		}
		cur.WriteString(p.header)
		cur.WriteByte('\n')
		for _, l := range p.lines {
			cur.WriteString(l)
			cur.WriteByte('\n')
		}
		curBytes += p.bytes()
		curLines += p.lineCount()
		hasPiece = true
	}
	closeChunk()
	return chunks
}

// sanitizeIndexText makes a path safe to print on one index line.
func sanitizeIndexText(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '?'
		}
		return r
	}, s)
}

// buildStagedEntries chunks every block (and a non-empty preamble) and assigns
// derived IDs and file names. Names never derive from PR paths, so a hostile or
// empty path, a duplicate, or a very long or non-ASCII name cannot escape or
// collide inside the directory. contents[i] is the file text of Entries[i].
func buildStagedEntries(blocks []diffFileBlock, preamble string) (entries []StagedEntry, contents []string) {
	add := func(path string, chunks []string) {
		for i, c := range chunks {
			id := fmt.Sprintf("%04d", len(entries)+1)
			lines := strings.Count(c, "\n")
			if c != "" && !strings.HasSuffix(c, "\n") {
				lines++
			}
			e := StagedEntry{
				ID: id, File: id + ".diff", Path: path,
				Chunk: i + 1, Chunks: len(chunks),
				Lines: lines, Bytes: len(c), content: c,
			}
			e.Long = len(c) > stagedChunkMaxBytes
			for _, l := range strings.Split(c, "\n") {
				if len(l) > stagedWideLineBytes {
					e.Wide = true
					break
				}
			}
			entries = append(entries, e)
			contents = append(contents, c)
		}
	}
	if preamble != "" {
		add(stagedPreamblePath, []string{preamble})
	}
	for _, b := range blocks {
		add(b.Path, chunkBlock(b.Raw, stagedChunkMaxBytes, stagedChunkMaxLines))
	}
	return entries, contents
}

// renderIndex renders INDEX.txt: one line per entry with its absolute file
// path, original path, chunk position, size and flags.
func renderIndex(dir string, entries []StagedEntry) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Pruefer staged diff index: %d entr", len(entries))
	if len(entries) == 1 {
		b.WriteString("y")
	} else {
		b.WriteString("ies")
	}
	b.WriteString(". Open every entry.\n")
	b.WriteString("id\tfile\tpath\tchunk\tlines\tbytes\tflags\n")
	for _, e := range entries {
		var flagList []string
		if e.Long {
			flagList = append(flagList, "long-line")
		}
		if e.Wide {
			flagList = append(flagList, "wide-line:open-with-cat")
		}
		flags := strings.Join(flagList, ",")
		fmt.Fprintf(&b, "%s\t%s\t%s\t%d/%d\t%d\t%d\t%s\n",
			e.ID, filepath.Join(dir, e.File), sanitizeIndexText(e.Path), e.Chunk, e.Chunks, e.Lines, e.Bytes, flags)
	}
	return b.String()
}

// stageDiff writes the staging directory for blocks (and preamble) and returns
// its description plus an idempotent cleanup. The directory is created outside
// the review clone so it never appears in the reviewed tree, and is resolved
// through symlinks so the permission rules name the path the CLI matches.
func stageDiff(blocks []diffFileBlock, preamble string, prNumber int) (*StagedDiff, func(), error) {
	noop := func() {}
	dir, err := os.MkdirTemp("", fmt.Sprintf("pruefer-pr-%d-diff-*", prNumber))
	if err != nil {
		return nil, noop, fmt.Errorf("creating staging dir: %w", err)
	}
	cleanup := func() { os.RemoveAll(dir) }
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		cleanup()
		return nil, noop, fmt.Errorf("resolving staging dir: %w", err)
	}
	entries, contents := buildStagedEntries(blocks, preamble)
	if len(entries) == 0 {
		cleanup()
		return nil, noop, fmt.Errorf("staging diff: nothing to stage")
	}
	for i, e := range entries {
		if err := os.WriteFile(filepath.Join(resolved, e.File), []byte(contents[i]), 0o600); err != nil {
			cleanup()
			return nil, noop, fmt.Errorf("writing staged entry %s: %w", e.ID, err)
		}
	}
	indexPath := filepath.Join(resolved, stagedIndexName)
	if err := os.WriteFile(indexPath, []byte(renderIndex(resolved, entries)), 0o600); err != nil {
		cleanup()
		return nil, noop, fmt.Errorf("writing staged index: %w", err)
	}
	return &StagedDiff{Dir: resolved, IndexPath: indexPath, Entries: entries}, cleanup, nil
}

// stagedBatchLimitText renders stagedBatchMaxBytes the way the prompt and the
// review skill state it.
func stagedBatchLimitText() string {
	return fmt.Sprintf("%d bytes", stagedBatchMaxBytes)
}
