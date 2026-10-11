package cmd

import (
	"bytes"
	"errors"
	"path/filepath"
	"testing"

	"github.com/handarbeit/fabrik/warnings"
)

func panelKeys(t *testing.T) map[string]bool {
	t.Helper()
	es, err := warnings.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	k := map[string]bool{}
	for _, e := range es {
		k[e.Key] = true
	}
	return k
}

func isolatePanel(t *testing.T) {
	t.Helper()
	warnings.WarningsPathOverride = filepath.Join(t.TempDir(), "warnings.json")
	t.Cleanup(func() { warnings.WarningsPathOverride = "" })
}

// An unrecognised config key reaches the panel, recurs without duplicating, and
// clears on the next start once it is fixed (#2135).
func TestUnrecognisedConfigKey_PanelRecordsAndClears(t *testing.T) {
	isolatePanel(t)
	resetFlags()
	var buf bytes.Buffer
	warnUnrecognizedConfigKeys([]string{"bogus_a", "bogus_b"}, &buf)
	warnUnrecognizedConfigKeys([]string{"bogus_a", "bogus_b"}, &buf)
	if k := panelKeys(t); len(k) != 2 || !k["config_key:bogus_a"] || !k["config_key:bogus_b"] {
		t.Fatalf("keys = %v", k)
	}
	// "Restart" with one key fixed.
	warnUnrecognizedConfigKeys([]string{"bogus_a"}, &buf)
	if k := panelKeys(t); len(k) != 1 || !k["config_key:bogus_a"] {
		t.Fatalf("keys after fix = %v", k)
	}
	warnUnrecognizedConfigKeys(nil, &buf)
	if k := panelKeys(t); len(k) != 0 {
		t.Fatalf("not cleared: %v", k)
	}
}

// The many "invalid; using default" lines collapse to one panel entry that
// clears when every setting resolves cleanly.
func TestConfigFallbacks_AggregateToOneEntry(t *testing.T) {
	isolatePanel(t)
	resetConfigFallbacks()
	t.Setenv("FABRIK_POLL", "banana")
	t.Setenv("FABRIK_MAX_CONCURRENT", "-3")
	_ = resolveInt(0, "FABRIK_POLL", "of seconds", 60)
	_ = resolveInt(0, "FABRIK_MAX_CONCURRENT", "", 5)
	reportConfigFallbacks()
	reportConfigFallbacks() // recurring: still one
	if k := panelKeys(t); len(k) != 1 || !k["config_value:settings"] {
		t.Fatalf("keys = %v", k)
	}
	es, _ := warnings.Load()
	if es[0].Title != "2 setting(s) invalid — using defaults" {
		t.Errorf("title = %q", es[0].Title)
	}

	// Fixed and restarted.
	resetConfigFallbacks()
	reportConfigFallbacks()
	if k := panelKeys(t); len(k) != 0 {
		t.Errorf("not cleared: %v", k)
	}
}

func TestPluginState_PanelRecordsAndClears(t *testing.T) {
	isolatePanel(t)
	var buf bytes.Buffer
	reportPluginState(&buf, pluginStateReport{
		EvalErr:       errors.New("boom"),
		RefreshErr:    errors.New("nope"),
		Customized:    true,
		CustomizedMsg: "[upgrade] warning: plugin skills have local customizations — skipping auto-refresh\n",
	})
	k := panelKeys(t)
	if len(k) != 3 || !k["plugin_state:check_failed"] || !k["plugin_state:refresh_failed"] || !k["plugin_state:customized"] {
		t.Fatalf("keys = %v", k)
	}
	// TUI mode: nil writer still records, and a clean state clears everything.
	reportPluginState(nil, pluginStateReport{})
	if k := panelKeys(t); len(k) != 0 {
		t.Errorf("not cleared: %v", k)
	}
	reportPluginState(nil, pluginStateReport{Customized: true})
	if k := panelKeys(t); !k["plugin_state:customized"] {
		t.Errorf("nil writer must still record: %v", k)
	}
}

func TestFineGrainedPAT_PanelRecordsAndClears(t *testing.T) {
	isolatePanel(t)
	reportTokenWarning("github_pat_abc")
	reportTokenWarning("github_pat_abc")
	if k := panelKeys(t); len(k) != 1 || !k["config_token:fine_grained_pat"] {
		t.Fatalf("keys = %v", k)
	}
	reportTokenWarning("ghp_classic")
	if k := panelKeys(t); len(k) != 0 {
		t.Errorf("not cleared: %v", k)
	}
}
