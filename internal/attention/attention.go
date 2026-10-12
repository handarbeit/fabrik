// Package attention classifies one board item as needs-human, suspended,
// escalated, waiting, working, stalled or idle, and says what the engine will do next and
// when (#1967, ADR-1966-a). It answers the overseer's question "stuck, or
// waiting correctly?".
//
// The package is pure: it takes a plain Input and returns a Result, with no
// dependency on engine, itemstate or GitHub. That keeps the classification
// table-testable on its own and lets the push-event work that follows reuse
// the exact same definitions. The mapping implemented here is a product
// definition (documented as-built in docs/state-machine.md §7.9), pinned by
// attention_test.go.
//
// Unknown is never defaulted to healthy. A time the cache cannot vouch for is
// the zero time.Time ("unknown") on Input and on every Deadline in the Result;
// a stall is never inferred from an unknown progress anchor.
package attention

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// State is the attention classification of an item.
type State string

const (
	// NeedsHuman: the engine is paused and only a human reply resumes it.
	NeedsHuman State = "needs-human"
	// Suspended: nothing is wrong and nothing will move until a human decides
	// (#2120): a stage is complete and the item does not auto-advance, or it is
	// blocked only by work parked in an unmanaged column. Derived, never a label;
	// ranked with needs-human; never stalled.
	Suspended State = "suspended"
	// Escalated: the engine gave up (a limit was hit or a settle scan
	// escalated) and a human must intervene; a plain comment may not suffice.
	Escalated State = "escalated"
	// Waiting: the engine is waiting on something it owns (CI, reviews,
	// dependencies, a cooldown, the merge train, the Claude usage limit).
	Waiting State = "waiting"
	// Working: a worker is in flight and has made observable progress recently.
	Working State = "working"
	// Stalled: not paused, nothing the engine is correctly waiting on, and no
	// observable progress for longer than the stall threshold.
	Stalled State = "stalled"
	// Idle: nothing to do and nothing wrong (terminal, closed, unmanaged
	// column, or recently active).
	Idle State = "idle"
)

// Reason codes that carry a rank in the attention view beyond the state itself.
const (
	CodeAwaitingMergeDecision = "awaiting-merge-decision"
	CodeOverdue               = "overdue"
	CodeWorkerNoProgress      = "worker-no-progress"
	CodeNoProgress            = "no-progress"
	CodeStageCompleteStuck    = "stage-complete-not-advanced"
	// CodeWaitingForPoll marks an item whose only obstacle is that the daemon's
	// poll loop is overdue (#2124); the engine, not the item, is stalled.
	CodeWaitingForPoll = "waiting-for-poll"
	// CodeDecisionPending is Suspended case (a): a stage is complete and the
	// item does not auto-advance. CodeParkedBlocker is case (b): every open
	// blocker sits in an unmanaged parking column.
	CodeDecisionPending = "decision-pending"
	CodeParkedBlocker   = "parked-blocker"
)

// Pending kinds (Result.Pending.Kind).
const (
	PendingDecision      = "decision"
	PendingParkedBlocker = "parked-blocker"
)

// SinceBasis values say where a pending decision's since-time came from.
const (
	// SinceGitHub: the GitHub time the deciding label was applied.
	SinceGitHub = "github-label"
	// SinceStageComment: the cached Fabrik stage comment's creation time. An
	// estimate, never exact: the comment is posted before the label, and a rework
	// or step back can leave an older completion's comment as the latest one.
	SinceStageComment = "stage-comment"
	// SinceBlockerParked: a parked-blocker wait dated by both facts that make it
	// one: the item's fabrik:blocked label (GitHub time) and the daemon having seen
	// every direct blocker move into its parking column.
	SinceBlockerParked = "blocker-parked"
	// SinceEligibility: the daemon-stamped time eligibility to proceed last changed.
	SinceEligibility = "eligibility-change"
	// SinceFirstSeen: daemon first-seen only; the wait is at least this long.
	SinceFirstSeen = "first-seen"
)

// Attention-view ranks: lower is more urgent. NotRanked items are omitted from
// the attention view.
const (
	RankNeedsHuman    = 0 // needs-human and suspended share it
	RankEscalated     = 1
	RankStalled       = 2
	RankMergeDecision = 3
	NotRanked         = -1
)

// StageKind classifies the board column the item sits in.
type StageKind int

const (
	// KindNone: the column matches no configured stage (or the item has no Status).
	KindNone StageKind = iota
	// KindManaged: an ordinary work stage.
	KindManaged
	// KindHolding: an engine-managed batch holding pen (merge-train Queued).
	KindHolding
	// KindUnmanaged: a parking column Fabrik runs no workflow for.
	KindUnmanaged
	// KindCleanup: the cleanup stage (Done).
	KindCleanup
)

// Counter is one StageState cycle counter. Max is the configured limit: > 0 a
// limit, MaxUnlimited for "no limit", 0 for "not configured / unknown" (a test
// engine may carry 0, which must never read as "limit 0").
type Counter struct {
	Name string
	N    int
	Max  int
}

// MaxUnlimited marks a counter whose configured limit is "unlimited".
const MaxUnlimited = -1

// AtLimit reports whether the counter has reached a known positive limit.
func (c Counter) AtLimit() bool { return c.Max > 0 && c.N >= c.Max }

// WorkerStallMargin is added to a worker's wall-clock budget before a worker
// that is still in flight is called stalled.
const WorkerStallMargin = 5 * time.Minute

// Config holds the timing thresholds the classification and deadlines use.
// A zero timeout disables the deadline it anchors.
type Config struct {
	StallThreshold    time.Duration
	CIWaitTimeout     time.Duration
	CIBackstopTimeout time.Duration
	ReviewWaitTimeout time.Duration
}

// PollStall describes an overdue poll loop (#2124). The zero value means the
// poll is healthy (or its state is unknown), so every classification is
// exactly what it was before the field existed.
type PollStall struct {
	// Active is true while the poll loop is overdue.
	Active bool
	// Blocked is how long the poll has been in progress or absent.
	Blocked time.Duration
	// Step is the poll step the engine reports, or "" when unknown.
	Step string
}

// Input is everything the classifier needs, already extracted from the cache.
// Zero times mean unknown.
type Input struct {
	Now time.Time
	Cfg Config

	Status    string
	StageKind StageKind
	Closed    bool
	// Terminal is the engine's own "finished" flag (cleanup-stage status plus
	// completion labels).
	Terminal bool

	Labels   []string
	Counters []Counter
	// PausedByEngine is the engine-initiated-pause flag for the item's current stage.
	PausedByEngine bool

	HasWorker       bool
	WorkerStartedAt time.Time
	// WorkerBudget is the wall-clock budget of the in-flight worker's stage:
	// its max_wall_time, or the Claude inactivity timeout when the stage sets
	// none. Zero means unknown. A worker is not classified stalled before
	// WorkerBudget + WorkerStallMargin has elapsed, however low StallThreshold is.
	WorkerBudget time.Duration

	// Cooldowns maps cooldown reason to expiry.
	Cooldowns map[string]time.Time
	// LabelAppliedAt is the record-on-write map; empty after a restart.
	LabelAppliedAt map[string]time.Time

	StatusEnteredAt  time.Time
	LastAttemptAt    time.Time // latest across stages
	LastCIProgressAt time.Time

	HasOpenUnmergedPR bool

	// ClaudeUsageLimitedUntil is the account-wide usage-limit hold end; zero if none.
	ClaudeUsageLimitedUntil time.Time

	// PollStall is set while the daemon's poll loop is overdue.
	PollStall PollStall
	// PollRecoveredAt is when the last overdue poll episode ended; zero if none.
	// It floors the progress anchor so items blocked by the stall get a fresh
	// stall window instead of reading as stalled the instant the poll recovers.
	PollRecoveredAt time.Time
	// EligibilityChangedAt is the daemon-stamped time the item's eligibility to
	// proceed last changed (an autonomy label, a pause, a blocker or a stage
	// completion label added or removed). Zero means unknown (after a restart).
	EligibilityChangedAt time.Time
	// AutoAdvances reports whether the engine advances this item by itself once
	// the current stage completes (yolo/cruise label, global yolo, or the
	// stage's auto_advance).
	AutoAdvances bool
	// NextStage is the stage a human advance would move the item to. Empty when
	// the next stage is the cleanup stage (Validate) or there is none.
	NextStage string
	// FeedbackPending reports unprocessed comments, review threads or review
	// bodies (the feedback gate's definition), read from the cache.
	FeedbackPending bool
	// StageCommentAt is the creation time of the cached Fabrik comment for the
	// current stage: a GitHub-time proxy for "the stage completed". Zero if none.
	StageCommentAt time.Time
	// ParkedBlockers lists the parked items the item is blocked on, resolved
	// cache-only and transitively by the caller. BlockedOnlyByParked is true only
	// when every open blocker resolved to a parked item; an unknown or
	// active-column blocker leaves it false.
	ParkedBlockers      []ParkedBlocker
	BlockedOnlyByParked bool
	// ParkedBlockersUnknown is true when BlockedOnlyByParked is false only
	// because the cache lacked data (a blocker it does not hold, or one not yet
	// deep-fetched), not because a blocker is definitely not parked. It is
	// echoed to Result.ParkedDataMissing and changes no classification.
	ParkedBlockersUnknown bool

	// Resume says how the item's pause is lifted according to the engine's own
	// pause comment; ResumeUnknown when the cache cannot say.
	Resume ResumeMethod
	// PRNumber is the linked PR number, for the merge-decision options.
	PRNumber int
}

// ParkedBlocker is an open item, sitting in an unmanaged column, that blocks
// another item.
type ParkedBlocker struct {
	Ref    string
	Column string
	// EnteredAt is when the daemon first saw the blocker in Column (or saw it move
	// there): a lower bound on how long it has been parked. MovedAt is non-zero
	// only when the daemon watched it move into Column, i.e. EnteredAt is the move
	// itself and not merely the first sight.
	EnteredAt time.Time
	MovedAt   time.Time
	// Transitive is true when the blocker was reached through an intermediate
	// blocker. The edges between two other items are not observed, so a wait that
	// involves one can only be dated by the daemon's first sight, never exactly.
	Transitive bool
}

// ResumeMethod is how a pause is lifted, as the engine's own pause comment says.
type ResumeMethod string

const (
	// ResumeUnknown: no cached pause comment states a method.
	ResumeUnknown ResumeMethod = ""
	// ResumeComment: a human comment created after the pause resumes it.
	ResumeComment ResumeMethod = "human-comment"
	// ResumeRemoveLabel: removing fabrik:paused resumes it.
	ResumeRemoveLabel ResumeMethod = "remove-label"
)

// Option is one way a human can act on a decision-bearing item. Tool names the
// MCP tool that performs it, empty when there is none (the human acts directly).
type Option struct {
	Action string `json:"action"`
	Tool   string `json:"tool,omitempty"`
}

// Pending describes a Suspended item's decision.
type Pending struct {
	Kind           string
	CompletedStage string
	NextStage      string
	// Since is when the wait began; SinceBasis says how far to trust it. Zero
	// Since never occurs: the weakest basis is the daemon's first sight.
	Since      time.Time
	SinceBasis string
	Blockers   []ParkedBlocker
	Options    []Option
}

// SinceExact reports whether Since is the real start of the wait rather than a
// lower bound.
func (p Pending) SinceExact() bool {
	switch p.SinceBasis {
	case SinceGitHub, SinceEligibility, SinceBlockerParked:
		return true
	}
	return false
}

// Reason is one label (or condition) contributing to the classification.
type Reason struct {
	// Code is the label name or a synthetic code such as "cooldown:retry".
	Code    string
	Meaning string
}

// Deadline is something the engine will act on at a time. At is the zero time
// when the cache cannot say when (unknown).
type Deadline struct {
	Kind  string
	At    time.Time
	Basis string
}

// Known reports whether the deadline has a computable time.
func (d Deadline) Known() bool { return !d.At.IsZero() }

// Next is what the engine will do next, and when.
type Next struct {
	Action string
	// At is the earliest known deadline; zero (unknown) when no deadline is known.
	At        time.Time
	Deadlines []Deadline
}

// Result is the classification of one item.
type Result struct {
	State State
	// Code is the primary reason code (a label name or one of the Code* constants).
	Code string
	// Summary is a one-line human reason.
	Summary string
	Reasons []Reason
	// Rank is the attention-view rank, or NotRanked.
	Rank int
	Next Next
	// ProgressAt is the latest observable-progress anchor; zero when unknown.
	ProgressAt time.Time
	// ProgressAge is Now - ProgressAt, meaningful only when ProgressAt is non-zero.
	ProgressAge time.Duration
	// Pending is set for a Suspended item.
	Pending *Pending
	// Options lists how a human can act, for decision-bearing results
	// (Suspended, awaiting-merge-decision, paused/needs-human, escalated).
	Options []Option
	// ResumeVia says how a paused or escalated item is resumed, in the words of
	// the engine's own pause comment: ResumeViaLabel, ResumeViaComment or
	// ResumeViaPauseComment (the cache cannot say). Empty for other results.
	ResumeVia string
	// ParkedDataMissing echoes Input.ParkedBlockersUnknown: the item is not
	// Suspended as a parked blocker only because blocker data is missing, so a
	// caller tracking a persisted episode must not read it as "decision made".
	ParkedDataMissing bool
}

// Values of Result.ResumeVia.
const (
	ResumeViaLabel        = "remove-label"
	ResumeViaComment      = "human-comment"
	ResumeViaPauseComment = "see-pause-comment"
)

type labelInfo struct {
	meaning string
	// waits marks labels that classify the item as Waiting.
	waits bool
}

// labelTable is the one-line meaning of every state-bearing label. Labels not
// listed here (stage:*, model:*, effort:* ...) carry no attention meaning.
var labelTable = map[string]labelInfo{
	"fabrik:paused":                        {"the engine paused this item; a human comment created after the pause resumes it", false},
	"fabrik:awaiting-input":                {"the engine asked a question and is waiting for a human reply", false},
	"fabrik:awaiting-review":               {"waiting for PR reviewers (or the review timeout / bot re-prompt)", true},
	"fabrik:awaiting-ci":                   {"waiting for CI on the linked PR (stage completion is deferred until it is green)", true},
	"fabrik:awaiting-done":                 {"stage reported no work needed; the engine is retrying the move to Done and the issue close", true},
	"fabrik:awaiting-member-close":         {"merge-train landing finished; the engine is retrying closing the member issue", true},
	"fabrik:awaiting-close":                {"PR merged; the engine is retrying closing the issue", true},
	"fabrik:awaiting-pr-ready":             {"the engine is retrying marking the draft PR ready for review", true},
	"fabrik:awaiting-advance":              {"the engine is retrying moving the board Status forward", true},
	"fabrik:awaiting-runaway-alert":        {"the merge-train runaway guard paused this item and its alert comment has not posted yet", true},
	"fabrik:awaiting-landing-verification": {"the engine is verifying that the credited PR really merged", true},
	"fabrik:awaiting-placement":            {"a spawned child could not be placed on the board; the engine is retrying", true},
	"fabrik:blocked":                       {"blocked by unresolved dependency issues; resumes when they close", true},
	"fabrik:rebase-needed":                 {"the linked PR no longer merges cleanly; the engine is dispatching a rebase", true},
	"fabrik:bot-reprompted":                {"a bot reviewer was re-prompted; the engine pauses if it still has not responded", true},
	"fabrik:children-spawned":              {"child issues were spawned and block this one", false},
	"fabrik:claude-limit":                  {"the Claude account usage limit was hit; the engine retries once the usage-limit hold lifts", true},
	"fabrik:tools-denied":                  {"a tool call was denied by the permission layer; the engine retries, bounded by its own counter", true},
	"fabrik:api-key-helper-detected":       {"the worktree sets apiKeyHelper, which Fabrik refuses; remove it from the worktree settings", false},
	"fabrik:skill-unresolved":              {"a stage skill reference matches no loaded plugin; fix the stage YAML or load the plugin", false},
	"fabrik:toolchain-stale":               {"the daemon's PATH toolchain does not satisfy the repo's declared version (warn-only)", false},
	"fabrik:landing-verification-failed":   {"the credited PR did not merge; the issue was reopened and needs a human", false},
	"fabrik:nondefault-base-pr-noted":      {"informational: PR targets a non-default base branch", false},
}

// MeaningOf returns the one-line meaning of a state-bearing label, or "".
func MeaningOf(label string) string { return labelTable[label].meaning }

func hasLabel(labels []string, l string) bool {
	for _, x := range labels {
		if x == l {
			return true
		}
	}
	return false
}

// Classify computes the attention Result for one item. It is a pure function
// of in.
func Classify(in Input) Result {
	res := attributePollStall(in, classify(in))
	res.ParkedDataMissing = in.ParkedBlockersUnknown
	return res
}

// attributePollStall blames the engine, not the item, when the poll loop is
// overdue (#2124). A result that says the item itself has stalled becomes an
// unranked wait for the poll; a wait or idle verdict (whose next step is a
// poll) keeps its state and code with the cause prepended. Verdicts that need
// a human or a running worker are untouched. With PollStall inactive the result
// is returned as is.
func attributePollStall(in Input, res Result) Result {
	if !in.PollStall.Active {
		return res
	}
	text := pollWaitText(in.PollStall)
	switch {
	case res.State == Stalled && (res.Code == CodeOverdue || res.Code == CodeNoProgress || res.Code == CodeStageCompleteStuck):
		res.State, res.Code, res.Rank = Waiting, CodeWaitingForPoll, NotRanked
		res.Summary = text
		res.Next.Action = text
	case res.State == Waiting, res.State == Idle && res.Code == "idle":
		res.Summary = text + "; " + res.Summary
		res.Next.Action = text + "; " + res.Next.Action
	}
	return res
}

func pollWaitText(p PollStall) string {
	text := fmt.Sprintf("waiting for the poll: blocked for %d s", int64(p.Blocked.Seconds()))
	if p.Step != "" {
		text += ", in step " + p.Step
	}
	return text
}

func classify(in Input) Result {
	res := Result{Rank: NotRanked}
	res.ProgressAt = progressAnchor(in)
	if !res.ProgressAt.IsZero() && !in.Now.IsZero() {
		res.ProgressAge = in.Now.Sub(res.ProgressAt)
	}
	res.Reasons = reasonsFor(in)
	res.Next.Deadlines = deadlinesFor(in)
	res.Next.At = earliestKnown(res.Next.Deadlines)

	finish := func(st State, code, summary, action string, rank int) Result {
		res.State, res.Code, res.Summary, res.Rank = st, code, summary, rank
		res.Next.Action = action
		return res
	}
	// finishNoSchedule is finish for a state only a human (or an external event)
	// moves: the engine schedules nothing, so no deadline is reported and next_at
	// is unknown. A dispatch cooldown's expiry is never a next action here (R5).
	finishNoSchedule := func(st State, code, summary, action string, rank int, opts []Option) Result {
		res.Next.Deadlines = nil
		res.Next.At = time.Time{}
		res.Options = opts
		return finish(st, code, summary, action, rank)
	}

	// 1. Items the engine has nothing to do for.
	switch {
	case in.Closed:
		return finish(Idle, "closed", "issue is closed", "nothing scheduled", NotRanked)
	case in.Terminal || in.StageKind == KindCleanup:
		return finish(Idle, "done", "item is in the cleanup stage", "nothing scheduled", NotRanked)
	case in.StageKind == KindUnmanaged:
		return finish(Idle, "unmanaged-column", fmt.Sprintf("sits in %q, a parking column Fabrik runs no workflow for", in.Status),
			"nothing scheduled; a human moves it to a real stage", NotRanked)
	case in.StageKind == KindNone:
		return finish(Idle, "no-stage", "no configured stage matches this item's board column", "nothing scheduled", NotRanked)
	}

	paused := hasLabel(in.Labels, "fabrik:paused")
	failedLanding := hasLabel(in.Labels, "fabrik:landing-verification-failed")

	// 2. Escalated: the engine has stopped on its own account.
	if (paused && escalationSignal(in)) || failedLanding {
		code, summary := escalationCode(in, failedLanding)
		res.ResumeVia = resumeVia(in, true)
		return finishNoSchedule(Escalated, code, summary,
			"nothing automatic: the engine has stopped; a human must intervene ("+resumeAdvice(in, true)+")", RankEscalated,
			resumeOptions(in, true))
	}

	// 3. Needs-human: paused with a question or without an escalation signal.
	if paused {
		code, summary := "fabrik:paused", "paused; "+resumeAdvice(in, false)
		if hasLabel(in.Labels, "fabrik:awaiting-input") {
			code, summary = "fabrik:awaiting-input", "the engine is waiting for a human reply"
			if in.Resume != ResumeComment {
				summary = "the engine paused and is waiting on a human; " + resumeAdvice(in, false)
			}
		}
		res.ResumeVia = resumeVia(in, false)
		return finishNoSchedule(NeedsHuman, code, summary,
			"nothing automatic: "+resumeAdvice(in, false), RankNeedsHuman, resumeOptions(in, false))
	}

	// 4. Settled at Validate under cruise: only a human merge is left.
	if in.Status == "Validate" && hasLabel(in.Labels, "stage:Validate:complete") &&
		hasLabel(in.Labels, "fabrik:cruise") && in.HasOpenUnmergedPR && !in.HasWorker {
		return finishNoSchedule(NeedsHuman, CodeAwaitingMergeDecision,
			"Validate is complete under cruise; the PR awaits a human merge decision",
			"nothing automatic: cruise never merges; a human merges the PR", RankMergeDecision,
			MergeDecisionOptions(in.PRNumber))
	}

	// 4b. The engine refuses to run until a human edits something it cannot
	// edit itself; no retry or deadline will ever clear it.
	if hasLabel(in.Labels, "fabrik:api-key-helper-detected") {
		return finishNoSchedule(NeedsHuman, "fabrik:api-key-helper-detected",
			labelTable["fabrik:api-key-helper-detected"].meaning,
			"nothing automatic: the engine skips this item until apiKeyHelper is removed from the worktree settings", RankNeedsHuman,
			[]Option{{Action: "remove apiKeyHelper from the worktree's .claude settings"}})
	}

	// 4c. Suspended: nothing is wrong and nothing will move until a human decides.
	if p, ok := pendingDecision(in); ok {
		return suspendedResult(res, in, p)
	}
	if hasLabel(in.Labels, "fabrik:skill-unresolved") {
		return finish(NeedsHuman, "fabrik:skill-unresolved",
			labelTable["fabrik:skill-unresolved"].meaning,
			"nothing automatic: the engine re-resolves each dispatch and runs the stage once the reference resolves", RankNeedsHuman)
	}

	// 5. Waiting on something the engine owns.
	if code, summary, ok := waitingCause(in); ok {
		if code == "fabrik:blocked" {
			// An open-ended dependency wait: a dispatch cooldown's expiry is not
			// what ends it (R5).
			res.Next.Deadlines = withoutCooldowns(res.Next.Deadlines)
			res.Next.At = earliestKnown(res.Next.Deadlines)
		}
		// An engine-owned wait whose known deadline passed more than the stall
		// threshold ago means the engine should have acted and has not.
		if overdue, d := overdueBy(res.Next.Deadlines, in); overdue {
			return finish(Stalled, CodeOverdue,
				fmt.Sprintf("%s, but the %s deadline passed %s ago", summary, d.Kind, in.Now.Sub(d.At).Round(time.Second)),
				actionFor(res.Next.Deadlines, "the engine should act on the next poll"), RankStalled)
		}
		// An engine-retried wait with no known deadline cannot go overdue, so
		// it would otherwise read as "waiting correctly" forever. Once nothing
		// observable has changed for longer than the stall threshold it is
		// stalled. Waits that are legitimately open-ended (dependencies owned
		// elsewhere, the merge train, the account-wide usage limit) are exempt.
		if !hasKnownDeadline(res.Next.Deadlines) && !openEndedWaitExempt(code) && stallExceeded(res, in) {
			return finish(Stalled, CodeNoProgress,
				fmt.Sprintf("%s, with no known deadline and no observable change for %s", summary, res.ProgressAge.Round(time.Second)),
				"nothing scheduled that the cache can see; the next poll re-evaluates the item", RankStalled)
		}
		return finish(Waiting, code, summary, actionFor(res.Next.Deadlines, waitAction(code)), NotRanked)
	}

	stalledAge := stallExceeded(res, in)

	// 6. A worker is in flight. The heartbeat is not progress (a hung process
	// keeps beating), but a healthy long run is: the worker is only stalled once
	// it has outlived its own stage's wall-clock budget plus a margin.
	if in.HasWorker {
		if workerOverdue(res, in) {
			return finish(Stalled, CodeWorkerNoProgress,
				fmt.Sprintf("a worker is in flight but nothing observable changed for %s", res.ProgressAge.Round(time.Second)),
				"the worker's own inactivity/wall-time limits will end it; the engine then reads its result", RankStalled)
		}
		summary := "a worker is running"
		if in.Cfg.StallThreshold > 0 && res.ProgressAge > in.Cfg.StallThreshold {
			summary = fmt.Sprintf("a worker is running (long-running: %s, within its stage's wall-clock budget)", res.ProgressAge.Round(time.Second))
		}
		return finish(Working, "worker-in-flight", summary,
			"the engine reads the invocation result when the worker exits", NotRanked)
	}

	// 7. Nothing running and nothing waited on.
	if stalledAge {
		code := CodeNoProgress
		summary := fmt.Sprintf("no worker, no wait and no observable change for %s", res.ProgressAge.Round(time.Second))
		if in.StageKind == KindManaged && hasLabel(in.Labels, "stage:"+in.Status+":complete") {
			code = CodeStageCompleteStuck
			summary = fmt.Sprintf("stage %s is complete but the item has not advanced for %s", in.Status, res.ProgressAge.Round(time.Second))
		}
		return finish(Stalled, code, summary, "nothing scheduled; the next poll re-evaluates the item", RankStalled)
	}
	return finish(Idle, "idle", "nothing to do right now", "the next poll re-evaluates the item", NotRanked)
}

// resumeAdvice says how a pause is lifted, in the engine's own words when the
// cached pause comment states the method and in words that claim neither
// otherwise (R-options-2): a generic method that contradicts the engine's
// comment is worse than none.
func resumeAdvice(in Input, escalated bool) string {
	switch resumeVia(in, escalated) {
	case ResumeViaLabel:
		return "remove fabrik:paused to resume it"
	case ResumeViaComment:
		return "a human comment created after the pause resumes this item"
	}
	return "see the engine's pause comment on the issue for how to resume it"
}

// resumeVia resolves the resume method: the engine's own comment when the cache
// has it, a human comment for a plain awaiting-input question, otherwise
// unknown.
func resumeVia(in Input, escalated bool) string {
	switch {
	case in.Resume == ResumeRemoveLabel:
		return ResumeViaLabel
	case in.Resume == ResumeComment:
		return ResumeViaComment
	case hasLabel(in.Labels, "fabrik:awaiting-input") && !escalated:
		return ResumeViaComment
	}
	return ResumeViaPauseComment
}

// resumeOptions are the options of a paused item: the single resume method the
// engine's own comment gives, or a pointer to that comment when unknown.
func resumeOptions(in Input, escalated bool) []Option {
	switch resumeVia(in, escalated) {
	case ResumeViaLabel:
		return []Option{{Action: "remove the fabrik:paused label (as the engine's pause comment says)"}}
	case ResumeViaComment:
		return []Option{{Action: "comment on the issue with your answer or instruction (a human comment created after the pause resumes it)"}}
	}
	return []Option{{Action: "read the engine's pause comment on the issue and follow its instructions"}}
}

// MergeDecisionOptions are the options of an item settled at Validate under
// cruise: the human's merge (no tool), or yolo so the engine merges.
func MergeDecisionOptions(pr int) []Option {
	merge := "merge the PR"
	if pr > 0 {
		merge = fmt.Sprintf("merge PR #%d", pr)
	}
	return []Option{
		{Action: merge},
		{Action: "set yolo so the engine merges it", Tool: "fabrik_set_autonomy yolo"},
	}
}

// DecisionOptions are the options of a completed stage that does not
// auto-advance.
func DecisionOptions(nextStage string) []Option {
	return []Option{
		{Action: "advance to " + nextStage, Tool: "fabrik_advance"},
		{Action: "set cruise so it advances through the remaining stages and waits for your merge", Tool: "fabrik_set_autonomy cruise"},
		{Action: "set yolo so it advances and merges by itself", Tool: "fabrik_set_autonomy yolo"},
	}
}

// ParkedBlockerOptions are the options of an item blocked only by parked work.
func ParkedBlockerOptions(blockers []ParkedBlocker) []Option {
	var out []Option
	for _, b := range blockers {
		out = append(out, Option{Action: fmt.Sprintf("promote blocker %s out of %s", b.Ref, b.Column), Tool: "fabrik_promote"})
	}
	return append(out, Option{Action: "remove the blocking dependency (edit the issue's blocked-by relationship)"})
}

// inFlightMarker reports labels that mean the engine is mid-work on the item.
func inFlightMarker(l string) bool {
	return l == "fabrik:editing" || strings.HasPrefix(l, "fabrik:locked:") ||
		(strings.HasPrefix(l, "stage:") && strings.HasSuffix(l, ":in_progress"))
}

// suspendedMasked reports whether something the engine owns is still going on,
// so the item is not simply waiting for a human. Cooldowns never mask: they can
// only mean "re-check later". exceptBlocked lets case (b) ignore fabrik:blocked,
// which is what parks the item in the first place.
func suspendedMasked(in Input, exceptBlocked bool) bool {
	if in.HasWorker || paused(in) {
		return true
	}
	for _, l := range in.Labels {
		if inFlightMarker(l) {
			return true
		}
		if info, ok := labelTable[l]; ok && info.waits && !(exceptBlocked && l == "fabrik:blocked") {
			return true
		}
	}
	return in.StageKind == KindHolding
}

func paused(in Input) bool { return hasLabel(in.Labels, "fabrik:paused") }

// pendingDecision derives the Suspended state (#2120 R1). Unknown inputs fail
// closed: the item stays on the pre-existing path.
func pendingDecision(in Input) (Pending, bool) {
	if in.StageKind != KindManaged {
		return Pending{}, false
	}
	// (b) every open blocker is parked in an unmanaged column.
	if in.BlockedOnlyByParked && len(in.ParkedBlockers) > 0 && !suspendedMasked(in, true) {
		since, basis := pendingSince(in, "fabrik:blocked")
		return Pending{
			Kind: PendingParkedBlocker, CompletedStage: in.Status,
			Since: since, SinceBasis: basis,
			Blockers: append([]ParkedBlocker(nil), in.ParkedBlockers...),
			Options:  ParkedBlockerOptions(in.ParkedBlockers),
		}, true
	}
	// (a) a completed stage that does not auto-advance. Validate is excluded: its
	// next stage is the cleanup stage, so NextStage is empty there.
	complete := "stage:" + in.Status + ":complete"
	if in.Status != "" && in.Status != "Validate" && in.NextStage != "" && hasLabel(in.Labels, complete) &&
		!in.AutoAdvances && !in.FeedbackPending && !suspendedMasked(in, false) {
		since, basis := pendingSince(in, complete)
		return Pending{
			Kind: PendingDecision, CompletedStage: in.Status, NextStage: in.NextStage,
			Since: since, SinceBasis: basis,
			Options: DecisionOptions(in.NextStage),
		}, true
	}
	return Pending{}, false
}

// pendingSince anchors a pending decision on the GitHub time the deciding fact
// became true. For a completed stage that is the label's time; when it is not
// known yet the stage comment's time stands in as an estimate and, failing that,
// the daemon's first sight (a lower bound). A later eligibility change wins: the
// wait starts again when the item's ability to proceed changed.
//
// A parked-blocker wait has two facts, the item's fabrik:blocked label and the
// blocker being parked, and starts at the later of them. The label's time alone
// is not the start (a 30-day-old label says nothing about a blocker parked
// minutes ago), so it is used only together with a column move the daemon
// watched for every direct blocker; otherwise the wait is dated by first sight.
func pendingSince(in Input, label string) (time.Time, string) {
	var since time.Time
	var basis string
	switch {
	case label == "fabrik:blocked":
		since, basis = parkedSince(in)
	case !in.LabelAppliedAt[label].IsZero():
		since, basis = in.LabelAppliedAt[label], SinceGitHub
	case !in.StageCommentAt.IsZero():
		since, basis = in.StageCommentAt, SinceStageComment
	default:
		since, basis = in.StatusEnteredAt, SinceFirstSeen
	}
	if in.EligibilityChangedAt.After(since) {
		since, basis = in.EligibilityChangedAt, SinceEligibility
	}
	return since, basis
}

func parkedSince(in Input) (time.Time, string) {
	first := in.StatusEnteredAt
	moved := time.Time{}
	allMoved := len(in.ParkedBlockers) > 0
	transitive := false
	for _, b := range in.ParkedBlockers {
		transitive = transitive || b.Transitive
		if b.EnteredAt.After(first) {
			first = b.EnteredAt
		}
		if b.MovedAt.IsZero() {
			allMoved = false
		} else if b.MovedAt.After(moved) {
			moved = b.MovedAt
		}
	}
	label := in.LabelAppliedAt["fabrik:blocked"]
	if allMoved && !transitive && !label.IsZero() {
		since := label
		if moved.After(since) {
			since = moved
		}
		return since, SinceBlockerParked
	}
	return first, SinceFirstSeen
}

func suspendedResult(res Result, in Input, p Pending) Result {
	res.State, res.Rank = Suspended, RankNeedsHuman
	res.Pending = &p
	res.Options = p.Options
	res.Next.Deadlines = nil
	res.Next.At = time.Time{}
	// The wait's age is the pending age, never the daemon-observed anchor: a
	// lower bound is not a progress age.
	res.ProgressAt, res.ProgressAge = time.Time{}, 0
	if p.SinceExact() && !p.Since.IsZero() && !in.Now.IsZero() {
		res.ProgressAt = p.Since
		res.ProgressAge = in.Now.Sub(p.Since)
	}
	waited := PendingWaitText(p, in.Now)
	switch p.Kind {
	case PendingParkedBlocker:
		res.Code = CodeParkedBlocker
		refs := make([]string, 0, len(p.Blockers))
		for _, b := range p.Blockers {
			refs = append(refs, fmt.Sprintf("%s (in %s)", b.Ref, b.Column))
		}
		res.Summary = fmt.Sprintf("blocked only by work parked in an unmanaged column: %s; %s", strings.Join(refs, ", "), waited)
		res.Next.Action = "nothing automatic: " + OptionsText(p.Options) + " — a human decides"
	default:
		res.Code = CodeDecisionPending
		res.Summary = fmt.Sprintf("stage %s is complete and does not advance by itself; a human decides the next step; %s", p.CompletedStage, waited)
		res.Next.Action = "nothing automatic: " + OptionsText(p.Options)
	}
	return res
}

// PendingWaitText renders how long a pending decision has waited, saying "at
// least" when only the daemon's first sight is known.
func PendingWaitText(p Pending, now time.Time) string {
	if p.Since.IsZero() {
		return "waiting for an unknown time"
	}
	age := HumanAge(now.Sub(p.Since))
	if p.SinceBasis == SinceStageComment {
		return fmt.Sprintf("waiting since about %s (%s; taken from the stage's comment, the label's own time is not known yet)", p.Since.UTC().Format(time.RFC3339), age)
	}
	if !p.SinceExact() {
		return fmt.Sprintf("waiting at least since %s (%s; the daemon first saw it then)", p.Since.UTC().Format(time.RFC3339), age)
	}
	return fmt.Sprintf("waiting since %s (%s)", p.Since.UTC().Format(time.RFC3339), age)
}

// HumanAge formats a duration with days when it is long.
func HumanAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d >= 48*time.Hour {
		days := int(d / (24 * time.Hour))
		return fmt.Sprintf("%dd%dh", days, int((d%(24*time.Hour))/time.Hour))
	}
	return d.Round(time.Second).String()
}

// OptionsText renders options as "a (tool); or b". The engine reuses it for
// event content so the classifier summary and the events read alike.
func OptionsText(opts []Option) string {
	parts := make([]string, 0, len(opts))
	for _, o := range opts {
		if o.Tool != "" {
			parts = append(parts, fmt.Sprintf("%s (%s)", o.Action, o.Tool))
		} else {
			parts = append(parts, o.Action)
		}
	}
	return strings.Join(parts, "; or ")
}

func withoutCooldowns(ds []Deadline) []Deadline {
	var out []Deadline
	for _, d := range ds {
		if !strings.HasPrefix(d.Kind, "cooldown:") {
			out = append(out, d)
		}
	}
	return out
}

// escalationSignal reports whether a paused item was stopped by the engine on
// its own account rather than to ask a question.
func escalationSignal(in Input) bool {
	if in.PausedByEngine || hasLabel(in.Labels, "fabrik:awaiting-runaway-alert") {
		return true
	}
	for _, c := range in.Counters {
		if c.AtLimit() {
			return true
		}
	}
	return false
}

func escalationCode(in Input, failedLanding bool) (string, string) {
	if failedLanding {
		return "fabrik:landing-verification-failed", "landing verification failed: the credited PR did not merge and the issue was reopened"
	}
	for _, c := range in.Counters {
		if c.AtLimit() {
			return "limit:" + c.Name, fmt.Sprintf("paused at the %s limit (%d of %d)", c.Name, c.N, c.Max)
		}
	}
	if hasLabel(in.Labels, "fabrik:awaiting-runaway-alert") {
		return "fabrik:awaiting-runaway-alert", "the merge-train runaway guard paused this item"
	}
	return "paused-by-engine", "the engine paused this item after exhausting its retries"
}

// waitingCause finds why the engine is correctly waiting, if it is.
func waitingCause(in Input) (code, summary string, ok bool) {
	// Deterministic: sorted label order, first state-bearing wait wins.
	var waits []string
	for _, l := range in.Labels {
		if info, found := labelTable[l]; found && info.waits {
			waits = append(waits, l)
		}
	}
	sort.Strings(waits)
	if len(waits) > 0 {
		return waits[0], labelTable[waits[0]].meaning, true
	}
	if reason, until, found := activeCooldown(in); found {
		return "cooldown:" + reason, fmt.Sprintf("cooldown %q active until %s", reason, until.UTC().Format(time.RFC3339)), true
	}
	if in.StageKind == KindHolding {
		return "queued", "held in the merge-train holding stage; the train worker batches it", true
	}
	return "", "", false
}

func hasKnownDeadline(ds []Deadline) bool {
	for _, d := range ds {
		if d.Known() {
			return true
		}
	}
	return false
}

// openEndedWaitExempt reports whether a wait may legitimately last longer than
// the stall threshold with no deadline the cache can see.
func openEndedWaitExempt(code string) bool {
	switch code {
	case "fabrik:blocked", "fabrik:claude-limit", "queued":
		return true
	}
	return strings.HasPrefix(code, "cooldown:")
}

func waitAction(code string) string {
	switch code {
	case "fabrik:blocked":
		return "resumes when every blocking issue is closed"
	case "queued":
		return "the merge-train worker assembles and lands the batch"
	default:
		return "the engine re-evaluates this item on its next poll"
	}
}

func activeCooldown(in Input) (reason string, until time.Time, ok bool) {
	var reasons []string
	for r, t := range in.Cooldowns {
		if t.After(in.Now) {
			reasons = append(reasons, r)
		}
	}
	if len(reasons) == 0 {
		return "", time.Time{}, false
	}
	sort.Strings(reasons)
	return reasons[0], in.Cooldowns[reasons[0]], true
}

// progressAnchor is the latest observable-progress time. It deliberately
// excludes the worker heartbeat (a ticker that stays fresh while Claude is
// hung) and UpdatedAt (bumped by Fabrik's own writes). Zero means unknown.
func progressAnchor(in Input) time.Time {
	latest := in.StatusEnteredAt
	consider := func(t time.Time) {
		if t.After(latest) {
			latest = t
		}
	}
	consider(in.LastAttemptAt)
	consider(in.LastCIProgressAt)
	consider(in.WorkerStartedAt)
	consider(in.EligibilityChangedAt)
	for _, t := range in.LabelAppliedAt {
		consider(t)
	}
	// An unknown anchor stays unknown; a known one is floored at the end of the
	// last overdue poll episode (#2124).
	if !latest.IsZero() {
		consider(in.PollRecoveredAt)
	}
	return latest
}

func stallExceeded(res Result, in Input) bool {
	return in.Cfg.StallThreshold > 0 && !res.ProgressAt.IsZero() && res.ProgressAge > in.Cfg.StallThreshold
}

// workerOverdue reports whether an in-flight worker has run longer than both the
// stall threshold and its stage's wall-clock budget plus WorkerStallMargin.
func workerOverdue(res Result, in Input) bool {
	if !stallExceeded(res, in) {
		return false
	}
	if in.WorkerBudget > 0 && res.ProgressAge <= in.WorkerBudget+WorkerStallMargin {
		return false
	}
	return true
}

func overdueBy(ds []Deadline, in Input) (bool, Deadline) {
	if in.Cfg.StallThreshold <= 0 {
		return false, Deadline{}
	}
	var worst Deadline
	found := false
	for _, d := range ds {
		if !d.Known() || !in.Now.After(d.At) {
			continue
		}
		if in.Now.Sub(d.At) > in.Cfg.StallThreshold && (!found || d.At.Before(worst.At)) {
			worst, found = d, true
		}
	}
	return found, worst
}

func earliestKnown(ds []Deadline) time.Time {
	var best time.Time
	for _, d := range ds {
		if d.Known() && (best.IsZero() || d.At.Before(best)) {
			best = d.At
		}
	}
	return best
}

func actionFor(ds []Deadline, fallback string) string {
	var pick *Deadline
	for i := range ds {
		if ds[i].Known() && (pick == nil || ds[i].At.Before(pick.At)) {
			pick = &ds[i]
		}
	}
	if pick != nil {
		return fmt.Sprintf("%s at %s (%s)", describeKind(pick.Kind), pick.At.UTC().Format(time.RFC3339), pick.Basis)
	}
	for _, d := range ds {
		if !d.Known() {
			return fmt.Sprintf("%s at an unknown time (%s)", describeKind(d.Kind), d.Basis)
		}
	}
	return fallback
}

func describeKind(kind string) string {
	switch {
	case strings.HasPrefix(kind, "cooldown:"):
		return "the dispatch cooldown expires and the item is re-dispatched"
	case kind == "ci-liveness-timeout":
		return "the CI liveness-stall timeout escalates"
	case kind == "ci-backstop-timeout":
		return "the CI absolute backstop escalates"
	case kind == "review-wait-timeout":
		return "the review wait times out (bot re-prompt, else pause)"
	case kind == "bot-reprompt-escalation":
		return "the bot re-prompt window ends and the engine pauses"
	case kind == "claude-usage-limit-end":
		return "the Claude usage-limit hold lifts"
	}
	return kind
}

func reasonsFor(in Input) []Reason {
	var out []Reason
	seen := map[string]bool{}
	labels := append([]string(nil), in.Labels...)
	sort.Strings(labels)
	for _, l := range labels {
		if info, ok := labelTable[l]; ok && !seen[l] {
			seen[l] = true
			out = append(out, Reason{Code: l, Meaning: info.meaning})
		}
	}
	var cds []string
	for r, t := range in.Cooldowns {
		if t.After(in.Now) {
			cds = append(cds, r)
		}
	}
	sort.Strings(cds)
	for _, r := range cds {
		out = append(out, Reason{Code: "cooldown:" + r, Meaning: "dispatch cooldown active; the engine will not re-dispatch before it expires"})
	}
	for _, c := range in.Counters {
		if c.AtLimit() {
			out = append(out, Reason{Code: "limit:" + c.Name, Meaning: fmt.Sprintf("%s counter is at its limit (%d of %d)", c.Name, c.N, c.Max)})
		}
	}
	return out
}

// deadlinesFor lists every deadline the engine will act on for this item. A
// deadline anchored on a value the cache does not hold (LabelAppliedAt after a
// restart, LastCIProgressAt after a restart) is listed with an unknown At.
func deadlinesFor(in Input) []Deadline {
	var ds []Deadline
	var cds []string
	for r := range in.Cooldowns {
		cds = append(cds, r)
	}
	sort.Strings(cds)
	for _, r := range cds {
		// Only an entry still in the future is a deadline. An expired one is either
		// history or an expiry poll admission has not yet acted on (the engine
		// deletes it once it has, #2096) — neither is a future action to report.
		if r == "periodic-re-eval" {
			// The safety-net re-evaluation can only re-check; it never changes
			// anything, so presenting its expiry as a next action is a lie (R5).
			continue
		}
		if t := in.Cooldowns[r]; t.After(in.Now) {
			ds = append(ds, Deadline{Kind: "cooldown:" + r, At: t, Basis: "cooldown expiry"})
		}
	}
	anchored := func(kind, label string, d time.Duration, basis string) {
		if d <= 0 {
			return
		}
		dl := Deadline{Kind: kind, Basis: basis}
		if t := in.LabelAppliedAt[label]; !t.IsZero() {
			dl.At = t.Add(d)
		} else {
			dl.Basis += "; label application time is not cached (unknown after a restart)"
		}
		ds = append(ds, dl)
	}
	if hasLabel(in.Labels, "fabrik:awaiting-ci") {
		dl := Deadline{Kind: "ci-liveness-timeout", Basis: "last CI progress + CI wait timeout"}
		if in.Cfg.CIWaitTimeout > 0 {
			if !in.LastCIProgressAt.IsZero() {
				dl.At = in.LastCIProgressAt.Add(in.Cfg.CIWaitTimeout)
			} else {
				dl.Basis += "; no CI progress observed since the daemon started"
			}
			ds = append(ds, dl)
		}
		anchored("ci-backstop-timeout", "fabrik:awaiting-ci", in.Cfg.CIBackstopTimeout, "awaiting-ci applied + CI backstop timeout")
	}
	if hasLabel(in.Labels, "fabrik:awaiting-review") {
		anchored("review-wait-timeout", "fabrik:awaiting-review", in.Cfg.ReviewWaitTimeout, "awaiting-review applied + review wait timeout")
	}
	if hasLabel(in.Labels, "fabrik:bot-reprompted") {
		anchored("bot-reprompt-escalation", "fabrik:bot-reprompted", in.Cfg.ReviewWaitTimeout, "bot-reprompted applied + review wait timeout")
	}
	if hasLabel(in.Labels, "fabrik:claude-limit") && in.ClaudeUsageLimitedUntil.After(in.Now) {
		ds = append(ds, Deadline{Kind: "claude-usage-limit-end", At: in.ClaudeUsageLimitedUntil, Basis: "account-wide usage-limit reset"})
	}
	return ds
}
