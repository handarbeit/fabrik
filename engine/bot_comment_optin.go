package engine

import (
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// botCommentOptIn builds the predicate filterNewComments uses to decide whether
// a bot-authored conversation comment is feedback (#2552, ADR 2552). A comment
// whose author is a bot login (gh.IsBotLogin) is not feedback unless the bot is
// opted in; this returns the opt-in test. A bot is opted in when it matches, by
// the gate's own identity matcher (reviewerIdentityMatches), either the stage's
// effective expected_reviewers (stage declaration as overridden by the
// expected-reviewers: label) or the stage's feedback_bots. The stage is
// reviewContextStage(status), so a holding-stage (Queued) item decides as the
// stage it was validated in (#2525).
//
// The result is never nil: a nil predicate handed to filterNewComments would
// also mean "no bot is opted in", but every reader must build it from the same
// (status, number, labels) so the dispatcher, the gate, the cache-only mirrors
// and the writers agree (TestBotCommentOptInAgreesAcrossReaders). With the test
// seam on, every bot is opted in, which restores the pre-#2552 behaviour in
// every reader and writer at once. Config and labels only: no GitHub reads.
func (e *Engine) botCommentOptIn(status string, number int, labels []string) func(author string) bool {
	if e.botCommentRuleOff.Load() {
		return func(string) bool { return true }
	}
	stage := e.reviewContextStage(status)
	if stage == nil {
		stage = &stages.Stage{}
	}
	var names []string
	if declared := e.effectiveExpectedReviewers(gh.ProjectItem{Number: number, Labels: labels}, stage); declared != nil {
		names = append(names, *declared...)
	}
	names = append(names, stage.FeedbackBots...)
	return func(author string) bool {
		for _, n := range names {
			if reviewerIdentityMatches(n, author) {
				return true
			}
		}
		return false
	}
}

// skipsBotComment reports whether c is a bot-authored comment from a bot that is
// not opted in, i.e. one the funnel drops. The writers that would otherwise
// react to or watermark every seen comment (markCommentsSeenByStage,
// settleBotServiceNoticesForItem) use it so a skipped bot comment stays derived
// state (R5): no reaction, no processed marker.
func skipsBotComment(c gh.Comment, botOptedIn func(author string) bool) bool {
	return gh.IsBotLogin(c.Author) && (botOptedIn == nil || !botOptedIn(c.Author))
}

// SetBotCommentRuleDisabledForTest turns the bot-comment rule off on this engine:
// every bot is treated as opted in, in every reader and writer (the
// neutralisation seam the non-vacuity tests and the sim twin's control run use).
func (e *Engine) SetBotCommentRuleDisabledForTest(off bool) { e.botCommentRuleOff.Store(off) }
