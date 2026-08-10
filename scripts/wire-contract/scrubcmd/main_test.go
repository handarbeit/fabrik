package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRun_StdinStdoutRedacts(t *testing.T) {
	var stdout, stderr bytes.Buffer
	in := strings.NewReader(`{"token":"ghp_abcdefghijklmnopqrstuvwxyz0123456789AB"}`)
	code := run(nil, in, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stderr: %s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "ghp_abcdefghijklmnopqrstuvwxyz0123456789AB") {
		t.Fatalf("stdout still contains the raw token: %s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "[SCRUBBED:") {
		t.Fatalf("stdout has no scrub marker: %s", stdout.String())
	}
}

func TestRun_FileInFileOut(t *testing.T) {
	dir := t.TempDir()
	inPath := filepath.Join(dir, "in.json")
	outPath := filepath.Join(dir, "out.json")
	if err := os.WriteFile(inPath, []byte(`{"email":"jane.doe@handarbeit.io"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"-in", inPath, "-out", outPath}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stderr: %s", code, stderr.String())
	}
	out, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "jane.doe@handarbeit.io") {
		t.Fatalf("output file still contains the raw email: %s", out)
	}
}

func TestRun_CheckModeFailsOnSecret(t *testing.T) {
	var stdout, stderr bytes.Buffer
	in := strings.NewReader(`{"token":"ghp_abcdefghijklmnopqrstuvwxyz0123456789AB"}`)
	code := run([]string{"-check"}, in, &stdout, &stderr)
	if code == 0 {
		t.Fatal("expected non-zero exit for a secret-shaped fixture in -check mode")
	}
	if stdout.Len() != 0 {
		t.Fatalf("-check mode should write nothing to stdout, got: %s", stdout.String())
	}
}

func TestRun_CheckModePassesOnClean(t *testing.T) {
	var stdout, stderr bytes.Buffer
	in := strings.NewReader(`{"id":"PR_kwDOABC123","number":42}`)
	code := run([]string{"-check"}, in, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0 for a clean fixture, got %d, stderr: %s", code, stderr.String())
	}
}
