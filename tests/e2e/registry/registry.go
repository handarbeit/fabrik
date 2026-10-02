// Package registry is the single per-test registry for the live e2e suite
// (tests/e2e). It is keyed by live-test function name and currently carries
// the sim-parity field; later issues add fields (pack, exclusive, ...) to Entry
// rather than creating their own lists.
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
