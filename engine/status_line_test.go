package engine

import (
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/tui"
)

// statusLineEngine is a testEngine with the display-only status-line feature
// on, its field resolved against a mock board that carries a "Fabrik" text
// field. The returned channel receives the engine's log events.
func statusLineEngine(t *testing.T, client *mockGitHubClient) (*Engine, chan tui.Event) {
	t.Helper()
	if client.textField == nil && client.fetchTextFieldErr == nil {
		client.textField = &gh.TextField{ID: "FIELD_TXT", Name: "Fabrik"}
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	eng.cfg.StatusLineField = "Fabrik"
	events := make(chan tui.Event, 64)
	eng.events = events
	eng.resolveStatusLineField("PVT_1")
	return eng, events
}

func drainLogs(ch chan tui.Event) []string {
	var out []string
	for {
		select {
		case ev := <-ch:
			if le, ok := ev.(tui.LogEvent); ok {
				out = append(out, le.Tag+": "+le.Message)
			}
		default:
			return out
		}
	}
}

func slItem(n int) gh.ProjectItem {
	return gh.ProjectItem{ItemID: "PVTI_" + string(rune('a'+n)), Number: n, Repo: "owner/repo"}
}

func slWrites(c *mockGitHubClient) []statusLineWrite {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]statusLineWrite(nil), c.statusLineWrites...)
}

func TestStatusLine_WritesExpectedLineOnce(t *testing.T) {
	client := &mockGitHubClient{}
	eng, _ := statusLineEngine(t, client)

	eng.setStatusLine(slItem(1), statusLineQueued(3))

	w := slWrites(client)
	if len(w) != 1 {
		t.Fatalf("writes = %+v, want exactly one", w)
	}
	if w[0].text != "queued · batch of 3" || w[0].fieldID != "FIELD_TXT" || w[0].itemID != slItem(1).ItemID || w[0].cleared {
		t.Errorf("write = %+v", w[0])
	}
}

func TestStatusLine_UnchangedValueWritesNothing(t *testing.T) {
	client := &mockGitHubClient{}
	eng, _ := statusLineEngine(t, client)

	eng.setStatusLine(slItem(1), "landing")
	eng.setStatusLine(slItem(1), "landing")
	eng.setStatusLine(slItem(1), "landing")
	if n := len(slWrites(client)); n != 1 {
		t.Fatalf("writes = %d, want 1 (repeats of an unchanged value must not write)", n)
	}

	eng.setStatusLine(slItem(1), "queued · batch of 2")
	eng.setStatusLine(slItem(1), "landing")
	if n := len(slWrites(client)); n != 3 {
		t.Fatalf("writes = %d, want 3 (each distinct change writes)", n)
	}
	// A different item is tracked independently.
	eng.setStatusLine(slItem(2), "landing")
	if n := len(slWrites(client)); n != 4 {
		t.Fatalf("writes = %d, want 4", n)
	}
}

func TestStatusLine_DoneClears(t *testing.T) {
	client := &mockGitHubClient{}
	eng, _ := statusLineEngine(t, client)

	eng.setStatusLine(slItem(1), "landing")
	eng.clearStatusLine(slItem(1))
	eng.clearStatusLine(slItem(1)) // already cleared: no second call

	w := slWrites(client)
	if len(w) != 2 || !w[1].cleared || w[1].itemID != slItem(1).ItemID {
		t.Fatalf("writes = %+v, want a set then one clear", w)
	}
	// Setting the same line again after a clear writes again.
	eng.setStatusLine(slItem(1), "landing")
	if n := len(slWrites(client)); n != 3 {
		t.Errorf("writes = %d, want 3", n)
	}
}

func TestStatusLine_ClearAfterRestartWithNoRecordClearsOnce(t *testing.T) {
	client := &mockGitHubClient{}
	eng, _ := statusLineEngine(t, client)

	eng.clearStatusLine(slItem(1))
	eng.clearStatusLine(slItem(1))
	if w := slWrites(client); len(w) != 1 || !w[0].cleared {
		t.Fatalf("writes = %+v, want one blind clear", w)
	}
}

func TestStatusLine_MissingFieldIsSilentWithOneStartupLine(t *testing.T) {
	client := &mockGitHubClient{} // textField nil: no such field
	eng := testEngine(t, client, &mockClaudeInvoker{})
	eng.cfg.StatusLineField = "Fabrik"
	events := make(chan tui.Event, 64)
	eng.events = events

	eng.resolveStatusLineField("PVT_1")
	eng.resolveStatusLineField("PVT_1")
	for i := 0; i < 5; i++ {
		eng.setStatusLine(slItem(i), "landing")
		eng.clearStatusLine(slItem(i))
	}

	if w := slWrites(client); len(w) != 0 {
		t.Fatalf("writes = %+v, want none", w)
	}
	logs := drainLogs(events)
	if len(logs) != 1 || !strings.HasPrefix(logs[0], "startup:") || !strings.Contains(logs[0], "unavailable") {
		t.Fatalf("logs = %q, want exactly one startup line saying the field is unavailable", logs)
	}
}

func TestStatusLine_OffIsSilentWithOneStartupLine(t *testing.T) {
	client := &mockGitHubClient{textField: &gh.TextField{ID: "FIELD_TXT", Name: "Fabrik"}}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	eng.cfg.StatusLineField = "" // project_fields.status_line: off
	events := make(chan tui.Event, 64)
	eng.events = events

	eng.resolveStatusLineField("PVT_1")
	eng.setStatusLine(slItem(1), "landing")
	eng.clearStatusLine(slItem(1))

	if w := slWrites(client); len(w) != 0 {
		t.Fatalf("writes = %+v, want none when off", w)
	}
	if logs := drainLogs(events); len(logs) != 1 || !strings.Contains(logs[0], "off") {
		t.Fatalf("logs = %q, want one startup line mentioning off", logs)
	}
}

func TestStatusLine_NoLookupMeansNoWriteAndNoPerTransitionLog(t *testing.T) {
	// A lookup error at startup is retried lazily; while it keeps failing,
	// writes are skipped and nothing is logged per transition.
	client := &mockGitHubClient{fetchTextFieldErr: errors.New("boom")}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	eng.cfg.StatusLineField = "Fabrik"
	events := make(chan tui.Event, 64)
	eng.events = events

	eng.resolveStatusLineField("PVT_1")
	before := len(drainLogs(events))
	if before != 1 {
		t.Fatalf("want one warning for the failed lookup, got %d", before)
	}
	// No project id is known to the lazy path (no cache), so it cannot retry.
	eng.setStatusLine(slItem(1), "landing")
	if w := slWrites(client); len(w) != 0 {
		t.Fatalf("writes = %+v, want none", w)
	}
	if logs := drainLogs(events); len(logs) != 0 {
		t.Fatalf("logs = %q, want none per transition", logs)
	}
}

// A lookup that keeps failing is not repeated on every transition: it is
// rate-limited, warned about once per outage, and retried once the gap passes.
func TestStatusLine_FailedLookupIsRateLimitedAndWarnedOnce(t *testing.T) {
	client := &mockGitHubClient{fetchTextFieldErr: errors.New("boom")}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	eng.cfg.StatusLineField = "Fabrik"
	events := make(chan tui.Event, 64)
	eng.events = events
	start := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	eng.SetClock(stubClock{t: start})

	lookups := func() int {
		client.mu.Lock()
		defer client.mu.Unlock()
		return client.fetchTextFieldCalls
	}

	for i := 0; i < 5; i++ {
		eng.resolveStatusLineField("PVT_1")
	}
	if n := lookups(); n != 1 {
		t.Fatalf("lookups within the retry gap = %d, want 1", n)
	}
	if logs := drainLogs(events); len(logs) != 1 {
		t.Fatalf("logs = %q, want exactly one warning", logs)
	}

	// Past the gap it retries, but the outage is not warned about again.
	eng.SetClock(stubClock{t: start.Add(statusLineLookupRetry + time.Second)})
	eng.resolveStatusLineField("PVT_1")
	if n := lookups(); n != 2 {
		t.Fatalf("lookups after the gap = %d, want 2", n)
	}
	if logs := drainLogs(events); len(logs) != 0 {
		t.Fatalf("logs = %q, want none for a repeat of the same outage", logs)
	}

	// Recovery resolves the field and writes start working.
	client.mu.Lock()
	client.fetchTextFieldErr = nil
	client.textField = &gh.TextField{ID: "FIELD_TXT", Name: "Fabrik"}
	client.mu.Unlock()
	eng.SetClock(stubClock{t: start.Add(2*statusLineLookupRetry + 2*time.Second)})
	eng.resolveStatusLineField("PVT_1")
	eng.setStatusLine(slItem(1), "landing")
	if w := slWrites(client); len(w) != 1 {
		t.Fatalf("writes after recovery = %+v, want 1", w)
	}
}

func TestStatusLine_FailedWriteIsSwallowedAndRetriedAtNextTransition(t *testing.T) {
	client := &mockGitHubClient{}
	eng, events := statusLineEngine(t, client)
	client.mu.Lock()
	client.statusLineErr = errors.New("api down")
	client.mu.Unlock()

	eng.setStatusLine(slItem(1), "landing") // must not panic or surface the error
	logs := drainLogs(events)
	if len(logs) != 1 || !strings.Contains(logs[0], "status-line:") || !strings.Contains(logs[0], "api down") {
		t.Fatalf("logs = %q, want one status-line warning", logs)
	}

	client.mu.Lock()
	client.statusLineErr = nil
	client.mu.Unlock()
	eng.setStatusLine(slItem(1), "landing") // same value: the failure was never recorded, so it retries
	if w := slWrites(client); len(w) != 1 || w[0].text != "landing" {
		t.Fatalf("writes = %+v, want the line written once the API recovered", w)
	}
}

func TestStatusLine_NoItemIDIsSkipped(t *testing.T) {
	client := &mockGitHubClient{}
	eng, _ := statusLineEngine(t, client)
	eng.setStatusLine(gh.ProjectItem{Number: 3, Repo: "owner/repo"}, "landing")
	if w := slWrites(client); len(w) != 0 {
		t.Fatalf("writes = %+v, want none without a project item id", w)
	}
}

func TestTruncateStatusLine(t *testing.T) {
	short := "queued · batch of 3"
	if got := truncateStatusLine(short); got != short {
		t.Errorf("short line changed: %q", got)
	}
	long := "paused: " + strings.Repeat("é", 100) // multi-byte: counts characters, not bytes
	got := truncateStatusLine(long)
	if n := utf8.RuneCountInString(got); n != statusLineMaxLen {
		t.Errorf("rune count = %d, want %d", n, statusLineMaxLen)
	}
	if !strings.HasSuffix(got, statusLineEllipsis) || !utf8.ValidString(got) {
		t.Errorf("got %q, want a valid string ending in an ellipsis", got)
	}
	if got := truncateStatusLine("a\n  b\tc"); got != "a b c" {
		t.Errorf("whitespace not collapsed: %q", got)
	}
	exact := strings.Repeat("x", statusLineMaxLen)
	if got := truncateStatusLine(exact); got != exact {
		t.Errorf("a line exactly at the limit must be untouched")
	}
}

func TestStatusLineBuilders(t *testing.T) {
	cases := map[string]string{
		statusLineQueued(3):                       "queued · batch of 3",
		statusLineTrial(1692, "CI running"):       "trial #1692 · CI running",
		statusLineTrial(0, "resolving conflicts"): "trial · resolving conflicts",
		statusLineBisect(2, 3):                    "bisecting · step 2 of 3",
		statusLineLanding:                         "landing",
		statusLineDeferred(1549):                  "deferred: overlaps #1549",
		statusLineCatchUp(77):                     "catch-up · CI on PR #77",
		statusLineStageRunning("Implement"):       "Implement · running",
		statusLineCommentReview("Validate"):       "Validate · comment review",
		statusLineBlocked(1568):                   "blocked by #1568",
		statusLinePaused("cycle limit"):           "paused: cycle limit",
		statusLinePaused(""):                      "paused",
		statusLineAwaitingCI(1615):                "awaiting CI on PR #1615",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}
