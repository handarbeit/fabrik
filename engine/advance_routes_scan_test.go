package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEveryAdvanceRouteIsReviewGatedOrAudited (#2491, ADR 2491, SC-002): every
// engine call of advanceToNextStage either consults the review verdict earlier in
// the same function, or is on the audited list below with the reason it does not
// need to. A new route out of a stage cannot silently skip the gate: it fails here
// until it consults reviewGateHold (via reviewGateHoldsAdvance, reviewGateHold or
// reviewGateBlocksLanding) or is audited with a reason.
func TestEveryAdvanceRouteIsReviewGatedOrAudited(t *testing.T) {
	gateTokens := []string{"reviewGateHoldsAdvance(", "reviewGateHold(", "reviewGateBlocksLanding("}

	// file → expected number of calls and why the review verdict is not needed (or
	// "gated" when the call must be preceded by a gate token in its function).
	audited := map[string]struct {
		calls  int
		reason string // empty = must be gated
	}{
		"poll.go":             {1, ""}, // runCatchUpPhase2's non-Validate advance
		"overseer_advance.go": {1, ""}, // fabrik_advance
		"stages.go":           {1, "handleStageComplete's shouldAdvance branch returns before this call for a wait_for_reviews stage (it applies fabrik:awaiting-review and leaves the decision to the catch-up Phase 2 gate)"},
		"advance_settle.go":   {1, "retry of a terminal advance whose other side effects already ran (the PR merged, or the item is closed/no-work): not a review-gated exit"},
		"merge_train.go":      {2, "merge-train landing of an already-merged batch: the review verdict was consulted at the landing decision (attemptMergeOnValidate / the train's landing gates) before the merge"},
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, ent := range entries {
		name := ent.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatal(err)
		}
		src := string(data)
		for off := 0; ; {
			i := strings.Index(src[off:], "advanceToNextStage(")
			if i < 0 {
				break
			}
			pos := off + i
			off = pos + len("advanceToNextStage(")
			lineStart := strings.LastIndex(src[:pos], "\n") + 1
			line := strings.TrimSpace(src[lineStart : strings.Index(src[pos:], "\n")+pos])
			if strings.HasPrefix(line, "//") || strings.HasPrefix(line, "func ") {
				continue
			}
			seen[name]++
			a, ok := audited[name]
			if !ok {
				t.Errorf("%s calls advanceToNextStage without being audited: consult the review verdict first or add it to the audited list with a reason", name)
				continue
			}
			if a.reason != "" {
				continue
			}
			fnStart := strings.LastIndex(src[:pos], "\nfunc ")
			if fnStart < 0 {
				fnStart = 0
			}
			prefix := src[fnStart:pos]
			gated := false
			for _, tok := range gateTokens {
				if strings.Contains(prefix, tok) {
					gated = true
				}
			}
			if !gated {
				t.Errorf("%s: advanceToNextStage is not preceded by a review-gate consult in its function", name)
			}
		}
	}
	for name, a := range audited {
		if seen[name] != a.calls {
			t.Errorf("%s: %d advanceToNextStage call(s), audited list expects %d — re-audit the route", name, seen[name], a.calls)
		}
	}
}
