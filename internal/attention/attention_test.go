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

func TestCooldownAndClaudeUsageLimitDeadlines(t *testing.T) {
	in := baseInput()
	in.Labels = []string{"fabrik:claude-limit"}
	in.ClaudeUsageLimitedUntil = t0.Add(3 * time.Hour)
	in.Cooldowns = map[string]time.Time{"retry": t0.Add(10 * time.Minute)}
	got := Classify(in)
	if !got.Next.At.Equal(t0.Add(10 * time.Minute)) {
		t.Errorf("Next.At = %v, want the cooldown expiry", got.Next.At)
	}
	var sawUsageLimit bool
	for _, d := range got.Next.Deadlines {
		if d.Kind == "claude-usage-limit-end" && d.At.Equal(t0.Add(3*time.Hour)) {
			sawUsageLimit = true
		}
	}
	if !sawUsageLimit {
		t.Errorf("claude usage-limit hold deadline missing: %+v", got.Next.Deadlines)
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

func stalledNoProgress() Input {
	in := baseInput()
	in.StatusEnteredAt = t0.Add(-2 * time.Hour)
	return in
}

func TestPollStallAttribution(t *testing.T) {
	stall := PollStall{Active: true, Blocked: 923 * time.Second, Step: "dispatch: waiting for a worker slot"}

	// Healthy poll: an item with no progress is stalled, exactly as before.
	healthy := Classify(stalledNoProgress())
	if healthy.State != Stalled || healthy.Rank != RankStalled {
		t.Fatalf("healthy poll: got %s rank %d, want stalled", healthy.State, healthy.Rank)
	}

	// Overdue poll: the same item waits for the poll and is not ranked.
	in := stalledNoProgress()
	in.PollStall = stall
	got := Classify(in)
	if got.State != Waiting || got.Code != CodeWaitingForPoll || got.Rank != NotRanked {
		t.Fatalf("overdue poll: got %s/%s rank %d, want waiting/%s unranked", got.State, got.Code, got.Rank, CodeWaitingForPoll)
	}
	want := "waiting for the poll: blocked for 923 s, in step dispatch: waiting for a worker slot"
	if got.Summary != want || got.Next.Action != want {
		t.Fatalf("summary %q / action %q, want %q", got.Summary, got.Next.Action, want)
	}

	// Stage-complete-stuck is also an engine-owned obstacle.
	in = stalledNoProgress()
	in.Labels = []string{"stage:Implement:complete"}
	in.PollStall = stall
	if got := Classify(in); got.State != Waiting || got.Code != CodeWaitingForPoll {
		t.Fatalf("stage-complete-stuck under stalled poll: got %s/%s", got.State, got.Code)
	}

	// A wait keeps its state and code; the cause is prepended.
	in = baseInput()
	in.Labels = []string{"fabrik:awaiting-ci"}
	in.PollStall = PollStall{Active: true, Blocked: 10 * time.Second}
	got = Classify(in)
	if got.State != Waiting || got.Code != "fabrik:awaiting-ci" || !strings.HasPrefix(got.Summary, "waiting for the poll: blocked for 10 s") {
		t.Fatalf("wait under stalled poll: got %s/%s %q", got.State, got.Code, got.Summary)
	}
	if strings.Contains(got.Summary, ", in step") {
		t.Fatalf("unknown step must not be invented: %q", got.Summary)
	}

	// Verdicts that need a human or a worker are untouched.
	for _, mod := range []func(*Input){
		func(in *Input) { in.Labels = []string{"fabrik:paused", "fabrik:awaiting-input"} },
		func(in *Input) { in.Labels = []string{"fabrik:api-key-helper-detected"} },
		func(in *Input) {
			in.HasWorker = true
			in.WorkerStartedAt = t0.Add(-3 * time.Hour)
			in.StatusEnteredAt = t0.Add(-3 * time.Hour)
		},
		func(in *Input) { in.Closed = true },
	} {
		a := stalledNoProgress()
		mod(&a)
		b := a
		b.PollStall = stall
		ra, rb := Classify(a), Classify(b)
		if ra.State != rb.State || ra.Code != rb.Code || ra.Summary != rb.Summary {
			t.Fatalf("verdict changed under stalled poll: %s/%s %q -> %s/%s %q", ra.State, ra.Code, ra.Summary, rb.State, rb.Code, rb.Summary)
		}
	}
}

func TestPollRecoveryFloorsProgressAnchor(t *testing.T) {
	in := stalledNoProgress()
	in.PollRecoveredAt = t0.Add(-time.Minute)
	got := Classify(in)
	if got.State == Stalled {
		t.Fatalf("an item must get a fresh stall window after the poll recovers, got %s/%s", got.State, got.Code)
	}
	// Unknown anchor stays unknown.
	in = baseInput()
	in.StatusEnteredAt = time.Time{}
	in.PollRecoveredAt = t0.Add(-time.Hour)
	if r := Classify(in); !r.ProgressAt.IsZero() {
		t.Fatalf("unknown anchor must stay unknown, got %v", r.ProgressAt)
	}
	// A recovery long ago does not hide a real stall.
	in = stalledNoProgress()
	in.PollRecoveredAt = t0.Add(-90 * time.Minute)
	if got := Classify(in); got.State != Stalled {
		t.Fatalf("stall after an old recovery: got %s", got.State)
	}
}

func TestSkillUnresolvedNeedsHuman(t *testing.T) {
	in := baseInput()
	in.Labels = []string{"fabrik:skill-unresolved"}
	in.StatusEnteredAt = t0.Add(-time.Minute)
	got := Classify(in)
	if got.State != NeedsHuman || got.Code != "fabrik:skill-unresolved" || got.Rank != RankNeedsHuman {
		t.Fatalf("got %s/%s rank %d (%s), want needs-human/skill-unresolved ranked", got.State, got.Code, got.Rank, got.Summary)
	}
}

// ---- Suspended (#2120) ----

func specifyComplete() Input {
	in := baseInput()
	in.Status = "Specify"
	in.NextStage = "Research"
	in.Labels = []string{"stage:Specify:complete"}
	in.LabelAppliedAt = map[string]time.Time{"stage:Specify:complete": t0.Add(-68 * 24 * time.Hour)}
	// A daemon that just started: first-seen is "now-ish".
	in.StatusEnteredAt = t0.Add(-time.Minute)
	return in
}

func TestSuspendedDecisionPendingAfterRestart(t *testing.T) {
	got := Classify(specifyComplete())
	if got.State != Suspended || got.Code != CodeDecisionPending || got.Rank != RankNeedsHuman {
		t.Fatalf("got %s/%s rank %d (%s), want suspended/decision-pending ranked with needs-human", got.State, got.Code, got.Rank, got.Summary)
	}
	if got.Pending == nil || got.Pending.SinceBasis != SinceGitHub {
		t.Fatalf("pending = %+v, want a GitHub-anchored since-time", got.Pending)
	}
	if got.ProgressAge < 67*24*time.Hour || got.ProgressAge > 69*24*time.Hour {
		t.Errorf("age = %s, want about 68 days (anchored on the GitHub label time, not daemon first-seen)", got.ProgressAge)
	}
	for _, want := range []string{"advance to Research", "cruise"} {
		if !strings.Contains(got.Next.Action, want) {
			t.Errorf("next action %q must name %q", got.Next.Action, want)
		}
	}
	if !got.Next.At.IsZero() || len(got.Next.Deadlines) != 0 {
		t.Errorf("a suspended item schedules nothing: %+v", got.Next)
	}
	if strings.Contains(got.Summary, "stalled") {
		t.Errorf("summary must not read as a stall: %q", got.Summary)
	}
}

func TestSuspendedNeverStalled(t *testing.T) {
	in := specifyComplete()
	in.StatusEnteredAt = t0.Add(-100 * 24 * time.Hour)
	in.LastAttemptAt = t0.Add(-99 * 24 * time.Hour)
	if got := Classify(in); got.State != Suspended {
		t.Fatalf("a long-waiting decision is %s, want suspended", got.State)
	}
	// Control: the same item behind an engine mask is the pre-existing stalled
	// path, proving the stall branch is reachable here.
	in.Labels = []string{"stage:Specify:complete", "fabrik:editing"}
	if got := Classify(in); got.State == Suspended {
		t.Fatalf("control: masked item must not be suspended")
	}
}

func TestSuspendedUnknownTimeSaysAtLeast(t *testing.T) {
	in := specifyComplete()
	in.LabelAppliedAt = nil
	got := Classify(in)
	if got.State != Suspended {
		t.Fatalf("got %s", got.State)
	}
	if got.Pending.SinceBasis != SinceFirstSeen || got.Pending.SinceExact() {
		t.Fatalf("basis = %s, want the first-seen lower bound", got.Pending.SinceBasis)
	}
	if !got.ProgressAt.IsZero() {
		t.Errorf("a daemon-observed age must not be presented as the wait: ProgressAt=%v", got.ProgressAt)
	}
	if !strings.Contains(got.Summary, "at least since") {
		t.Errorf("summary %q must say \"at least since\"", got.Summary)
	}
	// Control: the stage comment time is a GitHub proxy and is exact.
	in.StageCommentAt = t0.Add(-3 * time.Hour)
	got = Classify(in)
	if got.Pending.SinceBasis != SinceStageComment || got.ProgressAge != 3*time.Hour {
		t.Errorf("stage-comment fallback = %s / %s", got.Pending.SinceBasis, got.ProgressAge)
	}
}

func TestSuspendedAutonomyIsNotPending(t *testing.T) {
	in := specifyComplete()
	in.AutoAdvances = true
	in.Labels = append(in.Labels, "fabrik:cruise")
	if got := Classify(in); got.State == Suspended {
		t.Fatalf("an auto-advancing item is %s, must not be suspended", got.State)
	}
	in.AutoAdvances = false
	in.Labels = []string{"stage:Specify:complete"}
	if got := Classify(in); got.State != Suspended {
		t.Fatalf("control: got %s, want suspended", got.State)
	}
}

func TestSuspendedExcludesValidate(t *testing.T) {
	in := specifyComplete()
	in.Status = "Validate"
	in.NextStage = ""
	in.Labels = []string{"stage:Validate:complete"}
	in.StatusEnteredAt = t0.Add(-2 * time.Hour)
	if got := Classify(in); got.State == Suspended {
		t.Fatalf("Validate keeps its existing ranking, got suspended")
	}
}

func TestSuspendedMasks(t *testing.T) {
	for _, tc := range []struct {
		name string
		mod  func(*Input)
	}{
		{"worker", func(in *Input) { in.HasWorker = true; in.WorkerStartedAt = t0 }},
		{"awaiting-ci", func(in *Input) { in.Labels = append(in.Labels, "fabrik:awaiting-ci") }},
		{"rebase-needed", func(in *Input) { in.Labels = append(in.Labels, "fabrik:rebase-needed") }},
		{"editing", func(in *Input) { in.Labels = append(in.Labels, "fabrik:editing") }},
		{"in-progress", func(in *Input) { in.Labels = append(in.Labels, "stage:Research:in_progress") }},
		{"locked", func(in *Input) { in.Labels = append(in.Labels, "fabrik:locked:me") }},
		{"pending feedback", func(in *Input) { in.FeedbackPending = true }},
		{"blocked", func(in *Input) { in.Labels = append(in.Labels, "fabrik:blocked") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := specifyComplete()
			tc.mod(&in)
			if got := Classify(in); got.State == Suspended {
				t.Fatalf("%s must mask the decision, got suspended", tc.name)
			}
		})
	}
	// Cooldowns never mask: a periodic re-evaluation cannot change anything.
	in := specifyComplete()
	in.Cooldowns = map[string]time.Time{"periodic-re-eval": t0.Add(10 * time.Minute), "dep-blocked": t0.Add(time.Minute)}
	got := Classify(in)
	if got.State != Suspended || !got.Next.At.IsZero() {
		t.Fatalf("a cooldown must not mask or schedule a suspended item: %s next_at=%v", got.State, got.Next.At)
	}
}

func parkedInput() Input {
	in := baseInput()
	in.Status = "Specify"
	in.Labels = []string{"fabrik:blocked"}
	in.LabelAppliedAt = map[string]time.Time{"fabrik:blocked": t0.Add(-5 * 24 * time.Hour)}
	in.ParkedBlockers = []ParkedBlocker{{Ref: "o/r#1372", Column: "Backlog"}}
	in.BlockedOnlyByParked = true
	return in
}

func TestSuspendedParkedBlocker(t *testing.T) {
	got := Classify(parkedInput())
	if got.State != Suspended || got.Code != CodeParkedBlocker || got.Rank != RankNeedsHuman {
		t.Fatalf("got %s/%s rank %d, want suspended/parked-blocker", got.State, got.Code, got.Rank)
	}
	if !strings.Contains(got.Next.Action, "#1372") || !strings.Contains(got.Next.Action, "fabrik_promote") {
		t.Errorf("next action %q must name the blocker and the promote tool", got.Next.Action)
	}
	if got.ProgressAge < 4*24*time.Hour {
		t.Errorf("age = %s, want anchored on fabrik:blocked", got.ProgressAge)
	}
	// Control: a blocker in an active pipeline column stays waiting.
	in := parkedInput()
	in.BlockedOnlyByParked = false
	if got := Classify(in); got.State != Waiting || got.Code != "fabrik:blocked" {
		t.Fatalf("active-column blocker = %s/%s, want waiting/fabrik:blocked", got.State, got.Code)
	}
	// Control: a parked blocker with a worker running is masked.
	in = parkedInput()
	in.HasWorker, in.WorkerStartedAt = true, t0
	if got := Classify(in); got.State == Suspended {
		t.Fatalf("a worker masks the parked-blocker state")
	}
}

func TestEligibilityChangeResetsTheAnchor(t *testing.T) {
	// Settled at Validate under cruise 40 minutes ago; the operator then set yolo.
	in := baseInput()
	in.Status = "Validate"
	in.Labels = []string{"stage:Validate:complete", "fabrik:yolo"}
	in.StatusEnteredAt = t0.Add(-40 * time.Minute)
	in.LabelAppliedAt = map[string]time.Time{"stage:Validate:complete": t0.Add(-40 * time.Minute)}
	in.EligibilityChangedAt = t0.Add(-10 * time.Second)
	got := Classify(in)
	if got.State == Stalled {
		t.Fatalf("a decision made seconds ago read as a stall: %s", got.Summary)
	}
	if got.ProgressAge != 10*time.Second {
		t.Errorf("age = %s, want it to start at the change (10s)", got.ProgressAge)
	}
	// Past one threshold after the change it is stalled.
	in.Now = t0.Add(31 * time.Minute)
	if got := Classify(in); got.State != Stalled {
		t.Fatalf("after a threshold since the change: %s, want stalled", got.State)
	}
	// Control: without the stamp (unknown) the 40-minute age is what stalls it.
	in = baseInput()
	in.Status = "Validate"
	in.Labels = []string{"stage:Validate:complete", "fabrik:yolo"}
	in.StatusEnteredAt = t0.Add(-40 * time.Minute)
	in.LabelAppliedAt = map[string]time.Time{"stage:Validate:complete": t0.Add(-40 * time.Minute)}
	if got := Classify(in); got.State != Stalled {
		t.Fatalf("control without a stamp = %s, want stalled", got.State)
	}
}

func TestEligibilityChangeRestartsAPendingAge(t *testing.T) {
	in := specifyComplete()
	in.EligibilityChangedAt = t0.Add(-time.Hour)
	got := Classify(in)
	if got.Pending.SinceBasis != SinceEligibility || got.ProgressAge != time.Hour {
		t.Fatalf("basis=%s age=%s, want the age to start at the change", got.Pending.SinceBasis, got.ProgressAge)
	}
}

func TestNextAtIsNotACooldownForNonProgressingStates(t *testing.T) {
	cd := map[string]time.Time{"retry": t0.Add(8 * time.Minute), "periodic-re-eval": t0.Add(12 * time.Minute)}
	for _, tc := range []struct {
		name   string
		labels []string
	}{
		{"paused", []string{"fabrik:paused", "fabrik:awaiting-input"}},
		{"blocked", []string{"fabrik:blocked"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := baseInput()
			in.Labels = tc.labels
			in.Cooldowns = cd
			got := Classify(in)
			if !got.Next.At.IsZero() {
				t.Errorf("next_at = %v, want absent", got.Next.At)
			}
			if strings.Contains(got.Next.Action, "cooldown") {
				t.Errorf("next action %q names a cooldown", got.Next.Action)
			}
		})
	}
	// Control: an awaiting-ci wait keeps its real deadline.
	in := baseInput()
	in.Labels = []string{"fabrik:awaiting-ci"}
	in.LastCIProgressAt = t0
	if got := Classify(in); got.Next.At.IsZero() {
		t.Errorf("control: awaiting-ci lost its deadline")
	}
	// periodic-re-eval alone is no deadline anywhere.
	in = baseInput()
	in.Cooldowns = map[string]time.Time{"periodic-re-eval": t0.Add(time.Hour)}
	got := Classify(in)
	if !got.Next.At.IsZero() || strings.Contains(got.Next.Action, "re-dispatched") {
		t.Errorf("periodic-re-eval presented as a next action: %+v", got.Next)
	}
}

func TestResumeMethodFollowsTheEngineComment(t *testing.T) {
	paused := func(r ResumeMethod, extra ...string) Result {
		in := baseInput()
		in.Labels = append([]string{"fabrik:paused"}, extra...)
		in.Resume = r
		return Classify(in)
	}
	got := paused(ResumeRemoveLabel, "fabrik:awaiting-input")
	if !strings.Contains(got.Next.Action, "remove fabrik:paused") || strings.Contains(got.Next.Action, "human comment") {
		t.Errorf("remove-label pause: %q", got.Next.Action)
	}
	if len(got.Options) != 1 || !strings.Contains(got.Options[0].Action, "remove the fabrik:paused label") {
		t.Errorf("options = %+v", got.Options)
	}
	got = paused(ResumeComment, "fabrik:awaiting-input")
	if !strings.Contains(got.Next.Action, "human comment") || !strings.Contains(got.Options[0].Action, "comment") {
		t.Errorf("comment pause: %q / %+v", got.Next.Action, got.Options)
	}
	// Unknown claims neither contradicting method for a bare pause.
	got = paused(ResumeUnknown)
	if strings.Contains(got.Next.Action, "human comment") || strings.Contains(got.Next.Action, "remove fabrik:paused") ||
		!strings.Contains(got.Next.Action, "pause comment") {
		t.Errorf("unknown-method pause must point at the comment: %q", got.Next.Action)
	}
}

func TestOptionsPerDecisionKind(t *testing.T) {
	// Pending decision.
	got := Classify(specifyComplete())
	var tools []string
	for _, o := range got.Options {
		tools = append(tools, o.Tool)
	}
	if strings.Join(tools, ",") != "fabrik_advance,fabrik_set_autonomy cruise,fabrik_set_autonomy yolo" {
		t.Errorf("decision options tools = %v", tools)
	}
	if got.Options[0].Action != "advance to Research" {
		t.Errorf("first option = %+v", got.Options[0])
	}
	// Parked blocker.
	got = Classify(parkedInput())
	if len(got.Options) != 2 || got.Options[0].Tool != "fabrik_promote" || got.Options[1].Tool != "" {
		t.Errorf("parked options = %+v", got.Options)
	}
	// Validate settled under cruise.
	in := baseInput()
	in.Status = "Validate"
	in.Labels = []string{"stage:Validate:complete", "fabrik:cruise"}
	in.HasOpenUnmergedPR, in.PRNumber = true, 77
	got = Classify(in)
	if len(got.Options) != 2 || got.Options[0].Action != "merge PR #77" || got.Options[0].Tool != "" ||
		got.Options[1].Tool != "fabrik_set_autonomy yolo" {
		t.Errorf("merge-decision options = %+v", got.Options)
	}
}
