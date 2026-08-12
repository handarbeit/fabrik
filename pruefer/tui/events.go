// Package tui implements Pruefer's terminal UI, structurally mirroring
// Fabrik's own tui/ package (event types -> Component tree -> thin wiring
// layer) without importing it — see adrs/1114-pruefer-tui-architecture.md
// and adrs/1113-pruefer-v1-architecture.md's "no shared Go imports with
// engine" constraint, which this package also honors.
package tui

import (
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// Event is the interface implemented by all typed daemon events.
type Event interface {
	tuiEvent()
}

// RepoPollEvent is emitted once per watched repo per poll cycle, at the
// existing ListOpenPRs call site — combining last-poll timestamp, PR count
// found, and last error into one event (Pruefer's "poll status" per repo).
type RepoPollEvent struct {
	Repo    string // "owner/repo"
	At      time.Time
	PRCount int
	Err     string // empty means no error
}

func (RepoPollEvent) tuiEvent() {}

// ReviewStartedEvent is emitted by poll()'s dispatch goroutine just before
// calling ReviewPR, not from inside ReviewPR itself — ReviewPR's control
// flow is untouched by TUI wiring.
type ReviewStartedEvent struct {
	Repo      string // "owner/repo"
	PRNumber  int
	Title     string
	StartedAt time.Time
}

func (ReviewStartedEvent) tuiEvent() {}

// ReviewCompletedEvent is emitted by poll()'s dispatch goroutine after
// ReviewPR returns, carrying the full outcome. Reason is the SkipReason
// rendered as a string (not the pruefer.SkipReason type) to keep this
// package free of a dependency on the pruefer package it is imported by.
type ReviewCompletedEvent struct {
	Repo        string
	PRNumber    int
	Title       string
	Reviewed    bool
	Skipped     bool
	Reason      string // set iff Skipped
	Err         string // set iff a genuine failure occurred; empty otherwise
	NumTurns    int
	CostUSD     float64
	Duration    time.Duration
	CompletedAt time.Time
}

func (ReviewCompletedEvent) tuiEvent() {}

// RateLimitSnapshotEvent is emitted once per owner client per poll cycle
// with that client's current REST API rate-limit stats. Pruefer only issues
// REST calls (ListOpenPRs, FetchPRDiff, FetchPRReviews, SubmitPRReview), so
// this always carries github.Client.RateLimitStats()'s rest return value,
// never graphql. Owner identifies which owner's installation client the
// snapshot came from; the footer displays whichever snapshot arrived most
// recently rather than aggregating across owners.
type RateLimitSnapshotEvent struct {
	Owner string
	Stats gh.RateLimitStats
}

func (RateLimitSnapshotEvent) tuiEvent() {}

// TickEvent is emitted once per second by the TUI loop to drive
// elapsed-time and countdown updates.
type TickEvent struct {
	At time.Time
}

func (TickEvent) tuiEvent() {}

// DropEvent reports the cumulative count for one drop-reason category — see
// events.DropReason (pruefer/events) and ADR-1563: an operator must be able
// to tell "no events because nothing happened" apart from "no events
// because every delivery is being silently discarded" without reading
// logs, and a single lumped "dropped" counter wouldn't distinguish
// benign categories (an unwatched repo, a dedupe hit) from an actionable
// one (a signature failure). Total is the cumulative count, not a delta —
// see Daemon.recordDrop's doc comment for why (TUI events can be dropped
// under channel backpressure; a total self-heals on the next delivery, a
// delta would not). Reason is the DropReason rendered as a plain string,
// not the events.DropReason type, to keep this package free of a
// dependency on pruefer/events — mirroring ReviewCompletedEvent.Reason.
type DropEvent struct {
	Reason string
	Total  int
	At     time.Time
}

func (DropEvent) tuiEvent() {}

// SignatureDriftEvent reports a transition in whether an event source's
// signature verification is currently failing on every delivery over a
// sustained window — see hookdeck.Config.OnSignatureDrift and ADR-1563.
// Active=true means the drift is ongoing (a misconfigured webhook secret,
// or a wire-format change — not a transient blip); Active=false reports
// recovery, fired once on the next successful verification after an
// active streak.
type SignatureDriftEvent struct {
	Active bool
	At     time.Time
}

func (SignatureDriftEvent) tuiEvent() {}
