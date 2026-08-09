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

// parseReviewFindings splits Claude's review output into its two-part
// contract: free-form summary prose, followed by a single fenced ```json
// array of {"path","line","body"} findings. Everything before the fence is
// the summary; the fence's content is unmarshaled into findings.
//
// This is a graceful-degrade parser, not a strict one: when no fenced JSON
// block is present, or its content doesn't unmarshal as a findings array,
// the entire input is returned as the summary with zero findings. That
// covers both today's plain-prose reviews and Claude failing to follow the
// new contract — in neither case should the review fail to submit; it just
// posts without inline comments, exactly as it does today.
func parseReviewFindings(text string) (summary string, findings []ReviewFinding) {
	loc := findingsFenceRE.FindStringSubmatchIndex(text)
	if loc == nil {
		return strings.TrimSpace(text), nil
	}

	fenceContent := text[loc[2]:loc[3]]
	var parsed []ReviewFinding
	if err := json.Unmarshal([]byte(fenceContent), &parsed); err != nil {
		return strings.TrimSpace(text), nil
	}

	summary = strings.TrimSpace(text[:loc[0]])
	return summary, parsed
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
