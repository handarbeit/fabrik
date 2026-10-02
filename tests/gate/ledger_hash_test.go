package gate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hashFixture writes a tiny tests/e2e-like package and returns its dir.
func hashFixture(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func baseHashFiles() map[string]string {
	return map[string]string{
		"a_test.go": "package e2e\nimport \"testing\"\nfunc TestA(t *testing.T) { usesHelper(t) }\n",
		"b_test.go": "package e2e\nimport \"testing\"\nfunc TestB(t *testing.T) { deep(t) }\nfunc TestC(t *testing.T) { _ = 1 }\n",
		"helpers.go": "package e2e\nimport \"testing\"\nfunc usesHelper(t *testing.T) { inner() }\nfunc inner() {}\n" +
			"func deep(t *testing.T) { var e Env; e.Run() }\ntype Env struct{}\nfunc (Env) Run() { leaf() }\nfunc leaf() {}\nfunc unrelated() {}\n",
	}
}

func mustHash(t *testing.T, files map[string]string) map[string]string {
	t.Helper()
	h, err := HashTests(hashFixture(t, files))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func changed(a, b map[string]string) map[string]bool {
	out := map[string]bool{}
	for k, v := range a {
		if b[k] != v {
			out[k] = true
		}
	}
	return out
}

func TestHashTestsStableAndComplete(t *testing.T) {
	a, b := mustHash(t, baseHashFiles()), mustHash(t, baseHashFiles())
	if len(a) != 3 || len(changed(a, b)) != 0 {
		t.Fatalf("hashes must be stable and cover every test: %v vs %v", a, b)
	}
}

func TestHashTestsEditingOneTestReopensOnlyThatFile(t *testing.T) {
	base := mustHash(t, baseHashFiles())
	f := baseHashFiles()
	f["a_test.go"] += "// edited\n"
	if got := changed(base, mustHash(t, f)); len(got) != 1 || !got["TestA"] {
		t.Fatalf("only TestA may change, got %v", got)
	}
}

func TestHashTestsSameFileSiblingsReopen(t *testing.T) {
	base := mustHash(t, baseHashFiles())
	f := baseHashFiles()
	f["b_test.go"] = "package e2e\nimport \"testing\"\nfunc TestB(t *testing.T) { deep(t) }\nfunc TestC(t *testing.T) { _ = 2 }\n"
	got := changed(base, mustHash(t, f))
	if !got["TestB"] || !got["TestC"] || got["TestA"] {
		t.Fatalf("function + file is the floor: B and C reopen, A does not: %v", got)
	}
}

func TestHashTestsReachableHelperReopensDependents(t *testing.T) {
	base := mustHash(t, baseHashFiles())
	f := baseHashFiles()
	f["helpers.go"] = replaceOnce(f["helpers.go"], "func inner() {}", "func inner() { _ = 1 }")
	if got := changed(base, mustHash(t, f)); len(got) != 1 || !got["TestA"] {
		t.Fatalf("a transitively reachable helper reopens exactly its dependents: %v", got)
	}
	// A method reached by name through a selector (over-approximation) counts too.
	f = baseHashFiles()
	f["helpers.go"] = replaceOnce(f["helpers.go"], "func leaf() {}", "func leaf() { _ = 1 }")
	if got := changed(base, mustHash(t, f)); len(got) != 1 || !got["TestB"] {
		t.Fatalf("leaf reached through a method must reopen TestB only: %v", got)
	}
}

func TestHashTestsUnreferencedHelperReopensNothing(t *testing.T) {
	base := mustHash(t, baseHashFiles())
	f := baseHashFiles()
	f["helpers.go"] = replaceOnce(f["helpers.go"], "func unrelated() {}", "func unrelated() { _ = 1 }")
	if got := changed(base, mustHash(t, f)); len(got) != 0 {
		t.Fatalf("an unreferenced helper must reopen nothing: %v", got)
	}
}

func TestHashTestsInitReopensAll(t *testing.T) {
	base := mustHash(t, baseHashFiles())
	f := baseHashFiles()
	f["helpers.go"] += "func init() { _ = 1 }\n"
	if got := changed(base, mustHash(t, f)); len(got) != 3 {
		t.Fatalf("init functions run for every test: %v", got)
	}
}

func TestHashTestsParseErrorIsReturned(t *testing.T) {
	f := baseHashFiles()
	f["bad_test.go"] = "package e2e\nfunc TestX( {"
	if _, err := HashTests(hashFixture(t, f)); err == nil {
		t.Fatal("a file that cannot be parsed must be an error, never skipped")
	}
}

func TestHashTestsRealPackage(t *testing.T) {
	h, err := HashTests("../e2e")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := h["TestSwitchTrainMode"]; !ok || len(h) < 10 {
		t.Fatalf("expected the real e2e tests to be hashed, got %d", len(h))
	}
}

func replaceOnce(s, old, new string) string {
	if !strings.Contains(s, old) {
		panic("fixture drift: " + old)
	}
	return strings.Replace(s, old, new, 1)
}
