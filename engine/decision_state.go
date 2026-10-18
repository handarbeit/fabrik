package engine

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// Decision state that survives a restart (#2349, ADR 2349).
//
// Three pieces of engine-private state change decisions — the merge-train caps (ejection
// count, trial-rate window, auto-repair attempts) and the account-wide Claude usage-limit
// hold — and used to be memory only, so a frequently restarted daemon ran past the caps and
// dispatched into a limit it had already hit. They are persisted here, under
// .fabrik/state/engine-decisions/, in small versioned JSON files written atomically
// (tmp+rename, mode 0600) in the manner of the merge-train run record.
//
// The directory is deliberately not .fabrik/state/merge-train/: trainRunStore.loadDir treats
// every *.json there as a run record, and a run record is per-episode while these counters
// must outlive episodes (an ejected member stays Queued).
//
// A missing, corrupt or unknown-version file is treated as absent (corrupt ones are renamed
// aside to <file>.corrupt) and is never fatal. A failed write is logged and costs only the
// restart-survival of that update.

const (
	decisionStateVersion = 1
	trainCapsFileName    = "train-caps.json"
	usageLimitFileName   = "usage-limit.json"
)

// decisionStore is the directory the decision-state files live in. A nil store (every
// NewWithDeps engine) persists nothing.
type decisionStore struct {
	dir string
}

func newDecisionStore(dir string) *decisionStore {
	if dir == "" {
		return nil
	}
	return &decisionStore{dir: dir}
}

// decisionStateDir is where the production engine keeps its decision state.
func decisionStateDir(fabrikDir string) string {
	return filepath.Join(fabrikDir, ".fabrik", "state", "engine-decisions")
}

// save writes v (which must carry its own Version) atomically.
func (s *decisionStore) save(name string, v any) error {
	if s == nil {
		return nil
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding %s: %w", name, err)
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", s.dir, err)
	}
	if err := atomicWriteFile(filepath.Join(s.dir, name), data, 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", name, err)
	}
	return nil
}

// remove deletes the named file (no-op when absent).
func (s *decisionStore) remove(name string) {
	if s == nil {
		return
	}
	_ = os.Remove(filepath.Join(s.dir, name))
}

// load decodes the named file into v and reports whether a usable file was found. version
// extracts the decoded file's version. An unreadable-as-JSON or wrong-version file is
// quarantined to <file>.corrupt and reported absent; note then describes why.
func (s *decisionStore) load(name string, v any, version func() int) (found bool, note string) {
	if s == nil {
		return false, ""
	}
	path := filepath.Join(s.dir, name)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, ""
		}
		return false, fmt.Sprintf("cannot read %s: %v", path, err)
	}
	derr := json.Unmarshal(data, v)
	if derr == nil && version() == decisionStateVersion {
		return true, ""
	}
	why := "unknown version"
	if derr != nil {
		why = derr.Error()
	}
	if qerr := os.Rename(path, path+".corrupt"); qerr != nil {
		return false, fmt.Sprintf("%s unusable (%s) and could not be quarantined: %v", path, why, qerr)
	}
	return false, fmt.Sprintf("%s unusable (%s) — quarantined as %s.corrupt, starting fresh", path, why, path)
}

// ─── train caps ───

// ejectionRecord is one member's ejection count and the PR head it was earned against.
type ejectionRecord struct {
	Count int    `json:"count"`
	Head  string `json:"head,omitempty"`
}

// trainCapsFile is the persisted merge-train cap state.
type trainCapsFile struct {
	Version   int                         `json:"version"`
	Ejections map[string]ejectionRecord   `json:"ejections,omitempty"`
	Trials    map[string][]time.Time      `json:"trials,omitempty"`
	Attempts  map[string][]repairAttempt  `json:"repair_attempts,omitempty"`
	Requeued  map[string][]string         `json:"repair_requeued,omitempty"`
}

// decisionPersist serialises the persist step so concurrent mutations converge on the
// newest in-memory state (last write wins) without any disk I/O under a hot lock. Lock order:
// decisionWriteMu → (the state's own mutex); no holder of an inner mutex calls persist.
type decisionPersist struct {
	mu sync.Mutex
}

// persistTrainCaps snapshots the ejection, trial and auto-repair state and writes it.
// Callers invoke it after releasing the state's own mutex.
func (e *Engine) persistTrainCaps() {
	if e.decisionStore == nil {
		return
	}
	e.decisionWrite.mu.Lock()
	defer e.decisionWrite.mu.Unlock()

	f := trainCapsFile{Version: decisionStateVersion}

	e.mergeTrainEjectionsMu.Lock()
	for k, n := range e.mergeTrainEjectionCounts {
		if n <= 0 {
			continue
		}
		if f.Ejections == nil {
			f.Ejections = make(map[string]ejectionRecord)
		}
		f.Ejections[k] = ejectionRecord{Count: n, Head: e.mergeTrainEjectionHeads[k]}
	}
	e.mergeTrainEjectionsMu.Unlock()

	_, window := e.effectiveTrialWindow()
	cutoff := e.now().Add(-window)
	e.mergeTrainTrialsMu.Lock()
	for k, ts := range e.mergeTrainTrials {
		var kept []time.Time
		for _, t := range ts {
			if !t.Before(cutoff) {
				kept = append(kept, t)
			}
		}
		if len(kept) == 0 {
			continue
		}
		if f.Trials == nil {
			f.Trials = make(map[string][]time.Time)
		}
		f.Trials[k] = kept
	}
	e.mergeTrainTrialsMu.Unlock()

	s := &e.autoRepair
	s.mu.Lock()
	for k, a := range s.attempts {
		if len(a) == 0 {
			continue
		}
		if f.Attempts == nil {
			f.Attempts = make(map[string][]repairAttempt)
		}
		f.Attempts[k] = append([]repairAttempt(nil), a...)
	}
	for k, r := range s.requeued {
		if len(r) == 0 {
			continue
		}
		if f.Requeued == nil {
			f.Requeued = make(map[string][]string)
		}
		f.Requeued[k] = append([]string(nil), r...)
	}
	s.mu.Unlock()

	if err := e.decisionStore.save(trainCapsFileName, &f); err != nil {
		e.logf(0, "decision-state", "warn: could not persist merge-train caps: %v\n", err)
	}
}

// loadTrainCaps restores the persisted merge-train cap state. Called once before the first
// poll; a missing or unusable file leaves today's empty state.
func (e *Engine) loadTrainCaps() {
	var f trainCapsFile
	found, note := e.decisionStore.load(trainCapsFileName, &f, func() int { return f.Version })
	if note != "" {
		e.logf(0, "decision-state", "warn: %s\n", note)
	}
	if !found {
		return
	}
	e.mergeTrainEjectionsMu.Lock()
	for k, r := range f.Ejections {
		if r.Count <= 0 {
			continue
		}
		e.mergeTrainEjectionCounts[k] = r.Count
		if r.Head != "" {
			e.mergeTrainEjectionHeads[k] = r.Head
		}
	}
	e.mergeTrainEjectionsMu.Unlock()

	_, window := e.effectiveTrialWindow()
	cutoff := e.now().Add(-window)
	e.mergeTrainTrialsMu.Lock()
	for k, ts := range f.Trials {
		for _, t := range ts {
			if !t.Before(cutoff) {
				e.mergeTrainTrials[k] = append(e.mergeTrainTrials[k], t)
			}
		}
	}
	e.mergeTrainTrialsMu.Unlock()

	s := &e.autoRepair
	s.mu.Lock()
	if len(f.Attempts) > 0 && s.attempts == nil {
		s.attempts = make(map[string][]repairAttempt)
	}
	for k, a := range f.Attempts {
		s.attempts[k] = a
	}
	if len(f.Requeued) > 0 && s.requeued == nil {
		s.requeued = make(map[string][]string)
	}
	for k, r := range f.Requeued {
		s.requeued[k] = r
	}
	s.mu.Unlock()
}

// ─── usage-limit hold ───

// usageLimitFile is the persisted account-wide usage-limit deadline.
type usageLimitFile struct {
	Version int       `json:"version"`
	Until   time.Time `json:"until"`
}

// persistUsageLimit writes the current hold deadline, or removes the file when no hold is
// set. Callers invoke it after releasing claudeUsageLimitMu.
func (e *Engine) persistUsageLimit() {
	if e.decisionStore == nil {
		return
	}
	e.decisionWrite.mu.Lock()
	defer e.decisionWrite.mu.Unlock()
	e.claudeUsageLimitMu.Lock()
	until := e.claudeUsageLimitedUntil
	e.claudeUsageLimitMu.Unlock()
	if until.IsZero() {
		e.decisionStore.remove(usageLimitFileName)
		return
	}
	if err := e.decisionStore.save(usageLimitFileName, &usageLimitFile{Version: decisionStateVersion, Until: until.UTC()}); err != nil {
		e.logf(0, "decision-state", "warn: could not persist usage-limit hold: %v\n", err)
	}
}

// loadUsageLimit restores the persisted hold. An expired deadline is dropped (and the file
// removed); one beyond the live hold's ceiling is clamped to it. When a usable file exists it
// is authoritative: usageLimitFileAtStart is set so the label-based seeding never runs.
func (e *Engine) loadUsageLimit() {
	var f usageLimitFile
	found, note := e.decisionStore.load(usageLimitFileName, &f, func() int { return f.Version })
	if note != "" {
		e.logf(0, "decision-state", "warn: %s\n", note)
	}
	if !found {
		return
	}
	now := e.now()
	e.usageLimitFileAtStart = true
	if !now.Before(f.Until) {
		e.decisionStore.remove(usageLimitFileName)
		return
	}
	until := f.Until
	if ceiling := now.Add(usageLimitMaxResetHorizon); until.After(ceiling) {
		until = ceiling
	}
	e.claudeUsageLimitMu.Lock()
	e.claudeUsageLimitedUntil = until
	e.claudeUsageLimitMu.Unlock()
	e.logf(0, "claude-limit", "restored account-wide Claude dispatch hold until %s after restart\n", until.Format(time.RFC3339))
}

// ─── lifecycle ───

// loadDecisionState restores every persisted decision-state file. Called from New() before
// the first poll so the label sweep and dispatch gates see the restored state.
func (e *Engine) loadDecisionState() {
	if e.decisionStore == nil {
		return
	}
	e.loadTrainCaps()
	e.loadUsageLimit()
}

// EnableDecisionStateForTest turns on decision-state persistence under dir and loads whatever
// is there. Two engines pointed at the same dir model a daemon restart. Test-only;
// production gets the store from New().
func (e *Engine) EnableDecisionStateForTest(dir string) {
	e.decisionStore = newDecisionStore(dir)
	e.loadDecisionState()
}

// pruneDecisionState drops decision state for items that are closed or no longer on the board
// (FR-003) so persisted counters never accumulate: ejection counts and heads, auto-repair
// history and the feedback-gap budgets. An empty board is treated as unreadable and prunes
// nothing.
func (e *Engine) pruneDecisionState(board *gh.ProjectBoard) {
	if board == nil || len(board.Items) == 0 {
		return
	}
	live := make(map[string]bool, len(board.Items))
	for _, it := range board.Items {
		if it.IsClosed {
			continue
		}
		live[issueKey(it, e.defaultRepo())] = true
	}

	changed := false
	e.mergeTrainEjectionsMu.Lock()
	for k := range e.mergeTrainEjectionCounts {
		if !live[k] {
			delete(e.mergeTrainEjectionCounts, k)
			delete(e.mergeTrainEjectionHeads, k)
			changed = true
		}
	}
	for k := range e.mergeTrainEjectionHeads {
		if !live[k] {
			delete(e.mergeTrainEjectionHeads, k)
		}
	}
	e.mergeTrainEjectionsMu.Unlock()

	s := &e.autoRepair
	s.mu.Lock()
	for k := range s.attempts {
		if !live[k] {
			delete(s.attempts, k)
			changed = true
		}
	}
	for k := range s.requeued {
		if !live[k] {
			delete(s.requeued, k)
			changed = true
		}
	}
	s.mu.Unlock()

	e.feedbackGapMu.Lock()
	for k := range e.feedbackGaps {
		if !live[k] {
			delete(e.feedbackGaps, k)
		}
	}
	e.feedbackGapMu.Unlock()

	if changed {
		e.persistTrainCaps()
	}
}
