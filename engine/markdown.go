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

// buildPRSeedBody constructs the structured PR body template from issue and plan context.
// issueContent is the contents of .fabrik-context/issue.md.
// planContent is the contents of .fabrik-context/stage-Plan.md (may be empty if missing).
// issueNumber is used to add the closing reference at the bottom.
func buildPRSeedBody(issueContent, planContent string, issueNumber int) string {
	// Extract Summary from issue; fall back to first paragraph
	summary := extractMarkdownSection(issueContent, "Summary")
	if summary == "" {
		summary = firstParagraph(issueContent)
	}
	if summary == "" {
		summary = "(no summary available)"
	}

	// Extract Problem from issue; fall back to first paragraph
	problem := extractMarkdownSection(issueContent, "Problem")
	if problem == "" {
		problem = firstParagraph(issueContent)
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

	return fmt.Sprintf(`## Summary

%s

## Problem

%s

## Approach

%s

## Verification

(Populated by Implement on completion)

---

Closes #%d`, summary, problem, approach, issueNumber)
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
	return strings.TrimRight(strings.Join(result, "\n"), "\n"), true
}
