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
