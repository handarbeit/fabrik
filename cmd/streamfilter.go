package cmd

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// RunStreamFilter reads stream-json from stdin and prints a human-readable
// summary to stdout. Used inside tmux sessions so operators can observe
// Claude's progress when attaching to the pane.
func RunStreamFilter() {
	scanner := bufio.NewScanner(os.Stdin)
	// Allow up to 10MB per line (conversation messages can be large).
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		var msg struct {
			Type    string `json:"type"`
			Subtype string `json:"subtype"`
			Message *struct {
				Content []struct {
					Type    string `json:"type"`
					Text    string `json:"text"`
					Name    string `json:"name"`
					Input   json.RawMessage `json:"input"`
					Content json.RawMessage `json:"content"`
				} `json:"content"`
			} `json:"message"`
			Result   string  `json:"result"`
			NumTurns int     `json:"num_turns"`
			CostUSD  float64 `json:"total_cost_usd"`
		}
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}

		switch msg.Type {
		case "assistant":
			if msg.Message == nil {
				continue
			}
			for _, block := range msg.Message.Content {
				switch block.Type {
				case "text":
					if block.Text != "" {
						fmt.Print(block.Text)
					}
				case "tool_use":
					fmt.Printf("\n--- tool: %s ---\n", block.Name)
					// Show a compact summary of tool input
					if len(block.Input) > 0 {
						var inputMap map[string]interface{}
						if json.Unmarshal(block.Input, &inputMap) == nil {
							for k, v := range inputMap {
								s := fmt.Sprintf("%v", v)
								if len(s) > 120 {
									s = s[:120] + "..."
								}
								// Collapse newlines for readability
								s = strings.ReplaceAll(s, "\n", "\\n")
								fmt.Printf("  %s: %s\n", k, s)
							}
						}
					}
				case "tool_result":
					// Show truncated tool result
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
}
