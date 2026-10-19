package engine

import (
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // file-name hashing only, not a security use
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// Recorded stage outcome (#2394, ADR 2394).
//
// A stage run that finished with FABRIK_STAGE_COMPLETE used to leave no durable trace of
// that fact until its completion label landed on GitHub. When the label write failed (the
// forge was unreachable), GitHub and the Store both still showed the stage as not complete
// and the next dispatch ran Claude again on the same inputs — about 150 times in #2388.
//
// finalizeStageOutcome now writes a stageOutcomeRecord before its first GitHub write for a
// completed run. While the record is live the engine does not dispatch that stage again
// (itemMayNeedWork / itemNeedsWorkRetryAt), and settleStageOutcomes retries the publication —
// stage comment, push, completion signal — on every poll until it lands or the record is
// discarded. The record goes under .fabrik/state/stage-outcomes/, one file per item, written
// atomically, a sibling of engine-decisions/ and merge-train/ (the latter's loader
// quarantines every *.json it finds).

const (
	stageOutcomeVersion = 1

	// outcomeKindStage is a finished stage run whose output still has to be published.
	outcomeKindStage = "stage"
	// outcomeKindCompletion is a comment pass that completed the stage but could not write
	// the completion signal: there is no output to publish, only handleStageComplete to retry.
	outcomeKindCompletion = "completion"

	// outcomePhaseRecorded: written before the first GitHub write. The publication payload
	// (final comment text, head after the spec commit) is not yet known, so a record found in
	// this phase after a restart is discarded rather than replayed.
	outcomePhaseRecorded = "recorded"
	// outcomePhaseReady: the payload is complete; the scan may retry the publication.
	outcomePhaseReady = "ready"
)

// stageOutcomeRecord is the durable record of one finished, not-yet-published stage run.
type stageOutcomeRecord struct {
	Version int    `json:"version"`
	Kind    string `json:"kind"`
	Phase   string `json:"phase"`
	// Repo is "owner/repo"; with Number it keys the record (at most one per item).
	Repo    string `json:"repo"`
	Number  int    `json:"number"`
	Stage   string `json:"stage"`
	HeadSHA string `json:"head_sha"`
	// Nonce marks this record's comments (a hidden marker in the body) so a retry after a
	// partial publication can tell them from an earlier run's.
	Nonce      string `json:"nonce"`
	BaseBranch string `json:"base_branch,omitempty"`

	// Output is the stage's raw output, from which the issue-body update and the PR summary
	// are taken.
	Output     string `json:"output,omitempty"`
	BodyUpdate string `json:"body_update,omitempty"`
	BodyBefore string `json:"body_before,omitempty"`

	// The comment payload, fixed at the ready checkpoint.
	PostOutput string `json:"post_output,omitempty"`
	Footer     string `json:"footer,omitempty"`
	Branch     string `json:"branch,omitempty"`
	Commit     string `json:"commit,omitempty"`
	MainSHA    string `json:"main_sha,omitempty"`
	Timestamp  string `json:"timestamp,omitempty"`
	PRNumber   int    `json:"pr_number,omitempty"`

	// Publication progress. A step is flagged once it is known to have landed.
	BodyDone         bool `json:"body_done,omitempty"`
	PRCommentDone    bool `json:"pr_comment_done,omitempty"`
	IssueCommentDone bool `json:"issue_comment_done,omitempty"`
	PushDone         bool `json:"push_done,omitempty"`

	// Attempts counts the scan's failed retries that count against MaxRetries (transport
	// errors do not). Persisted so a restart cannot reset the bound.
	Attempts  int       `json:"attempts,omitempty"`
	LastError string    `json:"last_error,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

func (r *stageOutcomeRecord) key() string { return fmt.Sprintf("%s#%d", r.Repo, r.Number) }

// stageOutcomeStore keeps the records: an in-memory index backed by one JSON file per item.
// A nil store (every NewWithDeps engine unless a test opts in) records nothing.
type stageOutcomeStore struct {
	dir string
	mu  sync.Mutex
	idx map[string]*stageOutcomeRecord
}

func newStageOutcomeStore(dir string) *stageOutcomeStore {
	if dir == "" {
		return nil
	}
	return &stageOutcomeStore{dir: dir, idx: make(map[string]*stageOutcomeRecord)}
}

// stageOutcomeDir is where the production engine keeps its recorded outcomes.
func stageOutcomeDir(fabrikDir string) string {
	return filepath.Join(fabrikDir, ".fabrik", "state", "stage-outcomes")
}

func stageOutcomeFileName(key string) string {
	sum := sha1.Sum([]byte(key)) //nolint:gosec // see import
	return hex.EncodeToString(sum[:]) + ".json"
}

// newOutcomeNonce returns a random token for a record's comment markers.
func newOutcomeNonce() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// outcomeMarker is the hidden marker appended to a recorded outcome's comments. part
// distinguishes the PR comment from the issue summary.
func outcomeMarker(nonce, part string) string {
	return fmt.Sprintf("<!-- fabrik:outcome=%s:%s -->", nonce, part)
}

// load reads every record file into the index. Unreadable, wrong-version or malformed files
// are renamed to <file>.corrupt and ignored (FR-010); the returned notes describe them.
func (s *stageOutcomeStore) load() (notes []string) {
	if s == nil {
		return nil
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if !os.IsNotExist(err) {
			notes = append(notes, fmt.Sprintf("cannot read %s: %v", s.dir, err))
		}
		return notes
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".json") {
			continue
		}
		path := filepath.Join(s.dir, ent.Name())
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			notes = append(notes, fmt.Sprintf("cannot read %s: %v", path, rerr))
			continue
		}
		var rec stageOutcomeRecord
		derr := json.Unmarshal(data, &rec)
		why := ""
		switch {
		case derr != nil:
			why = derr.Error()
		case rec.Version != stageOutcomeVersion:
			why = "unknown version"
		case rec.Repo == "" || rec.Number == 0 || rec.Stage == "":
			why = "missing item or stage"
		case stageOutcomeFileName(rec.key()) != ent.Name():
			why = "file name does not match its item"
		}
		if why != "" {
			if qerr := os.Rename(path, path+".corrupt"); qerr != nil {
				notes = append(notes, fmt.Sprintf("%s unusable (%s) and could not be quarantined: %v", path, why, qerr))
			} else {
				notes = append(notes, fmt.Sprintf("%s unusable (%s) — quarantined as %s.corrupt", path, why, path))
			}
			continue
		}
		cp := rec
		s.idx[rec.key()] = &cp
	}
	return notes
}

// get returns a copy of the record for key, or nil.
func (s *stageOutcomeStore) get(key string) *stageOutcomeRecord {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.idx[key]; r != nil {
		cp := *r
		return &cp
	}
	return nil
}

// put stores rec in the index and writes it to disk atomically. The index is updated even
// when the write fails (the in-process gate still holds); the error is returned for logging.
func (s *stageOutcomeStore) put(rec *stageOutcomeRecord) error {
	if s == nil {
		return nil
	}
	rec.Version = stageOutcomeVersion
	cp := *rec
	s.mu.Lock()
	defer s.mu.Unlock()
	s.idx[cp.key()] = &cp
	data, err := json.MarshalIndent(&cp, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding stage outcome: %w", err)
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", s.dir, err)
	}
	if err := atomicWriteFile(filepath.Join(s.dir, stageOutcomeFileName(cp.key())), data, 0o600); err != nil {
		return fmt.Errorf("writing stage outcome: %w", err)
	}
	return nil
}

// remove drops the record for key (index and file). Reports whether one existed.
func (s *stageOutcomeStore) remove(key string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.idx[key]
	delete(s.idx, key)
	_ = os.Remove(filepath.Join(s.dir, stageOutcomeFileName(key)))
	return ok
}

// snapshot returns copies of every live record, ordered by key.
func (s *stageOutcomeStore) snapshot() []stageOutcomeRecord {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]stageOutcomeRecord, 0, len(s.idx))
	for _, r := range s.idx {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key() < out[j].key() })
	return out
}

// ─── engine glue ───

// SetStageOutcomeRecordDisabledForTest stops finalizeStageOutcome and finalizeComments
// writing records, restoring the pre-#2394 behaviour (a completed run whose completion write
// failed is dispatched again) so a test can show its assertions bite (the non-vacuity seam).
func (e *Engine) SetStageOutcomeRecordDisabledForTest(disabled bool) {
	e.stageOutcomeRecordOff.Store(disabled)
}

// EnableStageOutcomesForTest turns on outcome recording under dir and loads whatever is
// there. Two engines pointed at the same dir model a daemon restart. Test-only; production
// gets the store from New().
func (e *Engine) EnableStageOutcomesForTest(dir string) {
	e.stageOutcomes = newStageOutcomeStore(dir)
	e.loadStageOutcomes()
}

func (e *Engine) loadStageOutcomes() {
	for _, note := range e.stageOutcomes.load() {
		e.logf(0, "outcome", "warn: %s\n", note)
	}
	if n := len(e.stageOutcomes.snapshot()); n > 0 {
		e.logf(0, "outcome", "restored %d recorded stage outcome(s) awaiting publication\n", n)
	}
}

// StageOutcomeRecordsForTest returns how many recorded outcomes are awaiting publication.
func (e *Engine) StageOutcomeRecordsForTest() int {
	return len(e.stageOutcomes.snapshot())
}

// recordingOutcomes reports whether new records are written.
func (e *Engine) recordingOutcomes() bool {
	return e.stageOutcomes != nil && !e.stageOutcomeRecordOff.Load()
}

// stageOutcomePending reports whether a live recorded outcome exists for the item at stage
// (admission gate, R2). A pending outcome must be published, not produced again.
func (e *Engine) stageOutcomePending(item gh.ProjectItem, stage *stages.Stage) bool {
	if e.stageOutcomes == nil || stage == nil || stage.CleanupWorktree {
		return false
	}
	rec := e.stageOutcomes.get(issueKey(item, e.defaultRepo()))
	return rec != nil && rec.Stage == stage.Name
}

// dropStageOutcome removes the item's record and logs why.
func (e *Engine) dropStageOutcome(key string, number int, reason string) {
	if e.stageOutcomes.remove(key) {
		e.logf(number, "outcome", "recorded outcome discarded: %s\n", reason)
	}
}

// persistOutcome writes rec, logging (never failing on) a write error.
func (e *Engine) persistOutcome(rec *stageOutcomeRecord) {
	if rec == nil {
		return
	}
	if err := e.stageOutcomes.put(rec); err != nil {
		e.logf(rec.Number, "outcome", "warn: could not persist recorded outcome: %v\n", err)
	}
}

// beginStageOutcome writes the "recorded" record for a completed stage run, before the first
// GitHub write. It returns nil (record nothing, behave as before) unless recording is on and
// the run is one whose publication this change protects: completed without error, not a
// no-work completion, and not an Implement coordinator that may turn out to be one.
func (e *Engine) beginStageOutcome(p stageOutcomeParams, output string, completed bool) *stageOutcomeRecord {
	if !e.recordingOutcomes() || !completed || p.invokeErr != nil || strings.TrimSpace(output) == "" {
		return nil
	}
	if CheckNoWorkNeeded(output) || CheckBlockedOnInput(output) {
		return nil
	}
	if p.stage.Name == "Implement" && hasLabel(p.item.Labels, "fabrik:children-spawned") {
		return nil
	}
	head, err := gitRevParse(p.workDir, "HEAD")
	if err != nil || head == "" {
		return nil
	}
	rec := &stageOutcomeRecord{
		Kind:       outcomeKindStage,
		Phase:      outcomePhaseRecorded,
		Repo:       itemOwnerRepoString(p.item, e.defaultRepo()),
		Number:     p.item.Number,
		Stage:      p.stage.Name,
		HeadSHA:    head,
		Nonce:      newOutcomeNonce(),
		BaseBranch: p.baseBranch,
		Output:     output,
		BodyUpdate: extractUpdatedBody(output),
		BodyBefore: p.item.Body,
		CreatedAt:  e.now(),
	}
	e.persistOutcome(rec)
	return rec
}

// recordCompletionOutcome writes a "completion" record: a comment pass completed the stage
// but its completion signal could not be written (ADR 2394 audit finding a). Only
// handleStageComplete is retried.
func (e *Engine) recordCompletionOutcome(item gh.ProjectItem, stage *stages.Stage, workDir string, cause error) {
	if !e.recordingOutcomes() || stage == nil || stage.CleanupWorktree {
		return
	}
	head, err := gitRevParse(workDir, "HEAD")
	if err != nil || head == "" {
		return
	}
	rec := &stageOutcomeRecord{
		Kind:      outcomeKindCompletion,
		Phase:     outcomePhaseReady,
		Repo:      itemOwnerRepoString(item, e.defaultRepo()),
		Number:    item.Number,
		Stage:     stage.Name,
		HeadSHA:   head,
		Nonce:     newOutcomeNonce(),
		LastError: cause.Error(),
		CreatedAt: e.now(),
	}
	e.persistOutcome(rec)
	e.logf(item.Number, "outcome", "stage %q completion not written (%v) — recorded, will retry instead of re-running\n", stage.Name, cause)
}
