package bedevents

import (
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	for _, c := range []struct {
		in, want string
		bad      bool
	}{
		{"", Poll, false}, {"poll", Poll, false}, {" Tailscale ", Tailscale, false},
		{"direct", "", true}, {"hookdeck", "", true}, {"webhook", "", true},
	} {
		got, err := Parse(c.in)
		if (err != nil) != c.bad || got != c.want {
			t.Errorf("Parse(%q) = %q, %v; want %q (bad=%v)", c.in, got, err, c.want, c.bad)
		}
		if c.bad && !strings.Contains(err.Error(), RequestVar) {
			t.Errorf("Parse(%q) error %q should name %s", c.in, err, RequestVar)
		}
	}
}

func TestEffective(t *testing.T) {
	for _, c := range []struct{ req, auth, want string }{
		{"", "app", Poll},
		{"poll", "app", Poll},
		{"tailscale", "app", Tailscale},
		{"tailscale", "pat", Poll}, // PAT can never run the transport
		{"tailscale", "", Poll},
		{"bogus", "app", Poll}, // never crash a test process on it
	} {
		if got := Effective(c.req, c.auth); got != c.want {
			t.Errorf("Effective(%q,%q) = %q, want %q", c.req, c.auth, got, c.want)
		}
	}
}

func TestFromEnvAndWebhook(t *testing.T) {
	env := map[string]string{RequestVar: "tailscale", AuthVar: "app"}
	if got := FromEnv(func(k string) string { return env[k] }); !Webhook(got) {
		t.Errorf("FromEnv = %q, want a webhook source", got)
	}
	env[AuthVar] = "pat"
	if got := FromEnv(func(k string) string { return env[k] }); Webhook(got) {
		t.Errorf("a PAT leg must not be a webhook leg, got %q", got)
	}
}

func TestEnvAndStripKeys(t *testing.T) {
	if got := Env(Tailscale); len(got) != 1 || got[0] != "FABRIK_EVENT_SOURCE=tailscale" {
		t.Errorf("Env(tailscale) = %v", got)
	}
	for _, e := range []string{Poll, "", "junk"} {
		if got := Env(e); len(got) != 1 || got[0] != "FABRIK_EVENT_SOURCE=poll" {
			t.Errorf("Env(%q) = %v, want an explicit poll", e, got)
		}
	}
	has := func(k string) bool {
		for _, s := range StripKeys() {
			if s == k {
				return true
			}
		}
		return false
	}
	for _, k := range []string{LaunchVar, "FABRIK_TAILSCALE_HOSTNAME", "FABRIK_TAILSCALE_AUTH_KEY_ENV", "FABRIK_TAILSCALE_STATE_DIR"} {
		if !has(k) {
			t.Errorf("StripKeys must include %s", k)
		}
	}
}

func TestPollSecondsCadence(t *testing.T) {
	if PollSeconds(Poll) != "60" || PollSeconds(Tailscale) != "180" {
		t.Errorf("cadences: poll=%s tailscale=%s", PollSeconds(Poll), PollSeconds(Tailscale))
	}
}
