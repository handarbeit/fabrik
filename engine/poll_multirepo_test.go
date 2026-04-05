package engine

import (
	"context"
	"sync"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

// TestPoll_MultiRepoFilter_SkipsOtherRepos verifies that items belonging to
// a different repo on the same project board are not processed.
func TestPoll_MultiRepoFilter_SkipsOtherRepos(t *testing.T) {
	// The engine is configured for owner/repo.
	// The board has two items: one for owner/repo, one for owner/other-repo.
	// Only the matching item should be deep-fetched and dispatched.
	var (
		mu                 sync.Mutex
		deepFetchedNumbers []int
	)
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{
				ProjectID: "PVT_1",
				Items: []gh.ProjectItem{
					{Number: 1, Title: "Same repo", Status: "Research", ItemID: "PVTI_1", Repo: "owner/repo"},
					{Number: 2, Title: "Other repo", Status: "Research", ItemID: "PVTI_2", Repo: "owner/other-repo"},
					{Number: 3, Title: "No repo field", Status: "Research", ItemID: "PVTI_3", Repo: ""},
				},
			}, nil
		},
		fetchStatusFieldFn: func(projectID string) (*gh.StatusField, error) {
			return &gh.StatusField{
				FieldID: "F1",
				Options: map[string]string{"Research": "OPT_1"},
			}, nil
		},
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			mu.Lock()
			deepFetchedNumbers = append(deepFetchedNumbers, item.Number)
			mu.Unlock()
			return nil
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})

	if err := eng.poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	// Wait for all dispatched workers to finish before inspecting results.
	eng.wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	// Item #2 (owner/other-repo) should NOT have been deep-fetched
	for _, n := range deepFetchedNumbers {
		if n == 2 {
			t.Error("item #2 (other repo) should not be deep-fetched")
		}
	}
	// Items #1 and #3 may be deep-fetched (they pass the repo filter)
}

// TestPoll_MultiRepoFilter_YoloCatchup_SkipsOtherRepos verifies that the yolo
// catch-up loop also skips items from other repos.
func TestPoll_MultiRepoFilter_YoloCatchup_SkipsOtherRepos(t *testing.T) {
	var advancedItems []string
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{
				ProjectID: "PVT_1",
				Items: []gh.ProjectItem{
					// This item is in a different repo — should be skipped even in yolo mode
					{Number: 10, Title: "Other repo yolo", Status: "Research", ItemID: "PVTI_10",
						Repo: "owner/other-repo", Labels: []string{"fabrik:yolo", "stage:Research:complete"}},
				},
			}, nil
		},
		fetchStatusFieldFn: func(projectID string) (*gh.StatusField, error) {
			return &gh.StatusField{
				FieldID: "F1",
				Options: map[string]string{"Research": "OPT_1", "Plan": "OPT_2"},
			}, nil
		},
	}
	client.updateProjectItemStatusFn = func(projectID, itemID, statusFieldID, statusOptionID string) error {
		advancedItems = append(advancedItems, itemID)
		return nil
	}
	eng := testEngine(client, &mockClaudeInvoker{})
	eng.statusField = &gh.StatusField{
		FieldID: "F1",
		Options: map[string]string{"Research": "OPT_1", "Plan": "OPT_2"},
	}

	if err := eng.poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	eng.wg.Wait()

	// No items from other repo should have been advanced
	for _, id := range advancedItems {
		if id == "PVTI_10" {
			t.Error("item from other repo should not be advanced in yolo catchup")
		}
	}
}
