package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

func TestInvokeClaude_JSONOutput(t *testing.T) {
	binDir := t.TempDir()
	fakeClaude := filepath.Join(binDir, "claude")
	// Output a valid JSON envelope as --output-format json would.
	// Use printf '%s' to emit the string without interpreting escape sequences in the value,
	// keeping \n as JSON-encoded newlines (not literal newlines).
	script := `#!/bin/sh
cat >/dev/null
printf '%s\n' '{"result":"stage output\nFABRIK_STAGE_COMPLETE\n","session_id":"sess_json123","num_turns":12,"usage":{"input_tokens":5000,"output_tokens":1000}}'
`
	if err := os.WriteFile(fakeClaude, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}

	origPath := os.Getenv("PATH")
	os.Setenv("PATH", binDir+":"+origPath)
	defer os.Setenv("PATH", origPath)

	workDir := t.TempDir()
	stage := &stages.Stage{
		Name:       "Research",
		Prompt:     "Do research",
		MaxTurns:   30,
		Completion: stages.CompletionCriteria{Type: "claude"},
	}
	issue := gh.ProjectItem{Number: 77, Title: "Test JSON"}
	defer os.RemoveAll(SessionDir(77))
	defer os.RemoveAll(LogDir(77))

	output, completed, stats, err := InvokeClaude(context.Background(), stage, issue, nil, false, workDir, "")
	if err != nil {
		t.Fatalf("InvokeClaude: %v", err)
	}
	if !strings.Contains(output, "stage output") {
		t.Errorf("output = %q, expected to contain 'stage output'", output)
	}
	if !completed {
		t.Error("expected completed=true")
	}
	if stats.TurnsUsed != 12 {
		t.Errorf("TurnsUsed = %d, want 12", stats.TurnsUsed)
	}
	if stats.MaxTurns != 30 {
		t.Errorf("MaxTurns = %d, want 30", stats.MaxTurns)
	}
	if stats.InputTokens != 5000 {
		t.Errorf("InputTokens = %d, want 5000", stats.InputTokens)
	}
	if stats.OutputTokens != 1000 {
		t.Errorf("OutputTokens = %d, want 1000", stats.OutputTokens)
	}

	// Check session file was saved from JSON
	data, err := os.ReadFile(sessionFile(77, "Research"))
	if err != nil {
		t.Fatalf("reading session file: %v", err)
	}
	if string(data) != "sess_json123" {
		t.Errorf("session = %q, want sess_json123", string(data))
	}
}

func TestRealClaudeInvoker_ImplementsInterface(t *testing.T) {
	// Compile-time check that RealClaudeInvoker implements ClaudeInvoker
	var _ ClaudeInvoker = &RealClaudeInvoker{}
}

func TestRealClaudeInvoker_Invoke(t *testing.T) {
	binDir := t.TempDir()
	fakeClaude := filepath.Join(binDir, "claude")
	script := `#!/bin/sh
cat >/dev/null
printf '%s\n' '{"result":"real invoker output","session_id":"sess_ri","num_turns":1,"total_cost_usd":0.001}'
`
	if err := os.WriteFile(fakeClaude, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}

	origPath := os.Getenv("PATH")
	os.Setenv("PATH", binDir+":"+origPath)
	defer os.Setenv("PATH", origPath)

	invoker := &RealClaudeInvoker{}
	stage := &stages.Stage{
		Name:       "Test",
		Prompt:     "test",
		Completion: stages.CompletionCriteria{Type: "claude"},
	}
	issue := gh.ProjectItem{Number: 80, Title: "T"}

	output, _, _, err := invoker.Invoke(context.Background(), stage, issue, nil, false, t.TempDir(), "")
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !strings.Contains(output, "real invoker output") {
		t.Errorf("output = %q", output)
	}
	os.RemoveAll(SessionDir(80))
	os.RemoveAll(LogDir(80))
}

func TestInvokeClaude_FakeBinary(t *testing.T) {
	// Create a fake claude binary that outputs a valid JSON envelope
	binDir := t.TempDir()
	fakeClaude := filepath.Join(binDir, "claude")
	script := `#!/bin/sh
cat >/dev/null
printf '%s\n' '{"result":"Claude output for test\nFABRIK_STAGE_COMPLETE\n","session_id":"sess_test123","num_turns":3,"total_cost_usd":0.001,"is_error":false}'
`
	if err := os.WriteFile(fakeClaude, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}

	// Prepend fake binary dir to PATH
	origPath := os.Getenv("PATH")
	os.Setenv("PATH", binDir+":"+origPath)
	defer os.Setenv("PATH", origPath)

	workDir := t.TempDir()
	stage := &stages.Stage{
		Name:       "Research",
		Prompt:     "Do research",
		Completion: stages.CompletionCriteria{Type: "claude"},
	}
	issue := gh.ProjectItem{
		Number: 42,
		Title:  "Test Issue",
		Body:   "Body text",
		URL:    "https://example.com",
	}

	output, completed, _, err := InvokeClaude(context.Background(), stage, issue, nil, false, workDir, "")
	if err != nil {
		t.Fatalf("InvokeClaude: %v", err)
	}
	if !strings.Contains(output, "Claude output for test") {
		t.Errorf("output = %q", output)
	}
	if !completed {
		t.Error("expected completed=true")
	}

	// Check session file was saved from JSON
	sessFile := sessionFile(42, "Research")
	data, err := os.ReadFile(sessFile)
	if err != nil {
		t.Fatalf("reading session file: %v", err)
	}
	if string(data) != "sess_test123" {
		t.Errorf("session = %q", string(data))
	}
	// Cleanup
	os.RemoveAll(SessionDir(42))
	os.RemoveAll(LogDir(42))
}

func TestInvokeClaude_WithResume(t *testing.T) {
	binDir := t.TempDir()
	argsFile := filepath.Join(binDir, "args.txt")
	fakeClaude := filepath.Join(binDir, "claude")
	script := fmt.Sprintf(`#!/bin/sh
cat >/dev/null
echo "$@" > %s
printf '%%s\n' '{"result":"resume output","session_id":"sess_resume","num_turns":1,"total_cost_usd":0.001}'
`, argsFile)
	if err := os.WriteFile(fakeClaude, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}

	origPath := os.Getenv("PATH")
	os.Setenv("PATH", binDir+":"+origPath)
	defer os.Setenv("PATH", origPath)

	workDir := t.TempDir()
	stage := &stages.Stage{
		Name:       "Plan",
		Prompt:     "Plan",
		Completion: stages.CompletionCriteria{Type: "claude"},
	}
	issue := gh.ProjectItem{Number: 99, Title: "T"}

	// Write a session file to enable resume
	sessDir := SessionDir(99)
	os.MkdirAll(sessDir, 0700)
	os.WriteFile(sessionFile(99, "Plan"), []byte("sess_existing"), 0600)
	defer os.RemoveAll(sessDir)
	defer os.RemoveAll(LogDir(99))

	_, _, _, err := InvokeClaude(context.Background(), stage, issue, nil, true, workDir, "")
	if err != nil {
		t.Fatalf("InvokeClaude: %v", err)
	}
	args, _ := os.ReadFile(argsFile)
	if !strings.Contains(string(args), "--resume") {
		t.Errorf("expected --resume in args, got: %q", string(args))
	}

	if info, err := os.Stat(sessDir); err != nil {
		t.Fatalf("stat session dir: %v", err)
	} else if perm := info.Mode().Perm(); perm != 0700 {
		t.Errorf("session dir mode = %04o, want 0700", perm)
	}
}

func TestInvokeClaude_WithModelAndTools(t *testing.T) {
	binDir := t.TempDir()
	argsFile := filepath.Join(binDir, "args.txt")
	fakeClaude := filepath.Join(binDir, "claude")
	script := fmt.Sprintf(`#!/bin/sh
cat >/dev/null
echo "$@" > %s
printf '%%s\n' '{"result":"ok","session_id":"sess_mt","num_turns":1,"total_cost_usd":0.001}'
`, argsFile)
	if err := os.WriteFile(fakeClaude, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}

	origPath := os.Getenv("PATH")
	os.Setenv("PATH", binDir+":"+origPath)
	defer os.Setenv("PATH", origPath)

	workDir := t.TempDir()
	stage := &stages.Stage{
		Name:         "Impl",
		Prompt:       "Implement",
		Model:        "opus",
		MaxTurns:     10,
		AllowedTools: []string{"Read", "Write"},
		Completion:   stages.CompletionCriteria{Type: "claude"},
	}
	issue := gh.ProjectItem{Number: 50, Title: "T"}

	_, _, _, err := InvokeClaude(context.Background(), stage, issue, nil, false, workDir, "")
	if err != nil {
		t.Fatalf("InvokeClaude: %v", err)
	}
	args, _ := os.ReadFile(argsFile)
	argsStr := string(args)
	if !strings.Contains(argsStr, "--model") {
		t.Error("expected --model in args")
	}
	if !strings.Contains(argsStr, "opus") {
		t.Error("expected opus in args")
	}
	if !strings.Contains(argsStr, "--max-turns") {
		t.Error("expected --max-turns in args")
	}
	if !strings.Contains(argsStr, "--allowedTools") {
		t.Error("expected --allowedTools in args")
	}
	os.RemoveAll(SessionDir(50))
	os.RemoveAll(LogDir(50))
}

func TestInvokeClaude_WithModelOverride(t *testing.T) {
	binDir := t.TempDir()
	argsFile := filepath.Join(binDir, "args.txt")
	fakeClaude := filepath.Join(binDir, "claude")
	script := fmt.Sprintf(`#!/bin/sh
cat >/dev/null
echo "$@" > %s
printf '%%s\n' '{"result":"ok","session_id":"sess_mo","num_turns":1,"total_cost_usd":0.001}'
`, argsFile)
	if err := os.WriteFile(fakeClaude, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}

	origPath := os.Getenv("PATH")
	os.Setenv("PATH", binDir+":"+origPath)
	defer os.Setenv("PATH", origPath)

	workDir := t.TempDir()
	stage := &stages.Stage{
		Name:       "Test",
		Prompt:     "test",
		Model:      "sonnet", // stage model
		Completion: stages.CompletionCriteria{Type: "claude"},
	}
	issue := gh.ProjectItem{Number: 51, Title: "T"}

	_, _, _, err := InvokeClaude(context.Background(), stage, issue, nil, false, workDir, "opus")
	if err != nil {
		t.Fatalf("InvokeClaude: %v", err)
	}
	args, _ := os.ReadFile(argsFile)
	if !strings.Contains(string(args), "opus") {
		t.Error("expected model override 'opus' in args")
	}
	os.RemoveAll(SessionDir(51))
	os.RemoveAll(LogDir(51))
}

func TestInvokeClaude_BinaryError(t *testing.T) {
	binDir := t.TempDir()
	fakeClaude := filepath.Join(binDir, "claude")
	// Binary exits with error but still outputs valid JSON with partial result
	script := `#!/bin/sh
cat >/dev/null
printf '%s\n' '{"result":"partial output","session_id":"sess_err","num_turns":5,"total_cost_usd":0.01,"is_error":true}'
exit 1
`
	if err := os.WriteFile(fakeClaude, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}

	origPath := os.Getenv("PATH")
	os.Setenv("PATH", binDir+":"+origPath)
	defer os.Setenv("PATH", origPath)

	workDir := t.TempDir()
	stage := &stages.Stage{
		Name:       "Test",
		Prompt:     "test",
		Completion: stages.CompletionCriteria{Type: "claude"},
	}
	issue := gh.ProjectItem{Number: 60, Title: "T"}

	output, _, _, err := InvokeClaude(context.Background(), stage, issue, nil, false, workDir, "")
	if err == nil {
		t.Fatal("expected error for failing binary")
	}
	if !strings.Contains(output, "partial output") {
		t.Errorf("expected partial output, got: %q", output)
	}
	os.RemoveAll(SessionDir(60))
	os.RemoveAll(LogDir(60))
}

func TestInvokeClaude_WithComments(t *testing.T) {
	binDir := t.TempDir()
	stdinFile := filepath.Join(binDir, "stdin.txt")
	fakeClaude := filepath.Join(binDir, "claude")
	script := fmt.Sprintf(`#!/bin/sh
cat > %s
printf '%%s\n' '{"result":"comment output","session_id":"sess_c","num_turns":1,"total_cost_usd":0.001}'
`, stdinFile)
	if err := os.WriteFile(fakeClaude, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}

	origPath := os.Getenv("PATH")
	os.Setenv("PATH", binDir+":"+origPath)
	defer os.Setenv("PATH", origPath)

	workDir := t.TempDir()
	stage := &stages.Stage{
		Name:       "Test",
		Prompt:     "test",
		Completion: stages.CompletionCriteria{Type: "claude"},
	}
	issue := gh.ProjectItem{Number: 70, Title: "T"}
	comments := []gh.Comment{
		{Author: "user", Body: "Fix this", CreatedAt: time.Now()},
	}

	_, _, _, err := InvokeClaude(context.Background(), stage, issue, comments, false, workDir, "")
	if err != nil {
		t.Fatalf("InvokeClaude: %v", err)
	}
	stdin, _ := os.ReadFile(stdinFile)
	if !strings.Contains(string(stdin), "New Comments") {
		t.Errorf("expected comments in prompt, stdin: %q", string(stdin))
	}
	os.RemoveAll(SessionDir(70))
	os.RemoveAll(LogDir(70))
}

func TestDirectInvocation(t *testing.T) {
	// Verify that InvokeClaude runs Claude directly (exec.CommandContext with piped stdin/stdout).
	binDir := t.TempDir()
	fakeClaude := filepath.Join(binDir, "claude")
	script := `#!/bin/sh
cat >/dev/null
printf '%s\n' '{"result":"fallback output\nFABRIK_STAGE_COMPLETE\n","session_id":"sess_fallback","num_turns":1}'
`
	if err := os.WriteFile(fakeClaude, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}

	origPath := os.Getenv("PATH")
	os.Setenv("PATH", binDir+":"+origPath)
	defer os.Setenv("PATH", origPath)

	workDir := t.TempDir()
	stage := &stages.Stage{
		Name:       "Test",
		Prompt:     "test prompt",
		Completion: stages.CompletionCriteria{Type: "claude"},
	}
	issue := gh.ProjectItem{Number: 74, Title: "direct invocation test"}
	defer os.RemoveAll(SessionDir(74))
	defer os.RemoveAll(LogDir(74))

	output, completed, _, err := InvokeClaude(context.Background(), stage, issue, nil, false, workDir, "")
	if err != nil {
		t.Fatalf("InvokeClaude: %v", err)
	}
	if !strings.Contains(output, "fallback output") {
		t.Errorf("output = %q, expected 'fallback output'", output)
	}
	if !completed {
		t.Error("expected completed=true")
	}
}

func TestSaveDebugLog(t *testing.T) {
	dir := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { os.Chdir(orig) })

	const want = "hello debug output"
	saveDebugLog(42, "Research", want)

	debugDir := filepath.Join(dir, ".fabrik", "debug")
	entries, err := os.ReadDir(debugDir)
	if err != nil {
		t.Fatalf("reading debug dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 debug file, got %d", len(entries))
	}
	name := entries[0].Name()
	if !strings.HasPrefix(name, "issue-42_") || !strings.HasSuffix(name, "_Research.log") {
		t.Errorf("unexpected filename %q", name)
	}
	data, err := os.ReadFile(filepath.Join(debugDir, name))
	if err != nil {
		t.Fatalf("reading debug file: %v", err)
	}
	if string(data) != want {
		t.Errorf("debug file content = %q, want %q", string(data), want)
	}
	info, err := entries[0].Info()
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("file permissions = %o, want 0600", info.Mode().Perm())
	}
}

func TestSaveDebugLog_UniqueNames(t *testing.T) {
	dir := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { os.Chdir(orig) })

	saveDebugLog(1, "Implement", "output-a")
	saveDebugLog(1, "Implement", "output-b")

	debugDir := filepath.Join(dir, ".fabrik", "debug")
	entries, err := os.ReadDir(debugDir)
	if err != nil {
		t.Fatalf("reading debug dir: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("expected 2 distinct debug files, got %d (timestamp collision?)", len(entries))
	}
}
