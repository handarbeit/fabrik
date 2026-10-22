package bedtimeout

import (
	"strings"
	"testing"
	"time"
)

func TestEnvEntries(t *testing.T) {
	env := Env()
	want := map[string]bool{"FABRIK_REVIEW_WAIT_TIMEOUT=10": false, "FABRIK_CI_WAIT_TIMEOUT=30": false}
	for _, e := range env {
		if _, ok := want[e]; !ok {
			t.Errorf("unexpected entry %q", e)
		}
		want[e] = true
	}
	for e, seen := range want {
		if !seen {
			t.Errorf("missing %q", e)
		}
	}
	if ReviewWait() != 10*time.Minute || CIWait() != 30*time.Minute {
		t.Errorf("accessors disagree with constants: %v %v", ReviewWait(), CIWait())
	}
	// The review timeout must stay at or above Conjunctive's skip floor (10) and
	// the CI dwell above the 25-minute slow-gate check.
	if ReviewMinutes < 10 || CIMinutes <= 25 {
		t.Errorf("bed timeouts below their floors: review=%d ci=%d", ReviewMinutes, CIMinutes)
	}
}

func TestCeilingArithmetic(t *testing.T) {
	armed := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	w := Window{Armed: armed, Timeout: 10 * time.Minute, Poll: time.Minute, WorkerAllowance: 5 * time.Minute}
	if got, want := w.Ceiling(), armed.Add(18*time.Minute); !got.Equal(want) {
		t.Errorf("Ceiling = %v, want %v", got, want)
	}
	if got, want := w.Earliest(), armed.Add(10*time.Minute); !got.Equal(want) {
		t.Errorf("Earliest = %v, want %v", got, want)
	}
}

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time        { return c.t }
func (c *fakeClock) sleep(d time.Duration) { c.t = c.t.Add(d) }

func newWindow() (Window, *fakeClock) {
	armed := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	// The clock starts a little after arming, as in the test (reinvoke observed).
	return Window{Armed: armed, Timeout: 10 * time.Minute, Poll: time.Minute}, &fakeClock{t: armed.Add(2 * time.Minute)}
}

// A simulated bug (Phase 1 fires at the timeout) must end the window as a bug.
func TestWatchDetectsBug(t *testing.T) {
	w, c := newWindow()
	out, obs := Watch(w, c.now, c.sleep, func() Observation {
		if !c.now().Before(w.Earliest()) {
			return Observation{Bug: true, Detail: "fabrik:bot-reprompted applied"}
		}
		return Observation{}
	})
	if out != OutcomeBug || !strings.Contains(obs.Detail, "bot-reprompted") {
		t.Fatalf("got %v %+v, want bug", out, obs)
	}
}

// The correct escalation ends the window, but only once the timeout has passed.
func TestWatchSettlesOnlyAfterTimeout(t *testing.T) {
	w, c := newWindow()
	probes := 0
	out, _ := Watch(w, c.now, c.sleep, func() Observation {
		probes++
		return Observation{Settled: true} // an early "escalation" cannot count
	})
	if out != OutcomeSettled {
		t.Fatalf("got %v, want settled", out)
	}
	if c.now().Before(w.Earliest()) {
		t.Fatalf("settled at %v, before the earliest the bug could show (%v)", c.now(), w.Earliest())
	}
	if probes < 2 {
		t.Fatalf("settled on probe %d: an early escalation must be ignored", probes)
	}
}

// Nothing happening ends at the ceiling, never earlier, never an endless loop.
func TestWatchEndsAtCeiling(t *testing.T) {
	w, c := newWindow()
	out, _ := Watch(w, c.now, c.sleep, func() Observation { return Observation{} })
	if out != OutcomeCeiling {
		t.Fatalf("got %v, want ceiling", out)
	}
	if c.now().Before(w.Ceiling()) {
		t.Fatalf("ended at %v, before the ceiling %v", c.now(), w.Ceiling())
	}
	if c.now().After(w.Ceiling().Add(w.Poll)) {
		t.Fatalf("overshot the ceiling by more than a poll: %v", c.now())
	}
}

// A bug that shows before the timeout would have to be a different failure, but
// it must still fail the window rather than be waited out.
func TestWatchBugIsImmediate(t *testing.T) {
	w, c := newWindow()
	start := c.now()
	out, _ := Watch(w, c.now, c.sleep, func() Observation { return Observation{Bug: true} })
	if out != OutcomeBug || !c.now().Equal(start) {
		t.Fatalf("got %v after %v, want an immediate bug", out, c.now().Sub(start))
	}
}
