package pruefer

import (
	"context"
	"errors"
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
	// FetchFileAtRef resolves the reviewed repo's repo-resident
	// .pruefer/config.yaml at the PR's base ref (never the head — see
	// fetchRepoConfig and ADR-1642). Required unconditionally, not
	// type-asserted, so the security property (a reviewed repo can never
	// widen its own review) is structural for every GitHubReviewer, not
	// opt-in.
	FetchFileAtRef(owner, repo, path, ref string) ([]byte, error)
	// ResolveRefSHA backs the base-SHA pin (#2307): the base branch resolved
	// once per review to a commit SHA that every base-ref read (config,
	// guidance) then uses. Required unconditionally, not type-asserted, for the
	// same structural reason as FetchFileAtRef.
	ResolveRefSHA(owner, repo, branch string) (string, error)
	// ResolveRefKind, FetchSubtreeSHA, FetchTreeBySHA and FetchBlob back repo
	// plugin loading (#2366): classify and peel a ref to a fixed commit, find a
	// plugin directory's tree SHA at a commit, list that tree, and read a blob.
	// They are called only from repoplugin_fetch.go, only behind the
	// repo_plugins gate (TestRepoPluginReadersAreOnlyReachableBehindTheGate).
	// Required unconditionally, not type-asserted, like FetchFileAtRef.
	ResolveRefKind(owner, repo, ref string) (gh.ResolvedRef, error)
	FetchSubtreeSHA(owner, repo, commitSHA, dir string) (string, error)
	FetchTreeBySHA(owner, repo, treeSHA string) ([]gh.TreeEntry, bool, error)
	FetchBlob(owner, repo, sha string, maxBytes int64) ([]byte, error)
	// FetchCommit and FetchCommitsBehind back the pure merge-train
	// catch-up skip (#2066): the head's parent shape, and whether a commit is
	// on the PR's base branch. Required unconditionally, not type-asserted,
	// for the same structural reason as FetchFileAtRef — a fake that silently
	// lacked them would silently disable a security-relevant check.
	FetchCommit(owner, repo, sha string) (gh.CommitInfo, error)
	FetchCommitsBehind(owner, repo, base, head string) (int, error)
	FetchPRDiff(owner, repo string, prNumber int) (string, error)
	// FetchCompareDiff and FindPRByHead back the patch-identity skips (#2230):
	// the diff a previously reviewed head carried relative to its merge base
	// with the PR's base, and the PR a merge-train member branch belongs to.
	// Required unconditionally, not type-asserted, for the same structural
	// reason as FetchCommit — a fake silently lacking them would silently
	// disable a check.
	FetchCompareDiff(owner, repo, base, head string) (string, error)
	FindPRByHead(owner, repo, headRef string) ([]gh.PRDetails, error)
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
// headSHA is the commit the review was selected on, which the clone must end
// up at exactly. Injectable so tests can substitute a local git repo instead
// of hitting github.com.
type CloneFunc func(ctx context.Context, owner, repo, token string, prNumber int, headSHA string) (dir string, cleanup func(), err error)

// ReviewOutcome records what happened when ReviewPR was asked to review a
// PR, so callers (daemon.go, tests) can observe results without parsing logs.
type ReviewOutcome struct {
	Reviewed bool       // true iff a formal review was submitted
	Skipped  bool       // true iff the PR was ineligible
	Reason   SkipReason // set iff Skipped
	Err      error      // non-nil on a genuine failure (clone, claude invocation, API call)
	NumTurns int        // set iff Reviewed; turns used by the claude invocation
	CostUSD  float64    // set iff Reviewed; cost of the claude invocation
	// FallbackFrom is the review_skill reference that was selected but replaced
	// by the default skill (#2307); empty when the selected skill ran.
	FallbackFrom string

	// Conclusive is true iff this outcome is a settled verdict about the PR at
	// its current head and inputs: it was reviewed, or skipped for a reason,
	// with no error and without any degraded read having shaped the decision
	// (#1952 R3). Only a conclusive outcome may be memoised by the daemon. It
	// is opt-out by construction — the zero value is "not conclusive", and
	// ReviewPR computes it in one deferred place — because the failure modes
	// that must not be memoised include ones ReviewPR deliberately swallows
	// (a failed comment read treated as "no /pruefer review", a failed repo
	// config fetch degraded to operator config, a failed files-API fallback,
	// a failed notice post or force-review acknowledgement): none of them
	// surface in Err, yet every one of them can make an outcome wrong.
	Conclusive bool
}

// ReviewPR runs the full per-PR pipeline: repo-resident config resolution
// (#1642, at the PR's base ref — see the first block of the function body),
// on-demand-comment detection, eligibility check, path exclusion,
// pure-catch-up skip (#2066, catchup.go), diff-size guard, ephemeral clone, Claude invocation, and — on success —
// formal review submission pinned to
// the PR's current head SHA.
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
// #1610: cfg.Cadence/RepoCadence (resolved here via effectiveCadence) gate
// automatic reviews. CadenceOnRequest short-circuits immediately after
// forceReview is known, ahead of even the local tracker check, since it
// needs nothing else. CadenceOnce is enforced inside Eligible(), which
// already has ExistingReviews in scope. Both are bypassed by forceReview,
// exactly like the existing alreadyReviewedAtHead/tracker checks — a human-
// requested "/pruefer review" always still reviews the PR (R4). See
// adrs/1610-pruefer-review-cadence.md.
//
// tracker (#1631) is a second, independent, GitHub-independent source of
// truth for "have I already reviewed this exact head" — consulted
// immediately after PendingForceReview and before FetchPRReviews is even
// called. It exists because FetchPRReviews' own read can come back
// successful but partial during a provider degradation (the confirmed
// #1631 root cause: an outage caused GitHub to return HTTP 200 with a
// review list that omitted the bot's own prior submission, so the
// GitHub-derived alreadyReviewedAtHead check correctly, but wrongly,
// reported "no prior review" on every poll for the outage's duration). A
// nil tracker disables this backstop entirely — see ReviewTracker's doc
// comment and adrs/1631-pruefer-local-review-tracker-backstop.md.
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
// disposition, after posting a single idempotent PR notice so a human can
// see why the PR was never reviewed rather than a hot retry every poll with
// nothing to show for it. This fallback branch has no diff text at all, so
// none of the per-file filtering/trimming below ever applies to it — its
// own all-or-nothing path-exclusion check is unchanged from before #1462.
//
// When a diff *is* obtained, path exclusion now runs before the size gate
// (R1): excluded_paths is applied per file to the parsed diff blocks
// (splitDiffFiles/filterExcludedPaths) before max_diff_bytes is ever
// compared, so an excluded file can never contribute to a size verdict.
// "every changed path is excluded" is still checked first as its own
// terminal disposition (R3, allPathsExcluded, unchanged). If the
// post-exclusion diff still exceeds max_diff_bytes, trimToFit drops
// additional files (largest first) until it fits (R4) rather than skipping
// outright; only when nothing usable survives (R4's "review of a partial
// diff that presents as a review of the whole diff is worse than a skip")
// does this fall through to SkipDiffTooLarge. Either way, whatever was
// dropped — by exclusion or by trimming — is disclosed to the reviewing
// model (ReviewRequest.OmittedExcludedPaths/OmittedTrimmedPaths, see
// claude.go's renderOmittedPaths) and, when the raw diff was actually
// oversized, named in an idempotent PR notice (postDiffTooLargeAfterFetchNoticeOnce,
// R5). diff is rebound to the filtered/trimmed text before validRightAnchors
// is called below, so R6 (a finding can never anchor to an omitted file)
// holds with no separate anchor-scrubbing logic.
func ReviewPR(ctx context.Context, client GitHubReviewer, claude ClaudeInvoker, clone CloneFunc, cfg Config, botLogin, owner, repo string, pr gh.PRDetails, tracker *ReviewTracker) (outcome ReviewOutcome) {
	// degraded is set wherever a read or write whose failure ReviewPR
	// deliberately tolerates could have shaped the outcome. See
	// ReviewOutcome.Conclusive. Every return path passes through this defer,
	// so a new return site that forgets to think about it is safe by
	// construction: it can only be conclusive if it is a clean review/skip.
	degraded := false
	defer func() {
		outcome.Conclusive = !degraded && outcome.Err == nil && (outcome.Reviewed || outcome.Skipped)
	}()

	// R1/R2 (#1642): resolve owner/repo's repo-resident .pruefer/config.yaml
	// at the PR's base ref — never the head, so a PR can never change how it
	// is itself reviewed (mirrors --setting-sources' "the PR head is
	// untrusted" doctrine, adrs/1113-pruefer-v1-architecture.md). This is
	// deliberately the very first network call in ReviewPR, ahead of even
	// PendingForceReview/FetchPRReviews below: EligibilityInput (built just
	// below from cfg.ExcludedAuthors/cfg.ExcludedLabels) must see any
	// repo-narrowed exclusion before Eligible() runs — otherwise a
	// repo-narrowed author/label exclusion would never take effect, since
	// the PR would already have been dispatched to Claude on the
	// operator-only exclusion set by the time repo config was consulted.
	// applyRepoNarrowing never widens operator's configuration (see its doc
	// comment and adrs/1642-*.md); every field below this point reads the
	// resulting effective cfg exactly where it read the operator's cfg
	// before this issue.
	//
	// #2307: the base branch is resolved to one commit SHA here, and every
	// base-ref read of this review — config, guidance — uses it, so they can
	// never come from different base commits and the SHA a fallback notice names
	// is the one actually read. If the branch cannot be resolved the reads fall
	// back to the branch name (the pre-#2307 behaviour) and the outcome is not
	// conclusive.
	baseRead := pr.BaseRef
	baseSHA, shaErr := client.ResolveRefSHA(owner, repo, pr.BaseRef)
	if shaErr != nil {
		logf(pr.Number, "warn", "resolving base branch %s of %s/%s#%d to a commit: %v\n", pr.BaseRef, owner, repo, pr.Number, shaErr)
		baseSHA = ""
		degraded = true
	} else {
		baseRead = baseSHA
	}
	repoYAML, prov := fetchRepoConfig(client, owner, repo, baseRead)
	cfg, warnings := applyRepoNarrowing(cfg, repoYAML)
	logRepoConfigResolution(pr.Number, owner, repo, prov, warnings)
	if prov.FetchFailed {
		degraded = true // the repo's real config is unknown, not "invalid"
	}

	forceReview, err := PendingForceReview(client, owner, repo, pr.Number)
	if err != nil {
		logf(pr.Number, "warn", "checking for /pruefer review command on %s/%s#%d: %v\n", owner, repo, pr.Number, err)
		forceReview = false // not fatal to the poll cycle — treat as no forced review this round
		degraded = true     // ...but a decision made on that basis is not conclusive (#1952)
	}

	// #1610 R1/R4: cadence == on-request means no automatic review is ever
	// triggered — only a pending "/pruefer review" command does. Gated here,
	// immediately after forceReview is resolved and before the local tracker
	// check or FetchPRReviews, since this decision needs nothing but the
	// force-review boolean and costs zero further GitHub calls when it
	// fires. CadenceOnce is handled later, inside Eligible(), where the
	// already-fetched ExistingReviews it needs are in scope.
	cadence := effectiveCadence(cfg, owner, repo)
	if !forceReview && cadence == CadenceOnRequest {
		logf(pr.Number, "select", "skipping %s/%s#%d: %s\n", owner, repo, pr.Number, SkipCadenceOnRequest)
		return ReviewOutcome{Skipped: true, Reason: SkipCadenceOnRequest}
	}

	// #1631 R2/R3: the local tracker backstop, checked before FetchPRReviews
	// is ever called — a confirmed-duplicate head costs zero further GitHub
	// API calls, and (unlike the GitHub-derived check below) this decision
	// can never be fooled by a successful-but-partial FetchPRReviews
	// response, since it depends on nothing GitHub reports back. Bypassed by
	// forceReview, exactly like alreadyReviewedAtHead's own bypass — a
	// human-requested "/pruefer review" must still get a fresh review.
	if !forceReview && tracker.Recall(owner, repo, pr.Number, pr.HeadSHA) {
		logf(pr.Number, "select", "skipping %s/%s#%d: %s (local tracker: already reviewed this head this process, independent of FetchPRReviews)\n", owner, repo, pr.Number, SkipAlreadyReviewed)
		return ReviewOutcome{Skipped: true, Reason: SkipAlreadyReviewed}
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
		Cadence:         cadence,
	}
	if ok, reason := Eligible(cheapCheck); !ok {
		logf(pr.Number, "select", "skipping %s/%s#%d: %s\n", owner, repo, pr.Number, reason)
		return ReviewOutcome{Skipped: true, Reason: reason}
	}

	// #2066: a pure merge-train catch-up the engine has vouched for, over a
	// head Pruefer already reviewed, is skipped before any costly work and
	// without posting anything. Bypassed by forceReview — a human asked.
	// Lives here, in the shared path, so the event dispatch, the poll and
	// the reconciliation sweep all reach the same decision (R5).
	if !forceReview {
		v := catchUpSkip(ctx, client, cfg, botLogin, owner, repo, pr, reviews)
		if v.Degraded {
			degraded = true // a failed read is not a verdict (R4)
		}
		if v.Skip {
			logf(pr.Number, "select", "skipping %s/%s#%d: %s (%s)\n", owner, repo, pr.Number, SkipCatchUp, v.Detail)
			return ReviewOutcome{Skipped: true, Reason: SkipCatchUp}
		}
		if v.Detail != "" {
			logf(pr.Number, "select", "catch-up check on %s/%s#%d: reviewing (%s)\n", owner, repo, pr.Number, v.Detail)
		}
	}

	diff, err := client.FetchPRDiff(owner, repo, pr.Number)
	var changedPaths []string
	var omittedExcludedPaths, omittedTrimmedPaths, omittedReviewedPaths []string
	var stageBlocks []diffFileBlock // the blocks the model is told about, staged below
	var stagePreamble string
	if err != nil {
		if !errors.Is(err, gh.ErrDiffTooLarge) {
			return ReviewOutcome{Err: fmt.Errorf("fetching diff: %w", err)}
		}
		logf(pr.Number, "select", "%s/%s#%d: diff exceeds GitHub's 406 too_large ceiling, attempting files-API fallback\n", owner, repo, pr.Number)
		files, filesErr := client.FetchPRFiles(owner, repo, pr.Number)
		if filesErr != nil {
			logf(pr.Number, "select", "skipping %s/%s#%d: %s (files-API fallback also failed: %v)\n", owner, repo, pr.Number, SkipDiffTooLarge, filesErr)
			degraded = true // the 406 is deterministic; the fallback failure is not
			if noticeErr := postDiffUnavailableNoticeOnce(client, owner, repo, pr.Number, pr.HeadSHA); noticeErr != nil {
				logf(pr.Number, "warn", "posting diff-unavailable notice on %s/%s#%d: %v\n", owner, repo, pr.Number, noticeErr)
				degraded = true
			}
			return ReviewOutcome{Skipped: true, Reason: SkipDiffTooLarge}
		}
		// No diff text was ever obtained, so there is nothing for
		// max_diff_bytes to measure — GitHub's 406 already establishes the
		// diff exceeds 20,000 lines, which would exceed any reasonable
		// max_diff_bytes anyway. diff stays "" for the rest of this call;
		// validRightAnchors("") returns no anchors, so every finding
		// demotes cleanly into the review body instead of risking an
		// invalid inline-comment anchor. This fallback path list has no
		// per-file split to filter — excluded_paths' existing all-or-nothing
		// check is the only exclusion applied here (see doc comment above).
		changedPaths = files
		if len(cfg.ExcludedPaths) > 0 && allPathsExcluded(changedPaths, cfg.ExcludedPaths) {
			logf(pr.Number, "select", "skipping %s/%s#%d: %s\n", owner, repo, pr.Number, SkipExcludedPath)
			return ReviewOutcome{Skipped: true, Reason: SkipExcludedPath}
		}
	} else {
		// Both the terminal all-excluded check below and the per-file
		// filter (R2) must resolve each block's path identically — splitting
		// once here and deriving changedPaths from the same blocks (rather
		// than a second, independent ParseChangedPaths(diff) call) is what
		// guarantees that. ParseChangedPaths and resolveFilePath used to
		// disagree on an ordinary file whose path contains the literal
		// substring " b/": ParseChangedPaths always takes the "diff --git
		// a/X b/Y" header's greedy, ambiguous b/-side capture, while
		// resolveFilePath prefers the unambiguous "+++ b/<path>" content
		// line and only falls back to that same ambiguous header capture
		// when no such line exists. Two independent resolvers reaching
		// different verdicts for the same file would let the terminal
		// all-excluded gate and the per-file filter disagree about whether
		// that file was excluded at all — resolving once, here, closes that
		// divergence structurally rather than requiring the two call sites
		// to be kept in sync by hand.
		blocks, preamble := splitDiffFiles(diff)

		// #2230: patch-identity skips, on the raw diff (before exclusion or
		// trimming, so a change confined to an excluded path still counts as a
		// change). Bypassed by forceReview — a human asked. Every uncertain
		// input falls through to a normal review.
		if !forceReview && cfg.PatchSkip {
			pv := patchUnchangedSkip(client, cfg, botLogin, owner, repo, pr, reviews, diff, tracker)
			if pv.Degraded {
				degraded = true
			}
			if pv.Skip {
				logf(pr.Number, "select", "skipping %s/%s#%d: %s (%s)\n", owner, repo, pr.Number, SkipPatchUnchanged, pv.Detail)
				return ReviewOutcome{Skipped: true, Reason: SkipPatchUnchanged}
			}
			if pv.Detail != "" {
				logf(pr.Number, "select", "patch-identity check on %s/%s#%d: reviewing (%s)\n", owner, repo, pr.Number, pv.Detail)
			}
			tv := trialCheck(client, cfg, botLogin, owner, repo, pr, diff)
			if tv.Degraded {
				degraded = true
			}
			if tv.Skip {
				logf(pr.Number, "select", "skipping %s/%s#%d: %s (%s)\n", owner, repo, pr.Number, SkipTrialEquivalent, tv.Detail)
				return ReviewOutcome{Skipped: true, Reason: SkipTrialEquivalent}
			}
			if tv.Detail != "" {
				logf(pr.Number, "select", "trial check on %s/%s#%d: reviewing (%s)\n", owner, repo, pr.Number, tv.Detail)
			}
			if len(tv.Covered) > 0 {
				blocks, omittedReviewedPaths = withholdCovered(blocks, tv.Covered)
				diff = joinDiff(preamble, blocks)
			}
		}

		changedPaths = pathsOf(blocks)

		// R1/R3: path exclusion is checked before the size gate. The
		// terminal "every changed path is excluded" disposition is
		// unchanged — just reordered ahead of max_diff_bytes so an excluded
		// file can never contribute to a size verdict.
		if len(cfg.ExcludedPaths) > 0 && allPathsExcluded(changedPaths, cfg.ExcludedPaths) {
			logf(pr.Number, "select", "skipping %s/%s#%d: %s\n", owner, repo, pr.Number, SkipExcludedPath)
			return ReviewOutcome{Skipped: true, Reason: SkipExcludedPath}
		}

		rawBytes := int64(len(diff))
		rawOversized := cfg.MaxDiffBytes > 0 && rawBytes > cfg.MaxDiffBytes

		// R2: per-file exclusion, applied to the parsed diff blocks —
		// widens the all-or-nothing terminal check above into a partial
		// filter: a diff with some (but not all) excluded files proceeds on
		// the survivors instead of being skipped whole.
		kept, excludedBlocks := filterExcludedPaths(blocks, cfg.ExcludedPaths)
		measuredBytes := blocksLen(preamble, kept)

		var trimmedBlocks []diffFileBlock
		if cfg.MaxDiffBytes > 0 && measuredBytes > cfg.MaxDiffBytes {
			preTrimBytes := measuredBytes
			var fits bool
			kept, trimmedBlocks, fits = trimToFit(kept, int64(len(preamble)), cfg.MaxDiffBytes)
			if !fits {
				// R4: nothing usable survived exclusion+trimming — a
				// genuine skip, not a partial review that presents as
				// complete.
				logf(pr.Number, "select", "skipping %s/%s#%d: diff is %d bytes after excluding %d of %d files (%s), exceeds max_diff_bytes=%d\n",
					owner, repo, pr.Number, preTrimBytes, len(excludedBlocks), len(blocks), excludedPathsNote(cfg.ExcludedPaths), cfg.MaxDiffBytes)
				if noticeErr := postDiffTooLargeAfterFetchNoticeOnce(client, owner, repo, pr.Number, pr.HeadSHA, rawBytes, cfg.MaxDiffBytes, pathsOf(excludedBlocks), pathsOf(trimmedBlocks), false); noticeErr != nil {
					logf(pr.Number, "warn", "posting diff-too-large notice on %s/%s#%d: %v\n", owner, repo, pr.Number, noticeErr)
					degraded = true
				}
				return ReviewOutcome{Skipped: true, Reason: SkipDiffTooLarge}
			}
			logf(pr.Number, "select", "trimming %s/%s#%d: diff is %d bytes after excluding %d of %d files (%s), exceeds max_diff_bytes=%d — dropped %d additional file(s) to fit\n",
				owner, repo, pr.Number, preTrimBytes, len(excludedBlocks), len(blocks), excludedPathsNote(cfg.ExcludedPaths), cfg.MaxDiffBytes, len(trimmedBlocks))
		}

		// R6: rebind diff to exactly the subset the model is told about
		// below, so validRightAnchors (called further down) can never
		// validate an anchor on an omitted file.
		diff = joinDiff(preamble, kept)
		stageBlocks, stagePreamble = kept, preamble
		omittedExcludedPaths = pathsOf(excludedBlocks)
		omittedTrimmedPaths = pathsOf(trimmedBlocks)

		// R5: only announce a notice when the raw diff was actually
		// oversized — a routine vendor/** exclusion on an otherwise-small
		// PR must not spam a notice on every PR.
		if rawOversized && (len(omittedExcludedPaths) > 0 || len(omittedTrimmedPaths) > 0) {
			if noticeErr := postDiffTooLargeAfterFetchNoticeOnce(client, owner, repo, pr.Number, pr.HeadSHA, rawBytes, cfg.MaxDiffBytes, omittedExcludedPaths, omittedTrimmedPaths, true); noticeErr != nil {
				logf(pr.Number, "warn", "posting diff-too-large notice on %s/%s#%d: %v\n", owner, repo, pr.Number, noticeErr)
				degraded = true
			}
		}
	}

	if forceReview {
		if err := AcknowledgeForceReview(client, owner, repo, pr.Number); err != nil {
			logf(pr.Number, "warn", "acknowledging /pruefer review comment on %s/%s#%d: %v\n", owner, repo, pr.Number, err)
			degraded = true
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

	// #1446: resolve the repo's review-guidance skill, at the PR's base ref
	// (never the head — C1), advisory prompt content exactly like the
	// thread fetch above — a fetch failure or malformed file degrades to no
	// repo guidance rather than failing the review (R4).
	repoGuidance, repoGuidanceMode, guidanceProv := fetchRepoGuidance(client, owner, repo, baseRead)
	logReviewGuidanceResolution(pr.Number, owner, repo, guidanceProv)

	// #2307: the skill that performs the review, and the plugins it may come
	// from: the embedded pruefer-review plugin, the operator's own plugins used
	// in place and — only when the operator turned repo_plugins on — the plugins
	// the reviewed repo's base-ref config names (#2366).
	reviewSkill, skillNote := selectReviewSkill(cfg, repoYAML)
	if skillNote != "" {
		logf(pr.Number, "config", "%s/%s: %s\n", owner, repo, skillNote)
	}
	operatorPluginDirs := resolveOperatorPluginDirs(cfg.Plugins, pr.Number)

	// #2308: the coded review tools this review may use, and whether operator
	// plugins keep their hooks and MCP servers. Decided once here, from the
	// operator ceiling, the repo's base-ref selection and the PR author, and
	// carried in the request so the fallback re-run shares it.
	grant := resolveReviewGrant(cfg.ReviewTools, repoYAML.ReviewTools, pr.Author, pr.AuthorAssociation)
	logf(pr.Number, "config", "%s\n", grant.grantLogLine(pr.Author))
	if line := grant.ignoredLogLine(); line != "" {
		logf(pr.Number, "config", "%s/%s: %s\n", owner, repo, line)
	}

	// #2366: repo plugins. The gate runs first and is the only way to the
	// readers: with repo_plugins off (the default) nothing below reads, fetches
	// or loads anything, whatever the repo declares. Everything is pinned to
	// baseSHA, never the clone, which is the untrusted PR head.
	var repoPlugins repoPluginResult
	pluginGate, gateWhy := evaluateRepoPluginGate(cfg, repoYAML.Plugins, pr.BaseRef, pr.DefaultBranch, baseSHA)
	if gateWhy != "" {
		logf(pr.Number, "config", "%s/%s: %s\n", owner, repo, gateWhy)
	}
	if pluginGate.open {
		repoPlugins = fetchRepoPlugins(client, pluginGate, repoPluginInput{
			Owner: owner, Repo: repo, PRNumber: pr.Number, BaseSHA: baseSHA,
			Selected: repoYAML.Plugins, Allow: cfg.RepoPluginMarketplaces,
		})
		logRepoPlugins(pr.Number, owner, repo, baseSHA, repoPlugins)
		if repoPlugins.Degraded {
			degraded = true // a failed read is not a verdict about the repo's plugins
		}
	}

	dir, cleanup, err := clone(ctx, owner, repo, client.Token(), pr.Number, pr.HeadSHA)
	if err != nil {
		return ReviewOutcome{Err: fmt.Errorf("cloning PR head: %w", err)}
	}
	defer cleanup()

	// Hand the model the diff we already have. The clone is a depth-1 fetch
	// of the PR head alone (refs/pull/<N>/head, or the selected SHA when that
	// ref is missing), so the base branch is not a ref in it and
	// the model cannot derive the change from git — see ReviewRequest.Staged.
	// Staged as a directory outside the clone (one entry per changed path or
	// bounded chunk, plus an index), so it never appears in the reviewed tree,
	// git status, or a Grep/Glob sweep of the repo, and no part of it sits
	// behind a single Read call's limit (#2190).
	// Guard the empty case explicitly. When FetchPRDiff returns 406
	// too_large the files-API fallback above leaves diff == "", and staging
	// that would hand the reviewer an empty index the prompt then calls the
	// authoritative statement of the change while telling it not to fall
	// back to git — strictly worse than nothing, and it would hit exactly
	// the largest PRs.
	var staged *StagedDiff
	if diff == "" {
		logf(pr.Number, "warn", "no diff text available for %s/%s#%d (size-guard fallback) — review proceeds without a staged diff\n", owner, repo, pr.Number)
	} else if s, cleanupStaged, stageErr := stageDiff(stageBlocks, stagePreamble, pr.Number); stageErr != nil {
		// Non-fatal: the prompt degrades to its older base-branch wording
		// rather than failing the review outright, and coverage is partial.
		logf(pr.Number, "warn", "staging diff for %s/%s#%d: %v — review will proceed without it\n", owner, repo, pr.Number, stageErr)
	} else {
		staged = s
		defer cleanupStaged()
	}

	result, err := claude.Review(ctx, ReviewRequest{
		Owner: owner, Repo: repo, PRNumber: pr.Number, Title: pr.Title, Body: pr.Body,
		HeadSHA: pr.HeadSHA, BaseBranch: pr.BaseRef, Model: cfg.Model, Effort: cfg.Effort, ClaudeConfigDir: cfg.ClaudeConfigDir,
		WorkDir: dir, MaxWallTime: cfg.MaxWallTime, ReviewThreads: threads, ReviewThreadsTruncated: threadsTruncated,
		OmittedExcludedPaths: omittedExcludedPaths, OmittedTrimmedPaths: omittedTrimmedPaths,
		OmittedReviewedPaths: omittedReviewedPaths,
		OperatorGuidance:     cfg.ReviewGuidance, OperatorGuidanceMode: cfg.ReviewGuidanceMode,
		RepoGuidance: repoGuidance, RepoGuidanceMode: repoGuidanceMode,
		Staged:      staged,
		ReviewSkill: reviewSkill, OperatorPluginDirs: operatorPluginDirs,
		RepoPlugins: repoPlugins.Plugins, RepoPluginNotes: repoPlugins.Notes(),
		BaseSHA: baseSHA, Grant: grant,
		MaxCoveragePasses: cfg.MaxCoveragePasses,
	})
	if err != nil {
		logf(pr.Number, "claude", "review invocation failed for %s/%s#%d: %v — posting nothing\n", owner, repo, pr.Number, err)
		return ReviewOutcome{Err: fmt.Errorf("claude review invocation: %w", err)}
	}

	// #2190: the coverage report is stripped before the findings are parsed so
	// it can never be read as part of the summary or the findings fence.
	reported, reportOK, reviewText := extractCoverage(result.Text)
	cov := computeCoverage(coverageInput{
		Staged: staged, ChangedPaths: changedPaths, TrimmedPaths: omittedTrimmedPaths,
		Reported: reported, ReportOK: reportOK,
		Transcript: result.Opened, TranscriptAuthoritative: result.OpenedAuthoritative,
		UnobservedSubagents: result.UnobservedSubagents,
		Truncated:           result.Truncated,
	})
	// #2303: keep the raw transcript while the temp dirs still exist (their
	// removal is deferred above) and before the review is submitted.
	transcriptPath := maybeKeepTranscript(cfg, pr.Number, owner, repo, pr.HeadSHA, cov, result.Transcript)
	logCoverage(pr.Number, cov, result.OpenedAuthoritative, transcriptPath)
	logCoverageLoop(pr.Number, result.Loop, result.NumTurns, result.CostUSD)

	summary, findings, parseInfo := parseReviewFindings(reviewText)
	logSummaryParseInfo(pr.Number, parseInfo)
	findings = dedupeFindings(findings)
	event := decideEvent(findings, cfg.RequestChangesThreshold)
	comments, demoted := partitionFindings(findings, validRightAnchors(diff))
	body := appendCoverage(buildReviewBody(summary, demoted), cov)
	if result.FallbackFrom != "" {
		body = fallbackNotice(result.FallbackFrom, baseSHA, result.FallbackReason) + "\n\n" + body
	}
	if notice := readOnlyNotice(grant, result.PluginsStripped, result.PluginsSkipped, result.PluginsPartial); notice != "" {
		body = notice + "\n\n" + body
	}

	if _, err := client.SubmitPRReview(owner, repo, pr.Number, pr.HeadSHA, body, event, comments); err != nil {
		return ReviewOutcome{Err: fmt.Errorf("submitting review: %w", err)}
	}
	tracker.Record(owner, repo, pr.Number, pr.HeadSHA)

	if forceReview {
		if err := MarkForceReviewsProcessed(client, owner, repo, pr.Number); err != nil {
			logf(pr.Number, "warn", "marking /pruefer review comment processed on %s/%s#%d: %v\n", owner, repo, pr.Number, err)
			degraded = true
		}
	}

	logf(pr.Number, "review", "submitted review for %s/%s#%d at %s\n", owner, repo, pr.Number, pr.HeadSHA)
	return ReviewOutcome{Reviewed: true, NumTurns: result.NumTurns, CostUSD: result.CostUSD, FallbackFrom: result.FallbackFrom}
}

// logRepoPlugins is R8: which repo plugins were selected, which loaded with the
// repository and commit each came from, and why any were skipped.
func logRepoPlugins(prNumber int, owner, repo, baseSHA string, res repoPluginResult) {
	for _, p := range res.Plugins {
		logf(prNumber, "config", "%s/%s: repo plugin %s will load from %s %s at commit %s (path %q, tree %s, cached=%t); base %s\n",
			owner, repo, sanitizeLogID(p.ID), p.Origin, p.Repo, p.Commit, p.Path, p.TreeSHA, p.Cached, baseSHA)
	}
	for _, s := range res.Skipped {
		detail := ""
		if s.Detail != "" {
			detail = " [" + s.Detail + "]"
		}
		logf(prNumber, "warn", "%s/%s: repo plugin %s not loaded: %s%s\n", owner, repo, sanitizeLogID(s.ID), s.Reason, detail)
	}
}

// fallbackNotice is the line a posted review carries when the selected review
// skill was replaced by the default (#2307, R4): the requested reference, the
// base SHA it was looked up at and the reason. The reference and reason can
// come from a repo's files, so both are sanitised and rendered in code spans;
// nothing in them can form markdown or a mention. The reason keeps quotes,
// parentheses, commas and semicolons so the resolver's message and the plugin
// notes read as written.
func fallbackNotice(ref, baseSHA, reason string) string {
	sha := sanitizeNoticeText(baseSHA)
	if sha == "" {
		sha = "unknown"
	}
	return fmt.Sprintf("> 🏭 **Review skill fallback** — requested `%s` at base `%s` was not used (`%s`); this review ran `%s`.",
		sanitizeNoticeText(ref), sha, sanitizeNoticeReason(reason), DefaultReviewSkill)
}

// readOnlyNotice is the line a posted review carries when the outside-author
// policy removed something the author would otherwise have had (#2308, R3):
// the coded review tools, or the hooks and MCP servers of operator plugins. It
// is empty when nothing was removed, so a review with no tools configured gets
// no banner. The association comes from GitHub and the plugin names from the
// operator's manifests; both are sanitised and rendered in code spans, and no
// text from the repo (such as a requested rule) is ever echoed.
func readOnlyNotice(g reviewGrant, strippedPlugins, skippedPlugins, partialPlugins []string) string {
	if g.Trust.Trusted {
		return skippedPluginNotice(skippedPlugins)
	}
	if !g.DowngradedExtras && len(strippedPlugins) == 0 && len(skippedPlugins) == 0 && len(partialPlugins) == 0 {
		return ""
	}
	var removed []string
	if g.DowngradedExtras {
		removed = append(removed, "the coded review tools the operator allows were not available")
	}
	if len(strippedPlugins) > 0 {
		names := make([]string, len(strippedPlugins))
		for i, n := range strippedPlugins {
			names[i] = "`" + sanitizeNoticeText(n) + "`"
		}
		removed = append(removed, "the hooks, MCP servers and tool pre-approvals of operator plugin "+strings.Join(names, ", ")+" were not loaded")
	}
	if len(skippedPlugins) > 0 {
		names := make([]string, len(skippedPlugins))
		for i, n := range skippedPlugins {
			names[i] = "`" + sanitizeNoticeText(n) + "`"
		}
		removed = append(removed, "operator plugin "+strings.Join(names, ", ")+" could not be prepared for a read-only review and was not loaded")
	}
	if len(partialPlugins) > 0 {
		names := make([]string, len(partialPlugins))
		for i, n := range partialPlugins {
			names[i] = "`" + sanitizeNoticeText(n) + "`"
		}
		removed = append(removed, "operator plugin "+strings.Join(names, ", ")+" was loaded without the scripts and other files its read-only copy omits, so a skill that relies on them may be limited")
	}
	assoc := "unknown"
	if g.Trust.Association != "" {
		assoc = sanitizeNoticeText(g.Trust.Association)
	}
	return fmt.Sprintf("> 🏭 **Read-only review** — the PR author's association with this repository is `%s`, not owner, member or collaborator, so this review ran with the read-only tool grant: %s.",
		assoc, strings.Join(removed, "; "))
}

// skippedPluginNotice is the line a trusted author's posted review carries
// when an operator plugin was dropped because it could not be scanned or copied
// while its tool pre-approvals were checked. Nothing about the author's grant
// changed, but the review ran without that plugin's skills and agents, and the
// log line alone would leave a reader assuming it was loaded. Names come from
// the operator's plugin directories and are sanitised into code spans.
func skippedPluginNotice(skipped []string) string {
	if len(skipped) == 0 {
		return ""
	}
	names := make([]string, len(skipped))
	for i, n := range skipped {
		names[i] = "`" + sanitizeNoticeText(n) + "`"
	}
	return "> 🏭 **Operator plugin not loaded** — operator plugin " + strings.Join(names, ", ") +
		" could not be prepared for this review and was not loaded, so its skills and agents were unavailable."
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

// excludedPathsNote renders the R7 parenthetical distinguishing "no
// excluded_paths configured at all" — the state every operator starts in —
// from "excluded_paths is configured (but perhaps not matching what you
// expected)", so a size-related skip/trim log line is self-diagnosing about
// whether the exclusion lever was even in play, rather than looking
// byte-identical in both cases as it did before this issue.
func excludedPathsNote(patterns []string) string {
	if len(patterns) == 0 {
		return "no excluded_paths configured"
	}
	return fmt.Sprintf("excluded_paths=%v", patterns)
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
