package pruefer

import (
	"context"
	"errors"
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
	// FetchPRFiles returns the changed-path list via the paginated
	// /pulls/{n}/files endpoint, which has no 20,000-line ceiling — the
	// fallback source of changed paths when FetchPRDiff returns
	// gh.ErrDiffTooLarge (see R3, adrs/1427-pruefer-diff-too-large-degrade-not-block.md).
	FetchPRFiles(owner, repo string, prNumber int) ([]string, error)
	FetchPRReviews(owner, repo string, prNumber int) ([]gh.PRReview, error)
	// FetchPRReviewThreads returns existing review threads (resolved and
	// unresolved) on the PR, for buildReviewPrompt's prior-thread context
	// (R1), plus whether the PR has more threads than the fetch's own page
	// size returned (the fetch-layer cap, distinct from buildReviewPrompt's
	// smaller prompt-level cap). A fetch error here is non-fatal to the
	// review — see ReviewPR.
	FetchPRReviewThreads(owner, repo string, prNumber int) (threads []gh.PRReviewThread, threadsTruncated bool, err error)
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
//
// When FetchPRDiff returns gh.ErrDiffTooLarge — GitHub's deterministic 406
// refusal to render a diff exceeding its 20,000-line ceiling — ReviewPR
// degrades rather than blocks (R3): it falls back to FetchPRFiles (the
// paginated /pulls/{n}/files endpoint, which has no such ceiling) to
// reconstruct the changed-path list, and the review proceeds against the
// local clone as normal, with diff treated as empty (so inline-comment
// anchoring naturally demotes every finding into the review body — see
// adrs/1427-pruefer-diff-too-large-degrade-not-block.md). Only when the
// fallback also fails does this return the terminal SkipDiffTooLarge
// disposition (R2), after posting a single idempotent PR notice (R4) so a
// human can see why the PR was never reviewed rather than a hot retry every
// poll with nothing to show for it.
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
	var changedPaths []string
	if err != nil {
		if !errors.Is(err, gh.ErrDiffTooLarge) {
			return ReviewOutcome{Err: fmt.Errorf("fetching diff: %w", err)}
		}
		logf(pr.Number, "select", "%s/%s#%d: diff exceeds GitHub's 406 too_large ceiling, attempting files-API fallback\n", owner, repo, pr.Number)
		files, filesErr := client.FetchPRFiles(owner, repo, pr.Number)
		if filesErr != nil {
			logf(pr.Number, "select", "skipping %s/%s#%d: %s (files-API fallback also failed: %v)\n", owner, repo, pr.Number, SkipDiffTooLarge, filesErr)
			if noticeErr := postDiffUnavailableNoticeOnce(client, owner, repo, pr.Number, pr.HeadSHA); noticeErr != nil {
				logf(pr.Number, "warn", "posting diff-unavailable notice on %s/%s#%d: %v\n", owner, repo, pr.Number, noticeErr)
			}
			return ReviewOutcome{Skipped: true, Reason: SkipDiffTooLarge}
		}
		// No diff text was ever obtained, so there is nothing for
		// max_diff_bytes to measure — GitHub's 406 already establishes the
		// diff exceeds 20,000 lines, which would exceed any reasonable
		// max_diff_bytes anyway. diff stays "" for the rest of this call;
		// validRightAnchors("") returns no anchors, so every finding
		// demotes cleanly into the review body instead of risking an
		// invalid inline-comment anchor.
		changedPaths = files
	} else {
		if cfg.MaxDiffBytes > 0 && int64(len(diff)) > cfg.MaxDiffBytes {
			logf(pr.Number, "select", "skipping %s/%s#%d: diff is %d bytes, exceeds max_diff_bytes=%d\n", owner, repo, pr.Number, len(diff), cfg.MaxDiffBytes)
			return ReviewOutcome{Skipped: true, Reason: SkipDiffTooLarge}
		}
		changedPaths = ParseChangedPaths(diff)
	}
	if len(cfg.ExcludedPaths) > 0 && allPathsExcluded(changedPaths, cfg.ExcludedPaths) {
		logf(pr.Number, "select", "skipping %s/%s#%d: %s\n", owner, repo, pr.Number, SkipExcludedPath)
		return ReviewOutcome{Skipped: true, Reason: SkipExcludedPath}
	}

	if forceReview {
		if err := AcknowledgeForceReview(client, owner, repo, pr.Number); err != nil {
			logf(pr.Number, "warn", "acknowledging /pruefer review comment on %s/%s#%d: %v\n", owner, repo, pr.Number, err)
		}
	}

	// Thread context is purely advisory prompt content (R1) — a transient
	// GraphQL error here must not block a review that would otherwise
	// succeed, so this degrades to a cold-read (nil threads) rather than
	// failing the outcome, unlike FetchPRReviews above whose result gates
	// eligibility.
	threads, threadsTruncated, err := client.FetchPRReviewThreads(owner, repo, pr.Number)
	if err != nil {
		logf(pr.Number, "warn", "fetching review threads on %s/%s#%d: %v — proceeding without prior-thread context\n", owner, repo, pr.Number, err)
		threads = nil
		threadsTruncated = false
	}

	dir, cleanup, err := clone(ctx, owner, repo, client.Token(), pr.Number)
	if err != nil {
		return ReviewOutcome{Err: fmt.Errorf("cloning PR head: %w", err)}
	}
	defer cleanup()

	result, err := claude.Review(ctx, ReviewRequest{
		Owner: owner, Repo: repo, PRNumber: pr.Number, Title: pr.Title, Body: pr.Body,
		HeadSHA: pr.HeadSHA, BaseBranch: pr.BaseRef, Model: cfg.Model, Effort: cfg.Effort,
		WorkDir: dir, MaxWallTime: cfg.MaxWallTime, ReviewThreads: threads, ReviewThreadsTruncated: threadsTruncated,
	})
	if err != nil {
		logf(pr.Number, "claude", "review invocation failed for %s/%s#%d: %v — posting nothing\n", owner, repo, pr.Number, err)
		return ReviewOutcome{Err: fmt.Errorf("claude review invocation: %w", err)}
	}

	summary, findings, parseInfo := parseReviewFindings(result.Text)
	logSummaryParseInfo(pr.Number, parseInfo)
	findings = dedupeFindings(findings)
	event := decideEvent(findings, cfg.RequestChangesThreshold)
	comments, demoted := partitionFindings(findings, validRightAnchors(diff))
	body := buildReviewBody(summary, demoted)

	if _, err := client.SubmitPRReview(owner, repo, pr.Number, pr.HeadSHA, body, event, comments); err != nil {
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

// logSummaryParseInfo implements R4: logs when parseReviewFindings' summary
// extraction fell back to positional parsing (markers missing or malformed),
// or when a well-formed marker pair was found but non-empty preamble text
// was discarded ahead of PRUEFER_SUMMARY_BEGIN — both are compliance-drift
// signals distinct from the happy path (markers found, zero-byte preamble),
// which logs nothing (AC5). parseReviewFindings itself stays pure; this is
// the one call site with pr.Number in scope to log against.
func logSummaryParseInfo(prNumber int, info SummaryParseInfo) {
	if !info.MarkersFound {
		logf(prNumber, "warn", "review summary had no PRUEFER_SUMMARY_BEGIN/END markers (or a malformed pair) — used positional fallback\n")
		return
	}
	if info.DiscardedBytes > 0 {
		logf(prNumber, "warn", "discarded %d byte(s) of preamble before PRUEFER_SUMMARY_BEGIN\n", info.DiscardedBytes)
	}
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
