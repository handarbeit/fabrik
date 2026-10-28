package attention

import "testing"

func pausedWith(labels ...string) Input {
	in := baseInput()
	in.Labels = append([]string{"fabrik:paused", "fabrik:awaiting-input"}, labels...)
	return in
}

func TestPauseCauseOf(t *testing.T) {
	if got := PauseCauseOf([]string{"fabrik:paused", "fabrik:pause:ci-timeout"}); got != "ci-timeout" {
		t.Errorf("got %q", got)
	}
	if got := PauseCauseOf([]string{"fabrik:paused"}); got != "" {
		t.Errorf("got %q, want none", got)
	}
}

// The pair alone is a question (the pre-#2487 reading); a recorded non-question
// cause is reported as the plain pause it is, never as "waiting for a reply".
func TestClassify_PauseCauseIsSurfacedAndNonQuestionsAreNotQuestions(t *testing.T) {
	q := Classify(pausedWith("fabrik:pause:question"))
	if q.State != NeedsHuman || q.Code != "fabrik:awaiting-input" || q.PauseCause != "question" {
		t.Errorf("question: state=%s code=%s cause=%q", q.State, q.Code, q.PauseCause)
	}
	legacy := Classify(pausedWith())
	if legacy.Code != "fabrik:awaiting-input" || legacy.PauseCause != "" {
		t.Errorf("no cause label (pre-upgrade pause): code=%s cause=%q", legacy.Code, legacy.PauseCause)
	}
	for _, c := range []string{"review-timeout", "ci-timeout", "cycle-limit", "breaker", "failed", "other"} {
		r := Classify(pausedWith("fabrik:pause:" + c))
		if r.State != NeedsHuman {
			t.Errorf("%s: state %s — the cause must not change who must act", c, r.State)
		}
		if r.Code != "fabrik:paused" || r.PauseCause != c {
			t.Errorf("%s: code=%s cause=%q", c, r.Code, r.PauseCause)
		}
		if r.ResumeVia != legacy.ResumeVia {
			t.Errorf("%s: resume via %q, want %q — resume must not depend on the cause", c, r.ResumeVia, legacy.ResumeVia)
		}
	}
}

// FR-009: the label and the marker-derived Cause agree for a daemon stop.
func TestClassify_DaemonStopLabelAgreesWithTheMarker(t *testing.T) {
	byLabel := Classify(pausedWith("fabrik:pause:daemon-stop"))
	marker := pausedWith()
	marker.DaemonStop = true
	byMarker := Classify(marker)
	if byLabel.Cause != CauseDaemonStop || byMarker.Cause != CauseDaemonStop {
		t.Errorf("Cause by label %q, by marker %q, want %q", byLabel.Cause, byMarker.Cause, CauseDaemonStop)
	}
}

func TestClassify_NoPauseCauseOnAnUnpausedItem(t *testing.T) {
	in := baseInput()
	in.Labels = []string{"fabrik:pause:question"} // stale leftover
	if r := Classify(in); r.PauseCause != "" {
		t.Errorf("PauseCause = %q on an item that is not paused", r.PauseCause)
	}
}
