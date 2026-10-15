package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

// streamStep is one main-thread tool call in a synthetic stream: the call, and
// whether its result is an error (denied calls are errors, as captured live —
// engine/testdata/hook_denied_write_stream.jsonl). noResult drops the result.
type streamStep struct {
	id       string
	tool     string
	isError  bool
	noResult bool
	parent   string // non-empty = sub-agent traffic
	denied   bool   // listed in permission_denials
}

// buildDenialStream renders steps as stream-json lines followed by a clean
// non-completing result line carrying the denied steps.
func buildDenialStream(steps []streamStep, extraDenials ...string) []byte {
	var sb strings.Builder
	var denials []string
	parentField := func(p string) string {
		if p == "" {
			return "null"
		}
		return fmt.Sprintf("%q", p)
	}
	for _, s := range steps {
		fmt.Fprintf(&sb, `{"type":"assistant","parent_tool_use_id":%s,"message":{"content":[{"type":"tool_use","id":%q,"name":%q,"input":{}}]}}`+"\n", parentField(s.parent), s.id, s.tool)
		if !s.noResult {
			fmt.Fprintf(&sb, `{"type":"user","parent_tool_use_id":%s,"message":{"content":[{"type":"tool_result","tool_use_id":%q,"is_error":%t,"content":"x"}]}}`+"\n", parentField(s.parent), s.id, s.isError)
		}
		if s.denied {
			denials = append(denials, fmt.Sprintf(`{"tool_name":%q,"tool_use_id":%q,"tool_input":{}}`, s.tool, s.id))
		}
	}
	for _, d := range extraDenials {
		denials = append(denials, d)
	}
	fmt.Fprintf(&sb, `{"type":"result","result":"gave up","is_error":false,"subtype":"success","terminal_reason":"completed","num_turns":5,"total_cost_usd":0.05,"permission_denials":[%s]}`+"\n", strings.Join(denials, ","))
	return []byte(sb.String())
}

func interpretDenialStream(t *testing.T, raw []byte, commits int) error {
	t.Helper()
	_, completed, _, err := interpretClaudeResult(context.Background(), 1, raw, nil, false, t.TempDir()+"/sess", t.TempDir(), "", 2, commits)
	if completed {
		t.Fatalf("completed = true, want false")
	}
	return err
}

func isToolsDenied(err error) bool {
	var te *claudeToolsDeniedError
	return errors.As(err, &te)
}

func TestDenialCausal(t *testing.T) {
	denied := func(id, tool string) streamStep {
		return streamStep{id: id, tool: tool, isError: true, denied: true}
	}
	ok := func(id, tool string) streamStep { return streamStep{id: id, tool: tool} }

	tests := []struct {
		name       string
		steps      []streamStep
		extra      []string
		commits    int
		wantCause  bool
		wantReason string
	}{
		{
			name:      "1523 shape: reads succeed, every write denied, no commit",
			steps:     []streamStep{ok("r1", "Read"), denied("w1", "Write"), ok("r2", "Read"), denied("e1", "Edit")},
			commits:   -1,
			wantCause: true,
		},
		{
			name:      "1523 shape with read-only Bash succeeding after the denial stays exempt",
			steps:     []streamStep{denied("w1", "Write"), ok("b1", "Bash"), ok("b2", "Bash")},
			commits:   0,
			wantCause: true,
		},
		{
			name:      "run that ends on a denial stays exempt",
			steps:     []streamStep{ok("r1", "Read"), ok("e0", "Edit"), denied("w1", "Write")},
			commits:   0,
			wantCause: true,
			// the earlier Edit precedes the denial, so it is not recovery
		},
		{
			name:      "early incidental denial, successful edits, then a final denied commit stays exempt",
			steps:     []streamStep{denied("b1", "Bash"), ok("e1", "Edit"), ok("e2", "Write"), denied("b2", "Bash")},
			commits:   0,
			wantCause: true,
		},
		{
			name:       "single early denial then successful Edit/Write is incidental (FR-001)",
			steps:      []streamStep{denied("w1", "Write"), ok("e1", "Edit"), ok("e2", "Edit")},
			commits:    -1,
			wantReason: denialReasonRecovered,
		},
		{
			name:       "denied Bash then successful Write is incidental",
			steps:      []streamStep{denied("b1", "Bash"), ok("w1", "Write")},
			commits:    -1,
			wantReason: denialReasonRecovered,
		},
		{
			name:       "recovery with a missing result counts as recovery (bias to ordinary failure)",
			steps:      []streamStep{denied("w1", "Write"), {id: "e1", tool: "Edit", noResult: true}},
			commits:    -1,
			wantReason: denialReasonRecovered,
		},
		{
			name:      "later Edit that itself errored is not recovery",
			steps:     []streamStep{denied("w1", "Write"), {id: "e1", tool: "Edit", isError: true}},
			commits:   -1,
			wantCause: true,
		},
		{
			name:       "several denials plus a commit is incidental (SC-006)",
			steps:      []streamStep{denied("b1", "Bash"), denied("b2", "Bash"), denied("w1", "Write")},
			commits:    1,
			wantReason: denialReasonProgress,
		},
		{
			name:       "denial on a non-write tool only",
			steps:      []streamStep{denied("f1", "WebFetch"), denied("r1", "Read")},
			commits:    -1,
			wantReason: denialReasonNoWrite,
		},
		{
			name:       "denial with an empty tool_use_id is not joinable",
			steps:      []streamStep{ok("w1", "Write")},
			extra:      []string{`{"tool_name":"Write","tool_use_id":"","tool_input":{}}`},
			commits:    -1,
			wantReason: denialReasonNoJoin,
		},
		{
			name:       "denial whose tool_use_id matches no tool_use is not joinable",
			steps:      []streamStep{ok("r1", "Read")},
			extra:      []string{`{"tool_name":"Write","tool_use_id":"toolu_missing","tool_input":{}}`},
			commits:    -1,
			wantReason: denialReasonNoJoin,
		},
		{
			name:       "denial in sub-agent traffic is not joinable to the main thread",
			steps:      []streamStep{{id: "w1", tool: "Write", isError: true, denied: true, parent: "toolu_task"}},
			commits:    -1,
			wantReason: denialReasonNoJoin,
		},
		{
			name:      "sub-agent Edit success is not main-thread recovery",
			steps:     []streamStep{denied("w1", "Write"), {id: "e1", tool: "Edit", parent: "toolu_task"}},
			commits:   -1,
			wantCause: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := buildDenialStream(tt.steps, tt.extra...)
			var resp claudeResponse
			lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
			if err := json.Unmarshal([]byte(lines[len(lines)-1]), &resp); err != nil {
				t.Fatal(err)
			}
			cause, reason := denialCausal(resp, raw, tt.commits)
			if cause != tt.wantCause || reason != tt.wantReason {
				t.Errorf("denialCausal = (%v, %q), want (%v, %q)", cause, reason, tt.wantCause, tt.wantReason)
			}
			// The same verdict must reach interpretClaudeResult's return value.
			if got := isToolsDenied(interpretDenialStream(t, raw, tt.commits)); got != tt.wantCause {
				t.Errorf("interpretClaudeResult tools-denied = %v, want %v", got, tt.wantCause)
			}
		})
	}
}

// TestDenialCausal_UnreadableStream: with no stream at all (a bare result
// line), nothing can be joined, so the run is an ordinary failure (FR-005).
func TestDenialCausal_UnreadableStream(t *testing.T) {
	raw := []byte(`{"result":"x","is_error":false,"subtype":"success","terminal_reason":"completed","permission_denials":[{"tool_name":"Write","tool_use_id":"toolu_1","tool_input":{}}]}`)
	if isToolsDenied(interpretDenialStream(t, raw, -1)) {
		t.Errorf("bare result line must not be classified denial-caused")
	}
	truncated := []byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"w1","na`)
	if isToolsDenied(interpretDenialStream(t, append(truncated, raw...), -1)) {
		t.Errorf("truncated stream must not be classified denial-caused")
	}
}

// TestDenialCausal_RealCapturedStream replays a stream captured from the real
// CLI (2.1.299) with a PreToolUse hook denying Write: the denial's tool_use_id
// equals the stream's tool_use id, the denied call's result is an error, and
// the run is denial-caused.
func TestDenialCausal_RealCapturedStream(t *testing.T) {
	raw, err := os.ReadFile("testdata/hook_denied_write_stream.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	calls, isErr := scanMainThreadToolCalls(raw)
	if len(calls) != 2 || calls[1].Name != "Write" || !isErr[calls[1].ID] || isErr[calls[0].ID] {
		t.Fatalf("unexpected scan of captured stream: %+v %+v", calls, isErr)
	}
	// The captured run ends after the denial (the result line is non-completing).
	if !isToolsDenied(interpretDenialStream(t, raw, -1)) {
		t.Errorf("captured hook-denied Write stream must be denial-caused")
	}
}

// TestDenialCausal_NeutralizedRestoresOldBehavior proves the narrowing is not
// vacuous (FR-011): with the gate neutralized, the single-incidental-denial
// run is exempt again, exactly as before #1556.
func TestDenialCausal_NeutralizedRestoresOldBehavior(t *testing.T) {
	raw := buildDenialStream([]streamStep{
		{id: "w1", tool: "Write", isError: true, denied: true},
		{id: "e1", tool: "Edit"},
	})
	if isToolsDenied(interpretDenialStream(t, raw, -1)) {
		t.Fatalf("precondition: incidental denial must be an ordinary failure")
	}
	restore := SetToolsDeniedCausalityDisabledForTest()
	defer restore()
	if !isToolsDenied(interpretDenialStream(t, raw, -1)) {
		t.Errorf("with the causal gate neutralized the run must be exempt (old behavior)")
	}
}

// TestInterpretClaudeResult_IncidentalDenialLogsDiagnostic: an incidental run
// logs one engine-log line and returns no error.
func TestInterpretClaudeResult_IncidentalDenialLogsDiagnostic(t *testing.T) {
	raw := buildDenialStream([]streamStep{
		{id: "w1", tool: "Write", isError: true, denied: true},
		{id: "e1", tool: "Edit"},
	})
	var logs []string
	orig := claudeLogf
	claudeLogf = func(_ int, _, format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	defer func() { claudeLogf = orig }()
	if err := interpretDenialStream(t, raw, -1); err != nil {
		t.Fatalf("err = %v, want nil (ordinary failure)", err)
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "judged incidental (recovered)") || strings.Contains(joined, "not charged against max_retries") {
		t.Errorf("logs = %q", joined)
	}
}

// deniedCallLines returns the stream lines of one denied main-thread call
// (tool_use plus an is_error tool_result), for fixtures that carry their own
// terminal result line.
func deniedCallLines(id, tool string) string {
	return fmt.Sprintf(`{"type":"assistant","parent_tool_use_id":null,"message":{"content":[{"type":"tool_use","id":%q,"name":%q,"input":{}}]}}`+"\n"+
		`{"type":"user","parent_tool_use_id":null,"message":{"content":[{"type":"tool_result","tool_use_id":%q,"is_error":true,"content":"x"}]}}`+"\n", id, tool, id)
}
