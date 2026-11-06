package pruefer

import (
	"sync"
	"time"

	"github.com/handarbeit/fabrik/internal/appwebhook"
	"github.com/handarbeit/fabrik/internal/events"
	ptui "github.com/handarbeit/fabrik/pruefer/tui"
	"github.com/handarbeit/fabrik/warnings"
)

// Pruefer's implementations of the consumer interfaces of internal/appwebhook
// (#2476, ADR 2476), the counterpart of engine/appwebhook_adapters.go. The
// dedupe, the sink, the drop accounting and the TUI stay Pruefer's; the shared
// receiver reaches them only through these.

// ledgerView is the read-only delivery ledger the replayer and route liveness
// consult. It deliberately exposes only the read methods of events.Dedupe: no
// SeenBefore, which records on lookup. Checking a delivery must never mark it
// seen, or a redelivery the replayer asked for (or a route-check copy) would be
// dropped as a duplicate and replay would silently do nothing (FR-006).
type ledgerView struct{ d *events.Dedupe }

func (l ledgerView) Seen(id string) bool { return l.d.Seen(id) }
func (l ledgerView) Newest() time.Time   { return l.d.Newest() }
func (l ledgerView) ReceivedAt(id string, now time.Time) (time.Time, bool) {
	return l.d.ReceivedAt(id, now)
}

var _ appwebhook.ReplayLedger = ledgerView{}
var _ appwebhook.LivenessLedger = ledgerView{}

// logPanel implements appwebhook.Panel for a daemon that has no warnings panel
// and no local API (Pruefer writes neither .fabrik/warnings.json nor a socket):
// an operator condition is a log line plus a footer banner. It reports on a
// transition only, so a condition re-checked every few minutes (the webhook
// configuration check) logs once, not on every pass.
type logPanel struct {
	d   *Daemon
	tag string

	mu     sync.Mutex
	active map[string]activeCondition // by Key()
}

type activeCondition struct {
	typ         string
	fingerprint string
}

func newLogPanel(d *Daemon, tag string) *logPanel {
	return &logPanel{d: d, tag: tag, active: make(map[string]activeCondition)}
}

// Report raises c. A notice-level condition is logged only (no banner): it is
// information, not something that needs action.
func (p *logPanel) Report(c warnings.Condition) {
	key := c.Key()
	fp := c.Fingerprint
	if fp == "" {
		fp = c.Detail
	}
	p.mu.Lock()
	prev, had := p.active[key]
	changed := !had || prev.fingerprint != fp
	if c.Level != warnings.LevelNotice {
		p.active[key] = activeCondition{typ: c.Type, fingerprint: fp}
	}
	p.mu.Unlock()
	if !changed {
		return
	}
	line := c.LogLine
	if line == "" {
		line = c.Title
	}
	if line == "" {
		line = c.Detail
	}
	logf(0, p.tag, "warning: %s\n", line)
	if c.Detail != "" && c.Detail != line {
		logf(0, p.tag, "%s\n", c.Detail)
	}
	if c.Level == warnings.LevelNotice {
		return
	}
	p.d.emit(ptui.TransportWarningEvent{Key: key, Title: c.Title, Active: true, At: time.Now()})
}

// Resolve clears the condition with key. Clearing one that was never raised is a
// no-op (the shared package resolves defensively, e.g. on a fresh start).
func (p *logPanel) Resolve(key string) {
	p.mu.Lock()
	_, had := p.active[key]
	delete(p.active, key)
	p.mu.Unlock()
	if !had {
		return
	}
	logf(0, p.tag, "resolved: %s\n", key)
	p.d.emit(ptui.TransportWarningEvent{Key: key, Active: false, At: time.Now()})
}

// Reconcile sweeps one condition type to exactly conds: raises each, clears any
// active condition of the type that is not among them.
func (p *logPanel) Reconcile(typ string, conds []warnings.Condition) {
	want := make(map[string]bool, len(conds))
	for _, c := range conds {
		want[c.Key()] = true
	}
	p.mu.Lock()
	var stale []string
	for k, a := range p.active {
		if a.typ == typ && !want[k] {
			stale = append(stale, k)
		}
	}
	p.mu.Unlock()
	for _, k := range stale {
		p.Resolve(k)
	}
	for _, c := range conds {
		p.Report(c)
	}
}

// activeKeys returns the currently raised condition keys. Tests only.
func (p *logPanel) activeKeys() map[string]bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]bool, len(p.active))
	for k := range p.active {
		out[k] = true
	}
	return out
}

var _ appwebhook.Panel = (*logPanel)(nil)
