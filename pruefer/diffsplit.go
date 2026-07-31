package pruefer

import (
	"regexp"
	"sort"
	"strings"
)

// diffFile is one file's contiguous block within a unified diff, as produced
// by splitDiffFiles: the "diff --git a/... b/..." header line through (but
// not including) the next such header, or EOF.
type diffFile struct {
	Path  string // b/-side (post-change) path, matching ParseChangedPaths
	Body  string // this file's diff block, header line included, "\n"-terminated
	Bytes int64  // len(Body), in raw diff bytes
}

// diffFileHeaderLineRE matches a single "diff --git a/<path> b/<path>" line
// and detects a new file block's start. Its own capture groups are an
// AMBIGUOUS, greedy a/...b/... split — used only as resolveFilePath's
// last-resort fallback, never as the primary path source; see
// resolveFilePath for why.
var diffFileHeaderLineRE = regexp.MustCompile(`^diff --git a/(.+) b/(.+)$`)

// diffNewPathLineRE, diffOldPathLineRE, and diffRenameToLineRE each match a
// line whose path is unambiguous by construction — unlike the "diff --git"
// header line, none of these can contain a second occurrence of their own
// marker within a legitimate diff line (see resolveFilePath).
var (
	diffNewPathLineRE  = regexp.MustCompile(`^\+\+\+ b/(.+)$`)
	diffOldPathLineRE  = regexp.MustCompile(`^--- a/(.+)$`)
	diffRenameToLineRE = regexp.MustCompile(`^rename to (.+)$`)
)

// splitDiffFiles splits a unified diff (as returned by FetchPRDiff) into its
// per-file blocks, keyed by the b/-side path exactly like ParseChangedPaths
// (so renames key by their destination path, consistent with existing
// exclusion-glob matching). preamble carries any bytes before the first
// "diff --git" header — for a well-formed diff this is empty, but a
// malformed or headerless diff must never lose those bytes from the size
// accounting, since they can be neither excluded nor auto-dropped (see
// filterExcludedPaths and trimToFit).
func splitDiffFiles(diff string) (files []diffFile, preamble string) {
	if diff == "" {
		return nil, ""
	}
	trailingNewline := strings.HasSuffix(diff, "\n")
	lines := strings.Split(diff, "\n")
	if trailingNewline {
		lines = lines[:len(lines)-1] // drop the empty element strings.Split leaves after a trailing "\n"
	}

	var preambleLines []string
	var headerPath string
	var curLines []string
	inFile := false

	flush := func() {
		if !inFile {
			return
		}
		body := strings.Join(curLines, "\n") + "\n"
		files = append(files, diffFile{Path: resolveFilePath(curLines, headerPath), Body: body, Bytes: int64(len(body))})
	}

	for _, line := range lines {
		if m := diffFileHeaderLineRE.FindStringSubmatch(line); m != nil {
			flush()
			headerPath = m[2]
			curLines = []string{line}
			inFile = true
			continue
		}
		if inFile {
			curLines = append(curLines, line)
		} else {
			preambleLines = append(preambleLines, line)
		}
	}
	flush()

	if len(preambleLines) > 0 {
		preamble = strings.Join(preambleLines, "\n") + "\n"
	}
	return files, preamble
}

// resolveFilePath determines a file block's Path, preferring lines that are
// unambiguous by construction over the "diff --git a/X b/Y" header's own
// greedy a/...b/... split. That split is ambiguous whenever a path itself
// contains the literal substring " b/": Go's regexp greedily maximizes the
// first capture group, which resolves to the LAST " b/" occurrence in the
// line — not necessarily the true a/-b/ separator — silently truncating
// headerPath to a suffix of the real path (verified: for a file literally
// named "foo b/bar.txt", the header line's greedy match yields "bar.txt",
// not "foo b/bar.txt"). This used to be cosmetic (only ParseChangedPaths
// relied on it, and only for a since-removed all-or-nothing exclusion
// check); it is now load-bearing, since a wrong Path can dodge an
// excluded_paths glob it should have matched, or misattribute bytes in the
// too-large notice's DominantPaths.
//
// "+++ b/<path>" (content changes) and "--- a/<path>" (deletions, where
// there is no b-side) are each anchored at line-start with their marker
// consumed by literal characters, not a greedy group — a second occurrence
// of "+++ b/" or "--- a/" can never appear at the start of another line in
// the same block: every hunk content line is itself prefixed with a
// leading +/-/space by the diff format, which shifts any lookalike content
// out of alignment with these patterns (the same invariant
// diffFileHeaderLineRE's own boundary-detection already relies on — see
// TestSplitDiffFiles_HeaderLookalikeAsRealDiffContent_NotTreatedAsNewFile).
// "rename to <path>" covers a pure rename with no content hunks (so no
// "+++"/"--- " lines exist at all) and is only ever emitted by git itself
// in the pre-hunk metadata area, never derived from file content.
//
// Falls back to the ambiguous headerPath only when none of those lines are
// present — e.g. a pure file-mode change with no content or rename delta.
func resolveFilePath(blockLines []string, headerPath string) string {
	for _, line := range blockLines {
		if m := diffNewPathLineRE.FindStringSubmatch(line); m != nil {
			return m[1]
		}
	}
	for _, line := range blockLines {
		if m := diffOldPathLineRE.FindStringSubmatch(line); m != nil {
			return m[1]
		}
	}
	for _, line := range blockLines {
		if m := diffRenameToLineRE.FindStringSubmatch(line); m != nil {
			return m[1]
		}
	}
	return headerPath
}

// pathsOf extracts the Path of each file, in order — used to turn a
// diffFile slice (dropped-by-exclusion or dropped-by-trim) into the plain
// path lists ReviewPR threads into ReviewRequest.ExcludedPaths,
// buildReviewBody's omittedPaths, and DiffSizeDetail.OmittedPaths.
func pathsOf(files []diffFile) []string {
	if len(files) == 0 {
		return nil
	}
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.Path
	}
	return out
}

// filterExcludedPaths splits files into those that survive cfg.ExcludedPaths
// (kept) and those matching at least one glob pattern (dropped). Reuses the
// same matchesAny/matchGlob matcher select.go's now-removed all-or-nothing
// allPathsExcluded used — only the aggregation changes: per-file instead of
// whole-diff. "every file excluded" falls out naturally as len(kept) == 0,
// so no existing exclusion scenario is lost, only relocated.
func filterExcludedPaths(files []diffFile, patterns []string) (kept, dropped []diffFile) {
	if len(patterns) == 0 {
		return files, nil
	}
	for _, f := range files {
		if matchesAny(f.Path, patterns) {
			dropped = append(dropped, f)
		} else {
			kept = append(kept, f)
		}
	}
	return kept, dropped
}

// trimToFit greedily drops the largest remaining files (by Bytes, descending)
// until preambleBytes plus the kept files' total is at or under maxBytes.
// Original relative order is preserved in both kept and dropped. fits is
// true only when at least one file survives the drop AND the total then
// fits — dropping every file to satisfy the cap isn't "reviewed the
// remainder", it's nothing to review, so callers must treat fits=false as
// "nothing usable" and fall through to the too-large notice rather than
// retrying with a smaller set. preambleBytes can never be reduced by this
// function (there is nothing to drop it from), so a diff whose unattributed
// preamble alone exceeds maxBytes always reports fits=false regardless of
// how many files are dropped.
func trimToFit(files []diffFile, preambleBytes, maxBytes int64) (kept, dropped []diffFile, fits bool) {
	total := preambleBytes
	for _, f := range files {
		total += f.Bytes
	}
	if total <= maxBytes {
		return files, nil, len(files) > 0
	}

	order := make([]int, len(files))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(i, j int) bool { return files[order[i]].Bytes > files[order[j]].Bytes })

	drop := make(map[int]bool, len(files))
	for _, idx := range order {
		if total <= maxBytes {
			break
		}
		drop[idx] = true
		total -= files[idx].Bytes
	}

	for i, f := range files {
		if drop[i] {
			dropped = append(dropped, f)
		} else {
			kept = append(kept, f)
		}
	}
	return kept, dropped, total <= maxBytes && len(kept) > 0
}
