package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// HistoryPathOverride can be set by tests to redirect history I/O to a temp file.
var HistoryPathOverride string

// historyPath returns the path to the persistent history file.
func historyPath() string {
	if HistoryPathOverride != "" {
		return HistoryPathOverride
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".fabrik", "history.json")
}

// LoadHistory reads saved history entries from disk.
// Returns nil (not an error) if the file doesn't exist.
func LoadHistory() []HistoryEntry {
	data, err := os.ReadFile(historyPath())
	if err != nil {
		return nil
	}
	var entries []HistoryEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil
	}
	return entries
}

// SaveHistory writes history entries to disk.
func SaveHistory(entries []HistoryEntry) {
	data, err := json.Marshal(entries)
	if err != nil {
		return
	}
	dir := filepath.Dir(historyPath())
	os.MkdirAll(dir, 0700)
	os.WriteFile(historyPath(), data, 0600)
}
