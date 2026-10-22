package registry

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
)

// Wallclock classification (#2417, ADR-2417): where each slow live test's
// wall-clock goes. It is a separate file from registry.json because registry.json
// decodes strictly and drives the gate's required (test, leg) set; this table is
// reporting data only — `gate report` joins it by test name — and changing it can
// never change what the gate requires.

// WallclockCategory is where a test's time mostly goes.
type WallclockCategory string

const (
	CategoryRealWork       WallclockCategory = "real-work"
	CategoryEngineTimeout  WallclockCategory = "engine-timeout"
	CategoryNegativeWindow WallclockCategory = "negative-window"
	CategoryHarnessSlack   WallclockCategory = "harness-slack"
)

// WallclockEntry classifies one test.
type WallclockEntry struct {
	Name     string            `json:"name"`
	Dominant WallclockCategory `json:"dominant"`
	Note     string            `json:"note"`
}

// Wallclock is the decoded wallclock.json.
type Wallclock struct {
	Version    int               `json:"version"`
	Categories map[string]string `json:"categories"`
	Tests      []WallclockEntry  `json:"tests"`
}

//go:embed wallclock.json
var embeddedWallclock []byte

// LoadWallclock decodes the embedded wallclock.json strictly.
func LoadWallclock() (*Wallclock, error) { return DecodeWallclock(embeddedWallclock) }

// DecodeWallclock decodes and validates a wallclock table: known categories only,
// no duplicate names, every entry carrying a note.
func DecodeWallclock(data []byte) (*Wallclock, error) {
	var w Wallclock
	if err := decodeStrict(data, &w); err != nil {
		return nil, fmt.Errorf("decoding wallclock.json: %w", err)
	}
	seen := map[string]bool{}
	for _, e := range w.Tests {
		if _, ok := w.Categories[string(e.Dominant)]; !ok {
			return nil, fmt.Errorf("wallclock.json: %s has unknown category %q", e.Name, e.Dominant)
		}
		if seen[e.Name] {
			return nil, fmt.Errorf("wallclock.json: duplicate entry %s", e.Name)
		}
		seen[e.Name] = true
		if e.Note == "" {
			return nil, fmt.Errorf("wallclock.json: %s has no note", e.Name)
		}
	}
	sort.SliceStable(w.Tests, func(i, j int) bool { return w.Tests[i].Name < w.Tests[j].Name })
	return &w, nil
}

// Category returns the dominant category for a test, or "" when it is unclassified.
func (w *Wallclock) Category(test string) WallclockCategory {
	if w == nil {
		return ""
	}
	for _, e := range w.Tests {
		if e.Name == test {
			return e.Dominant
		}
	}
	return ""
}

// CheckAgainst reports every wallclock entry that is not a registry test.
func (w *Wallclock) CheckAgainst(reg *Registry) []string {
	known := map[string]bool{}
	for _, e := range reg.Tests {
		known[e.Name] = true
	}
	var bad []string
	for _, e := range w.Tests {
		if !known[e.Name] {
			bad = append(bad, e.Name)
		}
	}
	return bad
}

func decodeStrict(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}
