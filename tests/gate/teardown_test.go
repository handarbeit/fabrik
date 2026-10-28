package gate

import (
	"context"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/internal/sessionreap"
)

// These tests prove the bed-engine teardown (#2245, ADR-2245) over a scripted
// Procs: it runs on every exit path that reached the bed, signals only an
// engine the gate owns, and escalates in the documented order.

func fakeProcsOf(t *testing.T, g *Gate) *fakeProcs {
	t.Helper()
	fp, ok := g.Procs.(*fakeProcs)
	if !ok {
		t.Fatalf("Procs is %T", g.Procs)
	}
	return fp
}

func outText(g *Gate) string { return g.Out.(interface{ String() string }).String() }
func errText(g *Gate) string { return g.Err.(interface{ String() string }).String() }

// engineAtSuite makes the live suite "leave an engine behind", as the harness's
// TestSwitchTrainMode successor does on a real bed.
func engineAtSuite(t *testing.T, g *Gate, rf *runFake, diesOn string) {
	t.Helper()
	fp := fakeProcsOf(t, g)
	rf.suiteHook = func(Cmd) { fp.launch(t, g.Cfg.TestBed, 4242, "tok-new", diesOn) }
}

func wantOneStop(t *testing.T, g *Gate) {
	t.Helper()
	if got := fakeProcsOf(t, g).sent(); len(got) != 1 || got[0] != "4242 interrupt" {
		t.Errorf("signals = %v, want one SIGINT to 4242", got)
	}
	if n := strings.Count(outText(g), "bed engine:"); n != 1 || !strings.Contains(outText(g), "stopped pid 4242") {
		t.Errorf("want exactly one line naming the stopped pid:\n%s", outText(g))
	}
}

func TestTeardownOnSuiteFailure(t *testing.T) {
	rf := newRunFake()
	rf.suiteRC = 1
	g, _ := newRunGate(t, rf, "E2E_SKIP_PREGATE=1", "E2E_SKIP_PREP=1", "E2E_TRAIN_MODE=off")
	engineAtSuite(t, g, rf, "sigint")
	if code := g.Run(context.Background(), nil); code != 1 {
		t.Fatalf("the exit code stays the suite's own, got %d", code)
	}
	wantOneStop(t, g)
}

func TestTeardownOnSuccess(t *testing.T) {
	rf := newRunFake()
	g, _ := newRunGate(t, rf, "E2E_SKIP_PREGATE=1", "E2E_SKIP_PREP=1", "E2E_TRAIN_MODE=off")
	engineAtSuite(t, g, rf, "sigint")
	if code := g.Run(context.Background(), nil); code != 0 {
		t.Fatalf("exit %d", code)
	}
	wantOneStop(t, g)
}

func TestTeardownOnCancellation(t *testing.T) {
	rf := newRunFake()
	g, _ := newRunGate(t, rf, "E2E_SKIP_PREGATE=1", "E2E_SKIP_PREP=1", "E2E_TRAIN_MODE=off")
	fp := fakeProcsOf(t, g)
	ctx, cancel := context.WithCancel(context.Background())
	rf.suiteHook = func(Cmd) {
		fp.launch(t, g.Cfg.TestBed, 4242, "tok-new", "sigint")
		cancel()
	}
	if code := g.Run(ctx, nil); code != 130 {
		t.Fatalf("exit %d", code)
	}
	wantOneStop(t, g) // the already-cancelled ctx must not stop the cleanup
}

func TestTeardownOnPostSuiteWatchdog(t *testing.T) {
	rf := newRunFake()
	rf.budgets = []string{"4000", "HANG"}
	g, _ := newRunGate(t, rf, "E2E_SKIP_PREGATE=1", "E2E_SKIP_PREP=1", "E2E_TRAIN_MODE=off")
	engineAtSuite(t, g, rf, "sigint")
	g.Cfg.PostSuiteWatchdog = 300 * time.Millisecond
	if code := g.Run(context.Background(), nil); code != ExitPostSuiteWatchdog {
		t.Fatalf("exit %d", code)
	}
	wantOneStop(t, g)
}

// A preflight failure AFTER the engine exists (here: the stale-binary check)
// still stops it.
func TestTeardownOnFailureAfterTheBedWasStarted(t *testing.T) {
	rf := newRunFake()
	rf.headSHA = rf.wantSHA
	g, _ := newRunGate(t, rf, "E2E_SKIP_PREGATE=1", "E2E_BED_NO_BUILD=1")
	fp := fakeProcsOf(t, g)
	g.Exec.(*startWriter).fakeExec.handler = func(ctx context.Context, c Cmd) Result {
		if c.Name == "./fabrik" && len(c.Args) > 0 && c.Args[0] == "--version" {
			fp.launch(t, g.Cfg.TestBed, 4242, "tok-new", "sigint")
			writeStdout(c, "fabrik version 0.0.0-dev (0000000)\n")
			return Result{}
		}
		return rf.handle(ctx, c)
	}
	if code := g.Run(context.Background(), nil); code != ExitPreflightFailed {
		t.Fatalf("exit %d", code)
	}
	wantOneStop(t, g)
}

// ADR-1454: a pre-gate failure touches nothing and says nothing about a bed.
func TestTeardownNotRegisteredBeforeTheBedIsTouched(t *testing.T) {
	rf := newRunFake()
	rf.simRC = 1
	g, _ := newRunGate(t, rf, "E2E_SKIP_PREP=1")
	if code := g.Run(context.Background(), nil); code != ExitPregateFailed {
		t.Fatalf("exit %d", code)
	}
	if strings.Contains(outText(g)+errText(g), "bed engine:") || len(fakeProcsOf(t, g).sent()) != 0 {
		t.Errorf("a pre-gate failure must not touch the bed:\n%s%s", outText(g), errText(g))
	}
}

// Nothing running at exit: no signal, one line saying so.
func TestTeardownEngineAlreadyGone(t *testing.T) {
	rf := newRunFake()
	g, _ := newRunGate(t, rf, "E2E_SKIP_PREGATE=1", "E2E_SKIP_PREP=1", "E2E_TRAIN_MODE=off")
	if code := g.Run(context.Background(), nil); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(outText(g), "none running at exit") || len(fakeProcsOf(t, g).sent()) != 0 {
		t.Errorf("out:\n%s", outText(g))
	}
}

// An engine that was already holding the bed before the gate touched it (an
// operator's, or one E2E_SKIP_PREP left) is never stopped.
func TestTeardownNeverStopsAnEngineItDidNotStart(t *testing.T) {
	rf := newRunFake()
	rf.suiteRC = 1
	g, _ := newRunGate(t, rf, "E2E_SKIP_PREGATE=1", "E2E_SKIP_PREP=1", "E2E_TRAIN_MODE=off")
	// The leg switch's train-caps clean (#2482) stops a live engine, as the
	// harness's own switch does; this test is about the exit teardown only.
	g.trainCapsClearDisabledForTest = true
	fakeProcsOf(t, g).launch(t, g.Cfg.TestBed, 777, "tok-old", "sigint")
	if code := g.Run(context.Background(), nil); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if got := fakeProcsOf(t, g).sent(); len(got) != 0 {
		t.Errorf("signalled %v", got)
	}
	if e := errText(g); !strings.Contains(e, "left pid 777 running") || !strings.Contains(e, "before this gate invocation") {
		t.Errorf("the log line must name the pid and why:\n%s", e)
	}
}

func TestTeardownRefusals(t *testing.T) {
	tests := []struct {
		name  string
		snap  bedSnapshot
		tok   string
		unlk  bool
		reasn string
	}{
		{"recycled pid that holds no lock", bedSnapshot{pid: 5, token: "a"}, "b", true, "recycled PID"},
		{"unrecorded process that holds no lock", bedSnapshot{}, "b", true, "recycled PID"},
		{"unreadable token", bedSnapshot{}, "", false, "start token is unreadable"},
		{"same pid and token as the snapshot", bedSnapshot{pid: 5, token: "b"}, "b", false, "before this gate invocation"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g, _, _, _ := testGate(t)
			fp := fakeProcsOf(t, g)
			fp.launch(t, g.Cfg.TestBed, 5, tc.tok, "sigint").unlocked = tc.unlk
			g.teardownBedEngine(tc.snap)
			if got := fp.sent(); len(got) != 0 {
				t.Errorf("signalled %v", got)
			}
			if e := errText(g); !strings.Contains(e, "left pid 5 running") || !strings.Contains(e, tc.reasn) {
				t.Errorf("stderr = %q, want reason %q", e, tc.reasn)
			}
		})
	}
	t.Run("a different token on the same pid, lock held, is the gate's", func(t *testing.T) {
		g, _, _, _ := testGate(t)
		fp := fakeProcsOf(t, g)
		fp.launch(t, g.Cfg.TestBed, 5, "b", "sigint")
		g.teardownBedEngine(bedSnapshot{pid: 5, token: "a"})
		if got := fp.sent(); len(got) != 1 {
			t.Errorf("signals = %v", got)
		}
	})
}

func TestTeardownEscalationLadder(t *testing.T) {
	sigint := "4242 interrupt"
	tests := []struct {
		diesOn string
		want   []string
		how    string
	}{
		{"sigint", []string{sigint}, "(SIGINT)"},
		{"sigint2", []string{sigint, sigint}, "force-quit"},
		{"sigterm", []string{sigint, sigint, "4242 group terminated"}, "escalated to SIGTERM"},
		{"sigkill", []string{sigint, sigint, "4242 group terminated", "4242 group killed"}, "escalated to SIGKILL"},
	}
	for _, tc := range tests {
		t.Run(tc.diesOn, func(t *testing.T) {
			g, _, _, _ := testGate(t)
			fp := fakeProcsOf(t, g)
			fp.launch(t, g.Cfg.TestBed, 4242, "tok", tc.diesOn)
			g.teardownBedEngine(bedSnapshot{})
			if got := fp.sent(); strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("signals = %v, want %v", got, tc.want)
			}
			if o := outText(g); !strings.Contains(o, "stopped pid 4242") || !strings.Contains(o, tc.how) {
				t.Errorf("out = %q", o)
			}
		})
	}
	t.Run("an engine that never dies only warns", func(t *testing.T) {
		g, _, _, _ := testGate(t)
		fp := fakeProcsOf(t, g)
		fp.launch(t, g.Cfg.TestBed, 4242, "tok", "never")
		g.teardownBedEngine(bedSnapshot{})
		if len(fp.sent()) != 4 || !strings.Contains(errText(g), "left pid 4242 running") {
			t.Errorf("signals %v, stderr %q", fp.sent(), errText(g))
		}
	})
	t.Run("a refused group escalation is reported, not forced", func(t *testing.T) {
		g, _, _, _ := testGate(t)
		fp := fakeProcsOf(t, g)
		p := fp.launch(t, g.Cfg.TestBed, 4242, "tok", "never")
		g.Procs = &tokenSwapProcs{fakeProcs: fp, p: p}
		g.teardownBedEngine(bedSnapshot{})
		if !strings.Contains(errText(g), "escalation refused") {
			t.Errorf("stderr = %q", errText(g))
		}
	})
}

// tokenSwapProcs makes SignalGroup see a different owner than the process
// holds, as a recycled PID would.
type tokenSwapProcs struct {
	*fakeProcs
	p *fakeProc
}

func (s *tokenSwapProcs) SignalGroup(o sessionreap.Owner, sig syscall.Signal) error {
	o.Start = "other"
	return s.fakeProcs.SignalGroup(o, sig)
}

func TestTeardownMultiBedCoversEveryBedOnCancel(t *testing.T) {
	g, _, _, _ := twoBedGate(t)
	beds := g.beds()
	td := g.snapshotBeds()
	fp := fakeProcsOf(t, g)
	fp.launch(t, beds[0].Cfg.TestBed, 100, "tok-a", "sigint")
	// bed B's engine was already stopped (stopIdleEngine) before exit.
	td()
	if got := fp.sent(); len(got) != 1 || got[0] != "100 interrupt" {
		t.Errorf("signals = %v", got)
	}
	if o := outText(g); !strings.Contains(o, "[bed A] == bed engine: stopped pid 100") || !strings.Contains(o, "[bed B] == bed engine: none running") {
		t.Errorf("out:\n%s", o)
	}
}
