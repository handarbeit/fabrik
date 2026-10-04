package localapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
)

// UnknownText is the JSON string a field marshals to when the daemon cannot
// vouch for its value (R3). It is never a healthy default.
const UnknownText = "unknown"

// Known is a value that may be unknown. A zero Known is unknown. It marshals as
// the value when Valid, and as the string "unknown" otherwise.
type Known[T any] struct {
	Valid bool
	V     T
}

// Some returns a known value.
func Some[T any](v T) Known[T] { return Known[T]{Valid: true, V: v} }

// Unknown returns an unknown value.
func Unknown[T any]() Known[T] { return Known[T]{} }

// KnownTime is Some(t) for a non-zero t, and unknown for the zero time — the
// cache's convention for "never recorded".
func KnownTime(t time.Time) Known[time.Time] {
	if t.IsZero() {
		return Known[time.Time]{}
	}
	return Some(t.UTC())
}

// MarshalJSON implements json.Marshaler.
func (k Known[T]) MarshalJSON() ([]byte, error) {
	if !k.Valid {
		return json.Marshal(UnknownText)
	}
	return json.Marshal(k.V)
}

// UnmarshalJSON implements json.Unmarshaler.
func (k *Known[T]) UnmarshalJSON(b []byte) error {
	if bytes.Equal(bytes.TrimSpace(b), []byte(`"`+UnknownText+`"`)) {
		*k = Known[T]{}
		return nil
	}
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		return fmt.Errorf("known value: %w", err)
	}
	*k = Known[T]{Valid: true, V: v}
	return nil
}

// Milestone states.
const (
	MilestoneSet     = "set"
	MilestoneNone    = "none"
	MilestoneUnknown = UnknownText
)

// MilestoneField is an item's milestone: set (title and number), none (the
// cache captured the item and it has no milestone) or unknown (the cache has
// not captured it). Unknown must never be reported as none (R10).
type MilestoneField struct {
	State  string
	Title  string
	Number int
}

// MarshalJSON implements json.Marshaler: "unknown", "none" or {"title","number"}.
func (m MilestoneField) MarshalJSON() ([]byte, error) {
	switch m.State {
	case MilestoneSet:
		return json.Marshal(struct {
			Title  string `json:"title"`
			Number int    `json:"number"`
		}{m.Title, m.Number})
	case MilestoneNone:
		return json.Marshal(MilestoneNone)
	default:
		return json.Marshal(UnknownText)
	}
}

// UnmarshalJSON implements json.Unmarshaler.
func (m *MilestoneField) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		switch s {
		case MilestoneNone:
			*m = MilestoneField{State: MilestoneNone}
		default:
			*m = MilestoneField{State: MilestoneUnknown}
		}
		return nil
	}
	var o struct {
		Title  string `json:"title"`
		Number int    `json:"number"`
	}
	if err := json.Unmarshal(b, &o); err != nil {
		return fmt.Errorf("milestone: %w", err)
	}
	*m = MilestoneField{State: MilestoneSet, Title: o.Title, Number: o.Number}
	return nil
}

// LimitMax is a cycle counter's configured limit: a number, "unlimited" or
// "unknown" (a zero limit on an engine that never resolved one must not read
// as "limit 0").
type LimitMax struct {
	Kind  string // "limit", "unlimited" or "unknown"
	Value int
}

// MarshalJSON implements json.Marshaler.
func (l LimitMax) MarshalJSON() ([]byte, error) {
	switch l.Kind {
	case "limit":
		return json.Marshal(l.Value)
	case "unlimited":
		return json.Marshal("unlimited")
	default:
		return json.Marshal(UnknownText)
	}
}

// UnmarshalJSON implements json.Unmarshaler.
func (l *LimitMax) UnmarshalJSON(b []byte) error {
	var n int
	if err := json.Unmarshal(b, &n); err == nil {
		*l = LimitMax{Kind: "limit", Value: n}
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("limit max: %w", err)
	}
	if s == "unlimited" {
		*l = LimitMax{Kind: "unlimited"}
	} else {
		*l = LimitMax{Kind: "unknown"}
	}
	return nil
}
