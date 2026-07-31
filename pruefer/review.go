package pruefer

import (
	"context"
	"fmt"
	"strings"

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
	SubmitPRReview(owner, repo string, prNumber int, commitSHA, body string, event gh.ReviewEvent, comments []gh.ReviewComment) (int, error)
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
	// SizeDetail is set iff Skipped and Reason == SkipDiffTooLarge from a
	// fresh measurement (not the pre-diff-fetch "already noticed" skip,
	// which returns before any measurement exists). Mirrors the too-large
	// notice's own content — see notice.go's buildTooLargeNoticeBody.
	SizeDetail *DiffSizeDetail
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
// so a skip never costs an extra network round-trip. The already-noticed-
// too-large-at-this-SHA check is also pre-diff-fetch, but — unlike the
// other cheap checks — an unprocessed "/pruefer review" comment bypasses it:
// an operator who fixes excluded_paths or max_diff_bytes after a notice
// fires and asks for a re-check gets a genuine fresh measurement, not a
// second silent-feeling skip of the same shape this issue exists to fix.
// Configured path exclusions are applied to the diff, per file, BEFORE it is
// measured against MaxDiffBytes (FR-1) — an excluded file can no longer
// consume the size budget or suppress review of everything else. If
// exclusion alone isn't enough, ReviewPR makes one best-effort attempt to
// drop the largest remaining file(s) and review the rest (FR-5); only if
// that still doesn't fit does it post a one-per-head-SHA notice comment and
// skip (FR-2/FR-3), rather than failing silently — a forced re-check that
// still doesn't fit does not duplicate that notice, but does mark the
// "/pruefer review" command processed so it isn't retried forever.
func ReviewPR(ctx context.Context, client GitHubReviewer, claude ClaudeInvoker, clone CloneFunc, cfg Config, botLogin, owner, repo string, pr gh.PRDetails) ReviewOutcome {
	comments, err := client.FetchIssueComments(owner, repo, pr.Number)
	if err != nil {
		logf(pr.Number, "warn", "fetching comments for %s/%s#%d: %v\n", owner, repo, pr.Number, err)
		comments = nil // fail open: no pending force-review, no known prior too-large notice this round
	}
	forceReview := len(pendingReviewComments(comments)) > 0

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

	if forceReview {
		// Acknowledge immediately, before any skip path below, so an
		// operator who comments "/pruefer review" always gets visible
		// confirmation their request was seen — regardless of what the
		// fresh check below finds. Idempotent (checks for the EYES
		// reaction itself), so it's safe to no-op on a re-poll.
		if err := AcknowledgeForceReview(client, owner, repo, pr.Number); err != nil {
			logf(pr.Number, "warn", "acknowledging /pruefer review comment on %s/%s#%d: %v\n", owner, repo, pr.Number, err)
		}
	}

	noticeAlreadyExists := alreadyNoticedTooLarge(comments, pr.HeadSHA)
	if noticeAlreadyExists && !forceReview {
		logf(pr.Number, "select", "skipping %s/%s#%d: already noticed as too-large at head %s — not re-fetching the diff\n", owner, repo, pr.Number, pr.HeadSHA)
		return ReviewOutcome{Skipped: true, Reason: SkipDiffTooLarge}
	}

	diff, err := client.FetchPRDiff(owner, repo, pr.Number)
	if err != nil {
		return ReviewOutcome{Err: fmt.Errorf("fetching diff: %w", err)}
	}

	files, preamble := splitDiffFiles(diff)
	preambleBytes := int64(len(preamble))

	kept, excluded := filterExcludedPaths(files, cfg.ExcludedPaths)
	if len(cfg.ExcludedPaths) > 0 && len(files) > 0 && len(kept) == 0 {
		logf(pr.Number, "select", "skipping %s/%s#%d: %s\n", owner, repo, pr.Number, SkipExcludedPath)
		return ReviewOutcome{Skipped: true, Reason: SkipExcludedPath}
	}

	reviewFiles := kept
	omittedPaths := pathsOf(excluded)

	if cfg.MaxDiffBytes > 0 {
		measured := preambleBytes
		for _, f := range kept {
			measured += f.Bytes
		}
		if measured > cfg.MaxDiffBytes {
			trimmedKept, trimmedDropped, fits := trimToFit(kept, preambleBytes, cfg.MaxDiffBytes)
			if fits {
				logf(pr.Number, "select", "trimmed oversized diff for %s/%s#%d: dropping %d file(s) to review the rest\n", owner, repo, pr.Number, len(trimmedDropped))
				reviewFiles = trimmedKept
				omittedPaths = append(omittedPaths, pathsOf(trimmedDropped)...)
			} else {
				detail := buildDiffSizeDetail(measured, cfg.MaxDiffBytes, kept, pathsOf(excluded))
				logf(pr.Number, "select", "skipping %s/%s#%d: diff is %d bytes after exclusions, exceeds max_diff_bytes=%d\n", owner, repo, pr.Number, measured, cfg.MaxDiffBytes)
				if noticeAlreadyExists {
					// forceReview bypassed the pre-check above to get this
					// fresh measurement, but the diff still doesn't fit —
					// the existing notice for this head SHA already says
					// so; don't post a second one for the same SHA.
					logf(pr.Number, "select", "not re-posting too-large notice for %s/%s#%d — one already exists at head %s\n", owner, repo, pr.Number, pr.HeadSHA)
				} else if _, addErr := client.AddComment(owner, repo, pr.Number, buildTooLargeNoticeBody(detail, pr.HeadSHA)); addErr != nil {
					logf(pr.Number, "warn", "posting diff-too-large notice on %s/%s#%d: %v\n", owner, repo, pr.Number, addErr)
				}
				if forceReview {
					// The forced re-check happened and the verdict didn't
					// change; mark the command processed so it isn't
					// retried forever — the notice (existing or just
					// posted) is the durable "still too large" signal now.
					if err := MarkForceReviewsProcessed(client, owner, repo, pr.Number); err != nil {
						logf(pr.Number, "warn", "marking /pruefer review comment processed on %s/%s#%d: %v\n", owner, repo, pr.Number, err)
					}
				}
				return ReviewOutcome{Skipped: true, Reason: SkipDiffTooLarge, SizeDetail: &detail}
			}
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
		WorkDir: dir, MaxWallTime: cfg.MaxWallTime, ExcludedPaths: omittedPaths,
	})
	if err != nil {
		logf(pr.Number, "claude", "review invocation failed for %s/%s#%d: %v — posting nothing\n", owner, repo, pr.Number, err)
		return ReviewOutcome{Err: fmt.Errorf("claude review invocation: %w", err)}
	}

	reviewedDiff := reconstructDiff(reviewFiles)
	summary, findings := parseReviewFindings(result.Text)
	event := decideEvent(findings, cfg.RequestChangesThreshold)
	reviewComments, demoted := partitionFindings(findings, validRightAnchors(reviewedDiff))
	body := buildReviewBody(summary, demoted, omittedPaths)

	if _, err := client.SubmitPRReview(owner, repo, pr.Number, pr.HeadSHA, body, event, reviewComments); err != nil {
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

// reconstructDiff concatenates files' Body blocks back into a single unified
// diff covering only the files actually reviewed — used for validRightAnchors
// so a demoted/anchored finding can never reference a file ReviewPR excluded
// or auto-dropped before Claude ever saw it. For an unfiltered PR this
// reconstructs byte-identical to the original FetchPRDiff output (see
// TestSplitDiffFiles_MultiFile), so existing anchor behavior is unchanged
// when no exclusion or trimming occurred.
func reconstructDiff(files []diffFile) string {
	var b strings.Builder
	for _, f := range files {
		b.WriteString(f.Body)
	}
	return b.String()
}

// decideEvent computes the review event to submit, deterministically, from
// findings' already-JSON-parsed Severity fields — never from Claude's prose.
// threshold is the operator-configured Config.RequestChangesThreshold; the
// zero value ("") means the severity-gated behavior is off, so this always
// returns ReviewEventComment regardless of findings. An unrecognized non-empty
// threshold is treated identically to off (fail closed) rather than degrading
// to severityRank's 0 for both — LoadConfig rejects an invalid threshold, but
// Config is a public struct ReviewPR takes directly, so decideEvent must not
// assume its input was pre-validated: if it trusted severityRank(threshold)==0
// to mean "match everything", a typo'd threshold that bypassed LoadConfig
// would turn every review with any finding — including ones with a missing
// severity — into REQUEST_CHANGES, the opposite of fail-safe. Otherwise, if
// any finding's severity ranks at or above threshold, ReviewEventRequestChanges
// is returned; an unrecognized or missing per-finding severity ranks 0 and
// never meets any real threshold (see severityRank's fail-closed-toward-COMMENT
// doc comment). Takes the full pre-partition findings slice, not just the
// diff-anchorable subset — a severity-worthy finding that can't be anchored
// to a line must still count toward the threshold.
func decideEvent(findings []ReviewFinding, threshold Severity) gh.ReviewEvent {
	if !validSeverity(threshold) {
		return gh.ReviewEventComment
	}
	thresholdRank := severityRank(threshold)
	for _, f := range findings {
		if severityRank(f.Severity) >= thresholdRank {
			return gh.ReviewEventRequestChanges
		}
	}
	return gh.ReviewEventComment
}
