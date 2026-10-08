package engine

import (
	"fmt"
	"strings"
)

// extractMarkdownSection finds a `## heading` in content (case-insensitive) and returns
// the trimmed body of that section. The body is everything from the line after the heading
// until the next `## ` heading or end of file. Returns "" if the heading is not found.
func extractMarkdownSection(content, heading string) string {
	lines := strings.Split(content, "\n")
	target := strings.ToLower("## " + heading)
	inSection := false
	var collected []string
	for _, line := range lines {
		if inSection {
			// Stop at any other ## heading (--- horizontal rules can appear inside sections)
			if strings.HasPrefix(strings.TrimSpace(line), "## ") {
				break
			}
			collected = append(collected, line)
		} else if strings.EqualFold(strings.TrimSpace(line), target) {
			inSection = true
		}
	}
	if !inSection {
		return ""
	}
	return strings.TrimSpace(strings.Join(collected, "\n"))
}

// firstParagraph returns the first non-empty paragraph from content (lines up to the
// first blank line after content begins), or "" if content is empty.
func firstParagraph(content string) string {
	lines := strings.Split(strings.TrimSpace(content), "\n")
	var para []string
	started := false
	for _, line := range lines {
		if line == "" {
			if started {
				break
			}
			continue
		}
		started = true
		para = append(para, line)
	}
	return strings.TrimSpace(strings.Join(para, "\n"))
}

// specTemplateTitlePrefix starts the title line of a body written in the Spec Kit
// feature-specification template the Specify skill authors.
const specTemplateTitlePrefix = "# Feature Specification:"

// specTemplateHeaderFields are the bold header fields that follow that title.
var specTemplateHeaderFields = []string{"Feature Branch", "Created", "Status", "Input"}

// isSpecTemplateHeaderLine reports whether a line is the template's title line or
// one of its bold header-field lines ("**Status**: Draft").
func isSpecTemplateHeaderLine(line string) bool {
	t := strings.TrimSpace(line)
	if strings.HasPrefix(t, specTemplateTitlePrefix) {
		return true
	}
	for _, f := range specTemplateHeaderFields {
		if strings.HasPrefix(t, "**"+f+"**:") || strings.HasPrefix(t, "**"+f+":**") {
			return true
		}
	}
	return false
}

// firstBodyParagraph is firstParagraph over content with the Spec Kit template's
// title and header-field lines removed, so a PR section is never filled with the
// "# Feature Specification:" title. A body in any other shape is unchanged.
func firstBodyParagraph(content string) string {
	lines := strings.Split(content, "\n")
	kept := lines[:0:0]
	for _, l := range lines {
		if !isSpecTemplateHeaderLine(l) {
			kept = append(kept, l)
		}
	}
	return firstParagraph(strings.Join(kept, "\n"))
}

// extractInputField returns the text of the template's "**Input**:" header field —
// the original request — dropping the "User description:" label and one pair of
// surrounding double quotes. A request wrapped over several lines continues until
// a blank line or the next bold field. Returns "" if the field is absent or empty.
func extractInputField(content string) string {
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		t := strings.TrimSpace(line)
		rest, ok := strings.CutPrefix(t, "**Input**:")
		if !ok {
			rest, ok = strings.CutPrefix(t, "**Input:**")
		}
		if !ok {
			continue
		}
		parts := []string{strings.TrimSpace(rest)}
		for _, next := range lines[i+1:] {
			nt := strings.TrimSpace(next)
			if nt == "" || strings.HasPrefix(nt, "**") || strings.HasPrefix(nt, "#") {
				break
			}
			parts = append(parts, nt)
		}
		text := strings.TrimSpace(strings.Join(parts, "\n"))
		text = strings.TrimSpace(strings.TrimPrefix(text, "User description:"))
		if len(text) >= 2 && strings.HasPrefix(text, `"`) && strings.HasSuffix(text, `"`) {
			text = strings.TrimSpace(text[1 : len(text)-1])
		}
		return text
	}
	return ""
}

// balanceFences scans body for fence delimiter lines (any line whose trimmed content starts
// with ``` or ~~~, including language hints such as ```bash) using an indentation-aware
// state machine. A closer is valid only when its leading-space count is >= the opener's.
// An unindented closer (column 0) against an indented opener (column >= 1) is treated as
// content inside the fence, matching CommonMark §4.5 semantics. If the final state is an
// unclosed fence, a synthetic ``` closer (indented to match the opener) is inserted
// immediately before the last "---" line (the separator before Closes #N). If no "---" is
// present, it inserts before the last "Closes #" line (using the last occurrence avoids
// placing the synthetic closer before a top-of-body "Closes #N" header). If neither is
// present, it appends at the end. This ensures that interpolated content with unclosed
// fences cannot hide trailing lines from GitHub's parser.
func balanceFences(body string) string {
	lines := strings.Split(body, "\n")
	var fenceActive bool
	var fenceIndent int
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			indent := len(line) - len(strings.TrimLeft(line, " "))
			if !fenceActive {
				fenceActive = true
				fenceIndent = indent
			} else if indent >= fenceIndent {
				fenceActive = false
			}
			// else: closer is less-indented than opener → content inside fence, ignore
		}
	}
	if !fenceActive {
		return body
	}
	// Synthetic closer matches the opener's indentation; always uses backticks.
	syntheticCloser := strings.Repeat(" ", fenceIndent) + "```"
	// Insert closing fence before the last "---" line (the separator before Closes #N).
	// Using the last "---" rather than the first prevents inserting before an unclosed fence
	// opener when earlier "---" dividers appear in manually-edited PR bodies.
	insertIdx := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == "---" {
			insertIdx = i
		}
	}
	if insertIdx < 0 {
		// Fallback: before last "Closes #" line. Using the last occurrence avoids inserting
		// before a top-of-body "Closes #N" header (which would create a new fence opener).
		for i, line := range lines {
			if strings.HasPrefix(strings.TrimSpace(line), "Closes #") {
				insertIdx = i
			}
		}
	}
	if insertIdx < 0 {
		// Fallback: append at end.
		return body + "\n" + syntheticCloser
	}
	newLines := make([]string, 0, len(lines)+1)
	newLines = append(newLines, lines[:insertIdx]...)
	newLines = append(newLines, syntheticCloser)
	newLines = append(newLines, lines[insertIdx:]...)
	return strings.Join(newLines, "\n")
}

// buildPRSeedBody constructs the structured PR body template from issue and plan context.
// issueContent is the contents of .fabrik-context/issue.md.
// planContent is the contents of .fabrik-context/stage-Plan.md (may be empty if missing).
// issueNumber is used for both the top-of-body "Closes #N" (before ## Summary, ensuring
// GitHub's link extractor sees it regardless of code fence issues in the body) and the
// redundant footer "Closes #N" (after ---, for defense-in-depth).
func buildPRSeedBody(issueContent, planContent string, issueNumber int) string {
	// Extract Summary from issue: ## Summary, else the Spec Kit template's
	// **Input** field, else the first paragraph of the body.
	summary := extractMarkdownSection(issueContent, "Summary")
	if summary == "" {
		summary = extractInputField(issueContent)
	}
	if summary == "" {
		summary = firstBodyParagraph(issueContent)
	}
	if summary == "" {
		summary = "(no summary available)"
	}

	// Extract Problem from issue: ## Problem, else the Spec Kit template's
	// ## Background, else the first paragraph of the body.
	problem := extractMarkdownSection(issueContent, "Problem")
	if problem == "" {
		problem = extractMarkdownSection(issueContent, "Background")
	}
	if problem == "" {
		problem = firstBodyParagraph(issueContent)
	}
	if problem == "" {
		problem = "(no problem description available)"
	}

	// Extract Approach / Implementation Plan / Plan from plan content
	approach := ""
	for _, heading := range []string{"Approach", "Implementation Plan", "Plan"} {
		approach = extractMarkdownSection(planContent, heading)
		if approach != "" {
			break
		}
	}
	if approach == "" {
		approach = "(Populated by Implement)"
	}

	body := fmt.Sprintf(`Closes #%d

## Summary

%s

## Problem

%s

## Approach

%s

## Verification

(Populated by Implement on completion)

---

Closes #%d`, issueNumber, summary, problem, approach, issueNumber)
	return balanceFences(body)
}

// replaceVerificationSection replaces the content of the `## Verification` section in
// prBody with summary. It preserves everything from the next `## ` heading or `---`
// divider onward (ensuring "Closes #N" at the bottom is always preserved).
// Returns the updated body and true if the section was found, or the original body and
// false if `## Verification` is not present in prBody.
func replaceVerificationSection(prBody, summary string) (string, bool) {
	lines := strings.Split(prBody, "\n")
	verificationIdx := -1
	for i, line := range lines {
		if strings.EqualFold(strings.TrimSpace(line), "## verification") {
			verificationIdx = i
			break
		}
	}
	if verificationIdx < 0 {
		return prBody, false
	}

	// Find the end of the Verification section: next ## heading, --- divider, or Closes # footer.
	// Stopping at "Closes #" ensures the issue link is preserved even when the --- divider
	// is absent (e.g., manually edited PR body).
	endIdx := len(lines)
	for i := verificationIdx + 1; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if strings.HasPrefix(trimmed, "## ") || trimmed == "---" || strings.HasPrefix(trimmed, "Closes #") {
			endIdx = i
			break
		}
	}

	// Rebuild: heading + new content + rest
	var result []string
	result = append(result, lines[:verificationIdx+1]...) // up to and including ## Verification
	result = append(result, "")
	result = append(result, summary)
	result = append(result, "")
	result = append(result, lines[endIdx:]...) // from --- or next ## onward
	joined := balanceFences(strings.Join(result, "\n"))
	return strings.TrimRight(joined, "\n"), true
}
