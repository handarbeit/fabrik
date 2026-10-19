package boardcache

import (
	"fmt"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

// Webhook observation times and the Reconcile removal guard (#2348).

func iso(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func labelPayload(action, label string, updatedAt string) []byte {
	return []byte(fmt.Sprintf(
		`{"action":%q,"label":{"name":%q},"issue":{"number":1,"updated_at":%s},"repository":{"full_name":"owner/repo"}}`,
		action, label, updatedAt))
}

func hasStr(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// A labeled event carrying a GitHub time older than a recorded observation of the
// label is dropped; one carrying a newer time applies.
func TestDeltaLabelEvent_OrderedByPayloadTime(t *testing.T) {
	c := seedCache(t)
	now := time.Now()
	c.ApplyDelta("issues", labelPayload("labeled", "x", fmt.Sprintf("%q", iso(now))))
	c.ApplyDelta("issues", labelPayload("unlabeled", "x", fmt.Sprintf("%q", iso(now.Add(time.Minute)))))
	c.ApplyDelta("issues", labelPayload("labeled", "x", fmt.Sprintf("%q", iso(now.Add(-time.Minute))))) // late replay
	if hasStr(testGetState(t, c, "owner/repo", 1).Labels, "x") {
		t.Fatal("an older labeled event re-added the label")
	}
	c.ApplyDelta("issues", labelPayload("labeled", "x", fmt.Sprintf("%q", iso(now.Add(2*time.Minute)))))
	if !hasStr(testGetState(t, c, "owner/repo", 1).Labels, "x") {
		t.Fatal("a newer labeled event was dropped")
	}
}

// A mistyped or missing updated_at must not drop the delivery (lenient decode,
// #2383); it falls back to receipt order.
func TestDeltaLabelEvent_MistypedTimeStillApplies(t *testing.T) {
	for name, raw := range map[string]string{"number": "12345", "null": "null", "object": `{}`, "garbage": `"not-a-time"`} {
		name, raw := name, raw
		t.Run(name, func(t *testing.T) {
			c := seedCache(t)
			c.ApplyDelta("issues", labelPayload("labeled", "x", raw))
			if !hasStr(testGetState(t, c, "owner/repo", 1).Labels, "x") {
				t.Fatalf("delivery with updated_at=%s was dropped", raw)
			}
		})
	}
	t.Run("absent", func(t *testing.T) {
		c := seedCache(t)
		c.ApplyDelta("issues", issuesLabeledPayloadJSON("labeled", "owner/repo", 1, "x"))
		if !hasStr(testGetState(t, c, "owner/repo", 1).Labels, "x") {
			t.Fatal("delivery without updated_at was dropped")
		}
	})
}

// A webhook head older than the recorded one does not move the head, and a review
// event that carries no head leaves the recorded head alone.
func TestDeltaPullRequest_ReplayedHeadDropped(t *testing.T) {
	c := seedCache(t)
	c.RecordPRLinkage("owner/repo", 5, 1)
	now := time.Now()
	sync := func(sha string, at time.Time) {
		c.ApplyDelta("pull_request", []byte(fmt.Sprintf(
			`{"action":"synchronize","pull_request":{"number":5,"title":"t","state":"open","updated_at":%q,"head":{"sha":%q}},"repository":{"full_name":"owner/repo"}}`,
			iso(at), sha)))
	}
	sync("sha-b", now)
	sync("sha-a", now.Add(-time.Minute))
	if got := testGetState(t, c, "owner/repo", 1).LinkedPR.HeadSHA; got != "sha-b" {
		t.Fatalf("head = %q, want sha-b", got)
	}
	// A review delivery establishes linkage only.
	c.ApplyDelta("pull_request_review", []byte(
		`{"action":"submitted","review":{"id":9,"state":"approved","user":{"login":"alice"},"submitted_at":"`+iso(now)+`"},`+
			`"pull_request":{"number":5},"repository":{"full_name":"owner/repo"}}`))
	st := testGetState(t, c, "owner/repo", 1)
	if st.LinkedPR.HeadSHA != "sha-b" {
		t.Fatalf("a review without a head wiped it: %q", st.LinkedPR.HeadSHA)
	}
	if len(st.LinkedPR.Reviews) != 1 || st.LinkedPR.Reviews[0].SubmittedAt.IsZero() {
		t.Fatalf("review not stored with its submitted_at: %+v", st.LinkedPR.Reviews)
	}
}

// Reconcile keeps an item a webhook added after its listing's fetch started, and
// still removes one that was present before the fetch and is missing from it.
func TestReconcile_KeepsItemAddedAfterFetchStarted(t *testing.T) {
	c := seedCache(t) // issues 1 and 2
	fetchStarted := time.Now()
	time.Sleep(5 * time.Millisecond)
	// A webhook adds #3 while the listing fetch is in flight.
	c.ApplyDelta("issues", issuesOpenedPayloadJSON("owner/repo", 3, "I_3", "late", "", nil, nil))
	c.store.Apply(itemstate.WorkerEntered{Repo: "owner/repo", Number: 3, StageName: "Specify", StartedAt: time.Now()})

	board := &gh.ProjectBoard{ProjectID: testProjectID, FetchedAt: fetchStarted, Items: []gh.ProjectItem{
		{ID: "I_001", ItemID: "PVTI_001", Number: 1, Repo: "owner/repo", Status: "Research"},
		// #2 is gone from GitHub; #3 is not on this older listing.
	}}
	c.Reconcile(board)

	if _, ok := c.store.Peek("owner/repo", 3); !ok {
		t.Fatal("Reconcile removed an item added after its fetch started")
	}
	snap, _ := c.store.Peek("owner/repo", 3)
	if snap.State().Worker == nil {
		t.Fatal("the kept item lost its worker handle")
	}
	if _, ok := c.store.Peek("owner/repo", 2); ok {
		t.Fatal("Reconcile kept an item that was cached before the fetch and is missing from it")
	}
}

// A zero FetchedAt keeps the unconditional removal.
func TestReconcile_ZeroFetchedAtRemovesMissing(t *testing.T) {
	c := seedCache(t)
	c.ApplyDelta("issues", issuesOpenedPayloadJSON("owner/repo", 3, "I_3", "late", "", nil, nil))
	c.Reconcile(&gh.ProjectBoard{ProjectID: testProjectID})
	if _, ok := c.store.Peek("owner/repo", 3); ok {
		t.Fatal("zero FetchedAt kept an item")
	}
}

// A late check_run for a superseded commit must not move the head back through
// the auto-heal path, and must not wipe the current head's check runs. The same
// path still fills a head the Store does not have yet.
func TestDeltaCheckRun_AutoHealDoesNotRegressHead(t *testing.T) {
	mc := &mockClient{fetchPRsForSHAFn: func(owner, repo, sha string) ([]int, error) { return []int{5}, nil }}
	c := NewCacheImpl(mc, itemstate.NewStore(nil), nopLog)
	testBootstrapFromBoard(c, &gh.ProjectBoard{
		ProjectID: "PID",
		Items:     []gh.ProjectItem{{ID: "I_001", ItemID: "PVTI_001", Number: 1, Repo: "owner/repo", Status: "Implement"}},
	})
	c.RecordPRLinkage("owner/repo", 5, 1)
	now := time.Now()
	c.ApplyDelta("pull_request", []byte(fmt.Sprintf(
		`{"action":"synchronize","pull_request":{"number":5,"title":"t","state":"open","updated_at":%q,"head":{"sha":"sha-b"}},"repository":{"full_name":"owner/repo"}}`,
		iso(now))))
	c.ApplyDelta("check_run", checkRunPayloadJSON("completed", "owner/repo", 11, "ci", "completed", "success", "sha-b"))
	c.ApplyDelta("check_run", checkRunPayloadJSON("completed", "owner/repo", 10, "ci", "completed", "success", "sha-a"))
	st := testGetState(t, c, "owner/repo", 1)
	if st.LinkedPR.HeadSHA != "sha-b" || len(st.LinkedPR.CheckRuns) != 1 {
		t.Fatalf("head %q, check runs %v: a check_run for an old commit regressed the head", st.LinkedPR.HeadSHA, st.LinkedPR.CheckRuns)
	}

	// A PR with no recorded head is still filled by the heal.
	c2 := NewCacheImpl(mc, itemstate.NewStore(nil), nopLog)
	testBootstrapFromBoard(c2, &gh.ProjectBoard{
		ProjectID: "PID",
		Items:     []gh.ProjectItem{{ID: "I_001", ItemID: "PVTI_001", Number: 1, Repo: "owner/repo", Status: "Implement"}},
	})
	c2.RecordPRLinkage("owner/repo", 5, 1)
	c2.ApplyDelta("check_run", checkRunPayloadJSON("completed", "owner/repo", 12, "ci", "completed", "success", "sha-c"))
	if got := testGetState(t, c2, "owner/repo", 1).LinkedPR.HeadSHA; got != "sha-c" {
		t.Fatalf("head = %q, want sha-c filled by the heal", got)
	}
}

// A delivery that carries no GitHub time is ordered by when the daemon RECEIVED
// it, not by when it was applied (#2351): a delivery queued behind a slow apply
// must lose to an engine write that landed while it waited.
func TestApplyDeltaAt_ReceiptTimeLosesToALaterEngineWrite(t *testing.T) {
	c := seedCache(t)
	key := ItemKey("owner/repo", 1)
	received := time.Now().Add(-time.Minute) // arrived a minute ago, applied now

	c.ApplyLabelAdded(key, "x") // the engine's write, after the delivery arrived
	c.ApplyDeltaAt("issues", issuesLabeledPayloadJSON("unlabeled", "owner/repo", 1, "x"), received)
	if !hasStr(testGetState(t, c, "owner/repo", 1).Labels, "x") {
		t.Fatal("a stale unlabeled delivery (received before the engine write) removed the label")
	}

	// Non-vacuity: stamped at apply time, the same delivery looks newer and wins —
	// exactly what receipt-time stamping prevents.
	c.ApplyDelta("issues", issuesLabeledPayloadJSON("unlabeled", "owner/repo", 1, "x"))
	if hasStr(testGetState(t, c, "owner/repo", 1).Labels, "x") {
		t.Fatal("non-vacuity: an apply-time stamped delivery should have removed the label")
	}
}

// The same for a review edited/dismissed event, which has no time of its own
// (submitted_at is the ORIGINAL submission time): it is ordered by receipt, so one
// received before a fresher whole-list read of the PR's reviews does not undo it.
func TestApplyDeltaAt_DismissedReviewOrderedByReceiptTime(t *testing.T) {
	c := seedCache(t)
	c.RecordPRLinkage("owner/repo", 5, 1)
	now := time.Now()
	dismissed := []byte(`{"action":"dismissed","review":{"id":9,"state":"dismissed","user":{"login":"alice"},"submitted_at":"` + iso(now.Add(-time.Hour)) + `"},` +
		`"pull_request":{"number":5},"repository":{"full_name":"owner/repo"}}`)
	reviews := func() []gh.PRReview { return testGetState(t, c, "owner/repo", 1).LinkedPR.Reviews }

	// A live deep fetch, started at `now`, reads alice's review as APPROVED.
	fresh := gh.ProjectItem{
		Repo: "owner/repo", Number: 1, Status: "Plan", LabelsAbsent: true,
		LinkedPRNumber:  5,
		LinkedPRReviews: []gh.PRReview{{Author: "alice", State: "APPROVED", DatabaseID: 9, SubmittedAt: now.Add(-2 * time.Hour)}},
		DeepFetchLive:   true, DeepFetchStartedAt: now,
	}
	c.store.Apply(itemstate.ItemDeepFetched{Repo: "owner/repo", Number: 1, FreshState: fresh})
	if rs := reviews(); len(rs) != 1 || rs[0].State != "APPROVED" {
		t.Fatalf("setup: the deep fetch did not store the approval: %+v", rs)
	}

	// A dismissal RECEIVED before that read (but applied after, having waited in
	// the intake) must not undo it.
	c.ApplyDeltaAt("pull_request_review", dismissed, now.Add(-time.Hour))
	if rs := reviews(); len(rs) != 1 || rs[0].State != "APPROVED" {
		t.Fatalf("a dismissal received before the read overwrote it: %+v", rs)
	}

	// Non-vacuity: one received after the read applies.
	c.ApplyDeltaAt("pull_request_review", dismissed, now.Add(time.Minute))
	if rs := reviews(); len(rs) != 1 || rs[0].State != "DISMISSED" {
		t.Fatalf("a dismissal received after the read was dropped: %+v", rs)
	}
}
