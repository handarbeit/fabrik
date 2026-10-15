package pruefer

import (
	"strings"
	"testing"
)

// daemonEnv is a representative daemon environment: everything the review
// process needs, plus the secrets and configuration it must never see.
func daemonEnv() []string {
	return []string{
		"PATH=/usr/bin:/bin", "HOME=/home/pruefer", "USER=pruefer", "LANG=en_US.UTF-8",
		"LC_ALL=en_US.UTF-8", "CLAUDE_CONFIG_DIR=/home/pruefer/.claude-reviewer",
		"TMPDIR=/tmp", "HTTPS_PROXY=http://proxy:3128", "SSL_CERT_FILE=/etc/ssl/ca.pem",
		"GH_TOKEN=ghp_secret", "GITHUB_TOKEN=ghs_secret", "FABRIK_TOKEN=fab_secret",
		"ANTHROPIC_API_KEY=sk-ant-secret", "CLAUDE_CODE_OAUTH_TOKEN=oat-secret",
		"PRUEFER_GITHUB_APP_PRIVATE_KEY_PATH=.pruefer/app-private-key.pem",
		"PRUEFER_REPOS=a/b", "HOOKDECK_API_KEY=hk_secret", "DATABASE_URL=postgres://x",
		"CLAUDE_CODE_EFFORT_LEVEL=low", // daemon value must lose to the review's own
	}
}

func envMap(env []string) map[string]string {
	m := map[string]string{}
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i > 0 {
			m[kv[:i]] = kv[i+1:]
		}
	}
	return m
}

func TestReviewProcessEnv_DropsSecrets(t *testing.T) {
	env := reviewProcessEnv(daemonEnv(), ReviewRequest{})
	m := envMap(env)
	for _, name := range []string{
		"GH_TOKEN", "GITHUB_TOKEN", "FABRIK_TOKEN", "ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN",
		"PRUEFER_GITHUB_APP_PRIVATE_KEY_PATH", "PRUEFER_REPOS", "HOOKDECK_API_KEY", "DATABASE_URL",
	} {
		if _, ok := m[name]; ok {
			t.Errorf("review env holds %s", name)
		}
	}
	for _, kv := range env {
		for _, secret := range []string{"_secret", "-secret", "postgres://"} {
			if strings.Contains(kv, secret) {
				t.Errorf("review env value leaks %q", kv)
			}
		}
	}
	if err := checkReviewEnvScrubbed(env, nil); err != nil {
		t.Errorf("built env fails the scrub check: %v", err)
	}
}

func TestReviewProcessEnv_PassesAllowlisted(t *testing.T) {
	m := envMap(reviewProcessEnv(daemonEnv(), ReviewRequest{Effort: "max"}))
	for name, want := range map[string]string{
		"PATH": "/usr/bin:/bin", "HOME": "/home/pruefer", "USER": "pruefer", "LANG": "en_US.UTF-8",
		"LC_ALL": "en_US.UTF-8", "CLAUDE_CONFIG_DIR": "/home/pruefer/.claude-reviewer",
		"TMPDIR": "/tmp", "HTTPS_PROXY": "http://proxy:3128", "SSL_CERT_FILE": "/etc/ssl/ca.pem",
		"CLAUDE_CODE_EFFORT_LEVEL":              "max",
		"CLAUDE_CODE_DISABLE_ADAPTIVE_THINKING": "1",
	} {
		if m[name] != want {
			t.Errorf("%s = %q, want %q", name, m[name], want)
		}
	}
}

// TestCheckReviewEnvScrubbed_NonVacuous: the checker must flag an environment
// built the old way (the whole daemon environment), and each secret alone.
func TestCheckReviewEnvScrubbed_NonVacuous(t *testing.T) {
	unscrubbed := mergeEnv(daemonEnv(), buildReviewEnv(ReviewRequest{}))
	err := checkReviewEnvScrubbed(unscrubbed, nil)
	if err == nil {
		t.Fatal("checker accepted the unscrubbed daemon environment")
	}
	for _, name := range []string{"GH_TOKEN", "GITHUB_TOKEN", "FABRIK_TOKEN"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("violation list lacks %s: %v", name, err)
		}
	}
	for _, name := range []string{"GH_TOKEN", "GITHUB_TOKEN", "FABRIK_TOKEN", "PRUEFER_REPOS", "ANTHROPIC_API_KEY"} {
		good := reviewProcessEnv(daemonEnv(), ReviewRequest{})
		if checkReviewEnvScrubbed(append(good, name+"=x"), nil) == nil {
			t.Errorf("checker accepted %s", name)
		}
	}
}

func TestReviewProcessEnv_Passthrough(t *testing.T) {
	env := append(daemonEnv(), ReviewEnvPassthroughVar+"=CLAUDE_CODE_OAUTH_TOKEN, ANTHROPIC_API_KEY ,GH_TOKEN,FABRIK_TOKEN,PRUEFER_REPOS,bad=name,GITHUB_X")
	m := envMap(reviewProcessEnv(env, ReviewRequest{}))
	if m["CLAUDE_CODE_OAUTH_TOKEN"] != "oat-secret" || m["ANTHROPIC_API_KEY"] != "sk-ant-secret" {
		t.Errorf("named passthrough variables not forwarded: %v", m)
	}
	for _, name := range []string{"GH_TOKEN", "FABRIK_TOKEN", "PRUEFER_REPOS", ReviewEnvPassthroughVar, "GITHUB_TOKEN", "HOOKDECK_API_KEY"} {
		if _, ok := m[name]; ok {
			t.Errorf("%s must never be forwarded", name)
		}
	}
}

func TestParseReviewEnvPassthrough(t *testing.T) {
	names, refused := parseReviewEnvPassthrough("A_B, c1 ,GH_TOKEN,github_token,FABRIK_X,HOOKDECK_Y,1BAD,a=b,,")
	if strings.Join(names, ",") != "A_B,c1" {
		t.Errorf("names = %v", names)
	}
	if len(refused) != 6 {
		t.Errorf("refused = %v", refused)
	}
}
