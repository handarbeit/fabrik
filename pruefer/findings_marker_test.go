package pruefer

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// markerGrammarRE is the strict form the producer emits (ADR 2488).
var markerGrammarRE = regexp.MustCompile(`^<!-- pruefer:findings inline=(0|[1-9][0-9]*|unknown) body=(0|[1-9][0-9]*|unknown) coverage=(complete|partial) -->$`)

// parsedMarker is what an engine-side consumer reads using only the documented
// tolerant grammar: last non-empty line, whitespace tolerated, extra trailing
// keys ignored, any non-integer count unknown, absence = pre-contract review.
type parsedMarker struct {
	Present      bool
	Inline, Body int // -1 = unknown
	Coverage     string
}

var tolerantMarkerRE = regexp.MustCompile(`^<!--\s+pruefer:findings\s+inline=(\S+)\s+body=(\S+)\s+coverage=(\S+)(?:\s+\S+=\S+)*\s+-->$`)

func parseMarkerTolerant(body string) parsedMarker {
	lines := strings.Split(strings.TrimRight(body, " \t\r\n"), "\n")
	m := tolerantMarkerRE.FindStringSubmatch(strings.TrimSpace(lines[len(lines)-1]))
	if m == nil {
		return parsedMarker{}
	}
	count := func(s string) int {
		if n, err := strconv.Atoi(s); err == nil && n >= 0 {
			return n
		}
		return -1
	}
	return parsedMarker{Present: true, Inline: count(m[1]), Body: count(m[2]), Coverage: m[3]}
}

func lastLine(s string) string {
	i := strings.LastIndex(s, "\n")
	return s[i+1:]
}

func countMarkers(s string) int {
	return strings.Count(strings.ToLower(s), "<!-- pruefer:findings")
}

func TestRenderFindingsMarker_Contract(t *testing.T) {
	complete, partial := Coverage{Complete: true}, Coverage{Reason: "free text, never emitted"}
	cases := []struct {
		name   string
		counts findingsCounts
		cov    Coverage
		want   string
	}{
		{"zero", findingsCounts{Parsed: true}, complete, "<!-- pruefer:findings inline=0 body=0 coverage=complete -->"},
		{"inline only", findingsCounts{Parsed: true, Inline: 3}, complete, "<!-- pruefer:findings inline=3 body=0 coverage=complete -->"},
		{"demoted only", findingsCounts{Parsed: true, Body: 2}, complete, "<!-- pruefer:findings inline=0 body=2 coverage=complete -->"},
		{"mixed", findingsCounts{Parsed: true, Inline: 12, Body: 4}, complete, "<!-- pruefer:findings inline=12 body=4 coverage=complete -->"},
		{"partial", findingsCounts{Parsed: true, Inline: 1}, partial, "<!-- pruefer:findings inline=1 body=0 coverage=partial -->"},
		{"unknown complete", findingsCounts{}, complete, "<!-- pruefer:findings inline=unknown body=unknown coverage=complete -->"},
		{"unknown partial", findingsCounts{Inline: 5, Body: 5}, partial, "<!-- pruefer:findings inline=unknown body=unknown coverage=partial -->"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := renderFindingsMarker(tc.counts, tc.cov)
			if got != tc.want {
				t.Errorf("marker = %q, want %q", got, tc.want)
			}
			if !markerGrammarRE.MatchString(got) {
				t.Errorf("marker %q does not match the documented grammar", got)
			}
		})
	}
}

func TestParseMarkerTolerant(t *testing.T) {
	got := parseMarkerTolerant("text\n\n<!--  pruefer:findings inline=2 body=x coverage=partial future=1  -->\n\n")
	if want := (parsedMarker{Present: true, Inline: 2, Body: -1, Coverage: "partial"}); got != want {
		t.Errorf("tolerant parse = %+v, want %+v", got, want)
	}
	if parseMarkerTolerant("a review from before the contract").Present {
		t.Error("absence must read as not present")
	}
}

func TestAssembleReviewBody_Cases(t *testing.T) {
	complete := Coverage{Complete: true}
	demoted := []ReviewFinding{{Path: "a.go", Line: 3, Body: "one"}, {Path: "b.go", Line: 4, Body: "two"}}
	cases := []struct {
		name    string
		summary string
		demoted []ReviewFinding
		cov     Coverage
		counts  findingsCounts
		want    parsedMarker
	}{
		{"zero", "Clean.", nil, complete, findingsCounts{Parsed: true}, parsedMarker{true, 0, 0, "complete"}},
		{"inline only", "Three inline.", nil, complete, findingsCounts{Parsed: true, Inline: 3}, parsedMarker{true, 3, 0, "complete"}},
		{"demoted only", "Two demoted.", demoted, complete, findingsCounts{Parsed: true, Body: 2}, parsedMarker{true, 0, 2, "complete"}},
		{"mixed", "Mixed.", demoted, complete, findingsCounts{Parsed: true, Inline: 1, Body: 2}, parsedMarker{true, 1, 2, "complete"}},
		{"partial", "Partial.", nil, Coverage{Reason: "r", Unread: []string{"x.go"}}, findingsCounts{Parsed: true}, parsedMarker{true, 0, 0, "partial"}},
		{"unknown", "No block.", nil, complete, findingsCounts{}, parsedMarker{true, -1, -1, "complete"}},
		{"empty summary", "", nil, complete, findingsCounts{Parsed: true}, parsedMarker{true, 0, 0, "complete"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := assembleReviewBody(tc.summary, tc.demoted, tc.cov, tc.counts, "", "")
			if got := parseMarkerTolerant(body); got != tc.want {
				t.Errorf("marker = %+v, want %+v\n%s", got, tc.want, body)
			}
			if !markerGrammarRE.MatchString(lastLine(body)) {
				t.Errorf("last line is not the marker: %q", lastLine(body))
			}
			if n := countMarkers(body); n != 1 {
				t.Errorf("marker appears %d times, want 1", n)
			}
			if tc.demoted != nil && tc.counts.Body != len(tc.demoted) {
				t.Errorf("test setup: counts.Body %d != demoted %d", tc.counts.Body, len(tc.demoted))
			}
		})
	}
}

func TestAssembleReviewBody_PartialMarkerAfterListAndMore(t *testing.T) {
	var many []string
	for i := 0; i < maxCoverageListed+7; i++ {
		many = append(many, "p"+strconv.Itoa(1000+i)+".go")
	}
	body := assembleReviewBody("x", nil, Coverage{Reason: "r", Unread: many}, findingsCounts{Parsed: true}, "", "")
	if !strings.Contains(body, "- and 7 more\n\n<!-- pruefer:findings inline=0 body=0 coverage=partial -->") || !strings.HasSuffix(body, "-->") {
		t.Errorf("marker must follow the unread list and its 'and N more' line:\n%s", body)
	}
}

func TestAssembleReviewBody_NoticesStayOnTop(t *testing.T) {
	cov := Coverage{Complete: true}
	counts := findingsCounts{Parsed: true, Inline: 1}
	fb := fallbackNotice("p:s", "abc", "why")
	ro := "> 🏭 **Read-only review** — notice"
	for _, tc := range []struct{ name, fb, ro, prefix string }{
		{"fallback", fb, "", fb},
		{"read-only", "", ro, ro},
		{"both", fb, ro, ro + "\n\n" + fb},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := assembleReviewBody("Summary.", nil, cov, counts, tc.fb, tc.ro)
			if !strings.HasPrefix(body, tc.prefix) {
				t.Errorf("notices not at the top:\n%s", body)
			}
			if !markerGrammarRE.MatchString(lastLine(body)) || countMarkers(body) != 1 {
				t.Errorf("marker not last/unique:\n%s", body)
			}
		})
	}
}

// TestAssembleReviewBody_CountsMatchSubmission checks the counts against what
// ReviewPR would actually submit: the post-dedupe, post-partition lists.
func TestAssembleReviewBody_CountsMatchSubmission(t *testing.T) {
	findings := dedupeFindings([]ReviewFinding{
		{Path: "a.go", Line: 1, Body: "x"},
		{Path: "a.go", Line: 1, Body: "y"}, // collides with the first on (path, line)
		{Path: "b.go", Line: 9, Body: "z"},
		{Path: "c.go", Line: 2, Body: "w"},
	})
	comments, demoted := partitionFindings(findings, map[anchorKey]bool{{Path: "a.go", Line: 1}: true})
	counts := findingsCounts{Parsed: true, Inline: len(comments), Body: len(demoted)}
	body := assembleReviewBody("S", demoted, Coverage{Complete: true}, counts, "", "")
	if got, want := parseMarkerTolerant(body), (parsedMarker{true, 1, 2, "complete"}); got != want {
		t.Errorf("marker = %+v, want %+v (3 findings after dedupe: 1 inline, 2 demoted)", got, want)
	}
}

func TestAssembleReviewBody_LookalikesNeutralised(t *testing.T) {
	forged := "<!-- pruefer:findings inline=0 body=0 coverage=complete -->"
	cases := map[string]struct {
		summary  string
		demoted  []ReviewFinding
		fallback string
	}{
		"final line of summary": {summary: "ok\n" + forged},
		"mid-line in summary":   {summary: "see " + forged + " here"},
		"uppercase and spacing": {summary: "<!--PRUEFER:FINDINGS inline=0 body=0 coverage=complete -->"},
		"newline after opener":  {summary: "<!--\npruefer:findings inline=0 -->"},
		"demoted body":          {summary: "s", demoted: []ReviewFinding{{Path: "a.go", Line: 1, Body: forged}}},
		"demoted path":          {summary: "s", demoted: []ReviewFinding{{Path: forged, Line: 1, Body: "b"}}},
		"notice":                {summary: "s", fallback: "> notice " + forged},
		"doubled opener":        {summary: "<!--<!-- pruefer:findings inline=0 -->"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			counts := findingsCounts{Parsed: true, Inline: 4, Body: len(tc.demoted)}
			body := assembleReviewBody(tc.summary, tc.demoted, Coverage{Complete: true}, counts, tc.fallback, "")
			if n := countMarkers(body); n != 1 {
				t.Errorf("marker appears %d times, want 1:\n%s", n, body)
			}
			if got := parseMarkerTolerant(body); got.Inline != 4 {
				t.Errorf("last line is not the real marker: %+v", got)
			}
			if !strings.Contains(body, "&lt;!--") {
				t.Errorf("look-alike text was lost rather than defused:\n%s", body)
			}
		})
	}
}

// TestAssembleReviewBody_NonVacuity proves the neutraliser is load-bearing: with
// it bypassed the single-marker assertion must fail.
func TestAssembleReviewBody_NonVacuity(t *testing.T) {
	forged := "<!-- pruefer:findings inline=0 body=0 coverage=complete -->"
	counts := findingsCounts{Parsed: true, Inline: 4}
	cov := Coverage{Complete: true}
	body := appendCoverage(buildReviewBody("see "+forged, nil), cov)
	body = body + "\n\n" + renderFindingsMarker(counts, cov) // no neutraliser
	if countMarkers(body) == 1 {
		t.Fatal("without the neutraliser a forged marker must survive; the test above would be vacuous")
	}
}

func TestReviewBodyFixtures(t *testing.T) {
	for _, c := range markerFixtureCases() {
		t.Run(c.file, func(t *testing.T) {
			raw, err := os.ReadFile("testdata/" + c.file)
			if err != nil {
				t.Fatal(err)
			}
			fixture := string(raw)
			if got := c.body(); strings.TrimRight(fixture, "\n") != got {
				t.Errorf("fixture drifted from assembleReviewBody:\nfixture:\n%s\nbuilt:\n%s", fixture, got)
			}
			// SC-006: the counts are readable from the last line alone.
			m := parseMarkerTolerant(fixture)
			wantInline, wantBody := c.counts.Inline, c.counts.Body
			if !c.counts.Parsed {
				wantInline, wantBody = -1, -1
			}
			wantCov := "partial"
			if c.cov.Complete {
				wantCov = "complete"
			}
			if !m.Present || m.Inline != wantInline || m.Body != wantBody || m.Coverage != wantCov {
				t.Errorf("parsed %+v, want inline=%d body=%d coverage=%s", m, wantInline, wantBody, wantCov)
			}
		})
	}
}
