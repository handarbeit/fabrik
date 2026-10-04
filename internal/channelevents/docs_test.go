package channelevents

import (
	"os"
	"strings"
	"testing"
)

// Every event the catalogue can emit must be documented where an operator and a
// maintainer look: the user guide's event table and the state machine's
// derivation table (#1968 R11).
func TestEveryEventIsDocumented(t *testing.T) {
	docs := map[string]string{}
	for name, path := range map[string]string{
		"USER_GUIDE":    "../../docs/USER_GUIDE.md",
		"state-machine": "../../docs/state-machine.md",
		"ADR":           "../../adrs/1966-b-fabrik-mcp-channel-events.md",
	} {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		docs[name] = string(b)
	}
	var events []EventType
	events = append(events, Subscribable()...)
	events = append(events, EventsDropped, Digest)
	for _, e := range events {
		for _, name := range []string{"USER_GUIDE", "state-machine"} {
			if !strings.Contains(docs[name], "`"+string(e)+"`") {
				t.Errorf("event %q is not documented in %s", e, name)
			}
		}
	}
	if !strings.Contains(docs["ADR"], "# ADR 1966-b:") {
		t.Error("ADR 1966-b must carry its numbered heading")
	}
}
