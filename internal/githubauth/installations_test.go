package githubauth

import (
	"fmt"
	"strings"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

func TestDistinctOwnersLogging(t *testing.T) {
	got := distinctOwnersLogging([]string{"a/one", "b/two", "a/three", "malformed", "b/four"}, func(string, ...any) {})
	want := []string{"a", "b"}
	if len(got) != len(want) {
		t.Fatalf("distinctOwnersLogging = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("distinctOwnersLogging[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestSplitOwnerRepo(t *testing.T) {
	cases := []struct {
		spec        string
		owner, repo string
		ok          bool
	}{
		{"owner/repo", "owner", "repo", true},
		{"malformed", "", "", false},
		{"a/b/c", "", "", false},
		{"/repo", "", "", false},
		{"owner/", "", "", false},
	}
	for _, tc := range cases {
		owner, repo, ok := splitOwnerRepo(tc.spec)
		if owner != tc.owner || repo != tc.repo || ok != tc.ok {
			t.Errorf("splitOwnerRepo(%q) = (%q, %q, %v), want (%q, %q, %v)", tc.spec, owner, repo, ok, tc.owner, tc.repo, tc.ok)
		}
	}
}

func TestVerifyRepoAccess_ExclusionSurfacesGrantURL(t *testing.T) {
	srv, fake := newFakeAppServer("pruefer-bot", []gh.AppInstallation{
		{ID: 111, Account: "someorg", RepositorySelection: "selected"},
	}, func() time.Time { return time.Now().Add(time.Hour) })
	fake.selectedRepos = map[int64][]string{111: {"someorg/repo-one"}}
	defer srv.Close()

	dir := t.TempDir()
	keyPath := writeTestPrivateKey(t, dir)
	privateKey, err := loadPrivateKey(keyPath)
	if err != nil {
		t.Fatalf("loadPrivateKey: %v", err)
	}
	a, err := mintAuth(42, 111, "pruefer-bot[bot]", privateKey, srv.URL)
	if err != nil {
		t.Fatalf("mintAuth: %v", err)
	}

	statuses, err := verifyRepoAccess(srv.URL, a.client.Token(), 111, "someorg", []string{"someorg/repo-one", "someorg/repo-excluded"})
	if err != nil {
		t.Fatalf("verifyRepoAccess: %v", err)
	}
	if len(statuses) != 2 {
		t.Fatalf("expected 2 statuses, got %d: %+v", len(statuses), statuses)
	}
	byRepo := map[string]RepoStatus{}
	for _, s := range statuses {
		byRepo[s.Repo] = s
	}
	if !byRepo["someorg/repo-one"].Authorized {
		t.Error("expected someorg/repo-one to be authorized")
	}
	excluded := byRepo["someorg/repo-excluded"]
	if excluded.Authorized {
		t.Error("expected someorg/repo-excluded to be unauthorized")
	}
	if !strings.Contains(excluded.GrantURL, "111") {
		t.Errorf("GrantURL = %q, expected it to reference installation 111", excluded.GrantURL)
	}
}

// TestVerifyRepoAccess_RepoNameMatchIsCaseInsensitive is the regression test
// for a review finding: accessibleSet was keyed by GitHub's exact-case
// canonical full_name, but repoSpec comes verbatim from the user's
// watched_repos config string. GitHub repo names are case-insensitive, so a
// config entry like "someorg/Repo-One" must still match a canonical
// "someorg/repo-one" the installation actually grants — not be
// misreported as excluded/unauthorized.
func TestVerifyRepoAccess_RepoNameMatchIsCaseInsensitive(t *testing.T) {
	srv, fake := newFakeAppServer("pruefer-bot", []gh.AppInstallation{
		{ID: 111, Account: "someorg", RepositorySelection: "selected"},
	}, func() time.Time { return time.Now().Add(time.Hour) })
	fake.selectedRepos = map[int64][]string{111: {"someorg/repo-one"}}
	defer srv.Close()

	dir := t.TempDir()
	keyPath := writeTestPrivateKey(t, dir)
	privateKey, err := loadPrivateKey(keyPath)
	if err != nil {
		t.Fatalf("loadPrivateKey: %v", err)
	}
	a, err := mintAuth(42, 111, "pruefer-bot[bot]", privateKey, srv.URL)
	if err != nil {
		t.Fatalf("mintAuth: %v", err)
	}

	statuses, err := verifyRepoAccess(srv.URL, a.client.Token(), 111, "someorg", []string{"someorg/Repo-One"})
	if err != nil {
		t.Fatalf("verifyRepoAccess: %v", err)
	}
	if len(statuses) != 1 {
		t.Fatalf("expected 1 status, got %d: %+v", len(statuses), statuses)
	}
	if !statuses[0].Authorized {
		t.Errorf("expected a case-insensitive repo-name match to be authorized, got: %+v", statuses[0])
	}
}

// TestVerifyRepoAccess_TruncatedListYieldsAmbiguousReason is the regression
// test for a review finding: FetchInstallationRepositories caps at 100
// results and only surfaces truncation via a package-level log line —
// verifyRepoAccess previously had no way to distinguish "genuinely excluded"
// from "beyond page 1 of a >100-repo installation," and reported both with
// the same confident "excludes it" reason. A watched repo missing from a
// truncated 100-result page must instead get a reason that says the result
// may be incomplete, not that GitHub actively excluded it.
func TestVerifyRepoAccess_TruncatedListYieldsAmbiguousReason(t *testing.T) {
	accessibleRepos := make([]string, 100)
	for i := range accessibleRepos {
		accessibleRepos[i] = fmt.Sprintf("someorg/repo-%03d", i)
	}
	srv, fake := newFakeAppServer("pruefer-bot", []gh.AppInstallation{
		{ID: 111, Account: "someorg", RepositorySelection: "selected"},
	}, func() time.Time { return time.Now().Add(time.Hour) })
	fake.selectedRepos = map[int64][]string{111: accessibleRepos}
	fake.neverShortPage = true // simulate a server that never signals the end of the list
	defer srv.Close()

	dir := t.TempDir()
	keyPath := writeTestPrivateKey(t, dir)
	privateKey, err := loadPrivateKey(keyPath)
	if err != nil {
		t.Fatalf("loadPrivateKey: %v", err)
	}
	a, err := mintAuth(42, 111, "pruefer-bot[bot]", privateKey, srv.URL)
	if err != nil {
		t.Fatalf("mintAuth: %v", err)
	}

	statuses, err := verifyRepoAccess(srv.URL, a.client.Token(), 111, "someorg", []string{"someorg/repo-beyond-page-1"})
	if err != nil {
		t.Fatalf("verifyRepoAccess: %v", err)
	}
	if len(statuses) != 1 {
		t.Fatalf("expected 1 status, got %d: %+v", len(statuses), statuses)
	}
	got := statuses[0]
	if got.Authorized {
		t.Errorf("expected someorg/repo-beyond-page-1 to be reported unauthorized, got authorized")
	}
	if !strings.Contains(got.Reason, "100") || !strings.Contains(got.Reason, "pagination") {
		t.Errorf("Reason = %q, expected it to flag the 100-result truncation as a possible pagination gap", got.Reason)
	}
	if strings.Contains(got.Reason, "excludes it") {
		t.Errorf("Reason = %q, should not confidently claim exclusion when the result set was truncated", got.Reason)
	}
}

// TestVerifyRepoAccess_UntruncatedListKeepsConfidentExclusionReason confirms
// the ambiguous-truncation wording from
// TestVerifyRepoAccess_TruncatedListYieldsAmbiguousReason only applies when
// the accessible-repo list actually hit the 100-result cap — an ordinary,
// definitively-excluded repo below that cap still gets the original,
// confident reason.
func TestVerifyRepoAccess_UntruncatedListKeepsConfidentExclusionReason(t *testing.T) {
	srv, fake := newFakeAppServer("pruefer-bot", []gh.AppInstallation{
		{ID: 111, Account: "someorg", RepositorySelection: "selected"},
	}, func() time.Time { return time.Now().Add(time.Hour) })
	fake.selectedRepos = map[int64][]string{111: {"someorg/repo-one"}}
	defer srv.Close()

	dir := t.TempDir()
	keyPath := writeTestPrivateKey(t, dir)
	privateKey, err := loadPrivateKey(keyPath)
	if err != nil {
		t.Fatalf("loadPrivateKey: %v", err)
	}
	a, err := mintAuth(42, 111, "pruefer-bot[bot]", privateKey, srv.URL)
	if err != nil {
		t.Fatalf("mintAuth: %v", err)
	}

	statuses, err := verifyRepoAccess(srv.URL, a.client.Token(), 111, "someorg", []string{"someorg/repo-excluded"})
	if err != nil {
		t.Fatalf("verifyRepoAccess: %v", err)
	}
	if len(statuses) != 1 {
		t.Fatalf("expected 1 status, got %d: %+v", len(statuses), statuses)
	}
	if statuses[0].Reason != "repository_selection=selected excludes it" {
		t.Errorf("Reason = %q, want the original confident-exclusion wording", statuses[0].Reason)
	}
}

func TestVerifyRepoAccess_IgnoresOtherOwnersRepos(t *testing.T) {
	srv, fake := newFakeAppServer("pruefer-bot", []gh.AppInstallation{
		{ID: 111, Account: "someorg", RepositorySelection: "selected"},
	}, func() time.Time { return time.Now().Add(time.Hour) })
	fake.selectedRepos = map[int64][]string{111: {"someorg/repo-one"}}
	defer srv.Close()

	dir := t.TempDir()
	keyPath := writeTestPrivateKey(t, dir)
	privateKey, err := loadPrivateKey(keyPath)
	if err != nil {
		t.Fatalf("loadPrivateKey: %v", err)
	}
	a, err := mintAuth(42, 111, "pruefer-bot[bot]", privateKey, srv.URL)
	if err != nil {
		t.Fatalf("mintAuth: %v", err)
	}

	statuses, err := verifyRepoAccess(srv.URL, a.client.Token(), 111, "someorg", []string{"someorg/repo-one", "otherorg/repo-two"})
	if err != nil {
		t.Fatalf("verifyRepoAccess: %v", err)
	}
	if len(statuses) != 1 {
		t.Fatalf("expected only someorg's own repo to be checked, got %+v", statuses)
	}
}
