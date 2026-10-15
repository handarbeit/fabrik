package attention

import "strings"

// Red CI on the linked PR (#2122, ADR 2122).
//
// A red verdict is additional information about an item the classifier has
// already placed, not a new state: every existing row (and so the pause,
// escalation, suspended and CI-gated classifications) is exactly what it was,
// and Classify only overlays a reason, a summary prefix and, when nothing is
// handling it, an attention-view rank. Nothing here makes the engine act on a
// red CI; it changes what is reported.

// RankCIRed ranks an item whose linked PR has a red CI verdict that no worker is
// addressing. It sits below every state that needs a decision (lower numbers
// are more urgent) and is new, so no existing rank moves.
const RankCIRed = 4

// CodeCIRed is the Reason code the overlay adds.
const CodeCIRed = "ci:red"

const ciRedMeaning = "the linked PR's head has a failing check"

// CIFact is the linked PR's CI verdict as the daemon holds it. A nil Input.CI
// means there is nothing to report: the verdict is unknown, is for another head,
// or the PR is not open.
type CIFact struct {
	// Verdict is green, red, pending, none or unknown.
	Verdict string
	// Failing names the failing checks when the verdict is red; empty when the
	// verdict came from a required status context rather than a check run.
	Failing []string
	// Stale is true when the verdict was last confirmed more than one poll
	// interval ago. A stale verdict is shown by the status tool but never
	// surfaced as an attention item.
	Stale bool
	// Draft is true when the PR is a draft: red CI is routine while a stage is
	// still pushing, so it is reported but not ranked.
	Draft bool
	// PROpen is true when the PR is open and not merged.
	PROpen bool
}

// overlayCI adds a red CI verdict to res. It returns res unchanged unless every
// one of these holds: the item is managed and open, not waiting on a CI gate
// (fabrik:awaiting-ci), already classified as waiting, working, idle or stalled
// (needs-human, escalated and suspended keep their classification), and the
// verdict is a fresh red one on an open PR.
func overlayCI(in Input, res Result) Result {
	c := in.CI
	if c == nil || c.Verdict != "red" || c.Stale || !c.PROpen {
		return res
	}
	if in.Closed || in.Terminal || in.StageKind != KindManaged || hasLabel(in.Labels, "fabrik:awaiting-ci") {
		return res
	}
	switch res.State {
	case Waiting, Working, Idle, Stalled:
	default:
		return res
	}

	res.Reasons = append(append([]Reason(nil), res.Reasons...), Reason{Code: CodeCIRed, Meaning: ciRedMeaning})
	res.CIFailing = append([]string(nil), c.Failing...)

	text := "CI is red"
	if len(c.Failing) > 0 {
		text += " on " + strings.Join(c.Failing, ", ")
	} else {
		text += " (a required status context failed)"
	}
	if c.Draft {
		text += " (draft PR)"
	}
	switch {
	case in.HasWorker || in.Conflict != nil:
		if in.WorkerKind == WorkerKindCIFixReinvoke && in.HasWorker {
			text += "; " + workerTextFor(in) + " is addressing it"
		} else {
			text += "; " + workerTextFor(in) + " is running"
		}
	case !c.Draft && (res.Rank == NotRanked || res.Rank > RankCIRed):
		res.Rank = RankCIRed
	}
	res.Summary = text + "; " + res.Summary
	return res
}
