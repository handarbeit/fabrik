package engine

// bisectState is the merge-train red-batch bisection (ADR-059 D4) as plain data
// (#2051, ADR 2051). It replaces the recursive bisect: that function made its one
// recursive call as a tail call, so its entire state is the set currently known to
// be red, which half is next, how many validations the episode has spent and the
// cost cap — five values that survive a daemon restart by being persisted with the
// trial record.
//
// The state is pure (no engine, GitHub or git access) so it can be driven both by
// the synchronous worker driver and by the per-poll evaluator, and compared with a
// reference copy of the old recursion in the tests.
//
// Members are issue numbers; the engine maps them back to trainMember values.
type bisectState struct {
	// Origin is the batch the episode started from (the full red set). It is what the
	// one-at-a-time fallback lands, what a poisoner's ejection reports as "the other
	// batch members", and what is released when the episode is abandoned.
	Origin []int `json:"origin"`
	// Red is the set currently known to be red (the origin, then — recursively — the
	// survivors of the half just found red).
	Red []int `json:"red"`
	// NextHalf is which half of Red is validated next: 0 = Red[:mid], 1 = Red[mid:],
	// 2 = both halves were green (a non-isolable interaction).
	NextHalf int `json:"next_half"`
	// Used counts validations spent in this episode, starting at 1 for the initial red.
	Used int `json:"used"`
	// Cap is the per-episode validation budget, read once when the episode starts so a
	// config change mid-run cannot shift it.
	Cap int `json:"cap"`
	// RedDiag is the diagnostic of the validation that established Red as red; the
	// isolating run's diagnostic reaches ejectMember by being carried here rather than
	// by shared state.
	RedDiag *trainCIDiagnostic `json:"red_diag,omitempty"`
}

// bisectAction is what a bisectState asks the driver to do next.
type bisectAction int

const (
	// bisectIsolated: len(Red) == 1 — the single member is the poisoner; no further
	// validation is issued (the base case of the old recursion).
	bisectIsolated bisectAction = iota
	// bisectFallbackCap: the cost cap is reached before the next half — degrade to the
	// one-at-a-time fallback.
	bisectFallbackCap
	// bisectFallbackSplit: both halves were green — the redness spans the split.
	bisectFallbackSplit
	// bisectTrial: validate the returned half.
	bisectTrial
)

// newBisectState starts an episode on a validated-red set. The initial red validation
// counts as the first of the budget.
func newBisectState(red []int, diag *trainCIDiagnostic, costCap int) *bisectState {
	return &bisectState{
		Origin:  append([]int(nil), red...),
		Red:     append([]int(nil), red...),
		Used:    1,
		Cap:     costCap,
		RedDiag: diag,
	}
}

// next reports the next step. For bisectTrial it also returns the half to validate,
// computed exactly as the recursion did: mid := len(red)/2, halves red[:mid] then
// red[mid:], and the cost cap is checked BEFORE each half.
func (b *bisectState) next() (bisectAction, []int) {
	if len(b.Red) == 1 {
		return bisectIsolated, nil
	}
	if b.NextHalf >= 2 {
		return bisectFallbackSplit, nil
	}
	if b.Used >= b.Cap {
		return bisectFallbackCap, nil
	}
	mid := len(b.Red) / 2
	if b.NextHalf == 0 {
		return bisectTrial, b.Red[:mid]
	}
	return bisectTrial, b.Red[mid:]
}

// spent records that a half trial has run (the old `*used++`, which follows the
// validation and precedes the cancel / assembly-error / runaway / infra checks).
func (b *bisectState) spent() { b.Used++ }

// apply folds a half trial's outcome in. A red half that still has survivors becomes
// the new red set and bisection restarts at its first half (recursion into survivors,
// not into the half itself — members ejected during assembly are not in survivors);
// anything else — green, pending, or a half whose members were all ejected — moves on
// to the next half.
func (b *bisectState) apply(red bool, survivors []int, diag *trainCIDiagnostic) {
	if red && len(survivors) > 0 {
		b.Red = append([]int(nil), survivors...)
		b.RedDiag = diag
		b.NextHalf = 0
		return
	}
	b.NextHalf++
}

// poisoner returns the isolated member (valid when next() reported bisectIsolated).
func (b *bisectState) poisoner() int { return b.Red[0] }
