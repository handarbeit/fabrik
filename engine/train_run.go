package engine

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// ─── Persisted merge-train run (#2051, ADR 2051) ────────────────────────────────────
//
// A "run" is one merge-train episode for one (repo, base) partition. Until #2051 it
// lived entirely in a worker goroutine's locals; now the state that decides what
// happens next is data — trainRunRecord — kept by the trainRunStore and, when the
// store has a directory, written through to .fabrik/state/merge-train/ so a daemon
// restart resumes the same trial or bisection position instead of rebuilding.
//
// GitHub remains the validator, not the source: a record is only trusted at adoption
// time after the trial PR, the members and the pinned base have been re-checked
// (validateRun, train_run_poll.go). Anything unreadable, unknown-version or
// inconsistent is discarded and the partition forms fresh through the unchanged
// reconstructTrainState routes — a record can degrade a restart to today's
// behaviour, never wedge a partition.

// trainRunVersion is the on-disk schema version. A file with any other version is
// treated as absent (quarantined), never fatal.
const trainRunVersion = 1

// Steps of the run state machine (the record's persisted position).
const (
	// stepForm: at the top of the re-form loop — (re)build the batch's trial.
	stepForm = "form"
	// stepMain: the batch's trial is open and its CI is being awaited.
	stepMain = "trial-ci"
	// stepBisect: between bisection half-trials (the next half is not open yet).
	stepBisect = "bisect"
	// stepHalf: a bisection half-trial is open and its CI is being awaited.
	stepHalf = "bisect-trial"
	// stepOAT: between one-at-a-time singleton trials.
	stepOAT = "one-at-a-time"
	// stepSingle: a one-at-a-time singleton trial is open and awaited.
	stepSingle = "one-at-a-time-trial"
	// stepLanding: a landing path is executing inside a step goroutine. A restart here
	// drops the record; the durable reconstructTrainState routes finish the landing.
	stepLanding = "landing"
)

// Phase names exposed by TrainRunPhases (the spec's R1/R5 vocabulary).
const (
	runPhaseTrialCI    = "trial-ci"
	runPhaseBisect     = "bisect"
	runPhaseLanding    = "landing"
	runPhaseOneAtATime = "one-at-a-time"
)

// runMemberRecord is the persisted identity of one train member. The board item itself
// is not stored — it is re-attached from the live board snapshot at adoption, so a
// stale title or label set can never be acted on.
type runMemberRecord struct {
	Number       int    `json:"number"`
	PRNum        int    `json:"pr"`
	HeadSHA      string `json:"head_sha"`
	CaughtUpFrom string `json:"caught_up_from,omitempty"`
}

// runTrialRecord is the one open trial (assembled, draft CI PR open) of a run.
type runTrialRecord struct {
	Kind     string          `json:"kind"` // main | half | single
	Name     string          `json:"name"`
	HeadSHA  string          `json:"head_sha"`
	PRNum    int             `json:"pr"`
	Members  []int           `json:"members"` // survivors of assembly
	OpenedAt time.Time       `json:"opened_at"`
	Deadline time.Time       `json:"deadline"`
	CI       trialCIStateRec `json:"ci"`
}

// runOATRecord is the one-at-a-time fallback position.
type runOATRecord struct {
	Members []int `json:"members"` // the full fallback list, in order
	Index   int   `json:"index"`   // the member being (or next to be) processed
}

// trainRunRecord is the whole persisted run.
type trainRunRecord struct {
	Version       int       `json:"version"`
	TrainKey      string    `json:"train_key"`
	Owner         string    `json:"owner"`
	Repo          string    `json:"repo"`
	PartitionBase string    `json:"partition_base"`
	BaseBranch    string    `json:"base_branch"`
	BaseSHA       string    `json:"base_sha"`
	ProjectID     string    `json:"project_id"`
	BaseTrialName string    `json:"base_trial_name"`
	TrialSeq      int       `json:"trial_seq"`
	StartedAt     time.Time `json:"started_at"`
	WrittenAt     time.Time `json:"written_at"`

	Step           string    `json:"step"`
	PhaseStartedAt time.Time `json:"phase_started_at"`

	// Original is the dispatched batch (the "M" in the TUI row's "N of M").
	Original []int `json:"original"`
	// Members is every member a step may still reference, with the identity needed to
	// rebuild a trainMember.
	Members []runMemberRecord `json:"members"`
	// Current is the main loop's `current` set (the members the next main trial forms
	// from, or — while a main trial is open — the set that trial was formed from).
	Current []int `json:"current"`

	Trial  *runTrialRecord `json:"trial,omitempty"`
	Bisect *bisectState    `json:"bisect,omitempty"`
	OAT    *runOATRecord   `json:"oat,omitempty"`

	// Ejected is every member that assembly ejected from a trial of this run (an
	// unresolvable conflict reroutes it off Queued). It still appears in Current /
	// Bisect.Origin — those are the pre-assembly sets — so adoption must not require it to
	// be Queued, or a restart after an assembly-time ejection would discard a good trial.
	Ejected []int `json:"ejected,omitempty"`

	// Poisoner is the write-ahead mark of an ejection about to happen: set (and
	// persisted) BEFORE ejectMember so a restart finishes the ejection exactly once.
	Poisoner int `json:"poisoner,omitempty"`
}

// ─── JSON-safe mirrors of the per-trial CI state (#2052 / #2072 bookkeeping) ───

type startupWatchRec struct {
	Retriggers    int            `json:"retriggers"`
	Baseline      []int64        `json:"baseline,omitempty"`
	RetriggeredAt time.Time      `json:"retriggered_at"`
	LastFailed    gh.WorkflowRun `json:"last_failed"`
}

type rerunStateRec struct {
	Done           bool      `json:"done"`
	RunIDs         []int64   `json:"run_ids,omitempty"`
	FirstFailedIDs []int64   `json:"first_failed_ids,omitempty"`
	At             time.Time `json:"at"`
}

type trialCIStateRec struct {
	SW             startupWatchRec `json:"sw"`
	RR             rerunStateRec   `json:"rr"`
	ActionsRefused bool            `json:"actions_refused,omitempty"`
}

func sortedIDs(m map[int64]bool) []int64 {
	if len(m) == 0 {
		return nil
	}
	out := make([]int64, 0, len(m))
	for id := range m {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func idSet(ids []int64) map[int64]bool {
	if len(ids) == 0 {
		return nil
	}
	m := make(map[int64]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	return m
}

// record returns the persistable form of the evaluator's memory.
func (s *trialCIState) record() trialCIStateRec {
	return trialCIStateRec{
		SW: startupWatchRec{
			Retriggers:    s.sw.retriggers,
			Baseline:      sortedIDs(s.sw.baseline),
			RetriggeredAt: s.sw.retriggeredAt,
			LastFailed:    s.sw.lastFailed,
		},
		RR: rerunStateRec{
			Done:           s.rr.done,
			RunIDs:         append([]int64(nil), s.rr.runIDs...),
			FirstFailedIDs: sortedIDs(s.rr.firstFailedIDs),
			At:             s.rr.at,
		},
		ActionsRefused: s.actionsRefused,
	}
}

// restore loads the evaluator's memory from its persisted form.
func (s *trialCIState) restore(r trialCIStateRec) {
	s.sw = startupWatch{
		retriggers:    r.SW.Retriggers,
		baseline:      idSet(r.SW.Baseline),
		retriggeredAt: r.SW.RetriggeredAt,
		lastFailed:    r.SW.LastFailed,
	}
	s.rr = rerunState{
		done:           r.RR.Done,
		runIDs:         append([]int64(nil), r.RR.RunIDs...),
		firstFailedIDs: idSet(r.RR.FirstFailedIDs),
		at:             r.RR.At,
	}
	s.actionsRefused = r.ActionsRefused
}

// ─── the store ───

// trainRunStore is the engine's table of merge-train runs. It is nil on engines that
// do not run the asynchronous driver (every NewWithDeps engine), in which case
// runMergeTrainWorker is the synchronous driver and nothing here is touched.
//
// dir == "" keeps records in memory only (unit tests of the async driver); a non-empty
// dir writes each record through, atomically, to <dir>/<hash(trainKey)>.json.
type trainRunStore struct {
	mu   sync.Mutex
	dir  string
	live map[string]*trainRun // runs with a goroutine-free or goroutine-driven life in this process

	// pending are records loaded from disk (or handed over by a test) that have not been
	// adopted yet — adoption needs the board snapshot, so it happens in settleTrainRuns.
	pending map[string]*trainRunRecord
}

func newTrainRunStore(dir string) *trainRunStore {
	return &trainRunStore{dir: dir, live: map[string]*trainRun{}, pending: map[string]*trainRunRecord{}}
}

func trainRunFile(dir, trainKey string) string {
	sum := sha256.Sum256([]byte(trainKey))
	return filepath.Join(dir, fmt.Sprintf("%x.json", sum[:8]))
}

// write persists rec (no-op without a directory). Errors are returned for the caller to
// log; a failed write never stops the train — it only costs the restart-resume.
func (s *trainRunStore) write(rec *trainRunRecord) error {
	if s == nil || s.dir == "" {
		return nil
	}
	rec.WrittenAt = time.Now().UTC()
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding train run %s: %w", rec.TrainKey, err)
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", s.dir, err)
	}
	if err := atomicWriteFile(trainRunFile(s.dir, rec.TrainKey), data, 0o600); err != nil {
		return fmt.Errorf("writing train run %s: %w", rec.TrainKey, err)
	}
	return nil
}

// remove deletes trainKey's file (no-op without a directory or a file).
func (s *trainRunStore) remove(trainKey string) {
	if s == nil || s.dir == "" {
		return
	}
	_ = os.Remove(trainRunFile(s.dir, trainKey))
}

// loadDir reads every record in the directory into pending. A file that cannot be
// decoded, or has an unknown version, is renamed aside to <file>.corrupt and treated as
// absent — the partition then forms fresh. It returns one message per quarantined file
// for the caller to log.
func (s *trainRunStore) loadDir() []string {
	if s == nil || s.dir == "" {
		return nil
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil // absent directory: nothing to resume
	}
	var notes []string
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ent := range entries {
		if ent.IsDir() || filepath.Ext(ent.Name()) != ".json" {
			continue
		}
		path := filepath.Join(s.dir, ent.Name())
		data, rerr := os.ReadFile(path)
		var rec trainRunRecord
		var derr error
		if rerr == nil {
			derr = json.Unmarshal(data, &rec)
		}
		switch {
		case rerr != nil:
			notes = append(notes, fmt.Sprintf("cannot read %s: %v", path, rerr))
			continue
		case derr != nil || rec.Version != trainRunVersion || rec.TrainKey == "":
			why := "unknown version"
			if derr != nil {
				why = derr.Error()
			}
			if qerr := os.Rename(path, path+".corrupt"); qerr != nil {
				notes = append(notes, fmt.Sprintf("%s unusable (%s) and could not be quarantined: %v", path, why, qerr))
			} else {
				notes = append(notes, fmt.Sprintf("%s unusable (%s) — quarantined as %s.corrupt, forming fresh", path, why, path))
			}
			continue
		}
		if _, running := s.live[rec.TrainKey]; running {
			continue // a run in this process already owns the partition; its record is current
		}
		s.pending[rec.TrainKey] = &rec
	}
	return notes
}

// hasPending reports whether trainKey has a loaded-but-unadopted record, in which case
// dispatch must not form a fresh train for the partition yet.
func (s *trainRunStore) hasPending(trainKey string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.pending[trainKey]
	return ok
}

func (s *trainRunStore) liveRuns() []*trainRun {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*trainRun, 0, len(s.live))
	for _, r := range s.live {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].trainKey < out[j].trainKey })
	return out
}

func (s *trainRunStore) liveFor(trainKey string) *trainRun {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.live[trainKey]
}

// dropPending forgets an unadopted record without touching its file (the partition's
// current owner rewrites or removes the file itself).
func (s *trainRunStore) dropPending(trainKey string) {
	s.mu.Lock()
	delete(s.pending, trainKey)
	s.mu.Unlock()
}

func (s *trainRunStore) register(r *trainRun) {
	s.mu.Lock()
	s.live[r.trainKey] = r
	delete(s.pending, r.trainKey)
	s.mu.Unlock()
}

// release forgets the in-memory run but leaves its record on disk, for a step that was
// cancelled by shutdown: the next daemon adopts the record.
func (s *trainRunStore) release(trainKey string) {
	s.mu.Lock()
	delete(s.live, trainKey)
	delete(s.pending, trainKey)
	s.mu.Unlock()
}

func (s *trainRunStore) unregister(trainKey string) {
	s.mu.Lock()
	delete(s.live, trainKey)
	delete(s.pending, trainKey)
	s.mu.Unlock()
	s.remove(trainKey)
}

// trainRunStateDir is where an engine rooted at fabrikDir keeps run records.
func trainRunStateDir(fabrikDir string) string {
	return filepath.Join(fabrikDir, ".fabrik", "state", "merge-train")
}

// compactDiag returns a copy of d safe to persist: check-run output is bounded so a
// record stays small (the comment renderer truncates harder than this anyway).
func compactDiag(d *trainCIDiagnostic) *trainCIDiagnostic {
	if d == nil {
		return nil
	}
	const maxOut = 6000
	cp := *d
	cp.FailedChecks = make([]gh.CheckRun, len(d.FailedChecks))
	for i, c := range d.FailedChecks {
		c.OutputText = truncateMiddle(c.OutputText, maxOut, maxOut*2/3, maxOut/4)
		c.OutputSummary = truncateMiddle(c.OutputSummary, maxOut, maxOut*2/3, maxOut/4)
		cp.FailedChecks[i] = c
	}
	return &cp
}
