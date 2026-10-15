package pruefer

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

const (
	trEngine = "fabrik-dev[bot]"
	trBranch = "fabrik/merge-train/merge-train-main-1760000000"
	trM1Head = "1111111111111111111111111111111111111111"
	trM2Head = "2222222222222222222222222222222222222222"
)

// trFileDiff renders a one-file diff for path.
func trFileDiff(path, idx string, line int, added string) string {
	return fmt.Sprintf("diff --git a/%[1]s b/%[1]s\nindex %[2]s..%[3]s 100644\n--- a/%[1]s\n+++ b/%[1]s\n@@ -%[4]d,3 +%[4]d,4 @@ func f() {\n ctx1\n+%[5]s\n ctx2\n ctx3\n",
		path, idx[:7], idx[7:14], line, added)
}

type trFixture struct {
	t      *testing.T
	client *fakeReviewer
	cfg    Config
	pr     gh.PRDetails
	claude *mockClaudeInvoker
}

// newTRFixture: engine-authored trial PR #50 of two members (issues 11, 12 →
// PRs 101, 102), each touching its own file and each completely reviewed at its
// current head. The trial's diff is exactly the two members' patches.
func newTRFixture(t *testing.T) *trFixture {
	t.Helper()
	c := newFakeReviewer()
	c.botLogin = psBot
	m1 := trFileDiff("a.go", "1111111aaaaaaa", 10, "one")
	m2 := trFileDiff("b.go", "2222222bbbbbbb", 20, "two")
	c.prsByHead = map[string][]gh.PRDetails{
		"fabrik/issue-11": {{Number: 101, State: "open", HeadSHA: trM1Head, BaseRef: "main"}},
		"fabrik/issue-12": {{Number: 102, State: "open", HeadSHA: trM2Head, BaseRef: "main"}},
	}
	c.prDiffs = map[int]string{101: m1, 102: m2}
	c.prReviews = map[int][]gh.PRReview{
		101: {completeReview(trM1Head)},
		102: {completeReview(trM2Head)},
	}
	// The trial is forked from a newer base: same patches, different blobs and
	// line numbers.
	c.diff = trFileDiff("a.go", "3333333ccccccc", 410, "one") + trFileDiff("b.go", "4444444ddddddd", 520, "two")
	inv := okClaudeInvoker()
	return &trFixture{
		t: t, client: c, claude: inv,
		cfg: Config{PatchSkip: true, CatchUpMarkerAuthors: []string{trEngine}},
		pr: gh.PRDetails{
			Number: 50, Author: trEngine, HeadSHA: "5555555555555555555555555555555555555555", BaseRef: "main",
			HeadRefName: trBranch, Title: "chore(merge-train): trial integration for #11 #12",
			Body: "🏭 **Fabrik merge-train integration PR** (trial → main)\n\n- #11\n- #12\n\nCloses #11\nCloses #12\n\n<!-- fabrik-merge-train-batch -->",
		},
	}
}

func (f *trFixture) review() ReviewOutcome {
	clone, _ := fakeClone(f.t, nil)
	return ReviewPR(context.Background(), f.client, f.claude, clone, f.cfg, psBot, "o", "r", f.pr, nil)
}

func (f *trFixture) lastRequest() ReviewRequest {
	f.t.Helper()
	if len(f.claude.calls) == 0 {
		f.t.Fatal("claude was not invoked")
	}
	return f.claude.calls[len(f.claude.calls)-1]
}

func stagedPaths(s *StagedDiff) []string {
	var out []string
	if s == nil {
		return out
	}
	for _, e := range s.Entries {
		out = append(out, e.Path)
	}
	return out
}

func TestReviewPR_TrialPR_CleanIsSkipped(t *testing.T) {
	f := newTRFixture(t)
	out := f.review()
	if !out.Skipped || out.Reason != SkipTrialEquivalent {
		t.Fatalf("outcome = %+v, want Skipped with SkipTrialEquivalent", out)
	}
	if !out.Conclusive {
		t.Errorf("a clean skip must be conclusive")
	}
	if f.client.submitCallCount() != 0 || len(f.claude.calls) != 0 {
		t.Errorf("a skip must run and submit nothing")
	}
}

// Non-vacuity: the identical trial with the kill switch off is reviewed in full.
func TestReviewPR_TrialPR_KillSwitchReviewsEverything(t *testing.T) {
	f := newTRFixture(t)
	f.cfg.PatchSkip = false
	if out := f.review(); !out.Reviewed {
		t.Fatalf("outcome = %+v, want a review with patch_skip off", out)
	}
	if f.client.findCalls != 0 || f.client.compareCallCount() != 0 {
		t.Errorf("patch_skip off must not call the new client methods")
	}
	req := f.lastRequest()
	if len(req.OmittedReviewedPaths) != 0 || !reflect.DeepEqual(stagedPaths(req.Staged), []string{"a.go", "b.go"}) {
		t.Errorf("request = omitted %v staged %v, want the whole trial", req.OmittedReviewedPaths, stagedPaths(req.Staged))
	}
}

func TestReviewPR_TrialPR_ConflictEditReviewsOnlyTheDifference(t *testing.T) {
	f := newTRFixture(t)
	// Resolving a conflict changed b.go and added c.go; a.go is untouched.
	f.client.diff = trFileDiff("a.go", "3333333ccccccc", 410, "one") +
		trFileDiff("b.go", "4444444ddddddd", 520, "two, edited while resolving") +
		trFileDiff("c.go", "5555555eeeeeee", 3, "glue")
	out := f.review()
	if !out.Reviewed {
		t.Fatalf("outcome = %+v, want a review of the difference", out)
	}
	req := f.lastRequest()
	if !reflect.DeepEqual(req.OmittedReviewedPaths, []string{"a.go"}) {
		t.Errorf("OmittedReviewedPaths = %v, want [a.go]", req.OmittedReviewedPaths)
	}
	if got := stagedPaths(req.Staged); !reflect.DeepEqual(got, []string{"b.go", "c.go"}) {
		t.Errorf("staged paths = %v, want only the unreviewed [b.go c.go]", got)
	}
}

func TestReviewPR_TrialPR_MemberReviewedAtEarlierHeadWithSamePatch(t *testing.T) {
	f := newTRFixture(t)
	const old = "9999999999999999999999999999999999999999"
	f.client.prReviews[101] = []gh.PRReview{completeReview(old)}
	f.client.compareDiffs = map[string]string{old: trFileDiff("a.go", "6666666fffffff", 7, "one")}
	if out := f.review(); !out.Skipped || out.Reason != SkipTrialEquivalent {
		t.Fatalf("outcome = %+v, want SkipTrialEquivalent", out)
	}
	// ...but not when the member's patch moved since that review.
	f.client.compareDiffs = map[string]string{old: trFileDiff("a.go", "6666666fffffff", 7, "different")}
	if out := f.review(); !out.Reviewed {
		t.Fatalf("outcome = %+v, want a review: the member changed since it was reviewed", out)
	}
}

func TestReviewPR_TrialPR_FailsSafeToAFullReview(t *testing.T) {
	cases := map[string]func(f *trFixture){
		"member never reviewed": func(f *trFixture) { f.client.prReviews[102] = []gh.PRReview{} },
		"member review partial": func(f *trFixture) {
			r := completeReview(trM2Head)
			r.Body = "x\n\nPRUEFER_COVERAGE: partial — 1 of 2 diff entries not opened"
			f.client.prReviews[102] = []gh.PRReview{r}
		},
		"member PR not found":   func(f *trFixture) { f.client.prsByHead["fabrik/issue-12"] = nil },
		"member PR closed":      func(f *trFixture) { f.client.prsByHead["fabrik/issue-12"][0].State = "closed" },
		"member lookup fails":   func(f *trFixture) { f.client.findErr = fmt.Errorf("502") },
		"body names no members": func(f *trFixture) { f.pr.Body = "<!-- fabrik-merge-train-batch -->" },
		"member change dropped": func(f *trFixture) { f.client.diff = trFileDiff("a.go", "3333333ccccccc", 410, "one") },
		"binary trial diff": func(f *trFixture) {
			f.client.diff = "diff --git a/x.png b/x.png\nindex 1..2 100644\nBinary files a/x.png and b/x.png differ\n"
		},
		"all-shared files": func(f *trFixture) {
			f.client.prDiffs[102] = trFileDiff("a.go", "2222222bbbbbbb", 20, "one")
			f.client.diff = trFileDiff("a.go", "3333333ccccccc", 410, "one")
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newTRFixture(t)
			mutate(f)
			out := f.review()
			if !out.Reviewed {
				t.Fatalf("outcome = %+v, want a review", out)
			}
			if req := f.lastRequest(); len(req.OmittedReviewedPaths) != 0 {
				t.Errorf("OmittedReviewedPaths = %v, want none (a full review)", req.OmittedReviewedPaths)
			}
		})
	}
}

func TestReviewPR_TrialPR_FileTouchedByTwoMembersIsReviewed(t *testing.T) {
	f := newTRFixture(t)
	// Both members touch a.go; only b.go is exclusively member 2's.
	f.client.prDiffs[101] = trFileDiff("a.go", "1111111aaaaaaa", 10, "one")
	f.client.prDiffs[102] = trFileDiff("a.go", "2222222bbbbbbb", 30, "two-a") + trFileDiff("b.go", "2222222bbbbbbb", 20, "two")
	f.client.diff = trFileDiff("a.go", "3333333ccccccc", 410, "one two-a") + trFileDiff("b.go", "4444444ddddddd", 520, "two")
	out := f.review()
	if !out.Reviewed {
		t.Fatalf("outcome = %+v", out)
	}
	req := f.lastRequest()
	if !reflect.DeepEqual(req.OmittedReviewedPaths, []string{"b.go"}) || !reflect.DeepEqual(stagedPaths(req.Staged), []string{"a.go"}) {
		t.Errorf("omitted %v staged %v, want b.go withheld and a.go reviewed", req.OmittedReviewedPaths, stagedPaths(req.Staged))
	}
}

func TestReviewPR_TrialPR_RecognitionNeedsBothAuthorAndBranch(t *testing.T) {
	cases := map[string]func(f *trFixture){
		"author not an engine login":  func(f *trFixture) { f.pr.Author = "mallory" },
		"engine author, other branch": func(f *trFixture) { f.pr.HeadRefName = "feature/merge-train-main-1760000000" },
		"near-miss branch":            func(f *trFixture) { f.pr.HeadRefName = "fabrik/merge-train/merge-train-main-soon" },
		"no engine logins configured": func(f *trFixture) { f.cfg.CatchUpMarkerAuthors = nil },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newTRFixture(t)
			mutate(f)
			if out := f.review(); !out.Reviewed {
				t.Fatalf("outcome = %+v, want an ordinary full review", out)
			}
			if f.client.findCalls != 0 {
				t.Errorf("an unrecognised PR must cost no member lookups, got %d", f.client.findCalls)
			}
		})
	}
}

func TestReviewPR_TrialPR_ForceReviewBypasses(t *testing.T) {
	f := newTRFixture(t)
	f.client.comments = append(f.client.comments, gh.Comment{Author: "alice", Body: "/pruefer review"})
	if out := f.review(); !out.Reviewed {
		t.Fatalf("outcome = %+v, want /pruefer review to force a review", out)
	}
}

func TestTrialBranchRE(t *testing.T) {
	good := []string{
		"fabrik/merge-train/merge-train-main-1760000000",
		"fabrik/merge-train/merge-train-main-1760000000-t2",
		"fabrik/merge-train/merge-train-release-1.x-1760000000",
		"fabrik/merge-train/merge-train-release-2026-10-1760000000-t12",
	}
	bad := []string{
		"merge-train-main-1760000000",
		"fabrik/merge-train/merge-train-main-",
		"fabrik/merge-train/merge-train-main-17x",
		"fabrik/merge-train/merge-train-main-1760000000-x2",
		"fabrik/merge-train/merge-train-main-1760000000-t",
		"fabrik/issue-1",
		"fabrik/merge-train/other-main-1760000000",
	}
	for _, b := range good {
		if !trialBranchRE.MatchString(b) {
			t.Errorf("%q should match", b)
		}
	}
	for _, b := range bad {
		if trialBranchRE.MatchString(b) {
			t.Errorf("%q should not match", b)
		}
	}
}

func TestTrialMemberIssues(t *testing.T) {
	body := "- #5 — title\n\nCloses #11\nCloses #12  \nCloses #11\nsee Closes #99 inline\n"
	if got := trialMemberIssues(body); !reflect.DeepEqual(got, []int{11, 12}) {
		t.Errorf("trialMemberIssues = %v, want [11 12]", got)
	}
	if got := trialMemberIssues(strings.Repeat("x", 10)); len(got) != 0 {
		t.Errorf("got %v", got)
	}
}
