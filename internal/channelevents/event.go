package channelevents

import "time"

// EventType names one kind of pushed event. The values are the wire names the
// session sees in the channel tag's `event` attribute.
type EventType string

const (
	ValidateSettled           EventType = "validate-settled"
	LabelApplied              EventType = "label-applied"
	LabelRemoved              EventType = "label-removed"
	Paused                    EventType = "paused"
	AwaitingInput             EventType = "awaiting-input"
	AwaitingInputStale        EventType = "awaiting-input-stale"
	Escalated                 EventType = "escalated"
	Stalled                   EventType = "stalled"
	CycleLimitNear            EventType = "cycle-limit-near"
	MergeTrainEjected         EventType = "merge-train-ejected"
	MergeTrainFailed          EventType = "merge-train-failed"
	CITimeout                 EventType = "ci-timeout"
	ReviewTimeout             EventType = "review-timeout"
	Merged                    EventType = "merged"
	BlockerCleared            EventType = "blocker-cleared"
	ChildrenSpawned           EventType = "children-spawned"
	LandingVerificationFailed EventType = "landing-verification-failed"
	ClaudeLimitSuspended      EventType = "claude-limit-suspended"
	ClaudeLimitLifted         EventType = "claude-limit-lifted"
	DaemonUnreachable         EventType = "daemon-unreachable"
	DaemonReachable           EventType = "daemon-reachable"

	// EventsDropped is synthetic: the hub inserts it at the head of a
	// subscriber's queue when overflow discarded the oldest events.
	EventsDropped EventType = "events-dropped"
	// Digest is synthetic: one batched delivery of several ordinary events.
	Digest EventType = "digest"
	// StreamSuperseded is synthetic and shim-generated: another session attached
	// under the same subscriber name, so this session's push has stopped.
	StreamSuperseded EventType = "stream-superseded"
)

// Event is one thing that happened, ready to route and deliver.
//
// Repo/Issue/Milestone*/Label are routing attributes the hub matches
// subscriptions against; Meta is the delivered key/value set (R5). Meta keys
// must be identifiers — Claude Code silently drops any other key — and values
// are strings.
type Event struct {
	Type    EventType         `json:"type"`
	Repo    string            `json:"repo,omitempty"`
	Issue   int               `json:"issue,omitempty"`
	Stage   string            `json:"stage,omitempty"`
	PR      int               `json:"pr,omitempty"`
	Content string            `json:"content"`
	Meta    map[string]string `json:"meta,omitempty"`
	// CommentURL links the triggering 🏭 Fabrik comment when there is one.
	CommentURL string `json:"comment_url,omitempty"`

	// Label is the label a label-applied/-removed event is about; the
	// subscription label patterns match it.
	Label string `json:"label,omitempty"`
	// MilestoneKnown reports whether the item's milestone has been captured.
	// An item whose milestone is unknown never matches a milestone scope.
	MilestoneKnown  bool   `json:"milestone_known,omitempty"`
	MilestoneTitle  string `json:"milestone_title,omitempty"`
	MilestoneNumber int    `json:"milestone_number,omitempty"`

	// DedupKey identifies the transition: a second event with the same non-empty
	// key is dropped by the hub, across restarts. Empty means "never dedup".
	DedupKey string    `json:"dedup_key,omitempty"`
	At       time.Time `json:"at"`
}

// Info describes the catalogue entry for an event type.
type Info struct {
	// Immediate events are delivered at once even in digest mode (R7).
	Immediate bool
	// AccountWide events have no issue; they ignore repo/issue/milestone scopes
	// and reach every subscriber whose event filter includes them.
	AccountWide bool
	// ShimGenerated events are produced by the fabrik mcp shim, never the daemon.
	ShimGenerated bool
	// Synthetic events are hub bookkeeping and cannot be subscribed to.
	Synthetic bool
}

var catalog = map[EventType]Info{
	ValidateSettled:           {Immediate: true},
	LabelApplied:              {},
	LabelRemoved:              {},
	Paused:                    {Immediate: true},
	AwaitingInput:             {},
	AwaitingInputStale:        {},
	Escalated:                 {Immediate: true},
	Stalled:                   {},
	CycleLimitNear:            {},
	MergeTrainEjected:         {},
	MergeTrainFailed:          {},
	CITimeout:                 {},
	ReviewTimeout:             {},
	Merged:                    {},
	BlockerCleared:            {},
	ChildrenSpawned:           {},
	LandingVerificationFailed: {},
	ClaudeLimitSuspended:      {AccountWide: true},
	ClaudeLimitLifted:         {AccountWide: true},
	DaemonUnreachable:         {Immediate: true, AccountWide: true, ShimGenerated: true},
	DaemonReachable:           {AccountWide: true, ShimGenerated: true},
	EventsDropped:             {Immediate: true, AccountWide: true, Synthetic: true},
	Digest:                    {Immediate: true, AccountWide: true, Synthetic: true},
	StreamSuperseded:          {Immediate: true, AccountWide: true, ShimGenerated: true, Synthetic: true},
}

// Lookup returns the catalogue entry for t.
func Lookup(t EventType) (Info, bool) {
	i, ok := catalog[t]
	return i, ok
}

// IsImmediate reports whether t bypasses digest batching.
func IsImmediate(t EventType) bool { return catalog[t].Immediate }

// IsAccountWide reports whether t has no issue scope.
func IsAccountWide(t EventType) bool { return catalog[t].AccountWide }

// Subscribable lists every event type a subscription may name, in catalogue order.
func Subscribable() []EventType {
	order := []EventType{
		ValidateSettled, LabelApplied, LabelRemoved, Paused, AwaitingInput, AwaitingInputStale,
		Escalated, Stalled, CycleLimitNear, MergeTrainEjected, MergeTrainFailed, CITimeout,
		ReviewTimeout, Merged, BlockerCleared, ChildrenSpawned, LandingVerificationFailed,
		ClaudeLimitSuspended, ClaudeLimitLifted, DaemonUnreachable, DaemonReachable,
	}
	return order
}

// IsLabelEvent reports whether label patterns apply to t.
func IsLabelEvent(t EventType) bool { return t == LabelApplied || t == LabelRemoved }
