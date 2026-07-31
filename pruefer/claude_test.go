package pruefer

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestBuildReviewArgs_ReadOnlyAllowlist(t *testing.T) {
	args := buildReviewArgs(ReviewRequest{Model: "sonnet"})

	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--permission-mode dontAsk") {
		t.Errorf("args = %v, want --permission-mode dontAsk", args)
	}
	if strings.Contains(joined, "--dangerously-skip-permissions") {
		t.Errorf("args = %v, must never contain --dangerously-skip-permissions (Pruefer has no unrestricted opt-out)", args)
	}
	if strings.Contains(joined, "Bash(gh:*)") {
		t.Error("allowlist must not contain Bash(gh:*) — gh is not read-only (gh pr review --approve, gh pr merge)")
	}
	if strings.Contains(joined, "Edit") || strings.Contains(joined, "Write") {
		t.Error("allowlist must not contain Edit or Write — the reviewer must never mutate the working tree")
	}
	for _, want := range []string{"Read", "Grep", "Glob", "Bash(git diff:*)", "Bash(git log:*)", "Bash(git show:*)", "Bash(git blame:*)", "Bash(git grep:*)", "Bash(git status:*)"} {
		found := false
		for i, a := range args {
			if a == "--allowedTools" && i+1 < len(args) && args[i+1] == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected allowedTools to include %q, args = %v", want, args)
		}
	}
	if !slices.Contains(args, "--model") {
		t.Errorf("expected --model to be passed when Model is set, args = %v", args)
	}
}

func TestBuildReviewArgs_UserSettingsOnly(t *testing.T) {
	args := buildReviewArgs(ReviewRequest{})

	found := false
	for i, a := range args {
		if a == "--setting-sources" && i+1 < len(args) && args[i+1] == "user" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected --setting-sources user, args = %v", args)
	}
}

func TestBuildReviewArgs_NoModelWhenUnset(t *testing.T) {
	args := buildReviewArgs(ReviewRequest{})
	if slices.Contains(args, "--model") {
		t.Errorf("expected no --model flag when Model is empty, args = %v", args)
	}
}

func TestBuildReviewEnv_DefaultsEffort(t *testing.T) {
	env := buildReviewEnv(ReviewRequest{})
	joined := strings.Join(env, " ")
	if !strings.Contains(joined, "CLAUDE_CODE_EFFORT_LEVEL="+DefaultEffort) {
		t.Errorf("env = %v, want default effort %q", env, DefaultEffort)
	}
	if !strings.Contains(joined, "CLAUDE_CODE_DISABLE_ADAPTIVE_THINKING=1") {
		t.Errorf("env = %v, want adaptive thinking disabled", env)
	}
}

func TestBuildReviewEnv_HonorsExplicitEffort(t *testing.T) {
	env := buildReviewEnv(ReviewRequest{Effort: "max"})
	if !slices.Contains(env, "CLAUDE_CODE_EFFORT_LEVEL=max") {
		t.Errorf("env = %v, want CLAUDE_CODE_EFFORT_LEVEL=max", env)
	}
}

func TestMergeEnv_OverridesTakePrecedence(t *testing.T) {
	base := []string{"FOO=old", "BAR=keep"}
	overrides := []string{"FOO=new"}
	got := mergeEnv(base, overrides)
	want := map[string]string{"FOO": "new", "BAR": "keep"}
	seen := map[string]string{}
	for _, kv := range got {
		parts := strings.SplitN(kv, "=", 2)
		seen[parts[0]] = parts[1]
	}
	for k, v := range want {
		if seen[k] != v {
			t.Errorf("seen[%q] = %q, want %q (full env: %v)", k, seen[k], v, got)
		}
	}
}

func TestParseClaudeReviewJSON_SingleObject(t *testing.T) {
	resp, ok := parseClaudeReviewJSON([]byte(`{"result":"looks good, minor nit on line 4","is_error":false}`))
	if !ok {
		t.Fatal("expected ok=true")
	}
	if resp.Result != "looks good, minor nit on line 4" {
		t.Errorf("Result = %q", resp.Result)
	}
	if resp.IsError {
		t.Error("IsError = true, want false")
	}
}

func TestParseClaudeReviewJSON_ConversationArray(t *testing.T) {
	stream := `{"type":"system","subtype":"init"}
{"type":"assistant","message":{"content":[{"type":"text","text":"thinking..."}]}}
{"type":"result","result":"final review text","is_error":false,"num_turns":4,"total_cost_usd":0.0567}`
	resp, ok := parseClaudeReviewJSON([]byte(stream))
	if !ok {
		t.Fatal("expected ok=true")
	}
	if resp.Result != "final review text" {
		t.Errorf("Result = %q", resp.Result)
	}
	if resp.NumTurns != 4 {
		t.Errorf("NumTurns = %d, want 4 (NDJSON envelope path must carry turns through)", resp.NumTurns)
	}
	if resp.CostUSD != 0.0567 {
		t.Errorf("CostUSD = %v, want 0.0567 (NDJSON envelope path must carry cost through)", resp.CostUSD)
	}
}

func TestParseClaudeReviewJSON_Unparseable(t *testing.T) {
	_, ok := parseClaudeReviewJSON([]byte("not json at all"))
	if ok {
		t.Error("expected ok=false for unparseable output")
	}
}

// writeFakeClaude installs a fake "claude" binary on a temp bin dir added to
// PATH, mirroring engine/grandchild_test.go's pattern. The script reads and
// discards stdin (the prompt), then emits the given stdout.
func writeFakeClaude(t *testing.T, script string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake claude script uses #!/bin/sh")
	}
	binDir := t.TempDir()
	fakeClaude := filepath.Join(binDir, "claude")
	if err := os.WriteFile(fakeClaude, []byte("#!/bin/sh\ncat >/dev/null\n"+script), 0755); err != nil {
		t.Fatalf("writing fake claude script: %v", err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
}

func TestRealClaudeInvoker_Review_Success(t *testing.T) {
	writeFakeClaude(t, `printf '%s\n' '{"type":"result","result":"Found one bug: nil check missing on line 12.","is_error":false,"num_turns":7,"total_cost_usd":0.1234}'`+"\n")

	r := &RealClaudeInvoker{}
	workDir := t.TempDir()
	result, err := r.Review(context.Background(), ReviewRequest{
		Owner: "handarbeit", Repo: "fabrik", PRNumber: 1, Title: "Fix bug",
		HeadSHA: "abc123", WorkDir: workDir,
	})
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if result.Text != "Found one bug: nil check missing on line 12." {
		t.Errorf("Text = %q", result.Text)
	}
	if result.NumTurns != 7 {
		t.Errorf("NumTurns = %d, want 7", result.NumTurns)
	}
	if result.CostUSD != 0.1234 {
		t.Errorf("CostUSD = %v, want 0.1234", result.CostUSD)
	}
}

func TestRealClaudeInvoker_Review_ProcessError(t *testing.T) {
	writeFakeClaude(t, "exit 1\n")

	r := &RealClaudeInvoker{}
	_, err := r.Review(context.Background(), ReviewRequest{PRNumber: 1, WorkDir: t.TempDir()})
	if err == nil {
		t.Fatal("expected an error when claude exits non-zero")
	}
}

func TestRealClaudeInvoker_Review_IsErrorResponse(t *testing.T) {
	writeFakeClaude(t, `printf '%s\n' '{"type":"result","result":"something went wrong internally","is_error":true}'`+"\n")

	r := &RealClaudeInvoker{}
	_, err := r.Review(context.Background(), ReviewRequest{PRNumber: 1, WorkDir: t.TempDir()})
	if err == nil {
		t.Fatal("expected an error when claude reports is_error=true")
	}
}

func TestRealClaudeInvoker_Review_UnparseableOutput(t *testing.T) {
	writeFakeClaude(t, `printf 'not json\n'`+"\n")

	r := &RealClaudeInvoker{}
	_, err := r.Review(context.Background(), ReviewRequest{PRNumber: 1, WorkDir: t.TempDir()})
	if err == nil {
		t.Fatal("expected an error when claude's output cannot be parsed")
	}
}

func TestRealClaudeInvoker_Review_InactivityTimeout(t *testing.T) {
	orig := reviewInactivityTimeout
	reviewInactivityTimeout = 100 * time.Millisecond
	defer func() { reviewInactivityTimeout = orig }()

	// The fake claude script hangs (sleeps) without producing any stdout —
	// the inactivity watchdog must kill it well before the test timeout.
	writeFakeClaude(t, "sleep 30\n")

	r := &RealClaudeInvoker{}
	done := make(chan error, 1)
	go func() {
		_, err := r.Review(context.Background(), ReviewRequest{PRNumber: 1, WorkDir: t.TempDir()})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("expected an error when claude is killed for inactivity")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Review did not return within 10s — inactivity watchdog did not fire")
	}
}

func TestRealClaudeInvoker_Review_ContextCancelKillsProcess(t *testing.T) {
	origGrace := reviewKillGrace
	reviewKillGrace = 200 * time.Millisecond
	defer func() { reviewKillGrace = origGrace }()

	writeFakeClaude(t, "trap '' INT TERM\nsleep 30\n") // ignores SIGINT/SIGTERM so the test also exercises the SIGKILL escalation step

	r := &RealClaudeInvoker{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := r.Review(ctx, ReviewRequest{PRNumber: 1, WorkDir: t.TempDir()})
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Error("expected an error when the context is cancelled mid-invocation")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Review did not return within 10s of context cancellation")
	}
}

func TestBuildReviewPrompt_NoExcludedPaths_NoOutOfScopeSection(t *testing.T) {
	prompt := buildReviewPrompt(ReviewRequest{Owner: "o", Repo: "r", PRNumber: 1})
	if strings.Contains(prompt, "Out of scope for this review") {
		t.Errorf("prompt contains an out-of-scope section with no ExcludedPaths set:\n%s", prompt)
	}
}

func TestBuildReviewPrompt_ExcludedPaths_AddsOutOfScopeSection(t *testing.T) {
	prompt := buildReviewPrompt(ReviewRequest{
		Owner: "o", Repo: "r", PRNumber: 1, BaseBranch: "main",
		ExcludedPaths: []string{"packages/evals/corpus/quality-bench-corpus.jsonl"},
	})
	if !strings.Contains(prompt, "Out of scope for this review") {
		t.Fatalf("prompt missing out-of-scope section:\n%s", prompt)
	}
	if !strings.Contains(prompt, "packages/evals/corpus/quality-bench-corpus.jsonl") {
		t.Errorf("prompt does not name the excluded path:\n%s", prompt)
	}
	if !strings.Contains(prompt, ":(exclude)packages/evals/corpus/quality-bench-corpus.jsonl") {
		t.Errorf("prompt does not instruct excluding the path via a git pathspec:\n%s", prompt)
	}
	if !strings.Contains(prompt, "main...HEAD") {
		t.Errorf("prompt's pathspec example must still reference the base branch comparison:\n%s", prompt)
	}
}

func TestBuildReviewPrompt_ExcludedPathWithSingleQuote_EscapedNotBrokenOut(t *testing.T) {
	prompt := buildReviewPrompt(ReviewRequest{
		Owner: "o", Repo: "r", PRNumber: 1, BaseBranch: "main",
		ExcludedPaths: []string{"it's/evil.jsonl"},
	})
	// The malicious path must appear only inside a properly escaped
	// single-quoted shell token — never a bare, unescaped single quote that
	// could close the pathspec's quoting and let the rest of the path (or a
	// path crafted to look like additional shell/git arguments) be
	// interpreted outside the quoted string.
	if !strings.Contains(prompt, `:(exclude)it'\''s/evil.jsonl`) {
		t.Errorf("prompt does not safely escape the embedded single quote:\n%s", prompt)
	}
	if strings.Contains(prompt, `:(exclude)it's/evil.jsonl'`) {
		t.Errorf("prompt contains an unescaped single quote that would break out of the pathspec quoting:\n%s", prompt)
	}
}

func TestMockClaudeInvoker_RecordsCalls(t *testing.T) {
	m := &mockClaudeInvoker{}
	req := ReviewRequest{PRNumber: 7}
	result, err := m.Review(context.Background(), req)
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if result.Text != "mock review" {
		t.Errorf("Text = %q, want default mock review text", result.Text)
	}
	if m.callCount() != 1 {
		t.Errorf("callCount = %d, want 1", m.callCount())
	}
	if got := m.callsSnapshot()[0].PRNumber; got != 7 {
		t.Errorf("recorded PRNumber = %d, want 7", got)
	}
}
