package channelevents

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Digest interval bounds (R7).
const (
	MinDigest = 30 * time.Second
	MaxDigest = time.Hour
)

// Reminder interval bounds (#2120 R6a). A reminder is opt-in, so there is no
// default; the floor keeps a mistyped value from becoming a nag.
const (
	MinRemind = 10 * time.Minute
	MaxRemind = 30 * 24 * time.Hour
)

// IssueRef scopes a subscription to one issue. Repo may be empty to mean "any
// repo" when the subscriber gave a number the daemon could not resolve to one.
type IssueRef struct {
	Repo   string `json:"repo,omitempty"`
	Number int    `json:"number"`
}

// Subscription is one subscriber's standing request for events.
type Subscription struct {
	ID         string     `json:"id"`
	Subscriber string     `json:"subscriber"`
	Repos      []string   `json:"repos,omitempty"`
	Issues     []IssueRef `json:"issues,omitempty"`
	// Milestone is a milestone title, or "#N" for a milestone number.
	Milestone string `json:"milestone,omitempty"`
	// Labels are include patterns for label events; empty = every label.
	Labels []string `json:"labels,omitempty"`
	// ExcludeLabels are exclude patterns for label events. nil selects
	// DefaultExcludeLabels; a non-nil empty slice opts into everything.
	ExcludeLabels *[]string   `json:"exclude_labels,omitempty"`
	Events        []EventType `json:"events,omitempty"`
	// EventsDefault marks a subscription created without an events list on a
	// daemon that has the semantic default (#2169): an empty Events then means
	// "every type except the raw label events". Absent (a subscription
	// persisted before that) keeps the old meaning, every type — grandfathered
	// so a running overseer never silently loses events. It is not part of the
	// subscription's identity (canonical), so re-subscribing with the same
	// filters stays idempotent.
	EventsDefault bool `json:"events_default,omitempty"`
	// DigestSeconds batches ordinary events into one message per interval;
	// 0 delivers each at once. validate-settled, escalated, paused and
	// daemon-unreachable are always immediate.
	DigestSeconds int `json:"digest_seconds,omitempty"`
	// ProgressViaColumn drops label-applied/-removed for stage:*:complete and
	// stage:*:in_progress when the subscription also takes column-changed, so an
	// overseer can follow progress and decisions without the stage-label stream
	// (#2123). It wins over Labels and an empty ExcludeLabels. Default off.
	ProgressViaColumn bool `json:"progress_via_column,omitempty"`
	// RemindAfterSeconds re-announces a still-pending decision-pending episode
	// to this subscription once per interval, marked as a reminder (#2120 R6a).
	// 0 (the default) means never. The interval runs from the persisted time of
	// the original announcement or the previous reminder, never from daemon
	// uptime.
	RemindAfterSeconds int `json:"remind_after_seconds,omitempty"`
}

// StageProgressLabels are the labels ProgressViaColumn suppresses.
var StageProgressLabels = []string{"stage:*:complete", "stage:*:in_progress"}

// TakesLabelEvents reports whether the subscription's event filter admits the
// raw label-applied / -removed events: named explicitly, or an empty filter on a
// grandfathered subscription. An empty filter on a semantic-default
// subscription does not (#2169 R3).
func (s Subscription) TakesLabelEvents() bool {
	if len(s.Events) == 0 {
		return !s.EventsDefault
	}
	return containsType(s.Events, LabelApplied) || containsType(s.Events, LabelRemoved)
}

// TakesColumnChanged reports whether the subscription's event filter admits
// column-changed (an empty filter means every type).
func (s Subscription) TakesColumnChanged() bool {
	return len(s.Events) == 0 || containsType(s.Events, ColumnChanged)
}

// EffectiveExclude is the exclude list in force.
func (s Subscription) EffectiveExclude() []string {
	if s.ExcludeLabels == nil {
		return DefaultExcludeLabels
	}
	return *s.ExcludeLabels
}

// Remind returns the reminder interval, zero when reminders are off.
func (s Subscription) Remind() time.Duration {
	return time.Duration(s.RemindAfterSeconds) * time.Second
}

// Digest returns the batching interval, zero when not in digest mode.
func (s Subscription) Digest() time.Duration { return time.Duration(s.DigestSeconds) * time.Second }

// Validate checks the subscription's own fields. minDigest lets tests shorten
// the lower bound; pass MinDigest in production.
func (s Subscription) Validate(minDigest time.Duration) error {
	if err := ValidateSubscriberName(s.Subscriber); err != nil {
		return err
	}
	for _, r := range s.Issues {
		if r.Number <= 0 {
			return fmt.Errorf("issue number must be positive, got %d", r.Number)
		}
	}
	for _, t := range s.Events {
		info, ok := Lookup(t)
		if !ok || info.Synthetic {
			return fmt.Errorf("unknown event type %q", t)
		}
	}
	for _, p := range append(append([]string(nil), s.Labels...), s.EffectiveExclude()...) {
		if p == "" {
			return fmt.Errorf("empty label pattern")
		}
	}
	if s.DigestSeconds != 0 {
		d := s.Digest()
		if d < minDigest || d > MaxDigest {
			return fmt.Errorf("digest interval %s out of range [%s, %s]", d, minDigest, MaxDigest)
		}
	}
	if s.ProgressViaColumn && !s.TakesColumnChanged() {
		return fmt.Errorf("progress_via_column needs column-changed in events (or an empty events list)")
	}
	if s.RemindAfterSeconds != 0 {
		if d := s.Remind(); d < MinRemind || d > MaxRemind {
			return fmt.Errorf("remind interval %s out of range [%s, %s]", d, MinRemind, MaxRemind)
		}
	}
	if s.Milestone != "" && strings.TrimSpace(s.Milestone) == "" {
		return fmt.Errorf("empty milestone")
	}
	return nil
}

// ValidateSubscriberName enforces a stable, human-meaningful identity.
func ValidateSubscriberName(name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("subscriber name is required")
	}
	if len(name) > 128 {
		return fmt.Errorf("subscriber name longer than 128 bytes")
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("subscriber name contains a control character")
		}
	}
	return nil
}

// canonical is the subscription with its ID blanked, as a comparison key for
// idempotent re-subscription.
func (s Subscription) canonical() string {
	s.ID = ""
	s.EventsDefault = false
	b, _ := json.Marshal(s)
	return string(b)
}
