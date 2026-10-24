package gate

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"time"
)

// EventsSummary is the events dimension of one leg's roll-up (#2426, FR-006),
// archived in phases.json beside the phase measurements and printed after the leg.
// Every leg records its events dimension, its planned cadence and whether the
// operator overrode it; a webhook leg also records what the bed's intake saw.
type EventsSummary struct {
	Events      string `json:"events"`
	PollSeconds string `json:"poll_seconds"`
	// PollOverride is the operator's E2E_BED_POLL_SECONDS when it differs from the
	// planned cadence; such a leg is not recorded in the ledger (FR-010).
	PollOverride string `json:"poll_override,omitempty"`
	// Neutralise is the operator's E2E_NEUTRALISE fault list (#2427); such a leg is
	// not recorded in the ledger.
	Neutralise string `json:"neutralise,omitempty"`
	// Deliveries is the number of GitHub deliveries the App's delivery log shows for
	// the leg's window (webhook legs only; -1 when the log could not be read). The
	// log read is the newest page, so a very busy leg can be under-reported.
	Deliveries int `json:"deliveries"`
	// Deduped and Dropped count the intake's "duplicate delivery" and "intake queue
	// full ... dropping" engine-log lines written during the leg (webhook legs
	// only). A rotated log under-reports.
	Deduped int `json:"deduped"`
	Dropped int `json:"dropped"`
	// CountsSource names where the counts came from, so a reader can judge them.
	CountsSource string `json:"counts_source,omitempty"`
}

// The engine log lines the intake writes (engine/intake.go).
const (
	intakeDuplicateLine = "duplicate delivery "
	intakeDropLine      = "intake queue full for "
)

// logSize is the engine log's size now (0 when absent), the offset a leg counts from.
func logSize(path string) int64 {
	if fi, err := os.Stat(path); err == nil {
		return fi.Size()
	}
	return 0
}

// countIntakeLines counts the intake's dedupe and drop lines in the engine log
// from byte offset from. A log shorter than from was replaced, so it is read whole.
func countIntakeLines(path string, from int64) (deduped, dropped int) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil && fi.Size() < from {
		from = 0
	}
	if _, err := f.Seek(from, 0); err != nil {
		return 0, 0
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.Contains(line, intakeDuplicateLine):
			deduped++
		case strings.Contains(line, intakeDropLine):
			dropped++
		}
	}
	return deduped, dropped
}

// eventsSummary builds the leg's events record. start and logFrom were taken
// before the leg's bed restart.
func (g *Gate) eventsSummary(ctx context.Context, cell Cell, start time.Time, logFrom int64) *EventsSummary {
	s := &EventsSummary{Events: cell.EventsKind(), PollSeconds: cell.pollSeconds(), Deliveries: -1}
	if g.pollOverridden(cell) {
		s.PollOverride = g.Cfg.BedPollOverride
	}
	s.Neutralise = g.Cfg.Neutralise
	if !cell.Webhook() {
		s.Deliveries = 0
		return s
	}
	s.Deduped, s.Dropped = countIntakeLines(g.Cfg.EngineLog, logFrom)
	s.CountsSource = "deliveries: App delivery log (newest page) in the leg window; deduped/dropped: engine log lines"
	if g.Delivery == nil {
		return s
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ds, err := g.Delivery.AppDeliveries(cctx, g.Cfg.TestBed)
	if err != nil {
		s.CountsSource += fmt.Sprintf(" (delivery log unreadable: %v)", err)
		return s
	}
	end := g.Now()
	n := 0
	for _, d := range ds {
		if !d.DeliveredAt.Before(start) && !d.DeliveredAt.After(end) {
			n++
		}
	}
	s.Deliveries = n
	return s
}

// Format is the events lines of the leg summary block.
func (s *EventsSummary) Format() string {
	if s == nil {
		return ""
	}
	line := fmt.Sprintf("   events              %s  poll cadence %ss", s.Events, s.PollSeconds)
	if s.PollOverride != "" {
		line += fmt.Sprintf("  (OPERATOR OVERRIDE %ss — not recorded in the ledger)", s.PollOverride)
	}
	if s.Neutralise != "" {
		line += fmt.Sprintf("  (NEUTRALISED %s — not recorded in the ledger)", s.Neutralise)
	}
	line += "\n"
	if s.Events == EventsWebhook {
		d := "unknown"
		if s.Deliveries >= 0 {
			d = fmt.Sprint(s.Deliveries)
		}
		line += fmt.Sprintf("   webhook intake      deliveries %s  deduped %d  dropped %d\n", d, s.Deduped, s.Dropped)
	}
	return line
}
