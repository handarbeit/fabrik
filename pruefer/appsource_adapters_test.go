package pruefer

import (
	"sync"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/internal/events"
	ptui "github.com/handarbeit/fabrik/pruefer/tui"
	"github.com/handarbeit/fabrik/warnings"
)

// The replayer's "seen" check must not record: if it did, the redelivery it
// asks GitHub for would arrive and be dropped as a duplicate.
func TestLedgerViewSeenNeverRecords(t *testing.T) {
	d := events.NewDedupe(0)
	v := ledgerView{d}
	if v.Seen("x") {
		t.Fatal("Seen on empty ledger")
	}
	if v.Seen("x") || d.Seen("x") {
		t.Fatal("Seen recorded the id")
	}
	if !v.Newest().IsZero() {
		t.Fatal("Newest on empty ledger")
	}
	if _, ok := v.ReceivedAt("x", time.Now()); ok {
		t.Fatal("ReceivedAt of an unrecorded id")
	}
	if d.SeenBefore("x") {
		t.Fatal("SeenBefore reported a duplicate: a read-only lookup recorded the id")
	}
	if !v.Seen("x") {
		t.Fatal("Seen after the real delivery was recorded")
	}
	if _, ok := v.ReceivedAt("x", time.Now()); !ok {
		t.Fatal("ReceivedAt after the real delivery was recorded")
	}
}

type recordedEvents struct {
	mu  sync.Mutex
	evs []ptui.TransportWarningEvent
}

func (r *recordedEvents) emit(ev ptui.Event) {
	if w, ok := ev.(ptui.TransportWarningEvent); ok {
		r.mu.Lock()
		r.evs = append(r.evs, w)
		r.mu.Unlock()
	}
}

func (r *recordedEvents) snapshot() []ptui.TransportWarningEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ptui.TransportWarningEvent(nil), r.evs...)
}

func TestLogPanelReportResolveAndDedup(t *testing.T) {
	rec := &recordedEvents{}
	d := &Daemon{Emit: rec.emit}
	p := newLogPanel(d, "tailscale")
	c := warnings.Condition{Type: "tailscale_listen", Subject: "h", Title: "could not start", Detail: "d", Fingerprint: "f"}

	p.Report(c)
	p.Report(c) // unchanged repeat: no second banner event
	if got := rec.snapshot(); len(got) != 1 || !got[0].Active || got[0].Key != c.Key() || got[0].Title != "could not start" {
		t.Fatalf("events after repeat = %+v", got)
	}
	p.Resolve(c.Key())
	p.Resolve(c.Key()) // second resolve is a no-op
	got := rec.snapshot()
	if len(got) != 2 || got[1].Active || got[1].Key != c.Key() {
		t.Fatalf("events after resolve = %+v", got)
	}
	p.Resolve("never:raised")
	if len(rec.snapshot()) != 2 {
		t.Error("resolving an unraised condition emitted an event")
	}
}

func TestLogPanelNoticeIsLoggedNotBannered(t *testing.T) {
	rec := &recordedEvents{}
	p := newLogPanel(&Daemon{Emit: rec.emit}, "direct")
	p.Report(warnings.Condition{Type: "t", Subject: "s", Level: warnings.LevelNotice, Title: "fyi"})
	if len(rec.snapshot()) != 0 || len(p.activeKeys()) != 0 {
		t.Errorf("notice produced a banner: %+v", rec.snapshot())
	}
}

func TestLogPanelReconcileSweepsOnlyItsType(t *testing.T) {
	rec := &recordedEvents{}
	p := newLogPanel(&Daemon{Emit: rec.emit}, "tailscale")
	a := warnings.Condition{Type: "cfg", Subject: "a", Title: "A"}
	b := warnings.Condition{Type: "cfg", Subject: "b", Title: "B"}
	other := warnings.Condition{Type: "other", Subject: "x", Title: "X"}
	p.Report(a)
	p.Report(b)
	p.Report(other)

	p.Reconcile("cfg", []warnings.Condition{b})
	keys := p.activeKeys()
	if keys[a.Key()] || !keys[b.Key()] || !keys[other.Key()] {
		t.Errorf("active after reconcile = %v", keys)
	}
	p.Reconcile("cfg", nil) // an empty set clears the type
	keys = p.activeKeys()
	if keys[b.Key()] || !keys[other.Key()] {
		t.Errorf("active after empty reconcile = %v", keys)
	}
}

// The webhook-configuration check builds conditions with no Type and relies on
// Reconcile to stamp it; the banner key must carry the type or it can never be
// cleared by the next sweep.
func TestLogPanelReconcileStampsTheType(t *testing.T) {
	rec := &recordedEvents{}
	p := newLogPanel(&Daemon{Emit: rec.emit}, "tailscale")
	p.Reconcile("tailscale_webhook_config", []warnings.Condition{{Subject: "url", Title: "wrong URL"}})
	got := rec.snapshot()
	if len(got) != 1 || got[0].Key != "tailscale_webhook_config:url" || !got[0].Active {
		t.Fatalf("events = %+v", got)
	}
	p.Reconcile("tailscale_webhook_config", nil)
	got = rec.snapshot()
	if len(got) != 2 || got[1].Active || got[1].Key != "tailscale_webhook_config:url" {
		t.Fatalf("clear events = %+v", got)
	}
}
