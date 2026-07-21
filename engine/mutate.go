package engine

import (
	"errors"
	"time"

	"github.com/handarbeit/fabrik/boardcache"
	gh "github.com/handarbeit/fabrik/github"
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
func (e *Engine) applyLabelAdd(item gh.ProjectItem, label string, echo bool) {
	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	if err := e.client.AddLabelToIssue(owner, repo, item.Number, label); err != nil {
		e.logf(item.Number, "warn", "could not add label %q: %v\n", label, err)
		return
	}
	e.syncLabelAdd(item, label, echo)
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
}

// pauseIssue posts the given comment and applies fabrik:paused (plus,
// depending on opts, fabrik:awaiting-input and a fabrik:auto-merge-enabled
// removal), collapsing the shared tail of the 11 pauseFor* functions and the
// pause portion of the 2 escalate* functions. Callers that also need to apply
// additional labels (e.g. escalate*'s stage:<name>:failed, or
// pauseForPRClosedNotMerged's fabrik:awaiting-ci removal) do so themselves
// around this call — those steps are outside the shared pause tail.
func (e *Engine) pauseIssue(item gh.ProjectItem, comment string, opts pauseOpts) {
	e.postComment(item, comment, opts.reactRocket, opts.commentEcho) //nolint:errcheck // failure already logged by postComment
	e.applyLabelAdd(item, "fabrik:paused", opts.labelEcho)
	if opts.awaitingInput {
		e.applyLabelAdd(item, "fabrik:awaiting-input", opts.labelEcho)
	}
	if opts.removeAutoMerge {
		e.applyLabelRemove(item, "fabrik:auto-merge-enabled", opts.labelEcho)
	}
}
