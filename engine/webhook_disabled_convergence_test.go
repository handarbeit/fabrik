package engine

import (
	"context"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/boardcache"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// TestWebhooksDisabled_ConvergesAcrossDriftCategories is the #957 invariant
// guard: with webhooks fully disabled (cfg.Webhooks == false, nil webhook
// manager), the engine must still converge on cached-state drift via poll
// alone, across every correctness-critical category this audit confirmed a
// live repro for. Per ADR 003, webhooks are an optimization (latency/API
// spend) — never a correctness requirement — and this test is the guard that
// enforces that architectural invariant going forward, not just for the one
// category #955 originally caught.
//
// Categories covered here:
//   - labels (#955): reconcileLoop repairs a drifted fabrik-managed label
//     even though it is driven by the same ticker that runs regardless of wm.
//   - blockedBy (#977): checkDependencies self-heals a stale cached edge list
//     via a targeted live re-read, gated on the existing dep-blocked cooldown
//     rather than any webhook-delivered delta.
//
// comments (#982) is already covered by TestPreImplement_RecoversMissingPlanComment
// and friends in engine/spawn_test.go — not duplicated here.
func TestWebhooksDisabled_ConvergesAcrossDriftCategories(t *testing.T) {
	t.Run("LabelDrift", func(t *testing.T) {
		t1 := time.Now().Truncate(time.Second)
		client := &mockGitHubClient{}
		eng := testEngine(t, client, &mockClaudeInvoker{})
		eng.cfg.Webhooks = false
		eng.cfg.ReconcileInterval = 10 * time.Millisecond

		cache := boardcache.NewCacheImpl(client, eng.store, func(string, ...any) {})
		// Seed #1 at Validate WITHOUT fabrik:awaiting-ci — the drifted store state.
		testBootstrapFromBoard(cache, &gh.ProjectBoard{
			ProjectID: "PVT_1",
			Items: []gh.ProjectItem{
				{ID: "I_1", ItemID: "PVTI_1", Number: 1, Repo: "owner/repo", Status: "Validate", Labels: []string{"fabrik:cruise"}, UpdatedAt: t1},
			},
		})
		eng.readClient = cache

		// GitHub's fresh board: same status + updatedAt, but WITH fabrik:awaiting-ci.
		// Only the fabrik-managed label differs — updatedAt never advances for a
		// pure label mutation in this scenario, exactly like the #955 stranding case.
		client.fetchProjectBoardFn = func(_, _ string, _ int, _ string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{
				ProjectID: "PVT_1",
				Items: []gh.ProjectItem{
					{ID: "I_1", ItemID: "PVTI_1", Number: 1, Repo: "owner/repo", Status: "Validate", Labels: []string{"fabrik:cruise", "fabrik:awaiting-ci"}, UpdatedAt: t1},
				},
			}, nil
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		// eng.webhookMgr is nil (never assigned) and cfg.Webhooks is false —
		// the whole point: convergence must not depend on either.
		go eng.reconcileLoop(ctx, cache, eng.webhookMgr)

		deadline := time.Now().Add(2 * time.Second)
		for {
			snap, err := eng.store.Get("owner/repo", 1)
			if err == nil {
				for _, l := range snap.Labels() {
					if l == "fabrik:awaiting-ci" {
						return // converged: reconcile synced the gate label with webhooks disabled
					}
				}
			}
			if time.Now().After(deadline) {
				var labels []string
				if err == nil {
					labels = snap.Labels()
				}
				t.Fatalf("label drift did not converge within 2s with webhooks disabled; labels = %v", labels)
			}
			time.Sleep(10 * time.Millisecond)
		}
	})

	t.Run("BlockedByDrift", func(t *testing.T) {
		// Once the live re-read clears the stale edge, checkDependencies stops
		// suppressing the item and processItem continues on into real stage
		// dispatch (worktree setup + Claude invocation) — so this subtest needs
		// an actual local git repo and a Claude mock that completes cleanly,
		// unlike the unit-level checkDependencies tests in dependencies_test.go.
		skipIfNoGit(t)
		repoDir := initBareRepo(t)

		client := &mockGitHubClient{
			// The live API no longer reports any open dependency — the removal
			// happened without bumping the item's updatedAt (#977), so a
			// cache-served deep-fetch would still show the stale edge. The raw,
			// uncached FetchItemDetails call checkDependencies makes on the
			// recheck path is the only thing that can observe this.
			fetchItemDetailsFn: func(item *gh.ProjectItem) error {
				item.BlockedBy = nil
				return nil
			},
		}
		claude := &mockClaudeInvoker{
			invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
				return "FABRIK_STAGE_COMPLETE\n", true, TokenUsage{TurnsUsed: 1}, nil
			},
		}
		eng := NewWithDeps(
			Config{
				Owner:         "owner",
				Repo:          "repo",
				ProjectNum:    1,
				User:          "testuser",
				Token:         "token",
				MaxConcurrent: 5,
				Webhooks:      false,
				Stages:        testStages(),
			},
			client,
			claude,
			NewWorktreeManager(repoDir),
		)
		// webhookMgr is left nil (never started) — webhooks disabled end to end.

		board := &gh.ProjectBoard{ProjectID: "PVT_1"}
		item := gh.ProjectItem{
			Number: 977,
			Title:  "Blocked item",
			Status: "Research",
			Repo:   "owner/repo",
			Labels: []string{"fabrik:blocked"},
			// Stale cached edge, mirroring the live #977 repro: the API no
			// longer has this dependency, but the item's own updatedAt never
			// advanced, so nothing invalidated the cached BlockedBy.
			BlockedBy: []gh.Dependency{
				{Number: 976, State: "OPEN", Repo: "owner/repo"},
			},
		}

		// This poll-path entry point (processItem -> checkDependencies) is
		// synchronous: unlike the reconcile ticker above, the live re-read
		// fires inline on the recheck call, so a single dispatch converges
		// deterministically — no polling loop needed to observe it.
		if err := eng.processItem(context.Background(), board, item); err != nil {
			t.Fatalf("processItem: %v", err)
		}
		removed := false
		for _, call := range client.removeLabelCalls {
			if call.labelName == "fabrik:blocked" {
				removed = true
				break
			}
		}
		if !removed {
			t.Fatalf("blockedBy drift did not converge with webhooks disabled; removeLabelCalls = %v", client.removeLabelCalls)
		}
	})
}
