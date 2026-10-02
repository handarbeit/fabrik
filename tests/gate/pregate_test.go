package gate

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
)

// Ported from scripts/e2e/pregate_test.sh. The bash test shadowed `go` on PATH
// with a fake that appended to a marker file; here a fake Commander records
// the calls instead.

const (
	testHEAD = "0123456789abcdef0123456789abcdef01234567"
	// The exact value scripts/cut-release.sh's allowed_dirty_regex prints for
	// VERSION=v0.1.0 — proves Go's RE2 accepts the grep -E pattern the release
	// path really exports.
	cutReleaseAllowedRegex = `^\?\? release-notes/v0\.1\.0\.md$| M release-notes/v0\.1\.0\.md$|^M  release-notes/v0\.1\.0\.md$| M plugin/known_embedded_versions\.go$|^M  plugin/known_embedded_versions\.go$| M plugin/[^/]+/\.claude-plugin/plugin\.json$|^M  plugin/[^/]+/\.claude-plugin/plugin\.json$`
)

// pregateGate wires a fake whose `git` answers rev-parse HEAD / status and whose
// sim and go runs exit with simExit / goExit.
func pregateGate(t *testing.T, porcelain string, simExit, goExit int, env ...string) (*Gate, *fakeExec, *bytes.Buffer) {
	t.Helper()
	g, fe, out, _ := testGate(t, env...)
	fe.handler = func(_ context.Context, c Cmd) Result {
		switch {
		case c.Name == "git" && len(c.Args) > 0 && c.Args[0] == "rev-parse":
			writeStdout(c, testHEAD+"\n")
		case c.Name == "git" && len(c.Args) > 0 && c.Args[0] == "status":
			writeStdout(c, porcelain)
		case strings.HasSuffix(c.Name, "scripts/sim/run.sh"):
			return Result{ExitCode: simExit}
		case c.Name == "go":
			return Result{ExitCode: goExit}
		}
		return Result{}
	}
	return g, fe, out
}

// liveRuns counts the sim and go invocations — the pre-gate's actual work.
func liveRuns(fe *fakeExec) int {
	n := len(fe.callsNamed("go"))
	for _, l := range fe.lines() {
		if strings.Contains(l, "scripts/sim/run.sh") {
			n++
		}
	}
	return n
}

func TestPregate(t *testing.T) {
	ctx := context.Background()

	t.Run("E2E_SKIP_PREGATE=1 exits 0 and makes no call", func(t *testing.T) {
		g, fe, _ := pregateGate(t, "", 1, 1, "E2E_SKIP_PREGATE=1")
		if err := g.RunPregate(ctx); err != nil {
			t.Fatal(err)
		}
		if len(fe.lines()) != 0 {
			t.Errorf("calls: %v", fe.lines())
		}
	})

	t.Run("both layers passing runs sim then the github wire-contract tests", func(t *testing.T) {
		g, fe, _ := pregateGate(t, "", 0, 0)
		if err := g.RunPregate(ctx); err != nil {
			t.Fatal(err)
		}
		lines := fe.lines()
		if len(lines) != 2 || !strings.HasSuffix(lines[0], "scripts/sim/run.sh --all") || lines[1] != "go test -race -count=1 ./github/..." {
			t.Errorf("calls: %v", lines)
		}
	})

	t.Run("a sim failure exits 5 after exactly one call", func(t *testing.T) {
		g, fe, _ := pregateGate(t, "", 1, 0)
		err := g.RunPregate(ctx)
		if exitCode(err) != ExitPregateFailed {
			t.Fatalf("want exit 5, got %v", err)
		}
		if n := liveRuns(fe); n != 1 {
			t.Errorf("the github tests must never run after a sim failure, got %v", fe.lines())
		}
	})

	t.Run("a wire-contract failure exits 5", func(t *testing.T) {
		g, _, _ := pregateGate(t, "", 0, 1)
		if err := g.RunPregate(ctx); exitCode(err) != ExitPregateFailed || !strings.Contains(err.Error(), "wire-contract tests failed") {
			t.Fatalf("want exit 5, got %v", err)
		}
	})

	t.Run("a matching FABRIK_PREGATE_VERIFIED_SHA on a clean tree skips", func(t *testing.T) {
		g, fe, out := pregateGate(t, "", 1, 1, "FABRIK_PREGATE_VERIFIED_SHA="+testHEAD)
		if err := g.RunPregate(ctx); err != nil {
			t.Fatal(err)
		}
		if n := liveRuns(fe); n != 0 {
			t.Errorf("a verified SHA must run nothing, got %d", n)
		}
		if !strings.Contains(out.String(), "already verified for "+testHEAD) {
			t.Errorf("out=%q", out)
		}
	})

	t.Run("a mismatched SHA runs the full pre-gate", func(t *testing.T) {
		g, fe, out := pregateGate(t, "", 0, 0, "FABRIK_PREGATE_VERIFIED_SHA=0000000000000000000000000000000000000dead")
		if err := g.RunPregate(ctx); err != nil {
			t.Fatal(err)
		}
		if n := liveRuns(fe); n != 2 {
			t.Errorf("want the full pre-gate (2 runs), got %d", n)
		}
		if !strings.Contains(out.String(), "does not match HEAD") {
			t.Errorf("missing the mismatch note: %q", out)
		}
	})

	t.Run("a matching SHA with a dirty tree runs the full pre-gate, not a skip", func(t *testing.T) {
		g, fe, out := pregateGate(t, "?? .pregate_test_dirty_tree_scratch\n", 0, 0, "FABRIK_PREGATE_VERIFIED_SHA="+testHEAD)
		if err := g.RunPregate(ctx); err != nil {
			t.Fatal(err)
		}
		if n := liveRuns(fe); n != 2 {
			t.Errorf("want 2 runs, got %d", n)
		}
		if !strings.Contains(out.String(), "has uncommitted changes") {
			t.Errorf("missing the dirty-tree note: %q", out)
		}
	})

	t.Run("a matching SHA with only allowlisted dirt skips", func(t *testing.T) {
		g, fe, _ := pregateGate(t, "?? .pregate_test_dirty_tree_scratch\n", 1, 1,
			"FABRIK_PREGATE_VERIFIED_SHA="+testHEAD, `FABRIK_PREGATE_ALLOWED_DIRTY_REGEX=^\?\? \.pregate_test_dirty_tree_scratch$`)
		if err := g.RunPregate(ctx); err != nil {
			t.Fatal(err)
		}
		if n := liveRuns(fe); n != 0 {
			t.Errorf("want a skip, got %d runs", n)
		}
	})

	t.Run("a matching SHA with non-allowlisted dirt runs the full pre-gate", func(t *testing.T) {
		g, fe, _ := pregateGate(t, "?? .pregate_test_dirty_tree_scratch\n", 0, 0,
			"FABRIK_PREGATE_VERIFIED_SHA="+testHEAD, `FABRIK_PREGATE_ALLOWED_DIRTY_REGEX=^\?\? some/other/file\.go$`)
		if err := g.RunPregate(ctx); err != nil {
			t.Fatal(err)
		}
		if n := liveRuns(fe); n != 2 {
			t.Errorf("want 2 runs, got %d", n)
		}
	})

	t.Run("an allowlist regex that does not compile fails closed (deliberate delta from bash's fail-open)", func(t *testing.T) {
		g, fe, _ := pregateGate(t, "", 0, 0, "FABRIK_PREGATE_VERIFIED_SHA="+testHEAD, "FABRIK_PREGATE_ALLOWED_DIRTY_REGEX=([unclosed")
		// A clean tree with a bad regex still matches the SHA and has nothing dirty:
		// the bad regex only widens "what is allowed", so it can never skip a dirty tree.
		if err := g.RunPregate(ctx); err != nil {
			t.Fatal(err)
		}
		if n := liveRuns(fe); n != 0 {
			t.Errorf("clean tree + matching SHA still skips, got %d runs", n)
		}
		g, fe, _ = pregateGate(t, " M plugin/known_embedded_versions.go\n", 0, 0, "FABRIK_PREGATE_VERIFIED_SHA="+testHEAD, "FABRIK_PREGATE_ALLOWED_DIRTY_REGEX=([unclosed")
		if err := g.RunPregate(ctx); err != nil {
			t.Fatal(err)
		}
		if n := liveRuns(fe); n != 2 {
			t.Errorf("a dirty tree under a bad regex must run the full pre-gate, got %d runs", n)
		}
	})

	t.Run("the exact cut-release.sh allowlist compiles and classifies the files it names", func(t *testing.T) {
		porcelain := "?? release-notes/v0.1.0.md\n M plugin/known_embedded_versions.go\nM  plugin/fabrik-workflows/.claude-plugin/plugin.json\n"
		g, fe, _ := pregateGate(t, porcelain, 1, 1, "FABRIK_PREGATE_VERIFIED_SHA="+testHEAD, "FABRIK_PREGATE_ALLOWED_DIRTY_REGEX="+cutReleaseAllowedRegex)
		if err := g.RunPregate(ctx); err != nil {
			t.Fatal(err)
		}
		if n := liveRuns(fe); n != 0 {
			t.Errorf("every declared self-write is allowlisted: want a skip, got %d runs", n)
		}
		g, fe, _ = pregateGate(t, porcelain+" M engine/poll.go\n", 0, 0, "FABRIK_PREGATE_VERIFIED_SHA="+testHEAD, "FABRIK_PREGATE_ALLOWED_DIRTY_REGEX="+cutReleaseAllowedRegex)
		if err := g.RunPregate(ctx); err != nil {
			t.Fatal(err)
		}
		if n := liveRuns(fe); n != 2 {
			t.Errorf("an undeclared change must still fail the dedup: want 2 runs, got %d", n)
		}
	})
}

// ---- #1973 R5: the one-shot retry of the known TSan fork/exec crash ----

type pgAttempt struct {
	out string
	rc  int
}

func pregateFixture(t *testing.T, name string) string {
	t.Helper()
	return string(fixtureBytes(t, "pregate/"+name))
}

// scriptedPregate wires a fake whose successive sim runs follow simScript and
// whose github wire-contract run follows goScript (the last entry repeats).
func scriptedPregate(t *testing.T, simScript, goScript []pgAttempt) (*Gate, *fakeExec, *bytes.Buffer) {
	t.Helper()
	g, fe, out, _ := testGate(t)
	g.Cfg.CoverageDir = t.TempDir() + "/cov"
	g.LoadAvg = func() (float64, bool) { return 3.4, true }
	var simN, goN int
	pick := func(s []pgAttempt, n *int) pgAttempt {
		i := *n
		*n++
		if i >= len(s) {
			i = len(s) - 1
		}
		return s[i]
	}
	fe.handler = func(_ context.Context, c Cmd) Result {
		switch {
		case c.Name == "git" && len(c.Args) > 0 && c.Args[0] == "rev-parse":
			writeStdout(c, testHEAD+"\n")
		case strings.HasSuffix(c.Name, "scripts/sim/run.sh"):
			a := pick(simScript, &simN)
			writeStdout(c, a.out)
			return Result{ExitCode: a.rc}
		case c.Name == "go":
			a := pick(goScript, &goN)
			writeStdout(c, a.out)
			return Result{ExitCode: a.rc}
		}
		return Result{}
	}
	return g, fe, out
}

func simRuns(fe *fakeExec) int {
	n := 0
	for _, l := range fe.lines() {
		if strings.Contains(l, "scripts/sim/run.sh") {
			n++
		}
	}
	return n
}

var okRun = []pgAttempt{{rc: 0}}

func TestPregateRetriesTheKnownCrashOnceAndRecordsIt(t *testing.T) {
	for _, crash := range []string{"tsan-abort.log", "git-segfault.log"} {
		t.Run(crash, func(t *testing.T) {
			g, fe, out := scriptedPregate(t, []pgAttempt{{out: pregateFixture(t, crash), rc: 1}, {rc: 0}}, okRun)
			if err := g.RunPregate(context.Background()); err != nil {
				t.Fatalf("a crash followed by a pass must pass: %v", err)
			}
			if n := simRuns(fe); n != 2 {
				t.Errorf("sim runs = %d, want exactly 2 (one retry)", n)
			}
			if !strings.Contains(out.String(), "retrying once (host load average 3.40)") {
				t.Errorf("the retry and the load average must be announced:\n%s", out.String())
			}
			// Recorded: the pass record carries the retry, and a note exists.
			rec, ok := g.readPregateRecord(testHEAD)
			if !ok || !rec.Retried || rec.LoadAvg != "3.40" {
				t.Errorf("pass record = %+v ok=%v", rec, ok)
			}
			note, err := os.ReadFile(g.pregateRetriesPath(testHEAD))
			if err != nil || !strings.Contains(string(note), `"outcome":"pass"`) || !strings.Contains(string(note), `"load_avg":"3.40"`) || !strings.Contains(string(note), `"step":"sim suite"`) {
				t.Errorf("retry note = %q (%v)", note, err)
			}
			if line := g.pregateLine(context.Background()); !strings.Contains(line, "after 1 TSan-crash retry, load average 3.40") {
				t.Errorf("pregateLine = %q", line)
			}
		})
	}
}

func TestPregateSecondCrashIsAHardStopAndIsStillRecorded(t *testing.T) {
	crash := pgAttempt{out: pregateFixture(t, "tsan-abort.log"), rc: 1}
	g, fe, _ := scriptedPregate(t, []pgAttempt{crash}, okRun)
	err := g.RunPregate(context.Background())
	if exitCode(err) != ExitPregateFailed {
		t.Fatalf("a second crash is a hard stop (5): %v", err)
	}
	if n := simRuns(fe); n != 2 {
		t.Errorf("sim runs = %d, want 2 — never a second retry", n)
	}
	note, _ := os.ReadFile(g.pregateRetriesPath(testHEAD))
	if !strings.Contains(string(note), `"outcome":"crashed again"`) {
		t.Errorf("a retry that fails must still be recorded: %q", note)
	}
	if _, ok := g.readPregateRecord(testHEAD); ok {
		t.Error("a failed pre-gate must leave no pass record")
	}
}

func TestPregateDoesNotRetryOrdinaryFailuresOrEnginePanics(t *testing.T) {
	for _, f := range []string{"ordinary-failure.log", "engine-panic.log", "segfault-and-panic.log", "bare-sigsegv.log"} {
		t.Run(f, func(t *testing.T) {
			g, fe, _ := scriptedPregate(t, []pgAttempt{{out: pregateFixture(t, f), rc: 1}}, okRun)
			if exitCode(g.RunPregate(context.Background())) != ExitPregateFailed {
				t.Fatal("must be a hard stop")
			}
			if n := simRuns(fe); n != 1 {
				t.Errorf("sim runs = %d, want 1 — no retry", n)
			}
			if _, err := os.Stat(g.pregateRetriesPath(testHEAD)); err == nil {
				t.Error("no retry, so no retry note")
			}
		})
	}
}

func TestPregateWireContractStepGetsTheSameTreatment(t *testing.T) {
	g, fe, _ := scriptedPregate(t, okRun, []pgAttempt{{out: pregateFixture(t, "git-segfault.log"), rc: 1}, {rc: 0}})
	if err := g.RunPregate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(fe.callsNamed("go")); n != 2 {
		t.Errorf("wire-contract runs = %d, want 2", n)
	}
	if n := simRuns(fe); n != 1 {
		t.Errorf("the passing sim step must not re-run: %d", n)
	}
	// And an ordinary wire-contract failure is not retried.
	g, fe, _ = scriptedPregate(t, okRun, []pgAttempt{{out: pregateFixture(t, "ordinary-failure.log"), rc: 1}})
	if exitCode(g.RunPregate(context.Background())) != ExitPregateFailed || len(fe.callsNamed("go")) != 1 {
		t.Error("an ordinary wire-contract failure is a hard stop with no retry")
	}
}

func TestPregateAPassOnTheFirstRunRecordsNoRetry(t *testing.T) {
	g, _, _ := scriptedPregate(t, okRun, okRun)
	if err := g.RunPregate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rec, ok := g.readPregateRecord(testHEAD); !ok || rec.Retried || rec.LoadAvg != "" {
		t.Errorf("record = %+v ok=%v", rec, ok)
	}
}
