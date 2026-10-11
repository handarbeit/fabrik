package sim

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tests/sim/simclaude"
)

// #2180 sim scenarios: an external change must not be masked by the engine's
// own self-write (SelfWriteObserved, #1090). The mask is the engine-side
// freshness rule over the board cache, so BoardCache is required — against the
// live adapter every read is already fresh and nothing can be masked.
//
// The window is the one production hits: a worker runs for minutes, a human
// comments while it runs, and the stage's completion writes (labels, the stage
// comment) then advance the issue-layer baseline past the human comment's
// updatedAt. Without a webhook nothing else tells the cache, so before #2180 the
// comment was invisible until a restart.

func cacheFreshnessStages() []*stages.Stage {
	return []*stages.Stage{
		{Name: "Specify", Order: 1},
		{Name: "Done", Order: 2, CleanupWorktree: true},
	}
}

const cacheFreshnessHumanComment = "actually, please also cover the empty-input case"

// commentProbe records every comment-processing invocation's comment bodies.
type commentProbe struct {
	mu     sync.Mutex
	bodies [][]string
}

func (p *commentProbe) script() simclaude.CommentScript {
	return func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		var bs []string
		for _, c := range comments {
			bs = append(bs, c.Body)
		}
		p.mu.Lock()
		p.bodies = append(p.bodies, bs)
		p.mu.Unlock()
		return simclaude.DefaultCommentScript(ctx, stage, issue, comments, workDir, opts)
	}
}

func (p *commentProbe) saw(substr string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, bs := range p.bodies {
		for _, b := range bs {
			if strings.Contains(b, substr) {
				return true
			}
		}
	}
	return false
}

// A human comment posted while the stage runs, then the engine's completion
// writes: the dispatcher still sees the comment within a bounded number of
// polls, with no restart.
//
// The #1090 no-refetch-loop guarantee (FR-007) is pinned where the self-writes
// can be driven directly: engine/cache_freshness_test.go
// (TestCacheFreshness_SelfWritesAlone_NoRefetchLoop) and
// boardcache/freshness_test.go. The sim's idle polls refetch on the periodic
// re-evaluation for reasons that predate and are independent of #2180.
func TestCacheFreshness_HumanCommentDuringStageRun_SeenWithoutRestart(t *testing.T) {
	t.Parallel()
	env := NewEnv(t, EnvOptions{
		Stages:     cacheFreshnessStages(),
		Yolo:       boolPtr(false),
		StartTime:  time.Now(),
		BoardCache: true,
	})
	t.Cleanup(env.Engine.RegisterObservers())

	probe := &commentProbe{}
	env.Claude.ForStageComments("Specify", probe.script())
	var once sync.Once
	var num int
	env.Claude.ForStage("Specify", func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, nc []gh.Comment, resume bool, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		once.Do(func() { env.Sim.Sim().SeedComment(env.OwnerRepo, issue.Number, "a-human-operator", cacheFreshnessHumanComment) })
		return simclaude.DefaultScript(ctx, stage, issue, nc, resume, workDir, opts)
	})

	num = FileIssue(t, env, "sim cache freshness (#2180 repro)", "body", "Specify")
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	WaitForIssueLabel(t, env, num, "stage:Specify:complete", 40)
	AdvanceUntil(t, env, func(*Env) bool { return probe.saw(cacheFreshnessHumanComment) }, 40)
}
