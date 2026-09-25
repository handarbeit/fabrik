//go:build e2e

package e2e

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Pure log-analysis for TestMergeTrainConflictBisectPrefixRerere. The scenario's
// assertions are about ORDER within the bed log (a Claude invocation before vs after
// the bisect line; a prefix-reuse line inside a window), which WaitForLogLine and
// CountLogLines cannot express, so the scenario reads the log once after landing and
// hands the lines to analyzeConflictTrainLog. Keeping this free of *testing.T and
// file IO lets harness-style unit tests cover it without a bed
// (mergetrain_conflict_log_test.go).
//
// Every string matched below is copied verbatim from engine source on main; the
// file:line cited is where it is emitted. Log formats are less stable than GitHub
// state (see WaitForLogLine) — a drift here fails the scenario loudly, never silently.

// maxConflictResolutionTurns is the ceiling assertion A2 puts on the turn count of
// the ONE real-Claude conflict-resolution invocation.
//
// The holding stage sets no max_turns/comment_max_turns, so the engine falls back to
// a 50-turn comment cap (engine/claude.go commentMaxTurns) — the same cap the
// pre-#1841 prompt burned through (51 turns on a JS bed, by instructing Claude to run
// a Go build). A single-file add/add resolution with the #1841 prompt needs only a
// handful of turns (read, edit, git add, git commit), so 20 is under half the cap and
// several times a plausible actual.
//
// There is no live baseline for this number yet: it is a margin-based estimate. The
// failure message prints the actual N; tune this constant after the first live
// release-gate run.
const maxConflictResolutionTurns = 20

// Substrings/patterns, with their emit sites on main.
const (
	// engine/merge_train.go:1145
	logBisecting = "bisecting to isolate the poisoner"
	// engine/merge_train.go:1537 (prefixed by "[#P merge-train] ")
	logBisectionIsolatedFmt = "bisection isolated #%d as the batch poisoner — ejecting"
	// engine/merge_train.go:1308
	logConflictResolvedFmt = "conflict for #%d resolved"
	// engine/merge_train.go:1355
	logCannotResolveFmt = "cannot resolve conflict for #%d — ejecting"
	// engine/merge_train.go:3027
	logRerereReplayFmt = "conflict for #%d fully resolved by git rerere replay — no Claude invocation"
	// engine/merge_train.go:1698 — the "ahead of poisoner #P" / "did not fully replay
	// via rerere" pair is what forgetPoisonerResolutions warns when it cannot replay
	// the trial-so-far state through rerere.
	logRerereWarnFmt = "ahead of poisoner #%d (earlier member #"
	logRerereWarnTag = "did not fully replay via rerere"
	// engine/merge_train.go:4452
	logMainMoved = "(main moved) — rebasing off the new base"
	// engine/merge_train_admission.go:81 — the admission gate (#1821) deferred a member
	// whose own PR CI is already red, before any bisection could happen.
	logDeferringFmt = "deferring #%d (own PR CI confirmed red"
	// engine/merge_train_admission.go:46 — the benign case (gate not active on this bed).
	logAdmissionGateSkipped = "admission gate skipped"
	// engine/poll.go:2479
	logBatchSnapshot = "batch snapshot for "
	// engine/claude.go:1382 — "[#N claude] invoking (<label>) in <dir>"
	logClaudeInvokingFmt = "[#%d claude] invoking ("
)

var (
	// engine/merge_train.go:1228 — "reusing a recorded prefix (%d/%d member(s) already merged) for trial %s"
	prefixReuseRe = regexp.MustCompile(`reusing a recorded prefix \((\d+)/(\d+) member\(s\) already merged\)`)
	// engine/claude.go:1770/:1772 — "used %d turns" (error / turn-limit exit) or
	// "completed in %d turns" (clean exit). The per-invocation line is per-member
	// scoped by its [#N claude] tag.
	claudeTurnsRe = regexp.MustCompile(`\[#(\d+) claude\] (?:used|completed in) (\d+) turns`)
	// One "#N "title"" entry of the batch snapshot line.
	snapshotEntryRe = regexp.MustCompile(`(?:^|, )#(\d+) "`)
)

// parseBatchSnapshotNumbers returns the issue numbers listed on an engine
// "batch snapshot for <repo>: N item(s) — #A "…", #B "…"" line, in order, or nil if
// the line is not a snapshot line.
func parseBatchSnapshotNumbers(line string) []int {
	i := strings.Index(line, logBatchSnapshot)
	if i < 0 {
		return nil
	}
	rest := line[i:]
	sep := strings.Index(rest, "item(s) — ")
	if sep < 0 {
		return nil
	}
	rest = rest[sep+len("item(s) — "):]
	var nums []int
	for _, m := range snapshotEntryRe.FindAllStringSubmatch(rest, -1) {
		if n, err := strconv.Atoi(m[1]); err == nil {
			nums = append(nums, n)
		}
	}
	return nums
}

// firstBatchSnapshotNumbers returns the issue numbers of the first batch snapshot line
// in lines, and whether one was found.
func firstBatchSnapshotNumbers(lines []string) ([]int, bool) {
	for _, l := range lines {
		if nums := parseBatchSnapshotNumbers(l); nums != nil {
			return nums, true
		}
	}
	return nil, false
}

// intsEqual reports whether a and b hold the same numbers in the same order.
func intsEqual(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// conflictTrainReport is the result of scanning the log window for the conflict
// scenario. Every idx field is a line index into the analysed slice, -1 when absent.
type conflictTrainReport struct {
	A, B, C, P int

	BisectIdx int // "bisecting to isolate the poisoner"
	// WindowEndIdx is the first line after BisectIdx that ends the assertion-A4 window:
	// a "(3/3 …)" prefix-reuse line (the [A,B,C] re-form) or a "(main moved)" rebase
	// line — whichever comes first. -1 if neither exists, in which case A4/A5 cannot be
	// evidenced (vacuity guard) and analysis reports a problem.
	WindowEndIdx int

	BInvokeIdxs []int // every "[#B claude] invoking (" line, whole window
	BTurns      int   // turn count of BInvokeIdxs[0]'s invocation, -1 when not found
	// ClaudeAfterBisect lists "[#N claude] invoking (" lines for A/B/C/P strictly
	// between BisectIdx and WindowEndIdx.
	ClaudeAfterBisect []string

	PrefixFullFirstHalfIdx int // "(2/2 member(s) already merged)" after BisectIdx and before WindowEndIdx
	Prefix3of3Idx          int // first "(3/3 …)" after BisectIdx

	MainMovedIdx     int // first "(main moved)" after BisectIdx
	BReplayIdx       int // "conflict for #B fully resolved by git rerere replay"
	BReplayAfterMove bool
	PReplayWarnIdx   int // forgetPoisonerResolutions' "did not fully replay via rerere" for P

	BResolvedIdx      int // "conflict for #B resolved"
	BCannotResolveIdx int
	PIsolatedIdx      int // "bisection isolated #P as the batch poisoner"
	PDeferredIdx      int // admission gate deferred P
}

// analyzeConflictTrainLog scans lines (the bed log from before the members were
// created) for the scenario's signals. a/b/c/p are the four members' issue numbers.
func analyzeConflictTrainLog(lines []string, a, b, c, p int) conflictTrainReport {
	r := conflictTrainReport{
		A: a, B: b, C: c, P: p,
		BisectIdx: -1, WindowEndIdx: -1, BTurns: -1,
		PrefixFullFirstHalfIdx: -1, Prefix3of3Idx: -1, MainMovedIdx: -1,
		BReplayIdx: -1, PReplayWarnIdx: -1,
		BResolvedIdx: -1, BCannotResolveIdx: -1, PIsolatedIdx: -1, PDeferredIdx: -1,
	}

	for i, l := range lines {
		if r.BisectIdx < 0 && strings.Contains(l, logBisecting) {
			r.BisectIdx = i
		}
	}

	for i, l := range lines {
		switch {
		case strings.Contains(l, fmt.Sprintf(logConflictResolvedFmt, b)) && r.BResolvedIdx < 0:
			r.BResolvedIdx = i
		case strings.Contains(l, fmt.Sprintf(logCannotResolveFmt, b)) && r.BCannotResolveIdx < 0:
			r.BCannotResolveIdx = i
		case strings.Contains(l, fmt.Sprintf(logBisectionIsolatedFmt, p)) && r.PIsolatedIdx < 0:
			r.PIsolatedIdx = i
		case strings.Contains(l, fmt.Sprintf(logDeferringFmt, p)) && r.PDeferredIdx < 0:
			r.PDeferredIdx = i
		case strings.Contains(l, fmt.Sprintf(logRerereReplayFmt, b)) && r.BReplayIdx < 0:
			r.BReplayIdx = i
		case strings.Contains(l, logRerereWarnTag) && strings.Contains(l, fmt.Sprintf(logRerereWarnFmt, p)) && r.PReplayWarnIdx < 0:
			r.PReplayWarnIdx = i
		}
		if strings.Contains(l, fmt.Sprintf(logClaudeInvokingFmt, b)) {
			r.BInvokeIdxs = append(r.BInvokeIdxs, i)
		}
	}

	// B's turn count: the first per-member turn line after its first invocation.
	if len(r.BInvokeIdxs) > 0 {
		for i := r.BInvokeIdxs[0] + 1; i < len(lines); i++ {
			if m := claudeTurnsRe.FindStringSubmatch(lines[i]); m != nil && m[1] == strconv.Itoa(b) {
				if n, err := strconv.Atoi(m[2]); err == nil {
					r.BTurns = n
				}
				break
			}
		}
	}

	if r.BisectIdx >= 0 {
		for i := r.BisectIdx + 1; i < len(lines); i++ {
			l := lines[i]
			if m := prefixReuseRe.FindStringSubmatch(l); m != nil && m[1] == "3" && m[2] == "3" && r.Prefix3of3Idx < 0 {
				r.Prefix3of3Idx = i
			}
			if strings.Contains(l, logMainMoved) && r.MainMovedIdx < 0 {
				r.MainMovedIdx = i
			}
			if r.WindowEndIdx < 0 && (i == r.Prefix3of3Idx || i == r.MainMovedIdx) {
				r.WindowEndIdx = i
			}
		}
		if r.WindowEndIdx >= 0 {
			for i := r.BisectIdx + 1; i < r.WindowEndIdx; i++ {
				l := lines[i]
				if m := prefixReuseRe.FindStringSubmatch(l); m != nil && m[1] == "2" && m[2] == "2" && r.PrefixFullFirstHalfIdx < 0 {
					r.PrefixFullFirstHalfIdx = i
				}
				for _, n := range []int{a, b, c, p} {
					if strings.Contains(l, fmt.Sprintf(logClaudeInvokingFmt, n)) {
						r.ClaudeAfterBisect = append(r.ClaudeAfterBisect, strings.TrimSpace(l))
					}
				}
			}
		}
		r.BReplayAfterMove = r.MainMovedIdx >= 0 && r.BReplayIdx > r.MainMovedIdx
	}
	return r
}

// problems evaluates the log-derivable half of assertions A1–A5 and returns one
// message per violation (empty = all hold). maxTurns is the A2 bound.
//
// A5 is deliberately split (see the scenario's doc comment): unconditionally, B's
// Claude count is exactly 1 for the whole run and forgetPoisonerResolutions logs no
// "did not fully replay via rerere" warning; the rerere replay line for B is required
// ONLY when a main-moved rebuild happened, because prefix reuse otherwise covers every
// re-form so B is never re-merged by a logged assembly.
func (r conflictTrainReport) problems(maxTurns int) []string {
	var out []string
	add := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }

	// Bisection ran at all — everything below is anchored on it.
	if r.BisectIdx < 0 {
		add("no %q line — the combined trial never went red/bisected", logBisecting)
		return out
	}
	if r.PDeferredIdx >= 0 {
		add("admission gate (#1821) deferred P #%d before bisection — the scenario's shape was not exercised", r.P)
	}

	// A1: Claude resolved B's conflict and B was not ejected.
	if r.BCannotResolveIdx >= 0 {
		add("A1: conflict for B #%d was NOT resolved (%q logged) — pre-#1841 turn-limited exits ejected the member", r.B, fmt.Sprintf(logCannotResolveFmt, r.B))
	}
	if r.BResolvedIdx < 0 {
		add("A1: no %q line — the A/B conflict was never resolved by the initial trial assembly", fmt.Sprintf(logConflictResolvedFmt, r.B))
	} else if r.BResolvedIdx > r.BisectIdx {
		add("A1: B #%d's conflict was resolved only AFTER bisection started — the initial assembly did not resolve it", r.B)
	}

	// A2: exactly one invocation before bisect, small turn count.
	switch {
	case len(r.BInvokeIdxs) == 0:
		add("A2: no %q line for B #%d — Claude was never invoked to resolve the conflict", fmt.Sprintf(logClaudeInvokingFmt, r.B), r.B)
	default:
		if r.BInvokeIdxs[0] > r.BisectIdx {
			add("A2: B #%d's first Claude invocation came after the bisect line — it should resolve during the initial assembly", r.B)
		}
		switch {
		case r.BTurns < 0:
			add("A2: found B #%d's Claude invocation but no following \"used|completed in N turns\" line", r.B)
		case r.BTurns > maxTurns:
			add("A2: B #%d's conflict resolution used %d turns, over the bound of %d (holding-stage cap is 50; the pre-#1841 prompt burned it on build/test commands)", r.B, r.BTurns, maxTurns)
		}
	}

	// A3: P isolated.
	if r.PIsolatedIdx < 0 {
		add("A3: no %q line — P #%d was not isolated as the poisoner", fmt.Sprintf(logBisectionIsolatedFmt, r.P), r.P)
	}

	// A4: first-half prefix reuse, with no Claude invocation in the window.
	if r.WindowEndIdx < 0 {
		add("A4/A5 vacuity guard: after the bisect line there is neither a \"(3/3 member(s) already merged)\" re-form line nor a %q line — cannot show the survivors re-formed", logMainMoved)
	} else {
		if r.PrefixFullFirstHalfIdx < 0 {
			add("A4: no \"reusing a recorded prefix (2/2 member(s) already merged)\" line between the bisect line and the re-form — the first bisect half re-merged instead of reusing the trial's chain")
		}
		if len(r.ClaudeAfterBisect) > 0 {
			add("A4: %d Claude invocation(s) between the bisect line and the re-form (want 0): %s", len(r.ClaudeAfterBisect), strings.Join(r.ClaudeAfterBisect, " | "))
		}
	}

	// A5 (guaranteed set).
	if len(r.BInvokeIdxs) != 1 {
		add("A5: B #%d had %d Claude invocation(s) over the whole run, want exactly 1 (a re-resolution means rerere/prefix reuse did not hold)", r.B, len(r.BInvokeIdxs))
	}
	if r.PReplayWarnIdx >= 0 {
		add("A5: forgetPoisonerResolutions warned %q for P #%d — rerere could not replay the earlier resolution", logRerereWarnTag, r.P)
	}
	// A5 (conditional): a main-moved rebuild changes baseSHA, missing the prefix cache,
	// so B is re-merged and MUST replay via rerere rather than invoking Claude.
	if r.MainMovedIdx >= 0 && !r.BReplayAfterMove {
		add("A5: main moved during the run (%q) but no %q line follows — B was not replayed via rerere", logMainMoved, fmt.Sprintf(logRerereReplayFmt, r.B))
	}
	return out
}
