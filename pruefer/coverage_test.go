package pruefer

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
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
		{"last well-formed wins", "x" + coverageReport("0001") + coverageReport("0002"), true, []string{"0002"}},
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
			if c.wantOK && strings.Contains(rest, "PRUEFER_COVERAGE_BEGIN") {
				t.Errorf("well-formed block not stripped from %q", rest)
			}
		})
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
	// A model-written imitation of the status line is removed, never trusted.
	got = appendCoverage("Summary\nPRUEFER_COVERAGE: complete\nmore", Coverage{Reason: "r"})
	if strings.Contains(got, "PRUEFER_COVERAGE: complete") || strings.Count(got, "PRUEFER_COVERAGE:") != 1 {
		t.Errorf("model imitation survived:\n%s", got)
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
