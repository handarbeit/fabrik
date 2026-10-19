package sim

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/tests/sim/simgh"
)

// #2352 (ADR 2352): a review thread resolved or unresolved on GitHub, and a
// comment edited or deleted, reach the engine's Store from the webhook alone.
// The deliveries are the captured payloads (boardcache/testdata/webhooks) with
// the identifying fields rewritten to the sim's PR and thread.

// githubReadMethods are the read calls a refetch would make; the delivery of a
// webhook must add none.
var githubReadMethods = []string{"FetchItemDetails", "FetchLinkedPR", "FetchProjectItem", "FetchPRReviews", "FetchPRClosingIssues", "FetchProjectBoard", "FetchLabels"}

func readCalls(env *Env) int {
	n := 0
	for _, m := range githubReadMethods {
		n += env.Sim.Faults().CallCount(m)
	}
	return n
}

func capturedDelivery(t *testing.T, name string, rewrite func(map[string]any)) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "boardcache", "testdata", "webhooks", name+".json"))
	if err != nil {
		t.Fatalf("read capture %s: %v", name, err)
	}
	var d map[string]any
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatalf("decode capture %s: %v", name, err)
	}
	rewrite(d)
	out, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// threadEnv returns an Env whose paused item has a linked PR carrying one
// unresolved review thread, already loaded into the Store by the engine's own
// fetches.
func threadEnv(t *testing.T) (env *Env, issue, pr int, threadID string) {
	t.Helper()
	env = NewEnv(t, EnvOptions{Stages: smokeStages(), BoardCache: true})
	issue = FileIssue(t, env, "thread resolve", "body", "Specify", "fabrik:paused", "fabrik:awaiting-input")
	branch := fmt.Sprintf("fabrik/issue-%d", issue)
	env.Sim.Sim().SeedCommitFrom(env.OwnerRepo, branch, "", map[string]string{"a.txt": "one\n"}, "sim: thread resolve")
	env.Sim.Sim().SeedPR(env.OwnerRepo, simgh.PRSeed{Title: "thread resolve", Head: branch, IssueNumber: issue})
	linked, err := env.Sim.FetchLinkedPR(env.Owner, env.Repo, issue)
	if err != nil || linked == nil || linked.Number == 0 {
		t.Fatalf("expected a linked PR, got %+v (err %v)", linked, err)
	}
	pr = linked.Number
	env.Sim.Sim().SeedReviewThreadComment(env.OwnerRepo, pr, "reviewer", "please rename this", "a.txt", 1)
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	// Run polls until the engine's own fetch has put the thread in the Store.
	for i := 0; i < 12; i++ {
		RunPolls(t, env, 1)
		if st, ok := env.Engine.StoreItemForTest(env.OwnerRepo, issue); ok && st.LinkedPR != nil && len(st.LinkedPR.ThreadComments) > 0 {
			return env, issue, pr, st.LinkedPR.ThreadComments[0].ReviewThreadID
		}
	}
	t.Fatalf("the unresolved thread never reached the Store")
	return
}

func TestWebhookThreadResolve_SeenWithoutRefetch(t *testing.T) {
	t.Parallel()
	env, issue, pr, thread := threadEnv(t)

	// The reviewer resolves the thread on GitHub; the webhook arrives.
	if err := env.Sim.Sim().ResolveReviewThread(thread); err != nil {
		t.Fatalf("resolve on GitHub: %v", err)
	}
	var woke []itemstate.Change
	env.Engine.SubscribeStoreForTest(itemstate.ObserverFunc(func(c itemstate.Change, _ itemstate.Snapshot) { woke = append(woke, c) }))

	before := readCalls(env)
	env.BoardCache.ApplyDelta("pull_request_review_thread", capturedDelivery(t, "pull_request_review_thread_resolved", func(d map[string]any) {
		d["thread"].(map[string]any)["node_id"] = thread
		d["pull_request"].(map[string]any)["number"] = pr
		d["repository"].(map[string]any)["full_name"] = env.OwnerRepo
		d["updated_at"] = iso(time.Now())
	}))

	st, _ := env.Engine.StoreItemForTest(env.OwnerRepo, issue)
	if len(st.LinkedPR.ThreadComments) != 0 {
		t.Fatalf("the resolved thread still counts as unresolved feedback: %+v", st.LinkedPR.ThreadComments)
	}
	if st.LinkedPR.ResolvedThreadCount != 1 {
		t.Fatalf("ResolvedThreadCount = %d, want 1", st.LinkedPR.ResolvedThreadCount)
	}
	if got := readCalls(env) - before; got != 0 {
		t.Fatalf("applying the delivery made %d GitHub reads, want 0", got)
	}
	wakes := 0
	for _, c := range woke {
		if c.Fields&(itemstate.CommentsChanged|itemstate.LinkedPRChanged) != 0 {
			wakes++
		}
	}
	if wakes != 1 {
		t.Fatalf("the resolve raised %d waking changes, want 1", wakes)
	}
}

func TestWebhookThreadUnresolve_InvalidatesTheDeepCache(t *testing.T) {
	t.Parallel()
	env, issue, pr, thread := threadEnv(t)
	st, _ := env.Engine.StoreItemForTest(env.OwnerRepo, issue)
	if st.LastDeepFetchAt.IsZero() {
		t.Fatalf("item was not deep fetched; the unresolved-thread invalidation would be checked vacuously")
	}
	env.BoardCache.ApplyDelta("pull_request_review_thread", capturedDelivery(t, "pull_request_review_thread_unresolved", func(d map[string]any) {
		d["thread"].(map[string]any)["node_id"] = thread
		d["pull_request"].(map[string]any)["number"] = pr
		d["repository"].(map[string]any)["full_name"] = env.OwnerRepo
		d["updated_at"] = iso(time.Now())
	}))
	st, _ = env.Engine.StoreItemForTest(env.OwnerRepo, issue)
	if !st.LastDeepFetchAt.IsZero() {
		t.Fatalf("an unresolved thread must force the next fetch to rebuild the thread")
	}
}

func TestWebhookCommentEditDelete_SeenWithoutRefetch(t *testing.T) {
	t.Parallel()
	env := NewEnv(t, EnvOptions{Stages: smokeStages(), BoardCache: true})
	issue := FileIssue(t, env, "comment edit", "body", "Specify", "fabrik:paused", "fabrik:awaiting-input")
	env.Sim.Sim().SeedComment(env.OwnerRepo, issue, "human", "original")
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	var dbID int
	for i := 0; i < 12 && dbID == 0; i++ {
		RunPolls(t, env, 1)
		if st, ok := env.Engine.StoreItemForTest(env.OwnerRepo, issue); ok {
			for _, c := range st.Comments {
				if c.Body == "original" {
					dbID = c.DatabaseID
				}
			}
		}
	}
	if dbID == 0 {
		t.Fatal("the comment never reached the Store")
	}
	rewrite := func(d map[string]any) {
		d["comment"].(map[string]any)["id"] = dbID
		d["issue"].(map[string]any)["number"] = issue
		d["repository"].(map[string]any)["full_name"] = env.OwnerRepo
		if c, ok := d["comment"].(map[string]any); ok {
			c["updated_at"] = iso(time.Now())
		}
	}
	before := readCalls(env)
	env.BoardCache.ApplyDelta("issue_comment", capturedDelivery(t, "issue_comment_edited", rewrite))
	st, _ := env.Engine.StoreItemForTest(env.OwnerRepo, issue)
	edited := false
	for _, c := range st.Comments {
		if c.DatabaseID == dbID && c.Body == "probe comment edited" {
			edited = true
		}
	}
	if !edited {
		t.Fatalf("edit not applied: %+v", st.Comments)
	}
	env.BoardCache.ApplyDelta("issue_comment", capturedDelivery(t, "issue_comment_deleted", rewrite))
	st, _ = env.Engine.StoreItemForTest(env.OwnerRepo, issue)
	for _, c := range st.Comments {
		if c.DatabaseID == dbID {
			t.Fatalf("deleted comment still cached: %+v", c)
		}
	}
	if got := readCalls(env) - before; got != 0 {
		t.Fatalf("the deliveries made %d GitHub reads, want 0", got)
	}
}
