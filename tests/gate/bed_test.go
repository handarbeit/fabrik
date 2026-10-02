package gate

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// realGate is a Gate over the real OS (real git) with an isolated environment.
func realGate(t *testing.T, bed string, env ...string) (*Gate, *bytes.Buffer) {
	t.Helper()
	var out bytes.Buffer
	cfg := Config{
		RepoRoot: t.TempDir(), TestBed: bed, EngineLog: bed + "/.fabrik/fabrik.log", BedPollSeconds: "60",
		GHAPITimeout: 30 * time.Second, PostSuiteDrainTimeout: 30 * time.Second, PostSuiteWatchdog: 300 * time.Second,
		StallCheckInterval: time.Hour, StallWarn: 15 * time.Minute,
		BannerWait: 2 * time.Second, StopWait: 2 * time.Second, KillGrace: time.Second, TmpDir: t.TempDir(),
	}
	base := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + filepath.Join(t.TempDir(), "gitconfig")}
	g := &Gate{Cfg: cfg, Out: &out, Err: &out, Exec: OSExec{}, Env: append(base, env...), Sleep: time.Sleep, Now: time.Now, Self: os.Getpid()}
	g.ProcCwd = g.defaultProcCwd
	return g, &out
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// Ported from scripts/e2e/preflight_bed_ref_test.sh (#1693): preflight_bed must
// resolve an E2E_BED_REF other than origin/main even though the bed is a
// single-branch clone, and survive a force-pushed ref.
func TestPreflightBedRefResolution(t *testing.T) {
	skipIfNoGit(t)
	scratch := t.TempDir()
	origin, seed, bed := scratch+"/origin.git", scratch+"/seed", scratch+"/bed"
	git(t, scratch, "init", "-q", "--bare", origin)
	git(t, scratch, "init", "-q", seed)
	git(t, seed, "config", "user.email", "test@example.com")
	git(t, seed, "config", "user.name", "test")
	git(t, seed, "commit", "-q", "--allow-empty", "-m", "main commit")
	git(t, seed, "branch", "-M", "main")
	git(t, seed, "remote", "add", "origin", origin)
	git(t, seed, "push", "-q", "origin", "main")
	git(t, seed, "checkout", "-q", "-b", "feature")
	git(t, seed, "commit", "-q", "--allow-empty", "-m", "feature commit")
	git(t, seed, "push", "-q", "origin", "feature")
	git(t, seed, "checkout", "-q", "main")
	mainShort := git(t, seed, "rev-parse", "main")[:7]
	featureShort := git(t, seed, "rev-parse", "feature")[:7]

	// A single-branch clone reproduces the real bed's fetch refspec — the
	// condition under which the pre-#1693 bare `git fetch origin` never saw
	// any branch but main.
	git(t, scratch, "clone", "-q", "--single-branch", "--branch", "main", origin, bed)
	git(t, bed, "config", "user.email", "test@example.com")
	git(t, bed, "config", "user.name", "test")
	if got := git(t, bed, "config", "--get-all", "remote.origin.fetch"); got != "+refs/heads/main:refs/remotes/origin/main" {
		t.Fatalf("scratch bed does not reproduce the single-branch refspec: %q", got)
	}

	preflight := func(t *testing.T, env ...string) (string, error) {
		g, out := realGate(t, bed, append([]string{"E2E_BED_NO_BUILD=1"}, env...)...)
		_, err := g.PreflightBed(context.Background())
		if err != nil {
			out.WriteString(err.Error())
		}
		return out.String(), err
	}

	t.Run("a never-fetched branch resolves to its short SHA", func(t *testing.T) {
		out, _ := preflight(t, "E2E_BED_REF=origin/feature")
		if !strings.Contains(out, featureShort) {
			t.Errorf("want %s in:\n%s", featureShort, out)
		}
		if strings.Contains(out, "git fetch failed in") {
			t.Errorf("fetch must not fail:\n%s", out)
		}
	})

	t.Run("a force-pushed branch still resolves to its new short SHA", func(t *testing.T) {
		git(t, seed, "checkout", "-q", "feature")
		git(t, seed, "reset", "-q", "--hard", "HEAD~1")
		git(t, seed, "commit", "-q", "--allow-empty", "-m", "feature commit rewritten")
		git(t, seed, "push", "-q", "-f", "origin", "feature")
		git(t, seed, "checkout", "-q", "main")
		rewritten := git(t, seed, "rev-parse", "feature")[:7]
		out, _ := preflight(t, "E2E_BED_REF=origin/feature")
		if !strings.Contains(out, rewritten) {
			t.Errorf("want %s in:\n%s", rewritten, out)
		}
		if strings.Contains(out, "git fetch failed in") {
			t.Errorf("a non-fast-forward fetch needs the leading +:\n%s", out)
		}
	})

	t.Run("a bogus ref exits 4 with the wrapped message", func(t *testing.T) {
		out, err := preflight(t, "E2E_BED_REF=origin/does-not-exist")
		if exitCode(err) != ExitPreflightFailed {
			t.Fatalf("want exit %d, got %v", ExitPreflightFailed, err)
		}
		for _, want := range []string{"does-not-exist", bed, "bed is untouched"} {
			if !strings.Contains(out, want) {
				t.Errorf("missing %q in:\n%s", want, out)
			}
		}
		if strings.Contains(out, "ambiguous argument") {
			t.Errorf("a bare unwrapped rev-parse failure leaked:\n%s", out)
		}
	})

	t.Run("an unset ref defaults to origin/main", func(t *testing.T) {
		out, _ := preflight(t)
		if strings.Contains(out, "git fetch failed in") {
			t.Errorf("fetch must not fail:\n%s", out)
		}
		if !strings.Contains(out, mainShort) || !strings.Contains(out, "target ref origin/main") {
			t.Errorf("want main's short SHA %s and the default ref in:\n%s", mainShort, out)
		}
	})

	t.Run("modified tracked files in the bed are refused, untracked are not", func(t *testing.T) {
		mustWrite(t, bed+"/untracked.log", "x")
		out, err := preflight(t)
		if strings.Contains(out, "modified tracked files") {
			t.Errorf("untracked files must be ignored:\n%s", out)
		}
		_ = err
		git(t, bed, "commit", "-q", "--allow-empty", "-m", "bed-local") // keep a tracked file to modify below
		mustWrite(t, bed+"/tracked.txt", "v1")
		git(t, bed, "add", "tracked.txt")
		git(t, bed, "commit", "-q", "-m", "add tracked")
		mustWrite(t, bed+"/tracked.txt", "v2")
		out, err = preflight(t)
		if exitCode(err) != ExitPreflightFailed || !strings.Contains(out, "has modified tracked files") {
			t.Errorf("want a refusal with exit 4, got %v:\n%s", err, out)
		}
		git(t, bed, "checkout", "--", "tracked.txt")
		git(t, bed, "reset", "-q", "--hard", "origin/main")
	})

	t.Run("a bed that is not a git checkout fails preflight", func(t *testing.T) {
		g, out := realGate(t, t.TempDir())
		_, err := g.PreflightBed(context.Background())
		if exitCode(err) != ExitPreflightFailed || !strings.Contains(err.Error(), "is not a git checkout") {
			t.Errorf("got %v\n%s", err, out)
		}
	})
}

// Ported from scripts/e2e/bed_gitconfig_isolation_test.sh (#1756/R5, AC4).
func TestWriteIsolatedBedGitconfig(t *testing.T) {
	skipIfNoGit(t)
	work := t.TempDir()
	host := work + "/host-gitconfig"
	outCfg := work + "/bed-isolated-gitconfig"
	mustWrite(t, host, `[user]
	name = Test Operator
	email = operator@example.com
[credential]
	helper = osxkeychain
[credential "https://github.com"]
	helper = !gh auth git-credential
[url "git@github.com:"]
	insteadOf = https://github.com/
`)
	g, _ := realGate(t, t.TempDir())
	g.Env = withEnv(g.Env, "GIT_CONFIG_GLOBAL="+host, "GIT_CONFIG_NOSYSTEM=1")

	if err := g.WriteIsolatedBedGitconfig(context.Background(), outCfg); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(outCfg)
	got := string(data)
	for _, want := range []string{"osxkeychain", "gh auth git-credential"} {
		if !strings.Contains(got, want) {
			t.Errorf("credential entry %q must be preserved:\n%s", want, got)
		}
	}
	for _, bad := range []string{"insteadOf", "git@github.com", "Test Operator"} {
		if strings.Contains(got, bad) {
			t.Errorf("%q must be dropped (insteadOf would mask the App-auth+HTTPS worker-git failure, ADR-1756):\n%s", bad, got)
		}
	}

	// A host with no credential.* entries yields an EMPTY file, and a stale
	// previous file is overwritten, not left behind.
	mustWrite(t, host, "")
	mustWrite(t, outCfg, "[credential]\n\thelper = stale\n")
	if err := g.WriteIsolatedBedGitconfig(context.Background(), outCfg); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(outCfg); len(data) != 0 {
		t.Errorf("expected an empty generated config, got:\n%s", data)
	}
}

// The runner and tests/e2e/lifecycle.go's StartFabrikTestBed launch the bed
// independently (the runner is untagged and cannot import the e2e package) and
// must agree on three contracts. This pins the runner's half; lifecycle.go has
// the reciprocal comment.
func TestBedStartCmdContracts(t *testing.T) {
	g, _, _, _ := testGate(t,
		"FABRIK_GITHUB_APP_ID=1", "FABRIK_GITHUB_APP_PRIVATE_KEY_PATH=/k", "FABRIK_GITHUB_APP_INSTALLATION_ID=2",
		"FABRIK_TOKEN=keep", "GIT_CONFIG_GLOBAL=/operator/gitconfig")
	g.Cfg.BedPollSeconds = "75"
	c := g.BedStartCmd("/bed/.fabrik/git-config-isolated")

	if got := strings.Join(c.Args, " "); got != "-notui -poll 75" {
		t.Errorf("argv = %q, want -notui -poll <secs> (and no --auto-upgrade)", got)
	}
	if c.Dir != g.Cfg.TestBed || c.Name != "./fabrik" {
		t.Errorf("must run ./fabrik from the bed: name=%q dir=%q", c.Name, c.Dir)
	}
	envOf := func(k string) []string {
		var vs []string
		for _, kv := range c.Env {
			if strings.HasPrefix(kv, k+"=") {
				vs = append(vs, kv)
			}
		}
		return vs
	}
	if got := envOf("GIT_CONFIG_GLOBAL"); len(got) != 1 || got[0] != "GIT_CONFIG_GLOBAL=/bed/.fabrik/git-config-isolated" {
		t.Errorf("the isolated gitconfig must replace the operator's: %v", got)
	}
	if got := envOf("GIT_CONFIG_NOSYSTEM"); len(got) != 1 || got[0] != "GIT_CONFIG_NOSYSTEM=1" {
		t.Errorf("GIT_CONFIG_NOSYSTEM: %v", got)
	}
	for _, k := range bedAppEnvKeys {
		if got := envOf(k); len(got) != 0 {
			t.Errorf("%s must not reach the bed (auth mode is applied per leg through its .env): %v", k, got)
		}
	}
	if got := envOf("FABRIK_TOKEN"); len(got) != 1 {
		t.Errorf("unrelated env must pass through, got %v", got)
	}
}

func TestStartBedBannerChecks(t *testing.T) {
	setup := func(t *testing.T, banner string) (*Gate, *fakeExec) {
		g, fe, _, _ := testGate(t)
		fe.handler = func(_ context.Context, c Cmd) Result { return Result{} }
		// The fake Start writes the banner where the real bed would.
		g.Exec = &startWriter{fakeExec: fe, banner: banner, log: g.Cfg.TestBed + "/bed-run.log"}
		return g, fe
	}
	t.Run("a banner naming the ref passes and bed-run.log is truncated once", func(t *testing.T) {
		g, _ := setup(t, "Fabrik starting 0.1.0 (abc1234)")
		mustWrite(t, g.Cfg.TestBed+"/bed-run.log", "stale content from a previous run\n")
		if err := g.StartBed(context.Background(), "abc1234"); err != nil {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(g.Cfg.TestBed + "/bed-run.log")
		if strings.Contains(string(data), "stale") {
			t.Errorf("bed-run.log must be truncated at start: %q", data)
		}
	})
	t.Run("a banner naming another ref fails with exit 4", func(t *testing.T) {
		g, _ := setup(t, "Fabrik starting 0.1.0 (def5678)")
		err := g.StartBed(context.Background(), "abc1234")
		if exitCode(err) != ExitPreflightFailed || !strings.Contains(err.Error(), "ref under test is abc1234") {
			t.Errorf("got %v", err)
		}
	})
	t.Run("no banner within the wait fails with exit 4", func(t *testing.T) {
		g, _ := setup(t, "")
		err := g.StartBed(context.Background(), "abc1234")
		if exitCode(err) != ExitPreflightFailed || !strings.Contains(err.Error(), "did not report startup within") {
			t.Errorf("got %v", err)
		}
	})
	t.Run("the banner is also found in the engine log", func(t *testing.T) {
		g, _ := setup(t, "")
		mustWrite(t, g.Cfg.EngineLog, "Fabrik starting (abc1234)\n")
		if err := g.StartBed(context.Background(), "abc1234"); err != nil {
			t.Fatal(err)
		}
	})
}

// startWriter wraps fakeExec so Start() plays the bed: it writes its banner into
// the log file the runner handed it.
type startWriter struct {
	*fakeExec
	banner, log string
}

func (s *startWriter) Start(c Cmd) error {
	if err := s.fakeExec.Start(c); err != nil {
		return err
	}
	if f, ok := c.Stdout.(*os.File); ok && s.banner != "" {
		_, err := f.WriteString(s.banner + "\n")
		return err
	}
	return nil
}

func TestStopBedInstance(t *testing.T) {
	t.Run("no lock file is a no-op", func(t *testing.T) {
		g, _, out, _ := testGate(t)
		if err := g.StopBedInstance(context.Background()); err != nil || out.Len() != 0 {
			t.Errorf("err=%v out=%q", err, out)
		}
	})
	t.Run("a stale lock naming a dead pid is a no-op", func(t *testing.T) {
		g, _, out, _ := testGate(t)
		mustWrite(t, g.Cfg.TestBed+"/.fabrik/fabrik.lock", "999999\n")
		if err := g.StopBedInstance(context.Background()); err != nil || out.Len() != 0 {
			t.Errorf("err=%v out=%q", err, out)
		}
	})
	t.Run("a live pid is SIGTERMed and waited for", func(t *testing.T) {
		g, _, out, _ := testGate(t)
		g.Sleep = time.Sleep
		g.Cfg.StopWait = 5 * time.Second
		cmd := exec.Command("sleep", "30")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() { cmd.Wait(); close(done) }() // reap it, so the pid disappears once TERMed
		defer cmd.Process.Kill()
		mustWrite(t, g.Cfg.TestBed+"/.fabrik/fabrik.lock", strconv.Itoa(cmd.Process.Pid)+"\n")
		if err := g.StopBedInstance(context.Background()); err != nil {
			t.Fatal(err)
		}
		<-done
		if !strings.Contains(out.String(), "stopping running bed instance") {
			t.Errorf("out=%q", out)
		}
	})
	t.Run("a pid that ignores SIGTERM fails preflight after the wait", func(t *testing.T) {
		g, _, _, _ := testGate(t)
		g.Sleep = time.Sleep
		g.Cfg.StopWait = 2 * time.Second
		cmd := exec.Command("sh", "-c", "trap '' TERM; while :; do sleep 1; done")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		go cmd.Wait()
		defer cmd.Process.Kill()
		time.Sleep(200 * time.Millisecond) // let the trap install
		mustWrite(t, g.Cfg.TestBed+"/.fabrik/fabrik.lock", strconv.Itoa(cmd.Process.Pid)+"\n")
		err := g.StopBedInstance(context.Background())
		if exitCode(err) != ExitPreflightFailed || !strings.Contains(err.Error(), "did not exit within 2s") {
			t.Errorf("got %v", err)
		}
	})
}
