package engine

import (
	"testing"
	"time"

	"github.com/handarbeit/fabrik/internal/localapi"
)

// fabrik_health.channel (#2484, ADR 2484): absent with no hub, ready=false while a
// freshly started daemon is warming, ready=true once the readiness pass has run.
func TestHealthChannelReadiness(t *testing.T) {
	health := func(e *Engine) *localapi.ChannelHealth {
		t.Helper()
		res, err := e.LocalAPIBackend().Health(localapi.HealthParams{})
		if err != nil {
			t.Fatal(err)
		}
		return res.Channel
	}

	t.Run("no hub", func(t *testing.T) {
		e := apiEngine(t, 0)
		if got := health(e); got != nil {
			t.Fatalf("a daemon without a channel hub reports no channel block, got %+v", got)
		}
	})

	t.Run("warming then ready", func(t *testing.T) {
		e := apiEngine(t, 0)
		e.SetChannelTimingForTest(20*time.Millisecond, time.Hour)
		e.fabrikDir = t.TempDir()
		e.startChannelEventsIn(t.TempDir(), true)
		t.Cleanup(e.closeChannelEvents)
		if got := health(e); got == nil || got.Ready {
			t.Fatalf("a warming daemon reports ready=false, got %+v", got)
		}
		e.seedAPIItem(t, 1, "Implement", "a")
		e.MarkBoardPopulatedForTest()
		if err := e.DrainChannelEventsForTest(5 * time.Second); err != nil {
			t.Fatal(err)
		}
		if got := health(e); got == nil || !got.Ready {
			t.Fatalf("after the readiness pass the daemon reports ready=true, got %+v", got)
		}
	})

	t.Run("not warming", func(t *testing.T) {
		e := apiEngine(t, 0)
		e.fabrikDir = t.TempDir()
		e.startChannelEventsIn(t.TempDir(), false)
		t.Cleanup(e.closeChannelEvents)
		if got := health(e); got == nil || !got.Ready {
			t.Fatalf("a daemon that is not warming is ready, got %+v", got)
		}
	})
}

// Closing is true once Close or Quiesce has begun, and nil-safe.
func TestIntakeClosing(t *testing.T) {
	var none *intake
	if none.Closing() {
		t.Fatal("a nil intake is not closing")
	}
	in := newIntake(newDeliveryLedger("", time.Now(), nil), nil, nil, time.Now)
	if in.Closing() {
		t.Fatal("a fresh intake is not closing")
	}
	in.Quiesce(10 * time.Millisecond)
	if !in.Closing() {
		t.Fatal("a quiesced intake is closing")
	}
	in.Resume()
	if in.Closing() {
		t.Fatal("a resumed intake is not closing")
	}
	in.Close(10 * time.Millisecond)
	if !in.Closing() {
		t.Fatal("a closed intake is closing")
	}
}
