package engine

import (
	"fmt"
	"sort"
	"strings"

	gh "github.com/handarbeit/fabrik/github"
)

// This file is the one definition of a reviewer's verdict on the PR head
// (#2465, ADR 2465). The authoritative gate's no-branch-protection fallback
// (checkReviewGate, reviewGateBlocksLanding, pauseForReviewTimeout, through
// authorityVerdict) and the fabrik_status / channel reviewer view
// (reviewerVerdicts) all reach their per-reviewer verdict through
// reviewerStandings, so they cannot disagree about one review history.
//
// It is pure: no client, no Store. The callers supply the history, the head and
// the declared-reviewer set.

// reviewerStandingKind is a reviewer's standing against the current head.
type reviewerStandingKind int

const (
	// standingNoVerdict: no formal verdict (only COMMENTED reviews, or the
	// latest formal verdict was a dismissal). Never blocks, as before #2465.
	standingNoVerdict reviewerStandingKind = iota
	// standingClean: the reviewer's verdict on the head is not an objection.
	standingClean
	// standingChangesRequested: a CHANGES_REQUESTED stands — on the head, or on a
	// commit that cannot be shown to be stale (unknown head or commit; R5).
	standingChangesRequested
	// standingAwaitingReReview: the reviewer's latest formal verdict is on a
	// commit that is no longer the head, and nothing at the head supersedes it.
	standingAwaitingReReview
)

// reviewerStanding is one reviewer's standing, with the facts a reason needs.
type reviewerStanding struct {
	Author string
	Kind   reviewerStandingKind
	// VerdictState/VerdictCommit are the latest formal verdict's state and commit
	// ("" for standingNoVerdict). Head is the head the standing was judged at.
	VerdictState  string
	VerdictCommit string
	Head          string
}

// blocks reports whether the standing holds the authoritative gate.
func (s reviewerStanding) blocks() bool {
	return s.Kind == standingChangesRequested || s.Kind == standingAwaitingReReview
}

// reviewerKey is the identity a review author is grouped by: the same
// normalisation reviewerIdentityMatches applies, so the REST login "x[bot]" and
// the GraphQL login "x" are one reviewer.
func reviewerKey(author string) string {
	return strings.ToLower(botMentionHandle(stripBotSuffix(author)))
}

// reviewSubmittedBefore orders two submissions of one reviewer: by SubmittedAt
// when both are known and differ, else by database ID when both are known and
// differ, else not at all (the caller's sort is stable, so input order stands).
// It is the same rule as the Store's reviewNewer (internal/itemstate).
func reviewSubmittedBefore(a, b gh.PRReview) bool {
	if !a.SubmittedAt.Equal(b.SubmittedAt) && !a.SubmittedAt.IsZero() && !b.SubmittedAt.IsZero() {
		return a.SubmittedAt.Before(b.SubmittedAt)
	}
	if a.DatabaseID != b.DatabaseID && a.DatabaseID != 0 && b.DatabaseID != 0 {
		return a.DatabaseID < b.DatabaseID
	}
	return false
}

// isDeclaredReviewer reports whether author is named in the declared set.
func isDeclaredReviewer(declared []string, author string) bool {
	for _, d := range declared {
		if reviewerIdentityMatches(d, author) {
			return true
		}
	}
	return false
}

// reviewerStandings computes every reviewer's standing from the review history.
//
// Per reviewer, in submission order (PENDING never counts):
//
//	F = the latest APPROVED, CHANGES_REQUESTED or DISMISSED
//	C = the latest COMMENTED submitted after F
//
// F absent or DISMISSED → no verdict. Otherwise, with a known head and a known
// F commit:
//
//	F at head      APPROVED → clean; CHANGES_REQUESTED → changes requested,
//	               unless the reviewer is declared and C is at head → clean (R2)
//	F not at head  declared and C at head → clean (R2); else awaiting re-review (R1)
//
// An unknown head or an F without a commit cannot show F stale: CHANGES_REQUESTED
// stays a standing request (R5) and APPROVED stays non-blocking, as before.
// A COMMENTED review never withdraws a change request for an undeclared reviewer.
//
// The result is in first-seen reviewer order.
func reviewerStandings(history []gh.PRReview, head string, declared []string) []reviewerStanding {
	type group struct {
		author  string
		reviews []gh.PRReview
	}
	var order []string
	groups := map[string]*group{}
	for _, r := range history {
		if r.Author == "" || r.State == "PENDING" {
			continue
		}
		k := reviewerKey(r.Author)
		g, ok := groups[k]
		if !ok {
			g = &group{author: r.Author}
			groups[k] = g
			order = append(order, k)
		}
		g.reviews = append(g.reviews, r)
	}

	out := make([]reviewerStanding, 0, len(order))
	for _, k := range order {
		g := groups[k]
		sort.SliceStable(g.reviews, func(i, j int) bool { return reviewSubmittedBefore(g.reviews[i], g.reviews[j]) })
		var f, c *gh.PRReview
		for i := range g.reviews {
			switch g.reviews[i].State {
			case "APPROVED", "CHANGES_REQUESTED", "DISMISSED":
				f, c = &g.reviews[i], nil
			case "COMMENTED":
				c = &g.reviews[i]
			}
		}
		s := reviewerStanding{Author: g.author, Head: head}
		if f == nil || f.State == "DISMISSED" {
			out = append(out, s)
			continue
		}
		s.VerdictState, s.VerdictCommit = f.State, f.CommitID
		cleanComment := head != "" && c != nil && c.CommitID == head && isDeclaredReviewer(declared, g.author)
		switch {
		case head == "" || f.CommitID == "":
			// Unknown: cannot show the verdict stale (R5).
			if f.State == "CHANGES_REQUESTED" {
				s.Kind = standingChangesRequested
			} else {
				s.Kind = standingClean
			}
		case f.CommitID == head:
			switch {
			case f.State == "APPROVED", cleanComment:
				s.Kind = standingClean
			default:
				s.Kind = standingChangesRequested
			}
		default:
			if cleanComment {
				s.Kind = standingClean
			} else {
				s.Kind = standingAwaitingReReview
			}
		}
		out = append(out, s)
	}
	return out
}

// standingEvidence reduces a review history to what reviewerStandings needs and
// the Store can hold: per reviewer, the latest formal verdict and the latest
// COMMENTED submitted after it (at most two reviews), in first-seen order.
func standingEvidence(history []gh.PRReview) []gh.PRReview {
	var order []string
	groups := map[string][]gh.PRReview{}
	for _, r := range history {
		if r.Author == "" || r.State == "PENDING" {
			continue
		}
		k := reviewerKey(r.Author)
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], r)
	}
	var out []gh.PRReview
	for _, k := range order {
		rs := groups[k]
		sort.SliceStable(rs, func(i, j int) bool { return reviewSubmittedBefore(rs[i], rs[j]) })
		var f, c *gh.PRReview
		for i := range rs {
			switch rs[i].State {
			case "APPROVED", "CHANGES_REQUESTED", "DISMISSED":
				f, c = &rs[i], nil
			case "COMMENTED":
				c = &rs[i]
			}
		}
		if f != nil {
			out = append(out, *f)
		}
		if c != nil {
			out = append(out, *c)
		}
	}
	return out
}

// standingSHA abbreviates a commit for a reason ("unknown" when empty).
func standingSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	if sha == "" {
		return "unknown"
	}
	return sha
}

// reason is the operator-facing blocking reason for a standing that blocks. An
// awaiting-re-review reason never says the reviewer "requested changes": the
// verdict is on code that no longer exists.
func (s reviewerStanding) reason() string {
	const prefix = "no branch-protection review requirement; "
	if s.Kind == standingAwaitingReReview {
		return fmt.Sprintf("%s%s awaiting re-review (last verdict %s on %s, head %s)",
			prefix, s.Author, s.VerdictState, standingSHA(s.VerdictCommit), standingSHA(s.Head))
	}
	return fmt.Sprintf("%s%s requested changes", prefix, s.Author)
}

// firstBlockingStanding returns the first standing that holds the gate. A
// standing request on (or not provably off) the head is preferred over an
// awaited re-review, so the reason names the objection that stands.
func firstBlockingStanding(standings []reviewerStanding) (reviewerStanding, bool) {
	var awaiting *reviewerStanding
	for i := range standings {
		switch standings[i].Kind {
		case standingChangesRequested:
			return standings[i], true
		case standingAwaitingReReview:
			if awaiting == nil {
				awaiting = &standings[i]
			}
		}
	}
	if awaiting != nil {
		return *awaiting, true
	}
	return reviewerStanding{}, false
}
