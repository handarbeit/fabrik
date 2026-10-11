package engine

import (
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// originalAutoAdvanceGate is runCatchUpPhase2's autonomy gate as it was written
// before it was extracted into stageAutoAdvances (#2120): true = proceed.
func originalAutoAdvanceGate(cfgYolo bool, item gh.ProjectItem, stage *stages.Stage) bool {
	isAutoAdvance := hasYoloLabel(item) || hasCruiseLabel(item)
	if !cfgYolo && !isAutoAdvance && !(stage.AutoAdvance != nil && *stage.AutoAdvance) {
		return false
	}
	if !isAutoAdvance && stage.AutoAdvance != nil && !*stage.AutoAdvance {
		return false
	}
	return true
}

// TestStageAutoAdvances_ParityWithOriginalGate pins the extraction: every
// combination of global yolo, autonomy label and stage auto_advance agrees.
func TestStageAutoAdvances_ParityWithOriginalGate(t *testing.T) {
	tr, fa := true, false
	for _, cfgYolo := range []bool{false, true} {
		for _, labels := range [][]string{nil, {"fabrik:yolo"}, {"fabrik:cruise"}, {"fabrik:yolo", "fabrik:cruise"}, {"model:opus"}} {
			for _, aa := range []*bool{nil, &tr, &fa} {
				stage := &stages.Stage{Name: "Specify", AutoAdvance: aa}
				item := gh.ProjectItem{Labels: labels}
				want := originalAutoAdvanceGate(cfgYolo, item, stage)
				if got := stageAutoAdvances(cfgYolo, labels, stage); got != want {
					t.Errorf("yolo=%v labels=%v auto_advance=%v: got %v, want %v", cfgYolo, labels, aa, got, want)
				}
			}
		}
	}
	// Spot checks that the table is not vacuous.
	if stageAutoAdvances(false, nil, &stages.Stage{AutoAdvance: &fa}) {
		t.Error("auto_advance:false with no label must not advance")
	}
	if !stageAutoAdvances(false, []string{"fabrik:cruise"}, &stages.Stage{AutoAdvance: &fa}) {
		t.Error("a cruise label overrides auto_advance:false")
	}
	if stageAutoAdvances(true, nil, &stages.Stage{AutoAdvance: &fa}) {
		t.Error("global yolo respects a per-stage opt-out")
	}
}
