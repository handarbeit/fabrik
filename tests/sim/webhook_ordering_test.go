package sim

import (
	"fmt"
	"testing"
	"time"
)

// The observed-at ordering rule (#2348, ADR 2348), end to end through the real
// webhook decode, the real boardcache and the real Store. Each scenario runs
// twice: with the rule (the engine's behaviour) and with it neutralised through
// Engine.SetObservedOrderingDisabledForTest, where it must fail — the
// non-vacuity check (FR-016, SC-007).
//
// Webhook payloads carry wall-clock GitHub times (the Store compares them with
// listing fetch starts, which are wall-clock too); env.Clock is virtual and is
// not involved.

// orderingEnv returns an Env with the board cache, one parked item and the
// store populated by the first polls. Parked (paused) so no stage work races the
// scenario.
func orderingEnv(t *testing.T, rule bool, labels ...string) (*Env, int, string) {
	t.Helper()
	env := NewEnv(t, EnvOptions{Stages: smokeStages(), BoardCache: true})
	env.Engine.SetObservedOrderingDisabledForTest(!rule)
	num := FileIssue(t, env, "ordering", "body", "Specify", append([]string{"fabrik:paused", "fabrik:awaiting-input"}, labels...)...)
	RunPolls(t, env, 3)
	return env, num, projectItem(t, env, num).ItemID
}

func iso(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// A webhook-applied column move survives the probe and the reconcile listings
// that still show the old column (§2.1): no drift is declared, nothing reverts.
func TestWebhookOrdering_MoveSurvivesLaggingListing(t *testing.T) {
	t.Parallel()
	for _, rule := range []bool{true, false} {
		rule := rule
		t.Run(fmt.Sprintf("rule=%v", rule), func(t *testing.T) {
			t.Parallel()
			env, num, itemID := orderingEnv(t, rule)
			projectID := env.BoardCache.ProjectID()
			sf, err := env.Sim.Sim().FetchStatusField(projectID)
			if err != nil {
				t.Fatalf("FetchStatusField: %v", err)
			}
			const from, to = "Specify", "Research"

			// A human moves the card in the UI: GitHub has it in "Research".
			if err := env.Sim.Sim().UpdateProjectItemStatus(projectID, itemID, sf.FieldID, sf.Options[to]); err != nil {
				t.Fatalf("move: %v", err)
			}
			// The board listings lag: bulk reads still report the old column.
			env.Sim.Sim().LagBoardStatus(itemID, from, []string{"fabrik:paused", "fabrik:awaiting-input"})

			// The webhook arrives within a second.
			env.BoardCache.ApplyDelta("projects_v2_item", []byte(fmt.Sprintf(
				`{"action":"edited","sender":{"login":"human"},`+
					`"projects_v2_item":{"node_id":%q,"project_node_id":%q,"updated_at":%q},`+
					`"changes":{"field_value":{"field_node_id":%q,"field_type":"single_select","to":{"id":%q,"name":%q}}}}`,
				itemID, projectID, iso(time.Now()), sf.FieldID, sf.Options[to], to)))
			if got := statusOf(t, env, num); got != to {
				t.Fatalf("precondition: webhook did not apply, Status = %q", got)
			}

			// The reconcile listing is fetched from the lagging read.
			drift, _, board, err := env.BoardCache.LightReconcile(env.Owner, env.Repo, env.ProjectNum, env.cfg.OwnerType)
			if err != nil {
				t.Fatalf("LightReconcile: %v", err)
			}
			if rule && drift != 0 {
				t.Fatalf("the webhook value read as drift against an older listing (drift=%d)", drift)
			}
			env.BoardCache.Reconcile(board)
			// The reconcile applied the listing the lagging read produced.
			if got := statusOf(t, env, num); !rule {
				if got != from {
					t.Fatalf("non-vacuity: with the rule off the lagging listing must revert the move, Status = %q", got)
				}
				return
			} else if got != to {
				t.Fatalf("a lagging reconcile listing reverted the webhook move: Status = %q, want %q", got, to)
			}

			// The probe and the status sweep of the next polls, with the batch read
			// still lagging.
			RunPolls(t, env, 2)
			if got := statusOf(t, env, num); got != to {
				t.Fatalf("a poll's lagging read reverted the webhook move: Status = %q, want %q", got, to)
			}
		})
	}
}

// A late webhook echo of an earlier engine write does not bring back a label the
// engine has since removed (§2.2).
func TestWebhookOrdering_LateEchoDoesNotResurrectLabel(t *testing.T) {
	t.Parallel()
	for _, rule := range []bool{true, false} {
		rule := rule
		t.Run(fmt.Sprintf("rule=%v", rule), func(t *testing.T) {
			t.Parallel()
			const label = "wip-ordering"
			env, num, _ := orderingEnv(t, rule, label)
			key := fmt.Sprintf("%s#%d", env.OwnerRepo, num)

			// The engine removes the label (write-through to the cache).
			env.BoardCache.ApplyLabelRemoved(key, label)
			if hasLabel(labelsOf(t, env, num), label) {
				t.Fatal("precondition: label still present after the engine's removal")
			}

			// The echo of the engine's earlier add arrives late, carrying the time of
			// that earlier event.
			env.BoardCache.ApplyDelta("issues", []byte(fmt.Sprintf(
				`{"action":"labeled","sender":{"login":"fabrik-bot"},"label":{"name":%q},`+
					`"issue":{"number":%d,"updated_at":%q},"repository":{"full_name":%q}}`,
				label, num, iso(time.Now().Add(-5*time.Second)), env.OwnerRepo)))

			got := hasLabel(labelsOf(t, env, num), label)
			if rule && got {
				t.Fatal("a late echo re-added the label the engine removed")
			}
			if !rule && !got {
				t.Fatal("non-vacuity: with the rule off the late echo must re-add the label")
			}
		})
	}
}

// A replayed pull_request event for an earlier head does not move the head back,
// and the newer head's check runs survive (§2.3).
func TestWebhookOrdering_ReplayedSynchronizeDoesNotRegressHead(t *testing.T) {
	t.Parallel()
	for _, rule := range []bool{true, false} {
		rule := rule
		t.Run(fmt.Sprintf("rule=%v", rule), func(t *testing.T) {
			t.Parallel()
			env, num, _ := orderingEnv(t, rule)
			const pr = 4242
			const shaA, shaB = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			env.BoardCache.RecordPRLinkage(env.OwnerRepo, pr, num)

			sync := func(sha string, at time.Time) {
				env.BoardCache.ApplyDelta("pull_request", []byte(fmt.Sprintf(
					`{"action":"synchronize","pull_request":{"number":%d,"title":"t","state":"open","merged":false,"draft":false,`+
						`"updated_at":%q,"head":{"sha":%q,"ref":"fabrik/issue-%d"}},"repository":{"full_name":%q}}`,
					pr, iso(at), sha, num, env.OwnerRepo)))
			}
			t1, t2 := time.Now().Add(-2*time.Minute), time.Now().Add(-time.Minute)
			sync(shaA, t1)
			sync(shaB, t2)
			env.BoardCache.ApplyDelta("check_run", []byte(fmt.Sprintf(
				`{"action":"completed","check_run":{"id":1,"name":"ci","status":"completed","conclusion":"success","head_sha":%q},`+
					`"repository":{"full_name":%q}}`, shaB, env.OwnerRepo)))

			sync(shaA, t1) // the replay

			st, _ := env.Engine.StoreItemForTest(env.OwnerRepo, num)
			head, runs := "", 0
			if st.LinkedPR != nil {
				head, runs = st.LinkedPR.HeadSHA, len(st.LinkedPR.CheckRuns)
			}
			if rule && (head != shaB || runs != 1) {
				t.Fatalf("replay regressed the head: head=%q check runs=%d, want %q and 1", head, runs, shaB)
			}
			if !rule && (head != shaA || runs != 0) {
				t.Fatalf("non-vacuity: with the rule off the replay must regress the head and wipe the runs, got head=%q runs=%d", head, runs)
			}
		})
	}
}

func statusOf(t *testing.T, env *Env, num int) string {
	t.Helper()
	st, ok := env.Engine.StoreItemForTest(env.OwnerRepo, num)
	if !ok {
		t.Fatalf("#%d not in the store", num)
	}
	return st.Status
}

func labelsOf(t *testing.T, env *Env, num int) []string {
	t.Helper()
	st, ok := env.Engine.StoreItemForTest(env.OwnerRepo, num)
	if !ok {
		t.Fatalf("#%d not in the store", num)
	}
	return st.Labels
}
