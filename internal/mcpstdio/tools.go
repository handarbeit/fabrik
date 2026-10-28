package mcpstdio

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
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
	// ToolSituation (#2171, ADR 2171) is the rolled-up "what needs you" view.
	ToolSituation = "fabrik_situation"

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
	ToolAdvance          = "fabrik_advance"
	ToolMoveParked       = "fabrik_move_parked"
	ToolSetBlockedBy     = "fabrik_set_blocked_by"
)

// ActionTools lists the tools that make the daemon write to GitHub.
func ActionTools() []string {
	return []string{ToolPromote, ToolSetAutonomy, ToolRevalidate, ToolClearClaudeLimit, ToolAdvance, ToolMoveParked, ToolSetBlockedBy}
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

// issueRefHelp states which issue references the daemon accepts.
const issueRefHelp = "owner/repo#N, or just N, #N or a number when every issue the daemon has cached is in one repo (with several repos a bare number is ambiguous)"

// issueSchema is the input schema of an issue reference: a string
// (owner/repo#N, N or #N) or a positive integer (the same as the bare N).
func issueSchema() map[string]any {
	return map[string]any{"type": []string{"string", "integer"}, "minimum": 1, "description": issueRefHelp}
}

// issueArg is an issue reference as decoded from tool arguments: a JSON string
// is kept verbatim for the daemon's parser, and a JSON number must be a plain
// positive integer and becomes its decimal string (the bare-N form). null is the
// empty reference, which the callers reject as missing.
type issueArg string

func (a *issueArg) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	switch {
	case string(b) == "null":
		*a = ""
		return nil
	case len(b) > 0 && b[0] == '"':
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*a = issueArg(s)
		return nil
	}
	for _, c := range b {
		if c < '0' || c > '9' {
			return fmt.Errorf("issue number must be a positive integer (got %s)", b)
		}
	}
	n, err := strconv.Atoi(string(b))
	if len(b) == 0 || err != nil || n <= 0 {
		return fmt.Errorf("issue number must be a positive integer (got %s)", b)
	}
	*a = issueArg(strconv.Itoa(n))
	return nil
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
			"description": "Everything the Fabrik daemon knows about one issue, from its in-memory cache (no GitHub call): place on the board and time in column, every label (stage and fabrik groupings plus other_labels), milestone, " +
				"cycle counters as 'n of max' (none for a parking or holding column), worker, last invocation (figure_basis says which figures are per-invocation and which are session-cumulative, as the Claude CLI reports them), " +
				"linked PR / CI / merge-train position with a per-reviewer verdict at the PR head (at_head, inline_at_head, outstanding, clean_pass, unaddressed), blockers, and an `attention` block that answers " +
				"'stuck, or waiting correctly?' (state, reason with a meaning per label, link to the latest Fabrik comment, and what the engine will do next and when; a suspended item names the decision and its options instead, and no next time). " +
				"Anything the daemon cannot vouch for is the string \"unknown\".",
			"inputSchema": obj(map[string]any{
				"issue": issueSchema(),
			}, "issue"),
			"annotations": readOnly,
		},
		{
			"name": ToolBoard,
			"description": "Overseer view of the whole board from the daemon's cache (no GitHub call). view=attention lists only items that need looking at, ranked " +
				"needs-human and suspended (a completed stage or parked blocker waiting on a human decision, with the options to act and a GitHub-anchored age), escalated, stalled, then settled-at-Validate awaiting a human merge decision (cruise), each with a one-line reason. " +
				"view=flow gives per column the count, the oldest time-in-column and the in-flight workers; closed (Done) items are not listed but counted in excluded.closed, so the columns plus excluded.closed equal matched. " +
				"Every result carries matched, the number of items that passed the filter; an attention view always carries `attention` (an empty list when none of the matched items needs looking at). Time-in-column is daemon-observed (resets on restart). " +
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
			"description": "Pipeline health from the daemon's own state (no GitHub call): version and uptime, time since the last poll attempt and success, webhook and reconcile freshness (webhook.transports lists each event transport's own state, last delivery time and rejected-signature, malformed and oversized request counts; unknown values are \"unknown\"), " +
				"poll liveness (poll.in_progress and current_started_at, last_completed_at, the step an in-progress poll is in, next_due_at with backing_off, and the overdue verdict: true when a poll has run longer than 2 x interval_seconds or none started within 2 x interval of next_due_at, so a stalled engine is distinguishable from a stalled item), " +
				"the account-wide Claude usage-limit hold, GraphQL backoff (as_of says when a poll last refreshed it; it does not age on its own), worker slots in use (plus the reserved merge-train slot, train_in_use/train_max, when merge_train is on; and the reserved fix-round slots, fix_in_use/fix_max, when fix_round_slots is set), " +
				"and per (repo, base) merge-train partition members with oldest_queued_since, the run phase, and an idle_reason when members are queued and no run is active. " +
				"While the daemon drains for a SIGHUP restart a draining object (since, workers_remaining) is present and the poll overdue verdict is unknown, not overdue.",
			"inputSchema": obj(map[string]any{}),
			"annotations": readOnly,
		},
		{
			"name": ToolSituation,
			"description": "The one answer to 'what needs me?', from the daemon's in-memory state (no GitHub call): PRs settled at Validate under cruise awaiting a merge decision (with the review and CI verdicts as cached), " +
				"items held at the feedback / review / CI / merge gate (the gate and how long it has held), items waiting for human input (with how to resume them), pending decisions, stalled items, " +
				"merge-train state per (repo, base) partition, poll and worker health (in progress or overdue, slots in use, train members queued), and active anomalies (pipeline-stalled, retry-starved, item-wedged, cache-divergence). " +
				"Every item entry carries its title, link and the options to act, each with the MCP tool that performs it when one exists. Each section is ordered by attention rank and bounded: `omitted` counts the entries cut, and an item appears in one section only (an item already reported as item-wedged is under anomalies, not gates). " +
				"A fact the daemon cannot vouch for (a hold that began before the daemon started, labels it has not read yet) is \"unknown\", never a default; `nothing_needs_you` is true only when there is nothing to report and every open item could be classified. " +
				"The same view is what a `situation` event carries and what the catch-up on attach delivers. With push: true the view is also queued to this session's channel as a `situation` event, " +
				"unless it equals the last one sent to this subscriber (force: true sends it anyway); that writes nothing to GitHub or the board and needs this server to have a subscriber name (--subscriber, FABRIK_SUBSCRIBER or CLAUDE_TOPIC; the OS-username default cannot receive pushes).",
			"inputSchema": obj(map[string]any{
				"push":  map[string]any{"type": "boolean", "description": "also queue this view to this session as a situation event (deduplicated unless force)"},
				"force": map[string]any{"type": "boolean", "description": "with push, send even when the view equals the last digest sent to this subscriber"},
			}),
			"annotations": readOnly,
		},
		{
			"name": ToolSubscribe,
			"description": "Choose which Fabrik events are pushed into this session (Claude Code Channels, research preview) instead of polling. Costs no GitHub call. " +
				"Scopes combine with AND (entries inside one list with OR): issues, repos, a milestone, and label patterns (* matches any run of characters, e.g. fabrik:* or stage:*:complete; " +
				"patterns only affect label-applied/label-removed events). By default label events skip high-churn labels (fabrik:locked:*, stage:*:in_progress, fabrik:spawned-child:*, fabrik:credited-pr:*, fabrik:editing); " +
				"pass exclude_labels: [] to receive every label change. item-added (a new item arrived), column-changed (a board column move) and closed are lifecycle events; progress_via_column replaces the stage-label stream with column moves. events filters by type: " + eventList() + ". " +
				"digest_seconds (30-3600) batches ordinary events into one message; validate-settled, settlement-withdrawn, escalated, paused and daemon-unreachable always arrive at once. " +
				"situation_seconds (60 to 86400; default off) pushes the situation digest - the same rolled-up view fabrik_situation returns - at most that often, and only when it changed since the last one sent to this session; it needs situation in events (or no events list). " +
				"remind_after (seconds, 600 to 2592000; default off) re-sends a still-pending decision-pending event to this session once per interval, marked reminder=true and carrying the original's attributes; it is never sent from daemon uptime and never as `stalled`. " +
				"Without an events list you receive the semantic event family: every type below except the raw label-applied / label-removed events. Label events are opt-in - name label-applied and/or label-removed in events to receive them (labels, exclude_labels and progress_via_column only shape label events, so they do nothing unless you do; the response notes it). " +
				"On subscribing, on a fresh attach and after a daemon restart you receive one catch_up snapshot of what needs you: the situation digest if this subscription takes situation (the default family does), otherwise the per-item events it matches. After a restart you also receive daemon-reachable first and then the changes made while it was down (origin=downtime-delta) - never a replay of unchanged state. " +
				"Label events are not a readiness signal: a label such as stage:Validate:complete can appear and be removed again while the PR still needs a rebase, CI or a fix. " +
				"Only validate-settled (the item is ready for your decision) and settlement-withdrawn (it stopped being ready before you acted) say whether an item is ready; act on decision-pending/validate-settled events using their `options` and do not act on a settled event that a settlement-withdrawn event has since withdrawn. " +
				"Events are held (persisted, bounded) while no session with this subscriber name is attached and delivered in order on reconnect. " +
				"Pushes only appear if the session was started with Channels enabled for this server (claude --dangerously-load-development-channels server:fabrik, or plugin:fabrik@fabrik when installed through the fabrik plugin); the daemon cannot tell whether it was, so a successful subscribe does not prove delivery. " +
				"Re-subscribing with the same filters is harmless. " +
				"The daemon tolerates version skew: an event type it renamed since this server was built is mapped to its new name, and one it does not know is dropped; the response lists both under mapped and rejected, " +
				"and a request that names only unknown types is refused without touching the existing subscription.",
			"inputSchema": obj(map[string]any{
				"issues":              map[string]any{"type": "array", "items": issueSchema(), "description": "issue references: " + issueRefHelp},
				"repos":               map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "owner/repo"},
				"milestone":           map[string]any{"type": "string", "description": "milestone title, or #N for a milestone number; items whose milestone the daemon has not captured never match"},
				"labels":              map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "include patterns for label events (default: every label)"},
				"exclude_labels":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "exclude patterns for label events; omit for the default churn exclusions, [] for none"},
				"events":              map[string]any{"type": "array", "items": map[string]any{"type": "string", "enum": eventNames()}, "description": "event types to receive (default: the semantic family - every type except label-applied and label-removed; name those to opt in)"},
				"digest_seconds":      map[string]any{"type": "integer", "minimum": 30, "maximum": 3600, "description": "batch ordinary events into one message per interval"},
				"progress_via_column": map[string]any{"type": "boolean", "description": "drop label-applied/label-removed for stage:*:complete and stage:*:in_progress when column-changed is also taken, so progress arrives as column moves instead of the stage-label stream; overrides labels and exclude_labels for those two patterns; requires column-changed in events when events is set"},
				"subscriber":          map[string]any{"type": "string", "description": "subscriber name; defaults to this server's resolved name (--subscriber, else FABRIK_SUBSCRIBER, else CLAUDE_TOPIC, else the OS username)"},
				"remind_after":        map[string]any{"type": "integer", "minimum": 600, "maximum": 2592000, "description": "seconds between reminders for a still-pending decision-pending episode; omit for none"},
				"situation_seconds":   map[string]any{"type": "integer", "minimum": 60, "maximum": 86400, "description": "push the situation digest at most this often, only when it changed since the last one sent; omit for none (the catch-up and fabrik_situation push still deliver one)"},
			}),
			"annotations": map[string]any{"readOnlyHint": false, "destructiveHint": false, "idempotentHint": true, "openWorldHint": false},
		},
		{
			"name": ToolUnsubscribe,
			"description": "Remove one subscription by id (from fabrik_subscribe's response), or every subscription of the subscriber when id is omitted. Held events that no remaining subscription matches are discarded (all of them when none is left). " +
				"A hard stop on the daemon's side: once this returns it delivers nothing more for the removed subscription, but events it had already written to this session before then may still surface. The push connection stays attached, so subscribing again resumes delivery.",
			"inputSchema": obj(map[string]any{
				"id":         map[string]any{"type": "string", "description": "subscription id; omit to remove them all"},
				"subscriber": map[string]any{"type": "string", "description": "subscriber name; defaults to the name this server was launched with"},
			}),
			"annotations": map[string]any{"readOnlyHint": false, "destructiveHint": true, "idempotentHint": true, "openWorldHint": false},
		},
		{
			"name": ToolPromote,
			"description": "Move an item out of an unmanaged parking column (e.g. Backlog) into a pipeline column. The daemon writes the board Status itself, with the ids it already holds (one live read of the item's current column confirms the cache first); it refuses, with the current state, when the item is not in a parking column, `to` is not a pipeline stage (holding, cleanup and unmanaged columns are engine-managed), or the daemon cannot establish a fact; the refusal lists the valid targets only when the item is in a parking column. An item already in a pipeline column is refused with a pointer to fabrik_advance, which moves a completed stage one column forward. A legacy Backlog column with no configured stage counts as a parking column. " +
				"If the item carries fabrik:cruise or fabrik:yolo the result says it will auto-advance through the stages from `to`. Posts a short Fabrik audit comment on the issue naming the requesting subscriber. Needs a subscriber name (--subscriber, else FABRIK_SUBSCRIBER, CLAUDE_TOPIC or the OS username).",
			"inputSchema": obj(map[string]any{
				"issue": issueSchema(),
				"to":    map[string]any{"type": "string", "description": "the pipeline column to move the item into, e.g. Specify"},
			}, "issue", "to"),
			"annotations": action(false),
		},
		{
			"name": ToolSetAutonomy,
			"description": "Make an item's autonomy exactly the requested mode: cruise (auto-advance every stage but never auto-merge), yolo (auto-advance AND auto-merge the PR when Validate completes) or none. " +
				"Setting one mode removes the other label, so the outcome is exactly what was asked (cruise would otherwise silently beat yolo). Already in the requested mode: nothing is written and no comment is posted. " +
				"yolo can cause a merge: prefer cruise unless the item should land unattended. Posts a Fabrik audit comment naming the requesting subscriber. Needs a subscriber name (defaults to the OS username).",
			"inputSchema": obj(map[string]any{
				"issue":    issueSchema(),
				"autonomy": map[string]any{"type": "string", "enum": []string{"cruise", "yolo", "none"}, "description": "the autonomy the item should end up with"},
			}, "issue", "autonomy"),
			"annotations": action(true),
		},
		{
			"name": ToolRevalidate,
			"description": "Force a Validate re-run: applies fabrik:revalidate to an item sitting in the Validate column, and the engine clears the Validate completion and CI labels and re-dispatches on a following poll (deferred until an in-flight Validate worker exits). " +
				"Refused, with the current state, when the item is not in Validate, already carries the label, or is paused or awaiting input: a pause is lifted only by a human comment on the issue, never by this tool. Posts a Fabrik audit comment naming the requesting subscriber. Needs a subscriber name (defaults to the OS username).",
			"inputSchema": obj(map[string]any{
				"issue": issueSchema(),
			}, "issue"),
			"annotations": action(false),
		},
		{
			"name": ToolAdvance,
			"description": "Move an item whose current stage is complete (stage:<X>:complete) exactly one configured stage forward, using the engine's own advance (the one auto-advance uses); the engine then dispatches the next stage on its next poll. " +
				"Use it for the decision \"this finished stage should move on\" without changing the item's autonomy (cruise/yolo apply to every remaining stage). The daemon confirms the item's column and labels live and fails closed. " +
				"Refused, with the current state and the specific reason, when a worker is running, the stage is not complete, the item is paused/awaiting input (a human comment resumes it; this tool never lifts a pause), blocked, or held by a CI wait, review wait, unresolved dependency or unprocessed feedback the engine would also apply. " +
				"Validate -> Done and moves into a holding stage such as Queued are refused (use the PR merge); a parking column is refused (use fabrik_promote). Posts a Fabrik audit comment naming the requesting subscriber. Needs a subscriber name (defaults to the OS username).",
			"inputSchema": obj(map[string]any{
				"issue": issueSchema(),
			}, "issue"),
			"annotations": action(false),
		},
		{
			"name": ToolMoveParked,
			"description": "Move an item from one unmanaged parking column (e.g. Triage) to another (e.g. Backlog). This starts no work: the engine never dispatches for a parking column, so it is the \"accepted, not yet\" decision, not a promotion; use fabrik_promote to move an item into the pipeline. " +
				"It only moves BETWEEN parking columns and never pulls work out of the pipeline. The daemon confirms the item's current column live and fails closed. " +
				"Refused, with the current state and writing nothing, when the item is not in a parking column, `to` is not another parking column that exists on the board (holding, cleanup and pipeline columns are refused; the refusal lists the valid parking destinations), or `to` is the item's current column. " +
				"A legacy Backlog column with no configured stage counts as a parking column. Changes no labels and lifts no pause. Posts a Fabrik audit comment naming the requesting subscriber. Needs a subscriber name (defaults to the OS username).",
			"inputSchema": obj(map[string]any{
				"issue": issueSchema(),
				"to":    map[string]any{"type": "string", "description": "the parking column to move the item into, e.g. Backlog"},
			}, "issue", "to"),
			"annotations": action(false),
		},
		{
			"name": ToolSetBlockedBy,
			"description": "Order work: add and/or remove blockedBy edges on one issue (the target), so it waits on other issues. Set the edges BEFORE giving the target an autonomy label or moving it into a pipeline column, or Fabrik may run ahead of the intended order. " +
				"`add` and `remove` list blockers as owner/repo#N, or a bare N meaning the TARGET's repo; blockers may be in another repo and may already be closed. " +
				"All guards are checked for every edge before anything is written, and each edge is reported individually as applied or noop (an edge already present, or absent from the daemon's cached edges, is a no-op, not an error). " +
				"Refused, with the current state and writing nothing, when the target is closed, a pull request or not on the board, a blocker does not resolve to an issue (a PR is refused), an issue would block itself, an add would close a cycle among the cached edges, a cycle check would need an edge the daemon does not know (the refusal names the issue), a blocker is in both lists, or both lists are empty. " +
				"It starts no work, moves no column and never adds or removes fabrik:blocked: the engine's own dependency check decides that, so an item that loses its last blocker is released on a following poll. Posts one Fabrik audit comment naming the requesting subscriber and the edges changed (none when nothing was written). Needs a subscriber name (defaults to the OS username).",
			"inputSchema": obj(map[string]any{
				"issue":  issueSchema(),
				"add":    map[string]any{"type": "array", "items": issueSchema(), "description": "blockers to add: owner/repo#N, or a bare N meaning the target's repo"},
				"remove": map[string]any{"type": "array", "items": issueSchema(), "description": "blockers to remove, in the same form"},
			}, "issue"),
			"annotations": action(true),
		},
		{
			"name": ToolClearClaudeLimit,
			"description": "Clear an active account-wide Claude usage-limit hold without restarting the daemon, by applying fabrik:clear-claude-limit to one managed open issue (the daemon picks it: one already carrying fabrik:claude-limit first, else the lowest-numbered, and names it in the result and its audit comment). " +
				"Refused when no usage limit is active. The hold is account-wide, so the chosen issue is only the carrier of the label. Posts a Fabrik audit comment on that issue naming the requesting subscriber. Needs a subscriber name (defaults to the OS username).",
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
			Issue issueArg `json:"issue"`
		}
		if err := decodeArgs(args, &a); err != nil {
			return "", nil, err
		}
		if a.Issue == "" {
			return "", nil, fmt.Errorf("issue is required (%s)", issueRefHelp)
		}
		return localapi.MethodStatus, localapi.StatusParams{Issue: string(a.Issue)}, nil
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
	case ToolSituation:
		var a struct {
			Push  bool `json:"push"`
			Force bool `json:"force"`
		}
		if err := decodeArgs(args, &a); err != nil {
			return "", nil, err
		}
		p := localapi.SituationParams{Push: a.Push, Force: a.Force}
		if a.Push {
			// A push goes to this session's own channel only: the launched
			// --subscriber name, with no per-call override (as for the action
			// tools), so a call cannot queue a digest into, or overwrite the dedup
			// memory of, another subscriber. Without a name the daemon still
			// answers and says nothing was pushed.
			p.Subscriber = defaultSubscriber
		}
		return localapi.MethodSituation, p, nil
	case ToolSubscribe:
		var a struct {
			Issues        []issueArg `json:"issues"`
			Repos         []string   `json:"repos"`
			Milestone     string     `json:"milestone"`
			Labels        []string   `json:"labels"`
			ExcludeLabels *[]string  `json:"exclude_labels"`
			Events        []string   `json:"events"`
			DigestSeconds int        `json:"digest_seconds"`
			RemindAfter   int        `json:"remind_after"`
			Situation     int        `json:"situation_seconds"`
			Subscriber    string     `json:"subscriber"`

			ProgressViaColumn bool `json:"progress_via_column"`
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
		if a.RemindAfter < 0 {
			return "", nil, fmt.Errorf("remind_after must be positive")
		}
		if a.Situation < 0 {
			return "", nil, fmt.Errorf("situation_seconds must be positive")
		}
		var issues []string
		for _, i := range a.Issues {
			issues = append(issues, string(i))
		}
		return localapi.MethodSubscribe, localapi.SubscribeParams{
			Subscriber: who, Repos: a.Repos, Issues: issues, Milestone: a.Milestone,
			Labels: a.Labels, ExcludeLabels: a.ExcludeLabels, Events: a.Events, DigestSeconds: a.DigestSeconds,
			ProgressViaColumn:  a.ProgressViaColumn,
			RemindAfterSeconds: a.RemindAfter,
			SituationSeconds:   a.Situation,
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
			Issue issueArg `json:"issue"`
			To    string   `json:"to"`
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
		return localapi.MethodPromote, localapi.PromoteParams{Subscriber: who, Issue: string(a.Issue), To: a.To}, nil
	case ToolSetAutonomy:
		var a struct {
			Issue    issueArg `json:"issue"`
			Autonomy string   `json:"autonomy"`
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
		return localapi.MethodSetAutonomy, localapi.SetAutonomyParams{Subscriber: who, Issue: string(a.Issue), Mode: a.Autonomy}, nil
	case ToolRevalidate:
		var a struct {
			Issue issueArg `json:"issue"`
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
		return localapi.MethodRevalidate, localapi.RevalidateParams{Subscriber: who, Issue: string(a.Issue)}, nil
	case ToolMoveParked:
		var a struct {
			Issue issueArg `json:"issue"`
			To    string   `json:"to"`
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
		return localapi.MethodMoveParked, localapi.MoveParkedParams{Subscriber: who, Issue: string(a.Issue), To: a.To}, nil
	case ToolSetBlockedBy:
		var a struct {
			Issue  issueArg   `json:"issue"`
			Add    []issueArg `json:"add"`
			Remove []issueArg `json:"remove"`
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
		if len(a.Add) == 0 && len(a.Remove) == 0 {
			return "", nil, fmt.Errorf("add or remove must name at least one blocker")
		}
		p := localapi.SetBlockedByParams{Subscriber: who, Issue: string(a.Issue)}
		for _, b := range a.Add {
			if b == "" {
				return "", nil, fmt.Errorf("add contains an empty blocker reference")
			}
			p.Add = append(p.Add, string(b))
		}
		for _, b := range a.Remove {
			if b == "" {
				return "", nil, fmt.Errorf("remove contains an empty blocker reference")
			}
			p.Remove = append(p.Remove, string(b))
		}
		return localapi.MethodSetBlockedBy, p, nil
	case ToolAdvance:
		var a struct {
			Issue issueArg `json:"issue"`
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
		return localapi.MethodAdvance, localapi.AdvanceParams{Subscriber: who, Issue: string(a.Issue)}, nil
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
// the explicit argument, else the name this shim resolved (flag, environment or
// the fixed OS-username default). There is no PID-derived fallback — the name
// keys the held queue and must survive the session, so a call with neither is
// refused with the fix.
func subscriberName(explicit, launched string) (string, error) {
	switch {
	case explicit != "":
		return explicit, nil
	case launched != "":
		return launched, nil
	}
	return "", fmt.Errorf("no subscriber name: no OS username could be found either, so launch this server with --subscriber <name> (or set FABRIK_SUBSCRIBER), using a stable name such as your topic or session name, or pass subscriber in the call")
}

// requesterName is the subscriber an action is attributed to: only the name this
// server was launched with. Actions take no per-call override — the audit
// comment names who asked, so a call cannot choose another name — and are
// refused when the server has none.
func requesterName(launched string) (string, error) {
	if launched == "" {
		return "", fmt.Errorf("this action is refused: it posts an audit comment naming who requested it, and this server could not resolve a subscriber name (no --subscriber, FABRIK_SUBSCRIBER, CLAUDE_TOPIC or OS username) — launch it with --subscriber <name> (or set FABRIK_SUBSCRIBER), using a stable name such as your topic or session name")
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
