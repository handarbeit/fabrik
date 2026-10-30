package engine

import (
	"regexp"
	"strconv"
	"strings"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// findingsMarkerOpenerRE matches anything that could be read as the start of
// Pruefer's findings-count marker (#2488, ADR 2488). It mirrors the producer's
// look-alike pattern: not line-anchored, case-insensitive.
var findingsMarkerOpenerRE = regexp.MustCompile(`(?i)<!--\s*pruefer:findings`)

// parseFindingsMarker reads the findings-count marker out of a review body
// (#2489, ADR 2489). ok is true only when every doubt is resolved in favour of
// a real marker:
//   - the whole body holds exactly one opener (Pruefer neutralises look-alikes
//     elsewhere, so a second one is a quote or a forgery);
//   - that opener is on the last non-empty line, which closes with "-->";
//   - `inline` and `body` each appear exactly once as non-negative integers
//     (`unknown`, a non-integer or a negative value is not ok).
//
// Whitespace and unknown trailing key=value pairs are tolerated (ADR 2488's
// parser contract). `coverage` is never consulted.
func parseFindingsMarker(body string) (inline, bodyCount int, ok bool) {
	if len(findingsMarkerOpenerRE.FindAllStringIndex(body, 2)) != 1 {
		return 0, 0, false
	}
	lines := strings.Split(strings.TrimRight(body, " \t\r\n"), "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	if !strings.HasPrefix(last, "<!--") || !strings.HasSuffix(last, "-->") {
		return 0, 0, false
	}
	fields := strings.Fields(strings.TrimSuffix(strings.TrimPrefix(last, "<!--"), "-->"))
	if len(fields) == 0 || fields[0] != "pruefer:findings" {
		return 0, 0, false
	}
	var gotInline, gotBody bool
	for _, f := range fields[1:] {
		k, v, found := strings.Cut(f, "=")
		if !found {
			continue
		}
		switch k {
		case "inline", "body":
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 || strings.HasPrefix(v, "+") {
				return 0, 0, false
			}
			if k == "inline" {
				if gotInline {
					return 0, 0, false
				}
				gotInline, inline = true, n
			} else {
				if gotBody {
					return 0, 0, false
				}
				gotBody, bodyCount = true, n
			}
		}
	}
	if !gotInline || !gotBody {
		return 0, 0, false
	}
	return inline, bodyCount, true
}

// cleanFindingsMarker reports whether body carries a well-formed marker
// reporting zero inline and zero body findings.
func cleanFindingsMarker(body string) bool {
	inline, bodyCount, ok := parseFindingsMarker(body)
	return ok && inline == 0 && bodyCount == 0
}

// cleanReviewSkipper builds the predicate reviewBodyCandidates uses to drop a
// provably clean review (#2489, ADR 2489). A review is skipped only when its
// state is COMMENTED or APPROVED (a CHANGES_REQUESTED review stays actionable,
// so the authoritative verdict can never block with no worker to clear it), its
// author matches the stage's effective expected_reviewers through the gate's own
// identity matcher, and its body carries a clean marker. It returns nil — skip
// nothing — when nothing is declared or the test seam is on. The stage is
// reviewContextStage(status): for a holding-stage (Queued) item the stage the
// member was validated in, never the holding stage (#2525). Every reader of the
// funnel must build it from the same (status, number, labels) so the gate, the
// dispatcher view and the cache-only mirrors agree.
func (e *Engine) cleanReviewSkipper(status string, number int, labels []string) func(gh.PRReview) bool {
	if e.cleanReviewSkipOff.Load() {
		return nil
	}
	stage := e.reviewContextStage(status)
	if stage == nil {
		stage = &stages.Stage{}
	}
	declared := e.effectiveExpectedReviewers(gh.ProjectItem{Number: number, Labels: labels}, stage)
	if declared == nil || len(*declared) == 0 {
		return nil
	}
	names := *declared
	return func(r gh.PRReview) bool {
		if r.State != "COMMENTED" && r.State != "APPROVED" {
			return false
		}
		if !cleanFindingsMarker(r.Body) {
			return false
		}
		for _, d := range names {
			if reviewerIdentityMatches(d, r.Author) {
				return true
			}
		}
		return false
	}
}

// reviewContextStage returns the stage whose expected_reviewers apply to an item
// whose board Status is status (#2525, ADR 2525). For an ordinary stage that is
// the stage itself. A holding stage (Queued) declares no reviewers, so a Queued
// member is judged against the stage it was validated in: stageBeforeHolding,
// the same stage the Queued review-finding ejection reroutes to (ADR 1208). It
// returns nil — "declares nothing", so nothing is skipped — when the status has
// no configured stage or the holding stage has no earlier stage. Config only: it
// reads no GitHub state.
func (e *Engine) reviewContextStage(status string) *stages.Stage {
	st := e.stageByName(status)
	if st == nil || e.holdingReviewCtxOff.Load() {
		return st
	}
	if st.HoldingStage && !st.Unmanaged {
		return stageBeforeHolding(e.cfg, st)
	}
	return st
}

// SetHoldingStageReviewContextDisabledForTest makes reviewContextStage resolve a
// holding-stage item against the holding stage itself, as before #2525 (the
// neutralisation seam for the sim twin's red run).
func (e *Engine) SetHoldingStageReviewContextDisabledForTest(off bool) {
	e.holdingReviewCtxOff.Store(off)
}

// SetCleanReviewSkipDisabledForTest turns the clean-review exclusion off on this
// engine (the neutralisation seam the non-vacuity tests use).
func (e *Engine) SetCleanReviewSkipDisabledForTest(off bool) { e.cleanReviewSkipOff.Store(off) }
