package warnings

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// WarningsPathOverride can be set by tests to redirect I/O to a temp file.
var WarningsPathOverride string

// processDefaultPath, when set, replaces the working-directory default for the
// whole process (see SetProcessDefaultPath); WarningsPathOverride still wins.
var processDefaultPath string

var mu sync.Mutex

// SetProcessDefaultPath redirects the default warnings file for the life of the
// process. It exists for a test package's TestMain (via internal/testenv), so a
// recorder that becomes reachable from an existing test never writes a real
// .fabrik/warnings.json into the package directory, even after a test that set
// and then cleared WarningsPathOverride. Never call it from production code.
func SetProcessDefaultPath(p string) { processDefaultPath = p }

// Entry is a single actionable warning surfaced in the TUI Warnings panel.
type Entry struct {
	Key       string            `json:"key"`
	Type      string            `json:"type"`
	Title     string            `json:"title"`
	Detail    string            `json:"detail"`
	FixAction string            `json:"fix_action"`
	FixParams map[string]string `json:"fix_params,omitempty"`
	// Fingerprint is the stable identity of the condition for dismissal: a
	// dismissed entry stays dismissed while its fingerprint is unchanged and
	// reappears when it changes. Empty means "sticky" — Record keeps whatever
	// fingerprint the stored entry already has, so a caller that does not know
	// one (e.g. the TUI's fix-failed re-record) never un-dismisses an entry.
	Fingerprint string    `json:"fingerprint,omitempty"`
	FirstSeen   time.Time `json:"first_seen"`
	LastSeen    time.Time `json:"last_seen"`
	Dismissed   bool      `json:"dismissed"`
}

type warningsFile struct {
	Version int     `json:"version"`
	Entries []Entry `json:"entries"`
}

const currentVersion = 1

// lastSeenRefresh is how stale an unchanged entry's LastSeen may get before
// Record rewrites the file just to refresh it. A condition re-recorded every
// poll therefore costs no file write (and no TUI reload) while nothing changes.
// A var so tests can shorten it.
var lastSeenRefresh = 10 * time.Minute

func warningsPath() string {
	if WarningsPathOverride != "" {
		return WarningsPathOverride
	}
	if processDefaultPath != "" {
		return processDefaultPath
	}
	return filepath.Join(".fabrik", "warnings.json")
}

// Path returns the resolved warnings file path. Used by the TUI for mtime polling.
func Path() string {
	return warningsPath()
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
	// A unique temp name keeps concurrent processes (the daemon, the TUI's fix
	// action, fabrik upgrade) from clobbering each other's half-written file.
	f, err := os.CreateTemp(filepath.Dir(p), filepath.Base(p)+".*.tmp")
	if err != nil {
		return fmt.Errorf("creating warnings tmp: %w", err)
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("writing warnings tmp: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("closing warnings tmp: %w", err)
	}
	if err := os.Chmod(tmp, 0600); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("chmod warnings tmp: %w", err)
	}
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
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
// dismissed=false. For existing keys: preserves first_seen; updates the
// content and last_seen. Dismissed is preserved unless the entry's Fingerprint
// changed (both old and new non-empty and different) — then the condition is
// materially different and the entry reappears. An empty new Fingerprint keeps
// the stored one. When nothing material changed and LastSeen is younger than
// lastSeenRefresh, the file is not rewritten.
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
			entry.FirstSeen = e.FirstSeen
			entry.Dismissed = e.Dismissed
			if entry.Fingerprint == "" {
				entry.Fingerprint = e.Fingerprint
			} else if e.Fingerprint != "" && e.Fingerprint != entry.Fingerprint {
				entry.Dismissed = false
			}
			if sameContent(e, entry) && e.Dismissed == entry.Dismissed && now.Sub(e.LastSeen) < lastSeenRefresh {
				return nil
			}
			entry.LastSeen = now
			wf.Entries[i] = entry
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

func sameContent(a, b Entry) bool {
	if a.Type != b.Type || a.Title != b.Title || a.Detail != b.Detail ||
		a.FixAction != b.FixAction || a.Fingerprint != b.Fingerprint ||
		len(a.FixParams) != len(b.FixParams) {
		return false
	}
	for k, v := range a.FixParams {
		if bv, ok := b.FixParams[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

// Clear removes an entry by key regardless of its dismissed state. It does not
// write the file when the key is absent.
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
	if len(filtered) == len(wf.Entries) {
		return nil
	}
	wf.Entries = filtered
	return save(wf)
}

// ClearMissing removes every entry of the given warningType whose subject —
// the portion of Key after the "<warningType>:" prefix — is not a key in
// present. It is the bulk counterpart to Clear, built for settle scans that
// compare a set of recorded warnings against an already-known-good set (e.g.
// the current board repos, or the current stage names): rather than looping
// per-key calls to Clear (which always calls save() even when the key was
// never present), ClearMissing loads once, filters in memory, and only saves
// when at least one entry was actually removed — so a poll with nothing
// stale performs zero writes. Entries of other Types are never touched,
// regardless of their Key. Dismissed is not special-cased: a stale entry is
// removed whether or not it was dismissed, matching Clear's behavior; a
// present-subject entry (dismissed or not) is always preserved. Returns the
// full Key of each cleared entry, for caller-side logging.
func ClearMissing(warningType string, present map[string]bool) ([]string, error) {
	mu.Lock()
	defer mu.Unlock()
	wf, err := load()
	if err != nil {
		return nil, err
	}
	prefix := warningType + ":"
	var cleared []string
	filtered := wf.Entries[:0]
	for _, e := range wf.Entries {
		if e.Type == warningType && !present[strings.TrimPrefix(e.Key, prefix)] {
			cleared = append(cleared, e.Key)
			continue
		}
		filtered = append(filtered, e)
	}
	if len(cleared) == 0 {
		return nil, nil
	}
	wf.Entries = filtered
	if err := save(wf); err != nil {
		return nil, err
	}
	return cleared, nil
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
