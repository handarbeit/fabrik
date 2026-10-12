package engine

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/plugin"
	"github.com/handarbeit/fabrik/stages"
)

// TestLive_WorkerExcludesPMPluginAndLoadsStagePlugins is the SC-001
// demonstration (#2140, ADR 2140). It launches a real `claude` with the exact
// argument list buildClaudeArgs produces and reads the session's init event.
// It is opt-in (FABRIK_LIVE_PLUGIN_CHECK=1, claude on PATH) because it spends a
// one-turn model call, and it is only meaningful in the combined environment:
// run it with fabrik@fabrik installed and enabled for the current user, so the
// absence of the PM plugin is the settings' doing and not an accident.
func TestLive_WorkerExcludesPMPluginAndLoadsStagePlugins(t *testing.T) {
	if os.Getenv("FABRIK_LIVE_PLUGIN_CHECK") != "1" {
		t.Skip("set FABRIK_LIVE_PLUGIN_CHECK=1 to run (spends one model turn)")
	}
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude is not on PATH")
	}

	base := extractEmbeddedPlugin(t)
	withPluginDirs(t, []string{base})
	stage := &stages.Stage{Name: "Probe", Model: "haiku", Prompt: "p"}
	args := append([]string{"-p", "say hi"}, buildClaudeArgs(stage, "", "", 1, false, "", "")...)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Dir = t.TempDir()
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	var init struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
		Plugins []struct {
			Name   string `json:"name"`
			Source string `json:"source"`
		} `json:"plugins"`
		Skills []string `json:"skills"`
	}
	found := false
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var ev = init
		if json.Unmarshal(sc.Bytes(), &ev) == nil && ev.Type == "system" && ev.Subtype == "init" {
			init, found = ev, true
			break
		}
	}
	cancel() // the init event is all this needs; do not wait for the turn
	_ = cmd.Wait()
	if !found {
		t.Fatal("no system/init event from claude")
	}

	// The init event's plugin shape has not been observed on a host that has
	// the PM plugin installed, so match on every identity it could carry — the
	// marketplace id in source, or the bare PM plugin name — and log the raw list
	// so the first such run shows what the fields really hold.
	t.Logf("init plugins: %+v", init.Plugins)
	pmName, _, _ := strings.Cut(plugin.PMPluginID, "@")
	for _, p := range init.Plugins {
		if p.Source == plugin.PMPluginID || strings.HasPrefix(p.Source, plugin.PMPluginID) ||
			p.Name == pmName || p.Name == plugin.PMPluginID {
			t.Errorf("PM plugin is loaded in the worker: %+v", p)
		}
	}
	loaded := false
	for _, p := range init.Plugins {
		if p.Name == plugin.BasePluginName {
			loaded = true
		}
	}
	if !loaded {
		t.Errorf("stage plugin %s is not loaded: %+v", plugin.BasePluginName, init.Plugins)
	}
	have := map[string]bool{}
	for _, s := range init.Skills {
		have[s] = true
	}
	for _, s := range plugin.ShippedSkillNames() {
		if q := plugin.Qualify(plugin.BasePluginName, s); !have[q] {
			t.Errorf("stage skill %s is not registered", q)
		}
	}
}
