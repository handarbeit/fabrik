package gate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/handarbeit/fabrik/tests/e2e/bedstate"
)

const (
	capsBody  = `{"version":1,"trials":{"handarbeit/fabrik-test-beta":[1,2,3,4,5,6]}}`
	limitBody = `{"version":1,"until":"2026-10-29T00:00:00Z"}`
)

// seedBedDecisions writes a tripped train-caps file and a usage-limit hold.
func seedBedDecisions(t *testing.T, bed string) {
	t.Helper()
	mustWrite(t, bedstate.TrainCapsPath(bed), capsBody)
	mustWrite(t, filepath.Join(bedstate.DecisionsDir(bed), bedstate.UsageLimitFile), limitBody)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// assertLegSwitchCleansCaps is the acceptance check (FR-009): at the moment the
// harness's TestSwitchTrainMode is issued, the train-caps file is gone and the
// usage-limit file is byte-identical. The same helper runs with the clean
// neutralised, where it must fail (FR-010).
func assertLegSwitchCleansCaps(t *testing.T, disable bool) (failed bool) {
	t.Helper()
	lf := &legFake{}
	g, _, _ := newLegGate(t, lf)
	g.trainCapsClearDisabledForTest = disable
	bed := g.Cfg.TestBed
	seedBedDecisions(t, bed)

	var capsAtSwitch bool
	var limitAtSwitch string
	fe := g.Exec.(*fakeExec)
	inner := fe.handler
	fe.handler = func(ctx context.Context, c Cmd) Result {
		if isSwitch(c) {
			capsAtSwitch = exists(bedstate.TrainCapsPath(bed))
			b, _ := os.ReadFile(filepath.Join(bedstate.DecisionsDir(bed), bedstate.UsageLimitFile))
			limitAtSwitch = string(b)
		}
		return inner(ctx, c)
	}
	if err := g.restartBed(context.Background(), defaultCell, nil); err != nil {
		t.Fatalf("restartBed: %v", err)
	}
	if len(lf.switchCmds) != 1 {
		t.Fatalf("switch commands = %d, want 1", len(lf.switchCmds))
	}
	if capsAtSwitch {
		failed = true
		t.Log("train-caps file still present when the switch was issued")
	}
	if limitAtSwitch != limitBody {
		failed = true
		t.Logf("usage-limit file = %q, want it untouched", limitAtSwitch)
	}
	return failed
}

func TestLegSwitchRemovesTrainCapsAndLeavesUsageLimit(t *testing.T) {
	if assertLegSwitchCleansCaps(t, false) {
		t.Fatal("the leg switch did not clean the train caps")
	}
}

// TestLegSwitchTrainCapsCleanIsNotVacuous: with the clean neutralised the same
// assertion fails (FR-010, SC-002).
func TestLegSwitchTrainCapsCleanIsNotVacuous(t *testing.T) {
	var failed bool
	t.Run("neutralised", func(t *testing.T) {
		// Run the helper on a scratch *testing.T-free path: it only logs on failure.
		failed = assertLegSwitchCleansCaps(t, true)
	})
	if !failed {
		t.Fatal("the assertion passed with the clean neutralised: the test is vacuous")
	}
}

func TestLegSwitchWithNoTrainCapsFileSucceeds(t *testing.T) {
	lf := &legFake{}
	g, _, _ := newLegGate(t, lf)
	if err := g.restartBed(context.Background(), defaultCell, nil); err != nil {
		t.Fatalf("restartBed: %v", err)
	}
	if len(lf.switchCmds) != 1 {
		t.Errorf("switch commands = %d, want 1", len(lf.switchCmds))
	}
}

func TestLegSwitchStopsALiveEngineBeforeClearing(t *testing.T) {
	lf := &legFake{}
	g, _, _ := newLegGate(t, lf)
	bed := g.Cfg.TestBed
	seedBedDecisions(t, bed)
	fp := g.Procs.(*fakeProcs)
	fp.launch(t, bed, 4242, "tok", "sigint")

	var aliveAtSwitch bool
	fe := g.Exec.(*fakeExec)
	inner := fe.handler
	fe.handler = func(ctx context.Context, c Cmd) Result {
		if isSwitch(c) {
			aliveAtSwitch = fp.Alive(4242)
			if exists(bedstate.TrainCapsPath(bed)) {
				t.Error("train caps present at the switch")
			}
		}
		return inner(ctx, c)
	}
	if err := g.restartBed(context.Background(), defaultCell, nil); err != nil {
		t.Fatalf("restartBed: %v", err)
	}
	if aliveAtSwitch {
		t.Error("the engine was still alive when the switch was issued")
	}
	if sig := fp.sent(); len(sig) != 1 || !strings.HasPrefix(sig[0], "4242 ") {
		t.Errorf("signals = %v, want one to 4242", sig)
	}
}

func TestLegSwitchFailsWhenTheEngineWillNotDie(t *testing.T) {
	lf := &legFake{}
	g, _, _ := newLegGate(t, lf)
	bed := g.Cfg.TestBed
	seedBedDecisions(t, bed)
	g.Procs.(*fakeProcs).launch(t, bed, 4242, "tok", "never")

	err := g.restartBed(context.Background(), defaultCell, nil)
	var ee *ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("restartBed = %v, want an *ExitError", err)
	}
	if len(lf.switchCmds) != 0 {
		t.Error("the switch ran with an engine still up")
	}
	if !exists(bedstate.TrainCapsPath(bed)) {
		t.Error("the file was removed under a live engine")
	}
}

func TestLegSwitchDoesNotSignalARecycledPID(t *testing.T) {
	lf := &legFake{}
	g, _, _ := newLegGate(t, lf)
	bed := g.Cfg.TestBed
	seedBedDecisions(t, bed)
	fp := g.Procs.(*fakeProcs)
	fp.launch(t, bed, 4242, "tok", "never").unlocked = true

	if err := g.restartBed(context.Background(), defaultCell, nil); err != nil {
		t.Fatalf("restartBed: %v", err)
	}
	if sig := fp.sent(); len(sig) != 0 {
		t.Errorf("signals = %v, want none", sig)
	}
	if exists(bedstate.TrainCapsPath(bed)) {
		t.Error("train caps not cleared")
	}
}

func TestLegSwitchFailsWhenTheFileCannotBeRemoved(t *testing.T) {
	lf := &legFake{}
	g, _, _ := newLegGate(t, lf)
	mustWrite(t, filepath.Join(bedstate.TrainCapsPath(g.Cfg.TestBed), "child"), "x")
	err := g.restartBed(context.Background(), defaultCell, nil)
	var ee *ExitError
	if !errors.As(err, &ee) || !strings.Contains(ee.Msg, "train caps") {
		t.Fatalf("restartBed = %v, want a train-caps ExitError", err)
	}
	if len(lf.switchCmds) != 0 {
		t.Error("the switch ran with stale caps")
	}
}

func TestRunLegClearsTrainCapsBeforeTheSuite(t *testing.T) {
	lf := &legFake{suiteOut: passStream, budgets: []string{"4000", "1200"}}
	g, _, errb := newLegGate(t, lf)
	seedBedDecisions(t, g.Cfg.TestBed)
	if err := g.RunLeg(context.Background(), defaultCell); err != nil {
		t.Fatalf("RunLeg: %v\n%s", err, errb)
	}
	if exists(bedstate.TrainCapsPath(g.Cfg.TestBed)) {
		t.Error("train caps survived the leg switch")
	}
}

func TestResetClearsTrainCapsAndLeavesUsageLimit(t *testing.T) {
	g, _ := newResetGate(t, &resetFake{projectID: "PVT_x", remaining: "0"})
	seedBedDecisions(t, g.Cfg.TestBed)
	if err := g.Reset(context.Background(), ResetOptions{}); err != nil {
		t.Fatal(err)
	}
	if exists(bedstate.TrainCapsPath(g.Cfg.TestBed)) {
		t.Error("reset left the train caps")
	}
	b, err := os.ReadFile(filepath.Join(bedstate.DecisionsDir(g.Cfg.TestBed), bedstate.UsageLimitFile))
	if err != nil || string(b) != limitBody {
		t.Errorf("usage-limit = %q, %v; want untouched", b, err)
	}
}

func TestResetWithALiveEngineKeepsTrainCapsAndWarns(t *testing.T) {
	g, _ := newResetGate(t, &resetFake{projectID: "PVT_x", remaining: "0"})
	seedBedDecisions(t, g.Cfg.TestBed)
	fp := g.Procs.(*fakeProcs)
	fp.launch(t, g.Cfg.TestBed, 4242, "tok", "never")
	if err := g.Reset(context.Background(), ResetOptions{}); err != nil {
		t.Fatal(err)
	}
	if !exists(bedstate.TrainCapsPath(g.Cfg.TestBed)) {
		t.Error("reset removed the file under a live engine")
	}
	if !strings.Contains(g.Err.(interface{ String() string }).String(), "train caps are left in place") {
		t.Error("no warning")
	}
	if len(fp.sent()) != 0 {
		t.Error("reset signalled the engine")
	}
}

func TestResetBedsClearsOnlyTheNamedBed(t *testing.T) {
	g, _ := newResetGate(t, &resetFake{projectID: "PVT_x", remaining: "0"})
	other := t.TempDir()
	mustWrite(t, other+"/.env", "FABRIK_TOKEN=other\n")
	seedBedDecisions(t, other)
	seedBedDecisions(t, g.Cfg.TestBed)
	if err := g.ResetBeds(context.Background(), ResetOptions{}, other); err != nil {
		t.Fatal(err)
	}
	if exists(bedstate.TrainCapsPath(other)) {
		t.Error("the named bed was not cleared")
	}
	if !exists(bedstate.TrainCapsPath(g.Cfg.TestBed)) {
		t.Error("another bed was cleared")
	}
}

func TestResetTrainCapsCleanIsNotVacuous(t *testing.T) {
	g, _ := newResetGate(t, &resetFake{projectID: "PVT_x", remaining: "0"})
	g.trainCapsClearDisabledForTest = true
	seedBedDecisions(t, g.Cfg.TestBed)
	if err := g.Reset(context.Background(), ResetOptions{}); err != nil {
		t.Fatal(err)
	}
	if !exists(bedstate.TrainCapsPath(g.Cfg.TestBed)) {
		t.Fatal("the neutralised reset still removed the file")
	}
}
