package cmd

import (
	"bufio"
	"io"
	"os"
	"strings"
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
