package githubauth

import (
	"strings"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

func TestDistinctOwners(t *testing.T) {
	got := distinctOwners([]string{"a/one", "b/two", "a/three", "malformed", "b/four"})
	want := []string{"a", "b"}
	if len(got) != len(want) {
		t.Fatalf("distinctOwners = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("distinctOwners[%d] = %q, want %q", i, got[i], want[i])
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
