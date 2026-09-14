package pruefer

import (
	"fmt"
	"strings"
	"testing"
)

// --- parseSkillFrontmatter ---

func TestParseSkillFrontmatter_NoFrontmatter_WholeFileIsBody(t *testing.T) {
	mode, body, ok := parseSkillFrontmatter([]byte("Always check error wrapping uses %w.\n"))
	if !ok {
		t.Fatal("expected ok=true for a file with no frontmatter at all")
	}
	if mode != "" {
		t.Errorf("mode = %q, want empty", mode)
	}
	if body != "Always check error wrapping uses %w.\n" {
		t.Errorf("body = %q, want the whole file verbatim", body)
	}
}

func TestParseSkillFrontmatter_ValidFrontmatter_AppendMode(t *testing.T) {
	data := "---\nmode: append\n---\nCheck for %w error wrapping.\n"
	mode, body, ok := parseSkillFrontmatter([]byte(data))
	if !ok {
		t.Fatal("expected ok=true for valid frontmatter")
	}
	if mode != "append" {
		t.Errorf("mode = %q, want %q", mode, "append")
	}
	if body != "Check for %w error wrapping.\n" {
		t.Errorf("body = %q, want the text after the closing delimiter", body)
	}
}

func TestParseSkillFrontmatter_ValidFrontmatter_ReplaceMode(t *testing.T) {
	data := "---\nmode: replace\n---\nOnly check for %w error wrapping. Ignore everything else.\n"
	mode, body, ok := parseSkillFrontmatter([]byte(data))
	if !ok {
		t.Fatal("expected ok=true for valid frontmatter")
	}
	if mode != "replace" {
		t.Errorf("mode = %q, want %q", mode, "replace")
	}
	if !strings.Contains(body, "Only check for %w") {
		t.Errorf("body = %q, want the replace-mode guidance text", body)
	}
}

// TestParseSkillFrontmatter_CRLFLineEndings_RecognizesFrontmatter guards
// against a Windows-authored SKILL.md (CRLF line endings): the delimiter
// check must strip both "\r" and "\n", not "\n" alone, or a "---\r\n" line
// never equals frontmatterDelim and the whole frontmatter block (fences and
// mode declaration alike) leaks into the guidance body as literal text
// instead of being parsed — found in review (handarbeit-pruefer). Non-
// vacuous: reverting the fix to strings.TrimRight(lines[i], "\n") makes this
// fail, since the returned "body" would then contain the literal "---\r"
// fence lines and mode would come back empty instead of "append".
func TestParseSkillFrontmatter_CRLFLineEndings_RecognizesFrontmatter(t *testing.T) {
	data := "---\r\nmode: append\r\n---\r\nCheck for %w error wrapping.\r\n"
	mode, body, ok := parseSkillFrontmatter([]byte(data))
	if !ok {
		t.Fatal("expected ok=true for valid CRLF-terminated frontmatter")
	}
	if mode != "append" {
		t.Errorf("mode = %q, want %q — CRLF delimiter lines must be recognized", mode, "append")
	}
	if strings.Contains(body, "---") || strings.Contains(body, "mode:") {
		t.Errorf("body = %q, want the frontmatter fences/declaration stripped, not leaked into the guidance text", body)
	}
	if !strings.Contains(body, "Check for %w error wrapping.") {
		t.Errorf("body = %q, want the guidance text after the closing delimiter", body)
	}
}

func TestParseSkillFrontmatter_EmptyFrontmatter_EmptyMode(t *testing.T) {
	data := "---\n---\nJust guidance, no mode declared.\n"
	mode, body, ok := parseSkillFrontmatter([]byte(data))
	if !ok {
		t.Fatal("expected ok=true for an empty-but-well-formed frontmatter block")
	}
	if mode != "" {
		t.Errorf("mode = %q, want empty", mode)
	}
	if body != "Just guidance, no mode declared.\n" {
		t.Errorf("body = %q", body)
	}
}

// TestParseSkillFrontmatter_UnclosedDelimiter_Degrades and
// TestParseSkillFrontmatter_InvalidYAML_Degrades pin R4's fail-safe
// direction: malformed frontmatter degrades the *whole file*, not just the
// mode. This is non-vacuous (AC8) — a version of parseSkillFrontmatter that
// instead fell back to "treat the whole thing as an append-mode body" would
// return ok=true here, which these assertions would catch.
func TestParseSkillFrontmatter_UnclosedDelimiter_Degrades(t *testing.T) {
	data := "---\nmode: append\nno closing delimiter, guidance text runs on forever\n"
	_, _, ok := parseSkillFrontmatter([]byte(data))
	if ok {
		t.Error("expected ok=false for frontmatter opened but never closed")
	}
}

func TestParseSkillFrontmatter_InvalidYAML_Degrades(t *testing.T) {
	data := "---\nmode: [this is not valid yaml for a string field\n---\nguidance\n"
	_, _, ok := parseSkillFrontmatter([]byte(data))
	if ok {
		t.Error("expected ok=false for invalid YAML between the frontmatter delimiters")
	}
}

// --- normalizeGuidanceMode ---

func TestNormalizeGuidanceMode_RecognizedValues(t *testing.T) {
	for _, m := range []string{"", GuidanceModeAppend, GuidanceModeReplace} {
		got, ok := normalizeGuidanceMode(m)
		if !ok {
			t.Errorf("normalizeGuidanceMode(%q): ok = false, want true", m)
		}
		want := m
		if m == "" {
			want = GuidanceModeAppend
		}
		if got != want {
			t.Errorf("normalizeGuidanceMode(%q) = %q, want %q", m, got, want)
		}
	}
}

// TestNormalizeGuidanceMode_Unrecognized_DefaultsToAppend pins the fail-safe
// direction explicitly (AC8): an unrecognized mode must default to append —
// the non-destructive choice — never replace. A version that instead
// defaulted to replace would silently destroy a repo's default guidance on
// a mode typo; this assertion would fail against that version.
func TestNormalizeGuidanceMode_Unrecognized_DefaultsToAppend(t *testing.T) {
	got, ok := normalizeGuidanceMode("replce") // typo
	if ok {
		t.Error("ok = true, want false for an unrecognized mode value")
	}
	if got != GuidanceModeAppend {
		t.Errorf("normalizeGuidanceMode(typo) = %q, want %q (the safe, non-destructive default)", got, GuidanceModeAppend)
	}
}

// --- fetchRepoGuidance ---

func TestFetchRepoGuidance_Absent_Silent(t *testing.T) {
	client := newFakeReviewer() // repoSkillData is nil by default -> gh.ErrNotFound
	text, mode, prov := fetchRepoGuidance(client, "owner", "repo", "main")
	if text != "" || mode != "" {
		t.Errorf("text/mode = %q/%q, want empty when no skill file exists", text, mode)
	}
	if prov.Found {
		t.Error("prov.Found = true, want false")
	}
	if prov.Invalid {
		t.Error("prov.Invalid = true, want false — absent is not a compliance-drift signal")
	}
}

func TestFetchRepoGuidance_FetchError_DegradesInvalid(t *testing.T) {
	client := newFakeReviewer()
	client.repoSkillErr = fmt.Errorf("network blip")
	text, _, prov := fetchRepoGuidance(client, "owner", "repo", "main")
	if text != "" {
		t.Errorf("text = %q, want empty on fetch error", text)
	}
	if !prov.Invalid {
		t.Error("prov.Invalid = false, want true on a genuine fetch error")
	}
}

func TestFetchRepoGuidance_Oversized_Degrades(t *testing.T) {
	client := newFakeReviewer()
	client.repoSkillData = []byte(strings.Repeat("a", DefaultMaxReviewSkillBytes+1))
	text, _, prov := fetchRepoGuidance(client, "owner", "repo", "main")
	if text != "" {
		t.Errorf("text = %q, want empty for an oversized skill file", text)
	}
	if !prov.Invalid {
		t.Error("prov.Invalid = false, want true for a file over the size cap")
	}
	if !strings.Contains(prov.Reason, "exceeds") {
		t.Errorf("prov.Reason = %q, want it to name the size-cap violation", prov.Reason)
	}
}

func TestFetchRepoGuidance_NonUTF8_Degrades(t *testing.T) {
	client := newFakeReviewer()
	client.repoSkillData = []byte{0xff, 0xfe, 0x00, 0x01}
	text, _, prov := fetchRepoGuidance(client, "owner", "repo", "main")
	if text != "" {
		t.Errorf("text = %q, want empty for non-UTF-8 content", text)
	}
	if !prov.Invalid {
		t.Error("prov.Invalid = false, want true for non-UTF-8 content")
	}
}

// TestFetchRepoGuidance_MalformedFrontmatter_DegradesWholeFile pins R4's
// "malformed frontmatter degrades the entire file" rule at the
// fetchRepoGuidance level (not just parseSkillFrontmatter's own return
// value) — the review still runs with no repo guidance at all, not a
// partial/garbled guidance string.
func TestFetchRepoGuidance_MalformedFrontmatter_DegradesWholeFile(t *testing.T) {
	client := newFakeReviewer()
	client.repoSkillData = []byte("---\nmode: append\nnever closed\n")
	text, mode, prov := fetchRepoGuidance(client, "owner", "repo", "main")
	if text != "" || mode != "" {
		t.Errorf("text/mode = %q/%q, want both empty when frontmatter is malformed", text, mode)
	}
	if !prov.Invalid {
		t.Error("prov.Invalid = false, want true for malformed frontmatter")
	}
}

// TestFetchRepoGuidance_UnrecognizedMode_DegradesModeOnlyKeepsText is the R4
// non-vacuousness case (AC8) for the "unrecognized mode degrades only the
// mode, not the text" claim: the guidance text must still come through
// (composed in append mode), distinct from the malformed-frontmatter case
// above where the whole file is dropped.
func TestFetchRepoGuidance_UnrecognizedMode_DegradesModeOnlyKeepsText(t *testing.T) {
	client := newFakeReviewer()
	client.repoSkillData = []byte("---\nmode: replce\n---\nAlways check error wrapping uses %w.\n")
	text, mode, prov := fetchRepoGuidance(client, "owner", "repo", "main")
	if !strings.Contains(text, "error wrapping") {
		t.Errorf("text = %q, want the guidance body preserved despite the mode typo", text)
	}
	if mode != GuidanceModeAppend {
		t.Errorf("mode = %q, want %q (the safe default for an unrecognized mode)", mode, GuidanceModeAppend)
	}
	if prov.Invalid {
		t.Error("prov.Invalid = true, want false — an unrecognized mode is a narrower failure than malformed frontmatter")
	}
	if !prov.ModeInvalid {
		t.Error("prov.ModeInvalid = false, want true")
	}
}

func TestFetchRepoGuidance_ValidAppendMode(t *testing.T) {
	client := newFakeReviewer()
	client.repoSkillData = []byte("---\nmode: append\n---\nAlways check error wrapping uses %w.\n")
	text, mode, prov := fetchRepoGuidance(client, "owner", "repo", "main")
	if !strings.Contains(text, "error wrapping") {
		t.Errorf("text = %q, want the guidance body", text)
	}
	if mode != GuidanceModeAppend {
		t.Errorf("mode = %q, want %q", mode, GuidanceModeAppend)
	}
	if !prov.Found || prov.Invalid || prov.ModeInvalid {
		t.Errorf("prov = %+v, want Found=true, Invalid=false, ModeInvalid=false", prov)
	}
}

func TestFetchRepoGuidance_ValidReplaceMode(t *testing.T) {
	client := newFakeReviewer()
	client.repoSkillData = []byte("---\nmode: replace\n---\nOnly check for %w error wrapping.\n")
	text, mode, _ := fetchRepoGuidance(client, "owner", "repo", "main")
	if !strings.Contains(text, "Only check for %w") {
		t.Errorf("text = %q, want the replace-mode guidance body", text)
	}
	if mode != GuidanceModeReplace {
		t.Errorf("mode = %q, want %q", mode, GuidanceModeReplace)
	}
}

func TestFetchRepoGuidance_NoFrontmatter_WholeFileIsGuidance(t *testing.T) {
	client := newFakeReviewer()
	client.repoSkillData = []byte("Always check error wrapping uses %w.\n")
	text, mode, prov := fetchRepoGuidance(client, "owner", "repo", "main")
	if !strings.Contains(text, "error wrapping") {
		t.Errorf("text = %q, want the whole file as guidance", text)
	}
	if mode != GuidanceModeAppend {
		t.Errorf("mode = %q, want the default %q", mode, GuidanceModeAppend)
	}
	if !prov.Found || prov.Invalid {
		t.Errorf("prov = %+v, want Found=true, Invalid=false", prov)
	}
}

func TestFetchRepoGuidance_FrontmatterOnly_EmptyGuidance(t *testing.T) {
	client := newFakeReviewer()
	client.repoSkillData = []byte("---\nmode: append\n---\n")
	text, _, prov := fetchRepoGuidance(client, "owner", "repo", "main")
	if text != "" {
		t.Errorf("text = %q, want empty when the file has no guidance body after frontmatter", text)
	}
	if prov.Invalid {
		t.Error("prov.Invalid = true, want false — an empty body is not itself invalid")
	}
}

func TestFetchRepoGuidance_RequestsCorrectPathAndRef(t *testing.T) {
	client := newFakeReviewer()
	client.repoSkillData = []byte("guidance")
	_, _, _ = fetchRepoGuidance(client, "owner", "repo", "deadbeef")
	calls := client.fileAtRefCallArgs()
	if len(calls) != 1 {
		t.Fatalf("FetchFileAtRef called %d times, want 1", len(calls))
	}
	if calls[0].path != DefaultReviewSkillPath {
		t.Errorf("path = %q, want %q", calls[0].path, DefaultReviewSkillPath)
	}
	if calls[0].ref != "deadbeef" {
		t.Errorf("ref = %q, want %q", calls[0].ref, "deadbeef")
	}
}
