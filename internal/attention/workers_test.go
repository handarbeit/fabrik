package attention

import (
	"strings"
	"testing"
	"time"
)

func hasReasonCode(rs []Reason, prefix string) bool {
	for _, r := range rs {
		if strings.HasPrefix(r.Code, prefix) {
			return true
		}
	}
	return false
}

func hasDeadlineKind(ds []Deadline, prefix string) bool {
	for _, d := range ds {
		if strings.HasPrefix(d.Kind, prefix) {
			return true
		}
	}
	return false
}

// workerInput is an item with a running worker of the given kind plus every
// competing signal that used to produce another verdict: a wait label (F4), a
// dispatch cooldown (F15), a retry, a gate hold and dispatchability.
func workerInput(kind, trigger string) Input {
	in := baseInput()
	in.HasWorker = true
	in.WorkerKind = kind
	in.WorkerTrigger = trigger
	in.WorkerStartedAt = t0.Add(-2 * time.Minute)
	in.Labels = []string{"fabrik:awaiting-ci", "fabrik:editing"}
	in.Cooldowns = map[string]time.Time{"feedback-pending": t0.Add(10 * time.Minute), "retry-backoff": t0.Add(time.Minute)}
	in.Retry = &RetryFact{Stage: "Implement", Cause: "wall-time-kill", Due: t0.Add(-time.Minute)}
	in.Gate = &GateFact{Gate: "feedback", IDs: []string{"review-body:1"}}
	in.Dispatchable = true
	in.SlotsBusy, in.SlotsMax = 5, 5
	return in
}

// R1/R4 (F4, F15): every kind of worker in progress is working, names its kind
// and trigger, and never reports a dispatch cooldown.
func TestEveryWorkerKindIsWorkingAndNamedNeverCooldown(t *testing.T) {
	cases := []struct {
		kind, trigger string
		wantKind      string
		wantTrigger   string
	}{
		{WorkerKindStageRun, "", "a worker is running", ""},
		{"", "", "a worker is running", ""},
		{WorkerKindCommentReview, "C_77", "a comment review", "C_77"},
		{WorkerKindReviewReinvoke, "review-body:5483772616", "a review reinvoke", "review-body:5483772616"},
		{WorkerKindCIFixReinvoke, "C_9", "a CI-fix reinvoke", "C_9"},
		{WorkerKindRebaseReinvoke, "C_3", "a rebase reinvoke", "C_3"},
	}
	for _, c := range cases {
		t.Run(c.kind+"/"+c.wantKind, func(t *testing.T) {
			res := Classify(workerInput(c.kind, c.trigger))
			if res.State != Working || res.Code != "worker-in-flight" {
				t.Fatalf("got %s/%s: %s", res.State, res.Code, res.Summary)
			}
			if !strings.Contains(res.Summary, c.wantKind) {
				t.Errorf("summary %q does not name the kind %q", res.Summary, c.wantKind)
			}
			if c.wantTrigger != "" && !strings.Contains(res.Summary, c.wantTrigger) {
				t.Errorf("summary %q does not name the trigger %q", res.Summary, c.wantTrigger)
			}
			if strings.Contains(res.Summary, "waiting for CI") {
				t.Errorf("summary %q reports the wait label instead of the worker (F4)", res.Summary)
			}
			if hasReasonCode(res.Reasons, "cooldown:") || hasDeadlineKind(res.Next.Deadlines, "cooldown:") {
				t.Errorf("a running worker must not report a cooldown: reasons=%v deadlines=%v", res.Reasons, res.Next.Deadlines)
			}
			if strings.HasPrefix(res.Code, "cooldown:") {
				t.Errorf("code %q is a cooldown", res.Code)
			}
		})
	}
}

func TestMergeTrainConflictResolutionIsWorking(t *testing.T) {
	in := baseInput()
	in.StageKind, in.Status = KindHolding, "Queued"
	in.Conflict = &ConflictFact{Since: t0.Add(-time.Minute)}
	in.Cooldowns = map[string]time.Time{"merge-unsettled": t0.Add(time.Minute)}
	res := Classify(in)
	if res.State != Working || !strings.Contains(res.Summary, "merge-train conflict resolution") {
		t.Fatalf("got %s/%s: %s", res.State, res.Code, res.Summary)
	}
	if hasReasonCode(res.Reasons, "cooldown:") {
		t.Errorf("reasons %v carry a cooldown", res.Reasons)
	}
}

func TestMergeTrainConflictQueuedForSlot(t *testing.T) {
	in := baseInput()
	in.StageKind, in.Status = KindHolding, "Queued"
	in.Conflict = &ConflictFact{Queued: true, Since: t0.Add(-time.Minute)}
	res := Classify(in)
	if res.State != Waiting || res.Code != CodeQueuedForSlot || res.Cause != CauseSlotStarved {
		t.Fatalf("got %s/%s cause=%q: %s", res.State, res.Code, res.Cause, res.Summary)
	}
}

// R2 (F16): queued is not running.
func TestQueuedWorkerIsWaitingNotWorking(t *testing.T) {
	in := workerInput(WorkerKindReviewReinvoke, "")
	in.WorkerQueued = true
	in.WorkerQueuedSince = t0.Add(-110 * time.Second)
	in.WorkerStartedAt = t0.Add(-110 * time.Second)
	res := Classify(in)
	if res.State != Waiting || res.Code != CodeQueuedForSlot || res.Rank != NotRanked {
		t.Fatalf("got %s/%s rank=%d: %s", res.State, res.Code, res.Rank, res.Summary)
	}
	if res.Cause != CauseSlotStarved {
		t.Errorf("cause = %q, want slot-starved", res.Cause)
	}
	want := "queued: waiting for a worker slot (since " + in.WorkerQueuedSince.UTC().Format(time.RFC3339) + ")"
	if !strings.Contains(res.Summary, want) {
		t.Errorf("summary %q lacks %q", res.Summary, want)
	}
	if !strings.Contains(res.Summary, "all 5 of 5 worker slots are busy") {
		t.Errorf("summary %q should state the slot count", res.Summary)
	}
	if hasReasonCode(res.Reasons, "cooldown:") {
		t.Errorf("a queued worker must not report a cooldown: %v", res.Reasons)
	}
}

// R6: live turns when known, otherwise nothing (the engine's wire says unknown).
func TestWorkerSummaryReportsTurnsOnlyWhenKnown(t *testing.T) {
	in := workerInput(WorkerKindCommentReview, "C_1")
	in.WorkerTurnsKnown, in.WorkerTurnsUsed, in.WorkerMaxTurns = true, 12, 50
	if got := Classify(in).Summary; !strings.Contains(got, "(turn 12 of 50)") {
		t.Errorf("summary %q lacks the turns", got)
	}
	in.WorkerMaxTurns = 0
	if got := Classify(in).Summary; !strings.Contains(got, "(turn 12)") {
		t.Errorf("summary %q lacks the turns", got)
	}
	in.WorkerTurnsKnown = false
	if got := Classify(in).Summary; strings.Contains(got, "turn") {
		t.Errorf("summary %q invents a turn count", got)
	}
}

func TestLongTriggerListIsAbbreviated(t *testing.T) {
	res := Classify(workerInput(WorkerKindReviewReinvoke, "a,b,c,d,e"))
	if !strings.Contains(res.Summary, "a, b, c and 2 more") {
		t.Errorf("summary %q", res.Summary)
	}
}

// R3 (F10): an item with work to do and no worker is waiting for dispatch.
func TestWaitingForDispatch(t *testing.T) {
	base := func() Input {
		in := baseInput()
		in.Dispatchable = true
		in.Labels = []string{"fabrik:cruise"}
		in.SlotsBusy, in.SlotsMax = 1, 5
		return in
	}
	t.Run("all slots busy", func(t *testing.T) {
		in := base()
		in.SlotsBusy = 5
		res := Classify(in)
		if res.State != Waiting || res.Code != CodeWaitingForDispatch || res.Cause != CauseSlotStarved {
			t.Fatalf("got %s/%s cause=%q: %s", res.State, res.Code, res.Cause, res.Summary)
		}
		if !strings.Contains(res.Summary, "all 5 of 5 worker slots are busy") {
			t.Errorf("summary %q should name the slot count", res.Summary)
		}
	})
	t.Run("next poll", func(t *testing.T) {
		res := Classify(base())
		if res.State != Waiting || res.Code != CodeWaitingForDispatch || res.Cause != "" {
			t.Fatalf("got %s/%s cause=%q: %s", res.State, res.Code, res.Cause, res.Summary)
		}
		if !strings.Contains(res.Summary, "next poll") {
			t.Errorf("summary %q", res.Summary)
		}
	})
	t.Run("a cooldown that really re-dispatches", func(t *testing.T) {
		in := base()
		in.Cooldowns = map[string]time.Time{"retry-backoff": t0.Add(3 * time.Minute)}
		res := Classify(in)
		if res.State != Waiting || res.Code != CodeWaitingForDispatch || !strings.Contains(res.Summary, `"retry-backoff"`) {
			t.Fatalf("got %s/%s: %s", res.State, res.Code, res.Summary)
		}
		if !strings.Contains(res.Summary, t0.Add(3*time.Minute).UTC().Format(time.RFC3339)) {
			t.Errorf("summary %q should say when", res.Summary)
		}
	})
	t.Run("a safety-net cooldown is not the reason", func(t *testing.T) {
		in := base()
		in.Cooldowns = map[string]time.Time{"periodic-re-eval": t0.Add(3 * time.Minute)}
		res := Classify(in)
		if res.Code != CodeWaitingForDispatch || strings.Contains(res.Summary, "periodic") {
			t.Fatalf("got %s/%s: %s", res.State, res.Code, res.Summary)
		}
	})
	t.Run("never idle", func(t *testing.T) {
		in := base()
		in.Dispatchable = false
		if res := Classify(in); res.Code != "idle" {
			t.Fatalf("control: a non-dispatchable item is idle, got %s/%s", res.State, res.Code)
		}
	})
	t.Run("gated items are not waiting for dispatch", func(t *testing.T) {
		in := base()
		in.Dispatchable = false
		in.Labels = []string{"fabrik:cruise", "fabrik:blocked"}
		if res := Classify(in); res.Code != "fabrik:blocked" {
			t.Fatalf("got %s/%s", res.State, res.Code)
		}
	})
	t.Run("an unexplained gap still stalls", func(t *testing.T) {
		in := base()
		in.StatusEnteredAt = t0.Add(-2 * time.Hour)
		if res := Classify(in); res.State != Stalled {
			t.Fatalf("slots free, no cooldown, long idle: got %s/%s", res.State, res.Code)
		}
	})
	t.Run("busy slots never stall however long", func(t *testing.T) {
		in := base()
		in.SlotsBusy = 5
		in.StatusEnteredAt = t0.Add(-5 * time.Hour)
		if res := Classify(in); res.State != Waiting {
			t.Fatalf("got %s/%s", res.State, res.Code)
		}
	})
}

// FR-014/FR-015 (starved retry after a kill).
func TestKilledItemWithPendingRetryIsNotStalled(t *testing.T) {
	mk := func(cause string) Input {
		in := baseInput()
		in.StatusEnteredAt = t0.Add(-2 * time.Hour)
		in.LastAttemptAt = t0.Add(-40 * time.Minute)
		in.Labels = []string{"stage:Implement:in_progress", "fabrik:locked:fabrik-dev"}
		in.Retry = &RetryFact{Stage: "Implement", Cause: cause, Due: t0.Add(-39 * time.Minute)}
		in.Dispatchable = true
		in.SlotsBusy, in.SlotsMax = 5, 5
		return in
	}
	t.Run("wall time", func(t *testing.T) {
		res := Classify(mk("wall-time-kill"))
		if res.State != Waiting || res.Code != CodeRetryWaitingForSlot || res.Cause != CauseWallTimeKill {
			t.Fatalf("got %s/%s cause=%q: %s", res.State, res.Code, res.Cause, res.Summary)
		}
		if !strings.Contains(res.Summary, "wall-time limit") || !strings.Contains(res.Summary, "retry") {
			t.Errorf("summary %q should name the kill and the retry", res.Summary)
		}
		// The leftover labels are explained.
		found := map[string]string{}
		for _, r := range res.Reasons {
			found[r.Code] = r.Meaning
		}
		for _, l := range []string{"stage:Implement:in_progress", "fabrik:locked:fabrik-dev"} {
			if !strings.Contains(found[l], "left over from the killed run") {
				t.Errorf("label %s unexplained: %q", l, found[l])
			}
		}
	})
	t.Run("turn limit", func(t *testing.T) {
		res := Classify(mk("max-turns"))
		if res.State != Waiting || res.Cause != CauseMaxTurns || !strings.Contains(res.Summary, "turn limit") {
			t.Fatalf("got %s/%s cause=%q: %s", res.State, res.Code, res.Cause, res.Summary)
		}
	})
	t.Run("other causes are named but yield no vocabulary cause", func(t *testing.T) {
		res := Classify(mk("inactivity-kill"))
		if res.State != Waiting || res.Cause != "" || !strings.Contains(res.Summary, "inactivity") {
			t.Fatalf("got %s/%s cause=%q: %s", res.State, res.Code, res.Cause, res.Summary)
		}
	})
	t.Run("retry not yet due", func(t *testing.T) {
		in := mk("wall-time-kill")
		in.Retry.Due = t0.Add(time.Minute)
		res := Classify(in)
		if res.Code != CodeRetryPending || res.Next.At.IsZero() {
			t.Fatalf("got %s/%s next=%v: %s", res.State, res.Code, res.Next.At, res.Summary)
		}
	})
	t.Run("no retry scheduled is not described as one", func(t *testing.T) {
		in := mk("wall-time-kill")
		in.Retry = nil
		in.Dispatchable = false
		res := Classify(in)
		if res.State != Stalled || strings.Contains(res.Summary, "retry") {
			t.Fatalf("an exhausted or absent retry must not read as pending: got %s/%s: %s", res.State, res.Code, res.Summary)
		}
	})
	t.Run("a worker wins over the pending retry", func(t *testing.T) {
		in := mk("wall-time-kill")
		in.HasWorker, in.WorkerStartedAt = true, t0.Add(-time.Minute)
		if res := Classify(in); res.State != Working {
			t.Fatalf("got %s/%s", res.State, res.Code)
		}
	})
	t.Run("due long ago with a free slot is a real stall", func(t *testing.T) {
		in := mk("wall-time-kill")
		in.SlotsBusy = 1
		if res := Classify(in); res.State != Stalled {
			t.Fatalf("got %s/%s: %s", res.State, res.Code, res.Summary)
		}
	})
}

// FR-016 (feedback-gate hold).
func TestGateHoldIsTheReasonNotAStall(t *testing.T) {
	in := baseInput()
	in.StatusEnteredAt = t0.Add(-2 * time.Hour)
	in.Labels = []string{"stage:Implement:complete"}
	in.Gate = &GateFact{Gate: "feedback", Reason: "holding advance — 1 unprocessed review body",
		IDs: []string{"review-body:5483772616"}, Since: t0.Add(-35 * time.Minute)}
	res := Classify(in)
	if res.State != Waiting || res.Code != CodeGateHold || res.Cause != "gate-hold:feedback" {
		t.Fatalf("got %s/%s cause=%q: %s", res.State, res.Code, res.Cause, res.Summary)
	}
	if !strings.Contains(res.Summary, "review-body:5483772616") || !strings.Contains(res.Summary, "feedback gate") {
		t.Errorf("summary %q should name the gate and the pending feedback id", res.Summary)
	}
	in.Gate.IDs = []string{"c1", "c2", "c3", "c4"}
	if got := Classify(in).Summary; !strings.Contains(got, "and 1 more") {
		t.Errorf("several pieces of feedback: %q", got)
	}
	in.Gate = nil
	if res := Classify(in); res.State != Stalled || res.Code != CodeStageCompleteStuck {
		t.Fatalf("control without the hold: got %s/%s", res.State, res.Code)
	}
}

// R-cause: each vocabulary entry from the inputs that establish it, and absent
// when they are missing.
func TestCauseVocabulary(t *testing.T) {
	cases := []struct {
		name    string
		cause   string
		present func(*Input)
		absent  func(*Input)
	}{
		{"daemon-stop", CauseDaemonStop,
			func(in *Input) { in.Labels = []string{"fabrik:paused", "fabrik:awaiting-input"}; in.DaemonStop = true },
			func(in *Input) { in.Labels = []string{"fabrik:paused", "fabrik:awaiting-input"} }},
		{"daemon-stop on an escalated pause", CauseDaemonStop,
			func(in *Input) { in.Labels = []string{"fabrik:paused"}; in.PausedByEngine = true; in.DaemonStop = true },
			func(in *Input) { in.Labels = []string{"fabrik:paused"}; in.PausedByEngine = true }},
		{"slot-starved (queued)", CauseSlotStarved,
			func(in *Input) { in.HasWorker, in.WorkerQueued = true, true },
			func(in *Input) { in.HasWorker = true }},
		{"slot-starved (waiting for dispatch)", CauseSlotStarved,
			func(in *Input) { in.Dispatchable, in.SlotsBusy, in.SlotsMax = true, 5, 5 },
			func(in *Input) { in.Dispatchable, in.SlotsBusy, in.SlotsMax = true, 1, 5 }},
		{"wall-time-kill", CauseWallTimeKill,
			func(in *Input) { in.Retry = &RetryFact{Stage: "Implement", Cause: "wall-time-kill", Due: t0} },
			func(in *Input) { in.Retry = nil }},
		{"max-turns", CauseMaxTurns,
			func(in *Input) { in.Retry = &RetryFact{Stage: "Implement", Cause: "max-turns", Due: t0} },
			func(in *Input) { in.Retry = &RetryFact{Stage: "Implement", Cause: "error", Due: t0} }},
		{"gate-hold:review", CauseGateHold("review"),
			func(in *Input) { in.Gate = &GateFact{Gate: "review", Since: t0} },
			func(in *Input) { in.Gate = nil }},
		{"gate-hold:ci", CauseGateHold("ci"),
			func(in *Input) { in.Gate = &GateFact{Gate: "ci", Since: t0} },
			func(in *Input) { in.Gate = nil }},
		{"rebase-conflict-after-landing", CauseRebaseConflictAfterLanding,
			func(in *Input) { in.Labels = []string{"fabrik:rebase-needed"}; in.RebaseAfterLanding = true },
			func(in *Input) { in.Labels = []string{"fabrik:rebase-needed"} }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := baseInput()
			c.present(&in)
			if got := Classify(in).Cause; got != c.cause {
				t.Errorf("with the establishing inputs: cause = %q, want %q", got, c.cause)
			}
			in = baseInput()
			c.absent(&in)
			if got := Classify(in).Cause; got != "" {
				t.Errorf("without them: cause = %q, want it absent", got)
			}
		})
	}
	// A cause only describes the result it came with: no stray cause on a plain
	// idle item or a running worker.
	if got := Classify(baseInput()).Cause; got != "" {
		t.Errorf("idle item cause = %q", got)
	}
	if got := Classify(workerInput(WorkerKindStageRun, "")).Cause; got != "" {
		t.Errorf("running worker cause = %q", got)
	}
}

func TestCauseVocabularyIsPinned(t *testing.T) {
	want := []string{"daemon-stop", "slot-starved", "wall-time-kill", "max-turns", "gate-hold:<gate>", "rebase-conflict-after-landing"}
	got := Causes()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("cause vocabulary = %v, want %v (a new cause is a documented addition)", got, want)
	}
}

func TestMeaningOfResolvesVariableLabels(t *testing.T) {
	for _, l := range []string{"fabrik:reworking:Implement", "fabrik:locked:fabrik-dev", "fabrik:credited-pr:12", "fabrik:spawned-child:1:44"} {
		if MeaningOf(l) == "" {
			t.Errorf("no meaning for %s", l)
		}
	}
	if MeaningOf("fabrik:not-a-label") != "" || MeaningOf("model:opus") != "" {
		t.Error("MeaningOf must stay empty for non-state labels")
	}
	in := baseInput()
	in.Labels = []string{"fabrik:reworking:Implement"}
	if !hasReasonCode(Classify(in).Reasons, "fabrik:reworking:") {
		t.Error("fabrik:reworking:<Stage> must appear in reasons (F15)")
	}
}
