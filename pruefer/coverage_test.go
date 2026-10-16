package pruefer

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

// coverageReport renders a coverage block listing ids.
func coverageReport(ids ...string) string {
	if ids == nil {
		ids = []string{}
	}
	b, _ := json.Marshal(map[string][]string{"opened": ids})
	return "\nPRUEFER_COVERAGE_BEGIN\n" + string(b) + "\nPRUEFER_COVERAGE_END\n"
}

// coverageReportAll reports every staged entry of req as opened (empty when
// nothing was staged).
func coverageReportAll(req ReviewRequest) string {
	if req.Staged == nil {
		return ""
	}
	var ids []string
	for _, e := range req.Staged.Entries {
		ids = append(ids, e.ID)
	}
	return coverageReport(ids...)
}

// fixtureContent is the text of a staged test entry, for cat verification.
func fixtureContent(id string) string { return "content of " + id + "\nsecond line\n" }

// numbered renders what Read returns for lines from..to: "<n><tab><text>".
func numbered(from, to int) string {
	var b strings.Builder
	for n := from; n <= to; n++ {
		fmt.Fprintf(&b, "%d\tline %d\n", n, n)
	}
	return b.String()
}

// testStaged builds a StagedDiff with one entry per path (no files on disk).
func testStaged(paths ...string) *StagedDiff {
	s := &StagedDiff{Dir: "/stage"}
	for i, p := range paths {
		id := fmt.Sprintf("%04d", i+1)
		s.Entries = append(s.Entries, StagedEntry{ID: id, File: id + ".diff", Path: p, Chunk: 1, Chunks: 1, Lines: 10, content: fixtureContent(id)})
	}
	return s
}

func TestExtractCoverage(t *testing.T) {
	cases := []struct {
		name   string
		text   string
		wantOK bool
		want   []string
	}{
		{"well formed", "x\n```json\n[]\n```" + coverageReport("0001", "0002"), true, []string{"0001", "0002"}},
		{"empty list is a report", "x" + coverageReport(), true, []string{}},
		{"missing", "just prose\n```json\n[]\n```\n", false, nil},
		{"malformed json", "x\nPRUEFER_COVERAGE_BEGIN\n{nope\nPRUEFER_COVERAGE_END\n", false, nil},
		{"not an object with opened", "x\nPRUEFER_COVERAGE_BEGIN\n[\"0001\"]\nPRUEFER_COVERAGE_END\n", false, nil},
		{"missing opened key", "x\nPRUEFER_COVERAGE_BEGIN\n{}\nPRUEFER_COVERAGE_END\n", false, nil},
		{"begin without end", "x\nPRUEFER_COVERAGE_BEGIN\n{\"opened\":[\"0001\"]}\n", false, nil},
		{"mid-sentence marker is not a marker", "see PRUEFER_COVERAGE_BEGIN {\"opened\":[\"0001\"]} PRUEFER_COVERAGE_END", false, nil},
		{"the final block wins", "x" + coverageReport("0001") + coverageReport("0002"), true, []string{"0002"}},
		{"text after the block means no report", "x" + coverageReport("0001") + "\nand one more thing\n", false, nil},
		{"trailing whitespace is fine", "x" + coverageReport("0001") + "\n\n  \n", true, []string{"0001"}},
		{"a quoted BEGIN without an END does not swallow the report", "see\nPRUEFER_COVERAGE_BEGIN\nin the docs\n" + coverageReport("0003"), true, []string{"0003"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok, rest := extractCoverage(c.text)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if ok && strings.Join(got, ",") != strings.Join(c.want, ",") {
				t.Errorf("opened = %v, want %v", got, c.want)
			}
			if c.wantOK && strings.Contains(rest[strings.LastIndex(rest, "\n")+1:], "PRUEFER_COVERAGE_END") {
				t.Errorf("well-formed block not stripped from %q", rest)
			}
		})
	}
}

// TestExtractCoverage_QuotedMarkersInProseSurvive: a summary or finding that
// quotes the contract (this repo's own docs do) is neither deleted nor read as
// the report. Non-vacuity: the previous strip-everywhere behaviour fails it.
func TestExtractCoverage_QuotedMarkersInProseSurvive(t *testing.T) {
	quoted := "PRUEFER_COVERAGE_BEGIN\n{\"opened\":[\"0001\"]}\nPRUEFER_COVERAGE_END\n"
	text := "PRUEFER_SUMMARY_BEGIN\nThe PR documents:\n" + quoted + "PRUEFER_SUMMARY_END\n```json\n[]\n```\n"
	opened, ok, rest := extractCoverage(text)
	if ok || opened != nil {
		t.Errorf("a quoted block in the summary was read as the report: %v %v", opened, ok)
	}
	if rest != text {
		t.Errorf("quoted block was altered:\n%s", rest)
	}
	// With the real report after it, only the real one is removed.
	_, ok, rest = extractCoverage(text + coverageReport("0002"))
	if !ok || !strings.Contains(rest, quoted) {
		t.Errorf("ok=%v, quoted block must survive in %q", ok, rest)
	}
}

func TestComputeCoverage_MissingOrUnparseableIsPartial(t *testing.T) {
	staged := testStaged("a.go", "b.go")
	cov := computeCoverage(coverageInput{Staged: staged})
	if cov.Complete {
		t.Fatalf("a review with no coverage report was %+v, want partial", cov)
	}
	if cov.Reason != "reviewer did not report coverage" || strings.Join(cov.Unread, ",") != "a.go,b.go" {
		t.Errorf("cov = %+v, want every path unread and the missing-report reason", cov)
	}
}

// TestComputeCoverage_MissingFieldNonVacuous: with the fail-closed default
// neutralised the same missing-report input becomes complete, so the assertion
// above can only hold because of the fix.
func TestComputeCoverage_MissingFieldNonVacuous(t *testing.T) {
	coverageFailOpenForTest = true
	defer func() { coverageFailOpenForTest = false }()
	if cov := computeCoverage(coverageInput{Staged: testStaged("a.go", "b.go")}); !cov.Complete {
		t.Fatalf("with the fail-closed default neutralised the missing report should read complete, got %+v", cov)
	}
}

func TestComputeCoverage(t *testing.T) {
	staged := testStaged("a.go", "b.go", "c.go")
	cases := []struct {
		name         string
		in           coverageInput
		wantComplete bool
		wantUnread   string
	}{
		{"all reported", coverageInput{Reported: []string{"0001", "0002", "0003"}, ReportOK: true}, true, ""},
		{"one unreported entry is unread", coverageInput{Reported: []string{"0001", "0003"}, ReportOK: true}, false, "b.go"},
		{"unknown IDs ignored", coverageInput{Reported: []string{"0001", "9999", "0003"}, ReportOK: true}, false, "b.go"},
		{"file suffix tolerated", coverageInput{Reported: []string{"0001.diff", "0002.diff", "0003.diff"}, ReportOK: true}, true, ""},
		{"empty report", coverageInput{Reported: nil, ReportOK: true}, false, "a.go,b.go,c.go"},
		{"trimmed paths force partial", coverageInput{Reported: []string{"0001", "0002", "0003"}, ReportOK: true, TrimmedPaths: []string{"huge.go"}}, false, "huge.go"},
		{"authoritative transcript downgrades a forged claim", coverageInput{Reported: []string{"0001", "0002", "0003"}, ReportOK: true,
			Transcript: map[string]bool{"0001": true, "0003": true}, TranscriptAuthoritative: true}, false, "b.go"},
		{"non-authoritative transcript does not downgrade", coverageInput{Reported: []string{"0001", "0002", "0003"}, ReportOK: true,
			Transcript: map[string]bool{"0001": true}}, true, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.in.Staged = staged
			cov := computeCoverage(c.in)
			if cov.Complete != c.wantComplete {
				t.Fatalf("Complete = %v, want %v (%+v)", cov.Complete, c.wantComplete, cov)
			}
			if got := strings.Join(cov.Unread, ","); got != c.wantUnread {
				t.Errorf("Unread = %q, want %q", got, c.wantUnread)
			}
		})
	}
}

// TestComputeCoverage_ExcludedPathsNeverCount: paths dropped by the operator's
// excluded_paths are not in the staged diff and not in TrimmedPaths, so they
// neither appear as unread nor stop a review from being complete.
func TestComputeCoverage_ExcludedPathsNeverCount(t *testing.T) {
	cov := computeCoverage(coverageInput{Staged: testStaged("a.go"), Reported: []string{"0001"}, ReportOK: true})
	if !cov.Complete {
		t.Errorf("cov = %+v, want complete", cov)
	}
}

func TestComputeCoverage_ChunkedFileNeedsEveryChunk(t *testing.T) {
	s := &StagedDiff{Dir: "/stage", Entries: []StagedEntry{
		{ID: "0001", File: "0001.diff", Path: "big.go", Chunk: 1, Chunks: 2},
		{ID: "0002", File: "0002.diff", Path: "big.go", Chunk: 2, Chunks: 2},
	}}
	cov := computeCoverage(coverageInput{Staged: s, Reported: []string{"0001"}, ReportOK: true})
	if cov.Complete || strings.Join(cov.Unread, ",") != "big.go" || cov.UnreadCount != 1 {
		t.Errorf("cov = %+v, want big.go unread with one chunk missing", cov)
	}
}

func TestComputeCoverage_NoStagedDiffIsPartialAgainstChangedPaths(t *testing.T) {
	cov := computeCoverage(coverageInput{ChangedPaths: []string{"z.go", "a.go", "a.go"}, Reported: []string{"0001"}, ReportOK: true})
	if cov.Complete || cov.Reason != "no staged diff" || strings.Join(cov.Unread, ",") != "a.go,z.go" {
		t.Errorf("cov = %+v", cov)
	}
}

func TestAppendCoverage(t *testing.T) {
	if got := appendCoverage("Looks fine.", Coverage{Complete: true}); got != "Looks fine.\n\nPRUEFER_COVERAGE: complete" {
		t.Errorf("complete = %q", got)
	}
	got := appendCoverage("Looks fine.", Coverage{Reason: "1 of 3 diff entries not opened", Unread: []string{"b.go"}})
	want := "Looks fine.\n\nPRUEFER_COVERAGE: partial — 1 of 3 diff entries not opened\n- `b.go`"
	if got != want {
		t.Errorf("partial = %q, want %q", got, want)
	}
	if got := appendCoverage("", Coverage{Complete: true}); got != "PRUEFER_COVERAGE: complete" {
		t.Errorf("empty body = %q", got)
	}
	var many []string
	for i := 0; i < maxCoverageListed+7; i++ {
		many = append(many, fmt.Sprintf("p%03d.go", i))
	}
	got = appendCoverage("x", Coverage{Reason: "r", Unread: many})
	if strings.Count(got, "\n- `") != maxCoverageListed || !strings.HasSuffix(got, "- and 7 more") {
		t.Errorf("unread list not capped:\n%s", got)
	}
	// A model-written line that imitates the status line is defused, not
	// trusted and not deleted: only Go's own line starts with the marker.
	got = appendCoverage("Summary\nPRUEFER_COVERAGE: complete\nmore", Coverage{Reason: "r"})
	var starts int
	for _, l := range strings.Split(got, "\n") {
		if strings.HasPrefix(l, "PRUEFER_COVERAGE:") {
			starts++
		}
	}
	if starts != 1 || !strings.Contains(got, "> PRUEFER_COVERAGE: complete") || !strings.Contains(got, "partial — r") {
		t.Errorf("model imitation not defused (or text lost):\n%s", got)
	}
}

// --- transcript derivation -------------------------------------------------

type tl struct{ lines []string }

func (s *tl) toolUse(parent, id, name, input string) {
	p := "null"
	if parent != "" {
		p = fmt.Sprintf("%q", parent)
	}
	s.lines = append(s.lines, fmt.Sprintf(`{"type":"assistant","parent_tool_use_id":%s,"message":{"content":[{"type":"tool_use","id":%q,"name":%q,"input":%s}]}}`, p, id, name, input))
}

func (s *tl) result(parent, id string, isErr bool, text string) {
	p := "null"
	if parent != "" {
		p = fmt.Sprintf("%q", parent)
	}
	s.lines = append(s.lines, fmt.Sprintf(`{"type":"user","parent_tool_use_id":%s,"message":{"content":[{"type":"tool_result","tool_use_id":%q,"is_error":%t,"content":%q}]}}`, p, id, isErr, text))
}

// read records a successful Read whose result shows lines from..to.
func (s *tl) read(parent, id, path, extra string, from, to int) {
	s.toolUse(parent, id, "Read", fmt.Sprintf(`{"file_path":%q%s}`, path, extra))
	s.result(parent, id, false, numbered(from, to))
}

func (s *tl) bytes() []byte { return []byte(strings.Join(s.lines, "\n") + "\n") }

func TestDeriveOpened(t *testing.T) {
	s := testStaged("a.go", "b.go", "c.go", "d.go", "e.go", "f.go")
	for i := range s.Entries {
		s.Entries[i].Lines = 100
	}
	s.Entries[5].Wide = true
	var tr tl
	tr.read("", "r1", "/stage/0001.diff", "", 1, 100)                         // whole file
	tr.read("", "r2", "/stage/0002.diff", `,"offset":1,"limit":2000`, 1, 100) // explicit full window
	tr.read("", "r3", "/stage/0003.diff", `,"offset":50`, 50, 100)            // starts mid-file: not opened
	tr.toolUse("", "r4", "Read", `{"file_path":"/stage/0004.diff"}`)          // denied
	tr.result("", "r4", true, "denied")
	// The live-probe case: Read succeeds but silently stops early. A second
	// Read from the next line completes the entry.
	tr.read("", "r5a", "/stage/0005.diff", "", 1, 60)
	tr.read("", "r5b", "/stage/0005.diff", `,"offset":51`, 51, 100)
	tr.read("", "r6", "/stage/0006.diff", "", 1, 100) // wide-line entry: Read cuts lines, never credited
	got, auth := deriveOpened(tr.bytes(), s)
	if !got["0001"] || !got["0002"] || got["0003"] || got["0004"] || !got["0005"] || got["0006"] || !auth {
		t.Errorf("opened = %v authoritative=%v, want only 0001, 0002 and 0005", got, auth)
	}

	// A single Read cut short, with no error and no notice, is not an open.
	var cut tl
	cut.read("", "r1", "/stage/0001.diff", "", 1, 60)
	if got, _ := deriveOpened(cut.bytes(), s); got["0001"] {
		t.Errorf("a Read that stopped at line 60 of 100 was credited: %v", got)
	}

	var cat tl
	cat.toolUse("", "c1", "Bash", `{"command":"cat /stage/0001.diff /stage/0002.diff"}`)
	cat.result("", "c1", false, fixtureContent("0001")+fixtureContent("0002"))
	cat.toolUse("", "c2", "Bash", `{"command":"cat /stage/0003.diff | head -5"}`) // truncated by a pipe
	cat.result("", "c2", false, fixtureContent("0003"))
	cat.toolUse("", "c3", "Bash", `{"command":"head -n 5 /stage/0004.diff"}`)
	cat.result("", "c3", false, fixtureContent("0004"))
	cat.toolUse("", "c4", "Bash", `{"command":"cat /stage/0005.diff"}`) // output cut short
	cat.result("", "c4", false, "content of 0005\n")
	cat.toolUse("", "c5", "Bash", `{"command":"cat /stage/0006.diff"}`) // the way to open a wide-line entry
	cat.result("", "c5", false, fixtureContent("0006"))
	got, _ = deriveOpened(cat.bytes(), s)
	if !got["0001"] || !got["0002"] || got["0003"] || got["0004"] || got["0005"] || !got["0006"] {
		t.Errorf("cat opened = %v, want only 0001, 0002 and 0006", got)
	}
}

// subagentTranscript builds a review that splits four entries across two
// subagents, every Task call visible with its subagent's tagged traffic.
func subagentTranscript(readByB []string) []byte {
	var tr tl
	tr.toolUse("", "taskA", "Task", `{"prompt":"open 0001 0002"}`)
	tr.read("taskA", "a1", "/stage/0001.diff", "", 1, 100)
	tr.read("taskA", "a2", "/stage/0002.diff", "", 1, 100)
	tr.toolUse("", "taskB", "Task", `{"prompt":"open 0003 0004"}`)
	for i, p := range readByB {
		tr.read("taskB", fmt.Sprintf("b%d", i), p, "", 1, 100)
	}
	return tr.bytes()
}

func TestCoverage_DiffSplitAcrossSubagents(t *testing.T) {
	s := testStaged("a.go", "b.go", "c.go", "d.go")
	for i := range s.Entries {
		s.Entries[i].Lines = 100
	}
	reportAll := []string{"0001", "0002", "0003", "0004"}

	t.Run("every entry opened by some subagent is complete", func(t *testing.T) {
		opened, auth := deriveOpened(subagentTranscript([]string{"/stage/0003.diff", "/stage/0004.diff"}), s)
		if !auth {
			t.Fatal("all Task traffic is visible; the transcript should be authoritative")
		}
		cov := computeCoverage(coverageInput{Staged: s, Reported: reportAll, ReportOK: true, Transcript: opened, TranscriptAuthoritative: auth})
		if !cov.Complete {
			t.Errorf("cov = %+v, want complete", cov)
		}
	})

	partial := subagentTranscript([]string{"/stage/0003.diff"}) // subagent B never opened 0004
	t.Run("an entry no subagent opened is partial and named", func(t *testing.T) {
		opened, auth := deriveOpened(partial, s)
		cov := computeCoverage(coverageInput{Staged: s, Reported: reportAll, ReportOK: true, Transcript: opened, TranscriptAuthoritative: auth})
		if cov.Complete || strings.Join(cov.Unread, ",") != "d.go" {
			t.Errorf("cov = %+v, want partial naming d.go", cov)
		}
	})

	// Non-vacuity: if the transcript is ignored (treated as non-authoritative,
	// i.e. "a subagent reported it, so it counts"), the same fixture is wrongly
	// complete — so the downgrade above is what the assertion depends on.
	t.Run("ignoring the transcript wrongly reads complete", func(t *testing.T) {
		opened, _ := deriveOpened(partial, s)
		cov := computeCoverage(coverageInput{Staged: s, Reported: reportAll, ReportOK: true, Transcript: opened, TranscriptAuthoritative: false})
		if !cov.Complete {
			t.Errorf("neutralised variant should read complete, got %+v", cov)
		}
	})

	// A subagent whose reads are not visible cannot be verified: the model's
	// report of entries only that subagent could have read must not be credited.
	t.Run("unobserved subagents are not trusted on their report", func(t *testing.T) {
		var tr tl
		tr.toolUse("", "taskA", "Task", `{"prompt":"open everything"}`) // no tagged events follow
		tr.read("", "own", "/stage/0001.diff", "", 1, 100)              // the parent's own read is visible
		opened, auth := deriveOpened(tr.bytes(), s)
		cov := computeCoverage(coverageInput{Staged: s, Reported: reportAll, ReportOK: true, Transcript: opened, TranscriptAuthoritative: auth, UnobservedSubagents: !auth})
		if cov.Complete || strings.Join(cov.Unread, ",") != "b.go,c.go,d.go" {
			t.Errorf("cov = %+v, want partial naming b.go, c.go, d.go", cov)
		}
		if !strings.Contains(cov.Reason, "subagent reads are not visible") {
			t.Errorf("reason does not say why: %q", cov.Reason)
		}
		// Non-vacuity: trusting the report (the previous behaviour) reads complete.
		cov = computeCoverage(coverageInput{Staged: s, Reported: reportAll, ReportOK: true, Transcript: opened, TranscriptAuthoritative: false})
		if !cov.Complete {
			t.Errorf("neutralised variant should read complete, got %+v", cov)
		}
	})

	t.Run("hidden subagent traffic leaves the transcript non-authoritative", func(t *testing.T) {
		var tr tl
		tr.toolUse("", "taskA", "Task", `{"prompt":"open everything"}`) // no tagged events follow
		_, auth := deriveOpened(tr.bytes(), s)
		if auth {
			t.Error("a Task call with no visible traffic must not make the transcript authoritative")
		}
	})
}

// --- ReviewPR end to end -----------------------------------------------------

func TestReviewPR_CoverageComplete(t *testing.T) {
	client := newFakeReviewer()
	client.diff = reviewTestDiff
	var stagedDir string
	var existedDuringReview bool
	claude := &mockClaudeInvoker{fn: func(req ReviewRequest) (ReviewResult, error) {
		stagedDir = req.Staged.Dir
		_, err := os.Stat(req.Staged.IndexPath)
		existedDuringReview = err == nil
		return ReviewResult{Text: "All read." + coverageReportAll(req)}, nil
	}}
	clone, _ := fakeClone(t, nil)
	tracker := NewReviewTracker()
	pr := gh.PRDetails{Number: 1, Author: "alice", HeadSHA: "sha1"}
	outcome := ReviewPR(context.Background(), client, claude, clone, Config{}, "pruefer-bot[bot]", "owner", "repo", pr, tracker)

	if !outcome.Reviewed || outcome.Err != nil {
		t.Fatalf("outcome = %+v", outcome)
	}
	call := client.submitCalls[0]
	if call.body != "All read.\n\nPRUEFER_COVERAGE: complete" {
		t.Errorf("body = %q", call.body)
	}
	if call.event != gh.ReviewEventComment {
		t.Errorf("event = %v, want COMMENT — coverage never feeds decideEvent", call.event)
	}
	if !tracker.Recall("owner", "repo", 1, "sha1") {
		t.Error("tracker did not record the review")
	}
	if !existedDuringReview {
		t.Error("staged index was not on disk during the review")
	}
	if _, err := os.Stat(stagedDir); !os.IsNotExist(err) {
		t.Errorf("staging dir not cleaned up after the review: %v", err)
	}
}

func TestReviewPR_CoveragePartialWhenNotReported(t *testing.T) {
	client := newFakeReviewer()
	client.diff = reviewTestDiff
	claude := &mockClaudeInvoker{} // default "mock review": no coverage block
	clone, _ := fakeClone(t, nil)
	pr := gh.PRDetails{Number: 1, Author: "alice", HeadSHA: "sha1"}
	outcome := ReviewPR(context.Background(), client, claude, clone, Config{}, "pruefer-bot[bot]", "owner", "repo", pr, nil)

	if !outcome.Reviewed || outcome.Err != nil {
		t.Fatalf("a review missing the coverage field must still be submitted: %+v", outcome)
	}
	body := client.submitCalls[0].body
	if !strings.Contains(body, "\nPRUEFER_COVERAGE: partial — reviewer did not report coverage\n- `docs/readme.md`\n- `engine/claude.go`") {
		t.Errorf("body lacks the greppable partial line naming the unread path:\n%s", body)
	}
	if client.submitCalls[0].event != gh.ReviewEventComment {
		t.Errorf("event = %v, want COMMENT", client.submitCalls[0].event)
	}
}

func TestReviewPR_CoveragePartialNamesUnreportedEntries(t *testing.T) {
	client := newFakeReviewer()
	client.diff = threeFileDiffOneVendored()
	claude := &mockClaudeInvoker{fn: func(req ReviewRequest) (ReviewResult, error) {
		first := req.Staged.Entries[0].ID
		return ReviewResult{Text: "Read one." + coverageReport(first)}, nil
	}}
	clone, _ := fakeClone(t, nil)
	pr := gh.PRDetails{Number: 1, Author: "alice", HeadSHA: "sha1"}
	cfg := Config{ExcludedPaths: []string{"vendor/**"}}
	outcome := ReviewPR(context.Background(), client, claude, clone, cfg, "pruefer-bot[bot]", "owner", "repo", pr, nil)
	if !outcome.Reviewed {
		t.Fatalf("outcome = %+v", outcome)
	}
	body := client.submitCalls[0].body
	if !strings.Contains(body, "PRUEFER_COVERAGE: partial — 1 of 2 diff entries not opened") || strings.Contains(body, "vendor/") {
		t.Errorf("body = %q, want one unread survivor and no mention of the excluded path", body)
	}
}

func TestReviewPR_TrimmedPathsForcePartial(t *testing.T) {
	client := newFakeReviewer()
	client.diff = hugeExcludedFileDiff()
	claude := &mockClaudeInvoker{fn: func(req ReviewRequest) (ReviewResult, error) {
		return ReviewResult{Text: "Read everything staged." + coverageReportAll(req)}, nil
	}}
	clone, _ := fakeClone(t, nil)
	pr := gh.PRDetails{Number: 1, Author: "alice", HeadSHA: "sha1"}
	// No excluded_paths: the huge file is dropped by the size guard, not by the operator.
	outcome := ReviewPR(context.Background(), client, claude, clone, Config{MaxDiffBytes: 1000}, "pruefer-bot[bot]", "owner", "repo", pr, nil)
	if !outcome.Reviewed {
		t.Fatalf("outcome = %+v", outcome)
	}
	body := client.submitCalls[0].body
	if !strings.Contains(body, "PRUEFER_COVERAGE: partial — 1 file(s) dropped to fit max_diff_bytes were never reviewed") || !strings.Contains(body, "- `data/corpus.jsonl`") {
		t.Errorf("trimmed file not reported as unreviewed:\n%s", body)
	}
}

func TestReviewPR_ExcludedPathsDoNotForcePartial(t *testing.T) {
	client := newFakeReviewer()
	client.diff = hugeExcludedFileDiff()
	claude := &mockClaudeInvoker{fn: func(req ReviewRequest) (ReviewResult, error) {
		return ReviewResult{Text: "Read everything staged." + coverageReportAll(req)}, nil
	}}
	clone, _ := fakeClone(t, nil)
	pr := gh.PRDetails{Number: 1, Author: "alice", HeadSHA: "sha1"}
	cfg := Config{MaxDiffBytes: 1000, ExcludedPaths: []string{"data/corpus.jsonl"}}
	ReviewPR(context.Background(), client, claude, clone, cfg, "pruefer-bot[bot]", "owner", "repo", pr, nil)
	if body := client.submitCalls[0].body; !strings.HasSuffix(body, "PRUEFER_COVERAGE: complete") {
		t.Errorf("an operator-excluded path must not make the review partial:\n%s", body)
	}
}

func TestReviewPR_CoverageBlockNeverLeaksIntoBody(t *testing.T) {
	client := newFakeReviewer()
	client.diff = reviewTestDiff
	claude := &mockClaudeInvoker{fn: func(req ReviewRequest) (ReviewResult, error) {
		return ReviewResult{Text: "PRUEFER_SUMMARY_BEGIN\nSummary.\nPRUEFER_SUMMARY_END\n```json\n[]\n```" + coverageReportAll(req)}, nil
	}}
	clone, _ := fakeClone(t, nil)
	pr := gh.PRDetails{Number: 1, Author: "alice", HeadSHA: "sha1"}
	ReviewPR(context.Background(), client, claude, clone, Config{}, "pruefer-bot[bot]", "owner", "repo", pr, nil)
	body := client.submitCalls[0].body
	if body != "Summary.\n\nPRUEFER_COVERAGE: complete" || strings.Contains(body, "opened") {
		t.Errorf("body = %q", body)
	}
}

// TestReviewPR_TranscriptDowngradesForgedClaim: the model claims every entry,
// but the authoritative transcript shows none were read.
func TestReviewPR_TranscriptDowngradesForgedClaim(t *testing.T) {
	client := newFakeReviewer()
	client.diff = reviewTestDiff
	claude := &mockClaudeInvoker{fn: func(req ReviewRequest) (ReviewResult, error) {
		return ReviewResult{Text: "Claims it all." + coverageReportAll(req), Opened: map[string]bool{}, OpenedAuthoritative: true}, nil
	}}
	clone, _ := fakeClone(t, nil)
	pr := gh.PRDetails{Number: 1, Author: "alice", HeadSHA: "sha1"}
	ReviewPR(context.Background(), client, claude, clone, Config{}, "pruefer-bot[bot]", "owner", "repo", pr, nil)
	if body := client.submitCalls[0].body; !strings.Contains(body, "PRUEFER_COVERAGE: partial — 2 of 2 diff entries not opened") {
		t.Errorf("forged claim not downgraded:\n%s", body)
	}
}

// TestDeriveOpened_RealReadResultFormat pins the Read result format against a
// transcript captured from the installed claude (testdata/read_result_real.jsonl:
// a subagent's Read of line 1, paths rewritten to /stage). A fixture built by the
// same assumption as the parser cannot catch a format change; this one can.
func TestDeriveOpened_RealReadResultFormat(t *testing.T) {
	raw, err := os.ReadFile("testdata/read_result_real.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	s := testStaged("a.go")
	s.Entries[0].Lines = 1
	got, _ := deriveOpened(raw, s)
	if !got["0001"] {
		t.Errorf("a real one-line Read result was not parsed as covering the entry: %v", got)
	}
	// The same result must not credit an entry of two lines.
	s.Entries[0].Lines = 2
	if got, _ := deriveOpened(raw, s); got["0001"] {
		t.Errorf("a one-line Read credited a two-line entry: %v", got)
	}
}

func TestReadLineRE_AcceptsArrowSeparator(t *testing.T) {
	for _, l := range []string{"     7→text", "7\ttext", "12\t"} {
		if !readLineRE.MatchString(l) {
			t.Errorf("%q not recognised as a numbered Read line", l)
		}
	}
	if readLineRE.MatchString("no number here") {
		t.Error("matched a line with no number")
	}
}

// --- #2303: transcript retention end to end ------------------------------------

func reviewWithTranscript(t *testing.T, cfg Config, text string, transcript string) (ReviewOutcome, *fakeReviewer, []string) {
	t.Helper()
	logs := captureLogs(t)
	client := newFakeReviewer()
	client.diff = reviewTestDiff
	claude := &mockClaudeInvoker{fn: func(req ReviewRequest) (ReviewResult, error) {
		if text == "" {
			text = "All read." + coverageReportAll(req)
		}
		return ReviewResult{Text: text, Transcript: []byte(transcript)}, nil
	}}
	clone, _ := fakeClone(t, nil)
	pr := gh.PRDetails{Number: 1, Author: "alice", HeadSHA: "sha1"}
	outcome := ReviewPR(context.Background(), client, claude, clone, cfg, "pruefer-bot[bot]", "owner", "repo", pr, nil)
	return outcome, client, logs()
}

func TestReviewPR_PartialCoverageKeepsTranscript(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "transcripts")
	outcome, _, logs := reviewWithTranscript(t, Config{TranscriptDir: dir}, "mock review", `{"type":"result"}`+"\n")
	if !outcome.Reviewed || outcome.Err != nil {
		t.Fatalf("outcome = %+v", outcome)
	}
	files := transcriptFiles(t, dir)
	if len(files) != 1 {
		t.Fatalf("transcripts = %v, want exactly one", files)
	}
	var partialLine string
	for _, l := range logs {
		if strings.Contains(l, "coverage: partial") {
			partialLine = l
		}
	}
	if !strings.Contains(partialLine, filepath.Join(dir, files[0])) {
		t.Errorf("partial warn line %q does not name the saved file", partialLine)
	}
}

func TestReviewPR_CompleteCoverageKeepsNoTranscriptByDefault(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "transcripts")
	outcome, _, _ := reviewWithTranscript(t, Config{TranscriptDir: dir}, "", `{"type":"result"}`)
	if !outcome.Reviewed {
		t.Fatalf("outcome = %+v", outcome)
	}
	if files := transcriptFiles(t, dir); len(files) != 0 {
		t.Errorf("complete review left %v", files)
	}
}

func TestReviewPR_KeepTranscriptsAllAndOff(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "all")
	reviewWithTranscript(t, Config{TranscriptDir: dir, KeepTranscripts: KeepTranscriptsAll}, "", "x")
	if files := transcriptFiles(t, dir); len(files) != 1 {
		t.Errorf("keep_transcripts=all kept %v for a complete review, want one", files)
	}
	dir = filepath.Join(t.TempDir(), "off")
	reviewWithTranscript(t, Config{TranscriptDir: dir, KeepTranscripts: KeepTranscriptsOff}, "mock review", "x")
	if files := transcriptFiles(t, dir); len(files) != 0 {
		t.Errorf("keep_transcripts=off kept %v", files)
	}
}

func TestReviewPR_TranscriptWriteFailureStillSubmits(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	outcome, client, logs := reviewWithTranscript(t, Config{TranscriptDir: filepath.Join(blocker, "t")}, "mock review", "x")
	if !outcome.Reviewed || outcome.Err != nil || len(client.submitCalls) != 1 {
		t.Fatalf("review must still be submitted: %+v", outcome)
	}
	for _, l := range logs {
		if strings.Contains(l, "coverage: partial") && strings.Contains(l, "transcript saved") {
			t.Errorf("warn line claims a saved path after a failed write: %q", l)
		}
	}
}

func TestReviewPR_EmptyTranscriptCreatesNoDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "transcripts")
	reviewWithTranscript(t, Config{TranscriptDir: dir}, "mock review", "")
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("transcript dir created for an empty transcript: %v", err)
	}
}

// #2306: the reviewer now makes a Skill call first. It is not a Read, so it
// credits nothing and must not disturb coverage derivation or the
// subagent-authoritative rule.
func TestCoverage_SkillCallDoesNotDisturbDerivation(t *testing.T) {
	s := testStaged("a.go", "b.go", "c.go", "d.go")
	for i := range s.Entries {
		s.Entries[i].Lines = 100
	}
	build := func(readByB []string) []byte {
		var tr tl
		tr.toolUse("", "sk", "Skill", `{"skill":"pruefer-review:review"}`)
		tr.result("", "sk", false, "loaded")
		return append(tr.bytes(), subagentTranscript(readByB)...)
	}
	opened, auth := deriveOpened(build([]string{"/stage/0003.diff", "/stage/0004.diff"}), s)
	if !auth || len(opened) != 4 {
		t.Fatalf("opened=%v authoritative=%v, want all four credited and authoritative", opened, auth)
	}
	reportAll := []string{"0001", "0002", "0003", "0004"}
	cov := computeCoverage(coverageInput{Staged: s, Reported: reportAll, ReportOK: true, Transcript: opened, TranscriptAuthoritative: auth})
	if !cov.Complete {
		t.Errorf("cov = %+v, want complete", cov)
	}
	// Non-vacuous: a subagent that read less is still caught with the Skill call present.
	opened, auth = deriveOpened(build([]string{"/stage/0003.diff"}), s)
	cov = computeCoverage(coverageInput{Staged: s, Reported: reportAll, ReportOK: true, Transcript: opened, TranscriptAuthoritative: auth})
	if cov.Complete || strings.Join(cov.Unread, ",") != "d.go" {
		t.Errorf("cov = %+v, want partial naming d.go", cov)
	}
}

// bigContent is a staged entry text far larger than a 2 KB preview, so a
// preview can never contain a whole entry and the crediting tests below do not
// depend on the preview-never-credits guard.
func bigContent(id string) string {
	return "big content of " + id + "\n" + strings.Repeat("a line of the diff that is long enough\n", 120)
}

func bigStaged(n int) *StagedDiff {
	paths := make([]string, n)
	for i := range paths {
		paths[i] = fmt.Sprintf("f%d.go", i+1)
	}
	s := testStaged(paths...)
	for i := range s.Entries {
		s.Entries[i].content = bigContent(s.Entries[i].ID)
	}
	return s
}

// persistedNotice is the shape of a Bash result the CLI replaced with a preview
// (seen in the transcripts #2303 keeps; testdata/persisted_output_cat.jsonl is
// built from that shape, not captured from a live run).
const persistedNotice = "<persisted-output>\nOutput too large (62.1KB). Full output saved to: /session/tool-results/x.txt\n\nPreview (first 2KB):\n--- a/x.go\n...\n</persisted-output>"

func TestDeriveOpened_PersistedOutputFixture(t *testing.T) {
	stream, err := os.ReadFile("testdata/persisted_output_cat.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	s := bigStaged(6)
	f := analyzeTranscript(stream, s)
	if len(f.opened) != 0 {
		t.Errorf("a preview credited entries: %v", f.opened)
	}
	for _, id := range []string{"0001", "0002", "0003", "0004", "0005"} {
		if !f.truncated[id] {
			t.Errorf("entry %s not attributed to the truncated result", id)
		}
	}
	if f.truncated["0006"] || len(f.truncated) != 5 {
		t.Errorf("truncated = %v, want exactly 0001-0005", f.truncated)
	}
	opened, auth := deriveOpened(stream, s)
	if len(opened) != 0 || !auth {
		t.Errorf("deriveOpened = %v, %v", opened, auth)
	}
	cov := computeCoverage(coverageInput{
		Staged: s, Reported: []string{"0001", "0002", "0003", "0004", "0005", "0006"}, ReportOK: true,
		Transcript: f.opened, TranscriptAuthoritative: f.authoritative, Truncated: f.truncated,
	})
	if cov.Complete || cov.UnreadCount != 6 || cov.TruncatedUnread != 5 {
		t.Errorf("coverage = %+v, want 6 unread, 5 truncated", cov)
	}
}

func TestAnalyzeTranscript_Truncation(t *testing.T) {
	s := bigStaged(3)
	t.Run("a later proper read credits and uncounts the entry", func(t *testing.T) {
		var x tl
		x.toolUse("", "b1", "Bash", `{"command":"cat /stage/0001.diff /stage/0002.diff"}`)
		x.result("", "b1", false, persistedNotice)
		x.toolUse("", "b2", "Bash", `{"command":"cat /stage/0001.diff"}`)
		x.result("", "b2", false, strings.TrimRight(bigContent("0001"), "\n"))
		f := analyzeTranscript(x.bytes(), s)
		if !f.opened["0001"] || f.opened["0002"] {
			t.Errorf("opened = %v", f.opened)
		}
		cov := computeCoverage(coverageInput{Staged: s, Reported: []string{"0001", "0002", "0003"}, ReportOK: true,
			Transcript: f.opened, TranscriptAuthoritative: f.authoritative, Truncated: f.truncated})
		if cov.TruncatedUnread != 1 {
			t.Errorf("TruncatedUnread = %d, want 1 (0002 only)", cov.TruncatedUnread)
		}
	})
	t.Run("a result that only mentions the marker is not a preview", func(t *testing.T) {
		var x tl
		x.toolUse("", "b1", "Bash", `{"command":"cat /stage/0001.diff"}`)
		x.result("", "b1", false, "diff text mentioning <persisted-output> in a comment")
		if f := analyzeTranscript(x.bytes(), s); len(f.truncated) != 0 {
			t.Errorf("truncated = %v", f.truncated)
		}
	})
	t.Run("an error result is not a preview", func(t *testing.T) {
		var x tl
		x.toolUse("", "b1", "Bash", `{"command":"cat /stage/0001.diff"}`)
		x.result("", "b1", true, persistedNotice)
		if f := analyzeTranscript(x.bytes(), s); len(f.truncated) != 0 {
			t.Errorf("truncated = %v", f.truncated)
		}
	})
	t.Run("a preview for a command naming no staged entry is not counted", func(t *testing.T) {
		var x tl
		x.toolUse("", "b1", "Bash", `{"command":"cat /repo/README.md"}`)
		x.result("", "b1", false, persistedNotice)
		if f := analyzeTranscript(x.bytes(), s); len(f.truncated) != 0 {
			t.Errorf("truncated = %v", f.truncated)
		}
	})
	t.Run("mixed command attributes only the staged entries", func(t *testing.T) {
		var x tl
		x.toolUse("", "b1", "Bash", `{"command":"cat /stage/0002.diff /repo/README.md"}`)
		x.result("", "b1", false, persistedNotice)
		f := analyzeTranscript(x.bytes(), s)
		if len(f.truncated) != 1 || !f.truncated["0002"] {
			t.Errorf("truncated = %v", f.truncated)
		}
	})
	t.Run("a subagent's preview is counted", func(t *testing.T) {
		var x tl
		x.toolUse("", "task1", "Task", `{}`)
		x.toolUse("task1", "b1", "Bash", `{"command":"cat /stage/0003.diff"}`)
		x.result("task1", "b1", false, persistedNotice)
		f := analyzeTranscript(x.bytes(), s)
		if !f.truncated["0003"] || !f.authoritative {
			t.Errorf("facts = %+v", f)
		}
	})
	t.Run("a preview never credits even an entry that fits inside it", func(t *testing.T) {
		small := testStaged("a.go")
		var x tl
		x.toolUse("", "b1", "Bash", `{"command":"cat /stage/0001.diff"}`)
		x.result("", "b1", false, persistedNotice+"\n"+strings.TrimRight(small.Entries[0].content, "\n"))
		f := analyzeTranscript(x.bytes(), small)
		if f.opened["0001"] || !f.truncated["0001"] {
			t.Errorf("facts = %+v", f)
		}
	})
}

func TestLogCoverage_TruncatedCount(t *testing.T) {
	logs := captureLogs(t)
	base := Coverage{Reason: "2 of 3 diff entries not opened", Unread: []string{"a", "b"}, UnreadCount: 2, TotalEntries: 3}
	logCoverage(7, base, true, "")
	withTrunc := base
	withTrunc.TruncatedUnread = 2
	saved := filepath.Join(t.TempDir(), "t.jsonl")
	logCoverage(7, withTrunc, true, saved)
	got := logs()
	if len(got) != 2 {
		t.Fatalf("logs = %q", got)
	}
	if strings.Contains(got[0], "preview") {
		t.Errorf("zero count must not change the line: %q", got[0])
	}
	want := "(2 unread path(s); 2 of the unread entries were in shell results cut to a preview; transcript authoritative=true); transcript saved: " + saved
	if !strings.Contains(got[1], want) {
		t.Errorf("line = %q, want it to contain %q", got[1], want)
	}
}
