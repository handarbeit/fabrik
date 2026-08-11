package engine

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// TestClassifyToolsDenied covers structural detection from the CLI's own
// result object (claudeResponse.PermissionDenials) — never from output
// prose, mirroring the discipline TestClassifyAPIErrorExit/
// TestClassifyUsageLimitExit exercise for their own structural fields. See
// #1523.
func TestClassifyToolsDenied(t *testing.T) {
	tests := []struct {
		name          string
		resp          claudeResponse
		wantDetected  bool
		wantToolNames []string
	}{
		{
			name:         "nil PermissionDenials does not detect",
			resp:         claudeResponse{},
			wantDetected: false,
		},
		{
			name: "empty PermissionDenials does not detect",
			resp: claudeResponse{PermissionDenials: []struct {
				ToolName string `json:"tool_name"`
			}{}},
			wantDetected: false,
		},
		{
			name: "single denial detects",
			resp: claudeResponse{PermissionDenials: []struct {
				ToolName string `json:"tool_name"`
			}{
				{ToolName: "Write"},
			}},
			wantDetected:  true,
			wantToolNames: []string{"Write"},
		},
		{
			name: "multiple distinct tool names detects, order preserved",
			resp: claudeResponse{PermissionDenials: []struct {
				ToolName string `json:"tool_name"`
			}{
				{ToolName: "Write"},
				{ToolName: "Edit"},
				{ToolName: "Bash"},
			}},
			wantDetected:  true,
			wantToolNames: []string{"Write", "Edit", "Bash"},
		},
		{
			name: "duplicate tool names are deduped, first-seen order preserved",
			resp: claudeResponse{PermissionDenials: []struct {
				ToolName string `json:"tool_name"`
			}{
				{ToolName: "Write"},
				{ToolName: "Edit"},
				{ToolName: "Write"},
				{ToolName: "Write"},
			}},
			wantDetected:  true,
			wantToolNames: []string{"Write", "Edit"},
		},
		{
			name: "entries with empty tool_name are skipped",
			resp: claudeResponse{PermissionDenials: []struct {
				ToolName string `json:"tool_name"`
			}{
				{ToolName: ""},
			}},
			wantDetected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			toolNames, detected := classifyToolsDenied(tt.resp)
			if detected != tt.wantDetected {
				t.Fatalf("detected = %v, want %v", detected, tt.wantDetected)
			}
			if detected && !reflect.DeepEqual(toolNames, tt.wantToolNames) {
				t.Errorf("toolNames = %v, want %v", toolNames, tt.wantToolNames)
			}
		})
	}
}

// TestInterpretClaudeResult_ToolsDenied_ReturnsSentinelError verifies that
// interpretClaudeResult, when Claude exits cleanly (runErr == nil) with a
// populated permission_denials array and no FABRIK_STAGE_COMPLETE marker,
// returns a *claudeToolsDeniedError — this is the seam finalizeStageOutcome
// classifies on (AC1).
func TestInterpretClaudeResult_ToolsDenied_ReturnsSentinelError(t *testing.T) {
	raw := []byte(`{"result":"I could not write the file — permission denied.","session_id":"sid-1","is_error":false,"subtype":"success","terminal_reason":"completed","num_turns":3,"total_cost_usd":0.05,"permission_denials":[{"tool_name":"Write","tool_use_id":"toolu_1","tool_input":{}}]}`)

	text, completed, usage, err := interpretClaudeResult(context.Background(), 1, raw, nil, false, t.TempDir()+"/sess", t.TempDir(), "", 2)

	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	var toolsErr *claudeToolsDeniedError
	if !errors.As(err, &toolsErr) {
		t.Fatalf("errors.As(err, *claudeToolsDeniedError) = false; err = %v", err)
	}
	if !reflect.DeepEqual(toolsErr.ToolNames, []string{"Write"}) {
		t.Errorf("ToolNames = %v, want [Write]", toolsErr.ToolNames)
	}
	if completed {
		t.Errorf("expected completed=false for a tools-denied exit")
	}
	if usage.TurnsUsed != 3 {
		t.Errorf("usage.TurnsUsed = %d, want 3", usage.TurnsUsed)
	}
	_ = text
}

// TestInterpretClaudeResult_ToolsDenied_MarkerTakesPrecedence verifies that a
// genuine stage completion (FABRIK_STAGE_COMPLETE present) is never
// misclassified as a tools-denied exit, even when permission_denials is also
// present — a denial the model worked around and still completed the stage
// is ordinary success, per the Plan's "!completed gate" decision.
func TestInterpretClaudeResult_ToolsDenied_MarkerTakesPrecedence(t *testing.T) {
	raw := []byte(`{"result":"worked around the denial\nFABRIK_STAGE_COMPLETE","is_error":false,"subtype":"success","terminal_reason":"completed","permission_denials":[{"tool_name":"Write"}]}`)

	_, completed, _, err := interpretClaudeResult(context.Background(), 1, raw, nil, false, t.TempDir()+"/sess", t.TempDir(), "", 2)

	if err != nil {
		t.Fatalf("expected nil error (marker present), got %v", err)
	}
	if !completed {
		t.Errorf("expected completed=true (marker present)")
	}
}

// TestInterpretClaudeResult_ToolsDenied_UnparseableJSON_NotClassified
// verifies that when JSON parsing fails (ok == false), there is no
// structured result object to trust, so the invocation is never classified
// as a tools-denied exit by any means. Mirrors #1183 Requirement 2 and
// #1458 Acceptance 6, applied to this new classifier (AC8 non-vacuousness:
// a classifier that ran unconditionally would misfire here).
func TestInterpretClaudeResult_ToolsDenied_UnparseableJSON_NotClassified(t *testing.T) {
	raw := []byte("not valid json, but mentions permission_denials anyway")

	_, completed, _, err := interpretClaudeResult(context.Background(), 1, raw, nil, false, t.TempDir()+"/sess", t.TempDir(), "", 2)

	if err != nil {
		var toolsErr *claudeToolsDeniedError
		if errors.As(err, &toolsErr) {
			t.Fatalf("unparseable JSON must never be classified as tools-denied, got claudeToolsDeniedError")
		}
	}
	if completed {
		t.Errorf("expected completed=false")
	}
}

// TestInterpretClaudeResult_ToolsDenied_MarkerIndependence verifies that the
// classification outcome does not depend on whether the worker also emitted
// FABRIK_BLOCKED_ON_INPUT in its output text (R6/AC7 at the classification
// layer): the sentinel error still wins over any prose the model wrote, which
// is what makes item.go's "blockedOnInput := err == nil && ..." gate
// structurally unreachable for a detected denial — no explicit
// marker-suppression code is needed anywhere else.
func TestInterpretClaudeResult_ToolsDenied_MarkerIndependence(t *testing.T) {
	raw := []byte(`{"result":"Permission to use Edit has been denied because Claude Code is running in don't ask mode.\nFABRIK_BLOCKED_ON_INPUT","is_error":false,"subtype":"success","terminal_reason":"completed","permission_denials":[{"tool_name":"Edit"}]}`)

	_, completed, _, err := interpretClaudeResult(context.Background(), 1, raw, nil, false, t.TempDir()+"/sess", t.TempDir(), "", 2)

	if err == nil {
		t.Fatalf("expected error even though output text also contains FABRIK_BLOCKED_ON_INPUT")
	}
	var toolsErr *claudeToolsDeniedError
	if !errors.As(err, &toolsErr) {
		t.Fatalf("errors.As(err, *claudeToolsDeniedError) = false; err = %v", err)
	}
	if completed {
		t.Errorf("expected completed=false")
	}
}
