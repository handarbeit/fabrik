package pruefer

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

var shellcheckGrant = []string{"Bash(shellcheck *)"}

// FR-012: the grant and the author decision are made once per PR event, so the
// first attempt and the single fallback re-run to the default skill see the
// same rules and the same plugin copies.
func TestReview_GrantAppliesToEveryAttemptIncludingTheFallback(t *testing.T) {
	for name, tc := range map[string]struct {
		grant     reviewGrant
		wantExtra bool
	}{
		"trusted": {resolveReviewGrant(shellcheckGrant, nil, "alice", "MEMBER"), true},
		"outside": {resolveReviewGrant(shellcheckGrant, nil, "mallory", "NONE"), false},
	} {
		t.Run(name, func(t *testing.T) {
			rec := filepath.Join(t.TempDir(), "rec")
			fakeClaudeBySkill(t, rec, selNeverInvoked, defStream)
			req, op := selReqOp(t, "mine:deep")
			req.Grant = tc.grant
			res, err := (&RealClaudeInvoker{}).Review(context.Background(), req)
			if err != nil || res.FallbackFrom != "mine:deep" {
				t.Fatalf("res = %+v err = %v, want a fallback re-run", res, err)
			}
			calls := recordedCalls(t, rec)
			if len(calls) != 2 {
				t.Fatalf("claude ran %d times, want 2: %q", len(calls), calls)
			}
			for i, c := range calls {
				if has := strings.Contains(c, "Bash(shellcheck *)"); has != tc.wantExtra {
					t.Errorf("attempt %d: extra rule present = %v, want %v: %s", i+1, has, tc.wantExtra, c)
				}
				if usesOriginal := strings.Contains(c, "--plugin-dir "+op); usesOriginal == tc.grant.StripPluginHooks {
					t.Errorf("attempt %d: original operator plugin used = %v with StripPluginHooks=%v: %s", i+1, usesOriginal, tc.grant.StripPluginHooks, c)
				}
			}
		})
	}
}

// R3: for an author without write access an operator plugin's hook and MCP
// server are never started; for a trusted author they are.
func TestReview_OutsideAuthorPluginHooksAndMCPNeverStart(t *testing.T) {
	setDaemonSecrets(t)
	for name, tc := range map[string]struct {
		grant reviewGrant
		runs  bool
	}{
		"trusted":    {resolveReviewGrant(nil, nil, "alice", "OWNER"), true},
		"outside":    {resolveReviewGrant(nil, nil, "mallory", "FIRST_TIME_CONTRIBUTOR"), false},
		"unknown":    {resolveReviewGrant(nil, nil, "mallory", ""), false},
		"dependabot": {resolveReviewGrant(nil, nil, "dependabot[bot]", "NONE"), true},
	} {
		t.Run(name, func(t *testing.T) {
			rec := filepath.Join(t.TempDir(), "rec")
			writeFakeClaude(t, pluginChildrenScript(rec))
			res, err := (&RealClaudeInvoker{}).Review(context.Background(), ReviewRequest{
				PRNumber: 1, WorkDir: t.TempDir(), OperatorPluginDirs: []string{writeEnvPlugin(t)}, Grant: tc.grant,
			})
			if err != nil || res.Text == "" {
				t.Fatalf("review failed: %v", err)
			}
			for _, which := range []string{"hook", "mcp"} {
				_, statErr := os.Stat(rec + "." + which)
				if ran := statErr == nil; ran != tc.runs {
					t.Errorf("plugin %s ran = %v, want %v", which, ran, tc.runs)
				}
			}
			if tc.runs && res.PluginsStripped != nil {
				t.Errorf("PluginsStripped = %v for a trusted author", res.PluginsStripped)
			}
			if !tc.runs && !slices.Equal(res.PluginsStripped, []string{"envplug"}) {
				t.Errorf("PluginsStripped = %v, want [envplug]", res.PluginsStripped)
			}
		})
	}
}

// R5/FR-010: a granted tool is a descendant of the claude process, so it runs in
// the scrubbed review environment — no GitHub token, App key or daemon secret —
// for a trusted author as for anyone else.
func TestReview_GrantedToolRunsInTheScrubbedEnvironment(t *testing.T) {
	setDaemonSecrets(t)
	rec := filepath.Join(t.TempDir(), "tool.env")
	// The fake claude runs the "tool" the way the CLI runs a granted Bash rule:
	// as its own child.
	writeFakeClaude(t, "env > \""+rec+"\"\necho '"+resultLine+"'\n")

	req := ReviewRequest{PRNumber: 1, WorkDir: t.TempDir(), Grant: resolveReviewGrant(shellcheckGrant, nil, "alice", "MEMBER")}
	if res, err := (&RealClaudeInvoker{}).Review(context.Background(), req); err != nil || res.Text == "" {
		t.Fatalf("review failed: %v", err)
	}
	env := readEnvFile(t, rec)
	for name, secret := range daemonSecrets {
		if envHas(env, name) {
			t.Errorf("the tool inherited %s", name)
		}
		for _, kv := range env {
			if strings.Contains(kv, secret) {
				t.Errorf("the tool environment holds a daemon secret value: %s", kv)
			}
		}
	}
	if err := checkReviewEnvScrubbed(env, shellAdded); err != nil {
		t.Errorf("the tool environment is not the scrubbed one: %v", err)
	}
}

// Non-vacuity of the test above: the same child started from the daemon's own
// environment does see the secrets, and the assertions flag them.
func TestGrantedToolEnvironmentTest_FailsAgainstAnUnscrubbedEnvironment(t *testing.T) {
	setDaemonSecrets(t)
	cmd := exec.Command("/bin/sh", "-c", "env")
	cmd.Env = os.Environ()
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	env := strings.Split(strings.TrimSpace(string(out)), "\n")
	if !envHas(env, "GH_TOKEN") || checkReviewEnvScrubbed(env, shellAdded) == nil {
		t.Error("the scrub assertions did not detect an unscrubbed environment")
	}
}
