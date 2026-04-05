package engine

import (
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

func TestParseOwnerRepo(t *testing.T) {
	tests := []struct {
		input     string
		wantOwner string
		wantRepo  string
	}{
		{"owner/repo", "owner", "repo"},
		{"acme/widgets", "arbeithand", "develop"},
		{"org/my-repo", "org", "my-repo"},
		// Edge cases
		{"", "", ""},
		{"noslash", "", ""},
		{"/nooowner", "", ""},
		{"norepoo/", "", ""},
		{"a/b/c", "a", "b/c"}, // SplitN(2) keeps trailing slash in repo
	}
	for _, tt := range tests {
		owner, repo := parseOwnerRepo(tt.input)
		if owner != tt.wantOwner || repo != tt.wantRepo {
			t.Errorf("parseOwnerRepo(%q) = (%q, %q), want (%q, %q)",
				tt.input, owner, repo, tt.wantOwner, tt.wantRepo)
		}
	}
}

func TestRepoName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"owner/repo", "repo"},
		{"acme/widgets", "develop"},
		{"", ""},
		{"noslash", ""},
		{"/nooowner", ""},
	}
	for _, tt := range tests {
		got := repoName(tt.input)
		if got != tt.want {
			t.Errorf("repoName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestIssueKey(t *testing.T) {
	tests := []struct {
		item        gh.ProjectItem
		defaultRepo string
		want        string
	}{
		{
			item:        gh.ProjectItem{Number: 42, Repo: "owner/repo"},
			defaultRepo: "fallback/fallback",
			want:        "owner/repo#42",
		},
		{
			// Empty Repo falls back to defaultRepo
			item:        gh.ProjectItem{Number: 7, Repo: ""},
			defaultRepo: "owner/repo",
			want:        "owner/repo#7",
		},
		{
			// Empty both — result uses empty string as repo prefix
			item:        gh.ProjectItem{Number: 1, Repo: ""},
			defaultRepo: "",
			want:        "#1",
		},
	}
	for _, tt := range tests {
		got := issueKey(tt.item, tt.defaultRepo)
		if got != tt.want {
			t.Errorf("issueKey(item{Repo:%q, Number:%d}, %q) = %q, want %q",
				tt.item.Repo, tt.item.Number, tt.defaultRepo, got, tt.want)
		}
	}
}

func TestItemOwnerRepo(t *testing.T) {
	tests := []struct {
		item        gh.ProjectItem
		defaultRepo string
		wantOwner   string
		wantRepo    string
	}{
		{
			item:        gh.ProjectItem{Repo: "org/proj"},
			defaultRepo: "fallback/fb",
			wantOwner:   "org",
			wantRepo:    "proj",
		},
		{
			// Falls back to defaultRepo when Repo is empty
			item:        gh.ProjectItem{Repo: ""},
			defaultRepo: "owner/repo",
			wantOwner:   "owner",
			wantRepo:    "repo",
		},
		{
			// Both empty
			item:        gh.ProjectItem{Repo: ""},
			defaultRepo: "",
			wantOwner:   "",
			wantRepo:    "",
		},
	}
	for _, tt := range tests {
		owner, repo := itemOwnerRepo(tt.item, tt.defaultRepo)
		if owner != tt.wantOwner || repo != tt.wantRepo {
			t.Errorf("itemOwnerRepo(item{Repo:%q}, %q) = (%q, %q), want (%q, %q)",
				tt.item.Repo, tt.defaultRepo, owner, repo, tt.wantOwner, tt.wantRepo)
		}
	}
}
