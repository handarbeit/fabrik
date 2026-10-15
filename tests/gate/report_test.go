package gate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/handarbeit/fabrik/tests/e2e/registry"
)

func writeStream(t *testing.T, archive, cell, inv, body string) {
	t.Helper()
	dir := filepath.Join(archive, cell, inv)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go-test.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func reportEv(action, test string, elapsed float64) string {
	return fmt.Sprintf(`{"Action":%q,"Package":"p","Test":%q,"Elapsed":%v}`+"\n", action, test, elapsed)
}

func testRegistry() *registry.Registry {
	return &registry.Registry{Version: registry.Version, Tests: []registry.Entry{
		{Name: "TestFull", EntryStage: registry.StageSpecify, Traversal: registry.TraversalSubject, FullTraversal: true},
		{Name: "TestSeeded", EntryStage: registry.StageValidate, Traversal: registry.TraversalNone},
		{Name: "TestNeverRan", EntryStage: registry.StageImplement, Traversal: registry.TraversalNone},
	}}
}

func TestBuildRuntimeReport(t *testing.T) {
	archive := t.TempDir()
	// app-on: an interrupted first invocation, superseded by a later resume pass for TestSeeded.
	writeStream(t, archive, "app-on", "20260101T000000Z", reportEv("pass", "TestFull", 600)+reportEv("pass", "TestSeeded", 900)+reportEv("pass", "TestSeeded/sub", 5))
	writeStream(t, archive, "app-on", "20260102T000000Z", reportEv("pass", "TestSeeded", 120))
	writeStream(t, archive, "pat-off", "20260101T000000Z", reportEv("pass", "TestFull", 300)+reportEv("skip", "TestSeeded", 0))

	rep, err := BuildRuntimeReport("abcdef0123456789abcdef0123456789abcdef01", archive, testRegistry())
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]RuntimeRow{}
	for _, r := range rep.Rows {
		byName[r.Test] = r
	}
	if got := byName["TestFull"].Total; got != 900 {
		t.Fatalf("TestFull total = %v, want 600+300 across cells", got)
	}
	if got := byName["TestSeeded"].Seconds["app-on"]; got != 120 {
		t.Fatalf("TestSeeded app-on = %v, want the later invocation's 120 (a resume pass supersedes)", got)
	}
	if _, ok := byName["TestSeeded"].Seconds["pat-off"]; ok {
		t.Fatal("a skipped test ran nothing and must not be timed")
	}
	if rep.Total != 1020 {
		t.Fatalf("suite total = %v, want 1020", rep.Total)
	}
	if len(rep.Unmeasured) != 1 || rep.Unmeasured[0] != "TestNeverRan" {
		t.Fatalf("unmeasured = %v", rep.Unmeasured)
	}
	// Proxy: TestSeeded (Validate = 5 stages skipped) ran in one cell.
	if rep.StagesSkipped != 5 {
		t.Fatalf("stages skipped = %d, want 5", rep.StagesSkipped)
	}
	if rep.Rows[0].Test != "TestFull" {
		t.Fatalf("rows are not slowest-first: %v", rep.Rows)
	}
	out := rep.Format()
	for _, want := range []string{"[full traversal]", "model quota: not recorded", "TestNeverRan", "1 registry test(s) have no archived timing"} {
		if !strings.Contains(out, want) {
			t.Errorf("Format() missing %q:\n%s", want, out)
		}
	}
}

func TestBuildRuntimeReportMissingArchiveIsEmptyNotAnError(t *testing.T) {
	rep, err := BuildRuntimeReport("s", filepath.Join(t.TempDir(), "absent"), testRegistry())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Total != 0 || len(rep.Unmeasured) != 3 {
		t.Fatalf("report = %+v", rep)
	}
}

func TestFormatComparison(t *testing.T) {
	before := RuntimeReport{SHA: "b" + strings.Repeat("0", 39), StagesSkipped: 0, Total: 5400, Rows: []RuntimeRow{
		{Test: "TestSeeded", Entry: registry.StageSpecify, Total: 5400},
	}}
	after := RuntimeReport{SHA: "a" + strings.Repeat("0", 39), StagesSkipped: 5, Total: 900, Rows: []RuntimeRow{
		{Test: "TestSeeded", Entry: registry.StageValidate, Total: 900},
	}}
	out := FormatComparison(before, after)
	for _, want := range []string{"Specify    -> Validate", "-4500", "5400s -> 900s", "-75.0 min", "0 -> 5"} {
		if !strings.Contains(out, want) {
			t.Errorf("comparison missing %q:\n%s", want, out)
		}
	}
}

func TestReportCmdAgainstAFakeArchive(t *testing.T) {
	root := t.TempDir()
	sha := strings.Repeat("c", 40)
	l := &Ledger{Root: root, SHA: sha}
	writeStream(t, l.ArchiveDir(), "app-on", "20260101T000000Z", reportEv("pass", "TestSmokeSingleRepoFullPipeline", 1200))

	var out, errb strings.Builder
	g := NewGate(Config{CoverageDir: root}, &out, &errb)
	if code := g.ReportCmd(t.Context(), []string{"--sha", sha}); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "TestSmokeSingleRepoFullPipeline") || !strings.Contains(out.String(), "[full traversal]") {
		t.Fatalf("report did not name the full-traversal smoke test:\n%s", out.String())
	}
	out.Reset()
	if code := g.ReportCmd(t.Context(), []string{"--sha", strings.Repeat("d", 40)}); code == 0 {
		t.Fatal("a SHA with no archive must not report success")
	}
	if code := g.ReportCmd(t.Context(), []string{"--format", "yaml"}); code != ExitUsage {
		t.Fatalf("bad --format exit = %d", code)
	}
	if code := g.ReportCmd(t.Context(), []string{"--bogus"}); code != ExitUsage {
		t.Fatalf("unknown flag exit = %d", code)
	}
}

// A baseline from before #1992 has a registry with no entry stages. Its archived timings
// must still be reported (under no entry), and the comparison must not claim a
// stages-not-driven figure for it — nor borrow today's entry stages for it.
func TestBaselineWithoutEntryStagesKeepsItsTimingsAndOmitsTheProxy(t *testing.T) {
	archive := t.TempDir()
	writeStream(t, archive, "app-on", "20260101T000000Z", reportEv("pass", "TestSeeded", 5400))
	old := &registry.Registry{Version: registry.Version, Tests: []registry.Entry{{Name: "TestSeeded"}}}
	before, err := BuildRuntimeReport("b", archive, old)
	if err != nil {
		t.Fatal(err)
	}
	if before.Total != 5400 || before.hasEntries() {
		t.Fatalf("before = %+v, want its 5400s kept and no entry stages", before)
	}
	// A test archived but absent from the registry keeps its time too.
	orphan, err := BuildRuntimeReport("b", archive, &registry.Registry{Version: registry.Version})
	if err != nil || orphan.Total != 5400 {
		t.Fatalf("archived test missing from the registry lost its time: %+v, %v", orphan, err)
	}
	after := RuntimeReport{SHA: "a", StagesSkipped: 5, Total: 900, Rows: []RuntimeRow{{Test: "TestSeeded", Entry: registry.StageValidate, Total: 900}}}
	out := FormatComparison(before, after)
	for _, want := range []string{"-          -> Validate", "5400s -> 900s", "not recorded for the baseline"} {
		if !strings.Contains(out, want) {
			t.Errorf("comparison missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "0 -> 5") {
		t.Errorf("comparison invented a baseline proxy of 0:\n%s", out)
	}
}

// An INCONCLUSIVE test is retried into go-test.retry-N.json next to go-test.json; the gate
// counts the last attempt per test, so the report must too — otherwise a test that skipped
// as inconclusive and then ran in the retry reads as unmeasured and skews the before/after.
func TestBuildRuntimeReportMergesInconclusiveRetries(t *testing.T) {
	archive := t.TempDir()
	inconclusive := `{"Action":"skip","Package":"p","Test":"TestSeeded","Elapsed":3,"Output":"E2E-INCONCLUSIVE: lag"}` + "\n"
	writeStream(t, archive, "app-on", "20260101T000000Z", reportEv("pass", "TestFull", 600)+inconclusive)
	dir := filepath.Join(archive, "app-on", "20260101T000000Z")
	if err := os.WriteFile(filepath.Join(dir, "go-test.retry-1.json"), []byte(reportEv("pass", "TestSeeded", 240)), 0o644); err != nil {
		t.Fatal(err)
	}
	rep, err := BuildRuntimeReport("s", archive, testRegistry())
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]RuntimeRow{}
	for _, r := range rep.Rows {
		byName[r.Test] = r
	}
	if got := byName["TestSeeded"].Seconds["app-on"]; got != 240 {
		t.Fatalf("TestSeeded app-on = %v, want the retry's 240", got)
	}
	if got := byName["TestFull"].Seconds["app-on"]; got != 600 {
		t.Fatalf("TestFull app-on = %v, want the first attempt's 600 (the retry did not re-run it)", got)
	}
	for _, u := range rep.Unmeasured {
		if u == "TestSeeded" {
			t.Fatalf("TestSeeded ran in a retry yet is listed as unmeasured: %v", rep.Unmeasured)
		}
	}
}

// An explicit --sha is the key a run recorded under; it need not exist in this checkout, so
// it is never rev-parsed (an uppercase or abbreviated one included).
func TestExplicitSHAIsUsedVerbatim(t *testing.T) {
	var out, errb strings.Builder
	g := NewGate(Config{CoverageDir: t.TempDir()}, &out, &errb)
	for _, sha := range []string{strings.Repeat("A", 40), "abc1234", strings.Repeat("e", 40)} {
		got, code := g.resolveSHA(t.Context(), "coverage", sha, true)
		if code != 0 || got != sha {
			t.Errorf("resolveSHA(%q, verbatim) = %q, %d; want it returned unchanged", sha, got, code)
		}
	}
}

// writeFile writes one named file into an archived invocation directory.
func writeFile(t *testing.T, archive, cell, inv, name, body string) {
	t.Helper()
	dir := filepath.Join(archive, cell, inv)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The file names come from the writer's own helpers, so a rename there breaks this test
// instead of silently emptying the report (the #2257 failure mode).
func phaseFile(phase string, retry int) string {
	base := "go-test.json"
	if retry > 0 {
		base = filepath.Base(retryLogPath(base, retry))
	}
	return filepath.Base(phaseLogPath(base, Phase{Name: phase}))
}

const phasedInv = "20260101T000000Z"

// Since #1977 a leg archives one stream per phase and no bare go-test.json.
func TestBuildRuntimeReportReadsPhasedInvocationWithRetry(t *testing.T) {
	archive := t.TempDir()
	inconclusive := `{"Action":"skip","Package":"p","Test":"TestSeeded","Elapsed":3,"Output":"E2E-INCONCLUSIVE: lag"}` + "\n"
	writeFile(t, archive, "app-on", phasedInv, phaseFile("shared", 0), reportEv("pass", "TestFull", 600)+inconclusive)
	writeFile(t, archive, "app-on", phasedInv, phaseFile("default-base-train", 0), reportEv("pass", "TestTrain", 50))
	writeFile(t, archive, "app-on", phasedInv, phaseFile("exclusive", 0), reportEv("pass", "TestExcl", 70))
	// TestSeeded first ran in the shared phase; the retry re-ran it in its own phase stream.
	writeFile(t, archive, "app-on", phasedInv, phaseFile("shared", 1), reportEv("pass", "TestSeeded", 240))

	reg := testRegistry()
	reg.Tests = append(reg.Tests, registry.Entry{Name: "TestTrain"}, registry.Entry{Name: "TestExcl"})
	rep, err := BuildRuntimeReport("s", archive, reg)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]RuntimeRow{}
	for _, r := range rep.Rows {
		byName[r.Test] = r
	}
	for name, want := range map[string]float64{"TestFull": 600, "TestTrain": 50, "TestExcl": 70, "TestSeeded": 240} {
		if got := byName[name].Seconds["app-on"]; got != want {
			t.Errorf("%s app-on = %v, want %v", name, got, want)
		}
	}
	if len(rep.Unmeasured) != 1 || rep.Unmeasured[0] != "TestNeverRan" {
		t.Fatalf("unmeasured = %v, want only TestNeverRan", rep.Unmeasured)
	}
}

func TestReadInvocationEventsRetryOrderingAndLayouts(t *testing.T) {
	skipEv := func(test string) string {
		return `{"Action":"skip","Package":"p","Test":"` + test + `","Elapsed":1,"Output":"E2E-INCONCLUSIVE: x"}` + "\n"
	}
	elapsed := func(t *testing.T, evs []Event, test string) float64 {
		t.Helper()
		for _, tm := range Timings(evs) {
			if tm.Test == test {
				if tm.Result == "skip" {
					return -1
				}
				return tm.Elapsed
			}
		}
		return -2
	}

	t.Run("numeric order: retry-10 after retry-9, with a gap", func(t *testing.T) {
		a := t.TempDir()
		writeFile(t, a, "c", "i", "go-test.shared.json", skipEv("TestA"))
		writeFile(t, a, "c", "i", "go-test.retry-9.shared.json", reportEv("pass", "TestA", 9))
		writeFile(t, a, "c", "i", "go-test.retry-10.shared.json", reportEv("pass", "TestA", 10))
		evs, err := readInvocationEvents(filepath.Join(a, "c", "i"))
		if err != nil {
			t.Fatal(err)
		}
		if got := elapsed(t, evs, "TestA"); got != 10 {
			t.Fatalf("TestA = %v, want retry-10's 10 (lexical order would pick retry-9)", got)
		}
	})
	t.Run("mixed layout and cross-phase retry", func(t *testing.T) {
		a := t.TempDir()
		writeFile(t, a, "c", "i", "go-test.json", reportEv("pass", "TestA", 1)+skipEv("TestB"))
		writeFile(t, a, "c", "i", "go-test.exclusive.json", reportEv("pass", "TestC", 3))
		writeFile(t, a, "c", "i", "go-test.retry-1.exclusive.json", reportEv("pass", "TestB", 2))
		evs, err := readInvocationEvents(filepath.Join(a, "c", "i"))
		if err != nil {
			t.Fatal(err)
		}
		for test, want := range map[string]float64{"TestA": 1, "TestB": 2, "TestC": 3} {
			if got := elapsed(t, evs, test); got != want {
				t.Errorf("%s = %v, want %v", test, got, want)
			}
		}
	})
	t.Run("no stream yields nil events", func(t *testing.T) {
		a := t.TempDir()
		writeFile(t, a, "c", "i", "phases.json", "{}")
		evs, err := readInvocationEvents(filepath.Join(a, "c", "i"))
		if err != nil || evs != nil {
			t.Fatalf("events = %v, err = %v; want nil, nil", evs, err)
		}
	})
	t.Run("classifyStreamName", func(t *testing.T) {
		for name, want := range map[string]struct {
			n  int
			ok bool
		}{
			"go-test.json": {0, true}, "go-test.shared.json": {0, true}, "go-test.default-base-train.json": {0, true},
			"go-test.retry-2.json": {2, true}, "go-test.retry-12.exclusive.json": {12, true},
			"phases.json": {0, false}, "go-test.log": {0, false},
		} {
			if n, ok := classifyStreamName(name); n != want.n || ok != want.ok {
				t.Errorf("classifyStreamName(%q) = %d, %v; want %d, %v", name, n, ok, want.n, want.ok)
			}
		}
	})
}

func TestReportPrintsPhaseWallClock(t *testing.T) {
	archive := t.TempDir()
	writeFile(t, archive, "app-on", phasedInv, phaseFile("shared", 0), reportEv("pass", "TestFull", 600))
	writeFile(t, archive, "app-on", phasedInv, "phases.json", `{"label":"app/on","phases":[
		{"name":"shared","tests":12,"parallel":"4","wall_ns":3720000000000},
		{"name":"exclusive","tests":3,"parallel":"1","wall_ns":600000000000}],
		"retry":{"name":"retry","tests":1,"parallel":"1","wall_ns":90000000000}}`)
	// A resume pass records only the phases it re-ran; it is listed separately.
	writeFile(t, archive, "app-on", "20260102T000000Z", "phases.json", `{"label":"app/on","phases":[{"name":"exclusive","tests":3,"parallel":"1","wall_ns":300000000000}]}`)
	// Malformed: noted, never fatal. Absent (pat-off): no entry.
	writeFile(t, archive, "pat-on", phasedInv, "phases.json", "{not json")
	writeFile(t, archive, "pat-off", phasedInv, phaseFile("shared", 0), reportEv("pass", "TestFull", 5))

	rep, err := BuildRuntimeReport("s", archive, testRegistry())
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Phases) != 3 {
		t.Fatalf("phases = %+v, want 3 entries (2 app-on invocations + the malformed pat-on)", rep.Phases)
	}
	out := rep.Format()
	for _, want := range []string{
		"phase wall-clock", "app-on  " + phasedInv, "20260102T000000Z",
		"shared", "12 test(s)", "-parallel=4", "wall 1h2m0s", "wall 10m0s", "retry", "wall 1m30s",
		"pat-on  " + phasedInv + "  phases.json malformed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("Format() missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "pat-off  ") {
		t.Errorf("a cell with no phases.json must print no phase entry:\n%s", out)
	}
	data, err := json.Marshal(rep)
	if err != nil || !strings.Contains(string(data), `"phases"`) {
		t.Fatalf("JSON lacks phases: %s, %v", data, err)
	}
	// No phases.json anywhere: no section at all.
	if strings.Contains((RuntimeReport{SHA: "s"}).Format(), "phase wall-clock") {
		t.Error("a report without phases.json must print no phase section")
	}
}

func TestReportCmdAgainstAPhasedFakeArchive(t *testing.T) {
	root := t.TempDir()
	sha := strings.Repeat("f", 40)
	l := &Ledger{Root: root, SHA: sha}
	writeFile(t, l.ArchiveDir(), "app-on", phasedInv, phaseFile("shared", 0), reportEv("pass", "TestSmokeSingleRepoFullPipeline", 1200))
	writeFile(t, l.ArchiveDir(), "app-on", phasedInv, "phases.json", `{"label":"app/on","phases":[{"name":"shared","tests":1,"parallel":"4","wall_ns":1200000000000}]}`)
	var out, errb strings.Builder
	g := NewGate(Config{CoverageDir: root}, &out, &errb)
	if code := g.ReportCmd(t.Context(), []string{"--sha", sha}); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	for _, want := range []string{"1200  TestSmokeSingleRepoFullPipeline", "phase wall-clock", "wall 20m0s"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report missing %q:\n%s", want, out.String())
		}
	}
}
