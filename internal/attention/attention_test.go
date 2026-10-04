package attention

import (
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

func baseInput() Input {
	return Input{
		Now:       t0,
		Status:    "Implement",
		StageKind: KindManaged,
		Cfg: Config{
			StallThreshold:    30 * time.Minute,
			CIWaitTimeout:     30 * time.Minute,
			CIBackstopTimeout: 4 * time.Hour,
			ReviewWaitTimeout: time.Hour,
		},
		StatusEnteredAt: t0.Add(-5 * time.Minute),
	}
}

func TestClassifyStates(t *testing.T) {
	cases := []struct {
		name  string
		mod   func(*Input)
		state State
		code  string
		rank  int
	}{
		{"closed is idle", func(in *Input) { in.Closed = true; in.Labels = []string{"fabrik:paused"} }, Idle, "closed", NotRanked},
		{"terminal is idle", func(in *Input) { in.Terminal = true }, Idle, "done", NotRanked},
		{"cleanup stage is idle", func(in *Input) { in.StageKind = KindCleanup }, Idle, "done", NotRanked},
		{"unmanaged column is idle", func(in *Input) { in.StageKind = KindUnmanaged; in.Status = "Backlog" }, Idle, "unmanaged-column", NotRanked},
		{"column with no stage is idle", func(in *Input) { in.StageKind = KindNone }, Idle, "no-stage", NotRanked},

		{"paused + awaiting-input is needs-human", func(in *Input) {
			in.Labels = []string{"fabrik:paused", "fabrik:awaiting-input"}
		}, NeedsHuman, "fabrik:awaiting-input", RankNeedsHuman},
		{"bare paused is needs-human", func(in *Input) { in.Labels = []string{"fabrik:paused"} }, NeedsHuman, "fabrik:paused", RankNeedsHuman},
		{"paused by engine is escalated", func(in *Input) {
			in.Labels = []string{"fabrik:paused", "fabrik:awaiting-input"}
			in.PausedByEngine = true
		}, Escalated, "paused-by-engine", RankEscalated},
		{"paused at a cycle limit is escalated", func(in *Input) {
			in.Labels = []string{"fabrik:paused", "fabrik:awaiting-input"}
			in.Counters = []Counter{{Name: "review-cycles", N: 3, Max: 3}}
		}, Escalated, "limit:review-cycles", RankEscalated},
		{"paused with counters below limit stays needs-human", func(in *Input) {
			in.Labels = []string{"fabrik:paused", "fabrik:awaiting-input"}
			in.Counters = []Counter{{Name: "review-cycles", N: 2, Max: 3}}
		}, NeedsHuman, "fabrik:awaiting-input", RankNeedsHuman},
		{"limit 0 is unknown, never at-limit", func(in *Input) {
			in.Labels = []string{"fabrik:paused"}
			in.Counters = []Counter{{Name: "ci-fix-cycles", N: 5, Max: 0}}
		}, NeedsHuman, "fabrik:paused", RankNeedsHuman},
		{"unlimited is never at-limit", func(in *Input) {
			in.Labels = []string{"fabrik:paused"}
			in.Counters = []Counter{{Name: "attempts", N: 50, Max: MaxUnlimited}}
		}, NeedsHuman, "fabrik:paused", RankNeedsHuman},
		{"runaway alert pause is escalated", func(in *Input) {
			in.Labels = []string{"fabrik:paused", "fabrik:awaiting-runaway-alert"}
		}, Escalated, "fabrik:awaiting-runaway-alert", RankEscalated},
		{"landing verification failed is escalated even unpaused", func(in *Input) {
			in.Labels = []string{"fabrik:landing-verification-failed"}
		}, Escalated, "fabrik:landing-verification-failed", RankEscalated},

		{"settled at Validate under cruise awaits merge decision", func(in *Input) {
			in.Status = "Validate"
			in.Labels = []string{"stage:Validate:complete", "fabrik:cruise"}
			in.HasOpenUnmergedPR = true
		}, NeedsHuman, CodeAwaitingMergeDecision, RankMergeDecision},
		{"validate complete without cruise is not a merge decision", func(in *Input) {
			in.Status = "Validate"
			in.Labels = []string{"stage:Validate:complete"}
			in.HasOpenUnmergedPR = true
		}, Idle, "idle", NotRanked},
		{"validate complete under cruise with merged PR is not a merge decision", func(in *Input) {
			in.Status = "Validate"
			in.Labels = []string{"stage:Validate:complete", "fabrik:cruise"}
		}, Idle, "idle", NotRanked},

		{"awaiting-ci waits", func(in *Input) { in.Labels = []string{"fabrik:awaiting-ci"} }, Waiting, "fabrik:awaiting-ci", NotRanked},
		{"blocked waits", func(in *Input) { in.Labels = []string{"fabrik:blocked"} }, Waiting, "fabrik:blocked", NotRanked},
		{"claude-limit waits", func(in *Input) { in.Labels = []string{"fabrik:claude-limit"} }, Waiting, "fabrik:claude-limit", NotRanked},
		{"tools-denied waits", func(in *Input) { in.Labels = []string{"fabrik:tools-denied"} }, Waiting, "fabrik:tools-denied", NotRanked},
		{"active cooldown waits", func(in *Input) {
			in.Cooldowns = map[string]time.Time{"retry": t0.Add(5 * time.Minute)}
		}, Waiting, "cooldown:retry", NotRanked},
		{"expired cooldown does not wait", func(in *Input) {
			in.Cooldowns = map[string]time.Time{"retry": t0.Add(-time.Minute)}
		}, Idle, "idle", NotRanked},
		{"holding stage waits", func(in *Input) { in.StageKind = KindHolding; in.Status = "Queued" }, Waiting, "queued", NotRanked},
		{"toolchain-stale alone does not change state", func(in *Input) { in.Labels = []string{"fabrik:toolchain-stale"} }, Idle, "idle", NotRanked},

		{"recent worker is working", func(in *Input) {
			in.HasWorker = true
			in.WorkerStartedAt = t0.Add(-2 * time.Minute)
		}, Working, "worker-in-flight", NotRanked},
		{"worker with no progress past the threshold is stalled", func(in *Input) {
			in.HasWorker = true
			in.WorkerStartedAt = t0.Add(-45 * time.Minute)
			in.StatusEnteredAt = t0.Add(-2 * time.Hour)
		}, Stalled, CodeWorkerNoProgress, RankStalled},
		{"idle item past the threshold is stalled", func(in *Input) {
			in.StatusEnteredAt = t0.Add(-2 * time.Hour)
		}, Stalled, CodeNoProgress, RankStalled},
		{"stage-complete item not advanced is stalled with its own code", func(in *Input) {
			in.StatusEnteredAt = t0.Add(-2 * time.Hour)
			in.Labels = []string{"stage:Implement:complete"}
		}, Stalled, CodeStageCompleteStuck, RankStalled},
		{"recently active idle item is idle", func(in *Input) {}, Idle, "idle", NotRanked},
		{"unknown progress anchor never stalls", func(in *Input) { in.StatusEnteredAt = time.Time{} }, Idle, "idle", NotRanked},
		{"zero stall threshold disables stalling", func(in *Input) {
			in.Cfg.StallThreshold = 0
			in.StatusEnteredAt = t0.Add(-24 * time.Hour)
		}, Idle, "idle", NotRanked},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := baseInput()
			tc.mod(&in)
			got := Classify(in)
			if got.State != tc.state || got.Code != tc.code || got.Rank != tc.rank {
				t.Fatalf("got state=%s code=%s rank=%d (%s), want state=%s code=%s rank=%d",
					got.State, got.Code, got.Rank, got.Summary, tc.state, tc.code, tc.rank)
			}
			if got.Summary == "" || got.Next.Action == "" {
				t.Errorf("summary/next action must never be empty: %+v", got)
			}
		})
	}
}

// A recently active waiting item must not rank; a stalled one must.
func TestStalledRanksAndRecentlyActiveWaitingDoesNot(t *testing.T) {
	waiting := baseInput()
	waiting.Labels = []string{"fabrik:awaiting-ci"}
	waiting.LabelAppliedAt = map[string]time.Time{"fabrik:awaiting-ci": t0.Add(-10 * time.Minute)}
	waiting.LastCIProgressAt = t0.Add(-2 * time.Minute)
	if got := Classify(waiting); got.State != Waiting || got.Rank != NotRanked {
		t.Fatalf("recently active waiting item: %+v", got)
	}

	stalled := baseInput()
	stalled.StatusEnteredAt = t0.Add(-90 * time.Minute)
	if got := Classify(stalled); got.State != Stalled || got.Rank != RankStalled {
		t.Fatalf("stalled item: %+v", got)
	}
}

func TestWaitingBecomesOverdueStalled(t *testing.T) {
	in := baseInput()
	in.Labels = []string{"fabrik:awaiting-ci"}
	in.LastCIProgressAt = t0.Add(-2 * time.Hour) // liveness deadline passed 90m ago
	got := Classify(in)
	if got.State != Stalled || got.Code != CodeOverdue || got.Rank != RankStalled {
		t.Fatalf("overdue wait: %+v", got)
	}
	if !strings.Contains(got.Summary, "ci-liveness-timeout") {
		t.Errorf("summary should name the overdue deadline: %q", got.Summary)
	}

	// Just past the deadline (inside the threshold) is still waiting.
	in.LastCIProgressAt = t0.Add(-35 * time.Minute) // deadline passed 5m ago
	if got := Classify(in); got.State != Waiting {
		t.Fatalf("slightly late wait should still be waiting: %+v", got)
	}
}

func TestDeadlinesAndUnknown(t *testing.T) {
	in := baseInput()
	in.Labels = []string{"fabrik:awaiting-ci"}
	in.LastCIProgressAt = t0.Add(-10 * time.Minute)
	in.LabelAppliedAt = map[string]time.Time{"fabrik:awaiting-ci": t0.Add(-20 * time.Minute)}
	got := Classify(in)
	byKind := map[string]Deadline{}
	for _, d := range got.Next.Deadlines {
		byKind[d.Kind] = d
	}
	if d := byKind["ci-liveness-timeout"]; !d.Known() || !d.At.Equal(t0.Add(20*time.Minute)) {
		t.Errorf("liveness deadline = %+v, want t0+20m", d)
	}
	if d := byKind["ci-backstop-timeout"]; !d.Known() || !d.At.Equal(t0.Add(-20*time.Minute).Add(4*time.Hour)) {
		t.Errorf("backstop deadline = %+v", d)
	}
	if !got.Next.At.Equal(t0.Add(20 * time.Minute)) {
		t.Errorf("Next.At = %v, want earliest known deadline (t0+20m)", got.Next.At)
	}

	// After a restart neither anchor is cached: deadlines are listed but unknown,
	// and Next.At is unknown (zero) — never "now" and never a healthy default.
	cold := baseInput()
	cold.Labels = []string{"fabrik:awaiting-ci"}
	got = Classify(cold)
	if len(got.Next.Deadlines) != 2 {
		t.Fatalf("want both CI deadlines listed even when unknown, got %+v", got.Next.Deadlines)
	}
	for _, d := range got.Next.Deadlines {
		if d.Known() {
			t.Errorf("deadline %s should be unknown after a restart, got %v", d.Kind, d.At)
		}
	}
	if !got.Next.At.IsZero() {
		t.Errorf("Next.At must be unknown, got %v", got.Next.At)
	}
	if got.State != Waiting {
		t.Errorf("cold awaiting-ci must be waiting, got %s", got.State)
	}
}

func TestReviewAndBotDeadlines(t *testing.T) {
	in := baseInput()
	in.Labels = []string{"fabrik:awaiting-review", "fabrik:bot-reprompted"}
	in.LabelAppliedAt = map[string]time.Time{
		"fabrik:awaiting-review": t0.Add(-30 * time.Minute),
		"fabrik:bot-reprompted":  t0.Add(-10 * time.Minute),
	}
	got := Classify(in)
	want := map[string]time.Time{
		"review-wait-timeout":     t0.Add(30 * time.Minute),
		"bot-reprompt-escalation": t0.Add(50 * time.Minute),
	}
	for _, d := range got.Next.Deadlines {
		if w, ok := want[d.Kind]; ok {
			if !d.At.Equal(w) {
				t.Errorf("%s = %v, want %v", d.Kind, d.At, w)
			}
			delete(want, d.Kind)
		}
	}
	if len(want) != 0 {
		t.Errorf("missing deadlines: %v", want)
	}
}

func TestCooldownAndClaudeSuspensionDeadlines(t *testing.T) {
	in := baseInput()
	in.Labels = []string{"fabrik:claude-limit"}
	in.ClaudeSuspendedUntil = t0.Add(3 * time.Hour)
	in.Cooldowns = map[string]time.Time{"retry": t0.Add(10 * time.Minute)}
	got := Classify(in)
	if !got.Next.At.Equal(t0.Add(10 * time.Minute)) {
		t.Errorf("Next.At = %v, want the cooldown expiry", got.Next.At)
	}
	var sawSuspension bool
	for _, d := range got.Next.Deadlines {
		if d.Kind == "claude-suspension-end" && d.At.Equal(t0.Add(3*time.Hour)) {
			sawSuspension = true
		}
	}
	if !sawSuspension {
		t.Errorf("claude suspension deadline missing: %+v", got.Next.Deadlines)
	}
}

func TestProgressAnchorExcludesNothingElseAndIsUnknownWhenEmpty(t *testing.T) {
	in := baseInput()
	in.StatusEnteredAt = t0.Add(-3 * time.Hour)
	in.LastAttemptAt = t0.Add(-2 * time.Hour)
	in.LabelAppliedAt = map[string]time.Time{"fabrik:x": t0.Add(-time.Hour)}
	in.LastCIProgressAt = t0.Add(-40 * time.Minute)
	got := Classify(in)
	if !got.ProgressAt.Equal(t0.Add(-40*time.Minute)) || got.ProgressAge != 40*time.Minute {
		t.Errorf("progress = %v age %v, want latest anchor (t0-40m)", got.ProgressAt, got.ProgressAge)
	}

	empty := Input{Now: t0, Status: "Plan", StageKind: KindManaged, Cfg: in.Cfg}
	if got := Classify(empty); !got.ProgressAt.IsZero() || got.State == Stalled {
		t.Errorf("unknown anchor must stay unknown and never stall: %+v", got)
	}
}

func TestReasonsCarryEveryLabelMeaning(t *testing.T) {
	for label, info := range labelTable {
		if info.meaning == "" {
			t.Errorf("label %s has no meaning", label)
		}
	}
	in := baseInput()
	in.Labels = []string{"fabrik:awaiting-ci", "fabrik:toolchain-stale", "stage:Implement:complete", "model:opus"}
	got := Classify(in)
	codes := map[string]bool{}
	for _, r := range got.Reasons {
		codes[r.Code] = true
		if r.Meaning == "" {
			t.Errorf("reason %s has empty meaning", r.Code)
		}
	}
	if !codes["fabrik:awaiting-ci"] || !codes["fabrik:toolchain-stale"] {
		t.Errorf("reasons = %v, want both state-bearing labels", codes)
	}
	if codes["model:opus"] || codes["stage:Implement:complete"] {
		t.Errorf("non-attention labels must not appear as reasons: %v", codes)
	}
}

func TestClassifyIsDeterministic(t *testing.T) {
	in := baseInput()
	in.Labels = []string{"fabrik:blocked", "fabrik:awaiting-ci", "fabrik:tools-denied"}
	first := Classify(in)
	for i := 0; i < 20; i++ {
		if got := Classify(in); got.Code != first.Code || got.Summary != first.Summary {
			t.Fatalf("non-deterministic: %+v vs %+v", got, first)
		}
	}
}

// The store never deletes cooldown entries, so an expired one must be history:
// not a deadline, not an "overdue" trigger, not the next action.
func TestExpiredCooldownIsNotADeadline(t *testing.T) {
	in := baseInput()
	in.Labels = []string{"fabrik:blocked"}
	in.Cooldowns = map[string]time.Time{"retry": t0.Add(-3 * time.Hour)}
	in.StatusEnteredAt = t0.Add(-5 * time.Minute)
	got := Classify(in)
	if got.State != Waiting || got.Code != "fabrik:blocked" {
		t.Fatalf("got %s/%s (%s), want waiting/fabrik:blocked", got.State, got.Code, got.Summary)
	}
	if got.Rank != NotRanked {
		t.Errorf("a correctly waiting item must not rank, got %d", got.Rank)
	}
	for _, d := range got.Next.Deadlines {
		if strings.HasPrefix(d.Kind, "cooldown:") {
			t.Errorf("expired cooldown listed as a deadline: %+v", d)
		}
	}
	if !got.Next.At.IsZero() && !got.Next.At.After(t0) {
		t.Errorf("next action is in the past: %v", got.Next.At)
	}
	if strings.Contains(got.Next.Action, "cooldown") {
		t.Errorf("next action names an expired cooldown: %q", got.Next.Action)
	}
	for _, r := range got.Reasons {
		if strings.HasPrefix(r.Code, "cooldown:") {
			t.Errorf("expired cooldown reported as a reason: %+v", r)
		}
	}
}

// A healthy worker running past the stall threshold but inside its stage's
// wall-clock budget is working; one well past the budget is stalled.
func TestWorkerStalledOnlyPastItsWallClockBudget(t *testing.T) {
	mk := func(running, budget time.Duration) Input {
		in := baseInput()
		in.HasWorker = true
		in.WorkerStartedAt = t0.Add(-running)
		in.StatusEnteredAt = t0.Add(-3 * time.Hour)
		in.WorkerBudget = budget
		return in
	}
	got := Classify(mk(40*time.Minute, 45*time.Minute))
	if got.State != Working || got.Rank != NotRanked {
		t.Fatalf("40m in under a 45m budget: got %s/%s rank=%d, want working", got.State, got.Code, got.Rank)
	}
	if !strings.Contains(got.Summary, "long-running") {
		t.Errorf("a worker past the stall threshold should be flagged long-running: %q", got.Summary)
	}
	// Inside budget + margin is still working.
	if got := Classify(mk(45*time.Minute+WorkerStallMargin-time.Minute, 45*time.Minute)); got.State != Working {
		t.Errorf("inside budget+margin: got %s, want working", got.State)
	}
	got = Classify(mk(2*time.Hour, 45*time.Minute))
	if got.State != Stalled || got.Code != CodeWorkerNoProgress || got.Rank != RankStalled {
		t.Fatalf("well past budget: got %s/%s rank=%d, want stalled/worker-no-progress", got.State, got.Code, got.Rank)
	}
}

// A label that only a human can clear is never "waiting correctly": it is
// needs-human and ranks in the attention view.
func TestAPIKeyHelperDetectedNeedsHuman(t *testing.T) {
	in := baseInput()
	in.Labels = []string{"fabrik:api-key-helper-detected"}
	in.StatusEnteredAt = t0.Add(-time.Minute)
	got := Classify(in)
	if got.State != NeedsHuman || got.Code != "fabrik:api-key-helper-detected" || got.Rank != RankNeedsHuman {
		t.Fatalf("got %s/%s rank %d (%s), want needs-human/api-key-helper-detected ranked", got.State, got.Code, got.Rank, got.Summary)
	}
}

// An engine-retried wait with no known deadline must not read as waiting
// forever: past the stall threshold with no progress it is stalled. Open-ended
// waits (blocked, queued, claude-limit) and recently active waits are not.
func TestWaitWithNoDeadlineBecomesStalled(t *testing.T) {
	old := t0.Add(-3 * time.Hour)
	cases := []struct {
		name    string
		labels  []string
		entered time.Time
		want    State
	}{
		{"awaiting-advance stuck", []string{"fabrik:awaiting-advance"}, old, Stalled},
		{"awaiting-advance recent", []string{"fabrik:awaiting-advance"}, t0.Add(-time.Minute), Waiting},
		{"blocked is open-ended", []string{"fabrik:blocked"}, old, Waiting},
		{"claude-limit is open-ended", []string{"fabrik:claude-limit"}, old, Waiting},
	}
	for _, c := range cases {
		in := baseInput()
		in.Labels = c.labels
		in.StatusEnteredAt = c.entered
		got := Classify(in)
		if got.State != c.want {
			t.Errorf("%s: got %s/%s (%s), want %s", c.name, got.State, got.Code, got.Summary, c.want)
		}
		if c.want == Stalled && got.Rank != RankStalled {
			t.Errorf("%s: stalled wait must rank, got %d", c.name, got.Rank)
		}
		if c.want == Waiting && got.Rank != NotRanked {
			t.Errorf("%s: waiting must not rank, got %d", c.name, got.Rank)
		}
	}
}
