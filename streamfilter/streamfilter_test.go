package streamfilter

import (
	"bytes"
	"strings"
	"testing"
)

func TestTrimBOM(t *testing.T) {
	bom := []byte{0xEF, 0xBB, 0xBF}
	withBOM := append(bom, []byte(`hello`)...)
	if got := string(TrimBOM(withBOM)); got != "hello" {
		t.Errorf("TrimBOM with BOM = %q, want %q", got, "hello")
	}
	noBOM := []byte(`hello`)
	if got := string(TrimBOM(noBOM)); got != "hello" {
		t.Errorf("TrimBOM without BOM = %q, want %q", got, "hello")
	}
	if got := TrimBOM(nil); got != nil {
		t.Errorf("TrimBOM(nil) = %v, want nil", got)
	}
}

func TestRenderLine_Text(t *testing.T) {
	line := []byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"hello world"}]}}`)
	got := RenderLine(line)
	if !strings.Contains(got, "hello world") {
		t.Errorf("RenderLine text = %q, expected to contain 'hello world'", got)
	}
	if !strings.Contains(got, "📝") {
		t.Errorf("RenderLine text = %q, expected to contain 📝", got)
	}
}

func TestRenderLine_Thinking(t *testing.T) {
	line := []byte(`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"I think therefore I am"}]}}`)
	got := RenderLine(line)
	if !strings.Contains(got, "I think therefore I am") {
		t.Errorf("RenderLine thinking = %q, expected thinking content", got)
	}
	if !strings.Contains(got, "💭") {
		t.Errorf("RenderLine thinking = %q, expected to contain 💭", got)
	}
}

func TestRenderLine_ThinkingTruncated(t *testing.T) {
	long := strings.Repeat("x", 600)
	line := []byte(`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"` + long + `"}]}}`)
	got := RenderLine(line)
	// Should be truncated to 500 chars + "..."
	if !strings.Contains(got, "...") {
		t.Errorf("RenderLine long thinking = %q, expected truncation '...'", got)
	}
}

func TestRenderLine_ToolUse(t *testing.T) {
	line := []byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"ls"}}]}}`)
	got := RenderLine(line)
	if !strings.Contains(got, "tool: Bash") {
		t.Errorf("RenderLine tool_use = %q, expected 'tool: Bash'", got)
	}
	if !strings.Contains(got, "command") {
		t.Errorf("RenderLine tool_use = %q, expected 'command' input key", got)
	}
}

func TestRenderLine_Result(t *testing.T) {
	line := []byte(`{"type":"result","num_turns":5,"total_cost_usd":0.0123}`)
	got := RenderLine(line)
	if !strings.Contains(got, "Done: 5 turns") {
		t.Errorf("RenderLine result = %q, expected '5 turns'", got)
	}
	if !strings.Contains(got, "0.0123") {
		t.Errorf("RenderLine result = %q, expected cost '0.0123'", got)
	}
}

func TestRenderLine_InvalidJSON(t *testing.T) {
	got := RenderLine([]byte(`not json`))
	if got != "" {
		t.Errorf("RenderLine invalid JSON = %q, expected empty string", got)
	}
}

func TestRenderLine_UnknownType(t *testing.T) {
	got := RenderLine([]byte(`{"type":"system","content":"ignored"}`))
	if got != "" {
		t.Errorf("RenderLine unknown type = %q, expected empty string", got)
	}
}

func TestRenderLine_AssistantNilMessage(t *testing.T) {
	got := RenderLine([]byte(`{"type":"assistant"}`))
	if got != "" {
		t.Errorf("RenderLine assistant with nil message = %q, expected empty string", got)
	}
}

func TestRunFilter_NDJSON(t *testing.T) {
	input := `{"type":"assistant","message":{"content":[{"type":"text","text":"step one"}]}}
{"type":"assistant","message":{"content":[{"type":"text","text":"step two"}]}}
{"type":"result","num_turns":2,"total_cost_usd":0.001}
`
	var out bytes.Buffer
	RunFilter(strings.NewReader(input), &out)
	got := out.String()
	if !strings.Contains(got, "step one") {
		t.Errorf("RunFilter NDJSON missing 'step one': %q", got)
	}
	if !strings.Contains(got, "step two") {
		t.Errorf("RunFilter NDJSON missing 'step two': %q", got)
	}
	if !strings.Contains(got, "Done: 2 turns") {
		t.Errorf("RunFilter NDJSON missing result: %q", got)
	}
}

func TestRunFilter_JSONArray(t *testing.T) {
	input := `[{"type":"assistant","message":{"content":[{"type":"text","text":"array text"}]}},{"type":"result","num_turns":1,"total_cost_usd":0.0}]`
	var out bytes.Buffer
	RunFilter(strings.NewReader(input), &out)
	got := out.String()
	if !strings.Contains(got, "array text") {
		t.Errorf("RunFilter JSON array missing 'array text': %q", got)
	}
}

func TestRunFilter_Empty(t *testing.T) {
	var out bytes.Buffer
	RunFilter(strings.NewReader(""), &out)
	if out.Len() != 0 {
		t.Errorf("RunFilter empty input produced output: %q", out.String())
	}
}

func TestRunFilter_BOMStripped(t *testing.T) {
	bom := []byte{0xEF, 0xBB, 0xBF}
	ndjson := `{"type":"assistant","message":{"content":[{"type":"text","text":"bom test"}]}}` + "\n"
	input := append(bom, []byte(ndjson)...)
	var out bytes.Buffer
	RunFilter(bytes.NewReader(input), &out)
	if !strings.Contains(out.String(), "bom test") {
		t.Errorf("RunFilter BOM-prefixed input missing 'bom test': %q", out.String())
	}
}
