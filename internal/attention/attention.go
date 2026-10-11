// Package attention classifies one board item as needs-human, escalated,
// waiting, working, stalled or idle, and says what the engine will do next and
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
)

// Attention-view ranks: lower is more urgent. NotRanked items are omitted from
// the attention view.
const (
	RankNeedsHuman    = 0
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

	// ClaudeSuspendedUntil is the account-wide usage-limit suspension end; zero if none.
	ClaudeSuspendedUntil time.Time
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
}

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
	"fabrik:claude-limit":                  {"the Claude account usage limit was hit; the engine retries once the suspension lifts", true},
	"fabrik:tools-denied":                  {"a tool call was denied by the permission layer; the engine retries, bounded by its own counter", true},
	"fabrik:api-key-helper-detected":       {"the worktree sets apiKeyHelper, which Fabrik refuses; remove it from the worktree settings", false},
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
		return finish(Escalated, code, summary,
			"nothing automatic: the engine has stopped; a human must intervene (fix the cause, then comment or remove fabrik:paused)", RankEscalated)
	}

	// 3. Needs-human: paused with a question or without an escalation signal.
	if paused {
		code, summary := "fabrik:paused", "paused; a human comment resumes it"
		if hasLabel(in.Labels, "fabrik:awaiting-input") {
			code, summary = "fabrik:awaiting-input", "the engine is waiting for a human reply"
		}
		return finish(NeedsHuman, code, summary,
			"nothing automatic: a human comment created after the pause resumes this item", RankNeedsHuman)
	}

	// 4. Settled at Validate under cruise: only a human merge is left.
	if in.Status == "Validate" && hasLabel(in.Labels, "stage:Validate:complete") &&
		hasLabel(in.Labels, "fabrik:cruise") && in.HasOpenUnmergedPR && !in.HasWorker {
		return finish(NeedsHuman, CodeAwaitingMergeDecision,
			"Validate is complete under cruise; the PR awaits a human merge decision",
			"nothing automatic: cruise never merges; a human merges the PR", RankMergeDecision)
	}

	// 4b. The engine refuses to run until a human edits something it cannot
	// edit itself; no retry or deadline will ever clear it.
	if hasLabel(in.Labels, "fabrik:api-key-helper-detected") {
		return finish(NeedsHuman, "fabrik:api-key-helper-detected",
			labelTable["fabrik:api-key-helper-detected"].meaning,
			"nothing automatic: the engine skips this item until apiKeyHelper is removed from the worktree settings", RankNeedsHuman)
	}

	// 5. Waiting on something the engine owns.
	if code, summary, ok := waitingCause(in); ok {
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
	for _, t := range in.LabelAppliedAt {
		consider(t)
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
	case kind == "claude-suspension-end":
		return "the Claude usage-limit suspension lifts"
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
	if hasLabel(in.Labels, "fabrik:claude-limit") && in.ClaudeSuspendedUntil.After(in.Now) {
		ds = append(ds, Deadline{Kind: "claude-suspension-end", At: in.ClaudeSuspendedUntil, Basis: "account-wide usage-limit reset"})
	}
	return ds
}
