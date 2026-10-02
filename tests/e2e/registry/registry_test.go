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
		{Name: "TestA", Parity: ParitySim, Sim: []string{"TestSimA"}, Auth: Neutral, Train: Neutral},
		{Name: "TestB", Parity: ParityLiveOnly, LiveOnlyReason: ReasonRealCI, Auth: Sensitive, AuthReason: "identity", Train: Neutral},
		{Name: "TestC", Parity: ParityGap, Note: "unit only", Auth: Neutral, Train: Sensitive, TrainReason: "landing path"},
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
		{"malformed skip_ok_legs", func(r *Registry) { r.Tests[1].SkipOKLegs = []string{"nonsense"} }, "malformed skip_ok_legs"},
		{"missing auth", func(r *Registry) { r.Tests[0].Auth = "" }, "auth is \"\""},
		{"missing train", func(r *Registry) { r.Tests[0].Train = "" }, "train is \"\""},
		{"unknown auth value", func(r *Registry) { r.Tests[0].Auth = "maybe" }, "auth is \"maybe\""},
		{"unknown train value", func(r *Registry) { r.Tests[2].Train = "Sensitive" }, "train is \"Sensitive\""},
		{"sensitive auth without a reason", func(r *Registry) { r.Tests[1].AuthReason = "" }, "auth_reason is empty"},
		{"sensitive train without a reason", func(r *Registry) { r.Tests[2].TrainReason = "  " }, "train_reason is empty"},
		{"multi-line reason", func(r *Registry) { r.Tests[1].AuthReason = "a\nb" }, "single line"},
		{"reason on a neutral auth", func(r *Registry) { r.Tests[0].AuthReason = "why" }, "auth_reason is only valid"},
		{"reason on a neutral train", func(r *Registry) { r.Tests[1].TrainReason = "why" }, "train_reason is only valid"},
		{"unknown skip_ok_legs train", func(r *Registry) { r.Tests[1].SkipOKLegs = []string{"pat/maybe"} }, "malformed skip_ok_legs"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := baseRegistry()
			c.mut(r)
			mustContain(t, Check(r, liveNames, simNames), c.want)
		})
	}
}

func TestCheckSelfRecognitionMustBeAuthSensitive(t *testing.T) {
	r := &Registry{Version: Version, Tests: []Entry{
		{Name: "TestPATSelfRecognitionX", Parity: ParityGap, Auth: Neutral, Train: Neutral},
	}}
	mustContain(t, Check(r, []string{"TestPATSelfRecognitionX"}, nil), "SelfRecognition test must be auth: sensitive")
	r.Tests[0].Auth, r.Tests[0].AuthReason = Sensitive, "identity"
	if p := Check(r, []string{"TestPATSelfRecognitionX"}, nil); len(p) != 0 {
		t.Fatalf("unexpected problems: %v", p)
	}
}

func TestCheckIdentityCallers(t *testing.T) {
	r := baseRegistry()
	p := CheckIdentityCallers(r, []string{"TestA", "TestB"})
	if len(p) != 1 {
		t.Fatalf("want exactly TestA flagged, got %v", p)
	}
	mustContain(t, p, "TestA: reaches an author-identity assertion")
	// A caller with no entry is Check's concern, not this rule's.
	if p := CheckIdentityCallers(r, []string{"TestGone"}); len(p) != 0 {
		t.Fatalf("unexpected problems: %v", p)
	}
}

func TestScanIdentityAssertCallers(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"harness.go": `package e2e
import "testing"
func AssertPRAuthorIsExpectedIdentity(t *testing.T) {}
func AssertPRAuthorIsEngineIdentity(t *testing.T) {}
func seed(t *testing.T) { viaDeeper(t) }
func viaDeeper(t *testing.T) { AssertPRAuthorIsEngineIdentity(t) }
func unrelated(t *testing.T) {}
`,
		"a_test.go": `package e2e
import "testing"
func TestDirect(t *testing.T) { AssertPRAuthorIsExpectedIdentity(t) }
func TestInClosure(t *testing.T) { t.Run("x", func(t *testing.T) { AssertPRAuthorIsEngineIdentity(t) }) }
func TestViaHelper(t *testing.T) { seed(t) }
func TestClean(t *testing.T) { unrelated(t) }
func Testimony(t *testing.T) { AssertPRAuthorIsExpectedIdentity(t) }
`,
	})
	got, err := ScanIdentityAssertCallers(dir)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "TestDirect,TestInClosure,TestViaHelper" {
		t.Fatalf("callers = %v", got)
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
	problems := Check(reg, live, sim)
	// #1975: every test that reaches an author-identity assertion is auth-sensitive.
	callers, err := ScanIdentityAssertCallers("..")
	if err != nil {
		t.Fatal(err)
	}
	problems = append(problems, CheckIdentityCallers(reg, callers)...)
	// #1977: every test that reaches a bed stop/start/restart or .env rewrite is exclusive.
	lifecycle, err := ScanBedLifecycleCallers("..")
	if err != nil {
		t.Fatal(err)
	}
	problems = append(problems, CheckBedLifecycleCallers(reg, lifecycle)...)
	// #1977: t.Parallel() matches the class — shared tests call it, serial classes do not.
	parallel, err := ScanParallelTests("..")
	if err != nil {
		t.Fatal(err)
	}
	if len(parallel) == 0 {
		t.Fatal("ScanParallelTests found no parallel live test")
	}
	problems = append(problems, CheckParallelConsistency(reg, parallel)...)
	if len(problems) > 0 {
		t.Fatalf("tests/e2e/registry/registry.json is out of sync with the tree:\n  %s",
			strings.Join(problems, "\n  "))
	}
}

func TestMatchLegAndSkipOK(t *testing.T) {
	cases := []struct {
		pattern, label string
		want           bool
	}{
		{"*/off", "app/off", true},
		{"*/off", "pat/on", false},
		{"pat/*", "pat/on", true},
		{"*/*", "app/on", true},
		{"app/on", "app/on", true},
		{"app/on", "pat/on", false},
		{"bogus", "app/on", false},
	}
	for _, c := range cases {
		if got := MatchLeg(c.pattern, c.label); got != c.want {
			t.Errorf("MatchLeg(%q, %q) = %v, want %v", c.pattern, c.label, got, c.want)
		}
	}
	e := Entry{SkipOKLegs: []string{"*/off"}}
	if !e.SkipOK("pat/off") || e.SkipOK("pat/on") {
		t.Error("Entry.SkipOK mismatch")
	}
}

func TestCheckIsolationMarkers(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Registry)
		want string
	}{
		{"exclusive without a reason", func(r *Registry) { r.Tests[0].Exclusive = true }, "exclusive_reason is empty"},
		{"exclusive blank reason", func(r *Registry) { r.Tests[0].Exclusive, r.Tests[0].ExclusiveReason = true, "  " }, "exclusive_reason is empty"},
		{"exclusive multi-line reason", func(r *Registry) { r.Tests[0].Exclusive, r.Tests[0].ExclusiveReason = true, "a\nb" }, "single line"},
		{"reason without the flag", func(r *Registry) { r.Tests[0].ExclusiveReason = "why" }, "exclusive_reason is only valid"},
		{"default-base without a reason", func(r *Registry) { r.Tests[1].DefaultBaseTrain = true }, "default_base_train_reason is empty"},
		{"default-base reason without the flag", func(r *Registry) { r.Tests[1].DefaultBaseTrainReason = "why" }, "default_base_train_reason is only valid"},
		{"both classes", func(r *Registry) {
			e := &r.Tests[0]
			e.Exclusive, e.ExclusiveReason = true, "restarts"
			e.DefaultBaseTrain, e.DefaultBaseTrainReason = true, "main"
		}, "mutually exclusive"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := baseRegistry()
			c.mut(r)
			mustContain(t, Check(r, liveNames, simNames), c.want)
		})
	}
	r := baseRegistry()
	r.Tests[0].Exclusive, r.Tests[0].ExclusiveReason = true, "restarts the bed"
	r.Tests[1].DefaultBaseTrain, r.Tests[1].DefaultBaseTrainReason = true, "asserts on main's batch"
	if p := Check(r, liveNames, simNames); len(p) != 0 {
		t.Fatalf("unexpected problems: %v", p)
	}
	got := r.IsolationOf()
	if got["TestA"] != IsolationExclusive || got["TestB"] != IsolationDefaultBaseTrain || got["TestC"] != IsolationShared {
		t.Fatalf("IsolationOf = %v", got)
	}
}

func TestCheckBedLifecycleCallers(t *testing.T) {
	r := baseRegistry()
	r.Tests[1].Exclusive, r.Tests[1].ExclusiveReason = true, "restarts"
	p := CheckBedLifecycleCallers(r, []string{"TestA", "TestB", "TestGone"})
	if len(p) != 1 {
		t.Fatalf("want exactly TestA flagged, got %v", p)
	}
	mustContain(t, p, "TestA: reaches a bed lifecycle call")
}

func TestCheckParallelConsistency(t *testing.T) {
	r := baseRegistry()
	r.Tests[1].DefaultBaseTrain, r.Tests[1].DefaultBaseTrainReason = true, "main"
	r.Tests[2].Exclusive, r.Tests[2].ExclusiveReason = true, "restarts"
	// TestA shared + parallel (ok); TestB default-base + serial (ok); TestC exclusive + serial (ok).
	if p := CheckParallelConsistency(r, []string{"TestA"}); len(p) != 0 {
		t.Fatalf("unexpected problems: %v", p)
	}
	mustContain(t, CheckParallelConsistency(r, nil), "TestA: is shared but does not call t.Parallel()")
	p := CheckParallelConsistency(r, []string{"TestA", "TestB", "TestC"})
	mustContain(t, p, "TestB: is default-base-train but calls t.Parallel()")
	mustContain(t, p, "TestC: is exclusive but calls t.Parallel()")
}

func TestScanBedLifecycleCallers(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"lifecycle.go": `package e2e
import "testing"
func StopFabrikTestBed(t *testing.T) {}
func StartFabrikTestBed(t *testing.T) {}
func RestartFabrikTestBed(t *testing.T) { StopFabrikTestBed(t); StartFabrikTestBed(t) }
func writeEnvFileValue() {}
func switchMode() { writeEnvFileValue() }
func unrelated(t *testing.T) {}
`,
		"a_test.go": `package e2e
import "testing"
func TestDirect(t *testing.T) { StopFabrikTestBed(t) }
func TestInCleanup(t *testing.T) { t.Cleanup(func() { StartFabrikTestBed(t) }) }
func TestRestart(t *testing.T) { RestartFabrikTestBed(t) }
func TestViaHelper(t *testing.T) { switchMode() }
func TestClean(t *testing.T) { unrelated(t) }
`,
	})
	got, err := ScanBedLifecycleCallers(dir)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "TestDirect,TestInCleanup,TestRestart,TestViaHelper" {
		t.Fatalf("callers = %v", got)
	}
}

func TestScanParallelTests(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"a_test.go": `package e2e
import "testing"
func TestPar(t *testing.T) { t.Parallel() }
func TestParLater(t *testing.T) { LoadEnv(t); t.Parallel() }
func TestSerial(t *testing.T) { LoadEnv(t) }
func TestSubParallel(t *testing.T) { t.Run("x", func(t *testing.T) { t.Parallel() }) }
func helper(t *testing.T) { t.Parallel() }
`,
	})
	got, err := ScanParallelTests(dir)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "TestPar,TestParLater" {
		t.Fatalf("parallel = %v", got)
	}
}
