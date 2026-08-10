package pruefer

import (
	"strings"
	"testing"
)

func TestParseReviewFindings_WellFormedFence(t *testing.T) {
	text := "Overall looks good, one nit below.\n\n```json\n" +
		`[{"path":"engine/claude.go","line":954,"body":"missing edge case"}]` +
		"\n```\n"

	summary, findings, _ := parseReviewFindings(text)
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

	summary, findings, _ := parseReviewFindings(text)
	if summary != "Summary text." {
		t.Errorf("summary = %q", summary)
	}
	if len(findings) != 2 {
		t.Fatalf("findings = %+v, want 2 entries", findings)
	}
}

func TestParseReviewFindings_NoFence_WholeTextAsSummary(t *testing.T) {
	text := "Looks fine, one nit."
	summary, findings, _ := parseReviewFindings(text)
	if summary != text {
		t.Errorf("summary = %q, want %q", summary, text)
	}
	if findings != nil {
		t.Errorf("findings = %+v, want nil", findings)
	}
}

func TestParseReviewFindings_MalformedJSONInFence_FallsBack(t *testing.T) {
	text := "Some review text.\n\n```json\nnot valid json at all\n```\n"
	summary, findings, _ := parseReviewFindings(text)
	if want := strings.TrimSpace(text); summary != want {
		t.Errorf("summary = %q, want the whole original text (trimmed) as fallback: %q", summary, want)
	}
	if findings != nil {
		t.Errorf("findings = %+v, want nil on malformed JSON", findings)
	}
}

func TestParseReviewFindings_EmptyFindingsArray(t *testing.T) {
	text := "Nothing to flag.\n\n```json\n[]\n```\n"
	summary, findings, _ := parseReviewFindings(text)
	if summary != "Nothing to flag." {
		t.Errorf("summary = %q", summary)
	}
	if len(findings) != 0 {
		t.Errorf("findings = %+v, want empty", findings)
	}
}

func TestParseReviewFindings_IgnoresNonJSONFence(t *testing.T) {
	text := "Consider this fix:\n\n```go\nfunc foo() {}\n```\n\nOtherwise fine."
	summary, findings, _ := parseReviewFindings(text)
	if summary != text {
		t.Errorf("summary = %q, want whole text (no ```json fence present)", summary)
	}
	if findings != nil {
		t.Errorf("findings = %+v, want nil", findings)
	}
}

func TestBuildReviewBody_NoDemoted_ReturnsSummaryUnchanged(t *testing.T) {
	got := buildReviewBody("All good, minor nit inline.", nil)
	if got != "All good, minor nit inline." {
		t.Errorf("got %q", got)
	}
}

func TestBuildReviewBody_DemotedFindings_AppendedUnderHeading(t *testing.T) {
	demoted := []ReviewFinding{
		{Path: "vendor/lib.go", Line: 12, Body: "not part of this PR's diff"},
	}
	got := buildReviewBody("Reviewed the change.", demoted)
	want := "Reviewed the change.\n\n## Additional findings (could not anchor to diff)\n\n**vendor/lib.go:12**: not part of this PR's diff"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildReviewBody_EmptySummary_DemotedFindings_NoLeadingBlankLines(t *testing.T) {
	demoted := []ReviewFinding{{Path: "a.go", Line: 1, Body: "finding"}}
	got := buildReviewBody("", demoted)
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
	got := buildReviewBody("Summary.", demoted)
	want := "Summary.\n\n## Additional findings (could not anchor to diff)\n\n**a.go:1**: finding A\n\n**b.go:2**: finding B"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestDedupeFindings_SamePathLine_Collapses pins AC3: two findings on the
// same path and line collapse to one.
func TestDedupeFindings_SamePathLine_Collapses(t *testing.T) {
	findings := []ReviewFinding{
		{Path: "engine/board.go", Line: 42, Body: "ArchiveProjectItem bumps updatedAt on a re-archive no-op", Severity: SeverityMedium},
		{Path: "engine/board.go", Line: 42, Body: "ArchiveProjectItem bumps updatedAt again", Severity: SeverityMedium},
	}
	got := dedupeFindings(findings)
	if len(got) != 1 {
		t.Fatalf("dedupeFindings = %+v, want 1 entry", got)
	}
	if got[0].Path != "engine/board.go" || got[0].Line != 42 {
		t.Errorf("got[0] = %+v", got[0])
	}
}

// TestDedupeFindings_SeverityCollision_KeepsHigherSeverity pins the
// max-severity-on-collision policy: collapsing duplicates must never
// silently weaken a REQUEST_CHANGES threshold decision.
func TestDedupeFindings_SeverityCollision_KeepsHigherSeverity(t *testing.T) {
	findings := []ReviewFinding{
		{Path: "a.go", Line: 1, Body: "low take", Severity: SeverityLow},
		{Path: "a.go", Line: 1, Body: "critical take", Severity: SeverityCritical},
	}
	got := dedupeFindings(findings)
	if len(got) != 1 {
		t.Fatalf("dedupeFindings = %+v, want 1 entry", got)
	}
	if got[0].Severity != SeverityCritical {
		t.Errorf("got[0].Severity = %q, want %q", got[0].Severity, SeverityCritical)
	}
}

// TestDedupeFindings_DistinctBodiesOnCollision_BothPreserved guards against
// dedupeFindings silently discarding a genuinely distinct finding that
// happens to share an anchor with another: unlike partitionFindings (which
// happily emits multiple separate inline comments at one line), dedup must
// not pick a winner and drop the loser's content with no trace.
func TestDedupeFindings_DistinctBodiesOnCollision_BothPreserved(t *testing.T) {
	findings := []ReviewFinding{
		{Path: "a.go", Line: 1, Body: "out-of-bounds slice access", Severity: SeverityHigh},
		{Path: "a.go", Line: 1, Body: "unrelated: this branch never releases the lock", Severity: SeverityMedium},
	}
	got := dedupeFindings(findings)
	if len(got) != 1 {
		t.Fatalf("dedupeFindings = %+v, want 1 entry", got)
	}
	if !strings.Contains(got[0].Body, "out-of-bounds slice access") {
		t.Errorf("got[0].Body = %q, missing first finding's text", got[0].Body)
	}
	if !strings.Contains(got[0].Body, "unrelated: this branch never releases the lock") {
		t.Errorf("got[0].Body = %q, missing second finding's text", got[0].Body)
	}
	if got[0].Severity != SeverityHigh {
		t.Errorf("got[0].Severity = %q, want %q", got[0].Severity, SeverityHigh)
	}
}

// TestDedupeFindings_IdenticalBodyOnCollision_NotDuplicatedInMerge ensures
// the literal-restatement case (the one #1477 actually exhibited) doesn't
// produce a merged body with the same sentence repeated twice.
func TestDedupeFindings_IdenticalBodyOnCollision_NotDuplicatedInMerge(t *testing.T) {
	findings := []ReviewFinding{
		{Path: "a.go", Line: 1, Body: "same exact restatement", Severity: SeverityMedium},
		{Path: "a.go", Line: 1, Body: "same exact restatement", Severity: SeverityMedium},
	}
	got := dedupeFindings(findings)
	if len(got) != 1 {
		t.Fatalf("dedupeFindings = %+v, want 1 entry", got)
	}
	if got[0].Body != "same exact restatement" {
		t.Errorf("got[0].Body = %q, want the single unduplicated body", got[0].Body)
	}
}

// TestDedupeFindings_DistinctAnchors_Unchanged proves dedup only collapses
// true (Path, Line) collisions, not findings on distinct anchors.
func TestDedupeFindings_DistinctAnchors_Unchanged(t *testing.T) {
	findings := []ReviewFinding{
		{Path: "a.go", Line: 1, Body: "finding A"},
		{Path: "a.go", Line: 2, Body: "finding B"},
		{Path: "b.go", Line: 1, Body: "finding C"},
	}
	got := dedupeFindings(findings)
	if len(got) != 3 {
		t.Fatalf("dedupeFindings = %+v, want 3 entries (no true collisions)", got)
	}
}
