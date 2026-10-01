package registry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func mustContain(t *testing.T, problems []string, sub string) {
	t.Helper()
	for _, p := range problems {
		if strings.Contains(p, sub) {
			return
		}
	}
	t.Fatalf("no problem containing %q in %q", sub, problems)
}

func baseRegistry() *Registry {
	return &Registry{Version: Version, Tests: []Entry{
		{Name: "TestA", Parity: ParitySim, Sim: []string{"TestSimA"}},
		{Name: "TestB", Parity: ParityLiveOnly, LiveOnlyReason: ReasonRealCI},
		{Name: "TestC", Parity: ParityGap, Note: "unit only"},
	}}
}

var (
	liveNames = []string{"TestA", "TestB", "TestC"}
	simNames  = []string{"TestSimA", "TestSimOther"}
)

func TestCheckConsistentRegistryPasses(t *testing.T) {
	if p := Check(baseRegistry(), liveNames, simNames); len(p) != 0 {
		t.Fatalf("unexpected problems: %v", p)
	}
}

func TestCheckUnmappedLiveTest(t *testing.T) {
	p := Check(baseRegistry(), append([]string{"TestNew"}, liveNames...), simNames)
	mustContain(t, p, "TestNew: live scenario test has no registry entry")
}

func TestCheckStaleEntry(t *testing.T) {
	p := Check(baseRegistry(), []string{"TestA", "TestB"}, simNames)
	mustContain(t, p, "TestC: stale entry")
}

func TestCheckDanglingSimReference(t *testing.T) {
	r := baseRegistry()
	r.Tests[0].Sim = []string{"TestSimGone"}
	mustContain(t, Check(r, liveNames, simNames), "dangling sim reference TestSimGone")
}

func TestCheckMalformedEntries(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Registry)
		want string
	}{
		{"empty sim list", func(r *Registry) { r.Tests[0].Sim = nil }, "requires a non-empty sim list"},
		{"unknown reason", func(r *Registry) { r.Tests[1].LiveOnlyReason = "vibes" }, "unknown live_only_reason"},
		{"missing reason", func(r *Registry) { r.Tests[1].LiveOnlyReason = "" }, "unknown live_only_reason"},
		{"other without note", func(r *Registry) { r.Tests[1].LiveOnlyReason = ReasonOther }, "requires a note"},
		{"unknown parity", func(r *Registry) { r.Tests[2].Parity = "" }, "unknown parity"},
		{"sim list on gap", func(r *Registry) { r.Tests[2].Sim = []string{"TestSimA"} }, "must not carry a sim list"},
		{"sim list on live-only", func(r *Registry) { r.Tests[1].Sim = []string{"TestSimA"} }, "must not carry a sim list"},
		{"reason on sim", func(r *Registry) { r.Tests[0].LiveOnlyReason = ReasonRealCI }, "only valid with parity"},
		{"duplicate sim ref", func(r *Registry) { r.Tests[0].Sim = []string{"TestSimA", "TestSimA"} }, "listed twice"},
		{"duplicate entry", func(r *Registry) { r.Tests = append(r.Tests, r.Tests[2]) }, "duplicate registry entry"},
		{"unsorted", func(r *Registry) { r.Tests[0], r.Tests[1] = r.Tests[1], r.Tests[0] }, "not sorted"},
		{"bad version", func(r *Registry) { r.Version = 99 }, "registry version"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := baseRegistry()
			c.mut(r)
			mustContain(t, Check(r, liveNames, simNames), c.want)
		})
	}
}

func TestDecodeRejectsUnknownField(t *testing.T) {
	if _, err := Decode([]byte(`{"version":1,"tests":[{"name":"TestA","parity":"gap","bogus":1}]}`)); err == nil {
		t.Fatal("expected unknown field to be rejected")
	}
	if _, err := Decode([]byte(`{"version":1,"tests":[]} {}`)); err == nil {
		t.Fatal("expected trailing data to be rejected")
	}
}

func TestSummary(t *testing.T) {
	c := baseRegistry().Summary()
	if c != (Counts{Covered: 1, LiveOnly: 1, Gap: 1}) {
		t.Fatalf("Summary() = %+v", c)
	}
}

func TestScanLiveTestsRule(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"live_test.go": `//go:build e2e
package e2e
import "testing"
func TestLive(t *testing.T) { env := LoadEnv(t); _ = env }
func TestLiveInSubtest(t *testing.T) { t.Run("x", func(t *testing.T) { LoadEnv(t) }) }
func TestHarnessUnit(t *testing.T) {}
func helper(t *testing.T) {}
func Testimony(t *testing.T) { LoadEnv(t) }
`,
		"harness.go": `//go:build e2e
package e2e
import "testing"
func LoadEnv(t *testing.T) int { return 0 }
`,
	})
	// Testimony is not a Test function (lowercase after Test) and calls LoadEnv from
	// a non-Test body, so it must be reported as a stray call.
	if _, err := ScanLiveTests(dir); err == nil || !strings.Contains(err.Error(), "Testimony") {
		t.Fatalf("expected stray-call error naming Testimony, got %v", err)
	}
	if err := os.Remove(filepath.Join(dir, "live_test.go")); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, dir, map[string]string{"live_test.go": `package e2e
import "testing"
func TestLive(t *testing.T) { env := LoadEnv(t); _ = env }
func TestLiveInSubtest(t *testing.T) { t.Run("x", func(t *testing.T) { LoadEnv(t) }) }
func TestHarnessUnit(t *testing.T) {}
`})
	got, err := ScanLiveTests(dir)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "TestLive,TestLiveInSubtest" {
		t.Fatalf("live tests = %v", got)
	}
}

func TestScanLiveTestsStrayHelperCall(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"a_test.go": `package e2e
import "testing"
func TestViaHelper(t *testing.T) { setup(t) }
func setup(t *testing.T) { LoadEnv(t) }
`,
	})
	_, err := ScanLiveTests(dir)
	if err == nil || !strings.Contains(err.Error(), "setup") {
		t.Fatalf("expected stray-call error naming setup, got %v", err)
	}
}

func TestScanFailsLoudlyOnParseError(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"bad_test.go": "package e2e\nfunc TestX( {"})
	if _, err := ScanLiveTests(dir); err == nil {
		t.Fatal("live scan: expected parse error")
	}
	if _, err := ScanSimTests(dir); err == nil {
		t.Fatal("sim scan: expected parse error")
	}
}

func TestScanSimTests(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"a_test.go":  "package sim\nimport \"testing\"\nfunc TestOne(t *testing.T) {}\nfunc helper() {}\n",
		"helpers.go": "package sim\nfunc TestNotATestFile() {}\n",
	})
	got, err := ScanSimTests(dir)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "TestOne" {
		t.Fatalf("sim tests = %v", got)
	}
}

// TestRegistryMatchesTree is the R4 enforcement: it runs in plain `go test ./...`
// (no build tag) on every PR and fails when the registry and the live suite
// disagree — an unmapped live test, a stale entry, a dangling sim reference or a
// malformed entry.
func TestRegistryMatchesTree(t *testing.T) {
	reg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	live, err := ScanLiveTests("..")
	if err != nil {
		t.Fatal(err)
	}
	sim, err := ScanSimTests(filepath.Join("..", "..", "sim"))
	if err != nil {
		t.Fatal(err)
	}
	// Guard against a scanner that silently matches nothing.
	if len(live) == 0 || len(sim) == 0 {
		t.Fatalf("scanner found %d live and %d sim tests", len(live), len(sim))
	}
	if problems := Check(reg, live, sim); len(problems) > 0 {
		t.Fatalf("tests/e2e/registry/registry.json is out of sync with the tree:\n  %s",
			strings.Join(problems, "\n  "))
	}
}
