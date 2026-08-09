package pruefer

import (
	"fmt"
	"strings"
	"testing"
)

func TestParseReviewFindings_WellFormedFence(t *testing.T) {
	text := "Overall looks good, one nit below.\n\n```json\n" +
		`[{"path":"engine/claude.go","line":954,"body":"missing edge case"}]` +
		"\n```\n"

	summary, findings := parseReviewFindings(text)
	if summary != "Overall looks good, one nit below." {
		t.Errorf("summary = %q", summary)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %+v, want 1 entry", findings)
	}
	if findings[0].Path != "engine/claude.go" || findings[0].Line != 954 || findings[0].Body != "missing edge case" {
		t.Errorf("findings[0] = %+v", findings[0])
	}
}

func TestParseReviewFindings_MultipleFindings(t *testing.T) {
	text := "Summary text.\n\n```json\n" +
		`[{"path":"a.go","line":1,"body":"finding a"},{"path":"b.go","line":2,"body":"finding b"}]` +
		"\n```\n"

	summary, findings := parseReviewFindings(text)
	if summary != "Summary text." {
		t.Errorf("summary = %q", summary)
	}
	if len(findings) != 2 {
		t.Fatalf("findings = %+v, want 2 entries", findings)
	}
}

func TestParseReviewFindings_NoFence_WholeTextAsSummary(t *testing.T) {
	text := "Looks fine, one nit."
	summary, findings := parseReviewFindings(text)
	if summary != text {
		t.Errorf("summary = %q, want %q", summary, text)
	}
	if findings != nil {
		t.Errorf("findings = %+v, want nil", findings)
	}
}

func TestParseReviewFindings_MalformedJSONInFence_FallsBack(t *testing.T) {
	text := "Some review text.\n\n```json\nnot valid json at all\n```\n"
	summary, findings := parseReviewFindings(text)
	if want := strings.TrimSpace(text); summary != want {
		t.Errorf("summary = %q, want the whole original text (trimmed) as fallback: %q", summary, want)
	}
	if findings != nil {
		t.Errorf("findings = %+v, want nil on malformed JSON", findings)
	}
}

func TestParseReviewFindings_EmptyFindingsArray(t *testing.T) {
	text := "Nothing to flag.\n\n```json\n[]\n```\n"
	summary, findings := parseReviewFindings(text)
	if summary != "Nothing to flag." {
		t.Errorf("summary = %q", summary)
	}
	if len(findings) != 0 {
		t.Errorf("findings = %+v, want empty", findings)
	}
}

func TestParseReviewFindings_IgnoresNonJSONFence(t *testing.T) {
	text := "Consider this fix:\n\n```go\nfunc foo() {}\n```\n\nOtherwise fine."
	summary, findings := parseReviewFindings(text)
	if summary != text {
		t.Errorf("summary = %q, want whole text (no ```json fence present)", summary)
	}
	if findings != nil {
		t.Errorf("findings = %+v, want nil", findings)
	}
}

func TestBuildReviewBody_NoDemoted_ReturnsSummaryUnchanged(t *testing.T) {
	got := buildReviewBody("All good, minor nit inline.", nil, nil)
	if got != "All good, minor nit inline." {
		t.Errorf("got %q", got)
	}
}

func TestBuildReviewBody_DemotedFindings_AppendedUnderHeading(t *testing.T) {
	demoted := []ReviewFinding{
		{Path: "vendor/lib.go", Line: 12, Body: "not part of this PR's diff"},
	}
	got := buildReviewBody("Reviewed the change.", demoted, nil)
	want := "Reviewed the change.\n\n## Additional findings (could not anchor to diff)\n\n**vendor/lib.go:12**: not part of this PR's diff"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildReviewBody_EmptySummary_DemotedFindings_NoLeadingBlankLines(t *testing.T) {
	demoted := []ReviewFinding{{Path: "a.go", Line: 1, Body: "finding"}}
	got := buildReviewBody("", demoted, nil)
	want := "## Additional findings (could not anchor to diff)\n\n**a.go:1**: finding"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildReviewBody_MultipleDemotedFindings(t *testing.T) {
	demoted := []ReviewFinding{
		{Path: "a.go", Line: 1, Body: "finding A"},
		{Path: "b.go", Line: 2, Body: "finding B"},
	}
	got := buildReviewBody("Summary.", demoted, nil)
	want := "Summary.\n\n## Additional findings (could not anchor to diff)\n\n**a.go:1**: finding A\n\n**b.go:2**: finding B"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildReviewBody_OmittedPaths_AppendedUnderHeading(t *testing.T) {
	got := buildReviewBody("Reviewed the rest.", nil, []string{"packages/evals/corpus/quality-bench-corpus.jsonl"})
	want := "Reviewed the rest.\n\n## Omitted from this review\n- `packages/evals/corpus/quality-bench-corpus.jsonl`"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildReviewBody_DemotedAndOmitted_BothSectionsPresent(t *testing.T) {
	demoted := []ReviewFinding{{Path: "a.go", Line: 1, Body: "finding"}}
	got := buildReviewBody("Summary.", demoted, []string{"big.jsonl"})
	want := "Summary.\n\n## Additional findings (could not anchor to diff)\n\n**a.go:1**: finding\n\n## Omitted from this review\n- `big.jsonl`"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildReviewBody_EmptyOmittedPaths_NoSection(t *testing.T) {
	got := buildReviewBody("Summary.", nil, []string{})
	if got != "Summary." {
		t.Errorf("got %q, want unchanged summary for empty (non-nil) omittedPaths", got)
	}
}

// TestBuildReviewBody_ManyOmittedPaths_ListIsBounded proves the submitted
// review body's "Omitted from this review" section is bounded the same way
// as the too-large notice's path lists (notice.go's noticePathListLimit): a
// broad excluded_paths glob can match hundreds of files just as easily here
// as it can there, and an unbounded list risks exceeding GitHub's PR review
// body size limit, losing the whole review — including any real findings.
func TestBuildReviewBody_ManyOmittedPaths_ListIsBounded(t *testing.T) {
	paths := make([]string, 500)
	for i := range paths {
		paths[i] = fmt.Sprintf("vendor/pkg%d/file.go", i)
	}
	got := buildReviewBody("Summary.", nil, paths)

	if len(got) > 10_000 {
		t.Errorf("body length = %d, want well under GitHub's review-body size limit for %d omitted paths", len(got), len(paths))
	}
	if !strings.Contains(got, "and 480 more") {
		t.Errorf("got %q, want a truncation summary naming how many omitted paths were left off the list", got)
	}
}
