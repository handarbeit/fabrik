package engine

import (
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/channelevents"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

var settleValidateStage = &stages.Stage{Name: "Validate"}

func settledOnly() *channelevents.Subscription {
	return &channelevents.Subscription{Events: []channelevents.EventType{channelevents.ValidateSettled}}
}

// seedValidate places issue n at Validate with a linked open PR #34.
func seedValidate(t *testing.T, e *Engine, n int, labels ...string) {
	t.Helper()
	e.seedAPIItem(t, n, "Validate", labels...)
	e.store.Apply(itemstate.PRDetailsUpdated{Repo: "owner/repo", Number: n, PRNumber: 34, Title: "pr", State: "open"})
	e.store.Apply(itemstate.PRHeadSHAUpdated{Repo: "owner/repo", Number: n, LinkedPRNum: 34, SHA: "abc123"})
}

func hook(e *Engine, n int) {
	e.noteValidateSettled(gh.ProjectItem{Repo: "owner/repo", Number: n}, settleValidateStage)
}

func landing(e *Engine, n int) settleLanding {
	return e.noteValidateLanding(gh.ProjectItem{Repo: "owner/repo", Number: n}, settleValidateStage)
}

func TestValidateSettledNotEmittedWhileAwaitingCI(t *testing.T) {
	e, sink := channelEngine(t, 0, settledOnly())
	// FABRIK_STAGE_COMPLETE under wait_for_ci: awaiting-ci present, no complete label.
	seedValidate(t, e, 1, "fabrik:awaiting-ci")
	hook(e, 1)
	sink.quiet(t, 250*time.Millisecond)

	// Complete label added but the review gate has applied awaiting-review.
	e.store.Apply(itemstate.LocalLabelRemoved{Repo: "owner/repo", Number: 1, Label: "fabrik:awaiting-ci"})
	e.store.Apply(itemstate.LocalLabelAdded{Repo: "owner/repo", Number: 1, Label: "stage:Validate:complete"})
	e.store.Apply(itemstate.LocalLabelAdded{Repo: "owner/repo", Number: 1, Label: "fabrik:awaiting-review"})
	hook(e, 1)
	sink.quiet(t, 250*time.Millisecond)
}

func TestValidateSettledEmittedOnceThenAgainAfterRevalidate(t *testing.T) {
	e, sink := channelEngine(t, 0, settledOnly())
	seedValidate(t, e, 1, "fabrik:cruise", "stage:Validate:complete")

	hook(e, 1)
	hook(e, 1)
	ev := sink.next(t)
	if ev.Type != channelevents.ValidateSettled || ev.PR != 34 {
		t.Fatalf("unexpected: %+v", ev)
	}
	if ev.Meta["next"] != "waiting-for-human" || ev.Meta["autonomy"] != "cruise" {
		t.Fatalf("cruise meta: %+v", ev.Meta)
	}
	for _, k := range []string{"ci", "reviews", "unresolved_threads", "pr", "stage", "event"} {
		if ev.Meta[k] == "" {
			t.Errorf("meta %q missing", k)
		}
	}
	hook(e, 1)
	sink.quiet(t, 250*time.Millisecond) // exactly once per episode

	// fabrik:revalidate removes the complete label; Validate re-completes.
	e.store.Apply(itemstate.LocalLabelRemoved{Repo: "owner/repo", Number: 1, Label: "stage:Validate:complete"})
	time.Sleep(150 * time.Millisecond) // let the debounced episode close run
	e.store.Apply(itemstate.LocalLabelAdded{Repo: "owner/repo", Number: 1, Label: "stage:Validate:complete"})
	hook(e, 1)
	again := sink.next(t)
	if again.Type != channelevents.ValidateSettled || again.DedupKey == ev.DedupKey {
		t.Fatalf("expected a new episode, got %+v (first key %q)", again, ev.DedupKey)
	}
}

func TestValidateSettledNext(t *testing.T) {
	cases := []struct {
		name   string
		labels []string
		train  string
		want   string
	}{
		{"cruise waits for a human", []string{"fabrik:cruise"}, "off", "waiting-for-human"},
		{"cruise wins over yolo", []string{"fabrik:cruise", "fabrik:yolo"}, "on", "waiting-for-human"},
		{"no autonomy waits for a human", nil, "off", "waiting-for-human"},
		{"yolo without the train auto-merges", []string{"fabrik:yolo"}, "off", "auto-merge"},
		{"yolo with the train queues", []string{"fabrik:yolo"}, "on", "merge-train"},
		{"auto-merge already enabled", []string{"fabrik:yolo", "fabrik:auto-merge-enabled"}, "on", "auto-merge"},
	}
	for _, c := range cases {
		e, sink := channelEngine(t, 0, settledOnly())
		e.cfg.MergeTrain = c.train
		seedValidate(t, e, 1, append([]string{"stage:Validate:complete"}, c.labels...)...)
		if c.want == "waiting-for-human" {
			hook(e, 1)
		} else {
			// An acting item is announced by the landing decision, once it succeeded.
			landing(e, 1).finish(true)
		}
		ev := sink.next(t)
		if ev.Meta["next"] != c.want {
			t.Errorf("%s: next=%q want %q", c.name, ev.Meta["next"], c.want)
		}
	}
}

func TestValidateSettledHeldByUnprocessedFeedback(t *testing.T) {
	e, sink := channelEngine(t, 0, settledOnly())
	seedValidate(t, e, 1, "stage:Validate:complete")
	e.store.Apply(itemstate.IssueCommentCreated{Repo: "owner/repo", Number: 1, Comment: gh.Comment{ID: "c1", DatabaseID: 11, Author: "alice", Body: "please look"}})
	hook(e, 1)
	sink.quiet(t, 250*time.Millisecond)
}

func TestValidateSettledIgnoresOtherStagesAndNoHub(t *testing.T) {
	e := apiEngine(t, 0)
	hook(e, 1) // no hub: must not panic
	e2, sink := channelEngine(t, 0, settledOnly())
	seedValidate(t, e2, 1, "stage:Validate:complete")
	e2.noteValidateSettled(gh.ProjectItem{Repo: "owner/repo", Number: 1}, &stages.Stage{Name: "Implement"})
	sink.quiet(t, 200*time.Millisecond)
}

// Drift guard: the cache-only feedback predicate must agree with the live
// feedback gate on every shared fixture (the gate is fed the same item with
// alreadyLive=true, so neither side reads GitHub for the comparison).
func TestCachedFeedbackMatchesLiveGate(t *testing.T) {
	processedThread := openThread
	cases := map[string]func(*gh.ProjectItem){
		"clean":         func(*gh.ProjectItem) {},
		"issue comment": func(i *gh.ProjectItem) { i.Comments = []gh.Comment{humanComment} },
		"fabrik comment": func(i *gh.ProjectItem) {
			i.Comments = []gh.Comment{{ID: "c9", DatabaseID: 9, Author: "bot", Body: "🏭 **Fabrik — stage: Plan**\n\nx"}}
		},
		"rocketed comment": func(i *gh.ProjectItem) {
			c := humanComment
			c.Reactions = []gh.ReactionGroup{{Content: "ROCKET", Count: 1}}
			i.Comments = []gh.Comment{c}
		},
		"review thread": func(i *gh.ProjectItem) {
			i.LinkedPRNumber = 34
			i.LinkedPRReviewThreadComments = []gh.Comment{openThread}
		},
		"outdated thread": func(i *gh.ProjectItem) {
			th := openThread
			th.IsOutdated = true
			i.LinkedPRNumber = 34
			i.LinkedPRReviewThreadComments = []gh.Comment{th}
		},
		"processed thread": func(i *gh.ProjectItem) {
			i.LinkedPRNumber = 34
			i.LinkedPRReviewThreadComments = []gh.Comment{processedThread}
		},
		"review body": func(i *gh.ProjectItem) {
			i.LinkedPRNumber = 34
			i.LinkedPRReviews = []gh.PRReview{bodyOnlyReview}
		},
		"approved no body": func(i *gh.ProjectItem) {
			i.LinkedPRNumber = 34
			i.LinkedPRReviews = []gh.PRReview{{Author: "a", State: "APPROVED", DatabaseID: 9}}
		},
		"dismissed body": func(i *gh.ProjectItem) {
			r := bodyOnlyReview
			r.State = "DISMISSED"
			i.LinkedPRNumber = 34
			i.LinkedPRReviews = []gh.PRReview{r}
		},
	}
	for name, mod := range cases {
		t.Run(name, func(t *testing.T) {
			eng := testEngineForMerge(t, landingClient())
			item := feedbackItem(mod)
			eng.store.Apply(itemstate.IssueOpened{Item: item})
			if name == "processed thread" {
				eng.store.Apply(itemstate.CommentProcessed{Repo: "owner/repo", Number: 1, CommentID: processedThread.ID, At: time.Now()})
			}
			snap, ok := eng.store.Peek("owner/repo", 1)
			if !ok {
				t.Fatal("item not in store")
			}
			st := snap.State()
			cached := eng.cachedPendingFeedback(snap, &st).any()
			live := eng.feedbackGateBlocks(item, true, "advance")
			if cached != live {
				t.Errorf("cached feedback pending = %v, live gate blocks = %v", cached, live)
			}
		})
	}
}

func TestCachedAddressedReviewIDsOnlyTrustsFabrik(t *testing.T) {
	eng := testEngineForMerge(t, landingClient())
	self := eng.selfLogin()
	marker := "🏭 **Fabrik — review feedback**\n\n<!-- fabrik:review-ids-addressed: 5356334497 -->"
	eng.store.Apply(itemstate.IssueOpened{Item: feedbackItem(func(i *gh.ProjectItem) {
		i.LinkedPRNumber = 34
		i.LinkedPRReviews = []gh.PRReview{bodyOnlyReview}
		i.Comments = []gh.Comment{{ID: "m1", DatabaseID: 5, Author: "stranger", Body: marker}}
	})})
	snap, _ := eng.store.Peek("owner/repo", 1)
	st := snap.State()
	if !eng.cachedPendingFeedback(snap, &st).any() {
		t.Fatal("a stranger's marker must not mark the review addressed")
	}
	st.Comments = []gh.Comment{{ID: "m2", DatabaseID: 6, Author: self, Body: marker}}
	if eng.cachedPendingFeedback(snap, &st).any() {
		t.Fatal("Fabrik's own marker must mark the review addressed")
	}
}

// A yolo item acts on its own, so the settle point must not announce it ahead of
// the landing attempt; the landing announces it only once it succeeded.
func TestValidateSettledYoloAnnouncedOnlyAfterLandingSucceeds(t *testing.T) {
	e, sink := channelEngine(t, 0, settledOnly())
	seedValidate(t, e, 1, "fabrik:yolo", "stage:Validate:complete")

	hook(e, 1) // Phase 2 settle point: the landing attempt is still to come
	sink.quiet(t, 250*time.Millisecond)

	landing(e, 1).finish(false) // the merge or enqueue failed
	sink.quiet(t, 250*time.Millisecond)

	landing(e, 1).finish(true)
	ev := sink.next(t)
	if ev.Type != channelevents.ValidateSettled || ev.Meta["next"] != "auto-merge" {
		t.Fatalf("unexpected: %+v", ev)
	}
}

// A landing that bounced ends the episode (fabrik:rebase-needed), so the settle
// after the rebase is announced afresh instead of being deduplicated away.
func TestValidateSettledReannouncedAfterRebaseNeeded(t *testing.T) {
	e, sink := channelEngine(t, 0, settledOnly())
	seedValidate(t, e, 1, "fabrik:cruise", "stage:Validate:complete")
	hook(e, 1)
	first := sink.next(t)

	e.store.Apply(itemstate.LocalLabelAdded{Repo: "owner/repo", Number: 1, Label: "fabrik:rebase-needed"})
	time.Sleep(150 * time.Millisecond) // let the debounced episode close run
	e.store.Apply(itemstate.LocalLabelRemoved{Repo: "owner/repo", Number: 1, Label: "fabrik:rebase-needed"})
	hook(e, 1)
	again := sink.next(t)
	if again.Type != channelevents.ValidateSettled || again.DedupKey == first.DedupKey {
		t.Fatalf("expected a new episode after the rebase, got %+v (first key %q)", again, first.DedupKey)
	}
}
