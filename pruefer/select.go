package pruefer

import (
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	gh "github.com/handarbeit/fabrik/github"
)

// SkipReason names why a PR was not selected for review. Used both for
// structured logging and by callers that want to explain a skip decision to
// a human. The zero value (empty string) is never returned by Eligible when
// it reports true.
type SkipReason string

const (
	SkipDraft           SkipReason = "draft"
	SkipSelfAuthored    SkipReason = "self-authored: PR author is the review identity"
	SkipExcludedAuthor  SkipReason = "excluded author"
	SkipExcludedLabel   SkipReason = "excluded label"
	SkipExcludedPath    SkipReason = "excluded path: every touched file matches an exclusion glob"
	SkipAlreadyReviewed SkipReason = "already reviewed at this head SHA"
	SkipDiffTooLarge    SkipReason = "diff exceeds max_diff_bytes"
)

// EligibilityInput bundles everything Eligible needs to decide whether a PR
// should be reviewed.
type EligibilityInput struct {
	PR              gh.PRDetails
	BotLogin        string // Pruefer's own review identity, e.g. "pruefer-bot[bot]"
	ExcludedAuthors []string
	ExcludedLabels  []string
	// ExistingReviews are the PR's current reviews (any author); Eligible
	// looks specifically for one authored by BotLogin at PR.HeadSHA.
	ExistingReviews []gh.PRReview
	// ForceReview is true when an unprocessed "/pruefer review" comment
	// requests a fresh review of the current head regardless of prior
	// review state.
	ForceReview bool
}

// Eligible reports whether a PR should be reviewed, and if not, why. This is
// the cheap, pre-diff-fetch check: draft, self-authored, and excluded-
// author/label only — path exclusion is NOT decided here, since it
// fundamentally requires per-file diff structure that only exists after
// FetchPRDiff (see filterExcludedPaths in diffsplit.go, applied by
// ReviewPR before the diff is measured against MaxDiffBytes).
// ForceReview overrides only the already-reviewed-at-this-SHA check — draft,
// self-authored, and excluded-author/label checks always apply, since
// "/pruefer review" forces a *fresh* review, not a bypass of every safety
// check (see adrs/1113-pruefer-v1-architecture.md).
func Eligible(in EligibilityInput) (bool, SkipReason) {
	if in.PR.Draft {
		return false, SkipDraft
	}
	if in.BotLogin != "" && strings.EqualFold(in.PR.Author, in.BotLogin) {
		return false, SkipSelfAuthored
	}
	for _, a := range in.ExcludedAuthors {
		if strings.EqualFold(a, in.PR.Author) {
			return false, SkipExcludedAuthor
		}
	}
	for _, l := range in.ExcludedLabels {
		for _, prLabel := range in.PR.Labels {
			if strings.EqualFold(l, prLabel) {
				return false, SkipExcludedLabel
			}
		}
	}
	if !in.ForceReview && alreadyReviewedAtHead(in.ExistingReviews, in.BotLogin, in.PR.HeadSHA) {
		return false, SkipAlreadyReviewed
	}
	return true, ""
}

func matchesAny(path string, patterns []string) bool {
	for _, pat := range patterns {
		if matchGlob(pat, path) {
			return true
		}
	}
	return false
}

// matchGlob matches path against pattern using filepath.Match semantics per
// path segment, plus "**" as a segment that matches zero or more path
// segments (so "vendor/**" excludes everything under vendor/, matching the
// documented behavior in cmd/pruefer/README.md — plain filepath.Match alone
// never lets "*" cross a "/" and so cannot express that).
func matchGlob(pattern, path string) bool {
	return matchGlobParts(strings.Split(pattern, "/"), strings.Split(path, "/"))
}

func matchGlobParts(pat, name []string) bool {
	if len(pat) == 0 {
		return len(name) == 0
	}
	if pat[0] == "**" {
		if matchGlobParts(pat[1:], name) {
			return true
		}
		return len(name) > 0 && matchGlobParts(pat, name[1:])
	}
	if len(name) == 0 {
		return false
	}
	ok, err := filepath.Match(pat[0], name[0])
	if err != nil || !ok {
		return false
	}
	return matchGlobParts(pat[1:], name[1:])
}

// alreadyReviewedAtHead reports whether reviews contains a review authored
// by botLogin whose CommitID matches headSHA exactly.
func alreadyReviewedAtHead(reviews []gh.PRReview, botLogin, headSHA string) bool {
	if botLogin == "" || headSHA == "" {
		return false
	}
	for _, r := range reviews {
		if strings.EqualFold(r.Author, botLogin) && r.CommitID == headSHA {
			return true
		}
	}
	return false
}

// diffGitHeaderRE matches a unified diff's per-file header line:
// "diff --git a/<path> b/<path>".
var diffGitHeaderRE = regexp.MustCompile(`(?m)^diff --git a/(.+) b/(.+)$`)

// ParseChangedPaths extracts the set of file paths touched by a unified
// diff, from its "diff --git a/... b/..." header lines. Uses the b/
// (post-change) path; for renames this differs from the a/ path, but which
// side of a rename is reported doesn't matter for glob-based path exclusion.
// Returns nil if the diff contains no recognizable file headers.
func ParseChangedPaths(diff string) []string {
	matches := diffGitHeaderRE.FindAllStringSubmatch(diff, -1)
	if len(matches) == 0 {
		return nil
	}
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, m[2])
	}
	return out
}

// PathSize names a single file's contribution to a measured diff, in raw
// diff bytes (headers and hunk markers included, consistent with how the
// whole-diff measurement itself counts — see DiffSizeDetail).
type PathSize struct {
	Path  string
	Bytes int64
}

// dominantPathsLimit caps DiffSizeDetail.DominantPaths so a too-large notice
// stays actionable even when many small files, rather than one huge one,
// push a diff over the cap.
const dominantPathsLimit = 5

// DiffSizeDetail is the structured record of why a diff was measured as
// over cap, attached to ReviewOutcome.SizeDetail and rendered into the
// too-large notice (FR-3: "measured size, cap, dominant contributing
// paths"). OmittedPaths is the config-excluded paths only — every path
// removed via excluded_paths before MeasuredBytes was computed. TrimAttempted
// is separate: it's the path(s) FR-5's trimToFit decided to drop when it was
// actually attempted and still didn't bring the diff under cap, so the
// notice can say Pruefer already tried auto-dropping the largest file(s)
// rather than silently omitting that it was tried at all.
type DiffSizeDetail struct {
	MeasuredBytes int64
	MaxBytes      int64
	DominantPaths []PathSize
	OmittedPaths  []string
	TrimAttempted []string
}

// buildDiffSizeDetail computes a DiffSizeDetail from the files a size
// decision was actually measured against (reviewFiles), the paths removed
// via excluded_paths (omitted), and — only when FR-5's trim was actually
// attempted and still insufficient — the paths trimToFit decided to drop
// (trimAttempted). DominantPaths is reviewFiles sorted by Bytes descending,
// capped to dominantPathsLimit — a pure, deterministic computation from
// already-parsed diff structure, never from Claude's prose (mirrors
// decideEvent's same discipline; see adrs/1251-pruefer-severity-gated-request-changes.md).
func buildDiffSizeDetail(measuredBytes, maxBytes int64, reviewFiles []diffFile, omitted []string, trimAttempted []string) DiffSizeDetail {
	sorted := make([]diffFile, len(reviewFiles))
	copy(sorted, reviewFiles)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Bytes > sorted[j].Bytes })

	limit := dominantPathsLimit
	if limit > len(sorted) {
		limit = len(sorted)
	}
	dominant := make([]PathSize, limit)
	for i := 0; i < limit; i++ {
		dominant[i] = PathSize{Path: sorted[i].Path, Bytes: sorted[i].Bytes}
	}

	return DiffSizeDetail{
		MeasuredBytes: measuredBytes,
		MaxBytes:      maxBytes,
		DominantPaths: dominant,
		OmittedPaths:  omitted,
		TrimAttempted: trimAttempted,
	}
}
