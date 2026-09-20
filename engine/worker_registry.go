package engine

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// workerRecord is the durable, spawn-time record of one Claude worker
// invocation (#1814). Unlike trackedDescendant (one entry per *descendant*,
// written on a polling tick and therefore racy), a worker record is written
// exactly once per invocation, synchronously after cmd.Start(), when the
// worker's PID belongs to exactly one process and no race is possible.
//
// setCmdProcAttr's Setsid:true makes the worker its own session leader, so
// every descendant it ever forks carries the worker's PID as its session ID
// for life (until it calls setsid() itself). A registry-independent sweep can
// therefore find an orphan that was never recorded by enumerating live
// processes and matching Getsid(pid) against the PIDs of recorded, dead
// workers — see sweepOrphanedWorkerSessions.
//
// A record is identified by ID, never by PID: a recycled PID's new worker must
// not overwrite the dead worker's record, since that record is exactly what is
// needed to reap the dead worker's orphans (R8).
type workerRecord struct {
	ID          string    `json:"id"`
	PID         int       `json:"pid"`
	Comm        string    `json:"comm,omitempty"`
	LStart      string    `json:"lstart,omitempty"`
	IssueNumber int       `json:"issue_number"`
	Repo        string    `json:"repo,omitempty"`
	Stage       string    `json:"stage"`
	SpawnedAt   time.Time `json:"spawned_at"`
	// EmptyScans counts consecutive successful periodic scans that found no
	// live member of this dead worker's session. A record is pruned only at
	// >= 2, guarding against a truncated process table (R9).
	EmptyScans int `json:"empty_scans,omitempty"`
}

// workerRecordsMu serializes every load-modify-save cycle against the worker
// record file. Deliberately separate from descendantRegistryMu (#1798): the
// two are never held together, so there is no lock-order to maintain.
var workerRecordsMu sync.Mutex

// workerRecordsPathOverride, when non-empty, replaces the cwd-rooted default
// path. Test-only seam: ~50 existing tests drive runClaude without t.Chdir, and
// would otherwise write engine/.fabrik/state/workers.json into the package
// directory, where stale records (whose PIDs may since have been recycled to
// unrelated processes) would leak into later sweeps. Production never sets it.
var workerRecordsPathOverride string

// workerRecordsPath returns <cwd>/.fabrik/state/workers.json, cwd-rooted like
// descendantRegistryPath.
func workerRecordsPath() string {
	if workerRecordsPathOverride != "" {
		return workerRecordsPathOverride
	}
	cwd, _ := os.Getwd()
	return filepath.Join(cwd, ".fabrik", "state", "workers.json")
}

// loadWorkerRecords reads the record file; a missing file is an empty result.
// Callers must hold workerRecordsMu.
func loadWorkerRecords() ([]workerRecord, error) {
	data, err := os.ReadFile(workerRecordsPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}
	var recs []workerRecord
	if err := json.Unmarshal(data, &recs); err != nil {
		// A corrupt file would otherwise fail every append and every janitor
		// pass, silently disabling the registry-independent sweep until someone
		// deletes it by hand. Quarantine it and continue with an empty set: the
		// records it held are lost (fail open — a leak, never a false kill), but
		// new workers are recorded again.
		path := workerRecordsPath()
		quarantine := path + ".corrupt"
		if rerr := os.Rename(path, quarantine); rerr != nil {
			return nil, fmt.Errorf("worker records unparseable (%v) and could not be quarantined: %w", err, rerr)
		}
		claudeLog(0, "warn", "worker records file %s was unparseable (%v); moved to %s and continuing with an empty set\n", path, err, quarantine)
		return nil, nil
	}
	return recs, nil
}

// saveWorkerRecords atomically writes recs. Callers must hold workerRecordsMu.
func saveWorkerRecords(recs []workerRecord) error {
	path := workerRecordsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(path, data, 0600)
}

// mutateWorkerRecords runs one serialized load-modify-save cycle. fn receives
// the current records and returns the set to persist.
func mutateWorkerRecords(fn func([]workerRecord) []workerRecord) error {
	workerRecordsMu.Lock()
	defer workerRecordsMu.Unlock()
	recs, err := loadWorkerRecords()
	if err != nil {
		return err
	}
	return saveWorkerRecords(fn(recs))
}

// appendWorkerRecord adds r to the durable record file.
func appendWorkerRecord(r workerRecord) error {
	return mutateWorkerRecords(func(recs []workerRecord) []workerRecord {
		return append(recs, r)
	})
}

// removeWorkerRecords deletes every record whose ID is in ids.
func removeWorkerRecords(ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	drop := make(map[string]bool, len(ids))
	for _, id := range ids {
		drop[id] = true
	}
	return mutateWorkerRecords(func(recs []workerRecord) []workerRecord {
		kept := recs[:0]
		for _, r := range recs {
			if !drop[r.ID] {
				kept = append(kept, r)
			}
		}
		return kept
	})
}

// updateWorkerRecord applies fn to the record with the given ID, if present.
func updateWorkerRecord(id string, fn func(*workerRecord)) error {
	return mutateWorkerRecords(func(recs []workerRecord) []workerRecord {
		for i := range recs {
			if recs[i].ID == id {
				fn(&recs[i])
			}
		}
		return recs
	})
}

// allWorkerRecords returns every record, including ones from a prior engine
// run (R4).
func allWorkerRecords() ([]workerRecord, error) {
	workerRecordsMu.Lock()
	defer workerRecordsMu.Unlock()
	return loadWorkerRecords()
}
