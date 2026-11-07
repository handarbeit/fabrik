package pruefer

import (
	"strings"
	"testing"
)

func appCfg(src string) Config {
	return Config{
		EventSource:         src,
		DirectListen:        "127.0.0.1:8787",
		TailscaleHostname:   "pruefer-acme",
		TailscaleStateDir:   DefaultTailscaleStateDir,
		TailscaleAuthKeyEnv: DefaultTailscaleAuthKeyEnv,
		AppWebhookPath:      DefaultAppWebhookPath,
		AppWebhookSecretEnv: DefaultAppWebhookSecretEnv,
	}
}

func TestValidateAppSourceStatic(t *testing.T) {
	for _, src := range []string{EventSourcePoll} {
		c := Config{EventSource: src} // everything else empty: must not matter
		if err := validateAppSourceStatic(c); err != nil {
			t.Errorf("%s: %v", src, err)
		}
	}
	if err := validateAppSourceStatic(appCfg(EventSourceDirect)); err != nil {
		t.Errorf("valid direct: %v", err)
	}
	if err := validateAppSourceStatic(appCfg(EventSourceTailscale)); err != nil {
		t.Errorf("valid tailscale: %v", err)
	}

	cases := []struct {
		name string
		src  string
		mut  func(*Config)
		want string
	}{
		{"direct no listen", EventSourceDirect, func(c *Config) { c.DirectListen = " " }, "direct_listen"},
		{"tailscale no hostname", EventSourceTailscale, func(c *Config) { c.TailscaleHostname = "" }, "tailscale_hostname"},
		{"tailscale no state dir", EventSourceTailscale, func(c *Config) { c.TailscaleStateDir = "" }, "tailscale_state_dir"},
		{"relative path", EventSourceDirect, func(c *Config) { c.AppWebhookPath = "hook" }, "app_webhook_path"},
		{"empty path", EventSourceDirect, func(c *Config) { c.AppWebhookPath = "" }, "app_webhook_path"},
		{"secret env not a name", EventSourceDirect, func(c *Config) { c.AppWebhookSecretEnv = "s3cr3t value!" }, "app_webhook_secret_env"},
		{"auth key env not a name", EventSourceTailscale, func(c *Config) { c.TailscaleAuthKeyEnv = "tskey-auth-abc" }, "tailscale_auth_key_env"},
	}
	for _, tc := range cases {
		c := appCfg(tc.src)
		tc.mut(&c)
		err := validateAppSourceStatic(c)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want mention of %q", tc.name, err, tc.want)
		}
	}
}

func TestRefuseAppSourceMisconfigured(t *testing.T) {
	if err := RefuseAppSourceMisconfigured(Config{EventSource: EventSourcePoll}, "", "", false); err != nil {
		t.Errorf("non-app source refused: %v", err)
	}
	if err := RefuseAppSourceMisconfigured(appCfg(EventSourceDirect), "sec", "", false); err != nil {
		t.Errorf("direct with secret: %v", err)
	}
	if err := RefuseAppSourceMisconfigured(appCfg(EventSourceDirect), "", "", false); err == nil ||
		!strings.Contains(err.Error(), "$"+DefaultAppWebhookSecretEnv) {
		t.Errorf("empty secret: err = %v", err)
	}
	// Auth key is required only while the state dir holds no logged-in node.
	if err := RefuseAppSourceMisconfigured(appCfg(EventSourceTailscale), "sec", "", false); err == nil ||
		!strings.Contains(err.Error(), "$"+DefaultTailscaleAuthKeyEnv) {
		t.Errorf("first start without key: err = %v", err)
	}
	if err := RefuseAppSourceMisconfigured(appCfg(EventSourceTailscale), "sec", "", true); err != nil {
		t.Errorf("restart from state without key must be accepted: %v", err)
	}
	if err := RefuseAppSourceMisconfigured(appCfg(EventSourceTailscale), "sec", "tskey", false); err != nil {
		t.Errorf("first start with key: %v", err)
	}
}

// The refusal messages name variables, never the values they hold.
func TestRefuseAppSourceMessagesNeverEchoValues(t *testing.T) {
	const secret, key = "SECRET-VALUE-123", "tskey-auth-VALUE-456"
	c := appCfg(EventSourceTailscale)
	for _, mut := range []func(*Config){
		func(c *Config) { c.TailscaleHostname = "" },
		func(c *Config) { c.AppWebhookPath = "x" },
	} {
		cc := c
		mut(&cc)
		if err := RefuseAppSourceMisconfigured(cc, secret, key, false); err != nil {
			for _, v := range []string{secret, key} {
				if strings.Contains(err.Error(), v) {
					t.Errorf("error echoes a value: %v", err)
				}
			}
		}
	}
	if err := RefuseAppSourceMisconfigured(c, "", key, false); err != nil && strings.Contains(err.Error(), key) {
		t.Errorf("error echoes the key: %v", err)
	}
}
