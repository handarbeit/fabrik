package mcpstdio

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/handarbeit/fabrik/internal/localapi"
)

func TestIssueArgDecoding(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    string
		wantErr bool
	}{
		{`"2050"`, "2050", false},
		{`"#2050"`, "#2050", false},
		{`"o/r#8"`, "o/r#8", false},
		{`2050`, "2050", false},
		{`null`, "", false},
		{`1.5`, "", true},
		{`2050.0`, "", true},
		{`1e3`, "", true},
		{`0`, "", true},
		{`-3`, "", true},
		{`99999999999999999999999`, "", true},
		{`true`, "", true},
		{`{}`, "", true},
		{`[]`, "", true},
	} {
		var got struct {
			Issue issueArg `json:"issue"`
		}
		err := decodeArgs(json.RawMessage(`{"issue":`+tc.in+`}`), &got)
		if tc.wantErr {
			if err == nil {
				t.Errorf("issue %s: want an error, got %q", tc.in, got.Issue)
			} else if !strings.Contains(err.Error(), "positive integer") && !strings.Contains(err.Error(), "cannot unmarshal") {
				t.Errorf("issue %s: unclear error %v", tc.in, err)
			}
			continue
		}
		if err != nil || string(got.Issue) != tc.want {
			t.Errorf("issue %s: got %q, %v; want %q", tc.in, got.Issue, err, tc.want)
		}
	}
}

func TestBuildCallNumericIssueEqualsBareString(t *testing.T) {
	for _, tc := range []struct {
		tool, extra string
	}{
		{ToolStatus, ``},
		{ToolPromote, `,"to":"Specify"`},
		{ToolSetAutonomy, `,"autonomy":"cruise"`},
		{ToolRevalidate, ``},
	} {
		_, numeric, err := buildCall(tc.tool, json.RawMessage(`{"issue":2050`+tc.extra+`}`), "me")
		if err != nil {
			t.Errorf("%s numeric: %v", tc.tool, err)
			continue
		}
		_, str, err := buildCall(tc.tool, json.RawMessage(`{"issue":"2050"`+tc.extra+`}`), "me")
		if err != nil || !reflect.DeepEqual(numeric, str) {
			t.Errorf("%s: numeric params %+v != string params %+v (%v)", tc.tool, numeric, str, err)
		}
		for _, bad := range []string{`1.5`, `0`, `-3`} {
			if _, _, err := buildCall(tc.tool, json.RawMessage(`{"issue":`+bad+tc.extra+`}`), "me"); err == nil || !strings.Contains(err.Error(), "positive integer") {
				t.Errorf("%s issue %s: err = %v, want a positive-integer error", tc.tool, bad, err)
			}
		}
		if _, _, err := buildCall(tc.tool, json.RawMessage(`{"issue":null`+tc.extra+`}`), "me"); err == nil || !strings.Contains(err.Error(), "required") {
			t.Errorf("%s null issue: err = %v, want required", tc.tool, err)
		}
	}
}

func TestBuildCallSubscribeAcceptsNumericIssues(t *testing.T) {
	_, p, err := buildCall(ToolSubscribe, json.RawMessage(`{"issues":[7,"o/r#8","#9"]}`), "me")
	if err != nil {
		t.Fatal(err)
	}
	if got := p.(localapi.SubscribeParams).Issues; !reflect.DeepEqual(got, []string{"7", "o/r#8", "#9"}) {
		t.Errorf("issues = %v", got)
	}
	if _, _, err := buildCall(ToolSubscribe, json.RawMessage(`{"issues":[0]}`), "me"); err == nil {
		t.Error("issues: [0] accepted")
	}
}

func TestIssueSchemasAdvertiseStringAndInteger(t *testing.T) {
	want := []string{"string", "integer"}
	for _, d := range toolDefs() {
		n := d["name"].(string)
		schema := d["inputSchema"].(map[string]any)
		if schema["additionalProperties"] != false {
			t.Errorf("%s lost additionalProperties:false", n)
		}
		props := schema["properties"].(map[string]any)
		var s map[string]any
		switch {
		case n == ToolSubscribe:
			s = props["issues"].(map[string]any)["items"].(map[string]any)
		case props["issue"] != nil:
			s = props["issue"].(map[string]any)
		default:
			continue
		}
		if !reflect.DeepEqual(s["type"], want) {
			t.Errorf("%s issue type = %v, want %v", n, s["type"], want)
		}
		if desc, _ := s["description"].(string); !strings.Contains(desc, "owner/repo#N") || !strings.Contains(desc, "one repo") {
			t.Errorf("%s issue description = %q", n, desc)
		}
	}
}
