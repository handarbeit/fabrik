package engine

import (
	"errors"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

func stubHookConfig(t *testing.T, cfg gh.AppHookConfig, err error) {
	t.Helper()
	orig := fetchAppHookConfigFn
	fetchAppHookConfigFn = func(*Engine) (gh.AppHookConfig, error) { return cfg, err }
	t.Cleanup(func() { fetchAppHookConfigFn = orig })
}

// AC7 / SC-005.
func TestCheckAppWebhookConfig(t *testing.T) {
	const want = "https://fabrik-models.tail1.ts.net/webhook"
	for _, tc := range []struct {
		name string
		cfg  gh.AppHookConfig
		keys []string
	}{
		{"match", gh.AppHookConfig{URL: want, ContentType: "json"}, nil},
		{"match with trailing slash, caps and :443", gh.AppHookConfig{URL: "HTTPS://Fabrik-Models.tail1.ts.net:443/webhook/", ContentType: "json"}, nil},
		{"application/json is json", gh.AppHookConfig{URL: want, ContentType: "application/json"}, nil},
		{"wrong url", gh.AppHookConfig{URL: "https://other.example.com/webhook", ContentType: "json"}, []string{"tailscale_webhook_config:url"}},
		{"wrong path", gh.AppHookConfig{URL: "https://fabrik-models.tail1.ts.net/hook", ContentType: "json"}, []string{"tailscale_webhook_config:url"}},
		{"empty url", gh.AppHookConfig{ContentType: "json"}, []string{"tailscale_webhook_config:url"}},
		{"form content type", gh.AppHookConfig{URL: want, ContentType: "form"}, []string{"tailscale_webhook_config:content_type"}},
		{"both wrong", gh.AppHookConfig{URL: "http://x/", ContentType: "form"}, []string{"tailscale_webhook_config:url", "tailscale_webhook_config:content_type"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateWarnings(t)
			stubHookConfig(t, tc.cfg, nil)
			eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
			eng.checkAppWebhookConfig("tailscale", want)
			got := warningKeys(t)
			if len(got) != len(tc.keys) {
				t.Fatalf("keys = %v, want %v", got, tc.keys)
			}
			for _, k := range tc.keys {
				if !got[k] {
					t.Fatalf("keys = %v, want %v", got, tc.keys)
				}
			}
		})
	}
}

// A mismatch the operator fixes clears the entry on the next check.
func TestCheckAppWebhookConfig_ClearsOnFix(t *testing.T) {
	isolateWarnings(t)
	const want = "https://n.t.ts.net/webhook"
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	stubHookConfig(t, gh.AppHookConfig{URL: "https://wrong/", ContentType: "form"}, nil)
	eng.checkAppWebhookConfig("tailscale", want)
	if len(warningKeys(t)) != 2 {
		t.Fatalf("keys = %v", warningKeys(t))
	}
	stubHookConfig(t, gh.AppHookConfig{URL: want, ContentType: "json"}, nil)
	eng.checkAppWebhookConfig("tailscale", want)
	if k := warningKeys(t); len(k) != 0 {
		t.Fatalf("not cleared: %v", k)
	}
}

// FR-014: an unreadable config is skipped — one log line, no panel entry, and a
// previously recorded mismatch is left alone rather than cleared on a guess.
func TestCheckAppWebhookConfig_UnreadableIsSkipped(t *testing.T) {
	isolateWarnings(t)
	const want = "https://n.t.ts.net/webhook"
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	logs := &syncLog{}
	eng.SetLogSinkForTest(func(_ int, tag, msg string) { logs.add("[%s] %s", tag, msg) })
	stubHookConfig(t, gh.AppHookConfig{}, errors.New("403 forbidden"))
	eng.checkAppWebhookConfig("tailscale", want)
	if k := warningKeys(t); len(k) != 0 {
		t.Fatalf("keys = %v", k)
	}
	if n := strings.Count(logs.String(), "skipping the webhook URL check"); n != 1 {
		t.Fatalf("log lines = %d:\n%s", n, logs.String())
	}
	// An unknown node URL skips without reading at all.
	called := false
	orig := fetchAppHookConfigFn
	fetchAppHookConfigFn = func(*Engine) (gh.AppHookConfig, error) { called = true; return gh.AppHookConfig{}, nil }
	defer func() { fetchAppHookConfigFn = orig }()
	eng.checkAppWebhookConfig("tailscale", "")
	if called {
		t.Fatal("read the App config with no node URL to compare")
	}
}

// The check runs on every reconcile tick, so a standing failure logs once per
// streak: the first failure, then again only when the error text changes or after
// a successful read ended the streak.
func TestCheckAppWebhookConfig_UnreadableLogsOncePerStreak(t *testing.T) {
	isolateWarnings(t)
	const want = "https://n.t.ts.net/webhook"
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	logs := &syncLog{}
	eng.SetLogSinkForTest(func(_ int, tag, msg string) { logs.add("[%s] %s", tag, msg) })
	lines := func(sub string) int { return strings.Count(logs.String(), sub) }

	stubHookConfig(t, gh.AppHookConfig{}, errors.New("403 forbidden"))
	for i := 0; i < 5; i++ {
		eng.checkAppWebhookConfig("tailscale", want)
	}
	if n := lines("403 forbidden"); n != 1 {
		t.Fatalf("a standing failure logged %d times, want 1:\n%s", n, logs.String())
	}

	stubHookConfig(t, gh.AppHookConfig{}, errors.New("401 bad credentials"))
	eng.checkAppWebhookConfig("tailscale", want)
	eng.checkAppWebhookConfig("tailscale", want)
	if n := lines("401 bad credentials"); n != 1 {
		t.Fatalf("a changed error logged %d times, want 1", n)
	}

	stubHookConfig(t, gh.AppHookConfig{URL: want, ContentType: "json"}, nil)
	eng.checkAppWebhookConfig("tailscale", want)
	stubHookConfig(t, gh.AppHookConfig{}, errors.New("401 bad credentials"))
	eng.checkAppWebhookConfig("tailscale", want)
	if n := lines("401 bad credentials"); n != 2 {
		t.Fatalf("a failure after a successful read logged %d times in total, want 2", n)
	}
}

// The check rides the transport's coverage hook, so reconcile re-runs it.
func TestTailscale_CoverageHookRunsWebhookConfigCheck(t *testing.T) {
	nodes := installFakeTailnet(t, func(cfg tailnetNodeConfig) *fakeTailnet { return newFakeTailnet(cfg, "n.t.ts.net.") })
	_ = nodes
	isolateWarnings(t)
	stubHookConfig(t, gh.AppHookConfig{URL: "https://wrong/", ContentType: "json"}, nil)
	t.Setenv(DefaultAppWebhookSecretEnv, directTestSecret)
	t.Setenv(DefaultTailscaleAuthKeyEnv, "k")
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	eng.fabrikDir = t.TempDir()
	eng.cfg.EventSource = EventSourceTailscale
	eng.cfg.TailscaleHostname = "h"
	eng.cfg.AppWebhookPath = "/webhook"
	cleanup := eng.setUpIngestion(t.Context(), nil)
	defer cleanup()
	if k := warningKeys(t); !k["tailscale_webhook_config:url"] {
		t.Fatalf("startup check did not run: %v", k)
	}
	stubHookConfig(t, gh.AppHookConfig{URL: "https://n.t.ts.net/webhook", ContentType: "json"}, nil)
	tailscaleFromEngine(t, eng).CheckCoverage()
	if k := warningKeys(t); k["tailscale_webhook_config:url"] {
		t.Fatalf("reconcile-time check did not re-run: %v", k)
	}
}

func TestNormalizeWebhookURL(t *testing.T) {
	a := normalizeWebhookURL("https://Foo.ts.net:443/webhook/")
	b := normalizeWebhookURL("https://foo.ts.net/webhook")
	if a != b {
		t.Fatalf("%q != %q", a, b)
	}
	if normalizeWebhookURL("https://foo.ts.net:8443/webhook") == b {
		t.Fatal("a non-default port is significant")
	}
	if normalizeWebhookURL("http://foo.ts.net/webhook") == b {
		t.Fatal("scheme is significant")
	}
}
