package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/handarbeit/fabrik/internal/skillref"
)

// The fixtures below are hand-built NDJSON except failed_skill_call_real.ndjson
// (testdata/skill_load), a verbatim capture of a real failed Skill call from
// `claude -p --output-format stream-json --verbose` (2026-10-11): the assistant
// line holds a tool_use named "Skill" with input.skill, and the user line holds
// the tool_result with "is_error":true correlated by tool_use_id. Detection keys
// on exactly that structure and never on the content text (FR-002).

func ndjsonSkillUse(id, skill string) string {
	return fmt.Sprintf(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":%q,"name":"Skill","input":{"skill":%q}}]},"parent_tool_use_id":null}`, id, skill)
}

func ndjsonToolUse(id, name string) string {
	return fmt.Sprintf(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":%q,"name":%q,"input":{"x":"y"}}]},"parent_tool_use_id":null}`, id, name)
}

func ndjsonToolResult(id, content string, isError bool) string {
	errField := ""
	if isError {
		errField = `,"is_error":true`
	}
	return fmt.Sprintf(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":%q,"content":%q%s}]},"parent_tool_use_id":null}`, id, content, errField)
}

func ndjsonAssistantText(text string) string {
	return fmt.Sprintf(`{"type":"assistant","message":{"content":[{"type":"text","text":%q}]},"parent_tool_use_id":null}`, text)
}

func ndjsonResult(result string) string {
	return fmt.Sprintf(`{"type":"result","subtype":"success","is_error":false,"result":%q,"num_turns":3,"total_cost_usd":0.01,"session_id":"s1"}`, result)
}

func stream(lines ...string) []byte { return []byte(strings.Join(lines, "\n") + "\n") }

// fakeClaudeStream builds a complete stream for the run-level tests: a failed
// own-skill call, the model nevertheless printing the completion marker.
func failedSkillStreamWithMarker(skill string) []byte {
	return stream(
		ndjsonSkillUse("t1", skill),
		ndjsonToolResult("t1", "<tool_use_error>Unknown skill</tool_use_error>", true),
		ndjsonAssistantText("Did the work.\nFABRIK_STAGE_COMPLETE"),
		ndjsonResult("Specified.\nFABRIK_SUMMARY_BEGIN\nok\nFABRIK_SUMMARY_END\nFABRIK_STAGE_COMPLETE"),
	)
}

// TestInterpretWouldCompleteWithoutDetector pins SC-001's premise: on the
// existing classifier alone, a failed own-skill call plus the completion marker
// IS reported as a completed stage. This is the defect; the override in
// runClaude (TestRunClaude_FailedSkillOverridesMarker) is what removes it.
func TestInterpretWouldCompleteWithoutDetector(t *testing.T) {
	raw := failedSkillStreamWithMarker("fabrik-stages:research")
	_, completed, _, err := interpretClaudeResult(context.Background(), 1, raw, nil, false, filepath.Join(t.TempDir(), "s.session"), t.TempDir(), "", 3, -1)
	if err != nil || !completed {
		t.Fatalf("premise changed: completed=%v err=%v", completed, err)
	}
}

func TestSkillLoadOverride_BeatsTheCompletionMarker(t *testing.T) {
	const own = "fabrik-stages:research"
	raw := failedSkillStreamWithMarker(own)
	sess := filepath.Join(t.TempDir(), "s.session")

	// Clean exit with the marker (the reported case).
	_, completed, _, err := interpretClaudeResult(context.Background(), 1, raw, nil, false, sess, t.TempDir(), "", 3, -1)
	if sle := skillLoadOverride(context.Background(), raw, own, false, completed, err); sle == nil {
		t.Fatal("clean exit + marker: override did not fire")
	}

	// Non-zero exit where the marker was found: "completed is completed" wins in
	// interpretClaudeResult, the override must still beat it.
	_, completed, _, err = interpretClaudeResult(context.Background(), 1, raw, errors.New("exit status 1"), false, sess, t.TempDir(), "", 3, -1)
	if !completed || err == nil {
		t.Fatalf("premise changed: completed=%v err=%v", completed, err)
	}
	if sle := skillLoadOverride(context.Background(), raw, own, false, completed, err); sle == nil {
		t.Fatal("non-zero exit + marker: override did not fire")
	}

	// Timeout kill: no result line, marker only in streamed assistant text.
	truncated := stream(ndjsonSkillUse("t1", own), ndjsonToolResult("t1", "boom", true), ndjsonAssistantText("Did the work in full.\nFABRIK_SUMMARY_BEGIN\nok\nFABRIK_SUMMARY_END\nFABRIK_STAGE_COMPLETE"))
	_, completed, _, err = interpretClaudeResult(context.Background(), 1, truncated, errors.New("signal: killed"), true, sess, t.TempDir(), "", 3, -1)
	if !completed {
		t.Fatalf("premise changed: truncated stream with marker not completed (err=%v)", err)
	}
	if sle := skillLoadOverride(context.Background(), truncated, own, true, completed, err); sle == nil {
		t.Fatal("timeout kill + marker: override did not fire")
	}
}

func TestSkillLoadOverride_Inert(t *testing.T) {
	const own = "fabrik-stages:research"
	raw := failedSkillStreamWithMarker(own)

	if skillLoadOverride(context.Background(), raw, "", false, true, nil) != nil {
		t.Error("skill-less stage must be inert")
	}
	// A classified failure (completed=false with an error: usage limit, tools
	// denied, resume failure, ...) keeps precedence.
	if skillLoadOverride(context.Background(), raw, own, false, false, errors.New("classified")) != nil {
		t.Error("an already-classified error must keep precedence")
	}
	// Engine shutdown (ctx cancelled, not our timeout) is not a skill fault.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if skillLoadOverride(ctx, raw, own, false, false, nil) != nil {
		t.Error("engine shutdown must not be reported as a skill-load failure")
	}
	// ...but a cancelled-by-our-own-timeout run still is evidence.
	if skillLoadOverride(ctx, raw, own, true, false, nil) == nil {
		t.Error("our own timeout kill with a failed skill call should still be detected")
	}
}

func TestSkillLoadOverride_HealthyAndProseRunsComplete(t *testing.T) {
	const own = "fabrik-stages:research"
	healthy := stream(
		ndjsonSkillUse("t1", own), ndjsonToolResult("t1", "Launching skill: "+own, false),
		ndjsonSkillUse("t2", "fabrik:other"), ndjsonToolResult("t2", "Unknown skill", true), // unrelated failing call
		ndjsonAssistantText("Unknown skill is what the other call said"),
		ndjsonResult("Unknown skill\nFABRIK_STAGE_COMPLETE"),
	)
	_, completed, _, err := interpretClaudeResult(context.Background(), 1, healthy, nil, false, filepath.Join(t.TempDir(), "s.session"), t.TempDir(), "", 3, -1)
	if sle := skillLoadOverride(context.Background(), healthy, own, false, completed, err); sle != nil {
		t.Fatalf("healthy run flagged: %v", sle)
	}
	if !completed || err != nil {
		t.Errorf("healthy run: completed=%v err=%v", completed, err)
	}
}

// skillref.SanitizeDetail carries its own copy of the tools-denied truncation
// rule (internal/skillref must not import engine). Pin the two to identical
// output so they cannot drift.
func TestSkillrefSanitizeDetailMatchesToolsDeniedSanitiser(t *testing.T) {
	strip := strings.NewReplacer("<tool_use_error>", "", "</tool_use_error>", "")
	inputs := []string{
		"",
		"   ",
		"Unknown skill: x",
		"<tool_use_error>Unknown skill: x</tool_use_error>",
		"line one\nline two\r\nline three",
		"has `backticks` inside",
		strings.Repeat("a", 200),
		strings.Repeat("a", 201),
		strings.Repeat("long text with `ticks`\n", 40),
		strings.Repeat("é", 300),
		"<tool_use_error>" + strings.Repeat("日本語", 120) + "</tool_use_error>",
	}
	for _, in := range inputs {
		if got, want := skillref.SanitizeDetail(in), sanitizeToolsDeniedCommand(strip.Replace(in)); got != want {
			t.Errorf("input %q:\n skillref = %q\n engine   = %q", in, got, want)
		}
	}
}
