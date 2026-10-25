package sim

import (
	"testing"
	"time"

	"github.com/handarbeit/fabrik/internal/channelevents"
	"github.com/handarbeit/fabrik/internal/localapi"
)

// Label announcements around a SIGHUP restart (#2450, ADR 2450). Every label
// change the daemon applies is announced to a subscriber exactly once across the
// restart: live before the exec, or as a downtime delta after it. The scenarios
// drive the real restart tail (Engine.PerformSighupRestartForTest: transports,
// intake close, local API, channel events, then a stubbed exec) and rebuild the
// engine on the same state directory with RestartEnv.
//
// Two defects are shown, each with a neutralisation seam so the test fails
// without its fix (SC-002):
//   - lost: a change observed and recorded in the restart ledger but still queued
//     when the consumer stops is never published, and the new process finds no
//     difference to report (finalFlushOff);
//   - repeated: a webhook label event for an item whose labels are not known yet
//     is announced live and again as a downtime delta once the labels are known
//     (liveLabelDedupOff).

type sighupOpts struct {
	noFinalFlush bool
	noLiveDedup  bool
	intake       bool
	detached     bool // the subscriber is not attached to the first daemon
}

type sighupWorld struct {
	env  *Env
	dir  string
	num  int
	sink *simSink
	// restarted is the second daemon, set by restart().
	r     *Env
	sink2 *simSink
}

func newSighupWorld(t *testing.T, o sighupOpts) *sighupWorld {
	t.Helper()
	env := NewEnv(t, EnvOptions{Stages: lifecycleStages(), BoardCache: true})
	env.Engine.SetChannelTimingForTest(20*time.Millisecond, time.Hour)
	if o.noFinalFlush {
		env.Engine.SetFinalChannelFlushDisabledForTest()
	}
	if o.noLiveDedup {
		env.Engine.SetLiveLabelDeltaDedupDisabledForTest()
	}
	open, _ := restartWorld(t, env)
	w := &sighupWorld{env: env, dir: t.TempDir(), num: open[0], sink: &simSink{}}
	if o.intake {
		env.Engine.EnableDeliveryIntakeForTest(t.TempDir())
	}
	hub := env.Engine.StartChannelEventsForTest(w.dir)
	if hub == nil {
		t.Fatal("first hub did not start")
	}
	if _, err := hub.Subscribe(channelevents.Subscription{Subscriber: "S"}); err != nil {
		t.Fatal(err)
	}
	if !o.detached {
		hub.Attach("S", w.sink)
	}
	RunPolls(t, env, 4)
	reconcile(t, env)
	RunPolls(t, env, 2)
	settleWait(env)
	env.Engine.SetSighupExecForTest(func(string, []string, []string) error { return nil })
	return w
}

// current is the daemon that is running now: the restarted one once restart has run.
func (w *sighupWorld) current() *Env {
	if w.r != nil {
		return w.r
	}
	return w.env
}

// addToWorld adds label to the issue on the simulated GitHub, so the restarted
// daemon's listing agrees with the webhook that announces it.
func (w *sighupWorld) addToWorld(t *testing.T, label string) {
	t.Helper()
	if err := w.current().Sim.AddLabelToIssue(w.env.Owner, w.env.Repo, w.num, label); err != nil {
		t.Fatal(err)
	}
}

func (w *sighupWorld) removeFromWorld(t *testing.T, label string) {
	t.Helper()
	if err := w.current().Sim.RemoveLabelFromIssue(w.env.Owner, w.env.Repo, w.num, label); err != nil {
		t.Fatal(err)
	}
}

// restart performs the SIGHUP restart tail on the first daemon and starts the
// second on the same state directory, with the subscriber re-attached.
func (w *sighupWorld) restart(t *testing.T, o sighupOpts) {
	t.Helper()
	w.env.Engine.PerformSighupRestartForTest()
	r := RestartEnv(t, w.env)
	r.BoardCache, r.Wake = r.Engine.UseBoardCacheForTest()
	if o.intake {
		r.Engine.EnableDeliveryIntakeForTest(t.TempDir())
	}
	hub := r.Engine.StartChannelEventsWarmingForTest(w.dir)
	if hub == nil {
		t.Fatal("second hub did not start")
	}
	t.Cleanup(r.Engine.StopChannelEventsForTest)
	w.sink2 = &simSink{}
	detach, err := r.Engine.ChannelStreamerForTest().Attach(localapi.AttachParams{Subscriber: "S"}, w.sink2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(detach)
	w.r = r
}

// settleSecond polls the restarted daemon through its first listings.
func (w *sighupWorld) settleSecond(t *testing.T) {
	t.Helper()
	RunPolls(t, w.r, 4)
	reconcile(t, w.r)
	RunPolls(t, w.r, 2)
	settleWait(w.r)
	settleWait(w.r)
}

// count is the number of announcements of one label change across both daemons.
func (w *sighupWorld) count(typ channelevents.EventType, label string) int {
	n := 0
	for _, s := range []*simSink{w.sink, w.sink2} {
		if s == nil {
			continue
		}
		for _, ev := range s.ofType(typ, w.num) {
			if ev.Label == label {
				n++
			}
		}
	}
	return n
}

func (w *sighupWorld) wantOnce(t *testing.T, label string) {
	t.Helper()
	if got := w.count(channelevents.LabelApplied, label); got != 1 {
		t.Errorf("label %q announced %d time(s) across the restart, want exactly 1\nbefore the exec:%s\nafter it:%s",
			label, got, describe(w.sink.all()), describeSink(w.sink2))
	}
}

func describeSink(s *simSink) string {
	if s == nil {
		return " (none)"
	}
	return describe(s.all())
}

// A label applied after the consumer's last drain and still queued when the
// restart begins is announced once, not lost.
func TestChannelSighupRestartAnnouncesQueuedLabelOnce(t *testing.T) {
	t.Parallel()
	for _, neutralised := range []bool{true, false} {
		neutralised := neutralised
		name := "fixed"
		if neutralised {
			name = "without-final-flush"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			o := sighupOpts{noFinalFlush: neutralised}
			w := newSighupWorld(t, o)
			const label = "queued-before-exec"
			w.env.Engine.SetChannelConsumerParkedForTest(true) // queued after the last drain
			w.addToWorld(t, label)
			w.env.Engine.ApplyLabelWebhookForTest(w.env.OwnerRepo, w.num, label, true, "alice")
			w.restart(t, o)
			w.settleSecond(t)
			if neutralised {
				if got := w.count(channelevents.LabelApplied, label); got != 0 {
					t.Fatalf("without the final flush the label was announced %d time(s); the twin expects it lost (0)", got)
				}
				return
			}
			w.wantOnce(t, label)
		})
	}
}

// The same, for a change applied by the intake while it drains at shutdown.
func TestChannelSighupRestartAnnouncesIntakeDrainedLabelOnce(t *testing.T) {
	t.Parallel()
	for _, neutralised := range []bool{true, false} {
		neutralised := neutralised
		name := "fixed"
		if neutralised {
			name = "without-final-flush"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			o := sighupOpts{noFinalFlush: neutralised, intake: true}
			w := newSighupWorld(t, o)
			const label = "applied-by-intake-close"
			w.env.Engine.SetChannelConsumerParkedForTest(true)
			w.addToWorld(t, label)
			// Not drained here: intake.Close in the restart tail applies it.
			if !w.env.Engine.SubmitDeliveryForTest("webhook", "D-close", "issues", labelDelivery(w.env, w.num, "labeled", label)) {
				t.Fatal("the delivery was not queued")
			}
			w.restart(t, o)
			w.settleSecond(t)
			if neutralised {
				if got := w.count(channelevents.LabelApplied, label); got != 0 {
					t.Fatalf("without the final flush the label was announced %d time(s); the twin expects it lost (0)", got)
				}
				return
			}
			w.wantOnce(t, label)
		})
	}
}

// A delivery for a change made while the daemon was down arrives before the
// restarted daemon knows the item's labels. It is announced live; the downtime
// diff that runs once the labels are known must not announce it again.
func TestChannelSighupRestartDeliveryBeforeLabelsKnownIsAnnouncedOnce(t *testing.T) {
	t.Parallel()
	for _, neutralised := range []bool{true, false} {
		neutralised := neutralised
		name := "fixed"
		if neutralised {
			name = "without-live-dedup"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			o := sighupOpts{noLiveDedup: neutralised}
			w := newSighupWorld(t, o)
			const label = "changed-while-down"
			w.restart(t, o)
			w.addToWorld(t, label) // visible to the restarted daemon's listings
			// The replayed delivery reaches the store before any listing: the item's
			// labels are unknown.
			w.r.Engine.ApplyLabelWebhookForTest(w.r.OwnerRepo, w.num, label, true, "alice")
			w.settleSecond(t)
			got := w.count(channelevents.LabelApplied, label)
			if neutralised {
				if got != 2 {
					t.Fatalf("without the live-delta rule the label was announced %d time(s); the twin expects it repeated (2)%s", got, describeSink(w.sink2))
				}
				return
			}
			w.wantOnce(t, label)
		})
	}
}

// A delivery replayed after the restart for a change the first daemon already
// announced live adds no announcement.
func TestChannelSighupRestartReplayOfAnnouncedChangeAddsNothing(t *testing.T) {
	t.Parallel()
	o := sighupOpts{}
	w := newSighupWorld(t, o)
	const label = "announced-before-exec"
	w.addToWorld(t, label)
	w.env.Engine.ApplyLabelWebhookForTest(w.env.OwnerRepo, w.num, label, true, "alice")
	settleWait(w.env)
	w.restart(t, o)
	w.r.Engine.ApplyLabelWebhookForTest(w.r.OwnerRepo, w.num, label, true, "alice") // redelivered
	w.settleSecond(t)
	w.wantOnce(t, label)
}

// An add, a remove and a re-add are three changes, each announced once, whether
// they happen before or after the restart.
func TestChannelSighupRestartAddRemoveReaddCountsPerChange(t *testing.T) {
	t.Parallel()
	o := sighupOpts{}
	w := newSighupWorld(t, o)
	const label = "flapping"
	w.addToWorld(t, label)
	w.env.Engine.ApplyLabelWebhookForTest(w.env.OwnerRepo, w.num, label, true, "alice")
	settleWait(w.env)
	w.removeFromWorld(t, label)
	w.env.Engine.ApplyLabelWebhookForTest(w.env.OwnerRepo, w.num, label, false, "alice")
	settleWait(w.env)
	w.restart(t, o)
	w.addToWorld(t, label)
	w.r.Engine.ApplyLabelWebhookForTest(w.r.OwnerRepo, w.num, label, true, "alice")
	w.settleSecond(t)
	if got := w.count(channelevents.LabelApplied, label); got != 2 {
		t.Errorf("applied announced %d time(s), want 2 (one per add)%s", got, describe(append(w.sink.all(), w.sink2.all()...)))
	}
	if got := w.count(channelevents.LabelRemoved, label); got != 1 {
		t.Errorf("removed announced %d time(s), want 1", got)
	}
}

// A poll-only daemon (no intake, labels reaching the store by listing) keeps the
// guarantee: a label added on GitHub before the restart is announced once.
func TestChannelSighupRestartPollOnlyAnnouncesOnce(t *testing.T) {
	t.Parallel()
	o := sighupOpts{}
	w := newSighupWorld(t, o)
	const label = "poll-only"
	w.addToWorld(t, label)
	RunPolls(t, w.env, 2)
	reconcile(t, w.env)
	settleWait(w.env)
	w.restart(t, o)
	w.settleSecond(t)
	w.wantOnce(t, label)
}

// A subscriber that is detached at the restart receives each held announcement
// once when it re-attaches.
func TestChannelSighupRestartDetachedSubscriberGetsHeldOnce(t *testing.T) {
	t.Parallel()
	o := sighupOpts{detached: true}
	w := newSighupWorld(t, o)
	const label = "held-while-detached"
	w.env.Engine.SetChannelConsumerParkedForTest(true)
	w.addToWorld(t, label)
	w.env.Engine.ApplyLabelWebhookForTest(w.env.OwnerRepo, w.num, label, true, "alice")
	w.restart(t, o)
	w.settleSecond(t)
	w.wantOnce(t, label)
}
