// Package streamfilter parses Claude's stream-json (NDJSON) or JSON array output
// and converts it to human-readable text. It is used by cmd/stream-filter and by
// watch/logfollow for live rendering of log files.
package streamfilter

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Message is the structure we extract from each JSON event.
type Message struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	Message *struct {
		Content []struct {
			Type     string          `json:"type"`
			Text     string          `json:"text"`
			Thinking string          `json:"thinking"`
			Name     string          `json:"name"`
			Input    json.RawMessage `json:"input"`
			Content  json.RawMessage `json:"content"`
		} `json:"content"`
	} `json:"message"`
	Result   string  `json:"result"`
	NumTurns int     `json:"num_turns"`
	CostUSD  float64 `json:"total_cost_usd"`
}

// TrimBOM removes a leading UTF-8 BOM if present.
func TrimBOM(data []byte) []byte {
	if len(data) >= 3 && data[0] == 0xEF && data[1] == 0xBB && data[2] == 0xBF {
		return data[3:]
	}
	return data
}

// RenderLine parses a single NDJSON line and returns a human-readable string.
// Returns an empty string if the line is not a renderable message type.
func RenderLine(line []byte) string {
	var msg Message
	if err := json.Unmarshal(line, &msg); err != nil {
		return ""
	}
	return RenderMessage(msg)
}

// RenderMessage converts a parsed Message to a human-readable string.
// Returns an empty string for non-renderable message types.
func RenderMessage(msg Message) string {
	var sb strings.Builder
	switch msg.Type {
	case "assistant":
		if msg.Message == nil {
			return ""
		}
		for _, block := range msg.Message.Content {
			switch block.Type {
			case "thinking":
				thinking := block.Thinking
				if thinking == "" {
					thinking = block.Text
				}
				if thinking != "" {
					if len(thinking) > 500 {
						thinking = thinking[:500] + "..."
					}
					fmt.Fprintf(&sb, "\n💭 %s\n", thinking)
				}
			case "text":
				if block.Text != "" {
					fmt.Fprintf(&sb, "\n📝 %s\n", block.Text)
				}
			case "tool_use":
				fmt.Fprintf(&sb, "\n--- tool: %s ---\n", block.Name)
				if len(block.Input) > 0 {
					var inputMap map[string]interface{}
					if json.Unmarshal(block.Input, &inputMap) == nil {
						for k, v := range inputMap {
							s := fmt.Sprintf("%v", v)
							if len(s) > 120 {
								s = s[:120] + "..."
							}
							s = strings.ReplaceAll(s, "\n", "\\n")
							fmt.Fprintf(&sb, "  %s: %s\n", k, s)
						}
					}
				}
			case "tool_result":
				var content string
				if len(block.Content) > 0 {
					json.Unmarshal(block.Content, &content) //nolint:errcheck
				}
				if content != "" {
					if len(content) > 200 {
						content = content[:200] + "..."
					}
					content = strings.ReplaceAll(content, "\n", "\\n")
					fmt.Fprintf(&sb, "  → %s\n", content)
				}
			}
		}
	case "result":
		fmt.Fprintf(&sb, "\n\n=== Done: %d turns, $%.4f ===\n", msg.NumTurns, msg.CostUSD)
	default:
		return ""
	}
	return sb.String()
}

// RunFilter reads stream-json (NDJSON) or a JSON array from r and writes
// human-readable output to w.
func RunFilter(r io.Reader, w io.Writer) {
	data, err := io.ReadAll(r)
	if err != nil {
		return
	}
	data = TrimBOM(data)
	if len(data) == 0 {
		return
	}

	// Try JSON array first (output from --output-format json)
	if data[0] == '[' {
		var messages []Message
		if err := json.Unmarshal(data, &messages); err == nil {
			for _, msg := range messages {
				if s := RenderMessage(msg); s != "" {
					fmt.Fprint(w, s)
				}
			}
			return
		}
	}

	// Fall back to NDJSON (one object per line from --output-format stream-json)
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	for scanner.Scan() {
		if s := RenderLine(scanner.Bytes()); s != "" {
			fmt.Fprint(w, s)
		}
	}
}
