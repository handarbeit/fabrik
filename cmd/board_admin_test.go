package cmd

import (
	"context"
	"strings"
	"testing"

	"github.com/handarbeit/fabrik/config"
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

// TestBoardColumnNames: create and repair must produce the Done (cleanup)
// column — a board created without it can never complete an item — while an
// unmanaged parking stage (Backlog) stays optional.
func TestBoardColumnNames(t *testing.T) {
	allStages := []*stages.Stage{
		{Name: "Implement", Order: 3},
		{Name: "Specify", Order: 0},
		{Name: "Done", Order: 5, CleanupWorktree: true},
		{Name: "Backlog", Order: -1, Unmanaged: true},
		{Name: "Research", Order: 1},
		{Name: "Queued", Order: 4, HoldingStage: true},
	}
	got := boardColumnNames(allStages)
	want := []string{"Specify", "Research", "Implement", "Queued", "Done"}
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

// #1893 R5: an App-only deployment (no PAT) can run repair-board; a missing,
// partial or GHES-incompatible App configuration is decided before any network call.
func TestAppBoardClient_ConfigResolution(t *testing.T) {
	t.Setenv("FABRIK_GITHUB_APP_ID", "")
	t.Setenv("FABRIK_GITHUB_APP_PRIVATE_KEY_PATH", "")
	t.Setenv("FABRIK_GITHUB_APP_INSTALLATION_ID", "")

	c, err := appBoardClient(context.Background(), "org", config.ProjectConfig{}, "")
	if c != nil || err != nil {
		t.Errorf("no App config: got (%v, %v), want (nil, nil) so the caller falls back to the PAT path", c, err)
	}

	id := int64(1)
	_, err = appBoardClient(context.Background(), "org", config.ProjectConfig{GitHubAppID: &id}, "")
	if err == nil || !strings.Contains(err.Error(), "partially configured") {
		t.Errorf("partial config: err = %v", err)
	}

	inst := int64(2)
	full := config.ProjectConfig{GitHubAppID: &id, GitHubAppPrivateKeyPath: "/k.pem", GitHubAppInstallationID: &inst}
	_, err = appBoardClient(context.Background(), "org", full, "ghe.example.com")
	if err == nil || !strings.Contains(err.Error(), "Enterprise Server") {
		t.Errorf("GHES + App: err = %v", err)
	}
}

func strPtr(s string) *string { return &s }

func specNames(specs []displayFieldSpec) []string {
	var out []string
	for _, s := range specs {
		out = append(out, s.Name+":"+s.DataType)
	}
	return out
}

func TestRequiredDisplayFields_Defaults(t *testing.T) {
	got := strings.Join(specNames(requiredDisplayFields(config.ProjectFieldsConfig{})), ",")
	want := "Fabrik:TEXT,Last activity:DATE,Last run:TEXT"
	if got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestRequiredDisplayFields_CustomNameAndOff(t *testing.T) {
	pf := config.ProjectFieldsConfig{StatusLine: strPtr("Status line"), LastActivity: strPtr("OFF"), LastRun: strPtr("  ")}
	got := strings.Join(specNames(requiredDisplayFields(pf)), ",")
	want := "Status line:TEXT,Last run:TEXT" // blank means default; off (any case) drops the field
	if got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestPlanDisplayFields_Classification(t *testing.T) {
	specs := requiredDisplayFields(config.ProjectFieldsConfig{})
	fields := []gh.ProjectField{
		{ID: "1", Name: "Title", DataType: "TITLE"},
		{ID: "2", Name: "fabrik", DataType: "SINGLE_SELECT"}, // case-insensitive, wrong type
		{ID: "3", Name: "Last activity", DataType: "DATE"},
	}
	plan := planDisplayFields(specs, fields)
	want := []displayFieldState{displayFieldMismatch, displayFieldPresent, displayFieldMissing}
	for i, w := range want {
		if plan[i].State != w {
			t.Errorf("%s: state = %s, want %s", plan[i].Name, plan[i].State, w)
		}
	}
	if plan[0].Found != "SINGLE_SELECT" {
		t.Errorf("Found = %q", plan[0].Found)
	}
	if line := displayFieldLine(plan[0]); !strings.Contains(line, "expected TEXT, found SINGLE_SELECT") {
		t.Errorf("mismatch line = %q", line)
	}
}

func TestPlanDisplayFields_BuiltInNameIsMismatch(t *testing.T) {
	pf := config.ProjectFieldsConfig{StatusLine: strPtr("Labels")}
	plan := planDisplayFields(requiredDisplayFields(pf), []gh.ProjectField{{Name: "Labels", DataType: "LABELS"}})
	if plan[0].State != displayFieldMismatch {
		t.Errorf("state = %s, want mismatch", plan[0].State)
	}
}

func TestPlanDisplayFields_DuplicateConfiguredName(t *testing.T) {
	pf := config.ProjectFieldsConfig{StatusLine: strPtr("Shared"), LastRun: strPtr("shared")}
	plan := planDisplayFields(requiredDisplayFields(pf), nil)
	if plan[0].State != displayFieldMissing || plan[2].State != displayFieldCollision {
		t.Errorf("states = %s, %s, %s", plan[0].State, plan[1].State, plan[2].State)
	}
	rec := &recordingFieldClient{}
	applyDisplayFields(rec, "P", plan)
	if len(rec.creates) != 2 { // Shared + Last activity; never the collision
		t.Errorf("creates = %v", rec.creates)
	}
}

type recordingFieldClient struct {
	fields  []gh.ProjectField
	creates []string
	failOn  string
	emptyID bool
}

func (r *recordingFieldClient) FetchProjectFields(string) ([]gh.ProjectField, error) {
	return r.fields, nil
}

func (r *recordingFieldClient) CreateProjectV2Field(_, name, dataType string) (string, error) {
	r.creates = append(r.creates, name+":"+dataType)
	if name == r.failOn {
		return "", errString("boom")
	}
	if r.emptyID {
		return "", nil
	}
	return "F_" + name, nil
}

type errString string

func (e errString) Error() string { return string(e) }

func TestApplyDisplayFields_PartialFailureAttemptsAllAndNamesFailed(t *testing.T) {
	plan := planDisplayFields(requiredDisplayFields(config.ProjectFieldsConfig{}), nil)
	rec := &recordingFieldClient{failOn: "Last activity"}
	applyDisplayFields(rec, "P", plan)
	if len(rec.creates) != 3 {
		t.Fatalf("all three must be attempted, got %v", rec.creates)
	}
	err := displayFieldsErr(plan)
	if err == nil || !strings.Contains(err.Error(), "Last activity") || strings.Contains(err.Error(), `"Last run"`) {
		t.Fatalf("err = %v", err)
	}
	if plan[0].State != displayFieldCreated || plan[1].State != displayFieldFailed || plan[2].State != displayFieldCreated {
		t.Errorf("states = %s %s %s", plan[0].State, plan[1].State, plan[2].State)
	}
}

func TestApplyDisplayFields_EmptyIDIsFailure(t *testing.T) {
	plan := planDisplayFields(requiredDisplayFields(config.ProjectFieldsConfig{}), nil)
	applyDisplayFields(&recordingFieldClient{emptyID: true}, "P", plan)
	if countDisplayFields(plan, displayFieldFailed) != 3 {
		t.Errorf("empty IDs must fail every field: %+v", plan)
	}
}

func TestApplyDisplayFields_OnlyCreatesMissing(t *testing.T) {
	fields := []gh.ProjectField{{Name: "Fabrik", DataType: "SINGLE_SELECT"}, {Name: "Last run", DataType: "TEXT"}}
	plan := planDisplayFields(requiredDisplayFields(config.ProjectFieldsConfig{}), fields)
	rec := &recordingFieldClient{}
	applyDisplayFields(rec, "P", plan)
	if len(rec.creates) != 1 || rec.creates[0] != "Last activity:DATE" {
		t.Errorf("creates = %v", rec.creates)
	}
	if displayFieldsErr(plan) != nil {
		t.Error("a mismatch must not be an error")
	}
}
