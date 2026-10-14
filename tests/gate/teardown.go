package gate

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/handarbeit/fabrik/internal/sessionreap"
)

// Bed engine teardown on every gate exit (#2245, ADR-2245).
//
// The bed's engine used to be stopped only by the preflight (before a rebuild)
// and by the multi-bed scheduler when it parked an idle bed, so a gate whose
// suite failed, was killed by its watchdog or was cancelled left it polling —
// spending the bed identity's GraphQL budget and acting on the test board — for
// hours (the v0.0.84 orphans, pids 90575 and 95267).
//
// Ownership (R2). The engine that outlives a gate is almost never the one
// StartBed launched: every leg's TestSwitchTrainMode stops it and starts a
// harness-launched successor. A PID recorded at start is therefore stale by the
// time anything fails. The gate records instead what it must NOT touch: the
// lock-holder that already existed when the gate first mutated the bed
// (snapshotBed). At exit, the engine named in fabrik.lock is the gate's to stop
// only if it is live, has a readable start token different from the snapshot's,
// and still holds the lock's flock (which also rules out a stale pid that an
// unrelated process has since recycled).

// bedSnapshot is the bed's lock-holder before the gate touched the bed.
type bedSnapshot struct {
	pid   int    // 0 when no live engine held the bed
	token string // its start token
}

// procs is the Procs in use: the real one unless a test injected a fake.
func (g *Gate) procs() Procs {
	if g.Procs != nil {
		return g.Procs
	}
	return osProcs{}
}

// lockFilePID is the PID recorded in the bed's fabrik.lock, live or not; 0 when
// the file is absent or unreadable. The file is never unlinked by the engine, so
// it also names the last engine that ran.
func lockFilePID(bed string) int {
	data, err := os.ReadFile(filepath.Join(bed, ".fabrik", "fabrik.lock"))
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

// snapshotBed records the engine already holding the bed, if any.
func (g *Gate) snapshotBed() bedSnapshot {
	p := g.procs()
	pid := lockFilePID(g.Cfg.TestBed)
	if pid == 0 || !p.Alive(pid) {
		return bedSnapshot{}
	}
	return bedSnapshot{pid: pid, token: p.Token(pid)}
}

// snapshotBeds snapshots every bed and returns the teardown to defer.
func (g *Gate) snapshotBeds() func() {
	beds := g.beds()
	snaps := make([]bedSnapshot, len(beds))
	for i, b := range beds {
		snaps[i] = b.snapshotBed()
	}
	return func() {
		var wg sync.WaitGroup
		for i, b := range beds {
			wg.Add(1)
			go func(b *Gate, s bedSnapshot) {
				defer wg.Done()
				b.teardownBedEngine(s)
				b.flushOutput()
			}(b, snaps[i])
		}
		wg.Wait()
	}
}

// teardownBedEngine stops the bed's engine if the gate owns it, and prints
// exactly one line saying what became of it (R3). It never returns an error and
// never touches the gate's exit code: a failed stop only warns. It does not use
// the gate's context, so a cancelled gate still cleans up; the escalation is
// bounded by counted waits, not by that context.
func (g *Gate) teardownBedEngine(snap bedSnapshot) {
	p := g.procs()
	pid := lockFilePID(g.Cfg.TestBed)
	if pid == 0 || !p.Alive(pid) {
		g.outln("== bed engine: none running at exit, nothing to stop ==")
		return
	}
	tok := p.Token(pid)
	switch {
	case tok == "":
		g.leftRunning(pid, "its start token is unreadable, so its identity cannot be confirmed")
		return
	case snap.pid == pid && snap.token == tok:
		g.leftRunning(pid, "it was already running before this gate invocation, so it is not the gate's")
		return
	case !p.LockHeld(g.Cfg.TestBed):
		g.leftRunning(pid, "it does not hold the bed's instance lock, so it is not the bed engine (recycled PID)")
		return
	}

	owner := sessionreap.Owner{PID: pid, Start: tok}
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
	stopped := func(how string) {
		g.outf("== bed engine: stopped pid %d (%s) ==\n", pid, how)
	}

	// SIGINT is the engine's clean stop (#1393): it drains and durably pauses
	// in-flight issues. A second one is its force-quit (ADR-1393).
	_ = p.Signal(pid, syscall.SIGINT) //nolint:errcheck // liveness decides
	if wait(g.Cfg.StopWait) {
		stopped("SIGINT")
		return
	}
	_ = p.Signal(pid, syscall.SIGINT) //nolint:errcheck // liveness decides
	if wait(g.Cfg.KillGrace) {
		stopped("SIGINT, then force-quit")
		return
	}
	// The ownership-checked group path (ADR-1957) serves both launch paths: the
	// gate's Setsid engine and the harness's Setpgid one each lead their group.
	for _, s := range []struct {
		sig  syscall.Signal
		name string
	}{{syscall.SIGTERM, "escalated to SIGTERM"}, {syscall.SIGKILL, "escalated to SIGKILL"}} {
		if err := p.SignalGroup(owner, s.sig); err != nil {
			g.leftRunning(pid, fmt.Sprintf("escalation refused: %v", err))
			return
		}
		if wait(g.Cfg.KillGrace) {
			stopped(s.name)
			return
		}
	}
	g.leftRunning(pid, "it survived SIGINT, a second SIGINT, SIGTERM and SIGKILL")
}

func (g *Gate) leftRunning(pid int, why string) {
	g.errf("warning: bed engine: left pid %d running — %s\n", pid, why)
}
