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
