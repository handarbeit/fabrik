package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gh "github.com/verveguy/fabrik/github"
	"github.com/verveguy/fabrik/stages"
)

func TestBuildPrompt_Basic(t *testing.T) {
	stage := &stages.Stage{
		Name:   "Research",
		Prompt: "You are a research agent.",
	}
	issue := gh.ProjectItem{
		Number: 42,
		Title:  "Fix the bug",
		URL:    "https://github.com/owner/repo/issues/42",
		Body:   "It is broken",
	}

	prompt := buildPrompt(stage, issue, nil)

	if !strings.Contains(prompt, "You are a research agent.") {
		t.Error("prompt missing stage prompt")
	}
	if !strings.Contains(prompt, "# Issue #42: Fix the bug") {
		t.Error("prompt missing issue header")
	}
	if !strings.Contains(prompt, "https://github.com/owner/repo/issues/42") {
		t.Error("prompt missing URL")
	}
	if !strings.Contains(prompt, "It is broken") {
		t.Error("prompt missing body")
	}
	if !strings.Contains(prompt, "FABRIK_STAGE_COMPLETE") {
		t.Error("prompt missing completion instruction")
	}
}

func TestBuildPrompt_WithLabels(t *testing.T) {
	stage := &stages.Stage{Name: "Test", Prompt: "prompt"}
	issue := gh.ProjectItem{
		Number: 1,
		Title:  "T",
		Labels: []string{"bug", "priority"},
	}

	prompt := buildPrompt(stage, issue, nil)
	if !strings.Contains(prompt, "## Labels") {
		t.Error("prompt missing labels section")
	}
	if !strings.Contains(prompt, "bug, priority") {
		t.Error("prompt missing label values")
	}
}

func TestBuildPrompt_WithComments(t *testing.T) {
	stage := &stages.Stage{Name: "Test", Prompt: "prompt"}
	issue := gh.ProjectItem{Number: 1, Title: "T"}
	comments := []gh.Comment{
		{
			Author:    "alice",
			Body:      "Please fix this",
			CreatedAt: time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
		},
	}

	prompt := buildPrompt(stage, issue, comments)
	if !strings.Contains(prompt, "## New Comments") {
		t.Error("prompt missing comments section")
	}
	if !strings.Contains(prompt, "@alice") {
		t.Error("prompt missing comment author")
	}
	if !strings.Contains(prompt, "Please fix this") {
		t.Error("prompt missing comment body")
	}
}

func TestBuildPrompt_NoLabelsSection(t *testing.T) {
	stage := &stages.Stage{Name: "Test", Prompt: "prompt"}
	issue := gh.ProjectItem{Number: 1, Title: "T"}

	prompt := buildPrompt(stage, issue, nil)
	if strings.Contains(prompt, "## Labels") {
		t.Error("prompt should not have labels section when no labels")
	}
}

func TestBuildPrompt_NoCommentsSection(t *testing.T) {
	stage := &stages.Stage{Name: "Test", Prompt: "prompt"}
	issue := gh.ProjectItem{Number: 1, Title: "T"}

	prompt := buildPrompt(stage, issue, nil)
	if strings.Contains(prompt, "## New Comments") {
		t.Error("prompt should not have comments section when no comments")
	}
}

func TestCheckCompletion_Claude(t *testing.T) {
	stage := &stages.Stage{
		Completion: stages.CompletionCriteria{Type: "claude"},
	}

	if !checkCompletion(stage, "Some output\nFABRIK_STAGE_COMPLETE\n") {
		t.Error("expected completion when marker present on its own line")
	}
	if !checkCompletion(stage, "output\nFABRIK_STAGE_COMPLETE") {
		t.Error("expected completion when marker is last line with no trailing newline")
	}
	if checkCompletion(stage, "Some output without marker") {
		t.Error("expected no completion without marker")
	}
	if checkCompletion(stage, "Please output FABRIK_STAGE_COMPLETE when done") {
		t.Error("expected no completion when marker is embedded in a sentence")
	}
	if checkCompletion(stage, "`FABRIK_STAGE_COMPLETE`") {
		t.Error("expected no completion when marker is inside backticks")
	}
	if !checkCompletion(stage, "Some output\r\nFABRIK_STAGE_COMPLETE\r\n") {
		t.Error("expected completion when marker is on its own CRLF line")
	}
	if !checkCompletion(stage, "FABRIK_STAGE_COMPLETE\nmore output after") {
		t.Error("expected completion when marker appears on its own line but not as final line")
	}
}

func TestCheckCompletion_DefaultEmpty(t *testing.T) {
	// Empty type behaves like "claude"
	stage := &stages.Stage{
		Completion: stages.CompletionCriteria{Type: ""},
	}
	if !checkCompletion(stage, "prefix\nFABRIK_STAGE_COMPLETE\nsuffix") {
		t.Error("expected completion for empty type when marker present")
	}
}

func TestCheckCompletion_ExactLineOnly(t *testing.T) {
	// Marker embedded in a sentence must not trigger completion
	stage := &stages.Stage{
		Completion: stages.CompletionCriteria{Type: "claude"},
	}
	if checkCompletion(stage, "You said FABRIK_STAGE_COMPLETE in a sentence") {
		t.Error("marker inside a sentence should not complete (exact-line required)")
	}
}

func TestCheckCompletion_UnsupportedTypes(t *testing.T) {
	for _, typ := range []string{"tasklist", "label", "approval", "unknown"} {
		stage := &stages.Stage{
			Completion: stages.CompletionCriteria{Type: typ},
		}
		if checkCompletion(stage, "FABRIK_STAGE_COMPLETE") {
			t.Errorf("type %q should not complete", typ)
		}
	}
}

func TestParseClaudeJSON_ValidJSON(t *testing.T) {
	output := []byte(`{"result":"hello world","session_id":"sess_abc123","num_turns":5,"total_cost_usd":0.0042,"usage":{"input_tokens":100,"output_tokens":50,"cache_creation_input_tokens":10,"cache_read_input_tokens":5}}`)

	resp, ok := parseClaudeJSON(output)
	if !ok {
		t.Fatal("expected successful parse")
	}
	if resp.Result != "hello world" {
		t.Errorf("Result = %q, want %q", resp.Result, "hello world")
	}
	if resp.SessionID != "sess_abc123" {
		t.Errorf("SessionID = %q, want %q", resp.SessionID, "sess_abc123")
	}
	if resp.NumTurns != 5 {
		t.Errorf("NumTurns = %d, want 5", resp.NumTurns)
	}

	usage := tokenUsageFromResponse(resp)
	if usage.InputTokens != 100 {
		t.Errorf("InputTokens = %d, want 100", usage.InputTokens)
	}
	if usage.OutputTokens != 50 {
		t.Errorf("OutputTokens = %d, want 50", usage.OutputTokens)
	}
	if usage.CacheCreationTokens != 10 {
		t.Errorf("CacheCreationTokens = %d, want 10", usage.CacheCreationTokens)
	}
	if usage.CacheReadTokens != 5 {
		t.Errorf("CacheReadTokens = %d, want 5", usage.CacheReadTokens)
	}
	if usage.CostUSD != 0.0042 {
		t.Errorf("CostUSD = %f, want 0.0042", usage.CostUSD)
	}
}

func TestParseClaudeJSON_InvalidJSON(t *testing.T) {
	_, ok := parseClaudeJSON([]byte(`not json at all`))
	if ok {
		t.Error("expected parse failure for invalid JSON")
	}
}

func TestParseClaudeJSON_EmptyResult(t *testing.T) {
	_, ok := parseClaudeJSON([]byte(`{"result":"","session_id":"sess_1"}`))
	if ok {
		t.Error("expected parse failure for empty result")
	}
}

func TestSaveSessionIDDirect(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.session")

	saveSessionIDDirect(path, "sess_abc123")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading session file: %v", err)
	}
	if string(data) != "sess_abc123" {
		t.Errorf("session ID = %q, want sess_abc123", string(data))
	}
}

func TestSaveSessionIDDirect_Empty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.session")

	saveSessionIDDirect(path, "")

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("session file should not exist for empty session ID")
	}
}

func TestSessionDir(t *testing.T) {
	dir := SessionDir(42)
	if !strings.Contains(dir, "issue-42") {
		t.Errorf("SessionDir(42) = %q, expected to contain issue-42", dir)
	}
	if !strings.Contains(dir, ".fabrik/sessions") {
		t.Errorf("SessionDir(42) = %q, expected to contain .fabrik/sessions", dir)
	}
}

func TestSessionFile(t *testing.T) {
	path := sessionFile(42, "Research")
	if !strings.HasSuffix(path, "Research.session") {
		t.Errorf("sessionFile = %q, expected to end with Research.session", path)
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
echo "real invoker output"
`
	os.WriteFile(fakeClaude, []byte(script), 0755)

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
}

func TestInvokeClaude_FakeBinary(t *testing.T) {
	// Create a fake claude binary that echoes its arguments
	binDir := t.TempDir()
	fakeClaude := filepath.Join(binDir, "claude")
	script := `#!/bin/sh
cat >/dev/null
printf '%s\n' '{"result":"Claude output for test\nFABRIK_STAGE_COMPLETE\n","session_id":"sess_test123","num_turns":1,"total_cost_usd":0.001,"is_error":false}'
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

	// Check session file was saved
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
}

func TestInvokeClaude_WithResume(t *testing.T) {
	binDir := t.TempDir()
	fakeClaude := filepath.Join(binDir, "claude")
	// Script that checks for --resume flag
	script := `#!/bin/sh
cat >/dev/null
for arg in "$@"; do
	if [ "$arg" = "--resume" ]; then
		echo "RESUMED"
		exit 0
	fi
done
echo "NO RESUME"
`
	os.WriteFile(fakeClaude, []byte(script), 0755)

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

	output, _, _, err := InvokeClaude(context.Background(), stage, issue, nil,true, workDir, "")
	if err != nil {
		t.Fatalf("InvokeClaude: %v", err)
	}
	if !strings.Contains(output, "RESUMED") {
		t.Errorf("expected resume, got: %q", output)
	}

	if info, err := os.Stat(sessDir); err != nil {
		t.Fatalf("stat session dir: %v", err)
	} else if perm := info.Mode().Perm(); perm != 0700 {
		t.Errorf("session dir mode = %04o, want 0700", perm)
	}
}

func TestInvokeClaude_WithModelAndTools(t *testing.T) {
	binDir := t.TempDir()
	fakeClaude := filepath.Join(binDir, "claude")
	script := `#!/bin/sh
cat >/dev/null
echo "args: $@"
`
	os.WriteFile(fakeClaude, []byte(script), 0755)

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

	output, _, _, err := InvokeClaude(context.Background(), stage, issue, nil,false, workDir, "")
	if err != nil {
		t.Fatalf("InvokeClaude: %v", err)
	}
	// Check args include model and tools
	if !strings.Contains(output, "--model") {
		t.Error("expected --model in args")
	}
	if !strings.Contains(output, "opus") {
		t.Error("expected opus in args")
	}
	if !strings.Contains(output, "--max-turns") {
		t.Error("expected --max-turns in args")
	}
	if !strings.Contains(output, "--allowedTools") {
		t.Error("expected --allowedTools in args")
	}
	os.RemoveAll(SessionDir(50))
}

func TestInvokeClaude_WithModelOverride(t *testing.T) {
	binDir := t.TempDir()
	fakeClaude := filepath.Join(binDir, "claude")
	script := `#!/bin/sh
cat >/dev/null
echo "args: $@"
`
	os.WriteFile(fakeClaude, []byte(script), 0755)

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

	output, _, _, err := InvokeClaude(context.Background(), stage, issue, nil,false, workDir, "opus")
	if err != nil {
		t.Fatalf("InvokeClaude: %v", err)
	}
	// Override should take precedence
	if !strings.Contains(output, "opus") {
		t.Error("expected model override 'opus' in args")
	}
	os.RemoveAll(SessionDir(51))
}

func TestInvokeClaude_BinaryError(t *testing.T) {
	binDir := t.TempDir()
	fakeClaude := filepath.Join(binDir, "claude")
	script := `#!/bin/sh
cat >/dev/null
echo "partial output"
exit 1
`
	os.WriteFile(fakeClaude, []byte(script), 0755)

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

	output, _, _, err := InvokeClaude(context.Background(), stage, issue, nil,false, workDir, "")
	if err == nil {
		t.Fatal("expected error for failing binary")
	}
	if !strings.Contains(output, "partial output") {
		t.Errorf("expected partial output, got: %q", output)
	}
	os.RemoveAll(SessionDir(60))
}

func TestInvokeClaude_WithComments(t *testing.T) {
	binDir := t.TempDir()
	fakeClaude := filepath.Join(binDir, "claude")
	script := `#!/bin/sh
# Read prompt from stdin to verify comments are included
cat | grep -o "New Comments" && echo "HAS_COMMENTS" || echo "NO_COMMENTS"
`
	os.WriteFile(fakeClaude, []byte(script), 0755)

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

	output, _, _, err := InvokeClaude(context.Background(), stage, issue, comments, false, workDir, "")
	if err != nil {
		t.Fatalf("InvokeClaude: %v", err)
	}
	if !strings.Contains(output, "HAS_COMMENTS") {
		t.Errorf("expected comments in prompt, output: %q", output)
	}
	os.RemoveAll(SessionDir(70))
}

func TestBuildCommentReviewPrompt_Issue(t *testing.T) {
	stage := &stages.Stage{Name: "Research"}
	item := gh.ProjectItem{
		Number: 42,
		Title:  "Test Issue",
		URL:    "https://github.com/org/repo/issues/42",
		Body:   "Issue body content",
	}
	comments := []gh.Comment{
		{Author: "alice", Body: "looks good", CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
	}

	prompt := buildCommentReviewPrompt(stage, item, comments)

	if !strings.Contains(prompt, "# Issue #42: Test Issue") {
		t.Error("expected issue header in prompt")
	}
	if !strings.Contains(prompt, "## Current Issue Body") {
		t.Error("expected 'Current Issue Body' section")
	}
	if !strings.Contains(prompt, "updated issue body") {
		t.Error("expected issue-specific marker instructions")
	}
	if strings.Contains(prompt, "PR") {
		t.Error("should not contain PR terminology for issues")
	}
}

func TestBuildCommentReviewPrompt_PR(t *testing.T) {
	stage := &stages.Stage{Name: "Review"}
	item := gh.ProjectItem{
		Number: 7,
		Title:  "Fix bug",
		URL:    "https://github.com/org/repo/pull/7",
		Body:   "PR description",
		IsPR:   true,
	}
	comments := []gh.Comment{
		{Author: "bot", Body: "suggestion: use const", CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
	}

	prompt := buildCommentReviewPrompt(stage, item, comments)

	if !strings.Contains(prompt, "# PR #7: Fix bug") {
		t.Error("expected PR header in prompt")
	}
	if !strings.Contains(prompt, "## Current PR Description") {
		t.Error("expected 'Current PR Description' section")
	}
	if !strings.Contains(prompt, "updated PR description") {
		t.Error("expected PR-specific marker instructions")
	}
	if !strings.Contains(prompt, "FABRIK_ISSUE_UPDATE_BEGIN") {
		t.Error("expected consistent FABRIK_ISSUE_UPDATE markers for PRs")
	}
}

func TestBuildCommentReviewPrompt_CustomCommentPrompt(t *testing.T) {
	stage := &stages.Stage{Name: "Review", CommentPrompt: "Custom prompt text"}
	item := gh.ProjectItem{
		Number: 7,
		Title:  "Fix bug",
		IsPR:   true,
	}
	comments := []gh.Comment{
		{Author: "user", Body: "hello", CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
	}

	prompt := buildCommentReviewPrompt(stage, item, comments)

	if !strings.Contains(prompt, "Custom prompt text") {
		t.Error("expected custom comment prompt to be used")
	}
	if strings.Contains(prompt, "PR comment review agent") {
		t.Error("should use custom prompt, not default PR prompt")
	}
}

func TestDefaultPRCommentPrompt(t *testing.T) {
	prompt := defaultPRCommentPrompt()

	if !strings.Contains(prompt, "PR comment review agent") {
		t.Error("expected PR-specific agent description")
	}
	if !strings.Contains(prompt, "code changes") {
		t.Error("expected mention of code changes")
	}
	if !strings.Contains(prompt, "review feedback") {
		t.Error("expected mention of review feedback")
	}
}

func TestExtractUpdatedBody(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		expect string
	}{
		{
			name:   "normal extraction",
			input:  "Some preamble\nFABRIK_ISSUE_UPDATE_BEGIN\nUpdated body here\nFABRIK_ISSUE_UPDATE_END\nSome epilogue",
			expect: "Updated body here",
		},
		{
			name:   "no markers",
			input:  "Just some output without markers",
			expect: "",
		},
		{
			name:   "only begin marker",
			input:  "FABRIK_ISSUE_UPDATE_BEGIN\nBody without end",
			expect: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractUpdatedBody(tt.input)
			if got != tt.expect {
				t.Errorf("extractUpdatedBody() = %q, want %q", got, tt.expect)
			}
		})
	}
}
