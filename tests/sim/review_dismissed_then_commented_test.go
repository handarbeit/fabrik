package sim

import "testing"

// Sim twin of #2588 (ADR 2582): a declared bot's CHANGES_REQUESTED on head A is
// dismissed, then the bot leaves a clean COMMENTED review on head B, the current
// head. The pre-fix collapse (FetchPRReviews) kept the DISMISSED entry for good
// and dropped the COMMENTED, so the gate's live confirmation read saw a bot that
// "did not respond", ran the re-prompt ladder and paused. As the non-vacuity
// control every case also runs with simgh's pre-fix collapse restored
// (SetStickyDismissedForTest); the cases whose outcome the fix changes must flip.

func TestReviewDismissedThenCommented_DeclaredBotClearsTheGate(t *testing.T) {
	t.Parallel()
	for _, authority := range []struct {
		name   string
		labels []string
	}{
		{"advisory", nil},
		{"authoritative", []string{"review-authority:authoritative"}},
	} {
		for _, tc := range []struct {
			name   string
			sticky bool
			want   bool
		}{{"as shipped", false, true}, {"neutralised (pre-#2582 collapse)", true, false}} {
			t.Run(authority.name+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				env := newHeadAwareEnv(t, "pruefer-bot")
				env.Sim.Sim().SetStickyDismissedForTest(tc.sticky)
				num, prNum := seedReviewGateItem(t, env, "Review", "dismissed-then-commented-"+authority.name, authority.labels...)
				a := headOf(t, env, prNum)
				seedBareReview(t, env, prNum, "pruefer-bot[bot]", "CHANGES_REQUESTED", a)
				seedBareReview(t, env, prNum, "pruefer-bot[bot]", "DISMISSED", a)
				b := pushFix(t, env, num, prNum)
				seedBareReview(t, env, prNum, "pruefer-bot[bot]", "COMMENTED", b)
				got := gateCleared(t, env, num)
				if got != tc.want {
					t.Fatalf("gate cleared = %v, want %v", got, tc.want)
				}
				labels := IssueLabels(t, env, num)
				if tc.want {
					for _, l := range []string{"fabrik:bot-reprompted", "fabrik:paused", "fabrik:pause:review-timeout"} {
						if hasLabel(labels, l) {
							t.Fatalf("%s present: a bot that reviewed the head must not be re-prompted or paused", l)
						}
					}
				}
			})
		}
	}
}

// A dismissal with no later review is still "did not respond": the fix only
// stops a dismissal hiding a later COMMENTED, it does not count a dismissal.
func TestReviewDismissedThenCommented_LoneDismissalStillBlocks(t *testing.T) {
	t.Parallel()
	env := newHeadAwareEnv(t, "pruefer-bot")
	num, prNum := seedReviewGateItem(t, env, "Review", "dismissed-only")
	a := headOf(t, env, prNum)
	seedBareReview(t, env, prNum, "pruefer-bot[bot]", "CHANGES_REQUESTED", a)
	seedBareReview(t, env, prNum, "pruefer-bot[bot]", "DISMISSED", a)
	pushFix(t, env, num, prNum)
	if gateCleared(t, env, num) {
		t.Fatal("a dismissed review with no later review must not count as the bot's response")
	}
}
