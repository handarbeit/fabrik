package sim

import (
	"testing"
	"time"
	"fmt"
)

func TestZZDebug(t *testing.T) {
	env := NewEnv(t, EnvOptions{Stages: cacheFreshnessStages(), Yolo: boolPtr(false), StartTime: time.Now(), BoardCache: true})
	t.Cleanup(env.Engine.RegisterObservers())
	num := FileIssue(t, env, "dbg", "body", "Specify")
	WaitForIssueLabel(t, env, num, "stage:Specify:complete", 40)
	RunPolls(t, env, 10)
	for i := 0; i < 3; i++ {
		RunPoll(t, env)
		snap, _ := env.Engine.StoreForTest().Get(env.OwnerRepo, num)
		st := snap.State()
		probe, _, _ := env.Sim.Sim().ProbeProjectBoard(env.Owner, env.Repo, 1, "user")
		fmt.Printf("DBG store.UpdatedAt=%v L=%v probe=%v issueLayer=%v\n", st.UpdatedAt, st.LastSeenSourceUpdatedAt, probe[0].EffectiveUpdatedAt, probe[0].IssueLayerUpdatedAt)
	}
}
