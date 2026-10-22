package engine

import (
	"strings"
	"testing"

	"github.com/handarbeit/fabrik/internal/workerenv"
)

func TestRefuseUnknownEventSource_AcceptsTailscale(t *testing.T) {
	if err := RefuseUnknownEventSource(EventSourceTailscale); err != nil {
		t.Fatalf("tailscale refused: %v", err)
	}
	err := RefuseUnknownEventSource("bogus")
	if err == nil || !strings.Contains(err.Error(), `"tailscale"`) {
		t.Fatalf("unknown-source message should list tailscale, got %v", err)
	}
}

// R5 / AC4: every refusal names the missing setting.
func TestRefuseTailscaleMisconfigured(t *testing.T) {
	orig := tailscaleBuilt
	tailscaleBuilt = true
	defer func() { tailscaleBuilt = orig }()

	good := Config{EventSource: EventSourceTailscale, TailscaleHostname: "fabrik-models"}
	const key = "tskey-auth-zzz-secret"
	tests := []struct {
		name    string
		cfg     Config
		app     bool
		secret  string
		authKey string
		state   bool
		wantErr string
	}{
		{"not tailscale: ignored", Config{EventSource: EventSourceDirect}, false, "", "", false, ""},
		{"happy path first start", good, true, "s3cret", key, false, ""},
		{"happy path with state and no key", good, true, "s3cret", "", true, ""},
		{"happy path custom path", Config{EventSource: EventSourceTailscale, TailscaleHostname: "h", AppWebhookPath: "/x"}, true, "s3cret", key, false, ""},
		{"PAT auth", good, false, "s3cret", key, false, "requires GitHub App authentication"},
		{"missing hostname", Config{EventSource: EventSourceTailscale}, true, "s3cret", key, false, "tailscale_hostname"},
		{"blank hostname", Config{EventSource: EventSourceTailscale, TailscaleHostname: "  "}, true, "s3cret", key, false, "tailscale_hostname"},
		{"missing secret", good, true, "", key, false, "$FABRIK_GITHUB_WEBHOOK_SECRET"},
		{"missing secret custom name", Config{EventSource: EventSourceTailscale, TailscaleHostname: "h", AppWebhookSecretEnv: "MY_SECRET"}, true, "", key, false, "$MY_SECRET"},
		{"relative path", Config{EventSource: EventSourceTailscale, TailscaleHostname: "h", AppWebhookPath: "hook"}, true, "s3cret", key, false, "app_webhook_path"},
		{"no key and no state", good, true, "s3cret", "", false, "$FABRIK_TS_AUTHKEY"},
		{"empty key is missing, custom name", Config{EventSource: EventSourceTailscale, TailscaleHostname: "h", TailscaleAuthKeyEnv: "MY_TS"}, true, "s3cret", "", false, "$MY_TS"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := RefuseTailscaleMisconfigured(tt.cfg, tt.app, tt.secret, tt.authKey, tt.state)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected refusal: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.wantErr)
			}
			for _, v := range []string{tt.secret, tt.authKey} {
				if v != "" && strings.Contains(err.Error(), v) {
					t.Fatalf("refusal leaked a secret value: %v", err)
				}
			}
		})
	}
}

// AC3 (any build, via the seam): a binary without the embedded node refuses
// with the build hint, before any other check.
func TestRefuseTailscaleMisconfigured_NotBuiltIn(t *testing.T) {
	orig := tailscaleBuilt
	tailscaleBuilt = false
	defer func() { tailscaleBuilt = orig }()
	err := RefuseTailscaleMisconfigured(Config{EventSource: EventSourceTailscale}, false, "", "", false)
	if err == nil || !strings.Contains(err.Error(), "-tags tailscale") {
		t.Fatalf("err = %v, want the build hint", err)
	}
	if err := RefuseTailscaleMisconfigured(Config{EventSource: EventSourceDirect}, false, "", "", false); err != nil {
		t.Fatalf("other event sources must be unaffected: %v", err)
	}
}

// New() wires the refusal in: a tailscale config with PAT auth fails startup.
func TestNew_RefusesTailscaleWithoutAppAuth(t *testing.T) {
	orig := tailscaleBuilt
	tailscaleBuilt = true
	defer func() { tailscaleBuilt = orig }()
	_, err := New(Config{EventSource: EventSourceTailscale, TailscaleHostname: "h", Token: "tok", Owner: "o", Repo: "r", ProjectNum: 1})
	if err == nil || !strings.Contains(err.Error(), "event_source: tailscale") {
		t.Fatalf("err = %v", err)
	}
}

// The auth key variable (default and operator-chosen name) never reaches a worker.
func TestWorkerSecretEnvNames_ScrubsTailscaleAuthKey(t *testing.T) {
	resetWorkerEnv(t)
	cfg := Config{TailscaleAuthKeyEnv: "MY_TS_KEY"}
	claudeWorkerEnv = workerenv.Resolve(workerSecretEnvNames(cfg), "")
	got := constructedEnv(append(daemonEnv(), "MY_TS_KEY=tskey-x", "FABRIK_TS_AUTHKEY=tskey-y", "OTHER=o"), InvokeOptions{})
	if hasKey(got, "MY_TS_KEY") || hasKey(got, "FABRIK_TS_AUTHKEY") {
		t.Fatal("a Tailscale auth key variable reached the worker")
	}
	if !containsExact(got, "OTHER=o") {
		t.Fatal("unrelated variable must survive")
	}
}
