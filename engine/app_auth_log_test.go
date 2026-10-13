package engine

import (
	"strings"
	"testing"

	"github.com/handarbeit/fabrik/tui"
)

// The GitHub App reconciler's refresh loops log after the TUI owns the
// terminal. Once SetEvents is called, those lines must reach the event
// channel, never stdout.
func TestAppAuthLogf_RoutedThroughEventsInTUIMode(t *testing.T) {
	prev := appAuthLogf.Load()
	t.Cleanup(func() { appAuthLogf.Store(prev) })

	e := &Engine{}
	ch := make(chan tui.Event, 4)
	e.SetEvents(ch)
	(*appAuthLogf.Load())("auth: installation token refreshed, expires %s", "soon")

	select {
	case ev := <-ch:
		le, ok := ev.(tui.LogEvent)
		if !ok || le.Tag != "github-app" || !strings.Contains(le.Message, "token refreshed, expires soon") {
			t.Fatalf("got %#v", ev)
		}
	default:
		t.Fatal("reconciler log line did not reach the event channel")
	}
}
