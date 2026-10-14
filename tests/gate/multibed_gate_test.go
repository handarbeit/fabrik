package gate

import (
	"context"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// bedRendezvous makes "both beds ran legs" deterministic (#2284). Fake suite
// commands return instantly, so without it whichever bed reaches the shared
// queue first can drain it before the other takes a cell, and the split depends
// on goroutine scheduling. hook blocks every suite command until suite commands
// have been seen from n distinct FABRIK_TEST_DIR values: a bed stuck in its
// first suite cannot take the next cell, so the other bed must. If the
// scheduler gave every cell to one bed, that bed's first suite would never see
// a second dir and the failsafe records the timeout instead of hanging. Once
// open (or timed out) later calls pass straight through.
type bedRendezvous struct {
	mu       sync.Mutex
	n        int
	dirs     map[string]bool
	both     chan struct{}
	opened   bool
	timedOut bool
}

func newBedRendezvous(n int) *bedRendezvous {
	return &bedRendezvous{n: n, dirs: map[string]bool{}, both: make(chan struct{})}
}

func (r *bedRendezvous) hook(c Cmd) {
	r.mu.Lock()
	r.dirs[envValue(c.Env, "FABRIK_TEST_DIR")] = true
	if !r.opened && len(r.dirs) >= r.n {
		r.opened = true
		close(r.both)
	}
	r.mu.Unlock()
	select {
	case <-r.both:
	case <-time.After(failsafe):
		r.mu.Lock()
		r.timedOut = true
		r.mu.Unlock()
	}
}

// check fails the test if the rendezvous timed out, naming the beds that arrived.
func (r *bedRendezvous) check(t *testing.T) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.timedOut {
		return
	}
	var seen []string
	for d := range r.dirs {
		seen = append(seen, d)
	}
	sort.Strings(seen)
	t.Errorf("rendezvous timed out: suites ran on %v, wanted %d distinct beds running concurrently (one bed took every cell?)", seen, r.n)
}

// Gate.Run end to end over two stubbed beds (#1976): bed A is newRunGate's own
// bed (the default repos and board #2), bed B a second bed with its own token,
// repos and board #3.
func newTwoBedRunGate(t *testing.T, rf *runFake, env ...string) (*Gate, *startWriter, string) {
	t.Helper()
	g, sw := newRunGate(t, rf, env...)
	bedB := t.TempDir()
	mustWrite(t, bedB+"/.env", "FABRIK_TOKEN=token-b\nFABRIK_TEST_REPO_ALPHA=o/alpha-b\nFABRIK_TEST_REPO_BETA=o/beta-b\nFABRIK_TEST_PROJECT_NUMBER=3\n")
	if err := os.MkdirAll(bedB+"/.git", 0o755); err != nil {
		t.Fatal(err)
	}
	g.Cfg.BedDirs = []string{g.Cfg.TestBed, bedB}
	return g, sw, bedB
}

// bedLagFake plays the board-lag probe's gh calls, answering each board by number.
type bedLagFake struct {
	adds []string // "<project>@<token>"
}

func (l *bedLagFake) handle(c Cmd) (Result, bool) {
	if c.Name != "gh" || len(c.Args) < 2 || c.Args[1] != "graphql" {
		return Result{}, false
	}
	q := strings.Join(c.Args, " ")
	switch {
	case strings.Contains(q, "organization(login"):
		for _, n := range []string{"2", "3"} {
			if strings.Contains(q, "projectV2(number:"+n+")") {
				writeStdout(c, "PVT_"+n+"\n")
			}
		}
	case strings.Contains(q, "addProjectV2DraftIssue"):
		for _, n := range []string{"2", "3"} {
			if strings.Contains(q, `projectId:"PVT_`+n+`"`) {
				l.adds = append(l.adds, "PVT_"+n+"@"+envValue(c.Env, "GH_TOKEN"))
				writeStdout(c, "ITEM_PVT_"+n+"\n")
			}
		}
	case strings.Contains(q, "items(first:100, after:$after)"):
		writeStdout(c, `{"data":{"node":{"items":{"pageInfo":{"hasNextPage":false},"nodes":[{"id":"ITEM_PVT_2","content":{"title":"x"}},{"id":"ITEM_PVT_3","content":{"title":"x"}}]}}}}`)
	default:
		return Result{}, false
	}
	return Result{}, true
}

func TestMultiBedRunEndToEnd(t *testing.T) {
	rf := newRunFake()
	g, sw, bedB := newTwoBedRunGate(t, rf)
	lag := &bedLagFake{}
	sw.fakeExec.handler = func(ctx context.Context, c Cmd) Result {
		if res, ok := lag.handle(c); ok {
			return res
		}
		return rf.handle(ctx, c)
	}
	g.LivePreflights = DefaultLivePreflights()
	g.Cfg.LagProbeThreshold, g.Cfg.ProbeInterval, g.Cfg.ProbeWaitMax = 30*time.Second, time.Second, time.Second
	rdv := newBedRendezvous(2)
	rf.suiteHook = rdv.hook

	if code := g.Run(context.Background(), []string{"--clean"}); code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, g.Out, g.Err)
	}
	rdv.check(t)
	out := g.Out.(interface{ String() string }).String()

	// The pre-gate runs once per invocation, never once per bed (D10).
	if n := strings.Count(rf.milestones(), "pregate:sim"); n != 1 {
		t.Errorf("pre-gate runs: %d (%s)", n, rf.milestones())
	}
	// The board-lag probe runs once per bed, on that bed's board, with its token.
	if strings.Join(lag.adds, " ") != "PVT_2@bed-token PVT_3@token-b" {
		t.Errorf("board-lag probes: %v", lag.adds)
	}
	// Each bed is built and started in its own directory.
	if n := strings.Count(rf.milestones(), "bed:build"); n != 2 || len(sw.started) != 2 || sw.started[0].Dir == sw.started[1].Dir {
		t.Errorf("builds=%d starts=%d", n, len(sw.started))
	}
	// --clean resets each bed with its own token.
	resets := map[string]string{}
	for _, c := range sw.fakeExec.callsNamed("gh") {
		if l := argsLine(c); strings.HasPrefix(l, "gh pr list") {
			resets[c.Args[3]] = envValue(c.Env, "GH_TOKEN")
		}
	}
	want := map[string]string{"handarbeit/fabrik-test-alpha": "bed-token", "handarbeit/fabrik-test-beta": "bed-token", "o/alpha-b": "token-b", "o/beta-b": "token-b"}
	for repo, tok := range want {
		if resets[repo] != tok {
			t.Errorf("reset of %s used %q, want %q (all: %v)", repo, resets[repo], tok, resets)
		}
	}
	// Both beds ran legs, each aimed at its own bed.
	beds := map[string]bool{}
	for _, c := range rf.suiteCmds {
		beds[envValue(c.Env, "FABRIK_TEST_DIR")] = true
	}
	if !beds[g.Cfg.TestBed] || !beds[bedB] || len(beds) != 2 {
		t.Errorf("legs ran on %v", beds)
	}
	for _, want := range []string{"== beds (E2E_BEDS): A=", "[bed A] == preflight: bed at", "[bed B] == preflight: bed at", "== bed assignment: every bed", "== multi-bed summary =="} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// D11: the beds must run one engine SHA — refused before any leg.
func TestMultiBedRunRefusesDifferentEngineSHAs(t *testing.T) {
	rf := newRunFake()
	g, sw, bedB := newTwoBedRunGate(t, rf, "E2E_SKIP_PREGATE=1", "E2E_SKIP_PREP=1")
	sw.fakeExec.handler = func(ctx context.Context, c Cmd) Result {
		if c.Name == "git" && c.Dir == bedB && len(c.Args) > 1 && c.Args[0] == "rev-parse" {
			writeStdout(c, "ffffffff"+bedSHA[8:]+"\n")
			return Result{}
		}
		return rf.handle(ctx, c)
	}
	code := g.Run(context.Background(), nil)
	if code != ExitPreconditionFailed {
		t.Fatalf("exit %d", code)
	}
	if errs := g.Err.(interface{ String() string }).String(); !strings.Contains(errs, "different engine SHAs") || !strings.Contains(errs, "bed B at fffffff") {
		t.Errorf("err:\n%s", errs)
	}
	if strings.Contains(rf.milestones(), "leg:") {
		t.Errorf("no leg may run: %s", rf.milestones())
	}
}

// A bed the topology refuses stops the run before the pre-gate spends anything.
func TestMultiBedRunTopologyRefusalSpendsNothing(t *testing.T) {
	rf := newRunFake()
	g, _, bedB := newTwoBedRunGate(t, rf)
	mustWrite(t, bedB+"/.env", "FABRIK_TOKEN=token-b\n") // bed A's default board and repos
	if code := g.Run(context.Background(), nil); code != ExitPreconditionFailed {
		t.Fatalf("exit %d", code)
	}
	if len(rf.order) != 0 {
		t.Errorf("nothing may run: %s", rf.milestones())
	}
}

// R6: one bed keeps the serial scheduler and unprefixed output.
func TestSingleBedRunIsUnchanged(t *testing.T) {
	rf := newRunFake()
	g, _ := newRunGate(t, rf, "E2E_TRAIN_MODE=off", "E2E_SKIP_PREGATE=1")
	g.Cfg.BedDirs = []string{g.Cfg.TestBed}
	if code := g.Run(context.Background(), nil); code != 0 {
		t.Fatalf("exit %d", code)
	}
	out := g.Out.(interface{ String() string }).String() + g.Err.(interface{ String() string }).String()
	for _, absent := range []string{"[bed ", "multi-bed", "bed assignment", "beds (E2E_BEDS)", "identity budget"} {
		if strings.Contains(out, absent) {
			t.Errorf("single-bed output gained %q:\n%s", absent, out)
		}
	}
	if envValue(rf.suiteCmds[0].Env, "FABRIK_TEST_DIR") != "" {
		t.Error("a single-bed leg must not gain bed variables")
	}
}

// R4 end to end: both beds' legs land in one per-SHA ledger, attributed.
func TestMultiBedRunSharesOneLedger(t *testing.T) {
	f := covFixture(t)
	f.g.Env = withoutEnv(f.g.Env, "E2E_TRAIN_MODE") // pat/off and pat/on: one cell per bed
	bedB := t.TempDir()
	mustWrite(t, bedB+"/.env", "FABRIK_TOKEN=token-b\nFABRIK_TEST_PROJECT_NUMBER=3\n")
	f.g.Cfg.BedDirs = []string{f.g.Cfg.TestBed, bedB}
	f.lf.suiteOut = stream(pass("TestAlpha"), pass("TestBravo"))
	rdv := newBedRendezvous(2)
	f.lf.suiteHook = rdv.hook
	if code := f.g.Run(context.Background(), nil); code != 0 {
		t.Fatalf("exit %d\n%s", code, f.g.Err)
	}
	rdv.check(t)
	l := f.ledger(t)
	s := l.Load()
	// Every raw record (not only the latest per pair, which a later cell of the
	// same leg supersedes) carries its bed.
	beds := map[string]bool{}
	for _, leg := range []string{"pat-off", "pat-on"} {
		recs, _, err := readRecords(l.outcomesDir() + "/" + leg + ".jsonl")
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range recs {
			if r.Bed == "" {
				t.Errorf("%s %s has no bed", leg, r.Test)
			}
			beds[r.Bed] = true
		}
	}
	if !beds[f.g.Cfg.TestBed] || !beds[bedB] {
		t.Errorf("records from both beds must land in the one ledger: %v", beds)
	}
	h := f.g.cov.inputs.hashes
	for _, leg := range []string{"pat/off", "pat/on"} {
		if !s.Covered(leg, "TestAlpha", h["TestAlpha"]) {
			t.Errorf("%s TestAlpha not covered", leg)
		}
	}
	if f.ledger(t).InvocationCount() != 1 {
		t.Errorf("two beds are one invocation: %d", f.ledger(t).InvocationCount())
	}
}
