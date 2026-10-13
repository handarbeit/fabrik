package engine

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/warnings"
)

// displayWarnEngine is a testEngine with all three display fields enabled and
// the warnings panel redirected to a temp file. The mock board has no fields
// unless the test adds them.
func displayWarnEngine(t *testing.T, client *mockGitHubClient) *Engine {
	t.Helper()
	warnings.WarningsPathOverride = filepath.Join(t.TempDir(), "warnings.json")
	t.Cleanup(func() { warnings.WarningsPathOverride = "" })
	eng := testEngine(t, client, &mockClaudeInvoker{})
	eng.cfg.StatusLineField = "Fabrik"
	eng.cfg.LastActivityField = "Last activity"
	eng.cfg.LastRunField = "Last run"
	eng.cfg.OwnerType = "organization"
	return eng
}

func panelEntries(t *testing.T) map[string]warnings.Entry {
	t.Helper()
	es, err := warnings.Load()
	if err != nil {
		t.Fatalf("warnings.Load: %v", err)
	}
	m := map[string]warnings.Entry{}
	for _, e := range es {
		m[e.Key] = e
	}
	return m
}

// SC-002/SC-003 (#2218): a board with none of the three fields yields exactly
// one panel entry carrying the repair-board fix and naming all three.
// Non-vacuity: removing the warnings.Report/Reconcile call in
// reportDisplayFieldWarnings makes this fail.
func TestDisplayFieldWarnings_AllMissingOneEntryWithFix(t *testing.T) {
	client := &mockGitHubClient{textFieldsByName: map[string]*gh.TextField{}}
	eng := displayWarnEngine(t, client)

	eng.resolveDisplayFields("PVT_1")
	eng.resolveDisplayFields("PVT_1") // idempotent

	got := panelEntries(t)
	if len(got) != 1 {
		t.Fatalf("entries = %v, want exactly one", got)
	}
	e, ok := got["display_field:missing"]
	if !ok {
		t.Fatalf("entries = %v, want display_field:missing", got)
	}
	if e.FixAction != "fabrik_command" || e.FixParams["args"] != "repair-board --apply" {
		t.Errorf("fix = %q %v, want fabrik_command repair-board --apply", e.FixAction, e.FixParams)
	}
	for _, name := range []string{"Fabrik", "Last activity", "Last run"} {
		if !strings.Contains(e.Detail, name) {
			t.Errorf("detail %q does not name %q", e.Detail, name)
		}
	}
	if !strings.Contains(e.Detail, "fabrik repair-board --apply") {
		t.Errorf("detail %q does not name the fix", e.Detail)
	}
	// No lookup failure and no retry: three lookups, one per field.
	if client.fetchTextFieldCalls != 2 || client.fetchDateFieldCalls != 1 {
		t.Errorf("lookups = %d text, %d date; want 2, 1", client.fetchTextFieldCalls, client.fetchDateFieldCalls)
	}
}

func TestDisplayFieldWarnings_OffFieldIsNotMissing(t *testing.T) {
	client := &mockGitHubClient{textFieldsByName: map[string]*gh.TextField{}}
	eng := displayWarnEngine(t, client)
	eng.cfg.LastActivityField = "" // off
	eng.cfg.LastRunField = ""      // off

	eng.resolveDisplayFields("PVT_1")

	e := panelEntries(t)["display_field:missing"]
	if strings.Contains(e.Detail, "Last activity") || strings.Contains(e.Detail, "Last run") {
		t.Errorf("detail %q names an off field", e.Detail)
	}
	if !strings.Contains(e.Detail, "Fabrik") {
		t.Errorf("detail %q does not name the status-line field", e.Detail)
	}
}

// Every enabled field present, or off: a stale entry (from a previous process)
// is resolved.
func TestDisplayFieldWarnings_ResolvedWhenAllFound(t *testing.T) {
	client := &mockGitHubClient{
		textFieldsByName: map[string]*gh.TextField{
			"Fabrik":   {ID: "F1", Name: "Fabrik"},
			"Last run": {ID: "F3", Name: "Last run"},
		},
		dateField: &gh.DateField{ID: "F2", Name: "Last activity"},
	}
	eng := displayWarnEngine(t, client)
	if err := warnings.Record(warnings.Entry{Key: "display_field:missing", Type: "display_field", Title: "stale", Fingerprint: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := warnings.Record(warnings.Entry{Key: "display_field:mismatch:last-run", Type: "display_field", Title: "stale", Fingerprint: "y"}); err != nil {
		t.Fatal(err)
	}
	if err := warnings.Record(warnings.Entry{Key: "display_field_lookup:status-line", Type: "display_field_lookup", Title: "stale", Fingerprint: "z"}); err != nil {
		t.Fatal(err)
	}

	eng.resolveDisplayFields("PVT_1")

	if got := panelEntries(t); len(got) != 0 {
		t.Fatalf("entries = %v, want none", got)
	}
}

func TestDisplayFieldWarnings_ResolvedWhenMissingOnesSetOff(t *testing.T) {
	client := &mockGitHubClient{textFieldsByName: map[string]*gh.TextField{}}
	eng := displayWarnEngine(t, client)
	eng.resolveDisplayFields("PVT_1")
	if len(panelEntries(t)) != 1 {
		t.Fatalf("setup: want one entry, got %v", panelEntries(t))
	}

	// A restart with all three keys set to off: same panel file, fresh engine.
	eng2 := testEngine(t, client, &mockClaudeInvoker{})
	eng2.resolveDisplayFields("PVT_1")

	if got := panelEntries(t); len(got) != 0 {
		t.Fatalf("entries = %v, want none once the fields are off", got)
	}
}

func TestDisplayFieldWarnings_TypeMismatchHasNoFixAction(t *testing.T) {
	client := &mockGitHubClient{
		fetchTextFieldErr: &gh.FieldTypeMismatchError{Name: "Fabrik", Want: "TEXT", Found: "NUMBER"},
		dateField:         &gh.DateField{ID: "F2", Name: "Last activity"},
	}
	eng := displayWarnEngine(t, client)
	eng.cfg.LastRunField = "" // off, so the text-lookup error only concerns the status line

	eng.resolveDisplayFields("PVT_1")

	got := panelEntries(t)
	e, ok := got["display_field:mismatch:status-line"]
	if !ok || len(got) != 1 {
		t.Fatalf("entries = %v, want only display_field:mismatch:status-line", got)
	}
	if e.FixAction != "" {
		t.Errorf("FixAction = %q, want none (repair-board never writes a mismatched field)", e.FixAction)
	}
	if !strings.Contains(e.Title, "Fabrik") {
		t.Errorf("title %q does not name the field", e.Title)
	}
}

func TestDisplayFieldWarnings_UserOwnedBoardOmitsFixAction(t *testing.T) {
	client := &mockGitHubClient{textFieldsByName: map[string]*gh.TextField{}}
	eng := displayWarnEngine(t, client)
	eng.cfg.OwnerType = "user"

	eng.resolveDisplayFields("PVT_1")

	e, ok := panelEntries(t)["display_field:missing"]
	if !ok {
		t.Fatal("want a display_field:missing entry")
	}
	if e.FixAction != "" || len(e.FixParams) != 0 {
		t.Errorf("fix = %q %v, want none on a user-owned board", e.FixAction, e.FixParams)
	}
	if !strings.Contains(e.Detail, "by hand") {
		t.Errorf("detail %q should say to create the fields by hand", e.Detail)
	}
}

// FR-002: a genuine lookup failure is a panel entry (once per outage) and is
// resolved by a later successful lookup; it does not claim the field is
// missing, and an unresolved field never clears an existing missing entry.
func TestDisplayFieldWarnings_LookupFailureReportedAndResolved(t *testing.T) {
	client := &mockGitHubClient{fetchTextFieldErr: errors.New("boom")}
	eng := displayWarnEngine(t, client)
	eng.cfg.LastActivityField = ""
	eng.cfg.LastRunField = ""
	start := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	eng.SetClock(stubClock{t: start})

	eng.resolveDisplayFields("PVT_1")
	got := panelEntries(t)
	e, ok := got["display_field_lookup:status-line"]
	if !ok || len(got) != 1 {
		t.Fatalf("entries = %v, want only display_field_lookup:status-line", got)
	}
	if !strings.Contains(e.Detail, "boom") {
		t.Errorf("detail %q should carry the error", e.Detail)
	}
	if e.FixAction != "" {
		t.Errorf("FixAction = %q, want none", e.FixAction)
	}

	// Recovery.
	client.mu.Lock()
	client.fetchTextFieldErr = nil
	client.textField = &gh.TextField{ID: "F1", Name: "Fabrik"}
	client.mu.Unlock()
	eng.SetClock(stubClock{t: start.Add(statusLineLookupRetry + time.Second)})
	eng.resolveDisplayFields("PVT_1")
	if got := panelEntries(t); len(got) != 0 {
		t.Fatalf("entries after recovery = %v, want none", got)
	}
}

func TestDisplayFieldWarnings_UnresolvedFieldDoesNotClearMissingEntry(t *testing.T) {
	client := &mockGitHubClient{fetchDateFieldErr: errors.New("boom"), textFieldsByName: map[string]*gh.TextField{}}
	eng := displayWarnEngine(t, client)
	eng.resolveDisplayFields("PVT_1")

	got := panelEntries(t)
	m, ok := got["display_field:missing"]
	if !ok {
		t.Fatalf("entries = %v, want display_field:missing for the two text fields", got)
	}
	if strings.Contains(m.Detail, "Last activity") {
		t.Errorf("detail %q claims the unresolved date field is missing", m.Detail)
	}
	if _, ok := got["display_field_lookup:last-activity"]; !ok {
		t.Errorf("entries = %v, want the lookup failure too", got)
	}
}
