package sim

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/handarbeit/fabrik/engine"
)

// Webhook deliveries for a merge-train trial/integration PR must not attach it
// to a single member (#2483, ADR 2483). The sim has no webhook transport, so the
// scenario opens a real trial and drives the same pull_request / check_run
// payloads through the real board cache and Store. Each variant runs with the
// guards (the engine's behaviour) and with them neutralised through
// CacheImpl.SetPRHealGuardDisabledForTest, where it must fail.

type trainHealState struct {
	linkedPR  int
	headSHA   string
	hydrated  bool
	trialOpen int
}

func snapshotTrainHeal(t *testing.T, env *Env, members []int) map[int]trainHealState {
	t.Helper()
	out := make(map[int]trainHealState, len(members))
	for _, n := range members {
		st, ok := env.Engine.StoreItemForTest(env.OwnerRepo, n)
		if !ok {
			t.Fatalf("#%d is not in the Store", n)
		}
		s := trainHealState{hydrated: env.BoardCache.IsItemDeepFetched(env.OwnerRepo, n)}
		if st.LinkedPR != nil {
			s.linkedPR, s.headSHA = st.LinkedPR.Number, st.LinkedPR.HeadSHA
		}
		out[n] = s
	}
	return out
}

func trainPRDelivery(t *testing.T, repo string, prNum int, sha, ref string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"action":     "synchronize",
		"repository": map[string]any{"full_name": repo},
		"pull_request": map[string]any{
			"number": prNum, "title": "trial", "state": "open",
			"head": map[string]any{"sha": sha, "ref": ref},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func trainCheckRunDelivery(t *testing.T, repo string, prNum int, sha, ref string, withBranch bool) []byte {
	t.Helper()
	cr := map[string]any{"id": 4242, "name": "build", "status": "completed", "conclusion": "success", "head_sha": sha}
	if withBranch {
		cr["check_suite"] = map[string]any{"head_branch": ref}
		cr["pull_requests"] = []any{map[string]any{"number": prNum, "head": map[string]any{"ref": ref}}}
	}
	b, err := json.Marshal(map[string]any{
		"action": "completed", "repository": map[string]any{"full_name": repo}, "check_run": cr,
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestWebhookTrainPR_DeliveriesLeaveMembersUntouched(t *testing.T) {
	t.Parallel()
	for _, withBranch := range []bool{true, false} {
		for _, guards := range []bool{true, false} {
			withBranch, guards := withBranch, guards
			t.Run(fmt.Sprintf("branchFields=%v/guards=%v", withBranch, guards), func(t *testing.T) {
				t.Parallel()
				env := mergeTrainEnv(t, mergeTrainEnvOptions{AsyncTrain: true, BoardCache: true})
				var members []int
				for i, f := range []string{"a.txt", "b.txt", "c.txt"} {
					n, _ := QueueMember(t, env, fmt.Sprintf("heal-%d", i), map[string]string{f: "x\n"})
					members = append(members, n)
				}

				// No verdict is seeded: the trial opens and stays open.
				pollUntilRun(t, env, func(p []engine.TrainRunPhase) bool { return len(p) == 1 && p[0].TrialPR != 0 }, 15)
				trialPRs, heads := openTrialPRs(t, env)
				if len(trialPRs) != 1 {
					t.Fatalf("want one open trial PR, got %v", trialPRs)
				}
				prs, err := env.Sim.Sim().ListPRs(env.Owner, env.Repo)
				if err != nil {
					t.Fatal(err)
				}
				var sha string
				for _, pr := range prs {
					if pr.Number == trialPRs[0] {
						sha = pr.HeadSHA
					}
				}
				if sha == "" {
					t.Fatal("trial PR head SHA unknown")
				}

				env.BoardCache.SetPRHealGuardDisabledForTest(!guards)
				before := snapshotTrainHeal(t, env, members)

				// The trial PR names every member, so without the guards the heal
				// would attach it to one of them.
				ref := heads[0]
				if !withBranch {
					ref = "" // the multi-closing-issue refusal is then the only protection
				}
				env.BoardCache.ApplyDelta("pull_request", trainPRDelivery(t, env.OwnerRepo, trialPRs[0], sha, ref))
				env.BoardCache.ApplyDelta("check_run", trainCheckRunDelivery(t, env.OwnerRepo, trialPRs[0], sha, heads[0], withBranch))

				after := snapshotTrainHeal(t, env, members)
				changed := false
				for _, n := range members {
					if before[n] != after[n] {
						changed = true
						if guards {
							t.Errorf("#%d changed by trial-PR deliveries: before=%+v after=%+v", n, before[n], after[n])
						}
					}
				}
				if !guards {
					if !changed {
						t.Fatal("non-vacuity: with the guards off the deliveries must disturb a member")
					}
					return
				}

				// The trial stays the one trial; no further batch forms.
				drafts := draftPRCount(env)
				RunPolls(t, env, 3)
				if got := draftPRCount(env); got != drafts {
					t.Errorf("a new trial PR was opened after the deliveries (%d -> %d)", drafts, got)
				}
				if again, _ := openTrialPRs(t, env); len(again) != 1 || again[0] != trialPRs[0] {
					t.Errorf("open trial PRs = %v, want only #%d", again, trialPRs[0])
				}
			})
		}
	}
}
