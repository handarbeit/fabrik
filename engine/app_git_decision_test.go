package engine

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/testenv"
)

// Tests for #1860 / ADR 1860: the HTTPS-vs-SSH git decision under GitHub App
// auth is made once (decideAppGitHTTPS, called by New()) and that single value
// feeds both the grant requirement and Run()'s credential injection.
// testenv.IsolateGit forbids t.Parallel.

func appGitDecisionConfig(t *testing.T, gitSSH bool) Config {
	t.Helper()
	dir := t.TempDir()
	keyPath := writeEngineTestAppKey(t, dir)
	return Config{
		Owner: "handarbeit", Repo: "fabrik", GitSSH: gitSSH,
		GitHubAppID: 42, GitHubAppPrivateKeyPath: keyPath, GitHubAppInstallationID: 999,
	}
}

func TestDecideAppGitHTTPS(t *testing.T) {
	tests := []struct {
		name    string
		app     bool
		gitSSH  bool
		rewrite string
		want    *bool // nil = not decided
	}{
		{name: "PAT mode is not decided", app: false, want: nil},
		{name: "app, plain https", app: true, want: appGitBoolPtr(true)},
		{name: "app, git_ssh", app: true, gitSSH: true, want: appGitBoolPtr(false)},
		{name: "app, ssh rewrite", app: true, rewrite: sshRewriteGitConfig, want: appGitBoolPtr(false)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testenv.IsolateGit(t, tt.rewrite)
			cfg := Config{GitSSH: tt.gitSSH}
			if tt.app {
				cfg = appGitDecisionConfig(t, tt.gitSSH)
			}
			got := decideAppGitHTTPS(cfg)
			switch {
			case tt.want == nil && got != nil:
				t.Errorf("decideAppGitHTTPS = %v, want nil", *got)
			case tt.want != nil && got == nil:
				t.Errorf("decideAppGitHTTPS = nil, want %v", *tt.want)
			case tt.want != nil && *got != *tt.want:
				t.Errorf("decideAppGitHTTPS = %v, want %v", *got, *tt.want)
			}
		})
	}
}

// The grant check consumes the decision it is handed and never reads git
// config itself: the opposite live git config must not change its outcome.
func TestSetUpGitHubAppAuth_UsesPassedDecisionNotGitConfig(t *testing.T) {
	t.Run("decided https, live ssh rewrite: contents:read still refused", func(t *testing.T) {
		testenv.IsolateGit(t, sshRewriteGitConfig)
		cfg := appGitDecisionConfig(t, false)
		srv := newFakeGitHubAppServer(t, 999, "handarbeit", "organization", RequiredGitHubAppPermissions(false))
		_, _, err := setUpGitHubAppAuth(context.Background(), cfg, t.TempDir(), srv.URL, true)
		if err == nil {
			t.Fatal("expected a contents:write shortfall: the decision (https) must govern, not the live git config")
		}
	})
	t.Run("decided ssh, live no rewrite: contents:read accepted", func(t *testing.T) {
		testenv.IsolateGit(t, "")
		cfg := appGitDecisionConfig(t, false)
		srv := newFakeGitHubAppServer(t, 999, "handarbeit", "organization", RequiredGitHubAppPermissions(false))
		if _, _, err := setUpGitHubAppAuth(context.Background(), cfg, t.TempDir(), srv.URL, false); err != nil {
			t.Fatalf("decision (ssh) must govern, not the live git config: %v", err)
		}
	})
}

// A git config change after construction does not change Run()'s injection:
// the stored decision wins. This is the divergence the single decision closes.
func TestRun_AppGitInjection_FollowsStoredDecisionNotLiveGitConfig(t *testing.T) {
	t.Run("decided https, ssh rewrite appears afterwards: still injects", func(t *testing.T) {
		testenv.IsolateGit(t, sshRewriteGitConfig)
		eng := newAppAuthTestEngine(t)
		eng.hostClient = gh.NewClient("ghs_installation_token")
		eng.appGitHTTPS = appGitBoolPtr(true)

		runEngineUntilShutdownWith(t, eng, func() {
			waitFor(t, func() bool { _, err := os.Stat(AppGitTokenPath(eng.fabrikDir)); return err == nil })
			if _, err := os.Stat(AppGitTokenPath(eng.fabrikDir)); err != nil {
				t.Fatalf("token file missing: the stored https decision was overridden by live git config: %v", err)
			}
			if got := os.Getenv("GIT_CONFIG_COUNT"); got != "2" {
				t.Errorf("GIT_CONFIG_COUNT = %q, want 2", got)
			}
		})
	})
	t.Run("decided ssh, rewrite absent afterwards: no injection", func(t *testing.T) {
		testenv.IsolateGit(t, "")
		t.Setenv("GIT_CONFIG_COUNT", "2")
		t.Setenv("GIT_CONFIG_KEY_0", appGitCredentialKey)
		t.Setenv("GIT_CONFIG_KEY_1", appGitCredentialKey)
		t.Setenv("GIT_CONFIG_VALUE_1", appGitCredentialHelper("/stale/token"))
		eng := newAppAuthTestEngine(t)
		eng.hostClient = gh.NewClient("ghs_installation_token")
		eng.appGitHTTPS = appGitBoolPtr(false)

		runEngineUntilShutdownWith(t, eng, func() {
			waitFor(t, func() bool { _, ok := os.LookupEnv("GIT_CONFIG_COUNT"); return !ok })
			if v, ok := os.LookupEnv("GIT_CONFIG_COUNT"); ok {
				t.Errorf("GIT_CONFIG_COUNT = %q: the stored ssh decision was overridden by live git config", v)
			}
			if _, err := os.Stat(AppGitTokenPath(eng.fabrikDir)); !os.IsNotExist(err) {
				t.Errorf("token file present despite stored ssh decision (stat err %v)", err)
			}
		})
	})
}

// An undecided engine (built outside New()) falls back to one live read; nil
// never means "SSH, skip injection".
func TestAppGitUsesHTTPS_UndecidedFallsBackToLiveRead(t *testing.T) {
	t.Run("no rewrite: https", func(t *testing.T) {
		testenv.IsolateGit(t, "")
		eng := newAppAuthTestEngine(t)
		if eng.appGitHTTPS != nil {
			t.Fatal("test engine unexpectedly has a stored decision")
		}
		if !eng.appGitUsesHTTPS() {
			t.Error("undecided engine with no rewrite must resolve to HTTPS, not SSH")
		}
	})
	t.Run("rewrite: ssh", func(t *testing.T) {
		testenv.IsolateGit(t, sshRewriteGitConfig)
		eng := newAppAuthTestEngine(t)
		if eng.appGitUsesHTTPS() {
			t.Error("undecided engine with an ssh rewrite must resolve to SSH")
		}
	})
	t.Run("git_ssh: ssh", func(t *testing.T) {
		testenv.IsolateGit(t, "")
		eng := newAppAuthTestEngine(t)
		eng.cfg.GitSSH = true
		if eng.appGitUsesHTTPS() {
			t.Error("undecided engine with git_ssh must resolve to SSH")
		}
	})
	t.Run("stored decision wins over live config", func(t *testing.T) {
		testenv.IsolateGit(t, sshRewriteGitConfig)
		eng := newAppAuthTestEngine(t)
		eng.appGitHTTPS = appGitBoolPtr(true)
		if !eng.appGitUsesHTTPS() {
			t.Error("stored https decision must win over a live ssh rewrite")
		}
	})
	t.Run("PAT mode: false", func(t *testing.T) {
		testenv.IsolateGit(t, "")
		eng := newAppAuthTestEngine(t)
		eng.ghAppAuth = nil
		if eng.appGitUsesHTTPS() {
			t.Error("without App auth the value must be false (setUpAppGitCredential strips)")
		}
	})
}

// TestAppGitDecisionCallSitesAreSingle is the static guard (SC-002): the live
// git-config readers may only be reached from the decision site, the
// undecided-engine fallback and the advisory check. A second caller would
// reintroduce the two-answers-to-one-question bug.
func TestAppGitDecisionCallSitesAreSingle(t *testing.T) {
	allowed := map[string]map[string]bool{
		"AppGitUsesHTTPS":        {"decideAppGitHTTPS": true, "appGitUsesHTTPS": true},
		"gitHTTPSRewrittenToSSH": {"AppGitUsesHTTPS": true, "checkURLRewrite": true},
		"checkURLRewrite":        {"Run": true},
		"decideAppGitHTTPS":      {"New": true},
		"setUpGitHubAppAuth":     {"resolveGitHubAppAuth": true},
		"resolveGitHubAppAuth":   {"New": true},
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	found := map[string]int{}
	var pollSrc string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		if f == "poll.go" {
			b, _ := os.ReadFile(f)
			pollSrc = string(b)
		}
		for _, decl := range file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				var name string
				switch fn := call.Fun.(type) {
				case *ast.Ident:
					name = fn.Name
				case *ast.SelectorExpr:
					name = fn.Sel.Name
				}
				callers, guarded := allowed[name]
				if !guarded {
					return true
				}
				found[name]++
				if !callers[fd.Name.Name] {
					t.Errorf("%s: %s is called from %s; only %v may call it (#1860, ADR 1860)",
						f, name, fd.Name.Name, appGitKeys(callers))
				}
				return true
			})
		}
	}
	for name := range allowed {
		if found[name] == 0 {
			t.Errorf("no call to %s found: the guard is vacuous or the decision site moved", name)
		}
	}
	if !strings.Contains(pollSrc, "setUpAppGitCredential(ctx, e.appGitUsesHTTPS())") {
		t.Error("poll.go Run() must pass e.appGitUsesHTTPS() to setUpAppGitCredential")
	}
}

func appGitKeys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func appGitBoolPtr(b bool) *bool { return &b }
