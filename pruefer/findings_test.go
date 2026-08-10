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

// TestParseReviewFindings_1444Regression pins AC1: reproduced from
// handarbeit/fabrik#1444's actual review body shape (mid-investigation
// narration with no antecedent, immediately followed by the real summary)
// — but, unlike the issue's raw "Reproduction" snippet, wrapped in
// PRUEFER_SUMMARY_BEGIN/END markers, since that's the fixed shape this test
// proves the parser now handles. Per AC6, this fixture was hand-verified to
// fail (return the narration prefix as part of the summary) before Task 3's
// splitSummaryMarkers/parseReviewFindings changes landed.
func TestParseReviewFindings_1444Regression(t *testing.T) {
	narration := "That file exists, so the reference is valid. No issues found there either."
	realSummary := "## Review Summary\n\nReviewed `engine/startup.go`'s `checkStageColumnAlignment`, the corresponding\ntests in `engine/startup_test.go`, the new ADR, and confirmed the referenced\nfile exists. No issues found."
	text := narration + "\n\n" +
		"PRUEFER_SUMMARY_BEGIN\n" +
		realSummary + "\n" +
		"PRUEFER_SUMMARY_END\n\n" +
		"```json\n[]\n```\n"

	summary, findings, info := parseReviewFindings(text)
	if summary != realSummary {
		t.Errorf("summary = %q, want %q (narration must not leak into the summary)", summary, realSummary)
	}
	if strings.Contains(summary, narration) {
		t.Errorf("summary = %q contains the narration prefix, want it discarded", summary)
	}
	if len(findings) != 0 {
		t.Errorf("findings = %+v, want empty", findings)
	}
	if !info.MarkersFound {
		t.Errorf("info.MarkersFound = false, want true")
	}
	if info.DiscardedBytes != len(strings.TrimSpace(narration)) {
		t.Errorf("info.DiscardedBytes = %d, want %d", info.DiscardedBytes, len(strings.TrimSpace(narration)))
	}
}

// TestParseReviewFindings_MarkersPresent_ZeroPreamble covers the AC5/R4
// happy path: markers immediately open the text, so there is no preamble to
// discard.
func TestParseReviewFindings_MarkersPresent_ZeroPreamble(t *testing.T) {
	text := "PRUEFER_SUMMARY_BEGIN\nAll good here.\nPRUEFER_SUMMARY_END\n\n```json\n[]\n```\n"

	summary, _, info := parseReviewFindings(text)
	if summary != "All good here." {
		t.Errorf("summary = %q", summary)
	}
	if !info.MarkersFound {
		t.Errorf("info.MarkersFound = false, want true")
	}
	if info.DiscardedBytes != 0 {
		t.Errorf("info.DiscardedBytes = %d, want 0", info.DiscardedBytes)
	}
}

// TestParseReviewFindings_BeginWithoutEnd_FallsBack covers R3's malformed-
// pair handling: a BEGIN marker with no matching END falls all the way back
// to positional extraction, not a partial-extraction state.
func TestParseReviewFindings_BeginWithoutEnd_FallsBack(t *testing.T) {
	text := "PRUEFER_SUMMARY_BEGIN\nSome text with no closing marker.\n\n```json\n[]\n```\n"

	summary, _, info := parseReviewFindings(text)
	if info.MarkersFound {
		t.Errorf("info.MarkersFound = true, want false (no END marker)")
	}
	if info.DiscardedBytes != 0 {
		t.Errorf("info.DiscardedBytes = %d, want 0 on the fallback path", info.DiscardedBytes)
	}
	want := strings.TrimSpace(text[:strings.Index(text, "```json")])
	if summary != want {
		t.Errorf("summary = %q, want positional fallback %q", summary, want)
	}
}

// TestParseReviewFindings_EndWithoutBegin_FallsBack covers the other
// malformed-pair case: an END marker with no preceding BEGIN also falls
// back to positional extraction.
func TestParseReviewFindings_EndWithoutBegin_FallsBack(t *testing.T) {
	text := "Some text.\nPRUEFER_SUMMARY_END\nMore text.\n\n```json\n[]\n```\n"

	summary, _, info := parseReviewFindings(text)
	if info.MarkersFound {
		t.Errorf("info.MarkersFound = true, want false (no BEGIN marker)")
	}
	want := strings.TrimSpace(text[:strings.Index(text, "```json")])
	if summary != want {
		t.Errorf("summary = %q, want positional fallback %q", summary, want)
	}
}

// TestParseReviewFindings_EndBeforeBegin_FallsBack: an END marker that
// appears earlier in the text than BEGIN is also treated as a malformed
// pair (R3) — splitSummaryMarkers only searches for END after BEGIN's own
// offset, so an END preceding BEGIN is invisible to that search and the
// pair is correctly reported as not found.
func TestParseReviewFindings_EndBeforeBegin_FallsBack(t *testing.T) {
	text := "PRUEFER_SUMMARY_END\nleftover text\nPRUEFER_SUMMARY_BEGIN\nreal summary\n\n```json\n[]\n```\n"

	summary, _, info := parseReviewFindings(text)
	if info.MarkersFound {
		t.Errorf("info.MarkersFound = true, want false (END precedes BEGIN)")
	}
	want := strings.TrimSpace(text[:strings.Index(text, "```json")])
	if summary != want {
		t.Errorf("summary = %q, want positional fallback %q", summary, want)
	}
}

// TestParseReviewFindings_MultiParagraphSummaryWithHeadingSurvivesIntact
// pins R5: a legitimate multi-paragraph summary that itself contains a
// "## Review Summary"-style heading must survive intact inside the markers
// — discarding is driven only by the explicit marker, never by a heuristic
// like "drop text before the first heading."
func TestParseReviewFindings_MultiParagraphSummaryWithHeadingSurvivesIntact(t *testing.T) {
	realSummary := "## Review Summary\n\nFirst paragraph of legitimate content.\n\nSecond paragraph, also legitimate, with its own ## Subheading inside it."
	text := "PRUEFER_SUMMARY_BEGIN\n" + realSummary + "\nPRUEFER_SUMMARY_END\n\n```json\n[]\n```\n"

	summary, _, info := parseReviewFindings(text)
	if summary != realSummary {
		t.Errorf("summary = %q, want the full multi-paragraph summary with headings intact: %q", summary, realSummary)
	}
	if !info.MarkersFound {
		t.Errorf("info.MarkersFound = false, want true")
	}
}

// TestParseReviewFindings_MarkersFound_NoFence covers the case where
// markers are present but the findings fence is entirely absent — the
// delimited summary must still be used (findings-fence presence is
// independent of marker extraction).
func TestParseReviewFindings_MarkersFound_NoFence(t *testing.T) {
	text := "narration\n\nPRUEFER_SUMMARY_BEGIN\nreal summary, no fence follows\nPRUEFER_SUMMARY_END\n"

	summary, findings, info := parseReviewFindings(text)
	if summary != "real summary, no fence follows" {
		t.Errorf("summary = %q", summary)
	}
	if findings != nil {
		t.Errorf("findings = %+v, want nil", findings)
	}
	if !info.MarkersFound {
		t.Errorf("info.MarkersFound = false, want true")
	}
}

// TestDecideEvent_UnaffectedBySummaryMarkers pins AC4: the same findings
// JSON, with and without summary markers present around it, produces an
// identical findings slice and decideEvent verdict — the summary-parsing
// change is structurally isolated from findings extraction and severity
// decisions.
func TestDecideEvent_UnaffectedBySummaryMarkers(t *testing.T) {
	findingsJSON := `[{"path":"a.go","line":1,"body":"bug","severity":"critical"}]`
	withoutMarkers := "Some summary.\n\n```json\n" + findingsJSON + "\n```\n"
	withMarkers := "narration\n\nPRUEFER_SUMMARY_BEGIN\nSome summary.\nPRUEFER_SUMMARY_END\n\n```json\n" + findingsJSON + "\n```\n"

	_, findingsA, _ := parseReviewFindings(withoutMarkers)
	_, findingsB, _ := parseReviewFindings(withMarkers)

	if len(findingsA) != 1 || len(findingsB) != 1 {
		t.Fatalf("findingsA = %+v, findingsB = %+v, want 1 entry each", findingsA, findingsB)
	}
	if findingsA[0] != findingsB[0] {
		t.Errorf("findingsA[0] = %+v, findingsB[0] = %+v, want identical", findingsA[0], findingsB[0])
	}

	eventA := decideEvent(findingsA, SeverityLow)
	eventB := decideEvent(findingsB, SeverityLow)
	if eventA != eventB {
		t.Errorf("decideEvent(findingsA) = %v, decideEvent(findingsB) = %v, want identical", eventA, eventB)
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
