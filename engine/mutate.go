package engine

import (
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/handarbeit/fabrik/boardcache"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

// cache returns e.readClient cast to *boardcache.CacheImpl, or nil when the
// engine is running against a plain read-only client (e.g. in tests that don't
// wire a cache). Centralizes the cast-or-nil check that was previously
// re-typed at every call site as `if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok`.
func (e *Engine) cache() *boardcache.CacheImpl {
	c, ok := e.readClient.(*boardcache.CacheImpl)
	if !ok {
		return nil
	}
	return c
}

// syncLabelAdd mirrors a label addition into the cache and, when echo is
// true, registers a webhook echo. Split out from applyLabelAdd, symmetric to
// syncLabelRemoval, so callers that must perform the GitHub mutation
// themselves (e.g. lock/in-progress label sites that gate further side
// effects on success) can still share the write-through + echo tail instead
// of re-typing it.
func (e *Engine) syncLabelAdd(item gh.ProjectItem, label string, echo bool) {
	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	if c := e.cache(); c != nil {
		c.ApplyLabelAdded(boardcache.ItemKey(owner+"/"+repo, item.Number), label)
	}
	// Reaching this point means the caller's GitHub label-add mutation already
	// succeeded — this is the only real "did GitHub's own updatedAt just bump"
	// signal available here, so the probe staleness baseline advances
	// unconditionally (#1090).
	e.store.Apply(itemstate.SelfWriteObserved{Repo: owner + "/" + repo, Number: item.Number})
	if echo && e.webhookMgr != nil {
		e.webhookMgr.RegisterEcho("issues", "labeled", boardcache.ItemKey(owner+"/"+repo, item.Number)+"+"+label)
	}
}

// applyLabelAdd performs the three-beat label-add idiom: GitHub mutation ->
// cache write-through -> conditional webhook echo. The repo is resolved once,
// canonically, via itemOwnerRepo so a caller can never accidentally use
// item.Repo in one place and a resolved owner/repo elsewhere. echo controls
// whether a webhook echo is registered on success; it exists only so
// pauseIssue can suppress it for callers that historically didn't echo.
//
// recordLabelAppliedAtNow is called unconditionally on success (#1314): every
// caller of applyLabelAdd/addLabel is guarded on the label's absence in
// item.Labels before calling — but item.Labels can itself be stale relative to
// GitHub's actual state (a recurring theme elsewhere in this file's callers),
// so AddLabelToIssue's own idempotency (GitHub no-ops a duplicate add rather
// than erroring) means this path can still be reached when nothing genuinely
// changed on GitHub. recordLabelAppliedAtNow only writes when the cache
// doesn't already hold a value for this label (see its doc comment) so that
// case leaves the true, earlier-recorded timestamp untouched rather than
// stomping it with a spurious time.Now(). This is harmless no-op storage for
// the ~15+ other labels applyLabelAdd handles that nothing ever reads back
// via labelAppliedAt.
func (e *Engine) applyLabelAdd(item gh.ProjectItem, label string, echo bool) {
	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	if err := e.client.AddLabelToIssue(owner, repo, item.Number, label); err != nil {
		e.logf(item.Number, "warn", "could not add label %q: %v\n", label, err)
		return
	}
	e.syncLabelAdd(item, label, echo)
	e.recordLabelAppliedAtNow(item, label)
}

// recordLabelAppliedAtNow records that the engine just applied label to item,
// at the current time (e.now() — the #1449 Clock seam; see clock.go), into
// the in-memory record-at-write cache (#1314) — but only when the cache
// doesn't already hold a value for this label.
//
// This guard exists because the write sites that call this (applyLabelAdd's
// success path, plus the handful of direct AddLabelToIssue call sites in
// engine/stages.go that bypass applyLabelAdd) are guarded by an in-memory
// "label absent" check against item.Labels, which is a snapshot that can be
// stale relative to GitHub's true state. Since GitHub's add-label endpoint is
// itself idempotent, a stale snapshot can make this fire for a label that was
// never actually removed — recording a bogus time.Now() would silently reset
// every downstream timeout (CIWaitTimeout, ReviewWaitTimeout, the auto-merge
// convergence budget, the merge-queue stall dwell) with no corresponding real
// GitHub event and no way to detect it after the fact (flagged in PR #1369
// review). Skipping the write whenever an entry already exists closes this:
// a genuine removal always clears the entry first via clearLabelAppliedAtNow
// (syncLabelRemoval's removal-side counterpart to this function, plus its own
// direct call in cleanupClosedIssueTransientLabels), so by the time a genuine
// re-application happens the cache is empty and this still records the fresh
// timestamp — "applied → removed → re-applied" still yields the latest value,
// exactly as required. Only a spurious re-add with no matching removal is
// affected, and there the correct behavior is precisely to leave the existing
// (true) timestamp alone.
func (e *Engine) recordLabelAppliedAtNow(item gh.ProjectItem, label string) {
	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	repoStr := owner + "/" + repo
	if snap, err := e.store.Get(repoStr, item.Number); err == nil {
		if !snap.LabelAppliedAt(label).IsZero() {
			return
		}
	}
	e.store.Apply(itemstate.LabelAppliedAtRecorded{Repo: repoStr, Number: item.Number, Label: label, At: e.now()})
}

// clearLabelAppliedAtNow clears the in-memory record-at-write cache entry for
// label on item (#1314), so a later genuine re-application is recorded fresh
// rather than being skipped by recordLabelAppliedAtNow's "don't overwrite an
// existing entry" guard. Called unconditionally from syncLabelRemoval's
// success path (mirroring recordLabelAppliedAtNow's placement in
// applyLabelAdd) and explicitly from cleanupClosedIssueTransientLabels, the
// one removal path that bypasses syncLabelRemoval. Harmless no-op for labels
// that were never recorded in the first place.
func (e *Engine) clearLabelAppliedAtNow(item gh.ProjectItem, label string) {
	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	e.store.Apply(itemstate.LabelAppliedAtRecorded{Repo: owner + "/" + repo, Number: item.Number, Label: label, At: time.Time{}})
}

// labelAppliedAt returns the time label was last applied to item, preferring
// the in-memory record-at-write cache (ItemState.LabelAppliedAt, #1314) over a
// live REST fetch. On a cache miss — cold start (engine restart), or the
// label was most recently (re-)applied by a different engine instance — it
// falls through to e.client.FetchLabelAppliedAt unchanged and self-heals the
// cache with the result, so subsequent calls in this process become free.
// Same (time.Time, error) signature and fail-open contract as
// FetchLabelAppliedAt itself: a zero time or non-nil error must leave every
// caller inert, exactly as before this cache existed.
func (e *Engine) labelAppliedAt(item gh.ProjectItem, owner, repo, label string) (time.Time, error) {
	repoStr := owner + "/" + repo
	if snap, err := e.store.Get(repoStr, item.Number); err == nil {
		if at := snap.LabelAppliedAt(label); !at.IsZero() {
			return at, nil
		}
	}
	at, err := e.client.FetchLabelAppliedAt(owner, repo, item.Number, label)
	if err != nil {
		return time.Time{}, err
	}
	if !at.IsZero() {
		e.store.Apply(itemstate.LabelAppliedAtRecorded{Repo: repoStr, Number: item.Number, Label: label, At: at})
	}
	return at, nil
}

// addLabel is the always-echoing public entry point for applyLabelAdd, used by
// every call site except pauseIssue's non-echoing pauseFor* pattern.
func (e *Engine) addLabel(item gh.ProjectItem, label string) {
	e.applyLabelAdd(item, label, true)
}

// syncLabelRemoval mirrors a label removal into the cache and, when echo is
// true, registers a webhook echo. It is split out from applyLabelRemove so
// callers that must perform the GitHub mutation themselves (e.g.
// removeEditingLabel's retry-with-backoff loop) can still share the
// write-through + echo tail instead of re-typing it.
func (e *Engine) syncLabelRemoval(item gh.ProjectItem, label string, echo bool) {
	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	if c := e.cache(); c != nil {
		c.ApplyLabelRemoved(boardcache.ItemKey(owner+"/"+repo, item.Number), label)
	}
	e.clearLabelAppliedAtNow(item, label)
	// echo is only true when the underlying RemoveLabelFromIssue call actually
	// removed something on GitHub (not gh.ErrNotFound) — the same "real mutation
	// happened" signal RegisterEcho below relies on, so the staleness baseline
	// advances under the identical condition (#1090).
	if echo {
		e.store.Apply(itemstate.SelfWriteObserved{Repo: owner + "/" + repo, Number: item.Number})
	}
	if echo && e.webhookMgr != nil {
		e.webhookMgr.RegisterEcho("issues", "unlabeled", boardcache.ItemKey(owner+"/"+repo, item.Number)+"+"+label)
	}
}

// applyLabelRemove performs the three-beat label-remove idiom, with
// gh.ErrNotFound treated as success: the label is already absent on GitHub, so
// the cache is synced to match (this is the canonical fix for sites that used
// to skip the cache sync on ErrNotFound). echo is never applied on ErrNotFound,
// matching removeEditingLabel's pre-existing ErrNotFound behavior.
func (e *Engine) applyLabelRemove(item gh.ProjectItem, label string, echo bool) {
	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	err := e.client.RemoveLabelFromIssue(owner, repo, item.Number, label)
	if err != nil && !errors.Is(err, gh.ErrNotFound) {
		e.logf(item.Number, "warn", "could not remove label %q: %v\n", label, err)
		return
	}
	e.syncLabelRemoval(item, label, echo && err == nil)
}

// removeLabel is the always-echoing (on success) public entry point for
// applyLabelRemove, used by every call site except pauseIssue's non-echoing
// pauseFor* pattern.
func (e *Engine) removeLabel(item gh.ProjectItem, label string) {
	e.applyLabelRemove(item, label, true)
}

// postComment performs the three/four-beat comment-post idiom: AddComment ->
// cache write-through -> conditional webhook echo -> optional rocket reaction.
// Returns the posted comment's database id, or (0, err) on failure. echo
// exists only so pauseIssue can suppress it for callers that historically
// didn't echo their pause comment.
func (e *Engine) postComment(item gh.ProjectItem, body string, react, echo bool) (int, error) {
	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	dbID, err := e.client.AddComment(owner, repo, item.Number, body)
	if err != nil {
		e.logf(item.Number, "warn", "could not post comment: %v\n", err)
		return 0, err
	}
	if c := e.cache(); c != nil {
		c.ApplyCommentAdded(boardcache.ItemKey(owner+"/"+repo, item.Number), gh.Comment{
			DatabaseID: dbID, Body: body, Author: e.cfg.User, CreatedAt: time.Now(),
		})
	}
	// AddComment already succeeded (the error path returned above), so the
	// staleness baseline advances unconditionally, same as the cache
	// write-through above (#1090).
	e.store.Apply(itemstate.SelfWriteObserved{Repo: owner + "/" + repo, Number: item.Number})
	if echo && e.webhookMgr != nil {
		e.webhookMgr.RegisterEcho("issue_comment", "created", boardcache.ItemKey(owner+"/"+repo, item.Number))
	}
	if react {
		// no write-through: excluded — AddCommentReaction does not affect dispatch-relevant cache state
		if reactErr := e.client.AddCommentReaction(owner, repo, dbID, "rocket"); reactErr != nil {
			e.logf(item.Number, "warn", "could not add 🚀 to posted comment: %v\n", reactErr)
		}
	}
	return dbID, nil
}

// postItemComment is the always-echoing public entry point for postComment,
// used by every call site except pauseIssue's parameterized pattern. It
// swallows the AddComment error (already logged by postComment) and returns 0.
func (e *Engine) postItemComment(item gh.ProjectItem, body string, react bool) int {
	dbID, err := e.postComment(item, body, react, true)
	if err != nil {
		return 0
	}
	return dbID
}

// pauseOpts parameterizes pauseIssue's three independent axes of divergence
// across its 13 call sites (10 pauseFor* + escalatePRCreationFailure +
// escalateFailedStage + pauseForBrokenLinkage). A single bool cannot express
// this: the 10 "Pattern A" pauseFor* sites add fabrik:awaiting-input and never
// echo; the 2 "Pattern B" escalate* sites don't add awaiting-input but do
// echo both the label and comment writes; "Pattern C" (pauseForBrokenLinkage)
// adds neither awaiting-input nor a comment reaction/echo. See ADR for the
// full per-site table.
type pauseOpts struct {
	awaitingInput   bool // add fabrik:awaiting-input alongside fabrik:paused
	reactRocket     bool // rocket-react the posted comment
	labelEcho       bool // RegisterEcho after the paused/awaiting-input/auto-merge label writes
	commentEcho     bool // RegisterEcho after the comment write
	removeAutoMerge bool // also remove fabrik:auto-merge-enabled
	// labelFirst preserves call-site ordering: the 10 Pattern-A pauseFor*
	// functions historically posted their comment before adding fabrik:paused,
	// while escalatePRCreationFailure, escalateFailedStage, and
	// pauseForBrokenLinkage added fabrik:paused first. The Scope section
	// requires "no change to when labels/comments/pauses happen," so pauseIssue
	// must reproduce each call site's original ordering rather than pick one.
	labelFirst bool
}

// pauseIssue posts the given comment and applies fabrik:paused (plus,
// depending on opts, fabrik:awaiting-input and a fabrik:auto-merge-enabled
// removal), collapsing the shared tail of the 11 pauseFor* functions and the
// pause portion of the 2 escalate* functions. Callers that also need to apply
// additional labels (e.g. escalate*'s stage:<name>:failed, or
// pauseForPRClosedNotMerged's fabrik:awaiting-ci removal) do so themselves
// around this call — those steps are outside the shared pause tail.
func (e *Engine) pauseIssue(item gh.ProjectItem, comment string, opts pauseOpts) {
	// Note: the two branches below are intentionally not factored into a
	// shared closure — TestAddCommentCompliance's funnel-skip exemption for
	// pauseIssue's postComment call is keyed off this function's own name and
	// does not follow calls into a nested function literal.
	if opts.labelFirst {
		e.applyLabelAdd(item, "fabrik:paused", opts.labelEcho)
		if opts.awaitingInput {
			e.applyLabelAdd(item, "fabrik:awaiting-input", opts.labelEcho)
		}
		if opts.removeAutoMerge {
			e.applyLabelRemove(item, "fabrik:auto-merge-enabled", opts.labelEcho)
		}
		e.postComment(item, comment, opts.reactRocket, opts.commentEcho) //nolint:errcheck // failure already logged by postComment
		return
	}
	e.postComment(item, comment, opts.reactRocket, opts.commentEcho) //nolint:errcheck // failure already logged by postComment
	e.applyLabelAdd(item, "fabrik:paused", opts.labelEcho)
	if opts.awaitingInput {
		e.applyLabelAdd(item, "fabrik:awaiting-input", opts.labelEcho)
	}
	if opts.removeAutoMerge {
		e.applyLabelRemove(item, "fabrik:auto-merge-enabled", opts.labelEcho)
	}
}

// hasPauseComment reports whether item already carries a comment matching any
// of the given stable prose fragments — the shared primitive behind each
// pause domain's own hasXPauseComment wrapper (CI gate, review cycle, rebase
// cycle, enqueue cycle). The match is unscoped by time — it scans the issue's
// entire comment history, not just "this poll" — so it identifies a single
// pause *episode* rather than a single call. That's what lets a pauseFor*
// function tell a genuine same-poll duplicate call apart from a later
// re-escalation of the same unresolved episode after a human resumed the item
// by removing fabrik:paused (issue #1408) — both hit this same check, but
// only the caller-side context decides which one it is. Mirrors
// hasSkippedComment's precedent (no_work_needed_settle.go): match on a stable
// prose fragment rather than the full message, since counters/values embedded
// in the message can differ between posts of the "same" episode.
func hasPauseComment(item gh.ProjectItem, fragments ...string) bool {
	for _, c := range item.Comments {
		for _, frag := range fragments {
			if strings.Contains(c.Body, frag) {
				return true
			}
		}
	}
	return false
}

// reapplyPauseLabels re-applies fabrik:paused + fabrik:awaiting-input without
// posting a new comment. Used by a pauseFor* function's reapply branch (issue
// #1408) when hasPauseComment finds an existing pause comment for this
// episode: a human resuming the item removes only fabrik:paused, never the
// comment, so a still-blocked item must be re-escalated (labels reapplied)
// without spamming a duplicate comment. applyLabelAdd's underlying
// AddLabelToIssue call is idempotent, so this is safe to call even in the
// (should-be-rare) case the labels are still present.
func (e *Engine) reapplyPauseLabels(item gh.ProjectItem) {
	e.applyLabelAdd(item, "fabrik:paused", false)
	e.applyLabelAdd(item, "fabrik:awaiting-input", false)
}

// pauseIssueMuEntry is one entry in Engine.pauseIssueMu: the per-issue mutex
// pauseInterruptedIssue serializes on, plus a reference count so the entry
// can be deleted the instant no caller holds it — see acquirePauseIssueMutex/
// releasePauseIssueMutex.
type pauseIssueMuEntry struct {
	mu   sync.Mutex
	refs int // guarded by Engine.pauseIssueMuGuard, not mu
}

// acquirePauseIssueMutex returns the per-issue mutex entry for item, creating
// it on first use, and increments its reference count under
// pauseIssueMuGuard. Pair with releasePauseIssueMutex once the caller is done
// with entry.mu (locked or not) so the map entry can be reclaimed.
//
// Refcounting (rather than leaving entries in place forever, as an earlier
// sync.Map-based version did) keeps Engine.pauseIssueMu bounded by the number
// of issues *currently* being paused concurrently — never by the total
// number of issues ever paused over the daemon's lifetime (review finding on
// #1393). Map mutation (creation, refcount, deletion) is guarded by the
// separate pauseIssueMuGuard mutex, distinct from entry.mu itself: the two
// serve different critical sections — pauseIssueMuGuard protects the map,
// entry.mu protects one issue's pauseInterruptedIssue body — so a second
// caller for the same issue can register its reference (and thus prevent
// deletion) while the first caller still holds entry.mu.
func (e *Engine) acquirePauseIssueMutex(item gh.ProjectItem) *pauseIssueMuEntry {
	key := issueKey(item, e.defaultRepo())
	e.pauseIssueMuGuard.Lock()
	defer e.pauseIssueMuGuard.Unlock()
	entry, ok := e.pauseIssueMu[key]
	if !ok {
		entry = &pauseIssueMuEntry{}
		e.pauseIssueMu[key] = entry
	}
	entry.refs++
	return entry
}

// releasePauseIssueMutex drops the caller's reference to entry, deleting it
// from Engine.pauseIssueMu once no reference remains. Must be called exactly
// once per acquirePauseIssueMutex call, after the caller is done with
// entry.mu (i.e. after Unlock).
func (e *Engine) releasePauseIssueMutex(item gh.ProjectItem, entry *pauseIssueMuEntry) {
	key := issueKey(item, e.defaultRepo())
	e.pauseIssueMuGuard.Lock()
	defer e.pauseIssueMuGuard.Unlock()
	entry.refs--
	if entry.refs == 0 {
		delete(e.pauseIssueMu, key)
	}
}

// pauseInterruptedIssue is the shared R3 primitive (#1393/ADR-1393) behind
// both the TUI's single-issue stop (handleStopRequest, item.go) and the
// daemon-wide clean-stop pause (runShutdownPause/pauseIssueForDaemonShutdown,
// shutdown.go). Both callers cancel a per-issue context with their own kill
// reason first, then converge on this one call so the label pair
// (fabrik:paused + fabrik:awaiting-input), the write ordering, and the
// idempotency guard can never drift between the two "an issue got
// interrupted mid-stage" mechanisms.
//
// The AC2 idempotency guard (skip the comment if this pause episode is
// already recorded) is computed *inside* this function, under a per-issue
// mutex (acquirePauseIssueMutex) — not from a caller-supplied snapshot taken
// before the call. A caller-supplied bool was found in review to be racy: if
// a TUI stop (handleStopRequest) and a daemon-wide shutdown pause
// (pauseIssueForDaemonShutdown) land on the same in-flight issue at nearly
// the same moment, both would read "not yet paused" from their own
// pre-call snapshot and both proceed to post a comment — two audit comments
// for one pause episode. Locking here and re-reading e.store live under the
// lock closes that window: applyLabelAdd writes through to the store
// synchronously, so whichever caller's goroutine acquires the mutex second
// always observes the first caller's already-applied fabrik:paused label and
// takes the reapply-only branch instead.
//
// labelFirst is set so labels land before the comment, matching
// handleStopRequest's pre-existing ordering (Pattern C in pauseOpts' doc
// comment) — this refactor changes who writes the labels/comment, not when.
func (e *Engine) pauseInterruptedIssue(item gh.ProjectItem, comment string) {
	entry := e.acquirePauseIssueMutex(item)
	entry.mu.Lock()
	defer func() {
		entry.mu.Unlock()
		e.releasePauseIssueMutex(item, entry)
	}()

	alreadyPaused := false
	if snap, err := e.store.Get(itemOwnerRepoString(item, e.defaultRepo()), item.Number); err == nil {
		alreadyPaused = hasLabel(snap.Labels(), "fabrik:paused")
	}

	if alreadyPaused {
		e.reapplyPauseLabels(item)
		return
	}
	e.pauseIssue(item, comment, pauseOpts{
		awaitingInput: true,
		reactRocket:   false,
		labelEcho:     true,
		commentEcho:   true,
		labelFirst:    true,
	})
}
