package cmd

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// streamMessage is the structure we extract from each JSON message.
type streamMessage struct {
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

// RunStreamFilter reads stream-json (NDJSON) or a JSON array from stdin
// and prints a human-readable summary to stdout.
func RunStreamFilter() {
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return
	}
	data = trimBOM(data)
	if len(data) == 0 {
		return
	}

	// Try JSON array first (output from --output-format json)
	if data[0] == '[' {
		var messages []streamMessage
		if err := json.Unmarshal(data, &messages); err == nil {
			for _, msg := range messages {
				renderMessage(msg)
			}
			return
		}
	}

	// Fall back to NDJSON (one object per line from --output-format stream-json)
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		var msg streamMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}
		renderMessage(msg)
	}
}

func trimBOM(data []byte) []byte {
	if len(data) >= 3 && data[0] == 0xEF && data[1] == 0xBB && data[2] == 0xBF {
		return data[3:]
	}
	return data
}

func renderMessage(msg streamMessage) {
	switch msg.Type {
	case "assistant":
		if msg.Message == nil {
			return
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
					fmt.Printf("\n💭 %s\n", thinking)
				}
			case "text":
				if block.Text != "" {
					fmt.Printf("\n📝 %s\n", block.Text)
				}
			case "tool_use":
				fmt.Printf("\n--- tool: %s ---\n", block.Name)
				if len(block.Input) > 0 {
					var inputMap map[string]interface{}
					if json.Unmarshal(block.Input, &inputMap) == nil {
						for k, v := range inputMap {
							s := fmt.Sprintf("%v", v)
							if len(s) > 120 {
								s = s[:120] + "..."
							}
							s = strings.ReplaceAll(s, "\n", "\\n")
							fmt.Printf("  %s: %s\n", k, s)
						}
					}
				}
			case "tool_result":
				var content string
				if len(block.Content) > 0 {
					json.Unmarshal(block.Content, &content)
				}
				if content != "" {
					if len(content) > 200 {
						content = content[:200] + "..."
					}
					content = strings.ReplaceAll(content, "\n", "\\n")
					fmt.Printf("  → %s\n", content)
				}
			}
		}
	case "result":
		fmt.Printf("\n\n=== Done: %d turns, $%.4f ===\n", msg.NumTurns, msg.CostUSD)
	}
}
