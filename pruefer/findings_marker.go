package pruefer

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// findingsMarkerPrefix and findingsMarkerSuffix are the literal opener and
// closer of the hidden findings-count marker that ends every posted review
// body (#2488, ADR 2488). The grammar is a stable contract the engine parses
// (#2489); see adrs/2488-pruefer-findings-count-marker.md before changing it.
const (
	findingsMarkerPrefix = "<!-- pruefer:findings "
	findingsMarkerSuffix = " -->"
	// findingsUnknown is the value both counts take when the model's findings
	// block was missing or unparseable. Omitting the marker cannot express that,
	// because absence already means "a review from before this contract".
	findingsUnknown = "unknown"
)

// findingsMarkerLookalikeRE matches the start of anything that could be read as
// the marker. It is deliberately not line-anchored: an HTML comment can start
// mid-line, so a line-anchored match (like coverageLineRE) would miss text such
// as "see <!-- pruefer:findings inline=0 ... -->" at the end of a summary line.
var findingsMarkerLookalikeRE = regexp.MustCompile(`(?i)<!--(\s*)pruefer:findings`)

// findingsCounts carries what the review actually submits: the number of
// findings posted as inline comments and the number demoted into the body, both
// taken after de-duplication and partitioning. Parsed is false when the
// findings block could not be parsed, in which case the counts are unknown.
type findingsCounts struct {
	Parsed bool
	Inline int
	Body   int
}

// renderFindingsMarker renders the marker line. coverage comes from the same
// Coverage value that produces the visible PRUEFER_COVERAGE line, so the two
// cannot disagree; only the complete/partial verdict is emitted, never the
// free-text reason.
func renderFindingsMarker(c findingsCounts, cov Coverage) string {
	inline, body := findingsUnknown, findingsUnknown
	if c.Parsed {
		inline, body = strconv.Itoa(c.Inline), strconv.Itoa(c.Body)
	}
	verdict := "partial"
	if cov.Complete {
		verdict = "complete"
	}
	return fmt.Sprintf("%sinline=%s body=%s coverage=%s%s", findingsMarkerPrefix, inline, body, verdict, findingsMarkerSuffix)
}

// neutraliseFindingsMarker defuses any text that would parse as the marker by
// breaking its comment opener ("<!--" becomes "&lt;!--"). The text stays
// visible but can no longer form a comment or match the grammar.
func neutraliseFindingsMarker(body string) string {
	return findingsMarkerLookalikeRE.ReplaceAllString(body, "&lt;!--${1}pruefer:findings")
}

// appendFindingsMarker neutralises look-alikes in body and appends the real
// marker as its final line, preceded by one blank line.
func appendFindingsMarker(body string, c findingsCounts, cov Coverage) string {
	body = strings.TrimRight(neutraliseFindingsMarker(body), "\n")
	marker := renderFindingsMarker(c, cov)
	if body == "" {
		return marker
	}
	return body + "\n\n" + marker
}

// assembleReviewBody builds the full review body in its one fixed order:
// summary and demoted findings, the coverage line, the optional notices
// prepended (fallback first, then read-only, so read-only ends up on top), and
// finally the findings marker. Appending the marker last makes "ends the body"
// true by construction, and lets one neutraliser cover the summary, demoted
// findings and notices alike.
func assembleReviewBody(summary string, demoted []ReviewFinding, cov Coverage, counts findingsCounts, fallback, readOnly string) string {
	body := appendCoverage(buildReviewBody(summary, demoted), cov)
	if fallback != "" {
		body = fallback + "\n\n" + body
	}
	if readOnly != "" {
		body = readOnly + "\n\n" + body
	}
	return appendFindingsMarker(body, counts, cov)
}
