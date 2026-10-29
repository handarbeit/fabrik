package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

const cleanMarker = "<!-- pruefer:findings inline=0 body=0 coverage=complete -->"

func TestParseFindingsMarker(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		inline, bd int
		ok         bool
	}{
		{"clean", "text\n\n" + cleanMarker, 0, 0, true},
		{"clean, trailing newline", "text\n\n" + cleanMarker + "\n", 0, 0, true},
		{"non-zero inline", "x\n\n<!-- pruefer:findings inline=2 body=0 coverage=complete -->", 2, 0, true},
		{"non-zero body", "x\n\n<!-- pruefer:findings inline=0 body=3 coverage=complete -->", 0, 3, true},
		{"extra whitespace", "x\n\n  <!--   pruefer:findings   inline=0   body=0   coverage=complete   -->  ", 0, 0, true},
		{"extra trailing keys", "x\n\n<!-- pruefer:findings inline=0 body=0 coverage=complete future=1 -->", 0, 0, true},
		{"malformed coverage is ignored", "x\n\n<!-- pruefer:findings inline=0 body=0 coverage=??? -->", 0, 0, true},
		{"coverage absent", "x\n\n<!-- pruefer:findings inline=0 body=0 -->", 0, 0, true},
		{"unknown inline", "x\n\n<!-- pruefer:findings inline=unknown body=0 coverage=complete -->", 0, 0, false},
		{"unknown body", "x\n\n<!-- pruefer:findings inline=0 body=unknown coverage=complete -->", 0, 0, false},
		{"missing body key", "x\n\n<!-- pruefer:findings inline=0 coverage=complete -->", 0, 0, false},
		{"missing inline key", "x\n\n<!-- pruefer:findings body=0 coverage=complete -->", 0, 0, false},
		{"duplicate key", "x\n\n<!-- pruefer:findings inline=0 inline=0 body=0 -->", 0, 0, false},
		{"negative", "x\n\n<!-- pruefer:findings inline=-1 body=0 -->", 0, 0, false},
		{"plus sign", "x\n\n<!-- pruefer:findings inline=+0 body=0 -->", 0, 0, false},
		{"non-integer", "x\n\n<!-- pruefer:findings inline=0.5 body=0 -->", 0, 0, false},
		{"no marker", "Looks good to me", 0, 0, false},
		{"empty", "", 0, 0, false},
		{"duplicated marker", cleanMarker + "\n\n" + cleanMarker, 0, 0, false},
		{"clean quoted earlier, findings last", cleanMarker + "\n\n<!-- pruefer:findings inline=1 body=0 -->", 0, 0, false},
		{"marker not on the last line", cleanMarker + "\n\nmore text", 0, 0, false},
		{"marker mid-line", "see " + cleanMarker, 0, 0, false},
		{"marker quoted mid-body", "> " + cleanMarker + "\n\nreal text", 0, 0, false},
		{"unterminated", "x\n\n<!-- pruefer:findings inline=0 body=0", 0, 0, false},
		{"wrong name", "x\n\n<!-- pruefer:finding inline=0 body=0 -->", 0, 0, false},
		{"opener case-folded twin makes it ambiguous", "<!-- PRUEFER:FINDINGS inline=1 -->\n\n" + cleanMarker, 0, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in, b, ok := parseFindingsMarker(c.body)
			if ok != c.ok || (ok && (in != c.inline || b != c.bd)) {
				t.Fatalf("parseFindingsMarker = (%d,%d,%v), want (%d,%d,%v)", in, b, ok, c.inline, c.bd, c.ok)
			}
		})
	}
}

// The producer's own fixtures are the grammar source of truth (#2488): if
// Pruefer changes the marker shape, this fails.
func TestParseFindingsMarker_PrueferFixtures(t *testing.T) {
	want := map[string]struct {
		in, b int
		ok    bool
	}{
		"review_body_clean.md":   {0, 0, true},
		"review_body_mixed.md":   {2, 1, true},
		"review_body_partial.md": {1, 0, true},
		"review_body_unknown.md": {0, 0, false},
	}
	for name, w := range want {
		raw, err := os.ReadFile(filepath.Join("..", "pruefer", "testdata", name))
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		in, b, ok := parseFindingsMarker(string(raw))
		if ok != w.ok || (ok && (in != w.in || b != w.b)) {
			t.Errorf("%s: parseFindingsMarker = (%d,%d,%v), want (%d,%d,%v)", name, in, b, ok, w.in, w.b, w.ok)
		}
	}
}

func cleanReview(author, state string) gh.PRReview {
	return gh.PRReview{Author: author, State: state, Body: "Reviewed; no issues.\n\n" + cleanMarker, DatabaseID: 777, NodeID: "PRR_c"}
}

func TestCleanReviewSkipper(t *testing.T) {
	declare := func(e *Engine, names ...string) {
		e.stageByName("Validate").ExpectedReviewers = &names
	}
	t.Run("declared author, clean marker", func(t *testing.T) {
		e := testEngineForMerge(t, landingClient())
		declare(e, "handarbeit-pruefer")
		skip := e.cleanReviewSkipper("Validate", 1, nil)
		if skip == nil || !skip(cleanReview("handarbeit-pruefer", "COMMENTED")) {
			t.Fatal("a clean review by a declared reviewer must be skipped")
		}
		if !skip(cleanReview("handarbeit-pruefer[bot]", "APPROVED")) {
			t.Error("the [bot] login form must match")
		}
	})
	t.Run("not declared", func(t *testing.T) {
		e := testEngineForMerge(t, landingClient())
		declare(e, "handarbeit-pruefer")
		skip := e.cleanReviewSkipper("Validate", 1, nil)
		if skip(cleanReview("someone-else", "COMMENTED")) {
			t.Error("a marker by a non-declared author must be ignored")
		}
	})
	t.Run("CHANGES_REQUESTED stays actionable", func(t *testing.T) {
		e := testEngineForMerge(t, landingClient())
		declare(e, "handarbeit-pruefer")
		if e.cleanReviewSkipper("Validate", 1, nil)(cleanReview("handarbeit-pruefer", "CHANGES_REQUESTED")) {
			t.Error("a change request must stay actionable")
		}
	})
	t.Run("non-clean markers stay actionable", func(t *testing.T) {
		e := testEngineForMerge(t, landingClient())
		declare(e, "handarbeit-pruefer")
		skip := e.cleanReviewSkipper("Validate", 1, nil)
		for _, body := range []string{
			"x\n\n<!-- pruefer:findings inline=1 body=0 coverage=complete -->",
			"x\n\n<!-- pruefer:findings inline=0 body=2 coverage=complete -->",
			"x\n\n<!-- pruefer:findings inline=unknown body=unknown coverage=complete -->",
			"no marker",
		} {
			r := cleanReview("handarbeit-pruefer", "COMMENTED")
			r.Body = body
			if skip(r) {
				t.Errorf("body %q must stay actionable", body)
			}
		}
	})
	t.Run("nothing declared trusts nothing", func(t *testing.T) {
		e := testEngineForMerge(t, landingClient())
		if e.cleanReviewSkipper("Validate", 1, nil) != nil {
			t.Error("undeclared stage must skip nothing")
		}
		declare(e)
		if e.cleanReviewSkipper("Validate", 1, nil) != nil {
			t.Error("expected_reviewers: [] must skip nothing")
		}
		declare(e, "handarbeit-pruefer")
		if e.cleanReviewSkipper("Validate", 1, []string{"expected-reviewers:none"}) != nil {
			t.Error("expected-reviewers:none must skip nothing")
		}
		skip := e.cleanReviewSkipper("Validate", 1, []string{"expected-reviewers:declared"})
		if skip != nil && skip(cleanReview("handarbeit-pruefer", "COMMENTED")) {
			t.Error("expected-reviewers:declared resolves to a synthetic identity that matches no real author")
		}
		if e.cleanReviewSkipper("NoSuchColumn", 1, nil) != nil {
			t.Error("a status with no stage declares nothing")
		}
	})
	t.Run("seam", func(t *testing.T) {
		e := testEngineForMerge(t, landingClient())
		declare(e, "handarbeit-pruefer")
		e.SetCleanReviewSkipDisabledForTest(true)
		if e.cleanReviewSkipper("Validate", 1, nil) != nil {
			t.Error("the seam must disable the exclusion")
		}
	})
}

// The live gate, the cache-only mirrors and the dispatcher view must all agree on
// a clean review (#2489): none holds, none sees it.
func TestCleanReviewAgreesAcrossReaders(t *testing.T) {
	cases := []struct {
		name   string
		review gh.PRReview
		want   bool // pending feedback expected
	}{
		{"clean by declared reviewer", cleanReview("handarbeit-pruefer", "COMMENTED"), false},
		{"clean by stranger", cleanReview("stranger", "COMMENTED"), true},
		{"findings by declared reviewer", func() gh.PRReview {
			r := cleanReview("handarbeit-pruefer", "COMMENTED")
			r.Body = "x\n\n<!-- pruefer:findings inline=1 body=0 coverage=complete -->"
			return r
		}(), true},
		{"clean change request", cleanReview("handarbeit-pruefer", "CHANGES_REQUESTED"), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			eng := testEngineForMerge(t, landingClient())
			names := []string{"handarbeit-pruefer"}
			eng.stageByName("Validate").ExpectedReviewers = &names
			item := feedbackItem(func(i *gh.ProjectItem) {
				i.Status = "Validate"
				i.LinkedPRNumber = 34
				i.LinkedPRReviews = []gh.PRReview{c.review}
			})
			eng.store.Apply(itemstate.IssueOpened{Item: item})
			snap, ok := eng.store.Peek("owner/repo", 1)
			if !ok {
				t.Fatal("item not in store")
			}
			st := snap.State()
			cached := eng.cachedPendingFeedback(snap, &st).any()
			live := eng.feedbackGateBlocks(item, true, "advance")
			arrived := eng.cachedFeedbackArrivedSince(&st, time.Time{}).any()
			view := len(eng.dispatcherVisibleIDs(item)) > 0
			if cached != c.want || live != c.want || view != c.want {
				t.Errorf("cached=%v live=%v dispatcher-view=%v, want all %v", cached, live, view, c.want)
			}
			_ = arrived // zero SubmittedAt is never counted as arrived; covered by the sim twin
			if gap, _ := eng.dispatcherGap(item, pendingFeedback{}); len(gap) != 0 {
				t.Errorf("nothing held, expected no dispatcher gap, got %v", gap)
			}
		})
	}
}

func TestFindingsMarkerSourceReadsOnlyReviewBodies(t *testing.T) {
	raw, err := os.ReadFile("findings_marker.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"e.client", "e.readClient", "Comments"} {
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			if strings.Contains(line, banned) {
				t.Errorf("findings_marker.go must read only PRReview.Body, found %q in %q", banned, line)
			}
		}
	}
}
