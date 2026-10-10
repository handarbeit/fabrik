package engine

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/tui"
)

// rfNow is a fixed completion instant, late in the UTC day.
var rfNow = time.Date(2026, 10, 10, 23, 30, 0, 0, time.UTC)

const (
	rfDateFieldID = "FIELD_DATE"
	rfRunFieldID  = "FIELD_RUN"
	rfLineFieldID = "FIELD_TXT"
)

// runFieldsEngine is a testEngine with the Last activity and Last run fields
// on and resolved against a mock board carrying both. The status-line field is
// also on, to show the three are independent.
func runFieldsEngine(t *testing.T, client *mockGitHubClient) (*Engine, chan tui.Event) {
	t.Helper()
	if client.dateField == nil && client.fetchDateFieldErr == nil {
		client.dateField = &gh.DateField{ID: rfDateFieldID, Name: "Last activity"}
	}
	if client.textFieldsByName == nil {
		client.textFieldsByName = map[string]*gh.TextField{
			"Last run": {ID: rfRunFieldID, Name: "Last run"},
			"Fabrik":   {ID: rfLineFieldID, Name: "Fabrik"},
		}
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	eng.cfg.StatusLineField = "Fabrik"
	eng.cfg.LastActivityField = "Last activity"
	eng.cfg.LastRunField = "Last run"
	events := make(chan tui.Event, 64)
	eng.events = events
	eng.resolveStatusLineField("PVT_1")
	eng.resolveRunFields("PVT_1")
	return eng, events
}

func rfDateWrites(c *mockGitHubClient) []dateWrite {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]dateWrite(nil), c.dateWrites...)
}

// rfRunWrites returns the text writes to the Last run field only.
func rfRunWrites(c *mockGitHubClient) []statusLineWrite {
	var out []statusLineWrite
	for _, w := range slWrites(c) {
		if w.fieldID == rfRunFieldID {
			out = append(out, w)
		}
	}
	return out
}

func rfOutcome() runOutcome {
	return runOutcome{StageName: "Validate", Completed: true, TurnsUsed: 42, MaxTurns: 250, Duration: 18 * time.Minute}
}

func TestRunFields_CompletionWritesBothFromEventFields(t *testing.T) {
	client := &mockGitHubClient{}
	eng, _ := runFieldsEngine(t, client)

	eng.noteJobFinished(slItem(1), rfNow, rfOutcome())

	dw := rfDateWrites(client)
	if len(dw) != 1 || dw[0].date != "2026-10-10" || dw[0].fieldID != rfDateFieldID || dw[0].itemID != slItem(1).ItemID {
		t.Fatalf("date writes = %+v, want one 2026-10-10 on the date field", dw)
	}
	rw := rfRunWrites(client)
	if len(rw) != 1 || rw[0].text != "Validate · completed · 42/250 turns · 18m" || rw[0].itemID != slItem(1).ItemID {
		t.Fatalf("run writes = %+v", rw)
	}
	if n := len(slWrites(client)) - len(rw); n != 0 {
		t.Errorf("%d write(s) went to the status-line field, want none", n)
	}
}

func TestRunFields_StartWritesLastActivityOnly(t *testing.T) {
	client := &mockGitHubClient{}
	eng, _ := runFieldsEngine(t, client)

	eng.noteJobStarted(slItem(1), time.Date(2026, 10, 9, 22, 0, 0, 0, time.UTC))

	if dw := rfDateWrites(client); len(dw) != 1 || dw[0].date != "2026-10-09" {
		t.Fatalf("date writes = %+v, want one 2026-10-09", dw)
	}
	if w := slWrites(client); len(w) != 0 {
		t.Fatalf("text writes = %+v, want none on a start", w)
	}
}

func TestRunFields_StartDateIsUTC(t *testing.T) {
	client := &mockGitHubClient{}
	eng, _ := runFieldsEngine(t, client)
	loc := time.FixedZone("UTC+10", 10*3600)

	eng.noteJobStarted(slItem(1), time.Date(2026, 10, 11, 5, 0, 0, 0, loc)) // 2026-10-10 19:00 UTC

	if dw := rfDateWrites(client); len(dw) != 1 || dw[0].date != "2026-10-10" {
		t.Fatalf("date writes = %+v, want the UTC date 2026-10-10", dw)
	}
}

func TestRunFields_UnchangedValueWritesNothing(t *testing.T) {
	client := &mockGitHubClient{}
	eng, _ := runFieldsEngine(t, client)
	day := time.Date(2026, 10, 10, 1, 0, 0, 0, time.UTC)

	// Start and finish on the same day: one date write, one run write.
	eng.noteJobStarted(slItem(1), day)
	eng.noteJobFinished(slItem(1), rfNow, rfOutcome())
	// A second identical run: nothing at all.
	eng.noteJobStarted(slItem(1), day.Add(time.Hour))
	eng.noteJobFinished(slItem(1), rfNow, rfOutcome())

	if n := len(rfDateWrites(client)); n != 1 {
		t.Errorf("date writes = %d, want 1 (several events on one day write once)", n)
	}
	if n := len(rfRunWrites(client)); n != 1 {
		t.Errorf("run writes = %d, want 1 (an unchanged line is not rewritten)", n)
	}

	// A different outcome writes; a different item is tracked independently.
	o := rfOutcome()
	o.TurnLimited = true
	eng.noteJobFinished(slItem(1), rfNow, o)
	eng.noteJobFinished(slItem(2), rfNow, rfOutcome())
	if n := len(rfRunWrites(client)); n != 3 {
		t.Errorf("run writes = %d, want 3", n)
	}
	if n := len(rfDateWrites(client)); n != 2 {
		t.Errorf("date writes = %d, want 2 (item 2's first)", n)
	}
}

func TestRunFields_NextDayWritesNewDate(t *testing.T) {
	client := &mockGitHubClient{}
	eng, _ := runFieldsEngine(t, client)
	eng.noteJobFinished(slItem(1), rfNow, rfOutcome())
	eng.noteJobFinished(slItem(1), rfNow.Add(time.Hour), rfOutcome()) // crosses midnight UTC

	dw := rfDateWrites(client)
	if len(dw) != 2 || dw[1].date != "2026-10-11" {
		t.Fatalf("date writes = %+v, want 2026-10-10 then 2026-10-11", dw)
	}
	if n := len(rfRunWrites(client)); n != 1 {
		t.Errorf("run writes = %d, want 1", n)
	}
}

func TestRunFields_MissingOrWrongTypeFieldIsSilentWithOneStartupLine(t *testing.T) {
	// A date field "Last activity" that is absent and a "Last run" that is of
	// the wrong type both read as nil from the lookup (the client filters by
	// dataType).
	client := &mockGitHubClient{textFieldsByName: map[string]*gh.TextField{}}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	eng.cfg.LastActivityField = "Last activity"
	eng.cfg.LastRunField = "Last run"
	events := make(chan tui.Event, 64)
	eng.events = events

	eng.resolveRunFields("PVT_1")
	eng.resolveRunFields("PVT_1")
	for i := 0; i < 5; i++ {
		eng.noteJobStarted(slItem(i), time.Now())
		eng.noteJobFinished(slItem(i), rfNow, rfOutcome())
	}

	if n := len(rfDateWrites(client)) + len(slWrites(client)); n != 0 {
		t.Fatalf("writes = %d, want none", n)
	}
	logs := drainLogs(events)
	var activity, run int
	for _, l := range logs {
		if !strings.HasPrefix(l, "startup:") || !strings.Contains(l, "unavailable") {
			t.Errorf("unexpected log line %q (nothing may be logged per event)", l)
		}
		if strings.Contains(l, "last-activity") {
			activity++
		}
		if strings.Contains(l, "last-run") {
			run++
		}
	}
	if activity != 1 || run != 1 {
		t.Errorf("startup lines: last-activity=%d last-run=%d, want exactly one each (logs: %v)", activity, run, logs)
	}
}

func TestRunFields_OneMissingFieldDoesNotAffectTheOthers(t *testing.T) {
	client := &mockGitHubClient{}
	eng, _ := runFieldsEngine(t, client)
	// Drop the date field from the board and start over, as at startup.
	client.mu.Lock()
	client.dateField = nil
	client.mu.Unlock()
	eng.lastActivity = displayFieldState{}
	eng.resolveRunFields("PVT_1")

	eng.noteJobFinished(slItem(1), rfNow, rfOutcome())
	eng.setStatusLine(slItem(1), "landing")

	if n := len(rfDateWrites(client)); n != 0 {
		t.Errorf("date writes = %d, want 0 (field missing)", n)
	}
	if n := len(rfRunWrites(client)); n != 1 {
		t.Errorf("run writes = %d, want 1", n)
	}
	var line int
	for _, w := range slWrites(client) {
		if w.fieldID == rfLineFieldID {
			line++
		}
	}
	if line != 1 {
		t.Errorf("status-line writes = %d, want 1", line)
	}
}

func TestRunFields_OffWritesNothingAndDoesNotLookUp(t *testing.T) {
	client := &mockGitHubClient{}
	eng, _ := runFieldsEngine(t, client)
	eng.cfg.LastActivityField = ""
	eng.cfg.LastRunField = ""
	// Fresh state, as at startup with both keys "off".
	eng.lastActivity = displayFieldState{}
	eng.lastRun = displayFieldState{}
	client.mu.Lock()
	client.fetchDateFieldCalls = 0
	client.fetchTextFieldCalls = 0
	client.mu.Unlock()

	eng.resolveRunFields("PVT_1")
	eng.noteJobStarted(slItem(1), time.Now())
	eng.noteJobFinished(slItem(1), rfNow, rfOutcome())

	if n := len(rfDateWrites(client)) + len(slWrites(client)); n != 0 {
		t.Fatalf("writes = %d, want none when off", n)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.fetchDateFieldCalls != 0 || client.fetchTextFieldCalls != 0 {
		t.Errorf("lookups = %d/%d, want none when off", client.fetchDateFieldCalls, client.fetchTextFieldCalls)
	}
}

func TestRunFields_FailedWriteIsSwallowedAndRetriedNextEvent(t *testing.T) {
	client := &mockGitHubClient{dateWriteErr: errors.New("boom"), statusLineErr: errors.New("boom")}
	eng, events := runFieldsEngine(t, client)

	eng.noteJobFinished(slItem(1), rfNow, rfOutcome()) // both fail; must not panic or block

	logs := strings.Join(drainLogs(events), "\n")
	if !strings.Contains(logs, "warning: could not write last-activity") || !strings.Contains(logs, "warning: could not write last-run") {
		t.Errorf("want a logged warning per failed write, got: %s", logs)
	}

	client.mu.Lock()
	client.dateWriteErr = nil
	client.statusLineErr = nil
	client.mu.Unlock()
	eng.noteJobFinished(slItem(1), rfNow, rfOutcome()) // same values: retried because nothing was recorded

	if n := len(rfDateWrites(client)); n != 1 {
		t.Errorf("date writes = %d, want 1 (retry after failure)", n)
	}
	if n := len(rfRunWrites(client)); n != 1 {
		t.Errorf("run writes = %d, want 1 (retry after failure)", n)
	}
}

func TestRunFields_RestartRewritesOnceWithoutReadingBack(t *testing.T) {
	client := &mockGitHubClient{}
	eng, _ := runFieldsEngine(t, client)
	eng.noteJobFinished(slItem(1), rfNow, rfOutcome())

	// A restart forgets what was written: fresh writer state, same board.
	eng2, _ := runFieldsEngine(t, client)
	eng2.noteJobFinished(slItem(1), rfNow, rfOutcome())
	eng2.noteJobFinished(slItem(1), rfNow, rfOutcome())

	if n := len(rfDateWrites(client)); n != 2 {
		t.Errorf("date writes = %d, want 2 (one identical rewrite after restart)", n)
	}
	if n := len(rfRunWrites(client)); n != 2 {
		t.Errorf("run writes = %d, want 2 (one identical rewrite after restart)", n)
	}
}

func TestRunFields_ItemWithoutProjectItemIDWritesNothing(t *testing.T) {
	client := &mockGitHubClient{}
	eng, _ := runFieldsEngine(t, client)
	eng.noteJobFinished(gh.ProjectItem{Number: 9, Repo: "owner/repo"}, rfNow, rfOutcome())
	if n := len(rfDateWrites(client)) + len(slWrites(client)); n != 0 {
		t.Errorf("writes = %d, want none without an ItemID", n)
	}
}

// TestRunFields_NoNewStateOrPersistence pins R1/FR-003: the run-field writer
// adds no store mutation, persistence or board read-back. It is a source scan
// of the files that carry the feature.
func TestRunFields_NoNewStateOrPersistence(t *testing.T) {
	for _, f := range []string{"run_fields.go", "run_fields_text.go"} {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		code := stripGoComments(string(src))
		for _, forbidden := range []string{"e.store", "itemstate.", "os.WriteFile", "os.Create", "encoding/json", "FetchProjectItem", "FetchTextField", "FetchDateField", "TurnProgressEvent", "CostUSD"} {
			if strings.Contains(code, forbidden) {
				t.Errorf("%s contains %q — the Last activity/Last run feature must add no state, persistence or read-back", f, forbidden)
			}
		}
	}
}

func stripGoComments(src string) string {
	var b strings.Builder
	for _, line := range strings.Split(src, "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}
