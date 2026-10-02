package gate

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
)

// resetFake plays `gh` (and `ps`) for Reset.
type resetFake struct {
	mu        sync.Mutex
	prs       map[string][]string // repo -> open PR numbers
	issues    map[string][]string
	refs      map[string][]string
	projectID string // "" = unresolvable; returned from the organization query
	userOnly  bool   // resolve through the user query, org returns null
	boardIDs  [][]string
	remaining string
	prListErr bool
	calls     []string
	psOut     string
	failClose map[string]bool // PR numbers whose --delete-branch close fails
	tokens    []string
}

func (r *resetFake) handle(_ context.Context, c Cmd) Result {
	line := argsLine(c)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, line)
	if c.Name == "ps" {
		writeStdout(c, r.psOut)
		return Result{}
	}
	if c.Name != "gh" {
		return Result{}
	}
	r.tokens = append(r.tokens, envValue(c.Env, "GH_TOKEN"))
	a := c.Args
	repoOf := func() string {
		for i, x := range a {
			if x == "-R" && i+1 < len(a) {
				return a[i+1]
			}
		}
		return ""
	}
	switch {
	case a[0] == "pr" && a[1] == "list":
		if r.prListErr {
			return Result{ExitCode: 1}
		}
		writeStdout(c, strings.Join(r.prs[repoOf()], "\n")+"\n")
	case a[0] == "pr" && a[1] == "close":
		if r.failClose[a[2]] && strings.Contains(line, "--delete-branch") {
			return Result{ExitCode: 1}
		}
	case a[0] == "issue" && a[1] == "list":
		writeStdout(c, strings.Join(r.issues[repoOf()], "\n")+"\n")
	case a[0] == "api" && strings.Contains(line, "matching-refs"):
		repo := strings.TrimPrefix(strings.Split(a[1], "/git/")[0], "repos/")
		for _, ref := range r.refs[repo] {
			writeStdout(c, ref+"\n")
		}
	case a[0] == "api" && a[1] == "graphql":
		q := strings.Join(a, " ")
		switch {
		case strings.Contains(q, "organization(login"):
			if r.userOnly {
				writeStdout(c, "null\n")
			} else if r.projectID != "" {
				writeStdout(c, r.projectID+"\n")
			} else {
				writeStdout(c, "null\n")
			}
		case strings.Contains(q, "user(login"):
			if r.userOnly {
				writeStdout(c, r.projectID+"\n")
			} else {
				writeStdout(c, "null\n")
			}
		case strings.Contains(q, "items(first:50)"):
			if len(r.boardIDs) > 0 {
				writeStdout(c, strings.Join(r.boardIDs[0], "\n")+"\n")
				r.boardIDs = r.boardIDs[1:]
			}
		case strings.Contains(q, "totalCount"):
			writeStdout(c, r.remaining+"\n")
		}
	}
	return Result{}
}

func newResetGate(t *testing.T, rf *resetFake, env ...string) (*Gate, *fakeExec) {
	t.Helper()
	g, fe, _, _ := testGate(t, env...)
	fe.handler = rf.handle
	mustWrite(t, g.Cfg.TestBed+"/.env", "FABRIK_TOKEN=bed-pat\n")
	return g, fe
}

func countCalls(rf *resetFake, sub string) int {
	n := 0
	for _, l := range rf.calls {
		if strings.Contains(l, sub) {
			n++
		}
	}
	return n
}

func TestResetClearsEverythingThroughTheBedToken(t *testing.T) {
	rf := &resetFake{
		prs:       map[string][]string{"handarbeit/fabrik-test-alpha": {"11", "12"}, "handarbeit/fabrik-test-beta": nil},
		issues:    map[string][]string{"handarbeit/fabrik-test-alpha": {"1", "2", "3"}, "handarbeit/fabrik-test-beta": {"4"}},
		refs:      map[string][]string{"handarbeit/fabrik-test-alpha": {"refs/heads/fabrik/issue-1"}},
		projectID: "PVT_x",
		boardIDs:  [][]string{{"i1", "i2"}, {"i3"}},
		remaining: "0",
		failClose: map[string]bool{"12": true},
	}
	g, _ := newResetGate(t, rf)
	if err := g.Reset(context.Background(), ResetOptions{}); err != nil {
		t.Fatal(err)
	}
	out := g.Out.(interface{ String() string }).String()
	for _, want := range []string{
		"  handarbeit/fabrik-test-alpha: closing PRs: 11 12 \n", // the trailing space is bash's `echo | tr` artefact
		"  handarbeit/fabrik-test-beta: no open PRs",
		"  handarbeit/fabrik-test-alpha: closing issues: 1 2 3 \n",
		"  handarbeit/fabrik-test-beta: closing issues: 4 \n",
		"  handarbeit/fabrik-test-alpha: deleting 1 fabrik/* branch(es)",
		"  handarbeit/fabrik-test-beta: no leftover fabrik/* branches",
		"  project handarbeit/#2 drained (remaining items: 0)",
		"done.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q:\n%s", want, out)
		}
	}
	// A failing --delete-branch close falls back to a plain close.
	if !containsLine(rf.calls, "gh pr close 12 -R handarbeit/fabrik-test-alpha --delete-branch") || !containsLine(rf.calls, "gh pr close 12 -R handarbeit/fabrik-test-alpha") {
		t.Errorf("the fallback plain close was not attempted: %v", rf.calls)
	}
	// The ref delete endpoint wants the ref WITHOUT the leading refs/.
	if !containsLine(rf.calls, "gh api -X DELETE repos/handarbeit/fabrik-test-alpha/git/refs/heads/fabrik/issue-1") {
		t.Errorf("wrong ref delete: %v", rf.calls)
	}
	if n := countCalls(rf, "deleteProjectV2Item"); n != 3 {
		t.Errorf("want 3 board items deleted, got %d", n)
	}
	for _, tok := range rf.tokens {
		if tok != "bed-pat" {
			t.Errorf("every GitHub call must be scoped to the bed's own PAT, saw GH_TOKEN=%q", tok)
		}
	}
}

// The pagination bounds are part of the contract: a smaller bound would silently
// leave items behind.
func TestResetPaginationBoundsAndDrainRounds(t *testing.T) {
	rf := &resetFake{projectID: "PVT_x", remaining: "?"}
	for i := 0; i < 20; i++ {
		rf.boardIDs = append(rf.boardIDs, []string{"stuck"}) // never drains: eventual-consistency lag
	}
	g, _ := newResetGate(t, rf)
	if err := g.Reset(context.Background(), ResetOptions{}); err != nil {
		t.Fatal(err)
	}
	if !containsLine(rf.calls, "gh pr list -R handarbeit/fabrik-test-alpha --state open --limit 200 --json number --jq .[].number") {
		t.Errorf("PR list must use --limit 200: %v", rf.calls)
	}
	if !containsLine(rf.calls, "gh issue list -R handarbeit/fabrik-test-alpha --state open --limit 500 --json number --jq .[].number") {
		t.Errorf("issue list must use --limit 500: %v", rf.calls)
	}
	if n := countCalls(rf, "items(first:50)"); n != 12 {
		t.Errorf("the board drain is bounded at 12 rounds of 50, got %d fetches", n)
	}
}

func TestResetResolvesAUserOwnedBoardAndSkipsWhenUnresolvable(t *testing.T) {
	rf := &resetFake{userOnly: true, projectID: "PVT_user", remaining: "0"}
	g, _ := newResetGate(t, rf)
	if err := g.Reset(context.Background(), ResetOptions{}); err != nil {
		t.Fatal(err)
	}
	if countCalls(rf, "totalCount") != 1 || !strings.Contains(g.Out.(interface{ String() string }).String(), "drained") {
		t.Error("a user-owned board must be resolved through the user query")
	}

	rf = &resetFake{} // neither org nor user resolves
	g, _ = newResetGate(t, rf)
	if err := g.Reset(context.Background(), ResetOptions{}); err != nil {
		t.Fatalf("an unresolvable project only skips the drain: %v", err)
	}
	if !strings.Contains(g.Err.(interface{ String() string }).String(), "could not resolve project handarbeit/#2 — skipping board drain") {
		t.Errorf("stderr: %s", g.Err.(interface{ String() string }).String())
	}
}

func TestResetHonoursItsEnvOverrides(t *testing.T) {
	rf := &resetFake{prs: map[string][]string{"o/a": {"1"}}, projectID: "P", remaining: "0"}
	g, _ := newResetGate(t, rf, "FABRIK_TEST_REPO_ALPHA=o/a", "FABRIK_TEST_REPO_BETA=o/b", "FABRIK_TEST_PROJECT_OWNER=me", "FABRIK_TEST_PROJECT_NUMBER=9")
	if err := g.Reset(context.Background(), ResetOptions{}); err != nil {
		t.Fatal(err)
	}
	if !containsLine(rf.calls, "gh pr list -R o/a --state open --limit 200 --json number --jq .[].number") || countCalls(rf, `organization(login:"me"){ projectV2(number:9)`) != 1 {
		t.Errorf("overrides ignored: %v", rf.calls)
	}
}

func TestResetRefusesWithoutABedOrToken(t *testing.T) {
	g, _, _, _ := testGate(t)
	if err := g.Reset(context.Background(), ResetOptions{}); exitCode(err) != 1 || !strings.Contains(err.Error(), "test bed not found at") {
		t.Errorf("got %v", err)
	}
	mustWrite(t, g.Cfg.TestBed+"/.env", "OTHER=1\n")
	if err := g.Reset(context.Background(), ResetOptions{}); exitCode(err) != 1 || !strings.Contains(err.Error(), "FABRIK_TOKEN missing from") {
		t.Errorf("got %v", err)
	}
}

func TestResetListFailureAbortsLikeSetE(t *testing.T) {
	rf := &resetFake{prListErr: true}
	g, _ := newResetGate(t, rf)
	if err := g.Reset(context.Background(), ResetOptions{}); exitCode(err) != 1 {
		t.Errorf("a failing `gh pr list` aborted the bash script under set -e; got %v", err)
	}
}

func TestResetWorktreesRefusesAgainstALiveBed(t *testing.T) {
	rf := &resetFake{projectID: "P", remaining: "0"}
	g, _ := newResetGate(t, rf)
	bed := g.Cfg.TestBed
	mustWrite(t, bed+"/.fabrik/worktrees/x/keep.txt", "work in progress")
	mustWrite(t, bed+"/.fabrik/repos/r.git/HEAD", "x")
	mustWrite(t, bed+"/.fabrik/fabrik.lock", "123")
	rf.psOut = "  PID COMMAND\n  555 grep " + bed + "/fabrik\n  777 " + bed + "/fabrik -notui -poll 60\n"
	err := g.Reset(context.Background(), ResetOptions{Worktrees: true})
	if exitCode(err) != 1 || !strings.Contains(err.Error(), "fabrik-test pid 777 is running — stop it before --worktrees") {
		t.Fatalf("got %v", err)
	}
	// NEVER destroy worktrees with existing content when refusing.
	for _, p := range []string{"/.fabrik/worktrees/x/keep.txt", "/.fabrik/repos/r.git/HEAD", "/.fabrik/fabrik.lock"} {
		if _, err := os.Stat(bed + p); err != nil {
			t.Errorf("%s was removed despite the refusal", p)
		}
	}
}

func TestResetWorktreesRemovesThemWhenTheBedIsDown(t *testing.T) {
	rf := &resetFake{projectID: "P", remaining: "0"}
	g, _ := newResetGate(t, rf)
	bed := g.Cfg.TestBed
	mustWrite(t, bed+"/.fabrik/worktrees/x/a.txt", "x")
	mustWrite(t, bed+"/.fabrik/repos/r.git/HEAD", "x")
	mustWrite(t, bed+"/.fabrik/fabrik.lock", "123")
	mustWrite(t, bed+"/.fabrik/config.yaml", "keep")
	rf.psOut = "  PID COMMAND\n  555 grep " + bed + "/fabrik\n  556 /usr/bin/something else\n" // only the grep itself mentions the path
	if err := g.Reset(context.Background(), ResetOptions{Worktrees: true}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/.fabrik/worktrees", "/.fabrik/repos", "/.fabrik/fabrik.lock"} {
		if _, err := os.Stat(bed + p); err == nil {
			t.Errorf("%s should have been removed", p)
		}
	}
	if _, err := os.Stat(bed + "/.fabrik/config.yaml"); err != nil {
		t.Error("config must survive")
	}
	if !strings.Contains(g.Out.(interface{ String() string }).String(), "worktrees + bare clones removed; restart fabrik-test to re-init") {
		t.Error("missing the completion line")
	}
}

func TestResetWithoutWorktreesNeverTouchesThem(t *testing.T) {
	rf := &resetFake{projectID: "P", remaining: "0"}
	g, _ := newResetGate(t, rf)
	mustWrite(t, g.Cfg.TestBed+"/.fabrik/worktrees/x/a.txt", "x")
	if err := g.Reset(context.Background(), ResetOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(g.Cfg.TestBed + "/.fabrik/worktrees/x/a.txt"); err != nil {
		t.Error("the default form must leave worktrees alone")
	}
	if countCalls(rf, "ps ax") != 0 {
		t.Error("ps is only for the --worktrees form")
	}
}

func containsLine(lines []string, want string) bool {
	for _, l := range lines {
		if l == want {
			return true
		}
	}
	return false
}
