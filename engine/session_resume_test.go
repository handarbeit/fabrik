package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

func sessionTestEngine(t *testing.T) (*Engine, *[]string) {
	t.Helper()
	e := &Engine{}
	e.SetSessionDirForTest(t.TempDir())
	var mu sync.Mutex
	var lines []string
	e.SetLogSinkForTest(func(_ int, tag, msg string) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, tag+"|"+strings.TrimSpace(msg))
	})
	return e, &lines
}

func sessionTestStage(mode string, threshold int) *stages.Stage {
	return &stages.Stage{Name: "Implement", ResumeSession: mode, ResumeThresholdTokens: threshold}
}

var sessionTestItem = gh.ProjectItem{Number: 7}

// The decision for each resume_session value against a size below, above and
// unknown (R3/R5), on a re-entry (closed session) and on a comment review.
func TestDecideSessionResume_OptionTable(t *testing.T) {
	cases := []struct {
		name   string
		mode   string
		tokens int // <0 unknown
		resume bool
		reason string
	}{
		{"default resume", "", 900000, true, "resuming session S1 (resume_session: resume)"},
		{"resume", "resume", 900000, true, "resuming session S1 (resume_session: resume)"},
		{"fresh", "fresh", 1000, false, "fresh session (resume_session: fresh)"},
		{"auto below", "auto", 100000, true, "resuming session S1 (100k tokens, threshold 250k)"},
		{"auto at threshold", "auto", 250000, true, "resuming session S1 (250k tokens, threshold 250k)"},
		{"auto above", "auto", 450000, false, "fresh session (saved session 450k > threshold 250k)"},
		{"auto unknown", "auto", -1, true, "resuming session S1 (size unknown)"},
	}
	for _, kind := range []sessionInvocation{sessionStageDispatch, sessionCommentReview} {
		for _, c := range cases {
			t.Run(fmt.Sprintf("%d/%s", kind, c.name), func(t *testing.T) {
				e, _ := sessionTestEngine(t)
				e.RecordSessionForTest(sessionTestItem, "Implement", "S1", c.tokens, true)
				d := e.decideSessionResume(sessionTestItem, sessionTestStage(c.mode, 0), kind, true)
				if d.Resume != c.resume || d.Reason != c.reason {
					t.Fatalf("got (%v, %q), want (%v, %q)", d.Resume, d.Reason, c.resume, c.reason)
				}
			})
		}
	}
}

func TestDecideSessionResume_CustomThreshold(t *testing.T) {
	e, _ := sessionTestEngine(t)
	e.RecordSessionForTest(sessionTestItem, "Implement", "S1", 60000, true)
	d := e.decideSessionResume(sessionTestItem, sessionTestStage("auto", 50000), sessionCommentReview, false)
	if d.Resume || d.Reason != "fresh session (saved session 60k > threshold 50k)" {
		t.Fatalf("got (%v, %q)", d.Resume, d.Reason)
	}
}

// R2: a session the last stage run left open is a continuation and resumes
// whatever the option says — including when the in-memory LastAttemptAt is
// gone (a restart).
func TestDecideSessionResume_ContinuationAlwaysResumes(t *testing.T) {
	for _, mode := range []string{"", "resume", "fresh", "auto"} {
		for _, legacy := range []bool{true, false} { // false = restarted daemon, empty store
			t.Run(fmt.Sprintf("%q/legacy=%v", mode, legacy), func(t *testing.T) {
				e, _ := sessionTestEngine(t)
				e.RecordSessionForTest(sessionTestItem, "Implement", "S1", 900000, false)
				d := e.decideSessionResume(sessionTestItem, sessionTestStage(mode, 0), sessionStageDispatch, legacy)
				if !d.Resume || d.Reason != "resuming session S1 (continuation)" {
					t.Fatalf("got (%v, %q)", d.Resume, d.Reason)
				}
			})
		}
	}
}

// Non-vacuity (R2): without the exemption a continuation under fresh starts fresh.
func TestDecideSessionResume_ContinuationExemptionBites(t *testing.T) {
	e, _ := sessionTestEngine(t)
	e.SetContinuationExemptionDisabledForTest(true)
	e.RecordSessionForTest(sessionTestItem, "Implement", "S1", 10, false)
	if d := e.decideSessionResume(sessionTestItem, sessionTestStage("fresh", 0), sessionStageDispatch, true); d.Resume {
		t.Fatalf("continuation resumed with the exemption removed: %+v", d)
	}
}

// Non-vacuity: forcing the governed decision to resume makes fresh/auto tests fail.
func TestDecideSessionResume_ForcedResumeBites(t *testing.T) {
	e, _ := sessionTestEngine(t)
	e.SetSessionDecisionForcedResumeForTest(true)
	e.RecordSessionForTest(sessionTestItem, "Implement", "S1", 900000, true)
	for _, mode := range []string{"fresh", "auto"} {
		if d := e.decideSessionResume(sessionTestItem, sessionTestStage(mode, 0), sessionCommentReview, false); !d.Resume {
			t.Fatalf("%s: forced seam did not force a resume", mode)
		}
	}
}

// A comment review is governed even for a session the last stage run left open.
func TestDecideSessionResume_CommentReviewIgnoresContinuation(t *testing.T) {
	e, _ := sessionTestEngine(t)
	e.RecordSessionForTest(sessionTestItem, "Implement", "S1", 10, false)
	if d := e.decideSessionResume(sessionTestItem, sessionTestStage("fresh", 0), sessionCommentReview, true); d.Resume {
		t.Fatalf("comment review resumed under fresh: %+v", d)
	}
}

func TestDecideSessionResume_NoSavedSession(t *testing.T) {
	e, _ := sessionTestEngine(t)
	if d := e.decideSessionResume(sessionTestItem, sessionTestStage("fresh", 0), sessionStageDispatch, false); d.Resume || d.Reason != "" {
		t.Fatalf("stage, first run: got %+v", d)
	}
	// Today's "resume requested but none exists" warning is kept: the legacy bool passes through.
	if d := e.decideSessionResume(sessionTestItem, sessionTestStage("fresh", 0), sessionStageDispatch, true); !d.Resume || d.Reason != "" {
		t.Fatalf("stage, lastAttempt set: got %+v", d)
	}
	if d := e.decideSessionResume(sessionTestItem, sessionTestStage("fresh", 0), sessionCommentReview, false); !d.Resume || d.Reason != "" {
		t.Fatalf("comment review without a session keeps NoResume=false: got %+v", d)
	}
}

// A session that predates ADR 2300 has no meta: continuation iff the stage
// has run before in this uptime, otherwise fresh (today's behaviour).
func TestDecideSessionResume_LegacySessionWithoutMeta(t *testing.T) {
	e, _ := sessionTestEngine(t)
	path := e.SessionFileForTest(sessionTestItem, "Implement")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("S1"), 0600); err != nil {
		t.Fatal(err)
	}
	if d := e.decideSessionResume(sessionTestItem, sessionTestStage("fresh", 0), sessionStageDispatch, true); !d.Resume || d.Reason != "resuming session S1 (continuation)" {
		t.Fatalf("legacy with lastAttempt: got %+v", d)
	}
	if d := e.decideSessionResume(sessionTestItem, sessionTestStage("resume", 0), sessionStageDispatch, false); d.Resume {
		t.Fatalf("legacy without lastAttempt resumed: %+v", d)
	}
	// A comment review on a legacy session: size unknown under auto → resume.
	if d := e.decideSessionResume(sessionTestItem, sessionTestStage("auto", 0), sessionCommentReview, false); !d.Resume || d.Reason != "resuming session S1 (size unknown)" {
		t.Fatalf("legacy auto comment review: got %+v", d)
	}
}

// A comment-review pass over a pre-ADR-2300 session creates a meta record with
// a size but no stage-dispatch outcome. That record must not read as "last run
// not closed" (a continuation): the legacy rule still applies until a stage
// dispatch records its outcome.
func TestDecideSessionResume_CommentCreatedMetaKeepsLegacyRule(t *testing.T) {
	e, _ := sessionTestEngine(t)
	path := e.SessionFileForTest(sessionTestItem, "Implement")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("S1"), 0600); err != nil {
		t.Fatal(err)
	}
	// What runClaude's recordSessionContextSize leaves after a comment pass.
	updateSessionMeta(7, path, func(m *sessionMeta) { m.SessionID = "S1" })

	// After a restart (no LastAttemptAt): legacy says fresh, not a continuation.
	if d := e.decideSessionResume(sessionTestItem, sessionTestStage("fresh", 0), sessionStageDispatch, false); d.Resume || strings.Contains(d.Reason, "continuation") {
		t.Fatalf("comment-created meta read as a continuation: %+v", d)
	}
	// Once a stage dispatch records its outcome, the real signal governs.
	e.markSessionClosed(sessionTestItem, "Implement", false)
	if d := e.decideSessionResume(sessionTestItem, sessionTestStage("fresh", 0), sessionStageDispatch, false); !d.Resume || !strings.Contains(d.Reason, "continuation") {
		t.Fatalf("recorded open run must be a continuation: %+v", d)
	}
	e.markSessionClosed(sessionTestItem, "Implement", true)
	if d := e.decideSessionResume(sessionTestItem, sessionTestStage("fresh", 0), sessionStageDispatch, false); d.Resume {
		t.Fatalf("recorded closed run under fresh must start fresh: %+v", d)
	}
}

// R7: an explicit reset makes the next stage dispatch fresh, once, regardless of
// option, continuation state or uptime — and leaves comment review alone.
func TestDecideSessionResume_ResetIsOneShot(t *testing.T) {
	e, lines := sessionTestEngine(t)
	e.RecordSessionForTest(sessionTestItem, "Implement", "S1", 10, false)
	e.markSessionReset(sessionTestItem, "Implement")

	if d := e.decideSessionResume(sessionTestItem, sessionTestStage("resume", 0), sessionCommentReview, false); !d.Resume {
		t.Fatalf("reset must not affect a comment review: %+v", d)
	}
	d := e.decideSessionResume(sessionTestItem, sessionTestStage("resume", 0), sessionStageDispatch, false)
	if d.Resume || d.Reason != "fresh session (stage state was reset)" {
		t.Fatalf("got %+v", d)
	}
	// Decided but not applied (an early return before the invoke): still reset.
	if d2 := e.decideSessionResume(sessionTestItem, sessionTestStage("resume", 0), sessionStageDispatch, false); d2.Resume {
		t.Fatalf("reset consumed before the invocation: %+v", d2)
	}
	e.applySessionDecision(sessionTestItem, d)
	if got := *lines; len(got) != 1 || got[0] != "session|fresh session (stage state was reset)" {
		t.Fatalf("log lines = %v", got)
	}
	// The discarded pointer is dropped, and the flag is consumed: a later session
	// created over the same meta must not start fresh again.
	sessFile := e.SessionFileForTest(sessionTestItem, "Implement")
	if _, err := os.Stat(sessFile); !os.IsNotExist(err) {
		t.Fatalf("discarded session pointer still present: %v", err)
	}
	if m, found := readSessionMeta(sessFile); !found || m.Reset {
		t.Fatalf("reset not consumed by applySessionDecision: %+v (found=%v)", m, found)
	}
}

// A fresh decision over a saved session drops the old pointer, so a fresh run
// that dies before the CLI reports a session id cannot leave the next dispatch
// "continuing" the discarded session (closed=false + pointer to S1).
func TestApplySessionDecision_FreshOverSavedDropsPointer(t *testing.T) {
	for _, kind := range []sessionInvocation{sessionStageDispatch, sessionCommentReview} {
		e, _ := sessionTestEngine(t)
		e.RecordSessionForTest(sessionTestItem, "Implement", "S1", 450000, true)
		st := sessionTestStage("fresh", 0)
		d := e.decideSessionResume(sessionTestItem, st, kind, true)
		if d.Resume {
			t.Fatalf("kind %d: expected fresh: %+v", kind, d)
		}
		e.applySessionDecision(sessionTestItem, d)
		// The fresh run died early: the early-return path marks the session open.
		e.markSessionClosed(sessionTestItem, "Implement", false)
		next := e.decideSessionResume(sessionTestItem, st, sessionStageDispatch, false)
		if next.Resume && strings.Contains(next.Reason, "S1") {
			t.Fatalf("kind %d: next dispatch resumes the discarded session: %+v", kind, next)
		}
	}

	// A resume decision leaves the pointer alone.
	e, _ := sessionTestEngine(t)
	e.RecordSessionForTest(sessionTestItem, "Implement", "S1", 100, true)
	e.applySessionDecision(sessionTestItem, e.decideSessionResume(sessionTestItem, sessionTestStage("auto", 0), sessionCommentReview, false))
	if _, err := os.Stat(e.SessionFileForTest(sessionTestItem, "Implement")); err != nil {
		t.Fatalf("resume decision removed the pointer: %v", err)
	}
}

// markSessionClosed is the writer of the continuation signal.
func TestMarkSessionClosed_FlipsContinuationToReentry(t *testing.T) {
	e, _ := sessionTestEngine(t)
	e.RecordSessionForTest(sessionTestItem, "Implement", "S1", 900000, false)
	st := sessionTestStage("fresh", 0)
	if d := e.decideSessionResume(sessionTestItem, st, sessionStageDispatch, false); !d.Resume {
		t.Fatalf("open session should continue: %+v", d)
	}
	e.markSessionClosed(sessionTestItem, "Implement", true)
	if d := e.decideSessionResume(sessionTestItem, st, sessionStageDispatch, false); d.Resume || !d.freshOverSaved() {
		t.Fatalf("closed session under fresh should be a fresh re-entry: %+v", d)
	}
	// The size survives the flag write.
	if _, ok := savedSessionContextTokens(e.SessionFileForTest(sessionTestItem, "Implement")); !ok {
		t.Fatal("markSessionClosed dropped the recorded size")
	}
}

func TestApplySessionDecision_LogsOncePerLine(t *testing.T) {
	e, lines := sessionTestEngine(t)
	e.RecordSessionForTest(sessionTestItem, "Implement", "S1", 450000, true)
	e.applySessionDecision(sessionTestItem, e.decideSessionResume(sessionTestItem, sessionTestStage("auto", 0), sessionCommentReview, false))
	e.applySessionDecision(sessionTestItem, sessionDecision{}) // silent decision logs nothing
	want := "session|fresh session (saved session 450k > threshold 250k)"
	if got := *lines; len(got) != 1 || got[0] != want {
		t.Fatalf("log lines = %v, want [%s]", got, want)
	}
}

func TestFormatTokensK(t *testing.T) {
	for in, want := range map[int]string{250000: "250k", 450000: "450k", 1500: "1.5k", 0: "0k"} {
		if got := formatTokensK(in); got != want {
			t.Errorf("formatTokensK(%d) = %q, want %q", in, got, want)
		}
	}
}

// R4: a fresh stage re-entry gets the stage's own previous output; a resumed
// one and every later stage do not.
func TestWriteContextFilesWithCurrent_IncludesOwnOutputOnly(t *testing.T) {
	impl := &stages.Stage{Name: "Implement", Order: 3}
	review := &stages.Stage{Name: "Review", Order: 4}
	plan := &stages.Stage{Name: "Plan", Order: 2}
	e := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	e.cfg.Stages = []*stages.Stage{plan, impl, review}
	item := gh.ProjectItem{Number: 7, Comments: []gh.Comment{
		{Body: "🏭 **Fabrik — stage: Plan**\nplan"},
		{Body: "🏭 **Fabrik — stage: Implement**\nimpl"},
		{Body: "🏭 **Fabrik — stage: Review**\nrev"},
	}}
	for _, include := range []bool{false, true} {
		dir := t.TempDir()
		e.writeContextFilesWithCurrent(item, impl, dir, false, include)
		has := func(n string) bool {
			_, err := os.Stat(filepath.Join(dir, ".fabrik-context", n))
			return err == nil
		}
		if !has("stage-Plan.md") || has("stage-Review.md") || has("stage-Implement.md") != include {
			t.Errorf("include=%v: Plan=%v Implement=%v Review=%v", include, has("stage-Plan.md"), has("stage-Implement.md"), has("stage-Review.md"))
		}
	}
}

// R1/acceptance: the real invokers pass --resume exactly per resume / NoResume,
// and a real run records the last-call context size next to the pointer.
func TestInvokeClaude_ResumeArgPerDecisionAndSizeRecorded(t *testing.T) {
	t.Chdir(t.TempDir())
	binDir := t.TempDir()
	argsFile := filepath.Join(binDir, "args.txt")
	script := fmt.Sprintf(`#!/bin/sh
cat >/dev/null
echo "$@" > %s
printf '%%s\n' '{"type":"assistant","session_id":"sess_new","parent_tool_use_id":null,"message":{"usage":{"input_tokens":3,"cache_creation_input_tokens":1000,"cache_read_input_tokens":40000}}}'
printf '%%s\n' '{"type":"result","result":"ok","session_id":"sess_new","num_turns":1,"total_cost_usd":0.001}'
`, argsFile)
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	stage := &stages.Stage{Name: "Implement", Prompt: "x", Completion: stages.CompletionCriteria{Type: "claude"}}
	issue := gh.ProjectItem{Number: 99, Title: "T"}
	workDir := t.TempDir()

	args := func() string { b, _ := os.ReadFile(argsFile); return string(b) }
	seed := func() {
		os.MkdirAll(SessionDir(99), 0700)
		os.WriteFile(sessionFile(99, "Implement"), []byte("sess_old"), 0600)
	}

	seed()
	if _, _, _, err := InvokeClaude(context.Background(), stage, issue, nil, true, workDir, InvokeOptions{}); err != nil || !strings.Contains(args(), "--resume sess_old") {
		t.Fatalf("InvokeClaude resume=true: err=%v args=%q", err, args())
	}
	if tok, ok := savedSessionContextTokens(sessionFile(99, "Implement")); !ok || tok != 41003 {
		t.Fatalf("recorded size = (%d,%v), want (41003,true)", tok, ok)
	}

	seed()
	if _, _, _, err := InvokeClaude(context.Background(), stage, issue, nil, false, workDir, InvokeOptions{}); err != nil || strings.Contains(args(), "--resume") {
		t.Fatalf("InvokeClaude resume=false: err=%v args=%q", err, args())
	}

	seed()
	if _, _, _, err := InvokeClaudeForComments(context.Background(), stage, issue, nil, workDir, InvokeOptions{}); err != nil || !strings.Contains(args(), "--resume sess_old") {
		t.Fatalf("InvokeClaudeForComments default: err=%v args=%q", err, args())
	}

	seed()
	if _, _, _, err := InvokeClaudeForComments(context.Background(), stage, issue, nil, workDir, InvokeOptions{NoResume: true}); err != nil || strings.Contains(args(), "--resume") {
		t.Fatalf("InvokeClaudeForComments NoResume: err=%v args=%q", err, args())
	}
}
