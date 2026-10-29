package gate

import (
	"fmt"
	"syscall"
	"time"

	"github.com/handarbeit/fabrik/tests/e2e/bedstate"
)

// Clean train caps at every leg switch and reset (#2482, ADR-2482).
//
// The engine persists its merge-train caps — the runaway guard's trial window,
// ejection counts, auto-repair attempts — across a restart (#2349, ADR 2349). A
// gate leg switch is a restart of the same bed, so a leg that deliberately trips
// the guard left a tripped guard for the next leg (the 0.0.90 gate:
// TestCrossRepoSpawn paused by pat/on's TestMergeTrainRunawayGuardPausesBatch).
// The gate therefore removes the bed's train-caps file, and only that file, while
// the bed's engine is stopped. A running engine rewrites it from memory, so the
// engine is stopped first; the harness's own switch (TestSwitchTrainMode) then
// finds a stopped bed and just starts it. usage-limit.json is account state and
// is never touched.

// stopBedEngine stops the bed's engine if one holds the bed, with the engine's own
// clean-stop escalation (#1393: a first SIGINT drains and durably pauses in-flight
// issues, a second is the force-quit), each bounded by a counted wait. It returns
// nil when no engine is running, and an error when one survives — the caller must
// then not clear the file or start the leg. A PID that does not hold the bed's
// instance lock is a recycled one and is never signalled.
func (g *Gate) stopBedEngine() error {
	p := g.procs()
	bed := g.Cfg.TestBed
	pid := lockFilePID(bed)
	if pid == 0 || !p.Alive(pid) {
		return nil
	}
	tok := p.Token(pid)
	if tok == "" {
		return fmt.Errorf("bed engine pid %d: its start token is unreadable, so it cannot be confirmed as the bed's engine", pid)
	}
	if !p.LockHeld(bed) {
		return nil // a recycled PID, not an engine
	}
	same := func() bool { return p.Alive(pid) && p.Token(pid) == tok }
	wait := func(d time.Duration) bool {
		for n := 0; n < int(d.Seconds()) || n == 0; n++ {
			if !same() {
				return true
			}
			g.Sleep(time.Second)
		}
		return !same()
	}
	for i, d := range []time.Duration{g.Cfg.StopWait, g.Cfg.KillGrace} {
		if err := p.Signal(pid, syscall.SIGINT); err != nil && !same() {
			return nil
		}
		if wait(d) {
			g.outf("== bed engine: stopped pid %d before clearing train caps (signal %d) ==\n", pid, i+1)
			return nil
		}
	}
	return fmt.Errorf("bed engine pid %d survived two SIGINTs", pid)
}

// clearBedTrainCaps is the leg switch's clean: stop the bed's engine, then remove
// the bed's train-caps file. A missing file is fine; a stuck engine or a file that
// cannot be removed is an *ExitError, so a leg never starts on stale caps.
func (g *Gate) clearBedTrainCaps() error {
	if g.trainCapsClearDisabledForTest {
		return nil
	}
	if err := g.stopBedEngine(); err != nil {
		return exitErr(1, "gate: cannot clear the bed's train caps: %v", err)
	}
	removed, err := bedstate.ClearTrainCaps(g.Cfg.TestBed)
	if err != nil {
		return exitErr(1, "gate: cannot clear the bed's train caps: %v", err)
	}
	if removed {
		g.outf("== cleared %s (the next engine starts with empty train caps) ==\n", bedstate.TrainCapsPath(g.Cfg.TestBed))
	}
	return nil
}

// resetTrainCaps is `gate reset`'s clean. It acts only on a stopped bed: with a
// live engine the removal would be futile (the engine rewrites the file), and
// reset does not become a bed stopper, so it warns and leaves the file.
func (g *Gate) resetTrainCaps() error {
	if g.trainCapsClearDisabledForTest {
		return nil
	}
	p := g.procs()
	if pid := lockFilePID(g.Cfg.TestBed); pid != 0 && p.Alive(pid) && p.LockHeld(g.Cfg.TestBed) {
		g.errf("warning: reset: a bed engine (pid %d) is running, so the train caps are left in place — stop it and reset again to clear them\n", pid)
		return nil
	}
	removed, err := bedstate.ClearTrainCaps(g.Cfg.TestBed)
	if err != nil {
		return exitErr(1, "reset: cannot clear the bed's train caps: %v", err)
	}
	if removed {
		g.outf("  cleared the bed's train caps\n")
	}
	return nil
}
