package gate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const covSHA = "cccccccccccccccccccccccccccccccccccccccc"

// covFixture builds a Gate whose RepoRoot holds a tiny tests/e2e package (two
// live tests) and a registry, with the coverage ledger enabled, a fake git, and
// a fake `go test` (legFake). E2E_SKIP_PREP/E2E_SKIP_PREGATE keep the run to the
// legs; auth and train mode are pinned to one cell, pat/off.
type covFix struct {
	g   *Gate
	lf  *legFake
	out *strings.Builder
	dir string // coverage root
}

func covFixture(t *testing.T) *covFix {
	t.Helper()
	lf := &legFake{budgets: nil}
	g, _, _ := newLegGate(t, lf, "E2E_SKIP_PREP=1", "E2E_SKIP_PREGATE=1", "E2E_AUTH_MODE=pat", "E2E_TRAIN_MODE=off")
	g.Preflights = nil
	root := g.Cfg.RepoRoot
	mustWrite(t, root+"/tests/e2e/a_test.go", "package e2e\nimport \"testing\"\nfunc TestAlpha(t *testing.T) { LoadEnv(t) }\nfunc TestBravo(t *testing.T) { LoadEnv(t) }\n")
	mustWrite(t, root+"/tests/e2e/harness.go", "package e2e\nimport \"testing\"\nfunc LoadEnv(t *testing.T) int { return 0 }\n")
	mustWrite(t, root+"/tests/e2e/registry/registry.json", `{"version":1,"tests":[
{"name":"TestAlpha","parity":"gap"},{"name":"TestBravo","parity":"gap"}]}`)
	g.Cfg.CoverageDir = filepath.Join(t.TempDir(), "cov")
	g.Cfg.CoverageKeepSHAs = 5
	g.Cfg.IssueRepo = "o/r"

	fe := g.Exec.(*fakeExec)
	inner := fe.handler
	fe.handler = func(ctx context.Context, c Cmd) Result {
		if c.Name == "git" {
			line := strings.Join(c.Args, " ")
			switch {
			case strings.HasPrefix(line, "rev-parse"):
				writeStdout(c, covSHA+"\n")
			case strings.HasPrefix(line, "-C") || strings.HasPrefix(line, "cat-file"):
			}
			return Result{} // diff / ls-files / status: clean
		}
		return inner(ctx, c)
	}
	return &covFix{g: g, lf: lf, dir: g.Cfg.CoverageDir}
}

func stream(events ...string) string { return strings.Join(events, "\n") + "\n" }

func pass(test string) string {
	return `{"Action":"run","Test":"` + test + `"}` + "\n" + `{"Action":"pass","Test":"` + test + `","Elapsed":1}`
}
func fail(test string) string {
	return `{"Action":"run","Test":"` + test + `"}` + "\n" + `{"Action":"fail","Test":"` + test + `","Elapsed":1}`
}

func (f *covFix) ledger(t *testing.T) *Ledger {
	t.Helper()
	return &Ledger{Root: f.dir, SHA: covSHA, now: time.Now}
}

func lastSuite(f *covFix) Cmd { return f.lf.suiteCmds[len(f.lf.suiteCmds)-1] }

// The acceptance scenario end to end through Gate.run: a partial run, then a
// --resume that runs only what is missing, after which the gate is complete.
func TestRunResumeAddsUpToCompleteCoverage(t *testing.T) {
	f := covFixture(t)
	ctx := context.Background()

	// Invocation 1: Alpha passes, Bravo fails.
	f.lf.suiteOut = stream(pass("TestAlpha"), fail("TestBravo"))
	f.lf.suiteRC = 1
	if code := f.g.Run(ctx, nil); code != 1 {
		t.Fatalf("run 1 exit = %d", code)
	}
	// The phase split names every selected test (#1977) but must not drop any.
	if got := strings.Join(lastSuite(f).Args, " "); !strings.Contains(got, "TestAlpha") || !strings.Contains(got, "TestBravo") {
		t.Errorf("a plain run must not narrow the selection: %s", got)
	}
	l := f.ledger(t)
	if l.InvocationCount() != 1 {
		t.Errorf("invocations = %d, want 1", l.InvocationCount())
	}
	h := f.g.cov.inputs.hashes
	if s := l.Load(); !s.Covered("pat/off", "TestAlpha", h["TestAlpha"]) || s.Covered("pat/off", "TestBravo", h["TestBravo"]) {
		t.Fatalf("after run 1: %+v", s.Latest)
	}

	// Invocation 2: --resume runs ONLY Bravo, and passes.
	f.g.cov = nil
	f.lf.suiteOut = stream(pass("TestBravo"))
	f.lf.suiteRC = 0
	if code := f.g.Run(ctx, []string{"--resume"}); code != 0 {
		t.Fatalf("run 2 exit = %d", code)
	}
	if got := strings.Join(lastSuite(f).Args, " "); !strings.HasSuffix(got, "-run ^(TestBravo)$") {
		t.Errorf("--resume must select exactly the uncovered test: %s", got)
	}
	if l.InvocationCount() != 2 {
		t.Errorf("invocations = %d, want 2", l.InvocationCount())
	}

	// Invocation 3: everything is covered — no leg runs, exit 0, and it is NOT
	// counted as an invocation (it started no leg).
	f.g.cov = nil
	before := len(f.lf.suiteCmds)
	if code := f.g.Run(ctx, []string{"--resume"}); code != 0 {
		t.Fatalf("run 3 exit = %d", code)
	}
	if len(f.lf.suiteCmds) != before || len(f.lf.switchCmds) != 2 {
		t.Errorf("a fully covered resume must run no leg (and no bed restart)")
	}
	if l.InvocationCount() != 2 {
		t.Errorf("a zero-leg resume must not count as an invocation: %d", l.InvocationCount())
	}

	// The read-only acceptance check agrees, and emits the notes line.
	f.g.Out, f.g.Err = &strings.Builder{}, &strings.Builder{}
	var so strings.Builder
	f.g.Out = &so
	if code := f.g.Coverage(ctx, []string{"--sha", covSHA, "--format", "notes"}); code != 0 {
		t.Fatalf("coverage exit = %d", code)
	}
	if !strings.Contains(so.String(), "coverage complete for engine SHA "+covSHA) || !strings.Contains(so.String(), "across 2 gate invocations") {
		t.Errorf("notes line = %q", so.String())
	}
}

func TestResumeExitsIncompleteWhenCallerRunNarrowsTheSelection(t *testing.T) {
	f := covFixture(t)
	f.lf.suiteOut = stream(pass("TestAlpha"))
	code := f.g.Run(context.Background(), []string{"--resume", "-run", "TestAlpha"})
	if code != ExitCoverageIncomplete {
		t.Fatalf("legs passed but Bravo has no coverage: want exit %d, got %d", ExitCoverageIncomplete, code)
	}
	if got := strings.Join(lastSuite(f).Args, " "); !strings.HasSuffix(got, "-run ^(TestAlpha)$") {
		t.Errorf("selection = %s", got)
	}
	s := f.ledger(t).Load()
	if _, ok := s.Record("pat/off", "TestBravo"); ok {
		t.Error("a test the caller's -run excluded must never be recorded")
	}
}

func TestResumeRefusesSubtestFilterAndPlainRunDoesNotRecordIt(t *testing.T) {
	f := covFixture(t)
	if code := f.g.Run(context.Background(), []string{"--resume", "-run", "TestAlpha/case"}); code != ExitUsage {
		t.Errorf("--resume with a subtest filter: want exit %d, got %d", ExitUsage, code)
	}
	f = covFixture(t)
	f.lf.suiteOut = stream(pass("TestAlpha"))
	if code := f.g.Run(context.Background(), []string{"-run", "TestAlpha/case"}); code != 0 {
		t.Fatalf("plain run exit = %d", code)
	}
	if _, ok := f.ledger(t).Load().Record("pat/off", "TestAlpha"); ok {
		t.Error("a subtest-filtered run executes part of a test and must not be credited")
	}
}

func TestPlainRunKeepsItsExitCodeButPrintsTheSummary(t *testing.T) {
	f := covFixture(t)
	var out strings.Builder
	f.g.Out = &out
	f.lf.suiteOut = stream(pass("TestAlpha"))
	if code := f.g.Run(context.Background(), []string{"-run", "TestAlpha"}); code != 0 {
		t.Fatalf("a non-resume run must keep the suite's own exit code, got %d", code)
	}
	for _, want := range []string{"== live coverage for engine SHA", "pat/off", "missing 1", "== coverage INCOMPLETE"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("summary missing %q:\n%s", want, out.String())
		}
	}
}

func TestRunInvalidVoidsTheCellsOutcomes(t *testing.T) {
	f := covFixture(t)
	f.lf.suiteOut = stream(pass("TestAlpha"), pass("TestBravo"))
	mustWrite(t, f.g.Cfg.EngineLog, "activating rate-limit backoff\n")
	if code := f.g.Run(context.Background(), nil); code != ExitBudgetExhausted {
		t.Fatalf("exit = %d, want RUN INVALID", code)
	}
	s := f.ledger(t).Load()
	for _, n := range []string{"TestAlpha", "TestBravo"} {
		if _, ok := s.Record("pat/off", n); ok {
			t.Errorf("%s: a RUN INVALID leg's PASS must not count", n)
		}
	}
}

func TestKilledLegKeepsCompletedTestsInTheLedger(t *testing.T) {
	f := covFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	f.lf.suiteOut = stream(pass("TestAlpha"), `{"Action":"run","Test":"TestBravo"}`)
	f.lf.suiteHook = func(Cmd) { cancel() } // the operator suspends the run mid-leg
	code := f.g.Run(ctx, nil)
	if code != 130 {
		t.Fatalf("exit = %d, want 130", code)
	}
	s := f.ledger(t).Load()
	if !s.Covered("pat/off", "TestAlpha", f.g.cov.inputs.hashes["TestAlpha"]) {
		t.Error("the completed test must be recorded as PASS")
	}
	if _, ok := s.Record("pat/off", "TestBravo"); ok {
		t.Error("the in-flight test must be unrecorded")
	}
}

// Two consecutive legs leave two distinct bed logs in the archive (R7).
func TestConsecutiveLegsArchiveDistinctLogs(t *testing.T) {
	f := covFixture(t)
	g := f.g
	g.LoadAvg = func() (float64, bool) { return 1.5, true }
	g.Cfg.CoverageKeepSHAs = 5
	l, err := OpenLedger(f.dir, covSHA, g.Now)
	if err != nil {
		t.Fatal(err)
	}
	in, err := g.loadCoverageInputs()
	if err != nil {
		t.Fatal(err)
	}
	g.cov = &covState{ledger: l, inputs: in, invocation: "inv1", head: "h", preflight: "preflight text\n"}
	mustWrite(t, g.Cfg.TestBed+"/.fabrik/stages/a.yaml", "name: A\n")
	mustWrite(t, g.Cfg.TestBed+"/.fabrik/config.yaml", "x: 1\n")
	f.lf.suiteOut = stream(pass("TestAlpha"), pass("TestBravo"))

	for i, body := range []string{"first leg engine stdout\n", "second leg engine stdout\n"} {
		mustWrite(t, g.Cfg.TestBed+"/bed-run.log", body) // the harness/engine output of THIS leg
		mustWrite(t, g.Cfg.EngineLog, "engine log of leg "+string(rune('A'+i))+"\n")
		cell := Cell{Auth: "pat", Train: []string{"off", "on"}[i], Parallel: "4"}
		if err := g.RunLeg(context.Background(), cell); err != nil {
			t.Fatal(err)
		}
	}

	first := l.ArchiveCellDir("pat-off", "inv1")
	second := l.ArchiveCellDir("pat-on", "inv1")
	b1, _ := os.ReadFile(first + "/bed-run.log")
	b2, _ := os.ReadFile(second + "/bed-run.log")
	if string(b1) != "first leg engine stdout\n" || string(b2) != "second leg engine stdout\n" {
		t.Fatalf("each leg must keep its own bed log: %q / %q", b1, b2)
	}
	for _, d := range []string{first, second} {
		for _, f := range []string{"go-test.json", "preflight.txt", "load.json", "bed-config.sha256"} {
			if _, err := os.Stat(filepath.Join(d, f)); err != nil {
				t.Errorf("%s: missing %s: %v", d, f, err)
			}
		}
	}
	if pf, _ := os.ReadFile(first + "/preflight.txt"); string(pf) != "preflight text\n" {
		t.Errorf("preflight.txt = %q", pf)
	}
	if ld, _ := os.ReadFile(first + "/load.json"); !strings.Contains(string(ld), `"start_1m": 1.5`) || !strings.Contains(string(ld), `"end_1m": 1.5`) {
		t.Errorf("load.json = %s", ld)
	}
}

func TestBedConfigHashAndDriftWarning(t *testing.T) {
	bed := t.TempDir()
	mustWrite(t, bed+"/.fabrik/stages/a.yaml", "name: A\n")
	mustWrite(t, bed+"/.fabrik/config.yaml", "x: 1\n")
	mustWrite(t, bed+"/.env", "SECRET=1\n")
	h1, err := BedConfigHash(bed)
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, bed+"/.env", "SECRET=2\n")
	if h2, _ := BedConfigHash(bed); h2 != h1 {
		t.Error(".env must never be part of the hash")
	}
	mustWrite(t, bed+"/.fabrik/stages/a.yaml", "name: B\n")
	h3, _ := BedConfigHash(bed)
	if h3 == h1 {
		t.Error("a stage edit must change the hash")
	}
	l := testLedger(t)
	if d := l.NoteBedConfig("i1", h1); len(d) != 0 {
		t.Errorf("first invocation has nothing to differ from: %v", d)
	}
	if d := l.NoteBedConfig("i2", h1); len(d) != 0 {
		t.Errorf("same hash: %v", d)
	}
	if d := l.NoteBedConfig("i3", h3); len(d) != 1 || d[0] != h1 {
		t.Errorf("a changed config must be reported: %v", d)
	}
}

func TestLogArchiverKeepsEverySegmentAcrossEngineRestarts(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(t.TempDir(), "fabrik.log")
	mustWrite(t, src, "previous leg's tail\n")
	a := newLogArchiver(src, dir, 0)
	a.Start()

	appendTo := func(s string) {
		f, _ := os.OpenFile(src, os.O_APPEND|os.O_WRONLY, 0o644)
		f.WriteString(s)
		f.Close()
	}
	// The restart step truncates; the engine then logs its first run.
	mustWrite(t, src, "run1 start banner\n")
	a.Sample()
	appendTo("run1 more\n")
	a.Sample()
	// A mid-leg restart that regrows PAST the old size between two samples — only
	// the changed opening bytes reveal it.
	mustWrite(t, src, "run2 start banner with a very long line, longer than the whole run1 log so far\n")
	a.Sample()
	appendTo("run2 more\n")
	if err := a.Stop(); err != nil {
		t.Fatal(err)
	}

	seg := func(n string) string { b, _ := os.ReadFile(filepath.Join(dir, n)); return string(b) }
	if got := seg("fabrik.log.1"); got != "run1 start banner\nrun1 more\n" {
		t.Errorf("segment 1 = %q", got)
	}
	if got := seg("fabrik.log.2"); got != "run2 start banner with a very long line, longer than the whole run1 log so far\nrun2 more\n" {
		t.Errorf("segment 2 = %q", got)
	}
	if got := seg("fabrik.log.0"); got != "" {
		t.Errorf("the previous leg's tail is not this leg's: %q", got)
	}
}

func TestPruneArchivesRetention(t *testing.T) {
	root := t.TempDir()
	mk := func(sha string, used time.Time) *Ledger {
		l, err := OpenLedger(root, sha, func() time.Time { return used })
		if err != nil {
			t.Fatal(err)
		}
		mustWrite(t, l.ArchiveCellDir("pat-off", "i")+"/go-test.json", "bulky")
		l.Append(rec("TestA", "pat/off", "pat-off", "i", OutcomePass, "h"))
		return l
	}
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	var ls []*Ledger
	for i, sha := range []string{"sha1", "sha2", "sha3", "sha4"} {
		ls = append(ls, mk(sha, base.Add(time.Duration(i)*time.Hour)))
	}
	mustWrite(t, root+"/pregate/head.json", "{}")

	removed := PruneArchives(root, 2, "sha1") // keep the 2 newest, and never the SHA in use
	if strings.Join(removed, ",") != "sha2" {
		t.Fatalf("removed = %v, want only sha2 (sha1 is current, sha3/sha4 are the newest 2)", removed)
	}
	if _, err := os.Stat(ls[1].ArchiveDir()); !os.IsNotExist(err) {
		t.Error("pruned archive must be gone")
	}
	for _, l := range ls {
		if !l.Load().Covered("pat/off", "TestA", "h") {
			t.Errorf("%s: outcome records must survive pruning", l.SHA)
		}
	}
	if _, err := os.Stat(root + "/pregate/head.json"); err != nil {
		t.Error("pregate records must survive pruning")
	}
}

func TestLoadAvgParsers(t *testing.T) {
	if v, ok := parseProcLoadavg("0.52 0.58 0.59 1/466 12345\n"); !ok || v != 0.52 {
		t.Errorf("proc: %v %v", v, ok)
	}
	for _, bad := range []string{"", "x y z", "-1 0 0"} {
		if _, ok := parseProcLoadavg(bad); ok {
			t.Errorf("parseProcLoadavg(%q) must be unavailable", bad)
		}
	}
	b := make([]byte, 24)
	b[1] = 0x0c  // ldavg[0] = 3072
	b[17] = 0x08 // fscale = 2048 -> 3072/2048 = 1.5
	if v, ok := parseSysctlLoadavg(b); !ok || v != 1.5 {
		t.Errorf("sysctl: %v %v", v, ok)
	}
	if _, ok := parseSysctlLoadavg(b[:10]); ok {
		t.Error("a short sysctl blob is unavailable")
	}
	var zero [24]byte
	if _, ok := parseSysctlLoadavg(zero[:]); ok {
		t.Error("fscale 0 is unavailable")
	}
	// An unavailable probe must not fail a leg.
	f := covFixture(t)
	f.g.LoadAvg = func() (float64, bool) { return 0, false }
	f.lf.suiteOut = stream(pass("TestAlpha"), pass("TestBravo"))
	if code := f.g.Run(context.Background(), nil); code != 0 {
		t.Fatalf("exit = %d", code)
	}
}

func TestCoverageCommandExitCodesAndUsage(t *testing.T) {
	f := covFixture(t)
	g := f.g
	var out, errb strings.Builder
	g.Out, g.Err = &out, &errb
	ctx := context.Background()

	if code := g.Coverage(ctx, []string{"--sha", covSHA}); code != ExitCoverageIncomplete {
		t.Errorf("an empty ledger is incomplete: %d", code)
	}
	if _, err := os.Stat(filepath.Join(f.dir, covSHA)); !os.IsNotExist(err) {
		t.Error("the coverage check is read-only: it must not create a ledger directory")
	}
	if !strings.Contains(out.String(), "INCOMPLETE") || !strings.Contains(out.String(), "TestAlpha: no record") {
		t.Errorf("output:\n%s", out.String())
	}
	for _, bad := range [][]string{{"--bogus"}, {"--format", "xml"}} {
		if code := g.Coverage(ctx, bad); code != ExitUsage {
			t.Errorf("%v: want usage exit, got %d", bad, code)
		}
	}
	g.Cfg.CoverageDir = ""
	if code := g.Coverage(ctx, nil); code != ExitUsage {
		t.Errorf("disabled ledger: %d", code)
	}
}

func TestCoverageNotAcceptedWhenEngineDrifts(t *testing.T) {
	f := covFixture(t)
	g := f.g
	l, _ := OpenLedger(f.dir, covSHA, g.Now)
	in, _ := g.loadCoverageInputs()
	l.Append(Record{Test: "TestAlpha", Leg: "pat/off", Cell: "pat-off", Invocation: "i", Outcome: OutcomePass, Hash: in.hashes["TestAlpha"]})
	l.Append(Record{Test: "TestBravo", Leg: "pat/off", Cell: "pat-off", Invocation: "i", Outcome: OutcomePass, Hash: in.hashes["TestBravo"]})
	var out strings.Builder
	g.Out, g.Err = &out, &strings.Builder{}
	if code := g.Coverage(context.Background(), []string{"--sha", covSHA}); code != 0 {
		t.Fatalf("complete ledger must be accepted: %d\n%s", code, out.String())
	}

	// An engine-side change since the engine SHA invalidates it (R2).
	fe := g.Exec.(*fakeExec)
	inner := fe.handler
	fe.handler = func(ctx context.Context, c Cmd) Result {
		if c.Name == "git" && len(c.Args) > 1 && c.Args[0] == "diff" {
			writeStdout(c, "engine/poll.go\n")
			return Result{}
		}
		return inner(ctx, c)
	}
	out.Reset()
	if code := g.Coverage(context.Background(), []string{"--sha", covSHA}); code != ExitCoverageIncomplete || !strings.Contains(out.String(), "engine/poll.go") {
		t.Errorf("engine drift must not be accepted: %d\n%s", code, out.String())
	}
}

func TestReportFormatAndNotesLine(t *testing.T) {
	r := &Report{SHA: covSHA, Invocations: 1, Legs: []LegReport{{Leg: "app/off", Pairs: []Pair{
		{Test: "TestA", Leg: "app/off", Status: PairCovered},
		{Test: "TestB", Leg: "app/off", Status: PairKnownSkip, Detail: "blocked on open #916"},
		{Test: "TestC", Leg: "app/off", Status: PairStructural, Detail: "registry skip_ok_legs"},
	}}}}
	if !r.Complete() {
		t.Fatal("covered + accepted skips is complete")
	}
	out := r.Format()
	for _, want := range []string{"app/off", "covered 1/3", "known-skip 1", "structural-skip 1", "COMPLETE", "known-skip       TestB: blocked on open #916"} {
		if !strings.Contains(out, want) {
			t.Errorf("Format missing %q:\n%s", want, out)
		}
	}
	if n := r.NotesLine(); !strings.Contains(n, "across 1 gate invocation.") || !strings.Contains(n, "1 known skip(s)") {
		t.Errorf("notes line = %q", n)
	}
	r.Legs[0].Pairs[0].Status = PairInconclusive
	if r.Complete() || !strings.Contains(r.Format(), "INCOMPLETE") {
		t.Error("an INCONCLUSIVE pair must block the gate")
	}
	if (&Report{}).Complete() {
		t.Error("a report with no legs is never complete")
	}
	bad := &Report{Legs: r.Legs[:0], Drift: &Drift{Valid: false}}
	if bad.Complete() {
		t.Error("invalid drift is never complete")
	}
}
