package engine

import (
	"errors"
	"strings"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

const (
	commitA = "aaaaaaa1111111"
	commitB = "bbbbbbb2222222"
)

var errTest = errors.New("review history unavailable")

var t0 = time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)

func rv(id int, author, state, commit string, minutes int) gh.PRReview {
	return gh.PRReview{Author: author, State: state, DatabaseID: id, CommitID: commit, SubmittedAt: t0.Add(time.Duration(minutes) * time.Minute)}
}

// kinds maps author → standing kind for a quick table comparison.
func standingOf(t *testing.T, history []gh.PRReview, head string, declared []string, author string) reviewerStandingKind {
	t.Helper()
	for _, s := range reviewerStandings(history, head, declared) {
		if reviewerKey(s.Author) == reviewerKey(author) {
			return s.Kind
		}
	}
	t.Fatalf("no standing for %s", author)
	return 0
}

func TestReviewerStandings_Table(t *testing.T) {
	bot := "pruefer[bot]"
	declared := []string{"pruefer"}
	tests := []struct {
		name     string
		history  []gh.PRReview
		head     string
		declared []string
		want     reviewerStandingKind
	}{
		{"declared: CR@A, COMMENTED@B, head B → clean (R2)",
			[]gh.PRReview{rv(1, bot, "CHANGES_REQUESTED", commitA, 0), rv(2, bot, "COMMENTED", commitB, 5)}, commitB, declared, standingClean},
		{"declared: CR@A, APPROVED@B, head B → clean",
			[]gh.PRReview{rv(1, bot, "CHANGES_REQUESTED", commitA, 0), rv(2, bot, "APPROVED", commitB, 5)}, commitB, declared, standingClean},
		{"undeclared: CR@A, COMMENTED@B, head B → awaiting re-review (R2 boundary)",
			[]gh.PRReview{rv(1, "carol", "CHANGES_REQUESTED", commitA, 0), rv(2, "carol", "COMMENTED", commitB, 5)}, commitB, declared, standingAwaitingReReview},
		{"undeclared with no declared set at all",
			[]gh.PRReview{rv(1, bot, "CHANGES_REQUESTED", commitA, 0), rv(2, bot, "COMMENTED", commitB, 5)}, commitB, nil, standingAwaitingReReview},
		{"CR@A, no review of B → awaiting re-review (R1)",
			[]gh.PRReview{rv(1, "carol", "CHANGES_REQUESTED", commitA, 0)}, commitB, nil, standingAwaitingReReview},
		{"declared: CR@A, no review of B → awaiting re-review",
			[]gh.PRReview{rv(1, bot, "CHANGES_REQUESTED", commitA, 0)}, commitB, declared, standingAwaitingReReview},
		{"APPROVED@A, head B → awaiting re-review (a verdict on old code is not a verdict on the head)",
			[]gh.PRReview{rv(1, "carol", "APPROVED", commitA, 0)}, commitB, nil, standingAwaitingReReview},
		{"CR@head → changes requested",
			[]gh.PRReview{rv(1, "carol", "CHANGES_REQUESTED", commitB, 0)}, commitB, nil, standingChangesRequested},
		{"declared: CR@head, nothing later → stands (declined-finding deadlock is out of scope)",
			[]gh.PRReview{rv(1, bot, "CHANGES_REQUESTED", commitB, 0)}, commitB, declared, standingChangesRequested},
		{"declared: CR@head then COMMENTED@head → clean",
			[]gh.PRReview{rv(1, bot, "CHANGES_REQUESTED", commitB, 0), rv(2, bot, "COMMENTED", commitB, 3)}, commitB, declared, standingClean},
		{"declared: COMMENTED@head then CR@head (CR later) → stands",
			[]gh.PRReview{rv(2, bot, "COMMENTED", commitB, 0), rv(1, bot, "CHANGES_REQUESTED", commitB, 3)}, commitB, declared, standingChangesRequested},
		{"undeclared: CR@head then COMMENTED@head → GitHub semantics, stands",
			[]gh.PRReview{rv(1, "carol", "CHANGES_REQUESTED", commitB, 0), rv(2, "carol", "COMMENTED", commitB, 3)}, commitB, nil, standingChangesRequested},
		{"declared clean pass, then the head moves again → awaiting re-review",
			[]gh.PRReview{rv(1, bot, "CHANGES_REQUESTED", commitA, 0), rv(2, bot, "COMMENTED", commitB, 5)}, "ccccccc3333333", declared, standingAwaitingReReview},
		{"only COMMENTED → no verdict",
			[]gh.PRReview{rv(1, "carol", "COMMENTED", commitB, 0)}, commitB, nil, standingNoVerdict},
		{"DISMISSED ends the verdict",
			[]gh.PRReview{rv(1, "carol", "CHANGES_REQUESTED", commitA, 0), rv(2, "carol", "DISMISSED", commitA, 3)}, commitB, nil, standingNoVerdict},
		{"PENDING never counts",
			[]gh.PRReview{rv(1, "carol", "COMMENTED", commitB, 0), rv(2, "carol", "PENDING", commitB, 1)}, commitB, nil, standingNoVerdict},
		{"R5: unknown head keeps a change request blocking",
			[]gh.PRReview{rv(1, bot, "CHANGES_REQUESTED", commitA, 0), rv(2, bot, "COMMENTED", commitB, 5)}, "", declared, standingChangesRequested},
		{"R5: change request with no commit keeps blocking",
			[]gh.PRReview{rv(1, "carol", "CHANGES_REQUESTED", "", 0)}, commitB, nil, standingChangesRequested},
		{"R5: declared, CR with no commit, COMMENTED at head → still blocks (cannot show the CR stale)",
			[]gh.PRReview{rv(1, bot, "CHANGES_REQUESTED", "", 0), rv(2, bot, "COMMENTED", commitB, 5)}, commitB, declared, standingChangesRequested},
		{"unknown head: APPROVED stays non-blocking as before",
			[]gh.PRReview{rv(1, "carol", "APPROVED", commitA, 0)}, "", nil, standingClean},
		{"same-second tie falls to the larger review ID",
			[]gh.PRReview{rv(5, bot, "COMMENTED", commitB, 0), rv(4, bot, "CHANGES_REQUESTED", commitB, 0)}, commitB, declared, standingClean},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			author := tt.history[0].Author
			if got := standingOf(t, tt.history, tt.head, tt.declared, author); got != tt.want {
				t.Errorf("standing = %d, want %d", got, tt.want)
			}
		})
	}
}

// Both REST ("x[bot]") and GraphQL ("x") login shapes are one reviewer.
func TestReviewerStandings_BotSuffixIsOneReviewer(t *testing.T) {
	history := []gh.PRReview{rv(1, "pruefer[bot]", "CHANGES_REQUESTED", commitA, 0), rv(2, "pruefer", "COMMENTED", commitB, 5)}
	got := reviewerStandings(history, commitB, []string{"pruefer"})
	if len(got) != 1 || got[0].Kind != standingClean {
		t.Fatalf("standings = %+v, want one clean reviewer", got)
	}
}

// SC-004: an awaiting reviewer's reason never says they "requested changes".
func TestReviewerStanding_AwaitingReasonNeverSaysRequestedChanges(t *testing.T) {
	s := reviewerStandings([]gh.PRReview{rv(1, "carol", "CHANGES_REQUESTED", commitA, 0)}, commitB, nil)[0]
	if s.Kind != standingAwaitingReReview {
		t.Fatalf("kind = %d", s.Kind)
	}
	r := s.reason()
	if strings.Contains(r, "requested changes") || !strings.Contains(r, "awaiting re-review") || !strings.Contains(r, "carol") {
		t.Errorf("reason = %q", r)
	}
	s = reviewerStandings([]gh.PRReview{rv(1, "carol", "CHANGES_REQUESTED", commitB, 0)}, commitB, nil)[0]
	if !strings.Contains(s.reason(), "carol requested changes") {
		t.Errorf("standing request reason = %q", s.reason())
	}
}

func TestStandingEvidence_KeepsLatestFormalAndLaterComment(t *testing.T) {
	history := []gh.PRReview{
		rv(1, "bot", "COMMENTED", commitA, 0),
		rv(2, "bot", "CHANGES_REQUESTED", commitA, 1),
		rv(3, "bot", "COMMENTED", commitA, 2),
		rv(4, "bot", "COMMENTED", commitB, 3),
	}
	ev := standingEvidence(history)
	if len(ev) != 2 || ev[0].DatabaseID != 2 || ev[1].DatabaseID != 4 {
		t.Fatalf("evidence = %+v", ev)
	}
	// Evidence is enough to reproduce the standings of the full history.
	for _, declared := range [][]string{nil, {"bot"}} {
		a := reviewerStandings(history, commitB, declared)
		b := reviewerStandings(ev, commitB, declared)
		if a[0].Kind != b[0].Kind {
			t.Errorf("declared=%v: full %d vs evidence %d", declared, a[0].Kind, b[0].Kind)
		}
	}
}

// ---- gate-level scenarios ---------------------------------------------------

// gateScenario drives checkReviewGate and reviewGateBlocksLanding's hold over one
// review history. collapsed is the (GraphQL/Store-shaped) list the item carries.
type gateScenario struct {
	history   []gh.PRReview
	collapsed []gh.PRReview
	head      string
	declared  *[]string
	decision  string
	histErr   error
	noHead    bool
}

func (g gateScenario) run(t *testing.T, tweak func(*Engine)) (advanceBlocked bool, landing reviewHold) {
	t.Helper()
	client := &mockGitHubClient{
		fetchPRReviewDecisionFn: func(string, string, int) (string, error) { return g.decision, nil },
		fetchPRReviewHistoryFn: func(string, string, int) ([]gh.PRReview, error) {
			return g.history, g.histErr
		},
		fetchPRDetailsFn: func(string, string, int) (*gh.PRDetails, error) {
			if g.noHead {
				return &gh.PRDetails{Number: 55, State: "open"}, nil
			}
			return &gh.PRDetails{Number: 55, State: "open", HeadSHA: g.head}, nil
		},
	}
	eng := reviewTestEngine(t, client)
	if tweak != nil {
		tweak(eng)
	}
	item := gh.ProjectItem{Number: 10, Repo: "owner/repo", LinkedPRNumber: 55, LinkedPRHeadSHA: g.head, LinkedPRReviews: g.collapsed}
	mirrorLiveReviews(client, item)
	stage := &stages.Stage{Name: "Implement", WaitForReviews: boolPtr(true), ReviewAuthority: "authoritative", ExpectedReviewers: g.declared}
	blocked, _, _, _ := eng.checkReviewGate(&gh.ProjectBoard{ProjectID: "PVT_1"}, item, stage)
	return blocked, eng.reviewGateHold(item, stage, "owner", "repo")
}

func declaredSet(names ...string) *[]string { return &names }

func botHistory(commentedAt string) []gh.PRReview {
	return []gh.PRReview{
		rv(1, "pruefer[bot]", "CHANGES_REQUESTED", commitA, 0),
		rv(2, "pruefer[bot]", "COMMENTED", commentedAt, 5),
	}
}

// Acceptance 1 (the reported sequence) and 2 (the R2 boundary). The sticky REST
// collapse in `collapsed` is what the gate used to decide on.
func TestAuthoritativeGate_DeclaredBotCleanReReviewClears(t *testing.T) {
	g := gateScenario{
		history:   botHistory(commitB),
		collapsed: []gh.PRReview{rv(1, "pruefer[bot]", "CHANGES_REQUESTED", commitA, 0)}, // sticky collapse
		head:      commitB,
		declared:  declaredSet("pruefer"),
	}
	blocked, landing := g.run(t, nil)
	if blocked || landing.Blocked {
		t.Errorf("declared bot's clean re-review of the head: advance blocked=%v landing=%+v, want both clear", blocked, landing)
	}

	// Non-vacuity: with the head-aware verdict neutralised (today's rule) the
	// same input blocks at both sites.
	blocked, landing = g.run(t, func(e *Engine) { e.SetReviewStandingDisabledForTest(true) })
	if !blocked || !landing.Blocked {
		t.Errorf("neutralised: advance blocked=%v landing=%+v, want both blocked — the test is vacuous otherwise", blocked, landing)
	}
}

func TestAuthoritativeGate_UndeclaredReviewerStillBlocks(t *testing.T) {
	g := gateScenario{
		history:   []gh.PRReview{rv(1, "carol", "CHANGES_REQUESTED", commitA, 0), rv(2, "carol", "COMMENTED", commitB, 5)},
		collapsed: []gh.PRReview{rv(1, "carol", "CHANGES_REQUESTED", commitA, 0)},
		head:      commitB,
		declared:  declaredSet("pruefer"),
	}
	blocked, landing := g.run(t, nil)
	if !blocked || !landing.Blocked {
		t.Fatalf("undeclared reviewer: advance blocked=%v landing=%+v, want both blocked", blocked, landing)
	}
	if strings.Contains(landing.Msg, "requested changes") || !strings.Contains(landing.Msg, "awaiting re-review") {
		t.Errorf("landing message = %q, want an awaiting re-review reason", landing.Msg)
	}
}

// Acceptance 3.
func TestAuthoritativeGate_AwaitingReReviewBlocksWithHonestReason(t *testing.T) {
	g := gateScenario{
		history:   []gh.PRReview{rv(1, "carol", "CHANGES_REQUESTED", commitA, 0)},
		collapsed: []gh.PRReview{rv(1, "carol", "CHANGES_REQUESTED", commitA, 0)},
		head:      commitB,
	}
	blocked, landing := g.run(t, nil)
	if !blocked || !landing.Blocked {
		t.Fatalf("advance blocked=%v landing=%+v, want both blocked", blocked, landing)
	}
	if strings.Contains(landing.Msg, "requested changes") || !strings.Contains(landing.Msg, "awaiting re-review") {
		t.Errorf("landing message = %q", landing.Msg)
	}
}

// A stale APPROVED no longer counts toward the head.
func TestAuthoritativeGate_StaleApprovalAwaitsReReview(t *testing.T) {
	g := gateScenario{
		history:   []gh.PRReview{rv(1, "carol", "APPROVED", commitA, 0)},
		collapsed: []gh.PRReview{rv(1, "carol", "APPROVED", commitA, 0)},
		head:      commitB,
	}
	if blocked, landing := g.run(t, nil); !blocked || !landing.Blocked {
		t.Errorf("stale approval: advance blocked=%v landing=%+v, want both blocked", blocked, landing)
	}
	if blocked, landing := g.run(t, func(e *Engine) { e.SetReviewStandingDisabledForTest(true) }); blocked || landing.Blocked {
		t.Errorf("neutralised: stale approval blocked=%v landing=%+v, want clear (today's rule)", blocked, landing)
	}
}

// Acceptance 4 (R4): a non-empty reviewDecision decides, and the history is not even read.
func TestAuthoritativeGate_ReviewDecisionStillDecides(t *testing.T) {
	history := []gh.PRReview{rv(1, "carol", "CHANGES_REQUESTED", commitA, 0)} // would block as awaiting
	g := gateScenario{history: history, collapsed: history, head: commitB, decision: "APPROVED"}
	if blocked, landing := g.run(t, nil); blocked || landing.Blocked {
		t.Errorf("reviewDecision=APPROVED: blocked=%v landing=%+v, want clear", blocked, landing)
	}
	clean := []gh.PRReview{rv(1, "carol", "COMMENTED", commitB, 0)}
	g = gateScenario{history: clean, collapsed: clean, head: commitB, decision: "CHANGES_REQUESTED"}
	if blocked, landing := g.run(t, nil); !blocked || !landing.Blocked {
		t.Errorf("reviewDecision=CHANGES_REQUESTED: blocked=%v landing=%+v, want blocked", blocked, landing)
	}

	// The history read is skipped under a branch-protection decision.
	reads := 0
	client := &mockGitHubClient{fetchPRReviewHistoryFn: func(string, string, int) ([]gh.PRReview, error) { reads++; return history, nil }}
	eng := reviewTestEngine(t, client)
	if ok, _ := eng.authorityVerdict("owner", "repo", 10, 55, "APPROVED", history, gh.ProjectItem{Number: 10}, &stages.Stage{}); !ok || reads != 0 {
		t.Errorf("ok=%v reads=%d, want satisfied with no history read", ok, reads)
	}
}

// Acceptance 6 (R5).
func TestAuthoritativeGate_UnknownsFailClosed(t *testing.T) {
	declared := declaredSet("pruefer")
	t.Run("unknown head", func(t *testing.T) {
		g := gateScenario{history: botHistory(commitB), collapsed: botHistory(commitB)[1:], head: commitB, declared: declared, noHead: true}
		if blocked, landing := g.run(t, nil); !blocked || !landing.Blocked {
			t.Errorf("blocked=%v landing=%+v, want blocked", blocked, landing)
		}
	})
	t.Run("review without a commit", func(t *testing.T) {
		h := []gh.PRReview{rv(1, "pruefer[bot]", "CHANGES_REQUESTED", "", 0), rv(2, "pruefer[bot]", "COMMENTED", commitB, 5)}
		g := gateScenario{history: h, collapsed: h[1:], head: commitB, declared: declared}
		if blocked, landing := g.run(t, nil); !blocked || !landing.Blocked {
			t.Errorf("blocked=%v landing=%+v, want blocked", blocked, landing)
		}
	})
	t.Run("unreadable history", func(t *testing.T) {
		g := gateScenario{history: botHistory(commitB), collapsed: botHistory(commitB)[1:], head: commitB, declared: declared, histErr: errTest}
		if blocked, landing := g.run(t, nil); !blocked || !landing.Blocked {
			t.Errorf("blocked=%v landing=%+v, want blocked", blocked, landing)
		}
	})
	t.Run("no reviews, unknown head clears as before", func(t *testing.T) {
		h := []gh.PRReview{rv(1, "carol", "COMMENTED", commitB, 0)}
		g := gateScenario{history: h, collapsed: h, head: commitB, noHead: true}
		if blocked, landing := g.run(t, nil); blocked || landing.Blocked {
			t.Errorf("blocked=%v landing=%+v, want clear", blocked, landing)
		}
	})
}

// A GraphQL-shaped collapsed list that shows only a COMMENTED at head must not
// hide an undeclared reviewer's standing request (the dual-source fail-open path).
func TestAuthoritativeGate_CollapsedCommentedCannotHideUndeclaredRequest(t *testing.T) {
	h := []gh.PRReview{rv(1, "carol", "CHANGES_REQUESTED", commitB, 0), rv(2, "carol", "COMMENTED", commitB, 5)}
	g := gateScenario{history: h, collapsed: h[1:], head: commitB}
	if blocked, landing := g.run(t, nil); !blocked || !landing.Blocked {
		t.Errorf("blocked=%v landing=%+v, want blocked", blocked, landing)
	}
}

// ---- R3: gate and reviewer view agree ----------------------------------------

func TestReviewerView_AgreesWithGateStandings(t *testing.T) {
	bot := "pruefer[bot]"
	declared := []string{"pruefer"}
	histories := map[string][]gh.PRReview{
		"declared clean re-review":     {rv(1, bot, "CHANGES_REQUESTED", commitA, 0), rv(2, bot, "COMMENTED", commitB, 5)},
		"undeclared commented at head": {rv(1, "carol", "CHANGES_REQUESTED", commitA, 0), rv(2, "carol", "COMMENTED", commitB, 5)},
		"awaiting re-review":           {rv(1, "carol", "CHANGES_REQUESTED", commitA, 0)},
		"stale approval":               {rv(1, "carol", "APPROVED", commitA, 0)},
		"standing request at head":     {rv(1, "carol", "CHANGES_REQUESTED", commitB, 0)},
		"clean approval at head":       {rv(1, "carol", "APPROVED", commitB, 0)},
		"mixed":                        {rv(1, bot, "COMMENTED", commitB, 0), rv(2, "carol", "CHANGES_REQUESTED", commitA, 1)},
	}
	for name, history := range histories {
		t.Run(name, func(t *testing.T) {
			e := apiEngine(t, 0)
			e.cfg.Stages = []*stages.Stage{{Name: "Validate", ExpectedReviewers: &declared}}
			// The Store holds the latest review per author plus the standing
			// evidence the gate's live read wrote.
			collapsed := latestPerAuthorForTest(history)
			e.store.Apply(itemstate.IssueOpened{Item: gh.ProjectItem{
				ID: "I_B", Number: 1, Repo: "owner/repo", Title: "t", Status: "Validate",
				Labels: []string{"stage:Validate:complete"}, LinkedPRNumber: 34, LinkedPRHeadSHA: commitB, LinkedPRReviews: collapsed,
			}})
			e.store.Apply(itemstate.PRDetailsUpdated{Repo: "owner/repo", Number: 1, PRNumber: 34, Title: "pr", State: "open"})
			e.store.Apply(itemstate.PRHeadSHAUpdated{Repo: "owner/repo", Number: 1, LinkedPRNum: 34, SHA: commitB})
			e.store.Apply(itemstate.PRStandingReviewsRead{Repo: "owner/repo", Number: 1, PRNumber: 34, Reviews: standingEvidence(history)})

			snap, ok := e.store.Peek("owner/repo", 1)
			if !ok {
				t.Fatal("item not in the store")
			}
			st := snap.State()
			byReviewer := map[string]bool{} // reviewer key → clean per the view
			for _, v := range e.reviewerVerdicts(&st) {
				byReviewer[reviewerKey(v.Reviewer)] = v.CleanPass.Valid && v.CleanPass.V
			}
			for _, s := range reviewerStandings(history, commitB, declared) {
				gateBlocks := s.blocks()
				viewClean, seen := byReviewer[reviewerKey(s.Author)]
				if !seen {
					t.Fatalf("reviewer %s missing from the view", s.Author)
				}
				if gateBlocks && viewClean {
					t.Errorf("%s: the gate blocks (%s) but fabrik_status reports a clean pass", s.Author, s.reason())
				}
				if !gateBlocks && !viewClean {
					t.Errorf("%s: the gate does not block but fabrik_status reports no clean pass", s.Author)
				}
			}
		})
	}
}

// latestPerAuthorForTest is the GraphQL-shaped list: the latest submission per
// author, whatever its state.
func latestPerAuthorForTest(history []gh.PRReview) []gh.PRReview {
	best := map[string]gh.PRReview{}
	var order []string
	for _, r := range history {
		b, ok := best[r.Author]
		if !ok {
			order = append(order, r.Author)
		}
		if !ok || r.SubmittedAt.After(b.SubmittedAt) {
			best[r.Author] = r
		}
	}
	out := make([]gh.PRReview, 0, len(order))
	for _, a := range order {
		out = append(out, best[a])
	}
	return out
}
