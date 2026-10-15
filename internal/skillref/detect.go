package skillref

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/handarbeit/fabrik/internal/claudeerr"
	"github.com/handarbeit/fabrik/plugin"
)

// DetectLoadFailure reports whether the invocation's own configured skill
// demonstrably failed to load (#2138, ADR-2138). It reads only the structured
// tool-call record of the run — the stream-json NDJSON — never output prose:
//
//   - an assistant "tool_use" block named "Skill" whose input.skill is the
//     expected skill (SkillNamesMatch) is an own-skill call;
//   - its "tool_result" block, matched by tool_use_id, carries is_error. The
//     result's content text is never inspected for a decision — it only fills
//     Detail for the operator.
//
// It is a failure only when at least one own-skill call failed and none
// succeeded: a failed attempt followed by a working retry means the skill did
// load. No call at all is not a failure (a resumed session may already hold the
// skill in context), nor is a call with no result (truncated stream), an
// unparseable line or a block without a tool_use_id. Sub-agent traffic (a
// non-null parent_tool_use_id) is ignored: the skill must load in the main
// thread, and ignoring it fails toward not-detecting. An empty expectedSkill
// (a stage configured with prompt: and no skill) makes the detector inert.
//
// Returns nil when there is no demonstrated failure.
func DetectLoadFailure(rawOutput []byte, expectedSkill string) *claudeerr.SkillLoadError {
	if expectedSkill == "" || len(rawOutput) == 0 {
		return nil
	}
	type result struct {
		isError bool
		detail  string
	}
	var ownOrder []string          // own-skill tool_use ids, in stream order
	called := map[string]string{}  // id → input.skill as the worker typed it
	results := map[string]result{} // tool_use_id → result
	scanner := bufio.NewScanner(bytes.NewReader(rawOutput))
	// One NDJSON line can never be longer than the whole input.
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
			Type      string          `json:"type"`
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			Input     json.RawMessage `json:"input"`
			ToolUseID string          `json:"tool_use_id"`
			IsError   bool            `json:"is_error"`
			Content   json.RawMessage `json:"content"`
		}
		// message.content is a string for plain user turns; only an array holds blocks.
		if err := json.Unmarshal(env.Message.Content, &blocks); err != nil {
			continue
		}
		for _, b := range blocks {
			switch {
			case env.Type == "assistant" && b.Type == "tool_use" && b.Name == "Skill" && b.ID != "":
				var in struct {
					Skill string `json:"skill"`
				}
				if json.Unmarshal(b.Input, &in) != nil || !SkillNamesMatch(expectedSkill, in.Skill) {
					continue
				}
				ownOrder = append(ownOrder, b.ID)
				called[b.ID] = in.Skill
			case env.Type == "user" && b.Type == "tool_result" && b.ToolUseID != "":
				results[b.ToolUseID] = result{isError: b.IsError, detail: toolResultText(b.Content)}
			}
		}
	}
	var failed *claudeerr.SkillLoadError
	for _, id := range ownOrder {
		r, ok := results[id]
		if !ok {
			continue // no result: a truncated stream is not evidence of failure
		}
		if !r.isError {
			return nil // the skill loaded at least once
		}
		if failed == nil {
			failed = &claudeerr.SkillLoadError{Skill: expectedSkill, ToolUseID: id, Detail: SanitizeDetail(r.detail)}
			if called[id] != expectedSkill {
				failed.Called = called[id]
			}
		}
	}
	return failed
}

// SkillNamesMatch reports whether the name a worker passed to the Skill tool
// (called) refers to the invocation's configured skill. Base names must be
// equal; when both sides are plugin-qualified the plugin parts must be equal
// too, while a bare side matches any plugin (a worker may type the bare name of
// a qualified configuration, and an empty catalogue passes names through
// unqualified). A same-named skill from a different plugin does not match.
func SkillNamesMatch(configured, called string) bool {
	cp, cs, cq := plugin.Split(strings.TrimSpace(configured))
	kp, ks, kq := plugin.Split(strings.TrimSpace(called))
	if cs == "" || cs != ks {
		return false
	}
	if cq && kq {
		return cp == kp
	}
	return true
}

// toolResultText extracts the text of a tool_result content value, which the CLI
// emits either as a string or as an array of {"type":"text","text":…} blocks.
// Unparseable shapes yield "".
func toolResultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var sb strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			if sb.Len() > 0 {
				sb.WriteByte(' ')
			}
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
}

const (
	detailMaxLen  = 200
	detailHeadLen = 140
	detailTailLen = 40
)

// detailReplacer strips the CLI's <tool_use_error> wrapper tags so the text is
// not interpreted as markup when rendered.
var detailReplacer = strings.NewReplacer("<tool_use_error>", "", "</tool_use_error>", "")

// lineReplacer collapses line breaks and neutralises backticks.
var lineReplacer = strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ", "`", "'")

// SanitizeDetail makes the failed call's tool_result text safe for an
// operator-facing comment and warnings-panel entry: it is untrusted model-side
// text, so it is stripped of the CLI's wrapper tags, collapsed to one line with
// backticks neutralised, and truncated in the middle with an explicit
// "chars omitted" marker. The engine's tools-denied sanitiser applies the same
// rule; an engine test pins the two to identical output.
func SanitizeDetail(s string) string {
	s = detailReplacer.Replace(s)
	if s == "" {
		return ""
	}
	s = strings.TrimSpace(lineReplacer.Replace(s))
	if s == "" {
		return ""
	}
	if len(s) > detailMaxLen {
		omitted := len(s) - detailHeadLen - detailTailLen
		headEnd := detailHeadLen
		for headEnd > 0 && headEnd < len(s) && !utf8.RuneStart(s[headEnd]) {
			headEnd--
		}
		tailStart := len(s) - detailTailLen
		for tailStart < len(s) && !utf8.RuneStart(s[tailStart]) {
			tailStart++
		}
		s = fmt.Sprintf("%s … (%d chars omitted) … %s", s[:headEnd], omitted, s[tailStart:])
	}
	return s
}
