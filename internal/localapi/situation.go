package localapi

import "time"

// The situation view (#2171, ADR 2171): one rolled-up "what needs you" answer,
// built by one engine builder and carried by three paths that must agree — the
// fabrik_situation tool, the `situation` digest event and the attach/reconnect
// catch-up. Everything here is wire shape only; the classification behind it is
// internal/attention's, never a second one.

// MethodSituation is the local-API method behind fabrik_situation. It is served
// only when the Server has a SituationBackend (advertised by hello then), so an
// older daemon answers unknown_method.
const MethodSituation = "situation"

// SituationBackend is the optional half of the daemon API that serves the
// situation view, beside Backend, Streamer and Actor. Like Backend it answers
// purely from in-memory state — no GitHub call, ever — but a push request also
// writes to the channel hub (never to GitHub).
type SituationBackend interface {
	Situation(SituationParams) (*SituationResult, error)
}

// SituationParams asks for the view. Push additionally queues it to Subscriber
// as a `situation` digest; Force sends it even when it equals the last digest
// that subscriber got. Both are ignored without a subscriber.
type SituationParams struct {
	Subscriber string `json:"subscriber,omitempty"`
	Push       bool   `json:"push,omitempty"`
	Force      bool   `json:"force,omitempty"`
}

// SituationResult is the fabrik_situation result: the account-wide, unscoped
// view, with the freshness envelope the other read tools carry.
type SituationResult struct {
	Envelope
	View SituationView `json:"view"`
	// Pushed is true when a push was requested and a digest was queued.
	Pushed bool `json:"pushed,omitempty"`
	// PushNote says why nothing was queued when a push was requested.
	PushNote string `json:"push_note,omitempty"`
}

// SituationSection is one bounded, rank-ordered list. Omitted counts the entries
// cut by the bound, so a truncated section is never mistaken for a complete one.
type SituationSection[T any] struct {
	Entries []T `json:"entries"`
	Omitted int `json:"omitted"`
}

// SituationView is the rolled-up view. Every item appears in at most one of the
// item sections (MergeDecisions, Gates, HumanInput, Decisions, Stalled); the
// account sections (MergeTrain, Health, Anomalies) are always whole-account.
type SituationView struct {
	// NothingNeedsYou is true when no item section and no anomaly has an entry
	// and every open managed item could be classified.
	NothingNeedsYou bool `json:"nothing_needs_you"`
	// UnclassifiedItems counts open managed items whose labels the daemon has
	// not read yet (just after a restart): they cannot be placed in a section,
	// and are reported rather than assumed healthy.
	UnclassifiedItems int `json:"unclassified_items"`

	// MergeDecisions: PRs settled at Validate under cruise, awaiting a human merge.
	MergeDecisions SituationSection[SituationEntry] `json:"merge_decisions"`
	// Gates: items held at the feedback, review, CI or merge gate.
	Gates SituationSection[SituationEntry] `json:"gates"`
	// HumanInput: paused items and escalations waiting on a human.
	HumanInput SituationSection[SituationEntry] `json:"human_input"`
	// Decisions: suspended items — a pending decision or a parked blocker.
	Decisions SituationSection[SituationEntry] `json:"decisions"`
	// Stalled: items with no observable progress past the stall threshold.
	Stalled SituationSection[SituationEntry] `json:"stalled"`

	MergeTrain SituationSection[TrainPartition]   `json:"merge_train"`
	Health     SituationHealth                    `json:"health"`
	Anomalies  SituationSection[SituationAnomaly] `json:"anomalies"`
}

// SituationEntry is one item. The common fields are always present; exactly the
// section-specific block that applies is set.
type SituationEntry struct {
	Issue string        `json:"issue"`
	Title string        `json:"title"`
	URL   Known[string] `json:"url"`
	State string        `json:"state"`
	Code  string        `json:"code"`
	// Reason is the classifier's one-line reason.
	Reason string `json:"reason"`
	Rank   int    `json:"rank"`
	// Options lists how a human can act, each with the MCP tool that performs it
	// when one exists.
	Options []OptionInfo `json:"options,omitempty"`
	// LatestFabrikComment links the most recent 🏭 comment.
	LatestFabrikComment Known[string] `json:"latest_fabrik_comment"`

	Merge   *SituationMerge `json:"merge,omitempty"`
	Gate    *SituationGate  `json:"gate,omitempty"`
	Human   *SituationHuman `json:"human,omitempty"`
	Pending *PendingInfo    `json:"pending,omitempty"`
}

// SituationMerge is the merge decision's PR: its number and the review and CI
// verdicts as the daemon's cache holds them (the same caveats as fabrik_status).
type SituationMerge struct {
	PR     Known[int]    `json:"pr"`
	Review Known[string] `json:"review"`
	CI     Known[string] `json:"ci"`
}

// SituationGate names the gate holding an item and for how long. HeldSince is
// unknown when the hold began before this daemon run, never "now".
type SituationGate struct {
	Gate        string           `json:"gate"`
	Reason      string           `json:"reason,omitempty"`
	HeldSince   Known[time.Time] `json:"held_since"`
	HeldSeconds Known[int64]     `json:"held_seconds"`
}

// SituationHuman is a human-input entry: how the item is resumed.
type SituationHuman struct {
	// ResumeVia is remove-label, human-comment or see-pause-comment.
	ResumeVia string `json:"resume_via,omitempty"`
}

// SituationHealth is poll and worker health, from the same sources as
// fabrik_health.
type SituationHealth struct {
	Poll    PollHealth   `json:"poll"`
	Workers WorkerSlots  `json:"workers"`
	Claude  ClaudeHealth `json:"claude_usage_limit"`
	// TrainQueued is the number of merge-train members waiting in Queued, summed
	// over partitions. "Queued" here means train-queued: a dispatch deferred for
	// lack of a worker slot is not counted.
	TrainQueued int `json:"train_queued"`
}

// Anomaly kinds.
const (
	AnomalyPipelineStalled = "pipeline-stalled"
	AnomalyRetryStarved    = "retry-starved"
	AnomalyItemWedged      = "item-wedged"
	AnomalyCacheDivergence = "cache-divergence"
)

// SituationAnomaly is one active anomaly. Subject is the issue (item-scoped), the
// partition key (merge-train) or "poll".
type SituationAnomaly struct {
	Kind    string           `json:"kind"`
	Scope   string           `json:"scope"`
	Subject string           `json:"subject"`
	Title   string           `json:"title,omitempty"`
	URL     Known[string]    `json:"url"`
	Summary string           `json:"summary"`
	Since   Known[time.Time] `json:"since"`
}
