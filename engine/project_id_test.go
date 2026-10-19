package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

func captureProjectIDLogs(e *Engine) (*[]string, *sync.Mutex) {
	var mu sync.Mutex
	var lines []string
	e.SetLogSinkForTest(func(_ int, _ string, msg string) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, msg)
	})
	return &lines, &mu
}

func TestWriteProjectID_BoardIDWins(t *testing.T) {
	e := &Engine{boardProjectID: "ENGINE"}
	id, err := e.writeProjectID(&gh.ProjectBoard{ProjectID: "BOARD"})
	if err != nil || id != "BOARD" {
		t.Fatalf("got %q, %v", id, err)
	}
}

func TestWriteProjectID_FallsBackToEngineID(t *testing.T) {
	e := &Engine{boardProjectID: "ENGINE"}
	id, err := e.writeProjectID(&gh.ProjectBoard{})
	if err != nil || id != "ENGINE" {
		t.Fatalf("got %q, %v", id, err)
	}
}

func TestWriteProjectID_BothEmptyIsNamedErrorLoggedOncePerEpisode(t *testing.T) {
	e := &Engine{}
	lines, mu := captureProjectIDLogs(e)
	for i := 0; i < 3; i++ {
		id, err := e.writeProjectID(&gh.ProjectBoard{})
		if id != "" || !errors.Is(err, errProjectIDUnknown) {
			t.Fatalf("attempt %d: got %q, %v", i, id, err)
		}
	}
	if !strings.Contains(errProjectIDUnknown.Error(), "project ID unknown") {
		t.Fatalf("error text %q", errProjectIDUnknown)
	}
	mu.Lock()
	if n := len(*lines); n != 1 {
		t.Fatalf("logged %d lines, want 1: %v", n, *lines)
	}
	mu.Unlock()

	// Recovery ends the episode; the next unknown period logs again.
	e.mu.Lock()
	e.boardProjectID = "ENGINE"
	e.mu.Unlock()
	if _, err := e.writeProjectID(&gh.ProjectBoard{}); err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	e.boardProjectID = ""
	e.mu.Unlock()
	if _, err := e.writeProjectID(&gh.ProjectBoard{}); !errors.Is(err, errProjectIDUnknown) {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if n := len(*lines); n != 2 {
		t.Fatalf("logged %d lines after second episode, want 2: %v", n, *lines)
	}
}

func TestWriteProjectID_GuardNeutralised(t *testing.T) {
	e := &Engine{boardProjectID: "ENGINE"}
	e.SetProjectIDWriteGuardDisabledForTest(true)
	id, err := e.writeProjectID(&gh.ProjectBoard{})
	if err != nil || id != "" {
		t.Fatalf("neutralised guard should pass the empty ID through, got %q, %v", id, err)
	}
}

func TestAdvanceToNextStage_EmptyBoardIDUsesEngineID(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	eng.statusField = &gh.StatusField{FieldID: "F", Options: map[string]string{"Research": "OPT_R", "Plan": "OPT_P"}}
	eng.boardProjectID = "ENGINE_PID"
	item := gh.ProjectItem{Number: 1, ItemID: "ITEM", Repo: "owner/repo"}
	cur := &stages.Stage{Name: "Research"}
	if err := eng.advanceToNextStage(&gh.ProjectBoard{}, item, cur); err != nil {
		t.Fatal(err)
	}
	if len(client.updateStatusCalls) != 1 || client.updateStatusCalls[0].projectID != "ENGINE_PID" {
		t.Fatalf("calls = %+v", client.updateStatusCalls)
	}
}

func TestAdvanceToNextStage_BothEmptySendsNothingAndIsNotRetryCounted(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	eng.statusField = &gh.StatusField{FieldID: "F", Options: map[string]string{"Research": "OPT_R", "Plan": "OPT_P"}}
	item := gh.ProjectItem{Number: 1, ItemID: "ITEM", Repo: "owner/repo"}
	cur := &stages.Stage{Name: "Research"}
	err := eng.recordAdvanceOutcome(&gh.ProjectBoard{}, item, cur)
	if !errors.Is(err, errProjectIDUnknown) {
		t.Fatalf("err = %v", err)
	}
	if len(client.updateStatusCalls) != 0 {
		t.Fatalf("a write reached GitHub: %+v", client.updateStatusCalls)
	}
	if len(client.addLabelCalls) != 0 {
		t.Fatalf("a label/marker was written: %+v", client.addLabelCalls)
	}
}

// TestBoardWritesTakeProjectIDFromTheGuard keeps a future write site from
// reading board.ProjectID directly (#2396): every project-ID-taking board
// mutation in non-test engine code must be fed by writeProjectID.
func TestBoardWritesTakeProjectIDFromTheGuard(t *testing.T) {
	mutation := regexp.MustCompile(`\.(UpdateProjectItemStatus|ArchiveProjectItem|AddProjectV2ItemById|LookupIssueProjectItem)\(\s*(board|b)\.ProjectID`)
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if loc := mutation.FindIndex(src); loc != nil {
			line := strings.Count(string(src[:loc[0]]), "\n") + 1
			t.Errorf("%s:%d passes board.ProjectID to a board mutation; resolve it with Engine.writeProjectID", f, line)
		}
	}
}

// A Review/Validate stage may declare a mid-flight spawn, which needs a project
// ID. With none known the dispatch is deferred before Claude runs, so the
// stage's output is never generated and then dropped (#2396).
func TestProcessItem_ReviewDefersBeforeClaudeWhenProjectIDUnknown(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	invoked := 0
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			invoked++
			return "", false, TokenUsage{}, nil
		},
	}
	client := &mockGitHubClient{}
	eng := NewWithDeps(
		Config{Owner: "owner", Repo: "repo", ProjectNum: 1, User: "testuser", Token: "token",
			MaxRetries: 2, Stages: []*stages.Stage{
				{Name: "Review", Order: 1, Prompt: "Review it", PostToPR: true, Completion: stages.CompletionCriteria{Type: "claude"}},
			}},
		client, claude, wm,
	)

	item := gh.ProjectItem{Number: 301, Title: "review defer", Status: "Review", ItemID: "PVTI_301"}
	if err := eng.processItem(context.Background(), &gh.ProjectBoard{}, item); err != nil {
		t.Fatalf("processItem: %v", err)
	}
	if invoked != 0 {
		t.Fatalf("Claude was invoked %d time(s) with no project ID known; its output could be dropped", invoked)
	}

	// Non-vacuity: with a project ID the same dispatch does invoke Claude.
	eng.boardProjectID = "ENGINE_PID"
	item.Number = 302
	item.ItemID = "PVTI_302"
	if err := eng.processItem(context.Background(), &gh.ProjectBoard{}, item); err != nil {
		t.Fatalf("processItem: %v", err)
	}
	if invoked == 0 {
		t.Fatal("Claude was not invoked even with a known project ID: the test would pass vacuously")
	}
}
