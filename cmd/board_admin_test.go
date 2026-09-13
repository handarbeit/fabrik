package cmd

import (
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

func TestLoadGitHubToken_FlagWins(t *testing.T) {
	t.Setenv("FABRIK_TOKEN", "env-token")
	tok, err := loadGitHubToken("flag-token")
	if err != nil {
		t.Fatalf("loadGitHubToken: %v", err)
	}
	if tok != "flag-token" {
		t.Errorf("token = %q, want flag-token", tok)
	}
}

func TestLoadGitHubToken_EnvFallback(t *testing.T) {
	t.Setenv("FABRIK_TOKEN", "env-token")
	tok, err := loadGitHubToken("")
	if err != nil {
		t.Fatalf("loadGitHubToken: %v", err)
	}
	if tok != "env-token" {
		t.Errorf("token = %q, want env-token", tok)
	}
}

func TestLoadGitHubToken_Missing(t *testing.T) {
	t.Setenv("FABRIK_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	if _, err := loadGitHubToken(""); err == nil {
		t.Fatal("expected error when no token is available")
	}
}

func TestRefuseIfUserOwnedBoard(t *testing.T) {
	if err := refuseIfUserOwnedBoard("organization"); err != nil {
		t.Errorf("organization should be allowed, got: %v", err)
	}
	err := refuseIfUserOwnedBoard("user")
	if err == nil {
		t.Fatal("expected refusal for user-owned board")
	}
	if !strings.Contains(err.Error(), "organization-only") {
		t.Errorf("refusal message should explain org-only restriction, got: %v", err)
	}
}

func TestRequiredStageColumnNames(t *testing.T) {
	allStages := []*stages.Stage{
		{Name: "Implement", Order: 3},
		{Name: "Specify", Order: 0},
		{Name: "Done", Order: 5, CleanupWorktree: true},
		{Name: "Backlog", Order: -1, Unmanaged: true},
		{Name: "Research", Order: 1},
		{Name: "Queued", Order: 4, HoldingStage: true},
	}
	got := requiredStageColumnNames(allStages)
	want := []string{"Specify", "Research", "Implement", "Queued"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("index %d: got %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

func TestMissingStageColumns(t *testing.T) {
	sf := &gh.StatusField{
		Options: map[string]string{
			"Specify":   "OPT_1",
			"Implement": "OPT_2",
		},
	}
	required := []string{"Specify", "Research", "Implement", "Queued"}
	got := missingStageColumns(required, sf)
	want := []string{"Research", "Queued"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("index %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestMissingStageColumns_NilStatusField(t *testing.T) {
	got := missingStageColumns([]string{"Specify", "Research"}, nil)
	if len(got) != 2 {
		t.Fatalf("got %v, want everything missing when sf is nil", got)
	}
}
