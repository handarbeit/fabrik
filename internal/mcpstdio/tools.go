package mcpstdio

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/handarbeit/fabrik/internal/channelevents"
	"github.com/handarbeit/fabrik/internal/localapi"
)

type unknownToolError struct{ name string }

func (e *unknownToolError) Error() string { return fmt.Sprintf("unknown tool %q", e.name) }

// Tool names.
const (
	ToolStatus = "fabrik_status"
	ToolBoard  = "fabrik_board"
	ToolHealth = "fabrik_health"

	ToolSubscribe   = "fabrik_subscribe"
	ToolUnsubscribe = "fabrik_unsubscribe"

	// Overseer actions (#1969, ADR-1966-c): the first tools that change
	// GitHub-visible state. There is deliberately no tool that lifts a pause
	// (removes fabrik:paused / fabrik:awaiting-input) and none that posts a
	// free-form comment: a pause is lifted only by a human comment.
	ToolPromote          = "fabrik_promote"
	ToolSetAutonomy      = "fabrik_set_autonomy"
	ToolRevalidate       = "fabrik_revalidate"
	ToolClearClaudeLimit = "fabrik_clear_claude_limit"
)

// ActionTools lists the tools that make the daemon write to GitHub.
func ActionTools() []string {
	return []string{ToolPromote, ToolSetAutonomy, ToolRevalidate, ToolClearClaudeLimit}
}

// IsActionTool reports whether name is one of ActionTools.
func IsActionTool(name string) bool {
	for _, t := range ActionTools() {
		if t == name {
			return true
		}
	}
	return false
}

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
		{
			"name": ToolSubscribe,
			"description": "Choose which Fabrik events are pushed into this session (Claude Code Channels, research preview) instead of polling. Costs no GitHub call. " +
				"Scopes combine with AND (entries inside one list with OR): issues, repos, a milestone, and label patterns (* matches any run of characters, e.g. fabrik:* or stage:*:complete; " +
				"patterns only affect label-applied/label-removed events). By default label events skip high-churn labels (fabrik:locked:*, stage:*:in_progress, fabrik:spawned-child:*, fabrik:credited-pr:*, fabrik:editing); " +
				"pass exclude_labels: [] to receive every label change. events filters by type: " + eventList() + ". " +
				"digest_seconds (30-3600) batches ordinary events into one message; validate-settled, escalated, paused and daemon-unreachable always arrive at once. " +
				"Events are held (persisted, bounded) while no session with this subscriber name is attached and delivered in order on reconnect. " +
				"Pushes only appear if the session was started with Channels enabled for this server (claude --dangerously-load-development-channels server:fabrik); the daemon cannot tell whether it was, so a successful subscribe does not prove delivery. " +
				"Re-subscribing with the same filters is harmless.",
			"inputSchema": obj(map[string]any{
				"issues":         map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "owner/repo#N, or N when the daemon manages exactly one repo"},
				"repos":          map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "owner/repo"},
				"milestone":      map[string]any{"type": "string", "description": "milestone title, or #N for a milestone number; items whose milestone the daemon has not captured never match"},
				"labels":         map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "include patterns for label events (default: every label)"},
				"exclude_labels": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "exclude patterns for label events; omit for the default churn exclusions, [] for none"},
				"events":         map[string]any{"type": "array", "items": map[string]any{"type": "string", "enum": eventNames()}, "description": "event types to receive (default: all)"},
				"digest_seconds": map[string]any{"type": "integer", "minimum": 30, "maximum": 3600, "description": "batch ordinary events into one message per interval"},
				"subscriber":     map[string]any{"type": "string", "description": "subscriber name; defaults to the name this server was launched with (--subscriber / FABRIK_SUBSCRIBER)"},
			}),
			"annotations": map[string]any{"readOnlyHint": false, "destructiveHint": false, "idempotentHint": true, "openWorldHint": false},
		},
		{
			"name":        ToolUnsubscribe,
			"description": "Remove one subscription by id (from fabrik_subscribe's response), or every subscription of the subscriber when id is omitted. Held events for a subscriber with no subscriptions left are discarded.",
			"inputSchema": obj(map[string]any{
				"id":         map[string]any{"type": "string", "description": "subscription id; omit to remove them all"},
				"subscriber": map[string]any{"type": "string", "description": "subscriber name; defaults to the name this server was launched with"},
			}),
			"annotations": map[string]any{"readOnlyHint": false, "destructiveHint": true, "idempotentHint": true, "openWorldHint": false},
		},
		{
			"name": ToolPromote,
			"description": "Move an item out of an unmanaged parking column (e.g. Backlog) into a pipeline column. The daemon writes the board Status itself, with the ids it already holds (one live read of the item's current column confirms the cache first); it refuses, with the current state and the valid targets, when the item is not in a parking column, `to` is not a pipeline stage (holding, cleanup and unmanaged columns are engine-managed), or the daemon cannot establish a fact. " +
				"If the item carries fabrik:cruise or fabrik:yolo the result says it will auto-advance through the stages from `to`. Posts a short Fabrik audit comment on the issue naming the requesting subscriber. Requires the server to be launched with --subscriber.",
			"inputSchema": obj(map[string]any{
				"issue": map[string]any{"type": "string", "description": "owner/repo#N, or just N when the daemon manages exactly one repo"},
				"to":    map[string]any{"type": "string", "description": "the pipeline column to move the item into, e.g. Specify"},
			}, "issue", "to"),
			"annotations": action(false),
		},
		{
			"name": ToolSetAutonomy,
			"description": "Make an item's autonomy exactly the requested mode: cruise (auto-advance every stage but never auto-merge), yolo (auto-advance AND auto-merge the PR when Validate completes) or none. " +
				"Setting one mode removes the other label, so the outcome is exactly what was asked (cruise would otherwise silently beat yolo). Already in the requested mode: nothing is written and no comment is posted. " +
				"yolo can cause a merge: prefer cruise unless the item should land unattended. Posts a Fabrik audit comment naming the requesting subscriber. Requires --subscriber.",
			"inputSchema": obj(map[string]any{
				"issue":    map[string]any{"type": "string", "description": "owner/repo#N, or just N when the daemon manages exactly one repo"},
				"autonomy": map[string]any{"type": "string", "enum": []string{"cruise", "yolo", "none"}, "description": "the autonomy the item should end up with"},
			}, "issue", "autonomy"),
			"annotations": action(true),
		},
		{
			"name": ToolRevalidate,
			"description": "Force a Validate re-run: applies fabrik:revalidate to an item sitting in the Validate column, and the engine clears the Validate completion and CI labels and re-dispatches on a following poll (deferred until an in-flight Validate worker exits). " +
				"Refused, with the current state, when the item is not in Validate, already carries the label, or is paused or awaiting input: a pause is lifted only by a human comment on the issue, never by this tool. Posts a Fabrik audit comment naming the requesting subscriber. Requires --subscriber.",
			"inputSchema": obj(map[string]any{
				"issue": map[string]any{"type": "string", "description": "owner/repo#N, or just N when the daemon manages exactly one repo"},
			}, "issue"),
			"annotations": action(false),
		},
		{
			"name": ToolClearClaudeLimit,
			"description": "Clear an active account-wide Claude usage-limit suspension without restarting the daemon, by applying fabrik:clear-claude-limit to one managed open issue (the daemon picks it: one already carrying fabrik:claude-limit first, else the lowest-numbered, and names it in the result and its audit comment). " +
				"Refused when no suspension is active. The suspension is account-wide, so the chosen issue is only the carrier of the label. Posts a Fabrik audit comment on that issue naming the requesting subscriber. Requires --subscriber.",
			"inputSchema": obj(map[string]any{}),
			"annotations": action(true),
		},
	}
}

// action is the MCP annotation block of a tool that writes to GitHub through
// the daemon: never read-only, never destructive, open-world (GitHub).
func action(idempotent bool) map[string]any {
	return map[string]any{"readOnlyHint": false, "destructiveHint": false, "idempotentHint": idempotent, "openWorldHint": true}
}

func eventNames() []string {
	var out []string
	for _, t := range channelevents.Subscribable() {
		out = append(out, string(t))
	}
	return out
}

func eventList() string { return strings.Join(eventNames(), ", ") }

// buildCall validates a tool call's arguments and maps it to a daemon method
// and params.
func buildCall(name string, args json.RawMessage, defaultSubscriber string) (method string, params any, err error) {
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
	case ToolSubscribe:
		var a struct {
			Issues        []string  `json:"issues"`
			Repos         []string  `json:"repos"`
			Milestone     string    `json:"milestone"`
			Labels        []string  `json:"labels"`
			ExcludeLabels *[]string `json:"exclude_labels"`
			Events        []string  `json:"events"`
			DigestSeconds int       `json:"digest_seconds"`
			Subscriber    string    `json:"subscriber"`
		}
		if err := decodeArgs(args, &a); err != nil {
			return "", nil, err
		}
		who, err := subscriberName(a.Subscriber, defaultSubscriber)
		if err != nil {
			return "", nil, err
		}
		if a.DigestSeconds < 0 {
			return "", nil, fmt.Errorf("digest_seconds must be positive")
		}
		return localapi.MethodSubscribe, localapi.SubscribeParams{
			Subscriber: who, Repos: a.Repos, Issues: a.Issues, Milestone: a.Milestone,
			Labels: a.Labels, ExcludeLabels: a.ExcludeLabels, Events: a.Events, DigestSeconds: a.DigestSeconds,
		}, nil
	case ToolUnsubscribe:
		var a struct {
			ID         string `json:"id"`
			Subscriber string `json:"subscriber"`
		}
		if err := decodeArgs(args, &a); err != nil {
			return "", nil, err
		}
		who, err := subscriberName(a.Subscriber, defaultSubscriber)
		if err != nil {
			return "", nil, err
		}
		return localapi.MethodUnsubscribe, localapi.UnsubscribeParams{Subscriber: who, ID: a.ID}, nil
	case ToolPromote:
		var a struct {
			Issue string `json:"issue"`
			To    string `json:"to"`
		}
		if err := decodeArgs(args, &a); err != nil {
			return "", nil, err
		}
		who, err := requesterName(defaultSubscriber)
		if err != nil {
			return "", nil, err
		}
		if a.Issue == "" || a.To == "" {
			return "", nil, fmt.Errorf("issue and to are required")
		}
		return localapi.MethodPromote, localapi.PromoteParams{Subscriber: who, Issue: a.Issue, To: a.To}, nil
	case ToolSetAutonomy:
		var a struct {
			Issue    string `json:"issue"`
			Autonomy string `json:"autonomy"`
		}
		if err := decodeArgs(args, &a); err != nil {
			return "", nil, err
		}
		who, err := requesterName(defaultSubscriber)
		if err != nil {
			return "", nil, err
		}
		if a.Issue == "" {
			return "", nil, fmt.Errorf("issue is required")
		}
		switch a.Autonomy {
		case "cruise", "yolo", "none":
		default:
			return "", nil, fmt.Errorf("autonomy must be one of cruise, yolo, none (got %q)", a.Autonomy)
		}
		return localapi.MethodSetAutonomy, localapi.SetAutonomyParams{Subscriber: who, Issue: a.Issue, Mode: a.Autonomy}, nil
	case ToolRevalidate:
		var a struct {
			Issue string `json:"issue"`
		}
		if err := decodeArgs(args, &a); err != nil {
			return "", nil, err
		}
		who, err := requesterName(defaultSubscriber)
		if err != nil {
			return "", nil, err
		}
		if a.Issue == "" {
			return "", nil, fmt.Errorf("issue is required")
		}
		return localapi.MethodRevalidate, localapi.RevalidateParams{Subscriber: who, Issue: a.Issue}, nil
	case ToolClearClaudeLimit:
		var a struct{}
		if err := decodeArgs(args, &a); err != nil {
			return "", nil, err
		}
		who, err := requesterName(defaultSubscriber)
		if err != nil {
			return "", nil, err
		}
		return localapi.MethodClearClaudeLimit, localapi.ClearClaudeLimitParams{Subscriber: who}, nil
	}
	return "", nil, &unknownToolError{name: name}
}

// subscriberName resolves the subscriber a subscribe/unsubscribe call acts for:
// the explicit argument, else the name this shim was launched with. There is no
// generated or PID-derived fallback — the name keys the held queue and must
// survive the session, so a call with neither is refused with the fix.
func subscriberName(explicit, launched string) (string, error) {
	switch {
	case explicit != "":
		return explicit, nil
	case launched != "":
		return launched, nil
	}
	return "", fmt.Errorf("no subscriber name: launch this server with --subscriber <name> (or FABRIK_SUBSCRIBER), using a stable name such as your topic or session name, or pass subscriber in the call")
}

// requesterName is the subscriber an action is attributed to: only the name this
// server was launched with. Actions take no per-call override — the audit
// comment names who asked, so a call cannot choose another name — and are
// refused when the server has none.
func requesterName(launched string) (string, error) {
	if launched == "" {
		return "", fmt.Errorf("this action is refused: it posts an audit comment naming who requested it, and this server was launched without a subscriber name — launch it with --subscriber <name> (or FABRIK_SUBSCRIBER), using a stable name such as your topic or session name")
	}
	return launched, nil
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
