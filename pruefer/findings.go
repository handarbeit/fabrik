package pruefer

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// ReviewFinding is a single structured finding Claude reports against a
// specific file/line, extracted from the review's fenced JSON block. Line
// numbers are always interpreted against the PR's post-change (RIGHT side)
// file version — see diffanchor.go for anchor validation.
type ReviewFinding struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Body string `json:"body"`
	// Severity is Claude's self-reported tier for this finding (see
	// Severity's doc comment for the 4-tier vocabulary). An unrecognized or
	// missing value unmarshals fine (json.Unmarshal never errors over an
	// unknown string or an absent field) but ranks 0 — see severityRank —
	// so it never meets any REQUEST_CHANGES threshold. This is the sole
	// field decideEvent reads; it never inspects Body or the prose summary.
	Severity Severity `json:"severity"`
}

// findingsFenceRE matches a fenced ```json ... ``` code block, non-greedy so
// it stops at the first closing fence. The "json" language tag is required —
// buildReviewPrompt instructs Claude to always include it, so requiring it
// here avoids false-matching an unrelated fenced code snippet quoted inside
// the summary prose (e.g. Claude illustrating a fix with a ```go block).
var findingsFenceRE = regexp.MustCompile("(?s)```json\\s*(.*?)```")

// summaryMarkerBeginRE and summaryMarkerEndRE match the PRUEFER_SUMMARY_BEGIN
// / PRUEFER_SUMMARY_END delimiter lines buildReviewPrompt instructs Claude to
// wrap its prose summary in (R1/R2). Line-anchored (multiline `^...$`, `\r?`
// tolerant), mirroring engine/claude.go's stageCompleteRE convention for
// control tokens — a marker must occupy its own line, so a phrase that
// merely mentions "PRUEFER_SUMMARY_BEGIN" mid-sentence in narration can't be
// mistaken for the real delimiter.
var (
	summaryMarkerBeginRE = regexp.MustCompile(`(?m)^PRUEFER_SUMMARY_BEGIN\r?$`)
	summaryMarkerEndRE   = regexp.MustCompile(`(?m)^PRUEFER_SUMMARY_END\r?$`)
)

// SummaryParseInfo reports how parseReviewFindings extracted the summary,
// for the caller (review.go) to log per R4 — parseReviewFindings itself
// stays a pure function with no logf call, matching the rest of this file.
type SummaryParseInfo struct {
	// MarkersFound is true iff a well-formed PRUEFER_SUMMARY_BEGIN/END pair
	// was found (BEGIN present, END present after it) and the delimited text
	// was used as the summary. False means the positional fallback
	// (everything before the findings fence, or the whole text) was used
	// instead — either because Claude omitted the markers, or emitted a
	// malformed pair (missing END, or END before BEGIN).
	MarkersFound bool
	// DiscardedBytes is the trimmed length of the preamble text found before
	// PRUEFER_SUMMARY_BEGIN, when MarkersFound is true. Whitespace-trimmed,
	// not a raw byte count, so a stray trailing newline before the marker
	// doesn't count as discarded content. Always 0 when MarkersFound is
	// false — the fallback path discards nothing; it uses the full
	// positional text as the summary, exactly as before this change.
	DiscardedBytes int
}

// splitSummaryMarkers looks for a well-formed PRUEFER_SUMMARY_BEGIN/END pair
// in text and, if found, returns the trimmed text between them plus
// SummaryParseInfo{MarkersFound: true, ...}. If BEGIN is missing, END is
// missing, or END is found before BEGIN, it returns ("", SummaryParseInfo{})
// — a malformed pair is treated identically to "markers absent" (R3): the
// fallback logic stays binary (found-and-well-formed vs. not) rather than
// inventing a harder-to-test partial-extraction state.
//
// buildReviewPrompt instructs Claude to place the findings JSON fence after
// PRUEFER_SUMMARY_END, never between the markers, but that's an unenforced
// request — the same shape of problem this whole feature exists to close.
// If Claude nests the fence inside the marker pair anyway, the fence's
// content is never prose summary — it's already extracted separately as
// structured findings — so it is truncated out of the delimited text here,
// mirroring parseReviewFindings' own positional text[:loc[0]] behavior
// exactly. This is not the R5 "heuristic trimming" the doc comment there
// warns against: R5 protects legitimate summary prose from being guessed
// away, and a raw findings array was never summary prose to begin with.
func splitSummaryMarkers(text string) (summary string, info SummaryParseInfo) {
	beginLoc := summaryMarkerBeginRE.FindStringIndex(text)
	if beginLoc == nil {
		return "", SummaryParseInfo{}
	}
	endLoc := summaryMarkerEndRE.FindStringIndex(text[beginLoc[1]:])
	if endLoc == nil {
		return "", SummaryParseInfo{}
	}

	preamble := strings.TrimSpace(text[:beginLoc[0]])
	delimited := text[beginLoc[1] : beginLoc[1]+endLoc[0]]
	if fenceLoc := findingsFenceRE.FindStringIndex(delimited); fenceLoc != nil {
		delimited = delimited[:fenceLoc[0]]
	}
	summary = strings.TrimSpace(delimited)
	return summary, SummaryParseInfo{MarkersFound: true, DiscardedBytes: len(preamble)}
}

// parseReviewFindings splits Claude's review output into its two-part
// contract: free-form summary prose, followed by a single fenced ```json
// array of {"path","line","body"} findings. The summary is extracted from
// between PRUEFER_SUMMARY_BEGIN/END markers when a well-formed pair is
// present (R1/R2); otherwise it falls back to everything before the fence,
// exactly as before those markers existed (R3). The fence's content is
// unmarshaled into findings independently of which summary path was taken.
//
// This is a graceful-degrade parser, not a strict one: when no fenced JSON
// block is present, or its content doesn't unmarshal as a findings array,
// the entire input (or the delimited summary, if markers were found) is
// returned as the summary with zero findings. That covers both today's
// plain-prose reviews and Claude failing to follow the new contract — in
// neither case should the review fail to submit; it just posts without
// inline comments, exactly as it does today.
func parseReviewFindings(text string) (summary string, findings []ReviewFinding, info SummaryParseInfo) {
	delimited, delimitedInfo := splitSummaryMarkers(text)

	loc := findingsFenceRE.FindStringSubmatchIndex(text)
	if loc == nil {
		if delimitedInfo.MarkersFound {
			return delimited, nil, delimitedInfo
		}
		return strings.TrimSpace(text), nil, SummaryParseInfo{}
	}

	fenceContent := text[loc[2]:loc[3]]
	var parsed []ReviewFinding
	if err := json.Unmarshal([]byte(fenceContent), &parsed); err != nil {
		if delimitedInfo.MarkersFound {
			return delimited, nil, delimitedInfo
		}
		return strings.TrimSpace(text), nil, SummaryParseInfo{}
	}

	if delimitedInfo.MarkersFound {
		return delimited, parsed, delimitedInfo
	}
	summary = strings.TrimSpace(text[:loc[0]])
	return summary, parsed, SummaryParseInfo{}
}

// dedupeFindings collapses findings that share the same (Path, Line) into a
// single entry, implementing R3's within-pass deduplication in code (not
// just as a prompt instruction — Claude has already been observed producing
// exactly this kind of duplicate without being told not to, on #1477).
// (Path, Line) is the same anchor identity partitionFindings uses, so two
// findings that would collide as inline comments at the same anchor are
// exactly the ones collapsed here.
//
// (Path, Line) collision is a positional signal, not proof the two findings
// describe the same underlying defect — a genuinely distinct second finding
// can legitimately land on the same line as an unrelated one, and unlike
// dedupeFindings, partitionFindings itself never collapses same-anchor
// findings (GitHub's review-comments API supports multiple comments per
// line). So a collision here merges rather than picks a winner: both bodies
// survive, concatenated, under the higher-ranked Severity (via
// severityRank — collapsing must never accidentally weaken a
// REQUEST_CHANGES threshold decision). This costs a merged comment reading
// slightly awkwardly when the two really were the same restated point, but
// never silently discards a distinct finding. Output order matches
// first-seen order of each surviving (Path, Line) key.
func dedupeFindings(findings []ReviewFinding) []ReviewFinding {
	if len(findings) == 0 {
		return findings
	}
	type key struct {
		Path string
		Line int
	}
	kept := make(map[key]int, len(findings)) // key -> index into result
	var result []ReviewFinding
	for _, f := range findings {
		k := key{f.Path, f.Line}
		if idx, ok := kept[k]; ok {
			result[idx] = mergeFindings(result[idx], f)
			continue
		}
		kept[k] = len(result)
		result = append(result, f)
	}
	return result
}

// mergeFindings combines two findings colliding on the same (Path, Line)
// anchor: existing's fields form the base, with incoming's body appended
// (skipped if byte-identical, since that's the literal-restatement case
// dedupeFindings originally targeted) and Severity raised if incoming ranks
// higher.
func mergeFindings(existing, incoming ReviewFinding) ReviewFinding {
	merged := existing
	if severityRank(incoming.Severity) > severityRank(merged.Severity) {
		merged.Severity = incoming.Severity
	}
	if incoming.Body != "" && incoming.Body != existing.Body {
		if merged.Body == "" {
			merged.Body = incoming.Body
		} else {
			merged.Body = merged.Body + "\n\n---\n\n" + incoming.Body
		}
	}
	return merged
}

// buildReviewBody assembles the final review body from the prose summary
// plus any findings that could not be anchored to the diff. Demoted
// findings are appended under an explicit heading, each rendered as
// "**path:line**: body" — kept visually distinct from the summary so they
// read as identifiable findings rather than blending into ambient prose,
// even though (like all body text) they remain unconsumable by Fabrik's
// inline-comment-only review-reinvoke path.
func buildReviewBody(summary string, demoted []ReviewFinding) string {
	if len(demoted) == 0 {
		return summary
	}

	var findingsBlock strings.Builder
	findingsBlock.WriteString("## Additional findings (could not anchor to diff)")
	for _, f := range demoted {
		fmt.Fprintf(&findingsBlock, "\n\n**%s:%d**: %s", f.Path, f.Line, f.Body)
	}

	if summary == "" {
		return findingsBlock.String()
	}
	return summary + "\n\n" + findingsBlock.String()
}
