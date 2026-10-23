package gate

import (
	"testing"
	"time"
)

func TestLoadConfigInconclusiveTunables(t *testing.T) {
	get := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

	c, err := LoadConfig(get(nil), "/repo")
	if err != nil || c.InconclusiveRetries != 2 || c.InconclusiveWarn != 3 {
		t.Fatalf("defaults = %d/%d err=%v, want 2/3", c.InconclusiveRetries, c.InconclusiveWarn, err)
	}
	c, err = LoadConfig(get(map[string]string{"E2E_INCONCLUSIVE_RETRIES": "0", "E2E_INCONCLUSIVE_WARN": "7"}), "/repo")
	if err != nil || c.InconclusiveRetries != 0 || c.InconclusiveWarn != 7 {
		t.Fatalf("explicit = %d/%d err=%v, want 0/7 (0 disables retries)", c.InconclusiveRetries, c.InconclusiveWarn, err)
	}
	for _, bad := range []map[string]string{
		{"E2E_INCONCLUSIVE_RETRIES": "-1"},
		{"E2E_INCONCLUSIVE_RETRIES": "two"},
		{"E2E_INCONCLUSIVE_WARN": "x"},
	} {
		if _, err := LoadConfig(get(bad), "/repo"); err == nil {
			t.Errorf("%v must be a hard error", bad)
		}
	}
}

// #1977: the shared phase keeps the pre-#1977 caps until a measured leg justifies
// raising them; 8/4 are the values to try, opt-in through the environment.
func TestLoadConfigParallelDefaultsAreUnchangedAndOptInRaises(t *testing.T) {
	get := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	c, err := LoadConfig(get(nil), "/repo")
	if err != nil || c.Parallel != "4" || c.ParallelOn != "2" {
		t.Fatalf("defaults = %s/%s err=%v, want 4/2", c.Parallel, c.ParallelOn, err)
	}
	c, err = LoadConfig(get(map[string]string{"E2E_PARALLEL": "8", "E2E_PARALLEL_ON": "4"}), "/repo")
	if err != nil || c.Parallel != "8" || c.ParallelOn != "4" {
		t.Fatalf("opt-in = %s/%s err=%v, want 8/4", c.Parallel, c.ParallelOn, err)
	}
}

// #2263: the shared-identity budget floor is a percentage, 40 by default, 0
// disables it, and anything else is a hard error like the other tunables.
func TestLoadConfigSharedIdentityFloor(t *testing.T) {
	get := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

	c, err := LoadConfig(get(nil), "/repo")
	if err != nil || c.SharedIdentityFloor != 40 || c.SharedIdentityReprobe != 5*time.Minute {
		t.Fatalf("defaults = %d/%v err=%v, want 40/5m", c.SharedIdentityFloor, c.SharedIdentityReprobe, err)
	}
	for val, want := range map[string]int{"0": 0, "75": 75, "100": 100, " 25 ": 25} {
		c, err = LoadConfig(get(map[string]string{"E2E_SHARED_IDENTITY_FLOOR": val}), "/repo")
		if err != nil || c.SharedIdentityFloor != want {
			t.Errorf("floor %q = %d err=%v, want %d", val, c.SharedIdentityFloor, err, want)
		}
	}
	for _, bad := range []string{"-1", "101", "forty", "40%", "0.5"} {
		if _, err := LoadConfig(get(map[string]string{"E2E_SHARED_IDENTITY_FLOOR": bad}), "/repo"); err == nil {
			t.Errorf("E2E_SHARED_IDENTITY_FLOOR=%q must be a hard error", bad)
		}
	}
}

// #2408: E2E_EVENT_SOURCE defaults to poll-only, accepts tailscale, refuses
// anything else, and a PAT leg is always poll whatever was requested.
func TestLoadConfigEventSource(t *testing.T) {
	get := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	c, err := LoadConfig(get(nil), "/repo")
	if err != nil || c.EventSource != "poll" || c.EffectiveEventSource("app") != "poll" {
		t.Fatalf("default = %q err=%v, want poll", c.EventSource, err)
	}
	c, err = LoadConfig(get(map[string]string{"E2E_EVENT_SOURCE": "tailscale"}), "/repo")
	if err != nil || c.EffectiveEventSource("app") != "tailscale" || c.EffectiveEventSource("pat") != "poll" {
		t.Fatalf("tailscale = app:%q pat:%q err=%v", c.EffectiveEventSource("app"), c.EffectiveEventSource("pat"), err)
	}
	if c.DeliveryProbeWait != 5*time.Minute {
		t.Errorf("DeliveryProbeWait = %s, want 5m", c.DeliveryProbeWait)
	}
	for _, bad := range []string{"direct", "hookdeck", "gh-forward"} {
		if _, err := LoadConfig(get(map[string]string{"E2E_EVENT_SOURCE": bad}), "/repo"); err == nil {
			t.Errorf("E2E_EVENT_SOURCE=%s must be a usage error", bad)
		}
	}
}
