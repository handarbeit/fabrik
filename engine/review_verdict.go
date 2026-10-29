package engine

import (
	"encoding/json"
	"fmt"
	"sort"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/internal/localapi"
	"github.com/handarbeit/fabrik/stages"
)

// This file builds the per-reviewer verdicts (#2127 R5) and the web URLs the
// read surface derives (R2, R5). It reads only the Store's cached state through
// the pure cores the feedback gate shares: no client, no live review fetch and
// not the gate itself, so the local API and channel no-GitHub scans stay valid
// for the callers in localapi*.go and channel_*.go.

// webURL is the browser address of an issue ("issues") or a pull request
// ("pull") on the engine's GitHub host (github.com unless Config.GHESHost is
// set). It is "" for a repo key that is not owner/name.
func (e *Engine) webURL(repo, kind string, number int) string {
	if !isRealRepoKey(repo) || number <= 0 {
		return ""
	}
	host := "github.com"
	if e.cfg.GHESHost != "" {
		host = e.cfg.GHESHost
	}
	return fmt.Sprintf("https://%s/%s/%s/%d", host, repo, kind, number)
}

// reviewerVerdicts reports, for each reviewer who has a latest non-dismissed,
// non-pending review, what that review says about the PR head. It returns an
// empty (non-nil) slice when there is no linked PR or no review.
//
// Everything is evaluated at head = lpr.AgreedHead() (#2122): when the
// engine's head and the facts' head disagree the head is unknown, and so are
// the fields that depend on it. An unknown input never yields a clean pass.
func (e *Engine) reviewerVerdicts(st *itemstate.ItemState) []localapi.ReviewerVerdict {
	out := []localapi.ReviewerVerdict{}
	lpr := st.LinkedPR
	if lpr == nil || lpr.Number == 0 || readPolishDisabled(5) {
		return out
	}
	head := lpr.AgreedHead()
	prURL := e.webURL(st.Repo, "pull", lpr.Number)

	// The latest review per reviewer, in first-seen order, as reviewSummary does.
	latest := map[string]gh.PRReview{}
	var order []string
	for _, r := range lpr.Reviews {
		if r.State == "DISMISSED" || r.State == "PENDING" {
			continue
		}
		if _, seen := latest[r.Author]; !seen {
			order = append(order, r.Author)
		}
		latest[r.Author] = r
	}
	sort.Strings(order)

	// Each reviewer's standing at the head, through the one definition the
	// authoritative gate shares (#2465, R3): the Store's latest review per author
	// plus its standing evidence (the latest formal verdict and a later COMMENTED
	// from the gate's last live full-history read).
	standings := map[string]reviewerStanding{}
	if !e.reviewStandingOff.Load() {
		var declared []string
		// A status with no configured stage (parking column, unknown) declares no
		// bots; a per-issue expected-reviewers label still applies.
		stage := e.stageByName(st.Status)
		if stage == nil {
			stage = &stages.Stage{}
		}
		if d := e.effectiveExpectedReviewers(gh.ProjectItem{Number: st.Number, Labels: st.Labels}, stage); d != nil {
			declared = *d
		}
		for _, s := range reviewerStandings(standingInput(lpr), head, declared) {
			standings[reviewerKey(s.Author)] = s
		}
	}

	notProcessed := func(string) bool { return false }
	processed := func(id string) bool { return !st.StageState.ProcessedComments[id].IsZero() }
	var durable map[int]bool // resolved on first use: it scans the cached comments

	// What the feedback gate would hold on, per author (the same cores as
	// cachedPendingFeedbackOf, kept per reviewer).
	gateThreads := dropOutdatedThreadComments(filterReviewThreadComments(lpr.ThreadComments, processed))
	skipClean := e.cleanReviewSkipper(st.Status, st.Number, st.Labels)
	gateBodies := reviewBodyCandidates(lpr.Reviews, processed, skipClean)

	for _, author := range order {
		rev := latest[author]
		v := localapi.ReviewerVerdict{
			Reviewer:     author,
			State:        rev.State,
			AtHead:       localapi.Unknown[bool](),
			InlineAtHead: localapi.Unknown[int](),
			Outstanding:  localapi.Unknown[int](),
			CleanPass:    localapi.Unknown[bool](),
			PRURL:        prURL,
		}
		if head != "" && rev.CommitID != "" {
			v.AtHead = localapi.Some(rev.CommitID == head)
		}

		// Unresolved threads by this reviewer. Only unresolved threads are cached.
		inline, threads, inlineKnown := 0, 0, head != ""
		for _, c := range lpr.ThreadComments {
			if c.Author != author {
				continue
			}
			if head != "" {
				if c.CommitOID == "" {
					inlineKnown = false
				} else if c.CommitOID == head {
					inline++
				}
			}
		}
		for _, c := range dropOutdatedThreadComments(lpr.ThreadComments) {
			if c.Author == author {
				threads++
			}
		}
		if inlineKnown {
			v.InlineAtHead = localapi.Some(inline)
		}

		// The reviewer's latest review body counts while it is actionable and not
		// durably addressed; whether the engine already processed a thread does not
		// matter here, only whether it is still open.
		bodies := 0
		if cands := reviewBodyCandidates([]gh.PRReview{rev}, notProcessed, skipClean); len(cands) > 0 {
			if durable == nil {
				durable = e.cachedAddressedReviewIDs(st)
			}
			if !durable[rev.DatabaseID] {
				bodies = 1
			}
		}
		out2 := threads + bodies
		v.Outstanding = localapi.Some(out2)

		standing, hasStanding := standings[reviewerKey(author)]
		switch {
		case hasStanding && standing.blocks():
			// A standing change request, or a verdict on a commit that is no longer
			// the head (awaiting re-review), is not a pass, whatever the threads
			// say: resolving its threads does not withdraw it.
			v.CleanPass = localapi.Some(false)
		case rev.State == "CHANGES_REQUESTED" && !hasStanding:
			v.CleanPass = localapi.Some(false)
		case v.AtHead.Valid && !v.AtHead.V:
			v.CleanPass = localapi.Some(false) // a pass on an older head is never clean
		case v.AtHead.Valid && v.AtHead.V:
			v.CleanPass = localapi.Some(out2 == 0)
		}

		for _, c := range gateThreads {
			if c.Author == author {
				v.Unaddressed = true
			}
		}
		for _, c := range gateBodies {
			if c.review.Author != author {
				continue
			}
			if durable == nil {
				durable = e.cachedAddressedReviewIDs(st)
			}
			if !durable[c.review.DatabaseID] {
				v.Unaddressed = true
			}
		}
		out = append(out, v)
	}
	return out
}

// standingInput is the review history the reviewer view judges: the Store's
// latest review per author plus its standing evidence, without duplicates.
func standingInput(lpr *itemstate.LinkedPRState) []gh.PRReview {
	out := make([]gh.PRReview, 0, len(lpr.Reviews)+len(lpr.StandingReviews))
	seen := map[int]bool{}
	for _, list := range [][]gh.PRReview{lpr.StandingReviews, lpr.Reviews} {
		for _, r := range list {
			if r.DatabaseID != 0 {
				if seen[r.DatabaseID] {
					continue
				}
				seen[r.DatabaseID] = true
			}
			out = append(out, r)
		}
	}
	return out
}

// reviewersJSON renders verdicts as the compact JSON array a channel event
// carries in its meta (meta values are strings). "" when there are none.
func reviewersJSON(vs []localapi.ReviewerVerdict) string {
	if len(vs) == 0 {
		return ""
	}
	b, err := json.Marshal(vs)
	if err != nil {
		return ""
	}
	return string(b)
}
