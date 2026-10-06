package mcpstdio

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/handarbeit/fabrik/internal/localapi"
)

// stubActor records each action it is asked to perform.
type stubActor struct {
	promote  []localapi.PromoteParams
	autonomy []localapi.SetAutonomyParams
	reval    []localapi.RevalidateParams
	clear    []localapi.ClearClaudeLimitParams
}

func (a *stubActor) Promote(p localapi.PromoteParams) (*localapi.ActionResult, error) {
	a.promote = append(a.promote, p)
	if p.Issue == "9" {
		return nil, localapi.Refused(localapi.RefusalState{Issue: "o/r#9", Status: "Implement", ValidTargets: []string{"Specify"}}, "o/r#9 is already in the pipeline column %q", "Implement")
	}
	return &localapi.ActionResult{Action: localapi.MethodPromote, Issue: p.Issue, Changed: true, AuditComment: localapi.AuditPosted}, nil
}
func (a *stubActor) SetAutonomy(p localapi.SetAutonomyParams) (*localapi.ActionResult, error) {
	a.autonomy = append(a.autonomy, p)
	return &localapi.ActionResult{Action: localapi.MethodSetAutonomy, Issue: p.Issue}, nil
}
func (a *stubActor) Revalidate(p localapi.RevalidateParams) (*localapi.ActionResult, error) {
	a.reval = append(a.reval, p)
	return &localapi.ActionResult{Action: localapi.MethodRevalidate, Issue: p.Issue}, nil
}
func (a *stubActor) ClearClaudeLimit(p localapi.ClearClaudeLimitParams) (*localapi.ActionResult, error) {
	a.clear = append(a.clear, p)
	return &localapi.ActionResult{Action: localapi.MethodClearClaudeLimit, Issue: "o/r#3"}, nil
}

func startActionDaemon(t *testing.T, withActor bool) (*localapi.Server, *stubActor) {
	t.Helper()
	srv := localapi.NewServer(filepath.Join(shortDir(t), "s.sock"), &stubBackend{}, nil)
	act := &stubActor{}
	if withActor {
		srv.Actor = act
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	return srv, act
}

func TestActionToolsReachTheDaemonWithTheLaunchedSubscriber(t *testing.T) {
	srv, act := startActionDaemon(t, true)
	frames := sessionAs(t, srv.Path(), "my-topic",
		callTool(1, ToolPromote, `{"issue":"o/r#5","to":"Specify"}`),
		callTool(2, ToolSetAutonomy, `{"issue":"5","autonomy":"yolo"}`),
		callTool(3, ToolRevalidate, `{"issue":"5"}`),
		callTool(4, ToolClearClaudeLimit, `{}`),
	)
	ids := byID(frames)
	for _, id := range []string{"1", "2", "3", "4"} {
		if text, isErr := toolText(t, ids[id]); isErr {
			t.Errorf("call %s failed: %s", id, text)
		}
	}
	if !reflect.DeepEqual(act.promote, []localapi.PromoteParams{{Subscriber: "my-topic", Issue: "o/r#5", To: "Specify"}}) {
		t.Errorf("promote = %+v", act.promote)
	}
	if !reflect.DeepEqual(act.autonomy, []localapi.SetAutonomyParams{{Subscriber: "my-topic", Issue: "5", Mode: "yolo"}}) {
		t.Errorf("set_autonomy = %+v", act.autonomy)
	}
	if !reflect.DeepEqual(act.reval, []localapi.RevalidateParams{{Subscriber: "my-topic", Issue: "5"}}) {
		t.Errorf("revalidate = %+v", act.reval)
	}
	if !reflect.DeepEqual(act.clear, []localapi.ClearClaudeLimitParams{{Subscriber: "my-topic"}}) {
		t.Errorf("clear_claude_limit = %+v", act.clear)
	}
}

// Every action is attributable: with no --subscriber the shim refuses before
// the daemon is ever contacted.
func TestActionToolsRefuseWithoutASubscriber(t *testing.T) {
	srv, act := startActionDaemon(t, true)
	frames := session(t, srv.Path(),
		callTool(1, ToolPromote, `{"issue":"5","to":"Specify"}`),
		callTool(2, ToolSetAutonomy, `{"issue":"5","autonomy":"cruise"}`),
		callTool(3, ToolRevalidate, `{"issue":"5"}`),
		callTool(4, ToolClearClaudeLimit, `{}`),
	)
	ids := byID(frames)
	for _, id := range []string{"1", "2", "3", "4"} {
		text, isErr := toolText(t, ids[id])
		if !isErr || !strings.Contains(text, "--subscriber") {
			t.Errorf("call %s: isErr=%v text=%q", id, isErr, text)
		}
	}
	if len(act.promote)+len(act.autonomy)+len(act.reval)+len(act.clear) != 0 {
		t.Errorf("the daemon was reached without a requester: %+v", act)
	}
}

func TestActionToolArgumentErrors(t *testing.T) {
	srv, act := startActionDaemon(t, true)
	frames := sessionAs(t, srv.Path(), "s",
		callTool(1, ToolPromote, `{"issue":"5"}`),                                    // missing to
		callTool(2, ToolPromote, `{"issue":"5","to":"Specify","subscriber":"evil"}`), // no per-call requester override
		callTool(3, ToolSetAutonomy, `{"issue":"5","autonomy":"turbo"}`),
		callTool(4, ToolSetAutonomy, `{"issue":"5"}`),
		callTool(5, ToolRevalidate, `{}`),
		callTool(6, ToolClearClaudeLimit, `{"issue":"5"}`), // takes no arguments
	)
	ids := byID(frames)
	for _, id := range []string{"1", "2", "3", "4", "5", "6"} {
		text, isErr := toolText(t, ids[id])
		if !isErr || !strings.Contains(text, "invalid arguments") {
			t.Errorf("call %s: isErr=%v text=%q", id, isErr, text)
		}
	}
	if len(act.promote)+len(act.autonomy)+len(act.reval)+len(act.clear) != 0 {
		t.Errorf("invalid calls reached the daemon: %+v", act)
	}
}

func TestRefusalShowsCurrentState(t *testing.T) {
	srv, _ := startActionDaemon(t, true)
	frames := sessionAs(t, srv.Path(), "s", callTool(1, ToolPromote, `{"issue":"9","to":"Specify"}`))
	text, isErr := toolText(t, byID(frames)["1"])
	if !isErr || !strings.Contains(text, "refused:") || !strings.Contains(text, `"status": "Implement"`) || !strings.Contains(text, `"valid_targets"`) {
		t.Errorf("a refusal must carry the current state: isErr=%v text=%q", isErr, text)
	}
}

// A daemon without the action method set (older, or a read-only server) is
// reported as such, not as an opaque protocol code.
func TestActionAgainstDaemonWithoutActionsSaysSo(t *testing.T) {
	srv, _ := startActionDaemon(t, false)
	frames := sessionAs(t, srv.Path(), "s", callTool(1, ToolRevalidate, `{"issue":"5"}`))
	text, isErr := toolText(t, byID(frames)["1"])
	if !isErr || !strings.Contains(text, "does not support overseer actions") {
		t.Errorf("isErr=%v text=%q", isErr, text)
	}
}

// R2: the tool surface offers no way to lift a pause or to comment.
func TestNoPauseLiftOrCommentTool(t *testing.T) {
	var names []string
	for _, d := range toolDefs() {
		names = append(names, d["name"].(string))
	}
	for _, n := range names {
		for _, banned := range []string{"pause", "resume", "comment", "label", "unpause", "answer"} {
			if strings.Contains(n, banned) {
				t.Errorf("tool %q looks like a pause-lift or free-form write", n)
			}
		}
	}
	// Every state-changing tool is either a subscription tool or a documented action.
	for _, d := range toolDefs() {
		n := d["name"].(string)
		ro := d["annotations"].(map[string]any)["readOnlyHint"] == true
		if !ro && n != ToolSubscribe && n != ToolUnsubscribe && !IsActionTool(n) {
			t.Errorf("unexpected state-changing tool %q", n)
		}
	}
	if got, want := ActionTools(), []string{ToolPromote, ToolSetAutonomy, ToolRevalidate, ToolClearClaudeLimit}; !reflect.DeepEqual(got, want) {
		t.Errorf("ActionTools = %v", got)
	}
}

// The action schemas are exact: unknown fields rejected, autonomy is an enum,
// and there is no `subscriber` property (the requester is the launched name).
func TestActionToolSchemas(t *testing.T) {
	for _, d := range toolDefs() {
		n := d["name"].(string)
		if !IsActionTool(n) {
			continue
		}
		schema := d["inputSchema"].(map[string]any)
		if schema["additionalProperties"] != false {
			t.Errorf("%s must reject unknown properties", n)
		}
		props := schema["properties"].(map[string]any)
		if _, has := props["subscriber"]; has {
			t.Errorf("%s must not let the caller choose the requester", n)
		}
		if n == ToolSetAutonomy {
			enum := props["autonomy"].(map[string]any)["enum"].([]string)
			if !reflect.DeepEqual(enum, []string{"cruise", "yolo", "none"}) {
				t.Errorf("autonomy enum = %v", enum)
			}
		}
	}
}
