package pruefer

import (
	"path/filepath"
	"regexp"
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
)

// EligibilityInput bundles everything Eligible needs to decide whether a PR
// should be reviewed.
type EligibilityInput struct {
	PR              gh.PRDetails
	BotLogin        string // Pruefer's own review identity, e.g. "pruefer-bot[bot]"
	ExcludedAuthors []string
	ExcludedLabels  []string
	ExcludedPaths   []string // glob patterns (filepath.Match syntax), matched against every changed path
	// ExistingReviews are the PR's current reviews (any author); Eligible
	// looks specifically for one authored by BotLogin at PR.HeadSHA.
	ExistingReviews []gh.PRReview
	// ChangedPaths is the list of file paths touched by the PR, typically
	// parsed from its diff via ParseChangedPaths. Empty/nil means "unknown"
	// — path exclusion never fires when paths aren't available, since
	// exclusion requires ALL paths to match.
	ChangedPaths []string
	// ForceReview is true when an unprocessed "/pruefer review" comment
	// requests a fresh review of the current head regardless of prior
	// review state.
	ForceReview bool
}

// Eligible reports whether a PR should be reviewed, and if not, why.
// ForceReview overrides only the already-reviewed-at-this-SHA check — draft,
// self-authored, and excluded-author/label/path checks always apply, since
// "/pruefer review" forces a *fresh* review, not a bypass of every safety
// check (see adrs/073-pruefer-v1-architecture.md).
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
	if len(in.ExcludedPaths) > 0 && allPathsExcluded(in.ChangedPaths, in.ExcludedPaths) {
		return false, SkipExcludedPath
	}
	if !in.ForceReview && alreadyReviewedAtHead(in.ExistingReviews, in.BotLogin, in.PR.HeadSHA) {
		return false, SkipAlreadyReviewed
	}
	return true, ""
}

// allPathsExcluded reports whether every path in changed matches at least
// one glob in patterns. Returns false (not excluded) when changed is empty
// — an unknown diff must never be silently skipped just because path
// exclusions are configured.
func allPathsExcluded(changed, patterns []string) bool {
	if len(changed) == 0 {
		return false
	}
	for _, path := range changed {
		if !matchesAny(path, patterns) {
			return false
		}
	}
	return true
}

func matchesAny(path string, patterns []string) bool {
	for _, pat := range patterns {
		if ok, err := filepath.Match(pat, path); err == nil && ok {
			return true
		}
	}
	return false
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
