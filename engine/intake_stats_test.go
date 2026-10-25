package engine

import (
	"encoding/json"
	"testing"
)

// The intake counts accepted, duplicate and applied deliveries (#2427).
func TestIntakeStats_CountsAcceptedDuplicateApplied(t *testing.T) {
	in := newTestIntake(func(string, []byte) {})
	p := payloadFor("o/r", 1)
	in.Submit(delivery{ID: "A", EventType: "issues", Payload: p})
	in.Submit(delivery{ID: "A", EventType: "issues", Payload: p}) // duplicate
	in.Submit(delivery{ID: "B", EventType: "issue_comment", Payload: p})
	in.Drain()

	s := in.stats.snapshot()
	if s.Accepted != 2 || s.Duplicates != 1 || s.Dropped != 0 {
		t.Fatalf("accepted/duplicates/dropped = %d/%d/%d, want 2/1/0", s.Accepted, s.Duplicates, s.Dropped)
	}
	if s.Applied["issues"] != 1 || s.Applied["issue_comment"] != 1 {
		t.Errorf("applied = %v, want one issues and one issue_comment", s.Applied)
	}
}

// A delivery dropped on queue overflow is counted as dropped, not accepted.
func TestIntakeStats_CountsDropped(t *testing.T) {
	old := intakeMaxTotal
	intakeMaxTotal = 0
	defer func() { intakeMaxTotal = old }()
	in := newTestIntake(func(string, []byte) {})
	in.Submit(delivery{ID: "A", EventType: "issues", Payload: payloadFor("o/r", 1)})
	s := in.stats.snapshot()
	if s.Dropped != 1 || s.Accepted != 0 {
		t.Errorf("dropped/accepted = %d/%d, want 1/0", s.Dropped, s.Accepted)
	}
}

// A matched echo is counted; an unmatched inbound delivery is not.
func TestIntakeStats_CountsEchoMatched(t *testing.T) {
	var st intakeStats
	r := newEchoRegistry()
	r.onMatch = st.noteEchoMatched
	r.Reserve("issues", "labeled", "o/r#1:fabrik:cruise")
	if ok := r.MatchEchoReported("issues", "labeled", "o/r#1:fabrik:other"); ok {
		t.Fatal("unreserved key matched")
	}
	if !r.MatchEchoReported("issues", "labeled", "o/r#1:fabrik:cruise") {
		t.Fatal("reserved key did not match")
	}
	if got := st.snapshot().EchoMatched; got != 1 {
		t.Errorf("echo_matched = %d, want 1", got)
	}
}

// A poll-only daemon (no intake) reports no intake block; an engine with one
// serialises the counters under webhook.intake.
func TestIntakeHealth_NilWithoutIntakeAndSerialised(t *testing.T) {
	e := &Engine{}
	if e.intakeHealth() != nil {
		t.Fatal("intakeHealth must be nil with no intake")
	}
	e.intake = newTestIntake(func(string, []byte) {})
	e.intake.Submit(delivery{ID: "A", EventType: "issues", Payload: payloadFor("o/r", 1)})
	e.intake.Drain()
	raw, err := json.Marshal(e.intakeHealth())
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"accepted", "duplicates", "dropped", "echo_matched", "applied"} {
		if _, ok := m[k]; !ok {
			t.Errorf("wire key %q missing: %s", k, raw)
		}
	}
}
