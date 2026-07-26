package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func skipIfNoGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH")
	}
}

// runIn runs a command in dir, failing the test with its combined output on error.
func runIn(t *testing.T, dir, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %s: %v", name, args, out, err)
	}
	return string(out)
}

// initTestModule creates a minimal buildable Go module with one commit in a
// temp git repo and returns its directory.
func initTestModule(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module verifytest\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	mainSrc := `package main

func main() {}
`
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(mainSrc), 0o644); err != nil {
		t.Fatalf("write main.go: %v", err)
	}

	runIn(t, dir, "git", "init", "-b", "main")
	runIn(t, dir, "git", "config", "user.email", "test@test.com")
	runIn(t, dir, "git", "config", "user.name", "Test")
	runIn(t, dir, "git", "add", ".")
	runIn(t, dir, "git", "commit", "-m", "initial")

	return dir
}

func headRevision(t *testing.T, dir string) string {
	t.Helper()
	out := runIn(t, dir, "git", "rev-parse", "HEAD")
	return trimNewline(out)
}

func trimNewline(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

// buildBinary runs `go build` inside dir, producing outPath.
func buildBinary(t *testing.T, dir, outPath string) {
	t.Helper()
	runIn(t, dir, "go", "build", "-o", outPath, ".")
}

func TestVerify_CleanCheckoutPasses(t *testing.T) {
	skipIfNoGit(t)
	dir := initTestModule(t)
	rev := headRevision(t, dir)

	bin := filepath.Join(t.TempDir(), "artifact")
	buildBinary(t, dir, bin)

	if err := verify(bin, rev); err != nil {
		t.Fatalf("verify() on clean checkout = %v, want nil", err)
	}
}

func TestVerify_DirtyTreeFails(t *testing.T) {
	skipIfNoGit(t)
	dir := initTestModule(t)
	rev := headRevision(t, dir)

	// Dirty a tracked file before building.
	mainSrc := `package main

func main() {
	_ = 1
}
`
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(mainSrc), 0o644); err != nil {
		t.Fatalf("dirty main.go: %v", err)
	}

	bin := filepath.Join(t.TempDir(), "artifact")
	buildBinary(t, dir, bin)

	err := verify(bin, rev)
	if err == nil {
		t.Fatal("verify() on dirty tree = nil, want error")
	}
}

func TestVerify_WrongRevisionFails(t *testing.T) {
	skipIfNoGit(t)
	dir := initTestModule(t)

	bin := filepath.Join(t.TempDir(), "artifact")
	buildBinary(t, dir, bin)

	err := verify(bin, "0000000000000000000000000000000000000000")
	if err == nil {
		t.Fatal("verify() with wrong expected revision = nil, want error")
	}
}
