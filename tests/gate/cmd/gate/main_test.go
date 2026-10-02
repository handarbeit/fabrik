package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestSignalReapsTheChildTree builds the real binary and runs `gate reset`
// against a stub `gh` that parks a grandchild, then signals the gate. The
// runner must exit 128+signum AND leave no process behind — the orphaned
// sim.test/go-test tree class of #1624/#1989, which a bare `go run` shim
// (swallowing the signal) would reintroduce.
func TestSignalReapsTheChildTree(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not available")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "gate")
	if out, err := exec.Command("go", "build", "-o", bin, "github.com/handarbeit/fabrik/tests/gate/cmd/gate").CombinedOutput(); err != nil {
		t.Fatalf("building the gate: %v\n%s", err, out)
	}

	bed := filepath.Join(dir, "bed")
	if err := os.MkdirAll(bed, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bed, ".env"), []byte("FABRIK_TOKEN=tok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stubs := filepath.Join(dir, "stubs")
	if err := os.MkdirAll(stubs, 0o755); err != nil {
		t.Fatal(err)
	}
	pidfile := filepath.Join(dir, "grandchild.pid")
	stub := "#!/bin/sh\nsleep 60 &\necho $! > " + pidfile + "\nwait\n"
	if err := os.WriteFile(filepath.Join(stubs, "gh"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}

	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT} {
		os.Remove(pidfile)
		cmd := exec.Command(bin, "reset")
		cmd.Env = append(os.Environ(), "FABRIK_TEST_DIR="+bed, "PATH="+stubs+":"+os.Getenv("PATH"))
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		var grandchild int
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			if data, err := os.ReadFile(pidfile); err == nil {
				if grandchild, err = strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
					break
				}
			}
			time.Sleep(25 * time.Millisecond)
		}
		if grandchild == 0 {
			cmd.Process.Kill()
			t.Fatalf("%v: the stub gh never started", sig)
		}
		if syscall.Kill(grandchild, 0) != nil {
			t.Fatalf("%v: the grandchild is not alive before the signal", sig)
		}

		if err := cmd.Process.Signal(sig); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			want := 128 + int(sig)
			if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != want {
				t.Errorf("%v: want exit %d, got %v", sig, want, err)
			}
		case <-time.After(30 * time.Second):
			cmd.Process.Kill()
			t.Fatalf("%v: the gate did not exit", sig)
		}
		gone := false
		for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(25 * time.Millisecond) {
			if syscall.Kill(grandchild, 0) != nil {
				gone = true
				break
			}
		}
		if !gone {
			syscall.Kill(grandchild, syscall.SIGKILL)
			t.Errorf("%v: the grandchild %d outlived the gate — orphaned process tree", sig, grandchild)
		}
	}
}

func TestUnknownSubcommandRunsTheGate(t *testing.T) {
	// Arguments that are not a subcommand belong to the gate (they are go test
	// flags), exactly as run.sh passed them through.
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not available")
	}
	sub, rest := splitSubcommand([]string{"-run", "TestX"})
	if sub != "run" || len(rest) != 2 {
		t.Errorf("got %q %v", sub, rest)
	}
	sub, rest = splitSubcommand([]string{"reset", "--worktrees"})
	if sub != "reset" || len(rest) != 1 || rest[0] != "--worktrees" {
		t.Errorf("got %q %v", sub, rest)
	}
	sub, rest = splitSubcommand([]string{"run", "--clean"})
	if sub != "run" || len(rest) != 1 || rest[0] != "--clean" {
		t.Errorf("got %q %v", sub, rest)
	}
}

func TestParseResetArgs(t *testing.T) {
	for _, tc := range []struct {
		argv      []string
		worktrees bool
		bed       string
		err       bool
	}{
		{argv: nil},
		{argv: []string{"--worktrees"}, worktrees: true},
		{argv: []string{"--bed", "/beds/b"}, bed: "/beds/b"},
		{argv: []string{"--worktrees", "--bed=/beds/a"}, worktrees: true, bed: "/beds/a"},
		{argv: []string{"--bed"}, err: true},
		{argv: []string{"--bed="}, err: true},
		{argv: []string{"--bed", ""}, err: true},
	} {
		opts, bed, err := parseResetArgs(tc.argv)
		if (err != nil) != tc.err || opts.Worktrees != tc.worktrees || bed != tc.bed {
			t.Errorf("%v: opts=%+v bed=%q err=%v", tc.argv, opts, bed, err)
		}
	}
}
