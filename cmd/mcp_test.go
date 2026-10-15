package cmd

import (
	"bufio"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/internal/localapi"
)

type mcpStubBackend struct{}

func (mcpStubBackend) Status(p localapi.StatusParams) (*localapi.StatusResult, error) {
	return &localapi.StatusResult{Issue: p.Issue}, nil
}
func (mcpStubBackend) Board(p localapi.BoardParams) (*localapi.BoardResult, error) {
	return &localapi.BoardResult{View: p.View}, nil
}
func (mcpStubBackend) Health(localapi.HealthParams) (*localapi.HealthResult, error) {
	return &localapi.HealthResult{Version: "from-daemon"}, nil
}

// runMCPWithStdio runs `fabrik mcp <args>` via Execute() with os.Stdin/os.Stdout
// replaced by pipes, feeding input and returning everything written to stdout.
func runMCPWithStdio(t *testing.T, input string, args ...string) string {
	t.Helper()
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldIn, oldOut, oldArgs := os.Stdin, os.Stdout, os.Args
	os.Stdin, os.Stdout = inR, outW
	defer func() { os.Stdin, os.Stdout, os.Args = oldIn, oldOut, oldArgs }()

	resetFlags()
	os.Args = append([]string{"fabrik", "mcp"}, args...)

	errCh := make(chan error, 1)
	go func() { errCh <- Execute() }()
	if _, err := io.WriteString(inW, input); err != nil {
		t.Fatal(err)
	}
	inW.Close()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Execute(mcp): %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("fabrik mcp did not exit at stdin EOF")
	}
	outW.Close()
	b, _ := io.ReadAll(bufio.NewReader(outR))
	return string(b)
}

const mcpHandshake = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}` + "\n" +
	`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n" +
	`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"fabrik_health","arguments":{}}}` + "\n"

// `fabrik mcp --dir X` reaches the daemon whose fabrikDir is X, and stdout
// carries JSON-RPC frames only.
func TestExecute_MCPSubcommandProxiesToDaemonForDir(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "fm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	srv := localapi.NewServer(localapi.SocketPath(dir), mcpStubBackend{}, nil)
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	out := runMCPWithStdio(t, mcpHandshake, "--dir", dir)
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 frames on stdout (initialize, tools/call), got %d: %q", len(lines), out)
	}
	for _, l := range lines {
		if !strings.HasPrefix(l, `{"jsonrpc":"2.0"`) {
			t.Errorf("stdout carries a non-protocol line: %q", l)
		}
	}
	// The server handles each request line concurrently, so the two
	// responses may arrive in either order (JSON-RPC matches by id).
	if !strings.Contains(out, "from-daemon") {
		t.Errorf("tools/call did not reach the daemon for --dir: %s", out)
	}
}

func TestExecute_MCPSubcommandNoDaemonIsAToolError(t *testing.T) {
	dir := t.TempDir()
	out := runMCPWithStdio(t, mcpHandshake, "--dir", dir)
	if !strings.Contains(out, "Fabrik daemon not running at "+localapi.SocketPath(dir)) || !strings.Contains(out, `"isError":true`) {
		t.Errorf("want a 'daemon not running' tool error naming the socket path, got %s", out)
	}
}

// Without --dir the current directory is used (Claude Code launches the server
// in its own cwd, which is why --dir exists).
func TestExecute_MCPSubcommandDefaultsToCurrentDirectory(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "fm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	t.Setenv("FABRIK_MCP_DIR", "")
	t.Setenv("CLAUDE_PROJECT_DIR", "")
	chdirTest(t, dir)
	wd, _ := os.Getwd()
	out := runMCPWithStdio(t, mcpHandshake)
	if !strings.Contains(out, "Fabrik daemon not running at "+localapi.SocketPath(wd)) {
		t.Errorf("default dir should be the cwd (%s): %s", wd, out)
	}
}

func TestRunMCPRejectsStrayArguments(t *testing.T) {
	resetFlags()
	if err := runMCP([]string{"unexpected"}); err == nil || !strings.Contains(err.Error(), "unexpected argument") {
		t.Errorf("runMCP(stray) = %v", err)
	}
	if err := runMCP([]string{"--no-such-flag"}); err == nil {
		t.Error("an unknown flag must be an error")
	}
}

func TestSubscriberNameFlagBeatsEnvAndThereIsNoGeneratedDefault(t *testing.T) {
	t.Setenv("FABRIK_SUBSCRIBER", "from-env")
	if got := subscriberName("from-flag"); got != "from-flag" {
		t.Errorf("flag must win: %q", got)
	}
	if got := subscriberName(""); got != "from-env" {
		t.Errorf("env fallback: %q", got)
	}
	t.Setenv("FABRIK_SUBSCRIBER", "")
	if got := subscriberName(""); got != "" {
		t.Errorf("no name must stay empty (push off), got %q", got)
	}
}

// The directory precedence is --dir > $FABRIK_MCP_DIR > $CLAUDE_PROJECT_DIR >
// cwd, and an empty variable (an unset plugin setting) counts as unset (#2141).
func TestResolveMCPDirPrecedence(t *testing.T) {
	// A /tmp fixture with no .fabrik/ above it: the repo checkout this test runs
	// in tracks its own .fabrik/, which the cwd walk would otherwise find.
	chdirTest(t, mcpFixture(t))
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("FABRIK_MCP_DIR", "/from/mcp-dir")
	t.Setenv("CLAUDE_PROJECT_DIR", "/from/project")
	if got, _ := resolveMCPDir("/from/flag"); got != "/from/flag" {
		t.Errorf("flag must win: %q", got)
	}
	if got, _ := resolveMCPDir(""); got != "/from/mcp-dir" {
		t.Errorf("FABRIK_MCP_DIR must beat CLAUDE_PROJECT_DIR: %q", got)
	}
	t.Setenv("FABRIK_MCP_DIR", "")
	if got, _ := resolveMCPDir(""); got != "/from/project" {
		t.Errorf("CLAUDE_PROJECT_DIR fallback: %q", got)
	}
	t.Setenv("CLAUDE_PROJECT_DIR", "")
	if got, _ := resolveMCPDir(""); got != wd {
		t.Errorf("cwd fallback: got %q want %q", got, wd)
	}
}

// With no daemon, `fabrik mcp` writes nothing to stderr at startup — a user-scope
// plugin install starts it in every project, Fabrik or not (#2141).
func TestExecute_MCPNoDaemonStartupIsQuietOnStderr(t *testing.T) {
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldErr := os.Stderr
	os.Stderr = errW
	t.Setenv("FABRIK_MCP_DIR", "")
	t.Setenv("CLAUDE_PROJECT_DIR", "")
	runMCPWithStdio(t, mcpHandshake, "--dir", t.TempDir())
	os.Stderr = oldErr
	errW.Close()
	b, _ := io.ReadAll(errR)
	if len(b) != 0 {
		t.Errorf("fabrik mcp wrote to stderr at startup: %q", b)
	}
}

// If Claude Code leaves an unfilled plugin setting as its literal ${user_config.X}
// text instead of an empty string, it must read as unset, not as a subscriber
// name or a directory (#2141).
func TestUnexpandedPluginPlaceholdersCountAsUnset(t *testing.T) {
	chdirTest(t, mcpFixture(t)) // outside any .fabrik/, see TestResolveMCPDirPrecedence
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("FABRIK_SUBSCRIBER", "${user_config.subscriber}")
	if got := subscriberName(""); got != "" {
		t.Errorf("placeholder subscriber must read as unset, got %q", got)
	}
	t.Setenv("FABRIK_MCP_DIR", "${user_config.fabrik_dir}")
	t.Setenv("CLAUDE_PROJECT_DIR", "/from/project")
	if got, _ := resolveMCPDir(""); got != "/from/project" {
		t.Errorf("placeholder FABRIK_MCP_DIR must fall through to CLAUDE_PROJECT_DIR, got %q", got)
	}
	t.Setenv("CLAUDE_PROJECT_DIR", "${CLAUDE_PROJECT_DIR}")
	if got, _ := resolveMCPDir(""); got != wd {
		t.Errorf("all placeholders must fall through to cwd: got %q want %q", got, wd)
	}
}

// mcpFixture returns a short /tmp directory (short enough for a unix socket path)
// outside the user's home and the repo checkout, with no .fabrik/ anywhere above.
func mcpFixture(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "fm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func mkdirs(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// resolved is resolveMCPDir with the environment pinned: no flag, no
// FABRIK_MCP_DIR, the given CLAUDE_PROJECT_DIR, HOME set to home.
func resolvedFrom(t *testing.T, projectDir, home string) string {
	t.Helper()
	t.Setenv("FABRIK_MCP_DIR", "")
	t.Setenv("CLAUDE_PROJECT_DIR", projectDir)
	t.Setenv("HOME", home)
	got, err := resolveMCPDir("")
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestFindFabrikRoot(t *testing.T) {
	root := mcpFixture(t)
	repo := filepath.Join(root, "repo")
	deep := filepath.Join(repo, "sub", "deep")
	mkdirs(t, filepath.Join(repo, ".fabrik"), deep)

	t.Run("walks up to the nearest ancestor with .fabrik", func(t *testing.T) {
		for _, start := range []string{repo, filepath.Join(repo, "sub"), deep} {
			if got, ok := findFabrikRoot(start, ""); !ok || got != repo {
				t.Errorf("findFabrikRoot(%s) = %q,%v want %q", start, got, ok, repo)
			}
		}
	})
	t.Run("no match", func(t *testing.T) {
		other := filepath.Join(root, "other", "x")
		mkdirs(t, other)
		if got, ok := findFabrikRoot(other, ""); ok {
			t.Errorf("unexpected match %q", got)
		}
	})
	t.Run("nearest of nested projects wins", func(t *testing.T) {
		inner := filepath.Join(repo, "vendor", "inner")
		mkdirs(t, filepath.Join(inner, ".fabrik"), filepath.Join(inner, "src"))
		if got, ok := findFabrikRoot(filepath.Join(inner, "src"), ""); !ok || got != inner {
			t.Errorf("got %q,%v want %q", got, ok, inner)
		}
	})
	t.Run("a .fabrik plain file is not a project", func(t *testing.T) {
		p := filepath.Join(root, "filep")
		mkdirs(t, filepath.Join(p, "sub"))
		if err := os.WriteFile(filepath.Join(p, ".fabrik"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got, ok := findFabrikRoot(filepath.Join(p, "sub"), ""); ok {
			t.Errorf("a file named .fabrik matched %q", got)
		}
	})
	t.Run("stops at home without looking above it", func(t *testing.T) {
		above := filepath.Join(root, "above")
		home := filepath.Join(above, "home")
		work := filepath.Join(home, "proj", "sub")
		mkdirs(t, filepath.Join(above, ".fabrik"), work)
		if got, ok := findFabrikRoot(work, home); ok {
			t.Errorf("walk crossed home and matched %q", got)
		}
		// Without a home boundary the same walk does find it.
		if got, ok := findFabrikRoot(work, ""); !ok || got != above {
			t.Errorf("no-home walk = %q,%v want %q", got, ok, above)
		}
	})
	t.Run("home itself is checked", func(t *testing.T) {
		home := filepath.Join(root, "home2")
		work := filepath.Join(home, "a", "b")
		mkdirs(t, filepath.Join(home, ".fabrik"), work)
		if got, ok := findFabrikRoot(work, home); !ok || got != home {
			t.Errorf("got %q,%v want %q", got, ok, home)
		}
	})
	t.Run("a start outside home walks to the root", func(t *testing.T) {
		home := filepath.Join(root, "elsewhere-home")
		mkdirs(t, home)
		if got, ok := findFabrikRoot(deep, home); !ok || got != repo {
			t.Errorf("got %q,%v want %q", got, ok, repo)
		}
	})
}

// A session in a subdirectory of a Fabrik project resolves to the project (#2207).
func TestResolveMCPDirWalksUpFromDerivedSources(t *testing.T) {
	root := mcpFixture(t)
	repo := filepath.Join(root, "repo")
	sub := filepath.Join(repo, "sub")
	mkdirs(t, filepath.Join(repo, ".fabrik"), sub)
	home := filepath.Join(root, "home")
	mkdirs(t, home)

	if got := resolvedFrom(t, sub, home); got != repo {
		t.Errorf("CLAUDE_PROJECT_DIR=repo/sub resolved %q, want %q", got, repo)
	}

	t.Setenv("CLAUDE_PROJECT_DIR", "")
	chdirTest(t, sub)
	got, err := resolveMCPDir("")
	if err != nil {
		t.Fatal(err)
	}
	if wantRepo, _ := filepath.EvalSymlinks(repo); got != repo && got != wantRepo {
		t.Errorf("cwd=repo/sub resolved %q, want %q", got, repo)
	}
}

func TestResolveMCPDirLiteralSourcesAreNotWalked(t *testing.T) {
	root := mcpFixture(t)
	repo := filepath.Join(root, "repo")
	sub := filepath.Join(repo, "sub")
	mkdirs(t, filepath.Join(repo, ".fabrik"), sub)
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("CLAUDE_PROJECT_DIR", "")

	t.Setenv("FABRIK_MCP_DIR", "")
	if got, _ := resolveMCPDir(sub); got != sub {
		t.Errorf("--dir was walked: %q", got)
	}
	t.Setenv("FABRIK_MCP_DIR", sub)
	if got, _ := resolveMCPDir(""); got != sub {
		t.Errorf("FABRIK_MCP_DIR was walked: %q", got)
	}
}

func TestResolveMCPDirWithoutAProjectKeepsTheOriginalDirectory(t *testing.T) {
	root := mcpFixture(t)
	work := filepath.Join(root, "plain", "sub")
	mkdirs(t, work)
	if got := resolvedFrom(t, work, filepath.Join(root, "home")); got != work {
		t.Errorf("resolved %q, want the original %q", got, work)
	}
}

// End to end: a session in repo/sub reaches the daemon whose directory is repo.
func TestExecute_MCPSubcommandFromSubdirectoryReachesProjectDaemon(t *testing.T) {
	root := mcpFixture(t)
	repo := filepath.Join(root, "repo")
	sub := filepath.Join(repo, "sub")
	mkdirs(t, filepath.Join(repo, ".fabrik", "state"), sub)
	srv := localapi.NewServer(localapi.SocketPath(repo), mcpStubBackend{}, nil)
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	t.Setenv("FABRIK_MCP_DIR", "")
	t.Setenv("CLAUDE_PROJECT_DIR", sub)
	t.Setenv("HOME", filepath.Join(root, "home"))
	out := runMCPWithStdio(t, mcpHandshake)
	if !strings.Contains(out, "from-daemon") {
		t.Errorf("the subdirectory session did not reach the project's daemon: %s", out)
	}
}

func TestSettingValuePlaceholders(t *testing.T) {
	for in, want := range map[string]string{
		"":                          "",
		"plain":                     "plain",
		"/a/b":                      "/a/b",
		"${user_config.subscriber}": "",
		"${CLAUDE_PROJECT_DIR}":     "",
		"pre-${x}-post":             "",
	} {
		if got := settingValue(in); got != want {
			t.Errorf("settingValue(%q) = %q, want %q", in, got, want)
		}
	}
	if !isUnexpandedPlaceholder("${x}") || isUnexpandedPlaceholder("x") {
		t.Error("isUnexpandedPlaceholder is wrong")
	}
}

// Outside a Fabrik project a configured subscriber starts no stream (#2207);
// inside one it starts exactly one.
func TestNewMCPServerStreamsOnlyInsideAFabrikProject(t *testing.T) {
	root := mcpFixture(t)
	project := filepath.Join(root, "project")
	plain := filepath.Join(root, "plain")
	mkdirs(t, filepath.Join(project, ".fabrik"), plain)
	t.Setenv("FABRIK_MCP_DIR", "")
	t.Setenv("CLAUDE_PROJECT_DIR", "")
	t.Setenv("FABRIK_SUBSCRIBER", "")

	for _, tc := range []struct {
		name  string
		dir   string
		calls int32
	}{{"outside a project", plain, 0}, {"inside a project", project, 1}} {
		t.Run(tc.name, func(t *testing.T) {
			srv, err := newMCPServer(tc.dir, "my-topic")
			if err != nil {
				t.Fatal(err)
			}
			if srv.Subscriber != "my-topic" {
				t.Errorf("subscriber must be kept either way, got %q", srv.Subscriber)
			}
			var calls atomic.Int32
			started := make(chan struct{}, 1)
			srv.Stream = func(ctx context.Context, _ localapi.StreamOptions) error {
				calls.Add(1)
				select {
				case started <- struct{}{}:
				default:
				}
				<-ctx.Done()
				return nil
			}
			var out strings.Builder
			srv.In = strings.NewReader(mcpHandshake)
			srv.Out = &lockedWriter{w: &out}
			srv.Err = io.Discard
			if err := srv.Serve(context.Background()); err != nil {
				t.Fatal(err)
			}
			if tc.calls == 1 {
				select {
				case <-started:
				case <-time.After(5 * time.Second):
					t.Fatal("stream never started inside a project")
				}
			}
			if got := calls.Load(); got != tc.calls {
				t.Errorf("stream calls = %d, want %d", got, tc.calls)
			}
			if strings.Contains(out.String(), "daemon-unreachable") {
				t.Errorf("unexpected daemon-unreachable: %s", out.String())
			}
			if tc.calls == 0 && !strings.Contains(out.String(), "Fabrik daemon not running at ") {
				t.Errorf("tool should answer 'daemon not running' outside a project: %s", out.String())
			}
		})
	}
}

type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
