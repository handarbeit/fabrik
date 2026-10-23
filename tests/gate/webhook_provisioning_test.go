package gate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeBedEnv(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCheckBedWebhookProvisioning(t *testing.T) {
	full := "FABRIK_TOKEN=t\nFABRIK_TAILSCALE_HOSTNAME=bed-a\nFABRIK_TS_AUTHKEY=" + probeAuthKey + "\nFABRIK_GITHUB_WEBHOOK_SECRET=" + probeHookSecret + "\n"
	app := []string{"app"}

	t.Run("no-op without the tailscale source or an app leg", func(t *testing.T) {
		g, _, _, _ := testGate(t)
		if err := g.CheckBedWebhookProvisioning(app); err != nil {
			t.Fatal(err)
		}
		g.Cfg.EventSource = "tailscale"
		if err := g.CheckBedWebhookProvisioning([]string{"pat"}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("a provisioned bed passes", func(t *testing.T) {
		g, _, _, _ := testGate(t)
		g.Cfg.EventSource = "tailscale"
		writeBedEnv(t, g.Cfg.TestBed, full)
		if err := g.CheckBedWebhookProvisioning(app); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("names every missing piece and never a value", func(t *testing.T) {
		g, _, _, _ := testGate(t)
		g.Cfg.EventSource = "tailscale"
		writeBedEnv(t, g.Cfg.TestBed, "FABRIK_TOKEN=t\n")
		err := g.CheckBedWebhookProvisioning(app)
		msg := failureCode(t, err, ExitPreconditionFailed)
		for _, want := range []string{"FABRIK_TAILSCALE_HOSTNAME", "FABRIK_GITHUB_WEBHOOK_SECRET", "FABRIK_TS_AUTHKEY"} {
			if !strings.Contains(msg, want) {
				t.Errorf("message must name %s: %s", want, msg)
			}
		}
	})
	t.Run("a stored login stands in for the auth key", func(t *testing.T) {
		g, _, _, _ := testGate(t)
		g.Cfg.EventSource = "tailscale"
		writeBedEnv(t, g.Cfg.TestBed, "FABRIK_TAILSCALE_HOSTNAME=bed-a\nFABRIK_GITHUB_WEBHOOK_SECRET=s\n")
		state := filepath.Join(g.Cfg.TestBed, ".fabrik", "state", "tailscale")
		os.MkdirAll(state, 0o755)
		os.WriteFile(filepath.Join(state, "tailscaled.state"), []byte("{}"), 0o600)
		if err := g.CheckBedWebhookProvisioning(app); err != nil {
			t.Fatalf("a bed with a stored login needs no auth key: %v", err)
		}
	})
	t.Run("custom variable names are honoured", func(t *testing.T) {
		g, _, _, _ := testGate(t)
		g.Cfg.EventSource = "tailscale"
		writeBedEnv(t, g.Cfg.TestBed, "FABRIK_TAILSCALE_HOSTNAME=h\nFABRIK_APP_WEBHOOK_SECRET_ENV=MY_SECRET\nMY_SECRET=x\nFABRIK_TAILSCALE_AUTH_KEY_ENV=MY_KEY\nMY_KEY=y\n")
		if err := g.CheckBedWebhookProvisioning(app); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("two beds may not share a hostname", func(t *testing.T) {
		g, _, _, _ := testGate(t)
		g.Cfg.EventSource = "tailscale"
		a, b := t.TempDir(), t.TempDir()
		writeBedEnv(t, a, full)
		writeBedEnv(t, b, strings.Replace(full, "bed-a", "BED-A", 1))
		g.Cfg.BedDirs = []string{a, b}
		g.buildBedGates()
		msg := failureCode(t, g.CheckBedWebhookProvisioning(app), ExitPreconditionFailed)
		if !strings.Contains(msg, "same tailnet hostname") {
			t.Errorf("message: %s", msg)
		}
	})
}

func failureCode(t *testing.T, err error, code int) string {
	t.Helper()
	ee, ok := err.(*ExitError)
	if !ok || ee.Code != code {
		t.Fatalf("want ExitError code %d, got %v", code, err)
	}
	return ee.Msg
}
