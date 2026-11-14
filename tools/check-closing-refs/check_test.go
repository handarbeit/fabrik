package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/handarbeit/fabrik/internal/closingrefs"
)

const repo = "handarbeit/fabrik"

type fakePR struct {
	body    string
	commits []closingrefs.Commit
	bodyErr error
	comErr  error
}

func (f fakePR) Body(context.Context) (string, error)                  { return f.body, f.bodyErr }
func (f fakePR) Commits(context.Context) ([]closingrefs.Commit, error) { return f.commits, f.comErr }

type fakeBoard struct {
	verifyErr error
	members   map[string]bool
	err       error
	calls     int
}

func (b *fakeBoard) Verify(context.Context) error { b.calls++; return b.verifyErr }
func (b *fakeBoard) OnBoard(_ context.Context, r string, n int) (bool, error) {
	b.calls++
	return b.members[fmt.Sprintf("%s#%d", r, n)], b.err
}

func runCheck(t *testing.T, pr fakePR, b *fakeBoard, secrets ...string) (int, string, string) {
	t.Helper()
	var out, sum bytes.Buffer
	code := run(context.Background(), config{Repo: repo, BoardLabel: "board #3", Secrets: secrets, Annotate: true}, pr, b, &out, &sum)
	return code, out.String(), sum.String()
}

func mustContain(t *testing.T, s string, subs ...string) {
	t.Helper()
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			t.Errorf("output missing %q:\n%s", sub, s)
		}
	}
}

// The #2556 shape: a sentence that denies closing the report still closes it.
func TestNegatedBodyOnBoardFails(t *testing.T) {
	b := &fakeBoard{members: map[string]bool{"handarbeit/fabrik#2497": true}}
	code, out, sum := runCheck(t, fakePR{body: "This PR does not close #2497; that report is closed by hand after a release ships."}, b)
	if code != exitFail {
		t.Fatalf("code = %d", code)
	}
	mustContain(t, out, "handarbeit/fabrik#2497", `"close #2497" in PR body`, "mentioned for context: #N", "#N is out of scope here", "::error")
	mustContain(t, sum, "handarbeit/fabrik#2497")
}

// A merge-train integration or catch-up PR carries member commits; one member's keyword counts.
func TestMemberCommitKeywordFails(t *testing.T) {
	b := &fakeBoard{members: map[string]bool{"handarbeit/fabrik#2497": true}}
	pr := fakePR{
		body: "Closes #101\nCloses #102",
		commits: []closingrefs.Commit{
			{SHA: "1111111", Message: "feat: member one"},
			{SHA: "2222222", Message: "fix: member two\n\nFixes #2497"},
		},
	}
	code, out, _ := runCheck(t, pr, b)
	if code != exitFail {
		t.Fatalf("code = %d", code)
	}
	mustContain(t, out, "handarbeit/fabrik#2497", "commit 2222222", `"Fixes #2497"`)
}

func TestNormalFabrikPRPasses(t *testing.T) {
	b := &fakeBoard{members: map[string]bool{"handarbeit/fabrik#2497": true}}
	pr := fakePR{body: "Closes #2576\n\n## Summary\nAdds the guard. Mentioned for context: #2497.",
		commits: []closingrefs.Commit{{SHA: "abc", Message: "feat: thing"}}}
	if code, out, _ := runCheck(t, pr, b); code != exitOK {
		t.Fatalf("code = %d\n%s", code, out)
	}
}

func TestTrainIntegrationPRWithOnlyWorkIssuesPasses(t *testing.T) {
	b := &fakeBoard{members: map[string]bool{"handarbeit/fabrik#2497": true}}
	pr := fakePR{
		body: "Closes #2600\nCloses #2601\nCloses #2602",
		commits: []closingrefs.Commit{
			{SHA: "a1", Message: "feat: one\n\nCloses #2600"},
			{SHA: "a2", Message: "Merge branch 'fabrik/issue-2601'"},
			{SHA: "a3", Message: "fix: two\n\nResolves #2602"},
		},
	}
	if code, out, _ := runCheck(t, pr, b); code != exitOK {
		t.Fatalf("code = %d\n%s", code, out)
	}
}

func TestMixedNamesOnlyBoardIssues(t *testing.T) {
	b := &fakeBoard{members: map[string]bool{"handarbeit/fabrik#5": true, "other/repo#8": true}}
	pr := fakePR{body: "Closes #4, closes #5, fixes other/repo#8 and resolves #9"}
	code, out, _ := runCheck(t, pr, b)
	if code != exitFail {
		t.Fatalf("code = %d", code)
	}
	mustContain(t, out, "handarbeit/fabrik#5", "other/repo#8")
	for _, no := range []string{"offending handarbeit/fabrik#4", "\n  handarbeit/fabrik#4\n", "\n  handarbeit/fabrik#9\n"} {
		if strings.Contains(out, no) {
			t.Errorf("non-board issue reported as offender (%q):\n%s", no, out)
		}
	}
}

// FR-009: nothing to look up, nothing to read; an unreadable board cannot matter.
func TestNoReferencesNeverConsultsTheBoard(t *testing.T) {
	b := &fakeBoard{verifyErr: errors.New("boom")}
	if code, out, _ := runCheck(t, fakePR{body: "Closes #\nMentioned for context: #5", commits: []closingrefs.Commit{{SHA: "x", Message: "feat: y"}}}, b); code != exitOK {
		t.Fatalf("code = %d\n%s", code, out)
	}
	if b.calls != 0 {
		t.Fatalf("board consulted %d times", b.calls)
	}
}

func TestUnreadableBoardFailsClosed(t *testing.T) {
	cases := map[string]*fakeBoard{
		"verify error":     {verifyErr: errors.New("project 3 of handarbeit is not visible")},
		"membership error": {err: errors.New("HTTP 502")},
	}
	for name, b := range cases {
		t.Run(name, func(t *testing.T) {
			code, out, sum := runCheck(t, fakePR{body: "Closes #77"}, b)
			if code != exitFail {
				t.Fatalf("code = %d", code)
			}
			mustContain(t, out, "could not verify board #3")
			mustContain(t, sum, "could not verify board #3")
		})
	}
}

func TestPRReadFailureFailsClosed(t *testing.T) {
	for name, pr := range map[string]fakePR{
		"body":    {bodyErr: errors.New("HTTP 404")},
		"commits": {comErr: errors.New("HTTP 500")},
	} {
		t.Run(name, func(t *testing.T) {
			code, out, _ := runCheck(t, pr, &fakeBoard{})
			if code != exitFail {
				t.Fatalf("code = %d", code)
			}
			mustContain(t, out, "could not verify")
		})
	}
}

func TestCommitCapFailsClosed(t *testing.T) {
	commits := make([]closingrefs.Commit, commitCap)
	for i := range commits {
		commits[i] = closingrefs.Commit{SHA: fmt.Sprint(i), Message: "feat: x"}
	}
	code, out, _ := runCheck(t, fakePR{body: "no refs", commits: commits}, &fakeBoard{})
	if code != exitFail {
		t.Fatalf("code = %d", code)
	}
	mustContain(t, out, "could not verify", "250")
}

func TestSecretsAreRedacted(t *testing.T) {
	b := &fakeBoard{verifyErr: errors.New("bad credentials for ghp_SECRETTOKEN123")}
	code, out, sum := runCheck(t, fakePR{body: "Closes #1"}, b, "ghp_SECRETTOKEN123")
	if code != exitFail {
		t.Fatalf("code = %d", code)
	}
	if strings.Contains(out, "SECRETTOKEN") || strings.Contains(sum, "SECRETTOKEN") {
		t.Fatalf("token leaked:\n%s\n%s", out, sum)
	}
	mustContain(t, out, "***")
}

// ---- github.go against a fake API ----

type fakeAPI struct {
	t             *testing.T
	body          string
	commitCount   int
	boardVisible  bool
	issueProjects string // JSON for projectItems
	issueErr      string // raw "errors" JSON
	gotAuth       map[string]string
}

func (f *fakeAPI) handler(w http.ResponseWriter, r *http.Request) {
	if f.gotAuth == nil {
		f.gotAuth = map[string]string{}
	}
	f.gotAuth[r.URL.Path] = r.Header.Get("Authorization")
	switch {
	case r.URL.Path == "/repos/handarbeit/fabrik/pulls/9":
		_ = json.NewEncoder(w).Encode(map[string]any{"body": f.body})
	case r.URL.Path == "/repos/handarbeit/fabrik/pulls/9/commits":
		var page int
		fmt.Sscan(r.URL.Query().Get("page"), &page)
		var out []map[string]any
		for i := (page - 1) * 100; i < f.commitCount && i < page*100; i++ {
			out = append(out, map[string]any{"sha": fmt.Sprint("sha", i), "commit": map[string]any{"message": fmt.Sprint("msg ", i)}})
		}
		_ = json.NewEncoder(w).Encode(out)
	case r.URL.Path == "/graphql":
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), "projectV2(number") {
			if f.boardVisible {
				io.WriteString(w, `{"data":{"organization":{"projectV2":{"id":"PVT_x"}}}}`)
			} else {
				io.WriteString(w, `{"data":{"organization":{"projectV2":null}}}`)
			}
			return
		}
		if f.issueErr != "" {
			io.WriteString(w, f.issueErr)
			return
		}
		io.WriteString(w, `{"data":{"repository":{"issue":{"projectItems":`+f.issueProjects+`}}}}`)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeAPI) client(t *testing.T, boardToken string) *ghClient {
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(srv.Close)
	return newGHClient(srv.URL, "", "rest-token", boardToken, repo, 9, "handarbeit", 3)
}

func TestGitHubClientDetectsBoardMembership(t *testing.T) {
	api := &fakeAPI{boardVisible: true, body: "Closes #5", commitCount: 1,
		issueProjects: `{"pageInfo":{"hasNextPage":false},"nodes":[{"project":{"number":1,"owner":{"login":"handarbeit"}}},{"project":{"number":3,"owner":{"login":"Handarbeit"}}}]}`}
	cl := api.client(t, "board-token")
	var out bytes.Buffer
	if code := run(context.Background(), config{Repo: repo, BoardLabel: "board #3"}, cl, cl, &out, nil); code != exitFail {
		t.Fatalf("code = %d\n%s", code, out.String())
	}
	if api.gotAuth["/graphql"] != "Bearer board-token" || api.gotAuth["/repos/handarbeit/fabrik/pulls/9"] != "Bearer rest-token" {
		t.Fatalf("wrong tokens used: %v", api.gotAuth)
	}
}

func TestGitHubClientWorkIssueOnEngineBoardOnlyPasses(t *testing.T) {
	api := &fakeAPI{boardVisible: true, body: "Closes #5", commitCount: 2,
		issueProjects: `{"pageInfo":{"hasNextPage":false},"nodes":[{"project":{"number":1,"owner":{"login":"handarbeit"}}},{"project":{"number":3,"owner":{"login":"someone-else"}}}]}`}
	cl := api.client(t, "board-token")
	var out bytes.Buffer
	if code := run(context.Background(), config{Repo: repo}, cl, cl, &out, nil); code != exitOK {
		t.Fatalf("code = %d\n%s", code, out.String())
	}
}

// The trap: a token that cannot see the board gets an EMPTY projectItems list, not an error.
func TestGitHubClientBoardNotVisibleFailsClosed(t *testing.T) {
	api := &fakeAPI{boardVisible: false, body: "Closes #5", issueProjects: `{"pageInfo":{"hasNextPage":false},"nodes":[]}`}
	cl := api.client(t, "board-token")
	var out bytes.Buffer
	if code := run(context.Background(), config{Repo: repo, BoardLabel: "board #3"}, cl, cl, &out, nil); code != exitFail {
		t.Fatalf("code = %d\n%s", code, out.String())
	}
	mustContain(t, out.String(), "could not verify board #3", "not visible")
}

func TestGitHubClientMissingTokenFailsClosed(t *testing.T) {
	api := &fakeAPI{boardVisible: true, body: "Closes #5"}
	cl := api.client(t, "")
	var out bytes.Buffer
	if code := run(context.Background(), config{Repo: repo, BoardLabel: "board #3"}, cl, cl, &out, nil); code != exitFail {
		t.Fatalf("code = %d", code)
	}
	mustContain(t, out.String(), "could not verify board #3", "no board token")
}

func TestGitHubClientPaginationFailsClosed(t *testing.T) {
	api := &fakeAPI{boardVisible: true, body: "Closes #5",
		issueProjects: `{"pageInfo":{"hasNextPage":true},"nodes":[]}`}
	cl := api.client(t, "board-token")
	var out bytes.Buffer
	if code := run(context.Background(), config{Repo: repo, BoardLabel: "board #3"}, cl, cl, &out, nil); code != exitFail {
		t.Fatalf("code = %d\n%s", code, out.String())
	}
	mustContain(t, out.String(), "could not verify")
}

func TestGitHubClientMissingIssueIsNotAnOffender(t *testing.T) {
	api := &fakeAPI{boardVisible: true, body: "Closes #5",
		issueErr: `{"data":{"repository":{"issue":null}},"errors":[{"type":"NOT_FOUND","path":["repository","issue"],"message":"Could not resolve to an Issue"}]}`}
	cl := api.client(t, "board-token")
	var out bytes.Buffer
	if code := run(context.Background(), config{Repo: repo}, cl, cl, &out, nil); code != exitOK {
		t.Fatalf("code = %d\n%s", code, out.String())
	}
}

// A repository the token cannot read answers NOT_FOUND like a missing one; it must not pass.
func TestGitHubClientUnreadableRepositoryFailsClosed(t *testing.T) {
	api := &fakeAPI{boardVisible: true, body: "Closes someone/private-repo#5",
		issueErr: `{"data":{"repository":null},"errors":[{"type":"NOT_FOUND","path":["repository"],"message":"Could not resolve to a Repository"}]}`}
	cl := api.client(t, "board-token")
	var out bytes.Buffer
	if code := run(context.Background(), config{Repo: repo, BoardLabel: "board #3"}, cl, cl, &out, nil); code != exitFail {
		t.Fatalf("code = %d\n%s", code, out.String())
	}
	mustContain(t, out.String(), "could not verify board #3", "someone/private-repo", "not readable")
}

func TestGitHubClientOtherGraphQLErrorFailsClosed(t *testing.T) {
	api := &fakeAPI{boardVisible: true, body: "Closes #5",
		issueErr: `{"data":null,"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded"}]}`}
	cl := api.client(t, "board-token")
	var out bytes.Buffer
	if code := run(context.Background(), config{Repo: repo, BoardLabel: "board #3"}, cl, cl, &out, nil); code != exitFail {
		t.Fatalf("code = %d", code)
	}
	mustContain(t, out.String(), "could not verify board #3", "rate limit")
}

func TestGitHubClientCommitsPaginateAndCapFailsClosed(t *testing.T) {
	api := &fakeAPI{commitCount: 250, body: "x"}
	cl := api.client(t, "board-token")
	cs, err := cl.Commits(context.Background())
	if err != nil || len(cs) != 250 {
		t.Fatalf("commits = %d, err = %v", len(cs), err)
	}
	var out bytes.Buffer
	if code := run(context.Background(), config{Repo: repo}, cl, cl, &out, nil); code != exitFail {
		t.Fatalf("code = %d\n%s", code, out.String())
	}
	api.commitCount = 130
	cs, _ = cl.Commits(context.Background())
	if len(cs) != 130 {
		t.Fatalf("commits = %d", len(cs))
	}
}

func TestRealMainRequiresRepoAndPR(t *testing.T) {
	var out bytes.Buffer
	if code := realMain(func(string) string { return "" }, &out); code != 2 {
		t.Fatalf("code = %d", code)
	}
}
