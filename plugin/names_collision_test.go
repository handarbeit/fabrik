package plugin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

type manifestName struct {
	Name string `json:"name"`
}

// TestStagePluginAndPMPluginNamesDoNotCollide guards FR-006 (#2140, ADR 2140):
// Fabrik's stage plugin and the human-facing PM plugin once both declared the
// name "fabrik", so a --plugin-dir stage plugin silently replaced the installed
// PM plugin of the same name. They must never share a name again, and the Go
// constants must agree with the manifests.
func TestStagePluginAndPMPluginNamesDoNotCollide(t *testing.T) {
	stageData, err := FabrikPlugin.ReadFile("fabrik-workflows/.claude-plugin/plugin.json")
	if err != nil {
		t.Fatalf("reading embedded stage plugin manifest: %v", err)
	}
	var stage manifestName
	if err := json.Unmarshal(stageData, &stage); err != nil {
		t.Fatalf("parsing stage plugin manifest: %v", err)
	}

	pmData, err := os.ReadFile(filepath.Join("fabrik", ".claude-plugin", "plugin.json"))
	if err != nil {
		t.Fatalf("reading PM plugin manifest: %v", err)
	}
	var pm manifestName
	if err := json.Unmarshal(pmData, &pm); err != nil {
		t.Fatalf("parsing PM plugin manifest: %v", err)
	}

	mktData, err := os.ReadFile(filepath.Join("..", ".claude-plugin", "marketplace.json"))
	if err != nil {
		t.Fatalf("reading marketplace manifest: %v", err)
	}
	var mkt struct {
		Name    string `json:"name"`
		Plugins []struct {
			Name   string `json:"name"`
			Source struct {
				Path string `json:"path"`
			} `json:"source"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(mktData, &mkt); err != nil {
		t.Fatalf("parsing marketplace manifest: %v", err)
	}

	if stage.Name != BasePluginName {
		t.Errorf("stage plugin manifest name = %q, but plugin.BasePluginName = %q", stage.Name, BasePluginName)
	}
	if stage.Name == pm.Name {
		t.Errorf("the stage plugin and the PM plugin both declare the name %q", stage.Name)
	}
	if BasePluginName == pm.Name {
		t.Errorf("plugin.BasePluginName collides with the PM plugin name %q", pm.Name)
	}

	// The marketplace entry whose source is plugin/fabrik must carry the PM
	// plugin's name, and PMPluginID must be exactly <name>@<marketplace>.
	var entry string
	for _, p := range mkt.Plugins {
		if p.Source.Path == "plugin/fabrik" {
			entry = p.Name
		}
		if p.Name == stage.Name {
			t.Errorf("marketplace lists a plugin named %q, the stage plugin's name", stage.Name)
		}
	}
	if entry != pm.Name {
		t.Errorf("marketplace entry for plugin/fabrik is named %q, PM manifest says %q", entry, pm.Name)
	}
	if want := pm.Name + "@" + mkt.Name; PMPluginID != want {
		t.Errorf("PMPluginID = %q, want %q", PMPluginID, want)
	}
}
