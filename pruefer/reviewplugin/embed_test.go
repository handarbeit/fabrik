package reviewplugin

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/handarbeit/fabrik/internal/skillref"
	"github.com/handarbeit/fabrik/plugin"
)

func TestWriteProducesResolvablePlugin(t *testing.T) {
	dir := t.TempDir()
	if err := Write(dir); err != nil {
		t.Fatalf("Write: %v", err)
	}
	res, why, _, ok := skillref.NewResolver([]string{dir}, skillref.Options{}).Resolve(DefaultRef)
	if !ok {
		t.Fatalf("Resolve(%q) failed: %s", DefaultRef, why)
	}
	if res.Name != "pruefer-review:review" {
		t.Errorf("resolved name = %q", res.Name)
	}
	// A bare reference to a name the plugin lacks must not resolve: the
	// resolver is really looking at what Write produced.
	if _, _, _, ok := skillref.NewResolver([]string{dir}, skillref.Options{}).Resolve("pruefer-review:nope"); ok {
		t.Error("unknown skill resolved")
	}
}

func TestWriteTwiceIntoSeparateDirsIsIndependent(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	if err := Write(a); err != nil {
		t.Fatal(err)
	}
	if err := Write(b); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(a, "skills", "review", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(b, "skills", "review", "SKILL.md")); err != nil {
		t.Errorf("second directory affected by the first: %v", err)
	}
}

func TestSkillBodyAndVersion(t *testing.T) {
	body, err := SkillBody()
	if err != nil {
		t.Fatal(err)
	}
	if len(body) < 500 || body[:8] != "Write a " {
		t.Errorf("unexpected skill body start: %.60q", body)
	}
	if Version() != "0.1.0" {
		t.Errorf("Version = %q", Version())
	}
}

// TestSkillBodyIsTheOriginalDefaultGuidance pins that moving the default
// guidance from a Go constant into the skill changed no byte of it (R5: a repo
// with no customization sees the same review criteria as before). The digest
// is of the pre-#2306 defaultReviewGuidance constant.
func TestSkillBodyIsTheOriginalDefaultGuidance(t *testing.T) {
	body, err := SkillBody()
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(body))
	const want = "a62ec3991b1d58de9f7314449a64dd4d68aff6be4a549ff8b5bc5e74283b086f"
	if got := hex.EncodeToString(sum[:]); got != want {
		t.Errorf("skill body digest = %s, want %s (the default guidance text changed)", got, want)
	}
}

// The review plugin is loaded with --plugin-dir, where a plugin silently
// replaces an installed one of the same name (#2140's lesson). Its name must
// never equal Fabrik's stage plugin or the human-facing PM plugin.
func TestPluginNameDoesNotCollideWithFabrikPlugins(t *testing.T) {
	pmName, _, _ := strings.Cut(plugin.PMPluginID, "@")
	for _, other := range []string{plugin.BasePluginName, plugin.LegacyBasePluginName, pmName} {
		if PluginName == other {
			t.Errorf("pruefer plugin name %q collides with %q", PluginName, other)
		}
	}
	data, err := tree.ReadFile(root + "/.claude-plugin/plugin.json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"name": "`+PluginName+`"`) {
		t.Errorf("manifest name does not match PluginName %q", PluginName)
	}
}
