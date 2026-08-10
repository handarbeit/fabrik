package pruefer

import (
	"sort"
	"strings"
)

// diffFileBlock is one file's contiguous block within a unified diff, as
// produced by splitDiffFiles: from a "diff --git a/... b/..." header line
// through (but not including) the next such header, or EOF.
type diffFileBlock struct {
	Path string // b/-side (post-change) path, matching ParseChangedPaths
	Raw  string // this file's diff block, header line included, "\n"-terminated
}

// splitDiffFiles splits a unified diff (as returned by FetchPRDiff) into its
// per-file blocks, keyed by the b/-side path exactly like ParseChangedPaths —
// so renames key by their destination path, consistent with existing
// exclusion-glob matching (select.go's allPathsExcluded/matchesAny). preamble
// carries any bytes before the first "diff --git" header; for a well-formed
// diff this is empty, but a malformed or headerless diff must never lose
// those bytes from the size accounting, since they can be neither excluded
// nor trimmed (see filterExcludedPaths and trimToFit).
func splitDiffFiles(diff string) (blocks []diffFileBlock, preamble string) {
	if diff == "" {
		return nil, ""
	}
	trailingNewline := strings.HasSuffix(diff, "\n")
	lines := strings.Split(diff, "\n")
	if trailingNewline {
		lines = lines[:len(lines)-1] // drop the empty element strings.Split leaves after a trailing "\n"
	}

	var preambleLines []string
	var curLines []string
	inFile := false

	flush := func() {
		if !inFile {
			return
		}
		blocks = append(blocks, diffFileBlock{Path: resolveFilePath(curLines), Raw: strings.Join(curLines, "\n")})
	}

	for _, line := range lines {
		if diffGitHeaderRE.MatchString(line) {
			flush()
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
		preamble = strings.Join(preambleLines, "\n")
	}

	// Every segment boundary inside the original diff (preamble-to-first-block,
	// block-to-next-block) was a real "\n" in the source text and must always
	// be restored, regardless of whether the diff as a whole ended in one.
	// Only the very last segment — the last block if any exist, else the
	// preamble itself — should gain a trailing "\n", and only when the
	// original diff had one: joinDiff must reconstruct the raw fetched diff
	// byte-for-byte (blocksLen/measuredBytes and the diff rebound for
	// validRightAnchors depend on it), so a source diff lacking a trailing
	// newline must not silently gain a phantom one here.
	if len(blocks) > 0 {
		for i := range blocks[:len(blocks)-1] {
			blocks[i].Raw += "\n"
		}
		if preamble != "" {
			preamble += "\n"
		}
		if trailingNewline {
			blocks[len(blocks)-1].Raw += "\n"
		}
	} else if preamble != "" && trailingNewline {
		preamble += "\n"
	}

	return blocks, preamble
}

// resolveFilePath determines a single file block's path from its unambiguous
// "+++ b/<path>" content line, reusing diffanchor.go's own !inHunk gating
// (diffHunkHeaderRE/diffNewFileHeaderRE) rather than re-deriving the fix a
// second time: an added content line that itself happens to start with
// "+++ " (e.g. a diff fixture embedded as test data, or — in the wild — a
// JSONL corpus line that begins with those four characters) must never be
// misread as a new file header once a hunk has started. This is the same
// "+++ mid-hunk" regression validRightAnchors already guards against (old
// commit 97cdce20 on the abandoned fabrik/issue-1274 branch).
//
// Falls back to the "diff --git a/X b/Y" header's b/-side capture — an
// AMBIGUOUS greedy split whenever the path itself contains the literal
// substring " b/" — only when no "+++ b/<path>" line is found at all: a
// deleted file ("+++ /dev/null"), a binary file (no textual hunks, no "+++"
// line whatsoever), or a pure mode-change with no content delta. This
// mirrors ParseChangedPaths's own pre-existing header-only convention, so
// the common case (every non-binary changed file has an unambiguous
// "+++ b/<path>" line) is what this closes, while quoted/special-character
// paths and binary files remain a carried-forward, documented limitation —
// not new here, not closed here.
func resolveFilePath(lines []string) string {
	inHunk := false
	for _, line := range lines {
		if !inHunk && strings.HasPrefix(line, "+++ ") {
			if m := diffNewFileHeaderRE.FindStringSubmatch(line); m != nil {
				return m[1]
			}
			continue
		}
		if diffHunkHeaderRE.MatchString(line) {
			inHunk = true
		}
	}
	if len(lines) > 0 {
		if m := diffGitHeaderRE.FindStringSubmatch(lines[0]); m != nil {
			return m[2]
		}
	}
	return ""
}

// pathsOf extracts the Path of each block, in order. Used to turn a
// diffFileBlock slice (dropped-by-exclusion or dropped-by-trim) into the
// plain path lists review.go threads into ReviewRequest's disclosure fields
// and the too-large-after-fetch notice body.
func pathsOf(blocks []diffFileBlock) []string {
	if len(blocks) == 0 {
		return nil
	}
	out := make([]string, len(blocks))
	for i, b := range blocks {
		out[i] = b.Path
	}
	return out
}

// filterExcludedPaths splits blocks into those that survive patterns (kept)
// and those matching at least one glob (dropped) — reusing the same
// matchesAny/matchGlob matcher select.go's allPathsExcluded uses for its own
// terminal all-or-nothing check. Only the aggregation differs: per-file
// here, not whole-diff. "every file excluded" falls out naturally as
// len(kept) == 0, so no existing exclusion scenario is lost, only relocated
// — review.go still runs the terminal allPathsExcluded check first (R3)
// before ever reaching this per-file pass.
func filterExcludedPaths(blocks []diffFileBlock, patterns []string) (kept, dropped []diffFileBlock) {
	if len(patterns) == 0 {
		return blocks, nil
	}
	for _, b := range blocks {
		if matchesAny(b.Path, patterns) {
			dropped = append(dropped, b)
		} else {
			kept = append(kept, b)
		}
	}
	return kept, dropped
}

// trimToFit greedily drops the largest remaining blocks (by raw byte length,
// descending) until preambleBytes plus the kept blocks' total is at or under
// maxBytes. Original relative order is preserved in both kept and dropped.
//
// fits is true only when at least one block survives the drop AND the total
// then fits — dropping every block to satisfy the cap isn't "reviewed the
// remainder", it's nothing to review, so callers must treat fits=false as
// "nothing usable" and fall through to a terminal skip rather than
// presenting an empty diff as though it were a complete review (see
// review.go's pathological-exhaustion fallback). preambleBytes can never be
// reduced by this function — there is nothing to drop it from — so a diff
// whose unattributed preamble alone exceeds maxBytes always reports
// fits=false regardless of how many blocks are dropped.
func trimToFit(blocks []diffFileBlock, preambleBytes, maxBytes int64) (kept, dropped []diffFileBlock, fits bool) {
	total := preambleBytes
	for _, b := range blocks {
		total += int64(len(b.Raw))
	}
	if total <= maxBytes {
		return blocks, nil, total <= maxBytes && len(blocks) > 0
	}

	order := make([]int, len(blocks))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(i, j int) bool { return len(blocks[order[i]].Raw) > len(blocks[order[j]].Raw) })

	drop := make(map[int]bool, len(blocks))
	for _, idx := range order {
		if total <= maxBytes {
			break
		}
		drop[idx] = true
		total -= int64(len(blocks[idx].Raw))
	}

	for i, b := range blocks {
		if drop[i] {
			dropped = append(dropped, b)
		} else {
			kept = append(kept, b)
		}
	}
	return kept, dropped, total <= maxBytes && len(kept) > 0
}

// joinDiff reassembles preamble and blocks back into a single diff string,
// suitable for passing to ParseChangedPaths and validRightAnchors — this is
// what makes R6 (anchoring stays correct) hold automatically: whatever
// review.go hands to validRightAnchors is exactly the subset the model was
// told to review, with no separate anchor-scrubbing logic needed.
func joinDiff(preamble string, blocks []diffFileBlock) string {
	var b strings.Builder
	b.WriteString(preamble)
	for _, blk := range blocks {
		b.WriteString(blk.Raw)
	}
	return b.String()
}

// blocksLen returns the total byte length of preamble plus every block's Raw
// — the measured size review.go compares against cfg.MaxDiffBytes after
// exclusion (R1/R7), without needing to materialize the joined string.
func blocksLen(preamble string, blocks []diffFileBlock) int64 {
	total := int64(len(preamble))
	for _, b := range blocks {
		total += int64(len(b.Raw))
	}
	return total
}
