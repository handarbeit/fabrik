package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

// The worker handle outlives the Claude process (result handling, pushes, later
// work), so the invocation's turn count and budget must be forgotten when it
// ends rather than reported as live (#2121 R6).
func TestRunClaudeForgetsLiveTurnsWhenTheInvocationEnds(t *testing.T) {
	t.Chdir(t.TempDir())
	binDir := t.TempDir()
	script := `#!/bin/sh
cat >/dev/null
printf '%s\n' '{"type":"user"}'
printf '%s\n' '{"result":"done\nFABRIK_STAGE_COMPLETE\n","session_id":"sess_turns_end","num_turns":1,"total_cost_usd":0.0}'
`
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

	e := workerEngine(t)
	e.seedAPIItem(t, 11, "Implement")
	e.store.Apply(itemstate.WorkerEntered{Repo: "owner/repo", Number: 11, StageName: "Implement", StartedAt: time.Now(),
		Kind: itemstate.WorkerKindStageRun})

	var sawTurn bool
	rec := e.liveTurnRecorder("owner/repo", 11)
	onTurn := func(turns, budget int) {
		if turns > 0 {
			sawTurn = true
		}
		rec(turns, budget)
	}
	stage := &stages.Stage{Name: "Implement", Prompt: "go", Completion: stages.CompletionCriteria{Type: "claude"}}
	issue := gh.ProjectItem{Number: 11, Repo: "owner/repo", Title: "turns end"}
	if _, _, _, err := InvokeClaude(context.Background(), stage, issue, nil, false, t.TempDir(), InvokeOptions{OnTurn: onTurn}); err != nil {
		t.Fatalf("InvokeClaude: %v", err)
	}
	if !sawTurn {
		t.Fatal("the fake invocation recorded no turn; the test would be vacuous")
	}
	st := status(t, e, "11")
	if st.Worker.TurnsUsed.Valid || st.Worker.MaxTurns.Valid {
		t.Errorf("after the invocation ended turns %+v max %+v, want both unknown", st.Worker.TurnsUsed, st.Worker.MaxTurns)
	}
}
