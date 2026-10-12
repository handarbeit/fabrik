package attention

import (
	"strings"
	"testing"
	"time"
)

func redCI(failing ...string) *CIFact {
	return &CIFact{Verdict: "red", Failing: failing, PROpen: true}
}

func TestOverlayCI_SurfacesRedWithTheFailingCheckNamed(t *testing.T) {
	in := baseInput()
	in.CI = redCI("Test and vet", "lint")
	plain := Classify(baseInput())
	r := Classify(in)
	if r.State != plain.State || r.Code != plain.Code {
		t.Errorf("overlay changed the classification: %s/%s -> %s/%s", plain.State, plain.Code, r.State, r.Code)
	}
	if !strings.HasPrefix(r.Summary, "CI is red on Test and vet, lint; ") {
		t.Errorf("summary = %q", r.Summary)
	}
	if r.Rank != RankCIRed {
		t.Errorf("rank = %d, want %d", r.Rank, RankCIRed)
	}
	if len(r.CIFailing) != 2 || r.CIFailing[0] != "Test and vet" {
		t.Errorf("CIFailing = %v", r.CIFailing)
	}
	var found bool
	for _, rs := range r.Reasons {
		found = found || rs.Code == CodeCIRed
	}
	if !found {
		t.Errorf("reasons = %+v", r.Reasons)
	}
	if len(Classify(baseInput()).Reasons) != len(r.Reasons)-1 {
		t.Error("overlay must add exactly one reason")
	}
}

func TestOverlayCI_RedWithoutANamedCheck(t *testing.T) {
	in := baseInput()
	in.CI = redCI()
	if r := Classify(in); !strings.HasPrefix(r.Summary, "CI is red (a required status context failed); ") {
		t.Errorf("summary = %q", r.Summary)
	}
}

func TestOverlayCI_NothingAddedForOtherVerdictsOrConditions(t *testing.T) {
	cases := map[string]func(*Input){
		"green":       func(in *Input) { in.CI = &CIFact{Verdict: "green", PROpen: true} },
		"pending":     func(in *Input) { in.CI = &CIFact{Verdict: "pending", PROpen: true} },
		"none":        func(in *Input) { in.CI = &CIFact{Verdict: "none", PROpen: true} },
		"unknown":     func(in *Input) { in.CI = &CIFact{Verdict: "unknown", PROpen: true} },
		"stale red":   func(in *Input) { in.CI = &CIFact{Verdict: "red", Failing: []string{"x"}, Stale: true, PROpen: true} },
		"pr not open": func(in *Input) { in.CI = &CIFact{Verdict: "red", Failing: []string{"x"}} },
		"awaiting-ci": func(in *Input) { in.CI = redCI("x"); in.Labels = []string{"fabrik:awaiting-ci"} },
		"closed":      func(in *Input) { in.CI = redCI("x"); in.Closed = true },
		"unmanaged":   func(in *Input) { in.CI = redCI("x"); in.StageKind = KindUnmanaged },
		"holding":     func(in *Input) { in.CI = redCI("x"); in.StageKind = KindHolding },
		"cleanup":     func(in *Input) { in.CI = redCI("x"); in.StageKind = KindCleanup },
	}
	for name, mod := range cases {
		t.Run(name, func(t *testing.T) {
			in := baseInput()
			mod(&in)
			ref := in
			ref.CI = nil
			want := Classify(ref)
			got := Classify(in)
			if got.Summary != want.Summary || got.Rank != want.Rank || len(got.Reasons) != len(want.Reasons) || got.CIFailing != nil {
				t.Errorf("overlay applied: got %+v want %+v", got, want)
			}
		})
	}
}

func TestOverlayCI_KeepsDecisionBearingStates(t *testing.T) {
	for name, mod := range map[string]func(*Input){
		"paused": func(in *Input) { in.Labels = []string{"fabrik:paused", "fabrik:awaiting-input"} },
		"escalated": func(in *Input) {
			in.Labels = []string{"fabrik:paused"}
			in.Counters = []Counter{{Name: "review-cycles", N: 5, Max: 5}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			in := baseInput()
			mod(&in)
			ref := in
			in.CI = redCI("x")
			got, want := Classify(in), Classify(ref)
			if got.State != want.State || got.Code != want.Code || got.Rank != want.Rank || got.Summary != want.Summary {
				t.Errorf("red CI reclassified a %s item: %+v vs %+v", name, got, want)
			}
		})
	}
}

func TestOverlayCI_WorkerWordingAndRank(t *testing.T) {
	for _, tc := range []struct {
		kind, want string
	}{
		{WorkerKindCIFixReinvoke, "a CI-fix reinvoke is addressing it"},
		{WorkerKindStageRun, "a worker is running"},
		{WorkerKindReviewReinvoke, "a review reinvoke is running"},
	} {
		in := baseInput()
		in.HasWorker, in.WorkerKind, in.WorkerStartedAt = true, tc.kind, t0.Add(-time.Minute)
		in.CI = redCI("Test and vet")
		r := Classify(in)
		if !strings.Contains(r.Summary, "CI is red on Test and vet; "+tc.want) {
			t.Errorf("%s: summary = %q", tc.kind, r.Summary)
		}
		if r.Rank == RankCIRed {
			t.Errorf("%s: an addressed red CI must not be ranked", tc.kind)
		}
	}
}

func TestOverlayCI_DraftIsReportedNotRanked(t *testing.T) {
	in := baseInput()
	in.CI = &CIFact{Verdict: "red", Failing: []string{"Test and vet"}, Draft: true, PROpen: true}
	r := Classify(in)
	if !strings.Contains(r.Summary, "CI is red on Test and vet (draft PR); ") || r.Rank == RankCIRed {
		t.Errorf("rank=%d summary=%q", r.Rank, r.Summary)
	}
}

func TestOverlayCI_DoesNotOutrankAMoreUrgentState(t *testing.T) {
	in := baseInput()
	in.CI = redCI("x")
	in.StatusEnteredAt = t0.Add(-3 * time.Hour) // long past the stall threshold
	r := Classify(in)
	if r.State != Stalled {
		t.Fatalf("precondition: the item must classify as stalled, got %s/%s", r.State, r.Code)
	}
	if r.Rank != RankStalled {
		t.Errorf("stalled item rank = %d, want %d (the red CI overlay must not outrank it)", r.Rank, RankStalled)
	}
}
