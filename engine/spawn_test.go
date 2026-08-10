package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

// planCommentBody returns a fake Plan stage comment body containing the given spawn blocks.
func planCommentWithBlocks(blocksRaw string) string {
	return "🏭 **Fabrik — stage: Plan**\n\n" + blocksRaw
}

// ---- ParseSpawnBlocks unit tests ----

func TestParseSpawnBlocks_Empty(t *testing.T) {
	blocks := ParseSpawnBlocks("no spawn blocks here")
	if len(blocks) != 0 {
		t.Fatalf("expected 0 blocks, got %d", len(blocks))
	}
}

func TestParseSpawnBlocks_SingleBlock(t *testing.T) {
	body := `
FABRIK_SPAWN_CHILD_BEGIN owner/child-repo
TITLE: Add authentication module
Implement OAuth2 authentication.
FABRIK_SPAWN_CHILD_END
`
	blocks := ParseSpawnBlocks(body)
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	b := blocks[0]
	if b.Repo != "owner/child-repo" {
		t.Errorf("repo: got %q, want %q", b.Repo, "owner/child-repo")
	}
	if b.Title != "Add authentication module" {
		t.Errorf("title: got %q, want %q", b.Title, "Add authentication module")
	}
	if !strings.Contains(b.Body, "Implement OAuth2") {
		t.Errorf("body should contain body text, got: %q", b.Body)
	}
}

func TestParseSpawnBlocks_MultipleBlocks(t *testing.T) {
	body := `
FABRIK_SPAWN_CHILD_BEGIN owner/repo-a
TITLE: First child
First body.
FABRIK_SPAWN_CHILD_END

FABRIK_SPAWN_CHILD_BEGIN owner/repo-b
TITLE: Second child
Second body.
FABRIK_SPAWN_CHILD_END
`
	blocks := ParseSpawnBlocks(body)
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(blocks))
	}
	if blocks[0].Title != "First child" {
		t.Errorf("block[0] title: got %q", blocks[0].Title)
	}
	if blocks[1].Title != "Second child" {
		t.Errorf("block[1] title: got %q", blocks[1].Title)
	}
	if blocks[0].Repo != "owner/repo-a" {
		t.Errorf("block[0] repo: got %q", blocks[0].Repo)
	}
	if blocks[1].Repo != "owner/repo-b" {
		t.Errorf("block[1] repo: got %q", blocks[1].Repo)
	}
}

func TestParseSpawnBlocks_MissingRepo(t *testing.T) {
	body := `FABRIK_SPAWN_CHILD_BEGIN
TITLE: No repo given
body
FABRIK_SPAWN_CHILD_END`
	blocks := ParseSpawnBlocks(body)
	if len(blocks) != 0 {
		t.Fatalf("expected 0 blocks for malformed BEGIN (no repo), got %d", len(blocks))
	}
}

func TestParseSpawnBlocks_RepoWithoutSlash(t *testing.T) {
	body := `FABRIK_SPAWN_CHILD_BEGIN noslash
TITLE: Bad repo
body
FABRIK_SPAWN_CHILD_END`
	blocks := ParseSpawnBlocks(body)
	if len(blocks) != 0 {
		t.Fatalf("expected 0 blocks for repo without slash, got %d", len(blocks))
	}
}

func TestParseSpawnBlocks_MissingEnd(t *testing.T) {
	body := `FABRIK_SPAWN_CHILD_BEGIN owner/repo
TITLE: No end marker
body`
	blocks := ParseSpawnBlocks(body)
	if len(blocks) != 0 {
		t.Fatalf("expected 0 blocks for missing END, got %d", len(blocks))
	}
}

func TestParseSpawnBlocks_MissingTitle(t *testing.T) {
	body := `FABRIK_SPAWN_CHILD_BEGIN owner/repo
just body, no TITLE: line
FABRIK_SPAWN_CHILD_END`
	blocks := ParseSpawnBlocks(body)
	if len(blocks) != 0 {
		t.Fatalf("expected 0 blocks for missing TITLE, got %d", len(blocks))
	}
}

func TestParseSpawnBlocks_EmptyTitle(t *testing.T) {
	body := `FABRIK_SPAWN_CHILD_BEGIN owner/repo
TITLE:
body
FABRIK_SPAWN_CHILD_END`
	blocks := ParseSpawnBlocks(body)
	if len(blocks) != 0 {
		t.Fatalf("expected 0 blocks for empty TITLE, got %d", len(blocks))
	}
}

func TestParseSpawnBlocks_BodyEmpty(t *testing.T) {
	body := `FABRIK_SPAWN_CHILD_BEGIN owner/repo
TITLE: Title only
FABRIK_SPAWN_CHILD_END`
	blocks := ParseSpawnBlocks(body)
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	if blocks[0].Body != "" {
		t.Errorf("expected empty body, got %q", blocks[0].Body)
	}
}

// ---- DEPENDS_ON parser tests (#1337) ----

func TestParseSpawnBlocks_DependsOnValid(t *testing.T) {
	body := `
FABRIK_SPAWN_CHILD_BEGIN owner/repo
TITLE: First slice
First body.
FABRIK_SPAWN_CHILD_END

FABRIK_SPAWN_CHILD_BEGIN owner/repo
TITLE: Second slice
DEPENDS_ON: 1
Second body.
FABRIK_SPAWN_CHILD_END
`
	blocks := ParseSpawnBlocks(body)
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(blocks))
	}
	if blocks[0].DependsOnDeclared {
		t.Error("block 0 should not have DEPENDS_ON declared")
	}
	if !blocks[1].DependsOnDeclared {
		t.Fatal("block 1 should have DEPENDS_ON declared")
	}
	if blocks[1].DependsOn != 1 {
		t.Errorf("DependsOn: got %d, want 1", blocks[1].DependsOn)
	}
	if blocks[1].DependsOnRaw != "1" {
		t.Errorf("DependsOnRaw: got %q, want %q", blocks[1].DependsOnRaw, "1")
	}
	if !strings.Contains(blocks[1].Body, "Second body") {
		t.Errorf("body should retain the real content, got: %q", blocks[1].Body)
	}
	if strings.Contains(blocks[1].Body, "DEPENDS_ON") {
		t.Errorf("body must not contain the DEPENDS_ON header text, got: %q", blocks[1].Body)
	}
}

// TestParseSpawnBlocks_DependsOnAbsent is the regression guard for requirement
// 4 / acceptance criterion 3: a block with no DEPENDS_ON header must parse
// exactly as before this feature (DependsOnDeclared=false, DependsOn=0).
func TestParseSpawnBlocks_DependsOnAbsent(t *testing.T) {
	body := `
FABRIK_SPAWN_CHILD_BEGIN owner/repo
TITLE: No depends on
Body text.
FABRIK_SPAWN_CHILD_END
`
	blocks := ParseSpawnBlocks(body)
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	b := blocks[0]
	if b.DependsOnDeclared {
		t.Error("DependsOnDeclared should be false when no header is present")
	}
	if b.DependsOn != 0 {
		t.Errorf("DependsOn: got %d, want 0", b.DependsOn)
	}
	if b.DependsOnRaw != "" {
		t.Errorf("DependsOnRaw: got %q, want empty", b.DependsOnRaw)
	}
}

// TestParseSpawnBlocks_DependsOnMalformedValue verifies a non-numeric
// DEPENDS_ON value is retained (not silently dropped, per the Plan's Key
// Decisions) with DependsOnDeclared=true and DependsOn left at the sentinel
// 0 — validateSpawnDependsOn is where this becomes a hard failure.
func TestParseSpawnBlocks_DependsOnMalformedValue(t *testing.T) {
	body := `
FABRIK_SPAWN_CHILD_BEGIN owner/repo
TITLE: Bad depends on
DEPENDS_ON: abc
Body text.
FABRIK_SPAWN_CHILD_END
`
	blocks := ParseSpawnBlocks(body)
	if len(blocks) != 1 {
		t.Fatalf("expected the block to be retained (not silently dropped), got %d blocks", len(blocks))
	}
	b := blocks[0]
	if !b.DependsOnDeclared {
		t.Error("DependsOnDeclared should be true even for a malformed value")
	}
	if b.DependsOnRaw != "abc" {
		t.Errorf("DependsOnRaw: got %q, want %q", b.DependsOnRaw, "abc")
	}
	if b.DependsOn != 0 {
		t.Errorf("DependsOn: got %d, want sentinel 0 for a malformed value", b.DependsOn)
	}
}

// TestParseSpawnBlocks_DependsOnNonCanonicalNumeric verifies that values
// strconv.Atoi alone would accept but which are not canonical positive
// decimal integers — a leading '+' or a leading zero — are rejected the same
// as any other malformed value (sentinel 0), not silently normalized to the
// number they represent. Found in review (#1337): Atoi("+1") and Atoi("01")
// both succeed, which would have let non-canonical forms slip past a header
// grammar documented as strict.
func TestParseSpawnBlocks_DependsOnNonCanonicalNumeric(t *testing.T) {
	for _, raw := range []string{"+1", "01", "-1", "0"} {
		body := "\nFABRIK_SPAWN_CHILD_BEGIN owner/repo\nTITLE: Non-canonical depends on\nDEPENDS_ON: " + raw + "\nBody text.\nFABRIK_SPAWN_CHILD_END\n"
		blocks := ParseSpawnBlocks(body)
		if len(blocks) != 1 {
			t.Fatalf("DEPENDS_ON: %s: expected the block to be retained, got %d blocks", raw, len(blocks))
		}
		b := blocks[0]
		if !b.DependsOnDeclared {
			t.Errorf("DEPENDS_ON: %s: DependsOnDeclared should be true", raw)
		}
		if b.DependsOn != 0 {
			t.Errorf("DEPENDS_ON: %s: DependsOn: got %d, want sentinel 0 (non-canonical numeric form)", raw, b.DependsOn)
		}
	}
}

// TestParseSpawnBlocks_DependsOnEmptyValue covers "DEPENDS_ON:" with no value
// at all — also malformed, same sentinel treatment.
func TestParseSpawnBlocks_DependsOnEmptyValue(t *testing.T) {
	body := `
FABRIK_SPAWN_CHILD_BEGIN owner/repo
TITLE: Empty depends on
DEPENDS_ON:
Body text.
FABRIK_SPAWN_CHILD_END
`
	blocks := ParseSpawnBlocks(body)
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	b := blocks[0]
	if !b.DependsOnDeclared {
		t.Error("DependsOnDeclared should be true for an empty-valued header")
	}
	if b.DependsOn != 0 {
		t.Errorf("DependsOn: got %d, want sentinel 0", b.DependsOn)
	}
}

// TestParseSpawnBlocks_DependsOnBlankLineSeparated_TreatedAsBody pins the
// immediate-adjacency rule: DEPENDS_ON must follow TITLE: with no blank line
// between. Separated by a blank line, it is just body content.
func TestParseSpawnBlocks_DependsOnBlankLineSeparated_TreatedAsBody(t *testing.T) {
	body := `
FABRIK_SPAWN_CHILD_BEGIN owner/repo
TITLE: Not adjacent

DEPENDS_ON: 1
Body text.
FABRIK_SPAWN_CHILD_END
`
	blocks := ParseSpawnBlocks(body)
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	b := blocks[0]
	if b.DependsOnDeclared {
		t.Error("a DEPENDS_ON line separated from TITLE: by a blank line must not be recognized as a header")
	}
	if !strings.Contains(b.Body, "DEPENDS_ON: 1") {
		t.Errorf("blank-line-separated DEPENDS_ON should be treated as body content, got: %q", b.Body)
	}
}

// ---- validateSpawnDependsOn unit tests (#1337) ----

func TestValidateSpawnDependsOn_NoHeadersNoError(t *testing.T) {
	blocks := []SpawnBlock{{Title: "one"}, {Title: "two"}, {Title: "three"}}
	if err := validateSpawnDependsOn(blocks); err != nil {
		t.Fatalf("unexpected error with no DEPENDS_ON headers: %v", err)
	}
}

func TestValidateSpawnDependsOn_ValidChain(t *testing.T) {
	blocks := []SpawnBlock{
		{Title: "one"},
		{Title: "two", DependsOnDeclared: true, DependsOnRaw: "1", DependsOn: 1},
		{Title: "three", DependsOnDeclared: true, DependsOnRaw: "2", DependsOn: 2},
	}
	if err := validateSpawnDependsOn(blocks); err != nil {
		t.Fatalf("unexpected error for a valid chain: %v", err)
	}
}

func TestValidateSpawnDependsOn_OutOfRange(t *testing.T) {
	blocks := []SpawnBlock{
		{Title: "one"},
		{Title: "two"},
		{Title: "three", DependsOnDeclared: true, DependsOnRaw: "4", DependsOn: 4},
	}
	if err := validateSpawnDependsOn(blocks); err == nil {
		t.Fatal("expected error for an out-of-range DEPENDS_ON index")
	}
}

func TestValidateSpawnDependsOn_NonForward(t *testing.T) {
	blocks := []SpawnBlock{
		{Title: "one"},
		{Title: "two", DependsOnDeclared: true, DependsOnRaw: "3", DependsOn: 3},
		{Title: "three"},
	}
	if err := validateSpawnDependsOn(blocks); err == nil {
		t.Fatal("expected error for a non-forward (equal-or-higher) DEPENDS_ON index")
	}
}

func TestValidateSpawnDependsOn_SelfReference(t *testing.T) {
	blocks := []SpawnBlock{
		{Title: "one"},
		{Title: "two", DependsOnDeclared: true, DependsOnRaw: "2", DependsOn: 2},
	}
	if err := validateSpawnDependsOn(blocks); err == nil {
		t.Fatal("expected error for a block depending on itself")
	}
}

func TestValidateSpawnDependsOn_Block1Declares(t *testing.T) {
	blocks := []SpawnBlock{
		{Title: "one", DependsOnDeclared: true, DependsOnRaw: "1", DependsOn: 1},
	}
	err := validateSpawnDependsOn(blocks)
	if err == nil {
		t.Fatal("expected error when block 1 declares DEPENDS_ON")
	}
	if !strings.Contains(err.Error(), "block 1") {
		t.Errorf("error should mention block 1, got: %v", err)
	}
}

func TestValidateSpawnDependsOn_NonNumericRaw(t *testing.T) {
	blocks := []SpawnBlock{
		{Title: "one"},
		{Title: "two", DependsOnDeclared: true, DependsOnRaw: "abc", DependsOn: 0},
	}
	if err := validateSpawnDependsOn(blocks); err == nil {
		t.Fatal("expected error for a non-numeric DEPENDS_ON value")
	}
}

// ---- formatSpawnReceiptNote unit tests (#1338) ----

func TestFormatSpawnReceiptNote_NoBlocks_Empty(t *testing.T) {
	if note := formatSpawnReceiptNote("no spawn blocks here, just regular Plan output"); note != "" {
		t.Fatalf("expected empty note for zero blocks, got %q", note)
	}
}

func TestFormatSpawnReceiptNote_SingleBlock(t *testing.T) {
	body := `FABRIK_SPAWN_CHILD_BEGIN owner/child-repo
TITLE: Add authentication module
Implement OAuth2 authentication.
FABRIK_SPAWN_CHILD_END`
	note := formatSpawnReceiptNote(body)
	if note == "" {
		t.Fatal("expected a non-empty note for 1 well-formed block")
	}
	if !strings.Contains(note, "1 sub-issue") {
		t.Errorf("note should state the count of 1, got %q", note)
	}
	if !strings.Contains(note, "Implement") {
		t.Errorf("note should name the Implement stage transition, got %q", note)
	}
	if strings.Contains(note, "FABRIK_") {
		t.Errorf("note must not contain a literal FABRIK_ token, got %q", note)
	}
}

func TestFormatSpawnReceiptNote_MultipleBlocks(t *testing.T) {
	body := `FABRIK_SPAWN_CHILD_BEGIN owner/repo-a
TITLE: First child
First body.
FABRIK_SPAWN_CHILD_END

FABRIK_SPAWN_CHILD_BEGIN owner/repo-b
TITLE: Second child
Second body.
FABRIK_SPAWN_CHILD_END`
	note := formatSpawnReceiptNote(body)
	if !strings.Contains(note, "2 sub-issues") {
		t.Errorf("note should state the count of 2, got %q", note)
	}
	if !strings.Contains(note, "Implement") {
		t.Errorf("note should name the Implement stage transition, got %q", note)
	}
}

// TestFormatSpawnReceiptNote_MalformedMarkers_NoNote is the #1263 regression
// guard (AC3): prose that merely mentions the marker name must not produce a
// note, even though the raw marker text is still visible in the body. This
// only proves what it claims once formatSpawnReceiptNote actually calls
// ParseSpawnBlocks (never string-matches the marker) — see the Verification
// note in the issue.
func TestFormatSpawnReceiptNote_MalformedMarkers_NoNote(t *testing.T) {
	body := "- [ ] Emit `FABRIK_SPAWN_CHILD_BEGIN owner/repo` block scoping the child work\n" +
		"FABRIK_SPAWN_CHILD_END\n"
	if note := formatSpawnReceiptNote(body); note != "" {
		t.Fatalf("expected no note for malformed/prose-only markers, got %q", note)
	}
	if !strings.Contains(body, "FABRIK_SPAWN_CHILD_BEGIN") {
		t.Fatal("test setup invalid: body should still contain the raw marker text")
	}
}

// TestFormatSpawnReceiptNote_RoundTrip_NoteIsParseInert is AC6: appending the
// rendered note to a comment body and re-running ParseSpawnBlocks over it
// must yield the same block count as before the note was added, since
// preImplement re-parses the posted comment body verbatim at Implement time.
func TestFormatSpawnReceiptNote_RoundTrip_NoteIsParseInert(t *testing.T) {
	body := `FABRIK_SPAWN_CHILD_BEGIN owner/child-repo
TITLE: Add authentication module
Implement OAuth2 authentication.
FABRIK_SPAWN_CHILD_END`
	before := len(ParseSpawnBlocks(body))

	note := formatSpawnReceiptNote(body)
	if note == "" {
		t.Fatal("expected a non-empty note to round-trip")
	}
	withNote := body + note

	after := len(ParseSpawnBlocks(withNote))
	if after != before {
		t.Errorf("round-trip block count changed: before=%d after=%d (note is not parse-inert)", before, after)
	}
}

// ---- formatMidflightSpawnReceiptNote unit tests (#1419) ----

func TestFormatMidflightSpawnReceiptNote_Empty(t *testing.T) {
	if note := formatMidflightSpawnReceiptNote(nil); note != "" {
		t.Fatalf("expected empty note for zero spawned children, got %q", note)
	}
}

func TestFormatMidflightSpawnReceiptNote_SingleChild(t *testing.T) {
	note := formatMidflightSpawnReceiptNote([]string{"owner/child#101"})
	if !strings.Contains(note, "1 sub-issue") {
		t.Errorf("note should state the count of 1, got %q", note)
	}
	if !strings.Contains(note, "owner/child#101") {
		t.Errorf("note should name the spawned child, got %q", note)
	}
	if !strings.Contains(note, "registered, assigned, and linked") {
		t.Errorf("note should describe the wiring already performed (present tense), got %q", note)
	}
}

func TestFormatMidflightSpawnReceiptNote_MultipleChildren(t *testing.T) {
	note := formatMidflightSpawnReceiptNote([]string{"owner/child#101", "owner/child#102"})
	if !strings.Contains(note, "2 sub-issues") {
		t.Errorf("note should state the count of 2, got %q", note)
	}
	if !strings.Contains(note, "owner/child#101") || !strings.Contains(note, "owner/child#102") {
		t.Errorf("note should name both spawned children, got %q", note)
	}
}

// ---- stripSpawnBlocks unit tests (#1419 review finding) ----

func TestStripSpawnBlocks_NoBlock_Unchanged(t *testing.T) {
	body := "Nothing to spawn this cycle.\nFABRIK_STAGE_COMPLETE\n"
	if got := stripSpawnBlocks(body); got != body {
		t.Errorf("expected unchanged output with no block, got %q", got)
	}
}

func TestStripSpawnBlocks_SingleBlock_RemovedButSurroundingTextSurvives(t *testing.T) {
	body := "Found a blocker while reviewing.\n" +
		"FABRIK_SPAWN_CHILD_BEGIN owner/repo\n" +
		"TITLE: Fix the underlying library first\n" +
		"\n" +
		"Discovered mid-review; must land before this PR.\n" +
		"FABRIK_SPAWN_CHILD_END\n" +
		"FABRIK_STAGE_COMPLETE\n"

	got := stripSpawnBlocks(body)

	if strings.Contains(got, "FABRIK_SPAWN_CHILD_BEGIN") || strings.Contains(got, "FABRIK_SPAWN_CHILD_END") {
		t.Errorf("expected BEGIN/END markers stripped, got %q", got)
	}
	if strings.Contains(got, "TITLE: Fix the underlying library first") {
		t.Errorf("expected TITLE: line stripped, got %q", got)
	}
	if strings.Contains(got, "Discovered mid-review") {
		t.Errorf("expected block body stripped, got %q", got)
	}
	if !strings.Contains(got, "Found a blocker while reviewing.") {
		t.Errorf("expected text preceding the block to survive, got %q", got)
	}
	if !strings.Contains(got, "FABRIK_STAGE_COMPLETE") {
		t.Errorf("expected text following the block to survive, got %q", got)
	}
}

func TestStripSpawnBlocks_MultipleBlocks_AllRemoved(t *testing.T) {
	body := "Two blockers found.\n" +
		"FABRIK_SPAWN_CHILD_BEGIN owner/repo-a\n" +
		"TITLE: First blocker\n" +
		"Body one.\n" +
		"FABRIK_SPAWN_CHILD_END\n" +
		"FABRIK_SPAWN_CHILD_BEGIN owner/repo-b\n" +
		"TITLE: Second blocker\n" +
		"Body two.\n" +
		"FABRIK_SPAWN_CHILD_END\n" +
		"FABRIK_STAGE_COMPLETE\n"

	got := stripSpawnBlocks(body)

	if strings.Contains(got, "FABRIK_SPAWN_CHILD_BEGIN") || strings.Contains(got, "FABRIK_SPAWN_CHILD_END") {
		t.Errorf("expected all BEGIN/END markers stripped, got %q", got)
	}
	if strings.Contains(got, "First blocker") || strings.Contains(got, "Body one") {
		t.Errorf("expected first block content stripped, got %q", got)
	}
	if strings.Contains(got, "Second blocker") || strings.Contains(got, "Body two") {
		t.Errorf("expected second block content stripped, got %q", got)
	}
	if !strings.Contains(got, "Two blockers found.") {
		t.Errorf("expected leading text to survive, got %q", got)
	}
}

func TestStripSpawnBlocks_MalformedBlock_LeftVisible(t *testing.T) {
	// No TITLE: line — ParseSpawnBlocks would skip this as malformed, so
	// nothing was ever spawned for it; stripSpawnBlocks must leave it alone
	// rather than silently discarding content that was never processed.
	body := "FABRIK_SPAWN_CHILD_BEGIN owner/repo\n" +
		"Just prose, no TITLE: line.\n" +
		"FABRIK_SPAWN_CHILD_END\n"

	got := stripSpawnBlocks(body)
	if got != body {
		t.Errorf("expected malformed block left untouched, got %q, want %q", got, body)
	}
}

func TestStripSpawnBlocks_ProseOnlyMention_Unchanged(t *testing.T) {
	// #1263 own-line discipline: a marker merely mentioned in prose (not on
	// its own line) must not be treated as a real block boundary.
	body := "- [ ] Emit `FABRIK_SPAWN_CHILD_BEGIN owner/repo` block if a blocker turns up\n"
	if got := stripSpawnBlocks(body); got != body {
		t.Errorf("expected prose-only mention left untouched, got %q", got)
	}
}

// ---- resolveSpecifyOptionID unit tests ----

func TestResolveSpecifyOptionID_Nil(t *testing.T) {
	if got := resolveSpecifyOptionID(nil, nil); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestResolveSpecifyOptionID_ExactMatch(t *testing.T) {
	sf := &gh.StatusField{
		Options:            map[string]string{"Backlog": "OPT_0", "Specify": "OPT_1", "Done": "OPT_3"},
		OrderedOptionNames: []string{"Backlog", "Specify", "Done"},
	}
	if got := resolveSpecifyOptionID(sf, nil); got != "OPT_1" {
		t.Errorf("got %q, want OPT_1", got)
	}
}

func TestResolveSpecifyOptionID_Fallback(t *testing.T) {
	// No "Specify" column — fallback to first non-Backlog, non-terminal.
	sf := &gh.StatusField{
		Options:            map[string]string{"Backlog": "OPT_0", "Research": "OPT_1", "Done": "OPT_3"},
		OrderedOptionNames: []string{"Backlog", "Research", "Done"},
	}
	if got := resolveSpecifyOptionID(sf, nil); got != "OPT_1" {
		t.Errorf("got %q, want OPT_1 (Research)", got)
	}
}

func TestResolveSpecifyOptionID_BacklogAndDoneOnly(t *testing.T) {
	// Only two columns — fallback skips Backlog and the last; nothing qualifies.
	sf := &gh.StatusField{
		Options:            map[string]string{"Backlog": "OPT_0", "Done": "OPT_1"},
		OrderedOptionNames: []string{"Backlog", "Done"},
	}
	if got := resolveSpecifyOptionID(sf, nil); got != "" {
		t.Errorf("got %q, want empty (no viable column)", got)
	}
}

func TestResolveSpecifyOptionID_EmptyOrderedNames(t *testing.T) {
	sf := &gh.StatusField{
		Options:            map[string]string{"Research": "OPT_1"},
		OrderedOptionNames: nil,
	}
	if got := resolveSpecifyOptionID(sf, nil); got != "" {
		t.Errorf("got %q, want empty when OrderedOptionNames is nil", got)
	}
}

func TestResolveSpecifyOptionID_SingleColumn(t *testing.T) {
	// Exact match on "Specify" fires before the fallback len(names) < 2 guard,
	// so a single-column board named "Specify" still returns its option ID.
	sf := &gh.StatusField{
		Options:            map[string]string{"Specify": "OPT_1"},
		OrderedOptionNames: []string{"Specify"},
	}
	if got := resolveSpecifyOptionID(sf, nil); got != "OPT_1" {
		t.Errorf("got %q, want OPT_1 (exact match wins)", got)
	}
}

// TestResolveSpecifyOptionID_SkipsDeclaredUnmanagedColumn is the regression
// guard for the PR review finding on issue #973: the fallback previously only
// ever skipped the literal name "Backlog", so a parking column declared
// `unmanaged: true` under any OTHER name (e.g. "Icebox") fell through and was
// returned as the "Specify" fallback target — landing spawned children in a
// column itemMayNeedWork/itemNeedsWork deliberately never dispatch, with no
// fabrik:awaiting-placement marker recorded to ever recover them.
func TestResolveSpecifyOptionID_SkipsDeclaredUnmanagedColumn(t *testing.T) {
	sf := &gh.StatusField{
		Options:            map[string]string{"Icebox": "OPT_0", "Research": "OPT_1", "Done": "OPT_3"},
		OrderedOptionNames: []string{"Icebox", "Research", "Done"},
	}
	stagesCfg := []*stages.Stage{
		{Name: "Icebox", Unmanaged: true},
		{Name: "Research"},
		{Name: "Done", CleanupWorktree: true},
	}
	if got := resolveSpecifyOptionID(sf, stagesCfg); got != "OPT_1" {
		t.Errorf("got %q, want OPT_1 (Research) — declared unmanaged column Icebox must be skipped", got)
	}
}

// TestResolveSpecifyOptionID_UnmanagedAndDoneOnly verifies the fallback still
// correctly yields "" when the only non-terminal option is a declared
// unmanaged column under a non-Backlog name — mirroring
// TestResolveSpecifyOptionID_BacklogAndDoneOnly for the generalized case.
func TestResolveSpecifyOptionID_UnmanagedAndDoneOnly(t *testing.T) {
	sf := &gh.StatusField{
		Options:            map[string]string{"Icebox": "OPT_0", "Done": "OPT_1"},
		OrderedOptionNames: []string{"Icebox", "Done"},
	}
	stagesCfg := []*stages.Stage{
		{Name: "Icebox", Unmanaged: true},
		{Name: "Done", CleanupWorktree: true},
	}
	if got := resolveSpecifyOptionID(sf, stagesCfg); got != "" {
		t.Errorf("got %q, want empty (no viable column)", got)
	}
}

// ---- preImplement integration tests ----

func spawnTestEngine(t *testing.T, client *mockGitHubClient) *Engine {
	t.Helper()
	eng := testEngine(t, client, &mockClaudeInvoker{})
	// These tests exercise cross-repo spawning ("owner/repo" parent spawning
	// into "owner/child"), which only a multi-repo instance (cfg.Repo == "")
	// is configured to serve — testEngine's default cfg.Repo ("repo") would
	// now make spawnTargetServedByThisInstance refuse every one of them.
	// This is exactly the issue's own "Instance A processes multiple repos on
	// board 5 without issue" shape; the repo-scoped-mismatch shape (Instance
	// B / reproduction 1) is covered by its own dedicated tests below.
	eng.cfg.Repo = ""
	// Register "owner/repo" and "owner/child" as managed repos.
	eng.worktreeManagers["owner/repo"] = NewWorktreeManager(t.TempDir())
	eng.worktreeManagers["owner/child"] = NewWorktreeManager(t.TempDir())
	return eng
}

func planItemWithBlocks(blocksRaw string) gh.ProjectItem {
	return gh.ProjectItem{
		ID:     "I_parent",
		ItemID: "PVTI_parent",
		Number: 42,
		Repo:   "owner/repo",
		Title:  "Parent issue",
		Labels: []string{"stage:Plan:complete"},
		Comments: []gh.Comment{
			{
				DatabaseID: 1001,
				Author:     "testuser",
				Body:       planCommentWithBlocks(blocksRaw),
			},
		},
	}
}

func TestPreImplement_NoOp_ChildrenAlreadySpawned(t *testing.T) {
	client := &mockGitHubClient{}
	eng := spawnTestEngine(t, client)

	item := planItemWithBlocks(`
FABRIK_SPAWN_CHILD_BEGIN owner/child
TITLE: Child issue
Body.
FABRIK_SPAWN_CHILD_END
`)
	item.Labels = append(item.Labels, "fabrik:children-spawned")
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	spawned, err := eng.preImplement(context.Background(), board, item)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if spawned {
		t.Error("expected spawned=false when children-spawned label present")
	}
	if len(client.createIssueCalls) != 0 {
		t.Error("CreateIssue should not be called when children already spawned")
	}
}

// TestPreImplement_NoOp_NoPlanComment covers the #982 inconsistency: stage:Plan:complete
// is present but item.Comments has no Plan comment. This must NOT silently no-op —
// preImplement must attempt recovery via a live re-read before concluding there is
// nothing to spawn. This test's live re-read also finds no Plan comment, so the
// final outcome is still spawned=false, err=nil — but only after recovery genuinely ran.
func TestPreImplement_NoOp_NoPlanComment(t *testing.T) {
	var fetchCalls int
	client := &mockGitHubClient{
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			fetchCalls++
			return nil // fresh read still has no comments
		},
	}
	eng := spawnTestEngine(t, client)

	item := gh.ProjectItem{
		ID:     "I_parent",
		Number: 42,
		Repo:   "owner/repo",
		Labels: []string{"stage:Plan:complete"},
		// No comments at all.
	}
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	spawned, err := eng.preImplement(context.Background(), board, item)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if spawned {
		t.Error("expected spawned=false with no Plan comment")
	}
	if fetchCalls != 1 {
		t.Errorf("expected live re-read to be attempted exactly once, got %d calls", fetchCalls)
	}
}

// TestPreImplement_RecoversViaLiveRead_SpawnsChildren covers the #982 recovery path
// where the live re-read finds the Plan comment (with spawn blocks) that was missing
// from the stale item.Comments snapshot. Children must be spawned exactly once.
func TestPreImplement_RecoversViaLiveRead_SpawnsChildren(t *testing.T) {
	childCounter := 0
	client := &mockGitHubClient{
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			item.Comments = []gh.Comment{
				{
					DatabaseID: 1001,
					Author:     "testuser",
					Body: planCommentWithBlocks(`
FABRIK_SPAWN_CHILD_BEGIN owner/child
TITLE: Recovered child
Recovered body.
FABRIK_SPAWN_CHILD_END
`),
				},
			}
			return nil
		},
		createIssueFn: func(owner, repo, title, body string, assignees []string) (int, string, error) {
			childCounter++
			return 300 + childCounter, fmt.Sprintf("I_recovered%d", childCounter), nil
		},
		addProjectV2ItemByIdFn: func(projectID, contentNodeID string) (string, error) {
			return "PVTI_" + contentNodeID, nil
		},
	}
	eng := spawnTestEngine(t, client)

	item := gh.ProjectItem{
		ID:     "I_parent",
		ItemID: "PVTI_parent",
		Number: 42,
		Repo:   "owner/repo",
		Labels: []string{"stage:Plan:complete"},
		// item.Comments is empty — simulates the stale-snapshot miss.
	}
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	spawned, err := eng.preImplement(context.Background(), board, item)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !spawned {
		t.Fatal("expected spawned=true after recovery finds spawn blocks")
	}
	if len(client.createIssueCalls) != 1 {
		t.Fatalf("expected 1 CreateIssue call, got %d", len(client.createIssueCalls))
	}

	var spawnedLabelCount int
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:children-spawned" {
			spawnedLabelCount++
		}
	}
	if spawnedLabelCount != 1 {
		t.Errorf("expected fabrik:children-spawned added exactly once, got %d", spawnedLabelCount)
	}
}

// TestPreImplement_RecoversViaLiveRead_ConfirmsNothingToSpawn covers the #982 recovery
// path where the live re-read succeeds but confirms there really is nothing to spawn
// (Plan comment recovered, but with no spawn blocks).
func TestPreImplement_RecoversViaLiveRead_ConfirmsNothingToSpawn(t *testing.T) {
	client := &mockGitHubClient{
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			item.Comments = []gh.Comment{
				{DatabaseID: 1001, Author: "testuser", Body: planCommentWithBlocks("No spawn blocks in here.")},
			}
			return nil
		},
	}
	eng := spawnTestEngine(t, client)

	item := gh.ProjectItem{
		ID:     "I_parent",
		Number: 42,
		Repo:   "owner/repo",
		Labels: []string{"stage:Plan:complete"},
	}
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	spawned, err := eng.preImplement(context.Background(), board, item)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if spawned {
		t.Error("expected spawned=false when recovered Plan comment has no spawn blocks")
	}
	if len(client.createIssueCalls) != 0 {
		t.Error("CreateIssue should not be called when there is nothing to spawn")
	}
}

// TestPreImplement_RecoveryFails_DefersWithoutPausing covers the #982 outcome where the
// live re-read itself fails — preImplement must return errPreImplementDeferred (so the
// parent is retried on a subsequent poll) and must NOT pause the issue.
func TestPreImplement_RecoveryFails_DefersWithoutPausing(t *testing.T) {
	client := &mockGitHubClient{
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			return errors.New("simulated GraphQL failure")
		},
	}
	eng := spawnTestEngine(t, client)

	item := gh.ProjectItem{
		ID:     "I_parent",
		Number: 42,
		Repo:   "owner/repo",
		Labels: []string{"stage:Plan:complete"},
	}
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	spawned, err := eng.preImplement(context.Background(), board, item)
	if !errors.Is(err, errPreImplementDeferred) {
		t.Fatalf("expected errPreImplementDeferred, got %v", err)
	}
	if spawned {
		t.Error("expected spawned=false on deferred recovery")
	}
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			t.Error("fabrik:paused must not be added on the deferred outcome")
		}
	}
}

// TestPreImplement_RecoveryDeferred_CooldownSkipsLiveRead verifies that an active
// spawn-recovery cooldown short-circuits the live-read call entirely, bounding
// repeated GraphQL load during a sustained failure window (#971-style pressure).
func TestPreImplement_RecoveryDeferred_CooldownSkipsLiveRead(t *testing.T) {
	var fetchCalls int
	client := &mockGitHubClient{
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			fetchCalls++
			return nil
		},
	}
	eng := spawnTestEngine(t, client)
	eng.store.Apply(itemstate.CooldownRecorded{
		Repo:   "owner/repo",
		Number: 42,
		Reason: "spawn-recovery-deferred",
		Until:  time.Now().Add(10 * time.Minute),
	})

	item := gh.ProjectItem{
		ID:     "I_parent",
		Number: 42,
		Repo:   "owner/repo",
		Labels: []string{"stage:Plan:complete"},
	}
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	spawned, err := eng.preImplement(context.Background(), board, item)
	if !errors.Is(err, errPreImplementDeferred) {
		t.Fatalf("expected errPreImplementDeferred, got %v", err)
	}
	if spawned {
		t.Error("expected spawned=false while cooldown is active")
	}
	if fetchCalls != 0 {
		t.Errorf("expected live re-read to be skipped while cooldown is active, got %d calls", fetchCalls)
	}
}

func TestPreImplement_NoOp_NoBlocks(t *testing.T) {
	client := &mockGitHubClient{}
	eng := spawnTestEngine(t, client)

	item := planItemWithBlocks("No spawn blocks in here.")
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	spawned, err := eng.preImplement(context.Background(), board, item)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if spawned {
		t.Error("expected spawned=false when no spawn blocks in Plan comment")
	}
}

func TestPreImplement_HappyPath(t *testing.T) {
	childCounter := 0
	client := &mockGitHubClient{
		createIssueFn: func(owner, repo, title, body string, assignees []string) (int, string, error) {
			childCounter++
			return 100 + childCounter, fmt.Sprintf("I_child%d", childCounter), nil
		},
		addProjectV2ItemByIdFn: func(projectID, contentNodeID string) (string, error) {
			return "PVTI_" + contentNodeID, nil
		},
	}
	eng := spawnTestEngine(t, client)

	item := planItemWithBlocks(`
FABRIK_SPAWN_CHILD_BEGIN owner/child
TITLE: Child one
Child one body.
FABRIK_SPAWN_CHILD_END

FABRIK_SPAWN_CHILD_BEGIN owner/child
TITLE: Child two
Child two body.
FABRIK_SPAWN_CHILD_END
`)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	spawned, err := eng.preImplement(context.Background(), board, item)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !spawned {
		t.Fatal("expected spawned=true")
	}

	// Two issues created.
	if len(client.createIssueCalls) != 2 {
		t.Errorf("expected 2 CreateIssue calls, got %d", len(client.createIssueCalls))
	}
	if client.createIssueCalls[0].title != "Child one" {
		t.Errorf("first child title: got %q", client.createIssueCalls[0].title)
	}
	if client.createIssueCalls[1].title != "Child two" {
		t.Errorf("second child title: got %q", client.createIssueCalls[1].title)
	}

	// Footer injected into each child body.
	for i, c := range client.createIssueCalls {
		if !strings.Contains(c.body, "Spawned by Fabrik") {
			t.Errorf("child %d body missing footer: %q", i+1, c.body)
		}
	}

	// Added to project board twice.
	if len(client.addProjectV2ItemCalls) != 2 {
		t.Errorf("expected 2 AddProjectV2ItemById calls, got %d", len(client.addProjectV2ItemCalls))
	}

	// Linked as blockedBy twice.
	if len(client.addBlockedByIssueCalls) != 2 {
		t.Errorf("expected 2 AddBlockedByIssue calls, got %d", len(client.addBlockedByIssueCalls))
	}
	for _, c := range client.addBlockedByIssueCalls {
		if c.issueNodeID != "I_parent" {
			t.Errorf("AddBlockedByIssue issueNodeID: got %q, want %q", c.issueNodeID, "I_parent")
		}
	}

	// children-spawned label added.
	var spawnedLabelAdded bool
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:children-spawned" {
			spawnedLabelAdded = true
		}
	}
	if !spawnedLabelAdded {
		t.Error("fabrik:children-spawned label not added")
	}

	// sub-issue label added to each child.
	subIssueCount := 0
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:sub-issue" {
			subIssueCount++
		}
	}
	if subIssueCount != 2 {
		t.Errorf("expected 2 fabrik:sub-issue labels, got %d", subIssueCount)
	}
}

// TestPreImplement_CloneFailure replaces the old TestPreImplement_UnmanagedRepo.
// With on-demand initialization, an unregistered target repo triggers a clone
// attempt. This test verifies the failure path when the clone cannot succeed.
func TestPreImplement_CloneFailure(t *testing.T) {
	skipIfNoGit(t)
	client := &mockGitHubClient{}
	eng := spawnTestEngine(t, client)
	// Point fabrikDir at a tempdir — ensureBareClone will fail to clone the nonexistent repo.
	eng.fabrikDir = t.TempDir()

	// "owner/newrepo" is NOT in worktreeManagers, and the clone will fail.
	item := planItemWithBlocks(`
FABRIK_SPAWN_CHILD_BEGIN owner/newrepo
TITLE: Child in uncloneable repo
Body.
FABRIK_SPAWN_CHILD_END
`)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	spawned, err := eng.preImplement(context.Background(), board, item)
	if err == nil {
		t.Fatal("expected error when clone fails")
	}
	if spawned {
		t.Error("expected spawned=false on clone failure")
	}
	if len(client.createIssueCalls) != 0 {
		t.Error("CreateIssue should not be called when clone fails")
	}

	// fabrik:paused and fabrik:awaiting-input must both be added.
	var pausedAdded, awaitingInputAdded bool
	for _, c := range client.addLabelCalls {
		switch c.labelName {
		case "fabrik:paused":
			pausedAdded = true
		case "fabrik:awaiting-input":
			awaitingInputAdded = true
		}
	}
	if !pausedAdded {
		t.Error("fabrik:paused label not added on clone failure")
	}
	if !awaitingInputAdded {
		t.Error("fabrik:awaiting-input label not added on clone failure")
	}

	// Error comment must be posted and must not mention the old "not in worktreeManagers" message.
	if len(client.addCommentCalls) == 0 {
		t.Error("expected error comment on clone failure")
	}
	for _, c := range client.addCommentCalls {
		if strings.Contains(c.body, "not in worktreeManagers") {
			t.Errorf("error comment must not mention 'not in worktreeManagers': %q", c.body)
		}
	}
}

// TestPreImplement_OnDemandClone_Success verifies that a spawn into a repo not
// yet in worktreeManagers succeeds when the bare clone directory already exists
// on disk (so ensureBareClone returns nil without hitting the network).
func TestPreImplement_OnDemandClone_Success(t *testing.T) {
	skipIfNoGit(t)

	// Create a tempdir to serve as fabrikDir.
	fabrikDir := t.TempDir()

	// Pre-create the bare clone directory that ensureBareClone will find.
	// When the directory exists, ensureBareClone skips the `git clone` step and
	// returns nil (best-effort fetch errors are silently ignored).
	targetOwner, targetRepoName := "testowner", "testrepo"
	bareDir := filepath.Join(fabrikDir, ".fabrik", "repos", targetOwner+"-"+targetRepoName+".git")
	if err := os.MkdirAll(bareDir, 0755); err != nil {
		t.Fatalf("creating bare dir: %v", err)
	}
	initCmd := exec.Command("git", "init", "--bare", bareDir)
	if out, err := initCmd.CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %s: %v", out, err)
	}

	childCounter := 0
	client := &mockGitHubClient{
		createIssueFn: func(owner, repo, title, body string, assignees []string) (int, string, error) {
			childCounter++
			return 200 + childCounter, fmt.Sprintf("I_newchild%d", childCounter), nil
		},
		addProjectV2ItemByIdFn: func(projectID, contentNodeID string) (string, error) {
			return "PVTI_" + contentNodeID, nil
		},
	}
	eng := spawnTestEngine(t, client)
	eng.fabrikDir = fabrikDir

	// "testowner/testrepo" is NOT in worktreeManagers initially.
	item := planItemWithBlocks(fmt.Sprintf(`
FABRIK_SPAWN_CHILD_BEGIN %s/%s
TITLE: Child in on-demand-cloned repo
Body of the child issue.
FABRIK_SPAWN_CHILD_END
`, targetOwner, targetRepoName))
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	spawned, err := eng.preImplement(context.Background(), board, item)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !spawned {
		t.Fatal("expected spawned=true after on-demand clone")
	}

	// WorktreeManager must now be registered for the target repo.
	eng.mu.Lock()
	_, registered := eng.worktreeManagers[targetOwner+"/"+targetRepoName]
	eng.mu.Unlock()
	if !registered {
		t.Error("worktreeManagers should contain the on-demand-cloned target repo")
	}

	// CreateIssue must have been called for the child.
	if len(client.createIssueCalls) != 1 {
		t.Errorf("expected 1 CreateIssue call, got %d", len(client.createIssueCalls))
	}
}

// ---- DEPENDS_ON integration tests (#1337) ----

// TestPreImplement_DependsOnChain_WiresSiblingEdges is acceptance criteria 1-2:
// a 3-block chain where block 2 depends on block 1 and block 3 depends on
// block 2 produces the parent-blocked-by-all edges (unchanged) plus two
// sibling edges wiring the declared chain, and the children-spawned label is
// applied only after both passes succeed.
func TestPreImplement_DependsOnChain_WiresSiblingEdges(t *testing.T) {
	childCounter := 0
	client := &mockGitHubClient{
		createIssueFn: func(owner, repo, title, body string, assignees []string) (int, string, error) {
			childCounter++
			return 400 + childCounter, fmt.Sprintf("I_chain%d", childCounter), nil
		},
		addProjectV2ItemByIdFn: func(projectID, contentNodeID string) (string, error) {
			return "PVTI_" + contentNodeID, nil
		},
	}
	eng := spawnTestEngine(t, client)

	item := planItemWithBlocks(`
FABRIK_SPAWN_CHILD_BEGIN owner/child
TITLE: Slice one
Slice one body.
FABRIK_SPAWN_CHILD_END

FABRIK_SPAWN_CHILD_BEGIN owner/child
TITLE: Slice two
DEPENDS_ON: 1
Slice two body.
FABRIK_SPAWN_CHILD_END

FABRIK_SPAWN_CHILD_BEGIN owner/child
TITLE: Slice three
DEPENDS_ON: 2
Slice three body.
FABRIK_SPAWN_CHILD_END
`)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	spawned, err := eng.preImplement(context.Background(), board, item)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !spawned {
		t.Fatal("expected spawned=true")
	}

	if len(client.createIssueCalls) != 3 {
		t.Fatalf("expected 3 CreateIssue calls, got %d", len(client.createIssueCalls))
	}

	// 3 parent edges + 2 sibling edges = 5 total AddBlockedByIssue calls.
	if len(client.addBlockedByIssueCalls) != 5 {
		t.Fatalf("expected 5 AddBlockedByIssue calls (3 parent + 2 sibling), got %d", len(client.addBlockedByIssueCalls))
	}

	var parentEdges, siblingEdges int
	siblingPairs := make(map[string]bool)
	for _, c := range client.addBlockedByIssueCalls {
		if c.issueNodeID == "I_parent" {
			parentEdges++
			continue
		}
		siblingEdges++
		siblingPairs[c.issueNodeID+"<-"+c.blockerNodeID] = true
	}
	if parentEdges != 3 {
		t.Errorf("expected 3 parent edges, got %d", parentEdges)
	}
	if siblingEdges != 2 {
		t.Errorf("expected 2 sibling edges, got %d", siblingEdges)
	}
	if !siblingPairs["I_chain2<-I_chain1"] {
		t.Error("expected sibling edge: child 2 blocked-by child 1")
	}
	if !siblingPairs["I_chain3<-I_chain2"] {
		t.Error("expected sibling edge: child 3 blocked-by child 2")
	}

	var spawnedLabelCount int
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:children-spawned" {
			spawnedLabelCount++
		}
	}
	if spawnedLabelCount != 1 {
		t.Errorf("expected fabrik:children-spawned added exactly once, got %d", spawnedLabelCount)
	}
}

// TestPreImplement_NoDependsOn_MatchesTodaysCallsExactly is the regression
// guard for requirement 4 / acceptance criterion 3: a Plan with no DEPENDS_ON
// headers must produce exactly today's parent-only edges, byte-identical to
// before this feature.
func TestPreImplement_NoDependsOn_MatchesTodaysCallsExactly(t *testing.T) {
	childCounter := 0
	client := &mockGitHubClient{
		createIssueFn: func(owner, repo, title, body string, assignees []string) (int, string, error) {
			childCounter++
			return 500 + childCounter, fmt.Sprintf("I_star%d", childCounter), nil
		},
		addProjectV2ItemByIdFn: func(projectID, contentNodeID string) (string, error) {
			return "PVTI_" + contentNodeID, nil
		},
	}
	eng := spawnTestEngine(t, client)

	item := planItemWithBlocks(`
FABRIK_SPAWN_CHILD_BEGIN owner/child
TITLE: Star child one
Body one.
FABRIK_SPAWN_CHILD_END

FABRIK_SPAWN_CHILD_BEGIN owner/child
TITLE: Star child two
Body two.
FABRIK_SPAWN_CHILD_END
`)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	spawned, err := eng.preImplement(context.Background(), board, item)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !spawned {
		t.Fatal("expected spawned=true")
	}

	if len(client.addBlockedByIssueCalls) != 2 {
		t.Fatalf("expected exactly 2 AddBlockedByIssue calls (parent edges only, no sibling wiring), got %d", len(client.addBlockedByIssueCalls))
	}
	for _, c := range client.addBlockedByIssueCalls {
		if c.issueNodeID != "I_parent" {
			t.Errorf("AddBlockedByIssue issueNodeID: got %q, want %q (parent edge only)", c.issueNodeID, "I_parent")
		}
	}
}

// TestPreImplement_InvalidDependsOnIndex_PausesBeforeAnyCreate is acceptance
// criterion 4: an out-of-range DEPENDS_ON must fail the spawn step loudly,
// before any GitHub mutation — zero CreateIssue calls, parent paused, and the
// pause comment must read "Created so far: none".
func TestPreImplement_InvalidDependsOnIndex_PausesBeforeAnyCreate(t *testing.T) {
	client := &mockGitHubClient{}
	eng := spawnTestEngine(t, client)

	item := planItemWithBlocks(`
FABRIK_SPAWN_CHILD_BEGIN owner/child
TITLE: Slice one
Slice one body.
FABRIK_SPAWN_CHILD_END

FABRIK_SPAWN_CHILD_BEGIN owner/child
TITLE: Slice two
DEPENDS_ON: 4
Slice two body.
FABRIK_SPAWN_CHILD_END
`)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	spawned, err := eng.preImplement(context.Background(), board, item)
	if err == nil {
		t.Fatal("expected error for an out-of-range DEPENDS_ON index")
	}
	if spawned {
		t.Error("expected spawned=false")
	}
	if len(client.createIssueCalls) != 0 {
		t.Errorf("expected 0 CreateIssue calls (validation runs before any mutation), got %d", len(client.createIssueCalls))
	}

	var pausedAdded bool
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			pausedAdded = true
		}
	}
	if !pausedAdded {
		t.Error("fabrik:paused not added on invalid DEPENDS_ON")
	}

	var sawCreatedSoFarNone bool
	for _, c := range client.addCommentCalls {
		if strings.Contains(c.body, "Created so far: none") {
			sawCreatedSoFarNone = true
		}
	}
	if !sawCreatedSoFarNone {
		t.Error(`expected pause comment to read "Created so far: none"`)
	}
}

// TestPreImplement_InvalidDependsOnIndex_NonForward covers block 2 declaring
// DEPENDS_ON: 3 in a 3-block output (equal-or-higher index) per acceptance
// criterion 4's second example.
func TestPreImplement_InvalidDependsOnIndex_NonForward(t *testing.T) {
	client := &mockGitHubClient{}
	eng := spawnTestEngine(t, client)

	item := planItemWithBlocks(`
FABRIK_SPAWN_CHILD_BEGIN owner/child
TITLE: Slice one
Slice one body.
FABRIK_SPAWN_CHILD_END

FABRIK_SPAWN_CHILD_BEGIN owner/child
TITLE: Slice two
DEPENDS_ON: 3
Slice two body.
FABRIK_SPAWN_CHILD_END

FABRIK_SPAWN_CHILD_BEGIN owner/child
TITLE: Slice three
Slice three body.
FABRIK_SPAWN_CHILD_END
`)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	spawned, err := eng.preImplement(context.Background(), board, item)
	if err == nil {
		t.Fatal("expected error for a non-forward DEPENDS_ON index")
	}
	if spawned {
		t.Error("expected spawned=false")
	}
	if len(client.createIssueCalls) != 0 {
		t.Errorf("expected 0 CreateIssue calls, got %d", len(client.createIssueCalls))
	}
}

// TestPreImplement_SiblingWireFailure_PausesAfterChildrenCreated verifies the
// accepted partial-creation-on-failure shape when the second (sibling-wiring)
// pass fails: children already exist (unlike the upfront-validation failure
// above), the parent is paused, and fabrik:children-spawned must never be
// applied since the two-phase operation did not complete.
func TestPreImplement_SiblingWireFailure_PausesAfterChildrenCreated(t *testing.T) {
	childCounter := 0
	blockedByCalls := 0
	client := &mockGitHubClient{
		createIssueFn: func(owner, repo, title, body string, assignees []string) (int, string, error) {
			childCounter++
			return 600 + childCounter, fmt.Sprintf("I_wirefail%d", childCounter), nil
		},
		addProjectV2ItemByIdFn: func(projectID, contentNodeID string) (string, error) {
			return "PVTI_" + contentNodeID, nil
		},
		addBlockedByIssueFn: func(issueNodeID, blockerNodeID string) error {
			blockedByCalls++
			// Let the two parent edges succeed; fail the sibling edge (3rd call).
			if blockedByCalls == 3 {
				return fmt.Errorf("github: 500 internal server error")
			}
			return nil
		},
	}
	eng := spawnTestEngine(t, client)

	item := planItemWithBlocks(`
FABRIK_SPAWN_CHILD_BEGIN owner/child
TITLE: Slice one
Slice one body.
FABRIK_SPAWN_CHILD_END

FABRIK_SPAWN_CHILD_BEGIN owner/child
TITLE: Slice two
DEPENDS_ON: 1
Slice two body.
FABRIK_SPAWN_CHILD_END
`)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	spawned, err := eng.preImplement(context.Background(), board, item)
	if err == nil {
		t.Fatal("expected error when the sibling-wiring pass fails")
	}
	if spawned {
		t.Error("expected spawned=false")
	}

	// Both children were already created before the wiring pass ran.
	if len(client.createIssueCalls) != 2 {
		t.Errorf("expected 2 CreateIssue calls (children created before wiring), got %d", len(client.createIssueCalls))
	}

	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:children-spawned" {
			t.Error("fabrik:children-spawned must not be added when sibling wiring fails")
		}
	}

	var pausedAdded bool
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			pausedAdded = true
		}
	}
	if !pausedAdded {
		t.Error("fabrik:paused not added when sibling wiring fails")
	}
}

func TestPreImplement_PartialFailure(t *testing.T) {
	callCount := 0
	client := &mockGitHubClient{
		createIssueFn: func(owner, repo, title, body string, assignees []string) (int, string, error) {
			callCount++
			if callCount == 2 {
				return 0, "", fmt.Errorf("github: 500 internal server error")
			}
			return 100 + callCount, fmt.Sprintf("I_child%d", callCount), nil
		},
	}
	eng := spawnTestEngine(t, client)

	item := planItemWithBlocks(`
FABRIK_SPAWN_CHILD_BEGIN owner/child
TITLE: Child one
Body one.
FABRIK_SPAWN_CHILD_END

FABRIK_SPAWN_CHILD_BEGIN owner/child
TITLE: Child two
Body two.
FABRIK_SPAWN_CHILD_END
`)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	spawned, err := eng.preImplement(context.Background(), board, item)
	if err == nil {
		t.Fatal("expected error on partial failure")
	}
	if spawned {
		t.Error("expected spawned=false on error")
	}

	// Only one CreateIssue call succeeded before failure.
	if len(client.createIssueCalls) != 2 {
		t.Errorf("expected 2 CreateIssue attempts, got %d", len(client.createIssueCalls))
	}

	// children-spawned must NOT be added (partial state).
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:children-spawned" {
			t.Error("fabrik:children-spawned must not be added on partial failure")
		}
	}

	// Error comment and paused label should be added.
	if len(client.addCommentCalls) == 0 {
		t.Error("expected error comment on partial failure")
	}
	var pausedAdded bool
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			pausedAdded = true
		}
	}
	if !pausedAdded {
		t.Error("fabrik:paused not added on partial failure")
	}
}

// ---- board-servability scope-check tests (#1419, failure mode 1) ----

// TestPreImplement_RepoScopedInstance_RefusesMismatchedTarget covers Instance
// B's shape from the issue's reproduction 1: a repo:-scoped instance (cfg.Repo
// != "") asked to spawn into a repo other than its own must refuse loudly —
// zero CreateIssue calls, the parent paused, no silent registration onto this
// instance's own (wrong) board.
func TestPreImplement_RepoScopedInstance_RefusesMismatchedTarget(t *testing.T) {
	client := &mockGitHubClient{}
	// testEngine's default Config is repo:-scoped to "owner/repo" — do NOT
	// widen it to multi-repo mode here, unlike spawnTestEngine.
	eng := testEngine(t, client, &mockClaudeInvoker{})
	eng.worktreeManagers["owner/repo"] = NewWorktreeManager(t.TempDir())
	eng.worktreeManagers["owner/child"] = NewWorktreeManager(t.TempDir())

	item := planItemWithBlocks(`
FABRIK_SPAWN_CHILD_BEGIN owner/child
TITLE: Cross-repo child this instance cannot serve
Body.
FABRIK_SPAWN_CHILD_END
`)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	spawned, err := eng.preImplement(context.Background(), board, item)
	if err == nil {
		t.Fatal("expected error when the spawn target is not served by this instance")
	}
	if spawned {
		t.Error("expected spawned=false")
	}
	if len(client.createIssueCalls) != 0 {
		t.Errorf("expected 0 CreateIssue calls (scope check runs before any mutation), got %d", len(client.createIssueCalls))
	}
	if len(client.addProjectV2ItemCalls) != 0 {
		t.Errorf("expected 0 AddProjectV2ItemById calls, got %d", len(client.addProjectV2ItemCalls))
	}

	var pausedAdded bool
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			pausedAdded = true
		}
	}
	if !pausedAdded {
		t.Error("fabrik:paused not added when spawn target is not served by this instance")
	}

	var sawRefusalComment bool
	for _, c := range client.addCommentCalls {
		if strings.Contains(c.body, "not served by this Fabrik instance") {
			sawRefusalComment = true
		}
	}
	if !sawRefusalComment {
		t.Error("expected a comment explaining the spawn target is not served by this instance")
	}
}

// TestPreImplement_MultiRepoInstance_SpawnsCrossRepo pins the multi-repo shape
// (cfg.Repo == "") explicitly: this instance already legitimately serves any
// repo the org grants board access to (the issue's own "Instance A processes
// multiple repos on board 5 without issue"), so the scope-check must not
// interfere with it.
func TestPreImplement_MultiRepoInstance_SpawnsCrossRepo(t *testing.T) {
	client := &mockGitHubClient{
		createIssueFn: func(owner, repo, title, body string, assignees []string) (int, string, error) {
			return 700, "I_multirepo", nil
		},
		addProjectV2ItemByIdFn: func(projectID, contentNodeID string) (string, error) {
			return "PVTI_" + contentNodeID, nil
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	eng.cfg.Repo = "" // multi-repo mode
	eng.worktreeManagers["owner/repo"] = NewWorktreeManager(t.TempDir())
	eng.worktreeManagers["owner/child"] = NewWorktreeManager(t.TempDir())

	item := planItemWithBlocks(`
FABRIK_SPAWN_CHILD_BEGIN owner/child
TITLE: Cross-repo child a multi-repo instance may serve
Body.
FABRIK_SPAWN_CHILD_END
`)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	spawned, err := eng.preImplement(context.Background(), board, item)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !spawned {
		t.Fatal("expected spawned=true for a multi-repo instance")
	}
	if len(client.createIssueCalls) != 1 {
		t.Errorf("expected 1 CreateIssue call, got %d", len(client.createIssueCalls))
	}
}

// TestPreImplement_RepoScopedInstance_SameRepoSpawnUnaffected verifies the
// scope-check does not interfere with the ordinary same-repo spawn shape: a
// repo:-scoped instance spawning within its own configured repo.
func TestPreImplement_RepoScopedInstance_SameRepoSpawnUnaffected(t *testing.T) {
	client := &mockGitHubClient{
		createIssueFn: func(owner, repo, title, body string, assignees []string) (int, string, error) {
			return 701, "I_samerepo", nil
		},
		addProjectV2ItemByIdFn: func(projectID, contentNodeID string) (string, error) {
			return "PVTI_" + contentNodeID, nil
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{}) // cfg.Owner="owner", cfg.Repo="repo"
	eng.worktreeManagers["owner/repo"] = NewWorktreeManager(t.TempDir())

	item := planItemWithBlocks(`
FABRIK_SPAWN_CHILD_BEGIN owner/repo
TITLE: Same-repo child
Body.
FABRIK_SPAWN_CHILD_END
`)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	spawned, err := eng.preImplement(context.Background(), board, item)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !spawned {
		t.Fatal("expected spawned=true for a same-repo spawn")
	}
	if len(client.createIssueCalls) != 1 {
		t.Errorf("expected 1 CreateIssue call, got %d", len(client.createIssueCalls))
	}
}

// TestPreImplement_EveryCreateIssueCallCarriesAssignee is requirement 4: every
// spawned child must be assigned to the user: of the instance meant to
// process it, regardless of same-repo or cross-repo spawning.
func TestPreImplement_EveryCreateIssueCallCarriesAssignee(t *testing.T) {
	childCounter := 0
	client := &mockGitHubClient{
		createIssueFn: func(owner, repo, title, body string, assignees []string) (int, string, error) {
			childCounter++
			return 800 + childCounter, fmt.Sprintf("I_assignee%d", childCounter), nil
		},
		addProjectV2ItemByIdFn: func(projectID, contentNodeID string) (string, error) {
			return "PVTI_" + contentNodeID, nil
		},
	}
	eng := spawnTestEngine(t, client) // multi-repo mode; cfg.User == "testuser"

	item := planItemWithBlocks(`
FABRIK_SPAWN_CHILD_BEGIN owner/child
TITLE: Child one
Body one.
FABRIK_SPAWN_CHILD_END

FABRIK_SPAWN_CHILD_BEGIN owner/child
TITLE: Child two
Body two.
FABRIK_SPAWN_CHILD_END
`)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	if _, err := eng.preImplement(context.Background(), board, item); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(client.createIssueCalls) != 2 {
		t.Fatalf("expected 2 CreateIssue calls, got %d", len(client.createIssueCalls))
	}
	for i, c := range client.createIssueCalls {
		if len(c.assignees) != 1 || c.assignees[0] != "testuser" {
			t.Errorf("call %d: assignees = %v, want [testuser]", i, c.assignees)
		}
	}
}

// ---- status-set and label-inheritance tests ----

func spawnTestEngineWithSpecify(t *testing.T, client *mockGitHubClient) *Engine {
	t.Helper()
	eng := spawnTestEngine(t, client)
	eng.statusField = &gh.StatusField{
		FieldID: "FIELD_STATUS",
		Options: map[string]string{
			"Backlog": "OPT_0",
			"Specify": "OPT_1",
			"Done":    "OPT_9",
		},
		OrderedOptionNames: []string{"Backlog", "Specify", "Done"},
	}
	return eng
}

func TestPreImplement_SetsSpecifyStatus(t *testing.T) {
	client := &mockGitHubClient{
		createIssueFn: func(owner, repo, title, body string, assignees []string) (int, string, error) {
			return 101, "I_child101", nil
		},
		addProjectV2ItemByIdFn: func(projectID, contentNodeID string) (string, error) {
			return "PVTI_child101", nil
		},
	}
	eng := spawnTestEngineWithSpecify(t, client)

	item := planItemWithBlocks(`
FABRIK_SPAWN_CHILD_BEGIN owner/child
TITLE: Test child
Body.
FABRIK_SPAWN_CHILD_END
`)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	spawned, err := eng.preImplement(context.Background(), board, item)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !spawned {
		t.Fatal("expected spawned=true")
	}

	// UpdateProjectItemStatus must be called with correct args.
	if len(client.updateStatusCalls) != 1 {
		t.Fatalf("expected 1 UpdateProjectItemStatus call, got %d", len(client.updateStatusCalls))
	}
	call := client.updateStatusCalls[0]
	if call.projectID != "PVT_1" {
		t.Errorf("projectID = %q, want PVT_1", call.projectID)
	}
	if call.itemID != "PVTI_child101" {
		t.Errorf("itemID = %q, want PVTI_child101", call.itemID)
	}
	if call.fieldID != "FIELD_STATUS" {
		t.Errorf("fieldID = %q, want FIELD_STATUS", call.fieldID)
	}
	if call.optionID != "OPT_1" {
		t.Errorf("optionID = %q, want OPT_1 (Specify)", call.optionID)
	}
}

func TestPreImplement_StatusSetNilField(t *testing.T) {
	client := &mockGitHubClient{
		createIssueFn: func(owner, repo, title, body string, assignees []string) (int, string, error) {
			return 102, "I_child102", nil
		},
		addProjectV2ItemByIdFn: func(projectID, contentNodeID string) (string, error) {
			return "PVTI_child102", nil
		},
	}
	eng := spawnTestEngine(t, client)
	// statusField is nil by default in spawnTestEngine

	item := planItemWithBlocks(`
FABRIK_SPAWN_CHILD_BEGIN owner/child
TITLE: Test child no status
Body.
FABRIK_SPAWN_CHILD_END
`)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	spawned, err := eng.preImplement(context.Background(), board, item)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !spawned {
		t.Fatal("expected spawned=true even when statusField is nil")
	}

	// No UpdateProjectItemStatus call should happen.
	if len(client.updateStatusCalls) != 0 {
		t.Errorf("expected 0 UpdateProjectItemStatus calls, got %d", len(client.updateStatusCalls))
	}
}

func TestPreImplement_YoloInheritance(t *testing.T) {
	client := &mockGitHubClient{}
	eng := spawnTestEngineWithSpecify(t, client)

	item := planItemWithBlocks(`
FABRIK_SPAWN_CHILD_BEGIN owner/child
TITLE: Child for yolo
Body.
FABRIK_SPAWN_CHILD_END
`)
	item.Labels = append(item.Labels, "fabrik:yolo")
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	if _, err := eng.preImplement(context.Background(), board, item); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var yoloAdded, cruiseAdded bool
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:yolo" {
			yoloAdded = true
		}
		if c.labelName == "fabrik:cruise" {
			cruiseAdded = true
		}
	}
	if !yoloAdded {
		t.Error("fabrik:yolo not added to child when parent has it")
	}
	if cruiseAdded {
		t.Error("fabrik:cruise must not be added when parent does not have it")
	}
}

func TestPreImplement_CruiseInheritance(t *testing.T) {
	client := &mockGitHubClient{}
	eng := spawnTestEngineWithSpecify(t, client)

	item := planItemWithBlocks(`
FABRIK_SPAWN_CHILD_BEGIN owner/child
TITLE: Child for cruise
Body.
FABRIK_SPAWN_CHILD_END
`)
	item.Labels = append(item.Labels, "fabrik:cruise")
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	if _, err := eng.preImplement(context.Background(), board, item); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var yoloAdded, cruiseAdded bool
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:yolo" {
			yoloAdded = true
		}
		if c.labelName == "fabrik:cruise" {
			cruiseAdded = true
		}
	}
	if yoloAdded {
		t.Error("fabrik:yolo must not be added when parent does not have it")
	}
	if !cruiseAdded {
		t.Error("fabrik:cruise not added to child when parent has it")
	}
}

func TestPreImplement_BothLabelsInherited(t *testing.T) {
	client := &mockGitHubClient{}
	eng := spawnTestEngineWithSpecify(t, client)

	item := planItemWithBlocks(`
FABRIK_SPAWN_CHILD_BEGIN owner/child
TITLE: Child for both
Body.
FABRIK_SPAWN_CHILD_END
`)
	item.Labels = append(item.Labels, "fabrik:yolo", "fabrik:cruise")
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	if _, err := eng.preImplement(context.Background(), board, item); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var yoloAdded, cruiseAdded bool
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:yolo" {
			yoloAdded = true
		}
		if c.labelName == "fabrik:cruise" {
			cruiseAdded = true
		}
	}
	if !yoloAdded {
		t.Error("fabrik:yolo not added to child when parent has both labels")
	}
	if !cruiseAdded {
		t.Error("fabrik:cruise not added to child when parent has both labels")
	}
}

func TestPreImplement_NoAutonomyLabels(t *testing.T) {
	client := &mockGitHubClient{}
	eng := spawnTestEngineWithSpecify(t, client)

	item := planItemWithBlocks(`
FABRIK_SPAWN_CHILD_BEGIN owner/child
TITLE: Child without autonomy labels
Body.
FABRIK_SPAWN_CHILD_END
`)
	// item.Labels has only "stage:Plan:complete" — no yolo or cruise
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	if _, err := eng.preImplement(context.Background(), board, item); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:yolo" {
			t.Error("fabrik:yolo must not be added when parent does not have it")
		}
		if c.labelName == "fabrik:cruise" {
			t.Error("fabrik:cruise must not be added when parent does not have it")
		}
	}
}

// ---- #1263 regression: prose mentions must not destroy real blocks ----

// TestParseSpawnBlocks_ProseMentionDoesNotConsumeRealBlock reproduces the
// production failure from e2e TestCrossRepoSpawn (parent fabrik-test-alpha#3796).
// The Plan both described the marker in a markdown checklist AND emitted a real
// block. The backticked mention carried a slash-containing token
// ("handarbeit/fabrik-test-beta`"), so the pre-fix "contains a slash" check
// accepted it as a block opener; the parser then ran to the real END, consumed
// the authentic block as the phantom's content, failed the TITLE: check, and
// dropped it — yielding zero blocks and a silent no-op spawn.
func TestParseSpawnBlocks_ProseMentionDoesNotConsumeRealBlock(t *testing.T) {
	body := "## Approach\n" +
		"\n" +
		"1. Plan (this stage) emits a `FABRIK_SPAWN_CHILD_BEGIN` block scoped to the beta-side work.\n" +
		"\n" +
		"### Task checklist\n" +
		"\n" +
		"- [ ] Emit `FABRIK_SPAWN_CHILD_BEGIN handarbeit/fabrik-test-beta` block (this stage) scoping the function + test\n" +
		"\n" +
		"FABRIK_SPAWN_CHILD_BEGIN handarbeit/fabrik-test-beta\n" +
		"TITLE: Add HelloIssue3796() greeting function\n" +
		"\n" +
		"## Problem\n" +
		"\n" +
		"Beta-side half of the cross-repo spawn regression test.\n" +
		"FABRIK_SPAWN_CHILD_END\n"

	blocks := ParseSpawnBlocks(body)
	if len(blocks) != 1 {
		t.Fatalf("expected exactly 1 block (the real one), got %d — a prose mention consumed it", len(blocks))
	}
	b := blocks[0]
	if b.Repo != "handarbeit/fabrik-test-beta" {
		t.Errorf("repo: got %q, want %q", b.Repo, "handarbeit/fabrik-test-beta")
	}
	if b.Title != "Add HelloIssue3796() greeting function" {
		t.Errorf("title: got %q, want %q", b.Title, "Add HelloIssue3796() greeting function")
	}
	if !strings.Contains(b.Body, "Beta-side half") {
		t.Errorf("body should carry the real block's content, got: %q", b.Body)
	}
}

// TestParseSpawnBlocks_ProseMentionAtLineStartRejected covers the variant the
// leading-whitespace tolerance could otherwise let through: the marker opens
// the line, but trailing prose follows the repo token. Real blocks carry the
// repo and nothing else.
func TestParseSpawnBlocks_ProseMentionAtLineStartRejected(t *testing.T) {
	body := `
FABRIK_SPAWN_CHILD_BEGIN owner/repo is the marker you emit to spawn a child.
TITLE: Not a real block
FABRIK_SPAWN_CHILD_END
`
	if blocks := ParseSpawnBlocks(body); len(blocks) != 0 {
		t.Fatalf("expected 0 blocks for a prose line with trailing text, got %d", len(blocks))
	}
}

// TestParseSpawnBlocks_BacktickedRepoRejected pins the specific token shape that
// defeated the old "contains a slash" validation.
func TestParseSpawnBlocks_BacktickedRepoRejected(t *testing.T) {
	body := "FABRIK_SPAWN_CHILD_BEGIN handarbeit/fabrik-test-beta`\nTITLE: Phantom\nFABRIK_SPAWN_CHILD_END\n"
	if blocks := ParseSpawnBlocks(body); len(blocks) != 0 {
		t.Fatalf("expected 0 blocks for a backtick-suffixed repo, got %d", len(blocks))
	}
}

// TestParseSpawnBlocks_IndentedBlockParses documents the deliberate tolerance:
// a real block nested under a list item still parses. Only trailing content
// after the repo disqualifies a line, not leading whitespace.
func TestParseSpawnBlocks_IndentedBlockParses(t *testing.T) {
	body := `
  FABRIK_SPAWN_CHILD_BEGIN owner/child
  TITLE: Indented but genuine
  Body text here.
  FABRIK_SPAWN_CHILD_END
`
	blocks := ParseSpawnBlocks(body)
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	if blocks[0].Repo != "owner/child" {
		t.Errorf("repo: got %q, want %q", blocks[0].Repo, "owner/child")
	}
	if blocks[0].Title != "Indented but genuine" {
		t.Errorf("title: got %q, want %q", blocks[0].Title, "Indented but genuine")
	}
}

// TestParseSpawnBlocks_MalformedBlockDoesNotSwallowNext ensures a block that
// fails the TITLE: check does not re-open scanning inside its own content and
// consume the following well-formed block.
func TestParseSpawnBlocks_MalformedBlockDoesNotSwallowNext(t *testing.T) {
	body := `
FABRIK_SPAWN_CHILD_BEGIN owner/first
no title line here, so this block is malformed
FABRIK_SPAWN_CHILD_END

FABRIK_SPAWN_CHILD_BEGIN owner/second
TITLE: Second child
Second body.
FABRIK_SPAWN_CHILD_END
`
	blocks := ParseSpawnBlocks(body)
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block (the well-formed second), got %d", len(blocks))
	}
	if blocks[0].Repo != "owner/second" {
		t.Errorf("repo: got %q, want %q", blocks[0].Repo, "owner/second")
	}
}

// TestParseSpawnBlocks_MarkerRequiresWordBoundary pins the separator between the
// marker and its repo argument. Without it, "FABRIK_SPAWN_CHILD_BEGINowner/repo"
// satisfies HasPrefix and survives TrimPrefix as a lone repo-shaped field — the
// same boundary confusion the own-line rule exists to reject. Raised in review
// of #1263.
func TestParseSpawnBlocks_MarkerRequiresWordBoundary(t *testing.T) {
	body := "FABRIK_SPAWN_CHILD_BEGINowner/repo\nTITLE: Glued marker\nFABRIK_SPAWN_CHILD_END\n"
	if blocks := ParseSpawnBlocks(body); len(blocks) != 0 {
		t.Fatalf("expected 0 blocks when no separator follows the marker, got %d", len(blocks))
	}

	// A tab separator is legitimate and must still parse.
	tabbed := "FABRIK_SPAWN_CHILD_BEGIN\towner/repo\nTITLE: Tab separated\nFABRIK_SPAWN_CHILD_END\n"
	blocks := ParseSpawnBlocks(tabbed)
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block for a tab-separated repo, got %d", len(blocks))
	}
	if blocks[0].Repo != "owner/repo" {
		t.Errorf("repo: got %q, want %q", blocks[0].Repo, "owner/repo")
	}
}
