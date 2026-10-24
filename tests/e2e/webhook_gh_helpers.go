//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// GitHub writers the live webhook tests need and the shared harness lacks (#2427):
// removing a blockedBy edge, a review with an inline comment, resolving a review
// thread and editing a review comment. Each fails the test on an API error, like
// the writers in harness.go.

// RemoveBlockedBy removes the blockedBy edge issueRepo#issueNumber -> blockerRepo#
// blockerNumber with the GraphQL removeBlockedBy mutation (the counterpart of
// AddBlockedBy and of the engine's github.Client.RemoveBlockedByIssue).
func RemoveBlockedBy(t *testing.T, env *Env, issueRepo string, issueNumber int, blockerRepo string, blockerNumber int) {
	t.Helper()
	nodeID := func(repo string, n int) string {
		out, err := ghOutput(env, "api", fmt.Sprintf("repos/%s/issues/%d", repo, n), "--jq", ".node_id")
		if err != nil {
			t.Fatalf("resolve node id of %s#%d: %v\n%s", repo, n, err, out)
		}
		id := lastNonEmpty(out)
		if id == "" {
			t.Fatalf("empty node id for %s#%d", repo, n)
		}
		return id
	}
	const mutation = `mutation($issueId: ID!, $blockingIssueId: ID!) {
  removeBlockedBy(input: {issueId: $issueId, blockingIssueId: $blockingIssueId}) { issue { id } }
}`
	if out, err := ghOutput(env, "api", "graphql",
		"-f", "query="+mutation,
		"-f", "issueId="+nodeID(issueRepo, issueNumber),
		"-f", "blockingIssueId="+nodeID(blockerRepo, blockerNumber)); err != nil {
		t.Fatalf("removeBlockedBy %s#%d -> %s#%d: %v\n%s", issueRepo, issueNumber, blockerRepo, blockerNumber, err, out)
	}
}

// reviewThreadRef identifies the inline review comment SubmitPRReviewWithThread made.
type reviewThreadRef struct {
	ReviewID  int
	CommentID int64  // REST (database) id of the inline comment
	ThreadID  string // GraphQL node id of its thread
}

// SubmitPRReviewWithThread submits a COMMENT review with one inline comment on line
// 1 of path, as the reviewer (a non-author identity), and returns the review, the
// comment and the thread that comment opened.
func SubmitPRReviewWithThread(t *testing.T, env *Env, reviewerToken, repo string, prNumber int, path, commentBody string) reviewThreadRef {
	t.Helper()
	owner, name, ok := splitRepo(repo)
	if !ok {
		t.Fatalf("bad repo: %q", repo)
	}
	payload, err := json.Marshal(map[string]any{
		"event": "COMMENT",
		"body":  "e2e webhook review (inline comment follows)",
		"comments": []map[string]any{
			{"path": path, "line": 1, "side": "RIGHT", "body": commentBody},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "review.json")
	if err := os.WriteFile(file, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := ghOutputWithToken(reviewerToken, "api", "-X", "POST",
		fmt.Sprintf("repos/%s/%s/pulls/%d/reviews", owner, name, prNumber), "--input", file)
	if err != nil {
		t.Fatalf("submitting a review with an inline comment on %s PR #%d: %v\n%s", repo, prNumber, err, out)
	}
	var resp struct {
		ID int `json:"id"`
	}
	if uerr := json.Unmarshal([]byte(strings.TrimSpace(out)), &resp); uerr != nil || resp.ID == 0 {
		t.Fatalf("could not read the review id from the response (%v): %q", uerr, out)
	}
	ref := reviewThreadRef{ReviewID: resp.ID}

	const q = `query($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) { pullRequest(number: $number) {
    reviewThreads(first: 50) { nodes { id comments(first: 1) { nodes { databaseId body } } } }
  } }
}`
	out, err = ghOutput(env, "api", "graphql", "-f", "query="+q, "-F", "owner="+owner, "-F", "name="+name, "-F", fmt.Sprintf("number=%d", prNumber))
	if err != nil {
		t.Fatalf("listing review threads of %s PR #%d: %v\n%s", repo, prNumber, err, out)
	}
	var gq struct {
		Data struct {
			Repository struct {
				PullRequest struct {
					ReviewThreads struct {
						Nodes []struct {
							ID       string `json:"id"`
							Comments struct {
								Nodes []struct {
									DatabaseID int64  `json:"databaseId"`
									Body       string `json:"body"`
								} `json:"nodes"`
							} `json:"comments"`
						} `json:"nodes"`
					} `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &gq); err != nil {
		t.Fatalf("parsing review threads: %v\n%s", err, out)
	}
	for _, th := range gq.Data.Repository.PullRequest.ReviewThreads.Nodes {
		if len(th.Comments.Nodes) > 0 && th.Comments.Nodes[0].Body == commentBody {
			ref.ThreadID, ref.CommentID = th.ID, th.Comments.Nodes[0].DatabaseID
			return ref
		}
	}
	t.Fatalf("the inline comment %q did not open a review thread on %s PR #%d", commentBody, repo, prNumber)
	return ref
}

// ResolveReviewThread resolves a review thread as the harness account.
func ResolveReviewThread(t *testing.T, env *Env, threadID string) {
	t.Helper()
	const m = `mutation($id: ID!) { resolveReviewThread(input: {threadId: $id}) { thread { isResolved } } }`
	if out, err := ghOutput(env, "api", "graphql", "-f", "query="+m, "-f", "id="+threadID); err != nil {
		t.Fatalf("resolving review thread %s: %v\n%s", threadID, err, out)
	}
}

// EditReviewComment edits an inline review comment as its author (reviewerToken).
func EditReviewComment(t *testing.T, repo, reviewerToken string, commentID int64, body string) {
	t.Helper()
	owner, name, ok := splitRepo(repo)
	if !ok {
		t.Fatalf("bad repo: %q", repo)
	}
	out, err := ghOutputWithToken(reviewerToken, "api", "-X", "PATCH",
		fmt.Sprintf("repos/%s/%s/pulls/comments/%d", owner, name, commentID), "-f", "body="+body)
	if err != nil {
		t.Fatalf("editing review comment %d on %s: %v\n%s", commentID, repo, err, out)
	}
}

// createCheckRun creates a check run on sha with the installation token (only a
// GitHub App can write check runs) and returns its id. status is "in_progress" or
// "completed"; conclusion is required for "completed".
func createCheckRun(token, repo, sha, name, status, conclusion string) (int64, error) {
	owner, repoName, ok := splitRepo(repo)
	if !ok {
		return 0, fmt.Errorf("bad repo: %q", repo)
	}
	args := []string{"api", "-X", "POST", fmt.Sprintf("repos/%s/%s/check-runs", owner, repoName),
		"-f", "name=" + name, "-f", "head_sha=" + sha, "-f", "status=" + status}
	if conclusion != "" {
		args = append(args, "-f", "conclusion="+conclusion)
	}
	out, err := ghOutputWithToken(token, args...)
	if err != nil {
		return 0, fmt.Errorf("creating check run %q on %s@%.8s: %v\n%s", name, repo, sha, err, redactSecret(out, token))
	}
	var resp struct {
		ID int64 `json:"id"`
	}
	if uerr := json.Unmarshal([]byte(strings.TrimSpace(out)), &resp); uerr != nil || resp.ID == 0 {
		return 0, fmt.Errorf("check run response carried no id (%v): %q", uerr, redactSecret(out, token))
	}
	return resp.ID, nil
}

// completeCheckRun completes an existing check run with conclusion.
func completeCheckRun(token, repo string, id int64, conclusion string) error {
	owner, repoName, ok := splitRepo(repo)
	if !ok {
		return fmt.Errorf("bad repo: %q", repo)
	}
	out, err := ghOutputWithToken(token, "api", "-X", "PATCH",
		fmt.Sprintf("repos/%s/%s/check-runs/%d", owner, repoName, id),
		"-f", "status=completed", "-f", "conclusion="+conclusion)
	if err != nil {
		return fmt.Errorf("completing check run %d on %s: %v\n%s", id, repo, err, redactSecret(out, token))
	}
	return nil
}
