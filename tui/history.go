// Copyright (c) 2026 Fabrik Contributors. All rights reserved.

package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
)

// HistoryPathOverride can be set by tests to redirect history I/O to a temp file.
var HistoryPathOverride string

// historyPath returns the path to the persistent history file.
// Uses .fabrik/history.json in the current working directory so each
// project has its own history.
func historyPath() string {
	if HistoryPathOverride != "" {
		return HistoryPathOverride
	}
	return filepath.Join(".fabrik", "history.json")
}

// historyDedupKey is the composite key used to identify duplicate history entries.
type historyDedupKey struct {
	IssueNumber int
	Repo        string
	StageName   string
	IsComment   bool
}

// deduplicateHistory collapses duplicate entries by (IssueNumber, Repo, StageName, IsComment),
// keeping the most recent entry by CompletedAt. Entries for different stages on the same
// issue are preserved — that is expected multi-stage pipeline behavior.
// The returned slice is sorted by CompletedAt ascending (oldest first).
func deduplicateHistory(entries []HistoryEntry) []HistoryEntry {
	// Build a map: key → most recent entry seen so far.
	best := make(map[historyDedupKey]HistoryEntry, len(entries))
	for _, e := range entries {
		k := historyDedupKey{
			IssueNumber: e.IssueNumber,
			Repo:        e.Repo,
			StageName:   e.StageName,
			IsComment:   e.IsComment,
		}
		if prev, exists := best[k]; !exists || e.CompletedAt.After(prev.CompletedAt) {
			best[k] = e
		}
	}
	// Reconstruct the slice in original order (iterate entries, keep winners).
	seen := make(map[historyDedupKey]bool, len(best))
	out := make([]HistoryEntry, 0, len(best))
	for _, e := range entries {
		k := historyDedupKey{
			IssueNumber: e.IssueNumber,
			Repo:        e.Repo,
			StageName:   e.StageName,
			IsComment:   e.IsComment,
		}
		if !seen[k] && best[k].CompletedAt.Equal(e.CompletedAt) {
			out = append(out, e)
			seen[k] = true
		}
	}
	// Sort ascending by CompletedAt so newest appears at the bottom.
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].CompletedAt.Before(out[j].CompletedAt)
	})
	return out
}

// LoadHistory reads saved history entries from disk.
// Returns nil (not an error) if the file doesn't exist.
// Duplicate entries (same issue, repo, stage, and IsComment flag) are collapsed
// to the most recent, providing a defensive safeguard against dispatch loops.
func LoadHistory() []HistoryEntry {
	data, err := os.ReadFile(historyPath())
	if err != nil {
		return nil
	}
	var entries []HistoryEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil
	}
	return deduplicateHistory(entries)
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
