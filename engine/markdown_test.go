package engine

import (
	"strings"
	"testing"
)

// ── extractMarkdownSection ────────────────────────────────────────────────────

func TestExtractMarkdownSection_Found(t *testing.T) {
	content := `## Summary

This is the summary.

## Problem

This is the problem.
`
	got := extractMarkdownSection(content, "Summary")
	if got != "This is the summary." {
		t.Errorf("got %q, want %q", got, "This is the summary.")
	}
}

func TestExtractMarkdownSection_NotFound(t *testing.T) {
	content := `## Summary

Some content.
`
	got := extractMarkdownSection(content, "Approach")
	if got != "" {
		t.Errorf("got %q, want empty string", got)
	}
}

func TestExtractMarkdownSection_CaseInsensitive(t *testing.T) {
	content := `## SUMMARY

Upper case heading.
`
	got := extractMarkdownSection(content, "summary")
	if got != "Upper case heading." {
		t.Errorf("got %q, want %q", got, "Upper case heading.")
	}
}

func TestExtractMarkdownSection_MetadataHeaderPrefix(t *testing.T) {
	// stage-Plan.md starts with a non-## header before the first section
	content := "🏭 **Fabrik — stage: Plan**\n*branch: fabrik/issue-42 | commit: abc1234*\n\nSome preamble.\n\n## Approach\n\nThe approach content.\n\n## Risks\n\nSome risks.\n"
	got := extractMarkdownSection(content, "Approach")
	if got != "The approach content." {
		t.Errorf("got %q, want %q", got, "The approach content.")
	}
}

func TestExtractMarkdownSection_MultipleHeadings(t *testing.T) {
	content := `## First

First section.

## Second

Second section.

## Third

Third section.
`
	got := extractMarkdownSection(content, "Second")
	if got != "Second section." {
		t.Errorf("got %q, want %q", got, "Second section.")
	}
}

func TestExtractMarkdownSection_MultilineContent(t *testing.T) {
	content := `## Problem

Line one of problem.
Line two of problem.

More detail here.

## Solution

The solution.
`
	got := extractMarkdownSection(content, "Problem")
	want := "Line one of problem.\nLine two of problem.\n\nMore detail here."
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// ── buildPRSeedBody ───────────────────────────────────────────────────────────

func TestBuildPRSeedBody_BothFilesPresent(t *testing.T) {
	issueContent := `## Summary

This PR adds rich PR bodies.

## Problem

PR bodies were bare.
`
	planContent := "🏭 **Fabrik — stage: Plan**\n*branch: fabrik/issue-42*\n\n## Approach\n\nUse markdown parsing.\n"

	body := buildPRSeedBody(issueContent, planContent, 42)

	if !strings.Contains(body, "## Summary") {
		t.Error("body missing ## Summary")
	}
	if !strings.Contains(body, "This PR adds rich PR bodies.") {
		t.Error("body missing summary content")
	}
	if !strings.Contains(body, "## Problem") {
		t.Error("body missing ## Problem")
	}
	if !strings.Contains(body, "PR bodies were bare.") {
		t.Error("body missing problem content")
	}
	if !strings.Contains(body, "## Approach") {
		t.Error("body missing ## Approach")
	}
	if !strings.Contains(body, "Use markdown parsing.") {
		t.Error("body missing approach content")
	}
	if !strings.Contains(body, "## Verification") {
		t.Error("body missing ## Verification")
	}
	if !strings.Contains(body, "Closes #42") {
		t.Error("body missing Closes #42")
	}
	if !strings.HasSuffix(strings.TrimSpace(body), "Closes #42") {
		t.Errorf("Closes #42 must be at the end, got: %q", body[len(body)-30:])
	}
}

func TestBuildPRSeedBody_MissingPlanFile(t *testing.T) {
	issueContent := `## Summary

Brief summary.
`
	body := buildPRSeedBody(issueContent, "", 7)

	if !strings.Contains(body, "(Populated by Implement)") {
		t.Error("body should contain Approach placeholder when plan is missing")
	}
	if !strings.HasSuffix(strings.TrimSpace(body), "Closes #7") {
		t.Errorf("Closes #7 must be at the end")
	}
}

func TestBuildPRSeedBody_MissingSections_FallbackToFirstParagraph(t *testing.T) {
	// No ## Summary or ## Problem headings — should fall back to first paragraph
	issueContent := "This is the only content in the issue.\n\nThis is a second paragraph."
	body := buildPRSeedBody(issueContent, "", 3)

	if !strings.Contains(body, "This is the only content in the issue.") {
		t.Errorf("should fall back to first paragraph for Summary, got body: %q", body)
	}
}

func TestBuildPRSeedBody_PlanWithImplementationPlanHeading(t *testing.T) {
	planContent := "## Implementation Plan\n\nDetailed plan here.\n"
	body := buildPRSeedBody("## Summary\n\nS.\n", planContent, 5)
	if !strings.Contains(body, "Detailed plan here.") {
		t.Error("should extract Implementation Plan section")
	}
}

func TestBuildPRSeedBody_PlanWithPlanHeading(t *testing.T) {
	planContent := "## Plan\n\nPlan content here.\n"
	body := buildPRSeedBody("## Summary\n\nS.\n", planContent, 5)
	if !strings.Contains(body, "Plan content here.") {
		t.Error("should extract Plan section")
	}
}

func TestBuildPRSeedBody_ClosesAtEnd(t *testing.T) {
	body := buildPRSeedBody("## Summary\n\nS.\n## Problem\n\nP.\n", "## Approach\n\nA.\n", 99)
	trimmed := strings.TrimSpace(body)
	if !strings.HasSuffix(trimmed, "Closes #99") {
		t.Errorf("Closes #99 must be at end, body tail: %q", trimmed[max(0, len(trimmed)-50):])
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// ── replaceVerificationSection ────────────────────────────────────────────────

func TestReplaceVerificationSection_Replaces(t *testing.T) {
	prBody := `## Summary

Some summary.

## Verification

(Populated by Implement on completion)

---

Closes #42`

	updated, ok := replaceVerificationSection(prBody, "All tests pass.")
	if !ok {
		t.Fatal("expected ok=true")
	}
	if !strings.Contains(updated, "All tests pass.") {
		t.Error("updated body missing summary content")
	}
	if strings.Contains(updated, "(Populated by Implement on completion)") {
		t.Error("placeholder should have been replaced")
	}
}

func TestReplaceVerificationSection_PreservesDividerAndClosesN(t *testing.T) {
	prBody := `## Verification

(Populated by Implement on completion)

---

Closes #10`

	updated, ok := replaceVerificationSection(prBody, "Verified!")
	if !ok {
		t.Fatal("expected ok=true")
	}
	if !strings.Contains(updated, "Closes #10") {
		t.Error("Closes #10 must be preserved")
	}
	if !strings.Contains(updated, "---") {
		t.Error("--- divider must be preserved")
	}
	if !strings.Contains(updated, "Verified!") {
		t.Error("new summary content must be present")
	}
}

func TestReplaceVerificationSection_SectionNotFound(t *testing.T) {
	prBody := `## Summary

Some summary.

---

Closes #5`

	original := prBody
	updated, ok := replaceVerificationSection(prBody, "Some summary.")
	if ok {
		t.Error("expected ok=false when section not found")
	}
	if updated != original {
		t.Error("body should be unchanged when section not found")
	}
}

func TestReplaceVerificationSection_Idempotent(t *testing.T) {
	prBody := `## Verification

Old content.

---

Closes #1`

	updated1, ok1 := replaceVerificationSection(prBody, "New content.")
	if !ok1 {
		t.Fatal("first replacement failed")
	}
	updated2, ok2 := replaceVerificationSection(updated1, "Newest content.")
	if !ok2 {
		t.Fatal("second replacement failed")
	}
	if strings.Contains(updated2, "Old content.") {
		t.Error("old content should not appear after second replacement")
	}
	if strings.Contains(updated2, "New content.") {
		t.Error("first replacement content should not appear after second replacement")
	}
	if !strings.Contains(updated2, "Newest content.") {
		t.Error("latest content should appear")
	}
	if !strings.Contains(updated2, "Closes #1") {
		t.Error("Closes #1 must be preserved after both replacements")
	}
}

func TestReplaceVerificationSection_CaseInsensitive(t *testing.T) {
	prBody := "## Verification\n\nold.\n\n---\n\nCloses #2"
	updated, ok := replaceVerificationSection(prBody, "new.")
	if !ok {
		t.Fatal("expected ok=true")
	}
	if !strings.Contains(updated, "new.") {
		t.Error("new content missing")
	}
}
