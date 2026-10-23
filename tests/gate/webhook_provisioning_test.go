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

	t.Run("no-op when no webhook cell is planned (E2E_EVENTS=poll, or no app leg)", func(t *testing.T) {
		g, _, _, _ := testGate(t)
		g.Cfg.EventsFilter = EventsPoll
		if err := g.CheckBedWebhookProvisioning(app); err != nil {
			t.Fatal(err)
		}
		g.Cfg.EventsFilter = ""
		if err := g.CheckBedWebhookProvisioning([]string{"pat"}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("the default plan has a webhook cell, so an unprovisioned bed is refused", func(t *testing.T) {
		g, _, _, _ := testGate(t)
		failureCode(t, g.CheckBedWebhookProvisioning(app), ExitPreconditionFailed)
	})
	t.Run("one capable bed is enough; the other is told it gets no webhook cell", func(t *testing.T) {
		g, _, out, _ := testGate(t)
		a, b := t.TempDir(), t.TempDir()
		writeBedEnv(t, a, "FABRIK_TOKEN=t\n")
		writeBedEnv(t, b, full)
		g.Cfg.BedDirs = []string{a, b}
		g.buildBedGates()
		if err := g.CheckBedWebhookProvisioning(app); err != nil {
			t.Fatalf("a webhook-capable bed B must satisfy the check: %v", err)
		}
		if !strings.Contains(out.String(), "not a webhook-capable bed") {
			t.Errorf("the incapable bed must be named: %q", out.String())
		}
		beds := g.beds()
		if beds[0].bedWebhookCapable() || !beds[1].bedWebhookCapable() {
			t.Errorf("capability: A=%v B=%v, want false/true", beds[0].bedWebhookCapable(), beds[1].bedWebhookCapable())
		}
	})
	t.Run("E2E_EVENT_SOURCE=tailscale is a deprecated alias for E2E_EVENTS=webhook", func(t *testing.T) {
		c, err := LoadConfig(func(k string) string {
			if k == "E2E_EVENT_SOURCE" {
				return "tailscale"
			}
			return ""
		}, "/repo")
		if err != nil || c.EventsFilter != EventsWebhook || !c.EventSourceAliased {
			t.Fatalf("filter=%q aliased=%v err=%v", c.EventsFilter, c.EventSourceAliased, err)
		}
	})
	t.Run("a provisioned bed passes", func(t *testing.T) {
		g, _, _, _ := testGate(t)
		writeBedEnv(t, g.Cfg.TestBed, full)
		if err := g.CheckBedWebhookProvisioning(app); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("names every missing piece and never a value", func(t *testing.T) {
		g, _, _, _ := testGate(t)
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
		writeBedEnv(t, g.Cfg.TestBed, "FABRIK_TAILSCALE_HOSTNAME=h\nFABRIK_APP_WEBHOOK_SECRET_ENV=MY_SECRET\nMY_SECRET=x\nFABRIK_TAILSCALE_AUTH_KEY_ENV=MY_KEY\nMY_KEY=y\n")
		if err := g.CheckBedWebhookProvisioning(app); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("two beds may not share a hostname", func(t *testing.T) {
		g, _, _, _ := testGate(t)
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

// The preflight is keyed on the cells the plan actually contains (#2426): a sparse
// run whose selection holds no events: sensitive test plans no webhook cell, so an
// unprovisioned bed is not refused for one that would never run.
func TestCheckBedWebhookProvisioningForPlanFollowsThePlannedCells(t *testing.T) {
	g, _, _, _ := testGate(t)
	g.Cfg.RepoRoot = "../.." // the real registry: tests/gate -> repo root
	g.Cfg.Matrix = MatrixSparse
	app := []string{"app"}

	// The default sparse plan has an app/on/webhook cell: the unprovisioned bed is refused.
	failureCode(t, g.CheckBedWebhookProvisioningForPlan(&Plan{AuthModes: app}), ExitPreconditionFailed)

	// A caller -run naming only an events: neutral test narrows the webhook cell away.
	narrow := &Plan{AuthModes: app, Args: []string{"-run", "^TestAuthLockLabelShape$"}}
	if err := g.CheckBedWebhookProvisioningForPlan(narrow); err != nil {
		t.Fatalf("no webhook cell is planned, so no bed is refused for one: %v", err)
	}
}
