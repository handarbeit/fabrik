package gate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeIdentity is a scripted IdentityOps.
type fakeIdentity struct {
	mu        sync.Mutex
	logins    map[string]string // token -> login; a missing token fails
	budgets   map[string]int    // token -> remaining
	budgetErr error
	mintErr   error
	mintEmpty bool     // the mint "succeeds" with no token
	block     bool     // Budget blocks until its context ends
	resolves  []string // tokens ResolveLogin was asked about
	mints     []string // bed dirs MintAppToken was asked about
}

// mintedToken is what the fake mints; it must never reach the output.
const mintedToken = "ghs_MINTEDSECRET"

func (f *fakeIdentity) ResolveLogin(_ context.Context, token string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolves = append(f.resolves, token)
	if l, ok := f.logins[token]; ok {
		return l, nil
	}
	return "", errors.New("HTTP 401: Bad credentials")
}

func (f *fakeIdentity) Budget(ctx context.Context, token string) (int, string, error) {
	if f.block {
		<-ctx.Done()
		return 0, "", ctx.Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.budgetErr != nil {
		return 0, "", f.budgetErr
	}
	return f.budgets[token], "2026-10-02T10:15:00Z", nil
}

func (f *fakeIdentity) MintAppToken(_ context.Context, bedDir string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mints = append(f.mints, bedDir)
	if f.mintErr != nil {
		return "", f.mintErr
	}
	if f.mintEmpty {
		return "", nil
	}
	return mintedToken, nil
}

// appBanner is a bed startup under App auth, printed with the engine's own
// format strings (pinned against the source in TestBannerFormatsArePinned).
func appBanner(installation int, slug string) string {
	return "Fabrik starting dev (abc1234)\n" +
		"  repo:    handarbeit/fabrik-test-alpha\n" +
		fmt.Sprintf("  identity: GitHub App installation %d\n", installation) +
		"  stages:  9 loaded\n" +
		fmt.Sprintf("[startup] authenticated as %s (GitHub App installation, organization %q)\n", slug, "handarbeit")
}

// The parser must break CI, not a release run, if the engine's banner changes.
func TestBannerFormatsArePinned(t *testing.T) {
	for file, format := range map[string]string{
		"../../cmd/root.go":               `fmt.Printf("  identity: GitHub App installation %d\n"`,
		"../../engine/github_app_auth.go": `fmt.Printf("[startup] authenticated as %s (GitHub App installation, organization %q)\n"`,
		"../../cmd/root.go#start":         `fmt.Printf("Fabrik starting %s\n"`,
	} {
		file, _, _ = strings.Cut(file, "#")
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("reading %s: %v", file, err)
		}
		if !strings.Contains(string(data), format) {
			t.Errorf("%s no longer prints %s — update parseBedAppBanner and this pin together", file, format)
		}
	}
}

func TestParseBedAppBanner(t *testing.T) {
	log := appBanner(999, "old-bed[bot]") + "...\n" + appBanner(77, "fabrik-bed[bot]") + "[poll] ...\n"
	if inst, slug := parseBedAppBanner(log); inst != "77" || slug != "fabrik-bed[bot]" {
		t.Errorf("only the last startup counts: %q %q", inst, slug)
	}
	pat := log + "Fabrik starting dev (abc1234)\n  user:    arbeithand\n"
	if inst, slug := parseBedAppBanner(pat); inst != "" || slug != "" {
		t.Errorf("a later PAT startup has no App identity: %q %q", inst, slug)
	}
}

func TestVerifyBedAppIdentity(t *testing.T) {
	setup := func(t *testing.T, envInstall, log string) *Gate {
		g, _, _, _ := testGate(t)
		env := "FABRIK_TOKEN=t\n"
		if envInstall != "" {
			env += "E2E_APP_INSTALLATION_ID=" + envInstall + "\n"
		}
		mustWrite(t, g.Cfg.TestBed+"/.env", env)
		if log != "" {
			mustWrite(t, g.Cfg.TestBed+"/bed-run.log", log)
		}
		return g
	}
	t.Run("a matching banner passes and records the slug", func(t *testing.T) {
		g := setup(t, "77", appBanner(77, "fabrik-bed[bot]"))
		if err := g.verifyBedAppIdentity("app/on"); err != nil || g.appSlug != "fabrik-bed[bot]" {
			t.Errorf("err=%v slug=%q", err, g.appSlug)
		}
	})
	t.Run("another installation is a precondition failure", func(t *testing.T) {
		g := setup(t, "77", appBanner(78, "fabrik-bed-2[bot]"))
		err := g.verifyBedAppIdentity("app/on")
		if exitCode(err) != ExitPreconditionFailed || !strings.Contains(err.Error(), "installation 78") {
			t.Errorf("got %v", err)
		}
	})
	t.Run("no App identity in the banner is a precondition failure", func(t *testing.T) {
		g := setup(t, "77", "Fabrik starting dev\n  user:    arbeithand\n")
		if err := g.verifyBedAppIdentity("app/on"); exitCode(err) != ExitPreconditionFailed {
			t.Errorf("got %v", err)
		}
	})
	t.Run("an unreadable bed-run.log is a precondition failure", func(t *testing.T) {
		g := setup(t, "77", "")
		if err := g.verifyBedAppIdentity("app/on"); exitCode(err) != ExitPreconditionFailed {
			t.Errorf("got %v", err)
		}
	})
	t.Run("skipped when the bed configures no installation", func(t *testing.T) {
		g := setup(t, "", "")
		if err := g.verifyBedAppIdentity("app/on"); err != nil {
			t.Error(err)
		}
	})
}

func TestOSIdentityOpsAreBoundedAndTokenScoped(t *testing.T) {
	g, fe, _, _ := testGate(t)
	fe.handler = func(_ context.Context, c Cmd) Result {
		switch line := argsLine(c); {
		case strings.HasPrefix(line, "gh api user"):
			if envValue(c.Env, "GH_TOKEN") != "tok-1" {
				writeStderr(c, "HTTP 401: Bad credentials\n")
				return Result{ExitCode: 1}
			}
			writeStdout(c, "arbeithand\n")
		case strings.Contains(line, "rateLimit { remaining resetAt }"):
			writeStdout(c, "4210 2026-10-02T10:15:00Z\n")
		}
		return Result{}
	}
	ops := osIdentityOps{g: g}
	login, err := ops.ResolveLogin(context.Background(), "tok-1")
	if err != nil || login != "arbeithand" {
		t.Fatalf("login=%q err=%v", login, err)
	}
	_, err = ops.ResolveLogin(context.Background(), "tok-2")
	if err == nil || !strings.Contains(err.Error(), "Bad credentials") || strings.Contains(err.Error(), "tok-2") {
		t.Errorf("a failure carries gh's error and never the token: %v", err)
	}
	rem, reset, err := ops.Budget(context.Background(), "tok-1")
	if err != nil || rem != 4210 || reset != "2026-10-02T10:15:00Z" {
		t.Errorf("budget: %d %q %v", rem, reset, err)
	}
	for _, c := range fe.callsNamed("gh") {
		if !c.Session || c.Timeout != g.Cfg.GHAPITimeout || envValue(c.Env, "GH_TOKEN") == "" {
			t.Errorf("every identity call is bounded, session-reaped and token-scoped: %+v", c)
		}
	}
	t.Run("a timeout is an error", func(t *testing.T) {
		fe.handler = func(context.Context, Cmd) Result { return Result{ExitCode: 143, TimedOut: true} }
		if _, err := ops.ResolveLogin(context.Background(), "tok-1"); err == nil || !strings.Contains(err.Error(), "timed out") {
			t.Errorf("got %v", err)
		}
	})
	t.Run("the mint refuses an incomplete App configuration without a network call", func(t *testing.T) {
		if _, err := ops.MintAppToken(context.Background(), t.TempDir()); err == nil || !strings.Contains(err.Error(), "not all set") {
			t.Errorf("got %v", err)
		}
	})
}

func TestResolveBedIdentities(t *testing.T) {
	t.Run("multi-bed: one lookup per distinct token, logins recorded per bed", func(t *testing.T) {
		g, _, out, _ := twoBedGate(t)
		data, _ := os.ReadFile(g.Cfg.BedDirs[1] + "/.env")
		mustWrite(t, g.Cfg.BedDirs[1]+"/.env", strings.Replace(string(data), "token-b", "token-a", 1))
		fi := &fakeIdentity{logins: map[string]string{"token-a": "arbeithand"}}
		g.Identity = fi
		g.bedGates = nil
		g.buildBedGates()
		if err := g.ResolveBedIdentities(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(fi.resolves) != 1 {
			t.Errorf("resolved %d times", len(fi.resolves))
		}
		for _, b := range g.beds() {
			if b.login != "arbeithand" {
				t.Errorf("bed %s login %q", b.bed.Name, b.login)
			}
		}
		if !strings.Contains(out.String(), "[bed B] == bed identity: FABRIK_TOKEN authenticates as arbeithand ==") {
			t.Errorf("out:\n%s", out)
		}
	})
	t.Run("multi-bed: an unresolvable login refuses", func(t *testing.T) {
		g, _, _, _ := twoBedGate(t)
		g.Identity = &fakeIdentity{logins: map[string]string{"token-a": "a"}}
		g.bedGates = nil
		g.buildBedGates()
		err := g.ResolveBedIdentities(context.Background())
		if exitCode(err) != ExitPreconditionFailed || !strings.Contains(err.Error(), "bed B") || strings.Contains(err.Error(), "token-b") {
			t.Errorf("got %v", err)
		}
	})
	t.Run("single bed: an unresolvable login only warns", func(t *testing.T) {
		g, _, out, errb := testGate(t)
		g.Cfg.BedToken = "tok"
		g.Identity = &fakeIdentity{}
		if err := g.ResolveBedIdentities(context.Background()); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(errb.String(), "cannot resolve the GitHub login") || out.Len() != 0 {
			t.Errorf("out=%q err=%q", out, errb)
		}
	})
	t.Run("no resolver: nothing happens", func(t *testing.T) {
		g, fe, _, _ := testGate(t)
		g.Cfg.BedToken = "tok"
		if err := g.ResolveBedIdentities(context.Background()); err != nil || len(fe.calls) != 0 {
			t.Errorf("err=%v calls=%v", err, fe.lines())
		}
	})
}

func TestIdentitySet(t *testing.T) {
	g, _, _, _ := twoBedGate(t)
	a, b := g.beds()[0], g.beds()[1]
	a.bed.AppInstallationID, a.login = "77", "alice"
	keys := func(set []Identity) string {
		var k []string
		for _, id := range set {
			k = append(k, id.Key)
		}
		return strings.Join(k, ",")
	}
	if got := keys(a.identitySet("app")); got != "app:77,user:alice" {
		t.Errorf("an app cell charges the App and the harness user (D4): %s", got)
	}
	if got := keys(a.identitySet("pat")); got != "user:alice" {
		t.Errorf("a pat cell's engine and harness identity are one user: %s", got)
	}
	// An unresolved login still yields a stable, token-derived key — never the token.
	set := b.identitySet("pat")
	if len(set) != 1 || !strings.HasPrefix(set[0].Key, "token:") || strings.Contains(set[0].Key, "token-b") {
		t.Errorf("unresolved: %+v", set)
	}
	b2 := g.newBedGate(BedSpec{Name: "C", Dir: t.TempDir(), Token: "token-b"}, &sync.Mutex{}, &sync.Mutex{})
	if b2.identitySet("pat")[0].Key != set[0].Key {
		t.Error("the same token must collide on the same key")
	}
	if e, _ := a.engineIdentity("app"); e.Key != "app:77" {
		t.Errorf("engine identity on app: %s", e.Key)
	}
	if e, _ := a.engineIdentity("pat"); e.Key != "user:alice" {
		t.Errorf("engine identity on pat: %s", e.Key)
	}
}

func TestLogIdentityBudget(t *testing.T) {
	setup := func(t *testing.T, fi *fakeIdentity) *Gate {
		g, _, _, _ := testGate(t)
		g.Cfg.BedToken = "token-a"
		mustWrite(t, g.Cfg.TestBed+"/.env", "FABRIK_TOKEN=token-a\nE2E_APP_INSTALLATION_ID=77\n")
		g.login, g.appSlug = "alice", "fabrik-bed[bot]"
		if fi != nil {
			g.Identity = fi
		}
		return g
	}
	t.Run("one line per identity of an app cell", func(t *testing.T) {
		fi := &fakeIdentity{budgets: map[string]int{mintedToken: 4000, "token-a": 3000}}
		g := setup(t, fi)
		g.logIdentityBudget(context.Background(), Cell{Auth: "app", Train: "on"}, "start")
		out := g.Out.(interface{ String() string }).String()
		for _, want := range []string{
			"== identity budget (leg: app/on, start): app:77 fabrik-bed[bot] — 4000 remaining, resets 2026-10-02T10:15:00Z ==",
			"== identity budget (leg: app/on, start): user:alice — 3000 remaining, resets 2026-10-02T10:15:00Z ==",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("missing %q in:\n%s", want, out)
			}
		}
		if len(fi.mints) != 1 || fi.mints[0] != g.Cfg.TestBed {
			t.Errorf("the App's token is minted from the bed's own .env: %v", fi.mints)
		}
		if strings.Contains(out, mintedToken) || strings.Contains(out, "token-a") {
			t.Error("a token reached the output")
		}
	})
	t.Run("a pat cell reads only the user", func(t *testing.T) {
		fi := &fakeIdentity{budgets: map[string]int{"token-a": 3000}}
		g := setup(t, fi)
		g.logIdentityBudget(context.Background(), Cell{Auth: "pat", Train: "off"}, "end")
		if out := g.Out.(interface{ String() string }).String(); strings.Count(out, "identity budget") != 1 || len(fi.mints) != 0 {
			t.Errorf("out=%q mints=%v", out, fi.mints)
		}
	})
	t.Run("a failed mint or probe only warns", func(t *testing.T) {
		fi := &fakeIdentity{mintErr: errors.New("HTTP 401"), budgetErr: errors.New("gh exited 1: HTTP 502")}
		g := setup(t, fi)
		g.logIdentityBudget(context.Background(), Cell{Auth: "app", Train: "on"}, "start")
		errs := g.Err.(interface{ String() string }).String()
		if !strings.Contains(errs, "cannot mint a token for app:77") || !strings.Contains(errs, "reading user:alice's budget failed") {
			t.Errorf("err:\n%s", errs)
		}
		if g.Out.(interface{ String() string }).String() != "" {
			t.Error("no budget line without a reading")
		}
	})
	t.Run("an empty minted token is never probed with", func(t *testing.T) {
		// gh would fall back to the operator's ambient login and report its budget
		// under the App's name.
		fi := &fakeIdentity{mintEmpty: true, budgets: map[string]int{"": 9999, "token-a": 3000}}
		g := setup(t, fi)
		g.logIdentityBudget(context.Background(), Cell{Auth: "app", Train: "on"}, "start")
		out, errs := g.Out.(interface{ String() string }).String(), g.Err.(interface{ String() string }).String()
		if strings.Contains(out, "9999") || !strings.Contains(errs, "no token for app:77") || !strings.Contains(out, "user:alice — 3000") {
			t.Errorf("out=%q err=%q", out, errs)
		}
	})
	t.Run("all probes share one bound so the post-suite watchdog never trips on them", func(t *testing.T) {
		g := setup(t, &fakeIdentity{block: true})
		g.Cfg.GHAPITimeout = 50 * time.Millisecond
		done := make(chan struct{})
		go func() {
			g.logIdentityBudget(context.Background(), Cell{Auth: "app", Train: "on"}, "end")
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(failsafe):
			t.Fatal("the identity probes ignored their bound")
		}
		if errs := g.Err.(interface{ String() string }).String(); !strings.Contains(errs, "warning: identity budget (leg: app/on, end)") {
			t.Errorf("err=%q", errs)
		}
	})
	t.Run("no resolver prints nothing", func(t *testing.T) {
		g := setup(t, nil)
		g.Identity = nil
		g.logIdentityBudget(context.Background(), Cell{Auth: "app", Train: "on"}, "start")
		if g.Out.(interface{ String() string }).String() != "" {
			t.Error("printed")
		}
	})
}

// A whole app leg: the banner cross-check after the restart, and the identity
// budget lines at its start and end.
func TestRunLegIdentityBudgetAndBannerCheck(t *testing.T) {
	cell := Cell{Auth: "app", Train: "on", Parallel: "2"}
	setup := func(t *testing.T, bannerInstall int) (*Gate, *legFake, *fakeIdentity) {
		lf := &legFake{suiteOut: passStream, budgets: []string{"4000", "3000"}}
		g, _, _ := newLegGate(t, lf)
		mustWrite(t, g.Cfg.TestBed+"/.env", "FABRIK_TOKEN=bed-token\nE2E_APP_INSTALLATION_ID=77\n")
		mustWrite(t, g.Cfg.TestBed+"/bed-run.log", appBanner(bannerInstall, "fabrik-bed[bot]"))
		fi := &fakeIdentity{budgets: map[string]int{mintedToken: 4999, "bed-token": 2500}}
		g.Identity, g.login = fi, "arbeithand"
		g.Cfg.PostSuiteWatchdog = 10 * time.Second
		return g, lf, fi
	}
	t.Run("lines at start and end", func(t *testing.T) {
		g, _, _ := setup(t, 77)
		if err := g.RunLeg(context.Background(), cell); err != nil {
			t.Fatal(err)
		}
		out := g.Out.(interface{ String() string }).String()
		for _, which := range []string{"start", "end"} {
			if !strings.Contains(out, "== identity budget (leg: app/on, "+which+"): app:77 fabrik-bed[bot] — 4999 remaining") {
				t.Errorf("no %s line for the App:\n%s", which, out)
			}
		}
		if strings.Index(out, "(leg: app/on, start)") > strings.Index(out, "== per-test wall-clock") {
			t.Error("the start line belongs before the suite's report")
		}
	})
	t.Run("a banner naming another installation stops the leg before the suite", func(t *testing.T) {
		g, lf, _ := setup(t, 78)
		if err := g.RunLeg(context.Background(), cell); exitCode(err) != ExitPreconditionFailed {
			t.Fatalf("got %v", err)
		}
		if len(lf.suiteCmds) != 0 {
			t.Error("the suite must not run on an unscheduled identity")
		}
	})
}
