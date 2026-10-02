package gate

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Environment probes (#1974, ADR-1974): two checks that run before live budget is
// spent, because the two environmental causes of the 0.0.83 gate's flakes were
// invisible to the run that suffered them.
//
//   - Host load. Loads of 15–40 (other projects' CI runners, and orphaned
//     sim.test trees left behind by killed dev-daemon workers, #1989) produced
//     sim timeouts and -race SIGSEGVs. The load probe reports the 1-minute load
//     average against a threshold, lists orphaned test processes with their
//     working directories so the operator can tell which tree they came from, and
//     waits and re-probes while the load is high.
//   - Board-listing lag. GitHub's ProjectV2 item listing lagged adds by minutes
//     during the 2026-09-30 partial outage, and the harness could not tell "not
//     visible yet" from "the engine did something wrong". The lag probe adds a
//     temporary DRAFT item to the bed's own board, times it until it appears in
//     the listing, and always removes it. It waits and re-probes while the lag is
//     above the threshold.
//
// Both loops are bounded; if the condition still holds at the bound, preflight
// fails with ExitPreflightFailed and names the probe, rather than starting a run
// that is likely to be flaky. A probe that cannot take a reading reports
// "unavailable" and never blocks. THE PROBES NEVER KILL ANYTHING: they report and
// the operator decides. (probes_test.go pins this statically.)
//
// The results are recorded on the Gate and archived beside load.json as
// probes.json in every leg's archive directory (#1972).

// Probe outcomes (the Status of a probe result).
const (
	ProbeOK          = "ok"          // within the threshold at the first reading
	ProbeRecovered   = "recovered"   // above the threshold, then within it after waiting
	ProbeExceeded    = "exceeded"    // still above the threshold at the wait bound
	ProbeUnavailable = "unavailable" // no reading could be taken; never blocks
	ProbeCancelled   = "cancelled"   // the run was cancelled while waiting
	ProbeSkipped     = "skipped"     // E2E_SKIP_PROBES
)

// ProbeReport is what both probes measured, archived as probes.json.
type ProbeReport struct {
	Load *LoadProbeResult `json:"load,omitempty"`
	Lag  *LagProbeResult  `json:"lag,omitempty"`
}

func (r ProbeReport) empty() bool { return r.Load == nil && r.Lag == nil }

// LoadProbeResult is the host-load probe's record.
type LoadProbeResult struct {
	Status        string    `json:"status"`
	Readings      []float64 `json:"readings_1m,omitempty"`
	Threshold     float64   `json:"threshold,omitempty"`
	CPUs          int       `json:"cpus,omitempty"`
	Attempts      int       `json:"attempts"`
	WaitedSeconds float64   `json:"waited_seconds"`
	Orphans       []Orphan  `json:"orphans,omitempty"`
	Note          string    `json:"note,omitempty"`
}

// LagProbeResult is the board-listing lag probe's record.
type LagProbeResult struct {
	Status           string    `json:"status"`
	ReadingsSeconds  []float64 `json:"readings_seconds,omitempty"`
	ThresholdSeconds float64   `json:"threshold_seconds,omitempty"`
	Attempts         int       `json:"attempts"`
	WaitedSeconds    float64   `json:"waited_seconds"`
	SweptLeftovers   int       `json:"swept_leftovers,omitempty"`
	Note             string    `json:"note,omitempty"`
}

// Orphan is a test process re-parented to init, i.e. whose parent (a killed
// dev-daemon worker, usually) is gone. Cwd is "unknown" when it could not be
// resolved: that is reported, never treated as an error.
type Orphan struct {
	PID  int    `json:"pid"`
	Name string `json:"name"`
	Cwd  string `json:"cwd"`
}

// orphanNames are the test binaries whose orphaned trees loaded the 0.0.83 host.
var orphanNames = map[string]bool{"sim.test": true, "e2e.test": true, "fabrik.test": true}

// parseOrphans reads `ps -axo pid=,ppid=,comm=` output and returns the processes
// whose parent is init (PID 1) and whose command's base name is one of
// orphanNames. comm may be a full path (macOS) or truncated to 15 characters
// (Linux); both are best-effort. Linux orphans can re-parent to a per-user
// subreaper rather than PID 1 and are then not listed — the listing is
// informational and never gates.
func parseOrphans(ps string) []Orphan {
	var out []Orphan
	for _, line := range strings.Split(ps, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil || ppid != 1 {
			continue
		}
		name := filepath.Base(strings.Join(f[2:], " "))
		if orphanNames[name] {
			out = append(out, Orphan{PID: pid, Name: name})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out
}

// probesDisabled reports E2E_SKIP_PROBES (the same escape-hatch convention as the
// other E2E_SKIP_* knobs) and records it in the report.
func (g *Gate) probesSkipped() bool { return g.Getenv("E2E_SKIP_PROBES") != "" }

// numCPU is a test seam for the load threshold.
func (g *Gate) numCPU() int {
	if g.CPUs > 0 {
		return g.CPUs
	}
	return runtime.NumCPU()
}

// reprobe is the bounded wait-and-re-probe loop both probes share. read takes one
// reading (ok=false: no reading could be taken). The loop returns as soon as a
// reading is at or below limit; otherwise it pauses ProbeInterval and reads again
// until ProbeWaitMax of pauses has been spent. The bound is counted in attempts,
// not wall-clock, so it is deterministic under a stubbed Sleep.
func (g *Gate) reprobe(ctx context.Context, name string, limit float64, unit string, read func(context.Context) (float64, bool)) (readings []float64, status string, waited time.Duration) {
	interval := g.Cfg.ProbeInterval
	maxPauses := 0
	if interval > 0 {
		maxPauses = int(g.Cfg.ProbeWaitMax / interval)
	}
	for attempt := 0; ; attempt++ {
		v, ok := read(ctx)
		if !ok {
			return readings, ProbeUnavailable, waited
		}
		readings = append(readings, v)
		if v <= limit {
			if attempt == 0 {
				return readings, ProbeOK, waited
			}
			return readings, ProbeRecovered, waited
		}
		if attempt >= maxPauses {
			return readings, ProbeExceeded, waited
		}
		g.outf("== %s probe: %.1f%s is above the %.1f%s threshold — waiting %s, then re-probing (%d of %d) ==\n",
			name, v, unit, limit, unit, interval, attempt+1, maxPauses)
		if ctx.Err() != nil {
			return readings, ProbeCancelled, waited
		}
		g.Sleep(interval)
		waited += interval
	}
}

// loadThreshold is the 1-minute load average above which the host is too busy:
// the absolute override when set, else factor × CPU count. 0 = the probe is off.
func (g *Gate) loadThreshold() (threshold float64, cpus int) {
	cpus = g.numCPU()
	if g.Cfg.LoadProbeThreshold > 0 {
		return g.Cfg.LoadProbeThreshold, cpus
	}
	return g.Cfg.LoadProbeFactor * float64(cpus), cpus
}

// ProbeHostLoad is the host-load probe (a Preflight: host-only, no live spend, so
// it runs before the pre-gate it protects). It reuses Gate.loadAvg — the reading
// the leg archive already takes — rather than a second parser.
func (g *Gate) ProbeHostLoad(ctx context.Context) error {
	if g.probesSkipped() {
		g.outln("== host-load probe skipped (E2E_SKIP_PROBES set) ==")
		g.probes.Load = &LoadProbeResult{Status: ProbeSkipped}
		return nil
	}
	threshold, cpus := g.loadThreshold()
	if threshold <= 0 {
		return nil // disabled
	}
	res := &LoadProbeResult{Threshold: threshold, CPUs: cpus}
	g.probes.Load = res

	g.outf("== probing host load (1-minute average; threshold %.1f = %d CPUs × %.1f) ==\n", threshold, cpus, threshold/float64(cpus))
	readings, status, waited := g.reprobe(ctx, "load", threshold, "", func(context.Context) (float64, bool) { return g.loadAvg() })
	res.Readings, res.Status, res.WaitedSeconds = readings, status, waited.Seconds()
	res.Attempts = len(readings)
	if status == ProbeUnavailable && len(readings) == 0 {
		res.Attempts = 1
	}

	res.Orphans = g.listOrphans(ctx)

	switch status {
	case ProbeUnavailable:
		g.outln("   load average unavailable on this host — not blocking")
	case ProbeOK, ProbeRecovered:
		g.outf("   load %.2f is within the threshold (%s)\n", readings[len(readings)-1], status)
	}
	g.printOrphans(res.Orphans)

	switch status {
	case ProbeExceeded:
		res.Note = "load still above the threshold at the wait bound"
		return exitErr(ExitPreflightFailed, "preflight: host-load probe failed — 1-minute load %.1f is still above the %.1f threshold after waiting %s (%d reading(s)). "+
			"Another workload is competing for this host; wait for it to subside, or raise E2E_LOAD_PROBE_THRESHOLD / E2E_LOAD_PROBE_FACTOR (or set E2E_SKIP_PROBES=1) to proceed anyway. The probe never kills anything — orphaned processes are listed above for you to deal with.",
			readings[len(readings)-1], threshold, waited, len(readings))
	case ProbeCancelled:
		return ctx.Err()
	}
	return nil
}

// listOrphans lists orphaned test processes (ppid 1) with their working
// directories. Best-effort per OS: a failed `ps` yields no list, and a cwd that
// cannot be resolved is "unknown".
func (g *Gate) listOrphans(ctx context.Context) []Orphan {
	if runtime.GOOS == "windows" {
		return nil
	}
	so, _, res := output(ctx, g.Exec, Cmd{Name: "ps", Args: []string{"-axo", "pid=,ppid=,comm="}, Env: g.Env})
	if res.Err != nil || res.ExitCode != 0 {
		return nil
	}
	orphans := parseOrphans(so)
	for i := range orphans {
		if g.ProcCwd != nil {
			orphans[i].Cwd = g.ProcCwd(ctx, orphans[i].PID)
		}
		if orphans[i].Cwd == "" {
			orphans[i].Cwd = "unknown"
		}
	}
	return orphans
}

func (g *Gate) printOrphans(orphans []Orphan) {
	if len(orphans) == 0 {
		g.outln("   no orphaned sim.test / e2e.test / fabrik.test processes (ppid 1)")
		return
	}
	g.errf("warning: %d orphaned test process(es) (parent gone, ppid 1) — likely left behind by a killed worker; they load the host. NOT killed; deal with them yourself:\n", len(orphans))
	for _, o := range orphans {
		g.errf("   pid %d   %s   cwd %s\n", o.PID, o.Name, o.Cwd)
	}
}

// ---------------------------------------------------------------------------
// Board-listing lag probe

// lagProbeTitlePrefix marks the probe's draft items so a leftover from a run
// killed mid-probe can be recognised and swept at the start of the next one.
const lagProbeTitlePrefix = "e2e-lag-probe-"

// ProbeBoardLag is the board-listing lag probe. It is a LIVE GitHub write, so it
// is not a Preflight (those run before the pre-gate, which must spend nothing live,
// ADR-1454): it runs after the pre-gate and before the bed is prepared.
//
// The subject is a DRAFT project item — self-cleaning, creating no repository
// issue and so never tripping the bed's stale-Queued-member checks — added to the
// bed's own board and removed again whatever happens (success, timeout, error,
// cancellation).
func (g *Gate) ProbeBoardLag(ctx context.Context) error {
	if g.probesSkipped() {
		g.outln("== board-lag probe skipped (E2E_SKIP_PROBES set) ==")
		g.probes.Lag = &LagProbeResult{Status: ProbeSkipped}
		return nil
	}
	threshold := g.Cfg.LagProbeThreshold
	if threshold <= 0 {
		return nil // disabled
	}
	res := &LagProbeResult{ThresholdSeconds: threshold.Seconds()}
	g.probes.Lag = res

	token := EnvFileValue(filepath.Join(g.Cfg.TestBed, ".env"), "FABRIK_TOKEN")
	if token == "" {
		res.Status, res.Note = ProbeUnavailable, "no bed token (FABRIK_TOKEN in the bed's .env)"
		g.outln("== board-lag probe unavailable: no bed token — not blocking ==")
		return nil
	}
	rc := g.resetConfig()
	g.outf("== probing board-listing lag (threshold %s; draft item on %s/#%s) ==\n", threshold, rc.ProjectOwner, rc.ProjectNumber)

	pid := g.resolveProjectNodeID(ctx, token, rc)
	if pid == "" {
		res.Status, res.Note = ProbeUnavailable, "could not resolve the project"
		g.outf("   could not resolve project %s/#%s — not blocking\n", rc.ProjectOwner, rc.ProjectNumber)
		return nil
	}
	board := &lagBoard{g: g, token: token, projectID: pid}
	res.SweptLeftovers = board.sweepLeftovers(ctx)
	if res.SweptLeftovers > 0 {
		g.outf("   removed %d leftover probe item(s) from an earlier interrupted run\n", res.SweptLeftovers)
	}

	readings, status, waited := g.reprobe(ctx, "board-lag", threshold.Seconds(), "s", func(ctx context.Context) (float64, bool) {
		lag, ok := board.measure(ctx, threshold)
		return lag.Seconds(), ok
	})
	res.ReadingsSeconds, res.Status, res.WaitedSeconds = readings, status, waited.Seconds()
	res.Attempts = len(readings)
	if status == ProbeUnavailable && len(readings) == 0 {
		res.Attempts = 1
	}
	switch status {
	case ProbeUnavailable:
		res.Note = "could not add or list a draft item"
		g.outln("   could not take a reading (add or list failed) — not blocking")
	case ProbeOK, ProbeRecovered:
		g.outf("   board-listing lag %.1fs is within the threshold (%s)\n", readings[len(readings)-1], status)
	case ProbeExceeded:
		res.Note = "listing lag still above the threshold at the wait bound"
		return exitErr(ExitPreflightFailed, "preflight: board-lag probe failed — a project item added just now was still not in the board listing after %s, and this persisted for %s of waiting (%d measurement(s)). "+
			"GitHub's ProjectV2 listing is lagging (the 2026-09-30 outage shape): a run now would be flaky. Wait and retry, or raise E2E_LAG_PROBE_THRESHOLD (or set E2E_SKIP_PROBES=1) to proceed anyway.",
			threshold, waited, len(readings))
	case ProbeCancelled:
		return ctx.Err()
	}
	return nil
}

// lagBoard is the lag probe's view of the bed's board. Every call goes through
// the Commander with Session and a Timeout (budget.go's required routing point
// for any network call the gate adds) and GH_TOKEN scoped to the bed's own PAT.
type lagBoard struct {
	g         *Gate
	token     string
	projectID string
}

func (b *lagBoard) gh(ctx context.Context, args ...string) (string, bool) {
	so, _, res := output(ctx, b.g.Exec, Cmd{
		Name: "gh", Args: args, Env: withEnv(b.g.Env, "GH_TOKEN="+b.token),
		Session: true, Timeout: b.g.Cfg.GHAPITimeout, Grace: b.g.Cfg.KillGrace,
	})
	return so, res.ExitCode == 0 && res.Err == nil && !res.TimedOut
}

const lagListingQuery = `query($id:ID!,$after:String){ node(id:$id){ ... on ProjectV2 { items(first:100, after:$after){
  pageInfo{hasNextPage endCursor} nodes{ id content{ __typename ... on DraftIssue{ title } } } } } } }`

type lagItem struct{ ID, Title string }

// list reads the project's item listing through the same items(first:100)
// connection the engine and the harness list, following pagination.
func (b *lagBoard) list(ctx context.Context) ([]lagItem, bool) {
	var items []lagItem
	after := ""
	for page := 0; page < 20; page++ {
		args := []string{"api", "graphql", "-f", "query=" + lagListingQuery, "-f", "id=" + b.projectID}
		if after != "" {
			args = append(args, "-f", "after="+after)
		}
		so, ok := b.gh(ctx, args...)
		if !ok {
			return nil, false
		}
		var resp struct {
			Data struct {
				Node struct {
					Items struct {
						PageInfo struct {
							HasNextPage bool   `json:"hasNextPage"`
							EndCursor   string `json:"endCursor"`
						} `json:"pageInfo"`
						Nodes []struct {
							ID      string `json:"id"`
							Content struct {
								Title string `json:"title"`
							} `json:"content"`
						} `json:"nodes"`
					} `json:"items"`
				} `json:"node"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(so), &resp); err != nil {
			return nil, false
		}
		for _, n := range resp.Data.Node.Items.Nodes {
			items = append(items, lagItem{ID: n.ID, Title: n.Content.Title})
		}
		if !resp.Data.Node.Items.PageInfo.HasNextPage {
			return items, true
		}
		after = resp.Data.Node.Items.PageInfo.EndCursor
	}
	return items, true
}

func (b *lagBoard) add(ctx context.Context, title string) (string, bool) {
	q := fmt.Sprintf(`mutation { addProjectV2DraftIssue(input:{projectId:%q, title:%q}){ projectItem{ id } } }`, b.projectID, title)
	so, ok := b.gh(ctx, "api", "graphql", "-f", "query="+q, "--jq", ".data.addProjectV2DraftIssue.projectItem.id")
	id := strings.TrimSpace(so)
	if !ok || id == "" || id == "null" {
		return "", false
	}
	return id, true
}

func (b *lagBoard) remove(ctx context.Context, itemID string) bool {
	q := fmt.Sprintf(`mutation { deleteProjectV2Item(input:{projectId:%q, itemId:%q}){ deletedItemId } }`, b.projectID, itemID)
	_, ok := b.gh(ctx, "api", "graphql", "-f", "query="+q)
	return ok
}

// sweepLeftovers removes probe items an earlier, killed run left behind
// (identified by the title prefix), so a SIGKILL mid-probe leaves nothing the
// next run's stale-artifact checks could trip on. Returns how many were removed.
func (b *lagBoard) sweepLeftovers(ctx context.Context) int {
	items, ok := b.list(ctx)
	if !ok {
		return 0
	}
	n := 0
	for _, it := range items {
		if strings.HasPrefix(it.Title, lagProbeTitlePrefix) && b.remove(ctx, it.ID) {
			n++
		}
	}
	return n
}

// measure adds one draft item and times it until it appears in the listing, or
// until `limit` has elapsed without it appearing (the returned lag is then the
// time given up at, always strictly above the limit). ok=false: the add or every listing
// read failed, so no reading could be taken. The item is ALWAYS removed — on a
// bounded context detached from ctx's cancellation, so an interrupted gate still
// cleans up.
func (b *lagBoard) measure(ctx context.Context, limit time.Duration) (lag time.Duration, ok bool) {
	g := b.g
	title := fmt.Sprintf("%s%d", lagProbeTitlePrefix, g.Now().UnixNano())
	itemID, added := b.add(ctx, title)
	if !added {
		return 0, false
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*g.Cfg.GHAPITimeout+10*time.Second)
		defer cancel()
		if !b.remove(cctx, itemID) {
			g.errf("warning: could not remove the board-lag probe item %s (title %s) — the next run sweeps it, or run reset --clean\n", itemID, title)
		}
	}()

	start := g.Now()
	listed := false
	for {
		items, readOK := b.list(ctx)
		if readOK {
			listed = true
			for _, it := range items {
				if it.ID == itemID {
					return g.Now().Sub(start), true
				}
			}
		}
		elapsed := g.Now().Sub(start)
		if elapsed >= limit || ctx.Err() != nil {
			if !listed {
				return 0, false
			}
			// Not visible within the limit is, by definition, a lag ABOVE it, even
			// when the give-up instant lands exactly on it.
			if elapsed <= limit {
				elapsed = limit + time.Nanosecond
			}
			return elapsed, true
		}
		wait := g.Cfg.LagPollInterval
		if wait <= 0 {
			wait = 3 * time.Second
		}
		g.Sleep(wait)
	}
}

// ---------------------------------------------------------------------------
// Archive

// writeProbes writes probes.json beside load.json. Nothing is written when no
// probe ran or was skipped.
func (g *Gate) writeProbes(dir string) {
	if g.probes.empty() {
		return
	}
	if err := os.WriteFile(filepath.Join(dir, "probes.json"), mustJSON(g.probes), 0o644); err != nil {
		g.errf("warning: archiving probe results: %v\n", err)
	}
}
