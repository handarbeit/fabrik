package pruefer

import (
	"bufio"
	"regexp"
	"strconv"
	"strings"

	gh "github.com/handarbeit/fabrik/github"
)

// anchorKey identifies a single valid RIGHT-side (post-change) comment
// position in a diff.
type anchorKey struct {
	Path string
	Line int
}

var (
	diffFileHeaderRE    = regexp.MustCompile(`^diff --git `)
	diffNewFileHeaderRE = regexp.MustCompile(`^\+\+\+ b/(.+)$`)
	diffHunkHeaderRE    = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@`)
)

// validRightAnchors parses a unified diff (as returned by FetchPRDiff) and
// returns the set of (path, line) positions that are valid RIGHT-side
// GitHub review-comment anchors: every context or added line within a hunk,
// keyed by the hunk's "+++ b/<path>" header so renamed files resolve to
// their destination path. Deletion-only lines, lines outside any hunk, and
// files with no post-change path (deletions, "+++ /dev/null") produce no
// anchors — GitHub's line+side=RIGHT comment API only accepts a line number
// that exists in the new file version.
func validRightAnchors(diff string) map[anchorKey]bool {
	anchors := make(map[anchorKey]bool)
	var currentPath string
	var rightLine int
	inHunk := false

	scanner := bufio.NewScanner(strings.NewReader(diff))
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()

		switch {
		case diffFileHeaderRE.MatchString(line):
			currentPath = ""
			inHunk = false
			continue
		case !inHunk && strings.HasPrefix(line, "+++ "):
			// "+++ b/<path>" gives the post-change path; "+++ /dev/null"
			// (deleted file) or any other form leaves currentPath empty, so
			// this file contributes no RIGHT-side anchors. Gated on !inHunk
			// so an added content line that itself happens to start with
			// "+++ " (e.g. a diff fixture embedded as test data) is never
			// misread as a new file header mid-hunk.
			if m := diffNewFileHeaderRE.FindStringSubmatch(line); m != nil {
				currentPath = m[1]
			} else {
				currentPath = ""
			}
			inHunk = false
			continue
		}

		if m := diffHunkHeaderRE.FindStringSubmatch(line); m != nil {
			start, err := strconv.Atoi(m[1])
			if err != nil {
				inHunk = false
				continue
			}
			rightLine = start
			inHunk = true
			continue
		}

		if !inHunk || currentPath == "" {
			continue
		}

		switch {
		case strings.HasPrefix(line, "+"):
			anchors[anchorKey{Path: currentPath, Line: rightLine}] = true
			rightLine++
		case strings.HasPrefix(line, "-"):
			// Removed line: exists only on the left (base) side; the
			// right-side line counter does not advance.
		case strings.HasPrefix(line, "\\"):
			// "\ No newline at end of file" — not a content line.
		default:
			// Context line (unchanged, present on both sides).
			anchors[anchorKey{Path: currentPath, Line: rightLine}] = true
			rightLine++
		}
	}
	return anchors
}

// partitionFindings splits findings into those whose (Path, Line) is a
// valid RIGHT-side anchor per anchors — mapped to gh.ReviewComment, ready
// for SubmitPRReview's comments[] — and those that are not, returned
// unchanged so the caller can demote them into the review body rather than
// dropping them or risking a 422 on the whole submission.
func partitionFindings(findings []ReviewFinding, anchors map[anchorKey]bool) (comments []gh.ReviewComment, demoted []ReviewFinding) {
	for _, f := range findings {
		if anchors[anchorKey{Path: f.Path, Line: f.Line}] {
			comments = append(comments, gh.ReviewComment{Path: f.Path, Line: f.Line, Body: f.Body})
		} else {
			demoted = append(demoted, f)
		}
	}
	return comments, demoted
}
