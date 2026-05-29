package warnings

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// WarningsPathOverride can be set by tests to redirect I/O to a temp file.
var WarningsPathOverride string

var mu sync.Mutex

// Entry is a single actionable warning surfaced in the TUI Warnings panel.
type Entry struct {
	Key       string            `json:"key"`
	Type      string            `json:"type"`
	Title     string            `json:"title"`
	Detail    string            `json:"detail"`
	FixAction string            `json:"fix_action"`
	FixParams map[string]string `json:"fix_params,omitempty"`
	FirstSeen time.Time         `json:"first_seen"`
	LastSeen  time.Time         `json:"last_seen"`
	Dismissed bool              `json:"dismissed"`
}

type warningsFile struct {
	Version int     `json:"version"`
	Entries []Entry `json:"entries"`
}

const currentVersion = 1

func warningsPath() string {
	if WarningsPathOverride != "" {
		return WarningsPathOverride
	}
	return filepath.Join(".fabrik", "warnings.json")
}

// load reads and parses the warnings file. Missing file returns empty file, nil.
// Corrupt or version-mismatch returns empty file, err.
func load() (warningsFile, error) {
	data, err := os.ReadFile(warningsPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return warningsFile{Version: currentVersion}, nil
		}
		return warningsFile{}, fmt.Errorf("reading warnings file: %w", err)
	}
	var wf warningsFile
	if err := json.Unmarshal(data, &wf); err != nil {
		return warningsFile{}, fmt.Errorf("parsing warnings file: %w", err)
	}
	if wf.Version != currentVersion {
		return warningsFile{}, fmt.Errorf("warnings file version %d not supported (expected %d)", wf.Version, currentVersion)
	}
	return wf, nil
}

// save writes the warningsFile atomically via temp-file-then-rename.
func save(wf warningsFile) error {
	wf.Version = currentVersion
	data, err := json.MarshalIndent(wf, "", "  ")
	if err != nil {
		return fmt.Errorf("marshalling warnings: %w", err)
	}
	p := warningsPath()
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return fmt.Errorf("creating warnings dir: %w", err)
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return fmt.Errorf("writing warnings tmp: %w", err)
	}
	if err := os.Rename(tmp, p); err != nil {
		return fmt.Errorf("renaming warnings tmp: %w", err)
	}
	return nil
}

// Load reads the current warnings entries. Missing file returns nil, nil.
// Corrupt or version-mismatch file returns nil, err.
func Load() ([]Entry, error) {
	mu.Lock()
	defer mu.Unlock()
	wf, err := load()
	if err != nil {
		return nil, err
	}
	if len(wf.Entries) == 0 {
		return nil, nil
	}
	out := make([]Entry, len(wf.Entries))
	copy(out, wf.Entries)
	return out, nil
}

// Record upserts an entry. For new keys: sets first_seen=now, last_seen=now,
// dismissed=false. For existing keys: preserves first_seen and dismissed;
// updates detail and last_seen.
func Record(entry Entry) error {
	mu.Lock()
	defer mu.Unlock()
	wf, err := load()
	if err != nil {
		// On parse error treat as empty so we can still write the new entry.
		wf = warningsFile{Version: currentVersion}
	}
	now := time.Now().UTC()
	found := false
	for i, e := range wf.Entries {
		if e.Key == entry.Key {
			wf.Entries[i].Title = entry.Title
			wf.Entries[i].Detail = entry.Detail
			wf.Entries[i].LastSeen = now
			// Preserve first_seen and dismissed.
			found = true
			break
		}
	}
	if !found {
		entry.FirstSeen = now
		entry.LastSeen = now
		entry.Dismissed = false
		wf.Entries = append(wf.Entries, entry)
	}
	return save(wf)
}

// Clear removes an entry by key regardless of its dismissed state.
func Clear(key string) error {
	mu.Lock()
	defer mu.Unlock()
	wf, err := load()
	if err != nil {
		return err
	}
	filtered := wf.Entries[:0]
	for _, e := range wf.Entries {
		if e.Key != key {
			filtered = append(filtered, e)
		}
	}
	wf.Entries = filtered
	return save(wf)
}

// Dismiss sets dismissed=true for the entry with the given key.
func Dismiss(key string) error {
	mu.Lock()
	defer mu.Unlock()
	wf, err := load()
	if err != nil {
		return err
	}
	for i, e := range wf.Entries {
		if e.Key == key {
			wf.Entries[i].Dismissed = true
			return save(wf)
		}
	}
	return nil
}

// Undismiss sets dismissed=false for the entry with the given key.
func Undismiss(key string) error {
	mu.Lock()
	defer mu.Unlock()
	wf, err := load()
	if err != nil {
		return err
	}
	for i, e := range wf.Entries {
		if e.Key == key {
			wf.Entries[i].Dismissed = false
			return save(wf)
		}
	}
	return nil
}
