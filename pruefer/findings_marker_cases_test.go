package pruefer

// markerFixtureCase is one wire fixture: the inputs to assembleReviewBody and
// the file under testdata/ that holds the body the engine-side consumer parses.
type markerFixtureCase struct {
	file     string
	summary  string
	demoted  []ReviewFinding
	cov      Coverage
	counts   findingsCounts
	fallback string
	readOnly string
}

func markerFixtureCases() []markerFixtureCase {
	return []markerFixtureCase{
		{
			file:    "review_body_clean.md",
			summary: "Reviewed the change; no issues found.",
			cov:     Coverage{Complete: true},
			counts:  findingsCounts{Parsed: true},
		},
		{
			file:     "review_body_mixed.md",
			summary:  "Two issues are anchored inline; one concerns code outside the diff.",
			demoted:  []ReviewFinding{{Path: "pruefer/review.go", Line: 12, Body: "The error is dropped here."}},
			cov:      Coverage{Complete: true},
			counts:   findingsCounts{Parsed: true, Inline: 2, Body: 1},
			fallback: fallbackNotice("local-workflows:custom", "0123456789abcdef", "skill not found"),
		},
		{
			file:    "review_body_partial.md",
			summary: "Reviewed what I could open.",
			cov:     Coverage{Reason: "2 of 5 diff entries not opened", Unread: []string{"a/b.go", "c/d.go"}},
			counts:  findingsCounts{Parsed: true, Inline: 1},
		},
		{
			file:    "review_body_unknown.md",
			summary: "The review text had no parseable findings block.",
			cov:     Coverage{Complete: true},
			counts:  findingsCounts{},
		},
	}
}

func (c markerFixtureCase) body() string {
	return assembleReviewBody(c.summary, c.demoted, c.cov, c.counts, c.fallback, c.readOnly)
}
