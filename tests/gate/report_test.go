package gate

import (
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
