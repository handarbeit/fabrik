package gate

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
)

// Ported from scripts/e2e/auth_mode_check_test.sh.

func TestResolveAuthModes(t *testing.T) {
	cases := []struct {
		in   string
		want string
		fail bool
	}{
		{"", "pat app", false},  // unset runs both, pat first
		{"pat", "pat", false},   // E2E_AUTH_MODE=pat
		{"app", "app", false},   // E2E_AUTH_MODE=app
		{"APP", "app", false},   // case-insensitive
		{" pat ", "pat", false}, // trimmed
		{"p at", "", true},      // internal whitespace is not stripped (matches Go's TrimSpace)
		{"both", "", true},      // invalid fails
	}
	for _, c := range cases {
		got, err := ResolveAuthModes(c.in)
		if c.fail {
			if err == nil {
				t.Errorf("ResolveAuthModes(%q) = %v, want an error", c.in, got)
			} else if !strings.Contains(err.Error(), "is invalid (must be pat, app, or unset for both)") {
				t.Errorf("ResolveAuthModes(%q) error = %q", c.in, err)
			}
			continue
		}
		if err != nil || strings.Join(got, " ") != c.want {
			t.Errorf("ResolveAuthModes(%q) = %v, %v; want %q", c.in, got, err, c.want)
		}
	}
}

func TestAuthModeProblems(t *testing.T) {
	bed := t.TempDir()
	mustWrite(t, bed+"/.fabrik/config.yaml", "owner: handarbeit\n")
	mustWrite(t, bed+"/.env", "FABRIK_TOKEN=shared\n")

	if p := AuthModeProblems(bed, []string{"pat"}); len(p) != 0 {
		t.Errorf("pat-only with a neutral config: want no problems, got %v", p)
	}
	out := strings.Join(AuthModeProblems(bed, []string{"pat", "app"}), "\n")
	for _, want := range []string{"E2E_APP_ID is not set", "E2E_APP_INSTALLATION_ID is not set"} {
		if !strings.Contains(out, want) {
			t.Errorf("app leg without identity: missing %q in %q", want, out)
		}
	}

	mustWrite(t, bed+"/.fabrik/key.pem", "")
	mustWrite(t, bed+"/.env", "FABRIK_TOKEN=shared\nE2E_APP_ID=4960842\nE2E_APP_PRIVATE_KEY_PATH=.fabrik/key.pem\nE2E_APP_INSTALLATION_ID=162085522\n")
	if p := AuthModeProblems(bed, []string{"pat", "app"}); len(p) != 0 {
		t.Errorf("full identity and readable key: want no problems, got %v", p)
	}

	mustWrite(t, bed+"/.env", "FABRIK_TOKEN=shared\nE2E_APP_ID=4960842\nE2E_APP_PRIVATE_KEY_PATH=.fabrik/missing.pem\nE2E_APP_INSTALLATION_ID=162085522\n")
	if out := strings.Join(AuthModeProblems(bed, []string{"app"}), "\n"); !strings.Contains(out, "not a readable file") {
		t.Errorf("unreadable key file: want a problem, got %q", out)
	}
	if p := AuthModeProblems(bed, []string{"pat"}); len(p) != 0 {
		t.Errorf("unreadable key is irrelevant to a pat-only run, got %v", p)
	}

	mustWrite(t, bed+"/.fabrik/config.yaml", "owner: handarbeit\n# github_app_id: 1   (commented out is fine)\n")
	if p := AuthModeProblems(bed, []string{"pat"}); len(p) != 0 {
		t.Errorf("commented-out github_app_id is not a problem, got %v", p)
	}
	mustWrite(t, bed+"/.fabrik/config.yaml", "owner: handarbeit\ngithub_app_id: 4960842\n")
	if out := strings.Join(AuthModeProblems(bed, []string{"pat"}), "\n"); !strings.Contains(out, "sets github_app_* keys") {
		t.Errorf("config.yaml github_app_id is a problem even for pat, got %q", out)
	}
}

func TestEnvFileLastValueQuotes(t *testing.T) {
	f := t.TempDir() + "/.env"
	mustWrite(t, f, "E2E_APP_ID=1\nE2E_APP_ID=\"2\"\nOTHER='x'\n")
	if got := envFileLastValue(f, "E2E_APP_ID"); got != "2" {
		t.Errorf("last value, unquoted: got %q, want 2", got)
	}
}

func TestCheckAuthModePreconditionsRefuses(t *testing.T) {
	g, _, out, errb := testGate(t)
	err := g.CheckAuthModePreconditions([]string{"pat", "app"})
	if exitCode(err) != ExitPreconditionFailed {
		t.Fatalf("want exit %d, got %v", ExitPreconditionFailed, err)
	}
	msg := err.Error()
	for _, want := range []string{"PRECONDITION FAILED: auth-mode legs (pat app) cannot run (#1861)", "E2E_APP_ID is not set", "Or set E2E_AUTH_MODE=pat"} {
		if !strings.Contains(msg, want) {
			t.Errorf("banner missing %q:\n%s", want, msg)
		}
	}
	if out.Len() != 0 || errb.Len() != 0 {
		t.Errorf("the banner is the error's message; nothing else should be printed (out=%q err=%q)", out, errb)
	}
	if err := g.CheckAuthModePreconditions([]string{"pat"}); err != nil {
		t.Errorf("pat-only on an empty bed: %v", err)
	}
	if !strings.Contains(out.String(), "== auth-mode legs: pat ==") {
		t.Errorf("missing the legs banner, got %q", out)
	}
}

// Ported from scripts/e2e/token_consumer_check_test.sh.

func TestFindCompetingTokenConsumers(t *testing.T) {
	bed, other, noEnv := t.TempDir(), t.TempDir(), t.TempDir()
	mustWrite(t, bed+"/.env", "FABRIK_TOKEN=shared-token-abc\n")
	mustWrite(t, other+"/.env", "FABRIK_TOKEN=shared-token-abc\n")
	tok := "shared-token-abc"

	t.Run("matching-token competitor is reported", func(t *testing.T) {
		got := FindCompetingTokenConsumers(bed, tok, []Candidate{{1111, other}})
		if len(got) != 1 || got[0] != (Candidate{1111, other}) {
			t.Errorf("got %v", got)
		}
	})
	t.Run("mismatched token is not a match", func(t *testing.T) {
		mustWrite(t, other+"/.env", "FABRIK_TOKEN=a-different-token\n")
		defer mustWrite(t, other+"/.env", "FABRIK_TOKEN=shared-token-abc\n")
		if got := FindCompetingTokenConsumers(bed, tok, []Candidate{{1111, other}}); len(got) != 0 {
			t.Errorf("got %v", got)
		}
	})
	t.Run("the bed's own directory is excluded", func(t *testing.T) {
		if got := FindCompetingTokenConsumers(bed, tok, []Candidate{{2222, bed}}); len(got) != 0 {
			t.Errorf("got %v", got)
		}
	})
	t.Run("the bed's own directory is excluded by resolved path", func(t *testing.T) {
		link := t.TempDir() + "/bedlink"
		if err := os.Symlink(bed, link); err != nil {
			t.Skip("symlinks unavailable")
		}
		if got := FindCompetingTokenConsumers(bed, tok, []Candidate{{2222, link}}); len(got) != 0 {
			t.Errorf("a symlink to the bed is still the bed, got %v", got)
		}
	})
	t.Run("a candidate with no .env is excluded", func(t *testing.T) {
		if got := FindCompetingTokenConsumers(bed, tok, []Candidate{{3333, noEnv}}); len(got) != 0 {
			t.Errorf("got %v", got)
		}
	})
	t.Run("an empty candidate list matches nothing", func(t *testing.T) {
		if got := FindCompetingTokenConsumers(bed, tok, nil); len(got) != 0 {
			t.Errorf("got %v", got)
		}
	})
	t.Run("mixed candidates report only the genuine competitor", func(t *testing.T) {
		got := FindCompetingTokenConsumers(bed, tok, []Candidate{{2222, bed}, {3333, noEnv}, {1111, other}})
		if len(got) != 1 || got[0].PID != 1111 {
			t.Errorf("got %v", got)
		}
	})
}

func TestCheckCompetingTokenConsumers(t *testing.T) {
	other := t.TempDir()
	mustWrite(t, other+"/.env", "FABRIK_TOKEN=shared\n")
	discover := func(g *Gate) {
		g.Exec.(*fakeExec).handler = func(_ context.Context, c Cmd) Result {
			if c.Name == "pgrep" {
				writeStdout(c, "4242\n")
			}
			return Result{}
		}
		g.ProcCwd = func(context.Context, int) string { return other }
	}

	t.Run("E2E_SKIP_TOKEN_CHECK skips", func(t *testing.T) {
		g, _, out, _ := testGate(t, "E2E_SKIP_TOKEN_CHECK=1")
		g.Cfg.BedToken = "shared"
		discover(g)
		if err := g.CheckCompetingTokenConsumers(context.Background(), []string{"pat"}); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "skipped (E2E_SKIP_TOKEN_CHECK set)") {
			t.Errorf("out=%q", out)
		}
	})
	t.Run("an empty BED_TOKEN degrades to a warning", func(t *testing.T) {
		g, fe, _, errb := testGate(t)
		discover(g)
		if err := g.CheckCompetingTokenConsumers(context.Background(), []string{"pat"}); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(errb.String(), "BED_TOKEN unreadable") {
			t.Errorf("err=%q", errb)
		}
		if len(fe.callsNamed("pgrep")) != 0 {
			t.Error("no discovery without a token to compare against")
		}
	})
	t.Run("a competitor with a pat leg refuses naming pid and dir", func(t *testing.T) {
		g, _, _, _ := testGate(t)
		g.Cfg.BedToken = "shared"
		discover(g)
		err := g.CheckCompetingTokenConsumers(context.Background(), []string{"pat", "app"})
		if exitCode(err) != ExitPreconditionFailed {
			t.Fatalf("want exit 7, got %v", err)
		}
		for _, want := range []string{"PRECONDITION FAILED", "4242", other} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("missing %q in %q", want, err)
			}
		}
	})
	t.Run("an app-only run with a competitor warns, not refuses", func(t *testing.T) {
		g, _, _, errb := testGate(t)
		g.Cfg.BedToken = "shared"
		discover(g)
		if err := g.CheckCompetingTokenConsumers(context.Background(), []string{"app"}); err != nil {
			t.Fatalf("app-only must not refuse: %v", err)
		}
		if !strings.Contains(errb.String(), "pid 4242") {
			t.Errorf("warning should name the competitor, got %q", errb)
		}
	})
	t.Run("no competitors passes", func(t *testing.T) {
		g, _, out, _ := testGate(t)
		g.Cfg.BedToken = "shared"
		if err := g.CheckCompetingTokenConsumers(context.Background(), []string{"pat"}); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "no competing token consumers found") {
			t.Errorf("out=%q", out)
		}
	})
}

func TestDiscoverFabrikProcessDirsSkipsSelf(t *testing.T) {
	g, fe, _, _ := testGate(t)
	g.Self = 77
	fe.handler = func(_ context.Context, c Cmd) Result {
		if c.Name == "pgrep" && strings.Join(c.Args, " ") == "-x fabrik" {
			writeStdout(c, "77\n88\n99\n")
		}
		return Result{}
	}
	g.ProcCwd = func(_ context.Context, pid int) string {
		if pid == 99 {
			return "" // cwd unresolvable: silently dropped
		}
		return "/some/dir"
	}
	got := g.DiscoverFabrikProcessDirs(context.Background())
	if len(got) != 1 || got[0] != (Candidate{88, "/some/dir"}) {
		t.Errorf("got %v", got)
	}
}

func TestResolvePIDCwdLsofParsing(t *testing.T) {
	fe := &fakeExec{handler: func(_ context.Context, c Cmd) Result {
		writeStdout(c, "p123\nfcwd\nn/work/dir\n")
		return Result{}
	}}
	if got := ResolvePIDCwd(context.Background(), fe, "darwin", 123); got != "/work/dir" {
		t.Errorf("got %q", got)
	}
	if got := fe.callsNamed("lsof")[0].Args; strings.Join(got, " ") != "-a -p 123 -d cwd -Fn" {
		t.Errorf("lsof args = %v", got)
	}
	if got := ResolvePIDCwd(context.Background(), &fakeExec{handler: func(context.Context, Cmd) Result { return Result{ExitCode: 127, Err: os.ErrNotExist} }}, "darwin", 1); got != "" {
		t.Errorf("a missing lsof reads as unknown, got %q", got)
	}
	if got := ResolvePIDCwd(context.Background(), &fakeExec{}, "linux", os.Getpid()); got == "" {
		if _, err := os.Readlink("/proc/self/cwd"); err == nil {
			t.Error("linux should resolve through /proc")
		}
	}
}

// Ported from scripts/e2e/reviewer_reachable_check_test.sh.

func TestCheckReviewerReachable(t *testing.T) {
	dir := t.TempDir()
	lock := dir + "/.pruefer/pruefer.lock"
	mustWrite(t, lock, "")
	const deadPID = "999999"
	newG := func(extra ...string) *Gate {
		g, _, _, _ := testGate(t, extra...)
		g.Cfg.PrueferDir = dir
		return g
	}

	mustWrite(t, lock, strconv.Itoa(os.Getpid())+"\n")
	if err := newG().CheckReviewerReachable(nil); err != nil {
		t.Errorf("live PID in lock file: want pass, got %v", err)
	}

	os.Remove(lock)
	err := newG().CheckReviewerReachable(nil)
	if exitCode(err) != ExitPreconditionFailed || !strings.Contains(err.Error(), "lock file not found") {
		t.Errorf("missing lock file: want exit 7, got %v", err)
	}

	mustWrite(t, lock, deadPID+"\n")
	err = newG().CheckReviewerReachable(nil)
	if exitCode(err) != ExitPreconditionFailed || !strings.Contains(err.Error(), "names no live process") {
		t.Errorf("dead PID: want exit 7, got %v", err)
	}

	mustWrite(t, lock, "garbage\n")
	if err := newG().CheckReviewerReachable(nil); exitCode(err) != ExitPreconditionFailed || !strings.Contains(err.Error(), "pid='<empty>'") {
		t.Errorf("garbled lock: want exit 7 naming an empty pid, got %v", err)
	}

	mustWrite(t, lock, deadPID+"\n")
	if err := newG("E2E_SKIP_REVIEWER_CHECK=1").CheckReviewerReachable(nil); err != nil {
		t.Errorf("E2E_SKIP_REVIEWER_CHECK=1 must pass despite a dead PID, got %v", err)
	}

	g := newG()
	g.Cfg.PrueferDir = dir + "/does-not-exist"
	if err := g.CheckReviewerReachable(nil); err != nil {
		t.Errorf("a nonexistent PRUEFER_DIR is undeterminable, not down: want pass, got %v", err)
	}
	if !strings.Contains(g.Err.(interface{ String() string }).String(), "PRUEFER_DIR") {
		t.Error("a nonexistent PRUEFER_DIR must warn")
	}

	for _, args := range [][]string{{"-run", "TestSomeScenario"}, {"--run=TestSomeScenario"}, {"-run=X"}, {"--run", "X"}} {
		if err := newG().CheckReviewerReachable(args); err != nil {
			t.Errorf("%v supplied: want skip despite a dead PID, got %v", args, err)
		}
	}
}
