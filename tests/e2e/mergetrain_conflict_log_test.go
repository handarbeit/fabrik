//go:build e2e

package e2e

import (
	"fmt"
	"strings"
	"testing"
)

// Unit tests for the pure log analysis behind TestMergeTrainConflictBisectPrefixRerere.
// They run under -tags e2e (like harness_test.go) and need no bed.

const (
	tA = 101
	tB = 102
	tC = 103
	tP = 104
)

// ts prefixes each body with the engine's file-log timestamp; per-issue bodies carry
// their own "[#N tag]" already.
func tsLines(bodies ...string) []string {
	out := make([]string, len(bodies))
	for i, b := range bodies {
		out[i] = "2026-09-25T21:00:00Z " + b
	}
	return out
}

// happyLog is the shape Research derived for the scenario: B's conflict resolved once
// before bisection, first half reuses the recorded prefix, [A,B,C] re-forms on prefix.
func happyLog(turnLine string) []string {
	return tsLines(
		fmt.Sprintf(`[merge-train] batch snapshot for o/r: 4 item(s) — #%d "m A", #%d "m B", #%d "m C", #%d "m P"`, tA, tB, tC, tP),
		fmt.Sprintf(`[#%d merge-train] merged #%d cleanly into trial branch`, tA, tA),
		fmt.Sprintf(`[#%d merge-train] merge conflict for #%d: CONFLICT (add/add) — resolving`, tB, tB),
		fmt.Sprintf(`[#%d claude] invoking (Queued-comment-review) in /wt`, tB),
		turnLine,
		fmt.Sprintf(`[#%d merge-train] conflict for #%d resolved`, tB, tB),
		`[merge-train] combined Validate RED for o/r (4 member(s)) — bisecting to isolate the poisoner`,
		`[merge-train] reusing a recorded prefix (2/2 member(s) already merged) for trial t1`,
		`[merge-train] reusing a recorded prefix (1/1 member(s) already merged) for trial t3`,
		fmt.Sprintf(`[#%d merge-train] bisection isolated #%d as the batch poisoner — ejecting`, tP, tP),
		`[merge-train] reusing a recorded prefix (3/3 member(s) already merged) for trial t5`,
	)
}

func turnLine(form string, n int) string {
	return fmt.Sprintf("2026-09-25T21:00:00Z [#%d claude] %s %d turns, $0.0400", tB, form, n)
}

func TestParseBatchSnapshotNumbers(t *testing.T) {
	cases := []struct {
		name string
		line string
		want []int
	}{
		{"four", tsLines(fmt.Sprintf(`[merge-train] batch snapshot for o/r: 4 item(s) — #1 "a", #22 "b", #333 "c", #4 "d"`))[0], []int{1, 22, 333, 4}},
		{"partial", tsLines(`[merge-train] batch snapshot for o/r: 1 item(s) — #7 "only"`)[0], []int{7}},
		{"title with hash", tsLines(`[merge-train] batch snapshot for o/r: 2 item(s) — #5 "fix #9 thing", #6 "b"`)[0], []int{5, 6}},
		{"not a snapshot", tsLines(`[merge-train] landing complete`)[0], nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseBatchSnapshotNumbers(tc.line); !intsEqual(got, tc.want) {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
	if _, ok := firstBatchSnapshotNumbers(tsLines(`[x] nothing`)); ok {
		t.Fatal("firstBatchSnapshotNumbers found a snapshot in a log without one")
	}
	if nums, ok := firstBatchSnapshotNumbers(happyLog(turnLine("completed in", 6))); !ok || !intsEqual(nums, []int{tA, tB, tC, tP}) {
		t.Fatalf("first snapshot = %v ok=%v", nums, ok)
	}
}

func TestAnalyzeConflictTrainLog_Happy(t *testing.T) {
	for _, form := range []string{"completed in", "used"} {
		t.Run(form, func(t *testing.T) {
			r := analyzeConflictTrainLog(happyLog(turnLine(form, 6)), tA, tB, tC, tP)
			if got := r.problems(maxConflictResolutionTurns); len(got) != 0 {
				t.Fatalf("unexpected problems: %v", got)
			}
			if r.BTurns != 6 {
				t.Fatalf("BTurns = %d, want 6", r.BTurns)
			}
		})
	}
}

func TestAnalyzeConflictTrainLog_Problems(t *testing.T) {
	happy := func() []string { return happyLog(turnLine("completed in", 6)) }
	insertBefore := func(lines []string, marker string, extra ...string) []string {
		var out []string
		for _, l := range lines {
			if strings.Contains(l, marker) {
				out = append(out, tsLines(extra...)...)
			}
			out = append(out, l)
		}
		return out
	}
	cases := []struct {
		name    string
		lines   []string
		wantSub string // substring expected in some problem
	}{
		{"turns over bound", happyLog(turnLine("used", 51)), "used 51 turns"},
		{"turns one over bound", happyLog(turnLine("completed in", maxConflictResolutionTurns+1)), fmt.Sprintf("used %d turns", maxConflictResolutionTurns+1)},
		{"second claude invocation after bisect",
			insertBefore(happy(), "(2/2 member(s)", fmt.Sprintf(`[#%d claude] invoking (Queued-comment-review) in /wt`, tB)),
			"Claude invocation(s) between the bisect line"},
		{"claude for another member after bisect",
			insertBefore(happy(), "(1/1 member(s)", fmt.Sprintf(`[#%d claude] invoking (Queued-comment-review) in /wt`, tA)),
			"Claude invocation(s) between the bisect line"},
		{"no 2/2 prefix reuse",
			func() []string {
				var out []string
				for _, l := range happy() {
					if !strings.Contains(l, "(2/2 member(s)") {
						out = append(out, l)
					}
				}
				return out
			}(),
			"2/2 member(s) already merged"},
		{"missing window end",
			func() []string {
				l := happy()
				return l[:len(l)-1]
			}(),
			"vacuity guard"},
		{"B ejected", insertBefore(happy(), "combined Validate RED", fmt.Sprintf(`[#%d merge-train] cannot resolve conflict for #%d — ejecting`, tB, tB)), "was NOT resolved"},
		{"no bisect", tsLines(fmt.Sprintf(`[#%d merge-train] conflict for #%d resolved`, tB, tB)), "never went red/bisected"},
		{"P deferred by admission gate", insertBefore(happy(), "combined Validate RED", fmt.Sprintf(`[#%d merge-train] deferring #%d (own PR CI confirmed red at abc): x`, tP, tP)), "admission gate"},
		{"rerere replay warn for P", insertBefore(happy(), "(3/3 member(s)", fmt.Sprintf(`[#%d merge-train] warn: could not faithfully reconstruct the trial-so-far state ahead of poisoner #%d (earlier member #%d did not fully replay via rerere) — skipping forget: x`, tP, tP, tB)), "did not fully replay via rerere"},
		{"P never isolated", func() []string {
			var out []string
			for _, l := range happy() {
				if !strings.Contains(l, "bisection isolated") {
					out = append(out, l)
				}
			}
			return out
		}(), "not isolated as the poisoner"},
		{"no claude at all", func() []string {
			var out []string
			for _, l := range happy() {
				if !strings.Contains(l, "claude]") {
					out = append(out, l)
				}
			}
			return out
		}(), "Claude was never invoked"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := analyzeConflictTrainLog(tc.lines, tA, tB, tC, tP)
			got := r.problems(maxConflictResolutionTurns)
			for _, p := range got {
				if strings.Contains(p, tc.wantSub) {
					return
				}
			}
			t.Fatalf("no problem containing %q; got %v", tc.wantSub, got)
		})
	}
}

func TestAnalyzeConflictTrainLog_MainMoved(t *testing.T) {
	base := func() []string {
		l := happyLog(turnLine("completed in", 6))
		// Drop the 3/3 re-form line; a main-moved rebuild ends the window instead.
		return l[:len(l)-1]
	}
	moved := tsLines(`[merge-train] trial t5 is behind main (main moved) — rebasing off the new base (cycle 1/3)`)
	replay := tsLines(fmt.Sprintf(`[#%d merge-train] conflict for #%d fully resolved by git rerere replay — no Claude invocation`, tB, tB))

	t.Run("moved with replay after it", func(t *testing.T) {
		lines := append(append(base(), moved...), replay...)
		r := analyzeConflictTrainLog(lines, tA, tB, tC, tP)
		if got := r.problems(maxConflictResolutionTurns); len(got) != 0 {
			t.Fatalf("unexpected problems: %v", got)
		}
		if !r.BReplayAfterMove {
			t.Fatal("BReplayAfterMove = false")
		}
	})
	t.Run("moved without replay", func(t *testing.T) {
		r := analyzeConflictTrainLog(append(base(), moved...), tA, tB, tC, tP)
		got := r.problems(maxConflictResolutionTurns)
		if len(got) != 1 || !strings.Contains(got[0], "not replayed via rerere") {
			t.Fatalf("got %v, want the single conditional-replay problem", got)
		}
	})
	t.Run("moved, replay logged only before it, does not count", func(t *testing.T) {
		lines := append(append(base(), replay...), moved...)
		got := analyzeConflictTrainLog(lines, tA, tB, tC, tP).problems(maxConflictResolutionTurns)
		if len(got) == 0 {
			t.Fatal("a replay line preceding the main-moved line must not satisfy the conditional")
		}
	})
	t.Run("moved with a fresh Claude invocation for B", func(t *testing.T) {
		lines := append(append(base(), moved...), tsLines(fmt.Sprintf(`[#%d claude] invoking (Queued-comment-review) in /wt`, tB))...)
		lines = append(lines, replay...)
		got := analyzeConflictTrainLog(lines, tA, tB, tC, tP).problems(maxConflictResolutionTurns)
		found := false
		for _, p := range got {
			if strings.Contains(p, "want exactly 1") {
				found = true
			}
		}
		if !found {
			t.Fatalf("second invocation of B must fail the exactly-1 check; got %v", got)
		}
	})
	t.Run("no move and no 3/3 line is a vacuity failure", func(t *testing.T) {
		got := analyzeConflictTrainLog(base(), tA, tB, tC, tP).problems(maxConflictResolutionTurns)
		if len(got) == 0 || !strings.Contains(strings.Join(got, "\n"), "vacuity guard") {
			t.Fatalf("got %v", got)
		}
	})
}

func TestFirstSnapshotForRepo(t *testing.T) {
	lines := tsLines(
		`[merge-train] batch snapshot for o/other: 1 item(s) — #9 "x"`,
		`[merge-train] batch snapshot for o/r: 2 item(s) — #1 "a", #2 "b"`,
		`[merge-train] batch snapshot for o/r: 4 item(s) — #1 "a", #2 "b", #3 "c", #4 "d"`,
	)
	if got, ok := firstSnapshotForRepo(lines, "o/r"); !ok || !intsEqual(got, []int{1, 2}) {
		t.Fatalf("got %v ok=%v, want the FIRST o/r snapshot [1 2]", got, ok)
	}
	if _, ok := firstSnapshotForRepo(lines, "o/missing"); ok {
		t.Fatal("found a snapshot for a repo with none")
	}
}
