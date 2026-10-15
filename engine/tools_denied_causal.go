package engine

import (
	"bufio"
	"bytes"
	"encoding/json"
	"sync/atomic"
)

// Causal gate for the tools-denied exemption (#1556, ADR-1523 amendment).
//
// classifyToolsDenied only establishes that a denial occurred somewhere in the
// run. denialCausal decides whether a denial plausibly *caused* the
// non-completion, so an incidental, worked-around denial no longer exempts an
// otherwise ordinary failure from max_retries. All inputs are structural: the
// CLI's permission_denials array (tool_use_id), the stream-json tool_use /
// tool_result blocks (ids and is_error flags, never text) and the measured
// commit count. No prose is consulted.
//
// A run is denial-caused only if ALL of these hold; anything missing or
// ambiguous resolves to "not causal" (an ordinary failure):
//
//  1. No measured progress: commitsPushed <= 0 (-1 = unmeasured does not block).
//  2. Joinable evidence: at least one permission_denials entry carries a
//     tool_use_id that matches a main-thread tool_use in the stream.
//  3. Relevance: at least one joined denial names a write-capable tool
//     (Edit, Write, NotebookEdit, Bash).
//  4. No recovery: after the first relevant denial the main thread made no
//     Edit/Write/NotebookEdit call (other than the denied calls) whose result
//     is missing or not an error. Bash is deliberately not a recovery signal:
//     #1523's incident had read-only Bash succeeding beside denied mutating
//     calls. Committed work is covered by rule 1 instead.
//
// None of the rules depends on how many denials there are, so the rule stays
// correct once Fabrik's own enforcement (#1372) denies routinely.
//
// Error direction (FR-005, FR-009). False negatives (a real permission failure
// judged incidental) cost one retry against max_retries, e.g. a hook that
// denies Bash but lets Edit succeed. Residual false positive: a single
// incidental Bash denial in a run that afterwards only reads or runs Bash
// (typically a read_only stage) with no commit still looks denial-caused; it
// is bounded by MaxToolsDeniedRetries. Stage awareness would close it but
// changes interpretClaudeResult's signature, so it is a deliberate follow-up.

// toolsDeniedCausalityDisabled neutralizes the causal gate (the pre-#1556
// behavior: any denial is causal). Test-only; see
// SetToolsDeniedCausalityDisabledForTest.
var toolsDeniedCausalityDisabled atomic.Bool

// SetToolsDeniedCausalityDisabledForTest disables the causal gate until the
// returned restore func is called. Tests using it must not run in parallel.
func SetToolsDeniedCausalityDisabledForTest() (restore func()) {
	toolsDeniedCausalityDisabled.Store(true)
	return func() { toolsDeniedCausalityDisabled.Store(false) }
}

// Reasons returned by denialCausal for a non-causal verdict.
const (
	denialReasonProgress  = "progress"
	denialReasonNoJoin    = "no-joinable-denial"
	denialReasonNoWrite   = "no-write-denial"
	denialReasonRecovered = "recovered"
)

// writeCapableTools are the tools whose denial can plausibly block a stage.
var writeCapableTools = map[string]bool{"Edit": true, "Write": true, "NotebookEdit": true, "Bash": true}

// fileMutatingTools are the tools whose later success counts as recovery.
var fileMutatingTools = map[string]bool{"Edit": true, "Write": true, "NotebookEdit": true}

// streamToolCall is one main-thread tool_use in stream order.
type streamToolCall struct {
	ID   string
	Name string
}

// scanMainThreadToolCalls returns the main-thread tool_use blocks in stream
// order and each tool_result's is_error by tool_use_id. Sub-agent traffic (a
// non-null parent_tool_use_id) and unparseable lines are ignored, as in
// detectSkillLoadFailure.
func scanMainThreadToolCalls(rawOutput []byte) (calls []streamToolCall, isError map[string]bool) {
	isError = map[string]bool{}
	if len(rawOutput) == 0 {
		return nil, isError
	}
	scanner := bufio.NewScanner(bytes.NewReader(rawOutput))
	scanner.Buffer(make([]byte, 0, 64*1024), len(rawOutput)+1)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var env struct {
			Type            string          `json:"type"`
			ParentToolUseID json.RawMessage `json:"parent_tool_use_id"`
			Message         struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(line, &env); err != nil {
			continue
		}
		if env.Type != "assistant" && env.Type != "user" {
			continue
		}
		if p := bytes.TrimSpace(env.ParentToolUseID); len(p) > 0 && !bytes.Equal(p, []byte("null")) {
			continue
		}
		var blocks []struct {
			Type      string `json:"type"`
			ID        string `json:"id"`
			Name      string `json:"name"`
			ToolUseID string `json:"tool_use_id"`
			IsError   bool   `json:"is_error"`
		}
		if err := json.Unmarshal(env.Message.Content, &blocks); err != nil {
			continue
		}
		for _, b := range blocks {
			switch {
			case env.Type == "assistant" && b.Type == "tool_use" && b.ID != "":
				calls = append(calls, streamToolCall{ID: b.ID, Name: b.Name})
			case env.Type == "user" && b.Type == "tool_result" && b.ToolUseID != "":
				isError[b.ToolUseID] = b.IsError
			}
		}
	}
	return calls, isError
}

// denialCausal reports whether the denials in resp plausibly caused the
// non-completion (rules 1-4 above). reason is "" when causal, else one of the
// denialReason* values. Pure: callers log the verdict.
func denialCausal(resp claudeResponse, rawOutput []byte, commitsPushed int) (causal bool, reason string) {
	if toolsDeniedCausalityDisabled.Load() {
		return true, ""
	}
	if commitsPushed > 0 {
		return false, denialReasonProgress
	}
	calls, isError := scanMainThreadToolCalls(rawOutput)
	position := make(map[string]int, len(calls))
	name := make(map[string]string, len(calls))
	for i, c := range calls {
		position[c.ID] = i
		name[c.ID] = c.Name
	}
	denied := map[string]bool{}
	firstRelevant := -1
	joined := false
	for _, d := range resp.PermissionDenials {
		pos, ok := position[d.ToolUseID]
		if d.ToolUseID == "" || !ok {
			continue
		}
		joined = true
		denied[d.ToolUseID] = true
		if writeCapableTools[name[d.ToolUseID]] && (firstRelevant < 0 || pos < firstRelevant) {
			firstRelevant = pos
		}
	}
	if !joined {
		return false, denialReasonNoJoin
	}
	if firstRelevant < 0 {
		return false, denialReasonNoWrite
	}
	for _, c := range calls[firstRelevant+1:] {
		if !fileMutatingTools[c.Name] || denied[c.ID] {
			continue
		}
		if failed, hasResult := isError[c.ID]; !hasResult || !failed {
			return false, denialReasonRecovered
		}
	}
	return true, ""
}
