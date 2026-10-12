package mcpstdio

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/handarbeit/fabrik/internal/channelevents"
	"github.com/handarbeit/fabrik/internal/localapi"
)

func TestSituationToolBuildsTheMethodCall(t *testing.T) {
	m, p, err := buildCall(ToolSituation, json.RawMessage(`{}`), "")
	if err != nil || m != localapi.MethodSituation {
		t.Fatalf("plain read: %q %v", m, err)
	}
	if got := p.(localapi.SituationParams); got != (localapi.SituationParams{}) {
		t.Errorf("params = %+v", got)
	}
	// A plain read needs no subscriber name.
	if _, _, err := buildCall(ToolSituation, nil, ""); err != nil {
		t.Errorf("no arguments at all: %v", err)
	}

	_, p, err = buildCall(ToolSituation, json.RawMessage(`{"push":true,"force":true}`), "topic-x")
	if err != nil {
		t.Fatal(err)
	}
	if got := p.(localapi.SituationParams); got != (localapi.SituationParams{Subscriber: "topic-x", Push: true, Force: true}) {
		t.Errorf("push params = %+v (the launched --subscriber name is the target)", got)
	}
	// A push targets this session's own channel only: there is no per-call
	// subscriber override, so another subscriber's queue cannot be named.
	if _, _, err := buildCall(ToolSituation, json.RawMessage(`{"push":true,"subscriber":"other"}`), "topic-x"); err == nil {
		t.Error("a per-call subscriber must be refused")
	}
	// Without any name a push is still sent: the daemon answers and says why
	// nothing was pushed, so the model still gets the view.
	_, p, err = buildCall(ToolSituation, json.RawMessage(`{"push":true}`), "")
	if err != nil || p.(localapi.SituationParams).Subscriber != "" {
		t.Errorf("anonymous push: %+v %v", p, err)
	}
	if _, _, err := buildCall(ToolSituation, json.RawMessage(`{"nope":1}`), ""); err == nil {
		t.Error("an unknown argument must be refused")
	}
}

func TestSubscribeToolPassesSituationSeconds(t *testing.T) {
	_, p, err := buildCall(ToolSubscribe, json.RawMessage(`{"situation_seconds":300}`), "topic-x")
	if err != nil {
		t.Fatal(err)
	}
	if got := p.(localapi.SubscribeParams).SituationSeconds; got != 300 {
		t.Errorf("situation_seconds = %d", got)
	}
	if _, _, err := buildCall(ToolSubscribe, json.RawMessage(`{"situation_seconds":-1}`), "topic-x"); err == nil {
		t.Error("a negative cadence must be refused")
	}
	_, p, _ = buildCall(ToolSubscribe, json.RawMessage(`{}`), "topic-x")
	if got := p.(localapi.SubscribeParams).SituationSeconds; got != 0 {
		t.Errorf("the cadence is off by default, got %d", got)
	}
}

func TestSituationIsReadOnlyAndInTheEventEnum(t *testing.T) {
	var def map[string]any
	for _, d := range toolDefs() {
		if d["name"] == ToolSituation {
			def = d
		}
	}
	if def == nil {
		t.Fatal("fabrik_situation is not in the tool list")
	}
	if def["annotations"].(map[string]any)["readOnlyHint"] != true {
		t.Error("fabrik_situation is a read tool")
	}
	if IsActionTool(ToolSituation) {
		t.Error("fabrik_situation writes nothing to GitHub and is not an action tool")
	}
	found := false
	for _, n := range eventNames() {
		found = found || n == string(channelevents.Situation)
	}
	if !found {
		t.Error("situation is not in fabrik_subscribe's events enum")
	}
}

func TestSituationAgainstAnOlderDaemon(t *testing.T) {
	err := &localapi.Error{Code: localapi.CodeUnknownMethod, Message: `unknown method "situation"`}
	if got := describeToolError(ToolSituation, err); !strings.Contains(got, "predates fabrik_situation") {
		t.Errorf("unknown_method for situation = %q", got)
	}
	// Other tools keep the generic wording.
	if got := describeToolError(ToolHealth, err); strings.Contains(got, "predates fabrik_situation") {
		t.Errorf("health got the situation message: %q", got)
	}
	if got := describeToolError(ToolSituation, errors.New("boom")); strings.Contains(got, "predates") {
		t.Errorf("a non-protocol error is not a version skew: %q", got)
	}
}

func TestSituationEventReachesTheSessionAsAChannelNotification(t *testing.T) {
	n := channelNotification(channelevents.Event{
		Type:    channelevents.Situation,
		Content: "Situation:\n- one",
		Meta: map[string]string{
			"event": "situation", channelevents.MetaTrigger: "cadence", channelevents.MetaSource: "live",
			"human_input": "2", "bad-key": "dropped",
		},
	})
	meta := n["meta"].(map[string]string)
	if meta["event"] != "situation" || meta["trigger"] != "cadence" || meta["human_input"] != "2" || meta[MetaOrigin] != "live" {
		t.Errorf("meta = %v", meta)
	}
	if _, ok := meta["bad-key"]; ok {
		t.Error("a non-identifier key must be dropped")
	}
	if n["content"] != "Situation:\n- one" {
		t.Errorf("content = %v", n["content"])
	}
}
