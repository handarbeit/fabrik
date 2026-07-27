package pruefer

import (
	"context"
	"fmt"

	gh "github.com/handarbeit/fabrik/github"
)

// GitHubReviewer is the subset of *github.Client's methods review.go needs:
// diff fetch (for the size guard and path exclusion), existing-review
// lookup (for GitHub-derived review state), review submission, and — via
// GitHubCommenter — the /pruefer review comment lifecycle. Token exposes
// the current installation token for CloneForReview's HTTPS auth. A narrow
// interface keeps tests independent of HTTP mocking.
type GitHubReviewer interface {
	GitHubCommenter
	FetchPRDiff(owner, repo string, prNumber int) (string, error)
	FetchPRReviews(owner, repo string, prNumber int) ([]gh.PRReview, error)
	SubmitPRReview(owner, repo string, prNumber int, commitSHA, body string, comments []gh.ReviewComment) (int, error)
	Token() string
}

// CloneFunc creates an ephemeral clone of a PR's head commit and returns its
// directory plus a cleanup function. Matches CloneForReview's signature;
// injectable so tests can substitute a local git repo instead of hitting
// github.com.
type CloneFunc func(ctx context.Context, owner, repo, token string, prNumber int) (dir string, cleanup func(), err error)

// ReviewOutcome records what happened when ReviewPR was asked to review a
// PR, so callers (daemon.go, tests) can observe results without parsing logs.
type ReviewOutcome struct {
	Reviewed bool       // true iff a formal review was submitted
	Skipped  bool       // true iff the PR was ineligible
	Reason   SkipReason // set iff Skipped
	Err      error      // non-nil on a genuine failure (clone, claude invocation, API call)
	NumTurns int        // set iff Reviewed; turns used by the claude invocation
	CostUSD  float64    // set iff Reviewed; cost of the claude invocation
}

// ReviewPR runs the full per-PR pipeline: on-demand-comment detection,
// eligibility check, diff-size guard, ephemeral clone, Claude invocation,
// and — on success — formal review submission pinned to the PR's current
// head SHA.
//
// On any failure it posts nothing (per the issue's explicit "on invocation
// failure, post nothing rather than a stub" requirement) and returns a
// non-nil Err; the PR is naturally retried on the next poll since review
// state is derived from GitHub itself, not persisted locally.
//
// Cheap checks (draft, self-authored, excluded author/label, already-
// reviewed-at-SHA) run before any diff fetch; only a PR that passes those
// triggers the FetchPRDiff call used for the size guard and path exclusion,
// so a skip never costs an extra network round-trip.
func ReviewPR(ctx context.Context, client GitHubReviewer, claude ClaudeInvoker, clone CloneFunc, cfg Config, botLogin, owner, repo string, pr gh.PRDetails) ReviewOutcome {
	forceReview, err := PendingForceReview(client, owner, repo, pr.Number)
	if err != nil {
		logf(pr.Number, "warn", "checking for /pruefer review command on %s/%s#%d: %v\n", owner, repo, pr.Number, err)
		forceReview = false // not fatal to the poll cycle — treat as no forced review this round
	}

	reviews, err := client.FetchPRReviews(owner, repo, pr.Number)
	if err != nil {
		return ReviewOutcome{Err: fmt.Errorf("fetching existing reviews: %w", err)}
	}

	cheapCheck := EligibilityInput{
		PR:              pr,
		BotLogin:        botLogin,
		ExcludedAuthors: cfg.ExcludedAuthors,
		ExcludedLabels:  cfg.ExcludedLabels,
		ExistingReviews: reviews,
		ForceReview:     forceReview,
	}
	if ok, reason := Eligible(cheapCheck); !ok {
		logf(pr.Number, "select", "skipping %s/%s#%d: %s\n", owner, repo, pr.Number, reason)
		return ReviewOutcome{Skipped: true, Reason: reason}
	}

	diff, err := client.FetchPRDiff(owner, repo, pr.Number)
	if err != nil {
		return ReviewOutcome{Err: fmt.Errorf("fetching diff: %w", err)}
	}
	if cfg.MaxDiffBytes > 0 && int64(len(diff)) > cfg.MaxDiffBytes {
		logf(pr.Number, "select", "skipping %s/%s#%d: diff is %d bytes, exceeds max_diff_bytes=%d\n", owner, repo, pr.Number, len(diff), cfg.MaxDiffBytes)
		return ReviewOutcome{Skipped: true, Reason: SkipDiffTooLarge}
	}
	if len(cfg.ExcludedPaths) > 0 && allPathsExcluded(ParseChangedPaths(diff), cfg.ExcludedPaths) {
		logf(pr.Number, "select", "skipping %s/%s#%d: %s\n", owner, repo, pr.Number, SkipExcludedPath)
		return ReviewOutcome{Skipped: true, Reason: SkipExcludedPath}
	}

	if forceReview {
		if err := AcknowledgeForceReview(client, owner, repo, pr.Number); err != nil {
			logf(pr.Number, "warn", "acknowledging /pruefer review comment on %s/%s#%d: %v\n", owner, repo, pr.Number, err)
		}
	}

	dir, cleanup, err := clone(ctx, owner, repo, client.Token(), pr.Number)
	if err != nil {
		return ReviewOutcome{Err: fmt.Errorf("cloning PR head: %w", err)}
	}
	defer cleanup()

	result, err := claude.Review(ctx, ReviewRequest{
		Owner: owner, Repo: repo, PRNumber: pr.Number, Title: pr.Title, Body: pr.Body,
		HeadSHA: pr.HeadSHA, BaseBranch: pr.BaseRef, Model: cfg.Model, Effort: cfg.Effort,
		WorkDir: dir, MaxWallTime: cfg.MaxWallTime,
	})
	if err != nil {
		logf(pr.Number, "claude", "review invocation failed for %s/%s#%d: %v — posting nothing\n", owner, repo, pr.Number, err)
		return ReviewOutcome{Err: fmt.Errorf("claude review invocation: %w", err)}
	}

	summary, findings := parseReviewFindings(result.Text)
	comments, demoted := partitionFindings(findings, validRightAnchors(diff))
	body := buildReviewBody(summary, demoted)

	if _, err := client.SubmitPRReview(owner, repo, pr.Number, pr.HeadSHA, body, comments); err != nil {
		return ReviewOutcome{Err: fmt.Errorf("submitting review: %w", err)}
	}

	if forceReview {
		if err := MarkForceReviewsProcessed(client, owner, repo, pr.Number); err != nil {
			logf(pr.Number, "warn", "marking /pruefer review comment processed on %s/%s#%d: %v\n", owner, repo, pr.Number, err)
		}
	}

	logf(pr.Number, "review", "submitted review for %s/%s#%d at %s\n", owner, repo, pr.Number, pr.HeadSHA)
	return ReviewOutcome{Reviewed: true, NumTurns: result.NumTurns, CostUSD: result.CostUSD}
}
