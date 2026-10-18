package pruefer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

// #2363: the end-of-review session removal at every cap, the encoded-cwd
// directory removal under the R1b checks, and the hidden tool-results staging.

const (
	cwdPR7   = "-private-var-folders-xx-T-pruefer-pr-7-123456"
	cwdPR70  = "-private-var-folders-xx-T-pruefer-pr-70-654321"
	cwdOther = "-Users-someone-dev-project"
)

func seedToolResult(t *testing.T, sessionDir, name, body string) string {
	t.Helper()
	d := filepath.Join(sessionDir, "tool-results")
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(d, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRemoveReviewSessions_RemovesTheEmptiedReviewDirOnly(t *testing.T) {
	profile := t.TempDir()
	jsonl, dir := seedSession(t, profile, cwdPR7, sessA)
	// Another review of a different PR and an unrelated project are untouched,
	// even though each shares the projects/ parent.
	otherJSONL, _ := seedSession(t, profile, cwdPR70, sessB)
	foreignJSONL, _ := seedSession(t, profile, cwdOther, sessC)

	n, err := removeReviewSessions(profile, []string{sessA}, removeSessionsOpts{PRNumber: 7})
	if err != nil || n != 2 {
		t.Fatalf("n = %d err = %v, want 2 removals", n, err)
	}
	for _, p := range []string{jsonl, dir, filepath.Join(profile, "projects", cwdPR7)} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s survived: %v", p, err)
		}
	}
	for _, p := range []string{otherJSONL, foreignJSONL, filepath.Dir(otherJSONL), filepath.Dir(foreignJSONL)} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s was touched: %v", p, err)
		}
	}
}

func TestRemoveReviewSessions_PRNumberPrefixDoesNotMatch(t *testing.T) {
	// PR 7 must not remove the (empty) directory of PR 70, nor may PR 70's
	// directory be taken for "pruefer-pr-7-".
	profile := t.TempDir()
	_, _ = seedSession(t, profile, cwdPR70, sessA)
	if _, err := removeReviewSessions(profile, []string{sessA}, removeSessionsOpts{PRNumber: 7}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(profile, "projects", cwdPR70)); err != nil {
		t.Errorf("PR 7 removed PR 70's directory: %v", err)
	}
}

func TestRemoveReviewSessions_LeavesNonEmptyAndUnnamedDirs(t *testing.T) {
	profile := t.TempDir()
	// Non-empty: a sibling session of the same cwd stays, so does the directory.
	_, _ = seedSession(t, profile, cwdPR7, sessA)
	siblingJSONL, _ := seedSession(t, profile, cwdPR7, sessB)
	// Empty, but the cwd is not this review's.
	_, _ = seedSession(t, profile, cwdOther, sessC)

	if _, err := removeReviewSessions(profile, []string{sessA, sessC}, removeSessionsOpts{PRNumber: 7}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(siblingJSONL); err != nil {
		t.Errorf("a non-empty review directory lost a file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(profile, "projects", cwdOther)); err != nil {
		t.Errorf("a directory not named for this PR was removed: %v", err)
	}
	// Without a PR number nothing is removed either.
	_, _ = seedSession(t, profile, "-x-pruefer-pr-0-1", sessD)
	if _, err := removeReviewSessions(profile, []string{sessD}, removeSessionsOpts{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(profile, "projects", "-x-pruefer-pr-0-1")); err != nil {
		t.Errorf("a directory was removed without a PR number: %v", err)
	}
}

func TestRemoveReviewSessions_StagesToolResultsThenDeletes(t *testing.T) {
	profile := t.TempDir()
	_, dir := seedSession(t, profile, cwdPR7, sessA)
	seedToolResult(t, dir, "toolu_01.txt", "full output")
	seedToolResult(t, dir, "../evil name.txt", "x") // base name only: stays inside tool-results
	// A symlink in tool-results must not be followed out of the session.
	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(dir, "tool-results", "link.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	stage := t.TempDir()
	if _, err := removeReviewSessions(profile, []string{sessA}, removeSessionsOpts{PRNumber: 7, ToolResultsDest: stage}); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(stage, "toolu_01.txt")); err != nil || string(got) != "full output" {
		t.Errorf("tool result not staged: %q %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(stage, "link.txt")); !os.IsNotExist(err) {
		t.Errorf("a symlink was staged: %v", err)
	}
	if _, err := os.Stat(secret); err != nil {
		t.Errorf("the symlink target was touched: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the session folder survived: %v", err)
	}
}

func TestRemoveReviewSessions_NoStageDestDeletesToolResultsWithTheFolder(t *testing.T) {
	profile := t.TempDir()
	_, dir := seedSession(t, profile, cwdPR7, sessA)
	p := seedToolResult(t, dir, "a.txt", "x")
	if _, err := removeReviewSessions(profile, []string{sessA}, removeSessionsOpts{PRNumber: 7}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("tool result survived: %v", err)
	}
}

// A staging failure is reported but the session folder is still deleted.
func TestRemoveReviewSessions_StagingFailureStillDeletes(t *testing.T) {
	profile := t.TempDir()
	_, dir := seedSession(t, profile, cwdPR7, sessA)
	seedToolResult(t, dir, "a.txt", "x")
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := removeReviewSessions(profile, []string{sessA}, removeSessionsOpts{PRNumber: 7, ToolResultsDest: filepath.Join(blocker, "sub")})
	if err == nil {
		t.Error("expected the staging failure to be reported")
	}
	if _, serr := os.Stat(dir); !os.IsNotExist(serr) {
		t.Errorf("the session folder survived a staging failure: %v", serr)
	}
}

func TestReview_SessionRemovedAtEveryCapAndSurvivesBetweenInvocations(t *testing.T) {
	for _, capPasses := range []int{0, 1, 2} {
		profile := t.TempDir()
		jsonl, dir := seedSession(t, profile, cwdPR7, sessA)
		bed := newLoopBed(t, 4,
			loopPass{skill: "pruefer-review:review", reads: []string{"0001", "0002"}, text: passText("first", "0001", "0002"), session: sessA},
			loopPass{reads: []string{"0003", "0004"}, text: passText("second", "0003", "0004"), session: sessA},
		)
		req := bed.req(capPasses)
		req.ClaudeConfigDir = profile
		bed.review(req)

		// --no-session-persistence is never passed, at any cap.
		for i := 1; i <= bed.calls(); i++ {
			if strings.Contains(bed.read("args", i), "no-session-persistence") {
				t.Errorf("cap %d call %d passed --no-session-persistence", capPasses, i)
			}
		}
		// With the loop on there is a second invocation, and the session was
		// still in the profile when it ran.
		if capPasses > 0 {
			if bed.calls() != 2 {
				t.Fatalf("cap %d: claude ran %d times, want 2", capPasses, bed.calls())
			}
			if !strings.Contains(bed.read("ls", 2), sessA) {
				t.Errorf("cap %d: the session was gone before the resume: %q", capPasses, bed.read("ls", 2))
			}
		}
		// And it is gone, with its emptied directory, once the review ended.
		for _, p := range []string{jsonl, dir, filepath.Join(profile, "projects", cwdPR7)} {
			if _, err := os.Stat(p); !os.IsNotExist(err) {
				t.Errorf("cap %d: %s survived the review: %v", capPasses, p, err)
			}
		}
	}
}

func TestReview_SessionToolResultsReachTheStageDir(t *testing.T) {
	profile := t.TempDir()
	_, dir := seedSession(t, profile, cwdPR7, sessA)
	seedToolResult(t, dir, "toolu_01.txt", "hidden")
	bed := newLoopBed(t, 1, loopPass{skill: "pruefer-review:review", reads: []string{"0001"}, text: passText("x", "0001"), session: sessA})
	req := bed.req(0)
	req.ClaudeConfigDir = profile
	req.ToolResultsStageDir = t.TempDir()
	if _, err := (&RealClaudeInvoker{}).Review(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(req.ToolResultsStageDir, "toolu_01.txt")); err != nil || string(got) != "hidden" {
		t.Errorf("staged tool result = %q, %v", got, err)
	}
}

// ReviewPR hands the invoker a stage dir, and what the invoker leaves there
// ends up beside the kept transcript (all, and partial + partial coverage) but
// nowhere under off or partial + complete coverage; the stage dir is gone after.
func TestReviewPR_ToolResultsLandBesideTheTranscript(t *testing.T) {
	cases := []struct {
		name     string
		mode     string
		text     string // "" = complete coverage
		wantMove bool
		wantDir  bool // a stage dir is created at all
	}{
		{"default keeps", "", "", true, true},
		{"all keeps", KeepTranscriptsAll, "", true, true},
		{"partial with partial coverage", KeepTranscriptsPartial, "mock review", true, true},
		{"partial with complete coverage", KeepTranscriptsPartial, "", false, true},
		{"off", KeepTranscriptsOff, "mock review", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "transcripts")
			var stage string
			client := newFakeReviewer()
			client.diff = reviewTestDiff
			claude := &mockClaudeInvoker{fn: func(req ReviewRequest) (ReviewResult, error) {
				stage = req.ToolResultsStageDir
				if stage != "" {
					writeSized(t, filepath.Join(stage, "toolu_01.txt"), 4)
				}
				text := c.text
				if text == "" {
					text = "All read." + coverageReportAll(req)
				}
				return ReviewResult{Text: text, Transcript: []byte("x")}, nil
			}}
			clone, _ := fakeClone(t, nil)
			outcome := ReviewPR(context.Background(), client, claude, clone, Config{TranscriptDir: dir, KeepTranscripts: c.mode}, "pruefer-bot[bot]", "owner", "repo", gh.PRDetails{Number: 1, Author: "alice", HeadSHA: "sha1"}, nil)
			if !outcome.Reviewed || outcome.Err != nil {
				t.Fatalf("outcome = %+v", outcome)
			}
			if (stage != "") != c.wantDir {
				t.Fatalf("stage dir = %q, want one: %v", stage, c.wantDir)
			}
			if stage != "" && exists(stage) {
				t.Error("stage dir was not removed after the review")
			}
			moved := false
			for _, n := range transcriptFiles(t, dir) {
				if strings.HasSuffix(n, ".tool-results") && exists(filepath.Join(dir, n, "toolu_01.txt")) {
					moved = true
				}
			}
			if moved != c.wantMove {
				t.Errorf("tool results moved = %v, want %v (%v)", moved, c.wantMove, transcriptFiles(t, dir))
			}
		})
	}
}
