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

// diffFileHeaderLineRE matches a single "diff --git a/<path> b/<path>" line.
var diffFileHeaderLineRE = regexp.MustCompile(`^diff --git a/(.+) b/(.+)$`)

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
	var curPath string
	var curLines []string
	inFile := false

	flush := func() {
		if !inFile {
			return
		}
		body := strings.Join(curLines, "\n") + "\n"
		files = append(files, diffFile{Path: curPath, Body: body, Bytes: int64(len(body))})
	}

	for _, line := range lines {
		if m := diffFileHeaderLineRE.FindStringSubmatch(line); m != nil {
			flush()
			curPath = m[2]
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
