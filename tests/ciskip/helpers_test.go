// Package ciskip tests scripts/ci/classify-diff.sh and its rule table
// scripts/ci/go-skip-paths.txt (#2254, ADR-2254): the classifier that lets CI's
// two Go test jobs end early when a PR changes no Go build input.
//
// The package is untagged so plain `go test ./...` runs it on every PR, like
// tests/e2e/registry.
package ciskip

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// repoRoot walks up from the package directory to the directory holding go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test directory")
		}
		dir = parent
	}
}

func skipIfNoBash(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
}

func skipIfNoGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
}

// result is one classifier run.
type result struct {
	Skip, CutRelease, Killgroup, ADRRefs bool
	Stdout                               string
	Summary                              string
}

// parseOutputs reads a $GITHUB_OUTPUT file written by the classifier.
func parseOutputs(t *testing.T, path string) map[string]string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading GITHUB_OUTPUT: %v", err)
	}
	out := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("malformed GITHUB_OUTPUT line %q", line)
		}
		out[k] = v
	}
	return out
}

// runScript runs classify-diff.sh with args in dir, feeding stdin, and returns
// its result. Extra env entries (KEY=VALUE) are appended.
func runScript(t *testing.T, dir string, stdin []byte, extraEnv []string, args ...string) result {
	t.Helper()
	skipIfNoBash(t)
	script := filepath.Join(repoRoot(t), "scripts", "ci", "classify-diff.sh")
	tmp := t.TempDir()
	outFile := filepath.Join(tmp, "output")
	sumFile := filepath.Join(tmp, "summary")
	cmd := exec.Command("bash", append([]string{script}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GITHUB_OUTPUT="+outFile, "GITHUB_STEP_SUMMARY="+sumFile)
	cmd.Env = append(cmd.Env, extraEnv...)
	cmd.Stdin = bytes.NewReader(stdin)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stdout
	if err := cmd.Run(); err != nil {
		t.Fatalf("classify-diff.sh failed (it must always exit 0): %v\n%s", err, stdout.String())
	}
	o := parseOutputs(t, outFile)
	sum, _ := os.ReadFile(sumFile)
	return result{
		Skip:       o["skip"] == "true",
		CutRelease: o["cut_release"] == "true",
		Killgroup:  o["killgroup"] == "true",
		ADRRefs:    o["adr_refs"] == "true",
		Stdout:     stdout.String(),
		Summary:    string(sum),
	}
}

// classifyPaths runs the classifier over a path list (NUL-delimited stdin).
func classifyPaths(t *testing.T, extraEnv []string, paths ...string) result {
	t.Helper()
	var in []byte
	for _, p := range paths {
		in = append(in, p...)
		in = append(in, 0)
	}
	return runScript(t, repoRoot(t), in, extraEnv, "--paths-from-stdin0")
}
