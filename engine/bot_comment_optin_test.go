package engine

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

func botComment(id string, dbID int, author string) gh.Comment {
	return gh.Comment{ID: id, DatabaseID: dbID, Author: author, Body: "Deployment ready for preview", FromPR: 34}
}

func TestFilterNewComments_BotRule(t *testing.T) {
	never := func(string) bool { return false }
	all := func(string) bool { return true }
	vercel := botComment("c1", 1, "vercel[bot]")
	human := gh.Comment{ID: "c2", DatabaseID: 2, Author: "alice", Body: "please change X"}
	noAuthor := gh.Comment{ID: "c3", DatabaseID: 3, Body: "deleted account"}
	notProcessed := func(string) bool { return false }

	cases := []struct {
		name string
		in   []gh.Comment
		opt  func(string) bool
		want int
	}{
		{"undeclared bot dropped", []gh.Comment{vercel}, never, 0},
		{"nil predicate opts nobody in", []gh.Comment{vercel}, nil, 0},
		{"opted-in bot kept", []gh.Comment{vercel}, all, 1},
		{"human kept regardless", []gh.Comment{human}, never, 1},
		{"human kept with nil predicate", []gh.Comment{human}, nil, 1},
		{"empty author is not a bot", []gh.Comment{noAuthor}, never, 1},
		{"mixed: only human survives", []gh.Comment{vercel, human}, never, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := filterNewComments(c.in, notProcessed, c.opt); len(got) != c.want {
				t.Errorf("got %d comments, want %d", len(got), c.want)
			}
		})
	}

	// An opted-in bot's known service notice is still excluded (FR-003).
	notice := gh.Comment{ID: "c4", DatabaseID: 4, Author: "gemini-code-assist[bot]", Body: "You have reached your daily quota limit."}
	if got := filterNewComments([]gh.Comment{notice}, notProcessed, all); len(got) != 0 {
		t.Errorf("opted-in bot's service notice must stay excluded, got %v", got)
	}
}

func TestBotCommentOptIn(t *testing.T) {
	stagesWith := func(reviewers []string, bots []string) []*stages.Stage {
		v := &stages.Stage{Name: "Validate", Order: 4, Prompt: "validate", FeedbackBots: bots}
		if reviewers != nil {
			v.ExpectedReviewers = &reviewers
		}
		return []*stages.Stage{
			{Name: "Implement", Order: 3, Prompt: "implement"},
			v,
			{Name: "Queued", Order: 5, HoldingStage: true},
			{Name: "Done", Order: 6, Prompt: "done"},
		}
	}
	t.Run("undeclared", func(t *testing.T) {
		e := testEngineWithStages(t, landingClient(), stagesWith(nil, nil))
		if e.botCommentOptIn("Validate", 1, nil)("vercel[bot]") {
			t.Error("an undeclared bot must not be opted in")
		}
	})
	t.Run("expected_reviewers opts in, with and without [bot]", func(t *testing.T) {
		e := testEngineWithStages(t, landingClient(), stagesWith([]string{"handarbeit-pruefer"}, nil))
		opt := e.botCommentOptIn("Validate", 1, nil)
		if !opt("handarbeit-pruefer") || !opt("handarbeit-pruefer[bot]") || !opt("Handarbeit-Pruefer[bot]") {
			t.Error("a declared reviewer must be opted in under every spelling")
		}
		if opt("vercel[bot]") {
			t.Error("another bot must stay out")
		}
	})
	t.Run("feedback_bots opts in", func(t *testing.T) {
		e := testEngineWithStages(t, landingClient(), stagesWith(nil, []string{"vercel"}))
		opt := e.botCommentOptIn("Validate", 1, nil)
		if !opt("vercel[bot]") || !opt("vercel") {
			t.Error("a feedback bot must be opted in")
		}
		if opt("snyk-bot") {
			t.Error("only the named bot is opted in")
		}
	})
	t.Run("label override of expected_reviewers decides", func(t *testing.T) {
		e := testEngineWithStages(t, landingClient(), stagesWith([]string{"handarbeit-pruefer"}, nil))
		if e.botCommentOptIn("Validate", 1, []string{"expected-reviewers:none"})("handarbeit-pruefer[bot]") {
			t.Error("expected-reviewers:none must withdraw the declaration")
		}
		// feedback_bots is not label-overridable.
		e2 := testEngineWithStages(t, landingClient(), stagesWith(nil, []string{"vercel"}))
		if !e2.botCommentOptIn("Validate", 1, []string{"expected-reviewers:none"})("vercel[bot]") {
			t.Error("feedback_bots must survive the expected-reviewers label")
		}
	})
	t.Run("stage is per Status", func(t *testing.T) {
		e := testEngineWithStages(t, landingClient(), stagesWith(nil, []string{"vercel"}))
		if e.botCommentOptIn("Implement", 1, nil)("vercel[bot]") {
			t.Error("Implement declares nothing")
		}
		if e.botCommentOptIn("NoSuchColumn", 1, nil)("vercel[bot]") {
			t.Error("an unknown status declares nothing")
		}
	})
	t.Run("holding stage resolves to the validating stage", func(t *testing.T) {
		e := testEngineWithStages(t, landingClient(), stagesWith(nil, []string{"vercel"}))
		if !e.botCommentOptIn("Queued", 1, nil)("vercel[bot]") {
			t.Error("a Queued item must decide as Validate does (#2525)")
		}
		e.SetHoldingStageReviewContextDisabledForTest(true)
		if e.botCommentOptIn("Queued", 1, nil)("vercel[bot]") {
			t.Error("seam on must resolve against the holding stage itself")
		}
	})
	t.Run("seam opts everyone in", func(t *testing.T) {
		e := testEngineWithStages(t, landingClient(), stagesWith(nil, nil))
		e.SetBotCommentRuleDisabledForTest(true)
		if !e.botCommentOptIn("Validate", 1, nil)("vercel[bot]") {
			t.Error("the seam must restore the pre-#2552 behaviour")
		}
	})
}

// Every reader of the funnel — and the writers that would otherwise touch a
// skipped comment — must agree on a bot comment (#2552, R3), as
// TestCleanReviewAgreesAcrossReaders does for review bodies.
func TestBotCommentOptInAgreesAcrossReaders(t *testing.T) {
	type fixture struct {
		name    string
		status  string
		labels  []string
		author  string
		reviews []string
		bots    []string
		want    bool // pending feedback expected
	}
	cases := []fixture{
		{name: "undeclared bot", status: "Validate", author: "vercel[bot]", want: false},
		{name: "feedback_bots bot", status: "Validate", author: "vercel[bot]", bots: []string{"vercel"}, want: true},
		{name: "declared reviewer bot", status: "Validate", author: "vercel[bot]", reviews: []string{"vercel"}, want: true},
		{name: "second, undeclared bot", status: "Validate", author: "snyk-bot", bots: []string{"vercel"}, want: false},
		{name: "label withdraws declared reviewer", status: "Validate", author: "vercel[bot]", reviews: []string{"vercel"}, labels: []string{"expected-reviewers:none"}, want: false},
		{name: "human", status: "Validate", author: "alice", want: true},
		{name: "queued undeclared", status: "Queued", author: "vercel[bot]", want: false},
		{name: "queued feedback_bots", status: "Queued", author: "vercel[bot]", bots: []string{"vercel"}, want: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			validate := &stages.Stage{Name: "Validate", Order: 4, Prompt: "validate", FeedbackBots: c.bots}
			if c.reviews != nil {
				validate.ExpectedReviewers = &c.reviews
			}
			eng := testEngineWithStages(t, landingClient(), []*stages.Stage{
				{Name: "Implement", Order: 3, Prompt: "implement"},
				validate,
				{Name: "Queued", Order: 5, HoldingStage: true},
				{Name: "Done", Order: 6, Prompt: "done"},
			})
			cm := botComment("IC_1", 901, c.author)
			item := feedbackItem(func(i *gh.ProjectItem) {
				i.Status = c.status
				i.Labels = c.labels
				i.LinkedPRNumber = 34
				i.Comments = []gh.Comment{cm}
			})
			eng.store.Apply(itemstate.IssueOpened{Item: item})
			snap, ok := eng.store.Peek("owner/repo", 1)
			if !ok {
				t.Fatal("item not in store")
			}
			st := snap.State()

			dispatcher := len(eng.findNewComments(item)) > 0
			cached := eng.cachedPendingFeedback(snap, &st).any()
			live := eng.feedbackGateBlocks(item, true, "advance")
			// Zero CreatedAt is never "arrived", so give the comment a time.
			stArr := st
			stArr.Comments = []gh.Comment{func() gh.Comment { x := cm; x.CreatedAt = time.Now(); return x }()}
			arrived := eng.cachedFeedbackArrivedSince(&stArr, time.Time{}.Add(time.Second)).any()
			if dispatcher != c.want || cached != c.want || live != c.want || arrived != c.want {
				t.Errorf("dispatcher=%v cached=%v gate=%v arrived-since=%v, want all %v", dispatcher, cached, live, arrived, c.want)
			}

			// Writers: a comment the funnel does not report as pending is left
			// alone when it is a skipped bot (R5), and reacted to otherwise.
			client := eng.client.(*mockGitHubClient)
			client.addCommentReactionCalls = nil
			eng.markCommentsSeenByStage(item, []gh.Comment{cm})
			eng.settleBotServiceNoticesForItem(item)
			isSkippedBot := gh.IsBotLogin(c.author) && !c.want
			if isSkippedBot {
				if len(client.addCommentReactionCalls) != 0 {
					t.Errorf("a skipped bot comment must get no reaction, got %v", client.addCommentReactionCalls)
				}
				after, _ := eng.store.Peek("owner/repo", 1)
				if !after.CommentProcessed("IC_1").IsZero() {
					t.Error("a skipped bot comment must not be marked processed")
				}
			}
		})
	}
}

// A skipped bot comment is never watermarked, so repeated polling must neither
// report it as feedback nor re-admit the item for it.
func TestUndeclaredBotComment_NoReadmission(t *testing.T) {
	eng := testEngineWithStages(t, landingClient(), []*stages.Stage{
		{Name: "Implement", Order: 3, Prompt: "implement"},
		{Name: "Validate", Order: 4, Prompt: "validate"},
		{Name: "Done", Order: 6, Prompt: "done"},
	})
	cm := botComment("IC_1", 901, "vercel[bot]")
	cm.CreatedAt = time.Now()
	item := feedbackItem(func(i *gh.ProjectItem) {
		i.Status = "Validate"
		i.LinkedPRNumber = 34
		i.Comments = []gh.Comment{cm}
	})
	eng.store.Apply(itemstate.IssueOpened{Item: item})
	for n := 0; n < 5; n++ {
		if got := eng.findNewComments(item); len(got) != 0 {
			t.Fatalf("poll %d: undeclared bot comment reported as feedback: %v", n, got)
		}
		if eng.feedbackGateBlocks(item, true, "advance") {
			t.Fatalf("poll %d: gate held on an undeclared bot comment", n)
		}
	}
	snap, _ := eng.store.Peek("owner/repo", 1)
	st := snap.State()
	if eng.cachedFeedbackArrivedSince(&st, time.Time{}.Add(time.Second)).any() {
		t.Error("an undeclared bot comment must not count as arrived feedback")
	}
}

// Review threads, review bodies and CHANGES_REQUESTED are untouched by the
// bot-comment rule (R4).
func TestBotCommentRule_LeavesReviewsAlone(t *testing.T) {
	eng := testEngineWithStages(t, landingClient(), []*stages.Stage{
		{Name: "Implement", Order: 3, Prompt: "implement"},
		{Name: "Validate", Order: 4, Prompt: "validate"},
		{Name: "Done", Order: 6, Prompt: "done"},
	})
	thread := gh.Comment{ID: "PRRC_t", DatabaseID: 31, ReviewThreadID: "RT_1", Author: "copilot-pull-request-reviewer[bot]", Body: "fix this"}
	cr := gh.PRReview{Author: "vercel[bot]", State: "CHANGES_REQUESTED", Body: "no", DatabaseID: 77, NodeID: "PRR_x"}
	for name, mod := range map[string]func(*gh.ProjectItem){
		"bot review thread":  func(i *gh.ProjectItem) { i.LinkedPRReviewThreadComments = []gh.Comment{thread} },
		"bot change request": func(i *gh.ProjectItem) { i.LinkedPRReviews = []gh.PRReview{cr} },
	} {
		t.Run(name, func(t *testing.T) {
			item := feedbackItem(func(i *gh.ProjectItem) { i.Status = "Validate"; i.LinkedPRNumber = 34; mod(i) })
			eng.store.Apply(itemstate.IssueOpened{Item: item})
			if !eng.feedbackGateBlocks(item, true, "advance") {
				t.Error("an undeclared bot's thread or change request must still hold the gate")
			}
		})
	}
}

// Every production call of filterNewComments must take its opt-in predicate from
// botCommentOptIn, so a new reader cannot silently disagree with the others.
func TestFilterNewCommentsCallersPassTheOptIn(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	call := regexp.MustCompile(`filterNewComments\(`)
	callers := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		src := string(raw)
		for _, loc := range call.FindAllStringIndex(src, -1) {
			start := loc[0]
			if strings.HasPrefix(src[max(0, start-5):start], "func ") {
				continue // the definition
			}
			if line := lineAt(src, start); strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			callers++
			// The statement ends at the end of its line plus one continuation line.
			end := min(len(src), start+400)
			stmt := src[start:end]
			if nl := strings.Index(stmt, "\n\n"); nl >= 0 {
				stmt = stmt[:nl]
			}
			if !strings.Contains(stmt, "botCommentOptIn(") && !strings.Contains(stmt, "botOptedIn") {
				t.Errorf("%s: filterNewComments call does not pass Engine.botCommentOptIn: %.120s", f, stmt)
			}
		}
	}
	if callers < 4 {
		t.Errorf("expected at least the four known callers, found %d", callers)
	}
}

func lineAt(src string, idx int) string {
	s := strings.LastIndex(src[:idx], "\n") + 1
	e := strings.Index(src[idx:], "\n")
	if e < 0 {
		return src[s:]
	}
	return src[s : idx+e]
}
