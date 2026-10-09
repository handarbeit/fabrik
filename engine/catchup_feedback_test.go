package engine

import (
	"errors"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// Neutralisation (each test must fail with the branch it names removed):
//   - TestCatchUpSuppresses_*          : drop the matching conjunct in catchUpSuppresses.
//   - TestSettleQueuedReviewFindings_CatchUp*: remove the dropCatchUpFeedback call in
//     queuedReviewFindings.
//   - TestFeedbackGate_CatchUp*        : remove the dropCatchUpFeedback call in
//     unprocessedFeedback.

const (
	cuHead = "c0ffee0000000000000000000000000000000001"
	cuBase = "ba5e000000000000000000000000000000000002"
	cuOld  = "01d0000000000000000000000000000000000003"
	cuSelf = "fabrik-bot"
)

func cuIsBot(a string) bool { return a == "handarbeit-pruefer" || a == "copilot[bot]" }

func TestCatchUpMarker_RoundTrip(t *testing.T) {
	want := catchUpMarker{Head: cuHead, Base: cuBase, Pure: true}
	got := parseCatchUpMarkers("🏭 text\n" + formatCatchUpMarker(want) + "\nmore")
	if len(got) != 1 || got[0] != want {
		t.Fatalf("round trip = %+v, want [%+v]", got, want)
	}
	if got := parseCatchUpMarkers("<!-- fabrik:train-catch-up head=zzz base=1 pure=maybe -->"); len(got) != 0 {
		t.Errorf("malformed marker parsed: %+v", got)
	}
}

func TestCatchUpMarkerForHead_OnlyTrustsSelfAuthoredMarkerForTheLiveHead(t *testing.T) {
	mk := catchUpMarker{Head: cuHead, Base: cuBase, Pure: true}
	body := formatCatchUpMarker(mk)
	cases := map[string]struct {
		comments []gh.Comment
		self     string
		live     string
		want     bool
	}{
		"self-authored, matching head": {[]gh.Comment{{Author: cuSelf, Body: body}}, cuSelf, cuHead, true},
		"other author is ignored":      {[]gh.Comment{{Author: "mallory", Body: body}}, cuSelf, cuHead, false},
		"marker for another head":      {[]gh.Comment{{Author: cuSelf, Body: body}}, cuSelf, cuOld, false},
		"empty self fails closed":      {[]gh.Comment{{Author: "", Body: body}}, "", cuHead, false},
		"empty live head fails closed": {[]gh.Comment{{Author: cuSelf, Body: body}}, cuSelf, "", false},
		"no marker":                    {[]gh.Comment{{Author: cuSelf, Body: "hello"}}, cuSelf, cuHead, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, ok := catchUpMarkerForHead(tc.comments, tc.self, tc.live); ok != tc.want {
				t.Errorf("ok = %v, want %v", ok, tc.want)
			}
		})
	}
}

func TestCatchUpSuppresses_RequiresEveryCondition(t *testing.T) {
	mk := catchUpMarker{Head: cuHead, Base: cuBase, Pure: true}
	if !catchUpSuppresses(mk, cuHead, cuHead, "handarbeit-pruefer", cuIsBot) {
		t.Fatal("pure + live head + same-commit + bot must suppress")
	}
	cases := map[string]bool{
		"conflict-edited catch-up (pure=false)": catchUpSuppresses(catchUpMarker{Head: cuHead, Base: cuBase}, cuHead, cuHead, "handarbeit-pruefer", cuIsBot),
		"marker head is not the live head":      catchUpSuppresses(mk, cuOld, cuHead, "handarbeit-pruefer", cuIsBot),
		"finding made against an older head":    catchUpSuppresses(mk, cuHead, cuOld, "handarbeit-pruefer", cuIsBot),
		"finding with no commit":                catchUpSuppresses(mk, cuHead, "", "handarbeit-pruefer", cuIsBot),
		"human author":                          catchUpSuppresses(mk, cuHead, cuHead, "alice", cuIsBot),
		"nil bot predicate":                     catchUpSuppresses(mk, cuHead, cuHead, "handarbeit-pruefer", nil),
		"empty marker head":                     catchUpSuppresses(catchUpMarker{Pure: true}, "", "", "handarbeit-pruefer", cuIsBot),
	}
	for name, got := range cases {
		if got {
			t.Errorf("%s: suppressed, want actionable", name)
		}
	}
}

// catchUpItem is a Queued member whose linked PR head is cuHead with a Pruefer review
// body and a Pruefer inline thread comment, both made against commit.
func catchUpItem(commit string) gh.ProjectItem {
	return gh.ProjectItem{
		Number: 1, ItemID: "PVTI_1", Repo: "owner/repo", Status: "Queued",
		LinkedPRNumber: 10, LinkedPRHeadSHA: cuHead,
		LinkedPRReviews: []gh.PRReview{{
			Author: "handarbeit-pruefer[bot]", State: "COMMENTED", Body: "summary", DatabaseID: 900, NodeID: "PRR_1", CommitID: commit,
		}},
		LinkedPRReviewThreadComments: []gh.Comment{{
			ID: "PRRC_1", DatabaseID: 901, Author: "handarbeit-pruefer[bot]", Body: "nit", ReviewThreadID: "RT_1", CommitOID: commit,
		}},
	}
}

func catchUpClient(markerComments ...gh.Comment) *mockGitHubClient {
	return &mockGitHubClient{
		fetchIssueCommentsFn: func(owner, repo string, n int) ([]gh.Comment, error) { return markerComments, nil },
	}
}

func markerComment(author string, pure bool, head string) gh.Comment {
	return gh.Comment{Author: author, Body: "🏭 **Fabrik merge-train** — caught up\n" + formatCatchUpMarker(catchUpMarker{Head: head, Base: cuBase, Pure: pure})}
}

func TestFeedbackGate_CatchUpOnlyHead_BotReviewNeitherHolds(t *testing.T) {
	eng := testEngineForMerge(t, catchUpClient(markerComment(cuSelf, true, cuHead)))
	eng.cfg.User = cuSelf
	if eng.feedbackGateBlocks(catchUpItem(cuHead), true, "landing decision") {
		t.Fatal("a bot review of a pure catch-up head must not hold the gate")
	}
}

func TestFeedbackGate_CatchUp_StaysActionableWhenNotVerifiable(t *testing.T) {
	cases := map[string]struct {
		client *mockGitHubClient
		item   gh.ProjectItem
	}{
		"conflict-edited catch-up (pure=false)": {catchUpClient(markerComment(cuSelf, false, cuHead)), catchUpItem(cuHead)},
		"marker authored by someone else":       {catchUpClient(markerComment("mallory", true, cuHead)), catchUpItem(cuHead)},
		"no marker at all":                      {catchUpClient(), catchUpItem(cuHead)},
		"review made against an older head":     {catchUpClient(markerComment(cuSelf, true, cuHead)), catchUpItem(cuOld)},
		"review with no commit attribution":     {catchUpClient(markerComment(cuSelf, true, cuHead)), catchUpItem("")},
		"marker read fails": {&mockGitHubClient{fetchIssueCommentsFn: func(o, r string, n int) ([]gh.Comment, error) {
			return nil, errors.New("boom")
		}}, catchUpItem(cuHead)},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			eng := testEngineForMerge(t, tc.client)
			eng.cfg.User = cuSelf
			if !eng.feedbackGateBlocks(tc.item, true, "landing decision") {
				t.Error("gate did not hold — the finding must stay actionable")
			}
		})
	}
}

func TestFeedbackGate_CatchUp_HumanReviewAndHumanThreadStayActionable(t *testing.T) {
	item := catchUpItem(cuHead)
	item.LinkedPRReviews[0].Author = "alice"
	item.LinkedPRReviewThreadComments = nil
	eng := testEngineForMerge(t, catchUpClient(markerComment(cuSelf, true, cuHead)))
	eng.cfg.User = cuSelf
	if !eng.feedbackGateBlocks(item, true, "landing decision") {
		t.Fatal("a human review must hold even on a pure catch-up head")
	}

	item = catchUpItem(cuHead)
	item.LinkedPRReviews = nil
	item.LinkedPRReviewThreadComments[0].Author = "alice"
	if !eng.feedbackGateBlocks(item, true, "landing decision") {
		t.Fatal("a human thread comment must hold even on a pure catch-up head")
	}
}

func TestFeedbackGate_CatchUp_UnrelatedFindingStillHoldsAlongsideSuppressedOne(t *testing.T) {
	item := catchUpItem(cuHead)
	item.LinkedPRReviewThreadComments = append(item.LinkedPRReviewThreadComments,
		gh.Comment{ID: "PRRC_2", DatabaseID: 902, Author: "handarbeit-pruefer[bot]", Body: "older", ReviewThreadID: "RT_2", CommitOID: cuOld})
	eng := testEngineForMerge(t, catchUpClient(markerComment(cuSelf, true, cuHead)))
	eng.cfg.User = cuSelf
	if !eng.feedbackGateBlocks(item, true, "landing decision") {
		t.Fatal("the finding against the older head must still hold")
	}
}

func TestSettleQueuedReviewFindings_CatchUpOnlyHead_DoesNotEject(t *testing.T) {
	client := catchUpClient(markerComment(cuSelf, true, cuHead))
	eng := trainTestEngine(t, client, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))
	eng.cfg.User = cuSelf

	eng.settleQueuedReviewFindings(&gh.ProjectBoard{ProjectID: "PVT_1", Items: []gh.ProjectItem{catchUpItem(cuHead)}})

	if len(client.updateStatusCalls) != 0 {
		t.Errorf("a bot review of a pure catch-up head ejected the member: %d status update(s)", len(client.updateStatusCalls))
	}
}

func TestSettleQueuedReviewFindings_CatchUpWithConflictEdits_StillEjects(t *testing.T) {
	client := catchUpClient(markerComment(cuSelf, false, cuHead))
	eng := trainTestEngine(t, client, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))
	eng.cfg.User = cuSelf

	eng.settleQueuedReviewFindings(&gh.ProjectBoard{ProjectID: "PVT_1", Items: []gh.ProjectItem{catchUpItem(cuHead)}})

	if len(client.updateStatusCalls) != 1 {
		t.Errorf("a review of a conflict-edited catch-up must eject as usual, got %d status update(s)", len(client.updateStatusCalls))
	}
}

func TestIsCatchUpReviewBot(t *testing.T) {
	eng := testEngineForMerge(t, &mockGitHubClient{})
	if eng.isCatchUpReviewBot("alice") {
		t.Error("a human login is not a review bot")
	}
	if eng.isCatchUpReviewBot("") {
		t.Error("an empty author is not a review bot")
	}
	if !eng.isCatchUpReviewBot("copilot-pull-request-reviewer[bot]") {
		t.Error("a [bot] login is a review bot")
	}
	// handarbeit-pruefer carries no [bot] suffix on every ingestion path; a stage that
	// declares it in expected_reviewers makes it recognisable.
	if eng.isCatchUpReviewBot("handarbeit-pruefer") {
		t.Error("undeclared plain login must not be treated as a bot")
	}
	declared := []string{"handarbeit-pruefer"}
	eng.cfg.Stages = append(eng.cfg.Stages, &stages.Stage{Name: "Review", ExpectedReviewers: &declared})
	if !eng.isCatchUpReviewBot("handarbeit-pruefer") {
		t.Error("a declared expected_reviewers identity must be recognised")
	}
}

// Neutralisation: TestCatchUpMarkerCache_* fail if catchUpFeedbackFilterFor reads the
// comments directly (the repeated-read test) or if the TTL / forgetCatchUpMarker is
// removed (the expiry and invalidation tests).
func TestCatchUpMarkerCache_BotReviewedHeadReadsCommentsOncePerTTL(t *testing.T) {
	reads := 0
	client := &mockGitHubClient{fetchIssueCommentsFn: func(o, r string, n int) ([]gh.Comment, error) {
		reads++
		return nil, nil
	}}
	eng := testEngineForMerge(t, client)
	eng.cfg.User = cuSelf
	start := time.Now()
	eng.SetClock(stubClock{t: start})
	item := catchUpItem(cuHead)
	// Each gate call also reads the PR's comments once for the durably-addressed review
	// IDs (durablyAddressedReviewIDs), independent of catch-up recognition: n calls cost n
	// of those reads, plus the marker lookup — one per TTL window, not one per call.
	const calls = 5
	for i := 0; i < calls; i++ {
		eng.feedbackGateBlocks(item, true, "landing decision")
	}
	if want := calls + 1; reads != want {
		t.Fatalf("FetchIssueComments called %d times over %d gate calls on one bot-reviewed head with no marker; want %d (one marker lookup)", reads, calls, want)
	}
	eng.SetClock(stubClock{t: start.Add(catchUpNoMarkerTTL + time.Second)})
	eng.feedbackGateBlocks(item, true, "landing decision")
	if want := calls + 1 + 2; reads != want {
		t.Fatalf("after the TTL FetchIssueComments called %d times in total; want %d (a second marker lookup)", reads, want)
	}
}

func TestCatchUpMarkerCache_FoundMarkerIsCachedAndErrorsAreNot(t *testing.T) {
	reads, fail := 0, true
	client := &mockGitHubClient{fetchIssueCommentsFn: func(o, r string, n int) ([]gh.Comment, error) {
		reads++
		if fail {
			return nil, errors.New("boom")
		}
		return []gh.Comment{markerComment(cuSelf, true, cuHead)}, nil
	}}
	eng := testEngineForMerge(t, client)
	eng.cfg.User = cuSelf
	start := time.Now()
	eng.SetClock(stubClock{t: start})
	item := catchUpItem(cuHead)
	if !eng.feedbackGateBlocks(item, true, "landing decision") {
		t.Fatal("a failed marker read must leave the findings actionable")
	}
	fail = false
	if eng.feedbackGateBlocks(item, true, "landing decision") {
		t.Fatal("after a successful read the pure catch-up head's bot review must not hold")
	}
	// A found marker never expires: no further reads, even far past the negative TTL.
	eng.SetClock(stubClock{t: start.Add(time.Hour)})
	before := reads
	eng.feedbackGateBlocks(item, true, "landing decision")
	if reads != before {
		t.Fatalf("a found marker was re-read (%d reads, want %d)", reads, before)
	}
}

func TestCatchUpMarkerCache_PostingTheMarkerInvalidatesANegativeEntry(t *testing.T) {
	var posted []gh.Comment
	client := &mockGitHubClient{fetchIssueCommentsFn: func(o, r string, n int) ([]gh.Comment, error) { return posted, nil }}
	eng := testEngineForMerge(t, client)
	eng.cfg.User = cuSelf
	eng.SetClock(stubClock{t: time.Now()})
	item := catchUpItem(cuHead)
	if !eng.feedbackGateBlocks(item, true, "landing decision") {
		t.Fatal("with no marker yet the bot review must be actionable")
	}
	posted = []gh.Comment{markerComment(cuSelf, true, cuHead)}
	eng.forgetCatchUpMarker("owner", "repo", 10, cuHead)
	if eng.feedbackGateBlocks(item, true, "landing decision") {
		t.Fatal("after the marker is posted and the cache entry forgotten, the review must not hold")
	}
}
