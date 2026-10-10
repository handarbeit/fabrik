package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/channelevents"
)

// Red-singleton auto-repair (#2045, ADR-2045, amending ADR-1545).
//
// When a merge-train trial is red for exactly one member alone, ejectRedSingleton used to
// reroute that member to Validate and pause it for a human, who then applied
// fabrik:revalidate. With auto-repair the engine performs that fabrik:revalidate equivalent
// itself (reenterValidate), bounded by a per-member, per-base-SHA attempt cap that stands in
// for the external re-detection signal ADR-1545 found missing. The repair is an ordinary
// Validate dispatch from the poll loop: the train worker never starts Claude for it and never
// holds a slot for it.
//
// All state here is in memory (like mergeTrainEjectionCounts); a daemon restart resets the
// attempt counts — at worst one extra repair per member per base SHA — and drops any pending
// diagnostic, in which case Validate still runs without the context file.

const (
	// mergeTrainRepairFile is the context file the repair's Validate dispatch reads.
	mergeTrainRepairFile = "merge-train-repair.md"
	// repairContextTTL bounds how long a pending repair context may wait for its Validate
	// dispatch. A longer wait means the repair flow was left (paused, blocked, suspended) and
	// the facts in the context are no longer trustworthy, so it is discarded.
	repairContextTTL = 2 * time.Hour
	// repairHistoryMax bounds the per-member attempt history kept for the pause comment.
	repairHistoryMax = 20
)

// repairAttempt is one recorded auto-repair dispatch.
type repairAttempt struct {
	BaseSHA       string // pinned base of the failing trial
	MemberHeadSHA string // the member's head at failure
	FailedChecks  []string
	At            time.Time
}

// repairContext is the in-memory hand-off from the ejecting train worker to the member's
// next Validate dispatch (poll goroutine).
type repairContext struct {
	diag          *trainCIDiagnostic
	baseSHA       string
	memberHeadSHA string
	attempt       int
	cap           int
	at            time.Time // when the repair was started; see repairContextTTL
}

// autoRepairState is the shared (worker goroutine ↔ poll goroutine) auto-repair state.
// The zero value is ready to use.
type autoRepairState struct {
	mu       sync.Mutex
	attempts map[string][]repairAttempt // member key → attempts, oldest first
	requeued map[string][]string        // member key → base SHAs whose one R3 requeue is spent
	pending  map[string]*repairContext  // member key → context awaiting the Validate dispatch
}

func autoRepairKey(owner, repo string, num int) string {
	return fmt.Sprintf("%s/%s#%d", owner, repo, num)
}

// effectiveAutoRepairCap returns the configured per-member, per-base-SHA repair cap.
// 0 (or negative) disables auto-repair and the R3 requeue entirely.
func (e *Engine) effectiveAutoRepairCap() int {
	if e.cfg.MaxTrainAutoRepairAttempts < 0 {
		return 0
	}
	return e.cfg.MaxTrainAutoRepairAttempts
}

// autoRepairAttemptsAt counts the repairs already dispatched for key at baseSHA.
func (e *Engine) autoRepairAttemptsAt(key, baseSHA string) int {
	s := &e.autoRepair
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, a := range s.attempts[key] {
		if a.BaseSHA == baseSHA {
			n++
		}
	}
	return n
}

func (e *Engine) recordAutoRepairAttempt(key string, a repairAttempt) {
	s := &e.autoRepair
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.attempts == nil {
		s.attempts = make(map[string][]repairAttempt)
	}
	h := append(s.attempts[key], a)
	if len(h) > repairHistoryMax {
		h = h[len(h)-repairHistoryMax:]
	}
	s.attempts[key] = h
}

// takeRequeue spends the member's one R3 requeue for baseSHA. It reports false when it was
// already spent, so a member that returns to Queued and fails again at the same base goes to
// auto-repair instead of looping.
func (e *Engine) takeRequeue(key, baseSHA string) bool {
	s := &e.autoRepair
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sha := range s.requeued[key] {
		if sha == baseSHA {
			return false
		}
	}
	if s.requeued == nil {
		s.requeued = make(map[string][]string)
	}
	r := append(s.requeued[key], baseSHA)
	if len(r) > repairHistoryMax {
		r = r[len(r)-repairHistoryMax:]
	}
	s.requeued[key] = r
	return true
}

func (e *Engine) setPendingRepair(key string, rc *repairContext) {
	s := &e.autoRepair
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending == nil {
		s.pending = make(map[string]*repairContext)
	}
	s.pending[key] = rc
}

func (e *Engine) dropPendingRepair(key string) {
	s := &e.autoRepair
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pending, key)
}

// consumePendingRepair removes and returns the member's pending repair context (nil if none).
func (e *Engine) consumePendingRepair(key string) *repairContext {
	s := &e.autoRepair
	s.mu.Lock()
	defer s.mu.Unlock()
	rc := s.pending[key]
	delete(s.pending, key)
	return rc
}

// resetAutoRepair forgets everything recorded for a member; called with resetEjectionCount
// after a successful landing.
func (e *Engine) resetAutoRepair(owner, repo string, num int) {
	key := autoRepairKey(owner, repo, num)
	s := &e.autoRepair
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.attempts, key)
	delete(s.requeued, key)
	delete(s.pending, key)
}

// renderRepairAttemptHistory lists the member's recorded repairs for the pause comment
// ("" when there are none, so the cap-0 comment is unchanged).
func (e *Engine) renderRepairAttemptHistory(key string) string {
	s := &e.autoRepair
	s.mu.Lock()
	h := append([]repairAttempt(nil), s.attempts[key]...)
	s.mu.Unlock()
	if len(h) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "**Auto-repair attempts made (%d):**\n", len(h))
	for i, a := range h {
		checks := "(no failing check names recorded)"
		if len(a.FailedChecks) > 0 {
			checks = strings.Join(a.FailedChecks, ", ")
		}
		fmt.Fprintf(&b, "%d. %s — base `%s`, member head `%s`, failing: %s\n", i+1, a.At.UTC().Format("2006-01-02 15:04 UTC"), a.BaseSHA, a.MemberHeadSHA, checks)
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderRepairContext renders the context file for a repair dispatch. The instruction text is
// self-contained so it reaches the worker without any stage-YAML refresh.
func renderRepairContext(rc *repairContext) string {
	var b strings.Builder
	b.WriteString("# Merge-train repair\n\n")
	b.WriteString("This Validate run was started automatically by the merge train, not by a person. ")
	b.WriteString("This pull request's own combined Validate failed on the train's trial branch, and the cause is this pull request alone — ")
	b.WriteString("the base branch moved (other pull requests landed) and this change no longer fits it.\n\n")
	fmt.Fprintf(&b, "Repair attempt %d of %d for this base commit.\n\n", rc.attempt, rc.cap)
	b.WriteString("## What to do\n\n")
	b.WriteString("1. Bring this branch up to date with the current base branch (rebase or merge, whichever the repository's conventions favour).\n")
	b.WriteString("2. Reconcile this change with what landed: fix compile errors, signatures, call sites, test expectations and lists that other pull requests changed.\n")
	b.WriteString("3. **Never revert, disable or work around the change that landed on the base branch** to make this pull request green. The landed change is correct by definition; adapt this pull request to it.\n")
	b.WriteString("4. Run the checks named below and the relevant test suite, push, and complete Validate as usual.\n\n")
	b.WriteString("## Facts\n\n")
	if rc.diag != nil && rc.diag.TrialSHA != "" {
		fmt.Fprintf(&b, "- Failing trial head: `%s`\n", rc.diag.TrialSHA)
	}
	fmt.Fprintf(&b, "- Trial base (pinned) SHA: `%s`\n", rc.baseSHA)
	fmt.Fprintf(&b, "- This pull request's head at failure: `%s`\n\n", rc.memberHeadSHA)
	if block := renderDiagnosticBlock(rc.diag); block != "" {
		b.WriteString("## Failing checks\n\n")
		b.WriteString(block)
		b.WriteString("\n")
	}
	return b.String()
}

// writeMergeTrainRepair materialises the member's pending repair context as
// .fabrik-context/merge-train-repair.md for a Validate dispatch and consumes it, so the file
// exists for that one dispatch only. Called by writeContextFiles on every invocation: with no
// pending context (or for any other stage, or comment processing) a leftover file is removed,
// so it can never leak into a later stage or a later Validate run. Errors are non-fatal and a
// missing context never blocks the dispatch.
func (e *Engine) writeMergeTrainRepair(item gh.ProjectItem, repairDispatch bool, fabrikDir string) {
	path := filepath.Join(fabrikDir, mergeTrainRepairFile)
	var rc *repairContext
	if repairDispatch {
		owner, repo := itemOwnerRepo(item, e.defaultRepo())
		rc = e.consumePendingRepair(autoRepairKey(owner, repo, item.Number))
	}
	if rc != nil {
		// A context that outlived its repair flow must not leak into a later, unrelated
		// Validate run: discard it when it is old or the member's head has since moved.
		if age := e.now().Sub(rc.at); age > repairContextTTL {
			e.logf(item.Number, "merge-train", "discarding stale auto-repair context (%s old) for the Validate dispatch\n", age.Round(time.Minute))
			rc = nil
		} else if item.LinkedPRHeadSHA != "" && item.LinkedPRHeadSHA != rc.memberHeadSHA {
			e.logf(item.Number, "merge-train", "discarding stale auto-repair context: PR head moved from %s to %s since the repair started\n", rc.memberHeadSHA, item.LinkedPRHeadSHA)
			rc = nil
		}
	}
	if rc == nil {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			e.logf(item.Number, "warn", "could not remove stale .fabrik-context/%s: %v\n", mergeTrainRepairFile, err)
		}
		return
	}
	if err := os.WriteFile(path, []byte(renderRepairContext(rc)), 0644); err != nil {
		e.logf(item.Number, "warn", "could not write .fabrik-context/%s: %v\n", mergeTrainRepairFile, err)
		return
	}
	e.logf(item.Number, "merge-train", "wrote %s for the auto-repair Validate dispatch (attempt %d of %d)\n", mergeTrainRepairFile, rc.attempt, rc.cap)
}

// liveBaseSHA resolves the current origin/<base> tip live (not the trial's pinned base), the
// same idiom landOneAtATime uses to re-pin.
func (e *Engine) liveBaseSHA(p trialParams) (string, error) {
	if e.trainLiveBaseFn != nil {
		return e.trainLiveBaseFn(p)
	}
	if p.wm == nil {
		return "", fmt.Errorf("no worktree manager")
	}
	p.wm.FetchOrigin() // best-effort
	return gitRevParse(p.wm.baseDir, "refs/remotes/origin/"+p.baseBranch)
}

// memberFixedAgainstLiveBase is R3: it reports whether the member's LIVE head already
// contains the LIVE base and its own CI is green and complete, by re-fetching the PR, building
// a fresh trainMember from the live head and reusing singletonFastPathEligible unmodified (its
// snapshot-equality check passes trivially because the member is built from the same fetch).
// Fail-closed: any error or ambiguity is "not confirmed".
func (e *Engine) memberFixedAgainstLiveBase(p trialParams, m trainMember) (bool, string) {
	pr, err := e.client.FetchPRDetails(p.owner, p.repo, m.prNum)
	if err != nil || pr == nil {
		return false, fmt.Sprintf("could not fetch PR #%d: %v", m.prNum, err)
	}
	if pr.Merged || pr.State != "open" || pr.HeadSHA == "" {
		return false, fmt.Sprintf("PR #%d is not open with a head", m.prNum)
	}
	base, err := e.liveBaseSHA(p)
	if err != nil || base == "" {
		return false, fmt.Sprintf("could not resolve the live base: %v", err)
	}
	fresh := m
	fresh.headSHA = pr.HeadSHA
	live := p
	live.baseSHA = base
	return e.singletonFastPathEligible(live, fresh, pr)
}

// repairOutcome is the result of startAutoRepair.
type repairOutcome int

const (
	repairStarted       repairOutcome = iota // Validate re-entry secured; attempt recorded and posted
	repairRerouteFailed                      // member left in Queued, nothing posted or counted
	repairReentryFailed                      // member rerouted but re-entry not secured; caller pauses
)

// startAutoRepair reroutes the member to Validate, secures Validate re-entry (reenterValidate,
// falling back to the fabrik:revalidate trigger label that settleRevalidateScan retries every
// poll) and only then records the attempt, posts the comment and emits the event — so a
// failure before re-entry is secured never posts or counts anything (ADR-1208's
// reroute-before-side-effects ordering).
func (e *Engine) startAutoRepair(projectID, owner, repo string, m trainMember, p trialParams, diag *trainCIDiagnostic, capN int) repairOutcome {
	if !e.rerouteQueuedMemberOffHolding(projectID, m.item) {
		e.logf(m.item.Number, "merge-train", "#%d is a red singleton but could not be rerouted off Queued — leaving untouched for retry on the next poll\n", m.item.Number)
		return repairRerouteFailed
	}

	key := autoRepairKey(owner, repo, m.item.Number)
	attempt := e.autoRepairAttemptsAt(key, p.baseSHA) + 1
	// Registered before re-entry so the next poll's dispatch cannot miss it.
	e.setPendingRepair(key, &repairContext{diag: diag, baseSHA: p.baseSHA, memberHeadSHA: m.headSHA, attempt: attempt, cap: capN, at: e.now()})

	if !e.reenterValidate(m.item, owner, repo) {
		if err := e.addLabelChecked(m.item, "fabrik:revalidate"); err != nil {
			e.logf(m.item.Number, "warn", "auto-repair: could not clear Validate gates or apply fabrik:revalidate for #%d: %v — falling back to pausing\n", m.item.Number, err)
			e.dropPendingRepair(key)
			return repairReentryFailed
		}
		e.logf(m.item.Number, "merge-train", "auto-repair: direct Validate re-entry deferred for #%d — applied fabrik:revalidate so the settle scan retries it\n", m.item.Number)
	}

	checks := diagFailingChecks(diag)
	e.recordAutoRepairAttempt(key, repairAttempt{BaseSHA: p.baseSHA, MemberHeadSHA: m.headSHA, FailedChecks: checks, At: e.now()})

	sections := []string{
		fmt.Sprintf("#%d's own combined Validate is failing against the moved base — this is not a merge-train interaction. The train is repairing it automatically: Validate has been re-entered (attempt %d of %d for this base commit) with the failure details and an instruction to reconcile with what landed. No action is needed from you.", m.item.Number, attempt, capN),
		renderBatchContext(nil, m.item.Number),
	}
	if block := renderDiagnosticBlock(diag); block != "" {
		sections = append(sections, block)
	}
	sections = append(sections, "Once Validate completes again this issue re-queues and rejoins the train. If the repair does not converge within the cap, it is paused for a human with the attempt history listed.")
	msg := fmt.Sprintf("🏭 **Fabrik merge-train — auto-repair started**\n\n%s", strings.Join(sections, "\n\n"))
	if _, err := e.client.AddComment(owner, repo, m.item.Number, msg); err != nil {
		e.logf(m.item.Number, "merge-train", "warn: could not post auto-repair comment: %v\n", err)
	}

	e.logf(m.item.Number, "merge-train", "#%d is a red singleton — auto-repair attempt %d of %d started (Validate re-entered, not paused)\n", m.item.Number, attempt, capN)
	e.emitTrainEvent(owner, repo, m.item.Number, channelevents.MergeTrainFailed, "red-singleton-auto-repair",
		"its own combined Validate is failing; auto-repair started, no human action needed", diag,
		map[string]string{"auto_repair": "true", "attempt": fmt.Sprint(attempt), "cap": fmt.Sprint(capN)}) // observation only (#1968)
	return repairStarted
}
