package engine

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/warnings"
)

// reviewStatusFixture is an engine with a Review stage that waits for reviews
// and declares pruefer, a mutable PR head and a mutable review history.
type reviewStatusFixture struct {
	t       *testing.T
	client  *mockGitHubClient
	eng     *Engine
	head    string
	state   string
	history []gh.PRReview
	item    gh.ProjectItem
}

func newReviewStatusFixture(t *testing.T, mutate func(*stages.Stage, *Config)) *reviewStatusFixture {
	t.Helper()
	setWarningsOverride(t)
	f := &reviewStatusFixture{t: t, head: commitA, state: "open"}
	f.client = &mockGitHubClient{
		fetchPRDetailsFn: func(owner, repo string, pr int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: pr, State: f.state, HeadSHA: f.head}, nil
		},
		fetchPRReviewHistoryFn: func(owner, repo string, pr int) ([]gh.PRReview, error) {
			return f.history, nil
		},
	}
	f.eng = testEngine(t, f.client, &mockClaudeInvoker{})
	st := &stages.Stage{Name: "Review", Order: 1, WaitForReviews: boolP(true), ReviewStatus: boolP(true),
		ExpectedReviewers: &[]string{"pruefer"}}
	cfg := f.eng.cfg
	if mutate != nil {
		mutate(st, &cfg)
	}
	cfg.Stages = []*stages.Stage{st}
	f.eng.cfg = cfg
	f.item = gh.ProjectItem{Number: 7, Repo: "owner/repo", Status: "Review", LinkedPRNumber: 70}
	return f
}

func (f *reviewStatusFixture) scan() {
	f.eng.settleReviewStatusScan(&gh.ProjectBoard{Items: []gh.ProjectItem{f.item}})
}

func (f *reviewStatusFixture) statuses() []commitStatusCall {
	f.client.mu.Lock()
	defer f.client.mu.Unlock()
	return append([]commitStatusCall(nil), f.client.commitStatusCalls...)
}

func (f *reviewStatusFixture) wantLast(state, sha string, total int) {
	f.t.Helper()
	got := f.statuses()
	if len(got) != total {
		f.t.Fatalf("status writes = %d (%+v), want %d", len(got), got, total)
	}
	if total == 0 {
		return
	}
	last := got[len(got)-1]
	if last.State != state || last.SHA != sha || last.Context != "fabrik/review" {
		f.t.Fatalf("last write = %+v, want state %s on %s", last, state, sha)
	}
}

const prBot = "pruefer[bot]"

// AC1/AC2: pending → success on a clean review of the head; a push resets to
// pending for the new head; a change request at the head is failure.
func TestReviewStatusScan_TransitionsAndPushReset(t *testing.T) {
	f := newReviewStatusFixture(t, nil)

	f.scan()
	f.wantLast(reviewStatusPending, commitA, 1)

	f.history = []gh.PRReview{rv(1, prBot, "COMMENTED", commitA, 1)}
	f.scan()
	f.wantLast(reviewStatusSuccess, commitA, 2)

	f.head = commitB // a push: the old review is of the old head
	f.scan()
	f.wantLast(reviewStatusPending, commitB, 3)

	f.history = append(f.history, rv(2, prBot, "CHANGES_REQUESTED", commitB, 5))
	f.scan()
	f.wantLast(reviewStatusFailure, commitB, 4)
}

// A change request on an earlier commit is pending for the new head, never failure.
func TestReviewStatusScan_OldChangeRequestIsPendingNotFailure(t *testing.T) {
	f := newReviewStatusFixture(t, nil)
	f.history = []gh.PRReview{rv(1, prBot, "CHANGES_REQUESTED", commitA, 1)}
	f.head = commitB
	f.scan()
	f.wantLast(reviewStatusPending, commitB, 1)
}

// The head-blind neutraliser makes the push-reset assertion fail: proof the
// tests above bite on the head-aware verdict.
func TestReviewStatusScan_NonVacuity_HeadBlindSkipsPushReset(t *testing.T) {
	f := newReviewStatusFixture(t, nil)
	f.eng.SetReviewStatusHeadBlindForTest(true)
	f.history = []gh.PRReview{rv(1, prBot, "COMMENTED", commitA, 1)}
	f.head = commitB
	f.scan()
	if got := f.statuses(); len(got) != 1 || got[0].State != reviewStatusSuccess {
		t.Fatalf("head-blind verdict = %+v, want a (wrong) success on the new head", got)
	}
}

// An unchanged state writes nothing, however many polls pass.
func TestReviewStatusScan_UnchangedWritesNothing(t *testing.T) {
	f := newReviewStatusFixture(t, nil)
	for i := 0; i < 4; i++ {
		f.scan()
	}
	f.wantLast(reviewStatusPending, commitA, 1)
}

// AC4: off by default / no write when not enabled — no config key, no stage key,
// an explicit stage false over the repo-wide key, no wait_for_reviews, and the
// engine seam.
func TestReviewStatusScan_DisabledWritesNothing(t *testing.T) {
	for name, mutate := range map[string]func(*stages.Stage, *Config){
		"nothing enabled":               func(s *stages.Stage, c *Config) { s.ReviewStatus = nil },
		"stage false beats repo-wide":   func(s *stages.Stage, c *Config) { s.ReviewStatus = boolP(false); c.ReviewStatus = true },
		"no wait_for_reviews":           func(s *stages.Stage, c *Config) { s.WaitForReviews = nil },
		"no expected reviewers":         func(s *stages.Stage, c *Config) { s.ExpectedReviewers = &[]string{} },
		"expected reviewers undeclared": func(s *stages.Stage, c *Config) { s.ExpectedReviewers = nil },
	} {
		t.Run(name, func(t *testing.T) {
			f := newReviewStatusFixture(t, mutate)
			f.scan()
			f.wantLast("", "", 0)
		})
	}
	t.Run("seam", func(t *testing.T) {
		f := newReviewStatusFixture(t, nil)
		f.eng.SetReviewStatusDisabledForTest(true)
		f.scan()
		f.wantLast("", "", 0)
	})
}

func TestReviewStatusScan_RepoWideKeyEnablesAStageWithoutItsOwn(t *testing.T) {
	f := newReviewStatusFixture(t, func(s *stages.Stage, c *Config) { s.ReviewStatus = nil; c.ReviewStatus = true })
	f.scan()
	f.wantLast(reviewStatusPending, commitA, 1)
}

func TestReviewStatusScan_LabelOverrideOfExpectedReviewersIsHonoured(t *testing.T) {
	f := newReviewStatusFixture(t, func(s *stages.Stage, c *Config) { s.ExpectedReviewers = &[]string{"pruefer"} })
	f.item.Labels = []string{"expected-reviewers:none"} // no reviewer to wait on
	f.scan()
	f.wantLast("", "", 0)
}

// No status for a closed or merged PR, an unreadable PR or history, a PR with no head,
// a closed issue, an item with no linked PR, a PR item, or while the forge is held.
func TestReviewStatusScan_SkipsWhatCannotBeAnswered(t *testing.T) {
	t.Run("closed PR", func(t *testing.T) {
		f := newReviewStatusFixture(t, nil)
		f.state = "closed"
		f.scan()
		f.wantLast("", "", 0)
	})
	t.Run("merged PR", func(t *testing.T) {
		f := newReviewStatusFixture(t, nil)
		f.client.fetchPRDetailsFn = func(o, r string, pr int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: pr, State: "closed", Merged: true, HeadSHA: commitA}, nil
		}
		f.scan()
		f.wantLast("", "", 0)
	})
	t.Run("no linked PR", func(t *testing.T) {
		f := newReviewStatusFixture(t, nil)
		f.item.LinkedPRNumber = 0
		f.scan()
		f.wantLast("", "", 0)
	})
	t.Run("closed issue", func(t *testing.T) {
		f := newReviewStatusFixture(t, nil)
		f.item.IsClosed = true
		f.scan()
		f.wantLast("", "", 0)
	})
	t.Run("PR details unreadable", func(t *testing.T) {
		f := newReviewStatusFixture(t, nil)
		f.client.fetchPRDetailsFn = func(o, r string, pr int) (*gh.PRDetails, error) { return nil, errors.New("boom") }
		f.scan()
		f.wantLast("", "", 0)
	})
	t.Run("history unreadable never writes success", func(t *testing.T) {
		f := newReviewStatusFixture(t, nil)
		f.client.fetchPRReviewHistoryFn = func(o, r string, pr int) ([]gh.PRReview, error) { return nil, errors.New("boom") }
		f.scan()
		f.wantLast("", "", 0)
	})
	t.Run("no head", func(t *testing.T) {
		f := newReviewStatusFixture(t, nil)
		f.client.noHeadBackfill = true
		f.client.fetchPRDetailsFn = func(o, r string, pr int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: pr, State: "open"}, nil
		}
		f.scan()
		f.wantLast("", "", 0)
	})
}

// FR-012: a refused write is reported once on the warnings panel, never pauses
// an item or blocks a gate, is retried, and clears when the write succeeds.
func TestReviewStatusScan_WriteFailureReportedOnceAndNeverBlocks(t *testing.T) {
	f := newReviewStatusFixture(t, nil)
	fail := true
	f.client.createCommitStatusFn = func(owner, repo, sha, state, ctx, desc string) error {
		if fail {
			return fmt.Errorf("creating status: %w", gh.ErrForbidden)
		}
		return nil
	}
	for i := 0; i < 3; i++ {
		f.scan()
	}
	if got := len(f.statuses()); got != 3 {
		t.Fatalf("a failed write must be retried every poll: %d attempts, want 3", got)
	}
	entries, _ := warnings.Load()
	var n int
	for _, en := range entries {
		if en.Type == reviewStatusWriteWarnType {
			n++
			if !strings.Contains(en.Detail, "statuses: write") || !strings.Contains(en.Detail, "repo") {
				t.Errorf("panel detail should name the permission and the PAT scope: %q", en.Detail)
			}
		}
	}
	if n != 1 {
		t.Fatalf("review_status_write entries = %d, want exactly 1", n)
	}
	// Nothing was paused or labelled.
	f.client.mu.Lock()
	if len(f.client.addLabelCalls) != 0 || len(f.client.addCommentCalls) != 0 {
		t.Errorf("a failed status write touched labels/comments: %v %v", f.client.addLabelCalls, f.client.addCommentCalls)
	}
	f.client.mu.Unlock()

	fail = false
	f.scan()
	entries, _ = warnings.Load()
	for _, en := range entries {
		if en.Type == reviewStatusWriteWarnType {
			t.Errorf("entry %s still present after a successful write", en.Key)
		}
	}
	f.scan()
	if got := len(f.statuses()); got != 4 {
		t.Errorf("after the retry succeeded no further write is due: %d writes, want 4", got)
	}
}

func TestReviewStatusConfigWarnings(t *testing.T) {
	setWarningsOverride(t)
	keysOf := func() []string {
		entries, _ := warnings.Load()
		var ks []string
		for _, en := range entries {
			if en.Type == reviewStatusConfigWarnType {
				ks = append(ks, en.Key)
			}
		}
		return ks
	}
	t.Run("enabled with no expected reviewers", func(t *testing.T) {
		f := newReviewStatusFixture(t, func(s *stages.Stage, c *Config) { s.ExpectedReviewers = &[]string{} })
		f.scan()
		if ks := keysOf(); len(ks) != 1 || ks[0] != "review_status_config:stage:Review" {
			t.Fatalf("config entries = %v", ks)
		}
		f.eng.cfg.Stages[0].ExpectedReviewers = &[]string{"pruefer"}
		f.eng.reviewStatus.configDone = false
		f.scan()
		if ks := keysOf(); len(ks) != 0 {
			t.Fatalf("entry should clear once fixed: %v", ks)
		}
	})
	t.Run("review_status without wait_for_reviews", func(t *testing.T) {
		f := newReviewStatusFixture(t, func(s *stages.Stage, c *Config) { s.WaitForReviews = nil })
		f.scan()
		if ks := keysOf(); len(ks) != 1 {
			t.Fatalf("config entries = %v, want 1", ks)
		}
	})
	t.Run("repo-wide key but no stage waits for reviews", func(t *testing.T) {
		f := newReviewStatusFixture(t, func(s *stages.Stage, c *Config) {
			s.ReviewStatus = nil
			s.WaitForReviews = nil
			c.ReviewStatus = true
		})
		f.scan()
		if ks := keysOf(); len(ks) != 1 || ks[0] != "review_status_config:repo" {
			t.Fatalf("config entries = %v", ks)
		}
	})
	t.Run("healthy configuration reports nothing", func(t *testing.T) {
		f := newReviewStatusFixture(t, nil)
		f.scan()
		if ks := keysOf(); len(ks) != 0 {
			t.Fatalf("config entries = %v", ks)
		}
	})
}

// R3 plumbing: the review-wait floor is "now" while pending, the moment pending
// ended afterwards, and zero with no record, with the attribution seam, or for
// another PR. Instants are host-clock times (the CI dwells compare with time.Since).
func TestReviewWaitFloor(t *testing.T) {
	f := newReviewStatusFixture(t, nil)
	near := func(got time.Time) bool {
		return !got.IsZero() && time.Since(got) < 5*time.Second && !got.After(time.Now())
	}

	if !f.eng.reviewWaitFloor(f.item).IsZero() {
		t.Fatal("no record: floor must be zero")
	}
	f.scan() // pending
	if got := f.eng.reviewWaitFloor(f.item); !near(got) {
		t.Fatalf("pending floor = %v, want about now", got)
	}
	if head, ok := f.eng.reviewWaitPending(f.item); !ok || head != commitA {
		t.Fatalf("reviewWaitPending = (%q, %v)", head, ok)
	}

	f.history = []gh.PRReview{rv(1, prBot, "COMMENTED", commitA, 1)}
	f.scan() // success: the wait ended now
	ended := f.eng.reviewWaitFloor(f.item)
	if !near(ended) {
		t.Fatalf("floor after the wait = %v, want the end of the wait", ended)
	}
	time.Sleep(20 * time.Millisecond)
	if got := f.eng.reviewWaitFloor(f.item); !got.Equal(ended) {
		t.Fatalf("floor must stay at the end of the wait: %v then %v", ended, got)
	}
	if _, ok := f.eng.reviewWaitPending(f.item); ok {
		t.Fatal("success must not read as a review wait")
	}

	other := f.item
	other.LinkedPRNumber = 99
	if !f.eng.reviewWaitFloor(other).IsZero() {
		t.Fatal("another PR must have no floor")
	}
	f.eng.SetReviewWaitAttributionDisabledForTest(true)
	if !f.eng.reviewWaitFloor(f.item).IsZero() {
		t.Fatal("attribution seam must zero the floor")
	}
}

// A write that failed never counts as an attributed review wait: CI is not
// held on a status that does not exist.
func TestReviewWaitFloor_FailedWriteIsNotAWait(t *testing.T) {
	f := newReviewStatusFixture(t, nil)
	f.client.createCommitStatusFn = func(o, r, sha, st, c, d string) error { return gh.ErrForbidden }
	f.scan()
	if !f.eng.reviewWaitFloor(f.item).IsZero() {
		t.Fatal("a refused write must not produce a review wait floor")
	}
}

func TestReviewStatusScan_RecordDroppedWhenItemLeavesStage(t *testing.T) {
	f := newReviewStatusFixture(t, nil)
	f.scan()
	f.item.Status = "Validate" // no such enabling stage
	f.scan()
	if !f.eng.reviewWaitFloor(f.item).IsZero() {
		t.Fatal("record must not outlive the item's enabling stage")
	}
}

// A failed write must not leave the earlier pending record in place: a stale
// pending record would keep suppressing the CI timers and rewording the backstop
// as a review wait once the review it was waiting for has arrived. But the wait
// did last until that moment, so the end of the wait is kept (not dropped with
// the record): a later successful write must still move the CI dwell anchors
// past the review wait, or an item that waited hours for review could be paused
// as a CI stall straight away.
func TestReviewWaitFloor_FailedWriteKeepsTheEndOfTheWaitNotThePendingState(t *testing.T) {
	f := newReviewStatusFixture(t, nil)
	f.scan() // pending written
	if f.eng.reviewWaitFloor(f.item).IsZero() {
		t.Fatal("setup: a written pending status must produce a floor")
	}
	if _, waiting := f.eng.reviewWaitPending(f.item); !waiting {
		t.Fatal("setup: expected a pending review wait")
	}
	f.history = []gh.PRReview{rv(1, prBot, "COMMENTED", commitA, 1)}
	f.client.createCommitStatusFn = func(o, r, sha, st, c, d string) error { return gh.ErrForbidden }
	before := time.Now()
	f.scan() // success write refused
	if _, waiting := f.eng.reviewWaitPending(f.item); waiting {
		t.Error("a refused write left the stale pending record in place")
	}
	ended := f.eng.reviewWaitFloor(f.item)
	if ended.IsZero() || ended.Before(before) || ended.After(time.Now()) {
		t.Fatalf("a refused write must keep the moment the wait ended; floor = %v", ended)
	}
	// Retried scans keep failing, then succeed: the end of the wait is unchanged.
	f.scan()
	if got := f.eng.reviewWaitFloor(f.item); !got.Equal(ended) {
		t.Errorf("repeated failures moved the end of the wait: %v -> %v", ended, got)
	}
	f.client.createCommitStatusFn = nil
	f.scan()
	if got := f.eng.reviewWaitFloor(f.item); !got.Equal(ended) {
		t.Errorf("a successful write after a failed one lost the end of the wait: %v -> %v", ended, got)
	}
	if _, waiting := f.eng.reviewWaitPending(f.item); waiting {
		t.Error("the settled status must not read as pending")
	}
}
