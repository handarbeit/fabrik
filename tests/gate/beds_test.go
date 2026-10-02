package gate

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// twoBedGate is testGate with a second bed: bed A is testGate's own bed, bed B a
// fresh one, each with its own token, repo pair and board in its .env.
func twoBedGate(t *testing.T, env ...string) (*Gate, *fakeExec, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	g, fe, out, errb := testGate(t, env...)
	a, b := g.Cfg.TestBed, t.TempDir()
	mustWrite(t, a+"/.env", "FABRIK_TOKEN=token-a\nFABRIK_TEST_REPO_ALPHA=o/alpha-a\nFABRIK_TEST_REPO_BETA=o/beta-a\nFABRIK_TEST_PROJECT_NUMBER=2\n")
	mustWrite(t, b+"/.env", "FABRIK_TOKEN=token-b\nFABRIK_TEST_REPO_ALPHA=o/alpha-b\nFABRIK_TEST_REPO_BETA=o/beta-b\nFABRIK_TEST_PROJECT_NUMBER=3\n")
	g.Cfg.BedDirs = []string{a, b}
	g.Cfg.BedToken = "token-a"
	g.buildBedGates()
	return g, fe, out, errb
}

func TestParseBeds(t *testing.T) {
	t.Run("unset is the single FABRIK_TEST_DIR bed as given", func(t *testing.T) {
		got, err := parseBeds("  ", "rel/bed", "/h")
		if err != nil || len(got) != 1 || got[0] != "rel/bed" {
			t.Errorf("got %v, %v", got, err)
		}
	})
	t.Run("a list is made absolute in order", func(t *testing.T) {
		a, b := t.TempDir(), t.TempDir()
		got, err := parseBeds(a+" , "+b, "ignored", "/h")
		if err != nil || len(got) != 2 || got[0] != a || got[1] != b {
			t.Errorf("got %v, %v", got, err)
		}
	})
	t.Run("a leading ~/ is expanded in every entry", func(t *testing.T) {
		got, err := parseBeds("/beds/a,~/dev/fabrik-test-2", "", "/home/op")
		if err != nil || len(got) != 2 || got[1] != "/home/op/dev/fabrik-test-2" {
			t.Errorf("got %v, %v", got, err)
		}
	})
	t.Run("an empty entry is refused", func(t *testing.T) {
		if _, err := parseBeds(t.TempDir()+",,"+t.TempDir(), "", "/h"); err == nil || !strings.Contains(err.Error(), "empty entry") {
			t.Errorf("got %v", err)
		}
	})
	t.Run("the same directory twice is refused even through a symlink", func(t *testing.T) {
		a := t.TempDir()
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(a, link); err != nil {
			t.Fatal(err)
		}
		if _, err := parseBeds(a+","+a, "", "/h"); err == nil {
			t.Error("a literal duplicate must be refused")
		}
		if _, err := parseBeds(a+","+link, "", "/h"); err == nil || !strings.Contains(err.Error(), "same directory") {
			t.Errorf("a symlinked duplicate must be refused: %v", err)
		}
	})
	t.Run("more than 26 beds is refused", func(t *testing.T) {
		var dirs []string
		for i := 0; i < 27; i++ {
			dirs = append(dirs, fmt.Sprintf("/beds/%d", i))
		}
		if _, err := parseBeds(strings.Join(dirs, ","), "", "/h"); err == nil || !strings.Contains(err.Error(), "at most 26") {
			t.Errorf("got %v", err)
		}
	})
}

func TestLoadConfigBeds(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	c, err := LoadConfig(env(map[string]string{"HOME": "/h", "FABRIK_TEST_DIR": "/elsewhere", "E2E_BEDS": a + "," + b}), "/repo")
	if err != nil {
		t.Fatal(err)
	}
	if c.TestBed != a || c.EngineLog != filepath.Join(a, ".fabrik", "fabrik.log") || len(c.BedDirs) != 2 || c.BedDirs[1] != b {
		t.Errorf("E2E_BEDS names bed A and FABRIK_TEST_DIR is ignored: %+v", c)
	}
	c, err = LoadConfig(env(map[string]string{"HOME": "/h", "FABRIK_TEST_DIR": "/bed"}), "/repo")
	if err != nil || c.TestBed != "/bed" || len(c.BedDirs) != 1 || c.BedDirs[0] != "/bed" {
		t.Errorf("unset E2E_BEDS keeps the one bed: %+v %v", c, err)
	}
	if _, err := LoadConfig(env(map[string]string{"HOME": "/h", "E2E_BEDS": a + ",," + b}), "/repo"); err == nil {
		t.Error("a malformed E2E_BEDS must fail config loading")
	}
}

func TestResolveBedSpecPrecedence(t *testing.T) {
	g, _, _, _ := testGate(t, "FABRIK_TEST_REPO_BETA=env/beta", "FABRIK_TEST_PROJECT_OWNER=env-owner", "FABRIK_TEST_REPO_ALPHA=env/alpha")
	bed := t.TempDir()
	mustWrite(t, bed+"/.env", "FABRIK_TOKEN=tok\nFABRIK_TEST_REPO_ALPHA=\"bed/alpha\"\nE2E_APP_INSTALLATION_ID=42\n")
	s := g.resolveBedSpec("B", bed)
	want := ResetConfig{Alpha: "bed/alpha", Beta: "env/beta", ProjectOwner: "env-owner", ProjectNumber: "2"}
	if s.Reset != want {
		t.Errorf("bed .env > process env > default: got %+v, want %+v", s.Reset, want)
	}
	if s.Name != "B" || s.Dir != bed || s.Token != "tok" || s.AppInstallationID != "42" {
		t.Errorf("spec = %+v", s)
	}
}

func TestBedViewsHaveIndependentState(t *testing.T) {
	g, fe, _, _ := twoBedGate(t)
	sw := &startWriter{fakeExec: fe}
	g.Exec = sw
	beds := g.beds()
	if len(beds) != 2 || !g.multiBed() {
		t.Fatalf("beds: %d", len(beds))
	}
	for _, b := range beds {
		b.Exec = sw
	}
	a, b := beds[0], beds[1]
	if a.Cfg.TestBed == b.Cfg.TestBed || a.Cfg.EngineLog == b.Cfg.EngineLog || a.Cfg.BedToken != "token-a" || b.Cfg.BedToken != "token-b" {
		t.Fatalf("views share state: A=%+v B=%+v", a.Cfg, b.Cfg)
	}
	if a.resetConfig().ProjectNumber != "2" || b.resetConfig().ProjectNumber != "3" || b.resetConfig().Alpha != "o/alpha-b" {
		t.Errorf("each bed resets its own board and repos: A=%+v B=%+v", a.resetConfig(), b.resetConfig())
	}

	// Start: each bed writes its own bed-run.log and isolated gitconfig and is
	// launched in its own directory.
	for _, bg := range beds {
		sw.banner = "Fabrik starting dev (abc1234) " + bg.bed.Name
		if err := bg.StartBed(context.Background(), "abc1234"); err != nil {
			t.Fatal(err)
		}
	}
	for i, bg := range beds {
		data, err := os.ReadFile(filepath.Join(bg.Cfg.TestBed, "bed-run.log"))
		if err != nil || !strings.HasSuffix(strings.TrimSpace(string(data)), " "+bg.bed.Name) {
			t.Errorf("bed %s's bed-run.log = %q, %v", bg.bed.Name, data, err)
		}
		if _, err := os.Stat(filepath.Join(bg.Cfg.TestBed, ".fabrik", "git-config-isolated")); err != nil {
			t.Errorf("bed %s's isolated gitconfig: %v", bg.bed.Name, err)
		}
		if sw.started[i].Dir != bg.Cfg.TestBed {
			t.Errorf("bed %s started in %s", bg.bed.Name, sw.started[i].Dir)
		}
	}

	// Stop: bed B's stop never touches bed A's locked engine.
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	mustWrite(t, a.Cfg.TestBed+"/.fabrik/fabrik.lock", strconv.Itoa(cmd.Process.Pid)+"\n")
	if err := b.StopBedInstance(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !pidAlive(cmd.Process.Pid) {
		t.Error("bed B's stop terminated bed A's engine")
	}
}

func TestLegEnvCarriesItsOwnBed(t *testing.T) {
	g, fe, _, _ := twoBedGate(t)
	for _, b := range g.beds() {
		lf := &legFake{suiteOut: passStream}
		fe.handler = lf.handle
		if err := b.RunLeg(context.Background(), defaultCell); err != nil {
			t.Fatalf("bed %s: %v", b.bed.Name, err)
		}
		rc := b.bed.Reset
		for _, c := range append(lf.switchCmds, lf.suiteCmds...) {
			for k, want := range map[string]string{
				"FABRIK_TEST_DIR": b.bed.Dir, "FABRIK_TEST_REPO_ALPHA": rc.Alpha, "FABRIK_TEST_REPO_BETA": rc.Beta,
				"FABRIK_TEST_PROJECT_OWNER": rc.ProjectOwner, "FABRIK_TEST_PROJECT_NUMBER": rc.ProjectNumber,
			} {
				if got := envValue(c.Env, k); got != want {
					t.Errorf("bed %s %s: %s=%q, want %q", b.bed.Name, argsLine(c), k, got, want)
				}
			}
		}
	}

	// One bed: the leg inherits the gate's environment, exactly as before.
	single, sfe, _, _ := testGate(t)
	lf := &legFake{suiteOut: passStream}
	sfe.handler = lf.handle
	if err := single.RunLeg(context.Background(), defaultCell); err != nil {
		t.Fatal(err)
	}
	if envValue(lf.suiteCmds[0].Env, "FABRIK_TEST_DIR") != "" || envValue(lf.switchCmds[0].Env, "FABRIK_TEST_PROJECT_NUMBER") != "" {
		t.Error("a single-bed leg must not gain bed variables")
	}
}

func TestPrefixWriterNeverInterleavesLines(t *testing.T) {
	var buf bytes.Buffer
	mu := &sync.Mutex{}
	a := &prefixWriter{mu: mu, w: &buf, prefix: "[bed A] "}
	b := &prefixWriter{mu: mu, w: &buf, prefix: "[bed B] "}
	var wg sync.WaitGroup
	for _, w := range []*prefixWriter{a, b} {
		wg.Add(1)
		go func(w *prefixWriter) {
			defer wg.Done()
			tag := strings.TrimSpace(w.prefix)
			for i := 0; i < 200; i++ {
				// Every line arrives in three partial writes.
				fmt.Fprintf(w, "%s ", tag)
				fmt.Fprintf(w, "line %d", i)
				fmt.Fprint(w, " end\n")
			}
		}(w)
	}
	wg.Wait()
	a.Write([]byte("partial"))
	a.Flush()
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 401 {
		t.Fatalf("lines: %d", len(lines))
	}
	for _, l := range lines[:400] {
		p := l[:len("[bed A] ")]
		if !strings.HasPrefix(l, p+strings.TrimSpace(p)+" line ") || !strings.HasSuffix(l, " end") {
			t.Fatalf("an interleaved line: %q", l)
		}
	}
	if lines[400] != "[bed A] partial" {
		t.Errorf("flush: %q", lines[400])
	}
}

func TestSingleBedHasNoViews(t *testing.T) {
	g, _, _, _ := testGate(t)
	g.Cfg.BedDirs = []string{g.Cfg.TestBed}
	g.buildBedGates()
	if g.multiBed() || len(g.beds()) != 1 || g.beds()[0] != g {
		t.Error("one bed is the root Gate itself")
	}
	if _, ok := g.Out.(*prefixWriter); ok {
		t.Error("single-bed output must stay unprefixed")
	}
	if g.bedEnv() != nil || g.CheckBedTopology([]string{"pat", "app"}) != nil {
		t.Error("single bed: no bed env, no topology check")
	}
}

func TestCheckBedTopology(t *testing.T) {
	setup := func(t *testing.T, bEnv string) *Gate {
		g, _, _, _ := twoBedGate(t)
		mustWrite(t, g.Cfg.BedDirs[1]+"/.env", bEnv)
		g.bedGates = nil
		g.buildBedGates()
		return g
	}
	t.Run("distinct beds sharing only a harness login pass", func(t *testing.T) {
		g := setup(t, "FABRIK_TOKEN=token-a\nFABRIK_TEST_REPO_ALPHA=o/alpha-b\nFABRIK_TEST_REPO_BETA=o/beta-b\nFABRIK_TEST_PROJECT_NUMBER=3\n")
		if err := g.CheckBedTopology([]string{"pat", "app"}); err != nil {
			t.Errorf("the same harness token serializes, it is not refused: %v", err)
		}
	})
	for name, tc := range map[string]struct{ bEnv, aExtra, want string }{
		"the same App installation": {
			bEnv:   "FABRIK_TOKEN=token-b\nFABRIK_TEST_REPO_ALPHA=o/alpha-b\nFABRIK_TEST_REPO_BETA=o/beta-b\nFABRIK_TEST_PROJECT_NUMBER=3\nE2E_APP_INSTALLATION_ID=77\n",
			aExtra: "E2E_APP_INSTALLATION_ID=77\n", want: "same GitHub App installation 77",
		},
		"the same board": {
			bEnv: "FABRIK_TOKEN=token-b\nFABRIK_TEST_REPO_ALPHA=o/alpha-b\nFABRIK_TEST_REPO_BETA=o/beta-b\nFABRIK_TEST_PROJECT_NUMBER=2\n", want: "same board handarbeit/#2",
		},
		"a shared repo": {
			bEnv: "FABRIK_TOKEN=token-b\nFABRIK_TEST_REPO_ALPHA=O/Alpha-A\nFABRIK_TEST_REPO_BETA=o/beta-b\nFABRIK_TEST_PROJECT_NUMBER=3\n", want: "share the repo O/Alpha-A",
		},
		"a bed with no token": {
			bEnv: "FABRIK_TEST_REPO_ALPHA=o/alpha-b\nFABRIK_TEST_REPO_BETA=o/beta-b\nFABRIK_TEST_PROJECT_NUMBER=3\n", want: "no identity to schedule on",
		},
	} {
		t.Run(name+" is refused", func(t *testing.T) {
			g, _, _, _ := twoBedGate(t)
			data, _ := os.ReadFile(g.Cfg.BedDirs[0] + "/.env")
			mustWrite(t, g.Cfg.BedDirs[0]+"/.env", string(data)+tc.aExtra)
			mustWrite(t, g.Cfg.BedDirs[1]+"/.env", tc.bEnv)
			g.bedGates = nil
			g.buildBedGates()
			err := g.CheckBedTopology([]string{"app"})
			if exitCode(err) != ExitPreconditionFailed || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("got %v", err)
			}
		})
	}
	t.Run("the same App is fine when no app leg is planned", func(t *testing.T) {
		g, _, _, _ := twoBedGate(t)
		for _, d := range g.Cfg.BedDirs {
			data, _ := os.ReadFile(d + "/.env")
			mustWrite(t, d+"/.env", string(data)+"E2E_APP_INSTALLATION_ID=77\n")
		}
		g.bedGates = nil
		g.buildBedGates()
		if err := g.CheckBedTopology([]string{"pat"}); err != nil {
			t.Error(err)
		}
	})
}

// D5 / the #1684 acceptance cases: another bed's engine is never a competitor,
// any other process on a bed's own token (the dev daemon) still is.
func TestCompetingConsumersAcrossBeds(t *testing.T) {
	// procs maps a pid to its cwd; the ProcCwd seam reaches every bed view
	// through the root, so it is set before the views are rebuilt.
	setup := func(t *testing.T, procs map[int]string) *Gate {
		g, fe, _, _ := twoBedGate(t)
		fe.handler = func(_ context.Context, c Cmd) Result {
			if c.Name == "pgrep" {
				for pid := range procs {
					writeStdout(c, strconv.Itoa(pid)+"\n")
				}
			}
			return Result{}
		}
		g.ProcCwd = func(_ context.Context, pid int) string { return procs[pid] }
		g.bedGates = nil
		g.buildBedGates()
		return g
	}
	t.Run("bed B's check accepts bed A's running engine (and vice versa)", func(t *testing.T) {
		procs := map[int]string{}
		g := setup(t, procs)
		procs[101], procs[102] = g.Cfg.BedDirs[0], g.Cfg.BedDirs[1]
		// Bed B's own .env carries bed A's token: bed A's engine shares it.
		mustWrite(t, g.Cfg.BedDirs[1]+"/.env", "FABRIK_TOKEN=token-a\n")
		g.bedGates = nil
		g.buildBedGates()
		if err := g.CheckCompetingTokenConsumers(context.Background(), []string{"pat", "app"}); err != nil {
			t.Errorf("another bed's engine is never a competing consumer: %v", err)
		}
	})
	t.Run("a dev daemon on bed A's token is refused and named", func(t *testing.T) {
		dev := t.TempDir()
		mustWrite(t, dev+"/.env", "FABRIK_TOKEN=token-a\n")
		g := setup(t, map[int]string{4242: dev})
		err := g.CheckCompetingTokenConsumers(context.Background(), []string{"pat"})
		if exitCode(err) != ExitPreconditionFailed || !strings.Contains(err.Error(), "pid 4242") || !strings.Contains(err.Error(), dev) {
			t.Fatalf("got %v", err)
		}
		if !strings.Contains(err.Error(), g.Cfg.BedDirs[0]) || strings.Contains(err.Error(), "identical to this test bed's ("+g.Cfg.BedDirs[1]) {
			t.Errorf("the refusal names the bed whose token is shared, only: %v", err)
		}
	})
	t.Run("a dev daemon on bed B's token is refused too", func(t *testing.T) {
		dev := t.TempDir()
		mustWrite(t, dev+"/.env", "FABRIK_TOKEN=token-b\n")
		g := setup(t, map[int]string{4243: dev})
		if err := g.CheckCompetingTokenConsumers(context.Background(), []string{"pat"}); exitCode(err) != ExitPreconditionFailed {
			t.Errorf("got %v", err)
		}
	})
	t.Run("the process listing is taken once for every bed", func(t *testing.T) {
		g, fe, _, _ := twoBedGate(t)
		if err := g.CheckCompetingTokenConsumers(context.Background(), []string{"pat"}); err != nil {
			t.Fatal(err)
		}
		if n := len(fe.callsNamed("pgrep")); n != 1 {
			t.Errorf("pgrep calls: %d", n)
		}
	})
}

func TestAuthModePreconditionsCoverEveryBed(t *testing.T) {
	g, _, _, _ := twoBedGate(t)
	key := filepath.Join(t.TempDir(), "key.pem")
	mustWrite(t, key, "k")
	data, _ := os.ReadFile(g.Cfg.BedDirs[0] + "/.env")
	mustWrite(t, g.Cfg.BedDirs[0]+"/.env", string(data)+"E2E_APP_ID=1\nE2E_APP_PRIVATE_KEY_PATH="+key+"\nE2E_APP_INSTALLATION_ID=5\n")
	err := g.CheckAuthModePreconditions([]string{"app"})
	if exitCode(err) != ExitPreconditionFailed || !strings.Contains(err.Error(), g.Cfg.BedDirs[1]) || strings.Contains(err.Error(), g.Cfg.BedDirs[0]+"/.env (needed") {
		t.Errorf("only bed B is missing its App settings: %v", err)
	}
}
