package engine

import (
	"testing"

	"github.com/handarbeit/fabrik/stages"
)

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
	output := []byte(`{"result":"hello world","session_id":"sess_abc123","num_turns":5,"total_cost_usd":0.0042,"modelUsage":{"claude-sonnet-4-6":{"inputTokens":100,"outputTokens":50,"cacheCreationInputTokens":10,"cacheReadInputTokens":5}}}`)

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
	if diff := usage.CostUSD - 0.0042; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("CostUSD = %f, want ~0.0042", usage.CostUSD)
	}
}

func TestTokenUsageFromResponse_MultiModel(t *testing.T) {
	output := []byte(`{"result":"ok","session_id":"s","total_cost_usd":0.003,"modelUsage":{"claude-sonnet-4-6":{"inputTokens":100,"outputTokens":50,"cacheCreationInputTokens":10,"cacheReadInputTokens":5},"claude-haiku-3":{"inputTokens":200,"outputTokens":30,"cacheCreationInputTokens":0,"cacheReadInputTokens":20}}}`)
	resp, ok := parseClaudeJSON(output)
	if !ok {
		t.Fatal("expected successful parse")
	}
	usage := tokenUsageFromResponse(resp)
	if usage.InputTokens != 300 {
		t.Errorf("InputTokens = %d, want 300", usage.InputTokens)
	}
	if usage.OutputTokens != 80 {
		t.Errorf("OutputTokens = %d, want 80", usage.OutputTokens)
	}
	if usage.CacheCreationTokens != 10 {
		t.Errorf("CacheCreationTokens = %d, want 10", usage.CacheCreationTokens)
	}
	if usage.CacheReadTokens != 25 {
		t.Errorf("CacheReadTokens = %d, want 25", usage.CacheReadTokens)
	}
	if diff := usage.CostUSD - 0.003; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("CostUSD = %f, want ~0.003", usage.CostUSD)
	}
}

func TestTokenUsageAdd(t *testing.T) {
	a := TokenUsage{InputTokens: 10, OutputTokens: 5, CacheCreationTokens: 2, CacheReadTokens: 3, CostUSD: 0.001}
	b := TokenUsage{InputTokens: 20, OutputTokens: 15, CacheCreationTokens: 8, CacheReadTokens: 7, CostUSD: 0.002}
	got := a.add(b)
	if got.InputTokens != 30 {
		t.Errorf("InputTokens = %d, want 30", got.InputTokens)
	}
	if got.OutputTokens != 20 {
		t.Errorf("OutputTokens = %d, want 20", got.OutputTokens)
	}
	if got.CacheCreationTokens != 10 {
		t.Errorf("CacheCreationTokens = %d, want 10", got.CacheCreationTokens)
	}
	if got.CacheReadTokens != 10 {
		t.Errorf("CacheReadTokens = %d, want 10", got.CacheReadTokens)
	}
	if diff := got.CostUSD - 0.003; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("CostUSD = %f, want ~0.003", got.CostUSD)
	}
	// Adding zero value leaves original unchanged
	zero := TokenUsage{}
	if a.add(zero) != a {
		t.Error("adding zero TokenUsage should return original")
	}
}

func TestTokenUsageFromResponse_NoModelUsage(t *testing.T) {
	// When modelUsage is absent (older CLI or error response), token counts are zero
	// but CostUSD is still populated from the top-level field.
	output := []byte(`{"result":"hello","session_id":"s","total_cost_usd":0.005}`)
	resp, ok := parseClaudeJSON(output)
	if !ok {
		t.Fatal("expected successful parse")
	}
	usage := tokenUsageFromResponse(resp)
	if usage.InputTokens != 0 || usage.OutputTokens != 0 || usage.CacheCreationTokens != 0 || usage.CacheReadTokens != 0 {
		t.Errorf("expected zero token counts when modelUsage absent, got %+v", usage)
	}
	if diff := usage.CostUSD - 0.005; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("CostUSD = %f, want ~0.005", usage.CostUSD)
	}
}

func TestParseClaudeJSON_InvalidJSON(t *testing.T) {
	_, ok := parseClaudeJSON([]byte(`not json at all`))
	if ok {
		t.Error("expected parse failure for invalid JSON")
	}
}

func TestParseClaudeJSON_EmptyResultWithSessionID(t *testing.T) {
	// Empty result but valid session ID (e.g., max_turns hit) should still parse.
	resp, ok := parseClaudeJSON([]byte(`{"result":"","session_id":"sess_1"}`))
	if !ok {
		t.Error("expected parse success for empty result with session_id")
	}
	if resp.SessionID != "sess_1" {
		t.Errorf("SessionID = %q, want sess_1", resp.SessionID)
	}
}

func TestParseClaudeJSON_EmptyResultNoSessionID(t *testing.T) {
	// Truly empty/invalid — no result, no session ID.
	_, ok := parseClaudeJSON([]byte(`{"result":"","session_id":""}`))
	if ok {
		t.Error("expected parse failure for empty result with no session_id")
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
