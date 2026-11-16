package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/handarbeit/fabrik/stages"
)

func reviewStatusSetUp(t *testing.T, perms map[string]string, cfg Config) (string, error) {
	t.Helper()
	dir := t.TempDir()
	keyPath := writeEngineTestAppKey(t, dir)
	srv := newFakeGitHubAppServer(t, 999, "handarbeit", "organization", perms)
	cfg.Owner, cfg.Repo = "handarbeit", "fabrik"
	cfg.GitHubAppID, cfg.GitHubAppPrivateKeyPath, cfg.GitHubAppInstallationID = 42, keyPath, 999
	var err error
	out := captureStdout(func() {
		_, _, err = setUpGitHubAppAuth(context.Background(), cfg, dir, srv.URL, true)
	})
	return out, err
}

func boolP(b bool) *bool { return &b }

// The always-required set keeps statuses: read (#2471, ADR 2471): the write
// grant is a conditional tier, never a startup requirement for everyone.
func TestReviewStatusPermission_NotInAlwaysRequiredSet(t *testing.T) {
	for _, webhooks := range []bool{false, true} {
		if got := RequiredGitHubAppPermissions(webhooks)["statuses"]; got != "read" {
			t.Errorf("webhooks=%v: always-required statuses = %q, want read", webhooks, got)
		}
		for _, https := range []bool{false, true} {
			if got := RequiredGitHubAppPermissionsForGit(webhooks, https)["statuses"]; got != "read" {
				t.Errorf("always-required (https=%v) statuses = %q, want read", https, got)
			}
			if got := RequiredGitHubAppPermissionsWithReviewStatus(webhooks, https, false)["statuses"]; got != "read" {
				t.Errorf("disabled tier statuses = %q, want read", got)
			}
			if got := RequiredGitHubAppPermissionsWithReviewStatus(webhooks, https, true)["statuses"]; got != "write" {
				t.Errorf("enabled tier statuses = %q, want write", got)
			}
		}
	}
}

func TestReviewStatusEnabled(t *testing.T) {
	on := &stages.Stage{Name: "Review", ReviewStatus: boolP(true)}
	off := &stages.Stage{Name: "Validate", ReviewStatus: boolP(false)}
	unset := &stages.Stage{Name: "Plan"}
	for _, tc := range []struct {
		name     string
		repoWide bool
		stages   []*stages.Stage
		want     bool
	}{
		{"nothing", false, nil, false},
		{"unset and false stages", false, []*stages.Stage{off, unset, nil}, false},
		{"repo-wide", true, nil, true},
		{"one stage on", false, []*stages.Stage{unset, on}, true},
	} {
		if got := ReviewStatusEnabled(tc.repoWide, tc.stages); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

// enabled and missing: refused, naming the permission, with the shared remedy.
func TestSetUpGitHubAppAuth_ReviewStatusEnabledAndMissingRefused(t *testing.T) {
	for name, cfg := range map[string]Config{
		"repo-wide": {ReviewStatus: true},
		"one stage": {Stages: []*stages.Stage{{Name: "Review", ReviewStatus: boolP(true)}}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := reviewStatusSetUp(t, httpsGitAppPerms(), cfg) // statuses: read
			if err == nil {
				t.Fatal("expected startup to be refused without statuses:write")
			}
			for _, want := range []string{
				`statuses`, `required "write", granted "read"`,
				"`statuses: write` is required", "fabrik/review",
				"Fabrik cannot change App permissions itself",
				"https://github.com/organizations/handarbeit/settings/apps/fabrik/permissions",
				"https://github.com/organizations/handarbeit/settings/installations/999",
			} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error missing %q: %v", want, err)
				}
			}
		})
	}
}

// disabled and missing: starts exactly as before.
func TestSetUpGitHubAppAuth_ReviewStatusDisabledAndMissingStarts(t *testing.T) {
	cfg := Config{Stages: []*stages.Stage{{Name: "Review", ReviewStatus: boolP(false)}, {Name: "Plan"}}}
	out, err := reviewStatusSetUp(t, httpsGitAppPerms(), cfg) // statuses: read
	if err != nil {
		t.Fatalf("setUpGitHubAppAuth: %v", err)
	}
	if strings.Contains(out, "statuses") {
		t.Errorf("startup output mentions statuses with the feature off: %q", out)
	}
}

// enabled and present: starts.
func TestSetUpGitHubAppAuth_ReviewStatusEnabledAndPresentStarts(t *testing.T) {
	for _, level := range []string{"write", "admin"} {
		perms := httpsGitAppPerms()
		perms["statuses"] = level
		if _, err := reviewStatusSetUp(t, perms, Config{ReviewStatus: true}); err != nil {
			t.Fatalf("statuses:%s: setUpGitHubAppAuth: %v", level, err)
		}
	}
}
