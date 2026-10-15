package skillref

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fixtures below are hand-built NDJSON except failed_skill_call_real.ndjson
// (testdata), a verbatim capture of a real failed Skill call from
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

func TestDetectLoadFailure(t *testing.T) {
	const own = "fabrik-stages:research"
	tests := []struct {
		name     string
		raw      []byte
		expected string
		want     bool
		called   string
	}{
		{
			name:     "failed own-skill call is a failure",
			raw:      stream(ndjsonSkillUse("t1", own), ndjsonToolResult("t1", "<tool_use_error>Unknown skill: x</tool_use_error>", true)),
			expected: own,
			want:     true,
		},
		{
			name:     "bare-name call against a qualified configuration",
			raw:      stream(ndjsonSkillUse("t1", "research"), ndjsonToolResult("t1", "Unknown skill", true)),
			expected: own,
			want:     true,
			called:   "research",
		},
		{
			name:     "qualified call against a bare configuration (empty catalogue passthrough)",
			raw:      stream(ndjsonSkillUse("t1", own), ndjsonToolResult("t1", "boom", true)),
			expected: "research",
			want:     true,
			called:   own,
		},
		{
			name:     "same-named skill in a different plugin is not the own skill",
			raw:      stream(ndjsonSkillUse("t1", "other:research"), ndjsonToolResult("t1", "boom", true)),
			expected: own,
		},
		{
			name:     "unrelated failing Skill call",
			raw:      stream(ndjsonSkillUse("t1", "fabrik-stages:plan"), ndjsonToolResult("t1", "Unknown skill", true)),
			expected: own,
		},
		{
			name:     "failing non-Skill tool",
			raw:      stream(ndjsonToolUse("t1", "Bash"), ndjsonToolResult("t1", "Unknown skill", true)),
			expected: own,
		},
		{
			name:     "successful own-skill call",
			raw:      stream(ndjsonSkillUse("t1", own), ndjsonToolResult("t1", "Launching skill: "+own, false)),
			expected: own,
		},
		{
			name:     "failed then succeeded is not a failure",
			raw:      stream(ndjsonSkillUse("t1", "research"), ndjsonToolResult("t1", "Unknown skill", true), ndjsonSkillUse("t2", own), ndjsonToolResult("t2", "Launching skill", false)),
			expected: own,
		},
		{
			name:     "no skill call at all (resumed session)",
			raw:      stream(ndjsonAssistantText("continuing"), ndjsonResult("done")),
			expected: own,
		},
		{
			name:     "prose mention with a loaded skill",
			raw:      stream(ndjsonSkillUse("t1", own), ndjsonToolResult("t1", "Launching skill", false), ndjsonAssistantText("Unknown skill errors are documented in #1373"), ndjsonResult("Unknown skill")),
			expected: own,
		},
		{
			name:     "prose mention without any call",
			raw:      stream(ndjsonAssistantText("Unknown skill: "+own), ndjsonResult("Unknown skill: "+own)),
			expected: own,
		},
		{
			name:     "call with no result (truncated stream)",
			raw:      stream(ndjsonSkillUse("t1", own)),
			expected: own,
		},
		{
			name:     "unparseable lines are ignored",
			raw:      stream(`{not json`, ndjsonSkillUse("t1", own), `also not json`, ndjsonToolResult("t1", "boom", true)),
			expected: own,
			want:     true,
		},
		{
			name:     "result without tool_use_id is ignored",
			raw:      stream(ndjsonSkillUse("t1", own), `{"type":"user","message":{"content":[{"type":"tool_result","content":"x","is_error":true}]}}`),
			expected: own,
		},
		{
			name:     "sub-agent traffic is ignored",
			raw:      stream(strings.Replace(ndjsonSkillUse("t1", own), `"parent_tool_use_id":null`, `"parent_tool_use_id":"toolu_parent"`, 1), strings.Replace(ndjsonToolResult("t1", "boom", true), `"parent_tool_use_id":null`, `"parent_tool_use_id":"toolu_parent"`, 1)),
			expected: own,
		},
		{
			name:     "skill-less stage is inert",
			raw:      stream(ndjsonSkillUse("t1", own), ndjsonToolResult("t1", "boom", true)),
			expected: "",
		},
		{
			name:     "plain-string user turn content does not break parsing",
			raw:      stream(`{"type":"user","message":{"role":"user","content":"hello"}}`, ndjsonSkillUse("t1", own), ndjsonToolResult("t1", "boom", true)),
			expected: own,
			want:     true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DetectLoadFailure(tt.raw, tt.expected)
			if (got != nil) != tt.want {
				t.Fatalf("DetectLoadFailure = %v, want failure=%v", got, tt.want)
			}
			if got != nil {
				if got.Skill != tt.expected {
					t.Errorf("Skill = %q, want %q", got.Skill, tt.expected)
				}
				if got.Called != tt.called {
					t.Errorf("Called = %q, want %q", got.Called, tt.called)
				}
				if got.ToolUseID != "t1" {
					t.Errorf("ToolUseID = %q, want t1", got.ToolUseID)
				}
			}
		})
	}
}

func TestDetectLoadFailure_RealCapture(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "failed_skill_call_real.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	got := DetectLoadFailure(raw, "nonexistent-skill-xyz")
	if got == nil {
		t.Fatal("real captured failed Skill call not detected")
	}
	if got.Detail != "Unknown skill: nonexistent-skill-xyz" {
		t.Errorf("Detail = %q, want the tag-stripped tool_result text", got.Detail)
	}
	if DetectLoadFailure(raw, "some-other-skill") != nil {
		t.Error("a different configured skill must not match the captured call")
	}
}

func TestDetectLoadFailure_ContentArrayAndSanitising(t *testing.T) {
	raw := stream(
		ndjsonSkillUse("t1", "p:s"),
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","is_error":true,"content":[{"type":"text","text":"line one\nwith `+"`ticks`"+`"},{"type":"text","text":"two"}]}]}}`,
	)
	got := DetectLoadFailure(raw, "p:s")
	if got == nil {
		t.Fatal("expected a failure")
	}
	if strings.ContainsAny(got.Detail, "\n`") {
		t.Errorf("Detail %q must be one line without backticks", got.Detail)
	}
	if !strings.Contains(got.Detail, "line one") || !strings.Contains(got.Detail, "two") {
		t.Errorf("Detail %q lost content", got.Detail)
	}

	long := strings.Repeat("x", 5000)
	raw = stream(ndjsonSkillUse("t1", "p:s"), ndjsonToolResult("t1", long, true))
	got = DetectLoadFailure(raw, "p:s")
	if got == nil || len(got.Detail) >= 1000 {
		t.Errorf("Detail not truncated: %d bytes", len(got.Detail))
	}
}

func TestDetectLoadFailure_LargeLine(t *testing.T) {
	// A single line far over bufio.Scanner's default 64KB token limit.
	big := ndjsonToolResult("t0", strings.Repeat("y", 300_000), false)
	raw := stream(big, ndjsonSkillUse("t1", "p:s"), ndjsonToolResult("t1", "boom", true))
	if DetectLoadFailure(raw, "p:s") == nil {
		t.Fatal("large preceding line must not hide the failure")
	}
}
