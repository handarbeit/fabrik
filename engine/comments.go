package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/handarbeit/fabrik/boardcache"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tui"
)

func (e *Engine) findNewComments(item gh.ProjectItem) []gh.Comment {
	var newComments []gh.Comment
	repoStr := itemOwnerRepoString(item, e.defaultRepo())
	snap, _ := e.store.Get(repoStr, item.Number)
	for _, c := range item.Comments {
		// Skip comments we've already processed
		if !snap.CommentProcessed(c.ID).IsZero() {
			continue
		}
		// Skip comments that look like Fabrik output
		if strings.HasPrefix(c.Body, "🏭 **Fabrik") {
			continue
		}
		// Skip comments already processed (marked with 🚀 reaction)
		if c.HasReaction("ROCKET") {
			continue
		}
		// Skip non-actionable bot service-notices (e.g. quota/rate-limit banners) —
		// admitting these spawns a comment-processing worker and, if replied to,
		// re-triggers the subscribed bot into an unbounded reply loop (#1083, #1088).
		// Watermarking these so they don't re-admit on a later poll is handled
		// separately by settleBotServiceNotices, since a bot-notice-only backlog
		// never reaches processComments once excluded here.
		if isBotServiceNotice(c) {
			continue
		}
		newComments = append(newComments, c)
	}
	return newComments
}

// botServiceNoticePatterns are literal, case-insensitive substrings identifying
// non-actionable bot service/status notices (quota exhaustion, rate limiting,
// sunset/unsupported-content notices) as opposed to genuine bot review
// content. Deliberately narrow: a bare "quota"/"rate limit" substring would
// risk matching genuine review prose that discusses rate limiting, and would
// collide with this repo's own test fixtures (e.g. the literal body "quota
// notice" used across blocked_on_input_test.go to exercise the human-only
// resume gate, ADR-069).
//
// Grouped by vendor. Where a bot emits a non-prose signal (e.g. CodeRabbit's
// HTML comment marker), that pattern is preferred/listed first for that
// vendor since structural markers don't drift when marketing copy changes;
// prose patterns are kept alongside it as a fallback in case a vendor ever
// omits the marker.
var botServiceNoticePatterns = []string{
	// Gemini
	"daily quota limit",
	"you have reached your daily quota",
	"rate limit exceeded",
	"you have reached your rate limit",
	"you have exceeded your rate limit",
	"api rate limit",
	"the consumer version of gemini code assist on github has been sunset",
	"gemini is unable to generate a review for this pull request due to the file types involved not being currently supported",

	// CodeRabbit
	"rate limited by coderabbit.ai", // structural: HTML comment marker, not user-facing prose
	"## review limit reached",       // markdown heading form used in the actual banner
	"you've reached your pr review limit, so we couldn't start this review",
	// Structural marker on CodeRabbit's auto-generated acknowledgement replies
	// (e.g. "`@user`, acknowledged. No action taken.") — content-free replies
	// to a prior comment, not review findings. Distinct marker/trigger from
	// the rate-limit notice above (#1122, closed): this is CodeRabbit
	// acknowledging it has nothing to add, not CodeRabbit declining to review
	// due to quota. Admitting these spawned a comment-processing worker whose
	// "not actionable" reply re-mentioned the bot, re-triggering another
	// acknowledgement — a runaway loop distinct from #1083/#1088 ($7.75/39
	// invocations observed on #933, #1141).
	"auto-generated reply by coderabbit",
}

// isBotServiceNotice reports whether c is a non-actionable bot service/status
// notice (e.g. "you have reached your daily quota limit") rather than genuine
// bot review content. Both the bot-login check and a pattern match are
// required — a human comment mentioning the same phrasing is not classified
// as a notice, and a bot comment that doesn't match a known pattern (e.g. a
// CHANGES_REQUESTED review body) is left for normal processing.
func isBotServiceNotice(c gh.Comment) bool {
	if !gh.IsBotLogin(c.Author) {
		return false
	}
	lower := strings.ToLower(c.Body)
	for _, pattern := range botServiceNoticePatterns {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	return false
}

// filterBotServiceNotices drops non-actionable bot service/status notices
// (see isBotServiceNotice) from comments, preserving order. This is the single
// chokepoint applied inside processComments to the fully-assembled working
// slice — covering both findNewComments-sourced comments (already filtered,
// so this is idempotent for them) and comments merged in from
// item.LinkedPRReviewThreadComments or a reinvoke dispatcher's build() output,
// neither of which is otherwise filtered for bot notices (#1221).
func filterBotServiceNotices(comments []gh.Comment) []gh.Comment {
	var filtered []gh.Comment
	for _, c := range comments {
		if isBotServiceNotice(c) {
			continue
		}
		filtered = append(filtered, c)
	}
	return filtered
}

// filterHuman filters a comment slice down to comments authored by a human —
// excluding bot logins (gh.IsBotLogin) and comments with no resolvable author
// (fail closed: an unattributed author, e.g. a deleted GitHub account, is
// treated as non-human rather than silently defeating a pause).
//
// Deliberately does NOT also exclude e.cfg.User: that is the operator's own
// GitHub login, not Fabrik's bot identity, and in the common (currently only
// supported — see #671) single-account deployment Fabrik posts under that
// same account. Excluding it here would filter out the operator's own
// resume reply. Fabrik's own comments are already fully excluded upstream by
// findNewComments' 🏭 **Fabrik body-prefix check, independent of author.
func filterHuman(comments []gh.Comment) []gh.Comment {
	var human []gh.Comment
	for _, c := range comments {
		if c.Author == "" || gh.IsBotLogin(c.Author) {
			continue
		}
		human = append(human, c)
	}
	return human
}

// humanNewComments filters findNewComments to comments authored by a human.
// Used at the paused / awaiting-input resume-decision sites so bot chatter
// cannot silently defeat an operator-applied pause (#1083). Callers that also
// need the unfiltered set (to hand the full backlog to processComments once
// a resume is authorized) should call findNewComments once and pass it
// through filterHuman directly instead of calling this a second time.
func (e *Engine) humanNewComments(item gh.ProjectItem) []gh.Comment {
	return filterHuman(e.findNewComments(item))
}

// processComments handles new user comments on an issue.
// Flow: 👀 reactions → editing label → invoke Claude → perform actions / update issue body → remove editing label → 🚀 reactions
func (e *Engine) processComments(ctx context.Context, board *gh.ProjectBoard, item gh.ProjectItem, stage *stages.Stage, comments []gh.Comment, onPIDReady ...func(int)) error {
	owner, repo := itemOwnerRepo(item, e.defaultRepo())

	// Account-wide Claude usage-limit suspension gate (ADR-1120): checked before
	// any side effect (reactions, labels, worktree setup) so a suspended cycle is
	// a full no-op — the comment remains "new" and is retried on the next poll
	// once dispatch resumes, rather than being consumed by a doomed invocation.
	if _, suspended := e.claudeSuspendedUntilTime(time.Now()); suspended {
		e.logf(item.Number, "claude-limit", "Claude dispatch suspended account-wide; skipping comment review\n")
		return nil
	}

	// Merge any unresolved PR review thread comments into the working slice.
	// This ensures that when a user nudge arrives (e.g. "please address Copilot
	// feedback"), the review thread comments are processed alongside the
	// conversation comment without requiring a separate dispatchReviewReinvoke
	// cycle. For non-PR-backed items LinkedPRReviewThreadComments is empty, so
	// this is a no-op. For dispatchReviewReinvoke call sites the synthetic
	// comments were already filtered by buildReviewThreadComments, so the merge
	// adds nothing (ID dedup prevents duplicates).
	if len(item.LinkedPRReviewThreadComments) > 0 {
		existingIDs := make(map[string]bool, len(comments))
		for _, c := range comments {
			existingIDs[c.ID] = true
		}
		repoStr := itemOwnerRepoString(item, e.defaultRepo())
		snap, _ := e.store.Get(repoStr, item.Number)
		for _, c := range item.LinkedPRReviewThreadComments {
			if existingIDs[c.ID] {
				continue
			}
			if c.HasReaction("ROCKET") {
				continue
			}
			if !snap.CommentProcessed(c.ID).IsZero() {
				continue
			}
			comments = append(comments, c)
		}
	}

	// Single chokepoint (#1221): apply isBotServiceNotice to the fully-assembled
	// working slice, regardless of which dispatcher invoked processComments or
	// which collection (item.Comments via findNewComments, or
	// item.LinkedPRReviewThreadComments via the merge above) a comment came
	// from. Comments sourced from findNewComments are already filtered — this
	// is idempotent for them — but the merge above and the three reinvoke
	// dispatchers' build() output are not otherwise filtered, and this is what
	// closes that gap. If every candidate comment was a bot notice, return
	// before any reaction/label/worktree/invocation side effect, mirroring the
	// claudeSuspendedUntilTime early-return above.
	comments = filterBotServiceNotices(comments)
	if len(comments) == 0 {
		e.logf(item.Number, "comments", "all candidate comments were bot service notices; skipping\n")
		return nil
	}

	e.logf(item.Number, "comments", "processing %d new comment(s) — stage: %s\n",
		len(comments), stage.Name)

	// Circuit breaker (#1089, moved earlier by #1413): record this invocation
	// now that the working comment slice is final, before any setup side
	// effect (👀 reactions, editing label, worktree setup) runs. Recording
	// here — rather than just before the Claude invocation — means a setup
	// failure (e.g. an editing-label API failure) DOES count as a cycle: the
	// three setup-failure early-returns below each also call
	// checkCommentBreaker so a persistently failing setup step trips the
	// breaker instead of looping unbounded (see #1382/#1386).
	e.recordCommentBreakerInvocation(item, lastCommentAuthor(comments))

	itemRepo := itemOwnerRepoString(item, e.defaultRepo())
	startedAt := time.Now()
	e.emitStructural(tui.JobStartedEvent{
		IssueNumber: item.Number,
		Repo:        itemRepo,
		Title:       item.Title,
		StageName:   stage.Name,
		IsComment:   true,
		StartedAt:   startedAt,
	})
	defer e.emitStructural(tui.JobCompletedEvent{
		IssueNumber: item.Number,
		Repo:        itemRepo,
		Title:       item.Title,
		StageName:   stage.Name,
		IsComment:   true,
		Skipped:     true,
	})

	// Step 1: React with 👀 to all new comments.
	e.acknowledgeComments(owner, repo, item.Number, comments)

	// Step 2: Add editing label
	if err := e.client.AddLabelToIssue(owner, repo, item.Number, "fabrik:editing"); err != nil {
		// New breaker checked first (R5): if it trips, the issue is already
		// paused and the old breaker's check is skipped for this cycle — at
		// most one pause comment per cycle.
		if !e.checkNoOpCommentCycle(item, stage, false, lastCommentAuthor(comments)) {
			e.checkCommentBreaker(item, fmt.Sprintf("the fabrik:editing label add failed: %v", err))
		}
		return fmt.Errorf("adding editing label: %w", err)
	} else {
		e.syncLabelAdd(item, "fabrik:editing", true)
	}

	// Step 3: Ensure worktree
	wm := e.worktreesFor(item.Repo)
	baseBranch, err := e.baseBranchForItem(item, wm)
	if err != nil {
		e.removeEditingLabel(owner, repo, item.Number)
		if !e.checkNoOpCommentCycle(item, stage, false, lastCommentAuthor(comments)) {
			e.checkCommentBreaker(item, fmt.Sprintf("resolving the base branch failed: %v", err))
		}
		return fmt.Errorf("setting up worktree for %s/%s: %w", owner, repo, err)
	}
	// Merge-queue awareness (ADR-058 D3): skip the preemptive rebase when the PR is
	// in the queue (FR-1) or the repo is queue-enabled (FR-2). Both ProjectItem-sourced
	// signals are false-by-default, preserving legacy behavior on non-queue repos (FR-3).
	skipUpdate := prInMergeQueue(item) || e.suppressPreemptiveRebase(item)
	workDir, err := wm.EnsureWorktree(item.Number, baseBranch, skipUpdate)
	if err != nil {
		e.removeEditingLabel(owner, repo, item.Number)
		if !e.checkNoOpCommentCycle(item, stage, false, lastCommentAuthor(comments)) {
			e.checkCommentBreaker(item, fmt.Sprintf("setting up the worktree failed: %v", err))
		}
		return fmt.Errorf("setting up worktree for %s/%s: %w", owner, repo, err)
	}

	// If a PR exists and its base branch doesn't match the resolved base, update it.
	e.syncPRBase(item, baseBranch)
	e.ensureEnvExcluded(item.Number, workDir)
	e.symlinkEnvIfEnabled(item.Number, workDir)

	// Write context files (all stages including current) before Claude runs.
	e.writeContextFiles(item, stage, workDir, true)

	// Step 4: Invoke Claude with the comment review prompt
	modelOverride := e.extractModelOverride(item.Number, item.Labels)
	if modelOverride != "" {
		e.logf(item.Number, "model", "using model override %q\n", modelOverride)
	}
	effortOverride := e.extractEffortOverride(item.Number, item.Labels)
	if effortOverride != "" {
		e.logf(item.Number, "effort", "using effort override %q\n", effortOverride)
	}
	// Comment processing only ever runs on a stage that has already produced
	// at least one prior attempt (there's output to comment on), so a
	// CreateDraftPR stage's PR may already exist — pass resume=true.
	fabrikRoot, prNumber := e.resolveFabrikEnvOpts(item, stage, true)
	invokeOpts := InvokeOptions{
		ModelOverride:     modelOverride,
		EffortOverride:    effortOverride,
		BaseBranch:        baseBranch,
		FabrikRoot:        fabrikRoot,
		PRNumber:          prNumber,
		FabrikRepo:        e.defaultRepo(),
		MaxResumeFailures: e.cfg.MaxResumeFailures,
	}
	if len(onPIDReady) > 0 && onPIDReady[0] != nil {
		invokeOpts.OnPIDReady = onPIDReady[0]
	}

	// Snapshot extend-turns label before loop (stable across any mid-loop FetchItemDetails re-fetch).
	hadExtendTurnsLabel := hasExtendTurnsLabel(item)

	// Capture the pre-invocation HEAD so a commit landed during this cycle
	// resets the breaker counter below. The invocation itself was already
	// recorded above, before setup — see the comment there.
	preInvokeSHA, _ := gitHeadSHA(workDir)

	output, usage, invCompleted, err := e.runCommentExtensionLoop(ctx, stage, &item, comments, workDir, invokeOpts, hadExtendTurnsLabel)

	// headChanged is computed once per cycle and reused below for the
	// success-agnostic no-op breaker's "progressed" signal (R2) — a commit is
	// one of the three progress signals it must be computed identically from,
	// per the constraint that "no observable progress" is evaluated the same
	// way everywhere it's checked (#1555).
	headChanged := false
	if postInvokeSHA, shaErr := gitHeadSHA(workDir); shaErr == nil && postInvokeSHA != preInvokeSHA {
		headChanged = true
		e.resetCommentBreaker(item)
	}

	if line := formatStatsLogLine(usage); line != "" {
		e.logf(item.Number, "stats", "%s\n", line)
	}
	func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.totalTokens = addTokenUsage(e.totalTokens, usage)
	}()
	// Honor FABRIK_STAGE_COMPLETE consistently with stage runs: invCompleted is the
	// invoke layer's marker-based completion (engine/claude.go), which already treats
	// the marker as authoritative even when the process exits non-zero (e.g. a timeout
	// kill after the stage finished) and withholds completion on engine shutdown. A
	// non-zero exit does NOT veto completion; it is recorded separately via Errored so
	// the error is still visible in history (JobCompletedEvent.Success=false).
	completed := invCompleted
	// A turn-limit exit (CLI subtype error_max_turns) is not a genuine fault —
	// see the identical wiring in finalizeStageOutcome (engine/item.go) and
	// claudeTurnLimitError in engine/claude.go. Only what feeds the
	// InvocationRecorded write changes; err itself is left untouched for the
	// retry/circuit-breaker logic below.
	var turnLimitErr *claudeTurnLimitError
	turnLimited := errors.As(err, &turnLimitErr)
	e.store.Apply(itemstate.InvocationRecorded{
		Repo:        itemOwnerRepoString(item, e.defaultRepo()),
		Number:      item.Number,
		Completed:   completed,
		Errored:     err != nil && !turnLimited,
		TurnLimited: turnLimited,
		Usage:       usage,
		IsComment:   true,
		Duration:    time.Since(startedAt),
	})
	// Bail early ONLY if the stage did not complete. If FABRIK_STAGE_COMPLETE was
	// emitted before the process exited non-zero (e.g. a timeout kill after the stage
	// finished, or trailing work that ended non-zero), proceed with the completion path
	// exactly like a stage run — a non-zero exit must not silently swallow a real
	// completion. The error is already recorded via Errored above. On engine shutdown,
	// invCompleted is false (see engine/claude.go), so that case still bails here.
	if err != nil && !completed {
		e.removeEditingLabel(owner, repo, item.Number)
		if ctx.Err() != nil {
			e.logf(item.Number, "skip", "cancelled during claude comment review\n")
			return nil
		}
		// A Claude usage-limit hit is not "no forward progress" — it's an
		// account-wide condition unrelated to this issue's comment thread, already
		// handled by activateClaudeSuspension above. Counting it toward the
		// circuit breaker would risk tripping the breaker purely because the
		// account ran dry, not because this issue is stuck.
		var limitErr *claudeUsageLimitError
		if errors.As(err, &limitErr) {
			e.logf(item.Number, "claude-limit", "claude comment review hit the account usage limit; not counted toward the comment circuit breaker\n")
			return nil
		}
		// Deliberately NO exclusion for *claudeResumeFailureError here (#1414),
		// unlike the usage-limit exclusion immediately above: a resume failure
		// is specific to this issue's own session, not an account-wide
		// condition, so it counts toward the comment circuit breaker exactly
		// like *claudeAPIErrorExit already does (ADR-1458) — this path is the
		// comment-processing loop's only bound, and exempting it would leave
		// the comment-triggered dispatch path with no bound at all if a
		// session kept failing to resume. The max_retries-equivalent exemption
		// this type carries (see finalizeStageOutcome in item.go) is a
		// separate, narrower guarantee that only applies to the stage path.
		e.logf(item.Number, "warn", "claude comment review issue: %v\n", err)
		// A non-completing, erroring invocation is exactly the "no forward progress"
		// case both circuit breakers exist to catch — check them here too, not
		// only on the successful-completion path below. completed is false on
		// this branch by construction, so progressed reduces to headChanged
		// (publishCommentOutput, the other progress signal, has not run yet).
		if !e.checkNoOpCommentCycle(item, stage, headChanged, lastCommentAuthor(comments)) {
			e.checkCommentBreaker(item, "")
		}
		return err
	}
	if err != nil {
		e.logf(item.Number, "warn", "claude comment review exited with error but stage completed (marker found) — proceeding: %v\n", err)
	}

	summary := e.publishCommentOutput(owner, repo, item, stage, comments, output, workDir, baseBranch)

	e.finalizeComments(ctx, board, item, stage, comments, owner, repo, baseBranch, completed, summary)

	// Checked last so any reset applied above (stage-complete inside
	// finalizeComments, or an issue-body update inside publishCommentOutput)
	// takes effect before evaluating whether this cycle tripped the breaker.
	// progressed mirrors R2's three signals exactly: a commit landed
	// (headChanged), the issue body updated (extractUpdatedBody on the
	// original, unstripped output — publishCommentOutput took output by
	// value, so this copy is untouched by its marker-stripping), or the
	// stage completed (FABRIK_STAGE_COMPLETE was emitted).
	progressed := headChanged || extractUpdatedBody(output) != "" || completed
	if !e.checkNoOpCommentCycle(item, stage, progressed, lastCommentAuthor(comments)) {
		e.checkCommentBreaker(item, "")
	}

	return nil
}

// lastCommentAuthor returns the author of the last comment in comments, or ""
// if comments is empty. Used to attribute a circuit-breaker invocation to the
// comment that triggered it (#1089).
func lastCommentAuthor(comments []gh.Comment) string {
	if len(comments) == 0 {
		return ""
	}
	return comments[len(comments)-1].Author
}

// acknowledgeComments reacts with 👀 to all new comments. PR review thread
// (inline) comments use a different REST endpoint than issue comments.
func (e *Engine) acknowledgeComments(owner, repo string, itemNumber int, comments []gh.Comment) {
	for _, c := range comments {
		if c.DatabaseID == 0 {
			e.logf(itemNumber, "debug", "skipping 👀 reaction for synthetic comment %s (no DatabaseID)\n", c.ID)
			continue
		}
		if c.ReviewThreadID != "" {
			// no write-through: excluded — AddPRReviewCommentReaction does not affect dispatch-relevant cache state
			if err := e.client.AddPRReviewCommentReaction(owner, repo, c.DatabaseID, "eyes"); err != nil {
				e.logf(itemNumber, "warn", "could not add 👀 to review thread comment %s: %v\n", c.ID, err)
			}
		} else {
			// no write-through: excluded — AddCommentReaction does not affect dispatch-relevant cache state
			if err := e.client.AddCommentReaction(owner, repo, c.DatabaseID, "eyes"); err != nil {
				e.logf(itemNumber, "warn", "could not add 👀 to comment %s: %v\n", c.ID, err)
			}
		}
	}
}

// runCommentExtensionLoop determines the initial turn budget (label absent →
// MaxTurnsOverride=0, using commentMaxTurns naturally; label present → 2× the
// pre-granted budget, no progress check for the first hit), then invokes
// Claude for comment review, extending the turn budget while fabrik:extend-turns
// is present and progress is detected, up to a hard cap of 3× commentMaxTurns
// across all invocations. InvokeForComments resumes the existing session
// internally on each extension. Unlike the stage path, comment-review
// extension is intentionally label-gated: no silent budget expansion without
// opt-in. item is a pointer because a mid-loop progress check may re-fetch it
// (see detectProgress) — the caller observes the refreshed item afterward.
func (e *Engine) runCommentExtensionLoop(ctx context.Context, stage *stages.Stage, item *gh.ProjectItem, comments []gh.Comment, workDir string, invokeOpts InvokeOptions, hadExtendTurnsLabel bool) (output string, usage TokenUsage, completed bool, err error) {
	base := commentMaxTurns(stage)
	firstBudget := 0
	totalMultiple := 1
	if hadExtendTurnsLabel && base > 0 {
		firstBudget = 2 * base
		totalMultiple = 2
	}
	baseline := snapshotBaseline(stage, *item, workDir)

	currentBudget := firstBudget
	for {
		invokeOpts.MaxTurnsOverride = currentBudget
		var invOutput string
		var invUsage TokenUsage
		invOutput, completed, invUsage, err = e.claude.InvokeForComments(ctx, stage, *item, comments, workDir, invokeOpts)
		output += invOutput
		usage = addTokenUsage(usage, invUsage)

		var limitErr *claudeUsageLimitError
		if errors.As(err, &limitErr) {
			e.activateClaudeSuspension(item.Number, limitErr.ResetTime, time.Now())
		} else if err == nil {
			// Only a successful invocation is evidence the limit has cleared — a generic,
			// unrelated error proves nothing about account-wide usage-limit state and must
			// not clear it (see the matching comment in item.go's runInvocationWithExtension).
			e.clearClaudeSuspension("comment review invocation reached Claude")
		}

		// hitLimit uses currentBudget > 0 (not base > 0) so that extension only fires
		// when fabrik:extend-turns is present.
		hitLimit := !completed && err == nil && currentBudget > 0 && invUsage.TurnsUsed >= currentBudget
		if !hitLimit || totalMultiple >= 3 {
			break
		}
		issueLogf := func(tag, format string, args ...any) {
			e.logf(item.Number, tag, format, args...)
		}
		hasProgress, progressErr := detectProgress(ctx, stage, item, baseline, workDir, e.client, issueLogf)
		if progressErr != nil {
			e.logf(item.Number, "extend-turns", "comment progress check failed: %v\n", progressErr)
			break
		}
		if !hasProgress {
			break
		}
		totalMultiple++
		currentBudget = base
		e.logf(item.Number, "extend-turns", "extending comment review to %d× budget (%d turns used)\n", totalMultiple, usage.TurnsUsed)
	}
	// Report cumulative budget across all extensions.
	usage.MaxTurns = totalMultiple * base
	return output, usage, completed, err
}

// publishCommentOutput captures the summary from output (before markers are
// stripped in-place below — once stripped, extractSummary(output) returns ""
// and the Verification update would be silently lost), applies any
// FABRIK_ISSUE_UPDATE_BEGIN/END issue-body update, strips Fabrik markers, and
// posts the stage comment — plus, for a review-reinvoke, a Fabrik-marked
// summary comment on the linked PR so reviewers can see at a glance that their
// feedback was addressed. Returns the extracted summary for the caller to pass
// to updatePRVerification on stage completion.
func (e *Engine) publishCommentOutput(owner, repo string, item gh.ProjectItem, stage *stages.Stage, comments []gh.Comment, output, workDir, baseBranch string) string {
	branch, commit, mainSHA, timestamp := captureGitMeta(workDir, baseBranch)

	summary := extractSummary(output)
	// Captured before markers are stripped below — CheckNoWorkNeeded matches the
	// raw marker line, which stripLine removes a few lines down.
	noWorkNeeded := CheckNoWorkNeeded(output)

	// Strip FABRIK_ISSUE_UPDATE block from output, then update issue body.
	if updatedBody := extractUpdatedBody(output); updatedBody != "" {
		e.logf(item.Number, "edit", "updating issue body\n")
		// no write-through: excluded — issue body is not read from cache for dispatch decisions
		if err := e.client.UpdateIssueBody(owner, repo, item.Number, updatedBody); err != nil {
			e.logf(item.Number, "warn", "could not update issue body: %v\n", err)
		} else {
			// Advances the probe staleness baseline (#1090) — the edit above just
			// bumped the issue's real GitHub updatedAt, so the next probe cycle
			// must not treat that bump as a stale signal.
			e.store.Apply(itemstate.SelfWriteObserved{Repo: owner + "/" + repo, Number: item.Number})
			if e.webhookMgr != nil {
				e.webhookMgr.RegisterEcho("issues", "edited", boardcache.ItemKey(owner+"/"+repo, item.Number))
			}
		}
		output = stripMarkers(output, "FABRIK_ISSUE_UPDATE_BEGIN", "FABRIK_ISSUE_UPDATE_END")
		// Circuit breaker (#1089): a FABRIK_ISSUE_UPDATE is the only forward-progress
		// signal pre-PR stages (Specify/Research/Plan) produce — no commit, no PR,
		// no stage completion until the human is satisfied. Counting it as progress
		// avoids false-tripping normal spec/plan Q&A iteration.
		e.resetCommentBreaker(item)
	}

	// Strip all Fabrik markers from output before posting.
	output = stripLine(output, "FABRIK_STAGE_COMPLETE")
	output = stripLine(output, "FABRIK_BLOCKED_ON_INPUT")
	output = stripLine(output, "FABRIK_NO_WORK_NEEDED")
	output = stripLine(output, "FABRIK_SUMMARY_BEGIN")
	output = stripLine(output, "FABRIK_SUMMARY_END")
	output = strings.TrimSpace(output)

	// Rewrite or create the stage comment (unless post_to_pr). For post_to_pr
	// stages the stage output lives on the PR; comment processing output on
	// such stages is posted as a new comment on the issue as before.
	// Suppressed entirely on a "no action needed" verdict (#1088) — posting any
	// reply is what re-triggers a subscribed bot into a runaway reply loop
	// (#1083), so silence is the correct response here, not a "not actionable"
	// message.
	if output != "" && noWorkNeeded {
		e.logf(item.Number, "comments", "suppressing reply for %s — verdict was FABRIK_NO_WORK_NEEDED\n", stage.Name)
	} else if output != "" {
		if stage.PostToPR {
			comment := formatOutputComment(stage.Name+" (comment review)", output, "", branch, commit, mainSHA, timestamp)
			e.postItemComment(item, comment, true)
		} else {
			existing := findStageComment(item.Comments, stage.Name)
			stageComment := formatOutputComment(stage.Name, output, "", branch, commit, mainSHA, timestamp)
			if existing != nil {
				e.logf(item.Number, "edit", "rewriting stage comment for %s\n", stage.Name)
				if err := e.client.UpdateComment(owner, repo, existing.DatabaseID, stageComment); err != nil {
					e.logf(item.Number, "warn", "could not update stage comment: %v\n", err)
				}
			} else {
				e.postItemComment(item, stageComment, true)
			}
		}
	}

	// When this is a review-reinvoke (all comments are PR inline review thread
	// comments), also post a Fabrik-marked summary on the linked PR. The
	// existing issue comment above is unchanged (R4). Gate: output != "" and a
	// linked PR exists. No post_to_pr check — linked-PR existence is the only
	// gate (R5). Also suppressed on a "no action needed" verdict, same as above.
	if isReviewReinvoke(comments) && output != "" && !noWorkNeeded {
		prNumber, prErr := e.client.FindPRForIssue(owner, repo, item.Number)
		if prErr != nil {
			e.logf(item.Number, "warn", "review reinvoke: could not find PR for issue: %v\n", prErr)
		} else if prNumber > 0 {
			threads := buildThreadEntries(comments)
			addressedReviewIDs := addressedReviewIDsFromComments(comments)
			prComment := formatReviewFeedbackComment(stage.Name, output, branch, commit, mainSHA, timestamp, threads, len(comments), addressedReviewIDs)
			// no write-through: excluded — posts to prNumber (PR comment thread, not issue cache)
			if _, err := e.client.AddComment(owner, repo, prNumber, prComment); err != nil {
				e.logf(item.Number, "warn", "could not post review feedback summary to PR #%d: %v\n", prNumber, err)
			} else {
				if e.webhookMgr != nil {
					e.webhookMgr.RegisterEcho("issue_comment", "created", boardcache.ItemKey(owner+"/"+repo, prNumber))
				}
				e.logf(item.Number, "post", "review feedback summary posted to PR #%d (%d thread(s))\n", prNumber, len(threads))
			}
		} else {
			e.logf(item.Number, "warn", "review reinvoke: no linked PR found — skipping PR summary comment\n")
		}
	}

	return summary
}

// finalizeComments removes the editing label, reacts with 🚀 to all processed
// comments (resolving any addressed review threads), marks the comments as
// processed so they won't be retried, and — if comment processing resolved
// the stage — creates/marks-ready the draft PR and advances to the next
// stage. This avoids an unnecessary extra stage invocation after unblocking.
func (e *Engine) finalizeComments(ctx context.Context, board *gh.ProjectBoard, item gh.ProjectItem, stage *stages.Stage, comments []gh.Comment, owner, repo, baseBranch string, completed bool, summary string) {
	e.removeEditingLabel(owner, repo, item.Number)

	resolvedThreads := make(map[string]bool)
	for _, c := range comments {
		if c.DatabaseID == 0 {
			e.logf(item.Number, "debug", "skipping 🚀 reaction for synthetic comment %s (no DatabaseID)\n", c.ID)
			continue
		}
		if c.ReviewThreadID != "" {
			// no write-through: excluded — AddPRReviewCommentReaction does not affect dispatch-relevant cache state
			if err := e.client.AddPRReviewCommentReaction(owner, repo, c.DatabaseID, "rocket"); err != nil {
				e.logf(item.Number, "warn", "could not add 🚀 to review thread comment %s: %v\n", c.ID, err)
			}
			if !resolvedThreads[c.ReviewThreadID] {
				if err := e.client.ResolveReviewThread(c.ReviewThreadID); err != nil {
					e.logf(item.Number, "warn", "could not resolve review thread %s: %v\n", c.ReviewThreadID, err)
				} else {
					e.logf(item.Number, "review", "resolved review thread %s\n", c.ReviewThreadID)
				}
				resolvedThreads[c.ReviewThreadID] = true
			}
		} else {
			// no write-through: excluded — AddCommentReaction does not affect dispatch-relevant cache state
			if err := e.client.AddCommentReaction(owner, repo, c.DatabaseID, "rocket"); err != nil {
				e.logf(item.Number, "warn", "could not add 🚀 to comment %s: %v\n", c.ID, err)
			}
		}
	}

	// Mark comments as processed only after everything succeeded
	e.markCommentsProcessed(item, comments)

	if completed {
		e.logf(item.Number, "done", "comment processing completed stage %q\n", stage.Name)
		repoStr := itemOwnerRepoString(item, e.defaultRepo())
		e.store.Apply(itemstate.StageRetryCleared{Repo: repoStr, Number: item.Number, StageName: stage.Name})
		e.store.Apply(itemstate.EngineUnpaused{Repo: repoStr, Number: item.Number, StageName: stage.Name})
		var prNumber int
		if stage.CreateDraftPR {
			// Error is intentionally ignored here — comment processing implies the stage
			// already advanced; a PR creation failure here is non-fatal for this path.
			prNumber, _ = e.ensureDraftPR(item, baseBranch)
			e.updatePRVerification(item, prNumber, summary)
		}
		if stage.MarkPRReadyOnComplete {
			e.markPRReady(item, prNumber)
		}
		e.handleStageComplete(ctx, board, item, stage)
	} else {
		e.logf(item.Number, "done", "comment processing complete\n")
	}
}

// isReviewReinvoke reports whether this processComments invocation originated
// from a review-reinvoke dispatch (i.e., every comment is either a PR inline
// review thread comment or a synthetic review-body comment, Finding 4 —
// buildReviewBodyComments). A comment counts as review-reinvoke-eligible when
// c.ReviewThreadID != "" (a real thread comment) OR c.ID has the
// reviewBodyIDPrefix ("review-body:") — a body-derived comment has no thread
// to carry the marker, so it needs its own discriminator. This keeps a
// mixed batch (some thread comments plus a review body) classified as a
// review reinvoke, so publishCommentOutput still posts the PR feedback
// summary for it. Returns false for an empty slice.
func isReviewReinvoke(comments []gh.Comment) bool {
	if len(comments) == 0 {
		return false
	}
	for _, c := range comments {
		if c.ReviewThreadID == "" && !strings.HasPrefix(c.ID, reviewBodyIDPrefix) {
			return false
		}
	}
	return true
}

// markCommentsSeenByStage adds a rocket reaction to any user comments that were
// present when a stage ran. item provides owner/repo/number context for API
// calls; preStageComments must be the snapshot captured before stage dispatch
// (item.Comments at dispatch time) — it must NOT be item.Comments from a
// re-fetch, as that would include comments that arrived during the run and were
// never processed by the stage.
func (e *Engine) markCommentsSeenByStage(item gh.ProjectItem, preStageComments []gh.Comment) {
	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	repoStr := itemOwnerRepoString(item, e.defaultRepo())
	for _, c := range preStageComments {
		if strings.HasPrefix(c.Body, "🏭 **Fabrik") {
			continue
		}
		if c.HasReaction("ROCKET") {
			continue
		}
		// This comment was seen by the stage — mark it so it won't trigger unblock
		// no write-through: excluded — AddCommentReaction does not affect dispatch-relevant cache state
		if err := e.client.AddCommentReaction(owner, repo, c.DatabaseID, "rocket"); err != nil {
			e.logf(item.Number, "warn", "could not add rocket to seen comment %s: %v\n", c.ID, err)
		}
		e.store.Apply(itemstate.CommentProcessed{Repo: repoStr, Number: item.Number, CommentID: c.ID, At: time.Now()})
	}
}

// markCommentsProcessed records comments as processed so they won't be retried.
func (e *Engine) markCommentsProcessed(item gh.ProjectItem, comments []gh.Comment) {
	repoStr := itemOwnerRepoString(item, e.defaultRepo())
	for _, c := range comments {
		e.store.Apply(itemstate.CommentProcessed{Repo: repoStr, Number: item.Number, CommentID: c.ID, At: time.Now()})
	}
}
