package localapi

import "time"

// Envelope is the freshness envelope every result carries (R3).
type Envelope struct {
	// AsOf is when the daemon took the snapshot this result describes.
	AsOf time.Time `json:"as_of"`
	// LastPollAttemptAt / LastPollSuccessAt are the daemon's poll-loop
	// timestamps; unknown until a poll has run in this process.
	LastPollAttemptAt Known[time.Time] `json:"last_poll_attempt_at"`
	LastPollSuccessAt Known[time.Time] `json:"last_poll_success_at"`
	// SinceLastPollSuccessSeconds is AsOf minus LastPollSuccessAt.
	SinceLastPollSuccessSeconds Known[int64] `json:"since_last_poll_success_seconds"`
	DaemonUptimeSeconds         int64        `json:"daemon_uptime_seconds"`
}

// ---- status ----

// StatusParams selects one issue: "owner/repo#N", or "N" when the daemon
// manages exactly one repo.
type StatusParams struct {
	Issue string `json:"issue"`
}

// StatusResult is the fabrik_status result for one issue.
type StatusResult struct {
	Envelope
	Issue     string         `json:"issue"`
	Title     string         `json:"title"`
	URL       Known[string]  `json:"url"`
	Milestone MilestoneField `json:"milestone"`
	// Cache is how stale the daemon's copy of this item is.
	Cache CacheInfo `json:"cache"`

	Place      Place          `json:"place"`
	Limits     []Limit        `json:"limits"`
	Worker     WorkerInfo     `json:"worker"`
	LastInvoke InvocationInfo `json:"last_invocation"`
	PR         PRInfo         `json:"pr"`
	Blockers   BlockersInfo   `json:"blockers"`
	Attention  AttentionInfo  `json:"attention"`
}

// CacheInfo reports an item's cache age (R3: LastDeepFetchAt per item).
type CacheInfo struct {
	LastDeepFetchAt Known[time.Time] `json:"last_deep_fetch_at"`
	AgeSeconds      Known[int64]     `json:"age_seconds"`
}

// Place is identity-and-place: where the item sits on the board and its labels.
type Place struct {
	Status          Known[string]    `json:"status"`
	StatusEnteredAt Known[time.Time] `json:"status_entered_at"`
	// StatusEnteredBasis says what StatusEnteredAt measures: the daemon resets
	// it on every restart (ADR-1833), so it is "since this process first saw
	// the status", not true time-in-column.
	StatusEnteredBasis string   `json:"status_entered_basis"`
	StageLabels        []string `json:"stage_labels"`
	FabrikLabels       []string `json:"fabrik_labels"`
	// Autonomy is "cruise", "yolo" or "none" (cruise wins when both are set).
	Autonomy string `json:"autonomy"`
}

// Limit is one cycle counter reported as "n of max".
type Limit struct {
	Name string   `json:"name"`
	N    int      `json:"n"`
	Max  LimitMax `json:"max"`
}

// WorkerInfo describes the in-flight worker, if any.
type WorkerInfo struct {
	InFlight   bool             `json:"in_flight"`
	Stage      string           `json:"stage,omitempty"`
	StartedAt  Known[time.Time] `json:"started_at"`
	LastSignAt Known[time.Time] `json:"last_sign_at"`
	// TurnsUsed is always unknown for an in-flight worker: the daemon tracks
	// turns only for the last completed invocation.
	TurnsUsed Known[int] `json:"turns_used"`
	MaxTurns  Known[int] `json:"max_turns"`
	// LastTurnsUsed is the last completed invocation's turn count for the
	// stage, when the daemon has one.
	LastTurnsUsed Known[int] `json:"last_turns_used"`
}

// InvocationInfo is the outcome of the last completed Claude invocation.
type InvocationInfo struct {
	// Outcome is completed, blocked, errored, turn-limited, incomplete or unknown.
	Outcome         string         `json:"outcome"`
	IsComment       bool           `json:"is_comment"`
	DurationSeconds Known[float64] `json:"duration_seconds"`
	Tokens          Known[Tokens]  `json:"tokens"`
}

// Tokens is token consumption for one invocation.
type Tokens struct {
	Input         int     `json:"input"`
	Output        int     `json:"output"`
	CacheCreation int     `json:"cache_creation"`
	CacheRead     int     `json:"cache_read"`
	CostUSD       float64 `json:"cost_usd"`
	TurnsUsed     int     `json:"turns_used"`
	MaxTurns      int     `json:"max_turns"`
}

// PRInfo is the linked PR as the cache holds it.
type PRInfo struct {
	// State is "linked", "none" (the cache has fully read the item and it has
	// no PR) or "unknown" (never deep-fetched).
	State          string        `json:"state"`
	Number         Known[int]    `json:"number"`
	PRState        Known[string] `json:"pr_state"`
	Draft          Known[bool]   `json:"draft"`
	Merged         Known[bool]   `json:"merged"`
	Mergeable      Known[string] `json:"mergeable"`
	MergeableState Known[string] `json:"mergeable_state"`
	// CI is green, red, pending or none, from the cached check runs only.
	CI         Known[string]  `json:"ci"`
	HeadSHA    Known[string]  `json:"head_sha"`
	MergeTrain *TrainPosition `json:"merge_train,omitempty"`
}

// TrainPosition is the item's place in its (repo, base) merge-train partition.
type TrainPosition struct {
	Partition      string `json:"partition"`
	Position       int    `json:"position"`
	Of             int    `json:"of"`
	WorkerInFlight bool   `json:"worker_in_flight"`
	// Basis states how the partition was derived (cached base: label).
	Basis string `json:"basis"`
}

// BlockersInfo is the item's dependencies.
type BlockersInfo struct {
	// State is "known" (the item was deep-fetched) or "unknown".
	State string    `json:"state"`
	Items []Blocker `json:"items"`
}

// Blocker is one BlockedBy entry.
type Blocker struct {
	Issue string        `json:"issue"`
	State Known[string] `json:"state"`
}

// AttentionInfo answers "stuck, or waiting correctly?" (R4).
type AttentionInfo struct {
	State   string       `json:"state"`
	Code    string       `json:"code"`
	Summary string       `json:"summary"`
	Reasons []ReasonInfo `json:"reasons"`
	// LatestFabrikComment is the link to the most recent 🏭 comment; unknown
	// when the item's comments were never cached.
	LatestFabrikComment Known[string] `json:"latest_fabrik_comment"`
	Next                NextInfo      `json:"next"`
	Progress            ProgressInfo  `json:"progress"`
	// StallThresholdSeconds is the threshold the classification used.
	StallThresholdSeconds int64 `json:"stall_threshold_seconds"`
}

// ReasonInfo is one label (or condition) with its one-line meaning.
type ReasonInfo struct {
	Code    string `json:"code"`
	Meaning string `json:"meaning"`
}

// NextInfo is what the engine will do next, and when.
type NextInfo struct {
	Action    string           `json:"action"`
	At        Known[time.Time] `json:"at"`
	Deadlines []DeadlineInfo   `json:"deadlines"`
}

// DeadlineInfo is one deadline the engine will act on.
type DeadlineInfo struct {
	Kind  string           `json:"kind"`
	At    Known[time.Time] `json:"at"`
	Basis string           `json:"basis"`
}

// ProgressInfo is the observable-progress anchor.
type ProgressInfo struct {
	At         Known[time.Time] `json:"at"`
	AgeSeconds Known[int64]     `json:"age_seconds"`
}

// ---- board ----

// Board views.
const (
	ViewAttention = "attention"
	ViewFlow      = "flow"
)

// BoardFilter narrows a board view. Zero fields match everything.
type BoardFilter struct {
	// Milestone matches the milestone title (exact), or "none" for items with
	// no milestone. Items whose milestone is unknown never match a milestone
	// filter and are counted in Excluded.UnknownMilestone.
	Milestone string `json:"milestone,omitempty"`
	// Label matches items carrying this exact label.
	Label string `json:"label,omitempty"`
	// Repo matches "owner/repo".
	Repo string `json:"repo,omitempty"`
	// Column matches the board Status exactly.
	Column string `json:"column,omitempty"`
	// HasOpenBlockers, when true, keeps only items with an open blocker.
	HasOpenBlockers bool `json:"has_open_blockers,omitempty"`
}

// BoardParams selects a view and filter. StallThresholdSeconds overrides the
// daemon's default for this request only (0 = daemon default).
type BoardParams struct {
	View                  string       `json:"view"`
	Filter                *BoardFilter `json:"filter,omitempty"`
	StallThresholdSeconds int64        `json:"stall_threshold_seconds,omitempty"`
}

// BoardResult is the fabrik_board result.
type BoardResult struct {
	Envelope
	View                  string      `json:"view"`
	Filter                BoardFilter `json:"filter"`
	StallThresholdSeconds int64       `json:"stall_threshold_seconds"`
	// ItemsScanned is how many cached items were considered before filtering.
	ItemsScanned int `json:"items_scanned"`
	// Excluded counts items a filter could not evaluate because the cache lacks
	// the field (never silently treated as a non-match or a match).
	Excluded ExcludedCounts `json:"excluded"`
	// StatusEnteredBasis: see Place.StatusEnteredBasis.
	StatusEnteredBasis string `json:"status_entered_basis"`

	Attention []AttentionEntry `json:"attention,omitempty"`
	Flow      []ColumnFlow     `json:"flow,omitempty"`
}

// ExcludedCounts reports items excluded for lack of cached data.
type ExcludedCounts struct {
	UnknownMilestone int `json:"unknown_milestone"`
	NotDeepFetched   int `json:"not_deep_fetched"`
}

// AttentionEntry is one item in the attention view.
type AttentionEntry struct {
	Issue      string           `json:"issue"`
	Title      string           `json:"title"`
	Status     Known[string]    `json:"status"`
	Milestone  MilestoneField   `json:"milestone"`
	State      string           `json:"state"`
	Code       string           `json:"code"`
	Reason     string           `json:"reason"`
	Rank       int              `json:"rank"`
	NextAction string           `json:"next_action"`
	NextAt     Known[time.Time] `json:"next_at"`
	Cache      CacheInfo        `json:"cache"`
}

// ColumnFlow is one board column in the flow view.
type ColumnFlow struct {
	Column string `json:"column"`
	Count  int    `json:"count"`
	// OldestInColumn is the longest-resident item (by daemon-observed
	// StatusEnteredAt); unknown when the column is empty.
	OldestInColumn Known[OldestItem] `json:"oldest_in_column"`
	// InFlightWorkers lists the issues in this column with a worker running.
	InFlightWorkers []string `json:"in_flight_workers"`
}

// OldestItem is the longest-resident item of a column.
type OldestItem struct {
	Issue      string           `json:"issue"`
	Since      Known[time.Time] `json:"since"`
	AgeSeconds Known[int64]     `json:"age_seconds"`
}

// ---- health ----

// HealthParams is empty.
type HealthParams struct{}

// HealthResult is the fabrik_health result.
type HealthResult struct {
	Envelope
	Version string `json:"version"`

	Poll       PollHealth       `json:"poll"`
	Webhook    WebhookHealth    `json:"webhook"`
	Reconcile  ReconcileHealth  `json:"reconcile"`
	Claude     ClaudeHealth     `json:"claude_usage_limit"`
	Backoff    BackoffHealth    `json:"graphql_backoff"`
	Workers    WorkerSlots      `json:"workers"`
	MergeTrain []TrainPartition `json:"merge_train"`
}

// PollHealth describes the poll loop.
type PollHealth struct {
	LastAttemptAt       Known[time.Time] `json:"last_attempt_at"`
	LastSuccessAt       Known[time.Time] `json:"last_success_at"`
	SinceAttemptSeconds Known[int64]     `json:"since_attempt_seconds"`
	SinceSuccessSeconds Known[int64]     `json:"since_success_seconds"`
}

// WebhookHealth describes webhook ingestion.
type WebhookHealth struct {
	// Mode is "off", "healthy" or "unhealthy" (or "unknown" with no manager info).
	Mode              string           `json:"mode"`
	LastEventAt       Known[time.Time] `json:"last_event_at"`
	SinceEventSeconds Known[int64]     `json:"since_event_seconds"`
}

// ReconcileHealth describes the periodic board reconcile.
type ReconcileHealth struct {
	LastSuccessAt       Known[time.Time] `json:"last_success_at"`
	SinceSuccessSeconds Known[int64]     `json:"since_success_seconds"`
}

// ClaudeHealth is the account-wide usage-limit suspension.
type ClaudeHealth struct {
	Suspended      bool             `json:"suspended"`
	SuspendedUntil Known[time.Time] `json:"suspended_until"`
}

// BackoffHealth mirrors the GraphQL rate-limit backoff state.
type BackoffHealth struct {
	// Observed is false until a poll has reported a rate-limit reading.
	Observed   bool           `json:"observed"`
	Remaining  Known[int]     `json:"remaining"`
	Low        Known[bool]    `json:"low"`
	RestPaused Known[bool]    `json:"rest_paused"`
	Ratio      Known[float64] `json:"ratio"`
}

// WorkerSlots is worker concurrency.
type WorkerSlots struct {
	InUse         int        `json:"in_use"`
	MaxConcurrent Known[int] `json:"max_concurrent"`
}

// TrainPartition is one (repo, base) merge-train partition.
type TrainPartition struct {
	Repo           string   `json:"repo"`
	Base           string   `json:"base"`
	Members        []string `json:"members"`
	WorkerInFlight bool     `json:"worker_in_flight"`
}
