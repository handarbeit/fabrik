package mcpstdio

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/handarbeit/fabrik/internal/localapi"
)

type unknownToolError struct{ name string }

func (e *unknownToolError) Error() string { return fmt.Sprintf("unknown tool %q", e.name) }

// Tool names.
const (
	ToolStatus = "fabrik_status"
	ToolBoard  = "fabrik_board"
	ToolHealth = "fabrik_health"
)

func obj(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func toolDefs() []map[string]any {
	readOnly := map[string]any{"readOnlyHint": true, "openWorldHint": false}
	return []map[string]any{
		{
			"name": ToolStatus,
			"description": "Everything the Fabrik daemon knows about one issue, from its in-memory cache (no GitHub call): place on the board and time in column, labels, milestone, " +
				"cycle counters as 'n of max', worker, last invocation, linked PR / CI / merge-train position, blockers, and an `attention` block that answers " +
				"'stuck, or waiting correctly?' (state, reason with a meaning per label, link to the latest Fabrik comment, and what the engine will do next and when). " +
				"Anything the daemon cannot vouch for is the string \"unknown\".",
			"inputSchema": obj(map[string]any{
				"issue": map[string]any{"type": "string", "description": "owner/repo#N, or just N when the daemon manages exactly one repo"},
			}, "issue"),
			"annotations": readOnly,
		},
		{
			"name": ToolBoard,
			"description": "Overseer view of the whole board from the daemon's cache (no GitHub call). view=attention lists only items that need looking at, ranked " +
				"needs-human, escalated, stalled, then settled-at-Validate awaiting a human merge decision (cruise), each with a one-line reason. " +
				"view=flow gives per column the count, the oldest time-in-column and the in-flight workers. Time-in-column is daemon-observed (resets on restart). " +
				"Items the cache cannot evaluate for a filter (e.g. milestone not yet captured) are excluded and counted, never guessed.",
			"inputSchema": obj(map[string]any{
				"view": map[string]any{"type": "string", "enum": []string{localapi.ViewAttention, localapi.ViewFlow}, "description": "attention (default) or flow"},
				"filter": obj(map[string]any{
					"milestone":         map[string]any{"type": "string", "description": "milestone title, or \"none\" for items without one"},
					"label":             map[string]any{"type": "string", "description": "exact label name"},
					"repo":              map[string]any{"type": "string", "description": "owner/repo"},
					"column":            map[string]any{"type": "string", "description": "board Status / column name"},
					"has_open_blockers": map[string]any{"type": "boolean", "description": "only items with an open blocking dependency"},
				}),
				"stall_threshold_minutes": map[string]any{"type": "integer", "minimum": 1, "description": "override how long without progress counts as stalled for this call (default: the daemon's setting; it is echoed in the response)"},
			}),
			"annotations": readOnly,
		},
		{
			"name": ToolHealth,
			"description": "Pipeline health from the daemon's own state (no GitHub call): version and uptime, time since the last poll attempt and success, webhook and reconcile freshness, " +
				"the account-wide Claude usage-limit suspension, GraphQL backoff, worker slots in use, and per (repo, base) merge-train partition members.",
			"inputSchema": obj(map[string]any{}),
			"annotations": readOnly,
		},
	}
}

// buildCall validates a tool call's arguments and maps it to a daemon method
// and params.
func buildCall(name string, args json.RawMessage) (method string, params any, err error) {
	switch name {
	case ToolStatus:
		var a struct {
			Issue string `json:"issue"`
		}
		if err := decodeArgs(args, &a); err != nil {
			return "", nil, err
		}
		if a.Issue == "" {
			return "", nil, fmt.Errorf("issue is required (owner/repo#N, or N when the daemon manages exactly one repo)")
		}
		return localapi.MethodStatus, localapi.StatusParams{Issue: a.Issue}, nil
	case ToolBoard:
		var a struct {
			View    string                `json:"view"`
			Filter  *localapi.BoardFilter `json:"filter"`
			StallMi int64                 `json:"stall_threshold_minutes"`
		}
		if err := decodeArgs(args, &a); err != nil {
			return "", nil, err
		}
		if a.View != "" && a.View != localapi.ViewAttention && a.View != localapi.ViewFlow {
			return "", nil, fmt.Errorf("view must be %q or %q, got %q", localapi.ViewAttention, localapi.ViewFlow, a.View)
		}
		if a.StallMi < 0 {
			return "", nil, fmt.Errorf("stall_threshold_minutes must be positive")
		}
		return localapi.MethodBoard, localapi.BoardParams{View: a.View, Filter: a.Filter, StallThresholdSeconds: a.StallMi * 60}, nil
	case ToolHealth:
		return localapi.MethodHealth, nil, nil
	}
	return "", nil, &unknownToolError{name: name}
}

// decodeArgs strictly decodes tool arguments; empty or null arguments decode to
// the zero value.
func decodeArgs(raw json.RawMessage, into any) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return err
	}
	return nil
}
