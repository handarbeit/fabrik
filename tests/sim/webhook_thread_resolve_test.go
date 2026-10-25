package sim

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
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

// webhookThreadEnv returns an Env whose paused item has a linked PR in the Store
// and NO review thread yet, plus the sim comment seeded afterwards. Nothing
// polls after the seed, so the Store learns of the comment only from a delivery.
func webhookThreadEnv(t *testing.T) (env *Env, issue, pr int, thread string, dbID int) {
	t.Helper()
	env = NewEnv(t, EnvOptions{Stages: smokeStages(), BoardCache: true})
	issue = FileIssue(t, env, "thread resolve by webhook", "body", "Specify", "fabrik:paused", "fabrik:awaiting-input")
	branch := fmt.Sprintf("fabrik/issue-%d", issue)
	env.Sim.Sim().SeedCommitFrom(env.OwnerRepo, branch, "", map[string]string{"a.txt": "one\n"}, "sim: thread resolve")
	env.Sim.Sim().SeedPR(env.OwnerRepo, simgh.PRSeed{Title: "thread resolve", Head: branch, IssueNumber: issue})
	linked, err := env.Sim.FetchLinkedPR(env.Owner, env.Repo, issue)
	if err != nil || linked == nil || linked.Number == 0 {
		t.Fatalf("expected a linked PR, got %+v (err %v)", linked, err)
	}
	pr = linked.Number
	for i := 0; i < 12; i++ {
		RunPolls(t, env, 1)
		if st, ok := env.Engine.StoreItemForTest(env.OwnerRepo, issue); ok && st.LinkedPR != nil && st.LinkedPR.Number == pr {
			break
		}
	}
	if st, ok := env.Engine.StoreItemForTest(env.OwnerRepo, issue); !ok || st.LinkedPR == nil {
		t.Fatalf("the linked PR never reached the Store")
	}
	env.Sim.Sim().SeedReviewThreadComment(env.OwnerRepo, pr, "reviewer", "please rename this", "a.txt", 1)
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	// Read the sim's identifiers straight from the sim, not through the engine.
	st0, _ := env.Engine.StoreItemForTest(env.OwnerRepo, issue)
	probe := &gh.ProjectItem{ItemID: st0.ItemID, Repo: env.OwnerRepo, Number: issue, LinkedPRNumber: pr}
	if err := env.Sim.Sim().FetchItemDetails(probe); err != nil || len(probe.LinkedPRReviewThreadComments) != 1 {
		t.Fatalf("probing the sim thread: err=%v comments=%+v", err, probe.LinkedPRReviewThreadComments)
	}
	c := probe.LinkedPRReviewThreadComments[0]
	return env, issue, pr, c.ReviewThreadID, c.DatabaseID
}

// threadCommentCreated is a pull_request_review_comment "created" delivery for
// the sim's comment (DERIVED: built here, the sim has no webhook transport). It
// carries no thread ID, exactly as GitHub's does not.
func threadCommentCreated(env *Env, pr, dbID int) []byte {
	b, _ := json.Marshal(map[string]any{
		"action": "created",
		"comment": map[string]any{
			"node_id": fmt.Sprintf("comment:%d", dbID), "id": dbID, "body": "please rename this",
			"created_at": iso(time.Now()), "path": "a.txt", "line": 1,
			"user": map[string]any{"login": "reviewer"},
		},
		"pull_request": map[string]any{"number": pr},
		"repository":   map[string]any{"full_name": env.OwnerRepo},
	})
	return b
}

// #2444 (ADR 2444): a thread comment that reached the Store by webhook has no
// thread ID, so the thread's resolution must find it by the identities the
// delivery lists, before any deep fetch.
func TestWebhookThreadResolve_WebhookAddedCommentIsDropped(t *testing.T) {
	t.Parallel()
	env, issue, pr, thread, dbID := webhookThreadEnv(t)
	runThreadResolveByWebhook(t, env, issue, pr, thread, dbID, true)
}

// Non-vacuity: with identity matching neutralised the same sequence leaves the
// resolved thread's comment in the Store.
func TestWebhookThreadResolve_WebhookAddedCommentSurvivesWithoutTheFix(t *testing.T) {
	t.Parallel()
	env, issue, pr, thread, dbID := webhookThreadEnv(t)
	env.Engine.SetThreadResolveByIdentityDisabledForTest(true)
	runThreadResolveByWebhook(t, env, issue, pr, thread, dbID, false)
}

func runThreadResolveByWebhook(t *testing.T, env *Env, issue, pr int, thread string, dbID int, wantDropped bool) {
	t.Helper()
	before := readCalls(env)
	env.BoardCache.ApplyDelta("pull_request_review_comment", threadCommentCreated(env, pr, dbID))
	st, _ := env.Engine.StoreItemForTest(env.OwnerRepo, issue)
	if len(st.LinkedPR.ThreadComments) != 1 || st.LinkedPR.ThreadComments[0].ReviewThreadID != "" {
		t.Fatalf("the created delivery should store one thread-less comment, got %+v", st.LinkedPR.ThreadComments)
	}
	if err := env.Sim.Sim().ResolveReviewThread(thread); err != nil {
		t.Fatalf("resolve on GitHub: %v", err)
	}
	env.BoardCache.ApplyDelta("pull_request_review_thread", capturedDelivery(t, "pull_request_review_thread_resolved", func(d map[string]any) {
		th := d["thread"].(map[string]any)
		th["node_id"] = thread
		cm := th["comments"].([]any)[0].(map[string]any)
		cm["node_id"] = fmt.Sprintf("comment:%d", dbID)
		cm["id"] = dbID
		d["pull_request"].(map[string]any)["number"] = pr
		d["repository"].(map[string]any)["full_name"] = env.OwnerRepo
		d["updated_at"] = iso(time.Now().Add(time.Second))
	}))
	st, _ = env.Engine.StoreItemForTest(env.OwnerRepo, issue)
	if wantDropped {
		if len(st.LinkedPR.ThreadComments) != 0 || st.LinkedPR.ResolvedThreadCount != 1 {
			t.Fatalf("resolved thread's webhook-added comment still cached: comments=%+v resolved=%d",
				st.LinkedPR.ThreadComments, st.LinkedPR.ResolvedThreadCount)
		}
	} else if len(st.LinkedPR.ThreadComments) != 1 {
		t.Fatalf("neutralised run should leave the comment (non-vacuity), got %+v", st.LinkedPR.ThreadComments)
	}
	if got := readCalls(env) - before; got != 0 {
		t.Fatalf("the deliveries made %d GitHub reads, want 0", got)
	}
}
