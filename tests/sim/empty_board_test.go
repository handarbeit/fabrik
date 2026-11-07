package sim

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/stages"
)

// #1894: a freshly created board has zero items. The engine concludes it is
// empty after consecutive zero-item polls, logs exactly one line, and still
// discovers an item added later with no restart. The retry/quiet-mode half
// lives in the github client, which the sim does not model (see
// github/fetch_board_test.go); this pins the engine-side verdict.

func emptyBoardStages() []*stages.Stage {
	return []*stages.Stage{
		{Name: "Specify", Order: 1},
		{Name: "Done", Order: 2, CleanupWorktree: true},
	}
}

type logCapture struct {
	mu    sync.Mutex
	lines []string
}

func (c *logCapture) add(_ int, tag, msg string) {
	c.mu.Lock()
	c.lines = append(c.lines, "["+tag+"] "+msg)
	c.mu.Unlock()
}

func (c *logCapture) count(sub string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, l := range c.lines {
		if strings.Contains(l, sub) {
			n++
		}
	}
	return n
}

func TestEmptyBoard_ConcludedOnceThenItemDiscovered(t *testing.T) {
	t.Parallel()
	env := NewEnv(t, EnvOptions{
		Stages:     emptyBoardStages(),
		Yolo:       boolPtr(false),
		StartTime:  time.Now(),
		BoardCache: true,
	})
	t.Cleanup(env.Engine.RegisterObservers())
	logs := &logCapture{}
	env.Engine.SetLogSinkForTest(logs.add)

	RunPolls(t, env, 6)
	if n := logs.count("board is empty — add issues to the board to start"); n != 1 {
		t.Fatalf("empty-board line logged %d times over 6 polls, want exactly 1", n)
	}
	if n := logs.count("indexer hiccup") + logs.count("deferring bootstrap"); n != 0 {
		t.Errorf("%d retry/deferral warnings on an empty board, want 0", n)
	}
	if !env.Engine.BoardConcludedEmptyForTest() {
		t.Fatal("engine did not conclude the board is empty")
	}

	// An item added later is picked up on the normal schedule, no restart.
	num := FileIssue(t, env, "first item", "body", "Specify")
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	WaitForIssueLabel(t, env, num, "stage:Specify:complete", 40)
	if env.Engine.BoardConcludedEmptyForTest() {
		t.Error("board still concluded empty after an item was added")
	}
}
