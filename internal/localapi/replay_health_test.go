package localapi

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestTransportHealth_ReplayMarshal(t *testing.T) {
	// Omitted when replay does not apply.
	b, err := json.Marshal(TransportHealth{Name: "hookdeck", State: "healthy"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "replay") {
		t.Errorf("replay must be omitted: %s", b)
	}

	// Present with unknown values before the first pass.
	b, _ = json.Marshal(TransportHealth{Name: "direct", Replay: &ReplayHealth{}})
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	rep, ok := raw["replay"].(map[string]any)
	if !ok {
		t.Fatalf("replay missing: %s", b)
	}
	for _, k := range []string{"last_pass_at", "found_failed", "requested", "capped", "failed"} {
		if rep[k] != "unknown" {
			t.Errorf("%s = %v, want unknown", k, rep[k])
		}
	}

	// Populated.
	at := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	b, _ = json.Marshal(TransportHealth{Name: "direct", Replay: &ReplayHealth{
		LastPassAt: Some(at), FoundFailed: Some(int64(3)), Requested: Some(int64(2)),
		Capped: Some(int64(1)), Failed: Some(int64(0)), Outcome: "capped",
	}})
	var back TransportHealth
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	r := back.Replay
	if r == nil || !r.LastPassAt.Valid || !r.LastPassAt.V.Equal(at) || r.FoundFailed.V != 3 || r.Requested.V != 2 || r.Capped.V != 1 || !r.Failed.Valid || r.Outcome != "capped" {
		t.Errorf("round trip = %+v", r)
	}
}
