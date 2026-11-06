package engine

import (
	"time"

	"github.com/handarbeit/fabrik/internal/appwebhook"
	"github.com/handarbeit/fabrik/warnings"
)

// The engine's implementations of the consumer interfaces of
// internal/appwebhook (#2475, ADR 2475). The intake, delivery ledger, echo
// registry, dedupe and ordering stay in the engine; the shared receiver reaches
// them only through these.

// ledgerView is the read-only ledger the replayer and route liveness consult.
// It exposes no Accept: a redelivery the replayer requested must still be
// applied once, and only the intake records.
type ledgerView struct{ in *intake }

func (l ledgerView) Seen(id string) bool { return l.in.ledgerSeen(id) }
func (l ledgerView) Newest() time.Time   { return l.in.ledgerNewest() }
func (l ledgerView) ReceivedAt(id string, now time.Time) (time.Time, bool) {
	return l.in.ledgerReceivedAt(id, now)
}

var _ appwebhook.ReplayLedger = ledgerView{}
var _ appwebhook.LivenessLedger = ledgerView{}

// enginePanel reports operator warnings for one transport tag.
type enginePanel struct {
	e   *Engine
	tag string
}

func (p enginePanel) Report(c warnings.Condition) { p.e.reportWarning(p.tag, c) }
func (p enginePanel) Resolve(key string)          { p.e.resolveWarning(p.tag, key) }
func (p enginePanel) Reconcile(typ string, conds []warnings.Condition) {
	if err := warnings.Reconcile(p.e.warnWriter(p.tag), typ, conds); err != nil {
		p.e.logf(0, p.tag, "recording %s warning: %v\n", typ, err)
	}
}

var _ appwebhook.Panel = enginePanel{}
