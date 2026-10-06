package engine

import (
	"context"
	"errors"
	"fmt"
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

// hook calls the Phase 2 settle-point hook directly. The tests below that use it
// cover the hook's own cache-only predicate and episode rules; that the real
// runCatchUpPhase2 reaches the hook at the right moment (and not at
// FABRIK_STAGE_COMPLETE under fabrik:awaiting-ci) is covered end to end by the
// PollOnce scenarios in tests/sim/channel_events_test.go.
func hook(e *Engine, n int) {
	e.noteValidateSettled(gh.ProjectItem{Repo: "owner/repo", Number: n}, settleValidateStage)
}

// landing captures a landing-time settle point directly, for tests of how the
// announcement is built. Whether attemptMergeOnValidate announces only a landing
// that succeeded is TestValidateSettledYoloAnnouncedOnlyAfterLandingSucceeds,
// which drives the production call site.
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

// TestValidateSettledNextFromLabelsAndConfig covers how meta.next is derived from
// the item's labels and merge_train for a captured settle point. It calls the
// capture/announce helpers directly, so it does not claim the landing wiring.
func TestValidateSettledNextFromLabelsAndConfig(t *testing.T) {
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
			// An acting item is announced by the landing decision once it succeeded
			// (finish(true) stands in for that outcome here).
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

// landingEngine builds an engine wired with a mock GitHub client (so the real
// attemptMergeOnValidate landing path runs end to end), with the channel hub
// started and one yolo item at Validate in the store.
func landingEngine(t *testing.T, client *mockGitHubClient, train string) (*Engine, *chanSink) {
	t.Helper()
	oldD, oldT := channelDebounce, channelTick
	channelDebounce, channelTick = 20*time.Millisecond, time.Hour
	t.Cleanup(func() { channelDebounce, channelTick = oldD, oldT })

	e := testEngineWithStages(t, client, testStagesWithValidateAndHolding())
	e.cfg.MergeTrain = train
	e.health.markStarted(time.Now().Add(-time.Hour))
	e, sink := channelEngineOn(t, e, 0, settledOnly())
	seedValidate(t, e, 1, "fabrik:yolo", "stage:Validate:complete")
	return e, sink
}

// landYolo drives the production landing decision for issue 1 and returns what
// it reported.
func landYolo(t *testing.T, e *Engine) (enabled, deferred bool, err error) {
	t.Helper()
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Repo: "owner/repo", Labels: []string{"fabrik:yolo"}}
	return e.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item, validateStage())
}

// directMergeClient has a linked PR and an auto-merge enable that fails as
// "already clean", so the landing falls back to a direct MergePR.
func directMergeClient(mergeErr error) *mockGitHubClient {
	c := landingClient()
	c.enablePullRequestAutoMergeFn = func(owner, repo string, prNumber int, strategy string) error {
		return fmt.Errorf("%w: clean status", gh.ErrAutoMergeAlreadyClean)
	}
	c.mergePRFn = func(owner, repo string, prNumber int) error { return mergeErr }
	return c
}

// A yolo item acts on its own, so the settle point must not announce it ahead of
// the landing attempt; attemptMergeOnValidate announces it only once the merge or
// enqueue succeeded. These drive the real landing path against a mock GitHub
// client, so they protect the success condition at the call site in stages.go.
func TestValidateSettledYoloAnnouncedOnlyAfterLandingSucceeds(t *testing.T) {
	t.Run("the Phase 2 settle point does not announce a yolo item", func(t *testing.T) {
		e, sink := landingEngine(t, landingClient(), "off")
		hook(e, 1) // the landing attempt is still to come
		sink.quiet(t, 250*time.Millisecond)
	})

	t.Run("merge not mergeable: no event", func(t *testing.T) {
		e, sink := landingEngine(t, directMergeClient(gh.ErrNotMergeable), "off")
		if _, _, err := landYolo(t, e); !errors.Is(err, gh.ErrNotMergeable) {
			t.Fatalf("landing error = %v, want ErrNotMergeable", err)
		}
		sink.quiet(t, 250*time.Millisecond)
	})

	t.Run("enqueue to Queued fails: no event", func(t *testing.T) {
		c := &mockGitHubClient{updateProjectItemStatusFn: func(projectID, itemID, fieldID, optionID string) error {
			return fmt.Errorf("boom")
		}}
		e, sink := landingEngine(t, c, "on")
		if _, _, err := landYolo(t, e); err == nil {
			t.Fatal("expected the move to Queued to fail")
		}
		sink.quiet(t, 250*time.Millisecond)
	})

	t.Run("deferred by the direct-merge feedback recheck: no event", func(t *testing.T) {
		c := directMergeClient(nil)
		reads := 0
		c.fetchItemDetailsFn = func(item *gh.ProjectItem) error {
			if reads++; reads >= 2 { // a comment arrives after the first read
				item.Comments = []gh.Comment{humanComment}
			}
			return nil
		}
		e, sink := landingEngine(t, c, "off")
		if enabled, deferred, err := landYolo(t, e); enabled || !deferred || err != nil {
			t.Fatalf("want deferred without error, got enabled=%v deferred=%v err=%v", enabled, deferred, err)
		}
		if n := len(c.mergePRCalls); n != 0 {
			t.Fatalf("MergePR called %d time(s) while feedback was pending", n)
		}
		sink.quiet(t, 250*time.Millisecond)
	})

	t.Run("auto-merge enabled: one auto-merge event", func(t *testing.T) {
		e, sink := landingEngine(t, landingClient(), "off")
		if enabled, deferred, err := landYolo(t, e); !enabled || deferred || err != nil {
			t.Fatalf("want enabled, got enabled=%v deferred=%v err=%v", enabled, deferred, err)
		}
		ev := sink.next(t)
		if ev.Type != channelevents.ValidateSettled || ev.Meta["next"] != "auto-merge" {
			t.Fatalf("unexpected: %+v", ev)
		}
		sink.quiet(t, 250*time.Millisecond) // exactly one
	})

	t.Run("direct merge succeeds: one auto-merge event", func(t *testing.T) {
		e, sink := landingEngine(t, directMergeClient(nil), "off")
		if enabled, _, err := landYolo(t, e); !enabled || err != nil {
			t.Fatalf("want merged, got enabled=%v err=%v", enabled, err)
		}
		if ev := sink.next(t); ev.Meta["next"] != "auto-merge" {
			t.Fatalf("unexpected: %+v", ev)
		}
		sink.quiet(t, 250*time.Millisecond)
	})

	t.Run("merge train on and the move to Queued succeeds: one merge-train event", func(t *testing.T) {
		c := &mockGitHubClient{}
		e, sink := landingEngine(t, c, "on")
		if _, _, err := landYolo(t, e); err != nil {
			t.Fatalf("landing: %v", err)
		}
		if len(c.updateStatusCalls) != 1 {
			t.Fatalf("expected one move to the holding stage, got %d", len(c.updateStatusCalls))
		}
		ev := sink.next(t)
		if ev.Type != channelevents.ValidateSettled || ev.Meta["next"] != "merge-train" {
			t.Fatalf("unexpected: %+v", ev)
		}
		sink.quiet(t, 250*time.Millisecond)
	})
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
