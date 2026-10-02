package gate

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Per-leg measurements (#1977, R5.1): what the two-phase leg cost, so the
// -parallel defaults can be chosen from before/after runs rather than guessed.
// Everything here is a report — nothing in it gates a leg.

// PhaseStat is one phase's measurements.
type PhaseStat struct {
	Name           string        `json:"name"` // "" is the undivided phase, reported as "suite"
	Tests          int           `json:"tests"`
	Parallel       string        `json:"parallel"`
	Wall           time.Duration `json:"wall_ns"`
	PeakConcurrent int           `json:"peak_concurrent"`
	// BudgetBefore/After are the bed token's remaining GraphQL budget around the
	// phase, or -1 when a probe failed.
	BudgetBefore int `json:"budget_before"`
	BudgetAfter  int `json:"budget_after"`
	ExitCode     int `json:"exit_code"`
}

func (p PhaseStat) label() string {
	if p.Name == "" {
		return "suite"
	}
	return p.Name
}

// consumed is the GraphQL points the phase spent, or -1 when it cannot be told
// (a failed probe, or a budget reset mid-phase).
func (p PhaseStat) consumed() int {
	if p.BudgetBefore < 0 || p.BudgetAfter < 0 || p.BudgetAfter > p.BudgetBefore {
		return -1
	}
	return p.BudgetBefore - p.BudgetAfter
}

// LegSummary is one leg's roll-up.
type LegSummary struct {
	Label  string      `json:"label"`
	Phases []PhaseStat `json:"phases"`
	// Retry is the #1973 INCONCLUSIVE retries' own wall-clock and GraphQL spend
	// (nil when none ran), kept apart so the last phase's figures — which drive the
	// -parallel defaults — do not include them.
	Retry *PhaseStat `json:"retry,omitempty"`
	// PeakLoad1m is the highest 1-minute load average the archive's sampler saw
	// during the leg; HasLoad is false when the host reports none (or there is no
	// archive, i.e. the ledger is disabled).
	PeakLoad1m  float64 `json:"peak_load_1m"`
	HasLoad     bool    `json:"has_load"`
	LoadSamples int     `json:"load_samples"`
	// BedMaxConcurrent is the bed engine's effective max_concurrent (0: unknown).
	BedMaxConcurrent int `json:"bed_max_concurrent,omitempty"`
}

// Wall is the leg's suite wall-clock: the sum of its phases and any retries.
func (s LegSummary) Wall() time.Duration {
	var d time.Duration
	for _, p := range s.stats() {
		d += p.Wall
	}
	return d
}

// stats is the phases followed by the retry entry, if any.
func (s LegSummary) stats() []PhaseStat {
	if s.Retry == nil {
		return s.Phases
	}
	return append(append([]PhaseStat(nil), s.Phases...), *s.Retry)
}

// PeakConcurrent is the highest phase peak (phases run one after another).
func (s LegSummary) PeakConcurrent() int {
	peak := 0
	for _, p := range s.Phases {
		if p.PeakConcurrent > peak {
			peak = p.PeakConcurrent
		}
	}
	return peak
}

// Consumed is the leg's total GraphQL spend over the phases whose spend is known,
// and whether every phase's was.
func (s LegSummary) Consumed() (pts int, complete bool) {
	complete = true
	for _, p := range s.stats() {
		if c := p.consumed(); c >= 0 {
			pts += c
		} else {
			complete = false
		}
	}
	return pts, complete
}

// Format renders the summary block printed after the leg's suite.
func (s LegSummary) Format() string {
	var b strings.Builder
	fmt.Fprintf(&b, "== leg summary (leg: %s) ==\n", s.Label)
	for _, p := range s.stats() {
		spent := "n/a"
		if c := p.consumed(); c >= 0 {
			spent = fmt.Sprintf("%d pts", c)
		}
		fmt.Fprintf(&b, "   %-19s %2d test(s)  -parallel=%-2s  wall %s  peak concurrent %d  GraphQL %s\n",
			p.label(), p.Tests, p.Parallel, p.Wall.Round(time.Second), p.PeakConcurrent, spent)
	}
	pts, complete := s.Consumed()
	spent := fmt.Sprintf("%d pts", pts)
	if !complete {
		spent += " (a phase's spend was not computable)"
	}
	load := "n/a"
	if s.HasLoad {
		load = fmt.Sprintf("%.2f (1m, %d samples)", s.PeakLoad1m, s.LoadSamples)
	}
	fmt.Fprintf(&b, "   total               wall %s  peak concurrent %d  GraphQL %s  peak host load %s\n",
		s.Wall().Round(time.Second), s.PeakConcurrent(), spent, load)
	if s.BedMaxConcurrent > 0 {
		fmt.Fprintf(&b, "   bed max_concurrent  %d\n", s.BedMaxConcurrent)
	}
	return b.String()
}

// writePhasesJSON archives the leg summary as phases.json in the leg's directory.
// It is best-effort: a leg without an archive (the ledger disabled) prints the
// summary and keeps no file.
func (g *Gate) writePhasesJSON(cell Cell, s *LegSummary) {
	if g.cov == nil {
		return
	}
	dir := g.cov.ledger.ArchiveCellDir(cellDirName(cell), g.cov.invocation)
	if _, err := os.Stat(dir); err != nil {
		return
	}
	if err := os.WriteFile(filepath.Join(dir, "phases.json"), mustJSON(s), 0o644); err != nil {
		g.errf("warning: archiving the leg summary: %v\n", err)
	}
}
