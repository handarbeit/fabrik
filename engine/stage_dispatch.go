package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

// Per-episode stage dispatch bound and unchanged-input resume exemption
// (#2395, ADR 2395).
//
// max_retries counts only failed runs and max_turns bounds one invocation, so a
// stage that completes, fails to record its completion and is dispatched again
// reaches neither (#2388: one Research stage ran about 150 times). The dispatch
// record counts every stage-run dispatch of one stage of one item in the current
// episode, persists across restarts, and pauses the item at max_stage_dispatches.
// The same record keeps the inputs the previous run saw, which the session resume
// decision compares to tell a re-dispatch with nothing new to act on.

// stageDispatchOutcomeKeep is how many recent outcomes the record (and so the
// pause comment) keeps.
const stageDispatchOutcomeKeep = 5

// Outcomes recorded for a dispatch. A persisted "complete" is by construction a
// completion that was never recorded: recording one resets the record.
const (
	dispatchOutcomeComplete   = "complete"
	dispatchOutcomeIncomplete = "incomplete"
	dispatchOutcomeSlice      = "time slice"
	dispatchOutcomeBlocked    = "blocked on input"
	dispatchOutcomeError      = "error"
)

// stageInputs is what a stage run saw: the worktree head, the issue body and the
// set of non-Fabrik comments. Hashes keep the record small and free of issue text.
type stageInputs struct {
	HeadSHA      string `json:"head_sha,omitempty"`
	BodyHash     string `json:"body_hash,omitempty"`
	CommentsHash string `json:"comments_hash,omitempty"`
}

// stageDispatchRecord is the durable per-item, per-stage record, a JSON sidecar
// next to the stage's .session pointer (the .session.meta / .resumefails
// precedent). It is a file of its own because the session meta is a no-op
// without the pointer, and a fresh decision deletes the pointer, which is
// exactly when a loop is running.
//
//   - Count / Outcomes: dispatches in the current episode and the last few
//     outcomes. Reset by durable completion, a human resume or a column change.
//   - Paused: the cap paused the item. A record that is Paused while the item no
//     longer carries the pause was resumed by a human (also after a restart,
//     which loses the in-memory EnginePaused), so the count starts again.
//   - Inputs: what the previous run saw. Survives a reset: it describes the last
//     run, not the episode.
type stageDispatchRecord struct {
	Count    int          `json:"count,omitempty"`
	Outcomes []string     `json:"outcomes,omitempty"`
	Paused   bool         `json:"paused,omitempty"`
	Inputs   *stageInputs `json:"inputs,omitempty"`
}

var stageDispatchMu sync.Mutex

// SetStageDispatchCapDisabledForTest neutralises the dispatch bound: the count is
// still recorded but the item is never paused on account of it (R5's
// non-vacuity seam).
func (e *Engine) SetStageDispatchCapDisabledForTest(disabled bool) {
	e.stageDispatchCapOff.Store(disabled)
}

// SetUnchangedInputExemptionDisabledForTest removes the unchanged-input resume
// exemption (R4's non-vacuity seam): the decision is the #2300 one.
func (e *Engine) SetUnchangedInputExemptionDisabledForTest(disabled bool) {
	e.unchangedInputExemptionOff.Store(disabled)
}

// stageDispatchCap returns the effective max_stage_dispatches; 0 = no bound.
func (e *Engine) stageDispatchCap() int {
	if e.stageDispatchCapOff.Load() || e.cfg.MaxStageDispatches <= 0 {
		return 0
	}
	return e.cfg.MaxStageDispatches
}

func (e *Engine) stageDispatchPath(item gh.ProjectItem, stageName string) string {
	return strings.TrimSuffix(e.sessionFilePathFor(item, stageName), ".session") + ".dispatches"
}

// readStageDispatch reads the record. A missing, unreadable or unparseable file
// is "no record" (found == false).
func readStageDispatch(path string) (rec stageDispatchRecord, found bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return stageDispatchRecord{}, false
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		return stageDispatchRecord{}, false
	}
	return rec, true
}

// writeStageDispatch writes the record atomically (tmp+rename, mode 0600). It is
// best-effort: a failure is logged loudly (the bound for this run is lost) but
// never fails the dispatch.
func (e *Engine) writeStageDispatch(item gh.ProjectItem, path string, rec stageDispatchRecord) {
	data, err := json.Marshal(rec)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		e.logf(item.Number, "dispatch-limit", "warning: could not save stage dispatch record %s (the dispatch bound is not enforced for this run): %v\n", path, err)
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		e.logf(item.Number, "dispatch-limit", "warning: could not save stage dispatch record %s (the dispatch bound is not enforced for this run): %v\n", path, err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		e.logf(item.Number, "dispatch-limit", "warning: could not save stage dispatch record %s (the dispatch bound is not enforced for this run): %v\n", path, err)
	}
}

// updateStageDispatch applies fn to the current record and writes it back.
func (e *Engine) updateStageDispatch(item gh.ProjectItem, stageName string, fn func(*stageDispatchRecord)) {
	path := e.stageDispatchPath(item, stageName)
	stageDispatchMu.Lock()
	defer stageDispatchMu.Unlock()
	rec, _ := readStageDispatch(path)
	fn(&rec)
	e.writeStageDispatch(item, path, rec)
}

// recordStageDispatch counts one stage-run dispatch (a worker really ran) with
// its outcome and the inputs it saw. Called next to StageAttempted, which is the
// existing "Claude actually ran" signal, so a pre-spawn refusal never counts and
// one runInvocationWithExtension call (however many extensions) counts once.
func (e *Engine) recordStageDispatch(item gh.ProjectItem, stageName, outcome string, inputs stageInputs) {
	e.updateStageDispatch(item, stageName, func(r *stageDispatchRecord) {
		r.Count++
		r.Paused = false // a dispatch happened, so any earlier cap pause was lifted
		r.Outcomes = append(r.Outcomes, outcome)
		if n := len(r.Outcomes); n > stageDispatchOutcomeKeep {
			r.Outcomes = append([]string(nil), r.Outcomes[n-stageDispatchOutcomeKeep:]...)
		}
		in := inputs
		r.Inputs = &in
	})
}

// resetStageDispatches ends the episode: count, outcomes and the pause flag go,
// the previous run's inputs stay. It must only be called after the durable state
// that ends the episode was actually written, never on the `completed` branch
// itself — an unrecorded completion is exactly the failure being bounded.
func (e *Engine) resetStageDispatches(item gh.ProjectItem, stageName string) {
	path := e.stageDispatchPath(item, stageName)
	stageDispatchMu.Lock()
	defer stageDispatchMu.Unlock()
	rec, found := readStageDispatch(path)
	if !found || (rec.Count == 0 && len(rec.Outcomes) == 0 && !rec.Paused) {
		return
	}
	rec.Count, rec.Outcomes, rec.Paused = 0, nil, false
	e.writeStageDispatch(item, path, rec)
}

// clearOtherStageDispatches drops the dispatch records of every stage of the item
// other than stageName: dispatching stage X means the item is in X's column, so
// any other stage's episode is over (the column-change reset, also for a human
// move, which has no hook of its own).
func (e *Engine) clearOtherStageDispatches(item gh.ProjectItem, stageName string) {
	keep := e.stageDispatchPath(item, stageName)
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(keep), "*.dispatches"))
	if err != nil {
		return
	}
	stageDispatchMu.Lock()
	defer stageDispatchMu.Unlock()
	for _, m := range matches {
		if m != keep {
			os.Remove(m)
		}
	}
}

// computeStageInputs fingerprints what a stage run is about to see. Fabrik's own
// comments are excluded: the engine posts a fresh stage comment on every run, so
// including them would make every re-dispatch look changed.
func computeStageInputs(item gh.ProjectItem, workDir string) stageInputs {
	in := stageInputs{}
	if head, err := gitRevParse(workDir, "HEAD"); err == nil {
		in.HeadSHA = head
	}
	in.BodyHash = hashString(item.Body)

	comments := make([]gh.Comment, 0, len(item.Comments))
	for _, c := range item.Comments {
		if strings.HasPrefix(c.Body, "🏭 **Fabrik") {
			continue
		}
		comments = append(comments, c)
	}
	sort.SliceStable(comments, func(i, j int) bool {
		if comments[i].ID != comments[j].ID {
			return comments[i].ID < comments[j].ID
		}
		return comments[i].DatabaseID < comments[j].DatabaseID
	})
	h := sha256.New()
	for _, c := range comments {
		fmt.Fprintf(h, "%s\x00%d\x00%s\x00", c.ID, c.DatabaseID, c.Body)
	}
	in.CommentsHash = hex.EncodeToString(h.Sum(nil))
	return in
}

func hashString(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// unchangedSince reports whether cur equals the inputs the previous run saw.
// Anything unknown — no previous record, an empty head on either side — counts as
// changed, so the exemption never fires on a guess.
func (cur stageInputs) unchangedSince(prev *stageInputs) bool {
	if prev == nil || cur.HeadSHA == "" || prev.HeadSHA == "" {
		return false
	}
	return cur.HeadSHA == prev.HeadSHA && cur.BodyHash == prev.BodyHash && cur.CommentsHash == prev.CommentsHash
}

// previousStageInputs returns the inputs the previous dispatch of this stage saw.
func (e *Engine) previousStageInputs(item gh.ProjectItem, stageName string) *stageInputs {
	rec, found := readStageDispatch(e.stageDispatchPath(item, stageName))
	if !found {
		return nil
	}
	return rec.Inputs
}

// stageDispatchOutcome names how a dispatch ended, from the locals the retry
// branch of finalizeStageOutcome already has.
func stageDispatchOutcome(completed, blocked, slice bool, err error) string {
	switch {
	case completed:
		return dispatchOutcomeComplete
	case blocked:
		return dispatchOutcomeBlocked
	case slice:
		return dispatchOutcomeSlice
	case err != nil:
		return dispatchOutcomeError
	default:
		return dispatchOutcomeIncomplete
	}
}

// dispatchLimitReached is the pre-dispatch check. It returns true after pausing
// the item when the stage has already been dispatched cap times in this episode.
// It runs before the lock is taken, so the bound is strict: a stage is never
// dispatched more than max_stage_dispatches times without a human.
func (e *Engine) dispatchLimitReached(item gh.ProjectItem, stage *stages.Stage) bool {
	limit := e.stageDispatchCap()
	if limit == 0 {
		return false
	}
	path := e.stageDispatchPath(item, stage.Name)
	rec, found := readStageDispatch(path)
	if !found || rec.Count < limit {
		return false
	}
	if rec.Paused {
		// The cap paused this item and the pause is gone (the paused-item gates
		// return before this check): a human resumed it, possibly across a restart
		// that lost the in-memory pause record. A new episode begins.
		e.logf(item.Number, "dispatch-limit", "stage %q was resumed after the dispatch limit — starting a new episode\n", stage.Name)
		e.resetStageDispatches(item, stage.Name)
		return false
	}
	e.pauseForDispatchLimit(item, stage, rec, limit)
	return true
}

// pauseForDispatchLimit pauses the item at the dispatch cap. Modeled on
// pauseForSliceLimit: fabrik:paused + fabrik:awaiting-input, EnginePaused (so an
// unpause runs clearFailedStage, which resets the count) and no
// stage:<name>:failed label — the stage did not fail. The comment starts with
// the 🏭 **Fabrik prefix, so it is never steering under PAT auth.
func (e *Engine) pauseForDispatchLimit(item gh.ProjectItem, stage *stages.Stage, rec stageDispatchRecord, limit int) {
	e.logf(item.Number, "dispatch-limit", "stage %q dispatched %d time(s), reaching the limit of %d — pausing\n", stage.Name, rec.Count, limit)

	// The loop this bound exists for is typically a GitHub write failure, so the
	// pause label itself can fail. Write it with its error checked and stop if it
	// did not land: recording Paused/EnginePaused for a pause that is not on the
	// item would read as a manual unpause on the next poll and reset the count,
	// handing the runaway stage a fresh allowance. The count stays at the cap, so
	// the next poll retries the pause (with no comment posted until it lands).
	// pauseIssue's own add of the same label is an idempotent repeat.
	if err := e.addLabelChecked(item, "fabrik:paused"); err != nil {
		e.logf(item.Number, "dispatch-limit", "warning: could not add fabrik:paused for stage %q; the pause is not recorded and will be retried on the next poll: %v\n", stage.Name, err)
		return
	}

	// The literal prefix at the call site is what the comment-compliance scan
	// checks (it never reads through a helper's return value).
	comment := fmt.Sprintf("🏭 **Fabrik — stage dispatch limit reached**\n\n%s", formatDispatchLimitComment(stage.Name, rec, limit))
	e.pauseIssue(item, comment, pauseOpts{
		awaitingInput: true,
		reactRocket:   true,
	})

	e.updateStageDispatch(item, stage.Name, func(r *stageDispatchRecord) { r.Paused = true })
	repoStr := itemOwnerRepoString(item, e.defaultRepo())
	e.store.Apply(itemstate.EnginePaused{Repo: repoStr, Number: item.Number, StageName: stage.Name})
}

// formatDispatchLimitComment builds the pause comment's body, after its header: the stage, the count, the
// cap, the most recent outcomes and how to resume.
func formatDispatchLimitComment(stageName string, rec stageDispatchRecord, limit int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Stage **%s** has been dispatched %d time(s) in this episode, which has reached the configured limit of %d "+
		"(override with `--max-stage-dispatches` or `FABRIK_MAX_STAGE_DISPATCHES`; `0` disables the limit).\n\n", stageName, rec.Count, limit)
	if n := len(rec.Outcomes); n > 0 {
		fmt.Fprintf(&b, "Outcomes of the last %d dispatch(es), oldest first:\n", n)
		for _, o := range rec.Outcomes {
			if o == dispatchOutcomeComplete {
				o = "complete (completion not recorded)"
			}
			fmt.Fprintf(&b, "- %s\n", o)
		}
		b.WriteString("\n")
	}
	b.WriteString("A stage that keeps ending in \"complete\" but is dispatched again is finishing without its completion being recorded on the issue; " +
		"repeating it will not change the outcome.\n\n")
	b.WriteString("Fabrik has paused this issue. To continue, investigate, then reply with a comment (or remove the `fabrik:paused` and " +
		"`fabrik:awaiting-input` labels) — the dispatch count for this stage starts again from zero.")
	return b.String()
}
