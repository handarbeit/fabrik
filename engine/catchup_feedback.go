package engine

import (
	"fmt"
	"regexp"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// Review-churn recognition for the merge-train singleton catch-up (#2044, R3,
// ADR-2044). A catch-up pushes a merge commit to the member's own branch; Pruefer (and
// any other review bot) reviews every new head, and since #1953 an unaddressed review
// body holds landing while settleQueuedReviewFindings ejects a Queued member that has
// review findings — which would eject the very member the catch-up was meant to land.
//
// The engine therefore treats a review or review-thread comment as non-actionable for
// this landing when, and only when, ALL of these hold:
//
//  1. the member's linked PR carries a catch-up marker comment, authored by the engine's
//     own login, whose head equals the PR's live head;
//  2. the marker says pure=true — the merge brought in only base commits, with no
//     conflict-resolution edits (a conflict-edited catch-up is reviewed as usual);
//  3. the finding was made against exactly that head (the review's commit_id / the
//     thread comment's originalCommit — an exact SHA match, never a timestamp);
//  4. the finding's author is a bot (gh.IsBotLogin, or a declared expected_reviewers
//     identity).
//
// Anything that cannot be positively verified is actionable. In particular the
// `Fabrik-Train-Catch-Up:` commit trailer is for audit only and is NEVER trusted here: the
// member branch is writable by anyone with push access, so a forged trailer must not be
// able to mute review feedback. The marker comment is the authenticated record — only the
// engine's own login can author it — and the commit-SHA match makes it unforgeable per
// head. Human reviews and human comments are never suppressed.

// catchUpTrailerKey is the commit-message trailer stamped on a catch-up merge commit.
const catchUpTrailerKey = "Fabrik-Train-Catch-Up"

// catchUpMarkerRe matches the machine-readable marker the engine posts on the member's
// PR after a catch-up push.
var catchUpMarkerRe = regexp.MustCompile(`<!-- fabrik:train-catch-up head=([0-9a-f]{7,64}) base=([0-9a-f]{7,64}) pure=(true|false) -->`)

// catchUpMarker is one parsed marker comment.
type catchUpMarker struct {
	Head string // the catch-up merge commit's SHA — the head the push produced
	Base string // the pinned base SHA that was merged in
	Pure bool   // true: no conflict-resolution edits
}

func formatCatchUpMarker(m catchUpMarker) string {
	return fmt.Sprintf("<!-- fabrik:train-catch-up head=%s base=%s pure=%t -->", m.Head, m.Base, m.Pure)
}

// parseCatchUpMarkers returns every well-formed marker in body.
func parseCatchUpMarkers(body string) []catchUpMarker {
	var out []catchUpMarker
	for _, sm := range catchUpMarkerRe.FindAllStringSubmatch(body, -1) {
		out = append(out, catchUpMarker{Head: sm[1], Base: sm[2], Pure: sm[3] == "true"})
	}
	return out
}

// catchUpMarkerForHead finds the marker for liveHead among comments, trusting only
// those authored by self (the engine's own login). An empty self or an empty liveHead
// yields nothing — fail closed.
func catchUpMarkerForHead(comments []gh.Comment, self, liveHead string) (catchUpMarker, bool) {
	if self == "" || liveHead == "" {
		return catchUpMarker{}, false
	}
	var found catchUpMarker
	ok := false
	for _, c := range comments {
		if c.Author != self {
			continue
		}
		for _, mk := range parseCatchUpMarkers(c.Body) {
			if mk.Head == liveHead {
				found, ok = mk, true
			}
		}
	}
	return found, ok
}

// catchUpSuppresses is the pure suppression rule: a finding made against commitOID by
// author is non-actionable only under a pure catch-up marker for the live head, made
// against exactly that head, by a bot. See the file comment.
func catchUpSuppresses(mk catchUpMarker, liveHead, commitOID, author string, isBot func(string) bool) bool {
	return mk.Pure &&
		mk.Head != "" &&
		mk.Head == liveHead &&
		commitOID == mk.Head &&
		isBot != nil && isBot(author)
}

// catchUpFeedbackFilter drops the findings catchUpSuppresses names. The zero value
// suppresses nothing.
type catchUpFeedbackFilter struct {
	marker   catchUpMarker
	liveHead string
	active   bool
	isBot    func(string) bool
}

func (f catchUpFeedbackFilter) suppresses(commitOID, author string) bool {
	return f.active && catchUpSuppresses(f.marker, f.liveHead, commitOID, author, f.isBot)
}

// dropThreads removes suppressed review-thread comments.
func (f catchUpFeedbackFilter) dropThreads(threads []gh.Comment) []gh.Comment {
	if !f.active {
		return threads
	}
	out := make([]gh.Comment, 0, len(threads))
	for _, c := range threads {
		if f.suppresses(c.CommitOID, c.Author) {
			continue
		}
		out = append(out, c)
	}
	return out
}

// dropReviews removes suppressed reviews, so their bodies never become findings.
func (f catchUpFeedbackFilter) dropReviews(reviews []gh.PRReview) []gh.PRReview {
	if !f.active {
		return reviews
	}
	out := make([]gh.PRReview, 0, len(reviews))
	for _, r := range reviews {
		if f.suppresses(r.CommitID, r.Author) {
			continue
		}
		out = append(out, r)
	}
	return out
}

// isCatchUpReviewBot reports whether author is a bot reviewer: a recognisable bot login
// or an identity declared in any stage's expected_reviewers.
func (e *Engine) isCatchUpReviewBot(author string) bool {
	if author == "" {
		return false
	}
	if gh.IsBotLogin(author) {
		return true
	}
	for _, s := range e.cfg.Stages {
		if s == nil || s.ExpectedReviewers == nil {
			continue
		}
		for _, declared := range *s.ExpectedReviewers {
			if reviewerIdentityMatches(declared, author) {
				return true
			}
		}
	}
	return false
}

// catchUpFeedbackFilterFor builds the filter for item. The marker is read (one
// FetchIssueComments on the linked PR, cached per PR head — see catchUpMarkerCached) only
// when at least one finding is a bot's, made against the live head — so an item with no
// such finding costs nothing, and one with a bot-reviewed head costs at most one read per
// catchUpNoMarkerTTL. Any missing input (no linked PR, no live head, a failed read, no marker)
// returns the zero filter: nothing is suppressed.
func (e *Engine) catchUpFeedbackFilterFor(item gh.ProjectItem, threads []gh.Comment, reviews []gh.PRReview) catchUpFeedbackFilter {
	liveHead := item.LinkedPRHeadSHA
	if liveHead == "" || item.LinkedPRNumber == 0 {
		return catchUpFeedbackFilter{}
	}
	candidate := false
	for _, c := range threads {
		if c.CommitOID == liveHead && e.isCatchUpReviewBot(c.Author) {
			candidate = true
			break
		}
	}
	if !candidate {
		for _, r := range reviews {
			if r.CommitID == liveHead && e.isCatchUpReviewBot(r.Author) {
				candidate = true
				break
			}
		}
	}
	if !candidate {
		return catchUpFeedbackFilter{}
	}
	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	mk, ok, err := e.catchUpMarkerCached(owner, repo, item.LinkedPRNumber, liveHead)
	if err != nil {
		e.logf(item.Number, "warn", "catch-up recognition: could not read PR #%d comments: %v — findings stay actionable\n", item.LinkedPRNumber, err)
		return catchUpFeedbackFilter{}
	}
	if !ok {
		return catchUpFeedbackFilter{}
	}
	return catchUpFeedbackFilter{marker: mk, liveHead: liveHead, active: true, isBot: e.isCatchUpReviewBot}
}

// dropCatchUpFeedback applies catch-up recognition to the review findings the landing
// and eject gates consult. It is the one place both gates call, so the Queued settle
// scan (queuedReviewFindings) and the landing/advance gate (unprocessedFeedback) can
// never disagree about what a catch-up push made non-actionable. A suppression is
// logged, never silent.
func (e *Engine) dropCatchUpFeedback(item gh.ProjectItem, threads []gh.Comment, reviews []gh.PRReview) ([]gh.Comment, []gh.PRReview) {
	f := e.catchUpFeedbackFilterFor(item, threads, reviews)
	if !f.active {
		return threads, reviews
	}
	keptThreads, keptReviews := f.dropThreads(threads), f.dropReviews(reviews)
	if dropped := (len(threads) - len(keptThreads)) + (len(reviews) - len(keptReviews)); dropped > 0 {
		e.logf(item.Number, "merge-train", "%d bot review finding(s) on #%d are of the pure catch-up head %s (base %s merged in, no conflict edits) — non-actionable for this landing\n", dropped, item.Number, f.marker.Head, f.marker.Base)
	}
	return keptThreads, keptReviews
}

// catchUpNoMarkerTTL bounds how long "this head has no catch-up marker" is remembered.
// A head's marker never changes once posted, so a found marker is cached for good; the
// negative answer is the common one (a bot-reviewed head that was never caught up) and is
// re-read only this often, so the Queued settle scan does not read the PR's comments every
// poll for every bot-reviewed member. It errs on the fail-closed side: a stale negative
// only keeps findings actionable. The worker invalidates the entry when it posts a marker.
const catchUpNoMarkerTTL = 2 * time.Minute

// catchUpMarkerCacheMax caps the cache; past it the map is dropped wholesale (entries are
// cheap to recompute and the cap is only a leak guard).
const catchUpMarkerCacheMax = 512

// catchUpMarkerEntry is one cached lookup.
type catchUpMarkerEntry struct {
	marker catchUpMarker
	found  bool
	at     time.Time
}

func catchUpMarkerCacheKey(owner, repo string, pr int, head string) string {
	return fmt.Sprintf("%s/%s#%d@%s", owner, repo, pr, head)
}

// catchUpMarkerCached returns the catch-up marker for head on the PR, reading the PR's
// comments at most once per catchUpNoMarkerTTL (found markers: once). A read error is
// returned and never cached.
func (e *Engine) catchUpMarkerCached(owner, repo string, pr int, head string) (catchUpMarker, bool, error) {
	key := catchUpMarkerCacheKey(owner, repo, pr, head)
	now := e.now()
	e.trainCatchUp.mu.Lock()
	ent, hit := e.trainCatchUp.markers[key]
	e.trainCatchUp.mu.Unlock()
	if hit && (ent.found || now.Sub(ent.at) < catchUpNoMarkerTTL) {
		return ent.marker, ent.found, nil
	}
	comments, err := e.client.FetchIssueComments(owner, repo, pr)
	if err != nil {
		return catchUpMarker{}, false, err
	}
	mk, ok := catchUpMarkerForHead(comments, e.selfLogin(), head)
	e.trainCatchUp.mu.Lock()
	if e.trainCatchUp.markers == nil || len(e.trainCatchUp.markers) >= catchUpMarkerCacheMax {
		e.trainCatchUp.markers = make(map[string]catchUpMarkerEntry)
	}
	e.trainCatchUp.markers[key] = catchUpMarkerEntry{marker: mk, found: ok, at: now}
	e.trainCatchUp.mu.Unlock()
	return mk, ok, nil
}

// forgetCatchUpMarker drops the cached lookup for a head, called once the worker has
// posted that head's marker.
func (e *Engine) forgetCatchUpMarker(owner, repo string, pr int, head string) {
	e.trainCatchUp.mu.Lock()
	defer e.trainCatchUp.mu.Unlock()
	delete(e.trainCatchUp.markers, catchUpMarkerCacheKey(owner, repo, pr, head))
}
