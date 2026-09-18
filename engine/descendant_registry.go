package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// trackedDescendant records a single process discovered as a session-scoped
// descendant of a Fabrik worker (engine/descendant_reap_unix.go's
// trackWorkerDescendants). It is the durable unit R3's backstop sweep and R2's
// invocation-end reap both consume, and the R5 re-verification unit: Comm and
// LStart together form a fingerprint that must still match the live process at
// PID immediately before any kill, so a PID reused for an unrelated process
// after the original exited is never mistaken for the recorded one.
type trackedDescendant struct {
	PID          int       `json:"pid"`
	Comm         string    `json:"comm"`
	LStart       string    `json:"lstart"`
	WorkerPID    int       `json:"worker_pid"`
	IssueNumber  int       `json:"issue_number"`
	Repo         string    `json:"repo,omitempty"`
	Stage        string    `json:"stage"`
	DiscoveredAt time.Time `json:"discovered_at"`
}

// descendantRegistryMu serializes every load-modify-save cycle against the
// registry file, both within this process (concurrent invocations discovering
// descendants at the same time) and against the R3 sweep goroutine.
var descendantRegistryMu sync.Mutex

// descendantRegistryPath returns the path to the durable descendant registry,
// <cwd>/.fabrik/state/descendants.json — cwd-rooted rather than fabrikDir-
// threaded, mirroring sessionDirForItem/logDirForItem's existing convention
// (fabrikDir is always os.Getwd() per this repo's own architecture notes).
func descendantRegistryPath() string {
	cwd, _ := os.Getwd()
	return filepath.Join(cwd, ".fabrik", "state", "descendants.json")
}

// loadDescendantRegistry reads the registry file, returning an empty slice
// (not an error) when the file does not exist yet — the normal first-run case.
// Callers must hold descendantRegistryMu.
func loadDescendantRegistry() ([]trackedDescendant, error) {
	data, err := os.ReadFile(descendantRegistryPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}
	var entries []trackedDescendant
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, err
	}
	return entries, nil
}

// saveDescendantRegistry atomically writes entries to the registry file,
// creating .fabrik/state/ if necessary. Callers must hold descendantRegistryMu.
func saveDescendantRegistry(entries []trackedDescendant) error {
	path := descendantRegistryPath()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(path, data, 0600)
}

// atomicWriteFile writes data to a temp file in path's own directory and
// renames it into place, so a reader never observes a partially-written file.
// Mirrors internal/githubauth/credentials.go's atomicWriteFile precedent.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once the rename below succeeds
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, perm); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// upsertTrackedDescendant adds or replaces the entry for d.PID in the durable
// registry. Called as each new session-scoped descendant is discovered, so a
// crash between discovery and the R2 invocation-end reap still leaves a
// durable record for the R3 backstop sweep to find (AC3).
func upsertTrackedDescendant(d trackedDescendant) error {
	descendantRegistryMu.Lock()
	defer descendantRegistryMu.Unlock()
	entries, err := loadDescendantRegistry()
	if err != nil {
		return err
	}
	replaced := false
	for i := range entries {
		if entries[i].PID == d.PID {
			entries[i] = d
			replaced = true
			break
		}
	}
	if !replaced {
		entries = append(entries, d)
	}
	return saveDescendantRegistry(entries)
}

// removeTrackedDescendants deletes every entry whose PID is in pids from the
// durable registry. Called once a PID has been reaped (or confirmed stale) so
// steady state carries no entries for processes that no longer need action.
func removeTrackedDescendants(pids []int) error {
	if len(pids) == 0 {
		return nil
	}
	remove := make(map[int]bool, len(pids))
	for _, pid := range pids {
		remove[pid] = true
	}
	descendantRegistryMu.Lock()
	defer descendantRegistryMu.Unlock()
	entries, err := loadDescendantRegistry()
	if err != nil {
		return err
	}
	kept := entries[:0]
	for _, e := range entries {
		if !remove[e.PID] {
			kept = append(kept, e)
		}
	}
	return saveDescendantRegistry(kept)
}

// descendantsForWorker returns every registry entry recorded against workerPID.
func descendantsForWorker(workerPID int) ([]trackedDescendant, error) {
	descendantRegistryMu.Lock()
	defer descendantRegistryMu.Unlock()
	entries, err := loadDescendantRegistry()
	if err != nil {
		return nil, err
	}
	var out []trackedDescendant
	for _, e := range entries {
		if e.WorkerPID == workerPID {
			out = append(out, e)
		}
	}
	return out, nil
}

// allTrackedDescendants returns every entry in the durable registry, across
// all workers (including ones from a prior engine run) — the R3 backstop
// sweep's input set.
func allTrackedDescendants() ([]trackedDescendant, error) {
	descendantRegistryMu.Lock()
	defer descendantRegistryMu.Unlock()
	return loadDescendantRegistry()
}
