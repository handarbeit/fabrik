package engine

import (
	"strings"
	"testing"

	"github.com/handarbeit/fabrik/pruefer"
)

// TestCatchUpMarkerContract_PrueferParsesEngineMarker (#2066) pins the wire
// format between the two components. Pruefer keeps its own copy of the marker
// regex (it does not import engine), so this renders a marker with the
// engine's own formatter and requires Pruefer's parser to recover every
// field. A format change in the engine fails here instead of silently
// disabling Pruefer's catch-up skip.
func TestCatchUpMarkerContract_PrueferParsesEngineMarker(t *testing.T) {
	head, base := strings.Repeat("a", 40), strings.Repeat("b", 40)
	for _, pure := range []bool{true, false} {
		body := formatCatchUpMarker(catchUpMarker{Head: head, Base: base, Pure: pure})
		got := pruefer.ParseCatchUpMarker(body)
		if len(got) != 1 {
			t.Fatalf("pure=%t: pruefer parsed %d markers from %q, want 1", pure, len(got), body)
		}
		if got[0].Head != head || got[0].Base != base || got[0].Pure != pure {
			t.Errorf("pure=%t: pruefer parsed %+v from engine marker %q", pure, got[0], body)
		}
	}
}
