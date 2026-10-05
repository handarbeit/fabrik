package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

func TestSpecSlug(t *testing.T) {
	long := strings.Repeat("word-", 20)
	cases := []struct{ title, want string }{
		{"Specify: author specs in Spec Kit structure", "specify-author-specs-in-spec-kit-structure"},
		{"  --Hello,   World!!  ", "hello-world"},
		{"", "spec"},
		{"!!! ???", "spec"},
		{"日本語のタイトル", "spec"},
		{"Café au lait", "caf-au-lait"},
	}
	for _, c := range cases {
		if got := specSlug(c.title); got != c.want {
			t.Errorf("specSlug(%q) = %q, want %q", c.title, got, c.want)
		}
	}
	got := specSlug(long)
	if len(got) > specSlugMaxLen || strings.HasSuffix(got, "-") || got == "" {
		t.Errorf("long slug = %q (len %d)", got, len(got))
	}
}

func TestStripOpenQuestions(t *testing.T) {
	body := "## Problem\nx\n\n## Open Questions\n- q1\n- q2\n\n## Scope\ny\n"
	got := stripOpenQuestions(body)
	if strings.Contains(got, "Open Questions") || strings.Contains(got, "q1") {
		t.Errorf("section not stripped: %q", got)
	}
	if !strings.Contains(got, "## Problem") || !strings.Contains(got, "## Scope") || !strings.Contains(got, "y") {
		t.Errorf("neighbouring sections lost: %q", got)
	}
	// Trailing section runs to EOF.
	if got := stripOpenQuestions("## A\na\n## Open Questions\n- q\n"); strings.Contains(got, "q") {
		t.Errorf("trailing section not stripped: %q", got)
	}
	// No section: unchanged.
	if got := stripOpenQuestions("## A\na\n"); got != "## A\na\n" {
		t.Errorf("unchanged body modified: %q", got)
	}
	// Level-3 heading inside the section does not end it.
	if got := stripOpenQuestions("## Open Questions\n### Sub\nq\n## Next\nn\n"); strings.Contains(got, "Sub") || !strings.Contains(got, "## Next") {
		t.Errorf("subheading handling wrong: %q", got)
	}
}

func TestIsSpecOnlyCommit(t *testing.T) {
	if !isSpecOnlyCommit([]string{"specs/7-foo/spec.md"}, 7) {
		t.Error("own spec should be spec-only")
	}
	for _, files := range [][]string{
		nil,
		{"specs/8-foo/spec.md"},
		{"specs/7-foo/spec.md", "main.go"},
		{"specs/7-foo/plan.md"},
		{"specs/7-foo/sub/spec.md"},
	} {
		if isSpecOnlyCommit(files, 7) {
			t.Errorf("%v must not be spec-only", files)
		}
	}
}

func specStage() *stages.Stage {
	return &stages.Stage{Name: "Specify", ReadOnly: true, PersistSpec: true}
}

func TestPersistSpec_AddThenUpdateThenNoop(t *testing.T) {
	skipIfNoGit(t)
	dir := initBareRepo(t)
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	item := gh.ProjectItem{Number: 12, Title: "My Feature"}

	if !eng.persistSpec(item, specStage(), dir, "## Problem\nv1\n\n## Open Questions\n- q\n") {
		t.Fatal("first round should commit")
	}
	data, err := os.ReadFile(filepath.Join(dir, "specs/12-my-feature/spec.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "Open Questions") || !strings.Contains(string(data), "v1") {
		t.Errorf("file = %q", data)
	}
	if msg := gitOut(t, dir, "log", "-1", "--format=%s"); !strings.HasPrefix(msg, "docs(spec): add specs/12-my-feature/spec.md") {
		t.Errorf("first commit msg = %q", msg)
	}

	// Title changed mid-clarification: the directory is reused, never renamed.
	item.Title = "Totally Different Title"
	if !eng.persistSpec(item, specStage(), dir, "## Problem\nv2\n") {
		t.Fatal("changed round should commit")
	}
	if msg := gitOut(t, dir, "log", "-1", "--format=%s"); !strings.HasPrefix(msg, "docs(spec): update specs/12-my-feature/spec.md") {
		t.Errorf("second commit msg = %q", msg)
	}
	if _, err := os.Stat(filepath.Join(dir, "specs/12-totally-different-title")); err == nil {
		t.Error("slug must stay locked at first commit")
	}

	// Byte-identical content (modulo the stripped section): no commit.
	before := gitOut(t, dir, "rev-parse", "HEAD")
	if eng.persistSpec(item, specStage(), dir, "## Problem\nv2\n\n## Open Questions\n- new q\n") {
		t.Error("identical projection must not commit")
	}
	if after := gitOut(t, dir, "rev-parse", "HEAD"); after != before {
		t.Error("HEAD moved on identical content")
	}
	if n := gitOut(t, dir, "rev-list", "--count", "HEAD"); n != "3" { // initial + add + update
		t.Errorf("commit count = %s, want 3", n)
	}
}

func TestPersistSpec_OptInAndEmptyBody(t *testing.T) {
	skipIfNoGit(t)
	dir := initBareRepo(t)
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	item := gh.ProjectItem{Number: 3, Title: "x"}

	if eng.persistSpec(item, &stages.Stage{Name: "Specify", ReadOnly: true}, dir, "body") {
		t.Error("persist_spec unset must be a no-op")
	}
	if eng.persistSpec(item, specStage(), dir, "  \n") {
		t.Error("empty body must be a no-op")
	}
	if eng.persistSpec(item, specStage(), "", "body") {
		t.Error("empty workDir must be a no-op")
	}
	if _, err := os.Stat(filepath.Join(dir, "specs")); err == nil {
		t.Error("nothing should have been written")
	}
}

func TestPersistSpec_PathspecCommitIgnoresOtherDirtyState(t *testing.T) {
	skipIfNoGit(t)
	dir := initBareRepo(t)
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})

	if err := os.WriteFile(filepath.Join(dir, "unrelated.txt"), []byte("wip"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, dir, "add", "unrelated.txt") // even a staged unrelated file must not ride along
	if err := os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("u"), 0o644); err != nil {
		t.Fatal(err)
	}

	if !eng.persistSpec(gh.ProjectItem{Number: 5, Title: "t"}, specStage(), dir, "## A\nb\n") {
		t.Fatal("expected commit")
	}
	files := gitOut(t, dir, "show", "--name-only", "--format=", "HEAD")
	if files != "specs/5-t/spec.md" {
		t.Errorf("commit touched %q, want only the spec", files)
	}
	status := gitOut(t, dir, "status", "--porcelain")
	if !strings.Contains(status, "A  unrelated.txt") || !strings.Contains(status, "?? untracked.txt") {
		t.Errorf("unrelated state disturbed: %q", status)
	}
}

// TestPersistSpec_RetriesAfterFailedCommit pins the recovery path: a round that
// wrote the file but failed to commit must not leave it uncommitted forever
// when the next round projects the same content.
func TestPersistSpec_RetriesAfterFailedCommit(t *testing.T) {
	skipIfNoGit(t)
	dir := initBareRepo(t)
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	item := gh.ProjectItem{Number: 14, Title: "Retry"}

	// A failing pre-commit hook makes the first round write the file but not commit it.
	hook := filepath.Join(dir, ".git", "hooks", "pre-commit")
	if err := os.MkdirAll(filepath.Dir(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	before := gitOut(t, dir, "rev-parse", "HEAD")
	if eng.persistSpec(item, specStage(), dir, "## A\nb\n") {
		t.Fatal("commit should have failed")
	}
	if gitOut(t, dir, "rev-parse", "HEAD") != before {
		t.Fatal("HEAD moved despite failing hook")
	}

	// Hook fixed; the same body must now be committed as an add.
	if err := os.Remove(hook); err != nil {
		t.Fatal(err)
	}
	if !eng.persistSpec(item, specStage(), dir, "## A\nb\n") {
		t.Fatal("unchanged-but-uncommitted spec must be committed on the next round")
	}
	if msg := gitOut(t, dir, "log", "-1", "--format=%s"); !strings.HasPrefix(msg, "docs(spec): add specs/14-retry/spec.md") {
		t.Errorf("commit msg = %q", msg)
	}
	if eng.persistSpec(item, specStage(), dir, "## A\nb\n") {
		t.Error("now-committed spec must be a no-op")
	}
}

func TestPersistSpec_ReusesExistingDirAfterRestart(t *testing.T) {
	skipIfNoGit(t)
	dir := initBareRepo(t)
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	if err := os.MkdirAll(filepath.Join(dir, "specs/9-old-slug"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !eng.persistSpec(gh.ProjectItem{Number: 9, Title: "New Title"}, specStage(), dir, "## A\nb\n") {
		t.Fatal("expected commit")
	}
	if _, err := os.Stat(filepath.Join(dir, "specs/9-old-slug/spec.md")); err != nil {
		t.Errorf("existing directory not reused: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "specs/9-new-title")); err == nil {
		t.Error("a second directory was created")
	}
}

// A repo's own hand-written specs/<N>-*/spec.md that shares the issue number
// must never be reused or overwritten.
func TestPersistSpec_DoesNotOverwriteForeignSpec(t *testing.T) {
	skipIfNoGit(t)
	dir := initBareRepo(t)
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	foreign := filepath.Join(dir, "specs/12-auth/spec.md")
	if err := os.MkdirAll(filepath.Dir(foreign), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(foreign, []byte("hand written\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, dir, "add", "specs/12-auth/spec.md")
	gitOut(t, dir, "commit", "-m", "docs: auth spec")

	item := gh.ProjectItem{Number: 12, Title: "My Feature"}
	if !eng.persistSpec(item, specStage(), dir, "## Problem\nv1\n") {
		t.Fatal("expected a new directory to be created beside the foreign one")
	}
	if data, _ := os.ReadFile(foreign); string(data) != "hand written\n" {
		t.Errorf("foreign spec overwritten: %q", data)
	}
	if _, err := os.Stat(filepath.Join(dir, "specs/12-my-feature/spec.md")); err != nil {
		t.Errorf("fabrik spec not written to its own directory: %v", err)
	}
	// The next round locks onto the Fabrik directory, not the foreign one.
	if !eng.persistSpec(item, specStage(), dir, "## Problem\nv2\n") {
		t.Fatal("second round should commit")
	}
	if msg := gitOut(t, dir, "log", "-1", "--format=%s"); !strings.HasPrefix(msg, "docs(spec): update specs/12-my-feature/spec.md") {
		t.Errorf("second commit msg = %q", msg)
	}
	if data, _ := os.ReadFile(foreign); string(data) != "hand written\n" {
		t.Errorf("foreign spec overwritten on round 2: %q", data)
	}

	// A derived directory that is itself foreign is left alone, not overwritten.
	other := gh.ProjectItem{Number: 12, Title: "Auth"}
	dir2 := initBareRepo(t)
	f2 := filepath.Join(dir2, "specs/12-auth/spec.md")
	if err := os.MkdirAll(filepath.Dir(f2), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f2, []byte("hand written\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, dir2, "add", "specs/12-auth/spec.md")
	gitOut(t, dir2, "commit", "-m", "docs: auth spec")
	if eng.persistSpec(other, specStage(), dir2, "## Problem\nv1\n") {
		t.Error("must not persist over a foreign spec at the derived path")
	}
	if data, _ := os.ReadFile(f2); string(data) != "hand written\n" {
		t.Errorf("foreign spec overwritten: %q", data)
	}
}

// TestCommitsAheadOfBase_SpecOnlyCommitNotCounted pins the #921 interaction:
// Specify's engine-written spec commit must not make a delegated coordinator
// look like it has work of its own, while real commits (also mixed ones) count.
func TestCommitsAheadOfBase_SpecOnlyCommitNotCounted(t *testing.T) {
	skipIfNoGit(t)
	dir := initBareRepo(t)
	fakeOriginRef(t, dir, "main")
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	item := gh.ProjectItem{Number: 7, Title: "coordinator"}

	if !eng.persistSpec(item, specStage(), dir, "## A\nb\n") {
		t.Fatal("expected spec commit")
	}
	if n, err := commitsAheadOfBase(dir, "main", 7); err != nil || n != 0 {
		t.Fatalf("spec-only branch: ahead = %d, err = %v; want 0", n, err)
	}
	// Another issue's spec is real work from this issue's point of view.
	if n, _ := commitsAheadOfBase(dir, "main", 8); n != 1 {
		t.Errorf("other issue's view: ahead = %d, want 1", n)
	}

	if err := os.WriteFile(filepath.Join(dir, "code.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, dir, "add", "code.go")
	gitOut(t, dir, "commit", "-m", "feat: real work")
	if n, _ := commitsAheadOfBase(dir, "main", 7); n != 1 {
		t.Errorf("with real commit: ahead = %d, want 1", n)
	}
	// A commit mixing the spec with other files counts.
	if err := os.WriteFile(filepath.Join(dir, "specs/7-coordinator/spec.md"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "more.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, dir, "add", "-A")
	gitOut(t, dir, "commit", "-m", "feat: mixed")
	if n, _ := commitsAheadOfBase(dir, "main", 7); n != 2 {
		t.Errorf("with mixed commit: ahead = %d, want 2", n)
	}
}

// specCommentEngine builds an engine whose WorktreeManager has a real worktree
// for the issue, backed by a local bare "origin", so the comment path's push
// can be observed.
func specCommentEngine(t *testing.T, issue int) (*Engine, string, string) {
	t.Helper()
	repoDir := initBareRepo(t)
	origin := filepath.Join(t.TempDir(), "origin.git")
	gitOut(t, repoDir, "init", "--bare", origin)
	gitOut(t, repoDir, "remote", "add", "origin", origin)
	wm := NewWorktreeManager(repoDir)
	wtDir := wm.worktreeDir(issue)
	if err := os.MkdirAll(filepath.Dir(wtDir), 0o755); err != nil {
		t.Fatal(err)
	}
	gitOut(t, repoDir, "worktree", "add", "-b", wm.branchName(issue), wtDir)
	eng := NewWithDeps(Config{Owner: "owner", Repo: "repo", User: "u", Token: "t", MaxConcurrent: 1, Stages: testStages()},
		&mockGitHubClient{}, &mockClaudeInvoker{}, wm)
	return eng, wtDir, origin
}

func TestPublishCommentOutput_PersistsSpecAndPushes(t *testing.T) {
	skipIfNoGit(t)
	const issue = 31
	eng, wtDir, origin := specCommentEngine(t, issue)
	item := gh.ProjectItem{Number: issue, Title: "Clarify me", Repo: "owner/repo"}
	out := "FABRIK_ISSUE_UPDATE_BEGIN\n## Problem\nround one\n\n## Open Questions\n- q\nFABRIK_ISSUE_UPDATE_END\n"

	eng.publishCommentOutput("owner", "repo", item, specStage(), nil, out, wtDir, "main")

	if _, err := os.Stat(filepath.Join(wtDir, "specs/31-clarify-me/spec.md")); err != nil {
		t.Fatalf("spec not written: %v", err)
	}
	branch := eng.worktreesFor("owner/repo").branchName(issue)
	if remote := gitOut(t, origin, "log", "-1", "--format=%s", branch); !strings.HasPrefix(remote, "docs(spec): add") {
		t.Errorf("remote tip = %q; comment-round commit was not pushed", remote)
	}

	// A round whose projection is unchanged commits and pushes nothing.
	before := gitOut(t, wtDir, "rev-parse", "HEAD")
	eng.publishCommentOutput("owner", "repo", item, specStage(), nil, out, wtDir, "main")
	if after := gitOut(t, wtDir, "rev-parse", "HEAD"); after != before {
		t.Error("unchanged round created a commit")
	}

	// NO_WORK_NEEDED never persists.
	item2 := gh.ProjectItem{Number: issue, Title: "Clarify me", Repo: "owner/repo"}
	eng.publishCommentOutput("owner", "repo", item2, specStage(), nil, out+"changed\nFABRIK_STAGE_COMPLETE\nFABRIK_NO_WORK_NEEDED\n", wtDir, "main")
	if after := gitOut(t, wtDir, "rev-parse", "HEAD"); after != before {
		t.Error("NO_WORK_NEEDED round created a commit")
	}
}

func TestPublishCommentOutput_PersistSpecUnsetIsNoop(t *testing.T) {
	skipIfNoGit(t)
	const issue = 32
	eng, wtDir, _ := specCommentEngine(t, issue)
	item := gh.ProjectItem{Number: issue, Title: "t", Repo: "owner/repo"}
	eng.publishCommentOutput("owner", "repo", item, &stages.Stage{Name: "Specify", ReadOnly: true}, nil,
		"FABRIK_ISSUE_UPDATE_BEGIN\nbody\nFABRIK_ISSUE_UPDATE_END\n", wtDir, "main")
	if _, err := os.Stat(filepath.Join(wtDir, "specs")); err == nil {
		t.Error("specs/ written without persist_spec")
	}
}

func specifyStages(persist bool) []*stages.Stage {
	return []*stages.Stage{
		{Name: "Specify", Order: 0, Prompt: "specify it", ReadOnly: true, PersistSpec: persist,
			Completion: stages.CompletionCriteria{Type: "claude"}},
		{Name: "Done", Order: 99, CleanupWorktree: true},
	}
}

// runSpecifyRound drives one real processItem Specify run whose Claude output is
// out, and returns the worktree directory.
func runSpecifyRound(t *testing.T, persist bool, out string, completed bool) (*Engine, string) {
	t.Helper()
	origLock := lockVerifyDelay
	lockVerifyDelay = 0
	t.Cleanup(func() { lockVerifyDelay = origLock })

	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			return out, completed, TokenUsage{}, nil
		},
	}
	eng, _ := testEngineWithRepoAndStages(t, &mockGitHubClient{}, claude, specifyStages(persist))
	item := gh.ProjectItem{Number: 40, Title: "Persist Me", Status: "Specify", ItemID: "PVTI_40", Repo: "owner/repo"}
	if err := eng.processItem(t.Context(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item); err != nil {
		t.Fatalf("processItem: %v", err)
	}
	return eng, eng.worktreesFor("owner/repo").worktreeDir(40)
}

// FR-004/SC-003: a round ending in FABRIK_BLOCKED_ON_INPUT still persists the spec,
// pushes it via the existing post-run push, and strips Open Questions (FR-005).
func TestFinalizeStageOutcome_SpecifyBlockedRoundPersistsSpec(t *testing.T) {
	skipIfNoGit(t)
	out := "FABRIK_ISSUE_UPDATE_BEGIN\n## Problem\nround one\n\n## Open Questions\n- which?\nFABRIK_ISSUE_UPDATE_END\nFABRIK_BLOCKED_ON_INPUT\nFABRIK_SUMMARY_BEGIN\nwhich?\nFABRIK_SUMMARY_END\n"
	eng, wtDir := runSpecifyRound(t, true, out, false)

	data, err := os.ReadFile(filepath.Join(wtDir, "specs/40-persist-me/spec.md"))
	if err != nil {
		t.Fatalf("spec not persisted on a blocked round: %v", err)
	}
	if strings.Contains(string(data), "Open Questions") || !strings.Contains(string(data), "round one") {
		t.Errorf("spec = %q", data)
	}
	if msg := gitOut(t, wtDir, "log", "-1", "--format=%s"); !strings.HasPrefix(msg, "docs(spec): add specs/40-persist-me/spec.md") {
		t.Errorf("tip commit = %q; want the add commit, not a partial-progress WIP commit", msg)
	}
	if strings.Contains(gitOut(t, wtDir, "log", "--format=%s"), "partial") {
		t.Error("read-only stage must not produce a WIP commit")
	}
	// Pushed by the existing post-run push.
	remote := gitOut(t, wtDir, "ls-remote", "origin", eng.worktreesFor("owner/repo").branchName(40))
	if remote == "" {
		t.Error("branch was not pushed to origin")
	}
}

func TestFinalizeStageOutcome_SpecifyPersistSpecUnsetWritesNothing(t *testing.T) {
	skipIfNoGit(t)
	out := "FABRIK_ISSUE_UPDATE_BEGIN\n## Problem\nx\nFABRIK_ISSUE_UPDATE_END\nFABRIK_STAGE_COMPLETE\n"
	_, wtDir := runSpecifyRound(t, false, out, true)
	if _, err := os.Stat(filepath.Join(wtDir, "specs/40-persist-me")); err == nil {
		t.Error("spec written without persist_spec")
	}
}

func TestFinalizeStageOutcome_SpecifyNoBodyUpdateWritesNothing(t *testing.T) {
	skipIfNoGit(t)
	_, wtDir := runSpecifyRound(t, true, "FABRIK_STAGE_COMPLETE\n", true)
	if _, err := os.Stat(filepath.Join(wtDir, "specs/40-persist-me")); err == nil {
		t.Error("spec written on a round with no issue-body update")
	}
}
