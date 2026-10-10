package gate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/handarbeit/fabrik/internal/pollctl"
)

// The bed (~/dev/fabrik-test) is a full fabrik source checkout with its own
// built binary, and NOTHING in the live suite rebuilds it. That is a silent
// correctness hole: a stale bed produces a run that looks exactly like a real
// one while testing an engine nobody asked about (a bed once sat 194 commits
// behind main). The steps in this file encode what used to be tribal knowledge
// so the default path is the correct one.

func (g *Gate) bedGit(ctx context.Context, args ...string) (string, string, Result) {
	return output(ctx, g.Exec, Cmd{Name: "git", Args: args, Dir: g.Cfg.TestBed, Env: g.Env})
}

func preflightFail(format string, args ...any) *ExitError {
	return exitErr(ExitPreflightFailed, format, args...)
}

// PreflightBed is run.sh's preflight_bed: guarantee the bed is running the
// engine we mean to test — fast-forward its checkout to the ref under test,
// rebuild its binary IN PLACE, verify the binary carries that ref, report
// stage-config drift, and leave the bed STOPPED (reset refuses a live one).
// Returns the short SHA of the ref under test.
//
// Knobs: E2E_BED_REF (default origin/main); E2E_BED_NO_BUILD=1 verifies and
// reports only — never stops, builds or starts — and fails loud on a mismatch.
func (g *Gate) PreflightBed(ctx context.Context) (wantShort string, err error) {
	ref := g.Getenv("E2E_BED_REF")
	if ref == "" {
		ref = "origin/main"
	}
	// $ref is always origin/<name>; strip only the literal "origin/" prefix, not
	// the last path segment, since branch names can themselves contain slashes.
	refName := strings.TrimPrefix(ref, "origin/")
	bed := g.Cfg.TestBed
	noBuild := g.Getenv("E2E_BED_NO_BUILD") != ""

	g.outf("== preflight: bed at %s, target ref %s ==\n", bed, ref)

	if st, serr := os.Stat(filepath.Join(bed, ".git")); serr != nil || !st.IsDir() {
		return "", preflightFail("preflight: %s is not a git checkout — see tests/e2e/README.md", bed)
	}

	// Never clobber uncommitted engine work in the bed. Untracked files (logs,
	// .env backups) are normal there and deliberately ignored; modified TRACKED
	// files usually mean someone is mid-experiment and must not be reset.
	so, _, res := g.bedGit(ctx, "status", "--porcelain", "--untracked-files=no")
	if res.ExitCode != 0 {
		return "", preflightFail("preflight: git status failed in %s", bed)
	}
	if dirty := strings.TrimRight(so, "\n"); dirty != "" {
		return "", preflightFail("preflight: %s has modified tracked files — refusing to update it.\n%s\nCommit, stash, or revert them, or re-run with E2E_SKIP_PREP=1.", bed, dirty)
	}

	// Fetch with an explicit destination refspec rather than a bare `git fetch
	// origin`: the bed is a single-branch clone, so a bare fetch only ever
	// updates origin/main and silently fails to resolve any other ref. The
	// leading `+` is load-bearing — without it fetch refuses a non-fast-forward
	// update, whereas the remote's own configured refspec is +-prefixed, so a
	// rebased/force-pushed upstream ref would fail here (PR #1700).
	if _, _, res := g.bedGit(ctx, "fetch", "origin", "--quiet", fmt.Sprintf("+%s:refs/remotes/origin/%s", refName, refName)); res.ExitCode != 0 {
		return "", preflightFail("preflight: git fetch failed in %s — cannot resolve %s.\n"+
			"  Two likely causes:\n"+
			"  - The SSH key for the remote is not loaded: try 'ssh-add' (see 'ssh-add -l').\n"+
			"  - '%s' does not exist on origin (e.g. not pushed, or misspelled).\n"+
			"  The bed is untouched; nothing was stopped, rebuilt, or reset.", bed, ref, refName)
	}

	so, _, res = g.bedGit(ctx, "rev-parse", ref)
	want := strings.TrimSpace(so)
	if res.ExitCode != 0 || want == "" {
		return "", preflightFail("preflight: cannot resolve %s in %s", ref, bed)
	}
	wantShort = shortSHA(want)
	g.engineSHA = want // the coverage ledger's key (#1972)
	so, _, res = g.bedGit(ctx, "rev-parse", "HEAD")
	have := strings.TrimSpace(so)
	if res.ExitCode != 0 || have == "" {
		return "", preflightFail("preflight: cannot resolve HEAD in %s", bed)
	}

	if have != want {
		// Report both directions: the target is usually ahead (a stale bed), but
		// it can equally be an ancestor (deliberately testing an older ref), and
		// "0 commits behind" alone reads as a contradiction of the mismatch.
		ahead, behind := "?", "?"
		if so, _, res := g.bedGit(ctx, "rev-list", "--left-right", "--count", "HEAD..."+want); res.ExitCode == 0 {
			if f := strings.Fields(so); len(f) >= 2 {
				ahead, behind = f[0], f[1]
			}
		}
		divergence := fmt.Sprintf("%s commit(s) behind, %s ahead", behind, ahead)
		if noBuild {
			return "", preflightFail("preflight: bed is at %s, want %s (%s) and E2E_BED_NO_BUILD is set", shortSHA(have), wantShort, divergence)
		}
		g.outf("   bed checkout %s is %s vs %s — updating to %s\n", shortSHA(have), divergence, ref, wantShort)
		if _, se, res := g.bedGit(ctx, "checkout", "--quiet", "--detach", want); res.ExitCode != 0 {
			return "", preflightFail("preflight: git checkout %s failed in %s: %s", wantShort, bed, strings.TrimSpace(se))
		}
	} else {
		g.outf("   bed checkout already at %s\n", wantShort)
	}

	// Build IN PLACE. A binary built elsewhere and copied in can be SIGKILL'd on
	// Apple Silicon (README item 17), so this is not merely a convenience.
	if !noBuild {
		g.outln("   building bed binary in place")
		res := g.Exec.Run(ctx, Cmd{Name: "go", Args: []string{"build", "-o", "fabrik", "."}, Dir: bed, Env: g.Env, Stdout: g.Out, Stderr: g.Err})
		if res.ExitCode != 0 {
			return "", preflightFail("preflight: building the bed binary failed (exit %d)", res.ExitCode)
		}
	}

	// Fail loud if the binary does not actually carry the ref under test — the
	// check whose absence made the 194-commit run possible.
	so, se, _ := output(ctx, g.Exec, Cmd{Name: "./fabrik", Args: []string{"--version"}, Dir: bed, Env: g.Env})
	ver, _, _ := strings.Cut(so+se, "\n")
	if !strings.Contains(ver, wantShort) {
		return "", preflightFail("preflight: bed binary reports '%s' but the ref under test is %s", ver, wantShort)
	}
	g.outf("   bed binary: %s\n", ver)

	// Stage-config drift is reported, never auto-applied: .fabrik/stages/ is bed
	// configuration, and silently rewriting it could change what the suite means.
	so, se, _ = output(ctx, g.Exec, Cmd{Name: "./fabrik", Args: []string{"refresh-stages"}, Dir: bed, Env: g.Env})
	if drift := strings.TrimRight(so+se, "\n"); drift != "" {
		g.outln("   stage-config drift detected (not applied — run 'fabrik refresh-stages --apply' in the bed if intended):")
		for _, line := range strings.Split(drift, "\n") {
			g.outf("     %s\n", line)
		}
	} else {
		g.outln("   stage configs current")
	}

	// Verify-only mode inspects and reports; it never stops, builds, or starts
	// anything, so a bed someone else is driving stays untouched.
	if noBuild {
		g.outln("== preflight (verify-only) complete — bed left as-is ==")
		return wantShort, nil
	}

	// Leave the bed STOPPED on exit. reset refuses to run against a live
	// instance, and a running engine would still be holding the pre-build binary
	// anyway — so the instance is started by StartBed only after any --clean
	// reset has run.
	if err := g.StopBedInstance(ctx); err != nil {
		return "", err
	}
	g.outln("== preflight (build) complete — bed stopped, ready for reset ==")
	return wantShort, nil
}

func shortSHA(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}

// StopBedInstance is run.sh's stop_bed_instance: terminate a running bed engine
// and wait for it to exit. SIGTERM, not SIGKILL: the engine's clean-stop path
// (#1393) durably pauses in-flight issues rather than abandoning them mid-stage.
func (g *Gate) StopBedInstance(ctx context.Context) error {
	pid := lockedBedPID(g.Cfg.TestBed)
	if pid == 0 {
		return nil
	}
	g.outf("   stopping running bed instance (pid %d)\n", pid)
	termPID(pid)
	waited := 0
	for pidAlive(pid) && waited < int(g.Cfg.StopWait.Seconds()) {
		g.Sleep(time.Second)
		waited++
	}
	if pidAlive(pid) {
		return preflightFail("preflight: bed instance %d did not exit within %ds", pid, int(g.Cfg.StopWait.Seconds()))
	}
	return nil
}

// lockedBedPID is the live PID in the bed's .fabrik/fabrik.lock, or 0 when there
// is no lock, it is unreadable, or it names no live process.
func lockedBedPID(bed string) int {
	data, err := os.ReadFile(filepath.Join(bed, ".fabrik", "fabrik.lock"))
	if err != nil {
		return 0
	}
	pidStr := strings.TrimSpace(string(data))
	pid, perr := strconv.Atoi(pidStr)
	if pidStr == "" || perr != nil || !pidAlive(pid) {
		return 0
	}
	return pid
}

// bedEngineRunning reports whether the bed's engine holds its lock.
func bedEngineRunning(bed string) bool { return lockedBedPID(bed) != 0 }

// WriteIsolatedBedGitconfig is run.sh's write_isolated_bed_gitconfig: write a
// bed-local git config at out containing only the operating user's credential.*
// entries (helper registrations, host-scoped overrides) copied verbatim from
// their real --global config — never any url.*.insteadOf rewrite (#1756/R5).
// The bed daemon is launched with GIT_CONFIG_GLOBAL pointed at this file (plus
// GIT_CONFIG_NOSYSTEM=1), isolating its git behaviour from the operator's own
// ~/.gitconfig. A naive GIT_CONFIG_GLOBAL=/dev/null would also strip
// credential.helper and break the bed's PAT-mode HTTPS git; this keeps exactly
// the piece PAT mode needs while dropping the insteadOf rewrite that silently
// masked the App-auth+HTTPS worker-git 403 (ADR-1756, AC4).
//
// tests/e2e/lifecycle.go's bedGitConfigIsolationEnv reads the file this writes.
func (g *Gate) WriteIsolatedBedGitconfig(ctx context.Context, out string) error {
	if err := os.WriteFile(out, nil, 0o644); err != nil {
		return err
	}
	so, _, _ := output(ctx, g.Exec, Cmd{Name: "git", Args: []string{"config", "--global", "--get-regexp", `^credential\.`}, Env: g.Env})
	for _, line := range strings.Split(so, "\n") {
		key, value, _ := strings.Cut(strings.TrimSpace(line), " ")
		if key == "" {
			continue
		}
		value = strings.TrimLeft(value, " \t")
		if _, se, res := output(ctx, g.Exec, Cmd{Name: "git", Args: []string{"config", "--file", out, "--add", key, value}, Env: g.Env}); res.ExitCode != 0 {
			return fmt.Errorf("writing isolated bed gitconfig %s: git config --add %s: %s", out, key, strings.TrimSpace(se))
		}
	}
	return nil
}

// bedAppEnvKeys are the App-auth variables the bed daemon must NOT inherit from
// the operator's shell; auth mode is applied per leg through the bed's .env.
var bedAppEnvKeys = []string{"FABRIK_GITHUB_APP_ID", "FABRIK_GITHUB_APP_PRIVATE_KEY_PATH", "FABRIK_GITHUB_APP_INSTALLATION_ID"}

// BedStartCmd is the command that launches the bed engine, exposed so a test
// can assert it agrees with tests/e2e/lifecycle.go's StartFabrikTestBed on the
// three shared contracts: `-notui -poll`, the isolated gitconfig, and the
// unset FABRIK_GITHUB_APP_* variables. No --auto-upgrade: it would replace the
// freshly built binary with a release mid-suite (README item 17). -poll is
// passed explicitly because both launch sites must agree: the harness restarts
// the bed through the Go path on every mode switch, so a cadence set only here
// would be silently reverted for the legs that actually matter.
func (g *Gate) BedStartCmd(isolatedGitconfig string) Cmd {
	// pollctl.Env enables the bed-only poll hold/trigger seam (#1978, ADR-1978).
	// Enabled-but-released the bed is a normal free-running one, so every phase
	// shares this one start; lifecycle.go's StartFabrikTestBed appends the same
	// entry so a restarted bed cannot silently lose it.
	//
	// FABRIK_MAX_TRAIN_AUTO_REPAIR_ATTEMPTS=0 (#2045) disables red-singleton auto-repair
	// on the bed: the live red-singleton test asserts the ADR-1545 pause, and the
	// auto-repair path is covered by its sim twin. lifecycle.go appends the same entry.
	//
	// FABRIK_MERGE_TRAIN_OVERLAP_IGNORE=** (#2047) makes every path "ignored", which turns
	// the overlap-aware batch filter off on the bed: the live conflict-resolution and
	// bisection tests deliberately batch members that write the same path. The filter is
	// covered by its sim twin. lifecycle.go appends the same entry.
	env := withEnv(withoutEnv(g.Env, bedAppEnvKeys...), "GIT_CONFIG_GLOBAL="+isolatedGitconfig, "GIT_CONFIG_NOSYSTEM=1", pollctl.Env(g.Cfg.TestBed), "FABRIK_MAX_TRAIN_AUTO_REPAIR_ATTEMPTS=0", "FABRIK_MERGE_TRAIN_OVERLAP_IGNORE=**")
	return Cmd{
		Name: "./fabrik",
		Args: []string{"-notui", "-poll", g.Cfg.BedPollSeconds},
		Dir:  g.Cfg.TestBed,
		Env:  env,
	}
}

// StartBed is run.sh's preflight_bed_start: bring the bed engine up on the
// freshly built binary and refuse to continue unless its own startup banner
// names the ref under test. Runs after any --clean reset (reset needs a stopped
// instance). bed-run.log is truncated here, once; every harness restart appends.
func (g *Gate) StartBed(ctx context.Context, wantShort string) error {
	bed := g.Cfg.TestBed
	isolated := filepath.Join(bed, ".fabrik", "git-config-isolated")
	if err := os.MkdirAll(filepath.Join(bed, ".fabrik"), 0o755); err != nil {
		return preflightFail("preflight: %v", err)
	}
	if err := g.WriteIsolatedBedGitconfig(ctx, isolated); err != nil {
		return preflightFail("preflight: %v", err)
	}

	g.outf("== preflight: starting bed instance (-notui -poll %ss, no --auto-upgrade) ==\n", g.Cfg.BedPollSeconds)
	bedStdout := filepath.Join(bed, "bed-run.log")
	logf, err := os.OpenFile(bedStdout, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return preflightFail("preflight: %v", err)
	}
	cmd := g.BedStartCmd(isolated)
	cmd.Stdout, cmd.Stderr = logf, logf
	serr := g.Exec.Start(cmd)
	cerr := logf.Close()
	if serr != nil {
		return preflightFail("preflight: starting the bed failed: %v", serr)
	}
	if cerr != nil {
		return preflightFail("preflight: closing %s after starting the bed failed: %v", bedStdout, cerr)
	}

	// The startup banner goes to the engine's STDOUT (captured in bed-run.log),
	// while the engine log holds the structured per-item log, which never
	// contains it. Check both rather than picking one: which stream carries it is
	// exactly the kind of detail that drifts, and getting it wrong here fails the
	// run for a bed that is in fact perfectly healthy.
	for i := 0; i < int(g.Cfg.BannerWait.Seconds()); i++ {
		if g.bedBanner(bedStdout) != "" {
			break
		}
		g.Sleep(time.Second)
	}
	banner := g.bedBanner(bedStdout)
	if banner == "" {
		return preflightFail("preflight: bed did not report startup within %ds — see %s and %s", int(g.Cfg.BannerWait.Seconds()), bedStdout, g.Cfg.EngineLog)
	}
	if !strings.Contains(banner, wantShort) {
		return preflightFail("preflight: running bed reports '%s' but the ref under test is %s", banner, wantShort)
	}
	g.outf("   bed running: %s\n", banner)
	return nil
}

// bedBanner is the first "Fabrik starting" line, looking in bed-run.log first
// and the engine log second (grep -hm1 over both, head -1).
func (g *Gate) bedBanner(bedStdout string) string {
	for _, path := range []string{bedStdout, g.Cfg.EngineLog} {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			if strings.Contains(line, "Fabrik starting") {
				return line
			}
		}
	}
	return ""
}

// PrepareBedAndReset is run.sh's prepare_bed_and_reset: the side-effecting
// pre-run sequence. Ordering is load-bearing: preflight leaves the bed stopped,
// the reset runs against the stopped bed (it refuses a live one), then the
// engine starts. E2E_SKIP_PREP skips preflight (and the start); E2E_BED_NO_BUILD
// skips only the start.
func (g *Gate) PrepareBedAndReset(ctx context.Context, clean bool) error {
	skipPrep := g.Getenv("E2E_SKIP_PREP") != ""
	var wantShort string
	if skipPrep {
		g.outln("== preflight skipped (E2E_SKIP_PREP set) — bed assumed prepared ==")
	} else {
		var err error
		if wantShort, err = g.PreflightBed(ctx); err != nil {
			return err
		}
	}

	if clean {
		g.outln("== --clean: resetting the test bed via scripts/e2e/reset.sh ==")
		if err := g.Reset(ctx, ResetOptions{}); err != nil {
			return err
		}
		g.outln("== reset complete ==")
	}

	if !skipPrep && g.Getenv("E2E_BED_NO_BUILD") == "" {
		return g.StartBed(ctx, wantShort)
	}
	return nil
}
