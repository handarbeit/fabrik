package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// Session resume decision (#2300, ADR 2300).
//
// One pure function decides, above the ClaudeInvoker interface, whether a
// governed invocation resumes the stage's saved Claude session: a stage
// dispatch (passed on as the existing resume bool) or a comment-review-family
// pass (passed on as InvokeOptions.NoResume). The merge-train conflict sites
// keep their own NoResume: true and never call it.

// SetSessionDecisionForcedResumeForTest makes the option-governed part of the
// decision always resume, so a test can show its fresh/auto assertions bite
// (the non-vacuity seam). Continuations and resets are unaffected.
func (e *Engine) SetSessionDecisionForcedResumeForTest(forced bool) {
	e.sessionForcedResumeOff.Store(forced)
}

// SetContinuationExemptionDisabledForTest removes the continuation exemption:
// a continuation is governed by resume_session like a re-entry (R2's
// non-vacuity seam).
func (e *Engine) SetContinuationExemptionDisabledForTest(disabled bool) {
	e.continuationExemptionOff.Store(disabled)
}

// SetSessionDirForTest roots the session decision's lookups at <root>/.fabrik/sessions
// instead of the working directory, so a parallel sim test needs no t.Chdir.
// Production keeps sessionDirForItem. The real invoker is not affected.
func (e *Engine) SetSessionDirForTest(root string) {
	e.sessionRootForTest.Store(&root)
}

// SessionFileForTest returns the session pointer path the decision reads for an
// item's stage.
func (e *Engine) SessionFileForTest(item gh.ProjectItem, stageName string) string {
	return e.sessionFilePathFor(item, stageName)
}

// RecordSessionForTest seeds a saved session for an item's stage: the session
// pointer, its measured context size (tokens < 0 = unknown) and whether the
// last stage run closed. It is what a simclaude script calls, since the sim
// replaces the invoker and never runs the real recording.
func (e *Engine) RecordSessionForTest(item gh.ProjectItem, stageName, sessionID string, tokens int, closed bool) {
	path := e.sessionFilePathFor(item, stageName)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return
	}
	if err := os.WriteFile(path, []byte(sessionID), 0600); err != nil {
		return
	}
	updateSessionMeta(item.Number, path, func(m *sessionMeta) {
		m.SessionID = sessionID
		m.ContextTokens = nil
		if tokens >= 0 {
			m.ContextTokens = &tokens
		}
		m.Closed = closed
		m.StageRun = true
	})
}

func (e *Engine) sessionFilePathFor(item gh.ProjectItem, stageName string) string {
	var dir string
	if root := e.sessionRootForTest.Load(); root != nil {
		dir = sessionDirForItemUnder(*root, item)
	} else {
		dir = sessionDirForItem(item)
	}
	return filepath.Join(dir, sanitizeStageName(stageName)+".session")
}

// sessionInvocation says which kind of invocation is being decided.
type sessionInvocation int

const (
	// sessionStageDispatch is a stage run: continuations always resume, a
	// re-entry is governed by resume_session.
	sessionStageDispatch sessionInvocation = iota
	// sessionCommentReview is a comment-review-family pass (ordinary comment
	// review, review/ci-fix/rebase reinvoke): always governed.
	sessionCommentReview
)

// sessionDecision is the outcome of decideSessionResume.
type sessionDecision struct {
	// Resume is what the invocation passes on: the resume bool of a stage
	// dispatch, or !NoResume of a comment pass.
	Resume bool
	// Reason is the R5 log text; empty when nothing is logged (no saved session).
	Reason string

	sessFile     string
	consumeReset bool
}

// formatTokensK renders a token count as the R5 "<N>k" figure.
func formatTokensK(tokens int) string {
	if tokens%1000 == 0 {
		return fmt.Sprintf("%dk", tokens/1000)
	}
	return fmt.Sprintf("%.1fk", float64(tokens)/1000)
}

// decideSessionResume decides whether to resume the stage's saved session. It
// reads the session pointer and its meta record but writes and logs nothing;
// applySessionDecision does that. legacyContinuation is the in-memory "this
// stage has run before" signal (LastAttemptAt non-zero), used only for a saved
// session that has no meta record (one that predates ADR 2300).
//
// Stage dispatch, saved session present:
//  1. reset flag set            → fresh (explicit state reset, one-shot)
//  2. no stage-run outcome      → legacy: continuation iff legacyContinuation
//  3. last run not closed       → continuation: resume, whatever the option says
//  4. last run closed (re-entry) → governed by resume_session
//
// Comment review, saved session present: always governed by resume_session.
// No saved session: nothing to decide — Resume is the legacy default so the
// invoker's own "resume requested but none exists" warning is unchanged.
func (e *Engine) decideSessionResume(item gh.ProjectItem, stage *stages.Stage, kind sessionInvocation, legacyContinuation bool) sessionDecision {
	sessFile := e.sessionFilePathFor(item, stage.Name)
	d := sessionDecision{sessFile: sessFile}

	id, status, _ := readSessionIDForResume(sessFile)
	if status != resumeFound {
		d.Resume = kind == sessionCommentReview || legacyContinuation
		return d
	}

	if kind == sessionStageDispatch {
		meta, found := readSessionMeta(sessFile)
		switch {
		case found && meta.Reset:
			d.Reason = "fresh session (stage state was reset)"
			d.consumeReset = true
			return d
		case !found || !meta.StageRun:
			// No stage-dispatch outcome on record: no meta at all, or one created
			// only by a comment-review pass over a pre-ADR-2300 session.
			if legacyContinuation && !e.continuationExemptionOff.Load() {
				d.Resume, d.Reason = true, fmt.Sprintf("resuming session %s (continuation)", id)
			} else if legacyContinuation {
				return e.governedSessionDecision(d, stage, id, sessFile)
			} else {
				d.Reason = "fresh session (no prior run recorded)"
			}
			return d
		case !meta.Closed && !e.continuationExemptionOff.Load():
			d.Resume, d.Reason = true, fmt.Sprintf("resuming session %s (continuation)", id)
			return d
		}
	}
	return e.governedSessionDecision(d, stage, id, sessFile)
}

// governedSessionDecision applies resume_session to a saved session.
func (e *Engine) governedSessionDecision(d sessionDecision, stage *stages.Stage, id, sessFile string) sessionDecision {
	if e.sessionForcedResumeOff.Load() {
		d.Resume, d.Reason = true, fmt.Sprintf("resuming session %s (resume_session: forced)", id)
		return d
	}
	switch stage.ResumeMode() {
	case stages.ResumeSessionFresh:
		d.Reason = "fresh session (resume_session: fresh)"
	case stages.ResumeSessionAuto:
		threshold := stage.ResumeThreshold()
		tokens, known := savedSessionContextTokens(sessFile)
		switch {
		case !known:
			d.Resume, d.Reason = true, fmt.Sprintf("resuming session %s (size unknown)", id)
		case tokens > threshold:
			d.Reason = fmt.Sprintf("fresh session (saved session %s > threshold %s)", formatTokensK(tokens), formatTokensK(threshold))
		default:
			d.Resume, d.Reason = true, fmt.Sprintf("resuming session %s (%s tokens, threshold %s)", id, formatTokensK(tokens), formatTokensK(threshold))
		}
	default:
		d.Resume, d.Reason = true, fmt.Sprintf("resuming session %s (resume_session: resume)", id)
	}
	return d
}

// applySessionDecision logs the decision once and consumes a one-shot reset.
// Called right before the invocation so an early return between the decision
// and the invoke (a spawn, say) neither logs nor loses the reset.
func (e *Engine) applySessionDecision(item gh.ProjectItem, d sessionDecision) {
	if d.Reason != "" {
		e.logf(item.Number, "session", "%s\n", d.Reason)
	}
	if d.consumeReset {
		updateSessionMeta(item.Number, d.sessFile, func(m *sessionMeta) { m.Reset = false })
	}
	if d.freshOverSaved() {
		// The saved session is discarded on purpose. Drop its pointer now: a fresh
		// run that dies before the CLI reports a session id (timeout kill, cancel,
		// unparsable output) never overwrites it, and the next dispatch would then
		// read "last run not closed" as a continuation and resume the very session
		// the operator asked to leave behind. The meta stays; it is bound to the
		// old session id, so its size never applies to the next session.
		if err := os.Remove(d.sessFile); err != nil && !os.IsNotExist(err) {
			e.logf(item.Number, "session", "warning: removing discarded session pointer %s: %v\n", d.sessFile, err)
		}
	}
}

// freshOverSaved reports whether a stage dispatch decided to start a fresh
// session over a saved one — the case that needs the stage's own previous
// output as context (R4).
func (d sessionDecision) freshOverSaved() bool {
	return !d.Resume && strings.HasPrefix(d.Reason, "fresh session")
}

// markSessionClosed records whether the stage dispatch that just ran ended with
// the stage-complete marker (the continuation-vs-re-entry signal).
func (e *Engine) markSessionClosed(item gh.ProjectItem, stageName string, closed bool) {
	updateSessionMeta(item.Number, e.sessionFilePathFor(item, stageName), func(m *sessionMeta) { m.Closed, m.StageRun = closed, true })
}

// markSessionReset sets the one-shot "next stage dispatch starts fresh" flag
// next to the four explicit LastAttemptAt clears (R7): they are deliberate
// stage resets, and the in-memory clear alone would make the result depend on
// uptime.
func (e *Engine) markSessionReset(item gh.ProjectItem, stageName string) {
	updateSessionMeta(item.Number, e.sessionFilePathFor(item, stageName), func(m *sessionMeta) { m.Reset = true })
}
