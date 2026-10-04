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
	// DigestSeconds batches ordinary events into one message per interval;
	// 0 delivers each at once. validate-settled, escalated, paused and
	// daemon-unreachable are always immediate.
	DigestSeconds int `json:"digest_seconds,omitempty"`
}

// EffectiveExclude is the exclude list in force.
func (s Subscription) EffectiveExclude() []string {
	if s.ExcludeLabels == nil {
		return DefaultExcludeLabels
	}
	return *s.ExcludeLabels
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
	b, _ := json.Marshal(s)
	return string(b)
}
