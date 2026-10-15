package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// ungrantableTestWorkDir is the worktree the captured stream was recorded in,
// read from the stream's own init event so the fixture and the tests cannot
// disagree about it.
// It is resolved lazily: this test binary is also re-executed as a helper from
// other working directories, where an init-time read of testdata would panic.
var ungrantableTestWorkDir = sync.OnceValue(capturedWorkDir)

func capturedWorkDir() string {
	raw, err := os.ReadFile("testdata/protected_path_edit_denied_stream.jsonl")
	if err != nil {
		panic(err)
	}
	line, _, _ := strings.Cut(string(raw), "\n")
	var ev struct {
		Cwd string `json:"cwd"`
	}
	if err := json.Unmarshal([]byte(line), &ev); err != nil || ev.Cwd == "" {
		panic("protected_path_edit_denied_stream.jsonl: first line is not an init event with a cwd")
	}
	return ev.Cwd
}

func TestDecodeToolPath(t *testing.T) {
	cases := []struct {
		name  string
		tool  string
		input string
		want  string
	}{
		{"Edit file_path", "Edit", `{"file_path":"/w/a.go","old_string":"x"}`, "/w/a.go"},
		{"Write file_path", "Write", `{"file_path":"/w/a.go","content":"x"}`, "/w/a.go"},
		{"MultiEdit file_path", "MultiEdit", `{"file_path":"/w/a.go","edits":[]}`, "/w/a.go"},
		{"NotebookEdit notebook_path", "NotebookEdit", `{"notebook_path":"/w/n.ipynb"}`, "/w/n.ipynb"},
		{"NotebookEdit ignores file_path", "NotebookEdit", `{"file_path":"/w/a.go"}`, ""},
		{"Edit ignores notebook_path", "Edit", `{"notebook_path":"/w/n.ipynb"}`, ""},
		{"Bash is not a file tool", "Bash", `{"file_path":"/w/a.go"}`, ""},
		{"empty input", "Edit", ``, ""},
		{"empty object (redacted capture)", "Edit", `{}`, ""},
		{"malformed", "Edit", `{"file_path":`, ""},
		{"non-string", "Edit", `{"file_path":42}`, ""},
		{"null", "Edit", `null`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := decodeToolPath(tc.tool, json.RawMessage(tc.input)); got != tc.want {
				t.Errorf("decodeToolPath(%q, %s) = %q, want %q", tc.tool, tc.input, got, tc.want)
			}
		})
	}
}

func TestClassifyToolsDenied_DecodesFilePath(t *testing.T) {
	resp := claudeResponse{PermissionDenials: []permissionDenial{
		{ToolName: "Edit", ToolInput: json.RawMessage(`{"file_path":"/w/.claude/x"}`)},
		{ToolName: "Bash", ToolInput: json.RawMessage(`{"command":"git push"}`)},
	}}
	_, denials, ok := classifyToolsDenied(resp)
	if !ok || len(denials) != 2 {
		t.Fatalf("classifyToolsDenied = %+v, %v", denials, ok)
	}
	if denials[0].Path != "/w/.claude/x" || denials[0].Command != "" {
		t.Errorf("Edit denial = %+v", denials[0])
	}
	if denials[1].Path != "" || denials[1].Command != "git push" {
		t.Errorf("Bash denial = %+v", denials[1])
	}
}

func TestToolsDeniedLogSummary_ShowsFirstPath(t *testing.T) {
	got := toolsDeniedLogSummary([]string{"Edit"}, []toolDenial{{ToolName: "Edit", Path: "/w/.claude/skills/x/SKILL.md"}})
	if !strings.Contains(got, "first: `/w/.claude/skills/x/SKILL.md`") {
		t.Errorf("summary = %q, want the first denied path", got)
	}
	if got := toolsDeniedLogSummary([]string{"Edit"}, []toolDenial{{ToolName: "Edit"}}); strings.Contains(got, "first:") {
		t.Errorf("summary = %q, must not invent a path", got)
	}
}

func TestUngrantableDenial_Predicate(t *testing.T) {
	w := ungrantableTestWorkDir()
	edit := func(p string) toolDenial { return toolDenial{ToolName: "Edit", Path: p} }
	cases := []struct {
		name     string
		d        toolDenial
		rules    []string
		skip     bool
		want     bool
		wantRel  string
		wantRule string
	}{
		{"covered by scoped rule", edit(w + "/.claude/skills/x/SKILL.md"), []string{"Read", "Edit(" + w + "/**)"}, false, true, ".claude/skills/x/SKILL.md", "Edit(" + w + "/**)"},
		{"covered by bare name", edit(w + "/a.go"), []string{"Edit"}, false, true, "a.go", "Edit"},
		{"MultiEdit covered by bare name", toolDenial{ToolName: "MultiEdit", Path: w + "/a.go"}, []string{"MultiEdit"}, false, true, "a.go", "MultiEdit"},
		{"NotebookEdit scoped", toolDenial{ToolName: "NotebookEdit", Path: w + "/n.ipynb"}, []string{"NotebookEdit(" + w + "/**)"}, false, true, "n.ipynb", "NotebookEdit(" + w + "/**)"},
		{"outside the worktree", edit("/etc/hosts"), []string{"Edit(" + w + "/**)"}, false, false, "", ""},
		{"dotdot escape", edit(w + "/../other/a.go"), []string{"Edit(" + w + "/**)"}, false, false, "", ""},
		{"sibling prefix directory", edit(w + "-x/a.go"), []string{"Edit(" + w + "/**)"}, false, false, "", ""},
		{"the worktree root itself", edit(w), []string{"Edit(" + w + "/**)"}, false, false, "", ""},
		{"relative path", edit(".claude/x"), []string{"Edit(" + w + "/**)"}, false, false, "", ""},
		{"undecodable path", edit(""), []string{"Edit(" + w + "/**)"}, false, false, "", ""},
		{"tool absent from a custom list", edit(w + "/a.go"), []string{"Read", "Grep"}, false, false, "", ""},
		{"other rule form is not covered", edit(w + "/a.go"), []string{"Edit(src/**)", "Edit(*)"}, false, false, "", ""},
		{"rule for a different tool", edit(w + "/a.go"), []string{"Write(" + w + "/**)"}, false, false, "", ""},
		{"unrestricted invocation", edit(w + "/a.go"), []string{"Edit"}, true, false, "", ""},
		{"Bash denial", toolDenial{ToolName: "Bash", Command: "git push"}, []string{"Bash(git:*)"}, false, false, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rel, rule, ok := ungrantableDenial(tc.d, tc.rules, w, tc.skip)
			if ok != tc.want || rel != tc.wantRel || rule != tc.wantRule {
				t.Errorf("ungrantableDenial = (%q, %q, %v), want (%q, %q, %v)", rel, rule, ok, tc.wantRel, tc.wantRule, tc.want)
			}
		})
	}
}

// TestUngrantableDenial_RealCapturedStream replays the stream captured from
// the real CLI (2.1.299, dontAsk, Fabrik's own flags) in which an Edit on
// <worktree>/.claude/skills/x/SKILL.md was denied, and checks the verdict
// against args built by the real buildClaudeArgs (the drift guard).
func TestUngrantableDenial_RealCapturedStream(t *testing.T) {
	raw, err := os.ReadFile("testdata/protected_path_edit_denied_stream.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	interpret := func() *claudeToolsDeniedError {
		var te *claudeToolsDeniedError
		if !errors.As(interpretDenialStream(t, raw, -1), &te) {
			t.Fatal("captured protected-path Edit denial must be classified denial-caused")
		}
		return te
	}

	te := interpret()
	if len(te.Denials) != 1 || te.Denials[0].ToolName != "Edit" || te.Denials[0].Path != ungrantableTestWorkDir()+"/.claude/skills/x/SKILL.md" {
		t.Fatalf("decoded denials = %+v", te.Denials)
	}

	implement := &stages.Stage{Name: "Implement"}
	markUngrantableDenials(te, buildClaudeArgs(implement, "", "", 0, false, ungrantableTestWorkDir(), ""), ungrantableTestWorkDir())
	d := te.Denials[0]
	if !d.Ungrantable || d.RelPath != ".claude/skills/x/SKILL.md" || d.CoveredBy != "Edit("+ungrantableTestWorkDir()+"/**)" {
		t.Errorf("default stage: denial = %+v, want ungrantable and covered by the boundary rule", d)
	}

	te = interpret()
	markUngrantableDenials(te, buildClaudeArgs(implement, "", "", 0, true, ungrantableTestWorkDir(), ""), ungrantableTestWorkDir())
	if te.Denials[0].Ungrantable {
		t.Error("unrestricted invocation must never be ungrantable")
	}

	te = interpret()
	readOnly := &stages.Stage{Name: "Research", ReadOnly: true, AllowedTools: []string{"Read", "Grep"}}
	markUngrantableDenials(te, buildClaudeArgs(readOnly, "", "", 0, false, ungrantableTestWorkDir(), ""), ungrantableTestWorkDir())
	if te.Denials[0].Ungrantable {
		t.Error("a tool absent from the stage's custom allowed_tools must not be ungrantable")
	}

	te = interpret()
	bare := &stages.Stage{Name: "Plan", ReadOnly: true, AllowedTools: []string{"Read", "Edit"}}
	markUngrantableDenials(te, buildClaudeArgs(bare, "", "", 0, false, ungrantableTestWorkDir(), ""), ungrantableTestWorkDir())
	if !te.Denials[0].Ungrantable || te.Denials[0].CoveredBy != "Edit" {
		t.Errorf("bare Edit in a read-only stage covers the path, got %+v", te.Denials[0])
	}
}

func TestProtectedPathDoc(t *testing.T) {
	for rel, want := range map[string]bool{
		".claude/skills/x/SKILL.md": true,
		".git/hooks/pre-commit":     true,
		".vscode/settings.json":     true,
		".config/git/config":        true,
		".gitmodules":               true,
		".claude/worktrees/w/a.go":  false,
		"src/.claude/a":             false,
		"docs/readme.md":            false,
		".claudex/a":                false,
	} {
		if got := protectedPathDoc(rel); got != want {
			t.Errorf("protectedPathDoc(%q) = %v, want %v", rel, got, want)
		}
	}
}

func TestDenialCausal_MultiEditIsWriteCapable(t *testing.T) {
	raw := buildDenialStream([]streamStep{{id: "m1", tool: "MultiEdit", isError: true, denied: true}})
	if !isToolsDenied(interpretDenialStream(t, raw, -1)) {
		t.Error("a lone denied MultiEdit must be denial-caused")
	}
	raw = buildDenialStream([]streamStep{
		{id: "m1", tool: "MultiEdit", isError: true, denied: true},
		{id: "m2", tool: "MultiEdit"},
	})
	if isToolsDenied(interpretDenialStream(t, raw, -1)) {
		t.Error("a later successful MultiEdit is recovery")
	}
}

// ungrantableEditDenial is what runClaude hands the consumers for a denied Edit
// on .claude/skills/x/SKILL.md.
func ungrantableEditDenial() *claudeToolsDeniedError {
	return &claudeToolsDeniedError{
		ToolNames: []string{"Edit"},
		Denials: []toolDenial{{
			ToolName: "Edit", Path: ungrantableTestWorkDir() + "/.claude/skills/x/SKILL.md",
			Ungrantable: true, RelPath: ".claude/skills/x/SKILL.md", CoveredBy: "Edit(" + ungrantableTestWorkDir() + "/**)",
		}},
	}
}

func labelApplied(client *mockGitHubClient, name string) bool {
	for _, c := range client.addLabelCalls {
		if c.labelName == name {
			return true
		}
	}
	return false
}

func assertUngrantablePauseComment(t *testing.T, client *mockGitHubClient, issue int) {
	t.Helper()
	var body string
	for _, c := range client.addCommentCalls {
		if strings.Contains(c.body, "cannot be granted") {
			body = c.body
		}
		if strings.Contains(c.body, "tool permission denied**") || strings.Contains(c.body, "denial limit reached") {
			t.Errorf("only the ungrantable pause comment is expected, also got: %s", c.body)
		}
	}
	if body == "" {
		t.Fatalf("no ungrantable pause comment posted; comments: %+v", client.addCommentCalls)
	}
	for _, want := range []string{
		"**Edit** `.claude/skills/x/SKILL.md`",
		"No `allowed_tools` rule can change that",
		"1. Make the change by hand on the `fabrik/issue-",
		"2. Restructure the work",
		"3. Add the `fabrik:unrestricted` label",
		"removes all tool restrictions",
		"permission-modes",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("pause comment missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, ungrantableTestWorkDir()) {
		t.Errorf("pause comment must name the path relative to the worktree:\n%s", body)
	}
}

func TestUngrantableDenial_StagePath_PausesOnFirstOccurrence(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		name := "enabled"
		if disabled {
			name = "classification disabled (non-vacuity)"
		}
		t.Run(name, func(t *testing.T) {
			skipIfNoGit(t)
			repoDir := initBareRepo(t)
			client := &mockGitHubClient{}
			invocations := 0
			claude := &mockClaudeInvoker{
				invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
					invocations++
					return "", false, TokenUsage{TurnsUsed: 3}, ungrantableEditDenial()
				},
			}
			eng := NewWithDeps(
				Config{Owner: "owner", Repo: "repo", ProjectNum: 1, User: "testuser", Token: "token",
					MaxRetries: 2, MaxToolsDeniedRetries: 3, Stages: testStages()},
				client, claude, NewWorktreeManager(repoDir),
			)
			eng.SetUngrantableDenialDisabledForTest(disabled)

			board := &gh.ProjectBoard{ProjectID: "PVT_1"}
			item := gh.ProjectItem{Number: 401, Title: "ungrantable", Status: "Research", ItemID: "PVTI_401"}
			if err := eng.processItem(context.Background(), board, item); err != nil {
				t.Fatalf("processItem: %v", err)
			}

			paused := labelApplied(client, "fabrik:paused") && labelApplied(client, "fabrik:awaiting-input")
			if disabled {
				if paused {
					t.Error("with the classification disabled the first denial must stay on the bounded-retry path")
				}
				return
			}
			if invocations != 1 || !paused {
				t.Fatalf("invocations=%d paused=%v, want 1 invocation and a pause", invocations, paused)
			}
			if labelApplied(client, "stage:Research:failed") {
				t.Error("an ungrantable pause is never a stage failure")
			}
			if labelApplied(client, "fabrik:tools-denied") {
				t.Error("fabrik:tools-denied ('the engine retries') must not be applied to a paused item")
			}
			snap, err := eng.store.Get("owner/repo", 401)
			if err != nil {
				t.Fatal(err)
			}
			if got := snap.Attempts("Research"); got != 0 {
				t.Errorf("Attempts = %d, want 0 (no max_retries charge)", got)
			}
			if !snap.PausedByEngine("Research") {
				t.Error("EnginePaused must be recorded so removing fabrik:paused resumes the item")
			}
			assertUngrantablePauseComment(t, client, 401)
		})
	}
}

func TestUngrantableDenial_CommentPath_PausesOnFirstOccurrence(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		name := "enabled"
		if disabled {
			name = "classification disabled (non-vacuity)"
		}
		t.Run(name, func(t *testing.T) {
			skipIfNoGit(t)
			client := &mockGitHubClient{}
			claude := &mockClaudeInvoker{
				invokeForCommentsFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
					return "", false, TokenUsage{}, ungrantableEditDenial()
				},
			}
			eng := testEngineWithRepo(t, client, claude)
			eng.cfg.MaxToolsDeniedRetries = 3
			eng.SetUngrantableDenialDisabledForTest(disabled)

			board := &gh.ProjectBoard{ProjectID: "PVT_1"}
			stage := &stages.Stage{Name: "Review", Order: 1, Completion: stages.CompletionCriteria{Type: "claude"}}
			item := gh.ProjectItem{Number: 501, Body: "spec"}
			comments := []gh.Comment{{ID: "C_1", DatabaseID: 1, Author: "some-user", Body: "please fix"}}
			_ = eng.processComments(context.Background(), board, item, stage, comments)

			paused := labelApplied(client, "fabrik:paused") && labelApplied(client, "fabrik:awaiting-input")
			if disabled {
				if paused {
					t.Error("with the classification disabled the first denial must stay on the bounded-retry path")
				}
				return
			}
			if !paused {
				t.Fatal("expected a pause after the first ungrantable denial")
			}
			if labelApplied(client, "stage:Review:failed") || labelApplied(client, "fabrik:tools-denied") {
				t.Error("no failed label and no tools-denied label expected")
			}
			snap, err := eng.store.Get("owner/repo", 501)
			if err != nil {
				t.Fatal(err)
			}
			if !snap.PausedByEngine("Review") {
				t.Error("EnginePaused must be recorded")
			}
			assertUngrantablePauseComment(t, client, 501)
		})
	}
}

func TestUngrantableDenial_MixedWithBashPausesAtOnce(t *testing.T) {
	skipIfNoGit(t)
	client := &mockGitHubClient{}
	denied := ungrantableEditDenial()
	denied.ToolNames = []string{"Edit", "Bash"}
	denied.Denials = append(denied.Denials, toolDenial{ToolName: "Bash", Command: "git push"})
	claude := &mockClaudeInvoker{
		invokeForCommentsFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			return "", false, TokenUsage{}, denied
		},
	}
	eng := testEngineWithRepo(t, client, claude)
	eng.cfg.MaxToolsDeniedRetries = 3
	stage := &stages.Stage{Name: "Review", Order: 1, Completion: stages.CompletionCriteria{Type: "claude"}}
	_ = eng.processComments(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, gh.ProjectItem{Number: 502, Body: "spec"}, stage,
		[]gh.Comment{{ID: "C_1", DatabaseID: 1, Author: "some-user", Body: "please fix"}})
	if !labelApplied(client, "fabrik:paused") {
		t.Fatal("a run with an ungrantable denial must pause at once")
	}
	found := false
	for _, c := range client.addCommentCalls {
		if strings.Contains(c.body, "1 other denied tool call") {
			found = true
		}
	}
	if !found {
		t.Error("the pause comment should mention the other denied call")
	}
}

// A path outside the worktree, a Bash denial or an undecodable path keeps the
// #1704 bound: retry until MaxToolsDeniedRetries, then the original pause.
func TestUngrantableDenial_OtherDenialsKeepBoundedRetries(t *testing.T) {
	cases := map[string]*claudeToolsDeniedError{
		"bash":    {ToolNames: []string{"Bash"}, Denials: []toolDenial{{ToolName: "Bash", Command: "git push"}}},
		"outside": {ToolNames: []string{"Write"}, Denials: []toolDenial{{ToolName: "Write", Path: "/etc/hosts"}}},
		"no-path": {ToolNames: []string{"Edit"}, Denials: []toolDenial{{ToolName: "Edit"}}},
	}
	for name, denied := range cases {
		t.Run(name, func(t *testing.T) {
			skipIfNoGit(t)
			client := &mockGitHubClient{}
			claude := &mockClaudeInvoker{
				invokeForCommentsFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
					return "", false, TokenUsage{}, denied
				},
			}
			eng := testEngineWithRepo(t, client, claude)
			const bound = 3
			eng.cfg.MaxToolsDeniedRetries = bound
			stage := &stages.Stage{Name: "Review", Order: 1, Completion: stages.CompletionCriteria{Type: "claude"}}
			item := gh.ProjectItem{Number: 503, Body: "spec"}
			comments := []gh.Comment{{ID: "C_1", DatabaseID: 1, Author: "some-user", Body: "please fix"}}
			for i := 1; i <= bound; i++ {
				_ = eng.processComments(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item, stage, comments)
				if got := labelApplied(client, "fabrik:paused"); got != (i == bound) {
					t.Fatalf("after detection %d: paused=%v, want %v", i, got, i == bound)
				}
				item.Labels = []string{"fabrik:tools-denied"}
			}
			limit := false
			for _, c := range client.addCommentCalls {
				if strings.Contains(c.body, "denial limit reached") {
					limit = true
				}
				if strings.Contains(c.body, "cannot be granted") {
					t.Error("a non-ungrantable denial must not use the ungrantable wording")
				}
			}
			if !limit {
				t.Error("expected the original denial-limit pause comment")
			}
		})
	}
}

func TestPauseComment_NamesPathForFileToolDenialsOnBoundedPath(t *testing.T) {
	skipIfNoGit(t)
	client := &mockGitHubClient{}
	eng := testEngineWithRepo(t, client, &mockClaudeInvoker{})
	eng.cfg.MaxToolsDeniedRetries = 3
	stage := &stages.Stage{Name: "Review", Order: 1}
	eng.recordToolsDeniedDetection(gh.ProjectItem{Number: 504}, stage, []string{"Write"}, []toolDenial{{ToolName: "Write", Path: "/etc/hosts"}})
	if len(client.addCommentCalls) != 1 || !strings.Contains(client.addCommentCalls[0].body, "(path: `/etc/hosts`)") {
		t.Errorf("explanatory comment should name the denied path, got %+v", client.addCommentCalls)
	}
}
