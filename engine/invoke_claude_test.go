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

func TestInvokeClaude_MarkerOnNonZeroExit(t *testing.T) {
	// REQ-6: when Claude exits non-zero but FABRIK_STAGE_COMPLETE is in output,
	// InvokeClaude must return completed=true AND a non-nil error.
	binDir := t.TempDir()
	fakeClaude := filepath.Join(binDir, "claude")
	script := `#!/bin/sh
cat >/dev/null
printf '%s\n' '{"result":"work done\nFABRIK_STAGE_COMPLETE","session_id":"sess_mk","num_turns":10,"total_cost_usd":0.02,"is_error":true}'
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
	issue := gh.ProjectItem{Number: 61, Title: "T"}

	output, completed, _, err := InvokeClaude(context.Background(), stage, issue, nil, false, workDir, "")
	if err == nil {
		t.Fatal("expected non-nil error for failing binary")
	}
	if !completed {
		t.Errorf("expected completed=true when marker present in output, got false; output=%q", output)
	}
	os.RemoveAll(SessionDir(61))
	os.RemoveAll(LogDir(61))
}

func TestInvokeClaude_MarkerOnCancelledCtx(t *testing.T) {
	// REQ-3/REQ-6: when context is cancelled, FABRIK_STAGE_COMPLETE in output must
	// NOT cause completed=true — the engine is shutting down.
	binDir := t.TempDir()
	fakeClaude := filepath.Join(binDir, "claude")
	script := `#!/bin/sh
cat >/dev/null
printf '%s\n' '{"result":"work done\nFABRIK_STAGE_COMPLETE","session_id":"sess_cx","num_turns":5,"total_cost_usd":0.01,"is_error":true}'
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
	issue := gh.ProjectItem{Number: 62, Title: "T"}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before invoking

	_, completed, _, err := InvokeClaude(ctx, stage, issue, nil, false, workDir, "")
	if err == nil {
		t.Fatal("expected non-nil error for cancelled context")
	}
	if completed {
		t.Error("expected completed=false when context is cancelled, even with marker present")
	}
	os.RemoveAll(SessionDir(62))
	os.RemoveAll(LogDir(62))
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

// TestRunClaude_StdoutTeeToLogFile verifies that runClaude tees Claude's stdout
// (NDJSON stream-json) to the .log file in real time via io.MultiWriter.
// The test uses a fake claude binary that outputs NDJSON stream-json format
// and asserts that the .log file contains the same NDJSON content.
func TestRunClaude_StdoutTeeToLogFile(t *testing.T) {
	binDir := t.TempDir()
	fakeClaude := filepath.Join(binDir, "claude")
	// Emit two NDJSON lines: one assistant message and one result (stream-json format).
	ndjson := `{"type":"assistant","message":{"content":[{"type":"text","text":"hello from stream-json"}]}}
{"type":"result","subtype":"success","result":"stage text\nFABRIK_STAGE_COMPLETE\n","session_id":"sess_tee","num_turns":2,"total_cost_usd":0.002}
`
	// Write the NDJSON to a temp file so the fake binary can cat it.
	ndjsonFile := filepath.Join(binDir, "output.ndjson")
	if err := os.WriteFile(ndjsonFile, []byte(ndjson), 0600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\ncat >/dev/null\ncat " + ndjsonFile + "\n"
	if err := os.WriteFile(fakeClaude, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}

	origPath := os.Getenv("PATH")
	os.Setenv("PATH", binDir+":"+origPath)
	defer os.Setenv("PATH", origPath)

	workDir := t.TempDir()
	stage := &stages.Stage{
		Name:       "Tee",
		Prompt:     "tee test",
		Completion: stages.CompletionCriteria{Type: "claude"},
	}
	issue := gh.ProjectItem{Number: 200, Title: "Tee test"}
	defer os.RemoveAll(SessionDir(200))
	defer os.RemoveAll(LogDir(200))

	output, completed, stats, err := InvokeClaude(context.Background(), stage, issue, nil, false, workDir, "")
	if err != nil {
		t.Fatalf("InvokeClaude: %v", err)
	}
	if !strings.Contains(output, "stage text") {
		t.Errorf("output = %q, expected 'stage text'", output)
	}
	if !completed {
		t.Error("expected completed=true")
	}
	if stats.TurnsUsed != 2 {
		t.Errorf("TurnsUsed = %d, want 2", stats.TurnsUsed)
	}

	// Verify the .log file was created and contains the NDJSON content.
	logDir := LogDir(200)
	entries, err := os.ReadDir(logDir)
	if err != nil {
		t.Fatalf("reading log dir %s: %v", logDir, err)
	}
	var logFiles []os.DirEntry
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".log") {
			logFiles = append(logFiles, e)
		}
	}
	if len(logFiles) == 0 {
		t.Fatal("no .log file found in LogDir")
	}
	logContent, err := os.ReadFile(filepath.Join(logDir, logFiles[0].Name()))
	if err != nil {
		t.Fatalf("reading log file: %v", err)
	}
	// The .log file should contain the raw NDJSON written by the fake claude.
	if !strings.Contains(string(logContent), `"type":"assistant"`) {
		t.Errorf(".log file content = %q, expected to contain NDJSON assistant message", string(logContent))
	}
	if !strings.Contains(string(logContent), `"type":"result"`) {
		t.Errorf(".log file content = %q, expected to contain NDJSON result message", string(logContent))
	}
	// Verify the session was saved from the result message.
	data, err := os.ReadFile(sessionFile(200, "Tee"))
	if err != nil {
		t.Fatalf("reading session file: %v", err)
	}
	if string(data) != "sess_tee" {
		t.Errorf("session = %q, want sess_tee", string(data))
	}
}

// TestRunClaude_IssueUpdateInIntermediateTurn verifies that when a fake claude binary
// emits FABRIK_ISSUE_UPDATE_BEGIN/END in an intermediate assistant turn (not in the
// result field), runClaude still returns text containing the update block.
func TestRunClaude_IssueUpdateInIntermediateTurn(t *testing.T) {
	binDir := t.TempDir()
	fakeClaude := filepath.Join(binDir, "claude")

	// 3-line NDJSON: assistant turn with update block, user turn, result with FABRIK_STAGE_COMPLETE.
	ndjson := `{"type":"assistant","message":{"content":[{"type":"text","text":"Here is the refined spec:\nFABRIK_ISSUE_UPDATE_BEGIN\n## My updated spec\nFABRIK_ISSUE_UPDATE_END\n"}]}}
{"type":"user","message":{"content":[{"type":"tool_result","content":"ok"}]}}
{"type":"result","subtype":"success","result":"All done.\nFABRIK_STAGE_COMPLETE\n","session_id":"sess_inter","num_turns":2,"total_cost_usd":0.001}
`
	ndjsonFile := filepath.Join(binDir, "output.ndjson")
	if err := os.WriteFile(ndjsonFile, []byte(ndjson), 0600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\ncat >/dev/null\ncat " + ndjsonFile + "\n"
	if err := os.WriteFile(fakeClaude, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}

	origPath := os.Getenv("PATH")
	os.Setenv("PATH", binDir+":"+origPath)
	defer os.Setenv("PATH", origPath)

	workDir := t.TempDir()
	stage := &stages.Stage{
		Name:       "Specify",
		Prompt:     "specify",
		Completion: stages.CompletionCriteria{Type: "claude"},
	}
	issue := gh.ProjectItem{Number: 205, Title: "Intermediate update test"}
	defer os.RemoveAll(SessionDir(205))
	defer os.RemoveAll(LogDir(205))

	output, completed, _, err := InvokeClaude(context.Background(), stage, issue, nil, false, workDir, "")
	if err != nil {
		t.Fatalf("InvokeClaude: %v", err)
	}
	if !completed {
		t.Error("expected completed=true")
	}
	if !strings.Contains(output, "FABRIK_ISSUE_UPDATE_BEGIN") {
		t.Errorf("output missing FABRIK_ISSUE_UPDATE_BEGIN; got: %q", output)
	}
	if !strings.Contains(output, "## My updated spec") {
		t.Errorf("output missing update body; got: %q", output)
	}
	if !strings.Contains(output, "FABRIK_ISSUE_UPDATE_END") {
		t.Errorf("output missing FABRIK_ISSUE_UPDATE_END; got: %q", output)
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

func TestCommentMaxTurns(t *testing.T) {
	tests := []struct {
		name            string
		maxTurns        int
		commentMaxTurns int
		want            int
	}{
		{
			name:            "explicit CommentMaxTurns used directly",
			maxTurns:        50,
			commentMaxTurns: 20,
			want:            20,
		},
		{
			name:            "explicit CommentMaxTurns=1 is respected",
			maxTurns:        50,
			commentMaxTurns: 1,
			want:            1,
		},
		{
			name:     "default when MaxTurns > 15: uses MaxTurns",
			maxTurns: 50,
			want:     50,
		},
		{
			name:     "default when MaxTurns == 15: returns 15",
			maxTurns: 15,
			want:     15,
		},
		{
			name:     "default when MaxTurns < 15: uses MaxTurns",
			maxTurns: 10,
			want:     10,
		},
		{
			name:     "default when MaxTurns == 0 (unlimited): returns 50",
			maxTurns: 0,
			want:     50,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stage := &stages.Stage{
				Name:            "Implement",
				Prompt:          "implement",
				MaxTurns:        tc.maxTurns,
				CommentMaxTurns: tc.commentMaxTurns,
			}
			got := commentMaxTurns(stage)
			if got != tc.want {
				t.Errorf("commentMaxTurns() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestInvokeClaudeForComments_UsesCommentMaxTurns(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	binDir := t.TempDir()
	argsFile := filepath.Join(binDir, "args.txt")
	fakeClaude := filepath.Join(binDir, "claude")
	script := fmt.Sprintf(`#!/bin/sh
cat >/dev/null
echo "$@" > %q
printf '%%s\n' '{"result":"comment done","session_id":"sess_cmt","num_turns":3,"total_cost_usd":0.001}'
`, argsFile)
	if err := os.WriteFile(fakeClaude, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

	workDir := t.TempDir()

	// Stage with MaxTurns=50 and explicit CommentMaxTurns=7
	stage := &stages.Stage{
		Name:            "Implement",
		Prompt:          "implement",
		MaxTurns:        50,
		CommentMaxTurns: 7,
		Completion:      stages.CompletionCriteria{Type: "claude"},
	}
	issue := gh.ProjectItem{Number: 200, Title: "T"}
	comments := []gh.Comment{
		{Author: "user", Body: "Please fix the bug", CreatedAt: time.Now()},
	}

	_, _, usage, err := InvokeClaudeForComments(context.Background(), stage, issue, comments, workDir, "")
	if err != nil {
		t.Fatalf("InvokeClaudeForComments: %v", err)
	}

	args, _ := os.ReadFile(argsFile)
	argsStr := string(args)

	// --max-turns should use CommentMaxTurns (7), not MaxTurns (50)
	if !strings.Contains(argsStr, "--max-turns") {
		t.Errorf("expected --max-turns in args, got: %q", argsStr)
	}
	if strings.Contains(argsStr, "50") {
		t.Errorf("stage MaxTurns (50) leaked into comment args: %q", argsStr)
	}
	if !strings.Contains(argsStr, "7") {
		t.Errorf("expected comment limit (7) in args: %q", argsStr)
	}

	// usage.MaxTurns should reflect the comment limit, not stage.MaxTurns
	if usage.MaxTurns != 7 {
		t.Errorf("usage.MaxTurns = %d, want 7", usage.MaxTurns)
	}
}

func TestInvokeClaudeForComments_DefaultMaxTurns(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	binDir := t.TempDir()
	argsFile := filepath.Join(binDir, "args.txt")
	fakeClaude := filepath.Join(binDir, "claude")
	script := fmt.Sprintf(`#!/bin/sh
cat >/dev/null
echo "$@" > %q
printf '%%s\n' '{"result":"done","session_id":"sess_def","num_turns":2,"total_cost_usd":0.001}'
`, argsFile)
	if err := os.WriteFile(fakeClaude, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

	workDir := t.TempDir()

	// Stage with MaxTurns=50, no CommentMaxTurns — comment processing
	// should use the same MaxTurns as the stage.
	stage := &stages.Stage{
		Name:       "Implement",
		Prompt:     "implement",
		MaxTurns:   50,
		Completion: stages.CompletionCriteria{Type: "claude"},
	}
	issue := gh.ProjectItem{Number: 201, Title: "T"}
	comments := []gh.Comment{
		{Author: "user", Body: "Update the README", CreatedAt: time.Now()},
	}

	_, _, usage, err := InvokeClaudeForComments(context.Background(), stage, issue, comments, workDir, "")
	if err != nil {
		t.Fatalf("InvokeClaudeForComments: %v", err)
	}

	args, _ := os.ReadFile(argsFile)
	argsStr := string(args)

	if !strings.Contains(argsStr, "50") {
		t.Errorf("expected stage MaxTurns (50) in comment args: %q", argsStr)
	}

	if usage.MaxTurns != 50 {
		t.Errorf("usage.MaxTurns = %d, want 50", usage.MaxTurns)
	}
}

func TestInvokeClaudeForComments_UnlimitedStageDefaultsTo50(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	binDir := t.TempDir()
	argsFile := filepath.Join(binDir, "args.txt")
	fakeClaude := filepath.Join(binDir, "claude")
	script := fmt.Sprintf(`#!/bin/sh
cat >/dev/null
echo "$@" > %q
printf '%%s\n' '{"result":"done","session_id":"sess_unl","num_turns":2,"total_cost_usd":0.001}'
`, argsFile)
	if err := os.WriteFile(fakeClaude, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

	workDir := t.TempDir()

	// Stage with MaxTurns=0 (unlimited) — comment default should be 50 (safety cap)
	stage := &stages.Stage{
		Name:       "Research",
		Prompt:     "research",
		MaxTurns:   0,
		Completion: stages.CompletionCriteria{Type: "claude"},
	}
	issue := gh.ProjectItem{Number: 202, Title: "T"}
	comments := []gh.Comment{
		{Author: "user", Body: "Add more analysis", CreatedAt: time.Now()},
	}

	_, _, usage, err := InvokeClaudeForComments(context.Background(), stage, issue, comments, workDir, "")
	if err != nil {
		t.Fatalf("InvokeClaudeForComments: %v", err)
	}

	args, _ := os.ReadFile(argsFile)
	argsStr := string(args)

	if !strings.Contains(argsStr, "--max-turns") {
		t.Errorf("expected --max-turns in args, got: %q", argsStr)
	}
	if !strings.Contains(argsStr, "50") {
		t.Errorf("expected default limit (50) in args: %q", argsStr)
	}

	if usage.MaxTurns != 50 {
		t.Errorf("usage.MaxTurns = %d, want 50", usage.MaxTurns)
	}
}
