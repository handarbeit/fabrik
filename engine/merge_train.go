package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/handarbeit/fabrik/boardcache"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

// TrainCIResult is the typed outcome of the combined-Validate CI poll.
type TrainCIResult int

const (
	TrainCIPending TrainCIResult = iota // CI not yet resolved (timeout or still running)
	TrainCIGreen                        // all required checks passed
	TrainCIRed                          // at least one required check failed
)

// trainMember pairs a batch member's ProjectItem with its linked PR number and head SHA.
// Both are fetched once (fetchTrainMembers) and reused across every bisection trial and
// the landing step to avoid extra API calls.
type trainMember struct {
	item    gh.ProjectItem
	prNum   int
	headSHA string
}

// trainCIDiagnostic captures the combined Validate's failure output at the point of
// failure (R1/#1420), so it survives the bisection loop and is still in hand when the
// ejection comment is composed — the only run in which a merge-train failure exists is
// the combined trial that observed it (the branch's own CI is green by construction).
//
// Threaded as a return value through pollTrainCI -> assembleAndValidate ->
// bisect/handleRedBatch/landOneAtATime -> ejectMember, never stashed in shared or
// mutable state: bisection continues after isolating the poisoner (to validate the
// reformed survivor batch), and that later run is unrelated and must not overwrite the
// diagnostic that named the poisoner. A threaded return value makes that overwrite
// structurally impossible — there is no shared field a later call could clobber.
//
// Exactly one of FailedChecks, FailedContexts, or Note is populated, reflecting which
// branch of pollTrainCI produced the red result: ordinary check-run failures carry full
// CheckRun data (name, output text/summary, details/html URL); classic commit-status
// "required context" failures (ADR-933) carry only names, since there is no check-run
// output to extract; a "dirty" mergeable_state (no per-check signal at all) carries a
// free-text Note. nil means "no CI diagnostic available" — used at the three ejection
// call sites whose cause isn't a combined-Validate failure (fetch/head-SHA failures,
// unresolvable merge conflicts), which this issue leaves unaffected.
type trainCIDiagnostic struct {
	FailedChecks   []gh.CheckRun
	FailedContexts []string
	Note           string
	PRNum          int
	TrialSHA       string
}

// Truncation policy for rendering a trainCIDiagnostic into a comment body (R3): inline a
// failing check's output in full up to trainDiagPerCheckInlineMax chars; beyond that,
// inline trainDiagPerCheckHead chars from the start and trainDiagPerCheckTail from the
// end with an explicit "chars omitted" marker. At most trainDiagMaxInlineChecks failing
// checks get their output inlined; any remaining failing checks are named only. A final
// hard cap (trainDiagBlockMax) truncates the whole assembled block as a belt-and-suspenders
// against GitHub's ~65536-char comment limit, mirroring the tail-only idiom
// formatOutputComment/formatReviewFeedbackComment already use in engine/pr.go.
const (
	trainDiagPerCheckInlineMax = 3000
	trainDiagPerCheckHead      = 2000
	trainDiagPerCheckTail      = 800
	trainDiagMaxInlineChecks   = 5
	trainDiagBlockMax          = 15000
)

// truncateMiddle returns s unchanged if it fits within max chars; otherwise it keeps the
// first head chars and last tail chars, replacing the middle with an explicit
// "chars omitted" marker so a reader knows content was cut rather than mistaking the
// excerpt for the whole thing.
func truncateMiddle(s string, max, head, tail int) string {
	if len(s) <= max {
		return s
	}
	omitted := len(s) - head - tail
	headEnd := head
	for headEnd > 0 && headEnd < len(s) && !utf8.RuneStart(s[headEnd]) {
		headEnd--
	}
	tailStart := len(s) - tail
	for tailStart < len(s) && !utf8.RuneStart(s[tailStart]) {
		tailStart++
	}
	return fmt.Sprintf("%s\n… (%d chars omitted) …\n%s", s[:headEnd], omitted, s[tailStart:])
}

// renderFailedChecks renders the failing check-run portion of a diagnostic block (R1/R3):
// each check's name, status/conclusion, a truncated excerpt of its output (OutputText,
// falling back to OutputSummary), and a Details link when GitHub provided one — always,
// not only when truncated, since it's strictly more helpful. Beyond trainDiagMaxInlineChecks,
// remaining failing checks are named only, so a wide red batch never balloons the comment.
func renderFailedChecks(checks []gh.CheckRun) string {
	if len(checks) == 0 {
		return ""
	}
	inlineCount := len(checks)
	if inlineCount > trainDiagMaxInlineChecks {
		inlineCount = trainDiagMaxInlineChecks
	}
	var b strings.Builder
	for i, cr := range checks[:inlineCount] {
		if i > 0 {
			b.WriteString("\n\n")
		}
		state := cr.Status
		if cr.Status == "completed" {
			state = cr.Conclusion
		}
		fmt.Fprintf(&b, "**%s** (%s)", cr.Name, state)
		output := strings.TrimSpace(cr.OutputText)
		if output == "" {
			output = strings.TrimSpace(cr.OutputSummary)
		}
		if output != "" {
			b.WriteString("\n```\n")
			b.WriteString(truncateMiddle(output, trainDiagPerCheckInlineMax, trainDiagPerCheckHead, trainDiagPerCheckTail))
			b.WriteString("\n```")
		}
		link := cr.HTMLURL
		if link == "" {
			link = cr.DetailsURL
		}
		if link != "" {
			fmt.Fprintf(&b, "\nDetails: %s", link)
		}
	}
	if len(checks) > inlineCount {
		var rest []string
		for _, cr := range checks[inlineCount:] {
			rest = append(rest, cr.Name)
		}
		fmt.Fprintf(&b, "\n\n...and %d more failing check(s): %s", len(rest), strings.Join(rest, ", "))
	}
	return b.String()
}

// renderFailedContexts renders the classic-commit-status portion of a diagnostic block
// (ADR-933's RequiredContextsFailed path) — names only, since a required context has no
// check-run output to extract from. A pointer degraded to "name only" is still strictly
// more than the "no diagnostic" this issue reports (R3's minimum-acceptable bar).
func renderFailedContexts(contexts []string) string {
	if len(contexts) == 0 {
		return ""
	}
	return fmt.Sprintf("Failed required status context(s): %s\n(no check-run output is available for classic commit statuses)", strings.Join(contexts, ", "))
}

// renderDiagnosticBlock composes the full R1/R3 diagnostic section of an ejection or
// pause comment from diag, applying the final hard-cap truncation. Returns "" for a nil
// diag (the three out-of-scope ejection call sites) or a diag whose fields are all empty.
func renderDiagnosticBlock(diag *trainCIDiagnostic) string {
	if diag == nil {
		return ""
	}
	var body string
	switch {
	case len(diag.FailedChecks) > 0:
		body = renderFailedChecks(diag.FailedChecks)
	case len(diag.FailedContexts) > 0:
		body = renderFailedContexts(diag.FailedContexts)
	case diag.Note != "":
		body = diag.Note
	default:
		return ""
	}
	block := fmt.Sprintf("**Diagnostic** (trial %s, integration PR #%d):\n\n%s", diag.TrialSHA, diag.PRNum, body)
	if len(block) > trainDiagBlockMax {
		block = truncateBlockHard(block, trainDiagBlockMax) + "\n\n... (truncated)"
	}
	return block
}

// truncateBlockHard cuts s to at most max bytes, at a boundary that never splits a
// multi-byte UTF-8 rune and never leaves a dangling, unterminated ``` code fence open —
// renderFailedChecks wraps each check's output in its own fence, and cutting a block
// mid-fence would render every section after the cut (the batch-context sentence, the
// "remains in Queued" boilerplate) as part of an open code block instead of prose.
func truncateBlockHard(s string, max int) string {
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	s = s[:cut]
	if strings.Count(s, "```")%2 != 0 {
		s += "\n```"
	}
	return s
}

// renderBatchContext composes the R4 sentence naming the other members the isolated
// member's batch was combined against — informational grounding so an operator knows
// before investigating that the fault does not exist on their own branch (a merge-train
// failure is, by construction, a failure that doesn't exist on the branch alone; it
// arises from combining with a base that moved). otherMembers is the full batch at the
// point the failure was observed (handleRedBatch's top-level red set for a
// bisection-isolated poisoner; nil for landOneAtATime's fallback, which validates each
// member as a true singleton with no batch at all). isolated is excluded from the named
// list, and an empty remainder (a single-member train, or a genuine singleton
// validation) gets a distinct, coherent sentence rather than an awkward empty list.
func renderBatchContext(otherMembers []trainMember, isolated int) string {
	var names []string
	for _, m := range otherMembers {
		if m.item.Number == isolated {
			continue
		}
		names = append(names, fmt.Sprintf("#%d", m.item.Number))
	}
	if len(names) == 0 {
		return "No other members were present in this train attempt — the failure is against the moved base branch alone, not a cross-PR interaction."
	}
	return fmt.Sprintf("This train attempt also combined the following member(s), which are not implicated — the failure does not exist on their branches either: %s.", strings.Join(names, ", "))
}

// mergeTrainWorkerState tracks an in-flight or completed merge-train worker.
// Stored in Engine.mergeTrainInFlight keyed by trainKey ("owner/repo:baseBranch",
// mergeTrainKey — since #1648, one entry per independently-dispatched (repo,base)
// partition, not one per repo).
// mu guards all fields that the poll loop reads while the goroutine writes them.
type mergeTrainWorkerState struct {
	mu         sync.RWMutex
	assembling bool          // true while building the trial branch; false once PR is open
	bisecting  bool          // true while halving a red batch to isolate the poisoner (ADR-059 D4)
	prNum      int           // draft CI PR number (set after draft PR is created)
	CIResult   TrainCIResult // final CI result (set by pollTrainCI on exit)
	trialName  string        // trial name of the most recent trial (churns during bisection)
	projectID  string        // board project ID for advanceToNextStage (immutable after dispatch)

	// batchNumbers is the set of issue numbers this worker was dispatched with — the
	// capBatch(trainItems, effectiveMaxBatchSize())-truncated batch, set once in
	// dispatchMergeTrainWorker and never mutated afterward (like projectID). Worker
	// membership only ever shrinks from here (ejection/landing), never grows, so this
	// is a safe upper bound on "issue numbers this worker's checkpoints could ever
	// touch." settleQueuedReviewFindings (#1208) reads it via mergeTrainBatchMembers
	// to tell a Queued member genuinely inside the live batch (must use the
	// pending-eject signal — the worker owns its state) apart from one merely Queued
	// in the same repo but excluded by the batch cap (safe to eject directly — the
	// worker never looks at it, exactly like the no-worker-in-flight case).
	batchNumbers map[int]bool
}

// sanitizeBranchName replaces characters that are invalid in directory names
// (particularly '/') so trialName can be used as both a directory segment and
// as the suffix of the trial branch name.
func sanitizeBranchName(s string) string {
	return strings.ReplaceAll(s, "/", "-")
}

// defaultPartitionBase is the sentinel "base" value groupQueuedByRepoAndBase
// assigns to a Queued member with no base: label (#1648) — the common-case
// bucket, deliberately never resolved via wm.DefaultBaseBranch()/git at
// grouping time (see that function's and trialParams's doc comments for why).
// Every code path that reconstructs a trainKey for a no-label item (the
// runaway-alert settle scan's mergeTrainKeyForItem, most notably) must use
// this same sentinel, or its reconstructed key will silently disagree with
// the one the item was actually dispatched/paused under.
const defaultPartitionBase = ""

// mergeTrainKey returns the composite key identifying one merge-train
// partition: one independent train per (repo, resolved base branch), per
// #1648. repoKey is the usual "owner/repo" string every call site already
// computes. baseBranch is defaultPartitionBase ("") for the common default-base
// partition, or the real resolved branch name otherwise — see trialParams's doc
// comment in this file for the full partitionBase-vs-baseBranch distinction.
//
// For the default partition (baseBranch == defaultPartitionBase) this returns
// repoKey unchanged — no colon appended — rather than the theoretically-more-
// "consistent" "owner/repo:". This is deliberate, not an oversight: it makes
// the default partition's key byte-identical to the pre-#1648 bare-repoKey
// form everywhere that key is also used in a human-facing log line or alert
// comment (AC4), avoiding a confusing trailing "owner/repo:" that names no
// base at all. It's also collision-safe by construction: a real resolved
// branch name from baseBranchForItem is never empty, so "owner/repo" (no
// colon) can only ever mean the default partition, never collide with an
// explicit "owner/repo:somebranch" key.
//
// For every other baseBranch, the delimiter is ':' — illegal in both a GitHub
// "owner/repo" string and in any valid git ref name (git check-ref-format
// forbids a bare ':'), so the two components can never collide by
// construction; no sanitizeBranchName-style escaping of the key itself is
// needed. This key replaces the bare "owner/repo" string as the guard key for
// mergeTrainInFlight, the runaway guard's mergeTrainTrials/mergeTrainRunawayAlerted,
// store.repoWorkers (EnterRepoWorker/ExitRepoWorker/RepoWorkerActive), and
// mergeTrainBatchSnapshotSeen — every registry that must not let one base's
// train block, cancel, alert on, or be mistaken for another base's train in
// the same repo (R2). Registries that are deliberately NOT re-keyed
// (queuedReviewEjects, mergeTrainCloneSkipCounts, mergeTrainEjectionCounts)
// keep using the bare "owner/repo" (or "owner/repo#N") form — see their own
// doc comments for why.
func mergeTrainKey(repoKey, baseBranch string) string {
	if baseBranch == defaultPartitionBase {
		return repoKey
	}
	return repoKey + ":" + baseBranch
}

// trialBelongsToBase reports whether headRef — a "fabrik/merge-train/<trialName>"
// branch or PR head ref — was formed for baseBranch, by checking the base
// segment baseTrialName already embeds in every trial name
// ("merge-train-<sanitizeBranchName(baseBranch)>-<unix-ts>[-t<n>]"). Before #1648
// at most one merge-train worker could ever be live per repo, so
// reconstructTrainState's stale-open-PR-close and orphan-branch-sweep paths could
// safely treat any unrecognized "fabrik/merge-train/*" artifact as belonging to no
// live batch and therefore safe to close/delete. With concurrent per-base workers
// now a normal occurrence, those two paths must additionally confirm an unmatched
// artifact isn't simply a sibling partition's live trial before destroying it —
// this is the check that closes that gap (see ADR-1648). headRef that isn't a
// merge-train branch at all (trialNameFromBranch returns "") never belongs to
// any base.
func trialBelongsToBase(headRef, baseBranch string) bool {
	trialName := trialNameFromBranch(headRef)
	if trialName == "" {
		return false
	}
	want := "merge-train-" + sanitizeBranchName(baseBranch) + "-"
	return strings.HasPrefix(trialName, want)
}

// ceilLog2 returns ⌈log₂(n)⌉ for n ≥ 1 and 0 for n ≤ 1. It is the number of
// halving levels needed to bisect a set of n members down to a singleton, and
// underpins the default bisection cost cap (ADR-059 D-f).
func ceilLog2(n int) int {
	if n <= 1 {
		return 0
	}
	bits := 0
	for v := n - 1; v > 0; v >>= 1 {
		bits++
	}
	return bits
}

// effectiveMaxBatchSize returns the configured MaxBatchSize, defaulting to 5
// (ADR-059 D-f) when unset (≤ 0). This caps how many Queued items are snapshotted
// into a single merge-train batch (FR-4).
func (e *Engine) effectiveMaxBatchSize() int {
	if e.cfg.MaxBatchSize <= 0 {
		return 5
	}
	return e.cfg.MaxBatchSize
}

// effectiveBisectCap returns the maximum number of combined validations permitted
// per red-batch episode (the initial red validation plus all bisection trial
// validations), defaulting to 2·⌈log₂(max_batch_size)⌉ + 1 (ADR-059 D-f) when unset
// (≤ 0). Beyond this cap, a red batch degrades to the one-at-a-time fallback (FR-5).
// The default is derived from the configured max_batch_size, not the actual batch
// length, per FR-5.
func (e *Engine) effectiveBisectCap() int {
	if e.cfg.MaxBisectValidations > 0 {
		return e.cfg.MaxBisectValidations
	}
	return 2*ceilLog2(e.effectiveMaxBatchSize()) + 1
}

// effectiveMaxTrainRebaseCycles returns the maximum number of main-moved
// rebase+revalidate cycles permitted per merge-train batch, defaulting to 3
// (mirroring the per-issue MaxRebaseCycles default) when unset (≤ 0). Beyond
// this bound, a batch that keeps falling behind its base is dissolved back to
// Queued (ADR-059 D5, FR-2/FR-5).
func (e *Engine) effectiveMaxTrainRebaseCycles() int {
	if e.cfg.MaxTrainRebaseCycles <= 0 {
		return 3
	}
	return e.cfg.MaxTrainRebaseCycles
}

// capBatch returns the first max items of the batch, preserving entry order
// (ADR-059 D2 / FR-4). max ≤ 0 means no cap. Capping to the first N bounds the
// worst-case bisection cost if the batch turns out red.
func capBatch(items []gh.ProjectItem, max int) []gh.ProjectItem {
	if max <= 0 || len(items) <= max {
		return items
	}
	return items[:max]
}

// dispatchMergeTrainWorker checks whether a train worker is already in-flight for
// the batch's (repo, base) partition and, if not, starts one. Safe to call from
// the poll goroutine. projectID is the GitHub project board ID, threaded so
// landMergeTrainBatch can call advanceToNextStage without fetching the board
// again. partitionBase is this batch's partition-grouping key (#1648 R1),
// supplied by routeQueuedGroup — the empty string for the default-base partition
// (a deliberate zero-git-touch sentinel, never resolved via wm.DefaultBaseBranch()
// at grouping time — see trialParams's doc comment for why), or the real resolved
// branch name for a base:<branch> partition. The worker never re-resolves this
// value independently, so it can never disagree with the partition that
// dispatched it; prepareTrainWorker resolves the real git branch name from it.
func (e *Engine) dispatchMergeTrainWorker(ctx context.Context, batch []gh.ProjectItem, projectID string, partitionBase string) {
	if len(batch) == 0 {
		return
	}
	owner, repo := itemOwnerRepo(batch[0], e.defaultRepo())
	repoKey := owner + "/" + repo
	trainKey := mergeTrainKey(repoKey, partitionBase)

	batchNumbers := make(map[int]bool, len(batch))
	for _, item := range batch {
		batchNumbers[item.Number] = true
	}

	// Use LoadOrStore so the check-and-register is atomic: two concurrent callers
	// can never both pass the "not loaded" path and launch duplicate workers.
	// Keyed on trainKey (repo, base), not bare repoKey (#1648 R2): two different
	// bases in the same repo must be able to have independent in-flight workers.
	candidate := &mergeTrainWorkerState{assembling: true, projectID: projectID, batchNumbers: batchNumbers}
	existing, loaded := e.mergeTrainInFlight.LoadOrStore(trainKey, candidate)
	if loaded {
		state := existing.(*mergeTrainWorkerState)
		state.mu.RLock()
		assembling := state.assembling
		bisecting := state.bisecting
		prNum := state.prNum
		ciResult := state.CIResult
		state.mu.RUnlock()
		switch {
		case assembling:
			e.logf(0, "merge-train", "train worker already assembling for %s — skipping\n", trainKey)
		case bisecting:
			e.logf(0, "merge-train", "train worker bisecting red batch for %s — skipping\n", trainKey)
		default:
			switch ciResult {
			case TrainCIGreen:
				e.logf(0, "merge-train", "train CI green for %s (PR #%d) — awaiting landing step\n", trainKey, prNum)
			case TrainCIRed:
				e.logf(0, "merge-train", "train CI red for %s (PR #%d) — needs attention\n", trainKey, prNum)
			default:
				e.logf(0, "merge-train", "train CI pending for %s (PR #%d) — still polling\n", trainKey, prNum)
			}
		}
		return
	}

	// candidate was atomically stored — mark it live in the single liveness
	// registry the idle guard and mergeTrainWorkerActiveForRepo both read (FR-2), then
	// launch the worker. Doing this synchronously, before the goroutine even
	// starts, also closes the same-cycle gap: dispatchCandidates never counts
	// merge-train dispatch in `dispatched`, so without this the very poll cycle
	// that just launched this worker would still see zero in-flight workers.
	e.store.EnterRepoWorker(trainKey)

	state := candidate
	e.wg.Add(1)

	go func() {
		defer e.wg.Done()
		e.runMergeTrainWorker(ctx, state, owner, repo, partitionBase, batch)
	}()
}

// trialParams bundles the immutable per-worker context threaded through the
// assemble / bisect / land helpers so their signatures stay manageable. baseSHA is
// pinned once at batch start (ADR-059 D-b) and is only re-pinned deliberately, per
// singleton, inside landOneAtATime (the sequential-land base advance).
//
// baseBranch vs. trainKey (#1648): baseBranch is always the real, git-resolved
// target branch name (e.g. "main", "maint/1.x") — used for every git operation
// (SHA pinning, trial naming, PR targeting, trialBehind comparisons). trainKey is
// the composite (repo,partition) guard key this worker was dispatched under
// (mergeTrainKey(repoKey, partitionBase)) and is fixed for the worker's entire
// lifetime — used for every guard/counter operation (mergeTrainInFlight,
// mergeTrainTrials/isRunawayTripped/recordTrial/resetTrialCounter,
// mergeTrainRunawayAlerted via fireRunawayGuard). The two deliberately diverge for
// the common default-base case: partitionBase (and therefore the "base" segment of
// trainKey) is the empty-string sentinel for a Queued member with no base: label —
// this is what keeps grouping/dispatch a zero-git-touch, zero-cost path for the
// overwhelming common case (R5) — while baseBranch is only resolved from
// wm.DefaultBaseBranch() once prepareTrainWorker actually needs a real branch name
// to do git work. Every guard/counter site MUST key on trainKey, never on
// mergeTrainKey(repoKey, baseBranch) — the latter would silently disagree with the
// key dispatchMergeTrainWorker registered under for any default-base partition.
type trialParams struct {
	owner, repo      string
	baseBranch       string
	trainKey         string
	baseSHA          string
	wm               *WorktreeManager
	holdingStg       *stages.Stage
	maxTurnsOverride int
	nextTrialName    func() string // returns a unique trial name per call (first == base)
}

// finishTrain clears the per-repo in-flight marker, in both the duplicate-launch
// claim registry (mergeTrainInFlight) and the liveness registry (itemstate.Store)
// the idle guard and mergeTrainWorkerActive read. It is the single, centralized
// point through which either marker is ever cleared — sync.Map.Delete and
// Store.ExitRepoWorker are both safe no-ops on an absent key, so the two callers
// (prepareTrainWorker's own-failure defer and runMergeTrainWorker's top-level
// defer) never need to coordinate. Any new early-return path added to the
// runMergeTrainWorker call graph must rely on one of those two defers rather
// than clearing either marker directly — see ADR-067.
func (e *Engine) finishTrain(trainKey string) {
	e.mergeTrainInFlight.Delete(trainKey)
	e.store.ExitRepoWorker(trainKey)
}

// mergeTrainWorkerActiveForRepo reports whether a merge-train worker is
// currently in flight for repoKey ("owner/repo"), for ANY of its base-branch
// partitions — from dispatchMergeTrainWorker's EnterRepoWorker call through
// the goroutine's exit, when finishTrain clears the marker (see ADR-067).
// This is broader than "assembling": it also covers bisecting and the
// post-CI landing window, since the marker isn't cleared until the worker
// goroutine fully exits. Used by settleClosedItemsToDone to avoid racing a
// live batch member that was closed without merging — that caller only knows
// the item's repo, not which base partition it belongs to, so this is
// deliberately a repo-wide "is anything live" answer (RepoWorkerActiveForAnyBase's
// prefix scan), not an exact trainKey lookup. Renamed from mergeTrainWorkerActive
// (#1648): since a repo can now have several concurrent per-base workers, the old
// name's implied "the one worker for this repo" no longer holds.
func (e *Engine) mergeTrainWorkerActiveForRepo(repoKey string) bool {
	return e.store.RepoWorkerActiveForAnyBase(repoKey)
}

// mergeTrainBatchMembers returns the dispatched-batch issue-number set of the
// in-flight worker for trainKey (its immutable batchNumbers, see
// mergeTrainWorkerState's doc comment), or (nil, false) if no worker is currently
// registered for trainKey in mergeTrainInFlight. Used by settleQueuedReviewFindings
// (#1208) to distinguish a Queued member the live worker actually owns from one
// merely Queued in the same partition but excluded by the batch cap (effectiveMaxBatchSize)
// — the latter is safe to eject directly even while a worker is active for the
// partition, since the worker never looks at it. A nil/false result (no worker
// registered, e.g. a narrow race with the worker's own exit) is treated by the
// caller as "not owned by any live batch," which is always safe to eject directly.
// Since #1648 trainKey is a per-(repo,base) composite key (mergeTrainKey), not a
// bare "owner/repo" — an issue belongs to exactly one partition's live batch at a
// time, so an exact-key lookup here (rather than a repo-wide scan) is correct.
func (e *Engine) mergeTrainBatchMembers(trainKey string) (map[int]bool, bool) {
	v, ok := e.mergeTrainInFlight.Load(trainKey)
	if !ok {
		return nil, false
	}
	return v.(*mergeTrainWorkerState).batchNumbers, true
}

// mergeTrainMaxTurnsOverride computes the fabrik:extend-turns pre-grant for
// resolveConflictWithClaude's conflict-resolution invocation, which routes through
// InvokeForComments (InvokeClaudeForComments). That function's runClaude wall-time
// scaling (scaledWallTime, engine/claude.go) divides by commentMaxTurns(stage), so the
// override here must be based on the same commentMaxTurns(holdingStg) — not
// holdingStg.MaxTurns — or the two bases disagree and scaledWallTime computes the wrong
// multiplier (e.g. 4x instead of the intended 2x whenever comment_max_turns differs from
// max_turns, as every stage in this repo's own config does). See #1472.
func mergeTrainMaxTurnsOverride(holdingStg *stages.Stage, extendTurns bool) int {
	if !extendTurns {
		return 0
	}
	base := commentMaxTurns(holdingStg)
	if base <= 0 {
		return 0
	}
	return base * 2
}

// prepareTrainWorker performs all one-time setup for a merge-train worker: semaphore
// acquisition, repo readiness, holding-stage lookup, extend-turns computation,
// trialParams construction, restart-time state reconstruction (ADR-059 D5,
// FR-1/FR-4), base-SHA pinning, and member resolution. partitionBase is this
// worker's partition-grouping key (#1648 R1) — the empty-string sentinel for the
// default-base partition (never resolved via git at grouping time, so grouping
// stays zero-cost for the common case — see trialParams's doc comment), or the
// real resolved branch name for a base:<branch> partition. trainKey
// (mergeTrainKey(repoKey, partitionBase)) is fixed here for the worker's entire
// lifetime and is what every guard/counter operation below and in every nested
// helper must key on — never the real git branch name resolved a few lines down.
//
// On success (ok=true) it returns the assembled trialParams and members with the
// semaphore still held — the caller (runMergeTrainWorker) owns releasing it and
// clearing the in-flight marker for the remainder of the worker's lifetime.
//
// On failure (ok=false) it has already released the semaphore (if acquired) and
// cleared the in-flight marker via finishTrain; the caller must simply return.
func (e *Engine) prepareTrainWorker(ctx context.Context, state *mergeTrainWorkerState, owner, repo, partitionBase string, batch []gh.ProjectItem) (p trialParams, members []trainMember, ok bool) {
	repoKey := owner + "/" + repo
	trainKey := mergeTrainKey(repoKey, partitionBase)

	select {
	case e.sem <- struct{}{}:
	case <-ctx.Done():
		e.logf(0, "merge-train", "context cancelled before semaphore acquired for %s\n", trainKey)
		e.finishTrain(trainKey)
		return trialParams{}, nil, false
	}

	// The semaphore is now held. Every early-return below must release it, since
	// only a successful (ok=true) return transfers semaphore ownership to the
	// caller. Collapsing this into one deferred cleanup means a future early
	// return added here can't forget to release the semaphore or clear the marker.
	defer func() {
		if !ok {
			<-e.sem
			e.finishTrain(trainKey)
		}
	}()

	// Use batch[0] as the repo anchor for ensureRepoReady. Repo readiness (the
	// bare clone) is shared across every base partition of this repo, so this
	// stays keyed on repoKey, not trainKey (see mergeTrainCloneSkipCounts's doc
	// comment on engine.go).
	if err := e.ensureRepoReady(ctx, batch[0]); err != nil {
		if errors.Is(err, ErrSkipItem) {
			e.recordMergeTrainCloneSkip(repoKey, batch[0])
		} else {
			e.logf(0, "merge-train", "ensureRepoReady failed for %s: %v\n", repoKey, err)
		}
		return trialParams{}, nil, false
	}
	e.resetMergeTrainCloneSkip(repoKey)

	wm := e.worktreesFor(repoKey)

	// Resolve the real git branch name now — the one and only place this happens
	// for the default-base sentinel ("" partitionBase), mirroring exactly the
	// unconditional wm.DefaultBaseBranch() call this function made before #1648
	// (byte-identical timing/cost for AC4). A non-default partition already
	// carries its real resolved name (groupQueuedByRepoAndBase resolved it once,
	// via baseBranchForItem, when forming the partition), so it's used as-is.
	baseBranch := partitionBase
	if baseBranch == "" {
		var err error
		baseBranch, err = wm.DefaultBaseBranch()
		if err != nil {
			e.logf(0, "merge-train", "cannot determine base branch for %s: %v\n", repoKey, err)
			return trialParams{}, nil, false
		}
	}

	holdingStg := holdingStage(e.cfg)
	if holdingStg == nil {
		e.logf(0, "merge-train", "no holding stage configured — aborting train\n")
		return trialParams{}, nil, false
	}

	// Check if any batch member has fabrik:extend-turns — if so, double max_turns.
	extendTurns := false
	for _, m := range batch {
		if hasLabel(m.Labels, "fabrik:extend-turns") {
			extendTurns = true
			break
		}
	}
	maxTurnsOverride := mergeTrainMaxTurnsOverride(holdingStg, extendTurns)

	// Unique, monotonic trial-name generator (first call == base name). Every trial —
	// main-loop re-forms and bisection sub-trials — gets a distinct name so their branches,
	// worktrees, and draft CI PRs never collide.
	baseTrialName := fmt.Sprintf("merge-train-%s-%d", sanitizeBranchName(baseBranch), time.Now().Unix())
	trialSeq := 0
	nextTrialName := func() string {
		n := baseTrialName
		if trialSeq > 0 {
			n = fmt.Sprintf("%s-t%d", baseTrialName, trialSeq)
		}
		trialSeq++
		return n
	}

	p = trialParams{
		owner:            owner,
		repo:             repo,
		baseBranch:       baseBranch,
		trainKey:         trainKey,
		wm:               wm,
		holdingStg:       holdingStg,
		maxTurnsOverride: maxTurnsOverride,
		nextTrialName:    nextTrialName,
	}

	// FR-1/FR-4: reconstruct durable in-flight state before forming a fresh batch, so
	// a restart with an empty in-memory map resumes / completes / dissolves an existing
	// train instead of starting a duplicate. Reads only durable artifacts.
	if e.reconstructTrainState(ctx, state, p, batch) {
		return trialParams{}, nil, false
	}

	// Pin the base SHA once (ADR-059 D-b) so every trial — the initial batch and every
	// bisection sub-trial — forks off the same base and a red result is attributable to
	// member composition, not a moving base branch. Skipped under the test seam (no git).
	if e.trainValidateFn == nil {
		fetchCmd := exec.Command("git", "fetch", "origin")
		fetchCmd.Dir = wm.baseDir
		fetchCmd.Env = nonInteractiveGitEnv()
		if out, ferr := fetchCmd.CombinedOutput(); ferr != nil {
			e.logf(0, "merge-train", "warn: fetch origin before pinning base failed: %s\n", strings.TrimSpace(string(out)))
		}
		baseSHA, perr := gitRevParse(wm.baseDir, "refs/remotes/origin/"+baseBranch)
		if perr != nil {
			if baseSHA, perr = gitRevParse(wm.baseDir, baseBranch); perr != nil {
				e.logf(0, "merge-train", "cannot pin base SHA for %s: %v\n", repoKey, perr)
				return trialParams{}, nil, false
			}
		}
		p.baseSHA = baseSHA
		e.logf(0, "merge-train", "pinned base %s (%s) for %s train\n", baseBranch, baseSHA, repoKey)
	}

	// Resolve each member's linked PR number + head SHA once, ejecting fetch failures.
	current := e.fetchTrainMembers(ctx, owner, repo, batch)
	e.logf(0, "merge-train", "assembled %d train member(s) for %s\n", len(current), repoKey)

	return p, current, true
}

// recordMergeTrainCloneSkip tracks consecutive ensureRepoReady ErrSkipItem outcomes for
// prepareTrainWorker's batch[0] repo anchor (#1543 follow-up). batch[0] is an arbitrary
// Queued-column representative that can differ across polls whenever Queued membership
// churns — plausible and normal. ADR-1543's identity-gated retry boundary only reopens
// for the exact item pinned as cloneInFlight's ownerKey at the moment of the original
// failure, so once a later poll selects a different batch[0], that item's identity can
// never match and the gate stays shut — the repo would otherwise skip silently forever,
// recoverable only by the original anchor being reselected or an engine restart.
//
// This escalates instead, exactly once when the streak first reaches e.cfg.MaxRetries,
// by posting a comment on the current anchor naming the pinned owner and the remedy. It
// deliberately does NOT pause the anchor: an earlier revision called pauseIssue here,
// but batch[0] is an arbitrary, otherwise-healthy Queued member — pausing it removes it
// from dispatch eligibility, and since fixing the pinned owner's clone issue never
// un-pauses this anchor, every MaxRetries-poll streak would permanently exile a
// *different* innocent member as Queued membership rotates, without doing anything to
// actually resolve the wedge (review feedback on this PR). A comment-only notice keeps
// the escalation observable — which is the actual goal — without that collateral
// damage; the real remedy (clearing fabrik:paused on the pinned owner) is unaffected
// either way, since that item was already paused with its own explanatory comment at
// the moment its clone attempt failed (see ensureRepoReady).
//
// Unlike this file's own mergeTrainEjectionCounts/ejectMember (which resets its counter
// after pausing, because fabrik:paused itself is the idempotency guard that stops the
// pause path repeating), this counter is NOT reset after escalating: since the anchor
// is deliberately never paused, there is no label to gate a repeat, so a reset here
// would turn one escalation into a comment fired every MaxRetries skips for as long as
// the wedge persists (review feedback on this PR — see the "escalate exactly once"
// comment below). Escalating on count == MaxRetries (not >=, no reset) and leaving the
// count to climb past it unremarked is what actually delivers "escalate once per
// episode": a genuinely new episode only starts once resetMergeTrainCloneSkip deletes
// the counter on the next ensureRepoReady success. This is otherwise keyed per repo
// ("owner/repo"), not per member like mergeTrainEjectionCounts — the wedge is a
// property of the repo's cloneInFlight entry, not of whichever item happens to be
// batch[0] this poll.
//
// The anchor is not necessarily a bystander, though: on the very first ErrSkipItem for
// a repo (MaxRetries: 1 is trivially reachable; the default MaxRetries: 3 is reachable
// too, if the same anchor recurs across polls before its own fabrik:paused takes
// visible effect), anchor IS the pinned owner. The message branches on
// issueKey(anchor, ...) == ownerKey so it never claims a self-owning anchor's "own
// clone was never attempted" or that it "has NOT been paused" when both are false
// (review feedback on this PR).
func (e *Engine) recordMergeTrainCloneSkip(repoKey string, anchor gh.ProjectItem) {
	e.mergeTrainCloneSkipMu.Lock()
	e.mergeTrainCloneSkipCounts[repoKey]++
	count := e.mergeTrainCloneSkipCounts[repoKey]
	e.mergeTrainCloneSkipMu.Unlock()

	// Best-effort: name the pinned owner in the log/comment when known. Absent only if
	// the entry was cleared between ensureRepoReady's read and this one (e.g. a
	// concurrent successful retry elsewhere) — never treated as an error.
	ownerKey := ""
	if v, ok := e.cloneInFlight.Load(repoKey); ok {
		if call, ok := v.(*cloneCall); ok {
			ownerKey = call.ownerKey
		}
	}
	e.logf(0, "merge-train", "repo %s not ready (skip %d, pinned owner %q) — aborting train\n", repoKey, count, ownerKey)

	// Escalate exactly once per streak, at the moment count first reaches MaxRetries —
	// not "count >= MaxRetries" and not followed by a reset. A persistent wedge (the
	// only kind this exists for: it stays wedged until an operator clears the pin or
	// the engine restarts) means count keeps climbing past MaxRetries on every
	// subsequent skip; count != MaxRetries then falls through silently to the log line
	// above for every one of those, so the comment fires exactly once (review feedback
	// on this PR — a reset-after-escalate here turned "escalate once" into "escalate on
	// a timer," reposting every MaxRetries skips indefinitely, sprayed across whichever
	// item is anchor that poll). A genuinely new episode after recovery starts its own
	// fresh count from resetMergeTrainCloneSkip's delete (see below) and gets its own
	// single escalation when it reaches MaxRetries in turn.
	if e.cfg.MaxRetries <= 0 || count != e.cfg.MaxRetries {
		return
	}

	// The anchor is not always a bystander: on the very first ErrSkipItem for a repo
	// (or any later recurrence before the anchor's own fabrik:paused has taken visible
	// effect), anchor IS the pinned owner — its own clone attempt is what failed. The
	// message must not claim otherwise (review feedback on this PR).
	anchorIsOwner := ownerKey != "" && ownerKey == issueKey(anchor, e.defaultRepo())

	var msg string
	if anchorIsOwner {
		msg = fmt.Sprintf(
			"🏭 **Fabrik merge-train — repo clone wedged**\n\nThe merge train for `%s` has been unable to proceed for %d consecutive attempts. This item (#%d) is both the current train anchor and the issue whose own clone attempt failed and is pinning the retry gate — it already carries `fabrik:paused` and an explanatory \"cannot clone repo\" comment from that failure.\n\nFix the underlying clone issue and remove `fabrik:paused` here to retry.",
			repoKey, count, anchor.Number,
		)
	} else {
		ownerClause := "an earlier failed clone attempt"
		if ownerKey != "" {
			ownerClause = fmt.Sprintf("issue %s's failed clone attempt", ownerKey)
		}
		msg = fmt.Sprintf(
			"🏭 **Fabrik merge-train — repo clone wedged**\n\nThe merge train for `%s` has been unable to proceed for %d consecutive attempts. This item (#%d) is the current train anchor, but its own clone was never attempted — the retry is pinned to %s, which must have its `fabrik:paused` label removed (after the underlying clone issue is fixed) before any anchor, including this one, can retry.\n\nThis item itself is otherwise healthy and has NOT been paused — it remains eligible for the next merge-train batch. No action is needed here; resolve the wedge by clearing `fabrik:paused` on the pinned issue above.",
			repoKey, count, anchor.Number, ownerClause,
		)
	}
	e.logf(anchor.Number, "escalate", "merge-train repo %s clone wedged after %d skips (anchor is pinned owner: %v)\n", repoKey, count, anchorIsOwner)
	e.postItemComment(anchor, msg, true)
}

// resetMergeTrainCloneSkip clears the consecutive-skip counter for repoKey once
// ensureRepoReady succeeds for the merge-train anchor, so a stale streak from a
// previously-wedged repo doesn't count toward a future skip's escalation threshold.
func (e *Engine) resetMergeTrainCloneSkip(repoKey string) {
	e.mergeTrainCloneSkipMu.Lock()
	delete(e.mergeTrainCloneSkipCounts, repoKey)
	e.mergeTrainCloneSkipMu.Unlock()
}

// runMergeTrainWorker is the main body of the merge-train goroutine (ADR-059 D3/D4).
// After prepareTrainWorker hands off setup, it runs a re-form loop: assemble+validate
// the (re-formed) batch exactly once; a green result lands immediately (D-d — zero
// bisection on the common path); a red result opens a per-episode cost budget and
// bisects to isolate and eject the poisoner (FR-1/FR-2), re-forming the survivors and
// re-validating (FR-3); cost-cap exhaustion or a non-isolable interaction degrades to
// the one-at-a-time fallback (FR-5). Every exit from this function — including every
// nested landing/dissolve helper it calls — clears the in-flight marker via the single
// deferred finishTrain call below (see ADR-067).
func (e *Engine) runMergeTrainWorker(ctx context.Context, state *mergeTrainWorkerState, owner, repo, partitionBase string, batch []gh.ProjectItem) {
	repoKey := owner + "/" + repo
	trainKey := mergeTrainKey(repoKey, partitionBase)

	p, current, ok := e.prepareTrainWorker(ctx, state, owner, repo, partitionBase, batch)
	if !ok {
		return
	}
	defer func() { <-e.sem }()
	defer e.finishTrain(trainKey)

	// Re-form loop: validate, land-on-green, or bisect-eject-reform on red.
	for {
		if len(current) == 0 {
			e.logf(0, "merge-train", "no survivors remaining for %s — train complete with nothing to land\n", trainKey)
			return
		}

		// #1644: a length-1 batch may be landable directly from its own PR,
		// skipping the trial entirely — checked on every iteration (not just
		// the first) so a batch that bisects down to a single clean survivor
		// and re-forms is also eligible. See trySingletonFastPath's doc
		// comment for why this guard lives here rather than inside
		// assembleAndValidate itself.
		if len(current) == 1 {
			// Apply any pending review-finding eject signal (#1208) before
			// considering the fast path. The fast path never calls
			// assembleAndValidate, so without this it would silently bypass
			// every one of applyPendingReviewEjects' three existing
			// checkpoints (this loop's Hook 2 below, landOneAtATime,
			// landGreenBatch's rebase loop) — landing a member flagged for
			// unresolved review-thread findings on its own linked PR directly,
			// the exact case #1208 exists to prevent. An ejected member leaves
			// `current` empty, so `continue` re-enters the loop and the
			// top-of-loop zero-survivors check returns.
			if remaining, ejectedCount := e.applyPendingReviewEjects(state.projectID, repoKey, current); ejectedCount > 0 {
				e.logf(0, "merge-train", "%d member(s) ejected for unresolved review findings before the singleton fast path — re-forming for %s\n", ejectedCount, trainKey)
				current = remaining
				continue
			}
			if e.trySingletonFastPath(ctx, state, p, current[0]) {
				return
			}
		}

		trialName := p.nextTrialName()
		state.mu.Lock()
		state.trialName = trialName
		state.assembling = true
		state.mu.Unlock()

		survivors, result, prNum, diag, aerr := e.assembleAndValidate(ctx, p, current, trialName)
		if aerr != nil {
			e.logf(0, "merge-train", "assemble/validate failed for %s: %v\n", trainKey, aerr)
			e.cleanupTrialArtifacts(p.wm, trialName)
			return
		}
		if len(survivors) == 0 {
			// Every member was ejected during assembly (unresolvable conflicts).
			e.logf(0, "merge-train", "entire batch ejected during assembly for %s\n", trainKey)
			e.cleanupTrialArtifacts(p.wm, trialName)
			return
		}

		// Hook 1: check runaway guard after the initial re-form trial (ADR-059 D8).
		if count, tripped := e.isRunawayTripped(trainKey); tripped {
			e.cleanupTrialArtifacts(p.wm, trialName)
			e.fireRunawayGuard(ctx, p.owner, p.repo, p.baseBranch, membersToItems(current), count)
			return
		}

		// Hook 2: apply any pending review-finding ejects flagged externally while this
		// trial was assembling/CI-polling (#1208) — mirrors Hook 1's "poll writes a
		// signal, worker consumes it at a checkpoint" shape. A flagged member's trial
		// is always discarded here, regardless of its own CI result: a green trial
		// containing a flagged member must never reach landGreenBatch. An empty
		// `remaining` falls through to continue and is caught by the top-of-loop
		// zero-survivors return, so no special-casing is needed here.
		if remaining, ejectedCount := e.applyPendingReviewEjects(state.projectID, repoKey, survivors); ejectedCount > 0 {
			e.logf(0, "merge-train", "%d member(s) ejected for unresolved review findings mid-trial — discarding trial and re-forming for %s\n", ejectedCount, trainKey)
			e.cleanupTrialArtifacts(p.wm, trialName)
			current = remaining
			continue
		}

		state.mu.Lock()
		state.prNum = prNum
		state.assembling = false
		state.CIResult = result
		state.mu.Unlock()

		switch result {
		case TrainCIGreen:
			// D-d hard invariant: a green batch lands immediately, zero bisection.
			// landGreenBatch adds the D5 main-moved landing gate (behind → rebase →
			// revalidate → dissolve-on-exhaustion) around landMergeTrainBatch.
			e.logf(0, "merge-train", "combined Validate green for %s (%d survivor(s)) — landing\n", trainKey, len(survivors))
			e.landGreenBatch(ctx, state, p, survivors)
			return
		case TrainCIPending:
			e.logf(0, "merge-train", "combined Validate pending/timed out for %s — will retry next poll\n", trainKey)
			e.cleanupTrialArtifacts(p.wm, trialName)
			return
		default: // TrainCIRed
			if len(survivors) == 1 {
				// #1440 R1: a red batch of exactly one member has no poisoner to isolate —
				// bisection's own base case would just return that member immediately, at
				// the cost of the misleading "isolated by halving bisection" / "different
				// composition" ejection wording. Short-circuit straight to the dedicated
				// singleton disposition instead of calling handleRedBatch at all.
				e.logf(survivors[0].item.Number, "merge-train", "combined Validate RED for %s with a single member (#%d) — no poisoner to isolate; disposing as a red singleton\n", trainKey, survivors[0].item.Number)
				e.cleanupTrialArtifacts(p.wm, trialName)
				e.ejectRedSingleton(state.projectID, p.owner, p.repo, survivors[0], diag)
				return
			}
			e.logf(0, "merge-train", "combined Validate RED for %s (%d member(s)) — bisecting to isolate the poisoner\n", trainKey, len(survivors))
			// The red trial's artifacts are unneeded; bisection sub-trials build fresh.
			e.cleanupTrialArtifacts(p.wm, trialName)
			state.mu.Lock()
			state.bisecting = true
			state.mu.Unlock()
			nextSurvivors, fellBack, runaway := e.handleRedBatch(ctx, state, p, survivors, diag)
			state.mu.Lock()
			state.bisecting = false
			state.mu.Unlock()
			if runaway {
				// Runaway guard fired inside bisect or landOneAtATime.
				count, _ := e.isRunawayTripped(trainKey)
				e.fireRunawayGuard(ctx, p.owner, p.repo, p.baseBranch, membersToItems(survivors), count)
				return
			}
			if fellBack {
				// The one-at-a-time fallback already landed/ejected every member.
				return
			}
			current = nextSurvivors // re-form survivors and re-validate (FR-3)
		}
	}
}

// fetchTrainMembers resolves each batch member's linked PR number and head SHA once
// (reused across every bisection trial and the landing step), ejecting any member whose
// linked PR cannot be fetched or has no head SHA. The returned slice preserves batch order.
func (e *Engine) fetchTrainMembers(ctx context.Context, owner, repo string, batch []gh.ProjectItem) []trainMember {
	var members []trainMember
	for _, member := range batch {
		pr, fetchErr := e.client.FetchLinkedPR(owner, repo, member.Number)
		if fetchErr != nil || pr == nil {
			e.logf(member.Number, "merge-train", "cannot fetch linked PR for #%d: %v — ejecting\n", member.Number, fetchErr)
			// Out of scope for #1420 (no combined-Validate diagnostic exists yet at
			// this point — the fetch itself failed): diag and otherMembers are nil.
			e.ejectMember(owner, repo, member, fmt.Sprintf("ejected from merge-train — could not fetch linked PR: %v", fetchErr), nil, nil, true)
			continue
		}
		if pr.HeadSHA == "" {
			e.logf(member.Number, "merge-train", "#%d has no PR head SHA — ejecting\n", member.Number)
			e.ejectMember(owner, repo, member, "ejected from merge-train — linked PR has no head SHA", nil, nil, true)
			continue
		}
		members = append(members, trainMember{item: member, prNum: pr.Number, headSHA: pr.HeadSHA})
	}
	return members
}

// assembleTrialBranch creates a fresh trial worktree forked off the pinned base SHA (D-b)
// and sequentially merges each member's head SHA into it, resolving conflicts via Claude and
// ejecting members whose conflicts are unresolvable. It returns the survivors (members that
// merged or were resolved), the pushed trial branch HEAD SHA, and any fatal error. A zero-
// survivor result returns (nil, "", nil) — the caller handles the terminal.
func (e *Engine) assembleTrialBranch(ctx context.Context, p trialParams, members []trainMember, trialName string) ([]trainMember, string, error) {
	wtDir, err := p.wm.EnsureTrainWorktreeAt(trialName, p.baseSHA)
	if err != nil {
		return nil, "", fmt.Errorf("creating trial worktree: %w", err)
	}

	// No fetch is needed here: the trial worktree shares the bare repo's object database,
	// and the caller has already run `git fetch origin` in wm.baseDir before assembling —
	// the base-SHA pin at worker start (runMergeTrainWorker) covers the main-loop trial and
	// every bisection sub-trial, and landOneAtATime re-pins per singleton — so each member
	// headSHA (an immutable snapshot captured once in fetchTrainMembers) is already a local
	// object resolvable by `git merge <sha>`. Re-fetching per trial would be a wasted network
	// round-trip on every bisection sub-trial. Keep this invariant if the fetch is refactored.

	var survivors []trainMember
	for _, member := range members {
		preMergeHeadCmd := exec.Command("git", "rev-parse", "HEAD")
		preMergeHeadCmd.Dir = wtDir
		preMergeHeadOut, preMergeHeadErr := preMergeHeadCmd.Output()
		preMergeHEAD := strings.TrimSpace(string(preMergeHeadOut))
		if preMergeHeadErr != nil {
			return nil, "", fmt.Errorf("capturing pre-merge HEAD before merging #%d: %w", member.item.Number, preMergeHeadErr)
		}

		mergeCmd := exec.Command("git", "merge", "--no-ff", "--no-edit", member.headSHA)
		mergeCmd.Dir = wtDir
		mergeOut, mergeErr := mergeCmd.CombinedOutput()

		if mergeErr == nil {
			survivors = append(survivors, member)
			e.logf(member.item.Number, "merge-train", "merged #%d cleanly into trial branch\n", member.item.Number)
			continue
		}

		// Conflict — classify against the declared generated-file set and resolve.
		e.logf(member.item.Number, "merge-train", "merge conflict for #%d: %s — resolving\n", member.item.Number, strings.TrimSpace(string(mergeOut)))
		// PRNumber is deliberately left unset here (#1288): this invocation resolves a
		// merge conflict on the trial branch, not the member's own PR, so there's no
		// single "the PR" for FABRIK_PR to name. FabrikRoot is still cheap and correct
		// to set for consistency with the other two InvokeOptions call sites.
		opts := InvokeOptions{BaseBranch: p.baseBranch, MaxTurnsOverride: p.maxTurnsOverride, FabrikRoot: e.fabrikDir, FabrikRepo: e.defaultRepo(), MaxResumeFailures: e.cfg.MaxResumeFailures}
		resolved, reason, resolveErr := e.resolveTrainConflict(ctx, member.item, wtDir, p.holdingStg, member.headSHA, preMergeHEAD, opts)
		if resolved {
			survivors = append(survivors, member)
			e.logf(member.item.Number, "merge-train", "conflict for #%d resolved\n", member.item.Number)
			continue
		}
		if resolveErr != nil {
			// Resolution could not even be attempted (account-wide Claude usage-limit
			// suspension, ADR-1120) — this says nothing about whether #%d's conflict is
			// resolvable, so ejecting the member here would be a correctness bug. Abort
			// the in-progress merge and propagate as a fatal assembly error instead; the
			// caller's fatal-error path cleans up trial artifacts and retries on the
			// train's next natural cycle, with no member punished.
			abortCmd := exec.Command("git", "merge", "--abort")
			abortCmd.Dir = wtDir
			abortCmd.CombinedOutput() // best-effort
			return nil, "", fmt.Errorf("resolving conflict for #%d: %w", member.item.Number, resolveErr)
		}

		// Unresolvable (or regeneration failed, FR-4) — restore wtDir to its pre-merge
		// state and eject. `git merge --abort` alone is insufficient here: resolution
		// can fail *after* a commit already landed on wtDir (e.g. regenerateAndCommit's
		// premature-commit guard trips because Claude committed despite mixed-mode
		// instructions not to) — at that point MERGE_HEAD is already gone, so `git merge
		// --abort` is a silent no-op and would leave that bad commit as wtDir's HEAD,
		// contaminating every subsequent member's merge and the pushed trial branch.
		// Hard-reset to the captured preMergeHEAD unconditionally so the worktree is
		// clean regardless of how far resolution got before failing; this is a no-op
		// when `git merge --abort` already fully reverted things.
		abortCmd := exec.Command("git", "merge", "--abort")
		abortCmd.Dir = wtDir
		abortCmd.CombinedOutput() // best-effort; the reset below is the authoritative cleanup
		resetCmd := exec.Command("git", "reset", "--hard", preMergeHEAD)
		resetCmd.Dir = wtDir
		if out, resetErr := resetCmd.CombinedOutput(); resetErr != nil {
			e.logf(member.item.Number, "merge-train", "warn: could not reset trial worktree to pre-merge state after ejecting #%d: %s\n", member.item.Number, strings.TrimSpace(string(out)))
		}
		// `git reset --hard` only rewinds tracked content — it does not remove untracked
		// files a conflict-resolution attempt (Claude, or a regeneration command) may have
		// left behind. A stray untracked file surviving here would make the next member's
		// `git merge` fail with git's own "untracked working tree file would be
		// overwritten by merge" error — which has no MERGE_HEAD and no unmerged paths, so
		// resolveTrainConflict would misclassify it as a plain conflict and dispatch Claude
		// against a worktree with nothing to resolve. `-fd` removes untracked files and
		// directories but leaves ignored files alone, matching the scope of this cleanup.
		cleanCmd := exec.Command("git", "clean", "-fd")
		cleanCmd.Dir = wtDir
		if out, cleanErr := cleanCmd.CombinedOutput(); cleanErr != nil {
			e.logf(member.item.Number, "merge-train", "warn: could not remove untracked files from trial worktree after ejecting #%d: %s\n", member.item.Number, strings.TrimSpace(string(out)))
		}
		e.logf(member.item.Number, "merge-train", "cannot resolve conflict for #%d — ejecting\n", member.item.Number)
		if reason == "" {
			reason = fmt.Sprintf("ejected from merge-train batch — unresolvable conflict (PR SHA %s)", member.headSHA)
		} else {
			reason = fmt.Sprintf("ejected from merge-train batch — %s (PR SHA %s)", reason, member.headSHA)
		}
		// Out of scope for #1420 (unresolvable merge conflict, not a combined-Validate
		// failure): diag and otherMembers are nil.
		e.ejectMember(p.owner, p.repo, member.item, reason, nil, nil, true)
	}

	if len(survivors) == 0 {
		return nil, "", nil
	}

	if err := p.wm.PushTrainBranch(trialName); err != nil {
		return nil, "", fmt.Errorf("pushing trial branch: %w", err)
	}
	trialSHA, err := gitRevParse(wtDir, "HEAD")
	if err != nil {
		return nil, "", fmt.Errorf("reading trial branch SHA: %w", err)
	}
	return survivors, trialSHA, nil
}

// assembleAndValidate builds a trial branch for members (off the pinned base SHA), opens a
// draft CI PR, and polls the combined Validate. It returns the survivors, the CI result, the
// draft PR number, and — for a red result — the diagnostic that observed it (R1/#1420, nil
// for green/pending/error). The local trial worktree and both branches persist after this
// returns (success or failure) — the caller owns cleanup exactly once, via
// cleanupTrialArtifacts or an equivalent direct CleanupTrainWorktree call, regardless of
// outcome.
//
// This is a thin wrapper over assembleAndValidateInner that also records the trial against
// the runaway guard's counter — but only when the result is not TrainCIGreen (#1528). A green
// result is, by construction, either the landing attempt itself or a bisection sub-trial that
// just proved a sub-batch clean — never a "zero successful lands" event, which is the guard's
// entire premise. Counting it turned a successful bisection into a false runaway trip: the
// survivor-validation trial that was about to land got counted as a failure one second before
// landing. Red results, TrainCIPending, and assembly errors are still counted unconditionally —
// they represent no progress, exactly as before this fix.
func (e *Engine) assembleAndValidate(ctx context.Context, p trialParams, members []trainMember, trialName string) ([]trainMember, TrainCIResult, int, *trainCIDiagnostic, error) {
	survivors, result, prNum, diag, err := e.assembleAndValidateInner(ctx, p, members, trialName)
	if result != TrainCIGreen {
		e.recordTrial(p.trainKey)
	}
	return survivors, result, prNum, diag, err
}

// assembleAndValidateInner is assembleAndValidate's actual implementation (git assembly, draft
// CI PR, and CI polling), split out so the wrapper can decide whether the trial counts toward
// the runaway guard based on the outcome. See assembleAndValidate's doc comment.
//
// When e.trainValidateFn is set (tests), it short-circuits the whole git/CI path, keying the
// result (and diagnostic) on batch membership alone (ADR-059 D4 test seam). This is the ONLY
// combined validation on the common path — a green result must never trigger bisection (D-d).
func (e *Engine) assembleAndValidateInner(ctx context.Context, p trialParams, members []trainMember, trialName string) ([]trainMember, TrainCIResult, int, *trainCIDiagnostic, error) {
	if e.trainValidateFn != nil {
		result, diag := e.trainValidateFn(ctx, members)
		return members, result, 0, diag, nil
	}

	survivors, trialSHA, err := e.assembleTrialBranch(ctx, p, members, trialName)
	if err != nil {
		return nil, TrainCIPending, 0, nil, err
	}
	if len(survivors) == 0 {
		return nil, TrainCIPending, 0, nil, nil
	}

	// Open a draft CI PR listing the survivors.
	var memberRefs, closesLines []string
	for _, s := range survivors {
		memberRefs = append(memberRefs, fmt.Sprintf("#%d", s.item.Number))
		closesLines = append(closesLines, fmt.Sprintf("Closes #%d", s.item.Number))
	}
	prTitle := fmt.Sprintf("chore(merge-train): trial integration for %s", strings.Join(memberRefs, " "))
	// The draft CI PR IS the landing integration PR (same trial branch → base). It
	// carries mergeTrainBatchMarker so the landing step's findIntegrationPR reuses
	// it (marking it ready) rather than trying to CreatePR a second PR on the same
	// branch — which GitHub rejects with a 422 "a pull request already exists".
	// The "Closes #N" lines link each member issue to this landing PR and auto-close
	// them when it merges (into the default branch), restoring issue↔landing-PR
	// connectivity — the member PRs are closed-not-merged, so their own Closes #N
	// never fires. (A non-default base won't auto-close; the landing step closes the
	// issues explicitly as a fallback.)
	prBody := fmt.Sprintf("🏭 **Fabrik merge-train integration PR** (trial → %s)\n\n"+
		"This is a disposable trial branch combining the following Queued member PRs:\n%s\n\n"+
		"Do not merge this PR manually — Fabrik manages the landing step.\n"+
		"Orphaned integration PRs (if the train worker crashed) can be closed manually via the GitHub UI.\n\n"+
		"%s\n\n%s",
		p.baseBranch, strings.Join(memberRefs, "\n"), strings.Join(closesLines, "\n"), mergeTrainBatchMarker)

	trialBranch := "fabrik/merge-train/" + trialName
	prNum, err := e.client.CreateDraftPR(p.owner, p.repo, prTitle, trialBranch, p.baseBranch, prBody, 0)
	if err != nil {
		return nil, TrainCIPending, 0, nil, fmt.Errorf("creating draft CI PR: %w", err)
	}
	e.logf(0, "merge-train", "opened draft CI PR #%d for %s/%s (%d survivor(s))\n", prNum, p.owner, p.repo, len(survivors))

	result, diag := e.pollTrainCI(ctx, p.owner, p.repo, prNum, trialSHA)
	return survivors, result, prNum, diag, nil
}

// bisect recursively halves a known-red member set to isolate the single poisoning member
// (ADR-059 D4 / FR-1), reusing assembleAndValidate for each trial in the bors-ng test order
// (test half A; if red recurse into A; else test half B; if red recurse into B). diag is the
// diagnostic of the validation that established red is currently known-red (the caller's
// initial validation, or — recursively — the half that was just found red); the base case
// (len(red)==1) returns it unchanged rather than issuing a further validate call, which is
// what makes "the run that isolates the member" the diagnostic's origin by construction
// (R1/#1420): nothing after that isolating call can overwrite it, because there is no shared
// state to overwrite — only a threaded return value. It returns the isolated poisoner and its
// diagnostic, (nil, nil, true, false) when the redness is a non-isolable cross-PR interaction
// (both halves green) or the per-episode cost budget (*used vs costCap) is exhausted — either
// degrades to the FR-5 one-at-a-time fallback (D-e) — or (nil, nil, false, true) when the
// runaway guard fires. red is assumed to be a validated-red set.
func (e *Engine) bisect(ctx context.Context, p trialParams, red []trainMember, diag *trainCIDiagnostic, used *int, costCap int) (*trainMember, *trainCIDiagnostic, bool, bool) {
	if len(red) == 1 {
		return &red[0], diag, false, false
	}

	trainKey := p.trainKey
	mid := len(red) / 2
	for _, half := range [][]trainMember{red[:mid], red[mid:]} {
		if *used >= costCap {
			e.logf(0, "merge-train", "bisection cost cap (%d validations) reached — degrading to one-at-a-time fallback\n", costCap)
			return nil, nil, true, false
		}
		trialName := p.nextTrialName()
		survivors, result, _, halfDiag, err := e.assembleAndValidate(ctx, p, half, trialName)
		*used++
		e.cleanupTrialArtifacts(p.wm, trialName)
		if err != nil {
			e.logf(0, "merge-train", "bisection trial failed to assemble: %v — degrading to one-at-a-time fallback\n", err)
			if _, tripped := e.isRunawayTripped(trainKey); tripped {
				return nil, nil, false, true
			}
			return nil, nil, true, false
		}
		if _, tripped := e.isRunawayTripped(trainKey); tripped {
			return nil, nil, false, true
		}
		if result == TrainCIRed && len(survivors) > 0 {
			return e.bisect(ctx, p, survivors, halfDiag, used, costCap)
		}
	}

	// Both halves green: the redness spans the split — a non-isolable interaction (D-e).
	return nil, nil, true, false
}

// handleRedBatch bisects a red batch to isolate and eject the poisoning member (FR-1/FR-2),
// then returns the surviving members for the main loop to re-form and re-validate (FR-3).
// diag is the diagnostic of the validation that established red is currently red (the
// caller's own top-level assembleAndValidate) — bisect's starting point (see its doc comment
// for why this makes overwrite-by-a-later-run structurally impossible). When bisection cannot
// isolate a single culprit within the cost budget (a non-isolable interaction or cost-cap
// exhaustion), it degrades to the one-at-a-time fallback (FR-5), which lands/ejects every
// member itself, and returns (nil, true, false). Returns (nil, false, true) when the runaway
// guard fires inside bisect or landOneAtATime. The cost budget is per red-batch episode: it
// starts at 1 (the initial red validation) and is capped at effectiveBisectCap().
func (e *Engine) handleRedBatch(ctx context.Context, state *mergeTrainWorkerState, p trialParams, red []trainMember, diag *trainCIDiagnostic) ([]trainMember, bool, bool) {
	if e.trainRedBatchHook != nil {
		e.trainRedBatchHook()
	}
	used := 1 // the initial red validation counts toward the per-episode budget
	costCap := e.effectiveBisectCap()

	poisoner, isolationDiag, fellBack, runaway := e.bisect(ctx, p, red, diag, &used, costCap)
	if runaway {
		return nil, false, true
	}
	if fellBack {
		e.logf(0, "merge-train", "could not isolate a single poisoner for %s/%s (%d/%d validations used) — degrading to one-at-a-time landing of %d member(s)\n", p.owner, p.repo, used, costCap, len(red))
		runaway = e.landOneAtATime(ctx, state, p, red)
		return nil, true, runaway
	}

	// Eject the isolated poisoner (D-a shared counter, D-c comment, cap→pause reuse). red —
	// the full batch at the start of this episode — is passed as the R4 batch context: the
	// isolating run itself always validates the poisoner alone (bisect's base case makes no
	// further call), so "the other batch members" means who else rode in this train attempt,
	// not the isolating run's own (always-singleton) inputs.
	e.logf(poisoner.item.Number, "merge-train", "bisection isolated #%d as the batch poisoner — ejecting\n", poisoner.item.Number)
	e.ejectMember(p.owner, p.repo, poisoner.item,
		fmt.Sprintf("ejected from merge-train — the combined Validate fails whenever #%d is in the batch (isolated by halving bisection). It will be retried in a future train with a different composition.", poisoner.item.Number),
		isolationDiag, red, true)

	var survivors []trainMember
	for i := range red {
		if red[i].item.Number != poisoner.item.Number {
			survivors = append(survivors, red[i])
		}
	}
	return survivors, false, false
}

// landOneAtATime is the FR-5 fallback: it validates and lands each member as its own
// singleton batch, which dissolves any cross-PR interaction by construction (no two members
// co-reside). A green singleton lands via landSingleton; a red singleton fails even in
// isolation and is ejected; a pending singleton is left in Queued to retry. Returns true if
// the runaway guard fires during processing. In the real path the base is re-pinned to the
// current origin/<base> before each singleton so a prior land is visible to the next member's
// validation (this is what actually dissolves a genuine interaction); under the test seam this
// git step is skipped (the membership-keyed fn is stateless — see the ADR-059 D4
// landOneAtATime note in docs/state-machine.md).
func (e *Engine) landOneAtATime(ctx context.Context, state *mergeTrainWorkerState, p trialParams, members []trainMember) bool {
	repoKey := p.owner + "/" + p.repo
	trainKey := p.trainKey
	e.logf(0, "merge-train", "one-at-a-time fallback: processing %d member(s) as singleton batches\n", len(members))
	for _, m := range members {
		if e.trainValidateFn == nil {
			// Re-pin the base to current origin/<base> so a prior singleton's land is seen.
			fetchCmd := exec.Command("git", "fetch", "origin")
			fetchCmd.Dir = p.wm.baseDir
			fetchCmd.Env = nonInteractiveGitEnv()
			fetchCmd.CombinedOutput() // best-effort
			if sha, rerr := gitRevParse(p.wm.baseDir, "refs/remotes/origin/"+p.baseBranch); rerr == nil {
				p.baseSHA = sha // local copy; persists across this loop, does not leak to caller
			}
		}

		trialName := p.nextTrialName()
		survivors, result, _, diag, err := e.assembleAndValidate(ctx, p, []trainMember{m}, trialName)
		if err != nil || len(survivors) == 0 {
			e.logf(m.item.Number, "merge-train", "could not assemble #%d in isolation: %v — leaving in Queued\n", m.item.Number, err)
			e.cleanupTrialArtifacts(p.wm, trialName)
			if _, tripped := e.isRunawayTripped(trainKey); tripped {
				return true
			}
			continue
		}
		if _, tripped := e.isRunawayTripped(trainKey); tripped {
			e.cleanupTrialArtifacts(p.wm, trialName)
			return true
		}

		// Hook 2: apply any pending review-finding eject flagged externally while this
		// singleton trial was assembling/CI-polling (#1208) — mirrors the re-form loop's
		// identical checkpoint in runMergeTrainWorker. A flagged singleton's trial is
		// discarded regardless of its own CI result — there is nothing left to land or
		// eject via the normal green/red path this iteration, so move on to the next member.
		if _, ejectedCount := e.applyPendingReviewEjects(state.projectID, repoKey, survivors); ejectedCount > 0 {
			e.logf(m.item.Number, "merge-train", "pending review-finding eject flagged for singleton #%d — discarding trial\n", m.item.Number)
			e.cleanupTrialArtifacts(p.wm, trialName)
			continue
		}

		switch result {
		case TrainCIGreen:
			e.landSingleton(ctx, state, p, m, trialName)
		case TrainCIRed:
			e.cleanupTrialArtifacts(p.wm, trialName)
			e.logf(m.item.Number, "merge-train", "#%d fails combined Validate even in isolation — disposing as a red singleton\n", m.item.Number)
			// #1440: this validates m completely alone ([]trainMember{m}) — structurally
			// the same true-singleton scenario the top-level arity guard targets, just
			// reached via the one-at-a-time fallback instead. It gets the same
			// disposition (no "different composition" promise, no shared-counter churn)
			// rather than ejectMember's multi-member wording, which would be equally
			// misleading here.
			e.ejectRedSingleton(state.projectID, p.owner, p.repo, m, diag)
		default: // TrainCIPending
			e.cleanupTrialArtifacts(p.wm, trialName)
			e.logf(m.item.Number, "merge-train", "combined Validate pending for singleton #%d — leaving in Queued\n", m.item.Number)
		}
	}
	return false
}

// landedCommentRetryDelay is the base delay for addLandedCommentWithRetry's retry backoff.
// Declared as a var (not const) so tests can set it to 0 to avoid sleeping.
var landedCommentRetryDelay = 200 * time.Millisecond

// addLandedCommentWithRetry posts the "landed via ..." comment on a member's PR, retrying
// transient failures with exponential backoff. This comment is the sole cross-landing-path,
// member-scoped record of which integration/singleton PR actually landed the change (issue
// #1275) — losing it to a transient API hiccup silently degrades the audit trail even though
// the landing itself succeeded. The comment is purely informational and never gates a state
// transition, so on exhaustion this falls back to the pre-existing warn-and-continue behavior
// unchanged; it must never block or delay landing.
func (e *Engine) addLandedCommentWithRetry(owner, repo string, issueNumber, prNum int, body string) {
	const maxAttempts = 3
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		_, err := e.client.AddComment(owner, repo, prNum, body)
		if err == nil {
			return
		}
		if !isTransientError(err) {
			e.logf(issueNumber, "merge-train", "warn: could not post landed comment on PR #%d: %v\n", prNum, err)
			return
		}
		lastErr = err
		if attempt < maxAttempts-1 {
			delay := landedCommentRetryDelay << attempt
			time.Sleep(delay)
		}
	}
	e.logf(issueNumber, "merge-train", "warn: could not post landed comment on PR #%d after %d attempts: %v\n", prNum, maxAttempts, lastErr)
}

// landSingleton lands a single member from its own validated-green trial branch. It creates a
// dedicated integration PR WITHOUT the shared batch marker — sequential singleton lands must
// not collide on findIntegrationPR (which matches merged PRs via ListPRs state=all), which
// would make a later singleton skip its own merge and advance without landing its code (a
// data-loss bug; see the ADR-059 D4 landSingleton note). It merges the PR, advances the
// member Queued→Done, closes the member's linked PR, and resets its ejection counter.
func (e *Engine) landSingleton(ctx context.Context, state *mergeTrainWorkerState, p trialParams, m trainMember, trialName string) {
	trialBranch := "fabrik/merge-train/" + trialName
	defer e.cleanupTrialArtifacts(p.wm, trialName)

	title := fmt.Sprintf("[merge-train] singleton: #%d", m.item.Number)
	body := fmt.Sprintf("🏭 **Fabrik merge-train singleton landing PR**\n\n"+
		"Lands #%d — %s — one-at-a-time after the batch could not be landed together.\n\n"+
		"Do not merge manually; Fabrik manages the landing step.",
		m.item.Number, m.item.Title)

	prNum, err := e.client.CreatePR(p.owner, p.repo, title, trialBranch, p.baseBranch, body)
	if err != nil {
		e.logf(m.item.Number, "merge-train", "cannot create singleton landing PR for #%d: %v\n", m.item.Number, err)
		return
	}

	if !e.pollForMergeable(ctx, p.owner, p.repo, prNum, []trainMember{m}) {
		return // timeout / dirty — leave in Queued
	}
	if err := e.client.MergePR(p.owner, p.repo, prNum); err != nil {
		e.logf(m.item.Number, "merge-train", "merge of singleton PR #%d failed: %v\n", prNum, err)
		return
	}
	e.logf(m.item.Number, "merge-train", "merged singleton landing PR #%d for #%d\n", prNum, m.item.Number)

	// Advance Queued → Done (unless already Done from a prior partial run).
	if m.item.Status != "Done" {
		board := &gh.ProjectBoard{ProjectID: state.projectID}
		if e.statusField == nil {
			e.logf(m.item.Number, "merge-train", "warn: statusField unavailable — cannot advance #%d to Done\n", m.item.Number)
		} else if advErr := e.advanceToNextStage(board, m.item, p.holdingStg); advErr != nil {
			e.logf(m.item.Number, "merge-train", "warn: could not advance #%d to Done: %v\n", m.item.Number, advErr)
		} else {
			e.logf(m.item.Number, "merge-train", "advanced #%d to Done\n", m.item.Number)
			// #1616: record the singleton landing PR credited for this Done
			// transition and mark the item for post-Done landing verification.
			// See markCreditedLanding for why the marker is applied only when
			// the credited-PR record itself landed.
			e.markCreditedLanding(m.item, prNum)
		}
	} else {
		// #1616: already Done from a prior partial run — still record the
		// markers, for the same restart-safety reason as landMergeTrainBatch's
		// equivalent path (see the comment there).
		e.markCreditedLanding(m.item, prNum)
	}

	// Close the member's linked PR with a landing comment.
	if m.prNum != 0 {
		landedComment := fmt.Sprintf("🏭 **Fabrik merge-train** — Landed one-at-a-time via singleton PR #%d.", prNum)
		e.addLandedCommentWithRetry(p.owner, p.repo, m.item.Number, m.prNum, landedComment)
		if closeErr := e.client.CloseIssue(p.owner, p.repo, m.prNum); closeErr != nil {
			e.logf(m.item.Number, "merge-train", "warn: could not close member PR #%d: %v\n", m.prNum, closeErr)
		}
	}

	// Close the member issue. The singleton landing PR's Closes #N auto-closes it on
	// merge into the default branch; this explicit close is the fallback for
	// non-default bases and auto-close lag (idempotent). Without it the issue is
	// left landed-but-open (the member PR is closed-not-merged). On failure, the
	// outstanding close is durably recorded (fabrik:awaiting-member-close) and retried
	// by the settle scan in poll.go every poll until it succeeds or escalates — ADR-061.
	if closeErr := e.client.CloseIssue(p.owner, p.repo, m.item.Number); closeErr != nil {
		e.logf(m.item.Number, "merge-train", "warn: could not close member issue #%d: %v — will retry via settle scan\n", m.item.Number, closeErr)
		e.markMergeTrainMemberCloseOutstanding(m.item, p.owner, p.repo)
	} else {
		e.logf(m.item.Number, "merge-train", "closed member issue #%d\n", m.item.Number)
	}

	e.resetEjectionCount(p.owner, p.repo, m.item.Number)
	e.resetTrialCounter(p.trainKey)
}

// singletonFastPathEligible decides whether trainMember m can be landed directly
// from its own PR, skipping the trial branch entirely (#1644 R1/R2/R3). Checked
// in cheapest-first order, each step consuming only already-fetched data or a
// single extra API call:
//
//  1. The live PR head SHA (pr.HeadSHA) still equals the cached snapshot
//     (m.headSHA) taken once at batch formation (fetchTrainMembers) — closes an
//     otherwise-silent TOCTOU gap: a member re-pushed after batch formation but
//     before this check must never be landed on stale ancestry/CI evidence.
//  2. The pinned base SHA (p.baseSHA — never a live origin/<base> read, R3) is
//     an ancestor of the member's head: FetchCommitsBehind(base=p.baseSHA,
//     head=m.headSHA) == 0 means the merge would be a fast-forward, so the
//     trial's tree would be byte-identical to the member's own PR head tree —
//     already validated by the member's own CI.
//  3. The PR's mergeable_state is accepted (gh.MergeableStateAccepted, ADR-072)
//     — R1.3's narrower "not dirty" half, which Research judged uncontroversial
//     under either ADR-072 or ADR-1153.
//  4. The member's own CI is green AND complete by ADR-1153/ADR-1441's
//     standard — classifyLandingCI, reused unmodified (R1.2's "must not fork or
//     loosen that logic").
//
// R2's fail-closed discipline is structural, not a convention to remember: every
// branch that cannot positively confirm a condition returns (false, reason) —
// there is no code path that returns true except by every check having been
// positively satisfied. An API error is always "not confirmed," never "assumed
// fine" — the opposite polarity from trialBehind's "assume up to date on error,"
// which exists for a different, lower-stakes decision (FR-2 main-moved
// detection during an already-green trial, not an unattended skip-the-trial
// decision).
func (e *Engine) singletonFastPathEligible(p trialParams, m trainMember, pr *gh.PRDetails) (bool, string) {
	if pr.HeadSHA != m.headSHA {
		return false, fmt.Sprintf("live PR head %s no longer matches the batch-formation snapshot %s", pr.HeadSHA, m.headSHA)
	}
	if p.baseSHA == "" {
		return false, "no pinned base SHA available"
	}

	behind, err := e.client.FetchCommitsBehind(p.owner, p.repo, p.baseSHA, m.headSHA)
	if err != nil {
		return false, fmt.Sprintf("could not confirm pinned base is an ancestor of the member's head: %v", err)
	}
	if behind > 0 {
		return false, fmt.Sprintf("pinned base is %d commit(s) ahead of the member's head — not a fast-forward, base moved since pinning", behind)
	}

	if !gh.MergeableStateAccepted(pr.MergeableState) {
		return false, fmt.Sprintf("mergeable_state %q not accepted", pr.MergeableState)
	}

	checkRuns, err := e.client.FetchCheckRuns(p.owner, p.repo, m.headSHA)
	if err != nil {
		return false, fmt.Sprintf("could not fetch check runs for %s: %v", m.headSHA, err)
	}
	// classifyLandingCI's zero-check-runs branch falls back to mergeable_state
	// alone (ADR-933's "Actions disabled" case) — correct for its usual caller
	// (pollForMergeable, judging a landing PR whose tree was already validated
	// by a just-polled trial CI run) but wrong here: R1.2 requires positive
	// evidence that the member's OWN CI already ran and passed, and this fast
	// path never builds a trial, so zero check runs on the member's own PR
	// head means "nothing has actually run" (or CI hasn't reported yet), not
	// "green by absence." Falling through to classifyLandingCI's fallback
	// would land an unvalidated tree — exactly the false positive R2/AC4
	// require failing closed on — so it's rejected before classifyLandingCI
	// is even consulted, regardless of mergeable_state.
	if len(checkRuns) == 0 {
		return false, "zero check runs on the member's own PR head — no positive CI evidence to land on"
	}
	result, detail := e.classifyLandingCI(p.owner, p.repo, pr.MergeableState, m.headSHA, checkRuns)
	if result != TrainCIGreen {
		return false, fmt.Sprintf("CI not confirmed green and complete: %s", detail)
	}
	return true, detail
}

// finishSingletonFastPathLanding completes the Done transition for a
// singleton-fast-path landing (R5, #1644) — called once the member's own PR is
// confirmed merged (either just merged by trySingletonFastPath, or found
// already merged on a prior partial run). Unlike landSingleton this never mints
// a dedicated landing PR: there is no validated trial branch to build one from,
// by construction (the entire point of the fast path is skipping the trial).
//
// Modeled on advanceConvergedPRToDone (engine/merge_gate.go) — the ordinary
// auto-merge path's Done-transition template — rather than landSingleton:
// advance Queued -> Done via recordAdvanceOutcome (so a failed advance gets the
// same durable retry/escalation as every other terminal advance), then
// closeIssueIfNonDefaultBase unconditionally on the confirmed merge (this is
// the first merge-train landing path that needs it — landSingleton/
// landMergeTrainBatch always close explicitly regardless of base, but this path
// deliberately relies on the merged PR's own Closes #N per R5), then apply
// awaitingLandingVerificationLabel only once the advance itself succeeded — no
// fabrik:credited-pr:<N> is ever applied here, since the merged PR IS the
// item's own linked PR and is durably rediscoverable via #1616's settle scan's
// ordinary FetchLinkedPR fallback (see markCreditedLanding's doc comment for
// why that label exists only for merge-train's other two landing paths, whose
// credited PR is never the member's own).
func (e *Engine) finishSingletonFastPathLanding(state *mergeTrainWorkerState, p trialParams, m trainMember) {
	if m.item.Status != "Done" {
		var advErr error
		if e.statusField == nil {
			advErr = fmt.Errorf("statusField not available")
			e.logf(m.item.Number, "merge-train", "warn: statusField unavailable — cannot advance #%d to Done\n", m.item.Number)
		} else {
			board := &gh.ProjectBoard{ProjectID: state.projectID}
			if advErr = e.recordAdvanceOutcome(board, m.item, p.holdingStg); advErr != nil {
				e.logf(m.item.Number, "merge-train", "warn: could not advance #%d to Done: %v\n", m.item.Number, advErr)
			} else {
				e.logf(m.item.Number, "merge-train", "advanced #%d to Done via singleton fast path\n", m.item.Number)
			}
		}
		// closeIssueIfNonDefaultBase runs unconditionally on the confirmed merge
		// (advanceConvergedPRToDone's pattern), regardless of the advance's own
		// outcome above — see awaitingAdvanceLabel's doc comment for why that's
		// safe even when recordAdvanceOutcome just failed.
		e.closeIssueIfNonDefaultBase(m.item, m.prNum)
		if advErr == nil {
			e.addLabel(m.item, awaitingLandingVerificationLabel)
		}
	} else {
		// Restart safety: already Done from a prior partial run (e.g. crashed
		// between MergePR and the advance). Both calls are idempotent, so
		// re-applying them here costs at most a redundant API call — mirrors
		// landMergeTrainBatch's identical "already Done" restart branch.
		e.closeIssueIfNonDefaultBase(m.item, m.prNum)
		e.addLabel(m.item, awaitingLandingVerificationLabel)
	}

	e.resetEjectionCount(p.owner, p.repo, m.item.Number)
	e.resetTrialCounter(p.trainKey)
}

// trySingletonFastPath is runMergeTrainWorker's re-form-loop guard (#1644),
// checked on every iteration for a length-1 current batch, immediately before a
// trial would otherwise be built. It is deliberately NOT inside
// assembleAndValidate/assembleAndValidateInner: that would also fire inside
// bisect's poisoner-isolation sub-trials (wrong — a sub-batch member's own-PR
// CI says nothing about whether it's the poisoner in an already-red
// combination) and inside landOneAtATime's fallback (explicitly out of scope
// per the issue's Scope section). Returns true when the disposition for this
// poll is already decided — landed, or a landing attempt was made and should
// not fall through to a trial this cycle — and false when the caller should
// proceed to build the trial exactly as before this feature existed.
func (e *Engine) trySingletonFastPath(ctx context.Context, state *mergeTrainWorkerState, p trialParams, m trainMember) bool {
	pr, err := e.client.FetchPRDetails(p.owner, p.repo, m.prNum)
	if err != nil || pr == nil {
		e.logf(m.item.Number, "merge-train", "singleton fast path: could not fetch PR #%d details: %v — building trial\n", m.prNum, err)
		return false
	}

	if pr.Merged {
		e.logf(m.item.Number, "merge-train", "singleton fast path: PR #%d already merged (resuming a prior partial run) — completing Done transition\n", m.prNum)
		e.finishSingletonFastPathLanding(state, p, m)
		return true
	}

	eligible, reason := e.singletonFastPathEligible(p, m, pr)
	if !eligible {
		e.logf(m.item.Number, "merge-train", "singleton fast path not taken for #%d: %s — building trial\n", m.item.Number, reason)
		return false
	}
	e.logf(m.item.Number, "merge-train", "singleton fast path taken for #%d: %s — landing PR #%d directly, no trial branch, no draft CI PR\n", m.item.Number, reason, m.prNum)

	// MergePRAtHeadSHA, not MergePR: singletonFastPathEligible's CI/mergeability
	// evidence was gathered against pr.HeadSHA (== m.headSHA, confirmed equal
	// above) on the member's own, externally-writable PR branch. Pinning the
	// merge request to that exact SHA closes the window between that check and
	// this call — a push landing in between is rejected (ErrConflict, mapped
	// from GitHub's 409) rather than silently merged unvalidated.
	if err := e.client.MergePRAtHeadSHA(p.owner, p.repo, m.prNum, m.headSHA); err != nil {
		e.logf(m.item.Number, "merge-train", "singleton fast path: merge of PR #%d failed: %v — leaving #%d in Queued for retry\n", m.prNum, err, m.item.Number)
		return true
	}
	e.logf(m.item.Number, "merge-train", "singleton fast path: merged PR #%d for #%d\n", m.prNum, m.item.Number)
	e.finishSingletonFastPathLanding(state, p, m)
	return true
}

// cleanupTrialArtifacts removes a trial's local worktree and its local+remote branch (which
// implicitly closes the trial's draft CI PR). It is a no-op under the test seam, where no real
// git artifacts exist. Best-effort: failures are logged, not fatal.
func (e *Engine) cleanupTrialArtifacts(wm *WorktreeManager, trialName string) {
	if e.trainValidateFn != nil {
		return
	}
	if err := wm.CleanupTrainWorktree(trialName, true); err != nil {
		e.logf(0, "merge-train", "warn: could not clean up trial %s: %v\n", trialName, err)
	}
}

// unmergedPaths returns the paths still in an unmerged state (git status codes UU, AA,
// DD, AU, UD, UA, DU) in workDir. Parsed line-by-line from `git status --porcelain` to
// avoid false positives from file paths that happen to contain "UU"-like substrings.
// conflictedPath pairs an unmerged path with its two-letter `git status --porcelain`
// code (e.g. "UU", "DD"). The code lets callers distinguish an ordinary content
// conflict from one where a side deleted the file — see classifyConflictedPaths.
type conflictedPath struct {
	Path   string
	Status string
}

// conflictedPathNames extracts the Path field from each entry, in order, for callers
// that only need the plain path list (e.g. logging, or a generatedSet membership check
// keyed on path alone).
func conflictedPathNames(paths []conflictedPath) []string {
	if len(paths) == 0 {
		return nil
	}
	names := make([]string, len(paths))
	for i, p := range paths {
		names[i] = p.Path
	}
	return names
}

func unmergedPaths(workDir string) ([]conflictedPath, error) {
	statusCmd := exec.Command("git", "status", "--porcelain")
	statusCmd.Dir = workDir
	statusOut, err := statusCmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git status --porcelain: %w (%s)", err, strings.TrimSpace(string(statusOut)))
	}

	var paths []conflictedPath
	for _, line := range strings.Split(string(statusOut), "\n") {
		if len(line) < 3 {
			continue
		}
		code := line[:2]
		if code == "UU" || code == "AA" || code == "DD" ||
			code == "AU" || code == "UD" || code == "UA" || code == "DU" {
			paths = append(paths, conflictedPath{Path: strings.TrimSpace(line[2:]), Status: code})
		}
	}
	return paths, nil
}

// buildTrainConflictComment constructs a synthetic comment instructing Claude to
// resolve merge conflict markers in the current worktree (inline, without a rebase).
//
// When generatedPaths is non-empty, the conflict is mixed (FR-5): some conflicted
// paths are declared generated files that the engine will regenerate itself, after
// Claude finishes. Claude is instructed to leave those paths alone entirely — not
// edit, stage, or commit them — and to stop once its own (non-generated) part is
// resolved and staged, without committing. The engine performs the final commit
// once regeneration has staged the generated path(s) too (see regenerateAndCommit).
func buildTrainConflictComment(memberItem gh.ProjectItem, prSHA string, generatedPaths []string) gh.Comment {
	if len(generatedPaths) == 0 {
		body := fmt.Sprintf(
			"🏭 **Fabrik merge-train — conflict resolution required**\n\n"+
				"The merge of PR head `%s` (issue #%d) into the trial integration branch has left "+
				"conflict markers in the working tree. Resolve them and commit the resolution.\n\n"+
				"**Instructions:**\n"+
				"1. Run `git status` to identify conflicted files.\n"+
				"2. Open each conflicted file and resolve every `<<<<<<< / ======= / >>>>>>>` marker.\n"+
				"   Resolve **semantically** — understand what each side contributes and produce the "+
				"correct merged result (do not blindly pick one side).\n"+
				"   Watch for **semantic collisions** (two PRs chose the same counter value, migration "+
				"ID, or ADR number): keep both contributions with the correct identifiers.\n"+
				"3. `git add -A` to stage all resolved files.\n"+
				"4. `git commit -m \"chore(merge-train): resolve conflict for #%d\"` to finalize.\n"+
				"5. Run the project's build + test commands (`go build ./...` and `go vet ./...` at minimum).\n"+
				"6. **Do NOT emit `FABRIK_STAGE_COMPLETE`.** The merge-train engine takes over after resolution.\n\n"+
				"If the conflict cannot be resolved safely (ambiguous intent, requires human judgment), "+
				"abort with `git merge --abort` and explain in your response why resolution is not possible.\n",
			prSHA, memberItem.Number, memberItem.Number,
		)
		return gh.Comment{
			ID:         "merge-train-conflict-synthetic",
			DatabaseID: 0,
			Body:       body,
			Author:     "fabrik",
		}
	}

	body := fmt.Sprintf(
		"🏭 **Fabrik merge-train — conflict resolution required**\n\n"+
			"The merge of PR head `%s` (issue #%d) into the trial integration branch has left "+
			"conflict markers in the working tree. Resolve the **non-generated** conflicts and stage "+
			"your resolution — the engine handles the rest.\n\n"+
			"**The following path(s) are generated files and are OUT OF SCOPE — do not edit, stage, or "+
			"commit them. The engine will regenerate them itself once your part is done:**\n%s\n\n"+
			"**Instructions:**\n"+
			"1. Run `git status` to identify conflicted files.\n"+
			"2. Open every conflicted file **except the generated path(s) listed above** and resolve "+
			"every `<<<<<<< / ======= / >>>>>>>` marker.\n"+
			"   Resolve **semantically** — understand what each side contributes and produce the "+
			"correct merged result (do not blindly pick one side).\n"+
			"   Watch for **semantic collisions** (two PRs chose the same counter value, migration "+
			"ID, or ADR number): keep both contributions with the correct identifiers.\n"+
			"3. Stage only the files you resolved (e.g. `git add <file>` per file) — do **NOT** run "+
			"`git add -A` and do **NOT** touch the generated path(s) above.\n"+
			"4. **Do NOT commit.** Leave the merge in progress — the engine finalizes the commit after "+
			"regenerating the generated path(s).\n"+
			"5. Run the project's build + test commands (`go build ./...` and `go vet ./...` at minimum) "+
			"if they don't depend on the generated path(s) above.\n"+
			"6. **Do NOT emit `FABRIK_STAGE_COMPLETE`.** The merge-train engine takes over after resolution.\n\n"+
			"If the non-generated conflict cannot be resolved safely (ambiguous intent, requires human "+
			"judgment), abort with `git merge --abort` and explain in your response why resolution is not "+
			"possible.\n",
		prSHA, memberItem.Number, formatPathList(generatedPaths),
	)
	return gh.Comment{
		ID:         "merge-train-conflict-synthetic",
		DatabaseID: 0,
		Body:       body,
		Author:     "fabrik",
	}
}

// formatPathList renders paths as a markdown bullet list for embedding in a synthetic
// Claude comment.
func formatPathList(paths []string) string {
	var b strings.Builder
	for _, p := range paths {
		b.WriteString("- `")
		b.WriteString(p)
		b.WriteString("`\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// resolveConflictWithClaude invokes Claude inline to resolve merge conflicts in the
// trial branch worktree. Returns (true, nil) if resolution succeeded, (false, nil) if
// the conflict is genuinely unresolvable (the caller ejects the member), or (false,
// non-nil) if resolution could not even be attempted — currently only a
// claudeUsageLimitError (ADR-1120). Callers must treat the two false cases
// differently: an account-wide usage-limit hit is not evidence this member's conflict
// is unresolvable, so it must not be ejected.
//
// generatedPaths, when non-empty, names conflicted paths that are declared generated
// files (FR-5's mixed case): Claude is instructed to leave them untouched and to stop
// once its own part is staged, without committing. In that mode "resolution succeeded"
// means only the non-generated portion is clear of conflict markers — the generated
// path(s) are expected to still be unmerged, the unscoped `git diff --check` and the
// commit are both deferred to regenerateAndCommit, which runs after this returns and
// owns finishing the commit for the whole conflict. When generatedPaths is empty this
// function's behavior is unchanged from before FR-5: it also runs the unscoped check
// and commits the resolution itself.
//
// preMergeHEAD is trainWorkDir's HEAD SHA captured before the failed `git merge` was
// attempted. buildTrainConflictComment's fallback instructions tell Claude to run
// `git merge --abort` when it judges the conflict unresolvable — which clears every
// conflict marker (generated and non-generated alike) exactly as a genuine resolution
// would, making the two indistinguishable from unmergedPaths alone. preMergeHEAD
// disambiguates them: a merge still in progress (MERGE_HEAD present) or a HEAD that has
// moved past preMergeHEAD is genuine progress; a MERGE_HEAD-less worktree still sitting
// on preMergeHEAD means the member's entire contribution — not just the conflicted
// path(s) — was silently discarded by the abort.
func (e *Engine) resolveConflictWithClaude(ctx context.Context, memberItem gh.ProjectItem, trainWorkDir string, holdingStg *stages.Stage, prSHA string, generatedPaths []string, preMergeHEAD string, opts InvokeOptions) (bool, error) {
	if _, suspended := e.claudeSuspendedUntilTime(time.Now()); suspended {
		e.logf(memberItem.Number, "claude-limit", "Claude dispatch suspended account-wide; skipping conflict resolution for #%d\n", memberItem.Number)
		return false, &claudeUsageLimitError{Message: "account usage-limit suspension active"}
	}

	comment := buildTrainConflictComment(memberItem, prSHA, generatedPaths)

	_, _, _, err := e.claude.InvokeForComments(ctx, holdingStg, memberItem, []gh.Comment{comment}, trainWorkDir, opts)
	var limitErr *claudeUsageLimitError
	if errors.As(err, &limitErr) {
		e.activateClaudeSuspension(memberItem.Number, limitErr.ResetTime, time.Now())
		return false, err
	}
	if err != nil {
		// A generic, unrelated error proves nothing about account-wide usage-limit state
		// and must not clear an active suspension (see the matching comment in item.go's
		// runInvocationWithExtension) — only fall through to clear below on success.
		e.logf(memberItem.Number, "merge-train", "Claude conflict resolution failed: %v\n", err)
		return false, nil
	}
	e.clearClaudeSuspension("merge-train conflict resolution reached Claude")

	generatedSet := make(map[string]bool, len(generatedPaths))
	for _, p := range generatedPaths {
		generatedSet[p] = true
	}

	// Check whether conflicts remain after Claude's work — excluding the declared
	// generated path(s), which legitimately remain unmerged awaiting regeneration.
	remaining, err := unmergedPaths(trainWorkDir)
	if err != nil {
		e.logf(memberItem.Number, "merge-train", "could not check for remaining conflicts: %v\n", err)
		return false, nil
	}
	var remainingNonGenerated []string
	for _, p := range remaining {
		if !generatedSet[p.Path] {
			remainingNonGenerated = append(remainingNonGenerated, p.Path)
		}
	}
	if len(remainingNonGenerated) > 0 {
		e.logf(memberItem.Number, "merge-train", "conflict markers remain after Claude resolution: %s\n", strings.Join(remainingNonGenerated, ", "))
		return false, nil
	}

	// Distinguish "Claude resolved the conflict" from "Claude ran `git merge --abort`
	// per the fallback instructions" — both leave zero conflict markers behind, but an
	// abort means none of this member's changes are present at all.
	mergeHeadCmd := exec.Command("git", "rev-parse", "--verify", "MERGE_HEAD")
	mergeHeadCmd.Dir = trainWorkDir
	mergeInProgress := mergeHeadCmd.Run() == nil
	if !mergeInProgress {
		headCmd := exec.Command("git", "rev-parse", "HEAD")
		headCmd.Dir = trainWorkDir
		headOut, headErr := headCmd.Output()
		currentHEAD := strings.TrimSpace(string(headOut))
		if headErr != nil || currentHEAD == preMergeHEAD {
			e.logf(memberItem.Number, "merge-train", "merge for #%d has no remaining conflict markers but MERGE_HEAD is gone and HEAD is unchanged — treating as an abort, not a resolution\n", memberItem.Number)
			return false, nil
		}
	}

	if len(generatedPaths) > 0 {
		// Mixed case: the generated path(s) are still unmerged by design. The unscoped
		// `git diff --check` and the commit are deferred to regenerateAndCommit, which
		// runs next and owns finalizing the single commit across both parts.
		return true, nil
	}

	// Check that there are no staged conflict markers in the diff.
	diffCmd := exec.Command("git", "diff", "--check")
	diffCmd.Dir = trainWorkDir
	if out, diffErr := diffCmd.CombinedOutput(); diffErr != nil {
		e.logf(memberItem.Number, "merge-train", "git diff --check reports conflicts: %s\n", strings.TrimSpace(string(out)))
		return false, nil
	}

	// Verify git considers merge done (index clean or committed).
	checkMergeHead := exec.Command("git", "rev-parse", "--verify", "MERGE_HEAD")
	checkMergeHead.Dir = trainWorkDir
	if err := checkMergeHead.Run(); err == nil {
		// MERGE_HEAD still exists — merge not committed. Try to commit now.
		e.logf(memberItem.Number, "merge-train", "MERGE_HEAD still present after Claude resolution — attempting commit\n")
		addCmd := exec.Command("git", "add", "-A")
		addCmd.Dir = trainWorkDir
		addCmd.CombinedOutput()
		commitCmd := exec.Command("git", "commit", "--no-edit", "-m",
			fmt.Sprintf("chore(merge-train): resolve conflict for #%d", memberItem.Number))
		commitCmd.Dir = trainWorkDir
		if out, commitErr := commitCmd.CombinedOutput(); commitErr != nil {
			e.logf(memberItem.Number, "merge-train", "could not commit resolution: %s\n", strings.TrimSpace(string(out)))
			return false, nil
		}
	}

	return true, nil
}

// regenerationCommandTimeout bounds each declared regeneration command: a hung
// regeneration command would otherwise block the merge-train worker indefinitely with
// no way to eject the member. It is derived from the caller's ctx (see
// regenerateAndCommit), so both a caller-initiated cancellation (e.g. graceful
// shutdown) and this fixed upper bound can end the command promptly — whichever comes
// first. Only one command is declared today (bash scripts/generate-llms-full.sh, local
// and fast), so this is a circuit breaker for future declarations rather than a fix for
// an observed failure.
const regenerationCommandTimeout = 5 * time.Minute

// regenerateAndCommit regenerates each declared generated-file spec's artefact by
// running its regen command (deduplicated so a shared command runs once, not once per
// path), stages the result, verifies the working tree is fully resolved, and commits if
// a merge is still in progress. Returns (true, "") on success, or (false, reason) on any
// failure — resolveTrainConflict's caller must eject the member on failure (FR-4) rather
// than falling through to Claude. By the time this runs, any non-generated portion of
// the conflict has already been resolved by Claude (or there was none), so a failure
// here is specific to the regeneration step itself.
//
// protectedPaths (classifyConflictedPaths's deletionExcluded) names declared generated
// paths that were themselves part of this same conflict but routed to Claude instead of
// regeneration because their status involved a deletion. A command shared between a
// matched path and a protected sibling still regenerates the sibling on disk as a side
// effect — that side effect is discarded rather than staged, so Claude's deletion-aware
// resolution of the protected path is never silently overwritten by way of a command it
// happens to share with an unrelated matched path.
func (e *Engine) regenerateAndCommit(ctx context.Context, memberItem gh.ProjectItem, wtDir string, specs []generatedFileSpec, protectedPaths []string) (bool, string) {
	// MERGE_HEAD must still be present at entry: regenerateAndCommit is only ever
	// called either with the merge conflict still fully in progress (the all-generated
	// case, Claude never invoked) or immediately after resolveConflictWithClaude has
	// Claude resolve-and-stage — but not commit — the non-generated part (the mixed
	// case, FR-5). Its absence here means something already committed prematurely,
	// almost certainly Claude violating the mixed-mode "don't commit" instruction.
	// Detect this directly, structurally, before running any regeneration — rather than
	// relying on a post-hoc `git diff --cached` content comparison, which would miss
	// the (unlikely but possible) case where the premature commit's content happens to
	// byte-match what regeneration would have produced.
	checkMergeHeadAtEntry := exec.Command("git", "rev-parse", "--verify", "MERGE_HEAD")
	checkMergeHeadAtEntry.Dir = wtDir
	if err := checkMergeHeadAtEntry.Run(); err != nil {
		reason := "MERGE_HEAD is already gone before regeneration ran (likely Claude committed despite mixed-mode instructions not to)"
		e.logf(memberItem.Number, "merge-train", "%s\n", reason)
		return false, reason
	}

	// allSpecs is the full declared mapping, not just the conflicted subset in specs. A
	// command shared by multiple declared paths regenerates all of them as a side effect
	// of running once — including any sibling path that isn't part of this conflict and
	// so is absent from specs. Staging must follow that same scope: for each command
	// actually executed, every declared path tied to it is staged, not just the paths in
	// specs. Otherwise a non-conflicted sibling path's on-disk regeneration would be left
	// as an unstaged, uncommitted working-tree change that survives into the next
	// member's `git merge` in the same trial worktree — the tracked-file counterpart of
	// the untracked-file leftover this PR's ejection-cleanup fix already guards against.
	// A sibling in protectedPaths is the one exception: it must never be staged from a
	// shared command's side effect (see the doc comment above) — it goes to pathsToRestore
	// instead.
	allSpecs := e.generatedFileSet()
	protectedSet := make(map[string]bool, len(protectedPaths))
	for _, p := range protectedPaths {
		protectedSet[p] = true
	}

	seenCommands := make(map[string]bool, len(specs))
	var pathsToStage []string
	var pathsToRestore []string
	for _, spec := range specs {
		cmdKey := strings.Join(spec.Command, "\x00")
		if seenCommands[cmdKey] {
			continue
		}
		seenCommands[cmdKey] = true

		if len(spec.Command) == 0 {
			reason := fmt.Sprintf("regeneration for %s has an empty declared command", spec.Path)
			e.logf(memberItem.Number, "merge-train", "%s\n", reason)
			return false, reason
		}
		regenCtx, cancel := context.WithTimeout(ctx, regenerationCommandTimeout)
		regenCmd := exec.CommandContext(regenCtx, spec.Command[0], spec.Command[1:]...)
		regenCmd.Dir = wtDir
		out, err := regenCmd.CombinedOutput()
		// Classify before calling cancel(): cancel() unconditionally cancels regenCtx as
		// part of releasing its resources (the standard non-deferred-cancel idiom), so
		// checking regenCtx.Err() after that point would always report Canceled — even
		// for an ordinary command failure unrelated to any cancellation. ctx.Err() (the
		// caller's original, un-derived context) is unaffected by our own cancel() call,
		// so it alone reliably distinguishes caller-initiated cancellation from
		// regenCtx's own timeout from a plain command failure.
		if err != nil {
			reason := fmt.Sprintf("regeneration command %q failed: %v: %s", strings.Join(spec.Command, " "), err, strings.TrimSpace(string(out)))
			switch {
			case ctx.Err() != nil:
				reason = fmt.Sprintf("regeneration command %q was killed by caller cancellation (e.g. worker shutdown)", strings.Join(spec.Command, " "))
			case regenCtx.Err() == context.DeadlineExceeded:
				reason = fmt.Sprintf("regeneration command %q exceeded its %s timeout and was killed", strings.Join(spec.Command, " "), regenerationCommandTimeout)
			}
			cancel()
			e.logf(memberItem.Number, "merge-train", "%s\n", reason)
			return false, reason
		}
		cancel()

		for _, s := range allSpecs {
			if strings.Join(s.Command, "\x00") != cmdKey {
				continue
			}
			if protectedSet[s.Path] {
				pathsToRestore = append(pathsToRestore, s.Path)
				continue
			}
			pathsToStage = append(pathsToStage, s.Path)
		}
	}

	for _, path := range pathsToRestore {
		// Discard the shared command's side effect on a protected path: if it's still
		// tracked in the index (Claude staged content, including a resolution that
		// keeps the file), restore the working tree from the index. If it's absent
		// from the index (Claude staged its removal via `git rm`), the regenerated
		// file the command just (re)created on disk must not survive either.
		lsCmd := exec.Command("git", "ls-files", "--", path)
		lsCmd.Dir = wtDir
		lsOut, err := lsCmd.Output()
		if err != nil {
			reason := fmt.Sprintf("could not check index state for protected path %s after shared-command regeneration: %v", path, err)
			e.logf(memberItem.Number, "merge-train", "%s\n", reason)
			return false, reason
		}
		if len(strings.TrimSpace(string(lsOut))) > 0 {
			restoreCmd := exec.Command("git", "checkout-index", "-f", "--", path)
			restoreCmd.Dir = wtDir
			if out, err := restoreCmd.CombinedOutput(); err != nil {
				reason := fmt.Sprintf("could not restore protected path %s after shared-command regeneration: %v: %s", path, err, strings.TrimSpace(string(out)))
				e.logf(memberItem.Number, "merge-train", "%s\n", reason)
				return false, reason
			}
		} else if err := os.Remove(filepath.Join(wtDir, path)); err != nil && !os.IsNotExist(err) {
			reason := fmt.Sprintf("could not remove protected path %s recreated by shared-command regeneration: %v", path, err)
			e.logf(memberItem.Number, "merge-train", "%s\n", reason)
			return false, reason
		}
	}

	for _, path := range pathsToStage {
		addCmd := exec.Command("git", "add", "--", path)
		addCmd.Dir = wtDir
		if out, err := addCmd.CombinedOutput(); err != nil {
			reason := fmt.Sprintf("could not stage regenerated %s: %v: %s", path, err, strings.TrimSpace(string(out)))
			e.logf(memberItem.Number, "merge-train", "%s\n", reason)
			return false, reason
		}
	}

	if remaining, err := unmergedPaths(wtDir); err != nil {
		reason := fmt.Sprintf("could not verify merge state after regeneration: %v", err)
		e.logf(memberItem.Number, "merge-train", "%s\n", reason)
		return false, reason
	} else if len(remaining) > 0 {
		reason := fmt.Sprintf("conflict markers remain after regeneration: %s", strings.Join(conflictedPathNames(remaining), ", "))
		e.logf(memberItem.Number, "merge-train", "%s\n", reason)
		return false, reason
	}

	// --cached: scan the staged diff (what will actually be committed) for
	// conflict-marker-like content. A plain `git diff --check` compares working tree to
	// index, which is always empty here since every relevant path was just staged by
	// the loop above — it would never see the content being committed.
	diffCmd := exec.Command("git", "diff", "--cached", "--check")
	diffCmd.Dir = wtDir
	if out, err := diffCmd.CombinedOutput(); err != nil {
		reason := fmt.Sprintf("git diff --cached --check reports conflicts after regeneration: %s", strings.TrimSpace(string(out)))
		e.logf(memberItem.Number, "merge-train", "%s\n", reason)
		return false, reason
	}

	// MERGE_HEAD was confirmed present at entry and nothing above commits, so it is
	// still present here — always finalize the single commit across both parts.
	commitCmd := exec.Command("git", "commit", "--no-edit", "-m",
		fmt.Sprintf("chore(merge-train): resolve conflict for #%d (regenerated generated file(s))", memberItem.Number))
	commitCmd.Dir = wtDir
	if out, err := commitCmd.CombinedOutput(); err != nil {
		reason := fmt.Sprintf("could not commit after regeneration: %s", strings.TrimSpace(string(out)))
		e.logf(memberItem.Number, "merge-train", "%s\n", reason)
		return false, reason
	}

	return true, ""
}

// resolveTrainConflict classifies a merge conflict's paths against the declared
// generated-file set and dispatches accordingly:
//   - A conflict confined entirely to generated paths is regenerated without invoking
//     Claude (FR-1/FR-2).
//   - A conflict with no generated paths dispatches Claude exactly as before FR-1..5.
//   - A mixed conflict (FR-5) dispatches Claude for the non-generated part first, then
//     regenerates. Regeneration must always run last: if a co-conflicted non-generated
//     path is itself one of the generator's own inputs (e.g. one of the four docs/*.md
//     files generate-llms-full.sh reads), regenerating before Claude resolves it would
//     read stale/conflicted source content.
//
// Returns (resolved, reason, err). err carries only the ADR-1120 usage-limit sentinel
// and is otherwise nil — the caller must not eject on a non-nil err (see
// resolveConflictWithClaude's own doc comment). When resolved is false and err is nil,
// reason is a diagnosable message for ejectMember; an empty reason tells the caller to
// fall back to its own generic "unresolvable conflict" message.
func (e *Engine) resolveTrainConflict(ctx context.Context, memberItem gh.ProjectItem, wtDir string, holdingStg *stages.Stage, prSHA string, preMergeHEAD string, opts InvokeOptions) (bool, string, error) {
	paths, err := unmergedPaths(wtDir)
	if err != nil {
		// Can't classify conflicted paths — fall back to the plain Claude path exactly
		// as before this FR-1..5 change introduced generated-path awareness.
		e.logf(memberItem.Number, "merge-train", "could not list conflicted paths, falling back to Claude: %v\n", err)
		resolved, resolveErr := e.resolveConflictWithClaude(ctx, memberItem, wtDir, holdingStg, prSHA, nil, preMergeHEAD, opts)
		return resolved, "", resolveErr
	}

	matched, nonGenerated, deletionExcluded := classifyConflictedPaths(e.generatedFileSet(), paths)

	if len(matched) == 0 {
		// Nothing left for regeneration to do: either no declared generated path is
		// involved at all, or one is (deletionExcluded) but it was routed to Claude
		// because its status carries deletion intent, not a regenerable modification.
		// Either way this is a plain Claude dispatch, matching pre-FR-1..5 behavior.
		resolved, resolveErr := e.resolveConflictWithClaude(ctx, memberItem, wtDir, holdingStg, prSHA, nil, preMergeHEAD, opts)
		return resolved, "", resolveErr
	}

	if len(nonGenerated) == 0 {
		// All conflicted paths are declared generated (FR-1/FR-2) — skip Claude entirely.
		// nonGenerated is a superset of deletionExcluded, so an empty nonGenerated means
		// there are no deletion-excluded siblings to protect either.
		e.logf(memberItem.Number, "merge-train", "conflict for #%d confined to declared generated path(s) — regenerating instead of dispatching Claude\n", memberItem.Number)
		resolved, reason := e.regenerateAndCommit(ctx, memberItem, wtDir, matched, nil)
		return resolved, reason, nil
	}

	// Mixed (FR-5): dispatch Claude for the non-generated part first; regeneration
	// always runs after, never before.
	generatedPathNames := make([]string, len(matched))
	for i, spec := range matched {
		generatedPathNames[i] = spec.Path
	}
	resolved, resolveErr := e.resolveConflictWithClaude(ctx, memberItem, wtDir, holdingStg, prSHA, generatedPathNames, preMergeHEAD, opts)
	if resolveErr != nil || !resolved {
		return resolved, "", resolveErr
	}

	// deletionExcluded (declared generated paths conflicted in this same trial but
	// routed to Claude above due to a deletion-involving status) must be protected from
	// any command matched shares with them — see regenerateAndCommit's doc comment.
	regenResolved, reason := e.regenerateAndCommit(ctx, memberItem, wtDir, matched, deletionExcluded)
	return regenResolved, reason, nil
}

// diagCauseSummary renders a short, name-only summary of a diagnostic's cause — the
// failing check names, the failed required-context names, or the free-text Note —
// for use in the pause-after-N comment (R5), which links to rather than repeats the
// full diagnostic block. Returns "" for a nil diag.
func diagCauseSummary(diag *trainCIDiagnostic) string {
	if diag == nil {
		return ""
	}
	switch {
	case len(diag.FailedChecks) > 0:
		names := make([]string, len(diag.FailedChecks))
		for i, cr := range diag.FailedChecks {
			names[i] = cr.Name
		}
		return strings.Join(names, ", ")
	case len(diag.FailedContexts) > 0:
		return strings.Join(diag.FailedContexts, ", ")
	default:
		return diag.Note
	}
}

// pauseCauseLine composes R5's "name or link the cause" addendum to the pause-after-N
// comment: the failing check/context names (or free-text Note) from diag, plus a
// permalink to the ejection comment just posted (which carries the full diagnostic
// block) when its ID is known. Returns "" — leaving the pause comment's wording exactly
// as it was before #1420 — when diag is nil (an out-of-scope ejection cause) or when
// AddComment failed to report a comment ID.
func pauseCauseLine(diag *trainCIDiagnostic, owner, repo string, issueNumber, commentID int) string {
	if diag == nil {
		return ""
	}
	cause := diagCauseSummary(diag)
	var link string
	if commentID > 0 {
		link = fmt.Sprintf("https://github.com/%s/%s/issues/%d#issuecomment-%d", owner, repo, issueNumber, commentID)
	}
	switch {
	case cause != "" && link != "":
		return fmt.Sprintf("Cause: %s. See the ejection comment above for the full diagnostic: %s", cause, link)
	case cause != "":
		return fmt.Sprintf("Cause: %s.", cause)
	case link != "":
		return fmt.Sprintf("See the ejection comment above for the full diagnostic: %s", link)
	default:
		return ""
	}
}

// ejectRedSingleton disposes of a red batch whose only member is m (#1440 R1/R2): bisection
// exists to isolate a poisoner among two or more members, and there is nothing to isolate in
// a batch of one — a red combined Validate on a true singleton is logically identical to that
// PR's own Validate failing. Unlike ejectMember, this never routes through the shared
// mergeTrainEjectionCounts counter (R3): that counter exists to bound genuine multi-member
// bisection/one-at-a-time churn, and every red-singleton disposition for the same member
// carries identical information, so counting them measures retries of an already-deterministic
// outcome rather than train churn.
//
// #1545 R1/R2: before pausing, m is rerouted off the Queued holding column via
// rerouteQueuedMemberOffHolding — the same primitive and reroute-before-side-effects ordering
// ejectQueuedMemberForReviewFindings (ADR-1208) established for the structurally identical
// review-findings cause. Pausing a HoldingStage item in place left it permanently unreachable:
// itemMayNeedWork excludes HoldingStage items from dispatch, processItem (the comment-unpause
// path) is never reached, and settleQueuedReviewFindings applies the same closed/fabrik:paused
// exclusion — nothing could ever act on the pause without a human manually moving the board
// card. If the reroute fails, nothing is posted and the member is not paused (R2): it looks
// like nothing happened, and the very next poll's train re-forms the same singleton and
// re-hits this same disposition, retrying the whole operation — mirroring
// ejectQueuedMemberForReviewFindings's identical failure behavior.
//
// Unlike the review-findings cause, a standalone combined-Validate failure has no external,
// persistent re-detection signal once rerouted: the failure was only ever observed on the
// synthetic combined trial branch, never on the member's own already-green PR, so nothing
// would re-dispatch a rerouted-but-unpaused member (see ADR-1545). R3 therefore keeps the
// pause — the human gate the original design already relied on — but now on stageBeforeHolding
// (normally Validate), where the pause is actually reachable, and corrects the recovery
// instruction (R4): stage:Validate:complete is already set from this item's original
// completion, so a bare fabrik:paused removal would silently no-op (itemNeedsWork's
// completed-stage check would just skip it again). When the reroute target is literally
// named "Validate", the message instead points at fabrik:revalidate, whose existing
// handler (handleRevalidateLabel) clears stage:Validate:complete alongside
// fabrik:paused/fabrik:awaiting-input/etc. and is what actually makes Validate re-run.
// handleRevalidateLabel is hardcoded to that literal name, not generic over
// stageBeforeHolding's result (unlike stageBeforeHolding itself, which resolves
// structurally by Order) — so when the target isn't literally "Validate" (a config-only
// edge case; production .fabrik/stages/*.yaml always has Validate precede Queued, but
// nothing enforces that, and the test fixture below exercises "Implement" instead), the
// message names the item's real blocking labels directly instead of recommending a
// mechanism that would silently no-op against them.
//
// The posted comment deliberately never uses "ejected" framing, never promises a retry "in a
// future train with a different composition" (there is no different composition possible for
// this member alone), and never attributes the failure to a conflict — it states plainly that
// the PR's own combined Validate is failing and that the fix belongs in the PR, not the train.
// diag is threaded through the same rendering helpers ejectMember uses (renderBatchContext,
// renderDiagnosticBlock) so the failing check(s) are named identically to every other
// merge-train diagnostic (ADR-1420).
func (e *Engine) ejectRedSingleton(projectID, owner, repo string, m trainMember, diag *trainCIDiagnostic) {
	if !e.rerouteQueuedMemberOffHolding(projectID, m.item) {
		e.logf(m.item.Number, "merge-train", "#%d is a red singleton but could not be rerouted off Queued — leaving untouched for retry on the next poll\n", m.item.Number)
		return
	}
	targetName := "the preceding stage"
	if target := stageBeforeHolding(e.cfg, holdingStage(e.cfg)); target != nil {
		targetName = target.Name
	}

	sections := []string{
		fmt.Sprintf("#%d's own combined Validate is failing — this is not a merge-train interaction; the same failure occurs whether or not #%d is combined with any other members.", m.item.Number, m.item.Number),
		renderBatchContext(nil, m.item.Number),
	}
	if block := renderDiagnosticBlock(diag); block != "" {
		sections = append(sections, block)
	}
	sections = append(sections, reentryInstruction(targetName, "This is not a merge-train ejection to retry in a future batch — fix the failing check(s) on this PR"))
	msg := fmt.Sprintf("🏭 **Fabrik merge-train — validation failed**\n\n%s", strings.Join(sections, "\n\n"))

	if _, err := e.client.AddComment(owner, repo, m.item.Number, msg); err != nil {
		e.logf(m.item.Number, "merge-train", "warn: could not post red-singleton comment: %v\n", err)
	}

	e.logf(m.item.Number, "merge-train", "#%d is a red singleton (own validation failing, not a batch interaction) — rerouted to %s and pausing without bisection\n", m.item.Number, targetName)
	e.pauseMergeTrainMember(owner, repo, m.item.Number)
}

// reentryInstruction returns the closing guidance sentence for a merge-train
// escalation comment, explaining how a member that has been rerouted off Queued to
// targetName can re-enter the pipeline. fixDescription is the escalation-specific
// instruction for what to actually go fix before re-entering.
//
// Shared by ejectRedSingleton and the #1615 R4/R5 escalation helpers below — all
// three route a member off Queued to the same stageBeforeHolding target and face the
// same fabrik:revalidate name-literal caveat: handleRevalidateLabel (engine/item.go)
// clears stage:Validate:complete/failed specifically — it is hardcoded to the literal
// name "Validate", not generic over stageBeforeHolding's (Order-derived) result.
// targetName can differ from "Validate" in a custom stage config (trainTestEngine's
// own test fixture resolves it to "Implement" for exactly this reason). Pointing at
// fabrik:revalidate when targetName isn't literally "Validate" would recommend a
// mechanism that silently no-ops against the item's actual completion label,
// reproducing the same class of stranding this issue fixes — so it's only
// recommended when the names actually match; otherwise the real blocking labels are
// named directly.
func reentryInstruction(targetName, fixDescription string) string {
	if targetName == "Validate" {
		return fmt.Sprintf(
			"This issue has left the Queued column for %s, so this pause sits somewhere the pipeline can actually act on it. "+
				"%s, then apply `fabrik:revalidate` (not a bare `fabrik:paused` removal — %s already completed once for this "+
				"item, so removing just `fabrik:paused` would not by itself re-trigger it) to re-enter %s. Once it completes "+
				"again, this issue will re-queue and rejoin the train.",
			targetName, fixDescription, targetName, targetName,
		)
	}
	return fmt.Sprintf(
		"This issue has left the Queued column for %s, so this pause sits somewhere the pipeline can actually act on it. "+
			"%s, then remove `stage:%s:complete` and `fabrik:paused` (`fabrik:revalidate` only forces re-entry of a stage "+
			"literally named Validate, so it will not help here) to re-enter %s. Once it completes again, this issue will "+
			"re-queue and rejoin the train.",
		targetName, fixDescription, targetName, targetName,
	)
}

// ejectMember posts an ejection comment on the member issue, increments the ejection
// counter, and pauses the member after MaxMergeTrainEjections. diag is the combined-Validate
// diagnostic that caused this ejection (R1) — nil for the ejection causes that aren't a
// combined-Validate failure (fetch/head-SHA failures, unresolvable merge conflicts, and
// #1208's unresolved-review-finding cause). otherMembers names the R4 batch context (the
// other members riding in this train attempt); ignored when diag is nil. Every ejection
// comment carries diag's diagnostic, not only the terminal pause comment (R2) — the first
// ejection is exactly as informative as the last.
//
// stayInQueue is true for every pre-#1208 ejection cause (fetch/head-SHA failure,
// unresolvable conflict, bisection isolation): the member has nothing else to do but wait
// for a future train with a different composition, so it stays in the Queued column.
// #1208's new cause passes false — that ejection's whole point is to get the member off
// Queued and back onto a stage the ordinary review-reinvoke path can reach (see
// ejectQueuedMemberForReviewFindings), so the closing sentence must say the opposite of
// the other causes. Callers reroute the member's board Status themselves before calling
// this with stayInQueue=false; this function only varies the comment's wording.
func (e *Engine) ejectMember(owner, repo string, memberItem gh.ProjectItem, reason string, diag *trainCIDiagnostic, otherMembers []trainMember, stayInQueue bool) {
	sections := []string{reason}
	if diag != nil {
		sections = append(sections, renderBatchContext(otherMembers, memberItem.Number))
		if block := renderDiagnosticBlock(diag); block != "" {
			sections = append(sections, block)
		}
	}
	if stayInQueue {
		sections = append(sections, "This issue remains in the Queued column and will be retried in a future train with a different composition.")
	} else {
		sections = append(sections, "This issue has left the Queued column so the unresolved review-thread finding above can be addressed via the normal review pipeline. Once addressed and Validate completes again, it will re-queue and join a later batch.")
	}
	msg := fmt.Sprintf("🏭 **Fabrik merge-train — ejected**\n\n%s", strings.Join(sections, "\n\n"))

	var commentID int
	if id, commentErr := e.client.AddComment(owner, repo, memberItem.Number, msg); commentErr != nil {
		e.logf(memberItem.Number, "merge-train", "warn: could not post ejection comment: %v\n", commentErr)
	} else {
		commentID = id
	}

	counterKey := fmt.Sprintf("%s/%s#%d", owner, repo, memberItem.Number)
	e.mergeTrainEjectionsMu.Lock()
	e.mergeTrainEjectionCounts[counterKey]++
	count := e.mergeTrainEjectionCounts[counterKey]
	e.mergeTrainEjectionsMu.Unlock()

	maxEjections := e.cfg.MaxMergeTrainEjections
	if maxEjections <= 0 {
		maxEjections = 3
	}
	if count >= maxEjections {
		// Reset the counter so that if the user manually unpauses the issue,
		// it gets a fresh set of N attempts before being paused again.
		e.mergeTrainEjectionsMu.Lock()
		e.mergeTrainEjectionCounts[counterKey] = 0
		e.mergeTrainEjectionsMu.Unlock()

		e.logf(memberItem.Number, "merge-train", "#%d ejected %d time(s) — pausing\n", memberItem.Number, count)
		pauseBody := fmt.Sprintf("This issue has been ejected from the merge-train %d consecutive times. "+
			"Manual intervention is required. Remove `fabrik:paused` after resolving the underlying conflict.",
			count)
		if line := pauseCauseLine(diag, owner, repo, memberItem.Number, commentID); line != "" {
			pauseBody = pauseBody + "\n\n" + line
		}
		pauseMsg := fmt.Sprintf("🏭 **Fabrik merge-train — pausing after %d ejections**\n\n%s", count, pauseBody)
		if _, err := e.client.AddComment(owner, repo, memberItem.Number, pauseMsg); err != nil {
			e.logf(memberItem.Number, "merge-train", "warn: could not post pause comment: %v\n", err)
		}
		e.pauseMergeTrainMember(owner, repo, memberItem.Number)
	}
}

// pauseMergeTrainMember applies fabrik:paused and fabrik:awaiting-input to a merge-train
// member being taken out of automated circulation, updating the board cache and
// registering the webhook echo suppression for both labels so a redundant webhook
// delivery for this mutation doesn't double-apply. Extracted from ejectMember's
// cap-reached escalation (unchanged behavior there) and shared with ejectRedSingleton's
// immediate pause (#1440) — both callers pause a member outright, they just differ in
// when they decide to (after N ejections vs. immediately for a self-inflicted red
// singleton). ejectMember's cap-reached call pauses the member in place in Queued
// (deliberately — see ejectMember's stayInQueue doc comment, and #1545's Scope note
// excluding this cap-reached path); ejectRedSingleton's call (#1545) pauses only after
// rerouting the member off Queued, so its pause lands on a reachable, non-HoldingStage
// column instead.
func (e *Engine) pauseMergeTrainMember(owner, repo string, issueNumber int) {
	if err := e.client.AddLabelToIssue(owner, repo, issueNumber, "fabrik:paused"); err != nil {
		e.logf(issueNumber, "warn", "could not add fabrik:paused: %v\n", err)
	} else {
		if c := e.cache(); c != nil {
			c.ApplyLabelAdded(boardcache.ItemKey(owner+"/"+repo, issueNumber), "fabrik:paused")
		}
		if e.webhookMgr != nil {
			e.webhookMgr.RegisterEcho("issues", "labeled", boardcache.ItemKey(owner+"/"+repo, issueNumber)+"+"+"fabrik:paused")
		}
	}
	if err := e.client.AddLabelToIssue(owner, repo, issueNumber, "fabrik:awaiting-input"); err != nil {
		e.logf(issueNumber, "warn", "could not add fabrik:awaiting-input: %v\n", err)
	} else {
		if c := e.cache(); c != nil {
			c.ApplyLabelAdded(boardcache.ItemKey(owner+"/"+repo, issueNumber), "fabrik:awaiting-input")
		}
		if e.webhookMgr != nil {
			e.webhookMgr.RegisterEcho("issues", "labeled", boardcache.ItemKey(owner+"/"+repo, issueNumber)+"+"+"fabrik:awaiting-input")
		}
	}
}

// resetEjectionCount zeroes the per-member ejection counter after a successful landing
// so ejection history from a prior train doesn't count toward the pause cap on a future train.
func (e *Engine) resetEjectionCount(owner, repo string, memberNum int) {
	counterKey := fmt.Sprintf("%s/%s#%d", owner, repo, memberNum)
	e.mergeTrainEjectionsMu.Lock()
	delete(e.mergeTrainEjectionCounts, counterKey)
	e.mergeTrainEjectionsMu.Unlock()
}

// stageBeforeHolding returns the non-Unmanaged stage with the highest Order strictly
// less than hs's Order — the reroute target for a Queued member ejected for an
// unresolved review finding (#1208). Derived structurally by Order rather than
// hardcoded to "Validate", so a custom stage config where the stage immediately
// preceding the holding stage isn't literally named "Validate" is still handled
// correctly — mirroring holdingStage/cleanupStage's own order-based lookup idiom
// above. Returns nil if hs is nil or no such stage exists.
func stageBeforeHolding(cfg Config, hs *stages.Stage) *stages.Stage {
	if hs == nil {
		return nil
	}
	var best *stages.Stage
	for _, s := range cfg.Stages {
		if s.Unmanaged || s.Order >= hs.Order {
			continue
		}
		if best == nil || s.Order > best.Order {
			best = s
		}
	}
	return best
}

// rerouteQueuedMemberOffHolding moves item's board Status from the holding stage
// (Queued) back to the stage stageBeforeHolding resolves (normally Validate) — the
// routing primitive shared by #1208's review-finding ejection cause and #1545's
// standalone-validation-failure cause (ejectRedSingleton). It is deliberately a plain
// status move, nothing else: it must NOT add, remove, or otherwise touch
// stage:Validate:complete (already present from the original Validate completion, and
// never removed by advanceToQueued) or any ReviewCycles counter. For #1208's caller,
// preserving both untouched is what lets the existing MaxReviewCycles-bounded
// review-reinvoke loop pick this member back up "for free" on the very next poll — see
// ejectQueuedMemberForReviewFindings's doc comment and docs/state-machine.md's Queued
// Review-Finding Ejection section. #1545's caller has no equivalent "for free" pickup
// (see ejectRedSingleton's doc comment) and instead re-pauses the member on the newly
// reachable target stage, pointing the recovery instruction at fabrik:revalidate.
//
// Returns false, with no side effect, when the status-field metadata or target stage
// cannot be resolved, or when the status mutation itself fails — the caller must not
// proceed to post an ejection/pause comment or increment any counter in that case, so
// a transient failure here looks like nothing happened and is simply retried whole by
// the next settle scan pass (#1208's caller) or the next poll's train re-formation
// (#1545's caller).
func (e *Engine) rerouteQueuedMemberOffHolding(projectID string, item gh.ProjectItem) bool {
	hs := holdingStage(e.cfg)
	target := stageBeforeHolding(e.cfg, hs)
	if target == nil {
		e.logf(item.Number, "merge-train", "cannot reroute off holding stage — no preceding stage configured\n")
		return false
	}
	if e.statusField == nil {
		e.logf(item.Number, "merge-train", "cannot reroute off holding stage — status field metadata not available\n")
		return false
	}
	optionID, ok := e.statusField.Options[target.Name]
	if !ok {
		e.logf(item.Number, "merge-train", "cannot reroute off holding stage — no status option %q found on project board\n", target.Name)
		return false
	}

	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	if err := e.client.UpdateProjectItemStatus(projectID, item.ItemID, e.statusField.FieldID, optionID); err != nil {
		e.logf(item.Number, "merge-train", "cannot reroute off holding stage — status move to %s failed: %v\n", target.Name, err)
		return false
	}
	if c := e.cache(); c != nil {
		c.UpdateItemStatus(boardcache.ItemKey(owner+"/"+repo, item.Number), target.Name)
	}
	// Advances the probe staleness baseline (#1090), mirroring advanceToQueued/
	// advanceToNextStage — the status move above just bumped the item's real GitHub
	// updatedAt via the project-item mutation.
	e.store.Apply(itemstate.SelfWriteObserved{Repo: owner + "/" + repo, Number: item.Number})
	if e.webhookMgr != nil {
		e.webhookMgr.RegisterEchoIfSubscribed("projects_v2_item", "edited", item.ItemID)
	}

	e.logf(item.Number, "merge-train", "rerouted off %s to %s\n", hs.Name, target.Name)
	return true
}

// ejectQueuedMemberForReviewFindings ejects a Queued merge-train member whose linked
// PR has developed unresolved review-thread feedback while it sat in Queued (#1208) —
// the fourth ejectMember cause, and the only one that must NOT leave the member in
// Queued (see ejectMember's stayInQueue doc comment).
//
// Reroute happens BEFORE the ejection comment/counter, not after: if
// rerouteQueuedMemberOffHolding fails, nothing is posted and nothing is counted, so a
// transient board-mutation failure can never produce a duplicate ejection comment or
// double-count toward MaxMergeTrainEjections — the settle scan simply re-detects the
// same still-unresolved thread on a member still sitting in Queued and retries the
// whole operation on the next poll.
func (e *Engine) ejectQueuedMemberForReviewFindings(projectID string, item gh.ProjectItem, findingCount int) {
	if !e.rerouteQueuedMemberOffHolding(projectID, item) {
		return
	}
	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	reason := fmt.Sprintf(
		"ejected from merge-train — %d unresolved review-thread finding(s) arrived on the linked PR while this issue was Queued.",
		findingCount,
	)
	// diag/otherMembers are nil (no combined-Validate diagnostic exists for this
	// cause, per ADR-1420's contract); stayInQueue is false — this is the one
	// ejectMember cause where the member must leave Queued rather than stay.
	e.ejectMember(owner, repo, item, reason, nil, nil, false)
}

// markPendingReviewEject records that issueNumber (in repoKey) has count unresolved
// review-thread findings and should be ejected at the worker's next checkpoint (#1208),
// rather than immediately — used by settleQueuedReviewFindings when a merge-train
// worker is currently in flight for repoKey, so the ejection is applied by the worker
// goroutine itself (runMergeTrainWorker's re-form loop, landOneAtATime) rather than by
// the settle scan racing that goroutine's own in-memory batch state. Mirrors the
// existing isRunawayTripped/mergeTrainTrials "poll writes a signal, worker consumes it
// at a checkpoint" shape.
func (e *Engine) markPendingReviewEject(repoKey string, issueNumber, count int) {
	e.queuedReviewEjectsMu.Lock()
	defer e.queuedReviewEjectsMu.Unlock()
	if e.queuedReviewEjects[repoKey] == nil {
		e.queuedReviewEjects[repoKey] = make(map[int]int)
	}
	e.queuedReviewEjects[repoKey][issueNumber] = count
}

// takePendingReviewEject returns and clears the pending-eject finding count for
// issueNumber in repoKey, if any. Clearing on read makes this a one-shot signal: once a
// worker checkpoint consumes it, the same flag can't be double-applied by a later
// checkpoint in the same or a subsequent worker run.
func (e *Engine) takePendingReviewEject(repoKey string, issueNumber int) (int, bool) {
	e.queuedReviewEjectsMu.Lock()
	defer e.queuedReviewEjectsMu.Unlock()
	byIssue := e.queuedReviewEjects[repoKey]
	if byIssue == nil {
		return 0, false
	}
	count, ok := byIssue[issueNumber]
	if !ok {
		return 0, false
	}
	delete(byIssue, issueNumber)
	if len(byIssue) == 0 {
		delete(e.queuedReviewEjects, repoKey)
	}
	return count, true
}

// applyPendingReviewEjects checks every member in members for a pending review-finding
// eject signal (#1208) and, for each flagged member, ejects it via
// ejectQueuedMemberForReviewFindings and excludes it from the returned remaining slice.
// Called from inside the merge-train worker goroutine at its natural checkpoints —
// before the #1644 singleton fast path is even considered (a fast-path land never calls
// assembleAndValidate at all, so without this checkpoint it would bypass the other
// three entirely), after assembleAndValidate returns in runMergeTrainWorker's re-form
// loop, inside landOneAtATime's per-singleton loop, and inside landGreenBatch's
// main-moved rebase loop — so a flagged member can never ride a trial (green or
// otherwise), or the fast path, to landing: the caller must discard the current trial
// whenever ejectedCount > 0, regardless of that trial's own CI result.
func (e *Engine) applyPendingReviewEjects(projectID, repoKey string, members []trainMember) (remaining []trainMember, ejectedCount int) {
	for _, m := range members {
		if count, ok := e.takePendingReviewEject(repoKey, m.item.Number); ok {
			e.logf(m.item.Number, "merge-train", "applying pending review-finding eject flagged mid-trial (%d finding(s))\n", count)
			e.ejectQueuedMemberForReviewFindings(projectID, m.item, count)
			ejectedCount++
			continue
		}
		remaining = append(remaining, m)
	}
	return remaining, ejectedCount
}

// effectiveTrialWindow returns the runaway-guard threshold (N) and rolling window (M),
// applying zero-means-default semantics: N=20, M=60min (ADR-059 D8).
func (e *Engine) effectiveTrialWindow() (int, time.Duration) {
	n := e.cfg.MaxTrainTrialsPerWindow
	if n <= 0 {
		n = 20
	}
	m := e.cfg.TrainTrialWindowDuration
	if m <= 0 {
		m = 60 * time.Minute
	}
	return n, m
}

// recordTrial appends a timestamp for repoKey, prunes entries older than the window,
// and returns the current count. Called by assembleAndValidate's wrapper for every
// non-TrainCIGreen outcome — a green result never counts, since it represents progress
// (a landing or a productive bisection narrowing), not the "zero successful lands" signal
// the guard exists to detect (#1528).
func (e *Engine) recordTrial(repoKey string) int {
	_, m := e.effectiveTrialWindow()
	now := time.Now()
	cutoff := now.Add(-m)
	e.mergeTrainTrialsMu.Lock()
	ts := e.mergeTrainTrials[repoKey]
	ts = append(ts, now)
	pruned := ts[:0]
	for _, t := range ts {
		if !t.Before(cutoff) {
			pruned = append(pruned, t)
		}
	}
	e.mergeTrainTrials[repoKey] = pruned
	count := len(pruned)
	e.mergeTrainTrialsMu.Unlock()
	return count
}

// isRunawayTripped returns the current pruned trial count for repoKey and whether it has
// reached the threshold. Called after each trial-producing operation.
func (e *Engine) isRunawayTripped(repoKey string) (int, bool) {
	n, m := e.effectiveTrialWindow()
	cutoff := time.Now().Add(-m)
	e.mergeTrainTrialsMu.Lock()
	ts := e.mergeTrainTrials[repoKey]
	pruned := ts[:0]
	for _, t := range ts {
		if !t.Before(cutoff) {
			pruned = append(pruned, t)
		}
	}
	if len(pruned) == 0 {
		delete(e.mergeTrainTrials, repoKey)
	} else {
		e.mergeTrainTrials[repoKey] = pruned
	}
	count := len(pruned)
	e.mergeTrainTrialsMu.Unlock()
	return count, count >= n
}

// resetTrialCounter clears the trial counter for trainKey after a successful landing,
// so normal poison bisection (where survivors do land) never accumulates toward the cap.
// This is also the runaway guard's own "episode ends" signal: a successful land can only
// happen once the guard is no longer tripped, so it doubles as the boundary at which
// mergeTrainRunawayAlerted's per-member idempotency entries for trainKey are cleared —
// the next trip starts a fresh episode where every member is eligible for a fresh alert
// (#1533). Since #1648 trainKey is the per-(repo,base) composite key (mergeTrainKey), so
// a landing on one base never clears a sibling base's still-accumulating trial counter or
// alert idempotency entries in the same repo.
func (e *Engine) resetTrialCounter(trainKey string) {
	e.mergeTrainTrialsMu.Lock()
	delete(e.mergeTrainTrials, trainKey)
	e.mergeTrainTrialsMu.Unlock()

	prefix := trainKey + "#"
	e.mergeTrainRunawayMu.Lock()
	for key := range e.mergeTrainRunawayAlerted {
		if strings.HasPrefix(key, prefix) {
			delete(e.mergeTrainRunawayAlerted, key)
		}
	}
	e.mergeTrainRunawayMu.Unlock()
}

// runawayGuardAlertMessage builds the explanatory alert comment posted (or retried) for a
// single runaway-guard-paused member. Extracted from fireRunawayGuard so
// settleRunawayGuardAlertScan's retry and escalateRunawayAlertFailure's fallback comment can
// share the identical wording (#1533). trainKey (since #1648, a per-(repo,base) composite
// key) is shown as-is so the alert names exactly which base's train ran out of successful
// landings.
func runawayGuardAlertMessage(count int, trainKey string, window time.Duration) string {
	return fmt.Sprintf("🏭 **Fabrik merge-train — runaway guard tripped**\n\n"+
		"The merge-train has run **%d trial(s)** for `%s` within the last %s "+
		"with **zero successful landings**. This indicates a persistent infra failure "+
		"(e.g. billing-blocked CI, broken base branch, or all required checks erroring) "+
		"rather than a code-composition issue.\n\n"+
		"**Actions taken:** `fabrik:paused` and `fabrik:awaiting-input` applied to all Queued members.\n\n"+
		"**What to do:**\n"+
		"1. Investigate the infra root cause (check GitHub Actions billing, required check configuration, base branch health).\n"+
		"2. Resolve the underlying issue.\n"+
		"3. Manually remove `fabrik:paused` and `fabrik:awaiting-input` from each affected Queued member to re-enable the merge-train.",
		count, trainKey, window)
}

// fireRunawayGuard pauses every member in items and posts an alert comment on each, once per
// member per guard episode. Called from three independent sites — twice inside
// runMergeTrainWorker (Hook 1, the worker goroutine) and once from routeQueuedGroup (Hook 2,
// the poll goroutine) — whenever the trial counter reaches the runaway threshold (ADR-059
// D8). baseBranch identifies which (repo, base) partition tripped — #1648 R2/AC5 requires
// this guard to fire per partition, not repo-wide, so one base's runaway must never pause a
// healthy sibling base's members. Each call site constructs its own, possibly-overlapping
// items slice from whatever local state it holds, and nothing prevents Hook 1 and Hook 2
// from running concurrently for the same trainKey once the shared counter trips (the poll
// loop does not check whether a worker is mid-firing).
//
// The whole pause+alert sequence is therefore a single critical section, serialized by
// mergeTrainRunawayMu, so two concurrent calls can never interleave their loops. Within that
// section, mergeTrainRunawayAlerted (keyed "trainKey#N", trainKey a per-(repo,base)
// composite since #1648) makes re-encountering a member already alerted this episode a
// no-op — the pause labels were applied then too — so a member appearing in two racing
// calls' items slices is never double-alerted (R2/A3). mergeTrainRunawayAlerted is cleared
// per-trainKey by resetTrialCounter, the guard's own "episode ends" signal (a successful
// land) — the next trip starts a fresh episode.
//
// Episode-scoping also has to survive the *other* documented "episode ends" path: the alert
// text itself instructs operators to manually remove fabrik:paused/fabrik:awaiting-input to
// resume the train, and that path never calls resetTrialCounter (#1533 review, finding 2). A
// resumed member can trip the guard again while old trial timestamps are still inside the
// rolling window — the count keeps climbing (it can only climb because new trials actually
// ran, which requires the member to have been un-paused first) — and that second trip must
// still produce a fresh alert. mergeTrainRunawayAlerted therefore records the trial count in
// effect at the time of alerting, not just a boolean: a later call only treats a member as
// already-alerted while its own count is <= the recorded one. Trials cannot accumulate while
// every Queued member stays paused, so within one continuous, un-resumed episode the count
// can only hold steady or fall (as old trials age out of the window) — confirmed by the
// original bug report's own log, where all three log lines of one episode show the identical
// "6 trial(s)". An increase is only possible after an operator-driven resume let new trials
// run, at which point it is exactly the "genuinely new information" the settle-scan family
// exists to surface, not a duplicate of the earlier alert.
//
// A member whose AddComment call fails is NOT marked alerted: it is left with the durable
// fabrik:awaiting-runaway-alert marker instead, which settleRunawayGuardAlertScan retries
// every poll independent of any fireRunawayGuard call ever reaching that member again. This
// closes the residual gap a mutex alone cannot: once fabrik:paused lands, groupQueuedByRepo
// permanently excludes the member from every future items snapshot Hook 2 could construct,
// and Hook 1 only ever knows the members it started with — so a transient comment failure
// here would otherwise strand the member paused with no explanation forever, exactly the
// defect this function exists to close (#1533, R1).
//
// mergeTrainRunawayMu is a single engine-wide mutex, not sharded per repo, and it is held
// across each member's AddComment network call — so a slow or repeatedly-failing comment
// post for one repo's firing does serialize fireRunawayGuard (and settleRunawayGuardAlert's
// retry, which shares this same critical section) for every other repo, and blocks the poll
// goroutine's progress through the rest of that cycle's repo groups. This is a deliberate
// trade-off, not an oversight (flagged in PR review — Pruefer, #1533): the guard fires only
// during genuine runaway incidents (rare, exceptional), so cross-repo contention costs at
// most a few seconds in the worst case, versus the real complexity of a keyed-mutex-with-
// cleanup scheme a per-repo/sync.Map-of-mutexes approach would require. See ADR-1533's
// "Rejected alternatives" section.
func (e *Engine) fireRunawayGuard(ctx context.Context, owner, repo, baseBranch string, items []gh.ProjectItem, count int) {
	_, window := e.effectiveTrialWindow()
	repoKey := owner + "/" + repo
	trainKey := mergeTrainKey(repoKey, baseBranch)
	e.logf(0, "merge-train", "runaway guard fired for %s: %d trial(s) with zero successful lands within %s — pausing %d Queued member(s)\n",
		trainKey, count, window, len(items))

	e.mergeTrainRunawayMu.Lock()
	defer e.mergeTrainRunawayMu.Unlock()

	for _, item := range items {
		alertKey := trainKey + "#" + strconv.Itoa(item.Number)
		if recordedCount, ok := e.mergeTrainRunawayAlerted[alertKey]; ok && count <= recordedCount {
			// Already paused and alerted this episode by an earlier call (Hook 1 or
			// Hook 2, racing for the same member) at this or a higher trial count —
			// skip entirely to avoid a duplicate comment and redundant (though
			// individually idempotent) label calls. A strictly higher count here
			// would mean genuinely new trials ran since the last alert (only
			// possible after an operator resume), which falls through below.
			continue
		}

		// Pause labels are applied before the comment attempt (and thus before
		// markRunawayAlertOutstanding, below): runaway_alert_settle.go's whole
		// design assumes a member carrying fabrik:awaiting-runaway-alert also
		// already carries fabrik:paused (see settleRunawayGuardAlertScan's doc
		// comment) — a crash between the two label writes must not leave the
		// marker applied to a member that was never actually paused (#1533 review).
		e.addLabel(item, "fabrik:paused")
		e.addLabel(item, "fabrik:awaiting-input")

		if _, commentErr := e.postComment(item, runawayGuardAlertMessage(count, trainKey, window), false, true); commentErr != nil {
			e.logf(item.Number, "merge-train", "warn: could not post runaway guard comment: %v — will retry via settle scan\n", commentErr)
			e.markRunawayAlertOutstanding(item, owner, repo)
		} else {
			e.mergeTrainRunawayAlerted[alertKey] = count
			// Best-effort: clears a marker left by an earlier failed attempt for
			// this same member within this episode, if any. Unconditional rather
			// than gated on item.Labels (a snapshot that can be stale relative to
			// a marker a racing call just applied) — RemoveLabelFromIssue is a
			// harmless no-op when the marker isn't actually present.
			e.clearRunawayAlertMarker(item, owner, repo)
		}
	}
}

// mergeTrainBatchMarker is the idempotency marker embedded in integration PR bodies.
const mergeTrainBatchMarker = "<!-- fabrik-merge-train-batch -->"

// buildIntegrationPRTitle returns the title for the landing integration PR.
func buildIntegrationPRTitle(survivors []trainMember) string {
	parts := make([]string, len(survivors))
	for i, m := range survivors {
		parts[i] = fmt.Sprintf("#%d", m.item.Number)
	}
	return "[merge-train] batch: " + strings.Join(parts, ", ")
}

// buildIntegrationPRBody returns the body for the landing integration PR.
// Includes the idempotency marker and a human-readable member list.
func buildIntegrationPRBody(survivors []trainMember) string {
	var lines, closesLines []string
	for _, m := range survivors {
		lines = append(lines, fmt.Sprintf("- #%d — %s", m.item.Number, m.item.Title))
		closesLines = append(closesLines, fmt.Sprintf("Closes #%d", m.item.Number))
	}
	// Closes #N links each member issue to this landing PR and auto-closes them on
	// merge into the default branch (member PRs are closed-not-merged, so their own
	// Closes #N never fires). The landing step also closes them explicitly as a
	// fallback for non-default bases.
	return fmt.Sprintf("🏭 **Fabrik merge-train landing PR**\n\n"+
		"This PR lands the following Queued issues via the internal merge train:\n\n%s\n\n"+
		"%s\n\n%s",
		strings.Join(lines, "\n"), strings.Join(closesLines, "\n"), mergeTrainBatchMarker)
}

// findIntegrationPR searches recent PRs for an existing landing integration PR for
// THIS trial (idempotency check for restarts). trialBranch (fabrik/merge-train/<trialName>)
// is the sole, mandatory identity gate — matching HeadRefName == trialBranch is what
// distinguishes this trial's own draft-CI-PR-turned-integration-PR from every other
// merge-train PR in the repo, live or historical (#1615, R1). ListPRs still requests
// state=all (needed so an already-merged match can still short-circuit FR-2, and so a
// closed-unmerged match can be recognized as a failed trial rather than silently
// invisible), so State/Merged on the returned PR must be inspected by the caller — this
// function only narrows the search to this trial's branch.
//
// mergeTrainBatchMarker is checked too, but only as non-fatal corroboration: a branch
// match is returned regardless of whether the marker is present, with a warning logged
// if it's missing. Requiring the marker as an additional AND-condition would make an
// otherwise-correct branch match invisible in a hypothetical marker-less body — worse,
// not better, for idempotency — so the marker must never be sufficient on its own (R1),
// but it is also never necessary once branch identity has already gated the match.
func (e *Engine) findIntegrationPR(owner, repo, trialBranch string) (*gh.PRDetails, error) {
	prs, err := e.client.ListPRs(owner, repo)
	if err != nil {
		return nil, fmt.Errorf("listing PRs for integration PR search: %w", err)
	}
	for i := range prs {
		if prs[i].HeadRefName != trialBranch {
			continue
		}
		if !strings.Contains(prs[i].Body, mergeTrainBatchMarker) {
			e.logf(0, "merge-train", "warn: PR #%d matches trial branch %s but its body lacks the batch marker — reusing anyway (branch identity is authoritative)\n", prs[i].Number, trialBranch)
		}
		return &prs[i], nil
	}
	return nil, nil
}

// reTrainMember matches "#N" issue references in a train PR body.
var reTrainMember = regexp.MustCompile(`#(\d+)`)

// isTrainPR reports whether pr is a Fabrik merge-train PR, identified
// structurally by its fabrik/merge-train/* head branch (R7, #1615) — every
// genuine trial PR, draft CI PR or promoted landing PR alike, is Fabrik-created
// on that branch (trainBranchPrefix, engine/worktree.go). The shared batch
// marker is never sufficient on its own: a PR body may legitimately quote the
// marker literal in prose for reasons that have nothing to do with being a
// trial PR — this fix's own PR description did exactly that, and the
// marker-OR-branch version of this check let the reconstruct sweep (below)
// close that live, unrelated PR on the strength of the quote alone (#1615's
// own incident, reported by @verveguy). Callers wanting the marker as a
// secondary corroboration signal check pr.Body themselves, as
// reconstructTrainState does; it is never treated as identity here.
func isTrainPR(pr gh.PRDetails) bool {
	return strings.HasPrefix(pr.HeadRefName, trainBranchPrefix)
}

// shouldCloseStaleTrainPR reports whether pr's head ref is a genuine
// fabrik/merge-train/* branch, and therefore safe for reconstructTrainState's
// stale-PR sweep to close (R8, #1615/#1622). It is deliberately *not*
// implemented in terms of isTrainPR, even though the two bodies are
// textually identical: R8 exists to be an independent, directly-testable
// re-confirmation of structural identity immediately before the destructive
// CloseIssue call, not merely inherited from the isTrainPR filter earlier in
// the same loop. Today that filter already guarantees this is unreachable
// with a false result — the value here is that a future change to isTrainPR,
// or to the loop above it, cannot silently re-open the #1615 incident by
// letting an unrecognized PR reach the close call, because this check would
// still catch it independently. Delegating to isTrainPR would collapse the
// two into one check and defeat that purpose.
func shouldCloseStaleTrainPR(pr gh.PRDetails) bool {
	return strings.HasPrefix(pr.HeadRefName, trainBranchPrefix)
}

// trialNameFromBranch strips the fabrik/merge-train/ prefix from a trial branch
// head ref, returning the bare trial name (e.g. "merge-train-main-123"). Returns
// "" when headRef is not a merge-train branch.
func trialNameFromBranch(headRef string) string {
	if !strings.HasPrefix(headRef, trainBranchPrefix) {
		return ""
	}
	return strings.TrimPrefix(headRef, trainBranchPrefix)
}

// parseTrainMembers extracts the distinct member issue numbers referenced as "#N"
// in a train PR body, preserving first-seen order.
func parseTrainMembers(body string) []int {
	var nums []int
	seen := map[int]bool{}
	for _, m := range reTrainMember.FindAllStringSubmatch(body, -1) {
		n, err := strconv.Atoi(m[1])
		if err != nil || seen[n] {
			continue
		}
		seen[n] = true
		nums = append(nums, n)
	}
	return nums
}

// filterBatchByNumbers returns the subset of batch whose issue numbers appear in
// nums, preserving batch (entry) order. Used to intersect a reconstructed PR's
// parsed members with the still-Queued snapshot.
func filterBatchByNumbers(batch []gh.ProjectItem, nums []int) []gh.ProjectItem {
	want := make(map[int]bool, len(nums))
	for _, n := range nums {
		want[n] = true
	}
	var out []gh.ProjectItem
	for _, it := range batch {
		if want[it.Number] {
			out = append(out, it)
		}
	}
	return out
}

// trialBehind reports whether the trial branch has fallen behind its base branch —
// i.e. main advanced (via an external direct push) since the trial forked (ADR-059
// D5, FR-2). It uses the PR-independent GitHub compare API (FetchCommitsBehind), so
// it works under the membership-keyed test seam via the mocked fetchCommitsBehindFn.
func (e *Engine) trialBehind(owner, repo, baseBranch, trialBranch string) bool {
	behind, err := e.client.FetchCommitsBehind(owner, repo, baseBranch, trialBranch)
	if err != nil {
		e.logf(0, "merge-train", "warn: FetchCommitsBehind(%s...%s) failed: %v — assuming up to date\n", baseBranch, trialBranch, err)
		return false
	}
	return behind > 0
}

// classifyLandingCI classifies a landing PR's CI state for pollForMergeable
// (R6, ADR-1441) — the merge-train *landing* counterpart to
// settlePRMergeState/checkCIGate's (engine/pr_settle.go, engine/ci.go)
// per-check classification, and structurally mirroring pollTrainCI's own
// composition (engine/merge_train.go, ADR-1153) so the two pollers reach
// consistent verdicts for the same inputs. Built from the same shared
// primitives pollTrainCI already uses — gh.ClassifyCheckRuns,
// e.classifyRequiredContexts, describeCheckRuns, gh.MergeableStateAccepted —
// rather than a literal shared function with pollTrainCI: pollTrainCI is
// explicitly out of scope for this issue (already fixed by ADR-1153), and
// refactoring it to share a function would mean editing out-of-scope code for
// a cosmetic gain. This is R6's documented resolution of "if a single
// mechanism genuinely cannot serve both, say so explicitly" — the reason is
// scope, not technical infeasibility. Reuses TrainCIResult as the verdict
// type (rather than a parallel enum) since the three outcomes — not yet
// resolved, ready, confirmed-red — are identical in meaning to pollTrainCI's.
//
// dirty is checked by the caller before this is reached (mirrors pollTrainCI
// checking it first). A confirmed check-run failure or required-context
// failure is TrainCIRed unconditionally, regardless of mergeable_state. Zero
// check runs falls back to mergeable_state + required-context — the one case
// where mergeable_state is genuinely load-bearing for green, exactly as in
// pollTrainCI and settlePRMergeState's ADR-933 zero-check-runs branch.
func (e *Engine) classifyLandingCI(owner, repo, mergeableState, headSHA string, checkRuns []gh.CheckRun) (result TrainCIResult, detail string) {
	if len(checkRuns) > 0 {
		status, _, failed := gh.ClassifyCheckRuns(checkRuns)
		if status == gh.CheckRunsFailed {
			return TrainCIRed, fmt.Sprintf("failed check(s): %s", describeCheckRuns(failed))
		}
		if status == gh.CheckRunsReady {
			rcStatus, _, _, rcFailed := e.classifyRequiredContexts(0, owner, repo, headSHA, checkRuns)
			switch rcStatus {
			case gh.RequiredContextsSatisfied:
				return TrainCIGreen, fmt.Sprintf("checks: %s", describeCheckRuns(checkRuns))
			case gh.RequiredContextsFailed:
				return TrainCIRed, fmt.Sprintf("required status context(s) failed: %v", rcFailed)
			}
		}
		// CheckRunsPending, or a required context still pending above: keep polling.
		return TrainCIPending, fmt.Sprintf("checks: %s", describeCheckRuns(checkRuns))
	}

	// Zero check runs (e.g. GitHub Actions disabled — the local-CI-takeover
	// case #933 was filed for): mirrors settlePRMergeState/pollTrainCI's
	// ADR-933 zero-check-runs branch. A confirmed required-context failure
	// blocks regardless of mergeable_state; otherwise an accepted
	// mergeable_state is the only remaining evidence that nothing is
	// outstanding.
	rcStatus, _, _, rcFailed := e.classifyRequiredContexts(0, owner, repo, headSHA, nil)
	if rcStatus == gh.RequiredContextsFailed {
		return TrainCIRed, fmt.Sprintf("required status context(s) failed: %v", rcFailed)
	}
	if gh.MergeableStateAccepted(mergeableState) && rcStatus == gh.RequiredContextsSatisfied {
		return TrainCIGreen, fmt.Sprintf("mergeable_state %q accepted, zero check runs, required contexts satisfied", mergeableState)
	}
	return TrainCIPending, fmt.Sprintf("mergeable_state=%q, zero check runs", mergeableState)
}

// pollForMergeable polls the integration PR until CI is confirmed green —
// per classifyLandingCI (R6, ADR-1441) — blocking up to CIBackstopTimeout.
// Returns true when the PR is ready to merge.
// On timeout, posts a warning comment on the first batch member issue and returns false.
//
// Prior to ADR-1441 this only read mergeable_state and treated
// gh.MergeableStateAccepted (clean/unstable) as an unconditional green light
// — the same defect ADR-1153 already fixed for pollTrainCI, left unfixed here
// and explicitly flagged as a "candidate fast-follow" that never got its own
// issue. It now also fetches and classifies check runs, so a confirmed
// failure on a non-required check (mergeable_state=unstable) blocks landing
// exactly as it now blocks the advance gate — required or not (R5's strict
// policy, mirroring ADR-1153 §4).
//
// ADR-1410 (R6): bounded by CIBackstopTimeout, not the liveness-dwell
// CIWaitTimeout — this is a synchronous blocking poll inside a single
// goroutine, not re-entrant poll-driven state, so "wait indefinitely while
// progressing" would hold the goroutine open for the suite's full duration, a
// cost the async CI gate doesn't pay. It already degrades gracefully on
// timeout (the batch retries next merge-train cycle, no pause), so #342's
// destructive spurious-pause doesn't reproduce here; using the shorter,
// repurposed CIWaitTimeout instead would force a wasted trial-branch rebuild
// every ~30 minutes for a healthy-but-slow suite.
func (e *Engine) pollForMergeable(ctx context.Context, owner, repo string, prNum int, survivors []trainMember) bool {
	deadline := time.Now().Add(e.ciBackstopTimeout())

	for {
		select {
		case <-ctx.Done():
			e.logf(0, "merge-train", "context cancelled while polling integration PR #%d for mergeability\n", prNum)
			return false
		default:
		}

		if time.Now().After(deadline) {
			break
		}

		pr, err := e.client.FetchPRDetails(owner, repo, prNum)
		if err != nil {
			e.logf(0, "merge-train", "warn: FetchPRDetails failed for integration PR #%d: %v\n", prNum, err)
		} else if pr.MergeableState == "dirty" {
			e.logf(0, "merge-train", "integration PR #%d has merge conflict (dirty) — cannot land\n", prNum)
			return false
		} else {
			var checkRuns []gh.CheckRun
			var checkRunsErr error
			if pr.HeadSHA != "" {
				checkRuns, checkRunsErr = e.client.FetchCheckRuns(owner, repo, pr.HeadSHA)
			}
			if checkRunsErr != nil {
				// A fetch failure is "unknown," not "confirmed zero check
				// runs" — passing checkRuns=nil into classifyLandingCI here
				// would hit its zero-check-runs fallback and could let an
				// accepted mergeable_state (unstable) through as green while
				// a real, unobserved check-run failure sits on the head SHA.
				// Skip classification for this iteration instead (mirrors
				// pollTrainCI's identical if/else-if/else shape) and retry
				// next poll.
				e.logf(0, "merge-train", "warn: FetchCheckRuns failed for integration PR #%d: %v\n", prNum, checkRunsErr)
			} else {
				switch verdict, detail := e.classifyLandingCI(owner, repo, pr.MergeableState, pr.HeadSHA, checkRuns); verdict {
				case TrainCIGreen:
					e.logf(0, "merge-train", "integration PR #%d ready to land — %s\n", prNum, detail)
					return true
				case TrainCIRed:
					e.logf(0, "merge-train", "integration PR #%d not mergeable — %s\n", prNum, detail)
					return false
				default: // TrainCIPending — keep polling
					e.logf(0, "merge-train", "integration PR #%d not yet ready — %s\n", prNum, detail)
				}
			}
		}

		if time.Now().After(deadline) {
			break
		}

		select {
		case <-ctx.Done():
			return false
		case <-time.After(e.trainCIPollIntervalOrDefault()):
		}
	}

	// Note (#1615, deliberately out of scope): if the PR was open when found by
	// findIntegrationPR but closes (unmerged) mid-poll, this loop never observes
	// pr.State and just times out here like any other non-green result — it does
	// not route to the R5 escalation path, since #1615's R5 is anchored on a PR
	// already closed-unmerged when findIntegrationPR returns it, not one that
	// transitions during this poll. A narrower, related gap for a future issue if
	// ever observed in production; unlike the fixed bug, it can't misattribute a
	// landing — it only leaves members in Queued for a fresh retry.
	e.logf(0, "merge-train", "timed out waiting for integration PR #%d to become mergeable\n", prNum)
	if len(survivors) > 0 {
		msg := fmt.Sprintf("🏭 **Fabrik merge-train — landing timeout**\n\n"+
			"Timed out waiting for integration PR #%d to reach a mergeable state (`clean` or `unstable`). "+
			"Batch members remain in the Queued column and will be retried in the next train cycle.\n\n"+
			"Possible causes: branch protection checks are slow, or the base branch has advanced "+
			"and the integration PR needs rebasing.",
			prNum)
		if _, commentErr := e.client.AddComment(owner, repo, survivors[0].item.Number, msg); commentErr != nil {
			e.logf(0, "merge-train", "warn: could not post timeout comment: %v\n", commentErr)
		}
	}
	return false
}

// escalateStrandedTrainMember reroutes a single merge-train member off Queued back to
// its prior stage, posts an explanatory comment naming reason, and pauses it
// (fabrik:paused + fabrik:awaiting-input) — the fail-loud escalation #1615's R4/R5
// require for a landing that this member cannot ride to Done: either the trial's own
// integration PR closed unmerged (R5), or a matched integration PR's body doesn't
// actually list this member (R4). Mirrors ejectRedSingleton's reroute+comment+pause
// shape exactly (#1545): this is an infra-level landing failure, not a retryable
// member-level defect, so it bypasses ejectMember's counter/stay-in-Queued semantics
// entirely.
//
// If the reroute itself fails (no preceding stage, missing status option, API error),
// nothing is posted and nothing is paused: a failed reroute must look like nothing
// happened, so the very next poll's train re-forms this member and retries the whole
// disposition from scratch (mirrors ejectRedSingleton's identical failure behavior).
func (e *Engine) escalateStrandedTrainMember(projectID, owner, repo string, item gh.ProjectItem, reason string) {
	if !e.rerouteQueuedMemberOffHolding(projectID, item) {
		e.logf(item.Number, "merge-train", "#%d could not be rerouted off Queued for landing-failure escalation — leaving untouched for retry on the next poll\n", item.Number)
		return
	}

	targetName := "the preceding stage"
	if target := stageBeforeHolding(e.cfg, holdingStage(e.cfg)); target != nil {
		targetName = target.Name
	}

	sections := []string{
		reason,
		reentryInstruction(targetName, "No action is needed on this issue's own code — the merge-train landing step itself failed"),
	}
	msg := fmt.Sprintf("🏭 **Fabrik merge-train — landing failed**\n\n%s", strings.Join(sections, "\n\n"))

	if _, err := e.client.AddComment(owner, repo, item.Number, msg); err != nil {
		e.logf(item.Number, "merge-train", "warn: could not post landing-failure comment: %v\n", err)
	}

	e.logf(item.Number, "merge-train", "#%d rerouted off Queued to %s and paused after a landing failure: %s\n", item.Number, targetName, reason)
	e.pauseMergeTrainMember(owner, repo, item.Number)
}

// escalateClosedUnmergedTrial handles the R5 case: findIntegrationPR matched THIS
// trial's own PR (branch identity confirmed) but it is closed and unmerged — a failed
// trial, never a reusable integration PR (R2). Every survivor is escalated via
// escalateStrandedTrainMember; none of the FR-3 advance/close/"Landed via" sequence
// ever runs for any of them (R5 — no Done advance, no landed comment, no PR/issue
// close). Members already in Done are skipped (restart safety, mirrors FR-3's own
// Done-skip) — they landed in an earlier pass and this trial's later failure doesn't
// retroactively strand them.
func (e *Engine) escalateClosedUnmergedTrial(projectID, owner, repo string, prNum int, survivors []trainMember) {
	reason := fmt.Sprintf(
		"This trial's own integration PR #%d closed without merging. The batch never landed — "+
			"nothing was merged, and this issue's own PR and branch are untouched.",
		prNum,
	)
	for _, m := range survivors {
		if m.item.Status == "Done" {
			e.logf(m.item.Number, "merge-train", "#%d already in Done column — skipping closed-unmerged-trial escalation\n", m.item.Number)
			continue
		}
		e.escalateStrandedTrainMember(projectID, owner, repo, m.item, reason)
	}
}

// landMergeTrainBatch executes FR-1 through FR-5 after a green CI result:
// opens (or finds) the integration PR, polls until mergeable, merges, advances
// each member to Done and closes their PRs, then cleans up trial artifacts.
// baseBranch is the target branch for the integration PR (already known from
// runMergeTrainWorker) — the real git branch name, used for the PR itself.
// trainKey (#1648) is the worker's fixed (repo,partition) guard key (p.trainKey at
// every call site) — used only for resetTrialCounter below, and deliberately NOT
// derived from baseBranch here: for a default-base partition baseBranch is a real
// resolved name (e.g. "main") but trainKey's own base segment is the empty-string
// sentinel groupQueuedByRepoAndBase never resolves via git — see trialParams's doc
// comment for why the two must never be conflated.
func (e *Engine) landMergeTrainBatch(ctx context.Context, state *mergeTrainWorkerState, owner, repo, baseBranch, trainKey string, survivors []trainMember, wm *WorktreeManager) {
	repoKey := owner + "/" + repo
	trialName := state.trialName

	defer func() {
		// FR-4: cleanup trial worktree and remote branch regardless of landing outcome.
		// The in-flight marker itself is cleared by runMergeTrainWorker's top-level
		// defer, not here — see ADR-067.
		if cleanErr := wm.CleanupTrainWorktree(trialName, true); cleanErr != nil {
			e.logf(0, "merge-train", "warn: cleanup trial worktree for %s failed: %v\n", repoKey, cleanErr)
		}
	}()

	trialBranch := "fabrik/merge-train/" + trialName

	// FR-1 / FR-5: find or create the landing integration PR. findIntegrationPR now
	// gates on trialBranch identity (R1) — the returned PR, if any, is guaranteed to
	// be THIS trial's own, never a different batch's.
	integrationPR, err := e.findIntegrationPR(owner, repo, trialBranch)
	if err != nil {
		e.logf(0, "merge-train", "cannot search for existing integration PR for %s: %v\n", repoKey, err)
		return
	}

	// R2/R5: a PR found on this trial's own branch that is closed and unmerged is a
	// failed trial, not a reusable integration PR — it must never be treated as
	// "already landed." Escalate every survivor (fail loud) and stop before any of
	// the draft/merge/advance logic below runs.
	if integrationPR != nil && integrationPR.State == "closed" && !integrationPR.Merged {
		e.logf(0, "merge-train", "integration PR #%d for %s is closed and unmerged — trial failed to land, escalating %d survivor(s)\n", integrationPR.Number, repoKey, len(survivors))
		e.escalateClosedUnmergedTrial(state.projectID, owner, repo, integrationPR.Number, survivors)
		return
	}

	var integrationPRNum int
	var alreadyMerged bool
	var prBody string

	if integrationPR != nil {
		integrationPRNum = integrationPR.Number
		alreadyMerged = integrationPR.Merged
		prBody = integrationPR.Body
		e.logf(0, "merge-train", "found existing integration PR #%d (merged=%v, draft=%v) for %s\n", integrationPRNum, alreadyMerged, integrationPR.Draft, repoKey)
		// The reused PR is the trial's draft CI PR — promote it to ready-for-review
		// so it can be merged (GitHub refuses to merge a draft).
		if integrationPR.Draft && !alreadyMerged {
			if rerr := e.client.MarkPRReady(owner, repo, integrationPRNum); rerr != nil {
				e.logf(0, "merge-train", "cannot mark integration PR #%d ready for %s: %v — leaving members in Queued\n", integrationPRNum, repoKey, rerr)
				return
			}
			e.logf(0, "merge-train", "marked integration PR #%d ready for landing (%s)\n", integrationPRNum, repoKey)
		}
	} else {
		// FR-1: open the landing integration PR (not a draft).
		title := buildIntegrationPRTitle(survivors)
		body := buildIntegrationPRBody(survivors)
		prBody = body
		integrationPRNum, err = e.client.CreatePR(owner, repo, title, trialBranch, baseBranch, body)
		if err != nil {
			e.logf(0, "merge-train", "cannot create integration PR for %s: %v\n", repoKey, err)
			return
		}
		e.logf(0, "merge-train", "opened integration PR #%d for %s (%d survivors)\n", integrationPRNum, repoKey, len(survivors))
	}

	// FR-2: poll until mergeable, then merge (skip if already merged).
	if !alreadyMerged {
		if !e.pollForMergeable(ctx, owner, repo, integrationPRNum, survivors) {
			// Timeout or dirty — leave members in Queued.
			return
		}

		if err := e.client.MergePR(owner, repo, integrationPRNum); err != nil {
			e.logf(0, "merge-train", "merge of integration PR #%d failed: %v\n", integrationPRNum, err)
			msg := fmt.Sprintf("🏭 **Fabrik merge-train — merge failure**\n\n"+
				"Failed to merge integration PR #%d: %v\n\n"+
				"Batch members remain in the Queued column. Manual intervention may be required.",
				integrationPRNum, err)
			if len(survivors) > 0 {
				if _, commentErr := e.client.AddComment(owner, repo, survivors[0].item.Number, msg); commentErr != nil {
					e.logf(0, "merge-train", "warn: could not post merge-failure comment: %v\n", commentErr)
				}
			}
			return
		}
		e.logf(0, "merge-train", "merged integration PR #%d for %s\n", integrationPRNum, repoKey)
	}

	// FR-3: advance each member from Queued → Done and close their PR.
	holdingStg := holdingStage(e.cfg)
	if holdingStg == nil {
		e.logf(0, "merge-train", "no holding stage — cannot advance members to Done\n")
		return
	}
	board := &gh.ProjectBoard{ProjectID: state.projectID}

	// R4: before attributing a landing to integrationPRNum, verify each member is
	// actually claimed by its body — a batch that dropped a member during assembly
	// must not be able to claim it. Computed once, outside the loop.
	claimed := parseTrainMembers(prBody)
	claimedSet := make(map[int]bool, len(claimed))
	for _, n := range claimed {
		claimedSet[n] = true
	}

	for _, m := range survivors {
		// Skip members already in Done column (restart safety).
		if m.item.Status == "Done" {
			e.logf(m.item.Number, "merge-train", "#%d already in Done column — skipping\n", m.item.Number)
			// Still reset the ejection counter: this member landed successfully.
			e.resetEjectionCount(owner, repo, m.item.Number)
			// #1616: still record the landing-verification markers. A prior run
			// can have advanced this member to Done and then died before
			// markCreditedLanding ran — advanceToNextStage and the label writes
			// are separate, non-atomic API calls. Skipping the marker here would
			// leave that item permanently unverified, the backstop silently
			// absent for exactly the merge-train restart shape #1614 came from,
			// and nothing would ever revisit a Done item to notice. Both writes
			// are idempotent, so re-marking an item whose verification already
			// completed costs at most one redundant FetchPRMerged that confirms
			// merged and clears the markers again.
			e.markCreditedLanding(m.item, integrationPRNum)
			continue
		}

		// R4: the integration PR's body doesn't list this member — never attribute
		// a landing to a batch that dropped it. Escalate (fail loud) instead of
		// silently skipping, since a batch losing a member during assembly should
		// never happen and is worth a human look.
		if !claimedSet[m.item.Number] {
			e.logf(m.item.Number, "merge-train", "#%d not referenced in integration PR #%d's body — batch dropped this member, escalating\n", m.item.Number, integrationPRNum)
			reason := fmt.Sprintf(
				"Integration PR #%d landed, but its body doesn't reference this issue. This batch dropped this member — "+
					"nothing was landed on its behalf, and this issue's own PR and branch are untouched.",
				integrationPRNum,
			)
			e.escalateStrandedTrainMember(state.projectID, owner, repo, m.item, reason)
			continue
		}

		// Advance Queued → Done.
		if e.statusField == nil {
			e.logf(m.item.Number, "merge-train", "warn: statusField not available — cannot advance #%d to Done\n", m.item.Number)
		} else if advErr := e.advanceToNextStage(board, m.item, holdingStg); advErr != nil {
			e.logf(m.item.Number, "merge-train", "warn: could not advance #%d to Done: %v\n", m.item.Number, advErr)
		} else {
			e.logf(m.item.Number, "merge-train", "advanced #%d to Done\n", m.item.Number)
			// #1616: record the integration PR credited for this Done transition and
			// mark the item for post-Done landing verification. See
			// markCreditedLanding for why a failed credited-PR write skips the
			// marker outright rather than leaving it to the scan's fallback.
			e.markCreditedLanding(m.item, integrationPRNum)
		}

		// Close member PR with a comment citing the integration PR.
		if m.prNum != 0 {
			landedComment := fmt.Sprintf("🏭 **Fabrik merge-train** — Landed via batch PR #%d.", integrationPRNum)
			e.addLandedCommentWithRetry(owner, repo, m.item.Number, m.prNum, landedComment)
			if closeErr := e.client.CloseIssue(owner, repo, m.prNum); closeErr != nil {
				e.logf(m.item.Number, "merge-train", "warn: could not close member PR #%d: %v\n", m.prNum, closeErr)
			} else {
				e.logf(m.item.Number, "merge-train", "closed member PR #%d\n", m.prNum)
			}
		}

		// Close the member issue. The integration PR's Closes #N auto-closes it on
		// merge into the default branch; this explicit close is the fallback for
		// non-default bases and any auto-close lag (idempotent — no-op if already
		// closed). The member PR is closed-not-merged, so its own Closes #N never
		// fires — without this the issue is left landed-but-open.
		if closeErr := e.client.CloseIssue(owner, repo, m.item.Number); closeErr != nil {
			e.logf(m.item.Number, "merge-train", "warn: could not close member issue #%d: %v\n", m.item.Number, closeErr)
		} else {
			e.logf(m.item.Number, "merge-train", "closed member issue #%d\n", m.item.Number)
		}

		// Reset ejection counter: this member has landed; prior ejection history
		// from earlier trains must not count toward the pause cap on future trains.
		e.resetEjectionCount(owner, repo, m.item.Number)
	}
	e.resetTrialCounter(trainKey)
	e.logf(0, "merge-train", "landing complete for %s (integration PR #%d, %d members)\n", repoKey, integrationPRNum, len(survivors))
}

// dissolveBatch tears down an in-flight batch and returns every member to the
// Queued column untouched (ADR-059 D5, FR-5). It closes the integration/CI PR (if
// open), deletes the trial branch locally and on origin, and posts an explanatory
// comment on each member so the outcome is observable. The in-flight marker itself
// is cleared by the caller's chain back to runMergeTrainWorker or prepareTrainWorker,
// not by dissolveBatch (ADR-067). Members are never status-rolled-back — they only
// advance to Done on a successful landing, so leaving them in Queued needs no
// mutation. The next poll re-snapshots Queued and forms a fresh train.
//
// Idempotent: CloseIssue on an already-closed PR and CleanupTrainWorktree on an
// already-deleted branch are best-effort no-ops, so a crash mid-dissolve is safe
// to retry (the explanatory comment may double-post — acceptable and observable).
func (e *Engine) dissolveBatch(state *mergeTrainWorkerState, p trialParams, prNum int, trialName string, members []gh.ProjectItem, reason string) {
	repoKey := p.owner + "/" + p.repo
	e.logf(0, "merge-train", "dissolving batch for %s (%s) — %d member(s) remain in Queued\n", repoKey, reason, len(members))

	// Close the integration/CI PR if we have one (PRs are issues; no ClosePR).
	if prNum != 0 {
		if err := e.client.CloseIssue(p.owner, p.repo, prNum); err != nil {
			e.logf(0, "merge-train", "warn: could not close PR #%d during dissolve: %v\n", prNum, err)
		}
	}

	// Delete the trial branch locally and on origin (also closes the draft CI PR).
	if trialName != "" {
		e.cleanupTrialArtifacts(p.wm, trialName)
	}

	// Post an explanatory comment on each member for observability (FR-5).
	msg := fmt.Sprintf("🏭 **Fabrik merge-train — batch dissolved**\n\n%s\n\n"+
		"This issue remains in the Queued column, untouched, and will be picked up by a fresh train on the next poll.",
		reason)
	for _, m := range members {
		if _, err := e.client.AddComment(p.owner, p.repo, m.Number, msg); err != nil {
			e.logf(m.Number, "merge-train", "warn: could not post dissolve comment: %v\n", err)
		}
	}

	// The in-flight marker itself is cleared by runMergeTrainWorker's top-level
	// defer, not here — see ADR-067.
}

// membersToItems projects a []trainMember down to the underlying []gh.ProjectItem.
func membersToItems(members []trainMember) []gh.ProjectItem {
	items := make([]gh.ProjectItem, len(members))
	for i, m := range members {
		items[i] = m.item
	}
	return items
}

// landGreenBatch is the landing gate with main-moved recovery (ADR-059 D5,
// FR-2/FR-6). Before merging, it checks whether the validated-green trial branch
// has fallen behind its base (an external direct push advanced main). If up to
// date, it delegates to landMergeTrainBatch unchanged. If behind, it re-pins the
// base to the current origin/<base> and re-assembles+re-validates the survivors
// off the new base (reusing assembleAndValidate, which invokes Claude conflict
// resolution for FR-6), bounded by MaxTrainRebaseCycles. On a green re-validation
// it loops back to the gate; on exhaustion, a non-green re-validation, or an
// assembly wipeout it dissolves the batch (FR-5) and lets the next poll re-form.
//
// The rebase path is deliberately disjoint from red-batch bisection: a red
// re-validation here dissolves rather than bisecting, so the rebase-cycle budget
// and the bisection cost cap never interact (compose-not-duplicate).
func (e *Engine) landGreenBatch(ctx context.Context, state *mergeTrainWorkerState, p trialParams, survivors []trainMember) {
	maxCycles := e.effectiveMaxTrainRebaseCycles()
	cycles := 0

	for {
		state.mu.RLock()
		trialName := state.trialName
		prNum := state.prNum
		state.mu.RUnlock()
		trialBranch := trainBranchPrefix + trialName

		if !e.trialBehind(p.owner, p.repo, p.baseBranch, trialBranch) {
			// Up to date: land via the unchanged terminal path (clears the map).
			e.landMergeTrainBatch(ctx, state, p.owner, p.repo, p.baseBranch, p.trainKey, survivors, p.wm)
			return
		}

		// Main moved under the batch.
		if cycles >= maxCycles {
			e.dissolveBatch(state, p, prNum, trialName, membersToItems(survivors),
				fmt.Sprintf("the base branch advanced under the batch and it still could not catch up after %d rebase attempt(s) (main-moved rebase limit)", maxCycles))
			return
		}
		cycles++
		e.logf(0, "merge-train", "trial %s is behind %s (main moved) — rebasing off the new base (cycle %d/%d)\n", trialName, p.baseBranch, cycles, maxCycles)

		// Re-pin the base to the current origin/<base> so the re-assembly forks off
		// the advanced main (skipped under the test seam — no real git).
		if e.trainValidateFn == nil {
			fetchCmd := exec.Command("git", "fetch", "origin")
			fetchCmd.Dir = p.wm.baseDir
			fetchCmd.Env = nonInteractiveGitEnv()
			fetchCmd.CombinedOutput() // best-effort
			if sha, rerr := gitRevParse(p.wm.baseDir, "refs/remotes/origin/"+p.baseBranch); rerr == nil {
				p.baseSHA = sha // local copy; the loop reuses it, no leak to caller
			}
		}

		// Clean up the now-stale (behind) trial before building the next one.
		oldTrialName := trialName
		newTrialName := p.nextTrialName()
		state.mu.Lock()
		state.trialName = newTrialName
		state.assembling = true
		state.mu.Unlock()

		// The rebase re-validate's diagnostic is intentionally discarded: a non-green
		// result here dissolves the batch (see below), and dissolveBatch's messaging is
		// out of this issue's scope (#1420) — only ejectMember's comments are in scope.
		newSurvivors, result, newPRNum, _, aerr := e.assembleAndValidate(ctx, p, survivors, newTrialName)
		e.cleanupTrialArtifacts(p.wm, oldTrialName)

		state.mu.Lock()
		state.prNum = newPRNum
		state.assembling = false
		state.CIResult = result
		state.mu.Unlock()

		if aerr != nil || len(newSurvivors) == 0 {
			e.dissolveBatch(state, p, newPRNum, newTrialName, membersToItems(survivors),
				"the base branch advanced and the batch could not be re-assembled onto it")
			return
		}

		// Hook 2 (landing loop): apply any pending review-finding ejects flagged
		// while this rebase cycle's assemble/validate was running (#1208). This
		// loop is the one place a green trial can spend a second full CI wait
		// (up to CIBackstopTimeout, ADR-1410) without ever returning control to the outer
		// re-form loop in runMergeTrainWorker, where the primary Hook 2 lives —
		// so a finding arriving during a main-moved rebase would otherwise ride
		// the newly-green trial straight to landMergeTrainBatch. Discard the
		// whole trial and stop: the (non-flagged) remaining survivors are still
		// sitting in Queued untouched, so they simply re-form fresh on the next
		// poll's dispatchMergeTrainWorker — the same "checkpoint, not continuous
		// preemption" granularity the runaway guard and the other Hook 2 already
		// have, rather than threading a resume-with-reduced-membership path back
		// into this loop.
		if _, ejectedCount := e.applyPendingReviewEjects(state.projectID, p.owner+"/"+p.repo, newSurvivors); ejectedCount > 0 {
			e.logf(0, "merge-train", "%d member(s) ejected for unresolved review findings during main-moved rebase for %s/%s — discarding trial; remaining survivors will re-form on a future poll\n", ejectedCount, p.owner, p.repo)
			e.cleanupTrialArtifacts(p.wm, newTrialName)
			return
		}

		if result != TrainCIGreen {
			// A red/pending re-validation after a rebase dissolves (disjoint from
			// bisection); the next poll re-forms a fresh train that bisects cleanly.
			e.dissolveBatch(state, p, newPRNum, newTrialName, membersToItems(newSurvivors),
				"the base branch advanced and the re-validated batch was no longer green")
			return
		}
		survivors = newSurvivors // green off the new base — loop back to the gate
	}
}

// reconstructTrainState makes the per-repo in-flight guard durable and restart-safe
// (ADR-059 D5, FR-1/FR-4). Running inside the already-guarded worker goroutine
// (after LoadOrStore, before base pinning), it probes durable artifacts — merge-train
// PRs (via ListPRs) and fabrik/merge-train/* origin branches (via ls-remote) — and
// routes based on the train PR whose members are still in the current Queued snapshot
// (historical PRs from prior completed batches are skipped so they cannot abort or
// corrupt today's fresh batch). It returns true only when it has fully handled an
// in-flight batch — the caller (prepareTrainWorker) treats a true return as its own
// failure path and clears the in-flight marker itself (ADR-067):
//
//   - merged landing PR (batch marker) with members still Queued → complete the
//     deferred member lifecycle (idempotent landMergeTrainBatch advancement);
//   - open train PR backed by an origin branch → resume (poll CI, then land with
//     main-moved recovery);
//   - open PR (with still-Queued members) without a backing branch → dissolve (FR-5);
//   - nothing relevant, or only orphaned remnants → clean any orphaned trial branches
//     silently and return false so the caller forms a fresh train this poll.
//
// It reads only durable state (never the in-flight map) and never launches a
// goroutine, so it survives a restart with an empty map without a duplicate worker.
func (e *Engine) reconstructTrainState(ctx context.Context, state *mergeTrainWorkerState, p trialParams, batch []gh.ProjectItem) bool {
	repoKey := p.owner + "/" + p.repo

	prs, err := e.client.ListPRs(p.owner, p.repo)
	if err != nil {
		e.logf(0, "merge-train", "reconstruct: ListPRs failed for %s: %v — proceeding fresh\n", repoKey, err)
		return false
	}

	// Select the first train PR *relevant to the current Queued members*. ListPRs
	// returns state=all, so it also surfaces merged/closed integration PRs from prior
	// completed batches; those still carry the batch marker but have no members left
	// in today's Queued snapshot (members only leave Queued on a successful land).
	// Reconstructing from such a historical PR would wrongly abort today's fresh batch
	// (complete-deferred finds no still-Queued members and exits early), permanently
	// stalling the train after the first landing — so skip it. A stale *open* train PR
	// (no still-Queued members) is closed and its branch cleaned so it cannot later
	// hijack findIntegrationPR during a fresh batch's landing.
	var trainPR *gh.PRDetails
	for i := range prs {
		if !isTrainPR(prs[i]) {
			continue
		}
		if !strings.Contains(prs[i].Body, mergeTrainBatchMarker) {
			// Expected for a draft CI PR not yet promoted to landing PR — the marker
			// is corroboration only (R7, #1615), never required; branch identity above
			// is what makes this a train PR.
			e.logf(0, "merge-train", "reconstruct: PR #%d matches train branch %s but its body lacks the batch marker — branch identity is authoritative, continuing\n", prs[i].Number, prs[i].HeadRefName)
		}
		if len(filterBatchByNumbers(batch, parseTrainMembers(prs[i].Body))) > 0 {
			trainPR = &prs[i]
			break
		}
		if prs[i].State == "open" {
			// R8 (#1615): a destructive action — closing this PR — requires positive
			// structural identity re-confirmed at the point of the action itself, not
			// just inherited from the isTrainPR filter above. This is intentionally
			// redundant with that filter today; it exists so a future change to the
			// filter (or to this loop) cannot silently re-open the #1615 incident by
			// letting an unrecognized PR reach the close call. Ambiguity fails closed:
			// skip and log, never close.
			if !shouldCloseStaleTrainPR(prs[i]) {
				e.logf(0, "merge-train", "reconstruct: skipping PR #%d (state=open, no members still Queued) — head ref %q is not a train branch, not closing\n", prs[i].Number, prs[i].HeadRefName)
				continue
			}
			// #1648: an open train PR with no members still Queued *for this
			// worker's own batch* is only safe to treat as a stale remnant if it
			// also belongs to this worker's own base partition. Since concurrent
			// per-base workers are now normal (not just a post-restart scenario —
			// this function runs on every fresh dispatch), an unmatched PR could
			// equally be a sibling partition's live, healthy trial — closing it
			// here would be exactly the cross-partition destruction R4/AC3 guard
			// against. Skip and leave it for its own partition's worker to manage.
			if !trialBelongsToBase(prs[i].HeadRefName, p.baseBranch) {
				e.logf(0, "merge-train", "reconstruct: skipping PR #%d (state=open, no members still Queued) — head ref %q belongs to a different base partition, not %s — not closing\n", prs[i].Number, prs[i].HeadRefName, p.baseBranch)
				continue
			}
			e.logf(0, "merge-train", "reconstruct: closing stale open train PR #%d (no members still Queued) for %s\n", prs[i].Number, repoKey)
			if cerr := e.client.CloseIssue(p.owner, p.repo, prs[i].Number); cerr != nil {
				e.logf(0, "merge-train", "warn: could not close stale train PR #%d: %v\n", prs[i].Number, cerr)
			}
			if tn := trialNameFromBranch(prs[i].HeadRefName); tn != "" {
				e.cleanupTrialArtifacts(p.wm, tn)
			}
		}
	}

	// Probe origin branches. Skipped under the test seam (no real git); tests drive
	// reconstruction through listPRsFn and treat an open PR's branch as present.
	var originBranches []string
	if e.trainValidateFn == nil {
		if b, berr := p.wm.ListTrainBranchesOnOrigin(); berr != nil {
			e.logf(0, "merge-train", "reconstruct: ls-remote failed for %s: %v\n", repoKey, berr)
		} else {
			originBranches = b
		}
	}

	// Route 1: a merged landing PR (batch marker) with still-Queued members → complete
	// the deferred landing (already-landed work is never dropped; checked first).
	if trainPR != nil && trainPR.Merged && strings.Contains(trainPR.Body, mergeTrainBatchMarker) {
		e.completeDeferredLanding(ctx, state, p, *trainPR, batch)
		return true
	}

	// Route 2: an open train PR with still-Queued members.
	if trainPR != nil && trainPR.State == "open" {
		trialName := trialNameFromBranch(trainPR.HeadRefName)
		branchPresent := e.trainValidateFn != nil || // seam: treat as present
			(trialName != "" && containsBranch(originBranches, trainBranchPrefix+trialName))
		if trialName != "" && branchPresent {
			e.resumeTrain(ctx, state, p, *trainPR, trialName, batch)
			return true
		}
		// Open PR without a backing trial branch → orphan → dissolve. Comment only on
		// this PR's own members (never on unrelated fresh Queued items).
		members := filterBatchByNumbers(batch, parseTrainMembers(trainPR.Body))
		e.dissolveBatch(state, p, trainPR.Number, trialName, members,
			"reconstruct: found an open integration PR without a backing trial branch after a restart")
		return true
	}

	// Route 3: orphaned trial branch(es) on origin but no relevant train PR — a crash
	// remnant. Clean them up SILENTLY and proceed fresh: dissolving with today's members
	// would post confusing "batch dissolved" comments on unrelated fresh Queued items,
	// and returning true would abort today's batch. Returning false lets the current
	// batch form on this poll (a fresh trial gets a new, unique branch name — no clash).
	//
	// #1648: only sweep branches belonging to this worker's own base partition
	// (trialBelongsToBase) — an unmatched branch for a different base is not an
	// orphan at all, it may be a sibling partition's live trial (concurrent
	// per-base workers are now normal, not just a post-restart scenario). Deleting
	// it here would be exactly the cross-partition destruction R4/AC3 guards against.
	for _, b := range originBranches {
		tn := trialNameFromBranch(b)
		if tn == "" {
			continue
		}
		if !trialBelongsToBase(b, p.baseBranch) {
			e.logf(0, "merge-train", "reconstruct: leaving trial branch %s alone for %s — belongs to a different base partition, not %s\n", tn, repoKey, p.baseBranch)
			continue
		}
		e.logf(0, "merge-train", "reconstruct: cleaning up orphaned trial branch %s for %s\n", tn, repoKey)
		e.cleanupTrialArtifacts(p.wm, tn)
	}
	return false
}

// completeDeferredLanding finishes a landing that merged before a crash but whose
// members are still in Queued (ADR-059 D5, FR-4). It parses the merged PR's member
// list, intersects it with the still-Queued snapshot, and runs the idempotent
// landMergeTrainBatch advancement (which finds the already-merged PR, skips the
// merge, and advances each still-Queued member to Done).
func (e *Engine) completeDeferredLanding(ctx context.Context, state *mergeTrainWorkerState, p trialParams, pr gh.PRDetails, batch []gh.ProjectItem) {
	repoKey := p.owner + "/" + p.repo
	items := filterBatchByNumbers(batch, parseTrainMembers(pr.Body))
	if len(items) == 0 {
		e.logf(0, "merge-train", "reconstruct: merged integration PR #%d for %s has no still-Queued members — nothing to complete\n", pr.Number, repoKey)
		// The in-flight marker is cleared by prepareTrainWorker's own-failure defer
		// (this function is only reached via reconstructTrainState returning true,
		// which prepareTrainWorker treats as ok=false) — see ADR-067.
		return
	}
	e.logf(0, "merge-train", "reconstruct: completing deferred landing for %s from merged PR #%d (%d still-Queued member(s))\n", repoKey, pr.Number, len(items))

	state.mu.Lock()
	state.trialName = trialNameFromBranch(pr.HeadRefName)
	state.mu.Unlock()

	survivors := e.fetchTrainMembers(ctx, p.owner, p.repo, items)
	if len(survivors) == 0 {
		e.logf(0, "merge-train", "reconstruct: no member PRs resolvable for deferred landing of %s — clearing\n", repoKey)
		return
	}
	// landMergeTrainBatch re-finds the merged marker PR and skips FR-2 (merge already
	// happened); the in-flight marker is cleared by prepareTrainWorker's own-failure
	// defer once this whole call chain unwinds (this function is only reached via
	// reconstructTrainState returning true, which prepareTrainWorker treats as
	// ok=false) — see ADR-067.
	e.landMergeTrainBatch(ctx, state, p.owner, p.repo, p.baseBranch, p.trainKey, survivors, p.wm)
}

// resumeTrain re-establishes an in-flight batch from an open train PR after a
// restart (ADR-059 D5, FR-4). It re-resolves the still-Queued members, polls CI on
// the existing trial head, and — on green — lands via landGreenBatch (with
// main-moved recovery). Any non-green outcome (red, pending, or no resolvable
// members) dissolves the batch so the next poll re-forms a fresh, clean train
// rather than re-entering bisection on resume.
func (e *Engine) resumeTrain(ctx context.Context, state *mergeTrainWorkerState, p trialParams, pr gh.PRDetails, trialName string, batch []gh.ProjectItem) {
	repoKey := p.owner + "/" + p.repo
	items := filterBatchByNumbers(batch, parseTrainMembers(pr.Body))
	if len(items) == 0 {
		e.dissolveBatch(state, p, pr.Number, trialName, items,
			"reconstruct: an open train PR had no members still in Queued after a restart")
		return
	}
	survivors := e.fetchTrainMembers(ctx, p.owner, p.repo, items)
	if len(survivors) == 0 {
		e.dissolveBatch(state, p, pr.Number, trialName, items,
			"reconstruct: could not resolve any member PRs while resuming the batch")
		return
	}

	state.mu.Lock()
	state.trialName = trialName
	state.prNum = pr.Number
	state.assembling = false
	state.mu.Unlock()

	e.logf(0, "merge-train", "reconstruct: resuming train for %s from open PR #%d (trial %s, %d member(s))\n", repoKey, pr.Number, trialName, len(survivors))

	// The resumed trial's diagnostic is intentionally discarded: any non-green outcome
	// here dissolves the batch (dissolveBatch's messaging is out of this issue's scope,
	// #1420 — only ejectMember's comments are in scope).
	var result TrainCIResult
	if e.trainValidateFn != nil {
		result, _ = e.trainValidateFn(ctx, survivors)
	} else {
		result, _ = e.pollTrainCI(ctx, p.owner, p.repo, pr.Number, pr.HeadSHA)
	}

	if result == TrainCIGreen {
		e.landGreenBatch(ctx, state, p, survivors)
		return
	}
	e.dissolveBatch(state, p, pr.Number, trialName, items,
		"reconstruct: the resumed trial did not validate green — re-forming a fresh train")
}

// containsBranch reports whether branch is in slice.
func containsBranch(slice []string, branch string) bool {
	for _, v := range slice {
		if v == branch {
			return true
		}
	}
	return false
}

// describeCheckRuns renders a "name (status-or-conclusion)" summary for each
// check run, joined by ", ", so a green/red/pending pollTrainCI decision can
// be reconstructed after the fact from the logs alone (#1153). Mirrors the
// naming convention already used by classifyCIFromCheckRuns (engine/ci.go).
func describeCheckRuns(runs []gh.CheckRun) string {
	if len(runs) == 0 {
		return "(none)"
	}
	parts := make([]string, 0, len(runs))
	for _, cr := range runs {
		state := cr.Status
		if cr.Status == "completed" {
			state = cr.Conclusion
		}
		parts = append(parts, fmt.Sprintf("%s (%s)", cr.Name, state))
	}
	return strings.Join(parts, ", ")
}

// pollTrainCI polls the integration PR's CI signals, returning the typed result and,
// for a red result, the diagnostic that observed it (R1/#1420) — nil for green/pending.
// Blocks until the result is known or the CIBackstopTimeout elapses (ADR-1410, R6 —
// see pollForMergeable's doc comment for why this synchronous blocking loop keeps an
// elapsed-time bound, pointed at the backstop rather than the liveness-dwell
// CIWaitTimeout, instead of adopting liveness semantics itself). A confirmed CI
// failure (check-run or required-context) already returns TrainCIRed immediately
// below, unconditionally — it never waits out the deadline.
//
// mergeable_state is a red/permission gate only, not a green shortcut:
// GitHub computes it from required checks alone (per branch protection), so
// it can read "accepted" (clean/unstable) while a non-required check (e.g.
// the full test suite, if left unmarked-required) is still queued or
// in_progress on the trial SHA — accepting it as green in that state is
// exactly the #1150 defect this fixes (see adrs/1153-*.md). "dirty" is
// unambiguous and still returns TrainCIRed immediately. Otherwise, an
// accepted mergeable_state is recorded as necessary-but-not-sufficient: the
// check-run pass below is what actually confirms completeness (no run left
// queued/in_progress) before TrainCIGreen is returned. mergeable_state
// remains the sole basis for green only when there is no check-run
// footprint at all (e.g. GitHub Actions disabled — see the zero-check-runs
// branch below), since in that case there is no per-check signal to fall
// back on.
func (e *Engine) pollTrainCI(ctx context.Context, owner, repo string, prNum int, trialSHA string) (TrainCIResult, *trainCIDiagnostic) {
	deadline := time.Now().Add(e.ciBackstopTimeout())

	var lastPending, lastFailed []gh.CheckRun

	logTimeout := func() {
		if len(lastFailed) > 0 || len(lastPending) > 0 {
			e.logf(0, "merge-train", "CI wait timeout for integration PR #%d — pending: %s; failed: %s\n",
				prNum, describeCheckRuns(lastPending), describeCheckRuns(lastFailed))
		} else {
			e.logf(0, "merge-train", "CI wait timeout for integration PR #%d\n", prNum)
		}
	}

	for {
		select {
		case <-ctx.Done():
			e.logf(0, "merge-train", "context cancelled during CI poll for integration PR #%d\n", prNum)
			return TrainCIPending, nil
		default:
		}

		if time.Now().After(deadline) {
			logTimeout()
			return TrainCIPending, nil
		}

		_, mergeableState, err := e.client.FetchPRMergeableFields(owner, repo, prNum)
		mergeableAccepted := false
		if err != nil {
			e.logf(0, "merge-train", "warn: FetchPRMergeableFields failed for PR #%d: %v\n", prNum, err)
		} else if mergeableState == "dirty" {
			return TrainCIRed, &trainCIDiagnostic{
				Note:     "The trial branch stopped merging cleanly onto its base (mergeable_state \"dirty\") — the base moved again after the trial was assembled.",
				PRNum:    prNum,
				TrialSHA: trialSHA,
			}
		} else if gh.MergeableStateAccepted(mergeableState) {
			mergeableAccepted = true
		}

		// Check individual check runs via the shared classifier (mirrors
		// settlePRMergeState/checkCIGate in engine/ci.go — this used to be an
		// inline duplicate with its own dedup-by-ID drift; ClassifyCheckRuns
		// fixes that as a side effect of sharing it here). This is now
		// reachable on every iteration regardless of mergeable_state, so it
		// is the thing that actually determines completeness.
		checkRuns, err := e.client.FetchCheckRuns(owner, repo, trialSHA)
		if err != nil {
			e.logf(0, "merge-train", "warn: FetchCheckRuns failed for %s: %v\n", trialSHA, err)
		} else if len(checkRuns) > 0 {
			status, pending, failed := gh.ClassifyCheckRuns(checkRuns)
			lastPending, lastFailed = pending, failed
			if status == gh.CheckRunsFailed {
				// Strict non-required-failure policy (adrs/1153-*.md): any
				// confirmed check-run failure blocks the train, required or
				// not — Fabrik has no general way to distinguish the two
				// beyond the opt-in RequiredStatusContexts config, and a
				// wrong-direction Strict call costs one bisection cycle, not
				// a silently reintroduced version of this issue.
				e.logf(0, "merge-train", "trial %s red — failed check(s): %s\n", trialSHA, describeCheckRuns(failed))
				return TrainCIRed, &trainCIDiagnostic{FailedChecks: failed, PRNum: prNum, TrialSHA: trialSHA}
			}
			if status == gh.CheckRunsReady {
				// ADR-933: don't declare the trial green until any configured
				// required context has confirmed success on this exact trial
				// SHA — mirrors settlePRMergeState's guard in pr_settle.go. A
				// required context that's merely missing/pending falls through
				// to keep polling (nothing has regressed).
				rcStatus, _, _, rcFailed := e.classifyRequiredContexts(0, owner, repo, trialSHA, checkRuns)
				switch rcStatus {
				case gh.RequiredContextsSatisfied:
					e.logf(0, "merge-train", "trial %s green — checks: %s\n", trialSHA, describeCheckRuns(checkRuns))
					return TrainCIGreen, nil
				case gh.RequiredContextsFailed:
					e.logf(0, "merge-train", "required status context(s) failed for %s: %v\n", trialSHA, rcFailed)
					return TrainCIRed, &trainCIDiagnostic{FailedContexts: rcFailed, PRNum: prNum, TrialSHA: trialSHA}
				}
			}
			// CheckRunsPending (or a required context still pending above):
			// fall through and keep polling — this is the #1150 case, a
			// non-required check still queued/in_progress while
			// mergeable_state already reads accepted.
		} else {
			// ADR-933: zero check runs at all (e.g. GitHub Actions disabled —
			// the local-CI-takeover case #933 was filed for) must still be
			// checked against configured required contexts, mirroring
			// settlePRMergeState's zero-check-runs branch (pr_settle.go rule
			// 13). Without this, a confirmed required-context failure on a
			// trial branch with no check-run footprint at all would never
			// resolve to TrainCIRed — it would just poll to CIBackstopTimeout and
			// return TrainCIPending, stalling the batch instead of ejecting
			// the poisoning member. A merely missing/pending required context
			// is not short-circuited here — it keeps polling like any other
			// not-yet-settled signal.
			rcStatus, _, _, rcFailed := e.classifyRequiredContexts(0, owner, repo, trialSHA, nil)
			if rcStatus == gh.RequiredContextsFailed {
				e.logf(0, "merge-train", "required status context(s) failed for %s: %v\n", trialSHA, rcFailed)
				return TrainCIRed, &trainCIDiagnostic{FailedContexts: rcFailed, PRNum: prNum, TrialSHA: trialSHA}
			}
			// #1153: with zero check runs there is no per-check completeness
			// signal to consult at all, so an accepted mergeable_state is the
			// only remaining evidence that nothing is outstanding — this is
			// the one place mergeable_state is genuinely load-bearing for
			// green.
			if mergeableAccepted && rcStatus == gh.RequiredContextsSatisfied {
				e.logf(0, "merge-train", "trial %s green — mergeable_state %q accepted, zero check runs, required contexts satisfied\n", trialSHA, mergeableState)
				return TrainCIGreen, nil
			}
		}

		// Check deadline again before the sleep so a short CIBackstopTimeout
		// doesn't block unnecessarily in the poll interval when the deadline
		// has already elapsed.
		if time.Now().After(deadline) {
			logTimeout()
			return TrainCIPending, nil
		}

		// Poll again after the (test-overridable) retry interval.
		select {
		case <-ctx.Done():
			return TrainCIPending, nil
		case <-time.After(e.trainCIPollIntervalOrDefault()):
		}
	}
}
