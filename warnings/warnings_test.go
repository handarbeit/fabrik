package warnings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func setOverride(t *testing.T) {
	t.Helper()
	WarningsPathOverride = filepath.Join(t.TempDir(), "warnings.json")
	t.Cleanup(func() { WarningsPathOverride = "" })
}

func TestRecord_NewEntry(t *testing.T) {
	setOverride(t)
	before := time.Now().UTC().Add(-time.Second)
	entry := Entry{
		Key:       "allow_auto_merge:owner/repo",
		Type:      "allow_auto_merge",
		Title:     "allow_auto_merge disabled on owner/repo",
		Detail:    "fix: gh api ...",
		FixAction: "shell_command",
		FixParams: map[string]string{"cmd": "gh api -X PATCH repos/owner/repo -f allow_auto_merge=true"},
	}
	if err := Record(entry); err != nil {
		t.Fatalf("Record: %v", err)
	}
	entries, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	e := entries[0]
	if e.Key != entry.Key {
		t.Errorf("key = %q, want %q", e.Key, entry.Key)
	}
	if e.Dismissed {
		t.Errorf("dismissed should be false for new entry")
	}
	if e.FirstSeen.Before(before) {
		t.Errorf("first_seen too early")
	}
	if e.LastSeen.Before(before) {
		t.Errorf("last_seen too early")
	}
}

func TestRecord_Upsert_PreservesFirstSeenAndDismissed(t *testing.T) {
	setOverride(t)
	entry := Entry{
		Key:    "allow_auto_merge:owner/repo",
		Type:   "allow_auto_merge",
		Title:  "title v1",
		Detail: "detail v1",
	}
	if err := Record(entry); err != nil {
		t.Fatalf("Record: %v", err)
	}
	// Dismiss it.
	if err := Dismiss(entry.Key); err != nil {
		t.Fatalf("Dismiss: %v", err)
	}
	entries, _ := Load()
	firstSeen := entries[0].FirstSeen

	// Wait a moment then re-record with updated detail.
	time.Sleep(10 * time.Millisecond)
	entry.Title = "title v2"
	entry.Detail = "detail v2"
	if err := Record(entry); err != nil {
		t.Fatalf("Record upsert: %v", err)
	}

	entries, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	e := entries[0]
	if !e.FirstSeen.Equal(firstSeen) {
		t.Errorf("first_seen changed: got %v, want %v", e.FirstSeen, firstSeen)
	}
	if !e.Dismissed {
		t.Errorf("dismissed should be preserved (true)")
	}
	if e.Title != "title v2" {
		t.Errorf("title not updated: got %q", e.Title)
	}
	if e.Detail != "detail v2" {
		t.Errorf("detail not updated: got %q", e.Detail)
	}
	if !e.LastSeen.After(firstSeen) {
		t.Errorf("last_seen not updated")
	}
}

func TestClear_RemovesEntry(t *testing.T) {
	setOverride(t)
	if err := Record(Entry{Key: "k1", Type: "allow_auto_merge", Title: "K1"}); err != nil {
		t.Fatal(err)
	}
	if err := Record(Entry{Key: "k2", Type: "stage_drift", Title: "K2"}); err != nil {
		t.Fatal(err)
	}
	// Dismiss k1 — Clear should still remove it.
	if err := Dismiss("k1"); err != nil {
		t.Fatal(err)
	}
	if err := Clear("k1"); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	entries, _ := Load()
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry after clear, got %d", len(entries))
	}
	if entries[0].Key != "k2" {
		t.Errorf("expected k2 remaining, got %q", entries[0].Key)
	}
}

func TestDismiss_SetsDismissed(t *testing.T) {
	setOverride(t)
	if err := Record(Entry{Key: "k1", Type: "allow_auto_merge", Title: "K1"}); err != nil {
		t.Fatal(err)
	}
	if err := Dismiss("k1"); err != nil {
		t.Fatalf("Dismiss: %v", err)
	}
	entries, _ := Load()
	if len(entries) != 1 || !entries[0].Dismissed {
		t.Errorf("expected dismissed=true, got entries=%v", entries)
	}
}

func TestUndismiss_ClearsDismissed(t *testing.T) {
	setOverride(t)
	if err := Record(Entry{Key: "k1", Type: "allow_auto_merge", Title: "K1"}); err != nil {
		t.Fatal(err)
	}
	if err := Dismiss("k1"); err != nil {
		t.Fatal(err)
	}
	if err := Undismiss("k1"); err != nil {
		t.Fatalf("Undismiss: %v", err)
	}
	entries, _ := Load()
	if len(entries) != 1 || entries[0].Dismissed {
		t.Errorf("expected dismissed=false after undismiss, got %v", entries)
	}
}

func TestLoad_MissingFile(t *testing.T) {
	setOverride(t)
	// File doesn't exist yet.
	entries, err := Load()
	if err != nil {
		t.Errorf("expected nil error for missing file, got %v", err)
	}
	if entries != nil {
		t.Errorf("expected nil entries for missing file, got %v", entries)
	}
}

func TestLoad_CorruptFile(t *testing.T) {
	setOverride(t)
	path := WarningsPathOverride
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not valid json"), 0600); err != nil {
		t.Fatal(err)
	}
	entries, err := Load()
	if err == nil {
		t.Errorf("expected error for corrupt file, got nil")
	}
	if entries != nil {
		t.Errorf("expected nil entries for corrupt file")
	}
}

func TestLoad_VersionMismatch(t *testing.T) {
	setOverride(t)
	path := WarningsPathOverride
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	wf := warningsFile{Version: 99, Entries: nil}
	data, _ := json.Marshal(wf)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	entries, err := Load()
	if err == nil {
		t.Errorf("expected error for version mismatch, got nil")
	}
	if entries != nil {
		t.Errorf("expected nil entries for version mismatch")
	}
}

func TestConcurrentRecord(t *testing.T) {
	setOverride(t)
	var wg sync.WaitGroup
	keys := []string{"key-a", "key-b", "key-c", "key-d"}
	for _, k := range keys {
		k := k
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 5; i++ {
				_ = Record(Entry{Key: k, Type: "allow_auto_merge", Title: k})
			}
		}()
	}
	wg.Wait()

	entries, err := Load()
	if err != nil {
		t.Fatalf("Load after concurrent writes: %v", err)
	}
	if len(entries) != len(keys) {
		t.Errorf("expected %d entries, got %d: %v", len(keys), len(entries), entries)
	}
}
