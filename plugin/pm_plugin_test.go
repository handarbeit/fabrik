package plugin

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/handarbeit/fabrik/internal/attention"
)

// Guards for the human-facing PM plugin (plugin/fabrik, #2141, ADR 2141). The
// plugin cannot be exercised end to end in CI (that needs Claude Code), so
// these pin the static shape the release and the live checks rely on.

const pmDir = "fabrik"

func readPM(t *testing.T, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(pmDir, rel))
	if err != nil {
		t.Fatalf("reading %s: %v", rel, err)
	}
	return b
}

func TestPMPluginManifestsParseAndUserConfigReferencesExist(t *testing.T) {
	var manifest struct {
		Name       string `json:"name"`
		Version    string `json:"version"`
		UserConfig map[string]struct {
			Type     string `json:"type"`
			Title    string `json:"title"`
			Required *bool  `json:"required"`
		} `json:"userConfig"`
	}
	if err := json.Unmarshal(readPM(t, ".claude-plugin/plugin.json"), &manifest); err != nil {
		t.Fatalf("plugin.json: %v", err)
	}
	if manifest.Name != "fabrik" || manifest.Version == "" {
		t.Errorf("manifest name/version = %q/%q", manifest.Name, manifest.Version)
	}
	for _, key := range []string{"subscriber", "fabrik_dir"} {
		opt, ok := manifest.UserConfig[key]
		if !ok {
			t.Errorf("userConfig lacks %q", key)
			continue
		}
		// Optional: a user-scope install must not prompt non-Fabrik users.
		if opt.Required != nil && *opt.Required {
			t.Errorf("userConfig.%s must not be required", key)
		}
		if opt.Type == "" || opt.Title == "" {
			t.Errorf("userConfig.%s needs a type and title", key)
		}
	}

	var mcp struct {
		Servers map[string]struct {
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(readPM(t, ".mcp.json"), &mcp); err != nil {
		t.Fatalf(".mcp.json: %v", err)
	}
	srv, ok := mcp.Servers["fabrik"]
	if !ok {
		t.Fatal(".mcp.json lacks the fabrik server")
	}
	if srv.Command != "fabrik" || len(srv.Args) != 1 || srv.Args[0] != "mcp" {
		t.Errorf("server command = %q %v, want fabrik mcp", srv.Command, srv.Args)
	}
	// Configuration travels in the environment: an unset setting is an empty
	// variable, never an empty argument.
	for _, k := range []string{"FABRIK_SUBSCRIBER", "FABRIK_MCP_DIR", "CLAUDE_PROJECT_DIR"} {
		if _, ok := srv.Env[k]; !ok {
			t.Errorf(".mcp.json env lacks %s", k)
		}
	}

	ref := regexp.MustCompile(`\$\{user_config\.([A-Za-z0-9_]+)\}`)
	for _, f := range []string{".mcp.json", "hooks/hooks.json"} {
		for _, m := range ref.FindAllStringSubmatch(string(readPM(t, f)), -1) {
			if _, ok := manifest.UserConfig[m[1]]; !ok {
				t.Errorf("%s references ${user_config.%s}, which plugin.json does not declare", f, m[1])
			}
		}
	}
}

func TestPMPluginSessionStartHookIsWired(t *testing.T) {
	var hooks struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Type    string `json:"type"`
				Command string `json:"command"`
				Timeout int    `json:"timeout"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(readPM(t, "hooks/hooks.json"), &hooks); err != nil {
		t.Fatalf("hooks.json: %v", err)
	}
	groups := hooks.Hooks["SessionStart"]
	if len(groups) != 1 || len(groups[0].Hooks) != 1 {
		t.Fatalf("want exactly one SessionStart hook, got %+v", hooks.Hooks)
	}
	h := groups[0].Hooks[0]
	if h.Type != "command" || h.Timeout <= 0 || h.Timeout > 30 {
		t.Errorf("hook = %+v: want a command hook with a short timeout", h)
	}
	// A single command string, not an exec-form args array: the latter is
	// not known to be honoured, and an ignored args field would run bare sh.
	const prefix = `sh "${CLAUDE_PLUGIN_ROOT}/`
	if !strings.HasPrefix(h.Command, prefix) || !strings.HasSuffix(h.Command, `"`) {
		t.Fatalf("hook command = %q, want %s<script>\"", h.Command, prefix)
	}
	script := strings.TrimSuffix(strings.TrimPrefix(h.Command, prefix), `"`)
	info, err := os.Stat(filepath.Join(pmDir, script))
	if err != nil {
		t.Fatalf("hook script %s: %v", script, err)
	}
	if info.Mode()&0o111 == 0 {
		t.Errorf("hook script %s is not executable", script)
	}
}

func TestMarketplaceFabrikEntryHasARef(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", ".claude-plugin", "marketplace.json"))
	if err != nil {
		t.Fatal(err)
	}
	var mkt struct {
		Plugins []struct {
			Name   string `json:"name"`
			Source struct {
				Source string `json:"source"`
				Path   string `json:"path"`
				Ref    string `json:"ref"`
			} `json:"source"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(b, &mkt); err != nil {
		t.Fatal(err)
	}
	for _, p := range mkt.Plugins {
		if p.Source.Source == "git-subdir" && p.Source.Ref == "" {
			t.Errorf("marketplace entry %q has no ref: the release step pins it to the tag", p.Name)
		}
	}
}

// SC-005: the plugin reads board and worker state only through the daemon's
// tools. No scraping route may reappear in any shipped file.
func TestPMPluginDoesNotScrapeBoardOrWorkerState(t *testing.T) {
	if _, err := os.Stat(filepath.Join(pmDir, "skills", "fabrik", "scripts")); err == nil {
		t.Error("skills/fabrik/scripts must not exist: the helper scripts scraped GitHub")
	}
	forbidden := regexp.MustCompile(`gh api graphql|projectV2|gh project item-list|\bps (aux|-ef)\b|\bpgrep\b|--plugin-dir \$|(board|issue|comments|stage-output|lib)\.sh`)
	err := filepath.WalkDir(pmDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if m := forbidden.Find(b); m != nil {
			t.Errorf("%s contains %q: board and worker state must come from the daemon's tools", path, m)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Every fabrik:* label the PM plugin names must be one the daemon can explain
// (internal/attention), so the skill's label knowledge cannot drift from the
// source of truth. Wildcard families (fabrik:awaiting-*) are skipped; names
// that are skill references rather than labels are listed with their reason.
func TestPMPluginNamesOnlyLabelsTheDaemonExplains(t *testing.T) {
	notLabels := map[string]string{
		"fabrik:fabrik":       "the supervisor skill's qualified name",
		"fabrik:fabrik-setup": "the setup skill's qualified name",
		"fabrik:status":       "the /fabrik:status command",
	}
	re := regexp.MustCompile("fabrik:[a-z][a-z0-9-]*(?::[A-Za-z0-9<>*]+)*")
	seen := 0
	err := filepath.WalkDir(pmDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(path) != ".md" {
			return err
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		for _, name := range re.FindAllString(string(b), -1) {
			if _, ok := notLabels[name]; ok {
				continue
			}
			name = strings.TrimRight(name, "*")
			if strings.HasSuffix(name, "-") {
				continue // a wildcard family such as fabrik:awaiting-*
			}
			seen++
			if attention.MeaningOf(name) == "" {
				t.Errorf("%s names label %q, which internal/attention has no meaning for", path, name)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen == 0 {
		t.Error("found no fabrik:* labels in the plugin docs: is the scan still looking at the right files?")
	}
}
