package pollhold

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/internal/pollctl"
	"github.com/handarbeit/fabrik/tests/e2e/inconclusive"
)

// fakeTB records what a helper did to its test. Fatalf and Skip end the calling
// goroutine like the real testing.T, so run() executes a body in its own
// goroutine and reports how it ended.
type fakeTB struct {
	name     string
	mu       sync.Mutex
	logs     []string
	errs     []string
	fatal    string
	skipped  string
	cleanups []func()
}

func (f *fakeTB) Helper()      {}
func (f *fakeTB) Name() string { return f.name }
func (f *fakeTB) Logf(format string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logs = append(f.logs, sprintf(format, args...))
}
func (f *fakeTB) Errorf(format string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errs = append(f.errs, sprintf(format, args...))
}
func (f *fakeTB) Fatalf(format string, args ...any) {
	f.mu.Lock()
	f.fatal = sprintf(format, args...)
	f.mu.Unlock()
	runtime.Goexit()
}
func (f *fakeTB) Skip(args ...any) {
	f.mu.Lock()
	f.skipped = sprintf("%v", args[0])
	f.mu.Unlock()
	runtime.Goexit()
}
func (f *fakeTB) Cleanup(fn func()) { f.cleanups = append(f.cleanups, fn) }

// run executes body like a test function; cleanups then run in reverse order,
// as testing.T does — also after a Fatalf/Skip.
func (f *fakeTB) run(body func()) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			for i := len(f.cleanups) - 1; i >= 0; i-- {
				f.cleanups[i]()
			}
		}()
		body()
	}()
	<-done
}

func sprintf(format string, args ...any) string {
	return strings.TrimSpace(strings.ReplaceAll(strings.TrimSpace(fmt.Sprintf(format, args...)), "\n", " "))
}

// fakeEngine plays the engine's half of the protocol: it applies hold
// generations and answers triggers with a configurable outcome.
type fakeEngine struct {
	path    string
	stop    chan struct{}
	done    chan struct{}
	outcome string
	// stuck makes triggered polls never complete.
	stuck bool
	// neverAck makes the engine ignore the request file entirely.
	neverAck bool

	mu      sync.Mutex
	polls   int
	applied int64
	seq     int64
}

func startFakeEngine(t *testing.T, path string) *fakeEngine {
	t.Helper()
	fe := &fakeEngine{path: path, stop: make(chan struct{}), done: make(chan struct{}), outcome: pollctl.OutcomeRan}
	if err := pollctl.WriteAck(path, pollctl.Ack{}); err != nil { // seam is live
		t.Fatal(err)
	}
	go fe.loop()
	t.Cleanup(func() { close(fe.stop); <-fe.done })
	return fe
}

func (fe *fakeEngine) loop() {
	defer close(fe.done)
	ack := pollctl.Ack{}
	for {
		select {
		case <-fe.stop:
			return
		case <-time.After(5 * time.Millisecond):
		}
		fe.mu.Lock()
		skip := fe.neverAck
		fe.mu.Unlock()
		if skip {
			continue
		}
		req := pollctl.ReadRequest(fe.path)
		changed := false
		if req.HoldGen > ack.HoldGen {
			ack.HoldGen, ack.Held, changed = req.HoldGen, req.Hold, true
		}
		fe.mu.Lock()
		if req.TriggerSeq > ack.DoneSeq && !fe.stuck {
			fe.polls++
			ack.DoneSeq, ack.Outcome, ack.Detail = req.TriggerSeq, fe.outcome, "detail"
			changed = true
		}
		fe.mu.Unlock()
		if changed {
			_ = pollctl.WriteAck(fe.path, ack)
		}
	}
}

func (fe *fakeEngine) set(f func(*fakeEngine)) {
	fe.mu.Lock()
	defer fe.mu.Unlock()
	f(fe)
}

func (fe *fakeEngine) pollCount() int {
	fe.mu.Lock()
	defer fe.mu.Unlock()
	return fe.polls
}

func testController(path string, exclusive bool) *Controller {
	return &Controller{
		Path: path,
		IsExclusive: func(name string) (bool, error) {
			if name == "TestUnknown" {
				return false, errors.New("not in registry")
			}
			return exclusive, nil
		},
		AckTimeout:     2 * time.Second,
		TriggerTimeout: 2 * time.Second,
		Interval:       2 * time.Millisecond,
		MaxHold:        time.Minute,
	}
}

func TestHold_NonExclusiveTestFails(t *testing.T) {
	path := pollctl.ControlPath(t.TempDir())
	startFakeEngine(t, path)
	c := testController(path, false)
	tb := &fakeTB{name: "TestShared"}
	tb.run(func() { c.Hold(tb) })
	if !strings.Contains(tb.fatal, "not exclusive") {
		t.Fatalf("fatal = %q, want a not-exclusive failure", tb.fatal)
	}
	if req := pollctl.ReadRequest(path); req.Hold {
		t.Fatal("a non-exclusive test still managed to hold the bed")
	}
	if len(tb.cleanups) != 0 {
		t.Fatal("a refused hold registered a cleanup")
	}
}

func TestHold_UnknownTestAndSubtestResolution(t *testing.T) {
	path := pollctl.ControlPath(t.TempDir())
	startFakeEngine(t, path)
	c := testController(path, true)

	tb := &fakeTB{name: "TestUnknown"}
	tb.run(func() { c.Hold(tb) })
	if !strings.Contains(tb.fatal, "cannot establish") {
		t.Fatalf("fatal = %q", tb.fatal)
	}

	// A subtest resolves to its top-level test.
	sub := &fakeTB{name: "TestOwner/phase_one"}
	var asked string
	c.IsExclusive = func(name string) (bool, error) { asked = name; return true, nil }
	sub.run(func() { c.Hold(sub) })
	if asked != "TestOwner" || sub.fatal != "" {
		t.Fatalf("asked %q, fatal %q", asked, sub.fatal)
	}
}

func TestHold_BedWithoutSeamFails(t *testing.T) {
	path := pollctl.ControlPath(t.TempDir()) // no engine, no ack file
	c := testController(path, true)
	tb := &fakeTB{name: "TestOwner"}
	tb.run(func() { c.Hold(tb) })
	if !strings.Contains(tb.fatal, "no poll-control seam") {
		t.Fatalf("fatal = %q", tb.fatal)
	}
}

func TestHoldTriggerRelease_FullCycle(t *testing.T) {
	path := pollctl.ControlPath(t.TempDir())
	fe := startFakeEngine(t, path)
	c := testController(path, true)
	tb := &fakeTB{name: "TestOwner"}
	tb.run(func() {
		c.Hold(tb)
		if a := pollctl.ReadAck(path); !a.Held {
			t.Error("hold returned before the engine acknowledged it")
		}
		c.Trigger(tb)
		c.Trigger(tb)
		c.Release(tb)
		c.Release(tb) // idempotent
	})
	if tb.fatal != "" || len(tb.errs) != 0 || tb.skipped != "" {
		t.Fatalf("fatal=%q errs=%v skipped=%q", tb.fatal, tb.errs, tb.skipped)
	}
	if fe.pollCount() != 2 {
		t.Fatalf("engine ran %d triggered polls, want 2", fe.pollCount())
	}
	if a := pollctl.ReadAck(path); a.Held {
		t.Fatalf("still held after release: %+v", a)
	}
}

// t.Cleanup must release a held bed after a failing test.
func TestCleanupReleasesAfterFailedTest(t *testing.T) {
	path := pollctl.ControlPath(t.TempDir())
	startFakeEngine(t, path)
	c := testController(path, true)
	tb := &fakeTB{name: "TestOwner"}
	tb.run(func() {
		c.Hold(tb)
		tb.Fatalf("the scenario failed after holding")
	})
	if a := pollctl.ReadAck(path); a.Held {
		t.Fatalf("a failed test left the bed held: %+v", a)
	}
	if req := pollctl.ReadRequest(path); req.Hold {
		t.Fatalf("request still holds: %+v", req)
	}
}

func TestTrigger_RequiresHold(t *testing.T) {
	path := pollctl.ControlPath(t.TempDir())
	startFakeEngine(t, path)
	tb := &fakeTB{name: "TestOwner"}
	tb.run(func() { testController(path, true).Trigger(tb) })
	if !strings.Contains(tb.fatal, "not held") {
		t.Fatalf("fatal = %q", tb.fatal)
	}
}

func TestTrigger_Outcomes(t *testing.T) {
	cases := []struct {
		outcome   string
		wantFatal string
		wantSkip  bool
	}{
		{pollctl.OutcomeError, "failed", false},
		{pollctl.OutcomeBlocked, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.outcome, func(t *testing.T) {
			path := pollctl.ControlPath(t.TempDir())
			fe := startFakeEngine(t, path)
			fe.set(func(e *fakeEngine) { e.outcome = tc.outcome })
			c := testController(path, true)
			tb := &fakeTB{name: "TestOwner"}
			tb.run(func() { c.Hold(tb); c.Trigger(tb) })
			if tc.wantSkip {
				if !inconclusive.IsMarked(tb.skipped) {
					t.Fatalf("skipped = %q, want an Inconclusive skip", tb.skipped)
				}
			} else if !strings.Contains(tb.fatal, tc.wantFatal) {
				t.Fatalf("fatal = %q", tb.fatal)
			}
			if a := pollctl.ReadAck(path); a.Held {
				t.Fatalf("bed left held after %s: %+v", tc.outcome, a)
			}
		})
	}
}

func TestTrigger_TimeoutFailsAndNeverHangs(t *testing.T) {
	path := pollctl.ControlPath(t.TempDir())
	fe := startFakeEngine(t, path)
	fe.set(func(e *fakeEngine) { e.stuck = true })
	c := testController(path, true)
	c.TriggerTimeout = 100 * time.Millisecond
	tb := &fakeTB{name: "TestOwner"}
	start := time.Now()
	tb.run(func() { c.Hold(tb); c.Trigger(tb) })
	if !strings.Contains(tb.fatal, "did not complete") {
		t.Fatalf("fatal = %q", tb.fatal)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("the bounded wait took too long")
	}
}

func TestHold_UnacknowledgedHoldFailsAndStillReleasesViaCleanup(t *testing.T) {
	path := pollctl.ControlPath(t.TempDir())
	fe := startFakeEngine(t, path)
	fe.set(func(e *fakeEngine) { e.neverAck = true })
	c := testController(path, true)
	c.AckTimeout = 100 * time.Millisecond
	tb := &fakeTB{name: "TestOwner"}
	tb.run(func() { c.Hold(tb) })
	if !strings.Contains(tb.fatal, "did not acknowledge the hold") {
		t.Fatalf("fatal = %q", tb.fatal)
	}
	if len(tb.cleanups) != 1 {
		t.Fatal("a timed-out hold must still register its release")
	}
	if req := pollctl.ReadRequest(path); req.Hold {
		t.Fatalf("release request not written: %+v", req)
	}
	// The cleanup reports (Errorf) rather than failing hard when the engine stays silent.
	if len(tb.errs) != 1 || !strings.Contains(tb.errs[0], "did not acknowledge the release") {
		t.Fatalf("errs = %v", tb.errs)
	}
}

func TestDisabled_IsALoggedNoOp(t *testing.T) {
	path := pollctl.ControlPath(t.TempDir())
	c := testController(path, false)
	c.Disabled = true
	tb := &fakeTB{name: "TestShared"}
	tb.run(func() { c.Hold(tb); c.Trigger(tb); c.Release(tb) })
	if tb.fatal != "" || tb.skipped != "" || len(tb.errs) != 0 || len(tb.cleanups) != 0 {
		t.Fatalf("disabled controller acted: %+v", tb)
	}
}

func TestHoldContinuesSequencesFromTheFiles(t *testing.T) {
	path := pollctl.ControlPath(t.TempDir())
	startFakeEngine(t, path)
	// Leftover generation/sequence numbers from an earlier test on the same engine.
	if err := pollctl.WriteRequest(path, pollctl.Request{HoldGen: 5, TriggerSeq: 3}); err != nil {
		t.Fatal(err)
	}
	c := testController(path, true)
	tb := &fakeTB{name: "TestOwner"}
	tb.run(func() {
		c.Hold(tb)
		if r := pollctl.ReadRequest(path); r.HoldGen != 6 || r.TriggerSeq != 3 {
			t.Errorf("request after hold = %+v, want gen 6 / seq 3 preserved", r)
		}
		c.Trigger(tb)
		if r := pollctl.ReadRequest(path); r.TriggerSeq != 4 {
			t.Errorf("trigger seq = %d, want 4", r.TriggerSeq)
		}
	})
	if tb.fatal != "" {
		t.Fatal(tb.fatal)
	}
}

// Both bed launch sites must enable the seam through the shared pollctl.Env, so a
// restarted bed (TestSwitchTrainMode, restart-safety) cannot silently lose it. The
// tagged lifecycle.go cannot be exercised here, so this pins it structurally.
func TestLaunchSitesEnableTheSeam(t *testing.T) {
	for _, rel := range []string{
		filepath.Join("..", "lifecycle.go"),
		filepath.Join("..", "..", "gate", "bed.go"),
	} {
		f, err := parser.ParseFile(token.NewFileSet(), rel, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		ast.Inspect(f, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "Env" {
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == "pollctl" {
					found = true
				}
			}
			return !found
		})
		if !found {
			t.Errorf("%s does not call pollctl.Env — a bed started there would lack the poll-control seam", rel)
		}
	}
}

// A hold the engine already self-released (hold_until passed) must not be
// triggered as if still held: the request file still says hold, so Trigger has to
// read the deadline and the ack (#1978 review).
func TestTrigger_LapsedHoldIsInconclusive(t *testing.T) {
	t.Run("deadline passed", func(t *testing.T) {
		path := pollctl.ControlPath(t.TempDir())
		fe := startFakeEngine(t, path)
		c := testController(path, true)
		clock := time.Now()
		c.Now = func() time.Time { return clock }
		tb := &fakeTB{name: "TestOwner"}
		tb.run(func() {
			c.Hold(tb)
			clock = clock.Add(c.MaxHold + time.Second) // the harness's waits outlast the hold
			c.Trigger(tb)
		})
		if !inconclusive.IsMarked(tb.skipped) || !strings.Contains(tb.skipped, "lapsed") {
			t.Fatalf("skipped = %q fatal = %q, want an Inconclusive 'lapsed' skip", tb.skipped, tb.fatal)
		}
		if fe.pollCount() != 0 {
			t.Fatalf("a trigger was issued for a lapsed hold (%d polls)", fe.pollCount())
		}
	})
	t.Run("engine acked a release", func(t *testing.T) {
		path := pollctl.ControlPath(t.TempDir())
		fe := startFakeEngine(t, path)
		c := testController(path, true)
		tb := &fakeTB{name: "TestOwner"}
		tb.run(func() {
			c.Hold(tb)
			// The engine self-released: ack says not held, request still says hold.
			fe.set(func(e *fakeEngine) { e.neverAck = true })
			a := pollctl.ReadAck(path)
			a.Held = false
			if err := pollctl.WriteAck(path, a); err != nil {
				t.Fatal(err)
			}
			c.Trigger(tb)
		})
		if !inconclusive.IsMarked(tb.skipped) || !strings.Contains(tb.skipped, "lapsed") {
			t.Fatalf("skipped = %q fatal = %q, want an Inconclusive 'lapsed' skip", tb.skipped, tb.fatal)
		}
		if fe.pollCount() != 0 {
			t.Fatalf("a trigger was issued for a lapsed hold (%d polls)", fe.pollCount())
		}
	})
}
