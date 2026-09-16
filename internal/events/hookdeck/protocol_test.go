package hookdeck

import (
	"encoding/json"
	"testing"
)

func TestFlexHeaders_UnmarshalJSON_FlatStrings(t *testing.T) {
	var h flexHeaders
	if err := json.Unmarshal([]byte(`{"X-GitHub-Event":"pull_request"}`), &h); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := h["X-GitHub-Event"]; got != "pull_request" {
		t.Errorf("h[X-GitHub-Event] = %q, want pull_request", got)
	}
}

func TestFlexHeaders_UnmarshalJSON_ArrayValues(t *testing.T) {
	var h flexHeaders
	if err := json.Unmarshal([]byte(`{"X-GitHub-Event":["pull_request","extra"]}`), &h); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := h["X-GitHub-Event"]; got != "pull_request" {
		t.Errorf("h[X-GitHub-Event] = %q, want first array element pull_request", got)
	}
}

// TestFlexHeaders_UnmarshalJSON_MixedShapesInOneFrame guards against the
// case that motivated flexHeaders: an attemptBody's Headers field must not
// fail to decode just because one key's value is array-shaped while
// another's is a flat string — a shape surprise on a single header must
// not fail decoding of the whole attempt frame.
func TestFlexHeaders_UnmarshalJSON_MixedShapesInOneFrame(t *testing.T) {
	raw := `{"method":"POST","data_string":"{}","headers":{"X-Hub-Signature-256":"sig-value","X-GitHub-Event":["pull_request"],"X-GitHub-Delivery":"d1"}}`
	var req attemptRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("unmarshal attemptRequest: %v", err)
	}
	want := flexHeaders{
		"X-Hub-Signature-256": "sig-value",
		"X-GitHub-Event":      "pull_request",
		"X-GitHub-Delivery":   "d1",
	}
	for k, v := range want {
		if got := req.Headers[k]; got != v {
			t.Errorf("req.Headers[%q] = %q, want %q", k, got, v)
		}
	}
}

// TestFlexHeaders_UnmarshalJSON_SkipsUnknownShape confirms an unparseable
// per-key value (e.g. a number, which is neither a JSON string nor a JSON
// array of strings) doesn't fail decoding of the whole map — the key is
// simply omitted rather than surfacing an error that would drop the entire
// attempt frame.
func TestFlexHeaders_UnmarshalJSON_SkipsUnknownShape(t *testing.T) {
	var h flexHeaders
	if err := json.Unmarshal([]byte(`{"X-GitHub-Event":404,"X-GitHub-Delivery":"d1"}`), &h); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := h["X-GitHub-Event"]; ok {
		t.Errorf("h[X-GitHub-Event] should be absent for an unsupported shape, got %q", h["X-GitHub-Event"])
	}
	if got := h["X-GitHub-Delivery"]; got != "d1" {
		t.Errorf("h[X-GitHub-Delivery] = %q, want d1", got)
	}
}
