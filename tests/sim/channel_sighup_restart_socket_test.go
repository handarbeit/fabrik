package sim

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/internal/channelevents"
	"github.com/handarbeit/fabrik/internal/localapi"
)

// Label announcements around a SIGHUP restart, with the subscriber attached
// through the real local-API socket (#2484, ADR 2484). The in-process sink of
// channel_sighup_restart_test.go never exercises what a live subscriber sees: the
// restart tail closing its connection, localapi.RunStream reconnecting and
// re-attaching to the next process, and the hub's held queue serving the new
// session. Labels are changed before the signal, while the daemon drains (an
// intake delivery still queued when the tail runs) and after the restart; each
// must reach the subscriber exactly once, whatever its origin.

// socketCollector is a RunStream subscriber on a real socket.
type socketCollector struct {
	mu        sync.Mutex
	got       []channelevents.Event
	connected int
}

func startSocketCollector(t *testing.T, path string) *socketCollector {
	t.Helper()
	c := &socketCollector{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = localapi.RunStream(ctx, localapi.StreamOptions{
			Path:       path,
			Subscriber: "S",
			OnEvent: func(ev channelevents.Event) {
				c.mu.Lock()
				c.got = append(c.got, ev)
				c.mu.Unlock()
			},
			OnState: func(st localapi.StreamState) {
				if st.Connected {
					c.mu.Lock()
					c.connected++
					c.mu.Unlock()
				}
			},
			MinBackoff: 25 * time.Millisecond,
			MaxBackoff: 100 * time.Millisecond,
		})
	}()
	t.Cleanup(func() { cancel(); <-done })
	return c
}

func (c *socketCollector) attaches() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connected
}

func (c *socketCollector) count(typ channelevents.EventType, issue int, label string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, ev := range c.got {
		if ev.Type == typ && ev.Issue == issue && ev.Label == label {
			n++
		}
	}
	return n
}

func (c *socketCollector) snapshot() []channelevents.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]channelevents.Event(nil), c.got...)
}

// awaitAttaches waits (real time, a socket has no drain point) for n attaches.
func (c *socketCollector) awaitAttaches(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for c.attaches() < n {
		if time.Now().After(deadline) {
			t.Fatalf("subscriber attached %d time(s), want %d", c.attaches(), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// shortSocket returns a socket path short enough for sun_path on every platform.
func shortSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "fsr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "f.sock")
}

type socketVariant struct {
	name    string
	lateAPI bool // the subscriber re-attaches only after the daemon reports ready
}

func TestChannelSighupRestartRealSocketSubscriberGetsEveryLabelOnce(t *testing.T) {
	// Serial on purpose: see parallelExempt.
	for _, v := range []socketVariant{{"attaches-before-ready", false}, {"attaches-after-ready", true}} {
		v := v
		t.Run(v.name, func(t *testing.T) {
			sock := shortSocket(t)
			o := sighupOpts{intake: true, detached: true, socket: sock, lateAPI: v.lateAPI}
			w := newSighupWorld(t, o)
			w.env.Engine.StartLocalAPIForTest(sock)
			t.Cleanup(w.env.Engine.CloseLocalAPIForTest)
			col := startSocketCollector(t, sock)
			col.awaitAttaches(t, 1)

			submit := func(env *Env, id, label string) {
				t.Helper()
				w.addToWorld(t, label)
				if !env.Engine.SubmitDeliveryForTest("webhook", id, "issues", labelDelivery(env, w.num, "labeled", label)) {
					t.Fatalf("delivery %s was not queued", id)
				}
			}
			// Before the signal: accepted and applied.
			submit(w.env, "D-1", "sock-1")
			submit(w.env, "D-2", "sock-2")
			w.env.Engine.DrainDeliveriesForTest()
			// During the drain: accepted, still queued when the tail runs.
			w.env.Engine.SetIntakeLayer1DelayForTest(150 * time.Millisecond)
			submit(w.env, "D-3", "sock-3")
			submit(w.env, "D-4", "sock-4")

			w.restart(t, o)
			// After the restart, as replay or reconcile delivers them.
			for i, label := range []string{"sock-5", "sock-6"} {
				w.addToWorld(t, label)
				w.r.Engine.ApplyLabelWebhookForTest(w.r.OwnerRepo, w.num, label, true, "alice")
				_ = i
			}
			w.removeFromWorld(t, "sock-1")
			w.r.Engine.ApplyLabelWebhookForTest(w.r.OwnerRepo, w.num, "sock-1", false, "alice")
			w.settleSecond(t)
			if v.lateAPI {
				w.r.Engine.StartLocalAPIForTest(sock)
				t.Cleanup(w.r.Engine.CloseLocalAPIForTest)
			}
			col.awaitAttaches(t, 2)

			labels := []string{"sock-1", "sock-2", "sock-3", "sock-4", "sock-5", "sock-6"}
			deadline := time.Now().Add(15 * time.Second)
			for {
				missing := false
				for _, l := range labels {
					if col.count(channelevents.LabelApplied, w.num, l) == 0 {
						missing = true
					}
				}
				if col.count(channelevents.LabelRemoved, w.num, "sock-1") == 0 {
					missing = true
				}
				if !missing || time.Now().After(deadline) {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			// A duplicate would follow the first copy at once; give it a moment.
			waitRealTime(t, 300*time.Millisecond, "a duplicate announcement has no drain point to wait on")
			for _, l := range labels {
				if got := col.count(channelevents.LabelApplied, w.num, l); got != 1 {
					t.Errorf("label %q announced %d time(s) to the socket subscriber, want exactly 1", l, got)
				}
			}
			if got := col.count(channelevents.LabelRemoved, w.num, "sock-1"); got != 1 {
				t.Errorf("removal of sock-1 announced %d time(s), want exactly 1", got)
			}
			if t.Failed() {
				t.Logf("received:%s", describe(col.snapshot()))
			}
		})
	}
}
