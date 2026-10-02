// Package registry is the single per-test registry for the live e2e suite
// (tests/e2e). It is keyed by live-test function name and currently carries
// the sim-parity field and the auth/train sensitivity fields (#1975); later
// issues add fields (pack, exclusive, ...) to Entry rather than creating their
// own lists.
//
// The package is deliberately NOT behind the `e2e` build tag: the live suite is
// tagged, but the completeness check must run in plain `go test ./...`. Live and
// sim tests are therefore discovered by parsing source (scan.go), never by
// importing them.
//
// The data lives in registry.json so shell (`jq`) can read it with no Go
// toolchain in the path. See adrs/1933-per-test-registry-and-sim-parity.md.
package registry

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
)

// Version is the registry.json schema version. New fields are additive and do
// not bump it.
const Version = 1

// Parity says how a live test relates to the sim bed.
type Parity string

const (
	// ParitySim: a sim test (top-level Test* in tests/sim) covers the same engine path.
	ParitySim Parity = "sim"
	// ParityLiveOnly: no sim twin, for a reason from the fixed vocabulary.
	ParityLiveOnly Parity = "live-only"
	// ParityGap: no sim twin and no legitimate live-only reason. Allowed but counted.
	ParityGap Parity = "gap"
)

// Reason is the fixed vocabulary for ParityLiveOnly.
type Reason string

const (
	ReasonModelJudgement Reason = "model-judgement" // depends on what a real model decides
	ReasonWireFormat     Reason = "wire-format"     // shape of a GitHub request/response
	ReasonReviewBot      Reason = "review-bot"      // real external reviewer behaviour
	ReasonRealCI         Reason = "real-ci"         // real Actions / check-suite timing
	ReasonAppAuth        Reason = "app-auth"        // real App token minting / installation behaviour
	ReasonOther          Reason = "other"           // free text in Note (required)
)

// Reasons lists the accepted live-only reasons.
var Reasons = []Reason{
	ReasonModelJudgement, ReasonWireFormat, ReasonReviewBot,
	ReasonRealCI, ReasonAppAuth, ReasonOther,
}

// Sensitivity says whether a live test's outcome can depend on one axis of the
// gate's auth x train matrix (#1975, ADR-1975). There is deliberately no default:
// every test declares both axes.
type Sensitivity string

const (
	// Sensitive: the test's subject, or something it depends on, changes with the axis.
	Sensitive Sensitivity = "sensitive"
	// Neutral: the axis changes nothing the test can observe.
	Neutral Sensitivity = "neutral"
)

// Entry is one live test's registry record. JSON keys are named, never
// positional, so fields can be added without restructuring.
type Entry struct {
	// Name is the live e2e test function name (the registry key).
	Name string `json:"name"`
	// Parity is the discriminator: sim | live-only | gap.
	Parity Parity `json:"parity"`
	// Sim lists the tests/sim top-level Test* names covering the same path.
	// Required and non-empty iff Parity == sim; forbidden otherwise.
	Sim []string `json:"sim,omitempty"`
	// LiveOnlyReason is required iff Parity == live-only.
	LiveOnlyReason Reason `json:"live_only_reason,omitempty"`
	// Note is free text: required for reason "other"; optional elsewhere
	// (a unit-test pointer for a gap, "train-off leg only" for a partial twin).
	Note string `json:"note,omitempty"`
	// SkipOKLegs lists leg-label patterns ("<auth>/<train>", "*" matches any
	// part) on which this test is EXPECTED to skip itself for a structural reason
	// (e.g. a merge-train scenario under train mode "off"). The coverage ledger
	// (#1972, ADR-1972) reports such a skip as structural: it neither blocks the
	// gate nor counts as covered. Any other skip with no open cited issue counts as
	// missing. Complementary to Auth/Train, not subsumed by them: sensitivity says
	// where a test SHOULD run, this says where a skip is EXPECTED — and the sparse
	// plan (ADR-1975) drops a (test, cell) pair this matches from every non-baseline
	// cell rather than restart the bed just to watch the test skip.
	SkipOKLegs []string `json:"skip_ok_legs,omitempty"`
	// Auth is whether the test is sensitive to the auth mode (pat | app): access
	// (token source, git credentials, worker GH_TOKEN), identity (self-recognition,
	// lock labels, mentions, assignees, commit/PR authorship) or permissions.
	// Required, no default (#1975).
	Auth Sensitivity `json:"auth"`
	// AuthReason is one non-empty line, required iff Auth == sensitive.
	AuthReason string `json:"auth_reason,omitempty"`
	// Train is whether the test is sensitive to the merge-train mode (off | on):
	// its subject is, or depends on, the landing path. Required, no default (#1975).
	Train Sensitivity `json:"train"`
	// TrainReason is one non-empty line, required iff Train == sensitive.
	TrainReason string `json:"train_reason,omitempty"`
	// Exclusive marks a test that takes over the whole bed: it stops, restarts or
	// reconfigures it, or holds or poisons bed-wide state (#1977, ADR-1977). The
	// gate runs exclusive tests serially, last in the leg, so no shared test ever
	// inherits what one leaves behind. Mutually exclusive with DefaultBaseTrain.
	Exclusive bool `json:"exclusive,omitempty"`
	// ExclusiveReason is one non-empty line, required iff Exclusive.
	ExclusiveReason string `json:"exclusive_reason,omitempty"`
	// DefaultBaseTrain marks a test that needs the default-base merge-train
	// partition (the repository's default branch) and asserts on it, so it must
	// not overlap any other test that can enqueue there. The gate runs these
	// serially, after the shared phase and before the exclusive one (#1977).
	DefaultBaseTrain bool `json:"default_base_train,omitempty"`
	// DefaultBaseTrainReason is one non-empty line, required iff DefaultBaseTrain.
	DefaultBaseTrainReason string `json:"default_base_train_reason,omitempty"`
}

// Isolation is the class of a live test for the gate's two-phase leg (#1977).
type Isolation string

const (
	// IsolationShared tests run concurrently with each other at the shared -parallel.
	IsolationShared Isolation = "shared"
	// IsolationDefaultBaseTrain tests run serially after the shared phase.
	IsolationDefaultBaseTrain Isolation = "default-base-train"
	// IsolationExclusive tests run serially, last.
	IsolationExclusive Isolation = "exclusive"
)

// Isolation derives the entry's class from its two markers.
func (e Entry) Isolation() Isolation {
	switch {
	case e.Exclusive:
		return IsolationExclusive
	case e.DefaultBaseTrain:
		return IsolationDefaultBaseTrain
	}
	return IsolationShared
}

// IsolationOf returns the class of every registered test, keyed by name.
func (r *Registry) IsolationOf() map[string]Isolation {
	out := make(map[string]Isolation, len(r.Tests))
	for _, e := range r.Tests {
		out[e.Name] = e.Isolation()
	}
	return out
}

// MatchLeg reports whether the leg label ("auth/train", e.g. "app/off") matches
// pattern, a "<auth>/<train>" pair in which either part may be "*".
func MatchLeg(pattern, label string) bool {
	pa, pt, ok := strings.Cut(pattern, "/")
	la, lt, ok2 := strings.Cut(label, "/")
	if !ok || !ok2 {
		return false
	}
	return (pa == "*" || pa == la) && (pt == "*" || pt == lt)
}

// SkipOK reports whether a skip of this test on the leg is structurally expected.
func (e Entry) SkipOK(label string) bool {
	for _, p := range e.SkipOKLegs {
		if MatchLeg(p, label) {
			return true
		}
	}
	return false
}

// Registry is the top-level registry.json document. It is never a bare map.
type Registry struct {
	Version int     `json:"version"`
	Tests   []Entry `json:"tests"`
}

//go:embed registry.json
var embedded []byte

// Load decodes the embedded registry.json strictly: an unknown field fails.
func Load() (*Registry, error) {
	return Decode(embedded)
}

// Decode strictly decodes registry JSON (unknown fields are an error).
func Decode(data []byte) (*Registry, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var r Registry
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("decoding registry: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("decoding registry: trailing data after document")
	}
	return &r, nil
}

// Counts is the parity summary printed by the gate runner (tests/gate/parity.go).
type Counts struct {
	Covered  int
	LiveOnly int
	Gap      int
}

// Summary tallies entries by parity.
func (r *Registry) Summary() Counts {
	var c Counts
	for _, e := range r.Tests {
		switch e.Parity {
		case ParitySim:
			c.Covered++
		case ParityLiveOnly:
			c.LiveOnly++
		case ParityGap:
			c.Gap++
		}
	}
	return c
}
