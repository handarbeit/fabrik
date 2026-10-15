package engine

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// ─── #2297: landing settle wait, merge retry and keeping a green trial ───────────────

// landingRig scripts one landing PR through mark-ready: each FetchPRDetails read pops the
// next scripted mergeable_state (the last one repeats), every read reports the same head,
// and MergePR pops the next scripted error (nil = success). It records the order of merge
// and cleanup events so "no cleanup before the merge" is checkable.
type landingRig struct {
	t      *testing.T
	eng    *Engine
	client *mockGitHubClient
	wm     *WorktreeManager

	mu        sync.Mutex
	states    []string
	mergeErrs []error
	events    []string
	reads     int
	comments  []string
	deleted   []bool // deletedBranch of each release, in order
}

type landingRigOpts struct {
	checkRuns []gh.CheckRun
	headNow   string // head reported by FetchPRDetails (default "sha1")
	behind    int    // FetchCommitsBehind result
}

func newLandingRig(t *testing.T, states []string, mergeErrs []error, o landingRigOpts) *landingRig {
	t.Helper()
	if o.checkRuns == nil {
		o.checkRuns = []gh.CheckRun{{Name: "test", Status: "completed", Conclusion: "success"}}
	}
	if o.headNow == "" {
		o.headNow = "sha1"
	}
	r := &landingRig{t: t, states: states, mergeErrs: mergeErrs}
	r.client = &mockGitHubClient{
		listPRsFn: func(owner, repo string) ([]gh.PRDetails, error) {
			return []gh.PRDetails{{
				Number: 100, State: "open", Draft: true, HeadSHA: "sha1",
				HeadRefName: "fabrik/merge-train/trial-1",
				Body:        mergeTrainBatchMarker + "\nCloses #1\nCloses #2",
			}}, nil
		},
		fetchPRDetailsFn: func(owner, repo string, n int) (*gh.PRDetails, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			i := r.reads
			if i >= len(r.states) {
				i = len(r.states) - 1
			}
			r.reads++
			return &gh.PRDetails{Number: n, State: "open", HeadSHA: o.headNow, MergeableState: r.states[i]}, nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) { return o.checkRuns, nil },
		mergePRFn: func(owner, repo string, n int) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.events = append(r.events, "merge")
			if len(r.mergeErrs) == 0 {
				return nil
			}
			err := r.mergeErrs[0]
			r.mergeErrs = r.mergeErrs[1:]
			if err == nil {
				return nil
			}
			return err
		},
		fetchCommitsBehindFn: func(owner, repo, base, head string) (int, error) { return o.behind, nil },
		addCommentFn: func(owner, repo string, n int, body string) (int, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.comments = append(r.comments, body)
			return 1, nil
		},
		closeIssueFn: func(owner, repo string, n int) error { return nil },
	}
	r.wm = NewWorktreeManager(t.TempDir())
	r.eng = trainTestEngine(t, r.client, &mockClaudeInvoker{}, r.wm)
	r.eng.SetTrainCIPollIntervalForTest(time.Millisecond)
	r.eng.SetLandingBlockedBoundForTest(40 * time.Millisecond)
	r.eng.cfg.CIBackstopTimeout = 2 * time.Second
	r.eng.SetLandingCleanupObserverForTest(func(name string, deletedBranch bool) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.events = append(r.events, fmt.Sprintf("release(delete=%v)", deletedBranch))
		r.deleted = append(r.deleted, deletedBranch)
	})
	return r
}

func (r *landingRig) failureComments() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.comments {
		if strings.Contains(c, "fabrik:"+landingFailureMarker) {
			n++
		}
	}
	return n
}

func (r *landingRig) mergeCalls() int {
	r.client.mu.Lock()
	defer r.client.mu.Unlock()
	return len(r.client.mergePRCalls)
}

func (r *landingRig) eventLog() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.events, ",")
}

func (r *landingRig) lastDeleted() (deleted bool, released bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.deleted) == 0 {
		return false, false
	}
	return r.deleted[len(r.deleted)-1], true
}

// landingPath runs one landing attempt through a real landing function.
type landingPath struct {
	name string
	run  func(ctx context.Context, r *landingRig, survivors []trainMember)
}

var landingPaths = []landingPath{
	{"batch", func(ctx context.Context, r *landingRig, survivors []trainMember) {
		state := &mergeTrainWorkerState{trialName: "trial-1", projectID: "PVT_test"}
		r.eng.landMergeTrainBatch(ctx, state, "owner", "repo", "main", mergeTrainKey("owner/repo", "main"), survivors, r.wm)
	}},
	{"singleton", func(ctx context.Context, r *landingRig, survivors []trainMember) {
		state := &mergeTrainWorkerState{projectID: "PVT_test"}
		p := trialParams{owner: "owner", repo: "repo", baseBranch: "main", wm: r.wm, holdingStg: holdingStage(r.eng.cfg)}
		r.eng.landSingleton(ctx, state, p, survivors[0], "trial-1")
	}},
}

func landingSurvivors() []trainMember {
	return []trainMember{makeQueuedMember(1, 10, "Issue One"), makeQueuedMember(2, 11, "Issue Two")}
}

func forEachLandingPath(t *testing.T, fn func(t *testing.T, lp landingPath)) {
	for _, lp := range landingPaths {
		lp := lp
		t.Run(lp.name, func(t *testing.T) { fn(t, lp) })
	}
}

// A transient blocked with all checks green, then clean: the same PR merges once, with no
// comment and no release of the trial before the merge.
func TestLanding_TransientBlockedThenClean_MergesSamePR(t *testing.T) {
	forEachLandingPath(t, func(t *testing.T, lp landingPath) {
		r := newLandingRig(t, []string{"blocked", "blocked", "clean"}, nil, landingRigOpts{})
		lp.run(context.Background(), r, landingSurvivors())

		if n := r.mergeCalls(); n != 1 {
			t.Fatalf("MergePR calls = %d, want exactly 1 (events: %s)", n, r.eventLog())
		}
		if n := r.failureComments(); n != 0 {
			t.Errorf("failure comments = %d, want 0 for a landing that only waited", n)
		}
		if ev := r.eventLog(); !strings.HasPrefix(ev, "merge,") {
			t.Errorf("trial was released before the merge: %s", ev)
		}
		if r.reads < 3 {
			t.Errorf("FetchPRDetails reads = %d, want >= 3 — the blocked window was never observed", r.reads)
		}
		if del, ok := r.lastDeleted(); !ok || !del {
			t.Errorf("a merged landing must still delete the trial branch as before (released=%v delete=%v)", ok, del)
		}
	})
}

// A merge refused once as not-CI-clean (or not yet computed) is retried on the same PR.
func TestLanding_MergeRefusedOnce_RetriesSamePR(t *testing.T) {
	for _, refusal := range []error{gh.ErrNotMergeableCI, gh.ErrNotMergeable} {
		refusal := refusal
		t.Run(refusal.Error(), func(t *testing.T) {
			forEachLandingPath(t, func(t *testing.T, lp landingPath) {
				r := newLandingRig(t, []string{"clean"}, []error{fmt.Errorf("%w: mergeable_state=%q", refusal, "blocked"), nil}, landingRigOpts{})
				lp.run(context.Background(), r, landingSurvivors())

				if n := r.mergeCalls(); n != 2 {
					t.Fatalf("MergePR calls = %d, want 2 (refused once, then merged) (events: %s)", n, r.eventLog())
				}
				if n := r.failureComments(); n != 0 {
					t.Errorf("failure comments = %d, want 0", n)
				}
				if ev := r.eventLog(); !strings.HasPrefix(ev, "merge,merge,") {
					t.Errorf("trial released before the retried merge landed: %s", ev)
				}
			})
		})
	}
}

// blocked with a failed check is a landing failure, as before: no wait, nothing kept.
func TestLanding_BlockedWithFailedCheck_FailsAndCleansUp(t *testing.T) {
	forEachLandingPath(t, func(t *testing.T, lp landingPath) {
		r := newLandingRig(t, []string{"blocked"}, nil, landingRigOpts{
			checkRuns: []gh.CheckRun{{Name: "test", Status: "completed", Conclusion: "failure"}},
		})
		lp.run(context.Background(), r, landingSurvivors())

		if n := r.mergeCalls(); n != 0 {
			t.Errorf("MergePR calls = %d, want 0", n)
		}
		if del, ok := r.lastDeleted(); !ok || !del {
			t.Errorf("a red trial must be cleaned up as before (released=%v delete=%v)", ok, del)
		}
	})
}

func TestLanding_Dirty_FailsCleansUpAndCommentsOnce(t *testing.T) {
	forEachLandingPath(t, func(t *testing.T, lp landingPath) {
		r := newLandingRig(t, []string{"dirty"}, nil, landingRigOpts{})
		lp.run(context.Background(), r, landingSurvivors())

		if n := r.mergeCalls(); n != 0 {
			t.Errorf("MergePR calls = %d, want 0", n)
		}
		if del, _ := r.lastDeleted(); !del {
			t.Error("a conflicting trial must be cleaned up")
		}
		if n := r.failureComments(); n != 1 {
			t.Errorf("failure comments = %d, want 1", n)
		}
		assertFailureCommentSaysRemoved(t, r)
	})
}

// assertFailureCommentSaysRemoved checks the one failure comment tells the truth for a trial
// that was deleted: it must not claim the trial is kept and retried.
func assertFailureCommentSaysRemoved(t *testing.T, r *landingRig) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.comments) == 0 {
		t.Error("no failure comment posted")
		return
	}
	body := r.comments[0]
	if strings.Contains(body, "keeps a still-green trial") || !strings.Contains(body, "were removed") {
		t.Errorf("a removed trial's comment must say it was removed, not kept: %s", body)
	}
}

func TestLanding_HeadMoved_FailsAndCleansUp(t *testing.T) {
	forEachLandingPath(t, func(t *testing.T, lp landingPath) {
		r := newLandingRig(t, []string{"clean"}, nil, landingRigOpts{headNow: "sha-other"})
		lp.run(context.Background(), r, landingSurvivors())

		if n := r.mergeCalls(); n != 0 {
			t.Errorf("MergePR calls = %d, want 0 — a commit that was not validated must not merge", n)
		}
		if del, _ := r.lastDeleted(); !del {
			t.Error("a trial whose head moved must be cleaned up")
		}
		if n := r.failureComments(); n != 1 {
			t.Errorf("failure comments = %d, want 1", n)
		}
		assertFailureCommentSaysRemoved(t, r)
	})
}

// The short bound running out on a green, unchanged-base trial keeps the PR and branch, posts
// one comment, and a second attempt does not repost it.
func TestLanding_BlockedBoundExpires_KeepsTrialAndCommentsOnce(t *testing.T) {
	forEachLandingPath(t, func(t *testing.T, lp landingPath) {
		r := newLandingRig(t, []string{"blocked"}, nil, landingRigOpts{})
		survivors := landingSurvivors()
		lp.run(context.Background(), r, survivors)

		if n := r.mergeCalls(); n != 0 {
			t.Errorf("MergePR calls = %d, want 0 — the state never became mergeable", n)
		}
		if del, ok := r.lastDeleted(); !ok || del {
			t.Errorf("a green trial on an unchanged base must be kept (released=%v delete=%v)", ok, del)
		}
		if n := r.failureComments(); n != 1 {
			t.Fatalf("failure comments = %d, want 1", n)
		}
		body := r.comments[0]
		if !strings.Contains(body, "keeps a still-green trial") || strings.Contains(body, "were removed") {
			t.Errorf("a kept trial's comment must say it is kept, not removed: %s", body)
		}
		for _, want := range []string{"#100", "blocked", "merge failure"} {
			if !strings.Contains(body, want) {
				t.Errorf("failure comment lacks %q: %s", want, body)
			}
		}

		lp.run(context.Background(), r, survivors) // a later attempt on the same PR
		if n := r.failureComments(); n != 1 {
			t.Errorf("failure comments after a second attempt = %d, want still 1", n)
		}
	})
}

// The marker in the member's cached comments dedups across a restart (in-memory set empty).
func TestLanding_FailureCommentMarkerSurvivesRestart(t *testing.T) {
	r := newLandingRig(t, []string{"blocked"}, nil, landingRigOpts{})
	survivors := landingSurvivors()
	survivors[0].item.Comments = []gh.Comment{{Body: fmt.Sprintf("<!-- fabrik:%s pr=%d -->", landingFailureMarker, 100)}}
	landingPaths[0].run(context.Background(), r, survivors)
	if n := r.failureComments(); n != 0 {
		t.Errorf("failure comments = %d, want 0 when the marker is already on the member", n)
	}
}

func TestLanding_BaseMoved_BoundExpiryCleansUp(t *testing.T) {
	forEachLandingPath(t, func(t *testing.T, lp landingPath) {
		r := newLandingRig(t, []string{"blocked"}, nil, landingRigOpts{behind: 3})
		lp.run(context.Background(), r, landingSurvivors())
		if del, ok := r.lastDeleted(); !ok || !del {
			t.Errorf("once the base has moved the trial is cleaned up as before (released=%v delete=%v)", ok, del)
		}
	})
}

func TestLanding_Cancelled_KeepsTrialAndPostsNothing(t *testing.T) {
	forEachLandingPath(t, func(t *testing.T, lp landingPath) {
		r := newLandingRig(t, []string{"blocked"}, nil, landingRigOpts{})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		lp.run(ctx, r, landingSurvivors())
		if del, ok := r.lastDeleted(); !ok || del {
			t.Errorf("a cancelled landing must not destroy a green trial (released=%v delete=%v)", ok, del)
		}
		if n := r.failureComments(); n != 0 {
			t.Errorf("failure comments = %d, want 0 on cancellation", n)
		}
	})
}

func TestLanding_OtherMergeError_FailsKeepsTrialAndCommentsOnce(t *testing.T) {
	forEachLandingPath(t, func(t *testing.T, lp landingPath) {
		r := newLandingRig(t, []string{"clean"}, []error{fmt.Errorf("HTTP 500")}, landingRigOpts{})
		lp.run(context.Background(), r, landingSurvivors())
		if n := r.mergeCalls(); n != 1 {
			t.Errorf("MergePR calls = %d, want 1 — a non-transient error is not retried", n)
		}
		if n := r.failureComments(); n != 1 {
			t.Errorf("failure comments = %d, want 1", n)
		}
		if del, ok := r.lastDeleted(); !ok || del {
			t.Errorf("a green trial on an unchanged base is kept (released=%v delete=%v)", ok, del)
		}
	})
}

// A merge refusal for the whole bound ends the attempt; it never loops past the single bound.
func TestLanding_MergeRefusedForever_EndsAtBound(t *testing.T) {
	errs := make([]error, 100000)
	for i := range errs {
		errs[i] = fmt.Errorf("%w: mergeable_state=%q", gh.ErrNotMergeableCI, "blocked")
	}
	r := newLandingRig(t, []string{"clean"}, errs, landingRigOpts{})
	start := time.Now()
	landingPaths[0].run(context.Background(), r, landingSurvivors())
	if time.Since(start) > 1500*time.Millisecond {
		t.Errorf("a perpetually refused merge ran %v — the bound did not apply", time.Since(start))
	}
	if r.mergeCalls() < 2 {
		t.Errorf("MergePR calls = %d, want several retries on the same PR", r.mergeCalls())
	}
	if del, ok := r.lastDeleted(); !ok || del {
		t.Errorf("trial should be kept after the bound (released=%v delete=%v)", ok, del)
	}
}

// Pending checks are waited out under CIBackstopTimeout, not the short bound.
func TestLanding_PendingChecks_WaitPastShortBound(t *testing.T) {
	var mu sync.Mutex
	reads := 0
	r := newLandingRig(t, []string{"blocked", "blocked", "blocked", "blocked", "clean"}, nil, landingRigOpts{})
	r.client.fetchCheckRunsFn = func(owner, repo, sha string) ([]gh.CheckRun, error) {
		mu.Lock()
		defer mu.Unlock()
		reads++
		if reads < 120 { // well past the 40ms short bound at a 1ms poll
			return []gh.CheckRun{{Name: "test", Status: "in_progress"}}, nil
		}
		return []gh.CheckRun{{Name: "test", Status: "completed", Conclusion: "success"}}, nil
	}
	r.client.fetchPRDetailsFn = func(owner, repo string, n int) (*gh.PRDetails, error) {
		mu.Lock()
		state := "blocked"
		if reads >= 120 {
			state = "clean"
		}
		mu.Unlock()
		return &gh.PRDetails{Number: n, State: "open", HeadSHA: "sha1", MergeableState: state}, nil
	}
	landingPaths[0].run(context.Background(), r, landingSurvivors())
	if n := r.mergeCalls(); n != 1 {
		t.Errorf("MergePR calls = %d, want 1 — pending checks must be waited out", n)
	}
	if n := r.failureComments(); n != 0 {
		t.Errorf("failure comments = %d, want 0", n)
	}
}

// Non-vacuity: with the settle wait neutralised the original defect reproduces.
func TestLanding_SettleDisabled_ReproducesTheDefect(t *testing.T) {
	forEachLandingPath(t, func(t *testing.T, lp landingPath) {
		r := newLandingRig(t, []string{"blocked", "clean"}, []error{fmt.Errorf("%w: mergeable_state=%q", gh.ErrNotMergeableCI, "blocked"), nil}, landingRigOpts{})
		r.eng.SetLandingSettleDisabledForTest(true)
		lp.run(context.Background(), r, landingSurvivors())
		if n := r.mergeCalls(); n != 1 {
			t.Errorf("MergePR calls = %d, want 1 — the neutralised path merges at once and gives up", n)
		}
	})
}

func TestLanding_KeepDisabled_AlwaysDeletes(t *testing.T) {
	forEachLandingPath(t, func(t *testing.T, lp landingPath) {
		r := newLandingRig(t, []string{"blocked"}, nil, landingRigOpts{})
		r.eng.SetKeepGreenTrialDisabledForTest(true)
		lp.run(context.Background(), r, landingSurvivors())
		if del, ok := r.lastDeleted(); !ok || !del {
			t.Errorf("with keeping neutralised the trial is deleted (released=%v delete=%v)", ok, del)
		}
	})
}
