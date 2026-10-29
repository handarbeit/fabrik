package engine

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

// The declined at-head finding decision pause (#2466, ADR 2466).
//
// A declared bot reviewer (expected_reviewers) that reviews only on push never
// reviews again when Review answers its CHANGES_REQUESTED with a deliberate
// no-change: the verdict stays on the head, the gate waits for a review that
// cannot come and the item pauses on "review wait timeout" — again after every
// unpause. handleReviewGate calls declinedAtHeadFindings from its "nothing
// actionable" tail, level-triggered from durable inputs (the review history, the
// head and the processed marker of the review body), so a restart or a resume
// that changes nothing detects it again.
//
// The finding is never treated as answered (R4): the only writes are the pause
// pair, its cause label and EnginePaused. The standing request stays, so the gate
// still blocks after a resume until a reviewer, a dismissal or a human changes it.

// declinedFinding is one at-head change request whose body a review reinvoke
// already processed without moving the head.
type declinedFinding struct {
	Author   string
	ReviewID int
	Body     string
}

// declinedFindingsFrom is the pure decision core: the standings that are a
// CHANGES_REQUESTED on exactly head, from a declared reviewer, with a body and a
// stable review ID, whose body is already processed. Every other shape —
// including an unknown head — yields nothing (fail toward today's behaviour).
func declinedFindingsFrom(standings []reviewerStanding, head string, declared []string, processed func(reviewID int) bool) []declinedFinding {
	if head == "" || len(declared) == 0 {
		return nil
	}
	var out []declinedFinding
	for _, s := range standings {
		if s.Kind != standingChangesRequested || s.VerdictState != "CHANGES_REQUESTED" || s.VerdictCommit != head {
			continue
		}
		if !isDeclaredReviewer(declared, s.Author) || s.VerdictID == 0 || strings.TrimSpace(s.VerdictBody) == "" {
			continue
		}
		if !processed(s.VerdictID) {
			continue
		}
		out = append(out, declinedFinding{Author: s.Author, ReviewID: s.VerdictID, Body: s.VerdictBody})
	}
	return out
}

// declinedFindingExcerptMax caps the quoted reviewer text, in runes.
const declinedFindingExcerptMax = 400

var (
	declinedMentionRE = regexp.MustCompile(`@([A-Za-z0-9])`)
	declinedMarkerRE  = regexp.MustCompile(`(?i)<!--`)
)

// sanitizeReviewExcerpt makes reviewer-authored text safe to quote in a Fabrik
// comment: one line, no backticks, no live @mentions, no HTML comment opener (so
// it cannot forge an engine or Pruefer marker), capped.
func sanitizeReviewExcerpt(s string) string {
	s = strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ", "\t", " ", "`", "'").Replace(s)
	s = strings.Join(strings.Fields(s), " ")
	s = declinedMarkerRE.ReplaceAllString(s, "&lt;!--")
	s = declinedMentionRE.ReplaceAllString(s, "@\u200b$1")
	if utf8.RuneCountInString(s) > declinedFindingExcerptMax {
		r := []rune(s)
		s = strings.TrimSpace(string(r[:declinedFindingExcerptMax])) + "…"
	}
	return s
}

// declinedFindingFragment is the stable first line of the decision comment — the
// hasPauseComment dedup key. It names the head and the review IDs, so a new head
// or a new change-request review is a new episode with a fresh comment.
func declinedFindingFragment(head string, findings []declinedFinding) string {
	ids := make([]string, 0, len(findings))
	for _, f := range findings {
		ids = append(ids, fmt.Sprintf("%d", f.ReviewID))
	}
	return fmt.Sprintf("A reviewer's change request on head `%s` was reviewed and declined (review id(s) %s).",
		standingSHA(head), strings.Join(ids, ", "))
}

// declinedAtHeadFindings reports the declined at-head findings for the item, with
// the head they stand on. It reads live (history and head), and only after the
// cheap gates pass. found is empty on any doubt.
func (e *Engine) declinedAtHeadFindings(item gh.ProjectItem, stage *stages.Stage) (findings []declinedFinding, head string) {
	if e.declinedReviewPauseOff.Load() {
		return nil, ""
	}
	if e.effectiveReviewAuthority(item, stage) != "authoritative" {
		return nil, ""
	}
	var declared []string
	if d := e.effectiveExpectedReviewers(item, stage); d != nil {
		declared = *d
	}
	if len(declared) == 0 {
		return nil, ""
	}
	repoStr := itemOwnerRepoString(item, e.defaultRepo())
	snap, err := e.store.Get(repoStr, item.Number)
	if err != nil || snap.Worker() != nil {
		return nil, ""
	}
	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	prNumber := item.LinkedPRNumber
	if prNumber == 0 {
		pr, perr := e.readClient.FetchLinkedPR(owner, repo, item.Number)
		if perr != nil || pr == nil || pr.Number == 0 {
			return nil, ""
		}
		prNumber = pr.Number
	}
	history, head, err := e.liveReviewHistory(owner, repo, item.Number, prNumber)
	if err != nil || head == "" {
		return nil, ""
	}
	standings := reviewerStandings(history, head, declared)
	var durable map[int]bool
	durableLoaded := false
	findings = declinedFindingsFrom(standings, head, declared, func(id int) bool {
		if !snap.CommentProcessed(reviewBodyIDPrefix + fmt.Sprintf("%d", id)).IsZero() {
			return true
		}
		if !durableLoaded {
			durable, durableLoaded = e.durablyAddressedReviewIDs(item), true
		}
		return durable[id]
	})
	return findings, head
}

// pauseForDeclinedReviewFinding pauses the item with a decision comment naming
// each reviewer and the finding(s) declined. Dedup is by head and review IDs
// (hasPauseComment): a repeat on the same episode reapplies the pause without a
// new comment. EnginePaused is applied on both branches (as
// pauseForReviewCycleLimit does, #1460) so a label-removal resume resets the
// review counters.
func (e *Engine) pauseForDeclinedReviewFinding(item gh.ProjectItem, stage *stages.Stage, head string, findings []declinedFinding) {
	repoStr := itemOwnerRepoString(item, e.defaultRepo())
	fragment := declinedFindingFragment(head, findings)
	if hasPauseComment(item, fragment) {
		e.logf(item.Number, "review-decline", "declined-finding decision comment already exists for this episode — reapplying pause without reposting\n")
		e.reapplyPauseLabels(item, pauseCauseQuestion)
		e.store.Apply(itemstate.EnginePaused{Repo: repoStr, Number: item.Number, StageName: stage.Name})
		return
	}
	e.logf(item.Number, "review-decline", "at-head change request from a declared reviewer was reviewed and declined with no new commit (head %s) — pausing for a decision\n", standingSHA(head))

	var list strings.Builder
	for _, f := range findings {
		fmt.Fprintf(&list, "- **%s** (review %d): %s\n", f.Author, f.ReviewID, sanitizeReviewExcerpt(f.Body))
	}
	msg := fmt.Sprintf(
		"🏭 **Fabrik — review decision needed**\n\n%s\n\n"+
			"The stage **%s** looked at the finding(s) below and pushed no change, so the reviewer will not "+
			"review this commit again by itself. Fabrik does not treat a finding as answered on its own say-so, so "+
			"the review gate stays closed until you decide:\n\n%s\n"+
			"Options:\n"+
			"- Have the reviewer review again (these bots review on a push or when a PR is marked ready, so push a commit or re-run the bot by hand).\n"+
			"- Dismiss the review on GitHub if you judge the finding wrong.\n"+
			"- Apply the `review-authority:advisory` label to this issue to override the gate.\n\n"+
			"Then remove the `fabrik:paused` label to resume.",
		fragment, stage.Name, list.String(),
	)
	e.pauseIssue(item, msg, pauseOpts{
		awaitingInput: true,
		reactRocket:   true,
		cause:         pauseCauseQuestion,
	})
	e.store.Apply(itemstate.EnginePaused{Repo: repoStr, Number: item.Number, StageName: stage.Name})
}

// SetDeclinedReviewPauseDisabledForTest turns the declined-finding decision pause
// off on this engine (the neutralisation seam the non-vacuity tests use).
func (e *Engine) SetDeclinedReviewPauseDisabledForTest(off bool) { e.declinedReviewPauseOff.Store(off) }
