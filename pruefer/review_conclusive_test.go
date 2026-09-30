package pruefer

import (
	"context"
	"errors"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

// #1952 R3: ReviewOutcome.Conclusive gates memoisation. These tests pin both
// halves — every settled review/skip is conclusive, and every failure or
// swallowed-error path is not.

func runReviewPR(t *testing.T, client GitHubReviewer, claude ClaudeInvoker, cfg Config, pr gh.PRDetails, cloneErr error) ReviewOutcome {
	t.Helper()
	clone, _ := fakeClone(t, cloneErr)
	if claude == nil {
		claude = &mockClaudeInvoker{fn: func(ReviewRequest) (ReviewResult, error) { return ReviewResult{Text: "ok"}, nil }}
	}
	return ReviewPR(context.Background(), client, claude, clone, cfg, "pruefer-bot[bot]", "owner", "repo", pr, nil)
}

func TestReviewPR_Conclusive_SettledOutcomes(t *testing.T) {
	pr := gh.PRDetails{Number: 1, Author: "alice", HeadSHA: "sha1", BaseRef: "main"}
	cases := map[string]func() (*fakeReviewer, Config, gh.PRDetails){
		"reviewed": func() (*fakeReviewer, Config, gh.PRDetails) { return newFakeReviewer(), Config{}, pr },
		"draft": func() (*fakeReviewer, Config, gh.PRDetails) {
			p := pr
			p.Draft = true
			return newFakeReviewer(), Config{}, p
		},
		"already reviewed": func() (*fakeReviewer, Config, gh.PRDetails) {
			c := newFakeReviewer()
			c.reviews = []gh.PRReview{{Author: "pruefer-bot[bot]", CommitID: "sha1"}}
			return c, Config{}, pr
		},
		"cadence on-request": func() (*fakeReviewer, Config, gh.PRDetails) {
			return newFakeReviewer(), Config{Cadence: CadenceOnRequest}, pr
		},
		"excluded author": func() (*fakeReviewer, Config, gh.PRDetails) {
			return newFakeReviewer(), Config{ExcludedAuthors: []string{"alice"}}, pr
		},
		"diff too large (deterministic size verdict)": func() (*fakeReviewer, Config, gh.PRDetails) {
			c := newFakeReviewer()
			c.diff = "diff --git a/x b/x\n+++ b/x\n+x diff content\n"
			return c, Config{MaxDiffBytes: 5}, pr
		},
		"invalid repo config content (deterministic, not a fetch failure)": func() (*fakeReviewer, Config, gh.PRDetails) {
			c := newFakeReviewer()
			c.repoConfigData = []byte("{{{ not yaml")
			p := pr
			p.Draft = true
			return c, Config{}, p
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			client, cfg, p := mk()
			out := runReviewPR(t, client, nil, cfg, p, nil)
			if out.Err != nil || !(out.Reviewed || out.Skipped) {
				t.Fatalf("setup: outcome = %+v, want a clean review or skip", out)
			}
			if !out.Conclusive {
				t.Errorf("outcome %+v should be Conclusive", out)
			}
		})
	}
}

func TestReviewPR_Conclusive_FalseOnErrors(t *testing.T) {
	pr := gh.PRDetails{Number: 1, Author: "alice", HeadSHA: "sha1", BaseRef: "main"}
	t.Run("claude failure", func(t *testing.T) {
		claude := &mockClaudeInvoker{fn: func(ReviewRequest) (ReviewResult, error) { return ReviewResult{}, errors.New("boom") }}
		out := runReviewPR(t, newFakeReviewer(), claude, Config{}, pr, nil)
		if out.Err == nil || out.Conclusive {
			t.Errorf("outcome = %+v, want Err and !Conclusive", out)
		}
	})
	t.Run("clone failure", func(t *testing.T) {
		out := runReviewPR(t, newFakeReviewer(), nil, Config{}, pr, errors.New("clone boom"))
		if out.Err == nil || out.Conclusive {
			t.Errorf("outcome = %+v, want Err and !Conclusive", out)
		}
	})
	t.Run("submit failure", func(t *testing.T) {
		c := newFakeReviewer()
		c.submitErr = errors.New("submit boom")
		out := runReviewPR(t, c, nil, Config{}, pr, nil)
		if out.Err == nil || out.Conclusive {
			t.Errorf("outcome = %+v, want Err and !Conclusive", out)
		}
	})
	t.Run("reviews fetch failure", func(t *testing.T) {
		c := newFakeReviewer()
		c.reviewsErr = errors.New("reviews boom")
		out := runReviewPR(t, c, nil, Config{}, pr, nil)
		if out.Err == nil || out.Conclusive {
			t.Errorf("outcome = %+v, want Err and !Conclusive", out)
		}
	})
	t.Run("diff fetch failure", func(t *testing.T) {
		c := newFakeReviewer()
		c.diffErr = errors.New("diff boom")
		out := runReviewPR(t, c, nil, Config{}, pr, nil)
		if out.Err == nil || out.Conclusive {
			t.Errorf("outcome = %+v, want Err and !Conclusive", out)
		}
	})
}

// The swallowed-error cases: each yields Err == nil and a Skipped/Reviewed
// outcome that looks settled, but was reached through a degraded read.
func TestReviewPR_Conclusive_FalseOnSwallowedErrors(t *testing.T) {
	pr := gh.PRDetails{Number: 1, Author: "alice", HeadSHA: "sha1", BaseRef: "main"}

	t.Run("comment fetch failure behind a cadence on-request skip", func(t *testing.T) {
		c := newFakeReviewer()
		c.fetchErr = errors.New("comments boom")
		out := runReviewPR(t, c, nil, Config{Cadence: CadenceOnRequest}, pr, nil)
		if !out.Skipped || out.Reason != SkipCadenceOnRequest || out.Err != nil {
			t.Fatalf("setup: outcome = %+v", out)
		}
		if out.Conclusive {
			t.Error("a skip decided on a failed /pruefer review lookup must not be Conclusive")
		}
	})
	t.Run("repo config transport failure behind a skip", func(t *testing.T) {
		c := newFakeReviewer()
		c.repoConfigErr = errors.New("contents API 502")
		p := pr
		p.Draft = true
		out := runReviewPR(t, c, nil, Config{}, p, nil)
		if !out.Skipped || out.Err != nil {
			t.Fatalf("setup: outcome = %+v", out)
		}
		if out.Conclusive {
			t.Error("a skip decided while the repo config could not be read must not be Conclusive")
		}
	})
	t.Run("files-API fallback failure", func(t *testing.T) {
		c := newFakeReviewer()
		c.diffErr = wrapDiffTooLarge()
		c.filesErr = errors.New("files boom")
		out := runReviewPR(t, c, nil, Config{}, pr, nil)
		if !out.Skipped || out.Reason != SkipDiffTooLarge || out.Err != nil {
			t.Fatalf("setup: outcome = %+v", out)
		}
		if out.Conclusive {
			t.Error("a transient files-API failure must not be Conclusive")
		}
	})
	t.Run("diff-too-large notice post failure", func(t *testing.T) {
		c := newFakeReviewer()
		c.diff = "diff --git a/x b/x\n+++ b/x\n+x diff content\n"
		c.addErr = errors.New("comment boom")
		out := runReviewPR(t, c, nil, Config{MaxDiffBytes: 5}, pr, nil)
		if !out.Skipped || out.Reason != SkipDiffTooLarge || out.Err != nil {
			t.Fatalf("setup: outcome = %+v", out)
		}
		if out.Conclusive {
			t.Error("a skip whose one-time notice could not be posted must not be Conclusive: nothing would retry the notice")
		}
	})
	for _, failing := range []string{"eyes", "rocket"} {
		t.Run("force-review "+failing+" reaction failure", func(t *testing.T) {
			c := newFakeReviewer()
			c.comments = []gh.Comment{{DatabaseID: 42, Body: "/pruefer review"}}
			out := runReviewPR(t, &reactionFailingReviewer{fakeReviewer: c, failContent: failing}, nil, Config{}, pr, nil)
			if !out.Reviewed {
				t.Fatalf("setup: outcome = %+v", out)
			}
			if out.Conclusive {
				t.Errorf("a review whose /pruefer review %s reaction failed must not be Conclusive", failing)
			}
		})
	}
}

func wrapDiffTooLarge() error {
	return errors.Join(errors.New("406 too_large"), gh.ErrDiffTooLarge)
}

// reactionFailingReviewer fails AddCommentReaction for one reaction content
// only, so the acknowledge (eyes) and mark-processed (rocket) failures can be
// pinned independently.
type reactionFailingReviewer struct {
	*fakeReviewer
	failContent string
}

func (r *reactionFailingReviewer) AddCommentReaction(owner, repo string, id int, content string) error {
	if content == r.failContent {
		return errors.New("react boom")
	}
	return r.fakeReviewer.AddCommentReaction(owner, repo, id, content)
}
