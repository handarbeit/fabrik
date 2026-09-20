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
// GitHub login, not Fabrik's own posting identity — under PAT mode (#671)
// the two happen to be the same account, but under GitHub App auth (#1713)
// Fabrik posts under the installation's bot login (e.selfLogin(), matching
// gh.IsBotLogin's "<slug>[bot]" pattern) while cfg.User remains the
// operator's separate human login. Excluding cfg.User here would filter out
// the operator's own resume reply in either mode. Fabrik's own comments are
// excluded from both directions: gh.IsBotLogin already catches them by
// author whenever the cache correctly records e.selfLogin() as Author
// (#1754), and independently, findNewComments' 🏭 **Fabrik body-prefix
// check excludes them upstream regardless of author.
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

// pauseLabel is the label whose latest `labeled` event anchors "the moment the
// current pause began" for resumeAuthorised. isAwaitingInput implies paused, so
// this one label covers both resume gates.
const pauseLabel = "fabrik:paused"

// commentsPredatePause reports whether every comment in human strictly predates
// pausedAt — the only positive finding that may refuse a resume (ADR-1813, R6).
// Every indeterminate input resumes: a zero anchor, or any comment with a zero
// CreatedAt or one equal to the anchor (GitHub timestamps have one-second
// resolution, so equality cannot prove the comment came first).
func commentsPredatePause(human []gh.Comment, pausedAt time.Time) bool {
	if pausedAt.IsZero() || len(human) == 0 {
		return false
	}
	for _, c := range human {
		if c.CreatedAt.IsZero() || !c.CreatedAt.Before(pausedAt) {
			return false
		}
	}
	return true
}

// resumeAuthorised is the single predicate deciding whether a paused or
// awaiting-input item may be resumed by a human comment (ADR-069, ADR-1813).
// It is shared by itemNeedsWork (admission) and processItem (both resume
// gates), so the two can never disagree.
//
// A resume is authorised only by an *event* — a human comment created at or
// after the latest `fabrik:paused` labeled event — not by the *state* "an
// unprocessed human comment exists", which stays true forever when processing
// can never succeed and would let one comment lift the pause every poll.
//
// raw is always the full findNewComments set (including bot chatter), so an
// authorised resume hands the whole backlog to processComments (R5). refused
// is the number of human comments that predate the pause (0 when authorised or
// when there were no human comments), for distinct logging.
//
// The anchor is read directly from GitHub (client.FetchLabelAppliedAt), never
// through the record-on-write cache: a human removing the label in the UI
// leaves a stale early cache entry that a later pause would inherit, silently
// re-arming the loop. Any fetch error or missing event resumes (R6).
func (e *Engine) resumeAuthorised(item gh.ProjectItem) (authorised bool, raw []gh.Comment, refused int) {
	raw = e.findNewComments(item)
	human := filterHuman(raw)
	if len(human) == 0 {
		return false, raw, 0
	}
	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	pausedAt, err := e.client.FetchLabelAppliedAt(owner, repo, item.Number, pauseLabel)
	if err != nil {
		e.logf(item.Number, "resume", "pause anchor unavailable (%v) — resuming\n", err)
		return true, raw, 0
	}
	if commentsPredatePause(human, pausedAt) {
		return false, raw, len(human)
	}
	return true, raw, 0
}

// processComments handles new user comments on an issue.
// Flow: 👀 reactions → editing label → clear stale stage:<Stage>:complete (#1802) →
// invoke Claude → perform actions / update issue body → restore/re-derive
// stage:<Stage>:complete → remove editing label → 🚀 reactions
func (e *Engine) processComments(ctx context.Context, board *gh.ProjectBoard, item gh.ProjectItem, stage *stages.Stage, comments []gh.Comment, onPIDReady ...func(int)) error {
	_, err := e.processCommentsClassified(ctx, board, item, stage, comments, onPIDReady...)
	return err
}

// processCommentsClassified is processComments plus a side-channel result: the
// didNotRunKind (#1812) reporting whether this call provably never executed a
// Claude invocation. The user-comment callers keep using processComments (whose
// error contract is unchanged); dispatchReinvoke uses this variant to refund
// the reinvoke cycle counter charged before dispatch. The kind is deliberately
// NOT folded into the returned error: a usage-limit or suspension skip returns
// a nil error today (excluded from the comment circuit breakers), and changing
// that would alter what every caller and breaker sees.
//
// The kind is "" (ran, or ambiguous) at every exit other than the three that
// positively establish the invocation never ran; ambiguity is charged (R4).
func (e *Engine) processCommentsClassified(ctx context.Context, board *gh.ProjectBoard, item gh.ProjectItem, stage *stages.Stage, comments []gh.Comment, onPIDReady ...func(int)) (didNotRunKind, error) {
	owner, repo := itemOwnerRepo(item, e.defaultRepo())

	// Account-wide Claude usage-limit suspension gate (ADR-1120): checked before
	// any side effect (reactions, labels, worktree setup) so a suspended cycle is
	// a full no-op — the comment remains "new" and is retried on the next poll
	// once dispatch resumes, rather than being consumed by a doomed invocation.
	if _, suspended := e.claudeSuspendedUntilTime(time.Now()); suspended {
		e.logf(item.Number, "claude-limit", "Claude dispatch suspended account-wide; skipping comment review\n")
		return didNotRunSuspended, nil
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
		return "", nil
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
		return "", fmt.Errorf("adding editing label: %w", err)
	} else {
		e.syncLabelAdd(item, "fabrik:editing", true)
	}

	// #1802: clear stage's stale completion claim for the duration of this
	// rework, strictly inside the fabrik:editing bracket just opened above
	// (R4) — restored (or re-derived via handleStageComplete) on every exit
	// path below via endStageRework.
	wasReworking := e.beginStageRework(item, stage)

	// Step 3: Ensure worktree
	wm := e.worktreesFor(item.Repo)
	baseBranch, err := e.baseBranchForItem(item, wm)
	if err != nil {
		e.endStageRework(item, stage, wasReworking, false)
		e.removeEditingLabel(owner, repo, item.Number)
		if !e.checkNoOpCommentCycle(item, stage, false, lastCommentAuthor(comments)) {
			e.checkCommentBreaker(item, fmt.Sprintf("resolving the base branch failed: %v", err))
		}
		return "", fmt.Errorf("setting up worktree for %s/%s: %w", owner, repo, err)
	}
	// Merge-queue awareness (ADR-058 D3): skip the preemptive rebase when the PR is
	// in the queue (FR-1) or the repo is queue-enabled (FR-2). Both ProjectItem-sourced
	// signals are false-by-default, preserving legacy behavior on non-queue repos (FR-3).
	skipUpdate := prInMergeQueue(item) || e.suppressPreemptiveRebase(item)
	workDir, err := wm.EnsureWorktree(item.Number, baseBranch, skipUpdate)
	if err != nil {
		e.endStageRework(item, stage, wasReworking, false)
		e.removeEditingLabel(owner, repo, item.Number)
		if !e.checkNoOpCommentCycle(item, stage, false, lastCommentAuthor(comments)) {
			e.checkCommentBreaker(item, fmt.Sprintf("setting up the worktree failed: %v", err))
		}
		return "", fmt.Errorf("setting up worktree for %s/%s: %w", owner, repo, err)
	}

	// If a PR exists and its base branch doesn't match the resolved base, update it.
	e.syncPRBase(item, baseBranch)
	e.ensureEnvExcluded(item.Number, workDir)
	e.symlinkEnvIfEnabled(item.Number, workDir)

	// Write context files (all stages including current) before Claude runs.
	e.writeContextFiles(item, stage, workDir, true)

	// #1786: same warn-and-continue toolchain drift check as the stage-dispatch
	// path (runInvocationWithExtension) — R5 explicitly covers comment-review
	// cycles too, closing the gap the apiKeyHelper precedent left open here.
	e.checkToolchainDrift(ctx, item, workDir)

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
		e.endStageRework(item, stage, wasReworking, false)
		e.removeEditingLabel(owner, repo, item.Number)
		if ctx.Err() != nil {
			e.logf(item.Number, "skip", "cancelled during claude comment review\n")
			return "", nil
		}
		// A Claude usage-limit hit is not "no forward progress" — it's an
		// account-wide condition unrelated to this issue's comment thread, already
		// handled by activateClaudeSuspension above. Counting it toward the
		// circuit breaker would risk tripping the breaker purely because the
		// account ran dry, not because this issue is stuck.
		var limitErr *claudeUsageLimitError
		if errors.As(err, &limitErr) {
			e.logf(item.Number, "claude-limit", "claude comment review hit the account usage limit; not counted toward the comment circuit breaker\n")
			return didNotRunUsageLimit, nil
		}
		// A tool-permission-denial exit (#1523/#1704) is deterministic — a
		// "don't ask mode" denial short-circuits allowlist evaluation, so no
		// retry and no comment breaker cadence can converge it. Classify and
		// bound it here exactly like finalizeStageOutcome does for the
		// stage-dispatch path (R1), reusing the same
		// ToolsDeniedRetries/MaxToolsDeniedRetries counter (R2) so a mode
		// denial escalates on consecutive detections regardless of which
		// invocation type (ordinary comment review, ci-fix-reinvoke,
		// review-reinvoke) hit it (R3) — never on the comment breaker's
		// frequency-based thresholds, which stay unchanged (R2, Acceptance).
		// This early return bypasses checkNoOpCommentCycle/checkCommentBreaker
		// entirely for this cycle, mirroring the usage-limit exclusion above.
		var toolsDeniedErr *claudeToolsDeniedError
		if errors.As(err, &toolsDeniedErr) {
			toolsDeniedCount, willEscalate := e.recordToolsDeniedDetection(item, stage, toolsDeniedErr.ToolNames, toolsDeniedErr.Denials)
			e.logf(item.Number, "tools-denied", "comment review's tool call(s) denied by permission configuration: %s\n", toolsDeniedLogSummary(toolsDeniedErr.ToolNames, toolsDeniedErr.Denials))
			if willEscalate {
				e.pauseForToolsDeniedLimit(item, stage, toolsDeniedCount, e.cfg.MaxToolsDeniedRetries, toolsDeniedErr.ToolNames, toolsDeniedErr.Denials)
			}
			return "", err
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
		// Breaker checks above are unchanged and still count did-not-run
		// exits; only the reinvoke cycle counters are refunded (#1812).
		return classifyDidNotRun(err), err
	}
	if err != nil {
		e.logf(item.Number, "warn", "claude comment review exited with error but stage completed (marker found) — proceeding: %v\n", err)
	}

	summary := e.publishCommentOutput(owner, repo, item, stage, comments, output, workDir, baseBranch)

	e.finalizeComments(ctx, board, item, stage, comments, owner, repo, baseBranch, completed, wasReworking, summary)

	// Checked last so any reset applied above (stage-complete inside
	// finalizeComments, or an issue-body update inside publishCommentOutput)
	// takes effect before evaluating whether this cycle tripped the breaker.
	// progressed mirrors R2's three signals exactly: a commit landed
	// (headChanged), the issue body updated (extractUpdatedBody on the
	// original, unstripped output — publishCommentOutput took output by
	// value, so this copy is untouched by its marker-stripping), or the
	// stage completed (FABRIK_STAGE_COMPLETE was emitted).
	//
	// lastCommentAuthor(comments) below is intentionally computed from this
	// cycle's original comments slice, not a mid-loop-refreshed one:
	// runCommentExtensionLoop's progress check (detectProgress) may re-fetch
	// item via FetchItemDetails, but that only refreshes item's own fields —
	// comments itself is never reassigned, and every extension iteration
	// resumes Claude with this same, fixed slice. So the author attributed
	// in a trip comment always matches what was actually delivered to Claude
	// this cycle, even across an extend-turns loop.
	progressed := headChanged || extractUpdatedBody(output) != "" || completed
	if !e.checkNoOpCommentCycle(item, stage, progressed, lastCommentAuthor(comments)) {
		e.checkCommentBreaker(item, "")
	}

	return "", nil
}

// reworkingLabelPrefix is the prefix of every fabrik:reworking:<Stage> marker
// label. The stage name is encoded directly in the label — not resolved from
// the item's current board Status at recovery time — because Status can
// advance past the reworked stage before the marker is cleared (a completing
// rework that also auto-advances the board moves Status to the *next* stage
// synchronously inside handleStageComplete, before finalizeComments gets a
// chance to remove the marker). A Status-keyed recovery would then restore
// the wrong stage's completion claim; see runStartupCleanup's third pass.
const reworkingLabelPrefix = "fabrik:reworking:"

// reworkingLabelName returns the fabrik:reworking:<Stage> marker label name
// for stageName.
func reworkingLabelName(stageName string) string {
	return reworkingLabelPrefix + stageName
}

// beginStageRework clears stage's stale completion claim for the duration of a
// comment re-entry (#1802, R1): a stage being reworked must not keep
// advertising stage:<Stage>:complete for the whole rework window, since that
// misreports an actively-reworked stage as finished, distinguishable from a
// genuinely finished one only by the reader already knowing fabrik:editing
// silently overrides it.
//
// No-ops (returns false) when stage:<Stage>:complete isn't present on item —
// the common mid-flight-rework case (e.g. the stage never completed yet, or a
// prior rework already cleared it) has nothing to lie about, so nothing is
// marked or cleared.
//
// Mark-first-then-clear ordering is deliberate: fabrik:reworking:<Stage> is
// added before stage:<Stage>:complete is removed, so the marker's presence is
// always the correct, unambiguous trigger for crash recovery (R3) to restore
// the completion label — regardless of which of the two mutations actually
// landed before a crash. The reverse order would leave a crash window with no
// durable signal that a restore is owed, which is exactly the "silently
// re-run a finished stage" failure R2 says is worse than the pre-fix lie.
//
// Must only be called strictly inside the fabrik:editing bracket (R4): every
// dispatch-admission gate (itemNeedsWork) already refuses to act while
// fabrik:editing is present, so clearing/restoring stage:<Stage>:complete
// inside that window cannot change any gating decision.
//
// The fabrik:reworking:<Stage> add is error-checked (addLabelChecked), not
// best-effort: if it doesn't actually land on GitHub, stage:<Stage>:complete
// is deliberately left uncleared for this cycle (fail-open, logged) rather
// than removed anyway — removing it here without a durably-landed marker
// would strand the completion claim with no signal left for
// runStartupCleanup to recover it from, exactly the failure this function's
// whole ordering exists to prevent.
func (e *Engine) beginStageRework(item gh.ProjectItem, stage *stages.Stage) bool {
	completeLabel := "stage:" + stage.Name + ":complete"
	if !hasLabel(item.Labels, completeLabel) {
		return false
	}
	reworkingLabel := reworkingLabelName(stage.Name)
	if err := e.addLabelChecked(item, reworkingLabel); err != nil {
		e.logf(item.Number, "warn", "could not add %s marker: %v — leaving %q in place for this cycle\n", reworkingLabel, err, completeLabel)
		return false
	}
	e.removeLabel(item, completeLabel)
	return true
}

// endStageRework is beginStageRework's exit-path counterpart, called at every
// non-completing and completing exit of a comment re-entry that began a
// rework window (R2). No-ops when wasReworking is false — nothing was ever
// marked or cleared, so nothing needs restoring.
//
// When completedThisCycle is true, stage:<Stage>:complete is deliberately NOT
// re-added directly here — the re-entry itself re-signaled completion, so
// finalizeComments' call to handleStageComplete (unmodified) re-derives the
// correct label from scratch, including its wait_for_ci deferral. Restoring
// it here too would just be redundant (AddLabelToIssue is idempotent) but
// duplicates the single source of truth for "what does complete mean" in two
// places, so it's deliberately left to that flow alone. Only
// fabrik:reworking:<Stage> is removed on this path — and the caller
// (finalizeComments) MUST NOT call this until after handleStageComplete has
// already run: removing the marker any earlier would leave a crash window
// with no durable signal that a restore is owed, between a rework's
// completion and the label write that records it. See that call site and
// ADR-1802.
//
// When completedThisCycle is false, stage:<Stage>:complete is restored
// directly — the rework didn't re-signal completion this cycle (error,
// blocked-on-input, tools-denied, setup failure, or exhausted turns), and no
// other flow will restore it, so a direct re-add is the only way the
// stage's completion claim survives the cycle.
//
// fabrik:reworking:<Stage> is always removed last (mirroring
// beginStageRework's mark-first-then-clear invariant in reverse): the
// completion label lands (or is deliberately deferred to handleStageComplete)
// before the marker that says "a restore is owed" is cleared, so a crash
// between the two still leaves the correct, safe-to-retry signal for startup
// recovery.
//
// The restore add is error-checked (addLabelChecked), not best-effort: if
// stage:<Stage>:complete doesn't actually land on GitHub,
// fabrik:reworking:<Stage> is deliberately left in place (logged) rather than
// removed anyway — removing the marker here without the restore having
// landed would silently lose both the completion state and the only
// remaining signal that a restore is still owed, with no runStartupCleanup
// path left to recover it (this function only runs live, mid-session — the
// marker's continued presence is what lets a later startup pass retry it).
func (e *Engine) endStageRework(item gh.ProjectItem, stage *stages.Stage, wasReworking, completedThisCycle bool) {
	if !wasReworking {
		return
	}
	if !completedThisCycle {
		completeLabel := "stage:" + stage.Name + ":complete"
		if err := e.addLabelChecked(item, completeLabel); err != nil {
			e.logf(item.Number, "warn", "could not restore %q: %v — leaving %s in place for recovery\n", completeLabel, err, reworkingLabelName(stage.Name))
			return
		}
	}
	e.removeReworkingLabelRetrying(item, reworkingLabelName(stage.Name))
}

// removeReworkingLabelRetrying removes the fabrik:reworking:<Stage> marker
// named by label with the same bounded retry-with-backoff as
// removeEditingLabel: a single best-effort attempt would leave the marker
// dangling live (visible until the next runStartupCleanup pass, i.e. the next
// process restart) whenever the final RemoveLabelFromIssue call hits a
// transient error — reintroducing, for the marker itself, exactly the class
// of stale/misleading label this whole mechanism exists to eliminate. By the
// time this runs, stage:<Stage>:complete (or fabrik:awaiting-ci) has already
// durably landed, so nothing is at risk beyond the marker's own cosmetic
// staleness — but it's cheap to close.
func (e *Engine) removeReworkingLabelRetrying(item gh.ProjectItem, label string) {
	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	const maxAttempts = 3
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		err := e.client.RemoveLabelFromIssue(owner, repo, item.Number, label)
		if err == nil {
			e.syncLabelRemoval(item, label, true)
			return
		}
		if errors.Is(err, gh.ErrNotFound) {
			e.syncLabelRemoval(item, label, false)
			return
		}
		if !isTransientError(err) {
			e.logf(item.Number, "warn", "could not remove %s marker: %v\n", label, err)
			return
		}
		lastErr = err
		if attempt < maxAttempts-1 {
			delay := editingLabelRetryDelay << attempt
			time.Sleep(delay)
		}
	}
	e.logf(item.Number, "warn", "could not remove %s marker after %d attempts: %v\n", label, maxAttempts, lastErr)
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

// finalizeComments restores or re-derives the rework-cleared stage:<Stage>:complete
// label (#1802, R2 — see endStageRework), removes the editing label, reacts
// with 🚀 to all processed comments (resolving any addressed review threads),
// marks the comments as processed so they won't be retried, and — if comment
// processing resolved the stage — creates/marks-ready the draft PR and
// advances to the next stage. This avoids an unnecessary extra stage
// invocation after unblocking.
func (e *Engine) finalizeComments(ctx context.Context, board *gh.ProjectBoard, item gh.ProjectItem, stage *stages.Stage, comments []gh.Comment, owner, repo, baseBranch string, completed, wasReworking bool, summary string) {
	// On a non-completing exit there is nothing further in this function that
	// will restore stage:<Stage>:complete, so do it immediately. On a
	// completing exit, restoring fabrik:reworking's marker removal is
	// deferred until after handleStageComplete below has made its durable
	// completion decision (#1802) — see that call site for why.
	if !completed {
		e.endStageRework(item, stage, wasReworking, false)
	}
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
		// Only now — after handleStageComplete has durably written
		// stage:<Stage>:complete, deferred to fabrik:awaiting-ci, or (a
		// Validate yolo-merge failure) written neither, all outcomes the
		// normal, non-rework dispatch path also produces — is it safe to drop
		// the fabrik:reworking marker. Removing it any earlier (as a
		// single call at the top of this function, alongside the
		// non-completing branch above) would leave a crash window, spanning
		// every network call between here and there, where the rework's
		// stale-completion-claim marker is already gone but no completion
		// signal has landed yet: exactly the "silently re-run a finished
		// stage" failure R2 says is worse than the pre-fix lie. See ADR-1802
		// and the matching crash-recovery check in
		// runStartupCleanup (engine/worker_liveness.go).
		e.endStageRework(item, stage, wasReworking, true)
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
