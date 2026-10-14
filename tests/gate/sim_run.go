package gate

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/handarbeit/fabrik/internal/testlife"
)

// Defaults for the knobs below; a Config built by hand (the tests') sets the
// durations it wants, and a zero poll/heartbeat disables that watcher.
const (
	simSlowFloor     = time.Minute     // a test is never called slow under this
	simSlowNoHistory = 5 * time.Minute // threshold for a test with no recorded duration
	simSlowFactor    = 3               // × the last recorded duration
	simSlowestShown  = 10
)

// RunSim runs just the sim suite with the full #2247 diagnostics (per-test
// record, heartbeat, archive, failure and timing summaries, stuck-binary reaper)
// and returns the pre-gate's exit contract. It is `gate sim`, the entry
// scripts/cut-release.sh uses so a release's sim run has the same evidence the
// standalone pre-gate has. extra are forwarded to `go test` (e.g. -run Foo).
func (g *Gate) RunSim(ctx context.Context, extra []string) error {
	g.simAttempt = 0
	g.simExtraArgs = extra
	return g.runPregateStep(ctx, "sim suite", g.runSimSuite,
		"sim suite failed — fix it before continuing (scripts/sim/run.sh --all to reproduce).")
}

// simThreshold is when a test that has run this long is called slow (R2): 3× its
// last recorded duration, at least a minute, or five minutes with no history.
func simThreshold(baseline map[string]float64, test string) (time.Duration, string) {
	if secs, ok := baseline[test]; ok && secs > 0 {
		d := time.Duration(secs * float64(time.Second) * simSlowFactor)
		if d < simSlowFloor {
			d = simSlowFloor
		}
		return d, fmt.Sprintf("%d× its last %s", simSlowFactor, time.Duration(secs*float64(time.Second)).Round(100*time.Millisecond))
	}
	return simSlowNoHistory, "no history"
}

func shortDur(d time.Duration) string {
	if d >= time.Minute {
		return d.Round(time.Second).String()
	}
	return d.Round(100 * time.Millisecond).String()
}

// printSimHeartbeat is R2: what is running, for how long, and a SLOW note for
// any test past its threshold. Advisory only — it never changes the verdict
// (ADR-1676's stall philosophy).
func (g *Gate) printSimHeartbeat(tr *simTracker, baseline map[string]float64) {
	run := tr.runningNow()
	q := tr.queuedCount()
	if len(run) == 0 {
		g.outf("== sim heartbeat: %s into the run, no test running (building, or between packages); %d queued behind -parallel ==\n", shortDur(tr.elapsed()), q)
		return
	}
	g.outf("== sim heartbeat: %s into the run, %d running, %d queued behind -parallel ==\n", shortDur(tr.elapsed()), len(run), q)
	for _, r := range run {
		line := fmt.Sprintf("   running %-8s %s", shortDur(r.Elapsed), r.Test)
		if limit, why := simThreshold(baseline, r.Test); r.Elapsed > limit {
			line += fmt.Sprintf("   SLOW: over %s (%s)", shortDur(limit), why)
		}
		g.outln(line)
	}
}

// printSimFailure is R4: what was still running when the run ended, the ceiling
// message, the dump and the archive.
func (g *Gate) printSimFailure(tr *simTracker, sw *simStreamWriter, a *simArchive, dumpFile string, reaped []int) {
	cls := Classify(tr.snapshot())
	run := tr.runningNow()
	g.outf("== sim suite failed: %d test(s) failed, %d still running when it ended ==\n", len(cls.Fail), len(run))
	if len(cls.Fail) > 0 {
		g.outf("   failed: %s\n", strings.Join(cls.Fail, ", "))
	}
	for _, r := range run {
		g.outf("   still running after %-8s %s\n", shortDur(r.Elapsed), r.Test)
	}
	if c := sw.ceiling(); c != "" {
		g.outf("   %s\n", c)
	}
	if len(reaped) > 0 {
		g.outf("   the gate SIGKILLed test binary pid(s) %v: they outlived their ceiling and did not exit\n", reaped)
	}
	if paths := sw.dumps(); len(paths) > 0 {
		for _, path := range paths {
			g.outf("   goroutine dump: %s\n", path)
		}
	} else if dumpFile != "" {
		// No "written to" line reached us (killed mid-stream): the binaries name
		// their files <dump>.<pid>.<ext>, so look for them beside the base name.
		ext := filepath.Ext(dumpFile)
		matches, _ := filepath.Glob(strings.TrimSuffix(dumpFile, ext) + ".*" + ext)
		for _, m := range matches {
			g.outf("   goroutine dump: %s\n", m)
		}
	}
	if a != nil {
		g.outf("   archive: %s\n", a.pattern())
	}
}

// printSimTimings is R7: the slowest tests of a passing run.
func (g *Gate) printSimTimings(ts []Timing, a *simArchive) {
	if len(ts) > simSlowestShown {
		ts = ts[:simSlowestShown]
	}
	if len(ts) > 0 {
		g.outf("== sim suite: %d slowest tests ==\n%s", len(ts), FormatTimings(ts))
	}
	if a != nil {
		g.outf("== sim suite archive: %s ==\n", a.pattern())
	}
}

// runSimSuite is one attempt of the sim step: `scripts/sim/run.sh --all -json`
// decoded as it streams.
func (g *Gate) runSimSuite(ctx context.Context, sc *crashScanner) Result {
	g.simAttempt++
	arch := g.openSimArchive(ctx, g.simAttempt)
	defer arch.close()
	baseline := g.readSimBaseline()

	tr := newSimTracker(g.Now)
	var raw, rendered io.Writer
	if arch != nil {
		raw, rendered = arch.raw, arch.log
	}
	sw := newSimStreamWriter(tr, sc, g.Out, raw, rendered)

	dumpFile := ""
	env := g.Env
	if arch != nil {
		dumpFile = arch.path("dump.txt")
		env = withEnv(env, testlife.DumpEnv+"="+dumpFile)
	}

	wctx, stop := context.WithCancel(ctx)
	var wg sync.WaitGroup
	if iv := g.Cfg.PregateHeartbeat; iv > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			t := time.NewTicker(iv)
			defer t.Stop()
			for {
				select {
				case <-wctx.Done():
					return
				case <-t.C:
					g.printSimHeartbeat(tr, baseline)
				}
			}
		}()
	}
	reaper := g.startSimReaper(wctx, &wg)

	args := append([]string{"--all", "-json"}, g.simExtraArgs...)
	res := g.Exec.Run(ctx, Cmd{
		Name: g.Cfg.RepoRoot + "/scripts/sim/run.sh", Args: args,
		Dir: g.Cfg.RepoRoot, Env: env, Stdout: sw, Stderr: sw,
		Session: true, Grace: g.Cfg.KillGrace,
		OnStart: reaper.onStart,
	})
	stop()
	wg.Wait()
	sw.Flush()

	events := tr.snapshot()
	timings := Timings(events)
	if arch != nil {
		_ = writeFileAtomic(arch.path("timings.json"), mustJSON(struct {
			ExitCode int      `json:"exit_code"`
			Timings  []Timing `json:"timings"`
			Running  []string `json:"still_running,omitempty"`
		}{res.ExitCode, timings, runningNames(tr.runningNow())}))
	}
	if res.ExitCode == 0 {
		g.recordSimBaseline(baseline, timings)
		g.printSimTimings(timings, arch)
	} else if ctx.Err() == nil {
		g.printSimFailure(tr, sw, arch, dumpFile, reaper.reaped())
	}
	return res
}

func runningNames(rs []simRunning) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.Test)
	}
	return out
}

// simReaperHandle wires the reaper to the started child's PID.
type simReaperHandle struct {
	r      *simReaper
	pidCh  chan int
	active bool
}

func (h *simReaperHandle) onStart(pid int) {
	if h.active {
		h.pidCh <- pid
	}
}

func (h *simReaperHandle) reaped() []int {
	if h.r == nil {
		return nil
	}
	return h.r.killed()
}

// startSimReaper starts the stuck-binary reaper when it is enabled (a positive
// poll interval and a process table to read); otherwise the handle is inert.
func (g *Gate) startSimReaper(ctx context.Context, wg *sync.WaitGroup) *simReaperHandle {
	h := &simReaperHandle{}
	poll := g.Cfg.PregateReapPoll
	if poll <= 0 {
		return h
	}
	var ops *simReaperOps
	if g.simReaperOpsFn != nil {
		ops = g.simReaperOpsFn()
	} else {
		ops = g.simReaperOps()
	}
	if ops == nil {
		return h
	}
	ceiling := testlife.DefaultCeiling
	if v := strings.TrimSpace(g.Getenv(testlife.CeilingEnv)); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			ceiling = d
		}
	}
	h.r = newSimReaper(*ops, g.Now, ceiling+g.Cfg.PregateReapGrace, func(format string, args ...any) { g.errf(format, args...) })
	h.pidCh = make(chan int, 1)
	h.active = true
	wg.Add(1)
	go func() {
		defer wg.Done()
		select {
		case pid := <-h.pidCh:
			h.r.run(ctx, pid, poll)
		case <-ctx.Done():
		}
	}()
	return h
}
