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
	if Version() != "0.1.2" {
		t.Errorf("Version = %q", Version())
	}
}

// TestSkillBodyIsTheOriginalDefaultGuidance pins that moving the default
// guidance from a Go constant into the skill changed no byte of it (R5: a repo
// with no customization sees the same review criteria as before). The digest
// is of the pre-#2306 defaultReviewGuidance constant, less the two paragraphs
// cut out below. Its value changed when the second cut was added (#2375); the
// bytes it covers did not (checked against the pre-#2375 body).
func TestSkillBodyIsTheOriginalDefaultGuidance(t *testing.T) {
	body, err := SkillBody()
	if err != nil {
		t.Fatal(err)
	}
	// Two paragraphs are cut out before digesting, because each was reworded
	// by a later issue: #2362 added the one on reading the staged diff, and
	// #2375 reworded "Finish the review" so that naming what was skipped is not
	// a way to finish. The criteria around them are what this test guards;
	// TestFinishTheReviewParagraph guards the reworded one.
	cut := func(from, to string) {
		i := strings.Index(body, from)
		j := strings.Index(body, to)
		if i < 0 || j < i {
			t.Fatalf("paragraph %q not found before %q (i=%d, j=%d)", from, to, i, j)
		}
		body = body[:i] + body[j:]
	}
	cut("**Read the staged diff in amounts", "**The diff tells you what changed.")
	cut("**Finish the review.**", "**Make every finding falsifiable.**")
	sum := sha256.Sum256([]byte(body))
	const want = "255a91bb58964e06bce38657492ef379e0624ccf2e02951627fa674bf86e9be7"
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

// #2375: the closing paragraph must not excuse unread staged entries. The
// digest test above cuts this paragraph out, so it is pinned here instead: the
// old "name what you could not check" way out is gone and the rule that entries
// must be opened, and what "could not check" means now, are present.
func TestFinishTheReviewParagraph(t *testing.T) {
	body, err := SkillBody()
	if err != nil {
		t.Fatal(err)
	}
	i, j := strings.Index(body, "**Finish the review.**"), strings.Index(body, "**Make every finding falsifiable.**")
	if i < 0 || j < i {
		t.Fatalf("paragraph not found (i=%d, j=%d)", i, j)
	}
	para := body[i:j]
	for _, want := range []string{
		"open every entry before you conclude",
		"naming what you skipped is not a way to finish",
		"analysis you tried and could not complete",
		"never for entries you chose not to open",
	} {
		if !strings.Contains(para, want) {
			t.Errorf("Finish the review lacks %q:\n%s", want, para)
		}
	}
	if strings.Contains(para, "say so explicitly and name what you could not check") {
		t.Error("the old excuse for stopping early is still there")
	}
}
